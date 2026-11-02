//go:build linux

package desktopsandbox

import (
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// CI test binary only: the canonical lock is independently opened/checked in
// the current namespace. FD3 must match that exact inode and the ORIGINAL
// mount ID admitted by the root-owned lifecycle record before unshare. This
// exception does not alter production sameMount or create a new lock/OFD.
func nativeLeaseBinding(canonical, inherited unix.Stat_t, currentMount, device, originalMount uint64, boundMount string) error {
	admitted, e := strconv.ParseUint(boundMount, 10, 64)
	if e != nil || admitted == 0 || strconv.FormatUint(admitted, 10) != boundMount || currentMount == 0 || originalMount != admitted || device != canonical.Dev || !sameStat(canonical, inherited) || inherited.Mode != unix.S_IFREG|0o600 || inherited.Uid != 0 || inherited.Gid != 0 || inherited.Nlink != 1 || inherited.Size != 0 {
		return ErrState
	}
	return nil
}

func TestNativeLeaseBindingRequiresProtectedPreNamespaceIdentity(t *testing.T) {
	canonical := unix.Stat_t{Dev: 7, Ino: 11, Mode: unix.S_IFREG | 0o600, Nlink: 1}
	inherited := canonical
	for _, currentMount := range []uint64{101, 202} {
		if e := nativeLeaseBinding(canonical, inherited, currentMount, 7, 101, "101"); e != nil {
			t.Fatalf("same bound lease rejected across canonical mount clone: %v", e)
		}
	}
	if e := sameMount(7, 202, 7, 101); e == nil {
		t.Fatal("production mount validation was relaxed")
	}
	for _, name := range []string{"wrong-inode", "wrong-device", "wrong-owner", "wrong-group", "wrong-mode", "extra-link", "size", "timestamp", "zero-current-mount", "zero-inherited-mount", "wrong-recorded-mount", "zero-recorded-mount", "missing-recorded-mount", "noncanonical-recorded-mount"} {
		t.Run(name, func(t *testing.T) {
			c, i, current, device, original, bound := canonical, inherited, uint64(202), uint64(7), uint64(101), "101"
			switch name {
			case "wrong-inode":
				i.Ino++
			case "wrong-device":
				device++
			case "wrong-owner":
				c.Uid = 1
				i.Uid = 1
			case "wrong-group":
				c.Gid = 1
				i.Gid = 1
			case "wrong-mode":
				c.Mode = unix.S_IFREG | 0o644
				i.Mode = c.Mode
			case "extra-link":
				c.Nlink = 2
				i.Nlink = 2
			case "size":
				c.Size = 1
				i.Size = 1
			case "timestamp":
				i.Ctim.Nsec++
			case "zero-current-mount":
				current = 0
			case "zero-inherited-mount":
				original = 0
			case "wrong-recorded-mount":
				bound = "202"
			case "zero-recorded-mount":
				bound = "0"
			case "missing-recorded-mount":
				bound = ""
			case "noncanonical-recorded-mount":
				bound = "0101"
			}
			if nativeLeaseBinding(c, i, current, device, original, bound) == nil {
				t.Fatal("unbound or unsafe lease accepted")
			}
		})
	}
}
