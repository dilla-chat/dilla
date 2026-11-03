package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

// with puts the rune r between an "a" and a "b". The invisible characters are written as code points, never
// as literals, so the source shows what the test pins (and bidichk and gosec G116 stay quiet).
func with(r rune) string { return "a" + string(r) + "b" }

// The characters NormalizeUserText refuses by code point.
var refusedRunes = map[string]rune{
	"newline": 0x0a, "tab": 0x09, "nul": 0x00, "esc": 0x1b, "carriage return": 0x0d,
	"lrm": 0x200e, "rlm": 0x200f, "lre": 0x202a, "rle": 0x202b, "pdf": 0x202c, "lro": 0x202d, "rlo": 0x202e,
	"lri": 0x2066, "rli": 0x2067, "fsi": 0x2068, "pdi": 0x2069,
	"zero width space": 0x200b, "zwnj": 0x200c, "bare zwj": 0x200d, "word joiner": 0x2060, "bom": 0xfeff,
	"arabic letter mark": 0x061c, "line separator": 0x2028, "paragraph separator": 0x2029,
}

func TestNormalizeUserText(t *testing.T) {
	if got, err := auth.NormalizeUserText("cafe"+string(rune(0x301))+" open", 190, false); err != nil || got != "caf"+string(rune(0xe9))+" open" {
		t.Fatalf("NFC: %q, %v", got, err)
	}
	if got, err := auth.NormalizeUserText("  padded  ", 190, false); err != nil || got != "padded" {
		t.Fatalf("trimmed: %q, %v", got, err)
	}
	if got, err := auth.NormalizeUserText("", 190, false); err != nil || got != "" {
		t.Fatalf("empty clears: %q, %v", got, err)
	}
	if got, err := auth.NormalizeUserText(strings.Repeat(string(rune(0xe4)), 190), 190, false); err != nil || len([]rune(got)) != 190 {
		t.Fatalf("190 characters refused: %v", err)
	}
	if _, err := auth.NormalizeUserText(strings.Repeat("a", 191), 190, false); !errors.Is(err, auth.ErrTextLength) {
		t.Fatalf("191 characters = %v, want ErrTextLength", err)
	}
	zwj := string(rune(0x200d))
	family := string(rune(0x1f468)) + zwj + string(rune(0x1f469)) + zwj + string(rune(0x1f467))
	if got, err := auth.NormalizeUserText("family "+family, 190, false); err != nil || got != "family "+family {
		t.Fatalf("an emoji joiner sequence must pass: %q, %v", got, err)
	}
	for name, r := range refusedRunes {
		if _, err := auth.NormalizeUserText(with(r), 190, false); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want ErrTextChars", name, err)
		}
	}
	for name, r := range map[string]rune{"space": 0x20, "nbsp": 0xa0, "ideographic space": 0x3000} {
		if _, err := auth.NormalizeUserText(strings.Repeat(string(r), 3), 190, false); !errors.Is(err, auth.ErrTextBlank) {
			t.Errorf("%s: %v, want ErrTextBlank", name, err)
		}
	}
}

func TestNormalizeUserTextMultiline(t *testing.T) {
	if got, err := auth.NormalizeUserText("one\ntwo\n\nthree", 500, true); err != nil || got != "one\ntwo\n\nthree" {
		t.Fatalf("newlines kept: %q, %v", got, err)
	}
	if got, err := auth.NormalizeUserText("one\r\ntwo", 500, true); err != nil || got != "one\ntwo" {
		t.Fatalf("CRLF becomes LF: %q, %v", got, err)
	}
	if _, err := auth.NormalizeUserText("\n \n", 500, true); !errors.Is(err, auth.ErrTextBlank) {
		t.Fatalf("only line breaks = %v, want ErrTextBlank", err)
	}
	if _, err := auth.NormalizeUserText(strings.Repeat(string(rune(0xf6)), 500), 500, true); err != nil {
		t.Fatalf("500 characters refused: %v", err)
	}
	if _, err := auth.NormalizeUserText(strings.Repeat("a", 501), 500, true); !errors.Is(err, auth.ErrTextLength) {
		t.Fatalf("501 characters = %v, want ErrTextLength", err)
	}
	for name, r := range refusedRunes {
		if name == "newline" {
			continue
		}
		if _, err := auth.NormalizeUserText(with(r), 500, true); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want ErrTextChars", name, err)
		}
	}
}

// The display name shares the character rule: it refuses the invisible characters too.
func TestNormalizeDisplayRefusesInvisibleCharacters(t *testing.T) {
	for _, name := range []string{"zero width space", "arabic letter mark", "line separator", "rlo"} {
		if _, err := auth.NormalizeDisplay(with(refusedRunes[name])); err == nil {
			t.Errorf("display name with %s was accepted", name)
		}
	}
	if got, err := auth.NormalizeDisplay("Ada L" + string(rune(0xf6)) + "vel"); err != nil || got == "" {
		t.Fatalf("an ordinary name: %q, %v", got, err)
	}
}
