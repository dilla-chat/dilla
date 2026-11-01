package dillad_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Session capability negotiation adds bot wire 3 while preserving wire 2. Wire 1 remains
// unsupported, and discovery must match the hello offer.

// dialHello opens /gateway without a credential on the upgrade and returns the connection and the
// hello payload.
func dialHello(t *testing.T, ctx context.Context, base string) (*websocket.Conn, []any) {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{Subprotocols: []string{"dilla.v1"}})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	op, hello := readFrame(t, ctx, c)
	if op != 0 {
		t.Fatalf("first frame op %d, want hello (0)", op)
	}
	return c, hello
}

// identifyWith sends op 1 carrying the bearer in its first element and wire as its wire version;
// e2ee, media and caps are 1, 1 and 0.
func identifyWith(t *testing.T, ctx context.Context, c *websocket.Conn, bearer string, wire uint64) {
	t.Helper()
	b, err := cborx.Marshal([]any{uint64(1), uint64(1), nil,
		[]any{bearer, wire, uint64(1), uint64(1), uint64(0)}})
	if err != nil {
		t.Fatalf("marshal identify: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatalf("write identify: %v", err)
	}
}

func TestTheGatewayOffersWireFiveFourThreeAndTwo(t *testing.T) {
	_, ts, _ := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, hello := dialHello(t, ctx, ts.URL)
	for i, want := range [][]uint64{{5, 4, 3, 2}, {1}, {1}} {
		names := []string{"wire_versions", "e2ee_versions", "media_versions"}
		if got := uints(t, hello[i], names[i]); !slices.Equal(got, want) {
			t.Fatalf("hello %s = %v, want %v", names[i], got, want)
		}
	}
}

// The refusal and its control differ in the wire version alone: the same session token in the
// identify's first element.
func TestIdentifyWithWireOneIsRefused(t *testing.T) {
	_, ts, token := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _ := dialHello(t, ctx, ts.URL)
	identifyWith(t, ctx, c, token, 1)
	op, payload := readFrame(t, ctx, c)
	if op != 9 || len(payload) < 2 || payload[1] != "E_VERSION" {
		t.Fatalf("identify with wire 1: op %d %v, want an error frame (9) naming E_VERSION", op, payload)
	}
	_, _, err := c.Read(ctx)
	if got := websocket.CloseStatus(err); got != 4006 {
		t.Fatalf("identify with wire 1 ended with %v (close %d), want close 4006", err, got)
	}

	// Control: the same identify with wire 2 reaches ready.
	c2, _ := dialHello(t, ctx, ts.URL)
	identifyWith(t, ctx, c2, token, 2)
	if op, payload := readFrame(t, ctx, c2); op != 3 {
		t.Fatalf("control: identify with wire 2: op %d %v, want ready (3)", op, payload)
	}
}

func TestIdentifyWithWireThreeReachesReady(t *testing.T) {
	srv, ts, token := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _ := dialHello(t, ctx, ts.URL)
	identifyWith(t, ctx, c, token, 3)
	if op, payload := readFrame(t, ctx, c); op != 3 {
		t.Fatalf("identify with wire 3: op %d %v, want ready (3)", op, payload)
	}
	session, err := srv.Sessions().Resolve(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if session.WireVersion != 3 || session.E2EEVersion != 1 || session.MediaVersion != 1 {
		t.Fatalf("identify persisted versions = [%d %d %d], want [3 1 1]", session.WireVersion, session.E2EEVersion, session.MediaVersion)
	}
}

// The composed gateway resolves group ownership and current installation state through the real
// repository. An already identified wire-2 socket sees ordinary community traffic before an
// installation exists, is filtered as soon as one is committed, and resumes receiving after the
// installation is disabled.
func TestComposedGatewayReadsCurrentCommunityCompatibility(t *testing.T) {
	srv, ts, token := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _ := dialHello(t, ctx, ts.URL)
	identifyWith(t, ctx, c, token, 2)
	if op, _ := readFrame(t, ctx, c); op != 3 {
		t.Fatalf("identify op %d, want ready", op)
	}
	// Identify emits a presence update after ready; consume it before checking explicit delivery.
	if op, _ := readFrame(t, ctx, c); op != uint64(gateway.OpPresence) {
		t.Fatalf("post-ready op %d, want presence", op)
	}
	session, err := srv.Sessions().Resolve(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	now := srv.Now().Unix()
	communityID, groupID := id.New(), id.New()
	if err := srv.Repo().Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateCommunity(ctx, store.CommunityRow{ID: communityID, Owner: session.UserID,
			Name: "compatibility", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
			return err
		}
		community := communityID
		return tx.CreateGroup(ctx, store.GroupRow{GroupID: groupID, Binding: []byte{0x80}, Kind: api.GroupText,
			CommunityID: &community, TargetID: id.New(), Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(),
			E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now})
	}); err != nil {
		t.Fatalf("seed community/group: %v", err)
	}
	deliver := func() {
		group := groupID
		payload, err := cborx.Marshal("event")
		if err != nil {
			t.Fatal(err)
		}
		srv.Gateway().DeliverDevice(session.DeviceID, gateway.Frame{Op: gateway.OpMessageCT, GroupID: &group, Payload: payload})
	}
	deliver()
	if op, _ := readFrame(t, ctx, c); op != uint64(gateway.OpMessageCT) {
		t.Fatalf("pre-install delivery op %d, want message", op)
	}
	beforeBlocked := srv.Gateway().Debug(session.DeviceID, groupID).Conns[0].N
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: communityID, State: store.BotInstallationPending,
		Revision: 1, ApprovedBy: session.UserID, ApprovedAt: now, Created: now, Updated: now}
	if err := srv.Repo().CreateBotInstallation(ctx, installation); err != nil {
		t.Fatalf("CreateBotInstallation: %v", err)
	}
	deliver()
	if got := srv.Gateway().Debug(session.DeviceID, groupID).Conns[0].N; got != beforeBlocked {
		t.Fatalf("wire-2 replay n advanced after installation became active: %d -> %d", beforeBlocked, got)
	}
	installation.State = store.BotInstallationDisabled
	installation.Revision++
	installation.Updated = now + 1
	if err := srv.Repo().UpdateBotInstallation(ctx, installation.ID, 1, installation); err != nil {
		t.Fatalf("disable installation: %v", err)
	}
	deliver()
	if op, _ := readFrame(t, ctx, c); op != uint64(gateway.OpMessageCT) {
		t.Fatalf("post-disable delivery op %d, want message", op)
	}
}

func TestGatewayResumePersistsOriginalCapabilitiesOnReplacementBearer(t *testing.T) {
	srv, ts, token := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, hello := dialHello(t, ctx, ts.URL)
	identifyWith(t, ctx, c, token, 3)
	op, ready := readFrame(t, ctx, c)
	if op != 3 {
		t.Fatalf("identify op %d, want ready", op)
	}
	resumeToken, ok := ready[3].([]byte)
	if !ok || len(resumeToken) != 32 {
		t.Fatalf("ready resume token = %#v, want 32 bytes", ready[3])
	}
	first, err := srv.Sessions().Resolve(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	device, err := srv.Repo().GetDevice(ctx, first.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	var replacement auth.Token
	if err := srv.Repo().Tx(ctx, func(tx store.Repository) error {
		var issueErr error
		replacement, issueErr = srv.Sessions().NewDeviceSession(ctx, tx, first.UserID, first.DeviceID, device.Tier)
		return issueErr
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(websocket.StatusNormalClosure, "resume test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for srv.Gateway().Suspended() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("suspended = %d, want 1", srv.Gateway().Suspended())
		}
		time.Sleep(10 * time.Millisecond)
	}
	c2, resumedHello := dialHello(t, ctx, ts.URL)
	instanceA, _ := hello[5].([]byte)
	instanceB, _ := resumedHello[5].([]byte)
	if !bytes.Equal(instanceB, instanceA) {
		t.Fatal("resume connected to another instance")
	}
	frame, err := cborx.Marshal([]any{uint64(2), uint64(1), nil,
		[]any{replacement.Token, resumeToken, hello[6], uint64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	if op, payload := readFrame(t, ctx, c2); op != 4 {
		t.Fatalf("resume op %d %v, want resumed (4)", op, payload)
	}
	second, err := srv.Sessions().Resolve(ctx, replacement.Token)
	if err != nil {
		t.Fatal(err)
	}
	if second.WireVersion != 3 || second.E2EEVersion != 1 || second.MediaVersion != 1 {
		t.Fatalf("resume persisted versions = [%d %d %d], want [3 1 1]", second.WireVersion, second.E2EEVersion, second.MediaVersion)
	}
	if bytes.Equal(first.TokenHash, second.TokenHash) {
		t.Fatal("replacement bearer reused the original token hash")
	}
}

func TestTheInstanceDocumentAdvertisesWireFourThreeAndTwo(t *testing.T) {
	_, ts, _ := newGreetedServer(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/v1/instance", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/instance: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/instance = %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the instance document: %v", err)
	}
	var doc []any
	if err := cborx.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode the instance document: %v", err)
	}
	if len(doc) != 11 {
		t.Fatalf("the instance document has %d elements, want 11", len(doc))
	}
	if got := uints(t, doc[0], "wire_versions"); !slices.Equal(got, []uint64{5, 4, 3, 2}) {
		t.Fatalf("wire_versions = %v, want [5 4 3 2], the hello's list", got)
	}
}
