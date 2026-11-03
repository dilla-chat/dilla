//go:build linux && cgo

package frontend

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManualCacheRejectsInvalidUIDAndUnexpectedTransport(t *testing.T) {
	if _, ok := checkedCacheUID(-1); ok {
		t.Fatal("accepted negative uid")
	}
	if _, ok := checkedCacheUID(int64(^uint32(0)) + 1); ok {
		t.Fatal("accepted uid outside uint32")
	}
	if uid, ok := checkedCacheUID(42); !ok || uid != 42 {
		t.Fatal("rejected valid uid")
	}
	if _, err := manualTransport(manualRoundTrip(func(*http.Request) (*http.Response, error) { return nil, nil })); err == nil {
		t.Fatal("accepted unexpected transport")
	}
}

func TestManualCachedSignatureIsBoundToCompiledCapsule(t *testing.T) {
	f := makeCapsule(t, 1)
	e, err := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	hash, err := base64.StdEncoding.DecodeString(e.constants.ManifestSHA512)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "dilla", "sandbox-signatures")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, hex.EncodeToString(hash))
	sig := ed25519.Sign(f.private, append([]byte("Dilla Linux sandbox capsule v1\x00"), f.capsule.Manifest...))
	if err = os.WriteFile(path, sig, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = cachedSignedCapsule(root, e); err != nil {
		t.Fatal(err)
	}
	sig[0] ^= 1
	if err = os.WriteFile(path, sig, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = cachedSignedCapsule(root, e); err == nil {
		t.Fatal("accepted wrong signature")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("/dev/null", path); err != nil {
		t.Fatal(err)
	}
	if _, err = cachedSignedCapsule(root, e); err == nil {
		t.Fatal("accepted symlink")
	}
	if currentCapsule(e, time.Now().Add(48*time.Hour)) {
		t.Fatal("expired capsule accepted for a new introduction")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	correct := ed25519.Sign(f.private, append([]byte("Dilla Linux sandbox capsule v1\x00"), f.capsule.Manifest...))
	if err = writeCachedSignature(root, e, correct); err != nil {
		t.Fatal(err)
	}
	if _, err = cachedSignedCapsule(root, e); err != nil {
		t.Fatal(err)
	}
}

type manualRoundTrip func(*http.Request) (*http.Response, error)

func (f manualRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManualFetchUsesCompiledVersionAndExactManifest(t *testing.T) {
	f := makeCapsule(t, 1)
	e, err := NewEmbeddedCapsule(f.raw, f.trust.PublicKeys)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(f.private, append([]byte("Dilla Linux sandbox capsule v1\x00"), f.capsule.Manifest...))
	transport := manualRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "github.com" || r.URL.Path != "/dilla-chat/dilla/releases/download/desktop-v1.2.3/dilla-1.2.3-linux-x64.sandbox.json" && r.URL.Path != "/dilla-chat/dilla/releases/download/desktop-v1.2.3/dilla-1.2.3-linux-x64.sandbox.sig" {
			t.Fatalf("wrong URL: %s", r.URL)
		}
		body := f.capsule.Manifest
		if r.URL.Path[len(r.URL.Path)-4:] == ".sig" {
			body = []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})
	if _, _, err = fetchManualSignature("1.2.3", e, transport); err != nil {
		t.Fatal(err)
	}
	if _, _, err = fetchManualSignature("1.2.3/evil", e, transport); err == nil {
		t.Fatal("accepted version path")
	}
}
