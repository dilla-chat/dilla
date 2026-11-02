//go:build linux

package desktopsandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fixtureLayout(t *testing.T) fileLayout {
	t.Helper()
	anchor := t.TempDir()
	root := filepath.Join(anchor, "dilla/appimage")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return fileLayout{anchor: anchor, parts: []string{"dilla", "appimage"}, uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), helperMode: 0o555}
}
func fixtureFiles(t *testing.T) (*fixedFiles, fileLayout) {
	t.Helper()
	layout := fixtureLayout(t)
	f, err := openFixedFiles(layout, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.close() })
	return f, layout
}

func TestFixedFilesPinRealInodesAndRejectUnsafeChildren(t *testing.T) {
	f, l := fixtureFiles(t)
	payload := []byte("owned component bytes")
	p, err := f.writeNew(stageDir, "helper.next", payload, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if p.identity.SHA512 != digest(payload) || p.identity.Size != int64(len(payload)) {
		t.Fatal("wrong pinned bytes")
	}
	info, err := os.Stat(filepath.Join(l.root(), ".sandbox-staging/helper.next"))
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err = unix.Fstat(p.fd, &st); err != nil {
		t.Fatal(err)
	}
	if p.identity.Inode != strconv.FormatUint(st.Ino, 10) || info.Mode().Perm() != 0o755 {
		t.Fatal("identity was derived from a later pathname")
	}
	receiptStage, e := f.writeNew(stageDir, "receipt.next", []byte("owned receipt bytes"), 0o644)
	if e != nil {
		t.Fatal(e)
	}
	receiptStage.close()
	for _, kind := range []string{"symlink", "hardlink", "writable", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(l.root(), "sandbox-receipt.json")
			_ = os.Remove(path)
			switch kind {
			case "symlink":
				if err = os.Symlink("/dev/zero", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err = os.Link(filepath.Join(l.root(), ".sandbox-staging/receipt.next"), path); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err = os.WriteFile(path, []byte("unsafe"), 0o666); err != nil {
					t.Fatal(err)
				}
				if err = os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err = unix.Mkfifo(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if file, err := f.pin(rootDir, "sandbox-receipt.json", 128<<10, 0o644); err == nil {
				if file != nil {
					file.close()
				}
				t.Fatal("unsafe file accepted", kind)
			}
			_ = os.Remove(path)
		})
	}
}

func TestFixedFilesLeaveNativeSiblingUntouchedAndHoldIndependentLock(t *testing.T) {
	f, l := fixtureFiles(t)
	sibling := filepath.Join(filepath.Dir(l.root()), "native")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(sibling, "owned-by-package-manager")
	if err := os.WriteFile(sentinel, []byte("native content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.lock(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	other, err := openFixedFiles(l, true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = other.lock(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("different open description bypassed lock", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "native content" {
		t.Fatal("changed sibling namespace")
	}
}

func TestFixedFilesCatchLateRenameReplacementAndSyncFailure(t *testing.T) {
	f, l := fixtureFiles(t)
	old, err := f.writeNew(rootDir, "sandbox-maintainer", []byte("old"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer old.close()
	next, err := f.writeNew(stageDir, "maintainer.next", []byte("new"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer next.close()
	f.fault = func(op string, after bool) error {
		if op == "exchange:sandbox-maintainer" && !after {
			if err := os.Rename(filepath.Join(l.root(), "sandbox-maintainer"), filepath.Join(l.root(), "changed-owner-copy")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(l.root(), "sandbox-maintainer"), []byte("unexpected"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if err = f.exchange("sandbox-maintainer", "maintainer.next", old, next); err == nil {
		t.Fatal("renamed an unexpected late path")
	}
	if b, e := os.ReadFile(filepath.Join(l.root(), "sandbox-maintainer")); e != nil || string(b) != "unexpected" {
		t.Fatal("late-state guard mutated replacement")
	}
	f.fault = func(op string, after bool) error {
		if op == "sync:stage" && !after {
			return unix.ENOSPC
		}
		return nil
	}
	if err = f.sync(stageDir); !errors.Is(err, unix.ENOSPC) {
		t.Fatal("directory durability error ignored", err)
	}
}

func TestFixedFilesRejectMaskedWritableACLAndWrongDevice(t *testing.T) {
	f, l := fixtureFiles(t)
	p, err := f.writeNew(stageDir, "bootstrap-capsule", []byte("owned"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	p.close()
	// Real Linux ACL: named-user write is masked off, so mode remains0600.
	// Reject the latent write grant rather than relying solely on current mask.
	acl := []byte{2, 0, 0, 0, 1, 0, 6, 0, 255, 255, 255, 255, 2, 0, 2, 0, 0, 0, 0, 0, 4, 0, 0, 0, 255, 255, 255, 255, 16, 0, 0, 0, 255, 255, 255, 255, 32, 0, 0, 0, 255, 255, 255, 255}
	uid := uint32(os.Getuid() + 1)
	acl[16] = byte(uid)
	acl[17] = byte(uid >> 8)
	acl[18] = byte(uid >> 16)
	acl[19] = byte(uid >> 24)
	path := filepath.Join(l.root(), ".sandbox-staging/bootstrap-capsule")
	if err = unix.Setxattr(path, "system.posix_acl_access", acl, 0); err != nil {
		t.Fatal("fixture ACL setup", err)
	}
	if got, err := f.pin(stageDir, "bootstrap-capsule", MaxCapsule, 0o600); err == nil {
		if got != nil {
			got.close()
		}
		t.Fatal("masked writable ACL accepted")
	}
	// Read a real different mounted filesystem descriptor; this creates no mounts.
	proc, err := unix.Open("/proc", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(proc)
	if err = f.sameDevice(proc); err == nil {
		t.Fatal("different device accepted")
	}
}

func TestProductionSandboxNamespaceIsFixedAndIndependent(t *testing.T) {
	t.Setenv("DILLA_SANDBOX_ROOT", t.TempDir())
	l := appImageLayout()
	if l.root() != "/usr/lib/dilla/appimage" || l.anchor != "/" || l.uid != 0 || l.gid != 0 || l.helperMode != 0o4755 {
		t.Fatal("production fixed namespace became caller-controlled")
	}
}
func TestLockHandoffProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "owned-lock-handoff" {
		return
	}
	l := fileLayout{anchor: os.Getenv("OWNED_TEST_ANCHOR"), parts: []string{"dilla", "appimage"}, uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), helperMode: 0o555}
	f, err := openFixedFiles(l, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = f.lock(ctx, true); err != nil || f.lockFD != 3 {
		t.Fatal("did not adopt inherited locked open description", err)
	}
	flags, err := unix.FcntlInt(3, unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("inherited lock would leak into later children")
	}
	other, err := openFixedFiles(l, true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	short, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	if err = other.lock(short, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("inherited lock was released", err)
	}
}
func TestActualInheritedLockDescriptionSurvivesExec(t *testing.T) {
	f, l := fixtureFiles(t)
	if err := f.lock(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	duplicate, err := unix.Dup(f.lockFD)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(duplicate), "owned lock")
	defer file.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLockHandoffProcess$", "--", "owned-lock-handoff")
	command.Env = append(os.Environ(), "OWNED_TEST_ANCHOR="+l.anchor)
	command.ExtraFiles = []*os.File{file}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("owned handoff child failed: %v %s", err, output)
	}
}

func TestRequiredDirectoriesRejectUnexpectedGroup(t *testing.T) {
	f, _ := fixtureFiles(t)
	f.layout.gid++
	if e := f.safeDir(f.rootFD, 0o755, false); e == nil {
		t.Fatal("required shared/leaf directory accepted wrong GID")
	}
}
func TestUnexpectedLockHandoffProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "owned-lock-reject" {
		return
	}
	kind := os.Args[len(os.Args)-1]
	l := fileLayout{anchor: os.Getenv("OWNED_TEST_ANCHOR"), parts: []string{"dilla", "appimage"}, uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), helperMode: 0o555}
	f, e := openFixedFiles(l, true)
	if e != nil {
		t.Fatal(e)
	}
	defer f.close()
	if kind == "missing" {
		if e = unix.Close(3); e != nil {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if e = f.lock(ctx, true); !errors.Is(e, ErrState) {
		t.Fatal("unexpected handoff did not fail closed", e)
	}
}
func TestActualUnexpectedHandoffsFailClosed(t *testing.T) {
	f, l := fixtureFiles(t)
	if e := f.lock(t.Context(), false); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"missing", "wrong-inode", "wrong-access"} {
		t.Run(kind, func(t *testing.T) {
			name := "/dev/null"
			if kind == "wrong-access" {
				name = filepath.Join(l.root(), ".sandbox-lock")
			}
			var file *os.File
			var e error
			if kind == "wrong-inode" {
				file, e = os.CreateTemp(l.anchor, "second-owned-lock-")
			} else {
				file, e = os.Open(name)
			}
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUnexpectedLockHandoffProcess$", "--", "owned-lock-reject", kind)
			command.Env = append(os.Environ(), "OWNED_TEST_ANCHOR="+l.anchor)
			command.ExtraFiles = []*os.File{file}
			if out, e := command.CombinedOutput(); e != nil {
				t.Fatalf("handoff rejection child: %v %s", e, out)
			}
		})
	}
}
func TestMountIdentityRejectsSameDeviceBindTransitions(t *testing.T) {
	if e := sameMount(7, 101, 7, 102); e == nil {
		t.Fatal("same-device different mount accepted")
	}
	if e := sameMount(7, 101, 8, 101); e == nil {
		t.Fatal("different device accepted")
	}
	if e := sameMount(7, 101, 7, 101); e != nil {
		t.Fatal(e)
	}
}
