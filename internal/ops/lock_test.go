package ops_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/ops"
)

func TestTheLockIsExclusiveWithinOneProcess(t *testing.T) {
	dir := t.TempDir()
	l, err := ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := ops.AcquireLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("second AcquireLock = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after Release: %v", err)
	}
	_ = l2.Release()
}

// flock is per open file description, so the cross-process case is the one that
// matters and it needs a second process.
func TestTheLockIsExclusiveAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	l, err := ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = l.Release() }()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), "DILLA_LOCK_HELPER="+dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper acquired the lock: %s", out)
	}
	if !strings.Contains(string(out), "ErrLocked") {
		t.Fatalf("helper output = %s", out)
	}
}

func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv("DILLA_LOCK_HELPER")
	if dir == "" {
		t.Skip("not the helper")
	}
	if _, err := ops.AcquireLock(dir); errors.Is(err, ops.ErrLocked) {
		fmt.Println("ErrLocked")
		os.Exit(1)
	}
	os.Exit(0)
}

// Plan 2 task 13's shared/exclusive rule, landed with its first caller (task
// 12's backup): an exclusive holder excludes every shared one, several shared
// holders coexist, and any shared holder excludes an exclusive acquire — the
// restore-during-backup refusal.
func TestBackupAndServeHoldTheDirectoryTogether(t *testing.T) {
	dir := t.TempDir()
	ex, err := ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := ops.AcquireSharedLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("shared acquire under an exclusive holder = %v, want ErrLocked", err)
	}
	if err := ex.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	a, err := ops.AcquireSharedLock(dir)
	if err != nil {
		t.Fatalf("first shared acquire: %v", err)
	}
	b, err := ops.AcquireSharedLock(dir)
	if err != nil {
		t.Fatalf("second shared acquire: %v", err)
	}
	if _, err := ops.AcquireLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("exclusive acquire under shared holders = %v, want ErrLocked", err)
	}
	if err := a.Release(); err != nil {
		t.Fatalf("Release a: %v", err)
	}
	if err := b.Release(); err != nil {
		t.Fatalf("Release b: %v", err)
	}
	ex, err = ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after every shared holder released: %v", err)
	}
	if err := ex.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Release is idempotent.
	if err := ex.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

// The resolution of the plan's own contradiction (task 12's concern): `serve`
// holds dilla.lock SHARED plus its own dilla.serve.lock EXCLUSIVELY. A backup
// runs beside a live serve, a second serve is refused, and a restore is refused
// while either a serve or a backup holds the directory.
func TestServeAdmitsABackupAndExcludesASecondServeAndARestore(t *testing.T) {
	dir := t.TempDir()
	serve, err := ops.AcquireServeLock(dir)
	if err != nil {
		t.Fatalf("AcquireServeLock: %v", err)
	}
	backup, err := ops.AcquireSharedLock(dir)
	if err != nil {
		t.Fatalf("a backup beside a live serve was refused: %v", err)
	}
	if _, err := ops.AcquireServeLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("second serve = %v, want ErrLocked", err)
	}
	if _, err := ops.AcquireLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("restore under a live serve = %v, want ErrLocked", err)
	}
	if err := serve.Release(); err != nil {
		t.Fatalf("Release serve: %v", err)
	}
	if _, err := ops.AcquireLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("restore during a backup = %v, want ErrLocked", err)
	}
	if err := backup.Release(); err != nil {
		t.Fatalf("Release backup: %v", err)
	}
	restore, err := ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("restore with the directory free: %v", err)
	}
	if _, err := ops.AcquireServeLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("serve during a restore = %v, want ErrLocked", err)
	}
	// A refused serve must not keep dilla.serve.lock: once the restore is done
	// the next serve starts.
	if err := restore.Release(); err != nil {
		t.Fatalf("Release restore: %v", err)
	}
	again, err := ops.AcquireServeLock(dir)
	if err != nil {
		t.Fatalf("serve after the restore: %v", err)
	}
	_ = again.Release()
}
