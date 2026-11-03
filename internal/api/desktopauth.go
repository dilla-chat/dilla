package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dilla-chat/dilla/internal/desktopauth"
	"github.com/dilla-chat/dilla/internal/server"
)

func (d Deps) registerDesktopAuth(m *server.Mux) {
	var origins []string
	if d.Config != nil {
		origins = d.Config.Auth.WebAuthn.RPOrigins
	}
	handler := desktopauth.Handler(d.desktopAuth, desktopBrowserOrigin(d.Domain, origins), func(r *http.Request) bool {
		if d.Limiter == nil || d.Config == nil {
			return false
		}
		key := server.RateKey(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
		class := server.Class{Name: "desktop-browser", PerSecond: 4, Burst: 20}
		if strings.HasSuffix(r.URL.Path, "/request") {
			class = server.Class{Name: "desktop-browser-start", PerSecond: 0.1, Burst: 5}
		}
		ok, _ := d.Limiter.Allow(class, key)
		return ok
	})
	m.Handle("GET /desktop-auth/{path...}", handler)
	m.Handle("POST /desktop-auth/{path...}", handler)
}

// The ceremony runs at the configured browser relying-party origin. Never
// infer its CSRF boundary from an incoming Origin, Host or forwarded header.
func desktopBrowserOrigin(domain string, origins []string) string {
	for _, raw := range origins {
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			continue
		}
		loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
		if u.Scheme == "https" || (u.Scheme == "http" && loopback) {
			return u.Scheme + "://" + u.Host
		}
	}
	return "https://" + domain
}
