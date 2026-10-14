package tools

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newQueryLogs(t *testing.T, fake *fakeBackend) *QueryLogs {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	q, err := NewQueryLogs(srv.URL+"/", []string{"checkout-api", "inventory-api"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return fixedNow }
	return q
}

func callQL(t *testing.T, q *QueryLogs, input string) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return q.Call(ctx, json.RawMessage(input))
}

// logsBody mirrors a Loki 3.7 /loki/api/v1/query_range response, where
// structured metadata is merged into the stream labels. It also contains a
// fault-admin entry and an entry from a service outside the catalog, which
// must never be returned.
const logsBody = `{"status":"success","data":{"resultType":"streams","result":[
 {"stream":{"service_name":"checkout-api","severity_text":"WARN","severity_number":"13","detected_level":"warn",
   "trace_id":"be921db99b6e37d3195de22a3fe8275c","span_id":"10ccdf4e46b4844d","request_id":"986cb3ae6a729a678fd3d06263dffe97",
   "sku":"demo-sku","exception_message":"call inventory-api: context deadline exceeded","exception_type":"*url.Error",
   "service_version":"dev","telemetry_sdk_name":"opentelemetry","scope_name":"github.com/ipekutku/ai-sre-agent",
   "observed_timestamp":"1790963990000000100","flags":"1"},
  "values":[["1790963990000000000","inventory lookup failed"]]},
 {"stream":{"service_name":"checkout-api","severity_text":"ERROR","severity_number":"17",
   "trace_id":"be921db99b6e37d3195de22a3fe8275c","request_id":"986cb3ae6a729a678fd3d06263dffe97",
   "method":"GET","route":"/checkout","status":"502","duration_ms":"2001.843"},
  "values":[["1790963990000100000","request"]]},
 {"stream":{"service_name":"inventory-api","component":"fault-admin","severity_text":"INFO","latency_ms":"3000"},
  "values":[["1790963995000000000","fault injection set"]]},
 {"stream":{"service_name":"ai-sre-agent","severity_text":"INFO"},
  "values":[["1790963996000000000","investigation started"]]},
 {"stream":{"service_name":"inventory-api","detected_level":"info","route":"/inventory","status":"200"},
  "values":[["1790963980000000000","request"],["1790963985000000000","request"]]}
]}}`

