package api_test

import (
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// fakeRelay records the relay revocations the call routes make.
type fakeRelay struct {
	mu      sync.Mutex
	revoked []id.ID
	minted  []id.ID
}

func (f *fakeRelay) Revoke(device id.ID, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, device)
}

func (f *fakeRelay) Mint(device id.ID, _ time.Time) (time.Time, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted = append(f.minted, device)
	return time.Now(), 0
}

func (f *fakeRelay) has(device id.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.revoked, device)
}

// waitRevoked waits up to two seconds for device's relay revocation.
func waitRevoked(f *fakeRelay, device id.ID) bool {
	deadline := time.Now().Add(2 * time.Second)
	for !f.has(device) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// Review I1: CutDevice (the in-process revocation, quarantine and logout paths) revokes the
// device's relay credentials synchronously, before anything else — also on an instance with no
// SFU, where the call cut itself returns early.
func TestCutDeviceRevokesTheRelayAtOnce(t *testing.T) {
	b := newBarredEnv(t)
	relay := &fakeRelay{}
	b.calls.WithRelay(relay)
	b.calls.CutDevice(t.Context(), b.memberDev) // no retry loop runs: only the synchronous part
	if !relay.has(b.memberDev) {
		t.Fatal("CutDevice did not revoke the device's relay credentials synchronously")
	}

	e := newEnv(t)
	noSFU := api.NewCalls(e.Repo, api.NewResolver(e.Repo), nil, api.CallsConfig{}, e.Clk, slog.New(slog.DiscardHandler))
	relay2 := &fakeRelay{}
	noSFU.WithRelay(relay2)
	dev := id.New()
	noSFU.CutDevice(t.Context(), dev)
	if !relay2.has(dev) {
		t.Fatal("CutDevice on an instance with no SFU did not revoke the relay")
	}
}

// Review I1: CutUser revokes the relay of every device of the user, resolved on the retry loop.
func TestCutUserRevokesTheRelayOfEveryDevice(t *testing.T) {
	b := newBarredEnv(t)
	relay := &fakeRelay{}
	b.calls.WithRelay(relay)
	stop := b.calls.StartRetries(time.Hour)
	defer stop()
	if err := b.e.Repo.SetUserDisabled(t.Context(), b.member, ptr(b.e.Clk.Now().Unix())); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	b.calls.CutUser(t.Context(), b.member)
	if !waitRevoked(relay, b.memberDev) {
		t.Fatal("CutUser did not revoke the relay of the user's device")
	}
	if relay.has(b.ownerDev) {
		t.Fatal("CutUser revoked another user's device")
	}
}

// Review I1: the room sweep revokes the relay of a barred device it removes (a change another
// process wrote), and of no one else.
func TestTheRoomSweepRevokesABarredDevicesRelay(t *testing.T) {
	b := newBarredEnv(t)
	relay := &fakeRelay{}
	b.calls.WithRelay(relay)
	if err := b.e.Repo.RevokeDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	b.calls.SweepRooms(t.Context())
	if !relay.has(b.memberDev) {
		t.Fatal("the sweep did not revoke the barred device's relay")
	}
	if relay.has(b.ownerDev) {
		t.Fatal("the sweep revoked the relay of a device that is not barred")
	}
}

// Commit review, revocation parity: every cut from a call room also cuts the device from the relay
// — not only a barred one. A device that lost view_channel (a kick's effect on the channel), lost
// connect, or whose leaf the call group removed is revoked when the sweep removes it, and an
// eviction by the delivery service revokes at once; a device left in the room is not. (What a cut
// means for credentials — the old one refused, one issued after it admitted — is the relay's:
// internal/server TestARevokedCredentialIsRefusedOnEveryMethod and TestRevokingADeviceClosesItsRelay.)
func TestEveryCutFromACallRoomRevokesTheRelay(t *testing.T) {
	for name, cut := range map[string]func(t *testing.T, e *env, ch, group, member, memberDev id.ID){
		"lost view_channel": func(t *testing.T, e *env, ch, _, member, _ id.ID) {
			denyInChannel(t, e, ch, member, api.PermViewChannel)
		},
		"lost connect": func(t *testing.T, e *env, ch, _, member, _ id.ID) {
			denyInChannel(t, e, ch, member, api.PermConnect)
		},
		"leaf removed": func(t *testing.T, e *env, _, group, _, memberDev id.ID) {
			dropLeaf(t, e, group, memberDev)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
			owner := deviceOf(t, e, tok)
			seedLeaf(t, e, group, owner, 3, nil)
			if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusCreated {
				t.Fatalf("start = %d", status)
			}
			member, memberDev, _ := joinedMember(t, e, ch, group, "member")
			room := stub.minted()[0][0]
			stub.setPresent(room, &livekit.ParticipantInfo{Identity: owner.String()},
				&livekit.ParticipantInfo{Identity: memberDev.String()})
			relay := &fakeRelay{}
			calls.WithRelay(relay)
			cut(t, e, ch, group, member, memberDev)
			calls.SweepRooms(t.Context())
			if !removedDevice(stub, room, memberDev) {
				t.Fatal("the sweep did not cut the device from the room")
			}
			if !relay.has(memberDev) {
				t.Fatal("the device was cut from the room but kept the relay")
			}
			if relay.has(owner) {
				t.Fatal("a device left in the room lost the relay")
			}
		})
	}
	t.Run("evicted by the delivery service", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		relay := &fakeRelay{}
		f.calls.WithRelay(relay)
		f.events.Evict(t.Context(), f.group, []id.ID{f.dev})
		if !relay.has(f.dev) {
			t.Fatal("an evicted device kept the relay")
		}
	})
}

