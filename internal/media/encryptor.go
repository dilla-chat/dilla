package media

import "github.com/dilla-chat/dilla/internal/sframe"

// FrameEncryptor is one published track's e2eetypes.FrameEncryptor: the device's shared Sender, the
// track's codec, its slot and its layer (MD-22: every track of a device shares one counter space per
// (epoch, slot, layer)). The SDK calls it once per sample, under the track's lock, before
// packetisation. An error ends the track's write loop (lksdk's WriteSample returns it and its
// writeWorker stops), so the track publishes nothing more; nothing is ever sent unencrypted. Only
// empty Opus audio DTX markers on a microphone or screen-audio track pass unencrypted.
type FrameEncryptor struct {
	s     *sframe.Sender
	codec sframe.Codec
	slot  sframe.Slot
	layer uint8
}

func NewFrameEncryptor(s *sframe.Sender, c sframe.Codec, slot sframe.Slot, layer uint8) *FrameEncryptor {
	return &FrameEncryptor{s: s, codec: c, slot: slot, layer: layer}
}

func (e *FrameEncryptor) EncryptFrame(payload []byte) ([]byte, error) {
	if (e.slot == sframe.Mic || e.slot == sframe.ScreenAudio) && e.codec == sframe.Opus && len(payload) == 0 {
		return []byte{}, nil
	}
	return e.s.Encrypt(e.codec, e.slot, e.layer, payload)
}
