//go:build linux && cgo

package frontend

import (
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var errOwnedExecutable = errors.New("mounted AppImage executable unavailable or changed")

// OwnedExecutable is created only from this launcher's own kernel-reported
// mount. No public constructor accepts a caller path or environment.
type OwnedExecutable struct {
	launcher, electron                                        string
	launcherDev, launcherIno, electronDev, electronIno, mount uint64
}

func readOnlyFuseMount(info string, mountID uint64) bool {
	for _, line := range strings.Split(info, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		l, r := strings.Fields(left), strings.Fields(right)
		if len(l) < 6 || len(r) < 3 {
			continue
		}
		id, e := strconv.ParseUint(l[0], 10, 64)
		if e != nil || id != mountID {
			continue
		}
		fs := r[0]
		if fs != "fuse" && !strings.HasPrefix(fs, "fuse.") {
			return false
		}
		hasRO := func(v string) bool {
			for _, o := range strings.Split(v, ",") {
				if o == "ro" {
					return true
				}
			}
			return false
		}
		return hasRO(l[5]) || hasRO(r[2])
	}
	return false
}
func mountedIdentity(path string) (uint64, uint64, uint64, error) {
	var st unix.Statx_t
	if e := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &st); e != nil || st.Mask&unix.STATX_MNT_ID == 0 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0111 == 0 {
		return 0, 0, 0, errOwnedExecutable
	}
	var fs unix.Statfs_t
	if unix.Statfs(path, &fs) != nil || fs.Type != 0x65735546 || fs.Flags&unix.ST_RDONLY == 0 {
		return 0, 0, 0, errOwnedExecutable
	}
	data, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil || !readOnlyFuseMount(string(data), st.Mnt_id) {
		return 0, 0, 0, errOwnedExecutable
	}
	return uint64(st.Dev_major)<<32 | uint64(st.Dev_minor), st.Ino, st.Mnt_id, nil
}
func ownExecutable() (OwnedExecutable, error) {
	if os.Getuid() == 0 || os.Geteuid() != os.Getuid() {
		return OwnedExecutable{}, errOwnedExecutable
	}
	self, e := os.Readlink("/proc/self/exe")
	if e != nil || !filepath.IsAbs(self) || strings.HasSuffix(self, " (deleted)") || filepath.Clean(self) != self {
		return OwnedExecutable{}, errOwnedExecutable
	}
	resolved, e := filepath.EvalSymlinks(self)
	if e != nil || resolved != self {
		return OwnedExecutable{}, errOwnedExecutable
	}
	ld, li, lm, e := mountedIdentity(self)
	if e != nil {
		return OwnedExecutable{}, e
	}
	sibling := filepath.Join(filepath.Dir(self), "dilla-desktop")
	ed, ei, em, e := mountedIdentity(sibling)
	if e != nil || em != lm {
		return OwnedExecutable{}, errOwnedExecutable
	}
	image, e := elf.Open(sibling)
	if e != nil {
		return OwnedExecutable{}, errOwnedExecutable
	}
	defer func() { _ = image.Close() }()
	if image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB || image.Machine != elf.EM_X86_64 {
		return OwnedExecutable{}, errOwnedExecutable
	}
	return OwnedExecutable{self, sibling, ld, li, ed, ei, lm}, nil
}
func (o OwnedExecutable) recheck() error {
	if o.launcher == "" || o.electron == "" {
		return errOwnedExecutable
	}
	self, e := os.Readlink("/proc/self/exe")
	if e != nil || self != o.launcher {
		return errOwnedExecutable
	}
	ld, li, lm, e := mountedIdentity(o.launcher)
	if e != nil || ld != o.launcherDev || li != o.launcherIno || lm != o.mount {
		return errOwnedExecutable
	}
	ed, ei, em, e := mountedIdentity(o.electron)
	if e != nil || ed != o.electronDev || ei != o.electronIno || em != o.mount {
		return errOwnedExecutable
	}
	return nil
}
