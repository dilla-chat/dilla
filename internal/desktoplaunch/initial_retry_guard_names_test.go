package desktoplaunch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitialRetryLeaseMembershipUsesRetainedObjectsNotPathLookup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.bin"), []byte("package"), 0600); err != nil {
		t.Fatal(err)
	}
	guard, err := PinInitialRetryTree(root, map[string]bool{".": true, "file.bin": false, "absent.bin": false})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.VerifyRoot(expected); err != nil {
		t.Fatal(err)
	}
	other, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.VerifyRoot(other); err == nil {
		t.Fatal("accepted different archive identity")
	}
	if !guard.HasFile("file.bin") || guard.HasFile("absent.bin") || guard.HasFile("../outside") {
		_ = guard.Close()
		t.Fatal("incorrect retained membership")
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	if guard.HasFile("file.bin") {
		t.Fatal("closed lease remains available")
	}
}
