//go:build linux

package native

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCompiledHelperNativeLifecycleIntegration(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN and DILLA_BOT_HELPER_INTEGRATION=1 to run native integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "native")
	binding := Binding{Instance: integrationID(1), Community: integrationID(2), Installation: integrationID(3)}
	key := make([]byte, 32)
	copy(key, "synthetic-integration-key")
	ctx := context.Background()
	open := func(b Binding, k []byte) (*Client, error) {
		return Start(ctx, helper.Options{Executable: binary, StateDir: state}, b, k)
	}
	seed, err := open(binding, append([]byte(nil), key...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Bootstrap(ctx, [32]byte{0x77}); err != nil {
		t.Fatal(err)
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}

	wrongKey := append([]byte(nil), key...)
	wrongKey[0] ^= 0xff
	if c, e := open(binding, wrongKey); e == nil {
		_ = c.Close()
		t.Fatal("wrong key opened existing identity")
	}
	wrongBinding := binding
	wrongBinding.Instance = integrationID(9)
	if c, e := open(wrongBinding, append([]byte(nil), key...)); e == nil {
		_ = c.Close()
		t.Fatal("wrong instance opened existing identity")
	}

	c, err := open(binding, append([]byte(nil), key...))
	if err != nil {
		t.Fatal(err)
	}
	boot, err := c.Bootstrap(ctx, [32]byte{0x77})
	if err != nil {
		t.Fatal(err)
	}
	bootAgain, err := c.Bootstrap(ctx, [32]byte{0x77})
	if err != nil || !reflect.DeepEqual(boot, bootAgain) {
		t.Fatalf("repeat bootstrap changed: %v", err)
	}
	if _, err = c.CompleteEnrollment(ctx, integrationID(4), "synthetic bot", uint64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	emptySession, err := c.Session(ctx)
	if err != nil || emptySession.Token() != "" {
		t.Fatalf("session before first bearer should be empty: %v", err)
	}
	identity, err := c.Identity(ctx)
	if err != nil || identity.Phase != 2 || identity.Device == nil {
		t.Fatalf("identity %#v: %v", identity, err)
	}
	var nonce [32]byte
	nonce[0] = 0xa5
	nonce[31] = 0x5a
	signed, err := c.SignSession(ctx, nonce, 0)
	if err != nil {
		t.Fatal(err)
	}
	preimage := make([]byte, 0, 81)
	preimage = append(preimage, []byte("dilla session v1")...)
	preimage = append(preimage, binding.Instance[:]...)
	preimage = append(preimage, identity.Device[:]...)
	preimage = append(preimage, nonce[:]...)
	preimage = append(preimage, 0)
	if !ed25519.Verify(ed25519.PublicKey(boot.Device.DSKPublic[:]), preimage, signed.Signature[:]) {
		t.Fatal("session signature did not verify against public DSK and protocol preimage")
	}
	now := uint64(time.Now().Unix())
	token := "synthetic-local-session-do-not-log"
	if err = c.StoreSession(ctx, token, now+3600, now+1800); err != nil {
		t.Fatal(err)
	}
	stored, err := c.Session(ctx)
	if err != nil || stored.Token() != token {
		t.Fatalf("session store result: %v", err)
	}

	kp := protocol.KeyPackageIntent{OperationID: id.New(), Count: 1, ExpectedGeneration: 0}
	intent, err := c.KeyPackageIntent(ctx, kp)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := c.KeyPackageIntent(ctx, kp)
	if err != nil || !reflect.DeepEqual(intent, replayed) {
		t.Fatalf("KeyPackage intent replay differed: %v", err)
	}
	resolution := protocol.KeyPackageResolution{OperationID: kp.OperationID, PackageRefs: intent.PackageRefs, Outcome: 0, Now: now, CurrentGeneration: 0}
	if err = c.ResolveKeyPackageIntent(ctx, resolution); err != nil {
		t.Fatal(err)
	}
	if err = c.ResolveKeyPackageIntent(ctx, resolution); err != nil {
		t.Fatalf("resolution replay: %v", err)
	}

	stateKey := []byte("native-integration-state")
	value := []byte{0x81, 0x01}
	txID := id.New()
	changes := []StateChange{{Area: 1, Key: stateKey, ExpectedRevision: 0, Value: value}}
	revisions, err := c.StateTransaction(ctx, txID, changes)
	if err != nil || !reflect.DeepEqual(revisions, []uint64{1}) {
		t.Fatalf("state transaction: %v %v", revisions, err)
	}
	replayedRevisions, err := c.StateTransaction(ctx, txID, changes)
	if err != nil || !reflect.DeepEqual(revisions, replayedRevisions) {
		t.Fatalf("state receipt replay: %v %v", replayedRevisions, err)
	}
	_, err = c.StateTransaction(ctx, id.New(), changes)
	var status *helper.StatusError
	if !errors.As(err, &status) || status.Status != 4 {
		t.Fatalf("state CAS conflict = %v", err)
	}
	deleteRevision, err := c.StateTransaction(ctx, id.New(), []StateChange{{Area: 1, Key: stateKey, ExpectedRevision: 1, Value: nil}})
	if err != nil || !reflect.DeepEqual(deleteRevision, []uint64{2}) {
		t.Fatalf("state tombstone: %v %v", deleteRevision, err)
	}
	stateValue, err := c.StateGet(ctx, 1, stateKey)
	if err != nil || stateValue.Revision != 2 || !stateValue.Tombstone || stateValue.Value != nil {
		t.Fatalf("state tombstone read: %#v %v", stateValue, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := open(binding, append([]byte(nil), key...))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.Identity(ctx)
	if err != nil || !reflect.DeepEqual(identity, after) {
		t.Fatalf("identity changed across close/open: %#v %v", after, err)
	}
	storedAfter, err := reopened.Session(ctx)
	if err != nil || storedAfter.Token() != token {
		t.Fatal("encrypted session did not persist")
	}
	stateAfter, err := reopened.StateGet(ctx, 1, stateKey)
	if err != nil || !reflect.DeepEqual(stateValue, stateAfter) {
		t.Fatalf("encrypted application state did not persist: %#v %v", stateAfter, err)
	}
}

func integrationID(n byte) (v id.ID) { v[15] = n; return }
