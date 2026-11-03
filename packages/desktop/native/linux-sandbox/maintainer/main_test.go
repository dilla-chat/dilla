//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUnconfiguredAndUnknownRolesFailBeforeAnyMaintenance(t *testing.T) {
	for _, args := range [][]string{nil, {"--check"}, {"--remove"}, {"--root=/tmp"}, {"--check", "--remove"}, {"--setup-sandbox"}} {
		var out bytes.Buffer
		if code := run(args, false, &out); code == 0 || out.Len() != 0 {
			t.Fatal("unconfigured/unknown role accepted", args, code)
		}
	}
}
func TestBoundedInputStopsWithoutEOFAndPreservesBytes(t *testing.T) {
	fds := make([]int, 2)
	if e := unix.Pipe2(fds, unix.O_CLOEXEC); e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if _, e := unix.Write(fds[1], []byte("exact fixture bytes")); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	r := &boundedStdin{ctx: ctx, fd: fds[0]}
	b := make([]byte, 19)
	if _, e := io.ReadFull(r, b); e != nil || string(b) != "exact fixture bytes" {
		t.Fatal("changed input", e)
	}
	started := time.Now()
	if _, e := r.Read(b); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("unclosed writer unbounded", e)
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline not bounded")
	}
}
