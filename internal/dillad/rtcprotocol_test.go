package dillad

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/sfu"
)

// clientProto is the v0 query parameter both livekit-client 2.22.3 and the Go SDK send.
var clientProto = "&protocol=" + strconv.Itoa(sfu.ClientProtocol)

// clientInfo is the client info livekit-client 2.22.3 puts in its v1 join_request (room/utils.ts:479).
func clientInfo() *livekit.ClientInfo {
	return &livekit.ClientInfo{Sdk: livekit.ClientInfo_JS, Protocol: sfu.ClientProtocol}
}

// Branch review SFU-1: LiveKit labels its session histograms with the protocol number the client
// sent, and its pub/sub histogram with the v1 client_info.sdk enum, so the gate admits only the
// protocol dilla's clients speak and a known SDK. Every other value — missing, older, newer, not a
// number, out of int32 — is refused 400 E_INVALID_REQUEST before the request reaches LiveKit, on the
// v0 query and inside a v1 join_request alike; 40 distinct values reach LiveKit zero times.
func TestTheRTCGateRefusesAClientProtocolNoDillaClientSends(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	dev := id.New()
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, fakeGate{room: "room-1", leaves: map[id.ID]bool{dev: true}}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	tok := "access_token=" + url.QueryEscape(dev.String()+"@room-1")
	get := func(path, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path+"?"+query, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	v1 := func(ci *livekit.ClientInfo) string {
		return tok + "&join_request=" + wrappedJoin(t, &livekit.JoinRequest{ClientInfo: ci,
			ConnectionSettings: &livekit.ConnectionSettings{AutoSubscribe: true}}, livekit.WrappedJoinRequest_NONE)
	}
	for _, tc := range []struct{ name, path, query string }{
		{"no protocol", "/rtc", tok},
		{"protocol 0", "/rtc", tok + "&protocol=0"},
		{"an older protocol", "/rtc", tok + "&protocol=16"},
		{"a newer protocol", "/rtc", tok + "&protocol=18"},
		{"the int32 maximum", "/rtc", tok + "&protocol=2147483647"},
		{"a negative protocol", "/rtc", tok + "&protocol=-17"},
		{"not a number", "/rtc", tok + "&protocol=seventeen"},
		{"past int32", "/rtc", tok + "&protocol=4294967313"},
		{"on validate too", "/rtc/validate", tok + "&protocol=99"},
		{"a v1 request with no client info", "/rtc/v1", v1(nil)},
		{"a v1 request with another protocol", "/rtc/v1", v1(&livekit.ClientInfo{Sdk: livekit.ClientInfo_JS, Protocol: 1234})},
		{"a v1 request whose query names 17 but whose client info does not", "/rtc/v1", v1(&livekit.ClientInfo{Protocol: 99}) + "&protocol=17"},
		{"a v1 request with an SDK LiveKit's enum does not name", "/rtc/v1", v1(&livekit.ClientInfo{Sdk: 999, Protocol: sfu.ClientProtocol})},
	} {
		if rec := get(tc.path, tc.query); rec.Code != http.StatusBadRequest || rtcErrorCode(rec) != "E_INVALID_REQUEST" {
			t.Errorf("%s: %d %s, want 400 E_INVALID_REQUEST", tc.name, rec.Code, rtcErrorCode(rec))
		}
	}
	for n := range 40 {
		if rec := get("/rtc", tok+"&protocol="+strconv.Itoa(1000+n)); rec.Code != http.StatusBadRequest {
			t.Fatalf("protocol %d: %d, want 400", 1000+n, rec.Code)
		}
	}
	if hits != 0 {
		t.Fatalf("LiveKit saw %d requests with a protocol no dilla client sends", hits)
	}
	// What the clients send passes: livekit-client's v0 query and v1 join_request, the Go SDK's v0
	// query (sdk=go), and the v0 form LiveKit parses with a leading plus or zero as the same 17.
	for _, q := range []string{
		tok + "&sdk=js&protocol=" + strconv.Itoa(sfu.ClientProtocol),
		tok + "&sdk=go&protocol=" + strconv.Itoa(sfu.ClientProtocol),
		tok + "&protocol=0" + strconv.Itoa(sfu.ClientProtocol),
		v1(clientInfo()),
		v1(&livekit.ClientInfo{Sdk: livekit.ClientInfo_GO, Protocol: sfu.ClientProtocol}),
	} {
		if rec := get("/rtc", q); rec.Code != http.StatusTeapot {
			t.Errorf("%s: %d %s, want the SFU's answer", q, rec.Code, rtcErrorCode(rec))
		}
	}
	if hits != 5 {
		t.Fatalf("LiveKit saw %d requests, want the 5 admitted", hits)
	}
	// A refused device is still refused for what it is, whatever its protocol: the protocol is read
	// only once the gate admitted the device.
	other := id.New()
	if rec := get("/rtc", "access_token="+url.QueryEscape(other.String()+"@room-1")+"&protocol=1"); rec.Code != http.StatusForbidden || rtcErrorCode(rec) != "E_LEAF_NOT_CURRENT" {
		t.Errorf("a non-leaf with a bad protocol: %d %s, want 403 E_LEAF_NOT_CURRENT", rec.Code, rtcErrorCode(rec))
	}
}
