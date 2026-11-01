//go:build linux

package desktoplaunch

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Opening an expected-file FIFO before checking its type can hang startup forever.
func TestPortableTreeRejectsFIFOWithoutOpeningIt(t *testing.T) {
	root := t.TempDir()
	if e := syscall.Mkfifo(filepath.Join(root, "dilla-desktop.exe"), 0600); e != nil {
		t.Fatal(e)
	}
	raw := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program")})
	m, e := VerifyManifest(raw, digest(raw), stableContext)
	if e != nil {
		t.Fatal(e)
	}
	finished := make(chan error, 1)
	go func() {
		guard, err := VerifyTree(root, m)
		if guard != nil {
			_ = guard.Close()
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("accepted FIFO as executable")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("verification blocked opening FIFO")
	}
}
