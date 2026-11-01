//go:build windows

package desktoplaunch

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"
)

var securityDLL = syscall.NewLazyDLL("advapi32.dll")
var getSecurityInfo = securityDLL.NewProc("GetSecurityInfo")
var getAce = securityDLL.NewProc("GetAce")

type nativeACL struct {
	Revision  byte
	Reserved  byte
	Size      uint16
	Count     uint16
	Reserved2 uint16
}
type nativeACEHeader struct {
	Type  byte
	Flags byte
	Size  uint16
}

//nolint:gosec // G103: Windows token ABI returns owner SID within a size-bounded token buffer, kept alive through SID conversion.
func trustedPrincipals() (map[string]bool, error) {
	token, e := syscall.OpenCurrentProcessToken()
	if e != nil {
		return nil, e
	}
	defer func() { _ = token.Close() }()
	user, e := token.GetTokenUser()
	if e != nil {
		return nil, e
	}
	userSID, e := user.User.Sid.String()
	if e != nil {
		return nil, e
	}
	var size uint32
	e = syscall.GetTokenInformation(token, syscall.TokenOwner, nil, 0, &size)
	if !errors.Is(e, syscall.ERROR_INSUFFICIENT_BUFFER) {
		return nil, errors.New("cannot determine token owner")
	}
	if size < uint32(unsafe.Sizeof(uintptr(0))) || size > 65536 {
		return nil, errors.New("invalid token owner information size")
	}
	buf := make([]byte, size)
	if e = syscall.GetTokenInformation(token, syscall.TokenOwner, &buf[0], size, &size); e != nil {
		return nil, e
	}
	owner := *(**syscall.SID)(unsafe.Pointer(&buf[0]))
	if owner == nil {
		return nil, errors.New("missing token owner")
	}
	ownerSID, e := owner.String()
	runtime.KeepAlive(buf)
	if e != nil {
		return nil, e
	}
	return map[string]bool{userSID: true, ownerSID: true, "S-1-5-18": true, "S-1-5-32-544": true}, nil
}

//nolint:gosec // G103: GetSecurityInfo/GetAce return OS-owned pointers; every ACE range and SID size is checked before dereference, descriptor freed after use.
func checkHandleSecurity(handle syscall.Handle) error {
	trusted, e := trustedPrincipals()
	if e != nil {
		return e
	}
	var owner *syscall.SID
	var dacl *nativeACL
	var descriptor uintptr
	// SE_FILE_OBJECT, OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION.
	code, _, _ := getSecurityInfo.Call(uintptr(handle), 1, 0x1|0x4, uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&descriptor)))
	if code != 0 {
		return syscall.Errno(code)
	}
	if descriptor == 0 {
		return errors.New("missing program security descriptor")
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(descriptor)) }()
	if owner == nil {
		return errors.New("missing program entry owner")
	}
	sid, e := owner.String()
	if e != nil {
		return e
	}
	var grants []accessGrant
	if dacl != nil {
		if dacl.Size < 8 {
			return errors.New("invalid program DACL size")
		}
		for i := uint16(0); i < dacl.Count; i++ {
			var ace unsafe.Pointer
			ok, _, callErr := getAce.Call(uintptr(unsafe.Pointer(dacl)), uintptr(i), uintptr(unsafe.Pointer(&ace)))
			if ok == 0 {
				return callErr
			}
			start := uintptr(unsafe.Pointer(dacl))
			end := start + uintptr(dacl.Size)
			address := uintptr(ace)
			if address < start+8 || address+4 > end {
				return errors.New("invalid program ACE bounds")
			}
			header := (*nativeACEHeader)(ace)
			if header.Size < 4 || address+uintptr(header.Size) > end {
				return errors.New("invalid program ACE size")
			}
			if header.Type == 1 {
				continue
			} // A standard deny ACE never grants mutation authority.
			if header.Type != 0 || header.Size < 16 {
				return errors.New("unsupported program DACL ACE type")
			}
			mask := *(*uint32)(unsafe.Add(ace, 4))
			sidBytes := unsafe.Slice((*byte)(unsafe.Add(ace, 8)), int(header.Size)-8)
			if len(sidBytes) < 8 || int(sidBytes[1])*4+8 > len(sidBytes) {
				return errors.New("invalid program ACE SID")
			}
			principal, e := (*syscall.SID)(unsafe.Add(ace, 8)).String()
			if e != nil {
				return e
			}
			grants = append(grants, accessGrant{SID: principal, Mask: mask, InheritOnly: header.Flags&0x8 != 0})
		}
	}
	return validateSecurity(sid, dacl == nil, grants, trusted)
}
