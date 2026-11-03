//go:build linux

package desktopsandbox

import (
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// Status is the fixed bounded public protocol. It conveys no pathname authority.
type Status struct {
	Format             int    `json:"format"`
	Status             string `json:"status"`
	Generation         int64  `json:"generation"`
	CompatibilityEpoch int64  `json:"compatibilityEpoch"`
	SandboxAPI         int64  `json:"sandboxApi"`
	MaintainerSHA512   string `json:"maintainerSha512"`
	HelperSHA512       string `json:"helperSha512"`
}

func checkedTrust(t Trust) (Trust, error) {
	if !validKeys(t.PublicKeys) || t.Initial == nil || t.Initial.Generation < 1 || t.Initial.Generation > MaxSafeInteger || t.Initial.CompatibilityEpoch != 1 || t.Initial.SandboxAPI != 1 || t.Initial.Helper.Size < 1 || t.Initial.Helper.Size > MaxHelper || !validDigest(t.Initial.Helper.SHA512) {
		return Trust{}, ErrAuthentication
	}
	i := *t.Initial
	if _, _, e := manifestTimes(Manifest{PublishedAt: i.PublishedAt, ExpiresAt: i.ExpiresAt}); e != nil {
		return Trust{}, e
	}
	keys := make([]ed25519.PublicKey, len(t.PublicKeys))
	for n, k := range t.PublicKeys {
		keys[n] = append(ed25519.PublicKey(nil), k...)
	}
	return Trust{PublicKeys: keys, Initial: &i}, nil
}

// ProtectedSelfSHA512 measures the kernel's executable FD, never a mutable
// executable pathname. This is called before any transaction can unlink it.
func ProtectedSelfSHA512() (string, error) {
	fd, e := unix.Open("/proc/self/exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if e != nil {
		return "", e
	}
	defer func() { _ = unix.Close(fd) }()
	var before unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Uid != 0 || before.Gid != 0 || before.Nlink != 1 || before.Size < 1 || before.Size > MaxMaintainer || (before.Mode&0o7777 != 0o755 && before.Mode&0o7777 != 0o700) {
		return "", ErrState
	}
	f := fixedFiles{layout: appImageLayout()}
	if e = f.safeAttributes(fd); e != nil {
		return "", e
	}
	h := sha512.New()
	buffer := make([]byte, 65536)
	var offset int64
	for offset < before.Size {
		n, e := unix.Pread(fd, buffer, offset)
		if e != nil {
			return "", e
		}
		if n == 0 {
			return "", ErrState
		}
		_, _ = h.Write(buffer[:n])
		offset += int64(n)
	}
	var after unix.Stat_t
	if unix.Fstat(fd, &after) != nil || !sameStat(before, after) || offset != before.Size {
		return "", ErrState
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// OpenAppImageMaintainer uses only the compiled fixed root. FD3, if it is the
// exact validated lock inode, is adopted as the same open-file-description.
func OpenAppImageMaintainer(ctx context.Context, trust Trust, inheritedLock bool) (*Maintainer, error) {
	if unix.Geteuid() != 0 {
		return nil, ErrState
	}
	trust, e := checkedTrust(trust)
	if e != nil {
		return nil, e
	}
	self, e := ProtectedSelfSHA512()
	if e != nil {
		return nil, e
	}
	trust.Initial.SelfMaintainerSHA512 = self
	f, e := openFixedFiles(appImageLayout(), true)
	if e != nil {
		return nil, e
	}
	var fs unix.Statfs_t
	if unix.Fstatfs(f.rootFD, &fs) != nil || fs.Flags&unix.ST_NOSUID != 0 {
		_ = f.close()
		return nil, ErrState
	}
	if e = f.lock(ctx, inheritedLock); e != nil {
		_ = f.close()
		return nil, e
	}
	return &Maintainer{files: f, trust: trust, now: time.Now}, nil
}
func statusFor(m *Maintainer) Status {
	unsafe := Status{Format: 1, Status: "unsafe"}
	if m.files.inventory(rootDir) != nil {
		return unsafe
	}
	// Private staging is intentionally not needed by an ordinary user's check.
	// A public intent always wins over an otherwise valid older receipt.
	raw, e := m.files.read(rootDir, "sandbox-transaction.json", recordLimit, 0o644)
	if e != nil {
		return unsafe
	}
	if raw != nil {
		return Status{Format: 1, Status: "maintenance"}
	}
	raw, e = m.files.read(rootDir, "sandbox-receipt.json", recordLimit, 0o644)
	if e != nil {
		return unsafe
	}
	if raw == nil {
		return unsafe
	}
	r, e := ParseReceipt(raw)
	if e != nil || VerifyReceipt(r, m.trust, m.now()) != nil || m.current(r) != nil {
		return unsafe
	}
	check, e := m.files.read(rootDir, "sandbox-receipt.json", recordLimit, 0o644)
	if e != nil || hashBytes(check) != r.SHA512() {
		return unsafe
	}
	pending, e := m.files.read(rootDir, "sandbox-transaction.json", recordLimit, 0o644)
	if e != nil || pending != nil {
		return Status{Format: 1, Status: "maintenance"}
	}
	status := "ready"
	if r.State == "removed" {
		status = "removed"
	}
	return Status{Format: 1, Status: status, Generation: r.MaximumGeneration, CompatibilityEpoch: 1, SandboxAPI: 1, MaintainerSHA512: r.MaintainerSHA512, HelperSHA512: r.HelperSHA512}
}

// ReadAppImageStatus performs no writes, lock creation or private-stage reads.
func ReadAppImageStatus(trust Trust) Status {
	unsafe := Status{Format: 1, Status: "unsafe"}
	trust, e := checkedTrust(trust)
	if e != nil {
		return unsafe
	}
	f, e := openFixedFiles(appImageLayout(), false)
	if errors.Is(e, errAbsent) {
		return Status{Format: 1, Status: "missing"}
	}
	if e != nil {
		return unsafe
	}
	defer func() { _ = f.close() }()
	var fs unix.Statfs_t
	if unix.Fstatfs(f.rootFD, &fs) != nil || fs.Flags&unix.ST_NOSUID != 0 {
		return unsafe
	}
	return statusFor(&Maintainer{files: f, trust: trust, now: time.Now})
}
