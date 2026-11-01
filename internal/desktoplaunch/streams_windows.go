//go:build windows

package desktoplaunch

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var streamDLL = syscall.NewLazyDLL("kernel32.dll") // Go loads this known system DLL from System32.
var findFirstStream = streamDLL.NewProc("FindFirstStreamW")
var findNextStream = streamDLL.NewProc("FindNextStreamW")

// WIN32_FIND_STREAM_DATA is LARGE_INTEGER followed by WCHAR[MAX_PATH+36].
type nativeFindStreamData struct {
	Size int64
	Name [260 + 36]uint16
}

// checkNoNamedDataStreams is called only after the target and all its ancestors
// have been opened and pinned. FindFirstStreamW enumerates $DATA streams only:
// regular files have the unnamed ::$DATA stream; directories have none by default.
// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-findfirststreamw
//
// Sharing modes are per stream. Retaining the default-stream handle prevents its
// mutation/replacement, but does not lock the named-stream namespace against a
// trusted current-user process creating a new ADS after this verification snapshot.
// Callers must drain authorized program writers and retain the ownership/DACL/path
// guards through CreateProcess; this is not protection against a hostile same-user OS process.
//
//nolint:gosec // G103: fixed Win32 stream-enumeration ABI receives a held-target path and a bounded WIN32_FIND_STREAM_DATA buffer; the search handle is closed after use.
func checkNoNamedDataStreams(path string, directory bool) error {
	p, e := syscall.UTF16PtrFromString(`\\?\` + path)
	if e != nil {
		return e
	}
	var data nativeFindStreamData
	policy := dataStreamPolicy{directory: directory}
	handle, _, callErr := findFirstStream.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&data)), 0)
	if handle == uintptr(syscall.InvalidHandle) {
		if directory && errors.Is(callErr, syscall.ERROR_HANDLE_EOF) {
			return policy.finish()
		}
		return dataStreamEnumerationError(callErr)
	}
	defer func() { _ = syscall.FindClose(syscall.Handle(handle)) }()
	for {
		if data.Size < 0 {
			return errors.New("invalid program stream size")
		}
		if e = policy.add(syscall.UTF16ToString(data.Name[:])); e != nil {
			return e
		}
		found, _, nextErr := findNextStream.Call(handle, uintptr(unsafe.Pointer(&data)))
		if found != 0 {
			continue
		}
		if errors.Is(nextErr, syscall.ERROR_HANDLE_EOF) {
			return policy.finish()
		}
		return dataStreamEnumerationError(nextErr)
	}
}

func dataStreamEnumerationError(err error) error {
	// FindFirstStreamW reports ERROR_INVALID_PARAMETER when the filesystem cannot
	// enumerate streams. Unsupported filesystems cannot establish the no-ADS contract.
	if errors.Is(err, syscall.Errno(87)) {
		return errors.New("filesystem does not support required program data-stream enumeration")
	}
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("program data-stream enumeration failed without a native error")
	}
	return fmt.Errorf("program data-stream enumeration failed: %w", err)
}
