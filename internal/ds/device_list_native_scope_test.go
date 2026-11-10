package ds_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// signedDeviceListUser stores the user of testdata/signed-device-list: a real signed device list naming one device, its
// credential and the user's SSK (cut from the retired kind-3 fixture, Bot v2 Task 23).
func signedDeviceListUser(t *testing.T, h *dsHarness) id.ID {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join("dstest", "testdata", "signed-device-list")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		return b
	}
	var meta struct {
		User   string `json:"user_id_hex"`
		Device string `json:"device_id_hex"`
		DSK    string `json:"dsk_public_hex"`
		SSK    string `json:"ssk_public_hex"`
	}
	if err := json.Unmarshal(read("metadata.json"), &meta); err != nil {
		t.Fatal(err)
	}
	decode := func(v string) []byte {
		b, err := hex.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	user, device := id.ID(decode(meta.User)), id.ID(decode(meta.Device))
	credential, list := read("credential.cbor"), read("device_list.cbor")
	var identity struct {
		_          struct{} `cbor:",toarray"`
		Version    uint64
		UMKPub     []byte
		UserID     []byte
		DeviceID   []byte
		Kind       uint64
		Tier       uint64
		SignerTier uint64
		SSKPub     []byte
		SigUMKSSK  []byte
		SigSSKDev  []byte
	}
	if err := cborx.Unmarshal(credential, &identity); err != nil {
		t.Fatal(err)
	}
	now := h.clk.Now().Unix()
	verified := now
	if err := h.repo.CreateUser(ctx, store.UserRow{ID: user, Username: "signed" + user.String()[:8], Display: "Signed", UMKPub: identity.UMKPub,
		SSKPub: decode(meta.SSK), SigUMKSSK: identity.SigUMKSSK, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: user, DSKPub: decode(meta.DSK), CredentialBlob: credential, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PutDeviceList(ctx, store.DeviceListRow{UserID: user, Version: 1, Blob: list, SSKSignature: list[len(list)-ed25519.SignatureSize:], PrevHash: make([]byte, 32), Created: now}); err != nil {
		t.Fatal(err)
	}
	return user
}

func TestNativeDeviceListScopePreservesSignaturesLifetimeRuntimeAndCancellation(t *testing.T) {
	h := newDSHarness(t)
	ctx := t.Context()
	wasm, err := mlswasi.New(ctx, loadWasmForDS(t), mlswasi.Options{PoolSize: 1, CacheDir: sharedWasmCacheDir(t), Now: h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	lists := ds.NewDeviceLists(h.repo, wasm)
	other := ds.NewDeviceLists(h.repo, h.wasm)
	user := signedDeviceListUser(t, h)
	row, err := h.repo.GetUser(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	list, err := h.repo.GetDeviceList(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := lists.WithVerification(ctx, func(scoped context.Context) error {
		escaped = scoped
		if _, err := other.Verify(scoped, list.Blob, row.SSKPub, user); !errors.Is(err, ds.ErrDeviceListUnavailable) {
			return errors.New("mismatched native runtime was accepted")
		}
		if err := lists.WithVerification(scoped, func(nested context.Context) error {
			entries, err := lists.Verify(nested, list.Blob, row.SSKPub, user)
			if err != nil {
				return err
			}
			if len(entries) != 1 {
				return errors.New("signed list entries changed")
			}
			return nil
		}); err != nil {
			return err
		}
		var wg sync.WaitGroup
		results := make(chan error, 4)
		for range 4 {
			wg.Go(func() { _, err := lists.Verify(scoped, list.Blob, row.SSKPub, user); results <- err })
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				return err
			}
		}
		tampered := append([]byte(nil), list.Blob...)
		tampered[len(tampered)-1] ^= 1
		if _, err := lists.Verify(scoped, tampered, row.SSKPub, user); err == nil {
			return errors.New("borrowed verifier accepted a changed signature")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lists.Verify(context.WithoutCancel(escaped), list.Blob, row.SSKPub, user); !errors.Is(err, ds.ErrDeviceListUnavailable) {
		t.Fatalf("escaped native scope did not fail closed: %v", err)
	}
	called := false
	if err := lists.WithVerification(escaped, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatalf("expired scope callback=%v err=%v", called, err)
	}
	owner, cancelOwner := context.WithCancel(ctx)
	err = lists.WithVerification(owner, func(scoped context.Context) error {
		cancelOwner()
		_, err := lists.Verify(context.WithoutCancel(scoped), list.Blob, row.SSKPub, user)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation was detached: %v", err)
	}
	lease, err := wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	called = false
	err = lists.WithVerification(cancelled, func(context.Context) error { called = true; return nil })
	lease.Release()
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled acquire callback=%v err=%v", called, err)
	}
	final, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := lists.WithVerification(final, func(scoped context.Context) error {
		_, err := lists.Verify(scoped, list.Blob, row.SSKPub, user)
		return err
	}); err != nil {
		t.Fatalf("lease was leaked after cancellation: %v", err)
	}
}
