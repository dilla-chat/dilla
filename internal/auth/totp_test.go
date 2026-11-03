package auth

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/dilla-chat/dilla/internal/config"
)

func totpParams() TOTPParams {
	c := config.Default().Auth.TOTP
	c.Issuer = "chat.example"
	return TOTPParams{Issuer: c.Issuer, Period: c.Period, Skew: c.Skew,
		SecretSize: c.SecretSize, Digits: c.Digits, Algorithm: c.Algorithm}
}

func TestTOTPValidatesAtSkewOneAndRefusesBeyondIt(t *testing.T) {
	secret, url, err := GenerateTOTP("chat.example", "jonas", totpParams())
	if err != nil {
		t.Fatalf("GenerateTOTP: %v", err)
	}
	if len(secret) != 32 {
		t.Fatalf("secret is %d base32 characters, want 32 (20 bytes)", len(secret))
	}
	if url == "" {
		t.Fatal("no otpauth URL")
	}
	now := time.Unix(1_700_000_000, 0)
	code, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	for _, offset := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		if _, ok := ValidateTOTP(code, secret, totpParams(), now.Add(offset), 0); !ok {
			t.Fatalf("code refused at offset %s; skew 1 accepts one period either side", offset)
		}
	}
	if _, ok := ValidateTOTP(code, secret, totpParams(), now.Add(90*time.Second), 0); ok {
		t.Fatal("a code three periods away was accepted")
	}
}

func TestTOTPCounterIsReturnedForTheReplayGuard(t *testing.T) {
	secret, _, err := GenerateTOTP("chat.example", "jonas", totpParams())
	if err != nil {
		t.Fatalf("GenerateTOTP: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	code, _ := totp.GenerateCode(secret, now)
	counter, ok := ValidateTOTP(code, secret, totpParams(), now, 0)
	if !ok {
		t.Fatal("the code did not validate")
	}
	if counter != now.Unix()/30 {
		t.Fatalf("counter = %d, want %d: the store's ConsumeTOTPCounter needs the accepted counter, not the current one",
			counter, now.Unix()/30)
	}
}

func TestAMalformedSecretIsAnErrorNotASilentFalse(t *testing.T) {
	if _, ok := ValidateTOTP("123456", "not base32!", totpParams(), time.Now(), 0); ok {
		t.Fatal("a malformed secret validated a code")
	}
}

// acceptAt substitutes validateAt for the test with a double that accepts the code at exactly the
// given counters (a period of 30 s) and refuses it everywhere else. No search runs in the test: a
// six-digit code that validates at two counters of one skew window is a real collision (about one
// window in 10^6), and this double is that collision, constructed (web3.7 R34).
func acceptAt(t *testing.T, counters ...int64) {
	t.Helper()
	kept := validateAt
	t.Cleanup(func() { validateAt = kept })
	validateAt = func(_, _ string, at time.Time, _ totp.ValidateOpts) (bool, error) {
		for _, c := range counters {
			if at.Unix()/30 == c {
				return true, nil
			}
		}
		return false, nil
	}
}

// The collision: the code validates at c-1 and at c+1 and not at c. The counter c-1 is already
// spent (after = c-1), so the lowest match is one the replay guard refuses; the lowest match
// ABOVE it, c+1, is the answer, and the caller's guard accepts it.
func TestTheLowestMatchAboveTheSpentCounterIsReturned(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := now.Unix() / 30
	acceptAt(t, c-1, c+1)

	counter, ok := ValidateTOTP("123456", "JBSWY3DPEHPK3PXP", totpParams(), now, c-1)
	if !ok || counter != c+1 {
		t.Fatalf("ValidateTOTP(after = c-1) = %d, %v; want c+1 = %d, true: c-1 is spent, c+1 is not", counter, ok, c+1)
	}
	// Nothing spent inside the window: the lowest match is the answer.
	if counter, ok := ValidateTOTP("123456", "JBSWY3DPEHPK3PXP", totpParams(), now, c-2); !ok || counter != c-1 {
		t.Fatalf("ValidateTOTP(after = c-2) = %d, %v; want c-1 = %d, true", counter, ok, c-1)
	}
	// At or above every match there is nothing left to accept.
	for _, after := range []int64{c + 1, c + 5} {
		if counter, ok := ValidateTOTP("123456", "JBSWY3DPEHPK3PXP", totpParams(), now, after); ok {
			t.Fatalf("ValidateTOTP(after = c%+d) = %d, true; want false: every match is at or below after", after-c, counter)
		}
	}
}

// A code valid at one counter only answers that counter.
func TestACodeValidOnceAnswersItsCounter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := now.Unix() / 30
	acceptAt(t, c)
	if counter, ok := ValidateTOTP("123456", "JBSWY3DPEHPK3PXP", totpParams(), now, c-1); !ok || counter != c {
		t.Fatalf("ValidateTOTP = %d, %v; want c = %d, true", counter, ok, c)
	}
	if counter, ok := ValidateTOTP("123456", "JBSWY3DPEHPK3PXP", totpParams(), now, c); ok {
		t.Fatalf("ValidateTOTP(after = c) = %d, true; want false", counter)
	}
}
