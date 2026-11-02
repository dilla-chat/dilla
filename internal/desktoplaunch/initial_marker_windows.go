//go:build windows

package desktoplaunch

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// FinalizeInitialMarker removes only the fixed setup marker after its native
// caller has verified the complete same-package installation and registration.
// Comparison and disposition use one retained validated R/W/DELETE file handle;
// no matching-path deletion occurs after releasing that object's identity.
func FinalizeInitialMarker(path string, expected []byte) (err error) {
	if filepath.Base(path) != "initial-install.json" || len(expected) == 0 || len(expected) > 4096 {
		return errors.New("invalid initial finalization marker")
	}
	parent, err := PinOwnedDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	file, err := openInitialRetryFile(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	actual, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("initial marker changed before finalization")
	}
	disposition := byte(1)
	return windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileDispositionInfo, &disposition, 1)
}
