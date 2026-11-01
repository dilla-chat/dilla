//go:build !windows

package desktoplaunch

import (
	"errors"
	"os"
	"reflect"
	"syscall"
)

// Portable verification is used by publisher fixtures. Only the Windows adapter
// provides retained handles with deny-write/delete sharing for executable launch.
func openAbsolutePinned(path string, directory bool) (*os.File, error) {
	before, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if before.Mode()&os.ModeSymlink != 0 || (directory && !before.IsDir()) || (!directory && !before.Mode().IsRegular()) {
		return nil, errors.New("linked program path")
	}
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: native callers derive roots from compiled identity; this read-only portable path rejects links and checks file identity.
	if e != nil {
		return nil, e
	}
	return checkPortableFile(f, before, directory)
}
func openPinned(_ string, root *os.Root, path string, directory bool) (*os.File, error) {
	before, e := root.Lstat(path)
	if e != nil {
		return nil, e
	}
	if before.Mode()&os.ModeSymlink != 0 || (directory && !before.IsDir()) || (!directory && !before.Mode().IsRegular()) {
		return nil, errors.New("linked program path")
	}
	f, e := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	return checkPortableFile(f, before, directory)
}
func checkPortableFile(f *os.File, before os.FileInfo, directory bool) (*os.File, error) {
	after, e := f.Stat()
	if e != nil {
		_ = f.Close()
		return nil, e
	}
	valid := os.SameFile(before, after) && ((directory && after.IsDir()) || (!directory && after.Mode().IsRegular()))
	if !directory {
		v := reflect.Indirect(reflect.ValueOf(after.Sys()))
		if v.IsValid() && v.Kind() == reflect.Struct {
			links := v.FieldByName("Nlink")
			if links.IsValid() && links.CanUint() && links.Uint() != 1 {
				valid = false
			}
		}
	}
	if !valid {
		_ = f.Close()
		return nil, errors.New("linked or non-regular program entry")
	}
	return f, nil
}
