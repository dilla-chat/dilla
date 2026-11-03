package media

import (
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/dilla-chat/dilla/internal/sframe"
)

// RIGS-07: the rig parsed identities with hex.Decode into a 16-byte array, which accepted uppercase
// and panicked on 34 or more hex digits; neither Go peer checked the track's kind against its source.
func TestATrackIsBoundOnlyUnderProtocolFivesReceiverRules(t *testing.T) {
	lower := hex.EncodeToString(alice[:])
	for _, tc := range []struct {
		name     string
		identity string
		kind     webrtc.RTPCodecType
		mime     string
		source   livekit.TrackSource
		ok       bool
	}{
		{"mic", lower, webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_MICROPHONE, true},
		{"screen audio", lower, webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_SCREEN_SHARE_AUDIO, true},
		{"camera", lower, webrtc.RTPCodecTypeVideo, webrtc.MimeTypeVP8, livekit.TrackSource_CAMERA, true},
		{"opus on camera", lower, webrtc.RTPCodecTypeVideo, webrtc.MimeTypeOpus, livekit.TrackSource_CAMERA, false},
		{"vp8 on microphone", lower, webrtc.RTPCodecTypeAudio, webrtc.MimeTypeVP8, livekit.TrackSource_MICROPHONE, false},
		{"screen", lower, webrtc.RTPCodecTypeVideo, webrtc.MimeTypeH264, livekit.TrackSource_SCREEN_SHARE, true},
		{"uppercase identity", strings.ToUpper(lower), webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_MICROPHONE, false},
		{"34 hex digits", lower + "a1", webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_MICROPHONE, false},
		{"64 hex digits", lower + lower, webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_MICROPHONE, false},
		{"31 hex digits", lower[:31], webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_MICROPHONE, false},
		{"a '#' suffix", lower[:30] + "#1", webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_MICROPHONE, false},
		{"audio on the camera source", lower, webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_CAMERA, false},
		{"video on the microphone source", lower, webrtc.RTPCodecTypeVideo, webrtc.MimeTypeVP8, livekit.TrackSource_MICROPHONE, false},
		{"unknown source", lower, webrtc.RTPCodecTypeAudio, webrtc.MimeTypeOpus, livekit.TrackSource_UNKNOWN, false},
		{"AV1", lower, webrtc.RTPCodecTypeVideo, webrtc.MimeTypeAV1, livekit.TrackSource_CAMERA, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, _, _, ok := TrackBinding(tc.identity, tc.kind, tc.mime, tc.source)
			if ok != tc.ok {
				t.Fatalf("TrackBinding(%q, %s, %s, %s) ok = %v, want %v", tc.identity, tc.kind, tc.mime, tc.source, ok, tc.ok)
			}
			if ok && dev != alice {
				t.Fatalf("device = %x, want %x", dev, alice)
			}
		})
	}
}

// RIGS-04: a zero-byte Opus RTP payload (the audio DTX frame) reaches the decryptor through the
// decrypt loop and is counted as an empty frame, beside the sealed frames around it.
func TestTheDecryptLoopCountsAZeroByteOpusPacketAsAnEmptyFrame(t *testing.T) {
	sender, err := sframe.NewSender(base, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	enc := NewFrameEncryptor(sender, sframe.Opus, sframe.Mic, 0)
	var pkts []*rtp.Packet
	for i, payload := range [][]byte{{0xfc, 1}, nil, {0xfc, 2}, {0xfc, 3}} {
		out := []byte{}
		if payload != nil {
			if out, err = enc.EncryptFrame(payload); err != nil {
				t.Fatal(err)
			}
		}
		pkts = append(pkts, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: uint16(100 + i),
			Timestamp: uint32(960 * i), Marker: true}, Payload: out})
	}
	read := func() (*rtp.Packet, error) {
		if len(pkts) == 0 {
			return nil, io.EOF
		}
		p := pkts[0]
		pkts = pkts[1:]
		return p, nil
	}
	ring := sframe.NewKeyRing(nil)
	ring.InstallEpoch(1, base, roster, 1)
	var counters Counters
	codec := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}}
	if err := decryptPackets(t.Context(), read, codec, NewFrameDecryptor(ring, sframe.Opus, alice, sframe.Mic, nil, &counters)); err != nil {
		t.Fatal(err)
	}
	n, dropped := counters.Snapshot()
	if counters.EmptyFrames() != 1 || n != 3 || len(dropped) != 0 {
		t.Fatalf("empty frames %d, decrypted %d, dropped %v; want 1, 3, none", counters.EmptyFrames(), n, dropped)
	}
}
