//go:build windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func globalMutexTestConfig(t *testing.T) buildConfig {
	t.Helper()
	return buildConfig{AppID: fmt.Sprintf("dev.dilla.global.%d.%d", os.Getpid(), time.Now().UnixNano())}
}

func releaseGlobalTestMutex(t *testing.T, h syscall.Handle) {
	t.Helper()
	ok, _, _ := kernelDLL.NewProc("ReleaseMutex").Call(uintptr(h))
	if ok == 0 {
		t.Error("release global test mutex failed")
	}
	if err := syscall.CloseHandle(h); err != nil {
		t.Error("close global test mutex failed")
	}
}

// All subprocess selection and identities are test-only. Production derives its
// identity exclusively from the compiled AppID and the actual process token.
func TestGlobalOperationMutexProcessHelper(t *testing.T) {
	mode := os.Getenv("DILLA_TEST_GLOBAL_MUTEX_MODE")
	if mode == "" {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h, acquired, err := acquireGlobalOperationMutex(buildConfig{AppID: os.Getenv("DILLA_TEST_GLOBAL_MUTEX_APP")})
	if err != nil {
		os.Exit(21)
	}
	if mode == "busy" {
		if acquired || h != 0 {
			os.Exit(22)
		}
		return
	}
	if !acquired || h == 0 {
		os.Exit(23)
	}
	if mode == "abandon" {
		fmt.Println("owned")
		var signal [1]byte
		if _, err := os.Stdin.Read(signal[:]); err != nil {
			os.Exit(24)
		}
		os.Exit(0) // Deliberately abandon an owned mutex without ReleaseMutex.
	}
	if mode != "acquire" {
		os.Exit(25)
	}
	releaseGlobalTestMutex(t, h)
}

func globalMutexChild(t *testing.T, cfg buildConfig, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGlobalOperationMutexProcessHelper$")
	cmd.Env = append(os.Environ(), "DILLA_TEST_GLOBAL_MUTEX_MODE="+mode, "DILLA_TEST_GLOBAL_MUTEX_APP="+cfg.AppID)
	return cmd
}

func TestGlobalOperationMutexProcessContentionAndRelease(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cfg := globalMutexTestConfig(t)
	h, acquired, err := acquireGlobalOperationMutex(cfg)
	if err != nil || !acquired || h == 0 {
		t.Fatal("initial global acquisition failed", err)
	}
	defer func() {
		if h != 0 {
			releaseGlobalTestMutex(t, h)
		}
	}()
	if err := globalMutexChild(t, cfg, "busy").Run(); err != nil {
		t.Fatal("second process did not observe contention", err)
	}
	releaseGlobalTestMutex(t, h)
	h = 0
	if err := globalMutexChild(t, cfg, "acquire").Run(); err != nil {
		t.Fatal("second process could not acquire after release", err)
	}
}

func TestGlobalOperationMutexAbandonment(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cfg := globalMutexTestConfig(t)
	cmd := globalMutexChild(t, cfg, "abandon")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "owned" {
		t.Fatal("child did not acquire mutex")
	}
	// Retain the object while its owner exits: otherwise the namespace entry
	// disappears and this would only test creation of a fresh mutex.
	sid := globalMutexActualSID(t)
	name, _ := syscall.UTF16PtrFromString(`Global\Dilla.Install.` + cfg.AppID + "." + sid)
	raw, _, _ := kernelDLL.NewProc("OpenMutexW").Call(0x1f0001, 0, uintptr(unsafe.Pointer(name)))
	if raw == 0 {
		t.Fatal("cannot retain existing mutex")
	}
	defer syscall.CloseHandle(syscall.Handle(raw))
	if _, err := in.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal("abandoning child failed", err)
	}
	h, acquired, err := acquireGlobalOperationMutex(cfg)
	if err != nil || !acquired || h == 0 {
		t.Fatal("abandoned mutex was not acquired", err)
	}
	releaseGlobalTestMutex(t, h)
}

func globalMutexActualSID(t *testing.T) string {
	t.Helper()
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// Create the existing objects independently of the production constructor.
func precreateGlobalTestObject(t *testing.T, cfg buildConfig, sddl string, event bool) syscall.Handle {
	t.Helper()
	advapi, err := systemDLL("advapi32.dll")
	if err != nil {
		t.Fatal(err)
	}
	sdText, _ := syscall.UTF16PtrFromString(sddl)
	var sd uintptr
	ok, _, _ := advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(sdText)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if ok == 0 {
		t.Fatal("test descriptor conversion failed")
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}
	name, _ := syscall.UTF16PtrFromString(`Global\Dilla.Install.` + cfg.AppID + "." + globalMutexActualSID(t))
	var h uintptr
	if event {
		h, _, _ = kernelDLL.NewProc("CreateEventW").Call(uintptr(unsafe.Pointer(&sa)), 0, 0, uintptr(unsafe.Pointer(name)))
	} else {
		h, _, _ = kernelDLL.NewProc("CreateMutexW").Call(uintptr(unsafe.Pointer(&sa)), 0, uintptr(unsafe.Pointer(name)))
	}
	if h == 0 {
		t.Fatal("test object creation failed")
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(syscall.Handle(h)) })
	return syscall.Handle(h)
}

