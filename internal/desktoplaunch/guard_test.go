package desktoplaunch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedDirectoryStatRetainsIdentityAfterRename(t *testing.T) {
	parent := t.TempDir()
	original, archived := filepath.Join(parent, "original"), filepath.Join(parent, "archived")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	guard, err := PinOwnedDirectory(original)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Close() }()
	before, err := guard.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = guard.Stat(); err == nil {
		t.Fatal("closed directory guard returned metadata")
	}
	if err = os.Rename(original, archived); err != nil {
		t.Fatal(err)
	}
	archivedGuard, err := PinOwnedDirectory(archived)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archivedGuard.Close() }()
	after, err := archivedGuard.Stat()
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("held directory identity changed after rename", err)
	}
	if err = os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	replacementGuard, err := PinOwnedDirectory(original)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacementGuard.Close() }()
	replacement, err := replacementGuard.Stat()
	if err != nil || os.SameFile(before, replacement) {
		t.Fatal("recreated old path acquired the archived identity", err)
	}
	var missing *DirectoryGuard
	if _, err = missing.Stat(); err == nil {
		t.Fatal("nil directory guard returned metadata")
	}
}

func TestOwnedFileReadsOnlyBoundedRegularContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "slot.json")
	if e := os.WriteFile(path, []byte("contents"), 0600); e != nil {
		t.Fatal(e)
	}
	directory, e := PinOwnedDirectory(root)
	if e != nil {
		t.Fatal(e)
	}
	defer directory.Close()
	guard, e := PinOwnedFile(path)
	if e != nil {
		t.Fatal(e)
	}
	defer guard.Close()
	bytes, e := guard.ReadBounded(8)
	if e != nil || string(bytes) != "contents" {
		t.Fatalf("read %q, %v", bytes, e)
	}
	if _, e = guard.ReadBounded(7); e == nil {
		t.Fatal("read oversized file")
	}
	if _, e = guard.ReadBounded(0); e == nil {
		t.Fatal("accepted zero limit")
	}
	if _, e = guard.ReadBounded(1_048_577); e == nil {
		t.Fatal("accepted excessive limit")
	}
	if g, e := PinOwnedFile(root); e == nil {
		_ = g.Close()
		t.Fatal("accepted directory as file")
	}
	if g, e := PinOwnedDirectory(path); e == nil {
		_ = g.Close()
		t.Fatal("accepted file as directory")
	}
	longName := filepath.Join(root, strings.Repeat("a", 128)+".json")
	if e = os.WriteFile(longName, []byte("manifest"), 0600); e != nil {
		t.Fatal(e)
	}
	if g, e := PinOwnedFile(longName); e != nil {
		t.Fatalf("fixed external manifest name: %v", e)
	} else {
		_ = g.Close()
	}
}
