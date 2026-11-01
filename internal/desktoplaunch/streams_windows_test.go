//go:build windows

package desktoplaunch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

func requireNTFS(t *testing.T, path string) {
	t.Helper()
	volume, e := syscall.UTF16PtrFromString(filepath.VolumeName(path) + `\`)
	if e != nil {
		t.Fatal(e)
	}
	var filesystem [32]uint16
	ok, _, callErr := syscall.NewLazyDLL("kernel32.dll").NewProc("GetVolumeInformationW").Call(uintptr(unsafe.Pointer(volume)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&filesystem[0])), uintptr(len(filesystem)))
	if ok == 0 {
		t.Fatal(callErr)
	}
	if name := syscall.UTF16ToString(filesystem[:]); name != "NTFS" {
		t.Skipf("native NTFS ADS fixture requires NTFS; found %q", name)
	}
}

// These create real NTFS named streams. They must fail against the old verifier
// that saw only directory entries and unnamed-file hashes, and pass once wired.
// Cross-compiling this test on Linux does not prove either native result.
func TestWindowsTreeRejectsFileAndDirectoryNamedStreams(t *testing.T) {
	for _, target := range []string{"dilla-desktop.exe", "resources", "."} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			requireNTFS(t, root)
			if e := os.Mkdir(filepath.Join(root, "resources"), 0700); e != nil {
				t.Fatal(e)
			}
			for path, bytes := range map[string][]byte{filepath.Join(root, "dilla-desktop.exe"): []byte("program"), filepath.Join(root, "resources", "app.asar"): []byte("app")} {
				if e := os.WriteFile(path, bytes, 0600); e != nil {
					t.Fatal(e)
				}
			}
			raw := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program"), fileEntry("resources/app.asar", "app")})
			manifest, e := VerifyManifest(raw, digest(raw), stableContext)
			if e != nil {
				t.Fatal(e)
			}
			clean, e := VerifyTree(root, manifest)
			if e != nil {
				t.Fatalf("default-only NTFS fixture: %v", e)
			}
			if e = clean.Close(); e != nil {
				t.Fatal(e)
			}
			stream := filepath.Join(root, target) + ":payload"
			if e = os.WriteFile(stream, []byte("unlisted executable content"), 0600); e != nil {
				t.Fatalf("create real NTFS named stream: %v", e)
			}
			if bytes, e := os.ReadFile(stream); e != nil || string(bytes) != "unlisted executable content" {
				t.Fatalf("confirm native stream bytes=%q err=%v", bytes, e)
			}
			if guard, e := VerifyTree(root, manifest); e == nil {
				_ = guard.Close()
				t.Fatal("accepted a tree containing a named NTFS $DATA stream")
			}
		})
	}
}
