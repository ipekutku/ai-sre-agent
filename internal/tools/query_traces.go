package tools

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTraceLookbackMinutes = 15
	defaultTraceLimit           = 10
	maxTraceLimit               = 20
	maxMinDurationMS            = 600_000
	// spansPerTrace is how many matching spans Tempo returns per search result.
	spansPerTrace = 3
	// maxTraceSpans bounds the spans returned for one trace.
	maxTraceSpans = 50
	// maxStatusMessageLength bounds a span's status message.
	maxStatusMessageLength = 200
	// maxTempoResponseBytes bounds how much of a Tempo response is read.
	maxTempoResponseBytes = 2 << 20
	// maxTempoErrorLength bounds how much of a Tempo error body is reported.
	maxTempoErrorLength = 300
)

// spanAttributes are the span attributes query_traces returns. Everything
// else (peer addresses, user agents, SDK details) is dropped.
var spanAttributes = map[string]bool{
	"http.request.method":       true,
	"http.response.status_code": true,
	"http.route":                true,
	"server.address":            true,
	"error.type":                true,
}

// QueryTraces is the query_traces tool: structured, read-only searches and
// trace lookups against a single, fixed Tempo server. The tool builds the
// TraceQL itself; the model cannot send raw queries.
//
// Only spans from services in the catalog are returned. Tempo may also hold
// spans from other sources (such as the agent itself), and those must never
// reach the model.
type QueryTraces struct {
	baseURL    string
	services   map[string]bool
	httpClient *http.Client
	now        func() time.Time
}

// NewQueryTraces returns a query_traces tool for the Tempo server at baseURL
// that can see only the given services.
func NewQueryTraces(baseURL string, services []string, httpClient *http.Client) (*QueryTraces, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("tempo URL must be an absolute http(s) URL, got %q", baseURL)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("at least one service is required")
	}
	m := make(map[string]bool, len(services))
	for _, s := range services {
		// Names are embedded in TraceQL string literals; keep them plain.
		if !validServiceName(s) {
			return nil, fmt.Errorf("invalid service name %q", s)
		}
		if m[s] {
			return nil, fmt.Errorf("duplicate service %q", s)
		}
		m[s] = true
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &QueryTraces{baseURL: strings.TrimRight(baseURL, "/"), services: m, httpClient: httpClient, now: time.Now}, nil
}

func validServiceName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func (*QueryTraces) Name() string { return "query_traces" }

func (q *QueryTraces) Description() string {
	return `Query distributed traces of requests between services. Traces show how long each request
spent in each service and in calls to its dependencies, and where it failed.

Two modes:
- Search: set "service" (and optional filters) to find recent traces that contain a span from that
  service. Returns up to "limit" traces, newest first, each with its root span, total duration, and
  up to ` + strconv.Itoa(spansPerTrace) + ` matching spans from the service.
- Get: set only "trace_id" to return every span of one trace (service, name, kind, parent, start offset,
  duration, status, HTTP attributes), at most ` + strconv.Itoa(maxTraceSpans) + ` spans.

Span status is "error" when the span recorded a failure. Spans become searchable about 30 seconds
after the request ends. Known services: ` + strings.Join(q.names(), ", ") + "."
}

func (q *QueryTraces) InputSchema() json.RawMessage {
	names, _ := json.Marshal(q.names())
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "enum": ` + string(names) + `, "description": "Search: traces with a span from this service"},
    "min_duration_ms": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(maxMinDurationMS) + `, "description": "Search: only spans from the service lasting at least this long"},
    "status": {"type": "string", "enum": ["error", "ok"], "description": "Search: only spans from the service with (error) or without (ok) an error status"},
    "lookback_minutes": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(maxRangeMinutes) + `, "description": "Search: how far back to look (default ` + strconv.Itoa(defaultTraceLookbackMinutes) + `)"},
    "limit": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(maxTraceLimit) + `, "description": "Search: maximum traces to return (default ` + strconv.Itoa(defaultTraceLimit) + `)"},
    "trace_id": {"type": "string", "pattern": "^[0-9a-f]{32}$", "description": "Get: the trace to return; cannot be combined with search fields"}
  },
  "additionalProperties": false
}`)
}

type queryTracesInput struct {
	Service         string `json:"service"`
	MinDurationMS   int    `json:"min_duration_ms"`
	Status          string `json:"status"`
	LookbackMinutes int    `json:"lookback_minutes"`
	Limit           int    `json:"limit"`
	TraceID         string `json:"trace_id"`
}

