//go:build linux

package desktopsandbox

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const recordLimit = 128 << 10

func readPinned(p *pinnedFile) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	b := make([]byte, p.identity.Size)
	for off := 0; off < len(b); {
		n, e := unix.Pread(p.fd, b[off:], int64(off))
		if e != nil {
			return nil, e
		}
		if n == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		off += n
	}
	var after unix.Stat_t
	if unix.Fstat(p.fd, &after) != nil || !sameStat(p.info, after) {
		return nil, ErrState
	}
	return b, nil
}
func (f *fixedFiles) read(which directory, name string, limit int64, modes ...uint32) ([]byte, error) {
	p, e := f.pin(which, name, limit, modes...)
	if e != nil {
		return nil, e
	}
	defer closePinned(p)
	return readPinned(p)
}
func (f *fixedFiles) changeMode(p *pinnedFile, mode uint32, label string) error {
	if p == nil {
		return ErrState
	}
	var before unix.Stat_t
	if unix.Fstat(p.fd, &before) != nil || !sameStat(p.info, before) {
		return ErrState
	}
	if e := f.run("chmod:"+label, func() error { return unix.Fchmod(p.fd, mode) }); e != nil {
		return e
	}
	if e := unix.Fstat(p.fd, &p.info); e != nil {
		return e
	}
	return f.run("sync-file:"+label, func() error { return unix.Fsync(p.fd) })
}
func (f *fixedFiles) publish(current, stage string, old, next *pinnedFile) error {
	if old != nil {
		if e := f.exchange(current, stage, old, next); e != nil {
			return e
		}
	} else {
		if !allowedEntry(rootDir, current) || !allowedEntry(stageDir, stage) {
			return ErrState
		}
		if e := f.run("publish:"+current, func() error {
			if f.named(stageDir, stage, next) != nil {
				return ErrState
			}
			return unix.Renameat2(f.stageFD, stage, f.rootFD, current, unix.RENAME_NOREPLACE)
		}); e != nil {
			return e
		}
	}
	if e := f.sync(stageDir); e != nil {
		return e
	}
	return f.sync(rootDir)
}
func (f *fixedFiles) unlink(which directory, name string, p *pinnedFile) error {
	if !allowedEntry(which, name) || p == nil {
		return ErrState
	}
	if e := f.run("unlink:"+f.label(which)+":"+name, func() error {
		if f.named(which, name, p) != nil {
			return ErrState
		}
		return unix.Unlinkat(f.directory(which), name, 0)
	}); e != nil {
		return e
	}
	return f.sync(which)
}
func (f *fixedFiles) discard(which directory, name string, limit int64, modes ...uint32) error {
	p, e := f.pin(which, name, limit, modes...)
	if e != nil {
		return e
	}
	defer closePinned(p)
	if p == nil {
		return nil
	}
	return f.unlink(which, name, p)
}
func (f *fixedFiles) writeReceipt(r Receipt) (*pinnedFile, error) {
	raw, e := json.Marshal(r)
	if e != nil || len(raw) > recordLimit {
		return nil, ErrState
	}
	if _, e = ParseReceipt(raw); e != nil {
		return nil, e
	}
	data := make([]byte, recordLimit)
	copy(data, raw)
	for i := len(raw); i < len(data); i++ {
		data[i] = ' '
	}
	p, e := f.pin(stageDir, "receipt.next", recordLimit, 0o644)
	if e != nil {
		return nil, e
	}
	if p == nil {
		return f.writeNew(stageDir, "receipt.next", data, 0o644)
	}
	defer closePinned(p)
	// Keep the reserved allocation, exact inode and length. IO faults remain a
	// forward-repair hold; they can never authorize restoring an old SUID inode.
	if p.identity.Size != recordLimit {
		return nil, ErrState
	}
	fd, e := unix.Openat(f.stageFD, "receipt.next", unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	var s unix.Stat_t
	if unix.Fstat(fd, &s) != nil || !sameStat(p.info, s) || f.named(stageDir, "receipt.next", p) != nil {
		return nil, ErrState
	}
	for off := 0; off < len(data); {
		n := 0
		e = f.run("write:stage:receipt.next", func() error { var err error; n, err = unix.Pwrite(fd, data[off:], int64(off)); return err })
		if e != nil {
			return nil, e
		}
		if n <= 0 {
			return nil, io.ErrShortWrite
		}
		off += n
	}
	if e = f.run("sync-file:receipt.next", func() error { return unix.Fsync(fd) }); e != nil {
		return nil, e
	}
	if e = unix.Close(fd); e != nil {
		return nil, e
	}
	fd = -1
	return f.pin(stageDir, "receipt.next", recordLimit, 0o644)
}
func identityEqual(a, b FileIdentity) bool { return a == b }
func requireIdentity(p *pinnedFile, id FileIdentity) error {
	if p == nil || !identityEqual(p.identity, id) {
		return ErrState
	}
	return nil
}

// inventory opens a fresh descriptor (not dup, which would share a directory
// offset) and caps entries before considering any mutation. Native package
// siblings live outside this AppImage-owned leaf and are never enumerated.
func (f *fixedFiles) inventory(which directory) error {
	if f.directory(which) < 0 {
		return nil
	}
	fd, e := unix.Openat(f.directory(which), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return e
	}
	file := os.NewFile(uintptr(fd), "fixed sandbox directory")
	defer func() { _ = file.Close() }()
	names, e := file.Readdirnames(33)
	if e != nil && !errors.Is(e, io.EOF) {
		return e
	}
	if len(names) > 32 {
		return ErrState
	}
	for _, name := range names {
		if which == rootDir && name == ".sandbox-staging" {
			continue
		}
		if !allowedEntry(which, name) {
			return ErrState
		}
	}
	return nil
}

// Closing a read-only pin cannot affect previously checked write durability.
func closePinned(p *pinnedFile) { _ = p.close() }
