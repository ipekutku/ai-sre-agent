package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// RequestIDHeader carries the request ID between services and back to the
// caller.
const RequestIDHeader = "X-Request-ID"

// maxRequestIDLength bounds request IDs accepted from callers.
const maxRequestIDLength = 64

type requestIDKey struct{}

// RequestID returns the request ID stored in ctx by Handler, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// validRequestID reports whether a caller-supplied request ID is safe to
// adopt and log: 1 to 64 ASCII letters, digits, '.', '_' or '-'.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID returns 16 random bytes as 32 hex characters.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never returns an error
	return hex.EncodeToString(b[:])
}

// assignRequestID gives each request not matched by skip a request ID: the
// caller's X-Request-ID if valid, otherwise a new one. The ID is stored in the
// request context and echoed in the response.
func assignRequestID(next http.Handler, skip func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

// accessLog logs one line for each request not matched by skip when it
// completes:
//
//	msg="request" method route status duration_ms request_id trace_id span_id
//
// Responses with a 5xx status are logged at error level, all others at info.
// It passes r itself to next, so it (and any wrapper outside it) can read
// the route pattern the ServeMux in next sets.
func accessLog(logger *slog.Logger, next http.Handler, skip func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		logger.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("route", route(r)),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// route returns the path part of the ServeMux pattern that handled r, e.g.
// "/checkout" for "GET /checkout", or "unmatched". It matches the route label
// of the HTTP metrics.
func route(r *http.Request) string {
	p := r.Pattern
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return "unmatched"
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

// requestIDTransport sets the X-Request-ID header from the request context.
type requestIDTransport struct {
	next http.RoundTripper
}

func (t requestIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if id := RequestID(req.Context()); id != "" {
		req = req.Clone(req.Context()) // a RoundTripper must not modify its request
		req.Header.Set(RequestIDHeader, id)
	}
	return t.next.RoundTrip(req)
}
