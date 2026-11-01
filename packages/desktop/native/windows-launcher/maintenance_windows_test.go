//go:build windows

package main

import (
	"fmt"
	"os"
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
