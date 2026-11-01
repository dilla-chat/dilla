//go:build windows

package desktoplaunch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func openAbsolutePinned(path string, directory bool) (*os.File, error) {
	// The launcher supports local known-folder roots only. A long-path prefix is
	// added internally after rejecting network/device namespaces and aliases.
	if !filepath.IsAbs(path) || len(filepath.VolumeName(path)) != 2 || strings.HasPrefix(path, `\\`) {
		return nil, errors.New("program root must be a local absolute path")
	}
	p, e := syscall.UTF16PtrFromString(`\\?\` + path)
	if e != nil {
		return nil, e
	}
	// OPEN_REPARSE_POINT applies to the final component. Each ancestor is opened
	// and held first, so a directory cannot be replaced between traversal steps.
	h, e := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(h), path)
	var info syscall.ByHandleFileInformation
	if e = syscall.GetFileInformationByHandle(h, &info); e != nil {
		_ = f.Close()
		return nil, e
	}
	isDir := info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || isDir != directory || !directory && info.NumberOfLinks != 1 {
		_ = f.Close()
		return nil, errors.New("reparse, linked or wrong-type program entry")
	}
	return f, nil
}
func openPinned(base string, _ *os.Root, path string, directory bool) (*os.File, error) {
	absolute := filepath.Join(base, filepath.FromSlash(path))
	f, e := openAbsolutePinned(absolute, directory)
	if e != nil {
		return nil, e
	}
	if e = checkHandleSecurity(syscall.Handle(f.Fd())); e != nil {
		_ = f.Close()
		return nil, e
	}
	if e = checkNoNamedDataStreams(absolute, directory); e != nil {
		_ = f.Close()
		return nil, e
	}
	return f, nil
}
