package dillad

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// M-2 of the integration re-review: the proxy tells the call gate a join was admitted only once its
// own checks after AdmitRoom passed — the token's grants and the client's signalling protocol. A join
// it refuses (here a protocol no dilla client sends, and a token wider than the device holds) is
// never recorded, so it can neither hold a device's join window open nor push the device's real
// admission out of the few the call routes remember; a join it lets through is recorded once.
func TestTheProxyRecordsAnAdmissionOnlyForAJoinItLetsThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	dev := id.New()
	noted := &admissions{}
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL},
		fakeGate{room: "room-1", leaves: map[id.ID]bool{dev: true}, noted: noted}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	get := func(query string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rtc?"+query, nil))
		return rec.Code
	}
	tok := "access_token=" + url.QueryEscape(dev.String()+"@room-1")
	for _, refused := range []string{
		tok + "&protocol=16", // a protocol no dilla client sends: CheckClient refuses it
		"access_token=" + url.QueryEscape(dev.String()+"@room-1@cam") + clientProto, // wider than the base grant
	} {
		if code := get(refused); code == http.StatusTeapot {
			t.Fatalf("GET /rtc?%s reached the SFU", refused)
		}
	}
	if got := noted.list(); len(got) != 0 {
		t.Fatalf("refused joins were recorded as admitted: %v", got)
	}
	if code := get(tok + clientProto); code != http.StatusTeapot {
		t.Fatalf("an admissible join = %d, want the SFU's answer", code)
	}
	if got := noted.list(); len(got) != 1 || got[0] != dev {
		t.Fatalf("admissions = %v, want the one join let through", got)
	}
	// A signal resume of that session is admitted too, reported as a resume (web3.7: the call routes
	// never take it for a newer session).
	if code := get("reconnect=1&" + tok + clientProto); code != http.StatusTeapot {
		t.Fatalf("an admissible resume = %d, want the SFU's answer", code)
	}
	if got := noted.resumeFlags(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("resume flags = %v, want [false true]: the fresh join, then the resume", got)
	}
}
