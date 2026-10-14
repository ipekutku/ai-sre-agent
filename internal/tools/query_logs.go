package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultLogLookbackMinutes = 15
	defaultLogLimit           = 20
	maxLogLimit               = 50
	maxContainsLength         = 200
	maxRequestIDLength        = 64
	// maxLogFieldLength bounds a log message or attribute value.
	maxLogFieldLength = 300
	// maxLokiResponseBytes bounds how much of a Loki response is read.
	maxLokiResponseBytes = 2 << 20
	// maxLokiErrorLength bounds how much of a Loki error body is reported.
	maxLokiErrorLength = 300
	// faultAdminComponent marks the fault admin API's log records, which are
	// ground truth. They are never exported to Loki (ADR 0004); query_logs
	// also excludes them in case they ever are.
	faultAdminComponent = "fault-admin"
)

// severityNumbers maps min_severity to the lowest OpenTelemetry severity
// number of that level.
var severityNumbers = map[string]int{"debug": 5, "info": 9, "warn": 13, "error": 17}

// logAttributes are the log attributes query_logs returns. Everything else
// (resource attributes, SDK details, Loki's own fields) is dropped.
var logAttributes = map[string]bool{
	"method":            true,
	"route":             true,
	"status":            true,
	"duration_ms":       true,
	"sku":               true,
	"exception_message": true,
	"exception_type":    true,
}

// QueryLogs is the query_logs tool: structured, read-only log searches
// against a single, fixed Loki server. The tool builds the LogQL itself; the
// model cannot send raw queries.
//
// Only entries from services in the catalog are returned, and entries from
// the fault admin API are always dropped.
type QueryLogs struct {
	baseURL    string
	services   map[string]bool
	httpClient *http.Client
	now        func() time.Time
}

// NewQueryLogs returns a query_logs tool for the Loki server at baseURL that
// can see only the given services.
func NewQueryLogs(baseURL string, services []string, httpClient *http.Client) (*QueryLogs, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("loki URL must be an absolute http(s) URL, got %q", baseURL)
	}
	m, err := serviceSet(services)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &QueryLogs{baseURL: strings.TrimRight(baseURL, "/"), services: m, httpClient: httpClient, now: time.Now}, nil
}

func (*QueryLogs) Name() string { return "query_logs" }

func (q *QueryLogs) Description() string {
	return `Search recent application logs. Each service writes one "request" entry per HTTP request it
handles (method, route, status, duration_ms; logged at error severity for 5xx responses) and an entry
for each error it handles, with the error text in exception_message.

All filters are optional and combined with AND. Without "service", every known service is searched.
"trace_id" and "request_id" find the entries logged while handling one request, in every service it
reached. Returns up to "limit" entries, newest first: time, service, severity, message, trace_id,
request_id, and attributes. "truncated" means more entries matched.

Known services: ` + strings.Join(q.names(), ", ") + "."
}

func (q *QueryLogs) InputSchema() json.RawMessage {
	names, _ := json.Marshal(q.names())
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "enum": ` + string(names) + `, "description": "Only entries from this service (default: all known services)"},
    "min_severity": {"type": "string", "enum": ["debug", "info", "warn", "error"], "description": "Only entries at or above this severity"},
    "contains": {"type": "string", "minLength": 1, "maxLength": ` + strconv.Itoa(maxContainsLength) + `, "description": "Only entries whose message contains this text (case-sensitive; attributes are not searched)"},
    "trace_id": {"type": "string", "pattern": "^[0-9a-f]{32}$", "description": "Only entries logged within this trace"},
    "request_id": {"type": "string", "pattern": "^[A-Za-z0-9._-]{1,64}$", "description": "Only entries logged for this request"},
    "lookback_minutes": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(maxRangeMinutes) + `, "description": "How far back to look (default ` + strconv.Itoa(defaultLogLookbackMinutes) + `)"},
    "limit": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(maxLogLimit) + `, "description": "Maximum entries to return (default ` + strconv.Itoa(defaultLogLimit) + `)"}
  },
  "additionalProperties": false
}`)
}

type queryLogsInput struct {
	Service         string `json:"service"`
	MinSeverity     string `json:"min_severity"`
	Contains        string `json:"contains"`
	TraceID         string `json:"trace_id"`
	RequestID       string `json:"request_id"`
	LookbackMinutes int    `json:"lookback_minutes"`
	Limit           int    `json:"limit"`
}

// LogResult is the output of query_logs.
type LogResult struct {
	Entries []LogEntry `json:"entries"`
	// Truncated is set when more entries matched than were returned.
	Truncated bool `json:"truncated,omitempty"`
}

// LogEntry is one log record.
type LogEntry struct {
	Time       string            `json:"time"`
	Service    string            `json:"service"`
	Severity   string            `json:"severity"`
	Message    string            `json:"message"`
	TraceID    string            `json:"trace_id,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

