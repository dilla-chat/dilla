package dillad

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

// The call routes hand out this instance's own origin as the LiveKit URL (/rtc is proxied to the
// SFU), and the relay each TLS mode actually runs: TURN over TLS on server.listen's port in the
// direct-TLS modes, plain TURN over TCP on turn.listen's port behind a proxy, none when TURN is off.
func TestTheCallsConfigNamesTheRelayTheModeRuns(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "turn.secret")
	if err := os.WriteFile(secret, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	base := func() *config.Config {
		c := config.Default()
		c.Instance.Domain = "chat.example.test"
		c.Instance.PublicIP = netip.MustParseAddr("2001:db8::7")
		c.TURN.SharedSecretFile = secret
		c.Server.Listen = ":443"
		return c
	}
	for _, tc := range []struct {
		name     string
		tune     func(*config.Config)
		url      string
		relay    string
		haveTURN bool
	}{
		{"acme_tls_alpn", func(*config.Config) {}, "wss://chat.example.test",
			"turns:chat.example.test:443?transport=tcp", true},
		{"acme_ip", func(c *config.Config) { c.TLS.Mode = config.TLSModeACMEIP }, "wss://[2001:db8::7]",
			"turns:[2001:db8::7]:443?transport=tcp", true},
		{"behind_proxy", func(c *config.Config) {
			c.TLS.Mode = config.TLSModeBehindProxy
			c.TURN.Listen = "0.0.0.0:3478"
		}, "wss://chat.example.test", "turn:chat.example.test:3478?transport=tcp", true},
		// I14 (fix wave): a proxy that publishes the relay on another port, or terminates TLS in front
		// of it, tells clients so through turn.public_url, advertised verbatim.
		{"behind_proxy with public_url", func(c *config.Config) {
			c.TLS.Mode = config.TLSModeBehindProxy
			c.TURN.Listen = "127.0.0.1:13478"
			c.TURN.PublicURL = "turns:turn.example.test:5349?transport=tcp"
		}, "wss://chat.example.test", "turns:turn.example.test:5349?transport=tcp", true},
		{"acme with public_url", func(c *config.Config) {
			c.TURN.PublicURL = "turns:chat.example.test:8443?transport=tcp"
		}, "wss://chat.example.test", "turns:chat.example.test:8443?transport=tcp", true},
		{"turn off", func(c *config.Config) { c.TURN.Enabled = false }, "wss://chat.example.test", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.tune(c)
			got, err := callsConfig(c)
			if err != nil {
				t.Fatalf("callsConfig: %v", err)
			}
			if got.LiveKitURL != tc.url || got.CredentialTTL != time.Hour {
				t.Errorf("LiveKitURL %q, ttl %s; want %q, 1h", got.LiveKitURL, got.CredentialTTL, tc.url)
			}
			if !tc.haveTURN {
				if len(got.TURNURLs) != 0 || got.TURNSecret != "" {
					t.Errorf("TURN off, yet the relay is %v", got.TURNURLs)
				}
				return
			}
			if len(got.TURNURLs) != 1 || got.TURNURLs[0] != tc.relay || got.TURNSecret != "s3cret" {
				t.Errorf("relay %v, secret %q; want [%s] and the trimmed secret", got.TURNURLs, got.TURNSecret, tc.relay)
			}
		})
	}
	// dilla-media task 10: the call routes' limits and the caps they hand a client.
	c := base()
	c.LiveKit.MaxVoiceParticipants, c.LiveKit.MaxPublishers = 12, 4
	c.LiveKit.MaxAudioBitrateKbps, c.LiveKit.MaxShareBitrateKbps, c.LiveKit.VP9 = 48, 1500, true
	got, err := callsConfig(c)
	if err != nil {
		t.Fatalf("callsConfig: %v", err)
	}
	if got.MaxVoiceParticipants != 12 || got.MaxPublishers != 4 || got.MaxAudioBitrateKbps != 48 ||
		got.MaxShareBitrateKbps != 1500 || !got.VP9 {
		t.Errorf("call limits and caps = %+v", got)
	}
}

