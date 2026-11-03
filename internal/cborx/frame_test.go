package cborx_test

import (
	"bytes"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestMarshalFrameSplicesPayloadVerbatim(t *testing.T) {
	gid := id.New()
	payload, err := cborx.Marshal([]uint64{7, 9})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	b, err := cborx.MarshalFrame(19, 3, &gid, cborx.Raw(payload))
	if err != nil {
		t.Fatalf("MarshalFrame: %v", err)
	}
	if b[0] != 0x84 {
		t.Fatalf("frame is not a 4-element array: %x", b)
	}
	if !bytes.Contains(b, payload) {
		t.Fatalf("payload %x not spliced verbatim into %x", payload, b)
	}
	var frame []cbor.RawMessage
	if err := cborx.Unmarshal(b, &frame); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(frame) != 4 {
		t.Fatalf("len = %d, want 4", len(frame))
	}
	if !bytes.Equal(frame[3], payload) {
		t.Fatalf("element 3 = %x, want %x", frame[3], payload)
	}
}

func TestMarshalFrameNullGroupID(t *testing.T) {
	payload, _ := cborx.Marshal([]uint64{})
	b, err := cborx.MarshalFrame(0, 0, nil, cborx.Raw(payload))
	if err != nil {
		t.Fatalf("MarshalFrame: %v", err)
	}
	var frame []cbor.RawMessage
	if err := cborx.Unmarshal(b, &frame); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(frame[2]) != 1 || frame[2][0] != 0xf6 {
		t.Fatalf("element 2 = %x, want 0xf6 null", frame[2])
	}
}

func TestMarshalFrameNIsPatchableWithoutReencodingThePayload(t *testing.T) {
	gid := id.New()
	payload, _ := cborx.Marshal([]uint64{1, 2, 3})
	first, err := cborx.MarshalFrame(16, 1, &gid, cborx.Raw(payload))
	if err != nil {
		t.Fatalf("MarshalFrame: %v", err)
	}
	second, err := cborx.MarshalFrame(16, 2, &gid, cborx.Raw(payload))
	if err != nil {
		t.Fatalf("MarshalFrame: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("small n must keep the frame length stable: %d != %d", len(first), len(second))
	}
	diff := 0
	for i := range first {
		if first[i] != second[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Fatalf("frames differ in %d bytes, want exactly the n byte", diff)
	}
}
