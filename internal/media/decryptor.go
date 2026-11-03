package media

import (
	"bytes"
	"errors"
	"maps"
	"sync"

	"github.com/dilla-chat/dilla/internal/sframe"
)

// Counters tracks decrypted frames, empty audio DTX markers, and drops by reason: an E_SFRAME_* code,
// "sif" for LiveKit's own injected frames, or "other". The zero value is ready to use, and one Counters may
// be shared by every decryptor of a participant.
type Counters struct {
	mu          sync.Mutex
	decrypted   uint64
	emptyFrames uint64
	dropped     map[string]uint64
}

func (c *Counters) addDecrypted() {
	c.mu.Lock()
	c.decrypted++
	c.mu.Unlock()
}

func (c *Counters) addEmptyFrame() {
	c.mu.Lock()
	c.emptyFrames++
	c.mu.Unlock()
}

// EmptyFrames returns the number of zero-byte audio DTX markers passed through.
func (c *Counters) EmptyFrames() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.emptyFrames
}

func (c *Counters) addDropped(reason string) {
	c.mu.Lock()
	if c.dropped == nil {
		c.dropped = map[string]uint64{}
	}
	c.dropped[reason]++
	c.mu.Unlock()
}

// Snapshot copies the counters; dropped is never nil.
func (c *Counters) Snapshot() (decrypted uint64, dropped map[string]uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped = maps.Clone(c.dropped)
	if dropped == nil {
		dropped = map[string]uint64{}
	}
	return c.decrypted, dropped
}

// DropIncomplete is the drop code of an H.264 key frame whose parameter sets (SPS/PPS) never
// arrived: a transport loss, counted before decryption and never an E_SFRAME_* code.
const DropIncomplete = "incomplete"

// startsWithIDR reports whether the Annex-B frame's first NAL is an IDR slice (type 5), that is,
// the key frame has no SPS before it.
func startsWithIDR(frame []byte) bool {
	switch {
	case len(frame) >= 5 && frame[0] == 0 && frame[1] == 0 && frame[2] == 0 && frame[3] == 1:
		return frame[4]&0x1f == 5
	case len(frame) >= 4 && frame[0] == 0 && frame[1] == 0 && frame[2] == 1:
		return frame[3]&0x1f == 5
	}
	return false
}

// FrameDecryptor is one subscribed track's e2eetypes.FrameDecryptor: the participant's key ring,
// the track's codec, the device its identity names and the slot its source maps to.
type FrameDecryptor struct {
	ring     *sframe.KeyRing
	codec    sframe.Codec
	device   [16]byte
	slot     sframe.Slot
	sif      []byte
	counters *Counters
}

// NewFrameDecryptor makes a track's decryptor; a nil counters gets a private Counters.
func NewFrameDecryptor(r *sframe.KeyRing, c sframe.Codec, expectedDevice [16]byte, slot sframe.Slot, sif []byte, counters *Counters) *FrameDecryptor {
	if counters == nil {
		counters = &Counters{}
	}
	return &FrameDecryptor{ring: r, codec: c, device: expectedDevice, slot: slot, sif: bytes.Clone(sif), counters: counters}
}

// DecryptFrame answers P || plaintext, or (nil, nil) for a frame it drops — the SDK's "drop this
// frame" — after counting it. The SIF suffix is tested before anything is parsed (DEV-13): LiveKit's
// Opus silence begins with f8, which reads as an SFrame config byte.
func (d *FrameDecryptor) DecryptFrame(payload []byte) ([]byte, error) {
	if len(payload) == 0 && (d.slot == sframe.Mic || d.slot == sframe.ScreenAudio) && d.codec == sframe.Opus {
		d.counters.addEmptyFrame()
		return []byte{}, nil
	}
	if len(d.sif) > 0 && bytes.HasSuffix(payload, d.sif) {
		d.counters.addDropped("sif")
		return nil, nil
	}
	if d.codec == sframe.H264 && startsWithIDR(payload) {
		// The sender's key access unit is SPS, PPS, IDR, and the parameter sets sit in the
		// authenticated prefix, so an IDR with none lost its STAP-A packet in transport. Decrypting
		// it would fail closed as E_SFRAME_AUTH, indistinguishable from a forgery. Only clear NAL
		// header bytes are read; no SFrame check is relaxed.
		d.counters.addDropped(DropIncomplete)
		return nil, nil
	}
	dec, err := d.ring.Decrypt(d.codec, payload, &d.device, d.slot)
	if err != nil {
		var code sframe.Error
		if errors.As(err, &code) {
			d.counters.addDropped(string(code))
		} else {
			d.counters.addDropped("other")
		}
		return nil, nil
	}
	d.counters.addDecrypted()
	return dec.Frame, nil
}
