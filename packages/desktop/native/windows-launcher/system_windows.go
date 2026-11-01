//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

// bootstrapConfig is base64 JSON injected by -ldflags at build time. It is never
// populated from an environment variable, command-line argument or mutable file.
var bootstrapConfig string
var kernelDLL = syscall.NewLazyDLL("kernel32.dll") // Go restricts this known DLL to System32.

//nolint:gosec // G103: fixed kernel32 ABI writes into the passed, bounded UTF16 buffer.
func systemDLL(name string) (*syscall.LazyDLL, error) {
	buffer := make([]uint16, 32768)
	n, _, e := kernelDLL.NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if n == 0 || n >= uintptr(len(buffer)) {
		return nil, e
	}
	return syscall.NewLazyDLL(filepath.Join(syscall.UTF16ToString(buffer[:n]), name)), nil
}

//nolint:gosec // G103: fixed Windows known-folder ABI returns an OS-allocated NUL-terminated UTF16 path, freed with CoTaskMemFree.
func roamingAppData() (string, error) {
	folder := syscall.GUID{Data1: 0x3eb685db, Data2: 0x65f9, Data3: 0x4cf6, Data4: [8]byte{0xa0, 0x3a, 0xe3, 0xef, 0x65, 0x72, 0x9f, 0x3d}}
	var pointer *uint16
	code, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath").Call(uintptr(unsafe.Pointer(&folder)), 0, 0, uintptr(unsafe.Pointer(&pointer)))
	if code != 0 || pointer == nil {
		return "", errors.New("cannot resolve Windows roaming known folder")
	}
	ole, e := systemDLL("ole32.dll")
	if e != nil {
		return "", e
	}
	defer func() { _, _, _ = ole.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(pointer))) }()
	return syscall.UTF16ToString(unsafe.Slice(pointer, 32768)), nil
}

