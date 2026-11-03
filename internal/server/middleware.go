package server

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

// Recover turns a panic in a handler into a 500 with an empty detail. Without
// it one nil dereference in one handler takes the whole instance down, gateway
// connections and live calls included.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic in handler",
						"route", r.Pattern, "method", r.Method,
						"panic", v, "stack", string(debug.Stack()))
					WriteError(w, Errorf(CodeInternal, ""))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusWriter records the status so the log line and the metric agree.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap is the documented http.ResponseController contract (Go 1.20+): without
// it a wrapped ResponseWriter hides every optional interface the real one
// implements. RequestLog wraps the WHOLE mux, so without Unwrap the gateway's
// websocket.Accept cannot reach http.Hijacker and EVERY WebSocket upgrade fails
// at run time — in part 1b, with nothing in 1a to catch it.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack delegates as well, for the callers that still type-assert directly
// rather than going through http.NewResponseController.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// RequestLog writes one line per request and feeds the HTTP metrics. The route
// logged is r.Pattern — the REGISTERED pattern, not the URL — so a metric label
// can never be driven by a client and the cardinality is bounded by the routing
// table.
func RequestLog(log *slog.Logger, clk clock.Clock, observe func(route, method string, status int, d time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := clk.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			d := clk.Since(start)
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			if observe != nil {
				observe(route, r.Method, sw.status, d)
			}
			log.Info("http",
				"method", r.Method, "route", route, "status", sw.status,
				"bytes", sw.bytes, "duration_ms", d.Milliseconds())
		})
	}
}