func TestGlobalOperationMutexExistingSecurityAndType(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	sid := globalMutexActualSID(t)
	valid := "O:" + sid + "D:P(A;;0x1f0001;;;" + sid + ")(A;;0x1f0001;;;SY)(A;;0x1f0001;;;BA)"
	for _, tc := range []struct {
		name, sddl   string
		event, valid bool
	}{
		{"valid", valid, false, true},
		{"everyone", valid + "(A;;0x1f0001;;;WD)", false, false},
		{"unprotected", strings.Replace(valid, "D:P", "D:", 1), false, false},
		{"null-dacl", "O:" + sid + "D:NO_ACCESS_CONTROL", false, false},
		{"missing-system-admin", "O:" + sid + "D:P(A;;0x1f0001;;;" + sid + ")", false, false},
		{"wrong-type", valid, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			cfg := globalMutexTestConfig(t)
			precreateGlobalTestObject(t, cfg, tc.sddl, tc.event)
			h, acquired, err := acquireGlobalOperationMutex(cfg)
			if acquired {
				defer releaseGlobalTestMutex(t, h)
			}
			if tc.valid {
				if err != nil || !acquired || h == 0 {
					t.Fatal("valid existing gate refused", err)
				}
			} else if err == nil || acquired || h != 0 {
				t.Fatal("unsafe existing object accepted")
			}
		})
	}
}

func TestGlobalOperationMutexForeignExistingOwner(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cfg := globalMutexTestConfig(t)
	sid := globalMutexActualSID(t)
	if sid == "S-1-5-32-544" {
		t.Skip("requires a process user distinct from Administrators")
	}
	advapi, err := systemDLL("advapi32.dll")
	if err != nil {
		t.Fatal(err)
	}
	// An elevated runner can assign its enabled Administrators group as owner.
	// Standard users cannot forge a foreign owner; do not enable privileges or
	// weaken the OS check just to manufacture that fixture.
	sddl := "O:BAD:P(A;;0x1f0001;;;" + sid + ")(A;;0x1f0001;;;SY)(A;;0x1f0001;;;BA)"
	text, _ := syscall.UTF16PtrFromString(sddl)
	var sd uintptr
	ok, _, _ := advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if ok == 0 {
		t.Fatal("foreign-owner descriptor conversion failed")
	}
	defer syscall.LocalFree(syscall.Handle(sd))
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}
	name, _ := syscall.UTF16PtrFromString(`Global\Dilla.Install.` + cfg.AppID + "." + sid)
	raw, _, callErr := kernelDLL.NewProc("CreateMutexW").Call(uintptr(unsafe.Pointer(&sa)), 0, uintptr(unsafe.Pointer(name)))
	if raw == 0 {
		if callErr == syscall.Errno(1307) {
			t.Skip("process token cannot assign a foreign owner")
		}
		t.Fatal("foreign-owner fixture creation failed")
	}
	defer syscall.CloseHandle(syscall.Handle(raw))
	h, acquired, err := acquireGlobalOperationMutex(cfg)
	if acquired {
		defer releaseGlobalTestMutex(t, h)
	}
	if err == nil || acquired || h != 0 {
		t.Fatal("foreign-owner existing mutex accepted")
	}
}

func TestGlobalOperationMutexTokenIdentityOwnerAndNoninheritance(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// These cannot influence the process token or the production namespace.
	t.Setenv("USERNAME", "untrusted-test-user")
	t.Setenv("USERDOMAIN", "untrusted-test-domain")
	t.Setenv("DILLA_TEST_GLOBAL_MUTEX_SID", "S-1-1-0")
	cfg := globalMutexTestConfig(t)
	h, acquired, err := acquireGlobalOperationMutex(cfg)
	if err != nil || !acquired {
		t.Fatal("acquisition failed", err)
	}
	defer releaseGlobalTestMutex(t, h)
	sid := globalMutexActualSID(t)
	name, _ := syscall.UTF16PtrFromString(`Global\Dilla.Install.` + cfg.AppID + "." + sid)
	opened, _, _ := kernelDLL.NewProc("OpenMutexW").Call(0x1f0001, 0, uintptr(unsafe.Pointer(name)))
	if opened == 0 {
		t.Fatal("gate is not scoped to actual process TokenUser")
	}
	defer syscall.CloseHandle(syscall.Handle(opened))
	var flags uint32
	ok, _, _ := kernelDLL.NewProc("GetHandleInformation").Call(uintptr(h), uintptr(unsafe.Pointer(&flags)))
	if ok == 0 || flags&1 != 0 {
		t.Fatal("mutex handle is inheritable or unreadable")
	}
	advapi, err := systemDLL("advapi32.dll")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGlobalOperationMutexSecurity(advapi, h, "S-1-1-0"); err == nil {
		t.Fatal("kernel-object security check accepted a foreign owner")
	}
}

func TestGlobalOperationMutexRejectsInvalidIdentity(t *testing.T) {
	for _, app := range []string{"", "dev.dilla.", `dev\foreign`, "x\x00y", strings.Repeat("a", 129)} {
		h, acquired, err := acquireGlobalOperationMutex(buildConfig{AppID: app})
		if err == nil || acquired || h != 0 {
			t.Fatal("invalid compiled identity accepted")
		}
		if strings.Contains(err.Error(), app) && app != "" {
			t.Fatal("identity leaked in error")
		}
	}
}
