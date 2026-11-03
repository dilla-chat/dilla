package dilladtest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedPerson seeds one human with one device and satisfies the second factor a real sign-up would
// have asked for, so the session reaches the gateway and the call routes. It returns the user, the
// device and the device's session token.
func seedPerson(t *testing.T, h *Host, name string) (user, device id.ID, token string) {
	t.Helper()
	user, device, token = seedOwedPerson(t, h, name)
	satisfyFactor(t, h, user.String())
	return user, device, token
}

// seedOwedPerson is seedPerson without the satisfied factor: the account owes one, as a sign-up
// leaves it.
func seedOwedPerson(t *testing.T, h *Host, name string) (user, device id.ID, token string) {
	t.Helper()
	zero := func(n int) string { return hex.EncodeToString(make([]byte, n)) }
	device = id.New()
	out, err := SeedUsers(t.Context(), h.Server(), []SeedRequest{{
		Username: name, Display: name, UMKPub: zero(32), SSKPub: zero(32), SigUMKSSK: zero(64),
		Devices: []Device{{DeviceID: device.String(), DSKPub: zero(32), Credential: "01"}},
	}})
	if err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}
	user, err = id.Parse(out[0].UserID)
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	return user, device, out[0].Tokens[device.String()]
}

func controlPost(t *testing.T, base, path string, body any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

func TestTheClockRouteAnswersTheHostClockAndMovesWithTheAdvance(t *testing.T) {
	h := sfuHost(t, false)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	read := func() int64 {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, control.URL+"/debug/clock", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /debug/clock: %v", err)
		}
		defer func() { _ = res.Body.Close() }()
		var out struct {
			Now int64 `json:"now"`
		}
		if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil {
			t.Fatalf("GET /debug/clock = %d", res.StatusCode)
		}
		return out.Now
	}
	before := read()
	if before != h.Clock().Now().Unix() {
		t.Fatalf("now = %d, want the host clock %d", before, h.Clock().Now().Unix())
	}
	if status := controlPost(t, control.URL, "/debug/clock", map[string]int64{"seconds": 90}); status != http.StatusNoContent {
		t.Fatalf("POST /debug/clock = %d", status)
	}
	if after := read(); after != before+90 {
		t.Fatalf("now after +90 s = %d, want %d", after, before+90)
	}
}

func TestTheResetRouteRemovesTheFactorsKeepsTheSessionAndRefusesAnUnknownName(t *testing.T) {
	h := sfuHost(t, false)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	alice, _, aliceTok := seedPerson(t, h, "alice")
	bob, _, _ := seedPerson(t, h, "bob")
	repo := h.Server().Repo()
	confirmed := h.Clock().Now().Unix()
	for _, u := range []id.ID{alice, bob} {
		if err := repo.PutTOTP(t.Context(), store.TOTPRow{UserID: u, Secret: make([]byte, 20), Digits: 6, Period: 30,
			Algorithm: "SHA1", ConfirmedAt: &confirmed, Created: confirmed}); err != nil {
			t.Fatalf("PutTOTP: %v", err)
		}
	}

	if status := controlPost(t, control.URL, "/debug/reset-factors", map[string]string{"username": "nobody"}); status != http.StatusNotFound {
		t.Fatalf("an unknown username = %d, want 404", status)
	}
	if _, err := repo.GetTOTP(t.Context(), alice); err != nil {
		t.Fatalf("a refused reset removed the TOTP: %v", err)
	}

	if status := controlPost(t, control.URL, "/debug/reset-factors", map[string]string{"username": "alice"}); status != http.StatusNoContent {
		t.Fatalf("POST /debug/reset-factors = %d, want 204", status)
	}
	if _, err := repo.GetTOTP(t.Context(), alice); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("alice's TOTP survived: %v", err)
	}
	if u, _ := repo.GetUser(t.Context(), alice); !u.FactorOwed {
		t.Error("alice does not owe a factor")
	}
	if _, err := repo.GetSessionByHash(t.Context(), auth.TokenHash(aliceTok), h.Clock().Now().Unix()); err != nil {
		t.Errorf("alice's session did not survive (D5): %v", err)
	}
	// Control: bob, not named, keeps his factor and owes nothing.
	if _, err := repo.GetTOTP(t.Context(), bob); err != nil {
		t.Errorf("bob's TOTP was removed: %v", err)
	}
	if u, _ := repo.GetUser(t.Context(), bob); u.FactorOwed {
		t.Error("bob owes a factor")
	}
}