// TraceSearchResult is the output of a query_traces search.
type TraceSearchResult struct {
	Traces []TraceSummary `json:"traces"`
}

// TraceSummary describes one trace found by a search.
type TraceSummary struct {
	TraceID string `json:"trace_id"`
	// RootService and RootSpan are omitted when the root span is not from a
	// known service (or has not been received yet).
	RootService  string  `json:"root_service,omitempty"`
	RootSpan     string  `json:"root_span,omitempty"`
	Start        string  `json:"start"`
	DurationMS   float64 `json:"duration_ms"`
	MatchedSpans int     `json:"matched_spans"`
	// Spans holds up to spansPerTrace of the matching spans.
	Spans []MatchedSpan `json:"spans"`
}

// MatchedSpan is a span that matched a search.
type MatchedSpan struct {
	Service        string  `json:"service"`
	Name           string  `json:"name"`
	DurationMS     float64 `json:"duration_ms"`
	Status         string  `json:"status"`
	HTTPStatusCode string  `json:"http_status_code,omitempty"`
}

// Trace is the output of a query_traces get.
type Trace struct {
	TraceID string `json:"trace_id"`
	// DurationMS spans from the earliest start to the latest end of the
	// returned spans.
	DurationMS float64     `json:"duration_ms"`
	Spans      []TraceSpan `json:"spans"`
	// TotalSpans is the number of spans before truncation.
	TotalSpans int  `json:"total_spans"`
	Truncated  bool `json:"truncated,omitempty"`
}

