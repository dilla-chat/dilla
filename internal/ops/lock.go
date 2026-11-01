package ops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked is returned when another dillad holds the data directory in a mode
// that excludes the one asked for.
var ErrLocked = errors.New("ops: the data directory is in use by another dillad")

// LockFile is the advisory lock's file inside the data directory. `restore`
// holds it exclusively; `serve` and `backup` hold it shared.
const LockFile = "dilla.lock"

// ServeLockFile is the second lock file, which only `serve` takes, and takes
// exclusively: it is what keeps two `serve` processes off one data directory.
//
// One file cannot carry both rules. `backup` must run against a live `serve`
// (a nightly backup of a running instance), and flock refuses a shared holder
// while an exclusive one exists, so `serve` can only hold dilla.lock shared —
// and a shared hold does not exclude a second `serve`.
const ServeLockFile = "dilla.serve.lock"

// Lock holds shared/exclusive file locks inside a data directory. The OS drops
// them on Release or process exit (flock on Unix, LockFileEx on Windows).
type Lock struct{ files []*os.File }

// AcquireLock takes LOCK_EX|LOCK_NB on dilla.lock: it fails with ErrLocked
// while any other holder, shared or exclusive, has the directory. `restore`
// takes it.
func AcquireLock(dir string) (*Lock, error) { return acquire(dir, LockFile, true) }

// AcquireSharedLock takes LOCK_SH|LOCK_NB on dilla.lock. Several shared
// holders coexist; an exclusive holder excludes them all and is excluded by any
// of them. `backup` takes it.
func AcquireSharedLock(dir string) (*Lock, error) { return acquire(dir, LockFile, false) }

// AcquireServeLock is `serve`'s pair: LOCK_EX on dilla.serve.lock, so a second
// `serve` is refused, then LOCK_SH on dilla.lock, so a `backup` may run beside
// it and a `restore` may not.
func AcquireServeLock(dir string) (*Lock, error) {
	serve, err := acquire(dir, ServeLockFile, true)
	if err != nil {
		return nil, err
	}
	shared, err := acquire(dir, LockFile, false)
	if err != nil {
		_ = serve.Release()
		return nil, err
	}
	return &Lock{files: append(serve.files, shared.files...)}, nil
}

func acquire(dir, name string, exclusive bool) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the operator's configured data directory
	if err != nil {
		return nil, err
	}
	if err := tryFileLock(f, exclusive); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, dir)
		}
		return nil, fmt.Errorf("ops: lock %s: %w", dir, err)
	}
	return &Lock{files: []*os.File{f}}, nil
}

// Release drops the lock. Releasing twice, or a nil lock, is not an error.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	var err error
	for i := len(l.files) - 1; i >= 0; i-- {
		f := l.files[i]
		if uerr := unlockFile(f); uerr != nil && err == nil {
			err = uerr
		}
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	l.files = nil
	return err
}
