package dillad

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

// wrappedJoin is a join_request as livekit-client 2.22.3 sends it on /rtc/v1: a JoinRequest inside a
// WrappedJoinRequest, protobuf, base64url (padded, as LiveKit decodes it).
func wrappedJoin(t *testing.T, jr *livekit.JoinRequest, compression livekit.WrappedJoinRequest_Compression) string {
	t.Helper()
	inner, err := proto.Marshal(jr)
	if err != nil {
		t.Fatalf("marshal JoinRequest: %v", err)
	}
	if compression == livekit.WrappedJoinRequest_GZIP {
		inner = gzipped(t, inner)
	}
	return wrapRaw(t, &livekit.WrappedJoinRequest{Compression: compression, JoinRequest: inner})
}

func wrapRaw(t *testing.T, w *livekit.WrappedJoinRequest) string {
	t.Helper()
	outer, err := proto.Marshal(w)
	if err != nil {
		t.Fatalf("marshal WrappedJoinRequest: %v", err)
	}
	return base64.URLEncoding.EncodeToString(outer)
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return buf.Bytes()
}

// Important finding I2 of the task 10 review: livekit-client 2.22.3 signals over /rtc/v1, where the
// resume flag travels inside the wrapped join request. The gate parses it exactly as LiveKit does,
// so a sharer's v1 resume with the camera-carrying token LiveKit refreshed for it is admitted (the
// per-source comparison only is skipped), while a v1 fresh join with that token, a v1 resume of a
// device the gate refuses, and a join request with a compression LiveKit does not know are held to
// every check. A join request LiveKit would refuse — not base64url, not protobuf, an inner request
// that does not decode, over http.DefaultMaxHeaderBytes raw or once gunzipped — is refused 400, as
// LiveKit refuses it, before it reaches the SFU.
func TestTheRTCGateReadsTheV1ResumeAsLiveKitDoes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	sharer, kicked := id.New(), id.New()
	gate := fakeGate{
		room:    "room-1",
		leaves:  map[id.ID]bool{sharer: true, kicked: true},
		refused: map[id.ID]error{kicked: server.Errorf(server.CodeForbidden, "lost connect")},
	}
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, gate, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	camTok := "access_token=" + url.QueryEscape(sharer.String()+"@room-1@mic,cam,screen")
	resume := &livekit.JoinRequest{Reconnect: true, ParticipantSid: "PA_x", ClientInfo: clientInfo()}
	// Not empty: an empty JoinRequest encodes to an empty join_request, which LiveKit reads as none.
	fresh := &livekit.JoinRequest{ConnectionSettings: &livekit.ConnectionSettings{AutoSubscribe: true}, ClientInfo: clientInfo()}
	tooBig := make([]byte, http.DefaultMaxHeaderBytes+1)
	bomb := gzipped(t, make([]byte, http.DefaultMaxHeaderBytes+1))
	resumeBytes, _ := proto.Marshal(resume)
	for _, tc := range []struct {
		name, path, query string
		status            int
		code              string
	}{
		{"a sharer's v1 resume with its refreshed token", "/rtc/v1", camTok + "&join_request=" + wrappedJoin(t, resume, livekit.WrappedJoinRequest_NONE), http.StatusTeapot, ""},
		{"a sharer's gzip v1 resume", "/rtc/v1", camTok + "&join_request=" + wrappedJoin(t, resume, livekit.WrappedJoinRequest_GZIP), http.StatusTeapot, ""},
		{"a v1 resume on the /rtc path", "/rtc", camTok + "&join_request=" + wrappedJoin(t, resume, livekit.WrappedJoinRequest_NONE), http.StatusTeapot, ""},
		{"a v1 fresh join with that token", "/rtc/v1", camTok + "&join_request=" + wrappedJoin(t, fresh, livekit.WrappedJoinRequest_NONE), http.StatusForbidden, "E_FORBIDDEN"},
		{"a v1 fresh join even with reconnect=1 beside it", "/rtc/v1", camTok + "&reconnect=1&join_request=" + wrappedJoin(t, fresh, livekit.WrappedJoinRequest_NONE), http.StatusForbidden, "E_FORBIDDEN"},
		{"a v1 resume still needs the gate", "/rtc/v1", "access_token=" + url.QueryEscape(kicked.String()+"@room-1") + "&join_request=" + wrappedJoin(t, resume, livekit.WrappedJoinRequest_NONE), http.StatusForbidden, "E_FORBIDDEN"},
		{"an unknown compression is a fresh join to LiveKit", "/rtc/v1", camTok + "&join_request=" + wrapRaw(t, &livekit.WrappedJoinRequest{Compression: 7, JoinRequest: resumeBytes}), http.StatusForbidden, "E_FORBIDDEN"},
		{"a join_request that is not base64url", "/rtc/v1", camTok + "&join_request=x", http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"a join_request that is not protobuf", "/rtc/v1", camTok + "&join_request=" + base64.URLEncoding.EncodeToString([]byte{0xff, 0xff, 0xff}), http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"an inner request that does not decode", "/rtc/v1", camTok + "&join_request=" + wrapRaw(t, &livekit.WrappedJoinRequest{JoinRequest: []byte{0xff, 0xff, 0xff}}), http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"an inner request over LiveKit's bound", "/rtc/v1", camTok + "&join_request=" + wrapRaw(t, &livekit.WrappedJoinRequest{JoinRequest: tooBig}), http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"a gzip payload that inflates past LiveKit's bound", "/rtc/v1", camTok + "&join_request=" + wrapRaw(t, &livekit.WrappedJoinRequest{Compression: livekit.WrappedJoinRequest_GZIP, JoinRequest: bomb}), http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"a gzip payload that is no gzip", "/rtc/v1", camTok + "&join_request=" + wrapRaw(t, &livekit.WrappedJoinRequest{Compression: livekit.WrappedJoinRequest_GZIP, JoinRequest: []byte("plain")}), http.StatusBadRequest, "E_INVALID_REQUEST"},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path+"?"+tc.query, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.status || rtcErrorCode(rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, rec.Code, rtcErrorCode(rec), tc.status, tc.code)
		}
	}
}

