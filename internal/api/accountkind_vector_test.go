package api_test

// The account-kind attestation the server signs is byte for byte the one protocol/vectors/identity.json
// commits (account_kind), which the TypeScript generator writes and the Rust core checks: one
// structure, three implementations.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func TestTheAccountKindAttestationMatchesTheCommittedVector(t *testing.T) {
	raw, err := os.ReadFile("../../protocol/vectors/identity.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AccountKind struct {
			InstancePriv string `json:"instance_priv"`
			InstancePub  string `json:"instance_pub"`
			KeyID        string `json:"key_id"`
			InstanceID   string `json:"instance_id"`
			UserID       string `json:"user_id"`
			Cases        []struct {
				Kind        uint8  `json:"kind"`
				Message     string `json:"message"`
				Attestation string `json:"attestation"`
			} `json:"cases"`
		} `json:"account_kind"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ak := doc.AccountKind
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	asID := func(s string) id.ID {
		var out id.ID
		copy(out[:], unhex(s))
		return out
	}
	key := ed25519.NewKeyFromSeed(unhex(ak.InstancePriv))
	if hex.EncodeToString(key.Public().(ed25519.PublicKey)) != ak.InstancePub {
		t.Fatal("the vector's instance key pair disagrees")
	}
	if len(ak.Cases) != 2 {
		t.Fatalf("%d cases, want a human and a Bot", len(ak.Cases))
	}
	for _, c := range ak.Cases {
		user := store.UserRow{ID: asID(ak.UserID), Kind: c.Kind}
		message, err := api.AccountKindMessage(asID(ak.InstanceID), user.ID, c.Kind)
		if err != nil || hex.EncodeToString(message) != c.Message {
			t.Fatalf("kind %d: message %x, %v; want the vector's", c.Kind, message, err)
		}
		got, err := api.AccountKindAttestation(func(m []byte) ([]byte, error) { return ed25519.Sign(key, m), nil },
			asID(ak.InstanceID), asID(ak.KeyID), user)
		if err != nil || hex.EncodeToString(got) != c.Attestation {
			t.Fatalf("kind %d: attestation %x, %v; want the vector's", c.Kind, got, err)
		}
	}
}
