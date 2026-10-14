package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeBackend (Tempo or Loki) records the last request and replies with a fixed status and body.
type fakeBackend struct {
	status int
	body   string
	delay  time.Duration
	path   string
	query  url.Values
	calls  int
}

func (f *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	f.path, f.query = r.URL.Path, r.URL.Query()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

func newQueryTraces(t *testing.T, fake *fakeBackend) *QueryTraces {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	q, err := NewQueryTraces(srv.URL+"/", []string{"checkout-api", "inventory-api"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return fixedNow }
	return q
}

func callQT(t *testing.T, q *QueryTraces, input string) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return q.Call(ctx, json.RawMessage(input))
}

// searchBody mirrors a Tempo 3.1 /api/search response.
const searchBody = `{"traces":[
 {"traceID":"aaaa5fe42c2d085f0a74cf69ed417e7e","rootServiceName":"checkout-api","rootTraceName":"GET /checkout",
  "startTimeUnixNano":"1790964000000000000","durationMs":302,
  "spanSet":{"spans":[
   {"spanID":"9a7138dca149a4d3","name":"GET /inventory","startTimeUnixNano":"1790964000000100000","durationNanos":"300842281",
    "attributes":[{"key":"http.response.status_code","value":{"intValue":"200"}},
                  {"key":"service.name","value":{"stringValue":"inventory-api"}},
                  {"key":"status","value":{"stringValue":"unset"}}]}],"matched":4}},
 {"traceID":"bbb5fe42c2d085f0a74cf69ed417e7e","rootServiceName":"ai-sre-agent","rootTraceName":"investigation",
  "startTimeUnixNano":"1790964060000000000","durationMs":5000,
  "spanSet":{"spans":[
   {"spanID":"1a7138dca149a4d3","name":"GET /healthz","startTimeUnixNano":"1790964060000100000","durationNanos":"1500000",
    "attributes":[{"key":"service.name","value":{"stringValue":"inventory-api"}},
                  {"key":"status","value":{"stringValue":"error"}}]},
   {"spanID":"2a7138dca149a4d3","name":"tool.query_traces","startTimeUnixNano":"1790964060000100000","durationNanos":"1500000",
    "attributes":[{"key":"service.name","value":{"stringValue":"ai-sre-agent"}}]}],"matched":1}}
],"metrics":{"inspectedBytes":"22696"}}`

func TestQueryTracesSearch(t *testing.T) {
	fake := &fakeBackend{status: 200, body: searchBody}
	q := newQueryTraces(t, fake)

	out, err := callQT(t, q, `{"service":"inventory-api","min_duration_ms":200,"status":"ok","lookback_minutes":5,"limit":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.path != "/api/search" {
		t.Errorf("path = %q", fake.path)
	}
	wantQ := `{ resource.service.name = "inventory-api" && duration >= 200ms && status != error } | select(name, status, span.http.response.status_code)`
	if got := fake.query.Get("q"); got != wantQ {
		t.Errorf("q = %s\nwant %s", got, wantQ)
	}
	if fake.query.Get("start") != "1790963700" || fake.query.Get("end") != "1790964001" ||
		fake.query.Get("limit") != "3" || fake.query.Get("spss") != "3" {
		t.Errorf("query params = %v", fake.query)
	}

	res := out.(TraceSearchResult)
	if len(res.Traces) != 2 {
		t.Fatalf("got %d traces, want 2", len(res.Traces))
	}
	// Newest first.
	agentRooted, first := res.Traces[0], res.Traces[1]
	if first.TraceID != "aaaa5fe42c2d085f0a74cf69ed417e7e" || first.RootService != "checkout-api" ||
		first.RootSpan != "GET /checkout" || first.Start != "2026-10-02T18:00:00.000Z" ||
		first.DurationMS != 302 || first.MatchedSpans != 4 {
		t.Errorf("summary = %+v", first)
	}
	want := MatchedSpan{Service: "inventory-api", Name: "GET /inventory", DurationMS: 300.8, Status: "unset", HTTPStatusCode: "200"}
	if len(first.Spans) != 1 || first.Spans[0] != want {
		t.Errorf("spans = %+v, want [%+v]", first.Spans, want)
	}

	// Tempo drops leading zeros from search trace IDs; they are restored.
	if agentRooted.TraceID != "0bbb5fe42c2d085f0a74cf69ed417e7e" {
		t.Errorf("trace_id = %q, want it zero-padded to 32 characters", agentRooted.TraceID)
	}
	// A root from outside the catalog is hidden, and so are its spans.
	if agentRooted.RootService != "" || agentRooted.RootSpan != "" {
		t.Errorf("non-catalog root leaked: %+v", agentRooted)
	}
	if len(agentRooted.Spans) != 1 || agentRooted.Spans[0].Service != "inventory-api" || agentRooted.Spans[0].Status != "error" {
		t.Errorf("spans = %+v", agentRooted.Spans)
	}
	b, _ := json.Marshal(res)
	for _, leaked := range []string{"ai-sre-agent", "investigation", "tool.query_traces"} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("output contains %q: %s", leaked, b)
		}
	}
}

func TestQueryTracesSearchDefaultsAndFilters(t *testing.T) {
	tests := []struct {
		input, wantQ, wantLimit, wantStart string
	}{
		{`{"service":"checkout-api"}`,
			`{ resource.service.name = "checkout-api" } | select(name, status, span.http.response.status_code)`, "10", "1790963100"},
		{`{"service":"checkout-api","status":"error","lookback_minutes":60,"limit":20}`,
			`{ resource.service.name = "checkout-api" && status = error } | select(name, status, span.http.response.status_code)`, "20", "1790960400"},
	}
	for _, tt := range tests {
		fake := &fakeBackend{status: 200, body: `{"traces":[]}`}
		q := newQueryTraces(t, fake)
		out, err := callQT(t, q, tt.input)
		if err != nil {
			t.Fatalf("%s: %v", tt.input, err)
		}
		if got := fake.query.Get("q"); got != tt.wantQ {
			t.Errorf("%s: q = %s, want %s", tt.input, got, tt.wantQ)
		}
		if fake.query.Get("limit") != tt.wantLimit || fake.query.Get("start") != tt.wantStart {
			t.Errorf("%s: params = %v", tt.input, fake.query)
		}
		// An empty result is an empty list, not null.
		if b, _ := json.Marshal(out); string(b) != `{"traces":[]}` {
			t.Errorf("%s: output = %s", tt.input, b)
		}
	}
}

// traceBody mirrors a Tempo 3.1 /api/v2/traces/<id> response, with one
// resource that is not in the catalog.
const traceBody = `{"trace":{"resourceSpans":[
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"inventory-api"}},
                            {"key":"service.version","value":{"stringValue":"dev"}}]},
  "scopeSpans":[{"spans":[
   {"traceId":"EhNf5CwtCF8KdM9p7UF+fg==","spanId":"vWDw5vMMSi8=","parentSpanId":"M+s6bnbw5XE=","name":"GET /inventory",
    "kind":"SPAN_KIND_SERVER","startTimeUnixNano":"1790964000000500000","endTimeUnixNano":"1790964000301000000",
    "attributes":[{"key":"http.request.method","value":{"stringValue":"GET"}},
                  {"key":"http.response.status_code","value":{"intValue":"200"}},
                  {"key":"network.peer.address","value":{"stringValue":"172.18.0.5"}},
                  {"key":"user_agent.original","value":{"stringValue":"Go-http-client/1.1"}}],
    "status":{}}]}]},
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout-api"}}]},
  "scopeSpans":[{"spans":[
   {"spanId":"nMdQ1gklMFA=","name":"GET /checkout","kind":"SPAN_KIND_SERVER",
    "startTimeUnixNano":"1790964000000000000","endTimeUnixNano":"1790964000302000000",
    "attributes":[{"key":"http.response.status_code","value":{"intValue":"502"}}],
    "status":{"code":"STATUS_CODE_ERROR","message":"inventory lookup failed"}},
   {"spanId":"M+s6bnbw5XE=","parentSpanId":"nMdQ1gklMFA=","name":"HTTP GET","kind":"SPAN_KIND_CLIENT",
    "startTimeUnixNano":"1790964000000100000","endTimeUnixNano":"1790964000301400000",
    "attributes":[{"key":"server.address","value":{"stringValue":"inventory-api"}}],
    "status":{}}]}]},
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"ai-sre-agent"}}]},
  "scopeSpans":[{"spans":[
   {"spanId":"AAAAAAAAAAE=","name":"llm.request","kind":"SPAN_KIND_INTERNAL",
    "startTimeUnixNano":"1790963999000000000","endTimeUnixNano":"1790964001000000000","status":{}}]}]}
]},"metrics":{}}`

func TestQueryTracesGet(t *testing.T) {
	fake := &fakeBackend{status: 200, body: traceBody}
	q := newQueryTraces(t, fake)

	out, err := callQT(t, q, `{"trace_id":"12135fe42c2d085f0a74cf69ed417e7e"}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.path != "/api/v2/traces/12135fe42c2d085f0a74cf69ed417e7e" {
		t.Errorf("path = %q", fake.path)
	}
	tr := out.(Trace)
	if tr.TraceID != "12135fe42c2d085f0a74cf69ed417e7e" || tr.DurationMS != 302 || tr.TotalSpans != 3 || tr.Truncated {
		t.Errorf("trace = %+v", tr)
	}
	want := []TraceSpan{
		{SpanID: "9cc750d609253050", Service: "checkout-api", Name: "GET /checkout", Kind: "server",
			DurationMS: 302, Status: "error", StatusMessage: "inventory lookup failed",
			Attributes: map[string]string{"http.response.status_code": "502"}},
		{SpanID: "33eb3a6e76f0e571", ParentSpanID: "9cc750d609253050", Service: "checkout-api", Name: "HTTP GET", Kind: "client",
			StartOffsetMS: 0.1, DurationMS: 301.3, Status: "unset",
			Attributes: map[string]string{"server.address": "inventory-api"}},
		{SpanID: "bd60f0e6f30c4a2f", ParentSpanID: "33eb3a6e76f0e571", Service: "inventory-api", Name: "GET /inventory", Kind: "server",
			StartOffsetMS: 0.5, DurationMS: 300.5, Status: "unset",
			Attributes: map[string]string{"http.request.method": "GET", "http.response.status_code": "200"}},
	}
	got, _ := json.Marshal(tr.Spans)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Errorf("spans =\n%s\nwant\n%s", got, wantJSON)
	}
	b, _ := json.Marshal(tr)
	for _, leaked := range []string{"ai-sre-agent", "llm.request", "172.18.0.5", "Go-http-client", "service.version"} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("output contains %q", leaked)
		}
	}
}

