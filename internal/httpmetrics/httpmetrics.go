// Package httpmetrics records Prometheus metrics for inbound HTTP requests and
// outbound calls to dependencies.
//
// The metric model separates a service's own latency from the latency of its
// dependencies:
//
//	http_server_request_duration_seconds{route, method, status}
//	http_client_request_duration_seconds{peer, method, status}
//
// The service name is not a metric label; Prometheus attaches it as the
// "job" label at scrape time.
package httpmetrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry returns a registry that already includes Go runtime and process
// (CPU, memory, file descriptor) metrics.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// Handler serves GET /metrics from reg and routes every other request to app,
// instrumented with server metrics registered on reg. Scrapes of /metrics are
// not themselves recorded.
func Handler(reg *prometheus.Registry, app http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", NewServer(reg).Middleware(app))
	return mux
}

// Buckets are chosen to separate normal latency (~tens of ms) from degraded
// latency (hundreds of ms to seconds).
var Buckets = []float64{.005, .01, .025, .05, .1, .25, .5, .75, 1, 1.5, 2, 3, 5}

// unmatchedRoute is the route label for requests that matched no pattern,
// so arbitrary paths cannot create unbounded label values.
const unmatchedRoute = "unmatched"

// Server records inbound request metrics.
type Server struct {
	duration *prometheus.HistogramVec
}

// NewServer creates server metrics and registers them with reg.
func NewServer(reg prometheus.Registerer) *Server {
	s := &Server{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_server_request_duration_seconds",
			Help:    "Duration of inbound HTTP requests handled by this service.",
			Buckets: Buckets,
		}, []string{"route", "method", "status"}),
	}
	reg.MustRegister(s.duration)
	return s
}

// Middleware records the duration and status of each request served by next.
// next must be (or wrap) an *http.ServeMux so the matched route pattern is
// available after it runs.
func (s *Server) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		s.duration.WithLabelValues(route(r), r.Method, strconv.Itoa(rec.status)).
			Observe(time.Since(start).Seconds())
	})
}

// route returns the path part of the ServeMux pattern that handled r,
// e.g. "/checkout" for the pattern "GET /checkout".
func route(r *http.Request) string {
	p := r.Pattern
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return unmatchedRoute
	}
	return p
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Client records outbound request metrics.
type Client struct {
	duration *prometheus.HistogramVec
}

// NewClient creates client metrics and registers them with reg.
func NewClient(reg prometheus.Registerer) *Client {
	c := &Client{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_client_request_duration_seconds",
			Help:    "Duration of outbound HTTP requests from this service to a dependency, until response headers are received.",
			Buckets: Buckets,
		}, []string{"peer", "method", "status"}),
	}
	reg.MustRegister(c.duration)
	return c
}

// Transport wraps next so every request through it is recorded with the
// given peer label. A nil next uses http.DefaultTransport. Requests that fail
// without a response are recorded with status "error".
func (c *Client) Transport(peer string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		start := time.Now()
		resp, err := next.RoundTrip(req)
		status := "error"
		if err == nil {
			status = strconv.Itoa(resp.StatusCode)
		}
		c.duration.WithLabelValues(peer, req.Method, status).Observe(time.Since(start).Seconds())
		return resp, err
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