// fakeSFU verifies tokens spelled "<identity>@<room>".
type fakeSFU struct{ url string }

func (f fakeSFU) Token(room, identity string, _ *livekit.ParticipantPermission, _ map[string]string) (string, error) {
	return identity + "@" + room, nil
}
func (f fakeSFU) DeleteRoom(context.Context, string) error { return nil }
func (f fakeSFU) CreateRoom(context.Context, string) error { return nil }
func (f fakeSFU) UpdatePermission(context.Context, string, string, *livekit.ParticipantPermission) error {
	return nil
}
func (f fakeSFU) RemoveParticipants(context.Context, string, id.ID) error { return nil }
func (f fakeSFU) RemoveParticipant(context.Context, string, string) error { return nil }
func (f fakeSFU) Participants(context.Context, string) ([]*livekit.ParticipantInfo, error) {
	return nil, nil
}
func (f fakeSFU) Rooms(context.Context) ([]string, error) { return nil, nil }
func (f fakeSFU) HTTPURL() string                         { return f.url }

// VerifyToken reads "<identity>@<room>[@<sources>]", sources a comma list of mic, cam and screen
// (mic when left out), as a call token with that publish grant.
func (f fakeSFU) VerifyToken(token string) (sfu.RoomToken, error) {
	parts := strings.Split(token, "@")
	if len(parts) < 2 || len(parts) > 3 {
		return sfu.RoomToken{}, errors.New("not a token")
	}
	sources := "mic"
	if len(parts) == 3 {
		sources = parts[2]
	}
	grant := &auth.VideoGrant{RoomJoin: true, Room: parts[1]}
	grant.UpdateFromPermission(sfu.PublishGrant(strings.Contains(sources, "mic"),
		strings.Contains(sources, "cam"), strings.Contains(sources, "screen")))
	return sfu.RoomToken{Identity: parts[0], Room: parts[1], Claims: &auth.ClaimGrants{Identity: parts[0], Video: grant}}, nil
}

// fakeGate admits the devices of leaves in room with their base grant (the microphone unless
// allowed names another), and refuses with refused's error.
type fakeGate struct {
	room    string
	leaves  map[id.ID]bool
	allowed map[id.ID]*livekit.ParticipantPermission
	refused map[id.ID]error
	// noted, when set, records every device the proxy reported admitted (Admitted).
	noted *admissions
}

// admissions is a fakeGate's record of the joins the proxy let through.
type admissions struct {
	mu      sync.Mutex
	devs    []id.ID
	resumes []bool // per admission, whether the proxy reported it a signal resume
}

func (a *admissions) list() []id.ID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.devs)
}

func (a *admissions) resumeFlags() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.resumes)
}

func (g fakeGate) Admitted(_ context.Context, _ string, dev id.ID, resume bool) {
	if g.noted != nil {
		g.noted.mu.Lock()
		g.noted.devs = append(g.noted.devs, dev)
		g.noted.resumes = append(g.noted.resumes, resume)
		g.noted.mu.Unlock()
	}
}

func (g fakeGate) AdmitRoom(_ context.Context, room string, dev id.ID) (*livekit.ParticipantPermission, error) {
	if err := g.refused[dev]; err != nil {
		return nil, err
	}
	if room != g.room || !g.leaves[dev] {
		return nil, server.Errorf(server.CodeLeafNotCurrent, "not a current leaf")
	}
	if p, ok := g.allowed[dev]; ok {
		return p, nil
	}
	return sfu.PublishGrant(true, false, false), nil
}

