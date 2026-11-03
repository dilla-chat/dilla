package media

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"

	"github.com/dilla-chat/dilla/internal/sframe"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

var (
	base  = [16]byte{0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a}
	alice = [16]byte{0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1, 0xa1}
	bob   = [16]byte{0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0, 0xb0}
	// roster is epoch 1 of a call alice (leaf 0) and bob (leaf 1) are in.
	roster = []sframe.RosterEntry{{Leaf: 0, Device: alice}, {Leaf: 1, Device: bob}}
)

// stableSPS is task 4's hand-built libwebrtc-stable SPS (internal/sframe/h264_test.go).
const stableSPS = "6742c01fda0280f68078442350"

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func TestTheZeroValueOfCountersIsReadyToUse(t *testing.T) {
	var c Counters
	n, dropped := c.Snapshot()
	if n != 0 || dropped == nil || len(dropped) != 0 {
		t.Fatalf("zero Counters: %d %v, want 0 and an empty non-nil map", n, dropped)
	}
	c.addDropped("sif")
	c.addDecrypted()
	n, dropped = c.Snapshot()
	dropped["sif"] = 99 // the snapshot is a copy
	if again, d2 := c.Snapshot(); n != 1 || again != 1 || d2["sif"] != 1 {
		t.Fatalf("after one drop and one decrypt: %d, %v", again, d2)
	}
}

func TestEmptyAudioPassesAdaptersWithoutSpendingCounterOrTouchingKeyRing(t *testing.T) {
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	enc := NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0)
	for _, input := range [][]byte{nil, {}} {
		out, err := enc.EncryptFrame(input)
		if err != nil || len(out) != 0 {
			t.Fatalf("empty encrypt = %x, %v", out, err)
		}
	}
	sealed, err := enc.EncryptFrame([]byte{0xfc})
	if err != nil {
		t.Fatal(err)
	}
	_, ctr, _, err := sframe.DecodeHeader(sealed)
	if err != nil || ctr != 0 {
		t.Fatalf("first real counter = %d, %v; want 0", ctr, err)
	}
	var counters Counters
	dec := NewFrameDecryptor(nil, sframe.Opus, alice, sframe.Mic, nil, &counters)
	out, err := dec.DecryptFrame([]byte{})
	if err != nil || out == nil || len(out) != 0 {
		t.Fatalf("empty decrypt = %x, %v; want non-nil empty", out, err)
	}
	if counters.EmptyFrames() != 1 {
		t.Fatalf("empty frames = %d; want 1", counters.EmptyFrames())
	}
	if n, dropped := counters.Snapshot(); n != 0 || len(dropped) != 0 {
		t.Fatalf("decrypted=%d dropped=%v", n, dropped)
	}
	video := NewFrameEncryptor(sender, sframe.VP8, sframe.Camera, 0)
	if out, err := video.EncryptFrame([]byte{}); err == nil && len(out) == 0 {
		t.Fatal("empty video passed through")
	}
	videoDec := NewFrameDecryptor(sframe.NewKeyRing(nil), sframe.VP8, alice, sframe.Camera, nil, &counters)
	if out, _ := videoDec.DecryptFrame([]byte{}); out != nil {
		t.Fatal("empty video decrypt passed through")
	}
}

// RIGS-05: each clause of the pass-through condition is pinned on its own. Only an empty Opus frame
// on an audio slot (microphone or screen audio) passes; an empty Opus frame on a video slot and an
// empty frame of another codec on an audio slot are encrypted, refused or dropped, never passed.
func TestEveryClauseOfTheEmptyAudioExceptionIsPinned(t *testing.T) {
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		codec  sframe.Codec
		slot   sframe.Slot
		passes bool
	}{
		{"opus on the microphone", sframe.Opus, sframe.Mic, true},
		{"opus on screen audio", sframe.Opus, sframe.ScreenAudio, true},
		{"opus on the camera", sframe.Opus, sframe.Camera, false},
		{"opus on screen video", sframe.Opus, sframe.ScreenVideo, false},
		{"vp8 on the microphone", sframe.VP8, sframe.Mic, false},
		{"h264 on screen audio", sframe.H264, sframe.ScreenAudio, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NewFrameEncryptor(sender, tc.codec, tc.slot, 0).EncryptFrame([]byte{})
			if passed := err == nil && len(out) == 0; passed != tc.passes {
				t.Fatalf("encrypt of an empty frame = %x, %v; passed through %v, want %v", out, err, passed, tc.passes)
			}
			var counters Counters
			dec := NewFrameDecryptor(sframe.NewKeyRing(nil), tc.codec, alice, tc.slot, nil, &counters)
			got, err := dec.DecryptFrame([]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if passed := got != nil; passed != tc.passes {
				t.Fatalf("decrypt of an empty frame = %x; passed through %v, want %v", got, passed, tc.passes)
			}
			if _, dropped := counters.Snapshot(); (counters.EmptyFrames() == 1) != tc.passes || (len(dropped) == 1) == tc.passes {
				t.Fatalf("empty frames %d, dropped %v", counters.EmptyFrames(), dropped)
			}
		})
	}
}

