package dilladtest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// POST /debug/afk/legacy writes a web3.8 administrator's AFK pick, on the control listener only.
func TestDebugAFKLegacyIsControlOnly(t *testing.T) {
	h := sfuHost(t, false)
	repo := h.Server().Repo()
	owner, _, _ := seedPerson(t, h, "afk-owner")
	cid, lounge := id.New(), id.New()
	now := h.Clock().Now().Unix()
	if err := repo.CreateCommunity(t.Context(), store.CommunityRow{ID: cid, Owner: owner, Name: "legacy",
		PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateChannel(t.Context(), store.ChannelRow{ID: lounge, CommunityID: &cid, Kind: api.ChannelVoice,
		Name: "lounge", SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetCommunityAFK(t.Context(), cid, store.CommunityAFK{TimeoutSeconds: 900}); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"community": cid.String(), "channel": lounge.String()}

	public := httptest.NewServer(h.Handler())
	t.Cleanup(public.Close)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/debug/afk/legacy", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("public POST /debug/afk/legacy = %d, want 404", res.StatusCode)
	}
	if cfg, err := repo.GetCommunityAFK(t.Context(), cid); err != nil || cfg.ChannelID != nil {
		t.Fatalf("the public listener moved the pointer: %+v, %v", cfg, err)
	}

	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	if status := controlPost(t, control.URL, "/debug/afk/legacy", body); status != http.StatusNoContent {
		t.Fatalf("control POST /debug/afk/legacy = %d, want 204", status)
	}
	cfg, err := repo.GetCommunityAFK(t.Context(), cid)
	if err != nil || cfg.ChannelID == nil || *cfg.ChannelID != lounge || cfg.TimeoutSeconds != 900 {
		t.Fatalf("pointer after the control POST = %+v, %v; want lounge, 900", cfg, err)
	}
}
