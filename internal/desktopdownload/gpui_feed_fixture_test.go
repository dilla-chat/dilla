package desktopdownload_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/desktopdownload"
)

// The GPUI feed cross-test (GPUI release plan, Task 9): testdata/gpui-feed holds a feed that
// scripts/desktop-gpui-feed.mjs signed under a THROWAWAY test key (throwaway-test-key.pem), never a
// release key. scripts/desktop-gpui-feed.test.mjs rebuilds the same bytes. This test checks the
// Node signature and the format-1 shape with crypto/ed25519 directly;
// TestGPUIFeedFixtureVerifiesOnlyUnderGPUIAlpha (gpui_feed_verify_test.go) runs the same
// fixture through desktopdownload.Verify (Task 11).

type gpuiFeedAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA512 string `json:"sha512"`
}

type gpuiFeedDocument struct {
	Format      int             `json:"format"`
	Channel     string          `json:"channel"`
	Version     string          `json:"version"`
	PublishedAt string          `json:"publishedAt"`
	ExpiresAt   string          `json:"expiresAt"`
	Assets      []gpuiFeedAsset `json:"assets"`
}

var gpuiFeedSuffixes = []string{
	"linux-x64.AppImage",
	"linux-x64.tar.gz",
	"linux-x64.deb",
	"linux-x64.rpm",
	"linux-x64.aur.tar.gz",
	"mac-arm64.dmg",
	"mac-arm64.zip",
	"win-x64-setup.exe",
}

func readGPUIFeedFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gpui-feed", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGPUIFeedFixtureVerifiesUnderTheThrowawayKey(t *testing.T) {
	feed := readGPUIFeedFixture(t, "desktop-gpui-alpha.json")
	sigFile := readGPUIFeedFixture(t, "desktop-gpui-alpha.json.sig")
	keys, err := desktopdownload.ParseKeys([]string{string(readGPUIFeedFixture(t, "throwaway-test-key.pub.pem"))})
	if err != nil {
		t.Fatal(err)
	}

	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSuffix(sigFile, []byte("\n"))))
	if err != nil || len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature is not base64 of 64 bytes: %v", err)
	}
	if !ed25519.Verify(keys[0], feed, sig) {
		t.Fatal("the Node-signed feed does not verify under the throwaway key")
	}
	for _, pinned := range desktopdownload.PinnedKeys() {
		if pinned.Equal(keys[0]) || ed25519.Verify(pinned, feed, sig) {
			t.Fatal("the fixture must never be signed by a release key")
		}
	}
	tampered := bytes.Replace(feed, []byte(`"alpha"`), []byte(`"beta"`), 1)
	if ed25519.Verify(keys[0], tampered, sig) {
		t.Fatal("a tampered feed verifies")
	}

	var doc gpuiFeedDocument
	dec := json.NewDecoder(bytes.NewReader(feed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	// Canonical bytes: JSON.stringify(doc) + "\n" (Go's encoder adds the newline and keeps field order).
	var canonical bytes.Buffer
	enc := json.NewEncoder(&canonical)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical.Bytes(), feed) {
		t.Fatalf("feed is not canonical:\n%s\n%s", canonical.Bytes(), feed)
	}

	if doc.Format != 1 || doc.Channel != "alpha" {
		t.Fatalf("format %d channel %q, want 1 alpha", doc.Format, doc.Channel)
	}
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-alpha\.(0|[1-9][0-9]*)$`).MatchString(doc.Version) {
		t.Fatalf("version %q is not an alpha release", doc.Version)
	}
	published, err := time.Parse(time.RFC3339Nano, doc.PublishedAt)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339Nano, doc.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if expires.Sub(published) != 90*24*time.Hour {
		t.Fatalf("validity %v, want 90 days", expires.Sub(published))
	}

	if len(doc.Assets) != len(gpuiFeedSuffixes) {
		t.Fatalf("%d assets, want %d", len(doc.Assets), len(gpuiFeedSuffixes))
	}
	for i, asset := range doc.Assets {
		name := "dilla-" + doc.Version + "-" + gpuiFeedSuffixes[i]
		if asset.Name != name {
			t.Fatalf("asset %d is %q, want %q", i, asset.Name, name)
		}
		// The Node test writes each fixture asset as "fixture asset <name>\n".
		content := []byte("fixture asset " + name + "\n")
		sum := sha512.Sum512(content)
		if asset.Size != int64(len(content)) || asset.SHA512 != base64.StdEncoding.EncodeToString(sum[:]) {
			t.Fatalf("asset %s: size %d sha512 %s do not match its content", name, asset.Size, asset.SHA512)
		}
	}
}
