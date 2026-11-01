//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
)

// This subprocess speaks the helper protocol over fd3. The parent uses the REAL
// helper.Client and linuxExchange: cancellation really closes its socket and
// kills the process. Blocking occurs inside a protocol call, never fake HTTP.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--state-dir" {
		if mode, err := os.ReadFile(filepath.Join(os.Args[2], "worker-helper-fixture")); err == nil {
			if err := serveWorkerHelper(os.Args[2], string(mode)); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}
func serveWorkerHelper(dir, mode string) error {
	conn := os.NewFile(3, "worker-helper")
	defer conn.Close()
	blocked := false
	executions := 0
	result := []byte{0x80}
	digest := sha256.Sum256(result)
	transfer := make([]byte, 16)
	transfer[15] = 1
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(prefix[:])
		if size > 1<<20 {
			return errors.New("invalid helper frame")
		}
		frame := make([]byte, 4+int(size))
		copy(frame, prefix[:])
		if _, err := io.ReadFull(conn, frame[4:]); err != nil {
			return err
		}
		req, err := protocol.DecodeHelperRequestFrame(frame)
		if err != nil {
			return err
		}
		payload := []any{}
		shouldBlock := req.Operation == protocol.HelperStateGet && mode == "read"
		if req.Operation == protocol.HelperGroupTransfer {
			stage := req.Payload[0].(uint64)
			switch stage {
			case 0:
				payload = []any{transfer}
			case 1:
				payload = []any{req.Payload[2].(uint64) + uint64(len(req.Payload[3].([]byte)))}
			case 2:
				shouldBlock = mode == "apply"
				payload = []any{uint64(0), uint64(len(result)), digest[:]}
			case 3:
				payload = []any{result}
			}
		}
		if shouldBlock && !blocked {
			blocked = true
			if err = os.WriteFile(filepath.Join(dir, "entered"), []byte("1"), 0600); err != nil {
				return err
			}
			for {
				if _, err = os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		if req.Operation == protocol.HelperGroupTransfer && req.Payload[0] == uint64(2) {
			executions++
			if err = os.WriteFile(filepath.Join(dir, "executions"), []byte(strconv.Itoa(executions)), 0600); err != nil {
				return err
			}
		}
		response, err := protocol.EncodeHelperResponse(protocol.HelperResponse{RequestID: req.RequestID, Payload: payload})
		if err != nil {
			return err
		}
		for len(response) > 0 {
			n, e := conn.Write(response)
			if e != nil {
				return e
			}
			response = response[n:]
		}
		if req.Operation == protocol.HelperShutdown {
			return nil
		}
	}
}

type workerHelperFixture struct {
	client *helper.Client
	dir    string
}

func newWorkerHelperFixture(t *testing.T, mode string) workerHelperFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker-helper-fixture"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := helper.Start(helper.Options{Executable: exe, StateDir: dir, CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f := workerHelperFixture{h, dir}
	t.Cleanup(func() { f.release(t); _ = h.Close() })
	if _, err = h.Open(t.Context(), id.New(), id.New(), id.New(), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f workerHelperFixture) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "release"), []byte("1"), 0600); err != nil {
		t.Error(err)
	}
}
func (f workerHelperFixture) entered(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(f.dir, "entered")); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("helper never entered call")
}
func TestCommandHintDuringCatchupDoesNotCloseHelper(t *testing.T) { testCooperativeHelper(t, false) }
func TestGroupApplyFinishesBeforeCooperativeStop(t *testing.T)    { testCooperativeHelper(t, true) }
func testCooperativeHelper(t *testing.T, apply bool) {
	r := commandRuntime(t)
	mode := "read"
	if apply {
		mode = "apply"
	}
	f := newWorkerHelperFixture(t, mode)
	installation, catchup, command := id.New(), id.New(), id.New()
	done := make(chan error, 4)
	answered := make(chan error, 1)
	var applied atomic.Bool
	r.SetInteractionPoller(installation, func(ctx context.Context, group id.ID) error {
		if group == command {
			_, err := f.client.Call(ctx, protocol.HelperStateGet, nil)
			answered <- err
			return err
		}
		if applied.Load() {
			return nil
		}
		var err error
		if apply {
			_, err = f.client.GroupApply(ctx, []any{uint64(123)})
		} else {
			_, err = f.client.Call(ctx, protocol.HelperStateGet, nil)
		}
		if err == nil {
			applied.Store(true)
		}
		done <- err
		if err == nil {
			return client.CooperativeCheckpoint(ctx)
		}
		return err
	})
	r.notifyBackgroundInteraction(installation, catchup)
	f.entered(t)
	r.notifyInteraction(installation, command)
	select {
	case err := <-done:
		t.Fatalf("hint interrupted running helper call: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	f.release(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("helper operation outcome lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("helper did not finish")
	}
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("helper unavailable after catchup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not run after helper finished")
	}
	if apply {
		body, err := os.ReadFile(filepath.Join(f.dir, "executions"))
		if err != nil || strings.TrimSpace(string(body)) != "1" {
			t.Fatalf("GroupApply executions=%s err=%v", body, err)
		}
	}
}
func TestRepublishingPollerDoesNotInterruptOrRerunCommand(t *testing.T) {
	r := commandRuntime(t)
	f := newWorkerHelperFixture(t, "read")
	installation, group := id.New(), id.New()
	done := make(chan error, 1)
	var first, replacement atomic.Int32
	r.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error {
		first.Add(1)
		_, err := f.client.Call(ctx, protocol.HelperStateGet, nil)
		done <- err
		return err
	})
	r.notifyInteraction(installation, group)
	f.entered(t)
	next := func(context.Context, id.ID) error { replacement.Add(1); return nil }
	for range 10 {
		r.SetInteractionPoller(installation, next)
	}
	select {
	case err := <-done:
		t.Fatalf("reconcile interrupted command: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	f.release(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not finish")
	}
	if first.Load() != 1 || replacement.Load() != 0 {
		t.Fatalf("command replayed: first=%d replacement=%d", first.Load(), replacement.Load())
	}
	if _, err := f.client.Call(t.Context(), protocol.HelperStateGet, nil); err != nil {
		t.Fatalf("republish closed helper: %v", err)
	}
}

func TestCommandWorkerShutdownLetsInFlightHelperFinish(t *testing.T) {
	for _, removal := range []bool{false, true} {
		name := "shutdown"
		if removal {
			name = "installation_removal"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := newBotGatewayRuntime(ctx, nil, true, nil)
			r.StartCommandWorkers()
			f := newWorkerHelperFixture(t, "read")
			t.Cleanup(r.Close)
			installation, group := id.New(), id.New()
			result := make(chan error, 1)
			r.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error {
				_, err := f.client.Call(ctx, protocol.HelperStateGet, nil)
				result <- err
				return err
			})
			r.notifyInteraction(installation, group)
			f.entered(t)
			closed := make(chan struct{})
			go func() {
				if removal {
					r.Drop(installation)
				} else {
					cancel()
					r.Close()
				}
				close(closed)
			}()
			select {
			case err := <-result:
				t.Fatalf("retirement interrupted helper: %v", err)
			case <-closed:
				t.Fatal("retirement returned before active helper finished")
			case <-time.After(30 * time.Millisecond):
			}
			f.release(t)
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("helper did not complete")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("worker did not stop after helper returned")
			}
			if _, err := f.client.Call(t.Context(), protocol.HelperStateGet, nil); err != nil {
				t.Fatalf("helper closed by retirement: %v", err)
			}
		})
	}
}
