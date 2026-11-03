// Package identity verifies public credentials at the bot enrollment boundary.
package identity

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

var (
	ErrUnavailable = errors.New("bot identity: verifier unavailable")
	ErrInvalid     = errors.New("bot identity: credential is invalid")
)

// ExpectedNativeBot is the installation's independently provisioned identity. ExpectPlaceholder
// must be true only while signup has not assigned the bot's final user ID.
type ExpectedNativeBot struct {
	UMKPub            [32]byte
	SSKPub            [32]byte
	SigUMKSSK         [64]byte
	DeviceID          id.ID
	UserID            id.ID
	ExpectPlaceholder bool
}

// Verifier uses the shared WASI runtime. Each operation acquires one pooled instance and returns it
// immediately after the stateless credential check.
type Verifier struct {
	wasm *mlswasi.Runtime
}

func NewVerifier(wasm *mlswasi.Runtime) *Verifier { return &Verifier{wasm: wasm} }

// Verify performs only the core's cryptographic and canonical-CBOR checks. Callers that need bot
// enrollment restrictions must use VerifyNativeBot and supply the installation's expected fields.
func (v *Verifier) Verify(ctx context.Context, raw, dsk []byte) (mlswasi.CredentialInfo, error) {
	if v == nil || v.wasm == nil {
		return mlswasi.CredentialInfo{}, ErrUnavailable
	}
	inst, err := v.wasm.Acquire(ctx)
	if err != nil {
		return mlswasi.CredentialInfo{}, ErrInvalid
	}
	defer inst.Release()
	info, err := inst.VerifyCredential(ctx, raw, dsk)
	if err != nil {
		return mlswasi.CredentialInfo{}, ErrInvalid
	}
	return info, nil
}

// VerifyNativeBot requires a native bot credential whose public fields exactly match the
// installation's independently stored identity and signup context.
func (v *Verifier) VerifyNativeBot(ctx context.Context, raw, dsk []byte, expected ExpectedNativeBot) (mlswasi.CredentialInfo, error) {
	if expected.DeviceID.IsZero() || expected.ExpectPlaceholder != expected.UserID.IsZero() {
		return mlswasi.CredentialInfo{}, ErrInvalid
	}
	info, err := v.Verify(ctx, raw, dsk)
	if err != nil {
		return mlswasi.CredentialInfo{}, err
	}
	if info.Kind != 1 || info.Tier != 0 || info.SignerTier != 0 ||
		info.UMKPub != expected.UMKPub || info.SSKPub != expected.SSKPub ||
		info.SigUMKSSK != expected.SigUMKSSK || info.DeviceID != expected.DeviceID ||
		info.UserID != expected.UserID {
		return mlswasi.CredentialInfo{}, ErrInvalid
	}
	return info, nil
}