func TestQueryTracesGetTruncates(t *testing.T) {
	var spans []string
	for i := range maxTraceSpans + 5 {
		spans = append(spans, `{"spanId":"`+strings.Repeat("0", 14)+string(rune('a'+i/26))+string(rune('a'+i%26))+`","name":"s","kind":"SPAN_KIND_INTERNAL",
			"startTimeUnixNano":"1790964000000000000","endTimeUnixNano":"1790964000001000000","status":{}}`)
	}
	body := `{"trace":{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout-api"}}]},
		"scopeSpans":[{"spans":[` + strings.Join(spans, ",") + `]}]}]}}`
	q := newQueryTraces(t, &fakeBackend{status: 200, body: body})

	out, err := callQT(t, q, `{"trace_id":"12135fe42c2d085f0a74cf69ed417e7e"}`)
	if err != nil {
		t.Fatal(err)
	}
	tr := out.(Trace)
	if len(tr.Spans) != maxTraceSpans || tr.TotalSpans != maxTraceSpans+5 || !tr.Truncated {
		t.Errorf("got %d spans, total %d, truncated %v", len(tr.Spans), tr.TotalSpans, tr.Truncated)
	}
}

func TestQueryTracesGetNotFound(t *testing.T) {
	onlyAgent := `{"trace":{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"ai-sre-agent"}}]},
		"scopeSpans":[{"spans":[{"spanId":"AAAAAAAAAAE=","name":"investigation","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}}`
	for name, fake := range map[string]*fakeBackend{
		"empty trace":      {status: 200, body: `{"trace":{},"metrics":{}}`},
		"404":              {status: 404, body: `trace not found`},
		"only agent spans": {status: 200, body: onlyAgent},
	} {
		q := newQueryTraces(t, fake)
		out, err := callQT(t, q, `{"trace_id":"12135fe42c2d085f0a74cf69ed417e7e"}`)
		assertToolError(t, err, ErrInvalidInput)
		if b, _ := json.Marshal(out); strings.Contains(string(b), "investigation") {
			t.Errorf("%s: output leaked agent span: %s", name, b)
		}
		if err != nil && strings.Contains(err.Error(), "ai-sre-agent") {
			t.Errorf("%s: error leaked agent service: %v", name, err)
		}
	}
}