// identify opens /gateway with the bearer token and sends identify, returning the first frame's op
// and the connection.
func identify(t *testing.T, base, token string) (*websocket.Conn, uint64, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	readOp := func() (uint64, error) {
		_, b, err := c.Read(ctx)
		if err != nil {
			return 0, err
		}
		var frame []any
		if err := cborx.Unmarshal(b, &frame); err != nil || len(frame) != 4 {
			t.Fatalf("frame %x: %v", b, err)
		}
		op, _ := frame[0].(uint64)
		return op, nil
	}
	if op, err := readOp(); err != nil || op != 0 {
		t.Fatalf("first frame op %d (%v), want hello (0)", op, err)
	}
	// The wire version the instance offers (web3.8 S1).
	frame, err := cborx.Marshal([]any{uint64(1), uint64(1), nil, []any{"", uint64(dillad.WireVersion), uint64(1), uint64(1), uint64(0)}})
	if err != nil {
		t.Fatalf("marshal identify: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write identify: %v", err)
	}
	op, err := readOp()
	return c, op, err
}

// A live socket of a person whose factors the operator resets is closed 4003 at once, and their
// reconnect is refused: the session still resolves, but the account owes a factor. The control is
// the same person before the reset, who reaches ready.
func TestTheResetClosesALiveGatewaySocketAtOnce(t *testing.T) {
	h := sfuHost(t, false)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	_, _, token := seedPerson(t, h, "alice")

	conn, op, err := identify(t, ts.URL, token)
	if err != nil || op != 3 {
		t.Fatalf("before the reset: op %d (%v), want ready (3)", op, err)
	}
	if status := controlPost(t, control.URL, "/debug/reset-factors", map[string]string{"username": "alice"}); status != http.StatusNoContent {
		t.Fatalf("reset = %d", status)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var readErr error
	for readErr == nil {
		_, _, readErr = conn.Read(ctx)
	}
	if got := websocket.CloseStatus(readErr); got != 4003 {
		t.Fatalf("the live socket ended with %v (status %d), want close 4003", readErr, got)
	}

	if _, op, err := identify(t, ts.URL, token); err == nil && op == 3 {
		t.Fatal("a reconnect after the reset reached ready")
	}
}

// With an SFU, a person in a live call is cut from its room when the operator resets their factors:
// the LiveKit token they hold would otherwise stay good until it expires. The control is the
// participant listed in the room until the reset.
func TestTheResetCutsALiveCall(t *testing.T) {
	h := sfuHost(t, true)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	out, device, _ := openCallOn(t, h)
	token, _ := out[3].(string)
	room, err := lksdk.ConnectToRoomWithToken(h.SFU().URL(), token, &lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
	if err != nil {
		t.Fatalf("connect to the call's room: %v", err)
	}
	t.Cleanup(room.Disconnect)
	inRoom := func() bool {
		ps, err := h.SFU().Participants(t.Context(), room.Name())
		if err != nil {
			t.Fatalf("Participants: %v", err)
		}
		for _, p := range ps {
			if p.Identity == device.String() {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(20 * time.Second)
	for !inRoom() {
		if time.Now().After(deadline) {
			t.Fatal("the participant never joined the call's room")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status := controlPost(t, control.URL, "/debug/reset-factors", map[string]string{"username": "caller"}); status != http.StatusNoContent {
		t.Fatalf("reset = %d", status)
	}
	// The cut is woken at once; the call retry loop's own tick is 5 s and its room sweep 30 s, so
	// 3 s tells the reset's cut from both.
	deadline = time.Now().Add(3 * time.Second)
	for inRoom() {
		if time.Now().After(deadline) {
			t.Fatal("the reset left the person in the call's room")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
