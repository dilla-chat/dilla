//go:build windows

package main

import (
	"errors"
	"strings"
	"syscall"
	"unsafe"
)

const globalOperationMutexAccess = 0x1f0001 // MUTEX_ALL_ACCESS, not file access rights.

type globalMutexACL struct {
	Revision  byte
	Reserved  byte
	Size      uint16
	Count     uint16
	Reserved2 uint16
}

type globalMutexACE struct {
	Type  byte
	Flags byte
	Size  uint16
}

// acquireGlobalOperationMutex requires an already locked OS thread. The caller
// must revalidate durable state even after acquisition: WAIT_ABANDONED is an
// acquired mutex, not evidence that the previous transaction completed. Release
// the returned ownership on that same thread before closing the handle.
//
// The descriptor is checked on the opened kernel object, including existing
// objects for which CreateMutexW ignores our supplied security descriptor.
// Unsafe precreated objects fail closed; this helper never repairs their ACLs.
//
//nolint:gosec // G103: fixed Windows security/mutex ABIs use bounded Go inputs and OS-allocated descriptors freed in this function.
func acquireGlobalOperationMutex(cfg buildConfig) (handle syscall.Handle, acquired bool, err error) {
	if !appIDPattern.MatchString(cfg.AppID) || strings.HasSuffix(cfg.AppID, ".") {
		return 0, false, errors.New("invalid global operation identity")
	}
	// OpenCurrentProcessToken uses the process's primary token, not a thread
	// impersonation token or any environment-provided account identity.
	token, e := syscall.OpenCurrentProcessToken()
	if e != nil {
		return 0, false, errors.New("cannot read global operation token")
	}
	defer func() { _ = token.Close() }()
	user, e := token.GetTokenUser()
	if e != nil || user.User.Sid == nil {
		return 0, false, errors.New("cannot read global operation user")
	}
	sid, e := user.User.Sid.String()
	if e != nil || sid == "" {
		return 0, false, errors.New("invalid global operation user")
	}
	nameText := `Global\Dilla.Install.` + cfg.AppID + "." + sid
	// CreateMutexW names are limited to MAX_PATH, including the terminator.
	if len(nameText) >= 260 {
		return 0, false, errors.New("global operation identity exceeds bound")
	}
	name, e := syscall.UTF16PtrFromString(nameText)
	if e != nil {
		return 0, false, errors.New("invalid global operation name")
	}
	advapi, e := systemDLL("advapi32.dll")
	if e != nil {
		return 0, false, errors.New("cannot load global operation security")
	}
	// Explicit owner avoids an elevated token's default Administrators owner.
	// Deduplicate for a SYSTEM process, whose TokenUser already is SYSTEM.
	sddl := "O:" + sid + "D:P"
	for _, principal := range globalOperationPrincipals(sid) {
		sddl += "(A;;0x1f0001;;;" + principal + ")"
	}
	sdText, e := syscall.UTF16PtrFromString(sddl)
	if e != nil {
		return 0, false, errors.New("invalid global operation descriptor")
	}
	var descriptor uintptr
	ok, _, _ := advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(sdText)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	if ok == 0 || descriptor == 0 {
		return 0, false, errors.New("cannot construct global operation security")
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(descriptor)) }()
	attributes := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor, InheritHandle: 0}
	raw, _, _ := kernelDLL.NewProc("CreateMutexW").Call(uintptr(unsafe.Pointer(&attributes)), 0, uintptr(unsafe.Pointer(name)))
	if raw == 0 {
		return 0, false, errors.New("cannot open global operation mutex")
	}
	opened := syscall.Handle(raw)
	defer func() {
		if !acquired {
			_ = syscall.CloseHandle(opened)
		}
	}()
	if e := checkGlobalOperationMutexSecurity(advapi, opened, sid); e != nil {
		return 0, false, e
	}
	var flags uint32
	ok, _, _ = kernelDLL.NewProc("GetHandleInformation").Call(raw, uintptr(unsafe.Pointer(&flags)))
	if ok == 0 || flags&1 != 0 {
		return 0, false, errors.New("unsafe global operation handle")
	}
	status, e := syscall.WaitForSingleObject(opened, 0)
	if e != nil {
		return 0, false, errors.New("cannot wait for global operation mutex")
	}
	if status == syscall.WAIT_TIMEOUT {
		return 0, false, nil
	}
	if status != syscall.WAIT_OBJECT_0 && status != syscall.WAIT_ABANDONED {
		return 0, false, errors.New("cannot acquire global operation mutex")
	}
	return opened, true, nil
}

