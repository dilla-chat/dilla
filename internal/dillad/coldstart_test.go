package dillad_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// coldstart_test.go is gap G5's runtime proof (web-1 task 10, C12). A group in which nobody commits
// must keep delivering message.ct after the instance restarts: a second dillad.New over the same
// database is a new process — new pools, a new gateway with an empty registry, a new delivery
// service — and nothing but the member table can tell it who is in the group.

// coldMember is one device of the fixture: its account, its device and the session token
// auth.Sessions minted for it, which survives the restart because sessions are rows.
type coldMember struct {
	user, device id.ID
	token        string
}

func TestARestartedInstanceDeliversAQuietGroupsMessages(t *testing.T) {
	cfg := testConfig(t)
	cfg.Log.Level = "warn"
	ctx := context.Background()

	first := startColdServer(t, cfg)
	groupID := id.New()
	members := seedQuietGroup(t, first, groupID)

	// Before the restart the group is what Register leaves behind: its rows in SQL and its member
	// list on the gateway (internal/ds/registry.go:159-161). This half is the control: the fixture
	// delivers while the registry is warm, so a failure after the restart is the registry's.
	first.Gateway().SetGroupMembers(groupID, []id.ID{members[0].device, members[1].device})
	first.Gateway().SetGroupLeaves(groupID, map[id.ID]uint32{members[0].device: 0, members[1].device: 1})
	ts1 := httptest.NewServer(first.Handler())
	t.Cleanup(ts1.Close)
	listener1, _ := coldDial(t, ts1, members[0].token)
	seq1 := coldUpload(t, socketHandler(first), groupID, members[1].token)
	expectMessageCT(t, listener1, "before the restart", seq1, members[1].device)

	// The restart: the first process stops (its gateway, its delivery service, its pools), and a
	// second one starts over the same database file.
	_ = listener1.CloseNow()
	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown the first process: %v", err)
	}
	second := startColdServer(t, cfg)
	ts2 := httptest.NewServer(second.Handler())
	t.Cleanup(ts2.Close)

	listener2, ready := coldDial(t, ts2, members[0].token)
	if !readyListsGroup(ready, groupID) {
		t.Fatalf("ready after the restart does not list group %s: the member table did not survive, "+
			"so this test cannot be about the registry", groupID)
	}
	seq2 := coldUpload(t, socketHandler(second), groupID, members[1].token)
	if seq2 != seq1+1 {
		t.Fatalf("the upload after the restart took seq %d, want %d", seq2, seq1+1)
	}
	expectMessageCT(t, listener2, "after the restart", seq2, members[1].device)
}

func startColdServer(t *testing.T, cfg *config.Config) *dillad.Server {
	t.Helper()
	s, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

// seedQuietGroup writes what a registration of a two-member text group leaves in SQL — two
// accounts with one device and one session each, the group row at epoch 1 and both leaves — in
// one transaction. No MLS state blob is written: nothing on the upload or delivery path reads it
// (internal/ds/message.go:31-123 reads the group row, the member table and the message header).
func seedQuietGroup(t *testing.T, s *dillad.Server, groupID id.ID) [2]coldMember {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	var out [2]coldMember
	err := s.Repo().Tx(ctx, func(tx store.Repository) error {
		for i := range out {
			m := coldMember{user: id.New(), device: id.New()}
			if err := tx.CreateUser(ctx, store.UserRow{
				ID: m.user, Username: fmt.Sprintf("cold%d", i), Display: "cold",
				UMKPub: bytes.Repeat([]byte{1}, 32), SSKPub: bytes.Repeat([]byte{2}, 32),
				SigUMKSSK: bytes.Repeat([]byte{3}, 64), Created: now,
			}); err != nil {
				return err
			}
			// Each account holds a confirmed TOTP secret: the restart's second-factor reconcile
			// (web3.6 S1) marks an account without one as owing it, and an owed session is refused
			// the gateway and the upload this test makes.
			confirmed := now
			if err := tx.PutTOTP(ctx, store.TOTPRow{
				UserID: m.user, Secret: bytes.Repeat([]byte{5}, 20), Digits: 6, Period: 30,
				Algorithm: "SHA1", ConfirmedAt: &confirmed, Created: now,
			}); err != nil {
				return err
			}
			if err := tx.CreateDevice(ctx, store.DeviceRow{
				ID: m.device, UserID: m.user, DSKPub: bytes.Repeat([]byte{4}, 32),
				CredentialBlob: []byte{0x01}, LastSeen: now, Created: now,
			}); err != nil {
				return err
			}
			token, err := s.Sessions().NewDeviceSession(ctx, tx, m.user, m.device, 0)
			if err != nil {
				return err
			}
			m.token = token.Token
			out[i] = m
		}
		if err := tx.CreateGroup(ctx, store.GroupRow{
			GroupID: groupID, Binding: []byte{0x80}, Kind: 0, TargetID: id.New(), Ciphersuite: 1,
			Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 0,
			PolicyVersion: 1, Created: now,
		}); err != nil {
			return err
		}
		return tx.ReplaceMembers(ctx, groupID, 1, []store.MemberRow{
			{GroupID: groupID, LeafIndex: 0, UserID: out[0].user, DeviceID: out[0].device,
				SignatureKey: bytes.Repeat([]byte{6}, 32), AddedEpoch: 1},
			{GroupID: groupID, LeafIndex: 1, UserID: out[1].user, DeviceID: out[1].device,
				SignatureKey: bytes.Repeat([]byte{7}, 32), AddedEpoch: 1},
		})
	})
	if err != nil {
		t.Fatalf("seed the quiet group: %v", err)
	}
	return out
}

// coldDial opens /gateway with the Authorization header and returns the connection and the
// ready payload.
func coldDial(t *testing.T, ts *httptest.Server, token string) (*websocket.Conn, []any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if op, _ := readFrame(t, ctx, c); op != 0 {
		t.Fatalf("first frame op %d, want hello (0)", op)
	}
	identify(t, ctx, c)
	op, ready := readFrame(t, ctx, c)
	if op != 3 {
		t.Fatalf("op %d after identify, want ready (3)", op)
	}
	return c, ready
}

// coldUpload is POST /v1/groups/{id}/message with [epoch, private_message, class 0] and returns seq.
func coldUpload(t *testing.T, h http.Handler, groupID id.ID, token string) uint64 {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/v1/groups/"+groupID.String()+"/message", token,
		[]any{uint64(1), coldMessage(1), uint64(0)})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/groups/%s/message = %d: %s", groupID, rec.Code, rec.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 3 {
		t.Fatalf("upload response %x: %v", rec.Body.Bytes(), err)
	}
	seq, ok := out[0].(uint64)
	if !ok || seq == 0 {
		t.Fatalf("upload response seq is %#v", out[0])
	}
	return seq
}