// /rtc and everything under it reach the SFU for a current leaf, with the client address the
// trusted-proxy rule resolves as the one X-Forwarded-For LiveKit sees; a forged chain, the two
// headers LiveKit prefers over it and the publish parameter (in any spelling) are dropped.
func TestTheRTCPathsAreProxiedToTheSFU(t *testing.T) {
	var gotQuery url.Values
	var gotPath, gotXFF, gotCF, gotReal string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		gotXFF, gotCF, gotReal = r.Header.Get("X-Forwarded-For"), r.Header.Get("CF-Connecting-IP"), r.Header.Get("X-Real-IP")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	dev := id.New()
	tok := url.QueryEscape(dev.String() + "@room-1")
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, fakeGate{room: "room-1", leaves: map[id.ID]bool{dev: true}}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	for _, path := range []string{"/rtc?access_token=" + tok + clientProto, "/rtc/validate?access_token=" + tok + clientProto,
		"/rtc/v1?access_token=" + tok + clientProto + "&publish=x", "/rtc?access_token=" + tok + clientProto + "&%70ublish=y"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.RemoteAddr = "198.51.100.9:5555"
		req.Header.Set("X-Forwarded-For", "10.0.0.1")
		req.Header.Set("CF-Connecting-IP", "1.2.3.4")
		req.Header.Set("X-Real-IP", "5.6.7.8")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusTeapot || !strings.HasPrefix(gotPath, "/rtc") {
			t.Errorf("GET %s = %d, upstream saw %q", path, rec.Code, gotPath)
		}
		if gotQuery.Has("publish") {
			t.Errorf("GET %s: LiveKit saw publish=%q", path, gotQuery.Get("publish"))
		}
		if gotXFF != "198.51.100.9" || gotCF != "" || gotReal != "" {
			t.Errorf("GET %s: LiveKit saw X-Forwarded-For %q, CF-Connecting-IP %q, X-Real-IP %q", path, gotXFF, gotCF, gotReal)
		}
	}
	// The token in a Bearer header is read as well as the query parameter.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rtc/v1?"+clientProto[1:], nil)
	req.Header.Set("Authorization", "Bearer "+dev.String()+"@room-1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Errorf("a Bearer token = %d, want the SFU's answer", rec.Code)
	}
}

// DEV-25: only GET reaches LiveKit — a POST with upgrade headers and publish in its body never does.
func TestOnlyGETReachesTheSFU(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	defer upstream.Close()
	dev := id.New()
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, fakeGate{room: "room-1", leaves: map[id.ID]bool{dev: true}}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/rtc?access_token="+url.QueryEscape(dev.String()+"@room-1"), strings.NewReader("publish=y"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet || hit {
		t.Fatalf("POST /rtc = %d (Allow %q), upstream hit %v; want 405 and nothing proxied", rec.Code, rec.Header().Get("Allow"), hit)
	}
}

// DEV-44: the gate admits only a current leaf of the room the token names.
func TestTheRTCGateAdmitsOnlyACurrentLeaf(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	leaf, removed := id.New(), id.New()
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, fakeGate{room: "room-1", leaves: map[id.ID]bool{leaf: true}}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	for _, tc := range []struct {
		name, token string
		status      int
		code        string
	}{
		{"a device that is no leaf any more", removed.String() + "@room-1", http.StatusForbidden, "E_LEAF_NOT_CURRENT"},
		{"a leaf, but another room", leaf.String() + "@room-2", http.StatusForbidden, "E_LEAF_NOT_CURRENT"},
		{"a shadow identity", leaf.String() + "#x@room-1", http.StatusForbidden, "E_FORBIDDEN"},
		{"a token this instance never minted", "garbage", http.StatusForbidden, "E_FORBIDDEN"},
		{"no token at all", "", http.StatusForbidden, "E_FORBIDDEN"},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rtc?access_token="+url.QueryEscape(tc.token), nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.status || rtcErrorCode(rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, rec.Code, rtcErrorCode(rec), tc.status, tc.code)
		}
	}
}