func TestQueryTracesInvalidInput(t *testing.T) {
	inputs := []string{
		`{}`,
		`{"service":"payments-api"}`,
		`{"service":"checkout-api\" } || { true"}`,
		`{"service":"checkout-api","min_duration_ms":-1}`,
		`{"service":"checkout-api","min_duration_ms":600001}`,
		`{"service":"checkout-api","status":"unset"}`,
		`{"service":"checkout-api","lookback_minutes":61}`,
		`{"service":"checkout-api","limit":21}`,
		`{"service":"checkout-api","limit":-1}`,
		`{"trace_id":"12135FE42C2D085F0A74CF69ED417E7E"}`,
		`{"trace_id":"12135fe42c2d085f"}`,
		`{"trace_id":"00000000000000000000000000000000"}`,
		`{"trace_id":"../../api/echo/12135fe42c2d085f0a7"}`,
		`{"trace_id":"12135fe42c2d085f0a74cf69ed417e7e","service":"checkout-api"}`,
		`{"trace_id":"12135fe42c2d085f0a74cf69ed417e7e","limit":5}`,
		`{"service":"checkout-api","query":"{}"}`,
		`[]`,
	}
	for _, in := range inputs {
		fake := &fakeBackend{status: 200, body: `{"traces":[]}`}
		q := newQueryTraces(t, fake)
		_, err := callQT(t, q, in)
		assertToolError(t, err, ErrInvalidInput)
		if fake.calls != 0 {
			t.Errorf("%s: invalid input reached Tempo", in)
		}
	}
}

