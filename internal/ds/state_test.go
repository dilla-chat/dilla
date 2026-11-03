package ds_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// R12: the PublicGroup state blob is written in the SAME transaction as the group row. A forced
// rollback must leave neither. The registration is an honest one-leaf group (hardening G), so the
// transaction is reached and its injected failure is the refusal.
func TestAFailedTransactionLeavesNeitherTheGroupRowNorTheStateBlob(t *testing.T) {
	for _, failAt := range []string{"PutGroupState", "ReplaceMembers"} {
		t.Run(failAt, func(t *testing.T) {
			h := newDSHarness(t)
			req := registration(t, "one-leaf-0", h.honestRegistrant(t))
			h.failNextTx(failAt)
			if _, err := h.ds.Register(context.Background(), req); !errors.Is(err, errInjected) {
				t.Fatalf("Register must fail when its transaction fails: got %v, want the injected failure", err)
			}
			if _, err := h.repo.GetGroup(context.Background(), req.GroupID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetGroup after a rolled-back Register: %v, want ErrNotFound", err)
			}
			members, err := h.repo.ListMembers(context.Background(), req.GroupID)
			if err != nil {
				t.Fatalf("ListMembers: %v", err)
			}
			if len(members) != 0 {
				t.Fatalf("%d member rows survived a rolled-back Register", len(members))
			}
		})
	}
}

// On start the DS imports state blobs lazily, per group, through public_group_import_state —
// never from_external, which is O(n²) in credential validation at 1,500 leaves.
func TestARestartImportsTheStateBlobLazilyAndNeverRebuildsFromTheTree(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	before := h.wasmCalls("public_group_create")
	if before == 0 {
		t.Fatal("registration never called public_group_create; the counter is not wired")
	}

	h.restartDS()
	if got := h.wasmCalls("public_group_import_state"); got != 0 {
		t.Fatalf("a restart imported %d groups eagerly, want 0 — the import is lazy", got)
	}
	// Info is pure SQL: it must not touch the guest at all.
	if _, err := h.ds.Info(context.Background(), reg.GroupID, session); err != nil {
		t.Fatalf("Info after restart: %v", err)
	}
	if got := h.wasmCalls("public_group_import_state"); got != 0 {
		t.Fatalf("Info imported %d groups; it reads SQL, which is the record", got)
	}

	if _, err := h.ds.Tree(context.Background(), reg.GroupID, session); err != nil {
		t.Fatalf("Tree after restart: %v", err)
	}
	if got := h.wasmCalls("public_group_import_state"); got != 1 {
		t.Fatalf("public_group_import_state called %d times, want 1", got)
	}
	if got := h.wasmCalls("public_group_create"); got != before {
		t.Fatalf("public_group_create was called again after a restart (%d → %d): the DS must "+
			"import its own blob, never rebuild from the tree", before, got)
	}
	// A second read of the same group is served from the cached handle.
	if _, err := h.ds.Tree(context.Background(), reg.GroupID, session); err != nil {
		t.Fatalf("second Tree: %v", err)
	}
	if got := h.wasmCalls("public_group_import_state"); got != 1 {
		t.Fatalf("a cached group was re-imported: %d imports, want 1", got)
	}
}

// The state cache reserves a group's slot BEFORE it leaves its own mutex, so a burst of first
// touches of one uncached group imports it once and holds one instance.
//
// Without the reservation, making room and acquiring an instance are two separate steps: every
// goroutine passes the room check while the cache is below capacity, every one of them calls
// Runtime.Acquire, and the pool — PoolSize instances, GOMAXPROCS in production and 2 here — is
// drained for the length of the burst while the guest does the same import N times. Acquire
// returns only an instance or a cancelled context (internal/mlswasi/pool.go:33-46), and the DS
// releases an instance only through this cache, so the same window across N DISTINCT groups is
// what turns a burst into ctx-deadline failures. The import count is the visible half of that
// invariant, and it is the half one committed fixture can express.
//
// It calls the cache without the per-group DS lock the public read paths take: that lock already
// serialises Tree against Tree, so the cache's own concurrency has to be driven directly.
func TestConcurrentFirstTouchesOfOneGroupImportItOnce(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	h.restartDS() // drops the cache; the next touch is a cold import

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
				_, _, _, err := g.Tree(ctx)
				return err
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent first touch failed: %v", err)
		}
	}
	if got := h.wasmCalls("public_group_import_state"); got != 1 {
		t.Fatalf("%d concurrent first touches imported the group %d times, want 1", n, got)
	}
	if got := h.wasmCalls("public_group_create"); got != 1 {
		t.Fatalf("public_group_create ran %d times, want the registration's one: the DS must "+
			"never rebuild a group from its tree", got)
	}
}

