//go:build linux && cgo

package frontend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jonasthim/dilla/internal/desktopsandbox"
)

var manualVersion = regexp.MustCompile(`\A(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-beta\.(0|[1-9][0-9]*))?\z`)

func checkedCacheUID(value int64) (uint32, bool) {
	if value < 0 || value > math.MaxUint32 {
		return 0, false
	}
	return uint32(value), true
}

func manualTransport(base http.RoundTripper) (*http.Transport, error) {
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return nil, ErrSignedSidecarRequired
	}
	result := transport.Clone()
	result.Proxy = nil
	return result, nil
}

func allowedManualURL(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Hostname() == "github.com" || u.Hostname() == "release-assets.githubusercontent.com" || u.Hostname() == "objects.githubusercontent.com" || u.Hostname() == "github-releases.githubusercontent.com")
}
func fetchManualSignature(version string, e EmbeddedCapsule, transport http.RoundTripper) (EmbeddedCapsule, []byte, error) {
	if !manualVersion.MatchString(version) || !currentCapsule(e, time.Now()) {
		return EmbeddedCapsule{}, nil, ErrSignedSidecarRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || !allowedManualURL(req.URL) {
			return ErrSignedSidecarRequired
		}
		return nil
	}}
	base := "https://github.com/jonasthim/dilla/releases/download/desktop-v" + version + "/dilla-" + version + "-linux-x64.sandbox"
	get := func(suffix string, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+suffix, nil)
		if err != nil || !allowedManualURL(req.URL) {
			return nil, ErrSignedSidecarRequired
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return nil, ErrSignedSidecarRequired
		}
		b, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(b)) > limit {
			return nil, ErrSignedSidecarRequired
		}
		return b, nil
	}
	manifest, err := get(".json", 65536)
	if err != nil {
		return EmbeddedCapsule{}, nil, err
	}
	parsed, err := desktopsandbox.ParseCapsule(bytes.NewReader(e.unsigned), desktopsandbox.Limits{})
	if err != nil || !bytes.Equal(parsed.Manifest, manifest) {
		return EmbeddedCapsule{}, nil, ErrSignedSidecarRequired
	}
	raw, err := get(".sig", 89)
	if err != nil {
		return EmbeddedCapsule{}, nil, err
	}
	if len(raw) == 89 && raw[88] == '\n' {
		raw = raw[:88]
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(string(raw))
	if err != nil || len(signature) != 64 || base64.StdEncoding.EncodeToString(signature) != string(raw) {
		return EmbeddedCapsule{}, nil, ErrSignedSidecarRequired
	}
	signed, err := e.WithSignature(signature)
	return signed, signature, err
}

func writeCachedSignature(root string, e EmbeddedCapsule, signature []byte) error {
	if !filepath.IsAbs(root) || len(signature) != 64 {
		return ErrSignedSidecarRequired
	}
	if _, err := e.WithSignature(signature); err != nil || !currentCapsule(e, time.Now()) {
		return ErrSignedSidecarRequired
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	uid, ok := checkedCacheUID(int64(os.Getuid()))
	if !ok {
		return ErrSignedSidecarRequired
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range []string{"dilla", "sandbox-signatures"} {
		if err := unix.Mkdirat(fd, part, 0700); err != nil && err != unix.EEXIST {
			return err
		}
		if err := unix.Fsync(fd); err != nil {
			return err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if unix.Fstat(next, &stat) != nil || stat.Uid != uid || stat.Mode&0077 != 0 {
			_ = unix.Close(next)
			return ErrSignedSidecarRequired
		}
		_ = unix.Close(fd)
		fd = next
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(e.constants.ManifestSHA512)
	if err != nil || len(digest) != 64 {
		return ErrSignedSidecarRequired
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".new-" + hex.EncodeToString(nonce[:])
	defer func() { _ = unix.Unlinkat(fd, temp, 0) }()
	fileFD, err := unix.Openat(fd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fileFD), "cached signature")
	written, err := file.Write(signature)
	if err == nil && written != len(signature) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = unix.Renameat(fd, temp, fd, hex.EncodeToString(digest)); err != nil {
		return err
	}
	return unix.Fsync(fd)
}

func manualSignedCapsule(version string, e EmbeddedCapsule) (EmbeddedCapsule, error) {
	root, err := os.UserCacheDir()
	if err != nil || !filepath.IsAbs(root) {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	if signed, err := cachedSignedCapsule(root, e); err == nil {
		return signed, nil
	}
	transport, err := manualTransport(http.DefaultTransport)
	if err != nil {
		return EmbeddedCapsule{}, err
	}
	signed, signature, err := fetchManualSignature(version, e, transport)
	if err != nil {
		return EmbeddedCapsule{}, err
	}
	if err = writeCachedSignature(root, e, signature); err != nil {
		return EmbeddedCapsule{}, err
	}
	return signed, nil
}

func currentCapsule(e EmbeddedCapsule, now time.Time) bool {
	published, p := time.Parse(time.RFC3339Nano, e.constants.PublishedAt)
	expires, x := time.Parse(time.RFC3339Nano, e.constants.ExpiresAt)
	return p == nil && x == nil && !published.After(now.Add(5*time.Minute)) && expires.After(now)
}

func cachedSignedCapsule(root string, e EmbeddedCapsule) (EmbeddedCapsule, error) {
	if !filepath.IsAbs(root) || !currentCapsule(e, time.Now()) {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(e.constants.ManifestSHA512)
	if err != nil || len(digest) != 64 {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	uid, ok := checkedCacheUID(int64(os.Getuid()))
	if !ok {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range []string{"dilla", "sandbox-signatures"} {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return EmbeddedCapsule{}, ErrSignedSidecarRequired
		}
		var stat unix.Stat_t
		if unix.Fstat(next, &stat) != nil || stat.Uid != uid || stat.Mode&0077 != 0 {
			_ = unix.Close(next)
			return EmbeddedCapsule{}, ErrSignedSidecarRequired
		}
		_ = unix.Close(fd)
		fd = next
	}
	signatureFD, err := unix.Openat(fd, hex.EncodeToString(digest), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	file := os.NewFile(uintptr(signatureFD), "cached signature")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if unix.Fstat(signatureFD, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uid || stat.Mode&0077 != 0 || stat.Nlink != 1 || stat.Size != 64 {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	signature, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil || len(signature) != 64 {
		return EmbeddedCapsule{}, ErrSignedSidecarRequired
	}
	return e.WithSignature(signature)
}
