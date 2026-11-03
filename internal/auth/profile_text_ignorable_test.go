package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

// Every Default_Ignorable_Code_Point renders as nothing, so none may stand in a text: not alone, not between letters.
// These are the ranges the re-review found accepted, each by a few code points.
func TestDefaultIgnorableCodePointsAreRefused(t *testing.T) {
	cps := []rune{
		0x2065, 0xfff0, 0xfff8, 0x1d173, 0x1d17a, 0xe0001, 0xe0000, 0xe0002, 0xe001f, 0xe0080, 0xe00ff, 0xe01f0, 0xe0fff,
		0x1bca0, 0x1bca3, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180f, 0x00ad, 0x034f, 0x061c, 0x115f, 0x1160, 0x180e,
		0x200b, 0x2060, 0x2064, 0x206a, 0x206f, 0x3164, 0xfeff, 0xffa0,
	}
	for _, cp := range cps {
		if err := textErr(runes('a', cp, 'b')); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("U+%04X between letters: %v, want ErrTextChars", cp, err)
		}
		if err := textErr(runes(cp)); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("U+%04X alone: %v, want ErrTextChars (never an invisible name that is not blank)", cp, err)
		}
		if _, err := auth.NormalizeDisplay(runes(cp)); err == nil {
			t.Errorf("U+%04X alone was accepted as a display name", cp)
		}
	}
}

// Unassigned code points, noncharacters and private use drift between Unicode versions and render unpredictably.
func TestUnassignedNoncharacterAndPrivateUseAreRefused(t *testing.T) {
	cps := map[string]rune{
		"unassigned in the Latin block": 0x378, "unassigned in a high plane": 0x40000, "noncharacter FFFF": 0xffff, "noncharacter FFFE": 0xfffe,
		"noncharacter FDD0": 0xfdd0, "noncharacter 1FFFE": 0x1fffe, "noncharacter 10FFFF": 0x10ffff, "private use": 0xe000, "private use A": 0xf0000,
		"private use B": 0x10fffd,
	}
	for name, cp := range cps {
		if err := textErr(runes('a', cp, 'b')); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want ErrTextChars", name, err)
		}
	}
	// the replacement character is assigned and visible
	if err := textErr(runes('a', 0xfffd, 'b')); err != nil {
		t.Errorf("U+FFFD: %v, want accepted", err)
	}
}

// A string that is not valid UTF-8 is refused, so a client's lone surrogate cannot become U+FFFD on the server.
func TestInvalidUTF8IsRefused(t *testing.T) {
	for name, in := range map[string]string{"stray byte": "a\xffb", "truncated": "a\xe2\x82", "encoded surrogate": "a\xed\xa0\x80b"} {
		if err := textErr(in); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want ErrTextChars", name, err)
		}
	}
}

// A Mongolian free variation selector follows a Mongolian letter, and only one.
func TestMongolianFreeVariationSelectorsFollowALetter(t *testing.T) {
	const a = 0x1820 // MONGOLIAN LETTER A
	accepted(t, "letter and FVS1", a, 0x180b)
	accepted(t, "letter and FVS2", a, 0x180c)
	accepted(t, "letter and FVS3", a, 0x180d)
	accepted(t, "letter and FVS4", a, 0x180f)
	accepted(t, "two letters each with one", a, 0x180b, a, 0x180c)
	chars(t, "FVS after a Latin letter", 'a', 0x180b)
	chars(t, "FVS alone", 0x180b)
	chars(t, "two FVS on one letter", a, 0x180b, 0x180c)
	chars(t, "FVS after a space", a, ' ', 0x180b)
	chars(t, "the vowel separator, U+180E", a, 0x180e)
}

