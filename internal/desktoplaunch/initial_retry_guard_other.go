//go:build !windows

package desktoplaunch

import (
	"errors"
	"os"
)

// Portable fixtures verify namespace/prefix policy only. Mapping exclusion and
// share-delete-only leases are provided and tested by the Windows adapter.
func openInitialRetryFile(path string) (*os.File, error) { return openAbsolutePinned(path, false) }

func openInitialEnumeration(file *os.File) (*os.File, error) {
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	opened, err := openAbsolutePinned(file.Name(), true)
	if err != nil {
		return nil, err
	}
	after, err := opened.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = opened.Close()
		return nil, errors.New("initial enumeration identity changed")
	}
	return opened, nil
}
