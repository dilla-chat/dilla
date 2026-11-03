package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestDesktopInviteActionsUseTheConfiguredHostAndExistingGrammar(t *testing.T) {
	e := newEnv(t)
	code, hash := auth.NewInviteCode()
	if err := e.Repo.CreateInvite(t.Context(), store.InviteRow{ID: id.New(), CodeHash: hash, MaxUses: 3, Created: e.Clk.Now().Unix(), ExpiresAt: e.Clk.Now().Unix() + 3600}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		domain string
		link   bool
	}{
		{"dilla.test", true}, {"dilla.test:8443", true}, {"[::1]:8443", true},
		{"dilla.test/subpath", false}, {"dilla.test?credential=secret", false}, {"user:password@dilla.test", false}, {"dilla.test#fragment", false}, {"dilla.test:65536", false}, {"dilla.test.", false}, {`dilla.test"><script>alert(1)</script>`, false},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			d := api.Deps{Repo: e.Repo, Clock: e.Clk, Domain: tc.domain, DesktopDownloads: true}
			req := httptest.NewRequestWithContext(t.Context(), "GET", "https://untrusted.test/i/"+code, nil)
			req.SetPathValue("code", code)
			rec := httptest.NewRecorder()
			d.InviteLanding(rec, req)
			page := rec.Body.String()
			if rec.Code != 200 {
				t.Fatalf("status=%d %s", rec.Code, page)
			}
			if strings.Contains(page, "untrusted.test") || strings.Contains(page, "<script") || strings.Contains(page, "/welcome?invite=") {
				t.Fatalf("unsafe/wrong invite actions: %s", page)
			}
			want := `href="dilla://invite/` + tc.domain + `/` + code + `"`
			if tc.link != strings.Contains(page, want) {
				t.Fatalf("deep link %q present=%t want=%t", want, strings.Contains(page, want), tc.link)
			}
			if !tc.link && strings.Contains(page, `href="dilla:`) {
				t.Fatalf("invalid domain generated deep link: %s", page)
			}
			if !strings.Contains(page, `href="/download"`) || !strings.Contains(page, "paste this invite code") {
				t.Fatal("desktop invite lost download/manual code path")
			}
			// Browser actions do not change the existing CBOR response or spend a use.
			req.Header.Set("Accept", "application/cbor")
			cbor := httptest.NewRecorder()
			d.InviteLanding(cbor, req)
			if cbor.Code != http.StatusOK || cbor.Header().Get("Content-Type") != "application/cbor" {
				t.Fatalf("CBOR=%d %v", cbor.Code, cbor.Header())
			}
			row, err := e.Repo.GetInviteByHash(t.Context(), hash)
			if err != nil || row.UsedCount != 0 {
				t.Fatalf("landing consumed invite: %+v %v", row, err)
			}
		})
	}
}
