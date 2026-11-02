//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestNativeOperationMutexExcludesAnotherThreadUntilRelease(t *testing.T) {
	cfg := buildConfig{AppID: fmt.Sprintf("dev.dilla.mutex.fixture.%d", os.Getpid())}
	guard, acquired, err := acquireOperationMutex(cfg)
	if err != nil || !acquired {
		t.Fatalf("first mutex: %v %v", acquired, err)
	}
	defer guard.Close()
	result := make(chan error, 1)
	go func() {
		other, got, err := acquireOperationMutex(cfg)
		if other != nil {
			other.Close()
		}
		if err == nil && got {
			err = fmt.Errorf("acquired owned mutex from another thread")
		}
		result <- err
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	guard.Close()
	replacement, acquired, err := acquireOperationMutex(cfg)
	if err != nil || !acquired {
		t.Fatalf("released mutex remains busy: %v %v", acquired, err)
	}
	replacement.Close()
}

func TestNativeOperationAlsoOwnsGlobalGateUntilRelease(t *testing.T) {
	cfg := buildConfig{AppID: fmt.Sprintf("dev.dilla.global.integration.%d", os.Getpid())}
	operation, acquired, err := acquireOperationMutex(cfg)
	if err != nil || !acquired {
		t.Fatalf("operation: %v %v", acquired, err)
	}
	defer operation.Close()
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		handle, got, e := acquireGlobalOperationMutex(cfg)
		if got {
			_, _, _ = kernelDLL.NewProc("ReleaseMutex").Call(uintptr(handle))
			_ = syscall.CloseHandle(handle)
			e = fmt.Errorf("operation did not retain Global gate")
		}
		result <- e
	}()
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	operation.Close()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	handle, got, err := acquireGlobalOperationMutex(cfg)
	if err != nil || !got {
		t.Fatalf("Global gate remained held: %v %v", got, err)
	}
	_, _, _ = kernelDLL.NewProc("ReleaseMutex").Call(uintptr(handle))
	_ = syscall.CloseHandle(handle)
}
