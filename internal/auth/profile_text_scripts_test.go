package auth_test

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

// runes builds a string from code points: the invisible and joining characters are written as numbers.
func runes(cps ...rune) string { return string(cps) }

func textErr(s string) error {
	_, err := auth.NormalizeUserText(s, 190, false)
	return err
}

// RFC 5892 Appendix A.1 and A.2 (CONTEXTJ): Persian, Kurdish and Sinhala text keeps its joiners.
func TestJoinersKeepPersianAndSinhalaWorking(t *testing.T) {
	const zwnj, zwj = 0x200c, 0x200d
	accepted := map[string]string{
		"persian mikhaham":        runes(0x645, 0x6cc, zwnj, 0x62e, 0x648, 0x627, 0x647, 0x645),
		"persian mohammadi nejad": runes(0x645, 0x62d, 0x645, 0x62f, 0x6cc, zwnj, 0x646, 0x698, 0x627, 0x62f),
		"persian ketabha":         runes(0x6a9, 0x62a, 0x627, 0x628, zwnj, 0x647, 0x627),
		"sinhala shri":            runes(0xdc1, 0xdca, zwj, 0xdbb, 0xdd3),
		"devanagari zwnj":         runes(0x915, 0x94d, zwnj, 0x937),
		"devanagari zwj":          runes(0x915, 0x94d, zwj, 0x937),
		"malayalam chillu by zwj": runes(0xd23, 0xd4d, zwj),
		"zwnj across a mark":      runes(0x628, 0x64e, zwnj, 0x647),
	}
	for name, in := range accepted {
		if err := textErr(in); err != nil {
			t.Errorf("%s: %v, want accepted", name, err)
		}
		if _, err := auth.NormalizeDisplay(in); err != nil {
			t.Errorf("%s as a display name: %v, want accepted", name, err)
		}
	}
	refused := map[string]string{
		"leading zwnj":                            runes(zwnj, 0x645),
		"trailing zwnj":                           runes(0x645, zwnj),
		"bare zwnj between latin":                 runes('a', zwnj, 'b'),
		"zwnj after a right joiner":               runes(0x627, zwnj, 0x628),
		"zwnj before a letter that does not join": runes(0x628, zwnj, 0x621),
		"doubled zwnj":                            runes(0x628, zwnj, zwnj, 0x647),
		"leading zwj":                             runes(zwj, 0x645),
		"zwj between persian letters":             runes(0x645, zwj, 0x6cc),
		"zwj after a latin letter":                runes('a', zwj, 'b'),
		"zwj after a virama at the end":           runes('a', 0x94d, zwj, zwj, 'b'),
	}
	for name, in := range refused {
		if err := textErr(in); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want ErrTextChars", name, err)
		}
	}
}

// Emoji joiners rest on Extended_Pictographic, not on any symbol.
func TestEmojiJoinersNeedPictographs(t *testing.T) {
	const zwj = 0x200d
	if err := textErr(runes(0xa9, zwj, 0xa9)); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("copyright joined to copyright: %v, want ErrTextChars", err)
	}
	if err := textErr(runes('^', zwj, 0xa9)); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("caret joined to copyright: %v, want ErrTextChars", err)
	}
	if err := textErr(runes(0x2800, zwj, 0x2801)); err == nil {
		t.Error("braille patterns joined: accepted")
	}
	// head shaking horizontally: U+1F642 ZWJ U+2194 U+FE0F
	if err := textErr(runes(0x1f642, zwj, 0x2194, 0xfe0f)); err != nil {
		t.Errorf("head shaking horizontally: %v", err)
	}
	if err := textErr(runes(0x1f468, zwj, 0x1f469, zwj, 0x1f467)); err != nil {
		t.Errorf("family: %v", err)
	}
	if err := textErr(runes(0x1f642, zwj)); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("trailing joiner: %v", err)
	}
	if err := textErr(runes(0x1f642, zwj, zwj, 0x1f642)); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("doubled joiner: %v", err)
	}
}