// A token can never confer more at admission than the device holds now: a microphone token after
// speak was revoked, a camera token (one LiveKit refreshed while the device shared) after an unshare,
// and any token of a device that lost view or connect are refused; the base grant still joins, and a
// v0 resume of a sharer with its refreshed token passes, since LiveKit never re-reads a resumed
// participant's grants. A v1 join_request is held to the full comparison.
func TestTheRTCGateRefusesATokenWiderThanTheCurrentGrant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	speaker, muted, unshared, kicked := id.New(), id.New(), id.New(), id.New()
	gate := fakeGate{
		room:    "room-1",
		leaves:  map[id.ID]bool{speaker: true, muted: true, unshared: true, kicked: true},
		allowed: map[id.ID]*livekit.ParticipantPermission{muted: sfu.PublishGrant(false, false, false)},
		refused: map[id.ID]error{kicked: server.Errorf(server.CodeForbidden, "lost connect")},
	}
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, gate, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	for _, tc := range []struct {
		name, query string
		status      int
		code        string
	}{
		{"the happy path: the base grant", "access_token=" + speaker.String() + "@room-1", http.StatusTeapot, ""},
		{"a microphone token after speak was revoked", "access_token=" + muted.String() + "@room-1@mic", http.StatusForbidden, "E_FORBIDDEN"},
		{"a listen-only token after speak was revoked", "access_token=" + muted.String() + "@room-1@none", http.StatusTeapot, ""},
		{"a camera token after an unshare", "access_token=" + unshared.String() + "@room-1@mic,cam,screen", http.StatusForbidden, "E_FORBIDDEN"},
		{"a kicked leaf's base token", "access_token=" + kicked.String() + "@room-1", http.StatusForbidden, "E_FORBIDDEN"},
		{"a v0 resume with a sharer's refreshed token", "reconnect=1&access_token=" + unshared.String() + "@room-1@mic,cam", http.StatusTeapot, ""},
		{"a v0 resume still needs the gate", "reconnect=true&access_token=" + kicked.String() + "@room-1@mic", http.StatusForbidden, "E_FORBIDDEN"},
		{"a malformed v1 join_request is refused as LiveKit refuses it", "reconnect=1&join_request=x&access_token=" + unshared.String() + "@room-1@mic,cam", http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"reconnect=yes is no resume to LiveKit", "reconnect=yes&access_token=" + unshared.String() + "@room-1@mic,cam", http.StatusForbidden, "E_FORBIDDEN"},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rtc?"+tc.query+clientProto, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.status || rtcErrorCode(rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, rec.Code, rtcErrorCode(rec), tc.status, tc.code)
		}
	}
	// LiveKit reads a non-empty Authorization header before the parameter and refuses one that is
	// no Bearer; the gate reads the same token, so a valid parameter cannot hide a header LiveKit
	// would use.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rtc?access_token="+speaker.String()+"@room-1", nil)
	req.Header.Set("Authorization", "Basic "+unshared.String()+"@room-1@mic,cam")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-Bearer Authorization header beside a valid parameter = %d, want 403", rec.Code)
	}
}

// rtcErrorCode is element 0 of a CBOR error body, or "" when the body is not one. plan2_test.go's
// errorCode is the same rule in package dillad_test, which this internal test cannot see.
func rtcErrorCode(rec *httptest.ResponseRecorder) string {
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/cbor") {
		return ""
	}
	var e []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &e); err != nil || len(e) == 0 {
		return ""
	}
	code, _ := e[0].(string)
	return code
}

type admitEveryLeaf struct{}

func (admitEveryLeaf) AdmitRoom(context.Context, string, id.ID) (*livekit.ParticipantPermission, error) {
	return sfu.PublishGrant(true, false, false), nil
}

func (admitEveryLeaf) Admitted(context.Context, string, id.ID, bool) {}

