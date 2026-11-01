package sfu

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/jonasthim/dilla/internal/id"
)

var (
	srcMic         = livekit.TrackSource_MICROPHONE
	srcCam         = livekit.TrackSource_CAMERA
	srcScreen      = livekit.TrackSource_SCREEN_SHARE
	srcScreenAudio = livekit.TrackSource_SCREEN_SHARE_AUDIO
)

// G26: one pure function maps speak/video/screen_share onto LiveKit's sources, in a fixed order, and
// never says "none" with an empty list (GetCanPublishSource reads [] as every source).
func TestPublishGrantMirrorsSpeakVideoAndScreenShare(t *testing.T) {
	for _, tc := range []struct {
		speak, video, screen bool
		want                 []livekit.TrackSource
	}{
		{false, false, false, nil},
		{true, false, false, []livekit.TrackSource{srcMic}},
		{false, true, false, []livekit.TrackSource{srcCam}},
		{false, false, true, []livekit.TrackSource{srcScreen, srcScreenAudio}},
		{true, true, false, []livekit.TrackSource{srcMic, srcCam}},
		{true, false, true, []livekit.TrackSource{srcMic, srcScreen, srcScreenAudio}},
		{false, true, true, []livekit.TrackSource{srcCam, srcScreen, srcScreenAudio}},
		{true, true, true, []livekit.TrackSource{srcMic, srcCam, srcScreen, srcScreenAudio}},
	} {
		p := PublishGrant(tc.speak, tc.video, tc.screen)
		if !slices.Equal(p.CanPublishSources, tc.want) {
			t.Errorf("PublishGrant(%v, %v, %v) sources = %v, want %v", tc.speak, tc.video, tc.screen, p.CanPublishSources, tc.want)
		}
		if p.CanPublish != (len(tc.want) > 0) {
			t.Errorf("PublishGrant(%v, %v, %v).CanPublish = %v", tc.speak, tc.video, tc.screen, p.CanPublish)
		}
		if tc.want == nil && p.CanPublishSources != nil {
			t.Error("a listen-only grant carries a source list: LiveKit reads an empty list as every source")
		}
		if !p.CanSubscribe || p.CanPublishData || p.Hidden || p.CanUpdateMetadata || p.CanSubscribeMetrics || p.CanManageAgentSession {
			t.Errorf("PublishGrant(%v, %v, %v) = %+v: subscribe only, no data, never hidden", tc.speak, tc.video, tc.screen, p)
		}
		// What LiveKit stores from it agrees, source by source.
		var g auth.VideoGrant
		g.UpdateFromPermission(p)
		if g.GetCanPublishSource(srcMic) != tc.speak || g.GetCanPublishSource(srcCam) != tc.video ||
			g.GetCanPublishSource(srcScreen) != tc.screen || g.GetCanPublishSource(srcScreenAudio) != tc.screen || g.GetCanPublishData() {
			t.Errorf("the stored grant of PublishGrant(%v, %v, %v) disagrees: %+v", tc.speak, tc.video, tc.screen, g)
		}
	}
}

