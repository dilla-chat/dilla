package desktopauth

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

//go:embed browser.js
var browserJS []byte

const page = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Dilla desktop sign-in</title><h1>Dilla desktop sign-in</h1><p>Continue only if you started this request in your Dilla desktop app.</p><p id="status">Your passkey stays in your authenticator.</p><button id="continue" type="button">Continue</button><script src="./app.js"></script></html>`

// Handler is an optional, same-origin browser ceremony surface. The caller meters
// requests before dispatch. Only opaque identifiers occur in navigation URLs.
func Handler(b *Broker, origin string, allow func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		reject := func(code int) { http.Error(w, "Desktop browser request unavailable", code) }
		if (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != origin) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			reject(403)
			return
		}
		if allow != nil && !allow(r) {
			reject(429)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/desktop-auth/")
		if path == "app.js" && r.Method == "GET" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Write(browserJS)
			return
		}
		read := func(out any) bool {
			if r.Header.Get("Content-Type") != "application/json" {
				reject(415)
				return false
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 70000))
			decoder.DisallowUnknownFields()
			if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
				reject(400)
				return false
			}
			return true
		}
		write := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
		if path == "request" && r.Method == "POST" {
			var request Request
			if !read(&request) {
				return
			}
			id, err := b.Create(request)
			if err != nil {
				reject(400)
				return
			}
			write(201, struct {
				ID string `json:"id"`
			}{id})
			return
		}
		parts := strings.Split(path, "/")
		if len(parts) > 2 || !opaque.MatchString(parts[0]) {
			reject(404)
			return
		}
		id := parts[0]
		if len(parts) == 1 && r.Method == "GET" {
			_, cookie, err := b.Browser(id)
			if err != nil {
				reject(404)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "__Secure-dilla-desktop", Value: cookie, Path: "/desktop-auth/" + id + "/", MaxAge: 120, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, page)
			return
		}
		if len(parts) != 2 {
			reject(404)
			return
		}
		if parts[1] == "consume" && r.Method == "POST" {
			var input struct {
				Verifier string `json:"verifier"`
			}
			if !read(&input) {
				return
			}
			result, ready, err := b.Consume(id, input.Verifier)
			if err != nil {
				reject(404)
				return
			}
			if !ready {
				w.WriteHeader(202)
				return
			}
			write(200, result)
			return
		}
		cookie, err := r.Cookie("__Secure-dilla-desktop")
		if err != nil || !b.VerifyBrowser(id, cookie.Value) {
			reject(403)
			return
		}
		if parts[1] == "options" && r.Method == "GET" {
			request, _, err := b.Browser(id)
			if err != nil {
				reject(404)
				return
			}
			write(200, struct {
				Kind    string `json:"kind"`
				Options string `json:"options"`
			}{request.Kind, request.Options})
			return
		}
		if parts[1] == "oidc" && r.Method == "POST" {
			if r.Header.Get("Origin") != origin {
				reject(403)
				return
			}
			ticket, err := b.OIDCTicket(id)
			if err != nil {
				reject(404)
				return
			}
			write(200, struct {
				Ticket string `json:"ticket"`
			}{ticket})
			return
		}
		if parts[1] == "finish" && r.Method == "POST" {
			if r.Header.Get("Origin") != origin {
				reject(403)
				return
			}
			var result struct {
				Response string `json:"response"`
				Error    string `json:"error"`
			}
			if !read(&result) {
				return
			}
			if b.Complete(id, result.Response, result.Error) != nil {
				reject(400)
				return
			}
			w.WriteHeader(204)
			return
		}
		reject(404)
	})
}
