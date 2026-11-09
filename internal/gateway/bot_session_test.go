package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type botSweepAuth struct {
	Authenticator
	revoked bool
	checks  int
}

func (a *botSweepAuth) ValidateSessionHash(context.Context, []byte) error {
	a.checks++
	if a.revoked {
		return store.ErrNotFound
	}
	return nil
}

type botSweepStore struct {
	Store
	row store.SessionRow
}

func (s botSweepStore) GetSessionByHash(context.Context, []byte, int64) (store.SessionRow, error) {
	if s.row.TokenHash == nil {
		return store.SessionRow{}, store.ErrNotFound
	}
	return s.row, nil
}

func TestGatewayRequiresBoundJoiningOrReadyBotAtWireThree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state uint8
		wire  uint64
		allow bool
	}{
		{name: "enrolling", state: store.BotInstallationEnrolling, wire: 3},
		{name: "joining wire two", state: store.BotInstallationJoining, wire: 2, allow: true},
		{name: "joining wire three", state: store.BotInstallationJoining, wire: 3, allow: true},
		{name: "ready wire three", state: store.BotInstallationReady, wire: 3, allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			device := id.New()
			bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: tc.state}
			session := auth.Session{Scope: auth.ScopeEnrolled, WireVersion: tc.wire, Bot: &bot}
			token := scopedSession(h, device, session)
			got, err := h.gw.identify(context.Background(), token)
			if tc.allow {
				if err != nil || got.Bot == nil {
					t.Fatalf("identify = %+v, %v; want bound bot", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("identify accepted phase=%d wire=%d", tc.state, tc.wire)
			}
		})
	}
}

func TestManagedBotIdentifyUsesSelectedWireAndPersistsIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		persisted uint64
	}{{name: "unnegotiated", persisted: 0}, {name: "wire-two", persisted: 2}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.gw.opts.WireVersions = []uint64{6}
			recorder := &capabilityRecordingAuth{Authenticator: h}
			h.gw.opts.Auth = recorder
			device := id.New()
			hash := []byte("01234567890123456789012345678901")
			bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationJoining}
			session := auth.Session{Scope: auth.ScopeEnrolled, WireVersion: tc.persisted, TokenHash: hash, Bot: &bot}
			token := scopedSession(h, device, session)
			in := h.onBotSocket(identifyInbound(t, token, 6))
			sink := newRecordingSink(8, false)
			c, ok := h.gw.identifyConnection(context.Background(), sink, in)
			if !ok || c == nil {
				t.Fatal("selected wire 6 was refused for a valid Joining bot")
			}
			if got, ok := recorder.selected(hash); !ok || got != [3]uint64{6, 1, 1} {
				t.Fatalf("selected capabilities persisted = %v, %v; want [6 1 1]", got, ok)
			}
			if ready := h.readFrom(t, sink); ready.Op != OpReady {
				t.Fatalf("first frame op=%d, want ready", ready.Op)
			}
		})
	}
}

// Bot v2: a managed Bot identifies on wire 6 only, even where the instance still advertises an
// older wire to someone else; anything older is E_UPGRADE_REQUIRED with close 4006.
func TestManagedBotIdentifySelectedWireTwoReturnsUpgradeRequired(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{6, 2}
	recorder := &capabilityRecordingAuth{Authenticator: h}
	h.gw.opts.Auth = recorder
	device := id.New()
	hash := []byte("01234567890123456789012345678901")
	bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationJoining}
	token := scopedSession(h, device, auth.Session{Scope: auth.ScopeEnrolled, WireVersion: 6, TokenHash: hash, Bot: &bot})
	sink := newRecordingSink(8, false)
	if _, ok := h.gw.identifyConnection(context.Background(), sink, h.onBotSocket(identifyInbound(t, token, 2))); ok {
		t.Fatal("selected wire 2 was accepted for a managed bot")
	}
	frame := h.readFrom(t, sink)
	if frame.Op != OpError {
		t.Fatalf("refusal op=%d, want error", frame.Op)
	}
	code, err := rawText(frame.Payload[1])
	if err != nil || code != "E_UPGRADE_REQUIRED" {
		t.Fatalf("error code=%q err=%v, want E_UPGRADE_REQUIRED", code, err)
	}
	if got := h.waitCloseOn(t, sink); got != CloseVersion {
		t.Fatalf("close code=%d, want %d", got, CloseVersion)
	}
	if _, ok := recorder.selected(hash); ok {
		t.Fatal("wire-2 refusal persisted unsupported bot capability")
	}
}