// MD-9: the token carries exactly the permission and the dilla.vdec attribute, and VerifyToken
// answers its identity and room only for a room-join token this server signed.
func TestATokenCarriesThePermissionAndTheAttributes(t *testing.T) {
	s := &Server{cfg: testConfig()}
	tok, err := s.Token("room-1", "dev", PublishGrant(true, false, true), map[string]string{"dilla.vdec": "vp8,h264"})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	v, err := auth.ParseAPIToken(tok)
	if err != nil {
		t.Fatalf("ParseAPIToken: %v", err)
	}
	_, grants, err := v.Verify(s.cfg.APISecret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !grants.Video.RoomJoin || grants.Video.Room != "room-1" {
		t.Fatalf("video grant = %+v", grants.Video)
	}
	if got := grants.Video.GetCanPublishSources(); !slices.Equal(got, []livekit.TrackSource{srcMic, srcScreen, srcScreenAudio}) {
		t.Errorf("sources = %v", got)
	}
	if grants.Video.GetCanPublishData() || !grants.Video.GetCanSubscribe() {
		t.Error("the call token grants data or withholds subscribe")
	}
	if grants.Attributes["dilla.vdec"] != "vp8,h264" {
		t.Errorf("attributes = %v", grants.Attributes)
	}
	rt, err := s.VerifyToken(tok)
	if err != nil || rt.Identity != "dev" || rt.Room != "room-1" || rt.Claims.Attributes["dilla.vdec"] != "vp8,h264" {
		t.Fatalf("VerifyToken = %+v, %v", rt, err)
	}

	other := &Server{cfg: testConfig()}
	other.cfg.APISecret = "another-secret-0123456789abcdefghij"
	forged, err := other.Token("room-1", "dev", PublishGrant(true, true, true), nil)
	if err != nil {
		t.Fatalf("Token(other): %v", err)
	}
	if _, err := s.VerifyToken(forged); err == nil {
		t.Error("a token signed with another secret was verified")
	}
	admin, err := auth.NewAccessToken(s.cfg.APIKey, s.cfg.APISecret).
		SetVideoGrant(&auth.VideoGrant{RoomAdmin: true, Room: "room-1"}).SetValidFor(time.Minute).ToJWT()
	if err != nil {
		t.Fatalf("admin token: %v", err)
	}
	if _, err := s.VerifyToken(admin); err == nil {
		t.Error("an admin token without roomJoin passed as a join token")
	}
	if _, err := s.VerifyToken(""); err == nil {
		t.Error("an empty token was verified")
	}
	if _, err := s.Token("room-1", "dev", nil, nil); err == nil {
		t.Error("a token was minted without a permission")
	}
}

// The /rtc gate's comparison: a token confers nothing beyond the device's current permission. A
// microphone token against a revoked speak, a camera token against the base grant, and every right a
// call token never carries are refused; a resume skips only the per-source comparison.
func TestTokenWithinRefusesAnythingBeyondTheCurrentGrant(t *testing.T) {
	s := &Server{cfg: testConfig()}
	claims := func(perm *livekit.ParticipantPermission) *auth.ClaimGrants {
		t.Helper()
		tok, err := s.Token("room-1", "dev", perm, nil)
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		rt, err := s.VerifyToken(tok)
		if err != nil {
			t.Fatalf("VerifyToken: %v", err)
		}
		return rt.Claims
	}
	base, listen, sharer := PublishGrant(true, false, false), PublishGrant(false, false, false), PublishGrant(true, true, true)
	for _, tc := range []struct {
		name        string
		token, now  *livekit.ParticipantPermission
		withSources bool
		ok          bool
	}{
		{"the base grant against itself", base, base, true, true},
		{"listen-only against the base grant", listen, base, true, true},
		{"a microphone after speak was revoked", base, listen, true, false},
		{"a camera token against the base grant", sharer, base, true, false},
		{"a camera token on a resume", sharer, base, false, true},
	} {
		err := TokenWithin(claims(tc.token), tc.now, tc.withSources)
		if (err == nil) != tc.ok {
			t.Errorf("%s: TokenWithin = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	wide := claims(base)
	wide.Video.SetCanPublishSources(nil) // an empty list is every source
	if TokenWithin(wide, base, true) == nil {
		t.Error("a token with no source list (every source) passed")
	}
	for name, mutate := range map[string]func(*auth.ClaimGrants){
		"data":     func(c *auth.ClaimGrants) { c.Video.SetCanPublishData(true) },
		"metadata": func(c *auth.ClaimGrants) { c.Video.SetCanUpdateOwnMetadata(true) },
		"hidden":   func(c *auth.ClaimGrants) { c.Video.Hidden = true },
		"admin":    func(c *auth.ClaimGrants) { c.Video.RoomAdmin = true },
		"agent":    func(c *auth.ClaimGrants) { c.Kind = "agent" },
		"preset":   func(c *auth.ClaimGrants) { c.RoomPreset = "x" },
	} {
		c := claims(base)
		mutate(c)
		if TokenWithin(c, base, false) == nil {
			t.Errorf("a token with %s passed, even on a resume", name)
		}
	}
}

// bootGrantSFU starts an SFU on port/port+2 for one test and stops it when the test ends.
func bootGrantSFU(t *testing.T, port int, tune func(*Config)) *Server {
	t.Helper()
	c := testConfig()
	c.Port, c.UDPPort = port, port+2
	if tune != nil {
		tune(&c)
	}
	srv, err := Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return srv
}

// rtcUpgradeStatus asks LiveKit's v0 signalling path for a WebSocket upgrade with token and returns
// the refusal's status and body. It is only for joins that must fail: a 101 would leave the body open.
func rtcUpgradeStatus(t *testing.T, srv *Server, token string) (int, string) {
	t.Helper()
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.HTTPURL()+"/rtc?access_token="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body)
}

// pumpSamples writes data to track every `every` until the test ends: LiveKit announces a
// publication to the room only once its RTP arrives.
func pumpSamples(t *testing.T, track *lksdk.LocalTrack, data []byte, every time.Duration) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = track.WriteSample(media.Sample{Data: data, Duration: every}, nil)
			}
		}
	}()
}

