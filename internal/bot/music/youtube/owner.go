package youtube

import (
	"context"
	"unicode"
	"unicode/utf8"
)

const maxOwnerBytes = 128

// requireOwner makes every lookup, pre-buffer and start refuse a context that
// names no owner. It is true in every shipped build; only a dilla_testhost
// build (fixture_testhost.go) clears it, so the cache and the buffers can never
// be shared across communities by forgetting to scope them.
var requireOwner = true

// validOwner accepts non-empty, printable text without NUL or other controls.
func validOwner(owner string) bool {
	if owner == "" || len(owner) > maxOwnerBytes || !utf8.ValidString(owner) {
		return false
	}
	for _, r := range owner {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ownerFrom returns the owner ctx carries. An invalid owner is always refused;
// an empty one is refused unless this is a test-host build.
func ownerFrom(ctx context.Context) (string, error) {
	owner := ownerOf(ctx)
	if owner == "" && !requireOwner {
		return "", nil
	}
	if !validOwner(owner) {
		return "", ErrInvalidInput
	}
	return owner, nil
}
