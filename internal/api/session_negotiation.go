package api

import (
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/server"
)

const maxNegotiationBody = 4096

func negotiationSupported(d Deps) [][]uint64 {
	pick := func(v, fallback []uint64) []uint64 {
		if len(v) == 0 {
			return fallback
		}
		return append([]uint64(nil), v...)
	}
	return [][]uint64{
		pick(d.WireVersions, []uint64{5, 4, 3, 2}),
		pick(d.E2EEVersions, []uint64{1}),
		pick(d.MediaVersions, []uint64{1}),
	}
}

// NegotiateSession selects the highest common version independently for each layer and records
// the choice on the resolved bearer session before returning it.
func (d Deps) NegotiateSession(w http.ResponseWriter, r *http.Request) {
	var body []any
	if err := server.DecodeBody(w, r, maxNegotiationBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(body) != 3 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "negotiation body must contain three version lists"))
		return
	}
	sets := make([][]uint64, 3)
	for i, raw := range body {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 || len(list) > 8 {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "each version list must contain 1 to 8 versions"))
			return
		}
		seen := make(map[uint64]struct{}, len(list))
		for _, item := range list {
			v, ok := item.(uint64)
			if !ok || v == 0 {
				server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "versions must be positive unsigned integers"))
				return
			}
			if _, exists := seen[v]; exists {
				server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "version lists must not contain duplicates"))
				return
			}
			seen[v] = struct{}{}
			sets[i] = append(sets[i], v)
		}
	}
	supported := negotiationSupported(d)
	selected := [3]uint64{}
	for i := range sets {
		for _, offered := range sets[i] {
			for _, current := range supported[i] {
				if offered == current && current > selected[i] {
					selected[i] = current
				}
			}
		}
		if selected[i] == 0 {
			server.WriteError(w, server.Version(supported[0], supported[1], supported[2]))
			return
		}
	}
	session, ok := auth.FromContext(r.Context())
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if err := d.Sessions.RecordCapabilities(r.Context(), session.TokenHash, selected[0], selected[1], selected[2]); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []uint64{selected[0], selected[1], selected[2]})
}

// BotSessionRoute preserves enrolled-session and rate-limit checks for bot-only handlers. Each
// handler applies the wire-3 requirement after authorizing its bound resource, so hidden and
// unknown resources keep their established refusal.
func BotSessionRoute(sessions *auth.Sessions, l *server.RateLimiter) func(pattern string, h http.Handler) http.Handler {
	return func(pattern string, h http.Handler) http.Handler {
		return sessions.Middleware(dsMeter(l, routeClass(pattern), h.ServeHTTP), auth.ScopeEnrolled)
	}
}