// opusSilence is LiveKit's own Opus silence frame (downtrack.go OpusSilenceFrame, first three bytes).
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// vp8Key is a VP8 key-frame header (keyframe bit clear, start code 9d 01 2a, 320×240) with padding:
// enough for the SFU's key-frame detector, which reads nothing past byte 9.
var vp8Key = append([]byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00}, make([]byte, 100)...)

func publishMic(t *testing.T, room *lksdk.Room, name string) *lksdk.LocalTrackPublication {
	t.Helper()
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("NewLocalSampleTrack(opus): %v", err)
	}
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: name, Source: srcMic})
	if err != nil {
		t.Fatalf("PublishTrack(%s): %v", name, err)
	}
	pumpSamples(t, track, opusSilence, 20*time.Millisecond)
	return pub
}

func publishCamera(t *testing.T, room *lksdk.Room, name string) *lksdk.LocalTrackPublication {
	t.Helper()
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000})
	if err != nil {
		t.Fatalf("NewLocalSampleTrack(vp8): %v", err)
	}
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: name, Source: srcCam, VideoWidth: 320, VideoHeight: 240,
	})
	if err != nil {
		t.Fatalf("PublishTrack(%s): %v", name, err)
	}
	pumpSamples(t, track, vp8Key, 33*time.Millisecond)
	return pub
}

// trackEvents is an observer's view of remote publications, by track name.
type trackEvents struct {
	mu          sync.Mutex
	published   map[string]bool
	unpublished map[string]bool
}

func (e *trackEvents) callback() *lksdk.RoomCallback {
	e.published, e.unpublished = map[string]bool{}, map[string]bool{}
	return &lksdk.RoomCallback{ParticipantCallback: lksdk.ParticipantCallback{
		OnTrackPublished: func(p *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.published[p.Name()] = true
		},
		OnTrackUnpublished: func(p *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.unpublished[p.Name()] = true
		},
	}}
}

func (e *trackEvents) wait(t *testing.T, what string, within time.Duration, seen func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		ok := seen()
		e.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, within)
}

func sfuEventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, within)
}

// SP-17: room.max_participants is LiveKit's authoritative cap. The joiner past it gets HTTP 500
// "connection closed by media" on the upgrade, never a "room is full" signal — which is why the call
// route adds the advisory E_CALL_FULL count.
func TestAFullRoomRefusesTheNextJoinerWithAnHTTP500(t *testing.T) {
	srv := bootGrantSFU(t, 7930, func(c *Config) { c.MaxParticipants = 1 })
	const room = "sp17-room"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	aliceTok, _ := srv.Token(room, "alice", PublishGrant(true, false, false), nil)
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), aliceTok, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()
	bobTok, _ := srv.Token(room, "bob", PublishGrant(true, false, false), nil)
	status, body := rtcUpgradeStatus(t, srv, bobTok)
	t.Logf("SP-17 second joiner in a room of max_participants 1: status=%d body=%q", status, body)
	if status != http.StatusInternalServerError || !strings.Contains(body, "connection closed by media") {
		t.Fatalf("the joiner past the cap got %d %q, want 500 \"connection closed by media\"", status, body)
	}
}