// deriveLayout resolves the running image through a handle. Environment variables
// and command-line arguments cannot replace either authority root.
//
//nolint:gosec // G103: fixed GetFinalPathNameByHandleW ABI reads the held executable handle and writes a bounded UTF16 buffer.
func deriveLayout(cfg buildConfig) (result layout, selfGuard *desktoplaunch.FileGuard, err error) {
	path, e := os.Executable()
	if e != nil {
		return result, nil, e
	}
	path = strings.TrimPrefix(path, `\\?\`)
	guard, e := desktoplaunch.PinOwnedFile(path)
	if e != nil {
		return result, nil, e
	}
	defer func() {
		if err != nil {
			_ = guard.Close()
		}
	}()
	p, e := syscall.UTF16PtrFromString(`\\?\` + path)
	if e != nil {
		return result, nil, e
	}
	handle, e := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return result, nil, e
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	buffer := make([]uint16, 32768)
	n, _, callErr := kernelDLL.NewProc("GetFinalPathNameByHandleW").Call(uintptr(handle), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0)
	if n == 0 || n >= uintptr(len(buffer)) {
		return result, nil, callErr
	}
	canonical := syscall.UTF16ToString(buffer[:n])
	if !strings.HasPrefix(canonical, `\\?\`) || strings.HasPrefix(canonical, `\\?\UNC\`) {
		return result, nil, errors.New("launcher must reside on a local drive")
	}
	canonical = strings.TrimPrefix(canonical, `\\?\`)
	if !strings.EqualFold(filepath.Base(canonical), "Dilla.exe") {
		return result, nil, errors.New("invalid stable launcher identity")
	}
	roaming, e := roamingAppData()
	if e != nil {
		return result, nil, e
	}
	result = layout{stableRoot: filepath.Dir(canonical), userData: filepath.Join(roaming, cfg.ProductName)}
	result.updates = filepath.Join(result.userData, "updates")
	return result, guard, nil
}

func nativeLaunch(selected *selection, args []string, wait bool) (uint32, error) {
	if selected == nil || selected.tree == nil || selected.slot == nil {
		return 0, errors.New("missing verified program selection")
	}
	// CreateProcess receives an explicit application path and an independently
	// escaped command line. No command interpreter, PATH lookup or user executable.
	executable, e := syscall.UTF16PtrFromString(`\\?\` + selected.executable)
	if e != nil {
		return 0, e
	}
	command := syscall.EscapeArg(selected.executable)
	for _, arg := range args {
		command += " " + syscall.EscapeArg(arg)
	}
	line, e := syscall.UTF16PtrFromString(command)
	if e != nil {
		return 0, e
	}
	dir, e := syscall.UTF16PtrFromString(filepath.Dir(selected.executable))
	if e != nil {
		return 0, e
	}
	env := ordinaryEnvironment(os.Environ())
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	for _, entry := range env {
		if strings.ContainsRune(entry, 0) {
			return 0, errors.New("invalid process environment")
		}
	}
	block := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	info := syscall.StartupInfo{Cb: uint32(unsafe.Sizeof(syscall.StartupInfo{}))}
	if wait {
		info.Flags = syscall.STARTF_USESHOWWINDOW
		info.ShowWindow = syscall.SW_HIDE
	}
	var process syscall.ProcessInformation
	if e = syscall.CreateProcess(executable, line, nil, nil, false, syscall.CREATE_UNICODE_ENVIRONMENT, &block[0], dir, &info, &process); e != nil {
		return 0, e
	}
	defer func() { _ = syscall.CloseHandle(process.Process) }()
	defer func() { _ = syscall.CloseHandle(process.Thread) }()
	if !wait {
		return 0, nil
	}
	status, e := syscall.WaitForSingleObject(process.Process, 30_000)
	if e != nil {
		return 0, e
	}
	if status != syscall.WAIT_OBJECT_0 {
		return 0, errors.New("verified update bootstrap did not finish promptly")
	}
	var exit uint32
	if e = syscall.GetExitCodeProcess(process.Process, &exit); e != nil {
		return 0, e
	}
	return exit, nil
}

//nolint:gosec // G103: fixed MessageBoxW ABI receives only static NUL-terminated message/title strings; no private error text is shown.
func showStartupError() {
	user, e := systemDLL("user32.dll")
	if e != nil {
		return
	}
	message, _ := syscall.UTF16PtrFromString("Dilla could not verify its installed program or finish update recovery. Retry starting Dilla. If it still cannot start, close this message and quit any Dilla tray app. Uninstall Dilla in Windows Settings, then reinstall with the official Dilla installer. Your device profile is retained.")
	title, _ := syscall.UTF16PtrFromString("Dilla could not start")
	_, _, _ = user.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
}

func main() {
	args, e := parseArguments(os.Args[1:])
	if e != nil {
		fmt.Fprintln(os.Stderr, "Dilla: unsupported launcher arguments")
		os.Exit(1)
	}
	cfg, e := parseBuildConfig(bootstrapConfig)
	if e != nil {
		fmt.Fprintln(os.Stderr, "Dilla: invalid compiled launcher configuration")
		if args.mode == "launch" {
			showStartupError()
		}
		os.Exit(1)
	}
	if args.mode == "describe" {
		if e = describe(cfg, os.Stdout); e != nil {
			os.Exit(1)
		}
		return
	}
	if strings.HasPrefix(args.mode, "installer-") {
		if e = runMaintenance(cfg, strings.TrimPrefix(args.mode, "installer-"), args.packageManifest); e != nil {
			if errors.Is(e, errIncompleteInitial) {
				os.Exit(10)
			}
			os.Exit(1)
		}
		return
	}
	var operation *operationMutex
	if args.mode == "launch" {
		var acquired bool
		operation, acquired, e = acquireOperationMutex(cfg)
		if e != nil {
			os.Exit(1)
		}
		if !acquired {
			return
		} // Maintenance owns the program: exit without a modal image lock.
	}
	l, guard, e := deriveLayout(cfg)
	if e != nil {
		operation.Close()
		fmt.Fprintln(os.Stderr, "Dilla: installation identity verification failed")
		if args.mode == "launch" {
			showStartupError()
		}
		os.Exit(1)
	}
	code, e := runLauncher(l, cfg, args, os.Stdout, nativeLaunch)
	_ = guard.Close()
	operation.Close()
	if e != nil {
		fmt.Fprintln(os.Stderr, "Dilla: installed update verification failed; retry starting Dilla, or quit and uninstall it in Windows Settings, then reinstall with the official Dilla installer; your device profile is retained")
		if args.mode == "launch" {
			showStartupError()
		}
		os.Exit(1)
	}
	os.Exit(code)
}
