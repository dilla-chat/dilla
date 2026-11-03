// Package dstest is the delivery service's test-only seeding entry: it writes a group as a
// registration would leave it, from any tree, without the registration route's checks.
//
// POST /v1/groups adopts exactly one leaf, the registering device's own (ds.Register, hardening G),
// and that is the only production path that creates a group. Tests and benchmarks still need
// groups that already hold many members - the committed 1,500-leaf fixture
// (testkit/fixtures/ds-1500) is a group at epoch 6 that no registration can produce - so they seed
// them here, through the store and the public group, the way `dillad restore` hands a delivery
// service a database it did not write. Nothing in a production build imports this package; it is
// imported only from _test.go files.
package dstest

import (
	"context"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// Group is one group to seed: the same four values a registration uploads.
type Group struct {
	GroupID     id.ID
	Binding     []byte
	GroupInfo   []byte
	RatchetTree []byte
}

// SeedGroup writes g as ds.Register leaves a group behind - the group row, the public group's
// state blob with the GroupInfo, and one mls_members row per leaf - in one transaction, and then
// republishes the gateway's member lists from SQL (ds.DS.SeedGateway), as a restarted instance
// does. The delivery service imports the state blob on the group's first use. None of Register's
// checks run: not the binding against the instance, not the channel mode, not the registration
// ACL, not one-leaf. externalSenderKeyID is the instance's (ds.Keys.ExternalSenderKeyID); created
// is the group row's creation time.
func SeedGroup(ctx context.Context, d *ds.DS, repo store.Repository, wasm *mlswasi.Runtime,
	externalSenderKeyID id.ID, created int64, g Group,
) error {
	inst, err := wasm.Acquire(ctx)
	if err != nil {
		return err
	}
	defer inst.Release()
	group, err := inst.PublicGroupFromExternal(ctx, g.RatchetTree, g.GroupInfo)
	if err != nil {
		return fmt.Errorf("dstest: the tree and GroupInfo do not build a public group: %w", err)
	}
	defer func() { _ = group.Close(ctx) }()
	state, err := group.State(ctx)
	if err != nil {
		return err
	}
	if string(state.GroupID) != string(g.GroupID[:]) {
		return fmt.Errorf("dstest: the GroupInfo is of group %x, not %s", state.GroupID, g.GroupID)
	}
	var b ds.Binding
	if err := cborx.Unmarshal(g.Binding, &b); err != nil {
		return fmt.Errorf("dstest: the binding: %w", err)
	}
	blob, err := group.ExportState(ctx)
	if err != nil {
		return err
	}
	var callID *id.ID
	if b.Kind == 1 {
		target := b.TargetID
		callID = &target
	}
	rows := make([]store.MemberRow, 0, len(state.Members))
	for _, m := range state.Members {
		deviceID, userID, err := credentialIdentity(m.CredentialIdentity)
		if err != nil {
			return fmt.Errorf("dstest: leaf %d: %w", m.LeafIndex, err)
		}
		rows = append(rows, store.MemberRow{
			GroupID: g.GroupID, LeafIndex: m.LeafIndex, UserID: userID, DeviceID: deviceID,
			SignatureKey: m.SignatureKey, AddedEpoch: state.Epoch,
		})
	}
	err = repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateGroup(ctx, store.GroupRow{
			GroupID: g.GroupID, Binding: g.Binding, Kind: b.Kind, CommunityID: b.CommunityID,
			TargetID: b.TargetID, CallID: callID, Ciphersuite: 1, Epoch: state.Epoch, Seq: 0,
			GroupInfoBlob: g.GroupInfo, TreeHash: state.TreeHash,
			ExternalSenderKeyID: externalSenderKeyID, E2EEVersion: b.E2EEVersion,
			MediaVersion: b.MediaVersion, PolicyVersion: b.PolicyVersion, Created: created,
		}); err != nil {
			return err
		}
		if err := tx.PutGroupState(ctx, g.GroupID, state.Epoch, blob, g.GroupInfo, state.TreeHash); err != nil {
			return err
		}
		return tx.ReplaceMembers(ctx, g.GroupID, state.Epoch, rows)
	})
	if err != nil {
		return err
	}
	if d != nil {
		if _, err := d.SeedGateway(ctx); err != nil {
			return err
		}
	}
	return nil
}

// credentialIdentity reads the device and the user out of dilla's ten-element CredentialIdentity
// (core/dilla-core/src/identity/credential.rs), the same positions ds reads: user_id at 2,
// device_id at 3.
func credentialIdentity(b []byte) (deviceID, userID id.ID, err error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(b, &elems); err != nil {
		return id.ID{}, id.ID{}, err
	}
	if len(elems) != 10 {
		return id.ID{}, id.ID{}, fmt.Errorf("credential identity has %d elements, want 10", len(elems))
	}
	if err := cborx.Unmarshal(elems[2], &userID); err != nil {
		return id.ID{}, id.ID{}, err
	}
	if err := cborx.Unmarshal(elems[3], &deviceID); err != nil {
		return id.ID{}, id.ID{}, err
	}
	return deviceID, userID, nil
}