// DEV-44 / MD-16: with room.auto_create false the SFU never creates a room on a join, so a token for
// a room nobody created — or one the instance deleted — is refused 404; CreateRoom opens it, and a
// second CreateRoom of a live room leaves its participants alone.
func TestARoomNobodyCreatedCannotBeJoined(t *testing.T) {
	srv := bootGrantSFU(t, 7934, nil)
	if srv.cfg.AutoCreate {
		t.Fatal("DefaultConfig still renders room.auto_create: true")
	}
	const room = "never-created"
	tok, _ := srv.Token(room, "alice", PublishGrant(true, false, false), nil)
	status, body := rtcUpgradeStatus(t, srv, tok)
	if status != http.StatusNotFound || !strings.Contains(body, "requested room does not exist") {
		t.Fatalf("a join to a room nobody created = %d %q, want 404 \"requested room does not exist\"", status, body)
	}
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	var once sync.Once
	gone := make(chan struct{})
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), tok, &lksdk.RoomCallback{
		OnDisconnected: func() { once.Do(func() { close(gone) }) },
	})
	if err != nil {
		t.Fatalf("join after CreateRoom: %v", err)
	}
	defer alice.Disconnect()
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("a second CreateRoom of a live room: %v", err)
	}
	select {
	case <-gone:
		t.Fatal("a second CreateRoom disconnected the room's participant")
	case <-time.After(2 * time.Second):
	}
}

// SP-19: a grant change reaches a connected participant without a reconnect — revoking speak
// unpublishes the mic within 2 s — and a re-grant lets it publish again in the same session.
func TestAGrantChangeTakesEffectWithoutAReconnect(t *testing.T) {
	srv := bootGrantSFU(t, 7938, nil)
	const room = "sp19-room"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	var seen trackEvents
	bobTok, _ := srv.Token(room, "bob", PublishGrant(false, false, false), nil)
	bob, err := lksdk.ConnectToRoomWithToken(srv.URL(), bobTok, seen.callback())
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	defer bob.Disconnect()
	aliceTok, _ := srv.Token(room, "alice", PublishGrant(true, false, false), nil)
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), aliceTok, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()
	publishMic(t, alice, "mic-1")
	seen.wait(t, "bob seeing alice's mic", 10*time.Second, func() bool { return seen.published["mic-1"] })

	started := time.Now()
	if err := srv.UpdatePermission(t.Context(), room, "alice", PublishGrant(false, false, false)); err != nil {
		t.Fatalf("UpdatePermission(revoke): %v", err)
	}
	seen.wait(t, "the mic unpublishing after speak was revoked", 2*time.Second, func() bool { return seen.unpublished["mic-1"] })
	t.Logf("SP-19 revoke-to-unpublish=%s", time.Since(started))
	sfuEventually(t, "alice's own permission showing canPublish false", 2*time.Second, func() bool {
		return !alice.LocalParticipant.Permissions().GetCanPublish()
	})

	if err := srv.UpdatePermission(t.Context(), room, "alice", PublishGrant(true, false, false)); err != nil {
		t.Fatalf("UpdatePermission(re-grant): %v", err)
	}
	sfuEventually(t, "alice's permission showing the mic again", 2*time.Second, func() bool {
		return slices.Contains(alice.LocalParticipant.Permissions().GetCanPublishSources(), srcMic)
	})
	publishMic(t, alice, "mic-2")
	seen.wait(t, "bob seeing the re-granted mic", 5*time.Second, func() bool { return seen.published["mic-2"] })
	// Also under -race (the promotion test below is not): an identity the room does not hold.
	if err := srv.UpdatePermission(t.Context(), room, "nobody", PublishGrant(true, false, false)); !errors.Is(err, ErrNoParticipant) {
		t.Fatalf("UpdatePermission of an absent identity = %v, want ErrNoParticipant", err)
	}
}

