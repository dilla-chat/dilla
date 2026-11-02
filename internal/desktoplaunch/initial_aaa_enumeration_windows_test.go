//go:build windows

package desktoplaunch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Bounded diagnosis of an actual native access-denied result. Receipts contain
// fixed labels and numeric masks/errors only; no paths, SIDs or ACL contents.
func TestWindowsInitialEnumerationReopenControlMatrix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "claimed")
	claim, err := ClaimInitialUpdateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Rollback() }()
	type source struct {
		name   string
		handle syscall.Handle
	}
	sources := []source{{"nt-created-rwd", syscall.Handle(claim.entries["."].file.Fd())}}
	for _, entry := range []struct {
		name   string
		access uint32
	}{
		{"create-read", windows.FILE_GENERIC_READ}, {"create-rwd", windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE}, {"create-rwd-traverse", windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE | windows.FILE_TRAVERSE},
	} {
		controlRoot := filepath.Join(parent, entry.name)
		controlClaim, e := ClaimInitialUpdateRoot(controlRoot)
		if e != nil {
			t.Fatal(e)
		}
		if e = controlClaim.Close(); e != nil {
			t.Fatal(e)
		}
		path, e := syscall.UTF16PtrFromString(controlRoot)
		if e != nil {
			t.Fatal(e)
		}
		handle, e := syscall.CreateFile(path, entry.access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if e != nil {
			t.Fatalf("controlled CreateFile source %s: %v", entry.name, e)
		}
		defer syscall.CloseHandle(handle)
		sources = append(sources, source{entry.name, handle})
	}
	plain := filepath.Join(parent, "plain")
	if err = os.Mkdir(plain, 0700); err != nil {
		t.Fatal(err)
	}
	pinned, err := PinOwnedDirectory(plain)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	sources = append(sources, source{"pin-read", syscall.Handle(pinned.guard.handles[len(pinned.guard.handles)-1].Fd())})
	numeric := func(err error) uint32 {
		if err == nil || errors.Is(err, io.EOF) {
			return 0
		}
		var number syscall.Errno
		if errors.As(err, &number) {
			return uint32(number)
		}
		return 0xffffffff
	}
	for _, original := range sources {
		for _, access := range []uint32{windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE, windows.FILE_GENERIC_READ, windows.GENERIC_READ, windows.FILE_GENERIC_READ | windows.FILE_TRAVERSE} {
			for _, flags := range []uint32{windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT, windows.FILE_FLAG_BACKUP_SEMANTICS} {
				handle, _, callErr := streamDLL.NewProc("ReOpenFile").Call(uintptr(original.handle), uintptr(access), uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE), uintptr(flags))
				openCode, readCode := uint32(0), uint32(0)
				if handle == 0 || handle == uintptr(syscall.InvalidHandle) {
					openCode = numeric(callErr)
					if openCode == 0 {
						openCode = 0xffffffff
					}
				} else {
					file := os.NewFile(handle, "private-enumeration-control")
					_, readErr := file.ReadDir(1)
					readCode = numeric(readErr)
					if closeErr := file.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
				t.Logf("INITIAL_ENUM_CONTROL source=%s access=%08x flags=%08x open=%d read=%d", original.name, access, flags, openCode, readCode)
			}
		}
	}
	for _, original := range sources {
		for _, access := range []uint32{windows.FILE_GENERIC_READ, windows.FILE_GENERIC_READ | windows.FILE_TRAVERSE} {
			name, e := windows.NewNTUnicodeString(".")
			if e != nil {
				t.Fatal(e)
			}
			attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(original.handle), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
			attributes.Length = uint32(unsafe.Sizeof(attributes))
			var handle windows.Handle
			var status windows.IO_STATUS_BLOCK
			openErr := windows.NtCreateFile(&handle, access, &attributes, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
			openCode, identityCode, readCode := numeric(initialNTError(openErr)), uint32(0), uint32(0)
			if openErr == nil {
				var before, after syscall.ByHandleFileInformation
				beforeErr := syscall.GetFileInformationByHandle(original.handle, &before)
				afterErr := syscall.GetFileInformationByHandle(syscall.Handle(handle), &after)
				identityCode = numeric(errors.Join(beforeErr, afterErr))
				if identityCode == 0 && (before.VolumeSerialNumber != after.VolumeSerialNumber || before.FileIndexHigh != after.FileIndexHigh || before.FileIndexLow != after.FileIndexLow) {
					identityCode = 0xfffffffe
				}
				file := os.NewFile(uintptr(handle), "private-relative-enumeration-control")
				_, readErr := file.ReadDir(64)
				readCode = numeric(readErr)
				if closeErr := file.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			} else if handle != 0 && handle != windows.InvalidHandle {
				_ = windows.CloseHandle(handle)
			}
			t.Logf("INITIAL_ENUM_DOT_CONTROL source=%s access=%08x open=%d identity=%d read=%d", original.name, access, openCode, identityCode, readCode)
		}
	}

}
