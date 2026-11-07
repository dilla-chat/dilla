package botsupervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
)

// fakeKeeper starts pretend children. Each Start hands back a new pid; the test ends a child with exit, and Signal
// records what the supervisor sent (a SIGKILL, or a SIGTERM when exitOnTerm, ends the child at once).
type fakeKeeper struct {
	mu         sync.Mutex
	next       int
	tokens     [][32]byte
	signals    []syscall.Signal
	exits      map[int]chan string
	started    chan int
	exitOnTerm bool
	startErr   error
}

func newFakeKeeper() *fakeKeeper {
	return &fakeKeeper{next: 100, exits: map[int]chan string{}, started: make(chan int, 64), exitOnTerm: true}
}

func (k *fakeKeeper) Start(spec LaunchSpec) (int, error) {
	k.mu.Lock()
	if k.startErr != nil {
		err := k.startErr
		k.mu.Unlock()
		return 0, err
	}
	k.next++
	pid := k.next
	k.tokens = append(k.tokens, spec.Token)
	k.exits[pid] = make(chan string, 1)
	k.mu.Unlock()
	k.started <- pid
	return pid, nil
}

func (k *fakeKeeper) Signal(pid int, sig syscall.Signal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.signals = append(k.signals, sig)
	ch, ok := k.exits[pid]
	if !ok {
		return errors.New("not ours")
	}
	if sig == syscall.SIGKILL || (sig == syscall.SIGTERM && k.exitOnTerm) {
		select {
		case ch <- "signal: " + sig.String():
		default:
		}
	}
	return nil
}

func (k *fakeKeeper) Wait(pid int) string {
	k.mu.Lock()
	ch := k.exits[pid]
	k.mu.Unlock()
	return <-ch
}

func (k *fakeKeeper) exit(pid int, how string) {
	k.mu.Lock()
	ch := k.exits[pid]
	k.mu.Unlock()
	ch <- how
}

func (k *fakeKeeper) waitStart(t *testing.T) int {
	t.Helper()
	select {
	case pid := <-k.started:
		return pid
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not start a child")
		return 0
	}
}

// The fake keeper's pids name no process: the supervisor tests hold a handle that is always alive instead of a pidfd
// (TestSupervisorLaunchGate sets its own).
func init() {
	defaultOpenChild = func(int) (childHandle, error) { return &fakeHandle{}, nil }
}

func testBotConfig() config.Bot {
	c := config.Default().Bot
	c.Enabled = true
	c.DevSameUser = true
	return c
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitDelay reads the next backoff the supervisor armed.
func waitDelay(t *testing.T, delays <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case d := <-delays:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor armed no backoff")
		return 0
	}
}

