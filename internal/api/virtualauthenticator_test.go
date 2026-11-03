package api_test

// A software authenticator, so the two passkey finish legs can be driven down
// their SUCCESS path.
//
// Task 10 shipped with both finish routes exercised only on their refusals: an
// unknown ceremony and a spent one. Everything past those — CreateCredential,
// the columns FinishRegistration writes, ValidatePasskeyLogin, the sign-count
// and flag write-back, and every decision the route makes once a login
// succeeds — had never run. This is the ~200 lines that removes that blind
// spot. It is a CTAP2 responder in the shape the specification describes and
// nothing more: one ES256 key, "none" attestation, a monotonic counter.
//
// Nothing here is a stub of the code under test. The bytes it produces are
// parsed, verified and signature-checked by the real go-webauthn, through the
// real routes, against the real store.

import (
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

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// virtualAuthenticator is one discoverable credential on one relying party.
//
// userVerified is the whole point of the type being configurable: an
// authenticator that only proves possession (a security key tapped without a
// PIN) sets UP and not UV, and dillad must not treat that ceremony as two
// factors. Backup eligibility is set and stays set, because validateLogin
// refuses a login whose BE flag differs from the stored one — which is also
// what makes this the end-to-end check on dillad's flag codec.
type virtualAuthenticator struct {
	rpID         string
	origin       string
	key          *ecdsa.PrivateKey
	credID       []byte
	userHandle   []byte
	counter      uint32
	userVerified bool
}

func newVirtualAuthenticator(t *testing.T, rpID string, userVerified bool) *virtualAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return &virtualAuthenticator{
		rpID: rpID, origin: "https://" + rpID, key: key, credID: credID,
		userVerified: userVerified,
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// clientData is the CollectedClientData the CLIENT (not the authenticator)
// produces; its SHA-256 is what the authenticator signs over.
func (a *virtualAuthenticator) clientData(t *testing.T, ceremony, challenge string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type": ceremony, "challenge": challenge, "origin": a.origin, "crossOrigin": false,
	})
	if err != nil {
		t.Fatalf("clientDataJSON: %v", err)
	}
	return body
}

// authData is rpIdHash(32) || flags(1) || signCount(4) || attested, per §6.1.
func (a *virtualAuthenticator) authData(extra protocol.AuthenticatorFlags, attested []byte) []byte {
	hash := sha256.Sum256([]byte(a.rpID))
	flags := protocol.FlagUserPresent | protocol.FlagBackupEligible | protocol.FlagBackupState | extra
	if a.userVerified {
		flags |= protocol.FlagUserVerified
	}
	out := make([]byte, 0, 37+len(attested))
	out = append(out, hash[:]...)
	out = append(out, byte(flags))
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], a.counter)
	out = append(out, count[:]...)
	return append(out, attested...)
}

// attestedCredentialData is aaguid(16) || credIdLen(2) || credId || COSE key.
// The key is written as a raw CBOR map with the labels COSE defines (1 = kty,
// 3 = alg, -1 = crv, -2 = x, -3 = y) rather than through the library's own
// struct, so the test does not depend on the encoder it exercises.
func (a *virtualAuthenticator) attestedCredentialData(t *testing.T) []byte {
	t.Helper()
	// PublicKey.Bytes is the SEC 1 uncompressed point 0x04 || X || Y, 32 bytes each on P-256.
	point, err := a.key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("public key point: %v", err)
	}
	x, y := point[1:33], point[33:65]
	cose, err := cbor.Marshal(map[int64]any{
		1: int64(2), 3: int64(-7), -1: int64(1), -2: x, -3: y,
	})
	if err != nil {
		t.Fatalf("COSE key: %v", err)
	}
	out := make([]byte, 0, 16+2+len(a.credID)+len(cose))
	out = append(out, make([]byte, 16)...) // the all-zero AAGUID: no attestation identity
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(a.credID)))
	out = append(out, length[:]...)
	out = append(out, a.credID...)
	return append(out, cose...)
}

// register answers a navigator.credentials.create() options document with the
// PublicKeyCredential JSON the client would POST back.
func (a *virtualAuthenticator) register(t *testing.T, options string) string {
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
		t.Fatalf("creation options: %v (%q)", err, options)
	}
	handle, err := base64.RawURLEncoding.DecodeString(opts.PublicKey.User.ID)
	if err != nil {
		t.Fatalf("user handle is not base64url: %v", err)
	}
	// The authenticator keeps the handle it saw and hands it back at every
	// later discoverable login. That is what makes the login username-less.
	a.userHandle = handle

	clientData := a.clientData(t, "webauthn.create", opts.PublicKey.Challenge)
	authData := a.authData(protocol.FlagAttestedCredentialData, a.attestedCredentialData(t))
	attestation, err := cbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": authData,
	})
	if err != nil {
		t.Fatalf("attestationObject: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(clientData),
			"attestationObject": b64(attestation),
			"transports":        []string{"internal", "hybrid"},
		},
	})
	if err != nil {
		t.Fatalf("registration response: %v", err)
	}
	return string(body)
}