// SP-18: promotion is a complete permission that adds the video sources, and the camera then
// publishes; demotion takes them away and the camera is unpublished. An identity the room does not
// hold is ErrNoParticipant, which the lease rolls back on.
func TestPromotionAddsTheVideoSourcesAndDemotionRemovesThem(t *testing.T) {
	if raceEnabled {
		// Ruling I8: a test that publishes video into the pinned LiveKit is gated off the race
		// detector (upstream updateRidsFromSDP race) and runs in the non-race step.
		t.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")
	}
	srv := bootGrantSFU(t, 7942, nil)
	const room = "sp18-room"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	var seen trackEvents
	bobTok, _ := srv.Token(room, "bob", PublishGrant(false, false, false), nil)
	bob, err := lksdk.ConnectToRoomWithToken(srv.URL(), bobTok, seen.callback())
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	defer bob.Disconnect()
	aliceTok, _ := srv.Token(room, "alice", PublishGrant(true, false, false), nil)
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), aliceTok, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()

	started := time.Now()
	if err := srv.UpdatePermission(t.Context(), room, "alice", PublishGrant(true, true, true)); err != nil {
		t.Fatalf("promote: %v", err)
	}
	sfuEventually(t, "alice's permission showing the camera", 2*time.Second, func() bool {
		return slices.Contains(alice.LocalParticipant.Permissions().GetCanPublishSources(), srcCam)
	})
	t.Logf("SP-18 promote-to-ParticipantPermissionsChanged=%s", time.Since(started))
	publishCamera(t, alice, "camera")
	seen.wait(t, "bob seeing alice's camera", 10*time.Second, func() bool { return seen.published["camera"] })

	if err := srv.UpdatePermission(t.Context(), room, "alice", PublishGrant(true, false, false)); err != nil {
		t.Fatalf("demote: %v", err)
	}
	seen.wait(t, "the camera unpublishing after demotion", 2*time.Second, func() bool { return seen.unpublished["camera"] })

	if err := srv.UpdatePermission(t.Context(), room, "nobody", PublishGrant(true, true, true)); !errors.Is(err, ErrNoParticipant) {
		t.Fatalf("UpdatePermission of an absent identity = %v, want ErrNoParticipant", err)
	}
}

// G29: RemoveParticipants takes the device and every "<device>#…" shadow, and nobody else.
func TestRemoveParticipantsTakesTheDeviceAndItsShadows(t *testing.T) {
	srv := bootGrantSFU(t, 7946, nil)
	const room = "evict-room"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	dev, other := id.New(), id.New()
	join := func(identity string) *lksdk.Room {
		tok, _ := srv.Token(room, identity, PublishGrant(true, false, false), nil)
		r, err := lksdk.ConnectToRoomWithToken(srv.URL(), tok, &lksdk.RoomCallback{})
		if err != nil {
			t.Fatalf("join %s: %v", identity, err)
		}
		t.Cleanup(r.Disconnect)
		return r
	}
	join(dev.String())
	join(dev.String() + "#shadow")
	join(other.String())
	join("intruder")
	sfuEventually(t, "four participants listed", 5*time.Second, func() bool {
		parts, err := srv.Participants(t.Context(), room)
		return err == nil && len(parts) == 4
	})
	if err := srv.RemoveParticipants(t.Context(), room, dev); err != nil {
		t.Fatalf("RemoveParticipants: %v", err)
	}
	sfuEventually(t, "the other device and the intruder left", 5*time.Second, func() bool {
		parts, err := srv.Participants(t.Context(), room)
		return err == nil && len(parts) == 2
	})
	// RemoveParticipant takes exactly one identity; one already gone is not an error.
	if err := srv.RemoveParticipant(t.Context(), room, "intruder"); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	sfuEventually(t, "only the other device left", 5*time.Second, func() bool {
		parts, err := srv.Participants(t.Context(), room)
		return err == nil && len(parts) == 1 && parts[0].GetIdentity() == other.String()
	})
	if err := srv.RemoveParticipant(t.Context(), room, "intruder"); err != nil {
		t.Fatalf("RemoveParticipant of an identity already gone: %v", err)
	}
	if parts, err := srv.Participants(t.Context(), "no-such-room"); err != nil || len(parts) != 0 {
		t.Fatalf("Participants of an unknown room = %v, %v; want none and no error", parts, err)
	}
}

// DEV-61: the call token carries no data grant, so LiveKit drops a data packet from it.
func TestTheCallTokenCannotPublishData(t *testing.T) {
	srv := bootGrantSFU(t, 7950, nil)
	const room = "no-data-room"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	received := make(chan struct{}, 1)
	bobTok, _ := srv.Token(room, "bob", PublishGrant(true, true, true), nil)
	bob, err := lksdk.ConnectToRoomWithToken(srv.URL(), bobTok, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataPacket: func(lksdk.DataPacket, lksdk.DataReceiveParams) {
				select {
				case received <- struct{}{}:
				default:
				}
			},
		},
	})
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	defer bob.Disconnect()
	aliceTok, _ := srv.Token(room, "alice", PublishGrant(true, true, true), nil)
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), aliceTok, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		_ = alice.LocalParticipant.PublishDataPacket(lksdk.UserData([]byte("x")), lksdk.WithDataPublishReliable(true))
		select {
		case <-received:
			t.Fatal("bob received a data packet from a call token")
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}

