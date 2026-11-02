//go:build windows

package desktoplaunch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsInitialEnumerationPreservesIdentityAndRestartsBoundedScans(t *testing.T) {
	for _, kind := range []string{"claimed", "pinned"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "owned")
			other := filepath.Join(parent, "other")
			if err := os.Mkdir(other, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "foreign"), []byte("must never enumerate"), 0600); err != nil {
				t.Fatal(err)
			}
			var original *os.File
			if kind == "claimed" {
				claim, err := ClaimInitialUpdateRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer claim.Close()
				original = claim.entries["."].file
			} else {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				guard, err := PinOwnedDirectory(root)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
				original = guard.guard.handles[len(guard.guard.handles)-1]
			}
			expected := map[string]bool{}
			// More than64 entries exercises continuation as well as fresh first-query restart.
			for i := 0; i < 72; i++ {
				name := fmt.Sprintf("entry-%03d", i)
				expected[name] = true
				if err := os.WriteFile(filepath.Join(root, name), []byte("owned fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var alias windows.Handle
			if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(original.Fd()), windows.CurrentProcess(), &alias, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
				t.Fatal(err)
			}
			// The label deliberately names another directory: enumeration must use the
			// retained object only, never resolve os.File.Name as authority.
			labeled := os.NewFile(uintptr(alias), other)
			defer labeled.Close()
			for pass := 0; pass < 2; pass++ {
				if pass == 1 {
					expected["new-owned-child"] = true
					if err := os.WriteFile(filepath.Join(root, "new-owned-child"), []byte("new"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				seen := map[string]bool{}
				if err := readInitialDirectory(labeled, func(entry os.DirEntry) error {
					if !expected[entry.Name()] || seen[entry.Name()] {
						return fmt.Errorf("unexpected or duplicate controlled entry")
					}
					seen[entry.Name()] = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if len(seen) != len(expected) {
					t.Fatalf("fresh scan missed entries: %d, want %d", len(seen), len(expected))
				}
			}
			enumeration, err := openInitialEnumeration(original)
			if err != nil {
				t.Fatal(err)
			}
			var flags uint32
			ok, _, queryErr := streamDLL.NewProc("GetHandleInformation").Call(enumeration.Fd(), uintptr(unsafe.Pointer(&flags)))
			if ok == 0 {
				_ = enumeration.Close()
				t.Fatal("handle inheritance query failed", queryErr)
			}
			if flags&windows.HANDLE_FLAG_INHERIT != 0 {
				_ = enumeration.Close()
				t.Fatal("enumeration clone is inheritable")
			}
			if err = enumeration.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = original.Stat(); err != nil {
				t.Fatal("closing clone invalidated original guard", err)
			}
		})
	}
}
