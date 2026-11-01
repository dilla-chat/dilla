package ds_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/capability"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

func TestCurrentBotInteractionContextRequiresAcceptedReceiptAndExactLiveRoster(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := context.Background()
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request); err != nil {
		t.Fatalf("register kind-3 group: %v", err)
	}

	got, err := fixture.h.ds.CurrentBotInteractionGroupContext(ctx, fixture.request.Registration.GroupID)
	if err != nil {
		t.Fatalf("load current context: %v", err)
	}
	borrowedAfterReturn := false
	if err := got.WithDeviceListVerification(ctx, func(context.Context) error { borrowedAfterReturn = true; return nil }); err == nil || borrowedAfterReturn {
		t.Fatalf("returned proof extended native callback lifetime: called=%v err=%v", borrowedAfterReturn, err)
	}
	if got.Registration.GroupID != fixture.request.Registration.GroupID {
		t.Fatalf("registration receipt for group %s = %s", fixture.request.Registration.GroupID, got.Registration.GroupID)
	}
	if got.LeafCount != 2 || len(got.Roster) != 2 || got.Epoch != 1 || got.Registration.Epoch != 1 {
		t.Fatalf("current authenticated group=%+v", got)
	}
	expected := fixture.request.TrustedPreflight.Expected
	if got.Binding.InstallationID != fixture.request.InstallationID || got.Binding.CommunityID != expected.CommunityID || got.Binding.ParticipantUserID != expected.ParticipantUserID || got.Binding.ParticipantDeviceID != expected.ParticipantDeviceID {
		t.Fatalf("binding does not match accepted registration: %+v", got.Binding)
	}

	// A database-only roster shortcut would bless inconsistent member metadata even though
	// current PublicGroup state still has two authenticated leaves. The DS refuses the mismatch.
	if err := fixture.h.repo.Tx(ctx, func(tx store.Repository) error {
		members, err := tx.ListMembers(ctx, fixture.request.Registration.GroupID)
		if err != nil {
			return err
		}
		return tx.ReplaceMembers(ctx, fixture.request.Registration.GroupID, got.Epoch, members[:1])
	}); err != nil {
		t.Fatalf("corrupt member projection for negative case: %v", err)
	}
	if _, err := fixture.h.ds.CurrentBotInteractionGroupContext(ctx, fixture.request.Registration.GroupID); err == nil {
		t.Fatal("current context accepted a roster projection that differs from authenticated MLS state")
	}
	called := false
	if err := fixture.h.ds.WithCurrentBotInteractionGroupRead(ctx, fixture.request.Registration.GroupID, func(ds.CurrentBotInteractionGroupContext, store.Repository) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("atomic read accepted changed roster: err=%v callback called=%v", err, called)
	}

	// The still-present group row and MLS state must not substitute for proof that this was ever
	// accepted by kind-3 flow if the immutable registration record is absent.
	if err := fixture.h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.(store.BotGroupRegistrationReceipts).DeleteBotGroupRegistrationReceiptsByInstallation(ctx, fixture.request.InstallationID)
	}); err != nil {
		t.Fatalf("delete accepted receipt: %v", err)
	}
	if _, err := fixture.h.ds.CurrentBotInteractionGroupContext(ctx, fixture.request.Registration.GroupID); err == nil {
		t.Fatal("current context accepted a group with no immutable registration receipt")
	}
}

func TestCurrentBotInteractionContextCallbackHoldsGroupFenceThroughSigning(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := context.Background()
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request); err != nil {
		t.Fatalf("register kind-3 group: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- fixture.h.ds.WithCurrentBotInteractionGroupContext(ctx, fixture.request.Registration.GroupID, func(current ds.CurrentBotInteractionGroupContext) error {
			if current.Epoch != 1 {
				return fmt.Errorf("context epoch = %d, want 1", current.Epoch)
			}
			close(entered)
			<-release // stands in for encoding and signing the proof
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("context callback did not start")
	}

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- fixture.h.ds.Close(ctx, fixture.request.Registration.GroupID)
	}()
	<-closeStarted
	select {
	case err := <-closeDone:
		t.Fatalf("close passed the group fence while proof was being signed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-callbackDone; err != nil {
		t.Fatalf("current context callback: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close after signing: %v", err)
	}
}

func TestCurrentBotInteractionGroupReadFailsClosedAndFencesTransactionReads(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := context.Background()
	groupID := fixture.request.Registration.GroupID
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request); err != nil {
		t.Fatalf("register kind-3 group: %v", err)
	}
	// Registration uses multiple guest operations; after it completes, exercise the read
	// primitive with an isolated one-instance WASI pool to prove its callback can safely borrow it.
	wasm, err := mlswasi.New(ctx, loadWasmForDS(t), mlswasi.Options{PoolSize: 1, CacheDir: sharedWasmCacheDir(t), Now: fixture.h.clk.Now})
	if err != nil {
		t.Fatalf("create one-instance WASI runtime: %v", err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	fixture.h.wasm = wasm
	botCapability := capability.New(fixture.h.repo)
	fixture.h.ds, err = ds.New(ds.Options{Store: fixture.h.repo, BotCapability: botCapability, Wasm: wasm, Gateway: fixture.h.gw,
		Clock: fixture.h.clk, Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), Channels: fixture.h.channels,
		ACL: fixture.h.acl, PostPolicy: fixture.h.postPolicy})
	if err != nil {
		t.Fatalf("rebuild DS with one-instance WASI runtime: %v", err)
	}

	called := false
	if err := fixture.h.ds.WithCurrentBotInteractionGroupRead(ctx, id.New(), func(ds.CurrentBotInteractionGroupContext, store.Repository) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("foreign group read: err=%v callback called=%v", err, called)
	}

	// A callback reads the group row and holds the same database transaction and local fence.
	// Closing the group cannot complete until the read has finished.
	entered := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- fixture.h.ds.WithCurrentBotInteractionGroupRead(ctx, groupID, func(current ds.CurrentBotInteractionGroupContext, tx store.Repository) error {
			if current.Epoch != 1 {
				return fmt.Errorf("context epoch = %d, want 1", current.Epoch)
			}
			row, err := tx.GetGroup(ctx, groupID)
			if err != nil {
				return err
			}
			if row.ClosedAt != nil || row.Epoch != current.Epoch {
				return fmt.Errorf("transaction group row differs from validated context")
			}
			// Signed-list checks borrow the guest reserved before this transaction;
			// reacquiring the single slot here would deadlock behind our own lease.
			if err := current.WithDeviceListVerification(ctx, func(scoped context.Context) error {
				user, err := tx.GetUser(scoped, current.Binding.ParticipantUserID)
				if err != nil {
					return err
				}
				list, err := tx.GetDeviceList(scoped, user.ID)
				if err != nil {
					return err
				}
				_, err = ds.NewDeviceLists(fixture.h.repo, wasm).Verify(scoped, list.Blob, user.SSKPub, user.ID)
				return err
			}); err != nil {
				return err
			}

			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		select {
		case err := <-readDone:
			t.Fatalf("atomic read ended before callback: %v", err)
		default:
			t.Fatal("atomic read callback did not start")
		}
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- fixture.h.ds.Close(ctx, groupID) }()
	select {
	case err := <-closeDone:
		t.Fatalf("close passed active atomic read: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatalf("atomic read callback: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close after read: %v", err)
	}
	called = false
	if err := fixture.h.ds.WithCurrentBotInteractionGroupRead(ctx, groupID, func(ds.CurrentBotInteractionGroupContext, store.Repository) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("closed group read: err=%v callback called=%v", err, called)
	}
}