func TestSupervisorRestartsWithBackoff(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	keeper := newFakeKeeper()
	s := New(testBotConfig(), keeper, quietLog())
	s.clock = clk
	delays := make(chan time.Duration, 64)
	s.armed = func(d time.Duration) { delays <- d }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	want := []time.Duration{
		250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	pid := keeper.waitStart(t)
	for i, d := range want {
		keeper.exit(pid, "exit status 1")
		if got := waitDelay(t, delays); got != d {
			t.Fatalf("restart %d backed off %s, want %s", i+1, got, d)
		}
		if st := s.Status(); st.Running || st.Restarts != i+1 || st.LastExit != "exit status 1" {
			t.Fatalf("status while backing off = %+v, want not running, %d restarts, the exit", st, i+1)
		}
		clk.Advance(d)
		pid = keeper.waitStart(t)
	}
	// Sixty healthy seconds reset the backoff.
	clk.Advance(60 * time.Second)
	keeper.exit(pid, "exit status 2")
	if got := waitDelay(t, delays); got != 250*time.Millisecond {
		t.Fatalf("after 60 s healthy the backoff is %s, want 250ms", got)
	}
	clk.Advance(250 * time.Millisecond)
	pid = keeper.waitStart(t)
	if st := s.Status(); !st.Running || st.PID != pid || !st.Since.Equal(clk.Now()) {
		t.Fatalf("status of a running child = %+v, want running pid %d since %s", st, pid, clk.Now())
	}
	// 59 s is not healthy: the backoff carries on from 500 ms.
	clk.Advance(59 * time.Second)
	keeper.exit(pid, "exit status 3")
	if got := waitDelay(t, delays); got != 500*time.Millisecond {
		t.Fatalf("after 59 s the backoff is %s, want 500ms", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil after its context ended", err)
	}
}

// A failed start backs off like an exit.
func TestSupervisorBacksOffAFailedStart(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	keeper := newFakeKeeper()
	keeper.startErr = errors.New("no such user")
	s := New(testBotConfig(), keeper, quietLog())
	s.clock = clk
	delays := make(chan time.Duration, 8)
	s.armed = func(d time.Duration) { delays <- d }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	if got := waitDelay(t, delays); got != 250*time.Millisecond {
		t.Fatalf("first failed start backs off %s, want 250ms", got)
	}
	if st := s.Status(); st.Running || st.LastExit != "start: no such user" {
		t.Fatalf("status = %+v", st)
	}
	clk.Advance(250 * time.Millisecond)
	if got := waitDelay(t, delays); got != 500*time.Millisecond {
		t.Fatalf("second failed start backs off %s, want 500ms", got)
	}
}

func TestSupervisorStopsChildWithDillad(t *testing.T) {
	t.Run("SIGTERM is enough", func(t *testing.T) {
		keeper := newFakeKeeper()
		s := New(testBotConfig(), keeper, quietLog())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		keeper.waitStart(t)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after its context ended")
		}
		keeper.mu.Lock()
		defer keeper.mu.Unlock()
		if len(keeper.signals) != 1 || keeper.signals[0] != syscall.SIGTERM {
			t.Fatalf("signals = %v, want one SIGTERM", keeper.signals)
		}
		if st := s.Status(); st.Running || st.PID != 0 {
			t.Fatalf("status after stop = %+v", st)
		}
	})
	t.Run("SIGKILL after the grace", func(t *testing.T) {
		clk := clock.NewFake(time.Unix(1_800_000_000, 0))
		keeper := newFakeKeeper()
		keeper.exitOnTerm = false
		s := New(testBotConfig(), keeper, quietLog())
		s.clock = clk
		delays := make(chan time.Duration, 8)
		s.armed = func(d time.Duration) { delays <- d }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		keeper.waitStart(t)
		cancel()
		if got := waitDelay(t, delays); got != StopGrace {
			t.Fatalf("stop armed %s, want the %s grace", got, StopGrace)
		}
		clk.Advance(StopGrace)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after SIGKILL")
		}
		keeper.mu.Lock()
		defer keeper.mu.Unlock()
		if len(keeper.signals) != 2 || keeper.signals[0] != syscall.SIGTERM || keeper.signals[1] != syscall.SIGKILL {
			t.Fatalf("signals = %v, want SIGTERM then SIGKILL", keeper.signals)
		}
	})
}

// Restart stops the running child and starts the next one at once; it may be called from any goroutine.
func TestSupervisorRestartReplacesTheChild(t *testing.T) {
	keeper := newFakeKeeper()
	s := New(testBotConfig(), keeper, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	first := keeper.waitStart(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); s.Restart(); _ = s.Status() }()
	}
	wg.Wait()
	second := keeper.waitStart(t)
	if second == first {
		t.Fatalf("Restart kept pid %d", first)
	}
	if st := s.Status(); st.Restarts < 1 {
		t.Fatalf("status after Restart = %+v", st)
	}
}

