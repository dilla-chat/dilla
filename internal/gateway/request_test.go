package gateway

import (
	"bytes"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestSocketRequestFrameRoundTripsThroughDecode(t *testing.T) {
	want := SocketRequest{ID: 7, Method: "POST", Route: "/v1/x?y=1", Body: []byte{1, 2}, Idem: []byte("idem")}
	b, err := EncodeRequestFrame(want)
	if err != nil {
		t.Fatal(err)
	}
	in, err := Decode(b, 16384)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Method != want.Method || got.Route != want.Route ||
		!bytes.Equal(got.Body, want.Body) || !bytes.Equal(got.Idem, want.Idem) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
	noIdem, _ := EncodeRequestFrame(SocketRequest{ID: 8, Method: "GET", Route: "/v1/x", Body: []byte{}})
	in, _ = Decode(noIdem, 16384)
	if got, err := decodeRequest(in); err != nil || got.Idem != nil {
		t.Fatalf("null idem decoded as %x, %v", got.Idem, err)
	}
}

// The hand-written request and response codecs produce exactly cborx's deterministic bytes, and
// the response reader refuses what the deterministic subset refuses.
func TestSocketFrameCodecsMatchTheDeterministicEncoder(t *testing.T) {
	for _, r := range []SocketRequest{
		{ID: 0, Method: "GET", Route: "/", Body: nil},
		{ID: 23, Method: "POST", Route: "/v1/x", Body: []byte{1}, Idem: []byte{2}},
		{ID: 24, Method: "PATCH", Route: string(bytes.Repeat([]byte{'a'}, 300)), Body: make([]byte, 70000), Idem: make([]byte, 64)},
		{ID: 1 << 40, Method: "DELETE", Route: "/v1/y", Body: make([]byte, 255)},
	} {
		p, err := RequestPayload(r)
		if err != nil {
			t.Fatal(err)
		}
		want, err := cborx.MarshalFrame(uint64(OpRequest), 0, nil, p)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := EncodeRequestFrame(r)
		if !bytes.Equal(got, want) {
			t.Fatalf("request %d: hand-encoded %x, cborx %x", r.ID, got[:min(len(got), 40)], want[:min(len(want), 40)])
		}
	}
	for _, r := range []SocketResponse{
		{ID: 0, Status: 200, Body: nil},
		{ID: 1 << 33, Status: 599, Body: make([]byte, 65536)},
		{ID: 300, Status: 100, Body: []byte{9}},
	} {
		p, _ := ResponsePayload(r)
		want, _ := cborx.MarshalFrame(uint64(OpResponse), 0, nil, p)
		got, err := responseFrame(r)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("response %d: hand-encoded %x, cborx %x (%v)", r.ID, got[:min(len(got), 40)], want[:min(len(want), 40)], err)
		}
		back, err := DecodeResponseFrame(got)
		if err != nil || back.ID != r.ID || back.Status != r.Status || len(back.Body) != len(r.Body) {
			t.Fatalf("response %d did not round-trip: %+v %v", r.ID, back, err)
		}
	}
	good, _ := responseFrame(SocketResponse{ID: 5, Status: 200, Body: []byte{1, 2}})
	for name, b := range map[string][]byte{
		"trailing byte":      append(append([]byte(nil), good...), 0),
		"short body":         good[:len(good)-1],
		"non-minimal id":     {0x84, 0x18, 60, 0x00, 0xf6, 0x83, 0x18, 0x05, 0x18, 200, 0x42, 1, 2},
		"indefinite payload": {0x84, 0x18, 60, 0x00, 0xf6, 0x9f, 0x05, 0x18, 200, 0x42, 1, 2, 0xff},
		"wrong op":           {0x84, 0x18, 59, 0x00, 0xf6, 0x83, 0x05, 0x18, 200, 0x42, 1, 2},
		"status 99":          {0x84, 0x18, 60, 0x00, 0xf6, 0x83, 0x05, 0x18, 99, 0x42, 1, 2},
	} {
		if _, err := DecodeResponseFrame(b); err == nil {
			t.Fatalf("%s: decoded", name)
		}
	}
}

