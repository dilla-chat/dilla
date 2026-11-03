//go:build unix

package ops

import (
	"errors"
	"os"
	"syscall"
)

func tryFileLock(f *os.File, exclusive bool) error {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