// Rooms lists every room LiveKit holds (the room sweep's input): a created room is listed, a deleted
// one is not.
func TestRoomsListsTheRoomsTheSFUHolds(t *testing.T) {
	srv := bootGrantSFU(t, 7954, nil)
	for _, r := range []string{"room-a", "room-b"} {
		if err := srv.CreateRoom(t.Context(), r); err != nil {
			t.Fatalf("CreateRoom %s: %v", r, err)
		}
	}
	got, err := srv.Rooms(t.Context())
	slices.Sort(got)
	if err != nil || !slices.Equal(got, []string{"room-a", "room-b"}) {
		t.Fatalf("Rooms = %v, %v; want room-a and room-b", got, err)
	}
	if err := srv.DeleteRoom(t.Context(), "room-a"); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	if got, err := srv.Rooms(t.Context()); err != nil || !slices.Equal(got, []string{"room-b"}) {
		t.Fatalf("Rooms after a delete = %v, %v; want room-b", got, err)
	}
}

// Task 8c fix round 2 (security review): a call token cannot touch its own attributes, name or metadata — many small updates
// with new keys never reach another participant — and dillad's admin UpdateAttributes (the RTT route) is how dilla.rtt gets
// there.
func TestCallTokensCannotSetAttributesAndTheServerCan(t *testing.T) {
	srv := bootGrantSFU(t, 7990, nil)
	const room = "rtt-room"
	if err := srv.CreateRoom(t.Context(), room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	mint := func(who string) string {
		tok, err := srv.Token(room, who, PublishGrant(false, false, false), nil)
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		return tok
	}
	bob, err := lksdk.ConnectToRoomWithToken(srv.URL(), mint("bob"), &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	defer bob.Disconnect()
	alice, err := lksdk.ConnectToRoomWithToken(srv.URL(), mint("alice"), &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	defer alice.Disconnect()
	seen := func() *lksdk.RemoteParticipant { return bob.GetParticipantByIdentity("alice") }
	waitFor := func(what string, ok func(p *lksdk.RemoteParticipant) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if p := seen(); p != nil && ok(p) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("bob never saw %s", what)
	}
	waitFor("alice", func(*lksdk.RemoteParticipant) bool { return true })
	// A listen-only token (the narrowest one) tries the flood: many small updates, each a new key, and a rename.
	for i := range 50 {
		alice.LocalParticipant.SetAttributes(map[string]string{fmt.Sprintf("k%d", i): "x"})
	}
	alice.LocalParticipant.SetName("mallory")
	alice.LocalParticipant.SetMetadata("x")
	// The server's write lands; the client's never did (it was sent first, on the same signal connection).
	if err := srv.UpdateAttributes(t.Context(), room, "alice", map[string]string{"dilla.rtt": "24"}); err != nil {
		t.Fatalf("UpdateAttributes: %v", err)
	}
	waitFor("dilla.rtt from the server", func(p *lksdk.RemoteParticipant) bool { return p.Attributes()["dilla.rtt"] == "24" })
	time.Sleep(time.Second) // anything the client's updates would have broadcast has arrived by now
	p := seen()
	if len(p.Attributes()) != 1 || p.Name() != "alice" || p.Metadata() != "" {
		t.Errorf("alice: attributes %v name %q metadata %q; want only dilla.rtt, alice, none", p.Attributes(), p.Name(), p.Metadata())
	}
	if err := srv.UpdateAttributes(t.Context(), room, "nobody", map[string]string{"dilla.rtt": "1"}); !errors.Is(err, ErrNoParticipant) {
		t.Errorf("UpdateAttributes for an identity the room lacks = %v, want ErrNoParticipant", err)
	}
	if err := srv.UpdateAttributes(t.Context(), room, "alice", map[string]string{"dilla.rtt": ""}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	waitFor("the clear", func(p *lksdk.RemoteParticipant) bool { _, ok := p.Attributes()["dilla.rtt"]; return !ok })
}
