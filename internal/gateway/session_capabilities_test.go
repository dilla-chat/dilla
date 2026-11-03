package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type capabilityRecordingAuth struct {
	Authenticator
	mu      sync.Mutex
	records map[string][3]uint64
	err     error
}

func (a *capabilityRecordingAuth) RecordCapabilities(_ context.Context, hash []byte, wire, e2ee, media uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	if a.records == nil {
		a.records = make(map[string][3]uint64)
	}
	a.records[string(hash)] = [3]uint64{wire, e2ee, media}
	return nil
}

func (a *capabilityRecordingAuth) selected(hash []byte) ([3]uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.records[string(hash)]
	return v, ok
}

func TestWireThreeRefusesReadyWhenCapabilityPersistenceIsUnavailable(t *testing.T) {
	h := newHarness(t)
	sink := newRecordingSink(8, false)
	session := auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled}
	if _, err := h.gw.registerVersions(context.Background(), session, sink, [3]uint64{3, 1, 1}); err == nil {
		t.Fatal("wire 3 reached ready without a capability recorder")
	}
	select {
	case frame := <-sink.frames:
		t.Fatalf("frame %x was sent before capability persistence", frame)
	default:
	}
}

func TestIdentifyPersistsCapabilitiesBeforeReady(t *testing.T) {
	h := newHarness(t)
	recorder := &capabilityRecordingAuth{Authenticator: h}
	h.gw.opts.Auth = recorder
	hash := make([]byte, 32)
	hash[0] = 1
	session := auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled, TokenHash: hash}
	sink := newRecordingSink(8, false)
	c, err := h.gw.registerVersions(context.Background(), session, sink, [3]uint64{3, 1, 1})
	if err != nil {
		t.Fatalf("register wire 3: %v", err)
	}
	if got, ok := recorder.selected(hash); !ok || got != [3]uint64{3, 1, 1} {
		t.Fatalf("persisted identify versions = %v, %v; want [3 1 1]", got, ok)
	}
	if ready := h.readFrom(t, sink); ready.Op != OpReady {
		t.Fatalf("first frame op = %d, want ready after persistence", ready.Op)
	}
	if c.wire != 3 || c.e2ee != 1 || c.media != 1 {
		t.Fatalf("connection selected [%d %d %d], want [3 1 1]", c.wire, c.e2ee, c.media)
	}
}

func TestResumePersistsOriginalCapabilitiesAgainstReplacementBearer(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	if ready := h.waitFrame(t, c.deviceID); ready.Op != OpReady {
		t.Fatalf("first frame op = %d, want ready", ready.Op)
	}
	c.mu.Lock()
	c.wire, c.e2ee, c.media = 3, 1, 1
	c.mu.Unlock()
	old := c.resumeToken()
	h.gw.suspend(c)
	recorder := &capabilityRecordingAuth{Authenticator: h}
	h.gw.opts.Auth = recorder
	newHash := make([]byte, 32)
	newHash[0] = 2
	session := auth.Session{UserID: c.userID, DeviceID: c.deviceID, Scope: auth.ScopeEnrolled, TokenHash: newHash}
	newBearer := "replacement-session"
	h.mu.Lock()
	h.tokens[newBearer] = session
	h.mu.Unlock()
	p, err := payload(newBearer, old, h.generation, uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	var rawPayload []cbor.RawMessage
	if err := cborx.Unmarshal(p, &rawPayload); err != nil {
		t.Fatal(err)
	}
	sink := newRecordingSink(16, false)
	resumed, ok := h.gw.resumeConnection(context.Background(), sink, Inbound{Payload: rawPayload})
	if !ok || resumed != c {
		t.Fatal("valid same-device resume was refused")
	}
	if got, ok := recorder.selected(newHash); !ok || got != [3]uint64{3, 1, 1} {
		t.Fatalf("persisted resume versions = %v, %v; want [3 1 1]", got, ok)
	}
	c.mu.Lock()
	bound := append([]byte(nil), c.tokenHash...)
	c.mu.Unlock()
	if string(bound) != string(newHash) {
		t.Fatal("resumed connection kept the old bearer binding")
	}
}

func TestInvalidResumeDoesNotPersistCapabilities(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	if ready := h.waitFrame(t, c.deviceID); ready.Op != OpReady {
		t.Fatalf("first frame op = %d, want ready", ready.Op)
	}
	c.mu.Lock()
	c.wire, c.e2ee, c.media = 3, 1, 1
	c.mu.Unlock()
	resumeToken := c.resumeToken()
	h.gw.suspend(c)
	recorder := &capabilityRecordingAuth{Authenticator: h}
	h.gw.opts.Auth = recorder
	newHash := make([]byte, 32)
	newHash[0] = 3
	session := auth.Session{UserID: c.userID, DeviceID: c.deviceID, Scope: auth.ScopeEnrolled, TokenHash: newHash}
	newBearer := "invalid-resume-session"
	h.mu.Lock()
	h.tokens[newBearer] = session
	h.mu.Unlock()
	p, err := payload(newBearer, resumeToken, h.generation+1, uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	var rawPayload []cbor.RawMessage
	if err := cborx.Unmarshal(p, &rawPayload); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.gw.resumeConnection(context.Background(), newRecordingSink(8, false), Inbound{Payload: rawPayload}); ok {
		t.Fatal("resume with a stale generation was accepted")
	}
	if _, exists := recorder.selected(newHash); exists {
		t.Fatal("invalid resume mutated bearer capability state")
	}
}

func TestCapabilityRecorderFailureRefusesReady(t *testing.T) {
	h := newHarness(t)
	recorder := &capabilityRecordingAuth{Authenticator: h, err: errors.New("storage unavailable")}
	h.gw.opts.Auth = recorder
	hash := make([]byte, 32)
	session := auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled, TokenHash: hash}
	if _, err := h.gw.registerVersions(context.Background(), session, newRecordingSink(8, false), [3]uint64{3, 1, 1}); err == nil {
		t.Fatal("ready was accepted after capability persistence failed")
	}
}
