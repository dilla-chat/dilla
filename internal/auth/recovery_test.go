package auth_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

// unformat is the spelling the stored hash is taken over: the code without the
// transcription hyphens.
func unformat(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}

func TestRecoveryCodesAreTenDistinct31CharacterCodes(t *testing.T) {
	plain, hashes := auth.GenerateRecoveryCodes(10)
	if len(plain) != 10 || len(hashes) != 10 {
		t.Fatalf("GenerateRecoveryCodes(10) = %d codes, %d hashes; want 10 and 10", len(plain), len(hashes))
	}
	seen := map[string]bool{}
	for i, c := range plain {
		if len(c) != 31 {
			t.Fatalf("code %d is %q, %d characters; want 31 (26 base32 characters plus the group hyphens)", i, c, len(c))
		}
		bare := strings.ReplaceAll(c, "-", "")
		if len(bare) != 26 {
			t.Fatalf("code %d carries %d base32 characters, want the 26 of crypto/rand.Text", i, len(bare))
		}
		if seen[c] {
			t.Fatalf("code %d repeats an earlier code: %q", i, c)
		}
		seen[c] = true
	}
}

func TestTheStoredHashIsSHA256OfTheUnformattedCode(t *testing.T) {
	plain, hashes := auth.GenerateRecoveryCodes(3)
	for i, c := range plain {
		want := sha256.Sum256([]byte(strings.ReplaceAll(c, "-", "")))
		if string(hashes[i]) != string(want[:]) {
			t.Fatalf("hash %d is not sha256 of the unformatted code %q", i, c)
		}
	}
}

func TestVerifyRecoveryCodeAcceptsEverySpellingAndReturnsTheIndex(t *testing.T) {
	plain, hashes := auth.GenerateRecoveryCodes(10)
	for i, c := range plain {
		bare := strings.ReplaceAll(c, "-", "")
		for _, spelling := range []string{
			c,                                 // as printed, hyphenated
			strings.ToLower(c),                // a user who typed it in lower case
			bare,                              // no separators at all
			strings.ReplaceAll(c, "-", " "),   // spaces instead of hyphens
			" " + strings.ToLower(bare) + " ", // pasted with surrounding spaces
		} {
			idx, ok := auth.VerifyRecoveryCode(spelling, hashes)
			if !ok {
				t.Fatalf("code %d spelled %q was refused", i, spelling)
			}
			if idx != i {
				t.Fatalf("code %d spelled %q returned index %d", i, spelling, idx)
			}
		}
	}
}

func TestAnUnknownRecoveryCodeReturnsMinusOne(t *testing.T) {
	_, hashes := auth.GenerateRecoveryCodes(10)
	idx, ok := auth.VerifyRecoveryCode("AAAAA-AAAAA-AAAAA-AAAAA-AAAAA-A", hashes)
	if ok || idx != -1 {
		t.Fatalf("an unknown code returned (%d, %v), want (-1, false)", idx, ok)
	}
	if _, ok := auth.VerifyRecoveryCode("", hashes); ok {
		t.Fatal("the empty code verified")
	}
	// The unformatted spelling is what the hash is taken over, so a code that
	// never existed must not collide with one that did.
	if _, ok := auth.VerifyRecoveryCode(unformat("zzzzz-zzzzz"), hashes); ok {
		t.Fatal("a short unknown code verified")
	}
}
