package obs

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

// Gate is one readiness condition. A gate starts red: an instance is not ready
// until something has actively said it is.
type Gate struct {
	mu     sync.Mutex
	ok     bool
	detail string
}

func (g *Gate) Set(ok bool, detail string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ok, g.detail = ok, detail
}

func (g *Gate) state() (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ok, g.detail
}

// Health separates the two questions a supervisor asks. /healthz is liveness:
// "should this process be restarted?" — it fails only when a background worker
// has stopped making progress, because restarting on a dependency outage turns
// one outage into a restart loop. /readyz is readiness: "should traffic go
// here?" — it fails while any gate is red or the instance is draining.
type Health struct {
	clk clock.Clock

	mu       sync.Mutex
	gates    map[string]*Gate
	workers  map[string]time.Time
	draining bool
}

// GateNames is the fixed set. They are created red by NewHealth, so an
// instance that forgets to flip one is unready rather than quietly serving.
var GateNames = []string{"db", "schema", "wasi", "livekit", "tls", "heal"}

func NewHealth(clk clock.Clock) *Health {
	h := &Health{clk: clk, gates: map[string]*Gate{}, workers: map[string]time.Time{}}
	for _, name := range GateNames {
		h.gates[name] = &Gate{}
	}
	return h
}

// Gate returns the named gate, creating it red on first use. The six names are
// db, schema, wasi, livekit, tls and heal.
func (h *Health) Gate(name string) *Gate {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.gates[name]
	if !ok {
		g = &Gate{}
		h.gates[name] = g
	}
	return g
}

// MarkWorker records that a background worker is alive until deadline.
func (h *Health) MarkWorker(name string, deadline time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workers[name] = deadline
}

// Drain makes /readyz unready without touching /healthz, so a load balancer
// stops sending work while the process finishes what it has.
func (h *Health) Drain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
}

func (h *Health) Liveness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := h.clk.Now()
		h.mu.Lock()
		var stalled []string
		for name, deadline := range h.workers {
			if now.After(deadline) {
				stalled = append(stalled, name)
			}
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if len(stalled) > 0 {
			sort.Strings(stalled)
			w.WriteHeader(http.StatusInternalServerError)
			for _, name := range stalled {
				_, _ = w.Write([]byte("stalled: " + name + "\n"))
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}

func (h *Health) Readiness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		draining := h.draining
		names := make([]string, 0, len(h.gates))
		for name := range h.gates {
			names = append(names, name)
		}
		gates := make(map[string]*Gate, len(h.gates))
		for name, g := range h.gates {
			gates[name] = g
		}
		h.mu.Unlock()

		sort.Strings(names)
		failed := map[string]string{}
		for _, name := range names {
			if ok, detail := gates[name].state(); !ok {
				failed[name] = detail
			}
		}
		body := map[string]any{"status": "ready"}
		status := http.StatusOK
		switch {
		case draining:
			body["status"] = "draining"
			status = http.StatusServiceUnavailable
		case len(failed) > 0:
			// Only the FAILED gates are named. A green gate's name would tell an
			// unauthenticated caller which subsystems this instance runs.
			body["status"] = "unready"
			body["failed"] = failed
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}