// More than 8 combining marks on one base is overlay ("zalgo") text; real scripts stay well below it.
func TestAStackOfMoreThanEightCombiningMarksIsRefused(t *testing.T) {
	marks := func(n int) string { return "a" + strings.Repeat(string(rune(0x301)), n) }
	for _, n := range []int{1, 3, 8} {
		if err := textErr(marks(n)); err != nil {
			t.Errorf("a + %d marks: %v, want accepted", n, err)
		}
	}
	for _, n := range []int{9, 10, 30, 31, 100} {
		if err := textErr(marks(n)); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("a + %d marks: %v, want ErrTextChars", n, err)
		}
	}
	if err := textErr(marks(8) + marks(8) + marks(8)); err != nil {
		t.Errorf("three bases with 8 marks each: %v, want accepted", err)
	}
	// Vietnamese and Hebrew with points
	accepted(t, "vietnamese e with circumflex and dot below", 'e', 0x302, 0x323)
	accepted(t, "hebrew shin with dagesh, shin dot and a vowel", 0x5e9, 0x5bc, 0x5c1, 0x5b8)
	// a display name too
	if _, err := auth.NormalizeDisplay(marks(9)); err == nil {
		t.Error("a display name with 9 marks was accepted")
	}
}

// Whitespace is the White_Space property, not the runtime's idea of it: a figure space is trimmed.
func TestWhiteSpaceIsTrimmedAndBlank(t *testing.T) {
	if got, err := auth.NormalizeUserText(runes(0x2007, 'a', 0x3000), 190, false); err != nil || got != "a" {
		t.Errorf("trim: %q, %v", got, err)
	}
	if err := textErr(runes(0x2007, 0x2009, ' ')); !errors.Is(err, auth.ErrTextBlank) {
		t.Errorf("only spaces: %v, want ErrTextBlank", err)
	}
}

// A text of only marks and joiners shows nothing, so it is blank (a virama and a ZWNJ alone were stored).
func TestMarksAndJoinersAloneAreBlank(t *testing.T) {
	for name, in := range map[string]string{
		"virama and zwnj": runes(0xa4d, 0x200c), "virama and zwj": runes(0x94d, 0x200d), "mark, zwj, mark": runes(0x301, 0x200d, 0x302),
		"virama, space and zwnj": runes(0x94d, ' ', 0x200c),
	} {
		if err := textErr(in); !errors.Is(err, auth.ErrTextBlank) && !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want it refused", name, err)
		}
	}
	if err := textErr(runes(0xa4d, 0x200c)); !errors.Is(err, auth.ErrTextBlank) {
		t.Errorf("virama and zwnj: %v, want ErrTextBlank", err)
	}
	if _, err := auth.NormalizeDisplay(runes(0x94d, 0x200d)); err == nil {
		t.Error("a display name of a virama and a joiner was accepted")
	}
	accepted(t, "a letter with a virama and a joiner", 0x915, 0x94d, 0x200d)
}

// A joiner (or any other Default_Ignorable) between marks does not start a new count: the cap is per base character.
// The marks are U+0941 (Mn, combining class 0): NFC cannot reorder them, so the virama stays next to the joiner.
func TestTheMarkCapSurvivesJoinersAndIgnorables(t *testing.T) {
	seven := strings.Repeat(string(rune(0x941)), 7)
	eight := strings.Repeat(string(rune(0x941)), 8)
	// a base, 7 marks and a virama (8 marks), a ZWNJ after the virama, 8 more marks
	if err := textErr(runes(0x915) + seven + runes(0x94d, 0x200c) + eight); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("marks, virama, ZWNJ, marks: %v, want ErrTextChars", err)
	}
	if err := textErr(runes(0x915) + seven + runes(0x94d, 0x200d) + eight); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("marks, virama, ZWJ, marks: %v, want ErrTextChars", err)
	}
	// an emoji selector between marks is transparent as well
	eight301 := strings.Repeat(string(rune(0x301)), 8)
	if err := textErr(runes(0x1f642, 0xfe0f) + eight301 + eight301); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("emoji, selector, 16 marks: %v, want ErrTextChars", err)
	}
	// a new base does start a new count
	if err := textErr(runes(0x915) + eight + "b" + eight + "c" + eight); err != nil {
		t.Errorf("three bases with 8 marks: %v, want accepted", err)
	}
	// real text with joiners is not affected
	accepted(t, "sinhala shri", 0xdc1, 0xdca, 0x200d, 0xdbb, 0xdd3)
	accepted(t, "persian", 0x645, 0x6cc, 0x200c, 0x62e, 0x648, 0x627, 0x647, 0x645)
}
