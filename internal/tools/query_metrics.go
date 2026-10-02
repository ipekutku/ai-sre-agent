package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxQueryLength     = 1000
	maxRangeMinutes    = 60
	maxSeries          = 20
	maxPointsPerSeries = 30
	// minStep matches the Prometheus scrape interval; finer steps add no information.
	minStep = 5 * time.Second
	// maxPrometheusResponseBytes bounds how much of a Prometheus response is read.
	maxPrometheusResponseBytes = 2 << 20
)

// QueryMetrics is the query_metrics tool: read-only PromQL queries against
// a single, fixed Prometheus server. Only the query and query_range HTTP
// API endpoints are used.
type QueryMetrics struct {
	baseURL    string
	httpClient *http.Client
	now        func() time.Time
}

// NewQueryMetrics returns a query_metrics tool for the Prometheus server at baseURL.
func NewQueryMetrics(baseURL string, httpClient *http.Client) (*QueryMetrics, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("prometheus URL must be an absolute http(s) URL, got %q", baseURL)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &QueryMetrics{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient, now: time.Now}, nil
}

func (*QueryMetrics) Name() string { return "query_metrics" }

func (*QueryMetrics) Description() string {
	return `Run a read-only PromQL query against Prometheus.

Without range_minutes, runs an instant query at the current time. With range_minutes, returns
values over the last N minutes (at most ` + strconv.Itoa(maxPointsPerSeries) + ` points per series).
Results are limited to ` + strconv.Itoa(maxSeries) + ` series; aggregate (e.g. "sum by (...)") to stay within it.

Each service is a Prometheus job (label "job"). Available metrics:
- http_server_request_duration_seconds (histogram; labels: route, method, status): requests handled by a service.
- http_client_request_duration_seconds (histogram; labels: peer, method, status): calls a service makes to a
  dependency, measured by the caller; status="error" means no response was received.
- process_cpu_seconds_total, process_resident_memory_bytes, go_goroutines and other Go runtime metrics.
- up: whether Prometheus could scrape the service.

Example: histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{job="some-service"}[1m])))`
}

func (*QueryMetrics) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "PromQL expression", "maxLength": ` + strconv.Itoa(maxQueryLength) + `},
    "range_minutes": {"type": "integer", "minimum": 1, "maximum": ` + strconv.Itoa(maxRangeMinutes) + `, "description": "If set, return values over the last N minutes instead of the current value"}
  },
  "required": ["query"],
  "additionalProperties": false
}`)
}

type queryMetricsInput struct {
	Query        string `json:"query"`
	RangeMinutes int    `json:"range_minutes"`
}

// MetricResult is the output of query_metrics.
type MetricResult struct {
	ResultType string `json:"result_type"`
	// Series holds vector and matrix results, sorted by labels.
	Series []Series `json:"series,omitempty"`
	// Scalar holds scalar and string results.
	Scalar *Sample `json:"scalar,omitempty"`
	// TotalSeries is the number of series before truncation.
	TotalSeries int      `json:"total_series"`
	Truncated   bool     `json:"truncated,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// Series is one labelled time series.
type Series struct {
	Labels map[string]string `json:"labels"`
	// Value is set for instant queries, Values for range queries.
	Value  *Sample  `json:"value,omitempty"`
	Values []Sample `json:"values,omitempty"`
}

// Sample is a value at a point in time. Value is a string so NaN and ±Inf
// survive JSON encoding.
type Sample struct {
	Time  string `json:"time"`
	Value string `json:"value"`
}

func (q *QueryMetrics) Call(ctx context.Context, raw json.RawMessage) (any, error) {
	var in queryMetricsInput
	if err := decodeInput(raw, &in); err != nil {
		return nil, err
	}
	if err := validateQuery(in.Query); err != nil {
		return nil, err
	}
	if in.RangeMinutes < 0 || in.RangeMinutes > maxRangeMinutes {
		return nil, InvalidInput("range_minutes must be between 1 and %d", maxRangeMinutes)
	}

	form := url.Values{"query": {in.Query}}
	// Ask Prometheus to stop evaluating shortly before our own deadline.
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl) - 500*time.Millisecond; remaining > 0 {
			form.Set("timeout", strconv.FormatFloat(remaining.Seconds(), 'f', 3, 64))
		}
	}

	now := q.now()
	path := "/api/v1/query"
	if in.RangeMinutes == 0 {
		form.Set("time", formatUnix(now))
	} else {
		path = "/api/v1/query_range"
		window := time.Duration(in.RangeMinutes) * time.Minute
		form.Set("start", formatUnix(now.Add(-window)))
		form.Set("end", formatUnix(now))
		form.Set("step", strconv.FormatFloat(rangeStep(window).Seconds(), 'f', -1, 64))
	}

	resp, err := q.post(ctx, path, form)
	if err != nil {
		return nil, err
	}
	res, err := convert(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("decode prometheus result: %w", err)
	}
	res.Warnings = resp.Warnings
	return res, nil
}

