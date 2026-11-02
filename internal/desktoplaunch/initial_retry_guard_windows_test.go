//go:build windows

package desktoplaunch

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestInitialRetryLeaseMappedChild(t *testing.T) {
	if os.Getenv("DILLA_TEST_INITIAL_LEASE_CHILD") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("DILLA_TEST_INITIAL_LEASE_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func TestWindowsInitialRetryLeaseRejectsMappedImageAndPreventsNewMapping(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "image.exe")
	ready := filepath.Join(t.TempDir(), "ready")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	err = destination.Close()
	_ = source.Close()
	if copyErr != nil || err != nil {
		t.Fatalf("copy fixture: %v %v", copyErr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "-test.run=^TestInitialRetryLeaseMappedChild$")
	command.Env = append(os.Environ(), "DILLA_TEST_INITIAL_LEASE_CHILD=1", "DILLA_TEST_INITIAL_LEASE_READY="+ready)
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, e := os.ReadFile(ready); e == nil && string(raw) == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned mapped child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	expected := map[string]bool{".": true, "image.exe": false}
	if guard, e := PinInitialRetryTree(root, expected); e == nil {
		_ = guard.Close()
		t.Fatal("leased a mapped image for mutation")
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if command.ProcessState == nil || !command.ProcessState.Exited() {
		t.Fatal("owned mapped child exit was not verified")
	}
	exited = true
	guard, err := PinInitialRetryTree(root, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Close() }()
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer blockedCancel()
	if err = blockedCtx.Err(); err != nil {
		t.Fatal("mapping exclusion control expired before CreateProcess", err)
	}
	blocked := exec.CommandContext(blockedCtx, path, "-test.run=^TestInitialRetryLeaseMappedChild$")
	blocked.Env = command.Env
	if err = blocked.Start(); err == nil {
		_ = blocked.Process.Kill()
		_ = blocked.Wait()
		t.Fatal("retained mutation lease allowed a new image mapping")
	}
	if err = blockedCtx.Err(); err != nil {
		t.Fatal("mapping exclusion was not proved: CreateProcess context expired", err)
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	writable, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal("lease did not close", err)
	}
	_ = writable.Close()
}