// Every codec through the two adapters and back; the replayed copy is dropped and counted, never an
// error the SDK would act on.
func TestAFrameRoundTripsThroughTheAdapters(t *testing.T) {
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ring := sframe.NewKeyRing(nil)
	ring.InstallEpoch(1, base, roster, 1)
	for _, c := range []struct {
		name  string
		codec sframe.Codec
		slot  sframe.Slot
		frame []byte
	}{
		{"opus", sframe.Opus, sframe.Mic, []byte{0xfc, 1, 2, 3}},
		{"vp8 key", sframe.VP8, sframe.Camera, unhex(t, "5002009d012a8002e001ff00")},
		{"h264 idr", sframe.H264, sframe.ScreenVideo,
			unhex(t, "00000001"+stableSPS+"0000000168ce3c800000000165888421ff00000312345a5a5a5a")},
	} {
		sealed, err := NewFrameEncryptor(sender, c.codec, c.slot, 0).EncryptFrame(c.frame)
		if err != nil {
			t.Fatalf("%s: EncryptFrame: %v", c.name, err)
		}
		var counters Counters
		dec := NewFrameDecryptor(ring, c.codec, alice, c.slot, []byte("trailer-not-present-in-this-frame-0123456789"), &counters)
		out, err := dec.DecryptFrame(sealed)
		if err != nil || !bytes.Equal(out, c.frame) {
			t.Fatalf("%s: DecryptFrame = %x, %v, want %x", c.name, out, err, c.frame)
		}
		if out, err := dec.DecryptFrame(sealed); out != nil || err != nil {
			t.Fatalf("%s: the replayed frame = %x, %v, want nil, nil", c.name, out, err)
		}
		n, dropped := counters.Snapshot()
		if n != 1 || dropped["E_SFRAME_REPLAY"] != 1 || len(dropped) != 1 {
			t.Fatalf("%s: decrypted %d, dropped %v", c.name, n, dropped)
		}
	}
	// A frame on another device's track is a sender mismatch, on another source's track a slot
	// mismatch: both dropped and counted by their code.
	sealed, err := NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0).EncryptFrame([]byte{0xfc, 9})
	if err != nil {
		t.Fatal(err)
	}
	var counters Counters
	if out, err := NewFrameDecryptor(ring, sframe.Opus, bob, sframe.Mic, nil, &counters).DecryptFrame(sealed); out != nil || err != nil {
		t.Fatalf("on bob's track: %x, %v", out, err)
	}
	if out, err := NewFrameDecryptor(ring, sframe.Opus, alice, sframe.ScreenAudio, nil, &counters).DecryptFrame(sealed); out != nil || err != nil {
		t.Fatalf("on the screen-audio track: %x, %v", out, err)
	}
	if _, dropped := counters.Snapshot(); dropped["E_SFRAME_SENDER_MISMATCH"] != 1 || dropped["E_SFRAME_SLOT_MISMATCH"] != 1 {
		t.Fatalf("dropped %v", dropped)
	}
}

