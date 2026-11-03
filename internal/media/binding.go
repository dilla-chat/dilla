package media

import (
	"encoding/hex"
	"strings"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v4"

	"github.com/dilla-chat/dilla/internal/sframe"
)

// DeviceID parses a LiveKit participant identity as a device_id: exactly 32 lowercase hex digits
// (protocol/05 "Receiver rules"). Anything else — uppercase, another length, a '#' — is an
// unverified stream and is never decrypted.
func DeviceID(identity string) ([16]byte, bool) {
	var d [16]byte
	if len(identity) != 2*len(d) || strings.ToLower(identity) != identity {
		return d, false
	}
	if _, err := hex.Decode(d[:], []byte(identity)); err != nil {
		return [16]byte{}, false
	}
	return d, true
}

// CodecOf maps a track's MIME type to the cipher's codec; anything without a prefix rule is refused.
func CodecOf(mime string) (sframe.Codec, bool) {
	switch strings.ToLower(mime) {
	case strings.ToLower(webrtc.MimeTypeOpus):
		return sframe.Opus, true
	case strings.ToLower(webrtc.MimeTypeVP8):
		return sframe.VP8, true
	case strings.ToLower(webrtc.MimeTypeVP9):
		return sframe.VP9, true
	case strings.ToLower(webrtc.MimeTypeH264):
		return sframe.H264, true
	}
	return 0, false
}

// SlotOf maps a LiveKit track source to its slot; UNKNOWN has none.
func SlotOf(s livekit.TrackSource) (sframe.Slot, bool) {
	switch s {
	case livekit.TrackSource_MICROPHONE:
		return sframe.Mic, true
	case livekit.TrackSource_CAMERA:
		return sframe.Camera, true
	case livekit.TrackSource_SCREEN_SHARE:
		return sframe.ScreenVideo, true
	case livekit.TrackSource_SCREEN_SHARE_AUDIO:
		return sframe.ScreenAudio, true
	}
	return 0, false
}

// TrackBinding is everything a subscriber needs before it builds a FrameDecryptor, under
// protocol/05's receiver rules: the publisher's device (a strict device_id identity), the codec,
// and the slot of a source whose kind matches the track's (audio with the microphone or screen
// audio, video with the camera or screen share). ok is false for anything else, which the caller
// leaves undecrypted.
func TrackBinding(identity string, kind webrtc.RTPCodecType, mime string, source livekit.TrackSource) (device [16]byte, codec sframe.Codec, slot sframe.Slot, ok bool) {
	device, ok = DeviceID(identity)
	if !ok {
		return [16]byte{}, 0, 0, false
	}
	codec, ok = CodecOf(mime)
	if !ok {
		return [16]byte{}, 0, 0, false
	}
	slot, ok = SlotOf(source)
	if !ok {
		return [16]byte{}, 0, 0, false
	}
	audio := slot == sframe.Mic || slot == sframe.ScreenAudio
	codecAudio := codec == sframe.Opus
	if (kind == webrtc.RTPCodecTypeAudio) != audio || (kind == webrtc.RTPCodecTypeAudio) != codecAudio || (kind != webrtc.RTPCodecTypeAudio && kind != webrtc.RTPCodecTypeVideo) {
		return [16]byte{}, 0, 0, false
	}
	return device, codec, slot, true
}
