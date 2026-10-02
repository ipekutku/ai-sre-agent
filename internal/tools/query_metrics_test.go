package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)

// fakePrometheus records the last request and replies with a fixed status and body.
type fakePrometheus struct {
	status int
	body   string
	path   string
	form   url.Values
	method string
}

func (f *fakePrometheus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.method, f.path = r.Method, r.URL.Path
	_ = r.ParseForm()
	f.form = r.PostForm
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

func newQueryMetrics(t *testing.T, fake *fakePrometheus) *QueryMetrics {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	q, err := NewQueryMetrics(srv.URL+"/", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return fixedNow }
	return q
}

func callQM(t *testing.T, q *QueryMetrics, input string) (MetricResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := q.Call(ctx, json.RawMessage(input))
	if err != nil {
		return MetricResult{}, err
	}
	return out.(MetricResult), nil
}

func TestQueryMetricsInstant(t *testing.T) {
	fake := &fakePrometheus{status: 200, body: `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"job":"b"},"value":[1790964000,"0.987500000001"]},
		{"metric":{"job":"a"},"value":[1790964000,"NaN"]}
	]}}`}
	q := newQueryMetrics(t, fake)

	res, err := callQM(t, q, `{"query":"up"}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.method != http.MethodPost || fake.path != "/api/v1/query" {
		t.Errorf("request = %s %s, want POST /api/v1/query", fake.method, fake.path)
	}
	if fake.form.Get("query") != "up" || fake.form.Get("time") != fmt.Sprint(fixedNow.Unix())+".000" {
		t.Errorf("form = %v", fake.form)
	}
	if fake.form.Get("timeout") == "" {
		t.Error("expected a Prometheus-side timeout derived from the context deadline")
	}

	if res.ResultType != "vector" || res.TotalSeries != 2 || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	// Sorted by labels; values rounded to 6 significant digits; NaN preserved.
	if res.Series[0].Labels["job"] != "a" || res.Series[0].Value.Value != "NaN" {
		t.Errorf("series[0] = %+v", res.Series[0])
	}
	if res.Series[1].Value.Value != "0.9875" || res.Series[1].Value.Time != "2026-10-02T18:00:00Z" {
		t.Errorf("series[1] value = %+v", res.Series[1].Value)
	}
}

func TestQueryMetricsRange(t *testing.T) {
	fake := &fakePrometheus{status: 200, body: `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"job":"a"},"values":[[1790963700,"0.02"],[1790964000,"0.99"]]}
	]}}`}
	q := newQueryMetrics(t, fake)

	res, err := callQM(t, q, `{"query":"rate(x[1m])","range_minutes":5}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.path != "/api/v1/query_range" {
		t.Errorf("path = %s, want /api/v1/query_range", fake.path)
	}
	wantStart := fmt.Sprint(fixedNow.Add(-5*time.Minute).Unix()) + ".000"
	if fake.form.Get("start") != wantStart || fake.form.Get("step") != "11" {
		t.Errorf("form = %v, want start %s and step 11", fake.form, wantStart)
	}
	if res.ResultType != "matrix" || len(res.Series) != 1 || len(res.Series[0].Values) != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRangeStepBoundsPoints(t *testing.T) {
	for m := 1; m <= maxRangeMinutes; m++ {
		window := time.Duration(m) * time.Minute
		step := rangeStep(window)
		if step < minStep {
			t.Errorf("%dm: step %v below scrape interval", m, step)
		}
		if points := int(window/step) + 1; points > maxPointsPerSeries {
			t.Errorf("%dm: step %v yields %d points, limit %d", m, step, points, maxPointsPerSeries)
		}
	}
}

func TestQueryMetricsScalar(t *testing.T) {
	q := newQueryMetrics(t, &fakePrometheus{status: 200, body: `{"status":"success","data":{"resultType":"scalar","result":[1790964000,"42"]}}`})
	res, err := callQM(t, q, `{"query":"42"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultType != "scalar" || res.Scalar == nil || res.Scalar.Value != "42" {
		t.Errorf("result = %+v", res)
	}
}

func TestQueryMetricsTruncatesSeries(t *testing.T) {
	var series []string
	for i := range maxSeries + 5 {
		series = append(series, fmt.Sprintf(`{"metric":{"i":"%03d"},"value":[1790964000,"1"]}`, i))
	}
	q := newQueryMetrics(t, &fakePrometheus{status: 200,
		body: `{"status":"success","warnings":["w1"],"data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`})

	res, err := callQM(t, q, `{"query":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != maxSeries || !res.Truncated || res.TotalSeries != maxSeries+5 {
		t.Errorf("got %d series (total %d, truncated %v), want %d of %d truncated",
			len(res.Series), res.TotalSeries, res.Truncated, maxSeries, maxSeries+5)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "w1" {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestQueryMetricsInvalidInput(t *testing.T) {
	fake := &fakePrometheus{status: 200, body: `{}`}
	q := newQueryMetrics(t, fake)

	for _, in := range []string{
		`{}`,
		`{"query":"   "}`,
		`{"query":"` + strings.Repeat("a", maxQueryLength+1) + `"}`,
		`{"query":"up\u0000"}`,
		`{"query":"up","range_minutes":61}`,
		`{"query":"up","range_minutes":-1}`,
		`{"query":"up","url":"http://elsewhere"}`,
	} {
		_, err := callQM(t, q, in)
		assertToolError(t, err, ErrInvalidInput)
	}
	if fake.path != "" {
		t.Errorf("invalid input reached Prometheus (%s)", fake.path)
	}
}

func TestQueryMetricsPrometheusErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   ErrorCode
	}{
		{"bad query", 400, `{"status":"error","errorType":"bad_data","error":"parse error"}`, ErrInvalidInput},
		{"timeout", 503, `{"status":"error","errorType":"timeout","error":"query timed out"}`, ErrTimeout},
		{"execution", 422, `{"status":"error","errorType":"execution","error":"boom"}`, ErrExecutionFailed},
		{"not the API", 502, `<html>bad gateway</html>`, ""},
		{"too large", 200, `{"status":"success","data":{"resultType":"vector","result":[]},"pad":"` +
			strings.Repeat("x", maxPrometheusResponseBytes) + `"}`, ErrOutputTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := newQueryMetrics(t, &fakePrometheus{status: tt.status, body: tt.body})
			_, err := callQM(t, q, `{"query":"up"}`)
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.want != "" {
				assertToolError(t, err, tt.want)
			}
		})
	}
}

func TestNewQueryMetricsValidatesURL(t *testing.T) {
	for _, u := range []string{"", "prometheus:9090", "file:///etc/passwd"} {
		if _, err := NewQueryMetrics(u, nil); err == nil {
			t.Errorf("NewQueryMetrics(%q) succeeded", u)
		}
	}
}

func assertToolError(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	te, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %v (%T), want *tools.Error with code %s", err, err, want)
	}
	if te.Code != want {
		t.Errorf("code = %s (%s), want %s", te.Code, te.Message, want)
	}
}
