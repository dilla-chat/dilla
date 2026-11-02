//go:build windows

package desktoplaunch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsInitialMarkerFinalizationDisposesOnlyExactOwnedRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "initial-install.json")
	expected := []byte(`{"format":1,"kind":"dilla-initial-install"}`)
	if err := os.WriteFile(path, expected, 0600); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeInitialMarker(path, []byte("foreign package")); err == nil {
		t.Fatal("deleted a different setup marker")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != string(expected) {
		t.Fatal("marker changed", err)
	}
	alias := filepath.Join(root, "hardlink")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeInitialMarker(path, expected); err == nil {
		t.Fatal("removed a linked marker")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeInitialMarker(path, expected); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("exact setup marker remains", err)
	}
}
