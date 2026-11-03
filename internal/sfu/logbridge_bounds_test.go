package sfu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"

	dillaconfig "github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/obs"
)

// fakeNow is a clock the bridge's rate limiter reads.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// dilladBridged is a LiveKit logger through the bridge into dillad's own redacting JSON logger at
// debug, the configuration where the most reaches it, with the bridge's clock in the test's hands.
func dilladBridged(t *testing.T) (logger.Logger, *bytes.Buffer, *fakeNow) {
	t.Helper()
	var buf bytes.Buffer
	clk := &fakeNow{t: time.Unix(1_790_000_000, 0)}
	sink := obs.NewLogger(dillaconfig.Log{Level: "debug", Format: "json"}, &buf).Handler()
	h := newLogBridge(sink, clk.now)
	return logger.LogRLogger(logr.FromSlogHandler(h)).WithName("livekit"), &buf, clk
}

// sdpOf is a publisher SDP of about n bytes, as a client would send it: credentials, a fingerprint
// and candidates with addresses.
func sdpOf(n int) string {
	var b strings.Builder
	b.WriteString("v=0\r\no=- 4611731400430051336 2 IN IP4 203.0.113.7\r\ns=-\r\nt=0 0\r\n")
	b.WriteString("a=ice-ufrag:abcd\r\na=ice-pwd:0123456789abcdefghijklmn\r\n")
	b.WriteString("a=fingerprint:sha-256 AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89\r\n")
	for b.Len() < n {
		b.WriteString("a=candidate:1 1 udp 2122260223 2001:db8::7 54321 typ host generation 0\r\n")
	}
	return b.String()
}

