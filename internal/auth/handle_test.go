package auth_test

import (
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestNormalizeHandle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"lowercases", "Jonas", "jonas", true},
		{"all caps", "JONAS", "jonas", true},
		{"space is disallowed", "jo nas", "", false},
		{"zero width space fails the bidi rule", "jonas\u200b", "", false},
		{"roman numeral is disallowed", "Ⅹ", "", false},
		{"mixed script fails the bidi rule", "jonasא", "", false},
		// Trap 1 (facts-auth §6.2): UsernameCaseMapped has no DisallowEmpty and
		// returns ("", nil) for the empty string. The length check is ours.
		{"empty is refused by us, not by precis", "", "", false},
		{"two characters is too short", "jo", "", false},
		{"thirty-three characters is too long", strings.Repeat("a", 33), "", false},
		// Trap 2: LowerCase() is not case folding, so sharp s is preserved and
		// strasse and straße are two different handles. v1 accepts that and the
		// ASCII rule then refuses the non-ASCII one.
		{"sharp s is not folded and is not ASCII", "straße", "", false},
		{"non-ASCII handles are refused at v1", "åsa", "", false},
		{"dots inside are allowed", "jonas.thim", "jonas.thim", true},
		{"a leading dot is refused", ".jonas", "", false},
		{"a trailing dot is refused", "jonas.", "", false},
		{"an at sign is refused", "jonas@host", "", false},
		{"digits and dashes are allowed", "jonas-2", "jonas-2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.NormalizeHandle(tc.in)
			if tc.ok != (err == nil) {
				t.Fatalf("NormalizeHandle(%q) = %q, %v; want ok=%v", tc.in, got, err, tc.ok)
			}
			if tc.ok && got != tc.want {
				t.Fatalf("NormalizeHandle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeDisplay(t *testing.T) {
	if _, err := auth.NormalizeDisplay("Jonas\u202e"); err == nil {
		t.Fatal("a right-to-left override was accepted in a display name")
	}
	got, err := auth.NormalizeDisplay("Å")
	if err != nil {
		t.Fatalf("NormalizeDisplay: %v", err)
	}
	if got != "Å" {
		t.Fatalf("NormalizeDisplay did not apply NFC: %q", got)
	}
	if _, err := auth.NormalizeDisplay(strings.Repeat("x", 65)); err == nil {
		t.Fatal("a 65-code-point display name was accepted")
	}
}

func TestUserFlagBitLayout(t *testing.T) {
	// NV8: protocol/09-http-api.md § Flags fixes these two bits.
	if store.UserFlagInstanceAdmin != 1 {
		t.Fatalf("instance admin bit = %d, want bit 0", store.UserFlagInstanceAdmin)
	}
	if store.UserFlagBotOperator != 2 {
		t.Fatalf("bot operator bit = %d, want bit 1", store.UserFlagBotOperator)
	}
}