// TraceSpan is one span of a trace, sorted by start time.
type TraceSpan struct {
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Service      string `json:"service"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	// StartOffsetMS is relative to the earliest returned span.
	StartOffsetMS float64           `json:"start_offset_ms"`
	DurationMS    float64           `json:"duration_ms"`
	Status        string            `json:"status"`
	StatusMessage string            `json:"status_message,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

func (q *QueryTraces) Call(ctx context.Context, raw json.RawMessage) (any, error) {
	var in queryTracesInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	if in.TraceID != "" {
		if in.Service != "" || in.MinDurationMS != 0 || in.Status != "" || in.LookbackMinutes != 0 || in.Limit != 0 {
			return nil, InvalidInput("trace_id cannot be combined with search fields")
		}
		if !validTraceID(in.TraceID) {
			return nil, InvalidInput("trace_id must be 32 lowercase hex characters")
		}
		return q.get(ctx, in.TraceID)
	}
	if err := q.validateSearch(&in); err != nil {
		return nil, err
	}
	return q.search(ctx, in)
}

func (q *QueryTraces) validateSearch(in *queryTracesInput) error {
	if in.Service == "" {
		return InvalidInput("set service to search, or trace_id to get one trace")
	}
	if !q.services[in.Service] {
		return InvalidInput("unknown service %q; known services: %s", in.Service, strings.Join(q.names(), ", "))
	}
	if in.MinDurationMS < 0 || in.MinDurationMS > maxMinDurationMS {
		return InvalidInput("min_duration_ms must be between 1 and %d", maxMinDurationMS)
	}
	if in.Status != "" && in.Status != "error" && in.Status != "ok" {
		return InvalidInput(`status must be "error" or "ok"`)
	}
	if in.LookbackMinutes < 0 || in.LookbackMinutes > maxRangeMinutes {
		return InvalidInput("lookback_minutes must be between 1 and %d", maxRangeMinutes)
	}
	if in.Limit < 0 || in.Limit > maxTraceLimit {
		return InvalidInput("limit must be between 1 and %d", maxTraceLimit)
	}
	if in.LookbackMinutes == 0 {
		in.LookbackMinutes = defaultTraceLookbackMinutes
	}
	if in.Limit == 0 {
		in.Limit = defaultTraceLimit
	}
	return nil
}

func validTraceID(id string) bool {
	if len(id) != 32 || id == strings.Repeat("0", 32) {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// traceQL builds the search query. Every value comes from validated input:
// the service is from the catalog, the duration is an integer, and the status
// is one of two literals.
func traceQL(in queryTracesInput) string {
	conds := []string{`resource.service.name = "` + in.Service + `"`}
	if in.MinDurationMS > 0 {
		conds = append(conds, "duration >= "+strconv.Itoa(in.MinDurationMS)+"ms")
	}
	switch in.Status {
	case "error":
		conds = append(conds, "status = error")
	case "ok":
		conds = append(conds, "status != error")
	}
	return "{ " + strings.Join(conds, " && ") + " } | select(name, status, span.http.response.status_code)"
}

func (q *QueryTraces) search(ctx context.Context, in queryTracesInput) (TraceSearchResult, error) {
	now := q.now()
	params := url.Values{
		"q":     {traceQL(in)},
		"start": {strconv.FormatInt(now.Add(-time.Duration(in.LookbackMinutes)*time.Minute).Unix(), 10)},
		"end":   {strconv.FormatInt(now.Unix()+1, 10)},
		"limit": {strconv.Itoa(in.Limit)},
		"spss":  {strconv.Itoa(spansPerTrace)},
	}
	var resp tempoSearchResponse
	if _, err := q.getJSON(ctx, "/api/search?"+params.Encode(), &resp); err != nil {
		return TraceSearchResult{}, err
	}

	res := TraceSearchResult{Traces: []TraceSummary{}}
	for _, t := range resp.Traces {
		start, err := parseUnixNano(t.StartTimeUnixNano)
		if err != nil {
			return TraceSearchResult{}, fmt.Errorf("decode tempo search result: %w", err)
		}
		s := TraceSummary{
			// Tempo drops leading zeros from trace IDs in search results;
			// restore them so the ID can be passed back as trace_id.
			TraceID:    fmt.Sprintf("%032s", t.TraceID),
			Start:      formatTime(start),
			DurationMS: float64(t.DurationMS),
			Spans:      []MatchedSpan{},
		}
		if q.services[t.RootServiceName] {
			s.RootService, s.RootSpan = t.RootServiceName, t.RootTraceName
		}
		for _, sp := range t.SpanSet.Spans {
			attrs := attributeMap(sp.Attributes)
			if !q.services[attrs["service.name"]] {
				continue
			}
			dur, err := strconv.ParseInt(sp.DurationNanos, 10, 64)
			if err != nil {
				return TraceSearchResult{}, fmt.Errorf("decode tempo span duration: %w", err)
			}
			s.Spans = append(s.Spans, MatchedSpan{
				Service:        attrs["service.name"],
				Name:           sp.Name,
				DurationMS:     roundMS(time.Duration(dur)),
				Status:         orDefault(attrs["status"], "unset"),
				HTTPStatusCode: attrs["http.response.status_code"],
			})
		}
		s.MatchedSpans = max(t.SpanSet.Matched, len(s.Spans))
		res.Traces = append(res.Traces, s)
	}
	sort.SliceStable(res.Traces, func(i, j int) bool { return res.Traces[i].Start > res.Traces[j].Start })
	return res, nil
}

func (q *QueryTraces) get(ctx context.Context, traceID string) (Trace, error) {
	var resp tempoTraceResponse
	status, err := q.getJSON(ctx, "/api/v2/traces/"+traceID, &resp)
	if status == http.StatusNotFound || (err == nil && len(resp.Trace.ResourceSpans) == 0) {
		return Trace{}, InvalidInput("trace %s not found; it may be too old, too recent to be searchable yet, or mistyped", traceID)
	}
	if err != nil {
		return Trace{}, err
	}

	type timedSpan struct {
		span       TraceSpan
		start, end int64
	}
	var spans []timedSpan
	for _, rs := range resp.Trace.ResourceSpans {
		service := attributeMap(rs.Resource.Attributes)["service.name"]
		if !q.services[service] {
			continue
		}
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				start, err1 := strconv.ParseInt(sp.StartTimeUnixNano, 10, 64)
				end, err2 := strconv.ParseInt(sp.EndTimeUnixNano, 10, 64)
				if err1 != nil || err2 != nil {
					return Trace{}, fmt.Errorf("decode tempo span times for span %q", sp.SpanID)
				}
				out := TraceSpan{
					SpanID:        otlpID(sp.SpanID),
					ParentSpanID:  otlpID(sp.ParentSpanID),
					Service:       service,
					Name:          sp.Name,
					Kind:          strings.ToLower(strings.TrimPrefix(sp.Kind, "SPAN_KIND_")),
					DurationMS:    roundMS(time.Duration(end - start)),
					Status:        spanStatus(sp.Status.Code),
					StatusMessage: truncate(sp.Status.Message, maxStatusMessageLength),
				}
				for k, v := range attributeMap(sp.Attributes) {
					if spanAttributes[k] {
						if out.Attributes == nil {
							out.Attributes = map[string]string{}
						}
						out.Attributes[k] = v
					}
				}
				spans = append(spans, timedSpan{span: out, start: start, end: end})
			}
		}
	}
	if len(spans) == 0 {
		return Trace{}, InvalidInput("trace %s has no spans from known services", traceID)
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].span.SpanID < spans[j].span.SpanID
	})
	first, last := spans[0].start, spans[0].end
	for _, s := range spans {
		last = max(last, s.end)
	}
	res := Trace{TraceID: traceID, DurationMS: roundMS(time.Duration(last - first)), TotalSpans: len(spans)}
	if len(spans) > maxTraceSpans {
		spans = spans[:maxTraceSpans]
		res.Truncated = true
	}
	for _, s := range spans {
		s.span.StartOffsetMS = roundMS(time.Duration(s.start - first))
		res.Spans = append(res.Spans, s.span)
	}
	return res, nil
}