func (q *QueryLogs) Call(ctx context.Context, raw json.RawMessage) (any, error) {
	var in queryLogsInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	if err := q.validate(&in); err != nil {
		return nil, err
	}

	now := q.now()
	params := url.Values{
		"query":     {q.logQL(in)},
		"start":     {strconv.FormatInt(now.Add(-time.Duration(in.LookbackMinutes)*time.Minute).UnixNano(), 10)},
		"end":       {strconv.FormatInt(now.Add(time.Second).UnixNano(), 10)},
		"limit":     {strconv.Itoa(in.Limit + 1)}, // one extra to detect truncation
		"direction": {"backward"},
	}
	var resp lokiResponse
	if err := q.getJSON(ctx, "/loki/api/v1/query_range?"+params.Encode(), &resp); err != nil {
		return nil, err
	}
	if resp.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected loki result type %q", resp.Data.ResultType)
	}

	type timedEntry struct {
		entry LogEntry
		ns    int64
	}
	var entries []timedEntry
	for _, s := range resp.Data.Result {
		// Loki returns structured metadata merged into the stream labels.
		service := s.Stream["service_name"]
		if !q.services[service] || s.Stream["component"] == faultAdminComponent {
			continue
		}
		for _, v := range s.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("decode loki entry timestamp %q", v[0])
			}
			e := LogEntry{
				Time:      formatTime(time.Unix(0, ns).UTC()),
				Service:   service,
				Severity:  logSeverity(s.Stream),
				Message:   truncate(v[1], maxLogFieldLength),
				TraceID:   s.Stream["trace_id"],
				RequestID: s.Stream["request_id"],
			}
			for k, val := range s.Stream {
				if logAttributes[k] {
					if e.Attributes == nil {
						e.Attributes = map[string]string{}
					}
					e.Attributes[k] = truncate(val, maxLogFieldLength)
				}
			}
			entries = append(entries, timedEntry{entry: e, ns: ns})
		}
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].ns > entries[j].ns })
	res := LogResult{Entries: []LogEntry{}}
	if len(entries) > in.Limit {
		entries = entries[:in.Limit]
		res.Truncated = true
	}
	for _, e := range entries {
		res.Entries = append(res.Entries, e.entry)
	}
	return res, nil
}

func (q *QueryLogs) validate(in *queryLogsInput) error {
	if in.Service != "" && !q.services[in.Service] {
		return InvalidInput("unknown service %q; known services: %s", in.Service, strings.Join(q.names(), ", "))
	}
	if _, ok := severityNumbers[in.MinSeverity]; in.MinSeverity != "" && !ok {
		return InvalidInput(`min_severity must be "debug", "info", "warn" or "error"`)
	}
	if err := validateContains(in.Contains); err != nil {
		return err
	}
	if in.TraceID != "" && !validTraceID(in.TraceID) {
		return InvalidInput("trace_id must be 32 lowercase hex characters")
	}
	if in.RequestID != "" && !validRequestID(in.RequestID) {
		return InvalidInput("request_id must be 1 to %d letters, digits, '.', '_' or '-'", maxRequestIDLength)
	}
	if in.LookbackMinutes < 0 || in.LookbackMinutes > maxRangeMinutes {
		return InvalidInput("lookback_minutes must be between 1 and %d", maxRangeMinutes)
	}
	if in.Limit < 0 || in.Limit > maxLogLimit {
		return InvalidInput("limit must be between 1 and %d", maxLogLimit)
	}
	if in.LookbackMinutes == 0 {
		in.LookbackMinutes = defaultLogLookbackMinutes
	}
	if in.Limit == 0 {
		in.Limit = defaultLogLimit
	}
	return nil
}

func validateContains(s string) error {
	if s == "" {
		return nil
	}
	if !utf8.ValidString(s) {
		return InvalidInput("contains must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(s); n > maxContainsLength {
		return InvalidInput("contains is %d characters, limit is %d", n, maxContainsLength)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return InvalidInput("contains must not contain control characters")
		}
	}
	return nil
}

// validRequestID matches the request IDs the services accept and log.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// logQL builds the query. Service names come from the catalog, the severity
// is a fixed number, and the IDs are format-checked, so only the contains
// text needs escaping: strconv.Quote produces a string literal LogQL reads
// back verbatim, and |= matches it as plain text, not a regular expression.
func (q *QueryLogs) logQL(in queryLogsInput) string {
	var b strings.Builder
	if in.Service != "" {
		b.WriteString(`{service_name=` + strconv.Quote(in.Service) + `}`)
	} else {
		names := q.names()
		for i, n := range names {
			names[i] = regexp.QuoteMeta(n)
		}
		b.WriteString(`{service_name=~` + strconv.Quote(strings.Join(names, "|")) + `}`)
	}
	if in.Contains != "" {
		b.WriteString(` |= ` + strconv.Quote(in.Contains))
	}
	b.WriteString(` | component != "` + faultAdminComponent + `"`)
	if n := severityNumbers[in.MinSeverity]; in.MinSeverity != "" && in.MinSeverity != "debug" {
		b.WriteString(` | severity_number >= ` + strconv.Itoa(n))
	}
	if in.TraceID != "" {
		b.WriteString(` | trace_id = "` + in.TraceID + `"`)
	}
	if in.RequestID != "" {
		b.WriteString(` | request_id = "` + in.RequestID + `"`)
	}
	return b.String()
}

func logSeverity(stream map[string]string) string {
	if s := stream["severity_text"]; s != "" {
		return strings.ToLower(s)
	}
	return orDefault(stream["detected_level"], "unknown")
}

// getJSON GETs path from Loki and decodes a successful response into v.
func (q *QueryLogs) getJSON(ctx context.Context, path string, v *lokiResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build loki request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("query loki: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLokiResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read loki response: %w", err)
	}
	if len(body) > maxLokiResponseBytes {
		return &Error{Code: ErrOutputTooLarge, Message: "loki response too large; narrow the search"}
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{
			Code:    ErrExecutionFailed,
			Message: fmt.Sprintf("loki returned HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), maxLokiErrorLength)),
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode loki response: %w", err)
	}
	if v.Status != "success" {
		return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("loki returned status %q", v.Status)}
	}
	return nil
}

func (q *QueryLogs) names() []string { return sortedNames(q.services) }

// lokiResponse is a Loki query_range response for a log query.
type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}