func TestQueryLogs(t *testing.T) {
	fake := &fakeBackend{status: 200, body: logsBody}
	q := newQueryLogs(t, fake)

	out, err := callQL(t, q, `{"service":"checkout-api","min_severity":"warn","contains":"lookup failed",
		"trace_id":"be921db99b6e37d3195de22a3fe8275c","request_id":"986cb3ae6a729a678fd3d06263dffe97","lookback_minutes":5,"limit":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.path != "/loki/api/v1/query_range" {
		t.Errorf("path = %q", fake.path)
	}
	wantQ := `{service_name="checkout-api"} |= "lookup failed" | component != "fault-admin" | severity_number >= 13` +
		` | trace_id = "be921db99b6e37d3195de22a3fe8275c" | request_id = "986cb3ae6a729a678fd3d06263dffe97"`
	if got := fake.query.Get("query"); got != wantQ {
		t.Errorf("query = %s\nwant    %s", got, wantQ)
	}
	if fake.query.Get("start") != "1790963700000000000" || fake.query.Get("end") != "1790964001000000000" ||
		fake.query.Get("limit") != "11" || fake.query.Get("direction") != "backward" {
		t.Errorf("query params = %v", fake.query)
	}

	res := out.(LogResult)
	want := []LogEntry{
		{Time: "2026-10-02T17:59:50.000Z", Service: "checkout-api", Severity: "error", Message: "request",
			TraceID: "be921db99b6e37d3195de22a3fe8275c", RequestID: "986cb3ae6a729a678fd3d06263dffe97",
			Attributes: map[string]string{"method": "GET", "route": "/checkout", "status": "502", "duration_ms": "2001.843"}},
		{Time: "2026-10-02T17:59:50.000Z", Service: "checkout-api", Severity: "warn", Message: "inventory lookup failed",
			TraceID: "be921db99b6e37d3195de22a3fe8275c", RequestID: "986cb3ae6a729a678fd3d06263dffe97",
			Attributes: map[string]string{"sku": "demo-sku", "exception_message": "call inventory-api: context deadline exceeded", "exception_type": "*url.Error"}},
		{Time: "2026-10-02T17:59:45.000Z", Service: "inventory-api", Severity: "info", Message: "request",
			Attributes: map[string]string{"route": "/inventory", "status": "200"}},
		{Time: "2026-10-02T17:59:40.000Z", Service: "inventory-api", Severity: "info", Message: "request",
			Attributes: map[string]string{"route": "/inventory", "status": "200"}},
	}
	got, _ := json.Marshal(res)
	wantJSON, _ := json.Marshal(LogResult{Entries: want})
	if string(got) != string(wantJSON) {
		t.Errorf("result =\n%s\nwant\n%s", got, wantJSON)
	}
	for _, leaked := range []string{"fault", "latency_ms", "3000", "ai-sre-agent", "investigation",
		"service_version", "telemetry_sdk", "scope_name", "observed_timestamp", "span_id"} {
		if strings.Contains(string(got), leaked) {
			t.Errorf("output contains %q", leaked)
		}
	}
}

func TestQueryLogsDefaults(t *testing.T) {
	for _, input := range []string{`{}`, `{"min_severity":"debug"}`} {
		fake := &fakeBackend{status: 200, body: `{"status":"success","data":{"resultType":"streams","result":[]}}`}
		q := newQueryLogs(t, fake)
		out, err := callQL(t, q, input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		// Every catalog service, and nothing else.
		wantQ := `{service_name=~"checkout-api|inventory-api"} | component != "fault-admin"`
		if got := fake.query.Get("query"); got != wantQ {
			t.Errorf("%s: query = %s, want %s", input, got, wantQ)
		}
		if fake.query.Get("limit") != "21" || fake.query.Get("start") != "1790963100000000000" {
			t.Errorf("%s: params = %v", input, fake.query)
		}
		// An empty result is an empty list, not null.
		if b, _ := json.Marshal(out); string(b) != `{"entries":[]}` {
			t.Errorf("%s: output = %s", input, b)
		}
	}
}

// The contains text is the only free-form input. Whatever it holds, it must
// end up as one string literal that LogQL reads back unchanged.
func TestQueryLogsEscapesContains(t *testing.T) {
	for _, text := range []string{
		`say "hi"`,
		"back`tick",
		`C:\path\n`,
		`.*(?i)[a-z]+|^$`,
		`"} |= "" or {service_name=~".+"`,
		"ünïcødé 日本",
	} {
		fake := &fakeBackend{status: 200, body: `{"status":"success","data":{"resultType":"streams","result":[]}}`}
		q := newQueryLogs(t, fake)
		input, _ := json.Marshal(map[string]string{"service": "inventory-api", "contains": text})
		if _, err := callQL(t, q, string(input)); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		query := fake.query.Get("query")
		prefix, suffix := `{service_name="inventory-api"} |= `, ` | component != "fault-admin"`
		if !strings.HasPrefix(query, prefix) || !strings.HasSuffix(query, suffix) {
			t.Fatalf("%q: query = %s", text, query)
		}
		lit := strings.TrimSuffix(strings.TrimPrefix(query, prefix), suffix)
		if got, err := strconv.Unquote(lit); err != nil || got != text {
			t.Errorf("%q: literal %s reads back as %q (%v)", text, lit, got, err)
		}
	}
}

func TestQueryLogsTruncates(t *testing.T) {
	q := newQueryLogs(t, &fakeBackend{status: 200, body: logsBody})
	out, err := callQL(t, q, `{"limit":2}`)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(LogResult)
	if len(res.Entries) != 2 || !res.Truncated || res.Entries[1].Message != "inventory lookup failed" {
		t.Errorf("result = %+v, want the 2 newest entries, truncated", res)
	}

	out, err = callQL(t, q, `{"limit":4}`)
	if err != nil {
		t.Fatal(err)
	}
	if res := out.(LogResult); len(res.Entries) != 4 || res.Truncated {
		t.Errorf("got %d entries, truncated %v; want 4, not truncated", len(res.Entries), res.Truncated)
	}
}