func TestLastSampleIsDecryptedWhenTheTrackEnds(t *testing.T) {
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0).EncryptFrame([]byte{0xfc, 1})
	if err != nil {
		t.Fatal(err)
	}
	ring := sframe.NewKeyRing(nil)
	ring.InstallEpoch(1, base, roster, 1)
	var counters Counters
	joiner := frameJoiner{decryptor: NewFrameDecryptor(ring, sframe.Opus, alice, sframe.Mic, nil, &counters)}
	joiner.add(sealed, 123)
	if n, _ := counters.Snapshot(); n != 0 {
		t.Fatalf("decrypted before timestamp transition: %d, want 0", n)
	}
	joiner.finish()
	if n, dropped := counters.Snapshot(); n != 1 || len(dropped) != 0 {
		t.Fatalf("after EOF: decrypted %d, dropped %v; want one decrypted frame", n, dropped)
	}
}

// LiveKit's injected Opus silence ends in the room's SIF trailer and starts with f8, which parses as
// an SFrame config byte: the trailer is tested before any parse, so nothing else is counted.
func TestAFrameEndingInTheSIFTrailerIsDroppedBeforeItIsParsed(t *testing.T) {
	sif := []byte("Q2hlY2tUcmFpbGVyMDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1u")
	var counters Counters
	dec := NewFrameDecryptor(sframe.NewKeyRing(nil), sframe.Opus, alice, sframe.Mic, sif, &counters)
	silence := append([]byte{0xf8, 0xff, 0xfe}, make([]byte, 77)...)
	if out, err := dec.DecryptFrame(append(silence, sif...)); out != nil || err != nil {
		t.Fatalf("a SIF frame = %x, %v, want nil, nil", out, err)
	}
	if _, dropped := counters.Snapshot(); dropped["sif"] != 1 || len(dropped) != 1 {
		t.Fatalf("dropped %v, want only sif", dropped)
	}
}

// nalTypes lists the NAL unit types of an access unit the builder made (4-byte start codes only).
func nalTypes(au []byte) []byte {
	var types []byte
	for _, nal := range bytes.Split(au, []byte{0, 0, 0, 1})[1:] {
		if len(nal) > 0 {
			types = append(types, nal[0]&0x1f)
		}
	}
	return types
}

func TestTheAccessUnitBuilderEmitsCanonicalAccessUnits(t *testing.T) {
	p, err := newH264AUProvider("testdata/bars.h264")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for i := range 150 { // 5 s at 30 fps; the fixture's GOP is 60
		s, err := p.NextSample(t.Context())
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if !bytes.HasPrefix(s.Data, []byte{0, 0, 0, 1}) || s.Duration != time.Second/30 {
			t.Fatalf("sample %d: %x… lasting %s, want a start code and 1/30 s", i, s.Data[:min(8, len(s.Data))], s.Duration)
		}
		types := nalTypes(s.Data)
		switch types[0] {
		case 7:
			if len(types) < 3 || types[1] != 8 || types[2] != 5 {
				t.Fatalf("sample %d: key access unit types %v, want SPS, PPS, IDR…", i, types)
			}
			keys++
			sps := bytes.Split(s.Data, []byte{0, 0, 0, 1})[1]
			if err := sframe.CheckSPSVUI(sps); err != nil {
				t.Fatalf("sample %d: the builder's SPS %x: %v", i, sps, err)
			}
		case 1:
		default:
			t.Fatalf("sample %d: first NAL type %d", i, types[0])
		}
		for _, ty := range types {
			if ty == 6 || ty == 9 || ty == 12 {
				t.Fatalf("sample %d carries NAL type %d (SEI, AUD or filler)", i, ty)
			}
		}
		if _, err := sender.Encrypt(sframe.H264, sframe.ScreenVideo, 0, s.Data); err != nil {
			t.Fatalf("sample %d does not encrypt: %v", i, err)
		}
	}
	if keys < 2 {
		t.Fatalf("%d key access units in 150, want at least 2", keys)
	}
}

