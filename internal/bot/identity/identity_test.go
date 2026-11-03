package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

func runtimeForTest(t *testing.T) *mlswasi.Runtime {
	t.Helper()
	wasm, err := os.ReadFile("../../mlswasi/testdata/dilla_core_wasi.wasm")
	if err != nil {
		t.Fatalf("read compiled WASI core: %v", err)
	}
	r, err := mlswasi.New(context.Background(), wasm, mlswasi.Options{PoolSize: 1})
	if err != nil {
		t.Fatalf("start WASI core: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func nativeBot(t *testing.T, tier, signerTier, kind uint64, user id.ID) ([]byte, []byte, ExpectedNativeBot) {
	t.Helper()
	seed := func(label string) []byte {
		sum := sha256.Sum256([]byte(label))
		return sum[:]
	}
	umk := ed25519.NewKeyFromSeed(seed("bot verifier umk"))
	ssk := ed25519.NewKeyFromSeed(seed("bot verifier ssk"))
	dsk := ed25519.NewKeyFromSeed(seed("bot verifier dsk"))
	umkPub := umk.Public().(ed25519.PublicKey)
	sskPub := ssk.Public().(ed25519.PublicKey)
	dskPub := dsk.Public().(ed25519.PublicKey)
	var device id.ID
	copy(device[:], []byte("native-bot-device"))
	sigUMKSSK := ed25519.Sign(umk, append([]byte("dilla ssk v1"), sskPub...))
	dskMsg := append([]byte("dilla dsk v1"), device[:]...)
	dskMsg = append(dskMsg, dskPub...)
	dskMsg = append(dskMsg, byte(kind), byte(tier), byte(signerTier))
	sigSSKDev := ed25519.Sign(ssk, dskMsg)
	raw, err := cborx.Marshal([]any{
		uint64(1), umkPub, user[:], device[:], kind, tier, signerTier, sskPub, sigUMKSSK, sigSSKDev,
	})
	if err != nil {
		t.Fatalf("encode test credential: %v", err)
	}
	var expected ExpectedNativeBot
	copy(expected.UMKPub[:], umkPub)
	copy(expected.SSKPub[:], sskPub)
	copy(expected.SigUMKSSK[:], sigUMKSSK)
	expected.DeviceID, expected.UserID = device, user
	return raw, dskPub, expected
}

func TestVerifyNativeBotAcceptsOnlyExplicitPlaceholderOrAssignedUser(t *testing.T) {
	r := runtimeForTest(t)
	v := NewVerifier(r)
	ctx := context.Background()
	zero := id.ID{}

	raw, dsk, expected := nativeBot(t, 0, 0, 1, zero)
	if _, err := v.VerifyNativeBot(ctx, raw, dsk, expected); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero user without placeholder permission: got %v", err)
	}
	expected.ExpectPlaceholder = true
	if _, err := v.VerifyNativeBot(ctx, raw, dsk, expected); err != nil {
		t.Fatalf("explicit initial-signup placeholder: %v", err)
	}

	assigned := id.ID{0x71}
	raw, dsk, expected = nativeBot(t, 0, 0, 1, assigned)
	if _, err := v.VerifyNativeBot(ctx, raw, dsk, expected); err != nil {
		t.Fatalf("assigned user: %v", err)
	}
	expected.ExpectPlaceholder = true
	if _, err := v.VerifyNativeBot(ctx, raw, dsk, expected); !errors.Is(err, ErrInvalid) {
		t.Fatalf("placeholder flag with assigned user: got %v", err)
	}
}

func TestVerifyNativeBotRejectsHumanBrowserProvisionalAndMismatches(t *testing.T) {
	v := NewVerifier(runtimeForTest(t))
	ctx := context.Background()
	user := id.ID{0x72}

	for _, tc := range []struct {
		name       string
		kind       uint64
		tier       uint64
		signerTier uint64
	}{
		{name: "human", kind: 0, tier: 0, signerTier: 0},
		{name: "browser bot", kind: 1, tier: 1, signerTier: 0},
		{name: "browser signer", kind: 1, tier: 0, signerTier: 1},
		{name: "provisional", kind: 1, tier: 0, signerTier: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, dsk, expected := nativeBot(t, tc.tier, tc.signerTier, tc.kind, user)
			if _, err := v.VerifyNativeBot(ctx, raw, dsk, expected); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want sanitized invalid-credential error", err)
			}
		})
	}

	raw, dsk, expected := nativeBot(t, 0, 0, 1, user)
	wrongDSK := append([]byte(nil), dsk...)
	wrongDSK[0] ^= 0xff
	if _, err := v.VerifyNativeBot(ctx, raw, wrongDSK, expected); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong DSK: got %v", err)
	}

	mutations := []struct {
		name string
		edit func(*ExpectedNativeBot)
	}{
		{name: "umk", edit: func(e *ExpectedNativeBot) { e.UMKPub[0] ^= 1 }},
		{name: "ssk", edit: func(e *ExpectedNativeBot) { e.SSKPub[0] ^= 1 }},
		{name: "umk signature", edit: func(e *ExpectedNativeBot) { e.SigUMKSSK[0] ^= 1 }},
		{name: "device", edit: func(e *ExpectedNativeBot) { e.DeviceID[0] ^= 1 }},
		{name: "user", edit: func(e *ExpectedNativeBot) { e.UserID[0] ^= 1 }},
	}
	for _, tc := range mutations {
		t.Run("expected "+tc.name, func(t *testing.T) {
			got := expected
			tc.edit(&got)
			if _, err := v.VerifyNativeBot(ctx, raw, dsk, got); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want invalid-credential error", err)
			}
		})
	}

	badContext := expected
	badContext.DeviceID = id.ID{}
	if _, err := v.VerifyNativeBot(ctx, raw, dsk, badContext); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero expected device: got %v", err)
	}
}

func TestVerifyFailsClosedWithoutRuntimeAndSanitizesGuestErrors(t *testing.T) {
	if _, err := NewVerifier(nil).Verify(context.Background(), nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing runtime: got %v", err)
	}
	v := NewVerifier(runtimeForTest(t))
	if _, err := v.Verify(context.Background(), []byte("raw malformed credential"), make([]byte, 32)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed credential: got %v", err)
	}
}
