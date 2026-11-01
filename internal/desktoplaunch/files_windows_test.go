//go:build windows

package desktoplaunch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

func TestWindowsRejectsDirectoryJunction(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	junction := filepath.Join(root, "resources")
	if e := os.Mkdir(junction, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(outside, "app.asar"), []byte("app"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "dilla-desktop.exe"), []byte("program"), 0600); e != nil {
		t.Fatal(e)
	}
	path, e := syscall.UTF16PtrFromString(junction)
	if e != nil {
		t.Fatal(e)
	}
	h, e := syscall.CreateFile(path, syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if e != nil {
		t.Fatal(e)
	}
	// Mount-point reparse data: two NUL-terminated UTF16 names after an 8-byte
	// mount-point header. Unlike symlinks, a directory junction needs no privilege.
	substitute, e := syscall.UTF16FromString(`\??\` + outside)
	if e != nil {
		_ = syscall.CloseHandle(h)
		t.Fatal(e)
	}
	printName, e := syscall.UTF16FromString(outside)
	if e != nil {
		_ = syscall.CloseHandle(h)
		t.Fatal(e)
	}
	buffer := make([]byte, 16+2*(len(substitute)+len(printName)))
	binary.LittleEndian.PutUint32(buffer, 0xA0000003)
	binary.LittleEndian.PutUint16(buffer[4:], uint16(len(buffer)-8))
	binary.LittleEndian.PutUint16(buffer[10:], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:], uint16(2*(len(printName)-1)))
	for i, c := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], c)
	}
	var returned uint32
	e = syscall.DeviceIoControl(h, 0x000900A4, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
	_ = syscall.CloseHandle(h)
	if e != nil {
		t.Fatal(e)
	}
	defer os.Remove(junction)
	raw := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program"), fileEntry("resources/app.asar", "app")})
	m, e := VerifyManifest(raw, digest(raw), stableContext)
	if e != nil {
		t.Fatal(e)
	}
	if guard, e := VerifyTree(root, m); e == nil {
		_ = guard.Close()
		t.Fatal("accepted directory junction")
	}
}

// These tests run on native Windows. Cross-compilation does not exercise sharing,
// reparse handling or actual security descriptors and cannot satisfy this gate.
func TestWindowsTreePinsFilesUntilClose(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "dilla-desktop.exe")
	if e := os.WriteFile(path, []byte("program"), 0600); e != nil {
		t.Fatal(e)
	}
	raw := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program")})
	manifest, e := VerifyManifest(raw, digest(raw), stableContext)
	if e != nil {
		t.Fatal(e)
	}
	guard, e := VerifyTree(root, manifest)
	if e != nil {
		t.Fatal(e)
	}
	defer guard.Close()
	if f, e := os.OpenFile(path, os.O_WRONLY, 0); e == nil {
		_ = f.Close()
		t.Fatal("verified executable remains writable")
	}
	if e := os.Rename(path, path+".moved"); e == nil {
		t.Fatal("verified executable remains replaceable")
	}
	if e := os.Rename(root, root+".moved"); e == nil {
		t.Fatal("verified directory remains replaceable")
	}
	if e = guard.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte("changed"), 0600); e != nil {
		t.Fatalf("guard did not release sharing lock: %v", e)
	}
}

func TestWindowsRejectsWritableUntrustedDACL(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "dilla-desktop.exe")
	if e := os.WriteFile(path, []byte("program"), 0600); e != nil {
		t.Fatal(e)
	}
	// Set a hostile ACL only on a disposable fixture. Production never changes ACLs.
	convert := securityDLL.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	set := securityDLL.NewProc("SetFileSecurityW")
	sddl, e := syscall.UTF16PtrFromString("D:P(A;;FA;;;WD)")
	if e != nil {
		t.Fatal(e)
	}
	var descriptor uintptr
	result, _, callErr := convert.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	if result == 0 {
		t.Fatal(callErr)
	}
	defer syscall.LocalFree(syscall.Handle(descriptor))
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		t.Fatal(e)
	}
	result, _, callErr = set.Call(uintptr(unsafe.Pointer(p)), 4, descriptor)
	if result == 0 {
		t.Fatal(callErr)
	}
	raw := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program")})
	manifest, e := VerifyManifest(raw, digest(raw), stableContext)
	if e != nil {
		t.Fatal(e)
	}
	if guard, e := VerifyTree(root, manifest); e == nil {
		_ = guard.Close()
		t.Fatal("accepted writable Everyone ACE")
	}
}