// Every start gets a fresh 32-byte launch token; the gate admits only the current child, and only once.
func TestSupervisorLaunchGate(t *testing.T) {
	keeper := newFakeKeeper()
	s := New(testBotConfig(), keeper, quietLog())
	handles := map[int]*fakeHandle{}
	var hmu sync.Mutex
	s.openChild = func(pid int) (childHandle, error) {
		hmu.Lock()
		defer hmu.Unlock()
		h := &fakeHandle{}
		handles[pid] = h
		return h, nil
	}
	uid := uint32(syscall.Getuid())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	first := keeper.waitStart(t)
	waitRunning(t, s, first)
	keeper.mu.Lock()
	token := keeper.tokens[0]
	keeper.mu.Unlock()
	if token == ([32]byte{}) {
		t.Fatal("the launch token is all zero")
	}
	if !s.Peer(int32(first), uid) || s.Peer(int32(first)+1, uid) || s.Peer(int32(first), uid+1) {
		t.Fatal("Peer admits by the current child's pid and the Bot uid only")
	}
	if s.Authenticated(int32(first)) {
		t.Fatal("a child is authenticated before it spends its token")
	}
	wrong := token
	wrong[0] ^= 1
	if s.Spend(int32(first), wrong[:]) || s.Spend(int32(first)+1, token[:]) {
		t.Fatal("Spend accepted a wrong token or another pid")
	}
	if !s.Spend(int32(first), token[:]) || !s.Authenticated(int32(first)) {
		t.Fatal("the current token was refused")
	}
	if s.Spend(int32(first), token[:]) {
		t.Fatal("a spent token was accepted again")
	}
	s.Restart()
	second := keeper.waitStart(t)
	waitRunning(t, s, second)
	if s.Peer(int32(first), uid) || s.Authenticated(int32(first)) || s.Spend(int32(second), token[:]) {
		t.Fatal("the previous child's pid or token still counts after a restart")
	}
	keeper.mu.Lock()
	next := keeper.tokens[1]
	keeper.mu.Unlock()
	if bytes.Equal(next[:], token[:]) {
		t.Fatal("a restart reused the launch token")
	}
	if !s.Spend(int32(second), next[:]) {
		t.Fatal("the new child's token was refused")
	}
	// A child that died is no longer the peer, even before the supervisor heard of it: its pid may be anyone's.
	hmu.Lock()
	handles[second].dead.Store(true)
	hmu.Unlock()
	if s.Peer(int32(second), uid) || s.Authenticated(int32(second)) {
		t.Fatal("a dead child's pid still counts")
	}
}

type fakeHandle struct{ dead atomic.Bool }

func (h *fakeHandle) alive() bool { return !h.dead.Load() }
func (h *fakeHandle) close()      {}

// A keeper that is gone ends Run with ErrKeeperGone, so serve exits and systemd restarts the unit.
func TestSupervisorStopsWhenTheKeeperIsGone(t *testing.T) {
	keeper := newFakeKeeper()
	keeper.startErr = fmt.Errorf("%w: control socket: EOF", ErrKeeperGone)
	s := New(testBotConfig(), keeper, quietLog())
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrKeeperGone) {
			t.Fatalf("Run = %v, want ErrKeeperGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going without a keeper")
	}
}

func waitRunning(t *testing.T, s *Supervisor, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.Status(); st.Running && st.PID == pid {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pid %d never showed as running: %+v", pid, s.Status())
}

// Run is not re-entrant: a second Run while one is going refuses, and a later one after it returned works.
func TestSupervisorRunIsExclusive(t *testing.T) {
	keeper := newFakeKeeper()
	s := New(testBotConfig(), keeper, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	keeper.waitStart(t)
	if err := s.Run(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("a second Run = %v, want ErrRunning", err)
	}
	cancel()
	<-done
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = s.Run(ctx2) }()
	keeper.waitStart(t)
}

// A started child whose pidfd cannot be opened could never connect: the supervisor kills it and counts a failed start
// (N-3).
func TestSupervisorKillsAChildItCannotHold(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	keeper := newFakeKeeper()
	keeper.exitOnTerm = false
	s := New(testBotConfig(), keeper, quietLog())
	s.clock = clk
	s.openChild = func(int) (childHandle, error) { return nil, errors.New("pidfd_open: ENOSYS") }
	delays := make(chan time.Duration, 8)
	s.armed = func(d time.Duration) { delays <- d }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	keeper.waitStart(t)
	if got := waitDelay(t, delays); got != BackoffInitial {
		t.Fatalf("backoff after an unholdable child = %s", got)
	}
	keeper.mu.Lock()
	signals := append([]syscall.Signal(nil), keeper.signals...)
	keeper.mu.Unlock()
	if len(signals) != 1 || signals[0] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want the child killed", signals)
	}
	if st := s.Status(); st.Running || !strings.Contains(st.LastExit, "pidfd") {
		t.Fatalf("status = %+v, want a failed start naming the pidfd", st)
	}
}
