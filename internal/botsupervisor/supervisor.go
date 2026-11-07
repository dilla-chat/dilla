// Package botsupervisor runs the bundled community Bot as a supervised, sandboxed child of dillad (Bot v2 task 11).
//
// Three processes take part. The serving dillad owns a Supervisor, which decides when the Bot runs: it starts it,
// restarts it with backoff, and stops it with dillad. It cannot start anything itself: it asks a Keeper. In
// production the keeper is `dillad bot-keeper`, a small process dillad starts before it opens any listener and before
// it drops CAP_SETUID and CAP_SETGID; the keeper keeps those two and uses them for one thing, to run `dillad
// bot-launch` as the configured Bot user, with the configured paths, both fixed when the keeper started. bot-launch
// drops every capability and descriptor and execs the Bot. The Bot reaches dillad over a Unix socket; the gateway
// admits it by its peer credentials and a single-use launch token (Peer, Spend, Authenticated).
package botsupervisor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
)

// Backoff, verbatim from the plan: the first restart waits 250 ms, each further one doubles up to 30 s, and a child
// that ran 60 s before it ended resets it.
const (
	BackoffInitial = 250 * time.Millisecond
	BackoffMax     = 30 * time.Second
	HealthyAfter   = 60 * time.Second
)

// StopGrace is how long a stopping Bot has between SIGTERM and SIGKILL. dillabot's own shutdown closes its helper and
// its wasm runtime with a 5 s bound each (cmd/dillabot main.go), so 10 s lets an orderly exit finish; it is well
// inside dilla.service's TimeoutStopSec=45s, which also covers dillad's own 30 s shutdown_grace drain running beside it.
const StopGrace = 10 * time.Second

// ErrRunning is Run's answer while another Run of the same Supervisor is going.
var ErrRunning = errors.New("botsupervisor: already running")

// ErrKeeperGone is a keeper that can start nothing any more: its process ended or its control socket broke. Run
// returns it, so dillad exits and systemd restarts the unit, which is the only way to a new keeper.
var ErrKeeperGone = errors.New("botsupervisor: the bot keeper is gone")

// defaultOpenChild is openPidfd; the supervisor tests, whose fake pids have no process, swap it.
var defaultOpenChild = openPidfd

// childHandle tells whether the started child still lives (a pidfd on Linux). While it does, its pid can name no
// other process, so a peer whose SO_PEERCRED pid is that pid is the child.
type childHandle interface {
	alive() bool
	close()
}

// LaunchSpec is everything a start takes from the caller: the launch token, 32 random bytes the keeper writes into
// the pipe the Bot reads on fd 3. It is fixed-size data that can only become the contents of that pipe. The binary,
// its arguments, the user, the group and every path are the keeper's own configuration, read when it started, so a
// caller that controls the start request still cannot choose what runs or as whom.
type LaunchSpec struct {
	Token [32]byte
}

// Keeper starts and stops the Bot. Signal and Wait act only on children this keeper started; Wait blocks until the
// child ends and describes how ("exit status 1", "signal: killed"), and answers a pid that is not its child with a
// description rather than blocking.
type Keeper interface {
	Start(spec LaunchSpec) (pid int, err error)
	Signal(pid int, sig syscall.Signal) error
	Wait(pid int) (exit string)
}

// Status is a snapshot of the supervised Bot.
type Status struct {
	Running  bool
	PID      int
	Restarts int
	LastExit string
	Since    time.Time
}

// Supervisor runs one Bot child at a time. Status, Restart and the launch gate (Peer, Spend, Authenticated) are safe
// from any goroutine.
type Supervisor struct {
	keeper Keeper
	log    *slog.Logger
	// uid is the Bot's uid, the one the gateway's Bot listener admits (dillad's own with dev_same_user); -1 when the
	// configured user does not resolve, which admits nobody.
	uid int64

	// clock and armed are test seams: armed sees every delay the supervisor waits out (a backoff, a stop grace).
	// openChild is too: it is openPidfd, and the tests' fake pids have none.
	clock     clock.Clock
	armed     func(time.Duration)
	openChild func(pid int) (childHandle, error)

	running sync.Mutex // held for the whole of Run
	restart chan struct{}
	mu      sync.Mutex
	status  Status
	token   [32]byte
	spent   bool
	// child is the running child's handle; nil admits nobody.
	child childHandle
}

