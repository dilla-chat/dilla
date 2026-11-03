package mlswasi

import (
	"context"
	"errors"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/id"
)

const maxCredentialBytes = 8192
const maxCredentialResultBytes = 1024

var errCredentialInput = errors.New("mlswasi: invalid credential input")

// CredentialInfo contains the public identity fields returned by the core's strict credential
// verifier. It never contains private key material or the device signature.
type CredentialInfo struct {
	UMKPub     [32]byte
	UserID     id.ID
	DeviceID   id.ID
	Kind       uint8
	Tier       uint8
	SignerTier uint8
	SSKPub     [32]byte
	SigUMKSSK  [64]byte
}

// VerifyCredential asks dilla-core to strictly decode and verify a CredentialIdentity against
// the DSK from its MLS leaf. The guest applies the same Ed25519 verification rules as clients.
func (i *Instance) VerifyCredential(ctx context.Context, credential, dskPub []byte) (CredentialInfo, error) {
	if len(credential) == 0 || len(credential) > maxCredentialBytes || len(dskPub) != 32 {
		return CredentialInfo{}, errCredentialInput
	}
	elems, err := i.call(ctx, "verify_credential", credential, dskPub)
	if err != nil {
		return CredentialInfo{}, err
	}
	if len(elems) != 2 {
		return CredentialInfo{}, errors.New("mlswasi: verify_credential response has an invalid shape")
	}
	if len(elems[1]) > maxCredentialResultBytes {
		return CredentialInfo{}, errors.New("mlswasi: verify_credential response exceeds limit")
	}
	fields, err := rawArray(elems[1])
	if err != nil || len(fields) != 8 {
		return CredentialInfo{}, errors.New("mlswasi: verify_credential response has an invalid shape")
	}
	var out CredentialInfo
	if err = copyCredentialBytes(fields[0], out.UMKPub[:]); err != nil {
		return CredentialInfo{}, err
	}
	if err = copyCredentialBytes(fields[1], out.UserID[:]); err != nil {
		return CredentialInfo{}, err
	}
	if err = copyCredentialBytes(fields[2], out.DeviceID[:]); err != nil {
		return CredentialInfo{}, err
	}
	kind, err := rawUint(fields[3])
	if err != nil || kind > 1 {
		return CredentialInfo{}, errors.New("mlswasi: verify_credential returned an invalid kind")
	}
	tier, err := rawUint(fields[4])
	if err != nil || tier > 1 {
		return CredentialInfo{}, errors.New("mlswasi: verify_credential returned an invalid tier")
	}
	signerTier, err := rawUint(fields[5])
	if err != nil || signerTier > 2 {
		return CredentialInfo{}, errors.New("mlswasi: verify_credential returned an invalid signer tier")
	}
	out.Kind, out.Tier, out.SignerTier = uint8(kind), uint8(tier), uint8(signerTier)
	if err = copyCredentialBytes(fields[6], out.SSKPub[:]); err != nil {
		return CredentialInfo{}, err
	}
	if err = copyCredentialBytes(fields[7], out.SigUMKSSK[:]); err != nil {
		return CredentialInfo{}, err
	}
	return out, nil
}

func copyCredentialBytes(raw cbor.RawMessage, dst []byte) error {
	b, err := rawBytes(raw)
	if err != nil || len(b) != len(dst) {
		return errors.New("mlswasi: verify_credential returned a byte string with an invalid size")
	}
	copy(dst, b)
	return nil
}
