package service

import (
	"os"
	"testing"
)

// TestMain lets the package's tests send with no Bot-channel gate registered (most predate the gate); the gate's own
// tests register one, and TestUnregisteredCommunityPostsNowhere turns this off.
func TestMain(m *testing.M) {
	ungatedForTests = true
	os.Exit(m.Run())
}