func validateQuery(q string) error {
	if strings.TrimSpace(q) == "" {
		return InvalidInput("query is required")
	}
	if len(q) > maxQueryLength {
		return InvalidInput("query is %d characters, limit is %d", len(q), maxQueryLength)
	}
	for _, r := range q {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return InvalidInput("query contains control characters")
		}
	}
	return nil
}

// rangeStep returns a step that keeps a range query within maxPointsPerSeries.
func rangeStep(window time.Duration) time.Duration {
	step := (window / (maxPointsPerSeries - 1)).Round(time.Second)
	if step*(maxPointsPerSeries-1) < window {
		step += time.Second
	}
	return max(step, minStep)
}

// promResponse is the Prometheus HTTP API envelope.
type promResponse struct {
	Status    string   `json:"status"`
	Data      promData `json:"data"`
	ErrorType string   `json:"errorType"`
	Error     string   `json:"error"`
	Warnings  []string `json:"warnings"`
}

type promData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func (q *QueryMetrics) post(ctx context.Context, path string, form url.Values) (promResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return promResponse{}, fmt.Errorf("build prometheus request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return promResponse{}, fmt.Errorf("query prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPrometheusResponseBytes+1))
	if err != nil {
		return promResponse{}, fmt.Errorf("read prometheus response: %w", err)
	}
	if len(body) > maxPrometheusResponseBytes {
		return promResponse{}, &Error{Code: ErrOutputTooLarge, Message: "prometheus response too large; aggregate or narrow the query"}
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return promResponse{}, fmt.Errorf("prometheus returned HTTP %d with a non-API response", resp.StatusCode)
	}
	if pr.Status != "success" {
		return promResponse{}, prometheusError(pr.ErrorType, pr.Error)
	}
	return pr, nil
}

func prometheusError(errorType, msg string) error {
	switch errorType {
	case "bad_data":
		return InvalidInput("prometheus rejected the query: %s", msg)
	case "timeout", "canceled":
		return &Error{Code: ErrTimeout, Message: "prometheus query timed out: " + msg}
	}
	return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("prometheus error (%s): %s", errorType, msg)}
}

// promSample is a Prometheus [<unix seconds>, "<value>"] pair.
type promSample [2]any

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  *promSample       `json:"value"`
	Values []promSample      `json:"values"`
}

func convert(d promData) (MetricResult, error) {
	switch d.ResultType {
	case "vector", "matrix":
		var raw []promSeries
		if err := json.Unmarshal(d.Result, &raw); err != nil {
			return MetricResult{}, err
		}
		res := MetricResult{ResultType: d.ResultType, TotalSeries: len(raw)}
		for _, s := range raw {
			out := Series{Labels: s.Metric}
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			if s.Value != nil {
				v, err := s.Value.sample()
				if err != nil {
					return MetricResult{}, err
				}
				out.Value = &v
			}
			for _, p := range s.Values {
				v, err := p.sample()
				if err != nil {
					return MetricResult{}, err
				}
				out.Values = append(out.Values, v)
			}
			res.Series = append(res.Series, out)
		}
		return truncateSeries(res), nil
	case "scalar", "string":
		var p promSample
		if err := json.Unmarshal(d.Result, &p); err != nil {
			return MetricResult{}, err
		}
		v, err := p.sample()
		if err != nil {
			return MetricResult{}, err
		}
		return MetricResult{ResultType: d.ResultType, Scalar: &v}, nil
	default:
		return MetricResult{}, fmt.Errorf("unsupported result type %q", d.ResultType)
	}
}

func (p promSample) sample() (Sample, error) {
	ts, ok := p[0].(float64)
	if !ok {
		return Sample{}, errors.New("sample timestamp is not a number")
	}
	val, ok := p[1].(string)
	if !ok {
		return Sample{}, errors.New("sample value is not a string")
	}
	if f, err := strconv.ParseFloat(val, 64); err == nil {
		val = strconv.FormatFloat(f, 'g', 6, 64)
	}
	sec, frac := math.Modf(ts)
	t := time.Unix(int64(sec), int64(frac*1e9)).UTC()
	return Sample{Time: t.Format(time.RFC3339), Value: val}, nil
}

// truncateSeries sorts series by labels (for deterministic output) and keeps
// at most maxSeries.
func truncateSeries(res MetricResult) MetricResult {
	sort.Slice(res.Series, func(i, j int) bool {
		return labelKey(res.Series[i].Labels) < labelKey(res.Series[j].Labels)
	})
	if len(res.Series) > maxSeries {
		res.Series = res.Series[:maxSeries]
		res.Truncated = true
	}
	return res
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + l[k] + ",")
	}
	return b.String()
}

func formatUnix(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}
