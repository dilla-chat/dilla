//go:build linux

package helper

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// The helper dies with the Bot: a helper that outlived a restarted Bot kept state.lock, and the next Bot's Open was
// refused as busy (v0.1.0-bot2 on an upgraded instance).
func TestTheHelperIsKilledWhenTheBotDies(t *testing.T) {
	var seen *syscall.SysProcAttr
	stop := errors.New("not started")
	_, err := Start(Options{Executable: os.Args[0], StateDir: t.TempDir(), launcher: func(cmd *exec.Cmd) error {
		seen = cmd.SysProcAttr
		return stop
	}})
	if err == nil {
		t.Fatal("Start succeeded without a launched helper")
	}
	if seen == nil || seen.Pdeathsig != syscall.SIGKILL || !seen.Setpgid {
		t.Fatalf("helper process attributes = %+v, want Setpgid and Pdeathsig SIGKILL", seen)
	}
}
