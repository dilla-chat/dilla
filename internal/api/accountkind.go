package api

// accountkind.go is the instance-signed account-kind attestation (protocol/03 § Account kind
// attestation, Task S1 part 2). A leaf credential's `kind` is signed by its own user's SSK, so it
// is the user's claim; the attestation is the instance's statement of the account's kind, signed
// with the instance's current external-sender key, which a client pins (protocol/03 § Instance
// keys) and checks every leaf's credential kind against.
//
//	message     = "dilla account kind v1" || CBOR([1, instance_id(16), user_id(16), kind])
//	attestation = CBOR([1, key_id(16), kind, sig(64)])
//
// Ed25519 signatures are deterministic, so the attestation of an account is the same bytes on
// every request: it is signed when served rather than stored, and every account the instance holds,
// one created before this change included, has one without a backfill pass.

import (
	"crypto/ed25519"
	"errors"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// AccountKindDomain is the attestation's signature domain.
const AccountKindDomain = "dilla account kind v1"

// accountKindWire is the first wire version whose GET /v1/users/{id}/device-list head carries the
// attestation as its fifth element (protocol/07, wire 4).
const accountKindWire = 4

// AccountKindMessage is the signed message of an account-kind attestation. It binds the instance,
// the account and its kind, not a key: an account's kind never changes, so the statement holds for
// every key the account ever presents (Ruling AW).
func AccountKindMessage(instanceID, userID id.ID, kind uint8) ([]byte, error) {
	if kind > 1 {
		return nil, errors.New("api: account-kind attestation fields out of range")
	}
	body, err := cborx.Marshal([]any{uint64(1), instanceID[:], userID[:], uint64(kind)})
	if err != nil {
		return nil, err
	}
	return append([]byte(AccountKindDomain), body...), nil
}

// AccountKindAttestation signs the account's kind with sign (the instance's current external-sender
// key, named by keyID) and encodes the attestation.
func AccountKindAttestation(sign func([]byte) ([]byte, error), instanceID, keyID id.ID, user store.UserRow) ([]byte, error) {
	if sign == nil {
		return nil, errors.New("api: account-kind signer is not wired")
	}
	message, err := AccountKindMessage(instanceID, user.ID, user.Kind)
	if err != nil {
		return nil, err
	}
	sig, err := sign(message)
	if err != nil {
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, errors.New("api: account-kind signature has the wrong length")
	}
	return cborx.Marshal([]any{uint64(1), keyID[:], uint64(user.Kind), sig})
}
