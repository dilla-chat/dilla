package auth_test

import (
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
)

func chars(t *testing.T, name string, cps ...rune) {
	t.Helper()
	if err := textErr(runes(cps...)); !errors.Is(err, auth.ErrTextChars) {
		t.Errorf("%s: %v, want ErrTextChars", name, err)
	}
}

func accepted(t *testing.T, name string, cps ...rune) {
	t.Helper()
	if err := textErr(runes(cps...)); err != nil {
		t.Errorf("%s: %v, want accepted", name, err)
	}
}

// Only the RGI_Emoji_Tag_Sequence list (England, Scotland, Wales) may carry tag characters: any other run, however
// well-formed, hides text.
func TestOnlyAnRGITagSequenceMayCarryTags(t *testing.T) {
	accepted(t, "England", 0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f)
	accepted(t, "Scotland", 0x1f3f4, 0xe0067, 0xe0062, 0xe0073, 0xe0063, 0xe0074, 0xe007f)
	accepted(t, "Wales", 0x1f3f4, 0xe0067, 0xe0062, 0xe0077, 0xe006c, 0xe0073, 0xe007f)
	// a well-formed subdivision sequence that is not RGI (gbenh), and a hidden word in a flag
	chars(t, "unlisted subdivision", 0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0068, 0xe007f)
	chars(t, "hidden word", 0x1f3f4, 0xe0068, 0xe0069, 0xe0064, 0xe0064, 0xe0065, 0xe006e, 0xe007f)
	chars(t, "long run of valid tags", 0x1f3f4, 0xe0061, 0xe0062, 0xe0063, 0xe0064, 0xe0065, 0xe0066, 0xe0067, 0xe0068, 0xe007f)
	chars(t, "one tag", 0x1f3f4, 0xe0067, 0xe007f)
	chars(t, "England without its cancel tag", 0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067)
	chars(t, "England and a trailing tag", 0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f, 0xe0041)
	chars(t, "tags after another emoji", 0x1f642, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f)
	accepted(t, "a black flag then England", 0x1f3f4, 0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f)
}

// A variation selector: only U+FE0E or U+FE0F, one per base, right after an Emoji code point. Every other one is
// refused, so two names cannot look the same by stacking invisible selectors.
func TestVariationSelectorsAreRefusedExceptOnEmoji(t *testing.T) {
	accepted(t, "face with a text selector", 0x1f642, 0xfe0e)
	accepted(t, "face with an emoji selector", 0x1f642, 0xfe0f)
	accepted(t, "copyright with an emoji selector", 0xa9, 0xfe0f)
	accepted(t, "keycap", '1', 0xfe0f, 0x20e3)
	accepted(t, "heart on fire", 0x2764, 0xfe0f, 0x200d, 0x1f525)
	accepted(t, "head shaking horizontally", 0x1f642, 0x200d, 0x2194, 0xfe0f)
	chars(t, "emoji selector on a letter", 'a', 0xfe0f)
	chars(t, "text selector on a letter", 'a', 0xfe0e)
	chars(t, "a selector alone", 0xfe0f)
	chars(t, "a leading selector", 0xfe0f, 0x1f642)
	chars(t, "two selectors on one base", 0x1f642, 0xfe0f, 0xfe0f)
	chars(t, "stacked mixed selectors", 0x1f642, 0xfe0e, 0xfe0f)
	for vs := rune(0xfe00); vs <= 0xfe0d; vs++ {
		chars(t, "a CJK or math selector on an emoji", 0x1f642, vs)
		chars(t, "a CJK or math selector on a letter", 0x6c49, vs)
	}
	chars(t, "an ideographic variation selector", 0x6c49, 0xe0100)
	chars(t, "the last ideographic variation selector", 0x6c49, 0xe01ef)
	chars(t, "an ideographic selector on an emoji", 0x1f642, 0xe0100)
	chars(t, "stacked ideographic selectors", 0x6c49, 0xe0100, 0xe0101, 0xe0102)
	chars(t, "a selector after a space", ' ', 0xfe0f)
}

// A selector never makes a text visible: a base that is only invisible plus selectors stays refused.
func TestSelectorsDoNotMakeATextVisible(t *testing.T) {
	chars(t, "mark and selector", 0x301, 0xfe0f)
	chars(t, "spaces and selectors", ' ', 0xfe0e, ' ')
}