// New builds a Supervisor for cfg's Bot over keeper. It starts nothing: Run does.
func New(cfg config.Bot, keeper Keeper, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{keeper: keeper, log: log, uid: botUID(cfg), clock: clock.System(), openChild: defaultOpenChild,
		restart: make(chan struct{}, 1)}
}

func botUID(cfg config.Bot) int64 {
	if cfg.DevSameUser {
		return int64(os.Getuid())
	}
	u, err := user.Lookup(cfg.User)
	if err != nil {
		return -1
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return -1
	}
	return int64(uid)
}

// Status reports the current child.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Restart stops the running child and starts a new one at once, without a backoff. It does nothing when Run is not
// running, and coalesces with a restart already asked for.
func (s *Supervisor) Restart() {
	select {
	case s.restart <- struct{}{}:
	default:
	}
}

// Nudge tells the running child that its installations changed (Bot v2 task 14): SIGHUP through the keeper, on which
// dillabot re-reads its installations once. The Bot polls nothing, and a new installation in a community it has no
// connection for has no other way to reach it. With no living child it does nothing; a failed signal is logged.
func (s *Supervisor) Nudge() {
	s.mu.Lock()
	pid := s.status.PID
	live := s.status.Running && pid > 0 && s.child != nil && s.child.alive()
	s.mu.Unlock()
	if !live {
		return
	}
	if err := s.keeper.Signal(pid, syscall.SIGHUP); err != nil {
		s.log.Warn("could not tell the Bot its installations changed", "pid", pid, "err", err)
	}
}

// Peer reports whether a process connecting to the Bot listener is the current child: its pid is the one the keeper
// started (bot-launch execs the Bot in place, so the pid stays), the child behind that pid still lives (its pidfd,
// opened when it started: a live process's pid cannot have been reused by another), and its uid is the Bot's.
func (s *Supervisor) Peer(pid int32, uid uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current(pid) && s.uid >= 0 && int64(uid) == s.uid
}

// current is whether pid is the running, living child (s.mu held).
func (s *Supervisor) current(pid int32) bool {
	return s.status.Running && pid > 0 && int(pid) == s.status.PID && s.child != nil && s.child.alive()
}

// Spend accepts the current child's launch token once. The comparison runs in constant time over all 32 bytes
// whatever else fails.
func (s *Supervisor) Spend(pid int32, token []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	match := len(token) == len(s.token) && subtle.ConstantTimeCompare(token, s.token[:]) == 1
	if !match || s.spent || !s.current(pid) {
		return false
	}
	s.spent = true
	return true
}

// Authenticated reports whether the current child has spent its launch token.
func (s *Supervisor) Authenticated(pid int32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent && s.current(pid)
}