// SP-20: against the real SFU, neither a GET carrying publish=x nor a POST carrying publish=y in its
// body creates a "<device>#…" participant through the proxy.
func TestNoShadowParticipantReachesTheSFUThroughTheProxy(t *testing.T) {
	c := sfu.DefaultConfig()
	c.APISecret = "dilla-rtc-proxy-secret-0123456789ab"
	c.Port, c.UDPPort = sfutest.FreePorts(t) // outside internal/sfu, never a fixed port (task 7's sfutest)
	srv, err := sfu.Start(t.Context(), c)
	if err != nil {
		t.Fatalf("sfu.Start: %v", err)
	}
	defer func() {
		if err := srv.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	const room = "0123456789abcdef0123456789abcdef-1790000000"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	dev := id.New()
	tok, err := srv.Token(room, dev.String(), sfu.PublishGrant(true, false, false), nil)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	mux := server.NewMux()
	if err := mountRTC(mux, srv, admitEveryLeaf{}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	front := httptest.NewServer(mux)
	defer front.Close()

	// A real token carrying the camera (one minted, or refreshed by LiveKit, while the device held a
	// slot) is refused at the gate against the base grant the device holds now.
	camTok, err := srv.Token(room, id.New().String(), sfu.PublishGrant(true, true, true), nil)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	camReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/rtc?access_token="+url.QueryEscape(camTok), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	camResp, err := http.DefaultClient.Do(camReq)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	camResp.Body.Close()
	if camResp.StatusCode != http.StatusForbidden {
		t.Fatalf("a camera token through the gate = %d, want 403", camResp.StatusCode)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		front.URL+"/rtc?access_token="+url.QueryEscape(tok), strings.NewReader("publish=y"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST through the proxy = %d, want 405", resp.StatusCode)
	}

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	fmt.Fprintf(conn, "GET /rtc?access_token=%s"+clientProto+"&publish=x HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\n"+
		"Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n",
		url.QueryEscape(tok), front.Listener.Addr(), base64.StdEncoding.EncodeToString(key))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 101") {
		t.Fatalf("GET with publish=x through the proxy answered %q (%v), want the upgrade", status, err)
	}
	sawDevice := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		parts, err := srv.Participants(t.Context(), room)
		if err != nil {
			t.Fatalf("Participants: %v", err)
		}
		for _, p := range parts {
			if strings.Contains(p.GetIdentity(), "#") {
				t.Fatalf("LiveKit holds the shadow participant %q", p.GetIdentity())
			}
			sawDevice = sawDevice || p.GetIdentity() == dev.String()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("SP-20 the device itself was listed: %v", sawDevice)
}

// DEV-59: the calls leg warns on decrypt failures, the turn leg on quota refusals; both are OK when
// nothing is wrong, and the turn leg says when there is no relay at all.
func TestTheCallsAndTurnLegs(t *testing.T) {
	if l := callsLeg(api.StatsSummary{}); l.Name != "calls" || l.Status != ops.Green {
		t.Errorf("no reports = %+v", l)
	}
	l := callsLeg(api.StatsSummary{LiveCalls: 2, Reports: 5, RelayReports: 1, DecryptFailures: 3, FramesEncrypted: 3000, P50RTTms: 40, P95RTTms: 120})
	if l.Status != ops.Yellow || !strings.Contains(l.Detail, "2 live calls") || l.Fix == "" ||
		!strings.Contains(l.Detail, "3 decrypt failures against 3000 frames encrypted (1.0 per 1000)") {
		t.Errorf("decrypt failures = %+v, want WARN with a fix and the rate against frames encrypted", l)
	}
	// Review M4: the instance's relay leg (its counters) is not `dillad doctor`'s "turn" (an
	// allocation probe), so it has a name of its own.
	if l := turnLeg(config.TURN{Enabled: false}, 0, 0); l.Name != "turn_relay" || l.Status != ops.Green || !strings.Contains(l.Detail, "off") {
		t.Errorf("TURN off = %+v", l)
	}
	if l := turnLeg(config.TURN{Enabled: true, AllocationsPerDevice: 4}, 3, 0); l.Status != ops.Green || !strings.Contains(l.Detail, "3 live relay allocations") {
		t.Errorf("TURN healthy = %+v", l)
	}
	if l := turnLeg(config.TURN{Enabled: true, AllocationsPerDevice: 4}, 4, 1); l.Status != ops.Yellow ||
		!strings.Contains(l.Fix, "turn.allocations_per_device (now 4)") {
		t.Errorf("a quota refusal = %+v, want WARN naming the knob", l)
	}
}