// The committed identity vector is the one place the ten-element layout is written down outside
// the Rust encoder, so it is what the Go decoder is held to.
func TestCredentialIdentityDecodesTheCommittedVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "protocol", "vectors", "identity.json"))
	if err != nil {
		t.Fatalf("identity.json: %v", err)
	}
	var doc struct {
		CredentialIdentity struct {
			CBOR   string `json:"cbor"`
			Fields struct {
				UserID   string `json:"user_id"`
				DeviceID string `json:"device_id"`
			} `json:"fields"`
		} `json:"credential_identity"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("identity.json: %v", err)
	}
	c := doc.CredentialIdentity
	if c.CBOR == "" {
		t.Fatal("identity.json carries no credential_identity.cbor; the decoder is unpinned")
	}
	blob, err := hex.DecodeString(c.CBOR)
	if err != nil {
		t.Fatalf("credential_identity.cbor: %v", err)
	}
	device, user, err := ds.DecodeCredentialIdentityForTest(blob)
	if err != nil {
		t.Fatalf("decodeCredentialIdentity: %v", err)
	}
	if got := hex.EncodeToString(user[:]); got != c.Fields.UserID {
		t.Errorf("user_id = %s, want %s", got, c.Fields.UserID)
	}
	if got := hex.EncodeToString(device[:]); got != c.Fields.DeviceID {
		t.Errorf("device_id = %s, want %s", got, c.Fields.DeviceID)
	}

	// A three-element array is the old, wrong shape; the length check is what rejects it.
	if _, _, err := ds.DecodeCredentialIdentityForTest([]byte{0x83, 0x01, 0x02, 0x03}); err == nil {
		t.Error("a three-element credential identity was accepted; the layout is ten elements")
	}
}

// And the binding: the registered group's own dilla_binding, read back out of the column Register
// stored it in (an honest one-leaf registration since hardening G, bound to the registration
// fixture's channel target).
func TestTheBindingDecodesAsEightFixedPositionElements(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegisterOneLeaf(t, "one-leaf-0")
	row, err := h.repo.GetGroup(context.Background(), reg.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	b, err := ds.DecodeBindingForTest(row.Binding)
	if err != nil {
		t.Fatalf("decodeBinding: %v", err)
	}
	if b.V != 1 {
		t.Errorf("v = %d, want 1", b.V)
	}
	if want := registrationFixture(t).targetID; b.TargetID != want {
		t.Errorf("target_id = %s, want %s", b.TargetID, want)
	}
	if b.CommunityID != nil {
		t.Errorf("community_id = %v, want null", b.CommunityID)
	}
	if b.Kind != 0 || b.PolicyVersion != 1 || b.E2EEVersion != 1 || b.MediaVersion != 0 {
		t.Errorf("kind/policy/e2ee/media = %d/%d/%d/%d, want 0/1/1/0",
			b.Kind, b.PolicyVersion, b.E2EEVersion, b.MediaVersion)
	}
	// A JSON decode of the same bytes must fail, which is what stops the old shape coming back.
	var scratch map[string]any
	if err := json.Unmarshal(row.Binding, &scratch); err == nil {
		t.Fatal("dilla_binding decoded as JSON; it is deterministic CBOR")
	}
	// Deviation B11: the column holds the binding's own bytes, not a re-encoding.
	if string(row.Binding) != string(registration(t, "one-leaf-0", session).Binding) {
		t.Error("the stored binding is not the bytes the group context signed")
	}
	// R9: a text group has no call id.
	if row.CallID != nil {
		t.Errorf("call_id = %v on a text group, want NULL", row.CallID)
	}
}