// Run keeps the Bot running until ctx ends, then stops it (SIGTERM, then SIGKILL after StopGrace) and returns nil.
// A second Run while one is going returns ErrRunning; a Run after the previous one returned starts afresh.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.running.TryLock() {
		return ErrRunning
	}
	defer s.running.Unlock()
	// A Restart asked for while nothing ran has nothing to restart.
	select {
	case <-s.restart:
	default:
	}
	backoff := BackoffInitial
	var delay time.Duration
	for {
		if delay > 0 {
			if !s.sleep(ctx, delay) {
				return nil
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		pid, started, err := s.start()
		if errors.Is(err, ErrKeeperGone) {
			s.log.Error("the bot keeper is gone; stopping so the service restarts", "err", err)
			s.ended("start: "+err.Error(), false)
			return err
		}
		if err != nil {
			s.log.Error("the Bot did not start", "err", err)
			s.ended("start: "+err.Error(), false)
			delay, backoff = backoff, min(2*backoff, BackoffMax)
			continue
		}
		exited := make(chan string, 1)
		go func() { exited <- s.keeper.Wait(pid) }()
		select {
		case how := <-exited:
			s.log.Warn("the Bot exited", "pid", pid, "exit", how)
			s.ended(how, true)
			if s.clock.Since(started) >= HealthyAfter {
				backoff = BackoffInitial
			}
			delay, backoff = backoff, min(2*backoff, BackoffMax)
		case <-s.restart:
			s.log.Info("restarting the Bot", "pid", pid)
			s.ended(s.stop(pid, exited), true)
			backoff, delay = BackoffInitial, 0
		case <-ctx.Done():
			s.log.Info("stopping the Bot", "pid", pid)
			s.ended(s.stop(pid, exited), false)
			return nil
		}
	}
}

// start mints a launch token and asks the keeper for a child. The token is recorded before the keeper is asked, and
// the pid as soon as it answers: until then Peer admits nobody.
func (s *Supervisor) start() (pid int, started time.Time, err error) {
	var spec LaunchSpec
	if _, err := rand.Read(spec.Token[:]); err != nil {
		return 0, time.Time{}, fmt.Errorf("launch token: %w", err)
	}
	s.mu.Lock()
	s.token, s.spent = spec.Token, false
	s.mu.Unlock()
	pid, err = s.keeper.Start(spec)
	clear(spec.Token[:])
	if err != nil {
		s.mu.Lock()
		clear(s.token[:])
		s.mu.Unlock()
		return 0, time.Time{}, err
	}
	started = s.clock.Now()
	// The child cannot have been reaped yet, even if it already exited: the keeper reaps only on a wait op, and this
	// Run issues that only after the pidfd is open. So the pidfd is this child's, not a later holder of its pid.
	child, herr := s.openChild(pid)
	if herr != nil {
		// A Bot whose pidfd cannot be held could never connect (Peer admits nobody): stop it now, and count it as a
		// failed start.
		_ = s.keeper.Signal(pid, syscall.SIGKILL)
		_ = s.keeper.Wait(pid)
		return 0, time.Time{}, fmt.Errorf("pidfd for pid %d: %w", pid, herr)
	}
	s.mu.Lock()
	s.status.Running, s.status.PID, s.status.Since = true, pid, started
	s.child = child
	s.mu.Unlock()
	s.log.Info("the Bot started", "pid", pid)
	return pid, started, nil
}

// ended records how the last child went and retires its pid and token.
func (s *Supervisor) ended(how string, restart bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Running, s.status.PID, s.status.LastExit = false, 0, how
	if restart {
		s.status.Restarts++
	}
	clear(s.token[:])
	s.spent = false
	if s.child != nil {
		s.child.close()
		s.child = nil
	}
}

// stop sends SIGTERM, waits StopGrace, then SIGKILL, and returns how the child ended.
func (s *Supervisor) stop(pid int, exited <-chan string) string {
	if err := s.keeper.Signal(pid, syscall.SIGTERM); err != nil {
		s.log.Warn("could not SIGTERM the Bot", "pid", pid, "err", err)
	}
	t := s.clock.NewTimer(StopGrace)
	if s.armed != nil {
		s.armed(StopGrace)
	}
	defer t.Stop()
	select {
	case how := <-exited:
		return how
	case <-t.C():
	}
	s.log.Warn("the Bot did not stop within the grace; killing it", "pid", pid, "grace", StopGrace)
	if err := s.keeper.Signal(pid, syscall.SIGKILL); err != nil {
		s.log.Error("could not SIGKILL the Bot", "pid", pid, "err", err)
	}
	return <-exited
}

// sleep waits d, or until ctx ends (false). A Restart during a backoff cuts it short.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	t := s.clock.NewTimer(d)
	if s.armed != nil {
		s.armed(d)
	}
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.restart:
		return true
	case <-t.C():
		return true
	}
}