// getJSON GETs path from Tempo and decodes a successful response into v. It
// returns the HTTP status code when a response was received.
func (q *QueryTraces) getJSON(ctx context.Context, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("build tempo request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("query tempo: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTempoResponseBytes+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read tempo response: %w", err)
	}
	if len(body) > maxTempoResponseBytes {
		return resp.StatusCode, &Error{Code: ErrOutputTooLarge, Message: "tempo response too large; narrow the search"}
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, &Error{
			Code:    ErrExecutionFailed,
			Message: fmt.Sprintf("tempo returned HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), maxTempoErrorLength)),
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return resp.StatusCode, fmt.Errorf("decode tempo response: %w", err)
	}
	return resp.StatusCode, nil
}

func (q *QueryTraces) names() []string {
	names := make([]string, 0, len(q.services))
	for n := range q.services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Tempo HTTP API responses (search, and trace by ID in OTLP JSON).

type tempoSearchResponse struct {
	Traces []struct {
		TraceID           string `json:"traceID"`
		RootServiceName   string `json:"rootServiceName"`
		RootTraceName     string `json:"rootTraceName"`
		StartTimeUnixNano string `json:"startTimeUnixNano"`
		DurationMS        int64  `json:"durationMs"`
		SpanSet           struct {
			Spans []struct {
				Name          string          `json:"name"`
				DurationNanos string          `json:"durationNanos"`
				Attributes    []otlpAttribute `json:"attributes"`
			} `json:"spans"`
			Matched int `json:"matched"`
		} `json:"spanSet"`
	} `json:"traces"`
}

type tempoTraceResponse struct {
	Trace struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []otlpAttribute `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					SpanID            string          `json:"spanId"`
					ParentSpanID      string          `json:"parentSpanId"`
					Name              string          `json:"name"`
					Kind              string          `json:"kind"`
					StartTimeUnixNano string          `json:"startTimeUnixNano"`
					EndTimeUnixNano   string          `json:"endTimeUnixNano"`
					Attributes        []otlpAttribute `json:"attributes"`
					Status            struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	} `json:"trace"`
}

type otlpAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		BoolValue   *bool    `json:"boolValue"`
		DoubleValue *float64 `json:"doubleValue"`
	} `json:"value"`
}

// attributeMap flattens scalar OTLP attributes to strings.
func attributeMap(attrs []otlpAttribute) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		switch v := a.Value; {
		case v.StringValue != nil:
			m[a.Key] = *v.StringValue
		case v.IntValue != nil:
			m[a.Key] = *v.IntValue
		case v.BoolValue != nil:
			m[a.Key] = strconv.FormatBool(*v.BoolValue)
		case v.DoubleValue != nil:
			m[a.Key] = strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
		}
	}
	return m
}

// otlpID returns a span ID as hex. Tempo encodes IDs in OTLP JSON as base64;
// IDs that are already hex are returned unchanged.
func otlpID(id string) string {
	if id == "" {
		return ""
	}
	if _, err := hex.DecodeString(id); err == nil && len(id) == 16 {
		return id
	}
	b, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return id
	}
	return hex.EncodeToString(b)
}

func spanStatus(code string) string {
	switch code {
	case "STATUS_CODE_ERROR":
		return "error"
	case "STATUS_CODE_OK":
		return "ok"
	}
	return "unset"
}

func parseUnixNano(s string) (time.Time, error) {
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid unix nano timestamp %q", s)
	}
	return time.Unix(0, ns).UTC(), nil
}

func formatTime(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }

// roundMS converts d to milliseconds with one decimal place.
func roundMS(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*10) / 10
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