// Commit review (cut-map amplification): a start that mints a relay credential tells the relay,
// which records cuts only for devices that can hold one.
func TestAStartTellsTheRelayItMintedACredential(t *testing.T) {
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL,
		TURNSecret: "0123456789abcdef", TURNURLs: []string{"turns:chat.example.test:443?transport=tcp"}, CredentialTTL: time.Hour})
	relay := &fakeRelay{}
	calls.WithRelay(relay)
	dev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusCreated {
		t.Fatalf("start = %d", status)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if !slices.Equal(relay.minted, []id.ID{dev}) {
		t.Fatalf("minted = %v, want the starting device %s", relay.minted, dev)
	}
}

// startParked starts a call on a relay-enabled instance whose start parks at stage until the
// returned release runs; parked is closed when it waits. done yields the start's status and body.
func startParked(t *testing.T, stage string) (e *env, dev id.ID, rev *server.RelayRevocations,
	parked <-chan struct{}, release func(), done <-chan [2]any) {
	t.Helper()
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL,
		TURNSecret: "0123456789abcdef", TURNURLs: []string{"turns:chat.example.test:443?transport=tcp"}, CredentialTTL: time.Hour})
	rev = server.NewRelayRevocations(2*time.Hour, e.Clk)
	calls.WithRelay(rev)
	dev = deviceOf(t, e, tok)
	seedLeaf(t, e, group, dev, 3, nil)
	p, rel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls.SetStartHookForTest(func(s string) {
		if s == stage {
			once.Do(func() { close(p) })
			<-rel
		}
	})
	out := make(chan [2]any, 1)
	go func() {
		status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
		out <- [2]any{status, body}
	}()
	select {
	case <-p:
	case <-time.After(10 * time.Second):
		t.Fatalf("the start never reached %q", stage)
	}
	return e, dev, rev, p, func() { close(rel) }, out
}

// issuedOfCall is the issue time of the relay credential a start answered.
func issuedOfCall(t *testing.T, body []byte) time.Time {
	t.Helper()
	ice := decodeCall(t, body).ICE
	if len(ice) != 1 {
		t.Fatalf("ice_servers = %v, want one relay", ice)
	}
	var user string
	mustUnmarshal(t, ice[0][1], &user)
	fields := strings.Split(user, ":")
	issued, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil || len(fields) != 3 {
		t.Fatalf("username %q carries no issue time", user)
	}
	return time.UnixMilli(issued) // "<expiry_s>:<device_id>:<issued_ms>"
}

// retryAfterOf is the retry_after_ms of an error body.
func retryAfterOf(t *testing.T, body []byte) uint64 {
	t.Helper()
	var arr []cbor.RawMessage
	mustUnmarshalBody(t, body, &arr)
	var ms uint64
	if len(arr) < 3 {
		t.Fatalf("error body %x has no retry_after_ms", body)
	}
	mustUnmarshal(t, arr[2], &ms)
	return ms
}