// SP-04's Go leg (deferred to this task): the raw x264 SPS of the fixture and the builder's rewrite,
// each with its verdict, on one line Step 12 copies into the SP-04 document.
func TestTheRawFixtureSPSVerdictIsRecorded(t *testing.T) {
	f, err := os.Open("testdata/bars.h264")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := h264reader.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for raw == nil {
		nal, err := r.NextNAL()
		if err != nil {
			t.Fatalf("no SPS in the fixture: %v", err)
		}
		if nal.UnitType == h264reader.NalUnitTypeSPS {
			raw = bytes.Clone(nal.Data)
		}
	}
	verdict := func(sps []byte) string {
		if err := sframe.CheckSPSVUI(sps); err != nil {
			return err.Error()
		}
		return "ok"
	}
	rewritten, err := canonicalSPS(raw)
	if err != nil {
		t.Fatalf("canonicalSPS: %v", err)
	}
	t.Logf("SP-04-go raw_sps=%x raw_vui=%s rewritten_sps=%x rewritten_vui=%s", raw, verdict(raw), rewritten, verdict(rewritten))
	if verdict(rewritten) != "ok" {
		t.Fatal("the rewritten SPS fails the incoming VUI check")
	}
}

func TestCanonicalSPSAddsTheBitstreamRestriction(t *testing.T) {
	w := &bitWriter{}
	w.u(8, 66)   // profile_idc: Baseline
	w.u(8, 0xc0) // constraint_set0/1
	w.u(8, 31)   // level_idc 3.1
	w.ue(0)      // seq_parameter_set_id
	w.ue(0)      // log2_max_frame_num_minus4
	w.ue(2)      // pic_order_cnt_type
	w.ue(1)      // max_num_ref_frames
	w.u(1, 0)    // gaps_in_frame_num_value_allowed_flag
	w.ue(39)     // pic_width_in_mbs_minus1: 640
	w.ue(29)     // pic_height_in_map_units_minus1: 480
	w.u(1, 1)    // frame_mbs_only_flag
	w.u(1, 1)    // direct_8x8_inference_flag
	w.u(1, 0)    // frame_cropping_flag
	w.u(1, 0)    // vui_parameters_present_flag
	w.trailing()
	raw := append([]byte{0x67}, sframe.RBSPEscape(0, w.out)...)
	if err := sframe.CheckSPSVUI(raw); !errors.Is(err, sframe.ErrNonCanonicalSPS) {
		t.Fatalf("an SPS without a VUI: %v, want %v", err, sframe.ErrNonCanonicalSPS)
	}
	fixed, err := canonicalSPS(raw)
	if err != nil {
		t.Fatalf("canonicalSPS: %v", err)
	}
	if err := sframe.CheckSPSVUI(fixed); err != nil {
		t.Fatalf("the rewritten SPS %x: %v", fixed, err)
	}
	again, err := canonicalSPS(unhex(t, stableSPS))
	if err != nil {
		t.Fatalf("canonicalSPS(stable): %v", err)
	}
	if err := sframe.CheckSPSVUI(again); err != nil {
		t.Fatalf("the stable SPS rewritten: %x: %v", again, err)
	}
}

// codecSlot maps the publication names Publish* give their tracks to the codec and slot a
// subscriber's decryptor expects.
func codecSlot(name string) (sframe.Codec, sframe.Slot, bool) {
	switch name {
	case "opus":
		return sframe.Opus, sframe.Mic, true
	case "vp8":
		return sframe.VP8, sframe.Camera, true
	case "h264":
		return sframe.H264, sframe.ScreenVideo, true
	}
	return 0, 0, false
}

