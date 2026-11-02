//go:build windows

package main

import (
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

var errIncompleteInitial = errors.New("owned incomplete initial installation")

type operationMutex struct {
	handle       syscall.Handle
	global       syscall.Handle
	lockedThread bool
}

func (m *operationMutex) Close() {
	if m == nil {
		return
	}
	if m.handle != 0 {
		_, _, _ = kernelDLL.NewProc("ReleaseMutex").Call(uintptr(m.handle))
		_ = syscall.CloseHandle(m.handle)
		m.handle = 0
	}
	if m.global != 0 {
		_, _, _ = kernelDLL.NewProc("ReleaseMutex").Call(uintptr(m.global))
		_ = syscall.CloseHandle(m.global)
		m.global = 0
	}
	if m.lockedThread {
		m.lockedThread = false
		runtime.UnlockOSThread()
	}
}

// The restricted per-user Global gate is acquired before the legacy Local gate.
// Local-only participants remain excluded only within their own session.
//
//nolint:gosec // G103: fixed mutex ABI reads a Go-owned UTF16 compiled-identity name.
func acquireOperationMutex(cfg buildConfig) (owned *operationMutex, acquired bool, errResult error) {
	runtime.LockOSThread()
	var global syscall.Handle
	defer func() {
		if !acquired {
			if global != 0 {
				_, _, _ = kernelDLL.NewProc("ReleaseMutex").Call(uintptr(global))
				_ = syscall.CloseHandle(global)
			}
			runtime.UnlockOSThread()
		}
	}()
	var got bool
	var err error
	global, got, err = acquireGlobalOperationMutex(cfg)
	if err != nil || !got {
		return nil, false, err
	}
	name, err := syscall.UTF16PtrFromString(`Local\Dilla.Install.` + cfg.AppID)
	if err != nil {
		return nil, false, err
	}
	handle, _, callErr := kernelDLL.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return nil, false, callErr
	}
	mutex := &operationMutex{handle: syscall.Handle(handle), global: global, lockedThread: true}
	result, err := syscall.WaitForSingleObject(mutex.handle, 0)
	if err != nil {
		_ = syscall.CloseHandle(mutex.handle)
		return nil, false, err
	}
	if result == syscall.WAIT_TIMEOUT {
		_ = syscall.CloseHandle(mutex.handle)
		return nil, false, nil
	}
	if result != syscall.WAIT_OBJECT_0 && result != syscall.WAIT_ABANDONED {
		_ = syscall.CloseHandle(mutex.handle)
		return nil, false, errors.New("cannot acquire installation operation")
	}
	return mutex, true, nil
}

