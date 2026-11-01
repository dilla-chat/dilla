package api

import (
	"net/http"
	"strings"

	"github.com/jonasthim/dilla/internal/desktopauth"
	"github.com/jonasthim/dilla/internal/server"
)

func (d Deps) registerDesktopAuth(m *server.Mux) {
	handler := desktopauth.Handler(d.desktopAuth, "https://"+d.Domain, func(r *http.Request) bool {
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
