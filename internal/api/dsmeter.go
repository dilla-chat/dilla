package api

import (
	"net/http"
	"strings"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// The delivery service's rate classes, one per `[limits.rate]` pair. Every bucket is keyed by the
// uploading DEVICE session — protocol/01's "rate limits bind to the uploading device session" — so
// one device cannot spend another's allowance, and an account with several devices has one
// allowance per device.
const (
	dsClassMessage  = "message"
	dsClassCommit   = "commit"
	dsClassProposal = "proposal"
	dsClassRead     = "read"
	dsClassWrite    = "write"
	// dsClassKeyPackage meters GET /v1/devices/{id}/keypackage per (requester, target) pair at
	// the proposal rate: every fetched KeyPackage is consumed, so a requester that fetched without
	// bound would exhaust the target's directory (32 + 1 last resort) and leave it unaddable. The
	// requester's read bucket still applies on top.
	dsClassKeyPackage = "keypackage"
	// dsClassBotCommand meters POST /v1/communities/{community}/bot/commands per device at the write
	// rate (2/s, burst 20 by default) in its own bucket (Bot v2 Task 10): one device cannot flood the
	// Bot, and commands do not spend the device's write allowance for everything else.
	dsClassBotCommand = "bot_command"
	// dsClassBotRuntime meters the Bot's replies and state hints per Bot device at the read rate
	// (10/s, burst 60 by default): the one Bot answers the whole community, so its allowance is the
	// larger one. Every reply answers a command that was itself metered per device.
	dsClassBotRuntime = "bot_runtime"
)

func dsRateClass(r config.Rate, name string) server.Class {
	switch name {
	case dsClassMessage:
		return server.Class{Name: name, PerSecond: r.MessagePerSecond, Burst: r.MessageBurst}
	case dsClassCommit:
		return server.Class{Name: name, PerSecond: r.CommitPerSecond, Burst: r.CommitBurst}
	case dsClassProposal:
		return server.Class{Name: name, PerSecond: r.ProposalPerSecond, Burst: r.ProposalBurst}
	case dsClassKeyPackage:
		return server.Class{Name: name, PerSecond: r.ProposalPerSecond, Burst: r.ProposalBurst}
	case dsClassWrite, dsClassBotCommand:
		return server.Class{Name: name, PerSecond: r.WritePerSecond, Burst: r.WriteBurst}
	case dsClassBotRuntime:
		return server.Class{Name: name, PerSecond: r.ReadPerSecond, Burst: r.ReadBurst}
	default:
		return server.Class{Name: dsClassRead, PerSecond: r.ReadPerSecond, Burst: r.ReadBurst}
	}
}

// dsMeter takes one token from the session's bucket of `class` before f runs and answers
// 429 E_RATE_LIMITED with retry_after_ms when there is none. It runs INSIDE the session
// middleware, which is what puts the device on the context. A nil limiter meters nothing: the
// handler-level tests build Groups and Messages without one, and the composition root always
// passes the instance's.
func dsMeter(l *server.RateLimiter, class string, f http.HandlerFunc) http.HandlerFunc {
	if l == nil {
		return f
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s, ok := auth.FromContext(r.Context()); ok {
			if err := allowDS(l, class, s.DeviceID.String()); err != nil {
				server.WriteError(w, err)
				return
			}
		}
		f(w, r)
	}
}

// SessionRoute is what the composition root puts in front of every route a Plan 2 handler group
// registers bare (server.Mux.Wrapped): the enrolled-session middleware, then one token from the
// device session's bucket of the class the route's pattern names (routeClass). The buckets are the
// delivery service's own, keyed the same way, so one device has one read allowance and one write
// allowance across both plans' routes rather than one per route group.
func SessionRoute(sessions *auth.Sessions, l *server.RateLimiter) func(pattern string, h http.Handler) http.Handler {
	return func(pattern string, h http.Handler) http.Handler {
		return sessions.Middleware(dsMeter(l, routeClass(pattern), h.ServeHTTP), auth.ScopeEnrolled)
	}
}

// routeClass is the [limits.rate] class of a Plan 2 route: read for GET and HEAD, message for the
// readable upload (POST /v1/channels/{id}/messages is the plaintext twin of the ciphertext upload),
// write for every other change.
func routeClass(pattern string) string {
	method, path, _ := strings.Cut(pattern, " ")
	switch {
	case method == http.MethodGet || method == http.MethodHead:
		return dsClassRead
	case method == http.MethodPost && path == "/v1/channels/{id}/messages":
		return dsClassMessage
	default:
		return dsClassWrite
	}
}

// allowDS spends one token of (class, key) or returns the refusal.
func allowDS(l *server.RateLimiter, class, key string) error {
	if ok, wait := l.Allow(dsRateClass(l.Config(), class), key); !ok {
		return server.RateLimitedAfter(wait)
	}
	return nil
}

// keyPackageBucket is the (requester, target) key of dsClassKeyPackage.
func keyPackageBucket(requester, target id.ID) string {
	return requester.String() + "\x00" + target.String()
}
