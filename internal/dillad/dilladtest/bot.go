package dilladtest

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/dillad"
)

// BotHandler is the supervised Bot's Unix socket surface over the current server, stable across Restore like Handler:
// dillad.BotSocketHandler, the same allow-list dillad serves (/gateway, the Bot's upgrade, and the Bot's pre-session
// HTTP routes, answered by Handler so the host records them as it did over HTTP). Serve it behind
// gateway.BotPeerListener with gateway.BotConnContext.
func (h *Host) BotHandler() http.Handler {
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.Server().Gateway().BotHandler().ServeHTTP(w, r) })
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.Handler().ServeHTTP(w, r) })
	return dillad.BotSocketHandler(gw, app, h.Log())
}

// BotRuntimeInstallations serves the runtime credential's GET /v1/bot-runtime/installations as the Bot's socket does,
// for the specs on the host's private control listener (GET /debug/bot/installations, with the runtime credential as
// the bearer). Under a supervisor the public listener refuses the route (Bot v2 task 14 fix round 1, M-1).
func (h *Host) BotRuntimeInstallations() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.URL.Path, req.URL.RawPath, req.RequestURI = "/v1/bot-runtime/installations", "", ""
		h.BotHandler().ServeHTTP(w, req)
	})
}

// BotRunner runs a Bot supervisor on demand for the e2e host (dilla-testhost -bot): the specs start, stop and restart
// the Bot through BotControl on the host's private control listener. dillad itself has no such switch: serve runs
// its supervisor for its whole life, and this package never ships (TestTheReleaseBinaryDoesNotLinkTheTestHelpers).
type BotRunner struct {
	sup    *botsupervisor.Supervisor
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewBotRunner wraps sup; nothing runs until Start.
func NewBotRunner(sup *botsupervisor.Supervisor) *BotRunner { return &BotRunner{sup: sup} }

// Start runs the supervisor, which starts the Bot. Starting a running Bot does nothing.
func (r *BotRunner) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	go func() {
		defer close(done)
		_ = r.sup.Run(ctx)
	}()
}

// Stop stops the Bot (SIGTERM, then SIGKILL after botsupervisor.StopGrace) and waits for it.
func (r *BotRunner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel, r.done = nil, nil
}

// BotControl is the control listener's /debug/bot/ routes: POST start, stop and restart (each answers 204 once done;
// restart is a stop and a start) and GET status (JSON).
func BotControl(r *BotRunner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /debug/bot/start", func(w http.ResponseWriter, _ *http.Request) {
		r.Start()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/bot/stop", func(w http.ResponseWriter, _ *http.Request) {
		r.Stop()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /debug/bot/restart", func(w http.ResponseWriter, _ *http.Request) {
		r.Stop()
		r.Start()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /debug/bot/status", func(w http.ResponseWriter, _ *http.Request) {
		st := r.sup.Status()
		var since int64
		if !st.Since.IsZero() {
			since = st.Since.UnixMilli()
		}
		writeJSON(w, map[string]any{
			"running": st.Running, "pid": st.PID, "restarts": st.Restarts, "lastExit": st.LastExit,
			"sinceUnixMillis": since, "now": time.Now().UnixMilli(),
		})
	})
	return mux
}