// coldMessage frames an RFC 9420 PrivateMessage by hand, as internal/ds/message_harness_test.go's
// privateMessageOfContentType does: protocol 1, wire format 2, an 8-byte group id, the epoch,
// content_type 1 (application), a 32-byte authenticated_data (the franking commitment), 16 bytes of
// sender data and 48 of ciphertext. Every length is below 64, so each varint header is one byte.
// The instance parses the header and never decrypts.
func coldMessage(epoch uint64) []byte {
	out := []byte{0x00, 0x01, 0x00, 0x02, 8}
	out = append(out, "group-id"...)
	out = binary.BigEndian.AppendUint64(out, epoch)
	out = append(out, 1, 32)
	out = append(out, bytes.Repeat([]byte{5}, 32)...)
	out = append(out, 16)
	out = append(out, bytes.Repeat([]byte{7}, 16)...)
	out = append(out, 48)
	return append(out, bytes.Repeat([]byte{9}, 48)...)
}

// expectMessageCT reads frames until message.ct (op 19) and checks its seq, epoch and uploader.
// 30 seconds is the bound for a frame the instance writes in the same request that answered the
// upload; the wait is for a state (the frame), not a duration.
func expectMessageCT(t *testing.T, c *websocket.Conn, phase string, seq uint64, uploader id.ID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("%s: no message.ct reached the listening member within 30 s (%v): the gateway's "+
				"fan-out list does not hold the group", phase, err)
		}
		if typ != websocket.MessageBinary {
			continue
		}
		var frame []any
		if err := cborx.Unmarshal(b, &frame); err != nil || len(frame) != 4 {
			t.Fatalf("%s: undecodable frame %x: %v", phase, b, err)
		}
		if op, _ := frame[0].(uint64); op != 19 {
			continue
		}
		payload, _ := frame[3].([]any)
		// Seven since web3.7 SB1: the 7th is the upload class (protocol/02 § Upload class).
		if len(payload) != 7 {
			t.Fatalf("%s: message.ct payload has %d elements, want 7", phase, len(payload))
		}
		if got, _ := payload[0].(uint64); got != seq {
			t.Fatalf("%s: message.ct seq %d, want %d", phase, got, seq)
		}
		if got, _ := payload[1].(uint64); got != 1 {
			t.Fatalf("%s: message.ct epoch %d, want 1", phase, got)
		}
		if got, _ := payload[2].([]byte); !bytes.Equal(got, uploader[:]) {
			t.Fatalf("%s: message.ct uploader %x, want %x", phase, got, uploader[:])
		}
		return
	}
}

// readyListsGroup reports whether ready's groups element (index 8) names groupID.
func readyListsGroup(ready []any, groupID id.ID) bool {
	if len(ready) != 10 {
		return false
	}
	groups, _ := ready[8].([]any)
	for _, g := range groups {
		row, _ := g.([]any)
		if len(row) != 4 {
			continue
		}
		if gid, _ := row[0].([]byte); bytes.Equal(gid, groupID[:]) {
			return true
		}
	}
	return false
}
