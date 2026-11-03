package mlswasi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

func testNativeBotCredential(t *testing.T) (credential, dskPub []byte) {
	t.Helper()
	seed := func(label string) []byte {
		seed := sha256.Sum256([]byte(label))
		return seed[:]
	}
	umk := ed25519.NewKeyFromSeed(seed("credential test umk"))
	ssk := ed25519.NewKeyFromSeed(seed("credential test ssk"))
	dsk := ed25519.NewKeyFromSeed(seed("credential test dsk"))
	umkPub := umk.Public().(ed25519.PublicKey)
	sskPub := ssk.Public().(ed25519.PublicKey)
	dskPub = dsk.Public().(ed25519.PublicKey)
	userID := make([]byte, 16) // initial-signup placeholder; the crypto layer allows it.
	deviceID := []byte("credential-dev01")
	sigUMKSSK := ed25519.Sign(umk, append([]byte("dilla ssk v1"), sskPub...))
	dskMessage := append([]byte("dilla dsk v1"), deviceID...)
	dskMessage = append(dskMessage, dskPub...)
	dskMessage = append(dskMessage, 1, 0, 0) // bot, native, native signer
	sigSSKDev := ed25519.Sign(ssk, dskMessage)
	credential, err := cborx.Marshal([]any{
		uint64(1), umkPub, userID, deviceID, uint64(1), uint64(0), uint64(0), sskPub,
		sigUMKSSK, sigSSKDev,
	})
	if err != nil {
		t.Fatalf("encode test credential: %v", err)
	}
	return credential, dskPub
}

func TestVerifyCredentialAcceptsTheExistingHumanIdentityVector(t *testing.T) {
	var vector struct {
		Credential struct {
			DSKPub string `json:"dsk_pub"`
			CBOR   string `json:"cbor"`
		} `json:"credential_identity"`
	}
	doc, err := os.ReadFile("../../protocol/vectors/identity.json")
	if err != nil {
		t.Fatalf("read identity vector: %v", err)
	}
	if err := json.Unmarshal(doc, &vector); err != nil {
		t.Fatalf("decode identity vector: %v", err)
	}
	credential, err := hex.DecodeString(vector.Credential.CBOR)
	if err != nil {
		t.Fatalf("decode credential vector: %v", err)
	}
	dsk, err := hex.DecodeString(vector.Credential.DSKPub)
	if err != nil {
		t.Fatalf("decode DSK vector: %v", err)
	}
	r := newTestRuntime(t, Options{PoolSize: 1})
	inst, err := r.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()
	info, err := inst.VerifyCredential(context.Background(), credential, dsk)
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if info.Kind != 0 || info.Tier != 1 || info.SignerTier != 0 {
		t.Fatalf("vector enum fields = %d/%d/%d, want 0/1/0", info.Kind, info.Tier, info.SignerTier)
	}
}

func TestVerifyCredentialUsesTheCompiledCoreForNativeBot(t *testing.T) {
	r := newTestRuntime(t, Options{PoolSize: 1})
	inst, err := r.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()
	credential, dsk := testNativeBotCredential(t)
	info, err := inst.VerifyCredential(context.Background(), credential, dsk)
	if err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if info.Kind != 1 || info.Tier != 0 || info.SignerTier != 0 {
		t.Fatalf("credential enum fields = %d/%d/%d, want 1/0/0", info.Kind, info.Tier, info.SignerTier)
	}
	if info.DeviceID[0] != 'c' || !info.UserID.IsZero() {
		t.Fatal("credential public IDs were not returned as encoded")
	}
}

func TestVerifyCredentialRejectsWrongDSKAndMalformedCredential(t *testing.T) {
	r := newTestRuntime(t, Options{PoolSize: 1})
	inst, err := r.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()
	credential, dsk := testNativeBotCredential(t)
	wrong := append([]byte(nil), dsk...)
	wrong[0] ^= 0xff
	if _, err := inst.VerifyCredential(context.Background(), credential, wrong); err == nil {
		t.Fatal("wrong DSK was accepted")
	}
	if _, err := inst.VerifyCredential(context.Background(), []byte{0xff}, dsk); err == nil {
		t.Fatal("malformed credential was accepted")
	}
	if _, err := inst.VerifyCredential(context.Background(), credential, dsk[:31]); err == nil {
		t.Fatal("a DSK with the wrong length was accepted")
	}
	if _, err := inst.VerifyCredential(context.Background(), make([]byte, maxCredentialBytes+1), dsk); err == nil {
		t.Fatal("an oversized credential was accepted")
	}
}
