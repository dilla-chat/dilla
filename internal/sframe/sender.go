package sframe

import "sync"

type counterKey struct {
	epoch uint64
	slot  Slot
	layer uint8
}

// Sender is one device's sender for one call: the current epoch's KID and keys, and the counters.
// N1: a sender made after its device committed epoch minEpoch encrypts only at or above it. N3: seq
// runs per (epoch, slot, layer) for the Sender's life and no rekey resets it. Refuse-on-wrap: the
// counter after MaxSeq is ErrCounterExhausted. Every FrameEncryptor of a device shares one Sender,
// so it is safe for concurrent use (MD-22).
type Sender struct {
	mu        sync.Mutex
	epoch     uint64
	kid       uint64
	keys      Keys
	minEpoch  uint64
	next      map[counterKey]uint64
	exhausted bool
	// A reserved sender can spend only its durable Mic/0 range in one epoch.
	reserved        bool
	reservedEnd     uint64
	reservedEpoch   uint64
	reservedInvalid bool
}

// NewSender is ErrStaleEpoch when epoch is below minEpoch (N1).
func NewSender(baseKey [16]byte, leaf uint16, epoch, minEpoch uint64) (*Sender, error) {
	if epoch < minEpoch {
		return nil, ErrStaleEpoch
	}
	kid := KID(leaf, epoch)
	return &Sender{
		epoch: epoch, kid: kid, keys: DeriveKeys(baseKey, kid),
		minEpoch: minEpoch, next: map[counterKey]uint64{},
	}, nil
}

// NewSenderRange uses a durable, exclusive Mic/0 counter interval. The caller
// must persist the reservation before publishing. A fresh sender for the same
// key and epoch must receive a disjoint interval, including after restart.
func NewSenderRange(baseKey [16]byte, leaf uint16, epoch, start, end uint64) (*Sender, error) {
	if start >= end || end > MaxSeq+1 {
		return nil, ErrCounterExhausted
	}
	s, err := NewSender(baseKey, leaf, epoch, epoch)
	if err != nil {
		return nil, err
	}
	s.reserved = true
	s.reservedEnd = end
	s.reservedEpoch = epoch
	s.next[counterKey{epoch: epoch, slot: Mic, layer: 0}] = start
	return s, nil
}

// Rekey switches to a new epoch's key; the counters carry on.
func (s *Sender) Rekey(baseKey [16]byte, leaf uint16, epoch uint64) {
	kid := KID(leaf, epoch)
	keys := DeriveKeys(baseKey, kid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserved {
		// A new epoch requires a new durable reservation, even if this
		// caller supplies the same key and epoch again.
		s.reservedInvalid = true
	}
	s.epoch, s.kid, s.keys = epoch, kid, keys
}

// RekeyRange moves a reserved sender to a newer epoch's key with that epoch's own durable Mic/0 counter interval, in
// one step: the next frame is under the new key, and no frame after it under an older one. The caller must persist
// the reservation first, as for NewSenderRange. An older epoch than the current is ErrStaleEpoch.
func (s *Sender) RekeyRange(baseKey [16]byte, leaf uint16, epoch, start, end uint64) error {
	if start >= end || end > MaxSeq+1 {
		return ErrCounterExhausted
	}
	kid := KID(leaf, epoch)
	keys := DeriveKeys(baseKey, kid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reserved {
		return ErrSlotMismatch
	}
	if epoch < s.epoch {
		return ErrStaleEpoch
	}
	s.epoch, s.kid, s.keys, s.minEpoch = epoch, kid, keys, epoch
	s.reservedEpoch, s.reservedEnd, s.reservedInvalid = epoch, end, false
	s.next[counterKey{epoch: epoch, slot: Mic, layer: 0}] = start
	return nil
}

// Encrypt checks the frame's codec shape first, so a refused frame spends no counter, then takes
// the next counter and runs Protect. A reserved slot (above ScreenAudio) is ErrSlotMismatch before
// anything else, as the wasm sender's slot parse is. An H.264 frame whose prefix carries an SPS a
// libwebrtc receiver would rewrite is ErrNonCanonicalSPS.
func (s *Sender) Encrypt(c Codec, slot Slot, layer uint8, frame []byte) ([]byte, error) {
	if slot > ScreenAudio {
		return nil, ErrSlotMismatch
	}
	if c == H264 {
		canonical, p, err := canonicalizeH264(frame)
		if err != nil {
			return nil, err
		}
		if err := checkPrefixSPS(canonical[:p]); err != nil {
			return nil, err
		}
	} else if _, err := PrefixLen(c, frame); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserved {
		if s.reservedInvalid || s.epoch != s.reservedEpoch {
			return nil, ErrStaleEpoch
		}
		if slot != Mic || layer != 0 {
			return nil, ErrSlotMismatch
		}
	}
	if s.epoch < s.minEpoch {
		return nil, ErrStaleEpoch
	}
	if layer > 0xf {
		return nil, ErrLayerRange
	}
	k := counterKey{epoch: s.epoch, slot: slot, layer: layer}
	seq := s.next[k]
	if s.reserved && seq >= s.reservedEnd {
		s.exhausted = true
		return nil, ErrCounterExhausted
	}
	if seq > MaxSeq {
		s.exhausted = true
		return nil, ErrCounterExhausted
	}
	ctr, err := CTR(slot, layer, seq)
	if err != nil {
		return nil, err
	}
	s.next[k] = seq + 1
	return Protect(s.keys, s.kid, ctr, c, frame)
}

// Exhausted is true once any counter has been refused for exhaustion.
func (s *Sender) Exhausted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exhausted
}