func globalOperationPrincipals(sid string) []string {
	principals := []string{sid}
	for _, principal := range []string{"S-1-5-18", "S-1-5-32-544"} {
		if principal != sid {
			principals = append(principals, principal)
		}
	}
	return principals
}

//nolint:gosec // G103: GetSecurityInfo returns an OS-owned descriptor; validate it and bound each ACE/SID before reading, then LocalFree the descriptor.
func checkGlobalOperationMutexSecurity(advapi *syscall.LazyDLL, handle syscall.Handle, sid string) error {
	unsafeObject := errors.New("unsafe global operation object security")
	var owner *syscall.SID
	var dacl *globalMutexACL
	var descriptor uintptr
	// SE_KERNEL_OBJECT=6; OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION.
	code, _, _ := advapi.NewProc("GetSecurityInfo").Call(uintptr(handle), 6, 0x1|0x4, uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&descriptor)))
	if code != 0 || descriptor == 0 {
		return unsafeObject
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(descriptor)) }()
	ok, _, _ := advapi.NewProc("IsValidSecurityDescriptor").Call(descriptor)
	if ok == 0 || owner == nil || dacl == nil {
		return unsafeObject
	}
	var control uint16
	var revision uint32
	ok, _, _ = advapi.NewProc("GetSecurityDescriptorControl").Call(descriptor, uintptr(unsafe.Pointer(&control)), uintptr(unsafe.Pointer(&revision)))
	if ok == 0 || control&0x1004 != 0x1004 {
		return unsafeObject
	} // DACL_PRESENT and DACL_PROTECTED.
	actualOwner, e := owner.String()
	if e != nil || actualOwner != sid {
		return unsafeObject
	}
	ok, _, _ = advapi.NewProc("IsValidAcl").Call(uintptr(unsafe.Pointer(dacl)))
	if ok == 0 || dacl.Size < 8 {
		return unsafeObject
	}
	expected := globalOperationPrincipals(sid)
	if int(dacl.Count) != len(expected) {
		return unsafeObject
	}
	seen := make(map[string]bool, len(expected))
	for i := uint16(0); i < dacl.Count; i++ {
		var ace unsafe.Pointer
		ok, _, _ = advapi.NewProc("GetAce").Call(uintptr(unsafe.Pointer(dacl)), uintptr(i), uintptr(unsafe.Pointer(&ace)))
		if ok == 0 {
			return unsafeObject
		}
		start, address := uintptr(unsafe.Pointer(dacl)), uintptr(ace)
		end := start + uintptr(dacl.Size)
		if address < start+8 || address > end || end-address < 16 {
			return unsafeObject
		}
		header := (*globalMutexACE)(ace)
		if header.Type != 0 || header.Flags != 0 || header.Size < 16 || uintptr(header.Size) > end-address {
			return unsafeObject
		}
		mask := *(*uint32)(unsafe.Add(ace, 4))
		if mask != globalOperationMutexAccess {
			return unsafeObject
		}
		sidBytes := unsafe.Slice((*byte)(unsafe.Add(ace, 8)), int(header.Size)-8)
		if sidBytes[0] != 1 || int(sidBytes[1])*4+8 != len(sidBytes) {
			return unsafeObject
		}
		principal, e := (*syscall.SID)(unsafe.Add(ace, 8)).String()
		if e != nil || seen[principal] {
			return unsafeObject
		}
		allowed := false
		for _, candidate := range expected {
			if principal == candidate {
				allowed = true
			}
		}
		if !allowed {
			return unsafeObject
		}
		seen[principal] = true
	}
	return nil
}