// Every RGI emoji ZWJ sequence of the Unicode version the tables come from is accepted.
func TestEveryRGIEmojiZWJSequenceIsAccepted(t *testing.T) {
	f, err := os.Open("testdata/rgi-zwj-sequences.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var cps []rune
		for _, h := range strings.Fields(line) {
			v, err := strconv.ParseInt(h, 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			cps = append(cps, rune(v))
		}
		n++
		if err := textErr(runes(cps...)); err != nil {
			t.Errorf("%s: %v", line, err)
		}
	}
	if n < 1000 {
		t.Fatalf("read only %d sequences", n)
	}
}

// Tag characters belong only to a flag sequence after U+1F3F4; the other invisible characters are refused.
func TestInvisibleCharactersAreRefused(t *testing.T) {
	england := runes(0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f)
	if err := textErr(england); err != nil {
		t.Errorf("the England flag: %v", err)
	}
	refused := map[string]string{
		"bare tag":               runes('a', 0xe0041, 'b'),
		"tag cancel alone":       runes(0xe007f),
		"tags after a letter":    runes('a', 0xe0067, 0xe007f),
		"unterminated flag":      runes(0x1f3f4, 0xe0067, 0xe0062),
		"hidden ascii":           runes(0x1f642, 0xe0068, 0xe0069, 0xe007f),
		"soft hyphen":            runes('a', 0xad, 'b'),
		"combining grapheme":     runes('a', 0x34f, 'b'),
		"hangul choseong":        runes('a', 0x115f, 'b'),
		"hangul jungseong":       runes('a', 0x1160, 'b'),
		"hangul filler":          runes('a', 0x3164, 'b'),
		"halfwidth hangul":       runes('a', 0xffa0, 'b'),
		"braille blank":          runes('a', 0x2800, 'b'),
		"function application":   runes('a', 0x2061, 'b'),
		"invisible plus":         runes('a', 0x2064, 'b'),
		"mongolian vowel sep":    runes('a', 0x180e, 'b'),
		"deprecated format":      runes('a', 0x206a, 'b'),
		"interlinear annotation": runes('a', 0xfff9, 'b'),
	}
	for name, in := range refused {
		if err := textErr(in); !errors.Is(err, auth.ErrTextChars) {
			t.Errorf("%s: %v, want ErrTextChars", name, err)
		}
	}
}

// A text of nothing visible is blank: only spaces, joiners-free marks or variation selectors.
func TestAnInvisibleOnlyTextIsBlank(t *testing.T) {
	for name, in := range map[string]string{
		"a combining mark alone": runes(0x301), "marks and spaces": runes(' ', 0x301, ' ', 0x302),
	} {
		if err := textErr(in); !errors.Is(err, auth.ErrTextBlank) {
			t.Errorf("%s: %v, want ErrTextBlank", name, err)
		}
	}
	if err := textErr(runes('e', 0x301)); err != nil {
		t.Errorf("a letter with a mark: %v", err)
	}
}

// The display name is trimmed and may not be only whitespace (it may be empty: the username shows).
func TestNormalizeDisplayTrimsAndRefusesBlank(t *testing.T) {
	if got, err := auth.NormalizeDisplay("  Ada  "); err != nil || got != "Ada" {
		t.Fatalf("trimmed: %q, %v", got, err)
	}
	if got, err := auth.NormalizeDisplay(""); err != nil || got != "" {
		t.Fatalf("empty: %q, %v", got, err)
	}
	if _, err := auth.NormalizeDisplay("   "); err == nil {
		t.Fatal("a whitespace-only display name was accepted")
	}
	if _, err := auth.NormalizeDisplay(strings.Repeat("a", 65)); !errors.Is(err, auth.ErrDisplayLength) {
		t.Fatalf("too long: %v", err)
	}
}