// publishRetrying tolerates the SDK's refusal of a publication while the previous one is still
// negotiating.
func publishRetrying(t *testing.T, publish func() (*lksdk.LocalTrackPublication, error)) {
	t.Helper()
	var err error
	for range 20 {
		if _, err = publish(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("publish: %v", err)
}

// A Go publisher's Opus, VP8 and H.264 reach a Go subscriber through the real SFU and decrypt; the
// only drops are LiveKit's own SIF frames; LiveKit and the subscriber both see CUSTOM, so the NONE
// branch of a receiver is never reached.
func TestGoPublisherToGoSubscriberDecryptsThroughTheSFU(t *testing.T) {
	if raceEnabled {
		t.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := sfu.DefaultConfig()
	cfg.Port, cfg.UDPPort = sfutest.FreePorts(t)
	cfg.APISecret = strings.Repeat("g", 32)
	cfg.STUNServers = []string{"127.0.0.1:3478"} // RIGS-06: no third-party STUN host from a test
	srv, err := sfu.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("sfu.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	const room = "media-go-to-go"

	ring := sframe.NewKeyRing(nil)
	ring.InstallEpoch(1, base, roster, 1)
	counters := map[string]*Counters{"opus": {}, "vp8": {}, "h264": {}}
	var mu sync.Mutex
	seen := map[string]livekit.Encryption_Type{}
	ready := make(chan struct{})
	var bobRoom *lksdk.Room
	cb := &lksdk.RoomCallback{ParticipantCallback: lksdk.ParticipantCallback{
		OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
			codec, slot, ok := codecSlot(pub.Name())
			if !ok {
				return
			}
			mu.Lock()
			seen[pub.Name()] = pub.TrackInfo().GetEncryption()
			mu.Unlock()
			go func() {
				select {
				case <-ready:
				case <-ctx.Done():
					return
				}
				d := NewFrameDecryptor(ring, codec, alice, slot, bobRoom.SifTrailer(), counters[pub.Name()])
				_ = DecryptLoop(ctx, track, d)
			}()
		},
	}}
	if err := srv.CreateRoom(ctx, room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	bobTok, err := srv.Token(room, hex.EncodeToString(bob[:]), sfu.PublishGrant(true, true, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	bobRoom, err = lksdk.ConnectToRoomWithToken(srv.URL(), bobTok, cb)
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	close(ready)
	t.Cleanup(bobRoom.Disconnect)

	aliceTok, err := srv.Token(room, hex.EncodeToString(alice[:]), sfu.PublishGrant(true, true, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	aliceRoom, err := lksdk.ConnectToRoomWithToken(srv.URL(), aliceTok, &lksdk.RoomCallback{})
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	t.Cleanup(aliceRoom.Disconnect)
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	publishRetrying(t, func() (*lksdk.LocalTrackPublication, error) {
		return PublishOpusOgg(aliceRoom, "testdata/tone.ogg", NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0))
	})
	publishRetrying(t, func() (*lksdk.LocalTrackPublication, error) {
		return PublishVP8IVF(aliceRoom, "testdata/bars.ivf", NewFrameEncryptor(sender, sframe.VP8, sframe.Camera, 0), livekit.TrackSource_CAMERA)
	})
	publishRetrying(t, func() (*lksdk.LocalTrackPublication, error) {
		return PublishH264(aliceRoom, "testdata/bars.h264", NewFrameEncryptor(sender, sframe.H264, sframe.ScreenVideo, 0), livekit.TrackSource_SCREEN_SHARE)
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		done := true
		for _, c := range counters {
			if n, _ := c.Snapshot(); n < 25 {
				done = false
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			for name, c := range counters {
				n, dropped := c.Snapshot()
				t.Logf("%s: decrypted %d, dropped %v", name, n, dropped)
			}
			t.Fatal("not every track decrypted 25 frames within 30 s")
		}
		time.Sleep(250 * time.Millisecond)
	}
	for name, c := range counters {
		n, dropped := c.Snapshot()
		t.Logf("SP-15 go-to-go %s: decrypted=%d dropped=%v", name, n, dropped)
		for reason := range dropped {
			if reason != "sif" {
				t.Errorf("%s: dropped %v; only LiveKit's SIF frames may be dropped", name, dropped)
			}
		}
	}
	mu.Lock()
	for name, enc := range seen {
		if enc != livekit.Encryption_CUSTOM {
			t.Errorf("the subscriber sees %s as %s, want CUSTOM", name, enc)
		}
	}
	mu.Unlock()
	res, err := lksdk.NewRoomServiceClient(srv.HTTPURL(), cfg.APIKey, cfg.APISecret).
		ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		t.Fatalf("ListParticipants: %v", err)
	}
	custom := 0
	for _, p := range res.Participants {
		if p.Identity != hex.EncodeToString(alice[:]) {
			continue
		}
		for _, tr := range p.Tracks {
			if tr.Encryption != livekit.Encryption_CUSTOM {
				t.Errorf("LiveKit holds alice's %s as %s, want CUSTOM", tr.Name, tr.Encryption)
			}
			custom++
		}
	}
	if custom != 3 {
		t.Fatalf("LiveKit lists %d CUSTOM tracks for alice, want 3", custom)
	}
}