var (
	// An IPv4 literal, or an IPv6 one with "::" or at least five groups (a JSON time's hh:mm:ss is
	// neither).
	ipLiteral = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b|[0-9a-fA-F]{0,4}::[0-9a-fA-F]{1,4}|(?:[0-9a-fA-F]{1,4}:){4,7}[0-9a-fA-F]{1,4}`)
	hex32     = regexp.MustCompile(`[0-9a-f]{32}`)
)

// lines is buf split into its JSON lines.
func lines(buf *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Branch review SFU-2 and parked item 2: what LiveKit binds and logs at the levels dillad forwards —
// the participant's ParticipantInit with its client address and v1 publisher offer, a 2 MiB offer a
// member sent, an ICE candidate, a session description, the full call id in the room name, an error
// naming an address — reaches dillad's log as none of it: no SDP line, no address, no 32-hex id, no
// participantInit/offer/candidate/sdp key, and no line over 4 KiB.
func TestNoSDPAddressOrIdentityLeavesTheBridge(t *testing.T) {
	l, buf, _ := dilladBridged(t)
	device := "0123456789abcdef0123456789abcdef"
	pi := &routing.ParticipantInit{
		Identity: livekit.ParticipantIdentity(device),
		Client:   &livekit.ClientInfo{Address: "203.0.113.7", Os: "Linux", Browser: "Firefox", DeviceModel: "x"},
		Grants: &auth.ClaimGrants{Identity: device, Attributes: map[string]string{"dilla.vdec": "vp8"},
			Metadata: "secret-metadata"},
		PublisherOffer: &livekit.SessionDescription{Type: "offer", Sdp: sdpOf(4096)},
	}
	pl := l.WithName("rtc").WithValues("room", device+"-1790000000-deadbeef", "participant", device,
		"participantInit", pi, "participantID", "PA_abcdefgh123")
	offer := sdpOf(2 << 20)
	pl.Warnw("could not parse offer", errors.New("sdp: invalid line"), "offer", offer)
	pl.Warnw("could not handle offer", errors.New("bad"), "mungedOffer", offer)
	pl.Infow("trickle", "candidate", webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 1 198.51.100.4 4444 typ host"})
	// Re-review N4: the IPv4-mapped form netip prints and a zoned link-local address are masked whole.
	pl.Errorw("set remote description", errors.New("ice: 203.0.113.7:4444 and [2001:db8::7]:5555 and "+
		"::ffff:203.0.113.7:4444 and fe80::1%eth0 unreachable"),
		"sdp", webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer})
	pl.Infow("ice restart", "sdpFragment", "a=ice-ufrag:x\r\na=ice-pwd:y", "metadata", "user metadata")
	got := lines(buf)
	if len(got) != 5 {
		t.Fatalf("%d lines, want 5: %q", len(got), got)
	}
	for _, line := range got {
		if len(line) > maxBridgedLine {
			t.Errorf("a %d-byte line (cap %d): %.200s…", len(line), maxBridgedLine, line)
		}
		for _, bad := range []string{"0.113.7", "ffff", "fe80", "%eth0", "eth0", "v=0", "a=", "ice-pwd", "fingerprint", "candidate:", "participantInit",
			`"offer"`, `"mungedOffer"`, `"candidate"`, `"sdp"`, "sdpFragment", "metadata", "Firefox", "dilla.vdec"} {
			if strings.Contains(line, bad) {
				t.Errorf("%q leaked: %.300s", bad, line)
			}
		}
		if m := ipLiteral.FindString(line); m != "" {
			t.Errorf("address %q leaked: %.300s", m, line)
		}
		if m := hex32.FindString(line); m != "" {
			t.Errorf("full id %q leaked: %.300s", m, line)
		}
	}
}

// What dillad needs from a LiveKit line still arrives: the logger, the message, the room as the
// shortened call id and its suffix, the device and session ids shortened, the track id, scalars LiveKit
// uses for joins and leaves, and the error text with any address masked.
func TestTheAllowedAttributesStillArrive(t *testing.T) {
	l, buf, _ := dilladBridged(t)
	l.WithName("room").WithValues("room", "0123456789abcdef0123456789abcdef-1790000000-deadbeef", "roomID", "RM_abcdefgh123").
		Warnw("participant closed", errors.New("dial 203.0.113.7:4444: timeout"),
			"participant", "0123456789abcdef0123456789abcdef", "participantID", "PA_abcdefgh123",
			"trackID", "TR_abcdefgh123", "kind", livekit.TrackType_VIDEO, "reason", "CLIENT_REQUEST_LEAVE",
			"numParticipants", 3, "relayed", false, "state", "ACTIVE")
	got := records(t, buf)
	if len(got) != 1 {
		t.Fatalf("%d records", len(got))
	}
	r := got[0]
	want := map[string]any{
		"msg": "participant closed", "logger": "livekit/room", "room": "01234567-1790000000-deadbeef",
		"room_id": "RM_abcde", "device_id": "01234567", "participant_id": "PA_abcde", "track_id": "TR_abcde",
		"kind": "VIDEO", "reason": "CLIENT_REQUEST_LEAVE", "numParticipants": float64(3), "relayed": false,
		"state": "ACTIVE", "error": "dial [addr]: timeout",
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %#v, want %#v (record %v)", k, r[k], v, r)
		}
	}
}

// SFU-2: one participant looping a malformed offer cannot flood the log. 10 000 identical warnings,
// each with a 1 MiB offer, write the burst of 20 lines; once the suppression interval has passed, the
// next line comes with one line counting the 9 980 suppressed. A different message is not held by
// that flood, and every line stays under the cap.
func TestALoopOfIdenticalWarningsIsRateLimited(t *testing.T) {
	l, buf, clk := dilladBridged(t)
	pl := l.WithName("rtc").WithValues("participant", "0123456789abcdef0123456789abcdef")
	offer := sdpOf(1 << 20)
	for range 10_000 {
		pl.Warnw("could not parse offer", errors.New("sdp: invalid line"), "offer", offer)
	}
	if n := len(lines(buf)); n != bridgeBurst {
		t.Fatalf("%d lines from 10 000 identical warnings, want the burst of %d", n, bridgeBurst)
	}
	pl.Infow("participant active")
	if n := len(lines(buf)); n != bridgeBurst+1 {
		t.Fatalf("another message was held by the flood: %d lines", n)
	}
	clk.advance(suppressionInterval)
	pl.Warnw("could not parse offer", errors.New("sdp: invalid line"), "offer", offer)
	got := records(t, buf)
	if len(got) > 25 {
		t.Fatalf("%d lines, want at most 25", len(got))
	}
	var summary map[string]any
	for _, r := range got {
		if r["msg"] == "LiveKit log lines suppressed" {
			summary = r
		}
	}
	if summary == nil || summary["suppressed"] != float64(10_000-bridgeBurst) ||
		summary["suppressed_msg"] != "could not parse offer" || summary["logger"] != "livekit/rtc" {
		t.Fatalf("summary %v, want suppressed %d of \"could not parse offer\" on livekit/rtc", summary, 10_000-bridgeBurst)
	}
	if got[len(got)-1]["msg"] != "could not parse offer" {
		t.Errorf("the line after the interval was not logged: %v", got[len(got)-1])
	}
	for _, line := range lines(buf) {
		if len(line) > maxBridgedLine {
			t.Errorf("a %d-byte line", len(line))
		}
	}
}

// countingSink counts how often a handler chain is derived from it.
type countingSink struct {
	derived *int
	mu      *sync.Mutex
}

func (countingSink) Enabled(context.Context, slog.Level) bool  { return false }
func (countingSink) Handle(context.Context, slog.Record) error { return nil }
func (h countingSink) WithAttrs([]slog.Attr) slog.Handler {
	h.mu.Lock()
	*h.derived++
	h.mu.Unlock()
	return h
}
func (h countingSink) WithGroup(string) slog.Handler { return h.WithAttrs(nil) }

// SFU-6: a logger derived from the swap handler replays its attributes onto the current sink once
// per swap, not per call, and Enabled — which logr asks before every record — asks the sink alone.
func TestTheSwapHandlerDerivesOncePerSwap(t *testing.T) {
	var derived int
	var mu sync.Mutex
	var sink slog.Handler = countingSink{derived: &derived, mu: &mu}
	liveKitSink.Store(&sink)
	t.Cleanup(func() { d := slog.DiscardHandler; liveKitSink.Store(&d) })
	var h slog.Handler = &swapHandler{}
	for i := range 5 {
		h = h.WithAttrs([]slog.Attr{slog.String(fmt.Sprintf("k%d", i), "v")})
	}
	ctx := context.Background()
	for range 100 {
		_ = h.Enabled(ctx, slog.LevelDebug)
		_ = h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0))
	}
	if derived != 5 {
		t.Fatalf("100 records derived the chain %d times over, want its 5 attributes once", derived)
	}
	var next slog.Handler = countingSink{derived: &derived, mu: &mu}
	liveKitSink.Store(&next)
	_ = h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0))
	_ = h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0))
	if derived != 10 {
		t.Fatalf("after a swap the chain was derived %d times in all, want 10", derived)
	}
	if a := testing.AllocsPerRun(100, func() { _ = h.Enabled(ctx, slog.LevelDebug) }); a != 0 {
		t.Fatalf("Enabled allocates %v per call", a)
	}
}

// BenchmarkDisabledDebugThroughTheSwap is a LiveKit Debugw on a logger with five bound attributes
// while dillad logs at info: logr asks Enabled and stops there.
func BenchmarkDisabledDebugThroughTheSwap(b *testing.B) {
	var buf bytes.Buffer
	sink := obs.NewLogger(dillaconfig.Log{Level: "info", Format: "json"}, &buf).Handler()
	liveKitSink.Store(&sink)
	b.Cleanup(func() { d := slog.DiscardHandler; liveKitSink.Store(&d) })
	l := logger.LogRLogger(logr.FromSlogHandler(NewLogBridge(&swapHandler{}))).WithName("livekit").
		WithValues("room", "r", "participant", "p", "participantID", "PA_x", "trackID", "TR_x", "kind", "audio")
	b.ReportAllocs()
	for b.Loop() {
		l.Debugw("forwarding packet", "sn", 1)
	}
}

// Re-review N2: the bucket map is bounded, but a full map never hides a new ERROR behind the
// overflow of lower levels. 512 distinct INFO messages fill the map, 30 more exhaust the INFO
// overflow, and a first-ever ERROR is still written; once the buckets have refilled, a new message
// evicts an idle bucket and gets its own; the overflow's summary names its level.
func TestAFullBucketMapNeverHidesANewError(t *testing.T) {
	var buf bytes.Buffer
	clk := &fakeNow{t: time.Unix(1_790_000_000, 0)}
	sink := obs.NewLogger(dillaconfig.Log{Level: "debug", Format: "json"}, &buf).Handler()
	h := newLogBridge(sink, clk.now)
	l := logger.LogRLogger(logr.FromSlogHandler(h)).WithName("livekit")
	for i := range maxBuckets {
		l.Infow(fmt.Sprintf("message %d", i))
	}
	for i := range 30 {
		l.Infow(fmt.Sprintf("overflow %d", i))
	}
	before := len(lines(&buf))
	l.Errorw("a first-ever error", errors.New("boom"))
	got := records(t, &buf)
	if len(got) != before+1 || got[len(got)-1]["msg"] != "a first-ever error" {
		t.Fatalf("a new ERROR after %d INFO keys and a busy INFO overflow was not written (%d lines, %d before)",
			maxBuckets+30, len(got), before)
	}
	clk.advance(suppressionInterval)
	l.Infow("a new message once the buckets refilled")
	limits := h.(logBridge).limits
	limits.mu.Lock()
	_, own := limits.buckets[slog.LevelInfo.String()+"\x00livekit\x00a new message once the buckets refilled"]
	n := len(limits.buckets)
	limits.mu.Unlock()
	if !own || n > maxBuckets+3 {
		t.Errorf("own bucket %t, %d buckets: an idle bucket was not evicted for the new message", own, n)
	}
	var summary map[string]any
	for _, r := range records(t, &buf) {
		if r["msg"] == "LiveKit log lines suppressed" {
			summary = r
		}
	}
	if summary == nil || summary["suppressed_level"] != "INFO" || summary["suppressed"] != float64(10) {
		t.Errorf("overflow summary %v, want 10 suppressed at INFO", summary)
	}
}
