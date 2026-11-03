package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/server"
)

func testParams() auth.PasswordParams {
	return auth.ParamsFromConfig(config.Default().Auth.Password)
}

func TestPHCStringRoundTrips(t *testing.T) {
	phc, err := auth.HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC string %q does not carry the OWASP 2026 parameters", phc)
	}
	ok, rehash, err := auth.VerifyPassword("correct horse battery staple", phc)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword = %v, %v", ok, err)
	}
	if rehash {
		t.Fatal("a hash at the current parameters asked to be rehashed")
	}
	ok, _, err = auth.VerifyPassword("wrong", phc)
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("a wrong password verified")
	}
}

func TestNeedsRehashFiresWhenTheParametersMove(t *testing.T) {
	weak := testParams()
	weak.MemoryKiB = 19456
	weak.Iterations = 2
	phc, err := auth.HashPassword("pw", weak)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// Verify against a policy that has since been raised.
	strong := weak
	strong.Iterations = 4
	ok, rehash, err := auth.VerifyPasswordWithPolicy("pw", phc, strong)
	if err != nil || !ok {
		t.Fatalf("VerifyPasswordWithPolicy = %v, %v", ok, err)
	}
	if !rehash {
		t.Fatal("a hash below the current policy did not ask to be rehashed")
	}
}

func TestMalformedPHCStringsAreRefusedNotPanicked(t *testing.T) {
	for _, phc := range []string{
		"", "notaphc", "$argon2id$v=19$m=19456,t=2,p=1$", "$argon2i$v=19$m=19456,t=2,p=1$YWJj$ZGVm",
		"$argon2id$v=19$m=x,t=2,p=1$YWJj$ZGVm", "$argon2id$v=19$m=19456,t=2,p=1$!!!$ZGVm",
	} {
		if _, _, err := auth.VerifyPassword("pw", phc); err == nil {
			t.Fatalf("VerifyPassword accepted %q", phc)
		}
	}
}

func TestHasherRefusesOverConcurrencyInsteadOfQueueing(t *testing.T) {
	h := auth.NewHasher(testParams(), 1)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.Hash(ctx, "pw")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	refused := 0
	for err := range results {
		if err == nil {
			continue
		}
		var se *server.Error
		if !errors.As(err, &se) || se.Code != server.CodeRateLimited {
			t.Fatalf("hash error = %v, want E_RATE_LIMITED", err)
		}
		refused++
	}
	if refused == 0 {
		t.Fatal("eight concurrent hashes at concurrency 1 all succeeded; the semaphore queued instead of refusing")
	}
}

// The phc == "" branch of Hasher.Verify is the whole timing-oracle defence for
// "no such account", and nothing else in the module executes it: internal/api
// substitutes a sha256 stub for the hasher, so its unknown-account path never
// reaches this code. handle@host makes handles semi-public, which is exactly
// why the absence of an account must not be cheaper than a wrong password.
func TestVerifyBurnsTheDummyHashForAnAccountThatDoesNotExist(t *testing.T) {
	h := auth.NewHasher(testParams(), 4)
	ctx := context.Background()
	phc, err := auth.HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	start := time.Now()
	ok, rehash, err := h.Verify(ctx, "correct horse battery staple", "")
	unknown := time.Since(start)
	if ok || rehash || err != nil {
		t.Fatalf(`Verify(pw, "") = %v, %v, %v; want false, false, nil`, ok, rehash, err)
	}

	start = time.Now()
	if ok, _, err := h.Verify(ctx, "correct horse battery staple", phc); !ok || err != nil {
		t.Fatalf("Verify against a real hash = %v, %v", ok, err)
	}
	known := time.Since(start)

	// Deliberately generous: the claim is only that the unknown-account path
	// does Argon2id work at all. An early return, or an empty dummy that errors
	// out of decodePHC, is microseconds against tens of milliseconds.
	if unknown < known/5 {
		t.Fatalf("an unknown account cost %s against %s for a real verify; the dummy hash is not being burnt",
			unknown, known)
	}
}

// NewHasher used to swallow HashPassword's error and keep an empty dummy, which
// would disarm the burn above for the whole life of the process, silently, at
// startup. Parameters argon2 cannot hash are a programming or configuration
// error and must be loud.
func TestNewHasherRefusesParametersItCannotHash(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewHasher accepted t=0 and kept an empty timing-equaliser dummy")
		}
	}()
	bad := testParams()
	bad.Iterations = 0
	auth.NewHasher(bad, 1)
}
