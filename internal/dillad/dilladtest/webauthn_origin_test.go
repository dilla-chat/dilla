package dilladtest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// A software authenticator, as internal/api's virtualauthenticator_test.go, for the host's own
// relying party: one ES256 key, "none" attestation, user verification set.
type softKey struct {
	rpID, origin string
	key          *ecdsa.PrivateKey
	credID       []byte
	userHandle   []byte
	counter      uint32
}

func newSoftKey(t *testing.T, rpID, origin string) *softKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return &softKey{rpID: rpID, origin: origin, key: key, credID: credID}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *softKey) clientData(ceremony, challenge string) []byte {
	body, _ := json.Marshal(map[string]any{"type": ceremony, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return body
}

func (a *softKey) authData(extra protocol.AuthenticatorFlags, attested []byte) []byte {
	hash := sha256.Sum256([]byte(a.rpID))
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagBackupEligible | protocol.FlagBackupState | extra
	out := append([]byte{}, hash[:]...)
	out = append(out, byte(flags))
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], a.counter)
	return append(append(out, count[:]...), attested...)
}

func (a *softKey) register(t *testing.T, options string) string {
	t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(options), &opts); err != nil {
		t.Fatalf("creation options: %v", err)
	}
	a.userHandle, _ = base64.RawURLEncoding.DecodeString(opts.PublicKey.User.ID)
	point, err := a.key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	cose, _ := cbor.Marshal(map[int64]any{1: int64(2), 3: int64(-7), -1: int64(1), -2: point[1:33], -3: point[33:65]})
	attested := make([]byte, 16, 16+2+len(a.credID)+len(cose))
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(a.credID)))
	attested = append(append(append(attested, length[:]...), a.credID...), cose...)
	attestation, _ := cbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(protocol.FlagAttestedCredentialData, attested),
	})
	body, _ := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(a.clientData("webauthn.create", opts.PublicKey.Challenge)),
			"attestationObject": b64(attestation),
			"transports":        []string{"internal"},
		},
	})
	return string(body)
}

func (a *softKey) login(t *testing.T, options string) string {
	t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(options), &opts); err != nil {
		t.Fatalf("assertion options: %v", err)
	}
	a.counter++
	clientData := a.clientData("webauthn.get", opts.PublicKey.Challenge)
	authData := a.authData(0, nil)
	hash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), hash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64(clientData), "authenticatorData": b64(authData),
			"signature": b64(sig), "userHandle": b64(a.userHandle),
		},
	})
	return string(body)
}

func postCBOR(h http.Handler, path, token string, body []any) *httptest.ResponseRecorder {
	raw, _ := cborx.Marshal(body)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/cbor")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ceremonyAndOptions(t *testing.T, body []byte) (id.ID, string) {
	t.Helper()
	var out []any
	if err := cborx.Unmarshal(body, &out); err != nil || len(out) != 2 {
		t.Fatalf("begin body %x: %v", body, err)
	}
	raw, _ := out[0].([]byte)
	options, _ := out[1].(string)
	return id.ID(raw), options
}

// HostOptions.WebAuthnOrigin makes the host's relying party the origin's hostname with exactly that
// origin accepted, so a browser (or a virtual authenticator) at http://localhost:9999 can register
// a passkey and sign in with it. The control is an authenticator that answers from the instance
// domain's own origin: the host no longer accepts it.
func TestAPasskeyIsRegisteredAndUsedAgainstTheConfiguredOrigin(t *testing.T) {
	const origin = "http://localhost:9999"
	h, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), CorePath: corePath(t), WebAuthnOrigin: origin})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	cfg := h.cfg
	if cfg.Auth.WebAuthn.RPID != "localhost" || len(cfg.Auth.WebAuthn.RPOrigins) != 1 || cfg.Auth.WebAuthn.RPOrigins[0] != origin {
		t.Fatalf("relying party = %q %v, want localhost [%s]", cfg.Auth.WebAuthn.RPID, cfg.Auth.WebAuthn.RPOrigins, origin)
	}

	// A seeded human owes a factor, so this is the forced set-up path: the first factor needs no
	// step-up token, and the session may reach the passkey routes.
	_, _, token := seedOwedPerson(t, h, "alice")
	register := func(a *softKey) *httptest.ResponseRecorder {
		t.Helper()
		begin := postCBOR(h.Handler(), "/v1/auth/passkey/register/begin", token, []any{nil})
		if begin.Code != http.StatusOK {
			t.Fatalf("register/begin = %d: %q", begin.Code, begin.Body.String())
		}
		ceremony, options := ceremonyAndOptions(t, begin.Body.Bytes())
		return postCBOR(h.Handler(), "/v1/auth/passkey/register/finish", token, []any{ceremony, a.register(t, options), "Key"})
	}

	if rec := register(newSoftKey(t, "localhost", "https://dilla.test")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a passkey from the instance domain's origin = %d, want 400 (the origin is not rp_origins)", rec.Code)
	}
	a := newSoftKey(t, "localhost", origin)
	if rec := register(a); rec.Code != http.StatusOK {
		t.Fatalf("register/finish from %s = %d: %q", origin, rec.Code, rec.Body.String())
	}

	begin := postCBOR(h.Handler(), "/v1/auth/passkey/login/begin", "", []any{})
	if begin.Code != http.StatusOK {
		t.Fatalf("login/begin = %d: %q", begin.Code, begin.Body.String())
	}
	ceremony, options := ceremonyAndOptions(t, begin.Body.Bytes())
	fin := postCBOR(h.Handler(), "/v1/auth/passkey/login/finish", "", []any{ceremony, a.login(t, options)})
	if fin.Code != http.StatusOK {
		t.Fatalf("login/finish = %d: %q", fin.Code, fin.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(fin.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("login/finish body %x: %v", fin.Body.Bytes(), err)
	}
	if assertion, _ := out[0].(string); assertion == "" {
		t.Fatalf("assertion = %#v", out[0])
	}
	if needs, _ := out[1].(uint64); needs != 0 {
		t.Fatalf("needs = %d, want 0: a user-verified passkey is both factors", needs)
	}
}
