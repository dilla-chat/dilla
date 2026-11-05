package api

import (
	"net/http"
	"slices"
)

// The request headers the client sets (packages/client-core/src/http/client.ts) and the response
// headers it reads beyond the CORS-safelisted ones: X-Dilla-Generation on every response and
// Retry-After on a refusal. Content-Length is safelisted.
const (
	corsAllowMethods  = "GET, POST, PUT, PATCH, DELETE"
	corsAllowHeaders  = "Authorization, Content-Type, Accept"
	corsExposeHeaders = "X-Dilla-Generation, Retry-After"
	corsMaxAge        = "600"
)

// CORS lets the listed origins (http.mobile_origins: the Capacitor app at capacitor://localhost
// and https://localhost) call the wrapped routes cross-origin. An origin is compared whole —
// scheme, host and port — so http://localhost or https://localhost:8443 is not
// https://localhost.
//
// The app authenticates with a bearer token and never a cookie, so Access-Control-Allow-Credentials
// is never sent. CORS is advisory, not access control: a request from an unlisted origin, or with
// no Origin at all, gets no Access-Control-Allow-* header and still reaches the handler, where the
// session check is what refuses it. A preflight from a listed origin is answered here with 204 and
// never reaches the handler.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := slices.Clone(origins)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			if !slices.Contains(allowed, origin) {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", corsAllowMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", corsMaxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
			next.ServeHTTP(w, r)
		})
	}
}
