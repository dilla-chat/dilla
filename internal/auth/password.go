package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/server"
)

// PasswordParams is one Argon2id cost setting. The defaults are OWASP's 2026
// floor (19 MiB, t=2, p=1), not RFC 9106's 64 MiB / t=3 / p=4: dillad shares a
// process with LiveKit inside an LXC, and p=4 x 64 MiB turns eight concurrent
// logins into half a gigabyte of transient allocation.
type PasswordParams struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltBytes   uint32
	KeyBytes    uint32
}

func ParamsFromConfig(c config.Password) PasswordParams {
	return PasswordParams{
		MemoryKiB: c.Argon2MemoryKiB, Iterations: c.Argon2Iterations,
		Parallelism: c.Argon2Parallelism, SaltBytes: c.Argon2SaltBytes, KeyBytes: c.Argon2KeyBytes,
	}
}

// PasswordMinChars is the shortest password a person may set, at registration or when changing it
// (Jonas, 2026-10-30: 12). It counts Unicode code points, as the web client does. Only a password being
// set is measured: a stored one that is shorter, set before the minimum, still signs in.
const PasswordMinChars = 12

// PasswordTooShort reports whether pw is below PasswordMinChars.
func PasswordTooShort(pw string) bool { return utf8.RuneCountInString(pw) < PasswordMinChars }

var (
	ErrPHCMalformed = errors.New("auth: malformed PHC string")
	ErrPHCAlgorithm = errors.New("auth: not an argon2id PHC string")
)

// HashPassword returns the standard PHC string, so a dilla hash is verifiable
// by any other Argon2 implementation and vice versa. x/crypto/argon2 has no PHC
// encoder and no pepper input, so both are dilla's.
//
// Two source-level facts: t = 0 or p = 0 PANICS inside deriveKey rather than
// erroring, so config.Validate refuses them before a login ever runs; and the
// memory parameter is silently rounded down to a multiple of 4*p, which is why
// the CONFIGURED value rides in the PHC string and verification reproduces the
// same rounding.
func HashPassword(pw string, p PasswordParams) (string, error) {
	if p.Iterations < 1 || p.Parallelism < 1 {
		return "", fmt.Errorf("auth: argon2 parameters t=%d p=%d would panic", p.Iterations, p.Parallelism)
	}
	salt := make([]byte, p.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks pw against phc using phc's own parameters, and reports
// whether the stored parameters are below the package defaults.
func VerifyPassword(pw, phc string) (bool, bool, error) {
	return VerifyPasswordWithPolicy(pw, phc, PasswordParams{MemoryKiB: 19456, Iterations: 2, Parallelism: 1, SaltBytes: 16, KeyBytes: 32})
}

// VerifyPasswordWithPolicy is VerifyPassword against an explicit current policy.
func VerifyPasswordWithPolicy(pw, phc string, policy PasswordParams) (bool, bool, error) {
	stored, salt, want, err := decodePHC(phc)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(pw), salt, stored.Iterations, stored.MemoryKiB, stored.Parallelism, uint32(len(want))) //nolint:gosec // G115: the length of an in-memory Argon2 key or salt, a few dozen bytes
	// Constant-time on the derived key bytes, never on the PHC string and never
	// with == or bytes.Equal.
	if subtle.ConstantTimeEq(int32(len(got)), int32(len(want))) != 1 { //nolint:gosec // G115: the length of an in-memory Argon2 key or salt, a few dozen bytes
		return false, false, nil
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	rehash := stored.MemoryKiB < policy.MemoryKiB ||
		stored.Iterations < policy.Iterations ||
		stored.Parallelism != policy.Parallelism ||
		uint32(len(want)) != policy.KeyBytes || //nolint:gosec // G115: the length of an in-memory Argon2 key or salt, a few dozen bytes
		uint32(len(salt)) < policy.SaltBytes //nolint:gosec // G115: the length of an in-memory Argon2 key or salt, a few dozen bytes
	return true, rehash, nil
}

func decodePHC(phc string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" {
		return PasswordParams{}, nil, nil, ErrPHCMalformed
	}
	if parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, ErrPHCAlgorithm
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return PasswordParams{}, nil, nil, ErrPHCMalformed
	}
	var p PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return PasswordParams{}, nil, nil, ErrPHCMalformed
	}
	if p.Iterations < 1 || p.Parallelism < 1 {
		return PasswordParams{}, nil, nil, ErrPHCMalformed
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return PasswordParams{}, nil, nil, ErrPHCMalformed
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return PasswordParams{}, nil, nil, ErrPHCMalformed
	}
	p.SaltBytes = uint32(len(salt)) //nolint:gosec // G115: the length of an in-memory Argon2 key or salt, a few dozen bytes
	p.KeyBytes = uint32(len(key))   //nolint:gosec // G115: the length of an in-memory Argon2 key or salt, a few dozen bytes
	return p, salt, key, nil
}

// Hasher bounds concurrent Argon2id work. Over the limit a request is REFUSED
// with E_RATE_LIMITED rather than queued: queueing turns the login endpoint
// into a memory-exhaustion lever against the SFU in the same process.
type Hasher struct {
	params PasswordParams
	slots  chan struct{}
	// dummy is verified against for an unknown username, so "no such user" and
	// "wrong password" cost the same wall time.
	dummy string
}

func NewHasher(p PasswordParams, concurrency int) *Hasher {
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	// Loudly, not silently. A swallowed error here leaves dummy empty, Verify's
	// unknown-account branch falls straight out of decodePHC without hashing
	// anything, and the timing oracle this Hasher exists to close is open for
	// the life of the process with nothing to show for it. The only way to get
	// here is t = 0, p = 0 or a CSPRNG failure, none of which a running server
	// may paper over.
	dummy, err := HashPassword("dilla timing equaliser", p)
	if err != nil {
		panic("auth: NewHasher cannot build the timing-equaliser hash: " + err.Error())
	}
	return &Hasher{params: p, slots: make(chan struct{}, concurrency), dummy: dummy}
}

func (h *Hasher) acquire() error {
	select {
	case h.slots <- struct{}{}:
		return nil
	default:
		return server.RateLimited(1000)
	}
}

func (h *Hasher) release() { <-h.slots }

func (h *Hasher) Hash(ctx context.Context, pw string) (string, error) {
	if err := h.acquire(); err != nil {
		return "", err
	}
	defer h.release()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return HashPassword(pw, h.params)
}

func (h *Hasher) Verify(ctx context.Context, pw, phc string) (bool, bool, error) {
	if err := h.acquire(); err != nil {
		return false, false, err
	}
	defer h.release()
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	if phc == "" {
		// Unknown user: burn one hash against the dummy so the absence of an
		// account is not a timing oracle. handle@host makes handles semi-public,
		// so this matters more here than usual.
		_, _, _ = VerifyPasswordWithPolicy(pw, h.dummy, h.params)
		return false, false, nil
	}
	return VerifyPasswordWithPolicy(pw, phc, h.params)
}