func TestSocketRequestFrameOfTheWrongShapeIsRefused(t *testing.T) {
	for name, payload := range map[string][]any{
		"text body":     {uint64(1), "GET", "/v1/x", "body", nil},
		"uint method":   {uint64(1), uint64(1), "/v1/x", []byte{}, nil},
		"text idem":     {uint64(1), "GET", "/v1/x", []byte{}, "idem"},
		"text id":       {"1", "GET", "/v1/x", []byte{}, nil},
		"four elements": {uint64(1), "GET", "/v1/x", []byte{}},
		"long idem":     {uint64(1), "GET", "/v1/x", []byte{}, bytes.Repeat([]byte{1}, maxIdemBytes+1)},
		"long route":    {uint64(1), "GET", "/" + string(bytes.Repeat([]byte{'a'}, maxRouteBytes)), []byte{}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := cborx.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			b, err := cborx.MarshalFrame(uint64(OpRequest), 0, nil, p)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Decode(b, 1<<20)
			var fe *FrameError
			if !errors.As(err, &fe) || fe.Code != "E_FRAME_SHAPE" {
				t.Fatalf("Decode = %v, want E_FRAME_SHAPE", err)
			}
		})
	}
}

// A response is server-to-client: a client op 60 is E_FRAME_TYPE.
func TestSocketResponseFromAClientIsFrameType(t *testing.T) {
	p, _ := ResponsePayload(SocketResponse{ID: 1, Status: 200, Body: []byte{}})
	b, _ := cborx.MarshalFrame(uint64(OpResponse), 0, nil, p)
	_, err := Decode(b, 16384)
	var fe *FrameError
	if !errors.As(err, &fe) || fe.Code != "E_FRAME_TYPE" {
		t.Fatalf("Decode = %v, want E_FRAME_TYPE", err)
	}
}

// Only a request frame may be larger than the read limit, and only up to the request frame limit.
func TestOnlyARequestFrameMayExceedTheReadLimit(t *testing.T) {
	big, _ := EncodeRequestFrame(SocketRequest{ID: 1, Method: "POST", Route: "/v1/x", Body: make([]byte, 64<<10)})
	if !isRequestFrame(big) {
		t.Fatal("a request frame is not recognised by its head")
	}
	if frameReadLimit(big, 16384) != maxRequestFrameBytes {
		t.Fatal("a request frame is not read up to the request frame limit")
	}
	sub, _ := cborx.Marshal([]any{make([]byte, 64<<10)})
	other, _ := cborx.MarshalFrame(uint64(OpSubscribe), 0, nil, sub)
	if isRequestFrame(other) || frameReadLimit(other, 16384) != 16384 {
		t.Fatal("a subscribe frame was given the request frame limit")
	}
	if _, err := Decode(big, frameReadLimit(big, 16384)); err != nil {
		t.Fatalf("a 64 KiB request frame was refused: %v", err)
	}
}

func TestSocketBusyBudgetAdmitsSixtyFourAndThirtyTwoMiB(t *testing.T) {
	b := newRequestBudget()
	for range MaxInFlightRequests {
		if !b.admit(1) {
			t.Fatal("refused a request under the cap")
		}
	}
	if b.admit(1) {
		t.Fatal("admitted the 65th in-flight request")
	}
	b.finish(1)
	if !b.admit(1) {
		t.Fatal("refused a request after one finished")
	}
	b2 := newRequestBudget()
	if !b2.admit(30<<20) || b2.admit(3<<20) {
		t.Fatal("the 32 MiB byte budget is not enforced")
	}
	if b2.charge(3<<20) || !b2.charge(1<<20) {
		t.Fatal("a response charge ignores the byte budget")
	}
}