// The security claim the resume exemption rests on, against the real in-process SFU: a resume — v0
// or v1 — that presents a camera-carrying token for an identity LiveKit does not hold passes the
// gate (the per-source comparison is skipped) and is then refused by LiveKit, which sends a leave and
// never adds the participant. Such a resume confers nothing.
func TestAResumeOfAParticipantTheSFUDoesNotHoldConfersNothing(t *testing.T) {
	c := sfu.DefaultConfig()
	c.APISecret = "dilla-rtc-resume-secret-0123456789abc"
	c.Port, c.UDPPort = sfutest.FreePorts(t)
	srv, err := sfu.Start(t.Context(), c)
	if err != nil {
		t.Fatalf("sfu.Start: %v", err)
	}
	defer func() {
		if err := srv.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	const room = "0123456789abcdef0123456789abcdef-1790000001"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	mux := server.NewMux()
	if err := mountRTC(mux, srv, admitEveryLeaf{}, nil, unmetered()); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	front := httptest.NewServer(mux)
	defer front.Close()

	resume := &livekit.JoinRequest{Reconnect: true, ParticipantSid: "PA_nobody", ClientInfo: clientInfo()}
	for _, tc := range []struct{ name, path, extra string }{
		{"v0", "/rtc", "&reconnect=1&sid=PA_nobody" + clientProto},
		{"v1", "/rtc/v1", "&join_request=" + wrappedJoin(t, resume, livekit.WrappedJoinRequest_NONE)},
	} {
		ghost := id.New()
		tok, err := srv.Token(room, ghost.String(), sfu.PublishGrant(true, true, true), nil)
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		status := rawUpgrade(t, front, tc.path+"?access_token="+url.QueryEscape(tok)+tc.extra)
		if strings.Contains(status, " 403 ") {
			t.Fatalf("%s: the gate refused a resume (%q); this test is about what the SFU does with it", tc.name, status)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			parts, err := srv.Participants(t.Context(), room)
			if err != nil {
				t.Fatalf("Participants: %v", err)
			}
			for _, p := range parts {
				if strings.HasPrefix(p.GetIdentity(), ghost.String()) {
					t.Fatalf("%s: a resume of a participant LiveKit did not hold added %q (%v)", tc.name, p.GetIdentity(), p.GetPermission())
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("%s resume through the gate answered %q; LiveKit added no participant", tc.name, strings.TrimSpace(status))
	}
}

// rawUpgrade sends one WebSocket upgrade for path through front and answers its status line.
func rawUpgrade(t *testing.T, front *httptest.Server, path string) string {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n",
		path, front.Listener.Addr(), base64.StdEncoding.EncodeToString(key))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the upgrade's status: %v", err)
	}
	return status
}

// unmetered is a rate limiter whose buckets are off, for the gate tests that are not about the meter.
func unmetered() *server.RateLimiter {
	r := config.Default().Limits.Rate
	r.Enabled = false
	return server.NewRateLimiter(r, clock.System())
}

// Minor m5 of the task 10 review: /rtc is metered on the [limits.rate] unauth class per client
// address. Past the burst a request is 429 E_RATE_LIMITED with retry_after_ms and never reaches the
// gate or the SFU; another address keeps its own bucket; the bucket refills with time.
func TestTheRTCPathsAreMeteredPerClientAddress(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	rate := config.Default().Limits.Rate
	rate.Enabled, rate.UnauthPerSecond, rate.UnauthBurst = true, 1, 3
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	limiter := server.NewRateLimiter(rate, clk)
	dev := id.New()
	mux := server.NewMux()
	if err := mountRTC(mux, fakeSFU{url: upstream.URL}, fakeGate{room: "room-1", leaves: map[id.ID]bool{dev: true}}, nil, limiter); err != nil {
		t.Fatalf("mountRTC: %v", err)
	}
	get := func(addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rtc?access_token="+url.QueryEscape(dev.String()+"@room-1")+clientProto, nil)
		req.RemoteAddr = addr + ":5555"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for i := range rate.UnauthBurst {
		if rec := get("198.51.100.7"); rec.Code != http.StatusTeapot {
			t.Fatalf("request %d of a burst of %d = %d", i+1, rate.UnauthBurst, rec.Code)
		}
	}
	rec := get("198.51.100.7")
	if rec.Code != http.StatusTooManyRequests || rtcErrorCode(rec) != "E_RATE_LIMITED" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("past the burst = %d %s (Retry-After %q), want 429 E_RATE_LIMITED with a retry delay",
			rec.Code, rtcErrorCode(rec), rec.Header().Get("Retry-After"))
	}
	if hits != rate.UnauthBurst {
		t.Fatalf("the SFU saw %d requests, want the %d within the burst", hits, rate.UnauthBurst)
	}
	if rec := get("203.0.113.9"); rec.Code != http.StatusTeapot {
		t.Fatalf("another address = %d, want its own bucket", rec.Code)
	}
	clk.Advance(2 * time.Second)
	if rec := get("198.51.100.7"); rec.Code != http.StatusTeapot {
		t.Fatalf("after the bucket refilled = %d", rec.Code)
	}
}
