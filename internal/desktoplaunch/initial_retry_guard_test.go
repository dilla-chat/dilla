package desktoplaunch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInitialRetryLeasesValidateWholeAllowlistAndHeldPrefixes(t *testing.T) {
	for _, fault := range []string{"valid", "unknown", "symlink", "hardlink", "wrong prefix"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "nested", "image.exe")
			if err := os.WriteFile(path, []byte("packaged"), 0600); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "unknown":
				if err := os.WriteFile(filepath.Join(root, "unknown"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(path, filepath.Join(root, "alias")); err != nil {
					t.Skip(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			guard, err := PinInitialRetryTree(root, map[string]bool{".": true, "nested": true, "nested/image.exe": false})
			if fault == "unknown" || fault == "symlink" || fault == "hardlink" {
				if err == nil {
					_ = guard.Close()
					t.Fatal("accepted unowned/linked namespace")
				}
				writable, openErr := os.OpenFile(path, os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatalf("failed acquisition leaked a lease: %v", openErr)
				}
				_ = writable.Close()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = guard.Close() }()
			expected := []byte("packaged image suffix")
			if fault == "wrong prefix" {
				expected = []byte("unrelated")
			}
			err = guard.ComparePrefix("nested/image.exe", bytes.NewReader(expected))
			if (err == nil) != (fault == "valid") {
				t.Fatalf("unexpected prefix result: %v", err)
			}
			if err = guard.ComparePrefix("../outside", bytes.NewReader(nil)); err == nil {
				t.Fatal("accepted unlisted relative file")
			}
		})
	}
}

func TestOwnedFileUsesSameInitialPrefixPolicyWithoutReopeningDestination(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.bin")
	for _, tc := range []struct {
		name   string
		actual []byte
		ok     bool
	}{
		{"prefix", []byte("pack"), true}, {"zero-tail", []byte{'p', 'a', 'c', 0, 0, 0, 0, 0}, true},
		{"unrelated", []byte("other"), false}, {"overlong", []byte("packaged-extra"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.actual, 0600); err != nil {
				t.Fatal(err)
			}
			guard, err := PinOwnedFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = guard.CompareInitialPrefix(bytes.NewReader([]byte("packaged")))
			if closeErr := guard.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected prefix check: %v", err)
			}
		})
	}
}
