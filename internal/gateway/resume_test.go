package gateway

import (
	"bytes"
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

// A connection closed with a code protocol/02 marks as NOT resumable must not be parked in the
// resume window. The revocation path is auth.Sessions.OnRevoke -> Deps.CloseGateway ->
// Gateway.CloseDevice, and readLoop's `defer g.suspend(c)` fires right after it: without the gate
// the same connection goes straight back into g.suspended under its unchanged token, and the
// device that was just revoked can reconnect inside ResumeWindow.
func TestARevokedConnectionIsNotLeftResumable(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	token := c.resumeToken()

	h.revoke(device)
	// readLoop's deferred suspend, which fires as soon as the closed socket fails its next read.
	h.gw.suspend(c)

	if _, ok := h.gw.suspended.Load(string(token)); ok {
		t.Fatal("a connection closed 4004 session_revoked is still resumable")
	}
	if h.gw.Online(device) {
		t.Fatal("a revoked device must not be online")
	}
}

// The other half of the same rule: a connection that was ALREADY in the resume window when the
// revocation arrived is not in the registry CloseDevice walks, so it has to be swept out by hand.
func TestRevokingADeviceDropsAConnectionAlreadyInTheResumeWindow(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	token := c.resumeToken()
	h.gw.suspend(c)
	if _, ok := h.gw.suspended.Load(string(token)); !ok {
		t.Fatal("a cleanly suspended connection must be resumable straight away")
	}

	h.revoke(device)

	if _, ok := h.gw.suspended.Load(string(token)); ok {
		t.Fatal("revoking a device must drop its suspended connections too")
	}
}

// 4009 session_timeout is the other non-resumable code, and sweepLiveness is the only thing that
// sends it.
func TestAHeartbeatTimeoutLeavesNoResumableSession(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	token := c.resumeToken()

	h.clk.Advance(h.gw.beat.grace + 1)
	h.gw.sweepLiveness()

	if got := h.waitClose(t, c); got != CloseSessionTimeout {
		t.Fatalf("close code = %d, want 4009", got)
	}
	if _, ok := h.gw.suspended.Load(string(token)); ok {
		t.Fatal("a connection closed 4009 session_timeout is still resumable")
	}
}

// A revoked device presenting its old resume token and no credential is refused on the wire, not
// only in the registry.
func TestARevokedDeviceCannotResumeWithItsOldToken(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	token := c.resumeToken()
	h.revoke(device)
	h.gw.suspend(c)

	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	p, err := payload("", token, h.generation, uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

	in := h.readFrom(t, s)
	if in.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session", in.Op)
	}
	if h.gw.Online(device) {
		t.Fatal("a revoked device was put back into the live registry by a resume")
	}
}

// resume's element 0 is a session_token (protocol/02 line 135) and it is checked: possession of a
// resume token alone is not an authentication.
func TestResumeIsRefusedWhenTheSessionTokenNamesAnotherDevice(t *testing.T) {
	h := newHarness(t)
	mine := h.connect(t, id.New())
	theirs := h.connect(t, id.New())
	token := mine.resumeToken()
	h.gw.suspend(mine)

	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	// A valid credential — for the wrong device.
	p, err := payload(h.tokenFor(theirs.deviceID), token, h.generation, uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

	if in := h.readFrom(t, s); in.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session", in.Op)
	}
	if h.gw.Online(mine.deviceID) {
		t.Fatal("another device's session token resumed this connection")
	}
}

// And a resume carrying no credential at all — neither in the payload nor on the HTTP upgrade —
// is refused rather than accepted on the resume token.
func TestResumeIsRefusedWithoutAnyCredential(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	token := c.resumeToken()
	h.gw.suspend(c)

	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	p, err := payload("", token, h.generation, uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

	if in := h.readFrom(t, s); in.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session", in.Op)
	}
	if h.gw.Online(c.deviceID) {
		t.Fatal("a resume with an empty session token was accepted")
	}
}

// Two resumes in a row over two fresh sinks, driven the way a real client drives them: the token
// answered on the second resume is the one the FIRST `resumed` frame carried, never a read of the
// instance's private state. `resume_token` rotates on every `resumed`
// (facts-gateway-design.md §1.3), so a `resumed` that did not carry the rotated value would cap
// every client at one resume per identify — its second reconnect would answer a token the
// instance has already discarded. protocol/02's control catalogue op 4 is therefore
// `[replayed_from(uint), replayed_to(uint), resume_token(bstr 32)]`.
func TestAResumedConnectionCanResumeAgain(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	first := h.readyResumeToken(t, c)
	second := h.resumeOnce(t, c, first)
	if bytes.Equal(first, second) {
		t.Fatal("the resume token must rotate on every resumed")
	}
	third := h.resumeOnce(t, c, second)
	if bytes.Equal(second, third) {
		t.Fatal("the second resumed must rotate the token too")
	}
}

// readyResumeToken is the client's first sight of its resume token: element 3 of `ready`.
func (h *harness) readyResumeToken(t *testing.T, c *conn) []byte {
	t.Helper()
	in := h.waitFrame(t, c.deviceID)
	if in.Op != OpReady {
		t.Fatalf("first frame op %d, want ready", in.Op)
	}
	token, err := rawBytes(in.Payload[3])
	if err != nil {
		t.Fatalf("ready resume_token: %v", err)
	}
	return token
}

// resumeOnce suspends the connection, resumes it over a brand-new sink, insists the first frame
// is `resumed`, and answers the rotated token that frame itself carries.
func (h *harness) resumeOnce(t *testing.T, c *conn, token []byte) []byte {
	t.Helper()
	h.gw.suspend(c)
	s := newRecordingSink(16, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	p, err := payload(h.tokenFor(c.deviceID), token, h.generation, uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))
	in := h.readFrom(t, s)
	if in.Op != OpResumed {
		t.Fatalf("op %d, want resumed", in.Op)
	}
	if len(in.Payload) != 3 {
		t.Fatalf("resumed carries %d elements, want 3: element 2 is the rotated resume_token, "+
			"without which a client can resume at most once", len(in.Payload))
	}
	next, err := rawBytes(in.Payload[2])
	if err != nil {
		t.Fatalf("resumed resume_token: %v", err)
	}
	if len(next) != 32 {
		t.Fatalf("resumed resume_token is %d bytes, want 32", len(next))
	}
	if !bytes.Equal(next, c.resumeToken()) {
		t.Fatal("the token on the wire is not the one the instance kept")
	}
	return next
}