// Re-review N1, through the call route: a start whose device was cut in the same millisecond is a
// transient 429 E_RATE_LIMITED with a retry_after_ms of a few milliseconds, and moves nothing — a
// member whose every start lands in its own cut's millisecond, twice a second for two simulated
// minutes, leaves no trace on anyone else: a victim cut afterwards starts a millisecond later, with a
// credential issued at the clock's time, and so does the member itself once it waits retry_after_ms.
func TestAStartInItsCutsMillisecondIsTransientAndMovesNothing(t *testing.T) {
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL,
		TURNSecret: "0123456789abcdef", TURNURLs: []string{"turns:chat.example.test:443?transport=tcp"}, CredentialTTL: time.Hour})
	rev := server.NewRelayRevocations(2*time.Hour, e.Clk).WithCredentialTTL(time.Hour)
	calls.WithRelay(rev)
	victim := deviceOf(t, e, tok)
	seedLeaf(t, e, group, victim, 3, nil)
	_, attacker, attackerTok := joinedMember(t, e, ch, group, "attacker")
	path := "/v1/channels/" + ch.String() + "/calls"
	start := func(tok string) (int, []byte) { return e.Do(http.MethodPost, path, tok, []any{}) }
	for _, tk := range []string{tok, attackerTok} { // each device holds a credential, so its cuts count
		if status, body := start(tk); status != http.StatusCreated && status != http.StatusOK {
			t.Fatalf("first start = %d %s", status, e.ErrCode(body))
		}
	}
	for i := range 240 { // two a second for two minutes, each in its own cut's millisecond
		e.Clk.Advance(500 * time.Millisecond)
		rev.Revoke(attacker, e.Clk.Now())
		status, body := start(attackerTok)
		if status != http.StatusTooManyRequests || e.ErrCode(body) != "E_RATE_LIMITED" {
			t.Fatalf("attacker start %d in its cut's millisecond = %d %s, want 429 E_RATE_LIMITED", i, status, e.ErrCode(body))
		}
		if ms := retryAfterOf(t, body); ms < 1 || ms > 10 {
			t.Fatalf("retry_after_ms = %d, want a few milliseconds", ms)
		}
	}
	e.Clk.Advance(time.Second)
	rev.Revoke(victim, e.Clk.Now())
	e.Clk.Advance(time.Millisecond)
	status, body := start(tok)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("the victim's start a millisecond after its cut = %d %s, want it to succeed at once", status, e.ErrCode(body))
	}
	if issued := issuedOfCall(t, body); !issued.Equal(e.Clk.Now()) {
		t.Fatalf("the victim's credential was issued at %v, %v ahead of the clock", issued, issued.Sub(e.Clk.Now()))
	}
	// The honest retry: a start in its cut's millisecond waits retry_after_ms and gets in.
	rev.Revoke(attacker, e.Clk.Now())
	status, body = start(attackerTok)
	if status != http.StatusTooManyRequests {
		t.Fatalf("a start in its cut's millisecond = %d, want 429", status)
	}
	e.Clk.Advance(time.Duration(retryAfterOf(t, body)) * time.Millisecond)
	status, body = start(attackerTok)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("the retry after retry_after_ms = %d %s, want it to succeed", status, e.ErrCode(body))
	}
	if issued := issuedOfCall(t, body); !issued.Equal(e.Clk.Now()) {
		t.Fatalf("the retried credential was issued at %v, not the clock's %v", issued, e.Clk.Now())
	}
}

// cutCovers reports whether rev holds a cut of dev at or after issued — one that refuses a
// credential issued then: a mint for a request begun at issued is refused exactly then.
func cutCovers(rev *server.RelayRevocations, dev id.ID, issued time.Time) bool {
	_, wait := rev.Mint(dev, issued)
	return wait > 0
}

