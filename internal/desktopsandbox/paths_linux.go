//go:build linux

package desktopsandbox

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

type fileLayout struct {
	anchor     string
	parts      []string
	uid, gid   uint32
	helperMode uint32
}

func (l fileLayout) root() string { return filepath.Join(append([]string{l.anchor}, l.parts...)...) }
func appImageLayout() fileLayout {
	return fileLayout{anchor: "/", parts: []string{"usr", "lib", "dilla", "appimage"}, uid: 0, gid: 0, helperMode: 0o4755}
}

type directory int

const (
	rootDir directory = iota
	stageDir
)

var errAbsent = errors.New("sandbox namespace entry absent")

type pinnedFile struct {
	fd       int
	identity FileIdentity
	info     unix.Stat_t
}

func (p *pinnedFile) close() error {
	if p == nil || p.fd < 0 {
		return nil
	}
	fd := p.fd
	p.fd = -1
	return unix.Close(fd)
}

type fixedFiles struct {
	layout                  fileLayout
	rootFD, stageFD, lockFD int
	device                  uint64
	mountID                 uint64
	fault                   func(string, bool) error
}

func (f *fixedFiles) run(op string, fn func() error) error {
	if f.fault != nil {
		if err := f.fault(op, false); err != nil {
			return err
		}
	}
	if err := fn(); err != nil {
		return err
	}
	if f.fault != nil {
		return f.fault(op, true)
	}
	return nil
}
func (f *fixedFiles) directory(which directory) int {
	if which == stageDir {
		return f.stageFD
	}
	return f.rootFD
}
func (f *fixedFiles) label(which directory) string {
	if which == stageDir {
		return "stage"
	}
	return "root"
}
func allowedEntry(which directory, name string) bool {
	if which == rootDir {
		switch name {
		case "sandbox-maintainer", "chrome-sandbox", "sandbox-receipt.json", "sandbox-transaction.json", ".sandbox-lock":
			return true
		}
		return false
	}
	switch name {
	case "bootstrap-anchor.json", "bootstrap-anchor.next", "bootstrap-capsule", "bootstrap-maintainer", "maintainer.next", "helper.next", "receipt.next", "intent.next":
		return true
	}
	return false
}
func openFixedFiles(layout fileLayout, write bool) (result *fixedFiles, err error) {
	f := &fixedFiles{layout: layout, rootFD: -1, stageFD: -1, lockFD: -1}
	defer func() {
		if err != nil {
			_ = f.close()
		}
	}()
	fd, err := unix.Open(layout.anchor, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f.rootFD = fd
	if err = f.captureMount(fd); err != nil {
		return nil, err
	}
	if err = f.safeDir(fd, 0, false); err != nil {
		return nil, err
	}
	for index, name := range layout.parts {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return nil, ErrState
		}
		next, e := unix.Openat(f.rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(e, unix.ENOENT) {
			return nil, errAbsent
		}
		if e != nil {
			return nil, e
		}
		required := uint32(0)
		if index >= len(layout.parts)-2 {
			required = 0o755
		}
		if e = f.sameDevice(next); e != nil {
			_ = unix.Close(next)
			return nil, e
		}
		if e = f.safeDir(next, required, false); e != nil {
			_ = unix.Close(next)
			return nil, e
		}
		_ = unix.Close(f.rootFD)
		f.rootFD = next
	}
	var info unix.Stat_t
	if err = unix.Fstat(f.rootFD, &info); err != nil {
		return nil, err
	}
	f.device = info.Dev
	stage, err := unix.Openat(f.rootFD, ".sandbox-staging", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) && write {
		if err = unix.Mkdirat(f.rootFD, ".sandbox-staging", 0o700); err != nil {
			return nil, err
		}
		stage, err = unix.Openat(f.rootFD, ".sandbox-staging", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == nil {
			err = unix.Fchown(stage, int(layout.uid), int(layout.gid))
		}
		if err == nil {
			err = unix.Fchmod(stage, 0o700)
		}
		if err == nil {
			err = unix.Fsync(stage)
		}
		if err == nil {
			err = unix.Fsync(f.rootFD)
		}
	}
	if !write && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EACCES)) {
		return f, nil
	}
	if err != nil {
		if stage >= 0 {
			_ = unix.Close(stage)
		}
		return nil, err
	}
	f.stageFD = stage
	if err = f.safeDir(stage, 0o700, true); err != nil {
		return nil, err
	}
	if err = f.sameDevice(stage); err != nil {
		return nil, err
	}
	return f, nil
}
func (f *fixedFiles) close() error {
	var first error
	for _, fd := range []int{f.stageFD, f.rootFD, f.lockFD} {
		if fd >= 0 {
			if e := unix.Close(fd); e != nil && first == nil {
				first = e
			}
		}
	}
	f.rootFD = -1
	f.stageFD = -1
	f.lockFD = -1
	return first
}
func (f *fixedFiles) safeDir(fd int, mode uint32, private bool) error {
	var s unix.Stat_t
	if unix.Fstat(fd, &s) != nil || s.Mode&unix.S_IFMT != unix.S_IFDIR || s.Mode&0o6022 != 0 || (s.Uid != 0 && s.Uid != f.layout.uid) {
		return ErrState
	}
	if mode != 0 && (s.Mode&0o7777 != mode || s.Uid != f.layout.uid || s.Gid != f.layout.gid) {
		return ErrState
	}
	if private && s.Gid != f.layout.gid {
		return ErrState
	}
	return f.safeAttributes(fd)
}
func mountInfo(fd int) (uint64, uint64, error) {
	var s unix.Stat_t
	var x unix.Statx_t
	if unix.Fstat(fd, &s) != nil {
		return 0, 0, ErrState
	}
	if e := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &x); e != nil {
		return 0, 0, e
	}
	if x.Mask&unix.STATX_MNT_ID == 0 || x.Mnt_id == 0 {
		return 0, 0, ErrState
	}
	return s.Dev, x.Mnt_id, nil
}
func sameMount(device, mount, otherDevice, otherMount uint64) error {
	if mount == 0 || otherMount == 0 || device != otherDevice || mount != otherMount {
		return ErrState
	}
	return nil
}
func (f *fixedFiles) captureMount(fd int) error {
	d, m, e := mountInfo(fd)
	if e != nil {
		return e
	}
	f.device = d
	f.mountID = m
	return nil
}
func (f *fixedFiles) sameDevice(fd int) error {
	d, m, e := mountInfo(fd)
	if e != nil {
		return e
	}
	return sameMount(f.device, f.mountID, d, m)
}
func (f *fixedFiles) safeAttributes(fd int) error {
	for _, name := range []string{"security.capability", "system.posix_acl_default"} {
		value := make([]byte, 4096)
		n, err := unix.Fgetxattr(fd, name, value)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
			continue
		}
		if err != nil || n != 0 {
			return ErrState
		}
	}
	value := make([]byte, 4096)
	n, err := unix.Fgetxattr(fd, "system.posix_acl_access", value)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil || n < 4 || (n-4)%8 != 0 || binary.LittleEndian.Uint32(value) != 2 {
		return ErrState
	}
	for offset := 4; offset < n; offset += 8 {
		tag, perm, id := binary.LittleEndian.Uint16(value[offset:]), binary.LittleEndian.Uint16(value[offset+2:]), binary.LittleEndian.Uint32(value[offset+4:])
		if perm > 7 {
			return ErrState
		}
		switch tag {
		case 1, 2, 4, 8, 16, 32:
		default:
			return ErrState
		}
		if perm&2 != 0 && tag != 1 && tag != 16 && (tag != 2 || id != f.layout.uid) {
			return ErrState
		}
	}
	return nil
}
func (f *fixedFiles) safeInfo(s unix.Stat_t, limit int64, modes ...uint32) bool {
	if s.Mode&unix.S_IFMT != unix.S_IFREG || s.Uid != f.layout.uid || s.Gid != f.layout.gid || s.Nlink != 1 || s.Size < 0 || s.Size > limit || s.Dev != f.device {
		return false
	}
	for _, mode := range modes {
		if s.Mode&0o7777 == mode {
			return true
		}
	}
	return false
}
func (f *fixedFiles) pin(which directory, name string, limit int64, modes ...uint32) (*pinnedFile, error) {
	if !allowedEntry(which, name) || f.directory(which) < 0 {
		return nil, ErrState
	}
	parent := f.directory(which)
	var before unix.Stat_t
	if err := unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !f.safeInfo(before, limit, modes...) {
		return nil, ErrState
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	p := &pinnedFile{fd: fd}
	fail := func(e error) (*pinnedFile, error) { _ = p.close(); return nil, e }
	if unix.Fstat(fd, &p.info) != nil || !f.safeInfo(p.info, limit, modes...) || before.Dev != p.info.Dev || before.Ino != p.info.Ino {
		return fail(ErrState)
	}
	if err = f.sameDevice(fd); err != nil {
		return fail(err)
	}
	if err = f.safeAttributes(fd); err != nil {
		return fail(err)
	}
	hash := sha512.New()
	buffer := make([]byte, 65536)
	var offset int64
	for offset <= p.info.Size {
		size := int64(len(buffer))
		if left := p.info.Size + 1 - offset; left < size {
			size = left
		}
		n, e := unix.Pread(fd, buffer[:size], offset)
		if e != nil {
			return fail(e)
		}
		if n == 0 {
			break
		}
		_, _ = hash.Write(buffer[:n])
		offset += int64(n)
	}
	var after unix.Stat_t
	if unix.Fstat(fd, &after) != nil || offset != p.info.Size || !sameStat(p.info, after) {
		return fail(ErrState)
	}
	p.identity = FileIdentity{Size: p.info.Size, SHA512: base64.StdEncoding.EncodeToString(hash.Sum(nil)), Device: strconv.FormatUint(p.info.Dev, 10), Inode: strconv.FormatUint(p.info.Ino, 10)}
	return p, nil
}
func sameStat(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Size == b.Size && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Uid == b.Uid && a.Gid == b.Gid && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
func (f *fixedFiles) named(which directory, name string, p *pinnedFile) error {
	if p == nil {
		return ErrState
	}
	var current, opened unix.Stat_t
	if unix.Fstatat(f.directory(which), name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || unix.Fstat(p.fd, &opened) != nil || !sameStat(p.info, current) || !sameStat(p.info, opened) {
		return ErrState
	}
	return nil
}
func (f *fixedFiles) sync(which directory) error {
	return f.run("sync:"+f.label(which), func() error { return unix.Fsync(f.directory(which)) })
}
func (f *fixedFiles) writeNew(which directory, name string, data []byte, mode uint32) (result *pinnedFile, err error) {
	if !allowedEntry(which, name) || f.directory(which) < 0 || mode&0o6000 != 0 {
		return nil, ErrState
	}
	fd := -1
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	err = f.run("open:"+f.label(which)+":"+name, func() error {
		var e error
		fd, e = unix.Openat(f.directory(which), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		return e
	})
	if err != nil {
		return nil, err
	}
	if err = unix.Fchown(fd, int(f.layout.uid), int(f.layout.gid)); err != nil {
		return nil, err
	}
	if err = unix.Fchmod(fd, mode); err != nil {
		return nil, err
	}
	for offset := 0; offset < len(data); {
		end := offset + 65536
		if end > len(data) {
			end = len(data)
		}
		n := 0
		err = f.run("write:"+f.label(which)+":"+name, func() error { var e error; n, e = unix.Write(fd, data[offset:end]); return e })
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, io.ErrShortWrite
		}
		offset += n
	}
	if err = f.run("sync-file:"+name, func() error { return unix.Fsync(fd) }); err != nil {
		return nil, err
	}
	if err = unix.Close(fd); err != nil {
		return nil, err
	}
	fd = -1
	return f.pin(which, name, int64(len(data)), mode)
}
func (f *fixedFiles) exchange(current, stage string, old, next *pinnedFile) error {
	if !allowedEntry(rootDir, current) || !allowedEntry(stageDir, stage) {
		return ErrState
	}
	return f.run("exchange:"+current, func() error {
		if f.named(rootDir, current, old) != nil || f.named(stageDir, stage, next) != nil {
			return ErrState
		}
		return unix.Renameat2(f.stageFD, stage, f.rootFD, current, unix.RENAME_EXCHANGE)
	})
}
func (f *fixedFiles) lock(ctx context.Context, handoff bool) error {
	// Validate expected inherited authority before opening any new descriptor;
	// otherwise an absent FD3 could be reused by our own lock open.
	var inherited unix.Stat_t
	if handoff {
		if unix.Fstat(3, &inherited) != nil || !f.safeInfo(inherited, 0, 0o600) || f.safeAttributes(3) != nil || f.sameDevice(3) != nil {
			return ErrState
		}
		access, e := unix.FcntlInt(3, unix.F_GETFL, 0)
		if e != nil || access&unix.O_ACCMODE != unix.O_RDWR {
			return ErrState
		}
	}
	if f.lockFD >= 0 {
		return ErrState
	}
	var before unix.Stat_t
	statErr := unix.Fstatat(f.rootFD, ".sandbox-lock", &before, unix.AT_SYMLINK_NOFOLLOW)
	if statErr != nil && !errors.Is(statErr, unix.ENOENT) {
		return statErr
	}
	exists := statErr == nil
	if exists && !f.safeInfo(before, 0, 0o600) {
		return ErrState
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if !exists {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(f.rootFD, ".sandbox-lock", flags, 0o600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = unix.Close(fd)
		}
	}()
	var info unix.Stat_t
	if unix.Fstat(fd, &info) != nil || !f.safeInfo(info, 0, 0o600) || f.safeAttributes(fd) != nil || f.sameDevice(fd) != nil {
		return ErrState
	}
	if handoff {
		var current unix.Stat_t
		if unix.Fstat(3, &current) != nil || !sameStat(inherited, current) || inherited.Dev != info.Dev || inherited.Ino != info.Ino || !f.safeInfo(inherited, 0, 0o600) || f.safeAttributes(3) != nil || f.sameDevice(3) != nil {
			return ErrState
		}
		access, e := unix.FcntlInt(3, unix.F_GETFL, 0)
		if e != nil || access&unix.O_ACCMODE != unix.O_RDWR {
			return ErrState
		}
		_ = unix.Close(fd)
		fd = 3
		unix.CloseOnExec(fd)
	}
	for {
		if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			f.lockFD = fd
			success = true
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