func TestQueryLogsInvalidInput(t *testing.T) {
	inputs := []string{
		`{"service":"payments-api"}`,
		`{"service":"ai-sre-agent"}`,
		`{"service":"checkout-api\"} or {service_name=~\".+"}`,
		`{"min_severity":"fatal"}`,
		`{"min_severity":"WARN"}`,
		`{"contains":"` + strings.Repeat("x", maxContainsLength+1) + `"}`,
		`{"contains":"line\nbreak"}`,
		`{"contains":"nul\u0000"}`,
		`{"trace_id":"BE921DB99B6E37D3195DE22A3FE8275C"}`,
		`{"trace_id":"be921db99b6e37d3"}`,
		`{"trace_id":"00000000000000000000000000000000"}`,
		`{"request_id":"a b"}`,
		`{"request_id":"a\"b"}`,
		`{"request_id":"` + strings.Repeat("a", maxRequestIDLength+1) + `"}`,
		`{"lookback_minutes":61}`,
		`{"lookback_minutes":-1}`,
		`{"limit":51}`,
		`{"limit":-1}`,
		`{"query":"{service_name=~\".+\"}"}`,
		`[]`,
	}
	for _, in := range inputs {
		fake := &fakeBackend{status: 200, body: `{"status":"success","data":{"resultType":"streams","result":[]}}`}
		q := newQueryLogs(t, fake)
		_, err := callQL(t, q, in)
		assertToolError(t, err, ErrInvalidInput)
		if fake.calls != 0 {
			t.Errorf("%s: invalid input reached Loki", in)
		}
	}
}

func TestQueryLogsLokiErrors(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeBackend
		want ErrorCode
	}{
		{"server error", &fakeBackend{status: 500, body: "internal error"}, ErrExecutionFailed},
		{"parse error", &fakeBackend{status: 400, body: "parse error : unexpected IDENTIFIER"}, ErrExecutionFailed},
		{"error status", &fakeBackend{status: 200, body: `{"status":"error"}`}, ErrExecutionFailed},
		{"too large", &fakeBackend{status: 200, body: `{"status":"success","pad":"` + strings.Repeat("x", maxLokiResponseBytes) + `"}`}, ErrOutputTooLarge},
	}
	for _, tt := range tests {
		q := newQueryLogs(t, tt.fake)
		_, err := callQL(t, q, `{}`)
		assertToolError(t, err, tt.want)
	}

	// Malformed responses are plain errors; the registry reports them as execution_failed.
	for _, body := range []string{
		"<html>",
		`{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"checkout-api"},"values":[["soon","x"]]}]}}`,
	} {
		q := newQueryLogs(t, &fakeBackend{status: 200, body: body})
		if _, err := callQL(t, q, `{}`); err == nil {
			t.Errorf("expected an error for response %s", body)
		}
	}
}

func TestQueryLogsTimeout(t *testing.T) {
	q := newQueryLogs(t, &fakeBackend{status: 200, body: `{"status":"success","data":{"resultType":"streams","result":[]}}`, delay: time.Second})
	r := newRegistry(t, Options{CallTimeout: 50 * time.Millisecond}, q)
	res := r.Call(context.Background(), "query_logs", json.RawMessage(`{}`))
	if res.Err == nil || res.Err.Code != ErrTimeout {
		t.Errorf("err = %v, want timeout", res.Err)
	}
}

func TestQueryLogsSchemaListsCatalog(t *testing.T) {
	q := newQueryLogs(t, &fakeBackend{})
	var schema struct {
		Properties struct {
			Service struct {
				Enum []string `json:"enum"`
			} `json:"service"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(q.InputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if strings.Join(schema.Properties.Service.Enum, ",") != "checkout-api,inventory-api" {
		t.Errorf("service enum = %v", schema.Properties.Service.Enum)
	}
	if regexp.MustCompile(`(?i)\b(fault|inject)`).MatchString(q.Description() + string(q.InputSchema())) {
		t.Error("description or schema mentions fault injection")
	}
}

func TestNewQueryLogsValidates(t *testing.T) {
	for _, tc := range []struct {
		url      string
		services []string
	}{
		{"127.0.0.1:3100", []string{"a"}},
		{"ftp://loki", []string{"a"}},
		{"http://loki", nil},
		{"http://loki", []string{"a", "a"}},
		{"http://loki", []string{`a"} or {x=~".+`}},
	} {
		if _, err := NewQueryLogs(tc.url, tc.services, nil); err == nil {
			t.Errorf("NewQueryLogs(%q, %q) succeeded, want error", tc.url, tc.services)
		}
	}
}