// Commit review (mint race), re-review N1: no relay credential a start answers is usable after a cut
// that landed while the start was being served. A cut between the gates and the mint gets no
// credential, a transient 429 E_RATE_LIMITED (the retry's own gates answer whether the cut still
// applies); a cut between the mint and the response covers the credential (the relay refuses it); a
// device another process barred between the gates and the mint is refused 403 by the re-check after
// the mint, and its relay is cut, which covers the credential that was minted.
func TestNoCredentialFromAStartOutlivesACutDuringIt(t *testing.T) {
	t.Run("a cut between the gates and the mint", func(t *testing.T) {
		e, dev, rev, _, release, done := startParked(t, "mint")
		e.Clk.Advance(time.Millisecond) // the cut lands after the start began, not in its millisecond
		rev.Revoke(dev, e.Clk.Now())
		e.Clk.Advance(time.Millisecond)
		release()
		res := <-done
		if status := res[0].(int); status != http.StatusTooManyRequests || e.ErrCode(res[1].([]byte)) != "E_RATE_LIMITED" {
			t.Fatalf("a start cut before its mint = %d %s, want 429 E_RATE_LIMITED and no credential", status, e.ErrCode(res[1].([]byte)))
		}
		if ms := retryAfterOf(t, res[1].([]byte)); ms < 1 || ms > 10 {
			t.Fatalf("retry_after_ms = %d, want a few milliseconds", ms)
		}
	})
	t.Run("a cut between the mint and the response", func(t *testing.T) {
		e, dev, rev, _, release, done := startParked(t, "respond")
		rev.Revoke(dev, e.Clk.Now())
		release()
		res := <-done
		if status := res[0].(int); status != http.StatusCreated {
			t.Fatalf("start = %d %s", status, e.ErrCode(res[1].([]byte)))
		}
		if issued := issuedOfCall(t, res[1].([]byte)); !cutCovers(rev, dev, issued) {
			t.Fatalf("the credential issued at %v outlived the cut that followed its mint", issued)
		}
	})
	t.Run("barred by another process between the gates and the mint", func(t *testing.T) {
		e, dev, rev, _, release, done := startParked(t, "mint")
		if err := e.Repo.RevokeDevice(t.Context(), dev, e.Clk.Now().Unix()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		release()
		res := <-done
		if status := res[0].(int); status != http.StatusForbidden || e.ErrCode(res[1].([]byte)) != "E_FORBIDDEN" {
			t.Fatalf("a start barred before its mint = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(res[1].([]byte)))
		}
		// What the start minted carried the clock's time; the refusal's cut covers it, and no later
		// time: a credential issued a millisecond on is not cut.
		if !cutCovers(rev, dev, e.Clk.Now()) {
			t.Fatal("the refused start did not cut the credential it minted")
		}
		e.Clk.Advance(time.Millisecond)
		if cutCovers(rev, dev, e.Clk.Now()) {
			t.Fatal("the refused start's cut is ahead of the clock")
		}
	})
}

// BarredDevices is the relay's store lookup: barred as the call routes say; an id with no device
// row (doctor's probe: rows are never deleted) is not.
func TestBarredDevicesAnswersAsTheCallRoutes(t *testing.T) {
	b := newBarredEnv(t)
	look := api.BarredDevices{Repo: b.e.Repo}
	if bar, err := look.DeviceBarred(t.Context(), b.memberDev); err != nil || bar {
		t.Fatalf("a device in good standing = %v, %v", bar, err)
	}
	if bar, err := look.DeviceBarred(t.Context(), id.New()); err != nil || bar {
		t.Fatalf("an id with no device row = %v, %v; want not barred", bar, err)
	}
	if err := b.e.Repo.SetUserDisabled(t.Context(), b.member, ptr(b.e.Clk.Now().Unix())); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if bar, err := look.DeviceBarred(t.Context(), b.memberDev); err != nil || !bar {
		t.Fatalf("a disabled user's device = %v, %v; want barred", bar, err)
	}
	if err := b.e.Repo.SetUserDisabled(t.Context(), b.member, nil); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if err := b.e.Repo.QuarantineDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix(), "fork quorum"); err != nil {
		t.Fatalf("QuarantineDevice: %v", err)
	}
	if bar, err := look.DeviceBarred(t.Context(), b.memberDev); err != nil || !bar {
		t.Fatalf("a quarantined device = %v, %v; want barred", bar, err)
	}
}
