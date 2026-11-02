//go:build windows

package desktoplaunch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func openInitialRetryFile(path string) (file *os.File, err error) {
	if !filepath.IsAbs(path) || len(filepath.VolumeName(path)) != 2 || strings.HasPrefix(path, `\\`) {
		return nil, errors.New("initial retry file must be local")
	}
	name, err := syscall.UTF16PtrFromString(`\\?\` + path)
	if err != nil {
		return nil, err
	}
	// WRITE rejects already mapped images. No READ/WRITE sharing prevents new
	// mappings/writers; DELETE sharing supports identity-bound rollback disposition.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE|0x10000, syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file = os.NewFile(uintptr(handle), path)
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
			file = nil
		}
	}()
	var identity syscall.ByHandleFileInformation
	if err = syscall.GetFileInformationByHandle(handle, &identity); err != nil {
		return file, err
	}
	if identity.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || identity.NumberOfLinks != 1 {
		return file, errors.New("linked or wrong-type initial retry file")
	}
	if err = checkHandleSecurity(handle); err != nil {
		return file, err
	}
	if err = checkNoNamedDataStreams(path, false); err != nil {
		return file, err
	}
	return file, nil
}

// DuplicateHandle preserves the original object, access and sharing. A fresh
// os.File has fresh Go dirInfo, whose first query uses DirectoryRestartInfo.
// The kernel cursor is shared: callers serialize enumeration of a held object
// (the owning initial transaction and claims are not concurrent-use APIs).
func openInitialEnumeration(file *os.File) (*os.File, error) {
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(duplicate), file.Name()), nil
}
