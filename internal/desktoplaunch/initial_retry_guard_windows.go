//go:build windows

package desktoplaunch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func openInitialEnumeration(file *os.File) (*os.File, error) {
	handle, _, err := streamDLL.NewProc("ReOpenFile").Call(file.Fd(), syscall.FILE_LIST_DIRECTORY|0x80|syscall.SYNCHRONIZE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS)
	if handle == 0 || handle == uintptr(syscall.InvalidHandle) {
		if err == syscall.Errno(0) {
			err = errors.New("initial directory reopen failed")
		}
		return nil, err
	}
	return os.NewFile(handle, file.Name()), nil
}