// login answers a navigator.credentials.get() options document. The signature
// covers authData || SHA-256(clientDataJSON), as §6.3.3 requires, and the
// counter advances so the sign-count write-back has something to write.
func (a *virtualAuthenticator) login(t *testing.T, options string) string {
	t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(options), &opts); err != nil {
		t.Fatalf("assertion options: %v (%q)", err, options)
	}
	a.counter++
	clientData := a.clientData(t, "webauthn.get", opts.PublicKey.Challenge)
	authData := a.authData(0, nil)
	clientDataHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientDataHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("SignASN1: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(clientData),
			"authenticatorData": b64(authData),
			"signature":         b64(sig),
			"userHandle":        b64(a.userHandle),
		},
	})
	if err != nil {
		t.Fatalf("assertion response: %v", err)
	}
	return string(body)
}

// enrolPasskey walks /v1/auth/passkey/register/{begin,finish} for the session
// `token` holds, with no step-up token, and returns the credential id the route
// reports.
func enrolPasskey(t *testing.T, h http.Handler, token string, a *virtualAuthenticator) []byte {
	t.Helper()
	credID, _ := enrolPasskeyWith(t, h, token, a, nil)
	return credID
}

// enrolPasskeyWith is enrolPasskey with a step-up token (nil sends null) and the
// backup codes the finish leg answers alongside the credential id.
func enrolPasskeyWith(t *testing.T, h http.Handler, token string, a *virtualAuthenticator, stepup any) ([]byte, []any) {
	t.Helper()
	begin, _ := cborx.Marshal([]any{stepup})
	res := postCBORAuth(h, "/v1/auth/passkey/register/begin", begin, token)
	if res.Code != http.StatusOK {
		t.Fatalf("register/begin answered %d: %q", res.Code, res.Body.String())
	}
	ceremony, options := ceremonyAndOptions(t, res.Body.Bytes())
	body, _ := cborx.Marshal([]any{ceremony, a.register(t, options), "My passkey"})
	res = postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token)
	if res.Code != http.StatusOK {
		t.Fatalf("register/finish answered %d: %q", res.Code, res.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode register/finish: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("register/finish returned %d elements, want [cred_id, backup_codes]", len(out))
	}
	credID, ok := out[0].([]byte)
	if !ok {
		t.Fatalf("cred_id = %#v, want a byte string", out[0])
	}
	codes, ok := out[1].([]any)
	if !ok {
		t.Fatalf("backup_codes = %#v, want an array", out[1])
	}
	return credID, codes
}

// passkeyLoginNeeds walks /v1/auth/passkey/login/{begin,finish} and returns the
// assertion the finish leg issues with the `needs` it answers.
func passkeyLoginNeeds(t *testing.T, h http.Handler, a *virtualAuthenticator) (string, uint64) {
	t.Helper()
	res := passkeyLoginRaw(t, h, a)
	if res.Code != http.StatusOK {
		t.Fatalf("login/finish answered %d: %q", res.Code, res.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode login/finish: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("login/finish returned %d elements, want [assertion, needs]", len(out))
	}
	assertion, ok := out[0].(string)
	if !ok || assertion == "" {
		t.Fatalf("assertion = %#v, want a text string", out[0])
	}
	needs, ok := out[1].(uint64)
	if !ok {
		t.Fatalf("needs = %#v, want a uint", out[1])
	}
	return assertion, needs
}

// passkeyLoginRaw walks both login legs and returns the finish leg's response
// as it came, for the refusal rows.
func passkeyLoginRaw(t *testing.T, h http.Handler, a *virtualAuthenticator) *httptest.ResponseRecorder {
	t.Helper()
	empty, _ := cborx.Marshal([]any{})
	res := postCBOR(h, "/v1/auth/passkey/login/begin", empty)
	if res.Code != http.StatusOK {
		t.Fatalf("login/begin answered %d: %q", res.Code, res.Body.String())
	}
	ceremony, options := ceremonyAndOptions(t, res.Body.Bytes())
	body, _ := cborx.Marshal([]any{ceremony, a.login(t, options)})
	return postCBOR(h, "/v1/auth/passkey/login/finish", body)
}

// ceremonyAndOptions decodes the [ceremony_id, options] both begin legs answer.
func ceremonyAndOptions(t *testing.T, body []byte) (id.ID, string) {
	t.Helper()
	var out []any
	if err := cborx.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode begin: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("begin returned %d elements, want [ceremony_id, options]", len(out))
	}
	raw, ok := out[0].([]byte)
	if !ok || len(raw) != id.Size {
		t.Fatalf("ceremony_id = %#v", out[0])
	}
	var ceremony id.ID
	copy(ceremony[:], raw)
	options, ok := out[1].(string)
	if !ok {
		t.Fatalf("options = %#v, want a text string", out[1])
	}
	return ceremony, options
}

// spendAssertion reads what an assertion is worth: the account it names and
// whether it still owes a second factor.
func spendAssertion(t *testing.T, d api.Deps, assertion string) (id.ID, bool) {
	t.Helper()
	userID, needsSecondFactor, ok := d.Assertions.Spend(assertion)
	if !ok {
		t.Fatal("the assertion the login issued is not spendable")
	}
	return userID, needsSecondFactor
}