//nolint:gosec // G103: fixed known-folder ABI returns an OS-owned UTF16 path; CoTaskMemFree releases it.
func maintenanceLayout(cfg buildConfig) (layout, error) {
	var pointer *uint16
	local := syscall.GUID{Data1: 0xf1b32785, Data2: 0x6fba, Data3: 0x4fcf, Data4: [8]byte{0x9d, 0x55, 0x7b, 0x8e, 0x7f, 0x15, 0x70, 0x91}}
	code, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath").Call(uintptr(unsafe.Pointer(&local)), 0, 0, uintptr(unsafe.Pointer(&pointer)))
	if code != 0 || pointer == nil {
		return layout{}, errors.New("cannot resolve local application known folder")
	}
	ole, err := systemDLL("ole32.dll")
	if err != nil {
		return layout{}, err
	}
	defer func() { _, _, _ = ole.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(pointer))) }()
	root := filepath.Join(syscall.UTF16ToString(unsafe.Slice(pointer, 32768)), "Programs", cfg.ProductName)
	roaming, err := roamingAppData()
	if err != nil {
		return layout{}, err
	}
	profile := filepath.Join(roaming, cfg.ProductName)
	return layout{stableRoot: root, userData: profile, updates: filepath.Join(profile, "updates")}, nil
}
func insideMaintenance(root, path string) bool {
	relative, err := filepath.Rel(strings.ToLower(root), strings.ToLower(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, `..\`) && !filepath.IsAbs(relative)
}

// Query identity through the exact process handle; a reused unrelated PID is not
// treated as an owned running helper. No process is terminated by maintenance.
//
//nolint:gosec // G103: fixed QueryFullProcessImageNameW ABI writes a Go-owned bounded buffer.
func maintenanceProcessImage(pid int64, l *layout) (string, error) {
	process, err := syscall.OpenProcess(0x100000|0x1000, false, uint32(pid))
	if errors.Is(err, syscall.Errno(87)) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.CloseHandle(process) }()
	state, err := syscall.WaitForSingleObject(process, 0)
	if err != nil {
		return "", err
	}
	if state == syscall.WAIT_OBJECT_0 {
		return "", nil
	}
	if state != syscall.WAIT_TIMEOUT {
		return "", errors.New("unknown update process status")
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	ok, _, callErr := kernelDLL.NewProc("QueryFullProcessImageNameW").Call(uintptr(process), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 || size == 0 || size >= uint32(len(buffer)) {
		return "", callErr
	}
	image := strings.TrimPrefix(syscall.UTF16ToString(buffer[:size]), `\\?\`)
	// An unrelated image is sufficient to reject PID ownership; no token read
	// from that other application is needed or used as installation authority.
	if l != nil && !insideMaintenance(filepath.Join(l.stableRoot, "versions"), image) && !insideMaintenance(l.updates, image) {
		return image, nil
	}
	var token syscall.Token
	if err = syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer func() { _ = token.Close() }()
	owner, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	current, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer func() { _ = current.Close() }()
	user, err := current.GetTokenUser()
	if err != nil {
		return "", err
	}
	ownerSID, err := owner.User.Sid.String()
	if err != nil {
		return "", err
	}
	userSID, err := user.User.Sid.String()
	if err != nil {
		return "", err
	}
	if ownerSID != userSID {
		return "", nil
	}

	return image, nil
}

var maintenanceTreeID = regexp.MustCompile(`^v-[a-f0-9]{32}$`)

func ownedMaintenanceProcess(l layout, pid int64) (bool, error) {
	image, err := maintenanceProcessImage(pid, &l)
	if err != nil || image == "" {
		return false, err
	}
	nonce, err := activeNonce(l)
	if err != nil {
		return false, err
	}
	owned := false
	if insideMaintenance(filepath.Join(l.stableRoot, "versions"), image) {
		relative, err := filepath.Rel(filepath.Join(l.stableRoot, "versions"), image)
		parts := strings.Split(relative, string(filepath.Separator))
		owned = err == nil && len(parts) == 2 && maintenanceTreeID.MatchString(strings.ToLower(parts[0])) && (parts[1] == "dilla-desktop.exe" || parts[1] == "dilla-update-watchdog.exe")
	}
	if nonce != "" {
		ticket := filepath.Join(l.updates, "startup-"+nonce)
		for _, candidate := range []string{filepath.Join(ticket, "update.exe"), filepath.Join(ticket, "watchdog-app", "dilla-update-watchdog.exe"), filepath.Join(ticket, "previous-app", "dilla-desktop.exe")} {
			if strings.EqualFold(image, candidate) {
				owned = true
			}
		}
	}
	if !owned {
		return false, nil
	}
	guard, err := desktoplaunch.PinOwnedFile(image)
	if err != nil {
		return false, err
	}
	defer func() { _ = guard.Close() }()
	return true, nil
}

// The nonce comes only from the verified private active pointer. The actual
// private installer job, rather than a recycled journal PID, identifies writers.
//
//nolint:gosec // G103: fixed job accounting ABI writes a bounded Go structure.
func maintenanceJobActive(nonce string) (bool, error) {
	name, err := syscall.UTF16PtrFromString(`Local\DillaUpdate-` + nonce)
	if err != nil {
		return false, err
	}
	job, _, callErr := kernelDLL.NewProc("OpenJobObjectW").Call(4, 0, uintptr(unsafe.Pointer(name)))
	if job == 0 {
		if errors.Is(callErr, syscall.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		return false, callErr
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(job)) }()
	var accounting struct {
		Times                                                                     [4]int64
		PageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses uint32
	}
	ok, _, callErr := kernelDLL.NewProc("QueryInformationJobObject").Call(job, 1, uintptr(unsafe.Pointer(&accounting)), unsafe.Sizeof(accounting), 0)
	if ok == 0 {
		return false, callErr
	}
	return accounting.ActiveProcesses != 0, nil
}

// This snapshot validates every owned descendant's owner/DACL, type, hardlinks
// and streams. The NSIS mutex excludes cooperating launch/install operations.
// Current-user/admin malicious mutation remains outside this bounded guarantee.
func checkMaintenanceTrees(l layout) error {
	for _, root := range []string{filepath.Join(l.stableRoot, "versions"), filepath.Join(l.stableRoot, "manifests"), l.updates} {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				guard, err := desktoplaunch.PinOwnedDirectory(path)
				if err != nil {
					return err
				}
				return guard.Close()
			}
			guard, err := desktoplaunch.PinOwnedFile(path)
			if err != nil {
				return err
			}
			return guard.Close()
		}); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func runMaintenance(cfg buildConfig, mode, packageManifest string) error {
	l, err := maintenanceLayout(cfg)
	if err != nil {
		return err
	}
	err = maintenancePreflight(l, cfg, mode, func(pid int64) (bool, error) { return ownedMaintenanceProcess(l, pid) }, int64(os.Getppid()), packageManifest)
	if mode == "initial" {
		_, markerErr := os.Lstat(filepath.Join(l.userData, "initial-install.json"))
		if err == nil && os.IsNotExist(markerErr) {
			return nil
		}
		if err != nil && !errors.Is(err, errInitialExists) {
			return err
		}
		self, selfErr := os.Executable()
		if selfErr != nil {
			return selfErr
		}
		if retryErr := incompleteInitial(l, cfg, filepath.Dir(self)); retryErr != nil {
			return retryErr
		}
		return errIncompleteInitial
	}
	if err != nil {
		return err
	}
	stable, err := desktoplaunch.PinOwnedFile(filepath.Join(l.stableRoot, "Dilla.exe"))
	if err != nil {
		if mode != "uninstall" || !os.IsNotExist(err) {
			return err
		}
	} else {
		_ = stable.Close()
	}
	if mode == "stage" {
		pending, err := loadPending(l, cfg, time.Now())
		if err != nil || pending == nil {
			return errors.New("missing active stage transaction")
		}
		defer func() { _ = pending.old.Close() }()
		selected, err := selectCommitted(l, cfg, time.Now())
		if err != nil {
			return err
		}
		defer func() { _ = selected.Close() }()
		artifact, err := stagingRelease(l, cfg, pending, selected.slot.Version, packageManifest)
		if err != nil {
			return err
		}
		expected := filepath.Join(l.updates, "startup-"+pending.nonce, "update.exe")
		image, err := maintenanceProcessImage(int64(os.Getppid()), nil)
		if err != nil {
			return err
		}
		if !strings.EqualFold(image, expected) {
			return errors.New("stage parent is not the owned installer")
		}
		guard, err := desktoplaunch.PinOwnedFile(expected)
		if err != nil {
			return err
		}
		defer func() { _ = guard.Close() }()
		file, err := os.Open(expected) // #nosec G304 -- Fixed active nonce installer path stays authenticated and pinned.
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		hash := sha512.New()
		size, err := io.Copy(hash, io.LimitReader(file, artifact.Size+1))
		if err != nil {
			return err
		}
		if size != artifact.Size || base64.StdEncoding.EncodeToString(hash.Sum(nil)) != artifact.SHA512 {
			return errors.New("stage parent artifact changed")
		}
	}
	if err = checkMaintenanceTrees(l); err != nil {
		return err
	}
	if mode == "uninstall" {
		if _, markerErr := os.Lstat(filepath.Join(l.userData, "initial-install.json")); markerErr == nil {
			guard, err := desktoplaunch.PinOwnedFile(filepath.Join(l.userData, "initial-install.json"))
			if err != nil {
				return err
			}
			_ = guard.Close()
		} else if !os.IsNotExist(markerErr) {
			return markerErr
		}
		nonce, err := activeNonce(l)
		if err != nil {
			return err
		}
		if nonce != "" {
			active, err := maintenanceJobActive(nonce)
			if err != nil {
				return err
			}
			if active {
				return errors.New("private installer job remains active")
			}
		}
	}
	return nil
}
