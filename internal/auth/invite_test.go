package auth_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

func TestNewInviteCodeIsTwentySixBase32Characters(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		code, hash := auth.NewInviteCode()
		if len(code) != 26 {
			t.Fatalf("code %q is %d characters, want 26 (130 bits)", code, len(code))
		}
		if strings.ContainsAny(code, "0189") {
			t.Fatalf("code %q contains a character outside the RFC 4648 base32 alphabet", code)
		}
		if len(hash) != 32 {
			t.Fatalf("hash is %d bytes, want 32", len(hash))
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("code %q repeated", code)
		}
		seen[code] = struct{}{}
	}
}

func TestCanonicalizeInviteCode(t *testing.T) {
	code, _ := auth.NewInviteCode()
	spaced := code[:5] + "-" + code[5:10] + " " + code[10:]
	got, err := auth.CanonicalizeInviteCode(strings.ToLower(spaced))
	if err != nil {
		t.Fatalf("CanonicalizeInviteCode: %v", err)
	}
	if !slices.Contains(got, code) {
		t.Fatalf("candidates %v do not contain the original %q", got, code)
	}
}

func TestCanonicalizeMapsTheConfusablePairs(t *testing.T) {
	// The base32 alphabet has no 0, 1, 8 or 9, so a user who typed one of them
	// meant the letter that looks like it. 1 is ambiguous between I and L, so
	// both candidates are returned and both are looked up.
	base := strings.Repeat("A", 25)
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{base + "0", []string{base + "O"}},
		{base + "8", []string{base + "B"}},
		{base + "1", []string{base + "I", base + "L"}},
	} {
		got, err := auth.CanonicalizeInviteCode(tc.in)
		if err != nil {
			t.Fatalf("CanonicalizeInviteCode(%q): %v", tc.in, err)
		}
		for _, want := range tc.want {
			if !slices.Contains(got, want) {
				t.Fatalf("CanonicalizeInviteCode(%q) = %v, want it to contain %q", tc.in, got, want)
			}
		}
	}
}

func TestCanonicalizeRefusesTheWrongLengthBeforeAnyQuery(t *testing.T) {
	for _, in := range []string{strings.Repeat("A", 25), strings.Repeat("A", 27), ""} {
		if _, err := auth.CanonicalizeInviteCode(in); err == nil {
			t.Fatalf("a %d-character code was accepted", len(in))
		}
	}
}

func TestCandidateExplosionIsBounded(t *testing.T) {
	// Twenty-six 1s would be 2^26 candidates if every one branched. The cap
	// refuses rather than expanding, so a hostile code cannot cost the instance
	// 67 million hashes.
	if _, err := auth.CanonicalizeInviteCode(strings.Repeat("1", 26)); err == nil {
		t.Fatal("a code with 26 ambiguous characters was expanded instead of refused")
	}
}

func TestMatchInviteCodeHashesEveryCandidate(t *testing.T) {
	// A typed 1 is an I or an L, so a code with one 1 has two candidate hashes,
	// and the hash of the code that was actually minted is among them.
	base := strings.Repeat("A", 25)
	got, err := auth.MatchInviteCode(base + "1")
	if err != nil {
		t.Fatalf("MatchInviteCode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d hashes, want 2 (I and L)", len(got))
	}
	want := auth.HashInviteCode(base + "L")
	found := false
	for _, h := range got {
		if slices.Equal(h, want) {
			found = true
		}
	}
	if !found {
		t.Fatal("the hash of the L spelling is not among the candidates")
	}

	// An unambiguous code has exactly one, and it is the hash NewInviteCode stored.
	code, hash := auth.NewInviteCode()
	got, err = auth.MatchInviteCode(strings.ToLower(code))
	if err != nil {
		t.Fatalf("MatchInviteCode(lower): %v", err)
	}
	if len(got) != 1 || !slices.Equal(got[0], hash) {
		t.Fatalf("hashes = %x, want [%x]", got, hash)
	}

	if _, err := auth.MatchInviteCode("too short"); err == nil {
		t.Fatal("a malformed code produced hashes")
	}
}
