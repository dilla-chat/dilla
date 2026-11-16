package desktopdownload_test

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/desktopdownload"
)

// The Node-signed fixture (testdata/gpui-feed, Task 9) verifies through desktopdownload.Verify
// under GPUIAlpha with the throwaway key injected, redirects every GPUIAlpha platform under
// desktop-gpui-v<version>/, and is refused under Beta and the zero Channel.
func TestGPUIFeedFixtureVerifiesOnlyUnderGPUIAlpha(t *testing.T) {
	feed := readGPUIFeedFixture(t, "desktop-gpui-alpha.json")
	sig := readGPUIFeedFixture(t, "desktop-gpui-alpha.json.sig")
	keys, err := desktopdownload.ParseKeys([]string{string(readGPUIFeedFixture(t, "throwaway-test-key.pub.pem"))})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) // inside the fixture's 90-day validity
	verified, err := desktopdownload.Verify(desktopdownload.GPUIAlpha, feed, sig, keys, at)
	if err != nil {
		t.Fatalf("GPUIAlpha refused the Node-signed feed: %v", err)
	}
	for platform, suffix := range desktopdownload.GPUIAlpha.Platforms {
		got, err := verified.AssetURL(platform)
		want := "https://github.com/dilla-chat/dilla/releases/download/desktop-gpui-v" + verified.Version + "/dilla-" + verified.Version + "-" + suffix
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", platform, got, err, want)
		}
	}
	if _, err := desktopdownload.Verify(desktopdownload.Beta, feed, sig, keys, at); err == nil {
		t.Fatal("Beta accepted the GPUI feed")
	}
	if _, err := desktopdownload.Verify(desktopdownload.Channel{}, feed, sig, keys, at); err == nil {
		t.Fatal("the zero Channel (Beta) accepted the GPUI feed")
	}
	// The release pins never verify it: it is signed under the throwaway key only.
	if _, err := desktopdownload.Verify(desktopdownload.GPUIAlpha, feed, sig, desktopdownload.PinnedKeys(), at); err == nil {
		t.Fatal("the fixture verified under the release keys")
	}
}