func TestQueryTracesTempoErrors(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeBackend
		want ErrorCode
	}{
		{"server error", &fakeBackend{status: 500, body: "internal error"}, ErrExecutionFailed},
		{"bad request", &fakeBackend{status: 400, body: "invalid TraceQL query"}, ErrExecutionFailed},
		{"too large", &fakeBackend{status: 200, body: `{"traces":[],"pad":"` + strings.Repeat("x", maxTempoResponseBytes) + `"}`}, ErrOutputTooLarge},
	}
	for _, tt := range tests {
		q := newQueryTraces(t, tt.fake)
		_, err := callQT(t, q, `{"service":"checkout-api"}`)
		assertToolError(t, err, tt.want)
	}

	// A non-JSON body is a plain error; the registry reports it as execution_failed.
	q := newQueryTraces(t, &fakeBackend{status: 200, body: "<html>"})
	if _, err := callQT(t, q, `{"service":"checkout-api"}`); err == nil {
		t.Error("expected an error for a non-JSON response")
	}
}

func TestQueryTracesTimeout(t *testing.T) {
	q := newQueryTraces(t, &fakeBackend{status: 200, body: `{"traces":[]}`, delay: time.Second})
	r := newRegistry(t, Options{CallTimeout: 50 * time.Millisecond}, q)
	res := r.Call(context.Background(), "query_traces", json.RawMessage(`{"service":"checkout-api"}`))
	if res.Err == nil || res.Err.Code != ErrTimeout {
		t.Errorf("err = %v, want timeout", res.Err)
	}
}

func TestQueryTracesSchemaListsCatalog(t *testing.T) {
	q := newQueryTraces(t, &fakeBackend{})
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
}

func TestNewQueryTracesValidates(t *testing.T) {
	for _, tc := range []struct {
		url      string
		services []string
	}{
		{"127.0.0.1:3200", []string{"a"}},
		{"ftp://tempo", []string{"a"}},
		{"http://tempo", nil},
		{"http://tempo", []string{"a", "a"}},
		{"http://tempo", []string{`a" || true`}},
		{"http://tempo", []string{"Checkout"}},
	} {
		if _, err := NewQueryTraces(tc.url, tc.services, nil); err == nil {
			t.Errorf("NewQueryTraces(%q, %q) succeeded, want error", tc.url, tc.services)
		}
	}
}

func TestOTLPID(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"vWDw5vMMSi8=":     "bd60f0e6f30c4a2f",
		"bd60f0e6f30c4a2f": "bd60f0e6f30c4a2f",
	} {
		if got := otlpID(in); got != want {
			t.Errorf("otlpID(%q) = %q, want %q", in, got, want)
		}
	}
}