// app_build tells a web client to update; a managed Bot connection is not sent it (Bot v2 task 5, review M6).
func TestManagedBotConnectionIsNotSentAppBuild(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{6}
	h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
	h.gw.opts.CurrentAppEntry = func() string { return "/assets/index-current.js" }
	device := id.New()
	bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationJoining}
	token := scopedSession(h, device, auth.Session{Scope: auth.ScopeEnrolled, WireVersion: 0, TokenHash: []byte("01234567890123456789012345678901"), Bot: &bot})
	sink := newRecordingSink(16, false)
	c, ok := h.gw.identifyConnection(context.Background(), sink, h.onBotSocket(identifyInbound(t, token, 6)))
	if !ok {
		t.Fatal("a ready Bot on wire 6 was refused")
	}
	if !c.isBot {
		t.Fatal("the connection is not marked as a Bot's")
	}
	h.gw.sendAppBuild(c)
	for {
		select {
		case raw := <-sink.frames:
			if in, err := Decode(raw, 1<<20); err == nil && in.Op == OpAppBuild {
				t.Fatal("a managed Bot connection was sent app_build")
			}
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func identifyInbound(t *testing.T, token string, wire uint64) Inbound {
	t.Helper()
	b, err := payload(token, wire, uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	var raw []cbor.RawMessage
	if err := cborx.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	return Inbound{Payload: raw}
}

func TestManagedBotResumeRequiresOriginalAndCurrentBearerWireSix(t *testing.T) {
	for _, tc := range []struct {
		name           string
		original, auth uint64
	}{
		{name: "cannot borrow replacement bearer wire", original: 5, auth: 6},
		{name: "replacement bearer must have negotiated wire", original: 6, auth: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			recorder := &capabilityRecordingAuth{Authenticator: h}
			h.gw.opts.Auth = recorder
			device := id.New()
			bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationJoining}
			originalHash := make([]byte, 32)
			originalHash[0] = 1
			original := auth.Session{Scope: auth.ScopeEnrolled, WireVersion: 6, TokenHash: originalHash, Bot: &bot}
			firstToken := scopedSession(h, device, original)
			original, err := h.gw.identify(context.Background(), firstToken)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := h.gw.registerVersions(context.Background(), original, newRecordingSink(8, false), [3]uint64{6, 1, 1})
			if err != nil {
				t.Fatal(err)
			}
			if tc.original == 5 {
				conn.mu.Lock()
				conn.wire = 5 // legacy suspended connection fixture
				conn.mu.Unlock()
			}
			oldResume := conn.resumeToken()
			h.gw.suspend(conn)
			replacementHash := make([]byte, 32)
			replacementHash[0] = 2
			replacement := original
			replacement.TokenHash = replacementHash
			replacement.WireVersion = tc.auth
			h.mu.Lock()
			h.tokens["replacement"] = replacement
			h.mu.Unlock()
			p, err := payload("replacement", oldResume, h.generation, uint64(0))
			if err != nil {
				t.Fatal(err)
			}
			var raw []cbor.RawMessage
			if err := cborx.Unmarshal(p, &raw); err != nil {
				t.Fatal(err)
			}
			sink := newRecordingSink(8, false)
			if _, ok := h.gw.resumeConnection(context.Background(), sink, Inbound{Payload: raw}); ok {
				t.Fatal("bot resumed without wire 6 on both original connection and current bearer")
			}
			frame := h.readFrom(t, sink)
			if frame.Op != OpInvalidSession || len(frame.Payload) == 0 {
				t.Fatalf("resume refusal frame=%+v, want nonresumable invalid_session", frame)
			}
			resumable, err := rawUint(frame.Payload[0])
			if err != nil || resumable != 0 {
				t.Fatalf("invalid_session resumable=%d err=%v, want 0", resumable, err)
			}
			if _, exists := h.gw.suspended.Load(string(oldResume)); !exists {
				t.Fatal("wire refusal consumed the suspended connection before replay authorization")
			}
		})
	}
}

func TestGatewayHeartbeatSweepCutsBotWhenRuntimeIsRevoked(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	sess := session(device)
	sess.TokenHash = make([]byte, 32)
	sess.TokenHash[0] = 1
	c, err := h.gw.register(context.Background(), sess, newRecordingSink(16, false))
	if err != nil {
		t.Fatal(err)
	}
	c.isBot = true
	storeStub := botSweepStore{Store: h, row: store.SessionRow{TokenHash: c.tokenHash, UserID: c.userID, DeviceID: c.deviceID}}
	authStub := &botSweepAuth{Authenticator: h}
	h.gw.opts.Store = storeStub
	h.gw.opts.Auth = authStub
	h.gw.sweepSessions(context.Background())
	if c.noResume {
		t.Fatal("live bot session was closed before revocation")
	}
	authStub.revoked = true
	h.gw.sweepSessions(context.Background())
	if !c.noResume {
		t.Fatalf("revoked bot connection remained resumable (validator calls=%d)", authStub.checks)
	}
}
