//go:build linux

package botsupervisor

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/sys/unix"

	"github.com/dilla-chat/dilla/internal/config"
)

// LaunchPATH is the Bot's PATH: the system directories, where ffmpeg, yt-dlp and bubblewrap are installed.
const LaunchPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// launcherSelf is the binary the keeper runs bot-launch from: its own image. It is the dillad that started the keeper,
// even after an upgrade replaced the file on disk.
const launcherSelf = "/proc/self/exe"

// coreFileName is the wasi core's name beside dillad (dillad.CoreFileName; that package is not imported here).
const coreFileName = "dilla_core_wasi.wasm"

// KeeperConfig is everything a start uses. The keeper builds it once, from dilla.toml, when it starts, and nothing on
// its control pipe can change it.
type KeeperConfig struct {
	// Launcher is the binary that runs `bot-launch`: /proc/self/exe.
	Launcher   string
	Executable string
	Helper     string
	WASI       string
	StateDir   string
	Socket     string
	// UID and GID are the Bot user's; with SameUser (dev_same_user) no credential is changed.
	UID, GID uint32
	SameUser bool

	// noSetgroups leaves the supplementary groups alone: only tests set it, because an unprivileged user namespace
	// refuses setgroups. Production clears them.
	noSetgroups bool
	// insecurePaths skips the root-owned ancestor rule: only tests set it (in a user namespace the host's root owns
	// nothing), or a binary built with the dilla_unsafe_test tag (UnsafeTestBuild). Production checks every component.
	insecurePaths bool
	// allowCgoNarrowing lets a cgo test keeper keep its capabilities when it cannot narrow them on every thread.
	allowCgoNarrowing bool
}

// requireRoot is whether the Bot's files and their directories must be root's.
func (k KeeperConfig) requireRoot() bool { return !k.SameUser && !k.insecurePaths }

// NewKeeperConfig resolves b into a launch configuration: the Bot user's uid and primary gid, and the wasi core
// beside dillad (coreDir) when bot.wasi_module is empty. Unless dev_same_user, the user must not be root and every
// file the Bot runs or loads must be root's and not writable by group or others: the serving user must not be able
// to swap in code that would then run as the Bot.
func NewKeeperConfig(b config.Bot, coreDir string) (KeeperConfig, error) {
	k := KeeperConfig{
		Launcher: launcherSelf, Executable: b.Executable, Helper: b.HelperExecutable, WASI: b.WASIModule,
		StateDir: b.StateDir, Socket: b.SocketPath(), SameUser: b.DevSameUser,
	}
	if k.WASI == "" {
		k.WASI = filepath.Join(coreDir, coreFileName)
	}
	if !b.DevSameUser {
		u, err := user.Lookup(b.User)
		if err != nil {
			return KeeperConfig{}, fmt.Errorf("bot: user %s: %w", b.User, err)
		}
		uid, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return KeeperConfig{}, fmt.Errorf("bot: user %s has uid %q: %w", b.User, u.Uid, err)
		}
		gid, err := strconv.ParseUint(u.Gid, 10, 32)
		if err != nil {
			return KeeperConfig{}, fmt.Errorf("bot: user %s has gid %q: %w", b.User, u.Gid, err)
		}
		if uid == 0 || uid == uint64(os.Getuid()) { //nolint:gosec // G115: a uid is never negative
			return KeeperConfig{}, fmt.Errorf("bot: user %s has uid %d; the Bot needs a user of its own, neither root nor dillad's", b.User, uid)
		}
		k.UID, k.GID = uint32(uid), uint32(gid)
	}
	k.insecurePaths = UnsafeTestBuild()
	if err := k.checkFiles(); err != nil {
		return KeeperConfig{}, err
	}
	return k, nil
}

// checkFiles runs checkTrustedFile on the executable, the helper and the wasi core. The keeper runs it when it starts
// and again before every launch.
func (k KeeperConfig) checkFiles() error {
	for _, f := range []struct{ key, path string }{
		{"bot.executable", k.Executable}, {"bot.helper_executable", k.Helper}, {"bot.wasi_module", k.WASI},
	} {
		if _, err := resolveTrusted(f.path, k.requireRoot(), k.SameUser || k.insecurePaths); err != nil {
			return fmt.Errorf("bot: %s: %w", f.key, err)
		}
	}
	return nil
}

// CheckTrustedFile is the keeper's check of a file the Bot runs or loads, for `dillad doctor`.
func CheckTrustedFile(path string, requireRoot bool) error {
	_, err := resolveTrusted(path, requireRoot, !requireRoot)
	return err
}

// checkTrustedFile is CheckTrustedFile for the tests.
func checkTrustedFile(path string, requireRoot bool) error {
	return CheckTrustedFile(path, requireRoot)
}

// maxSymlinkHops bounds resolveTrusted, as the kernel's own 40.
const maxSymlinkHops = 40

// resolveTrusted resolves path one component at a time and returns the file it names.
//
// The file must be regular, not writable by group or others, without a set-id bit, and (requireRoot) root's. Unless
// leafOnly, so must be every directory on the way, the root included, and every symlink followed must be root's:
// otherwise whoever can write one of them (dillad, the Bot) could swap in another file between this check and the
// next launch, or re-point a link at any root-owned program. A sticky world-writable directory fails like any other
// writable one. leafOnly is dev_same_user, where everything is the user's own anyway.
func resolveTrusted(path string, requireRoot, leafOnly bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path", path)
	}
	checkDir := func(dir string) error {
		if leafOnly {
			return nil
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		return trustedEntry(dir, info, requireRoot, false)
	}
	if err := checkDir("/"); err != nil {
		return "", err
	}
	resolved := "/"
	rest := splitPath(path)
	hops := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > maxSymlinkHops {
				return "", fmt.Errorf("%s: too many symlinks", path)
			}
			if !leafOnly {
				if err := trustedOwner(next, info, requireRoot); err != nil {
					return "", err
				}
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				resolved = "/"
			}
			rest = append(splitPath(target), rest...)
			continue
		}
		if len(rest) > 0 {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", next)
			}
			if err := checkDir(next); err != nil {
				return "", err
			}
		} else if err := trustedEntry(next, info, requireRoot, true); err != nil {
			return "", err
		}
		resolved = next
	}
	return resolved, nil
}

func splitPath(p string) []string { return strings.Split(strings.Trim(p, "/"), "/") }

// trustedEntry checks one resolved component: not writable by group or others, root's when requireRoot, and for the
// final file regular and without a set-id bit.
func trustedEntry(path string, info os.FileInfo, requireRoot, file bool) error {
	if file {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return fmt.Errorf("%s has a set-id bit", path)
		}
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %04o)", path, info.Mode().Perm())
	}
	return trustedOwner(path, info, requireRoot)
}

func trustedOwner(path string, info os.FileInfo, requireRoot bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner", path)
	}
	if requireRoot && st.Uid != 0 {
		return fmt.Errorf("%s must be owned by root (it is owned by uid %d)", path, st.Uid)
	}
	return nil
}

// openExecutable opens the resolved executable for the launch, which execs this very descriptor (fd 4), and checks
// the open file again with fstat: what runs is what was checked, whatever happens to the path afterwards.
func (k KeeperConfig) openExecutable() (*os.File, error) {
	resolved, err := resolveTrusted(k.Executable, k.requireRoot(), k.SameUser || k.insecurePaths)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(resolved) //nolint:gosec // G304: the configured executable, resolved and checked just above
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = trustedEntry(resolved, info, k.requireRoot(), true)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// launchArgs is bot-launch's command line, built from the keeper's own configuration only.
func launchArgs(k KeeperConfig, sameUser bool) []string {
	args := []string{"bot-launch",
		"--executable", k.Executable, "--state-dir", k.StateDir, "--socket", k.Socket,
		"--helper", k.Helper, "--wasi", k.WASI,
	}
	if sameUser {
		args = append(args, "--same-user")
	}
	return args
}

// children is the set of Bot processes one keeper started and has not yet been asked to wait for.
type children struct {
	cfg KeeperConfig
	mu  sync.Mutex
	m   map[int]*child
}

// child is one started Bot. The keeper never reaps it by itself: exited closes when the process has ended
// (waitid with WNOWAIT, which leaves it a zombie), and only a wait op reaps it. Until then its pid can name no other
// process, which is what the supervisor's pidfd, opened right after start answers, relies on.
type child struct {
	cmd     *exec.Cmd
	exited  chan struct{}
	waiting bool // a wait is pending: one per child

	// reaped makes the reap happen once: a pending wait and killAll at keeper shutdown may both reach it.
	reaped   sync.Once
	exitText string
}

func newChildren(cfg KeeperConfig) *children { return &children{cfg: cfg, m: map[int]*child{}} }

// errNotChild is the answer for a pid this keeper did not start, or already reported.
var errNotChild = errors.New("not a child of this keeper")

// notChildExit is Wait's answer for such a pid.
const notChildExit = "not a child of this keeper"

// errBotRunning refuses a second start while the keeper's Bot is still in its table: one Bot per keeper.
var errBotRunning = errors.New("a Bot started by this keeper has not been waited for yet")

// start runs `<launcher> bot-launch …` with the launch token on fd 3, the executable it checked open on fd 4, an
// empty environment, cwd /, and, unless SameUser, the Bot user's uid and gid with no supplementary groups. It
// re-checks the Bot's files first, every time: a start long after the keeper's own may otherwise run whatever a
// path came to name since.
func (c *children) start(token [32]byte) (int, error) {
	c.mu.Lock()
	running := len(c.m) > 0
	c.mu.Unlock()
	if running {
		return 0, errBotRunning
	}
	if err := c.cfg.checkFiles(); err != nil {
		return 0, err
	}
	exe, err := c.cfg.openExecutable()
	if err != nil {
		return 0, err
	}
	defer func() { _ = exe.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer func() { _ = r.Close() }()
	// 32 bytes fit any pipe buffer, so this write cannot block.
	_, err = w.Write(token[:])
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	attr := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if !c.cfg.SameUser {
		attr.Credential = &syscall.Credential{Uid: c.cfg.UID, Gid: c.cfg.GID, Groups: []uint32{}, NoSetGroups: c.cfg.noSetgroups}
	}
	cmd := &exec.Cmd{
		Path: c.cfg.Launcher, Args: append([]string{c.cfg.Launcher}, launchArgs(c.cfg, c.cfg.SameUser)...),
		Env: []string{}, Dir: "/", Stdout: os.Stdout, Stderr: os.Stderr,
		ExtraFiles: []*os.File{r, exe}, SysProcAttr: attr,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	ch := &child{cmd: cmd, exited: make(chan struct{})}
	pid := cmd.Process.Pid
	go func() {
		defer close(ch.exited)
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(err, unix.EINTR) {
				return
			}
		}
	}()
	c.mu.Lock()
	c.m[cmd.Process.Pid] = ch
	c.mu.Unlock()
	return cmd.Process.Pid, nil
}

// signal sends SIGTERM, SIGKILL or SIGHUP (Bot v2 task 14: "your installations changed", Supervisor.Nudge) to a
// running child of this keeper. os.Process signals through a pidfd, so a reaped
// child's pid, which may already be someone else's, is never signalled.
func (c *children) signal(pid int, sig syscall.Signal) error {
	if sig != syscall.SIGTERM && sig != syscall.SIGKILL && sig != syscall.SIGHUP {
		return fmt.Errorf("signal %d is not SIGTERM, SIGKILL or SIGHUP", sig)
	}
	c.mu.Lock()
	ch, ok := c.m[pid]
	c.mu.Unlock()
	if !ok {
		return errNotChild
	}
	select {
	case <-ch.exited:
		return errors.New("the child has exited")
	default:
	}
	return ch.cmd.Process.Signal(sig)
}

// alreadyWaitedExit is Wait's answer while another wait for the same child is pending.
const alreadyWaitedExit = "already being waited for"

// wait blocks until the child ends and forgets it. One wait per child; a second one answers at once.
func (c *children) wait(pid int) string {
	c.mu.Lock()
	ch, ok := c.m[pid]
	if ok && ch.waiting {
		c.mu.Unlock()
		return alreadyWaitedExit
	}
	if ok {
		ch.waiting = true
	}
	c.mu.Unlock()
	if !ok {
		return notChildExit
	}
	<-ch.exited
	// The reap: from here on the pid is free for anyone, and the entry goes.
	exit := ch.reap()
	c.mu.Lock()
	// Only this child's entry: the pid may already name a later one.
	if c.m[pid] == ch {
		delete(c.m, pid)
	}
	c.mu.Unlock()
	return exit
}

// reap collects the exit status (os/exec's Wait) once and describes it; later calls answer the same text.
func (ch *child) reap() string {
	ch.reaped.Do(func() {
		_ = ch.cmd.Wait()
		if ch.cmd.ProcessState == nil {
			ch.exitText = "unknown exit"
			return
		}
		ch.exitText = ch.cmd.ProcessState.String()
	})
	return ch.exitText
}

// killAll ends every child: the keeper is going away, and so does what it started.
func (c *children) killAll() {
	c.mu.Lock()
	all := make([]*child, 0, len(c.m))
	for _, ch := range c.m {
		all = append(all, ch)
	}
	c.mu.Unlock()
	for _, ch := range all {
		_ = ch.cmd.Process.Kill()
		<-ch.exited
		_ = ch.reap()
	}
}

// LocalKeeper is the keeper for dev_same_user: the same starts, in dillad's own process, with no credential change.
type LocalKeeper struct{ c *children }

// NewLocalKeeper is a LocalKeeper over cfg. Close kills what it started.
func NewLocalKeeper(cfg KeeperConfig) *LocalKeeper { return &LocalKeeper{c: newChildren(cfg)} }

func (k *LocalKeeper) Start(spec LaunchSpec) (int, error)       { return k.c.start(spec.Token) }
func (k *LocalKeeper) Signal(pid int, sig syscall.Signal) error { return k.c.signal(pid, sig) }
func (k *LocalKeeper) Wait(pid int) string                      { return k.c.wait(pid) }
func (k *LocalKeeper) Close()                                   { k.c.killAll() }

// The control protocol. Requests are CBOR arrays [op, id, …]:
//
//	["start",  id, token(bstr 32)]
//	["signal", id, pid(uint), signal(uint: 15 or 9)]
//	["wait",   id, pid(uint)]
//
// and answers are [id, 0, value] (a pid, 0, or the exit description) or [id, 1, error(tstr)]. A frame that is not
// well-formed CBOR, not that shape, longer than maxKeeperFrame, or names any other op ends the keeper.
const (
	opStart  = "start"
	opSignal = "signal"
	opWait   = "wait"

	maxKeeperFrame = 4096
)

var keeperDec = func() cbor.DecMode {
	dm, err := cbor.DecOptions{
		MaxNestedLevels: 4, MaxArrayElements: 16, MaxMapPairs: 16,
		IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, DupMapKey: cbor.DupMapKeyEnforcedAPF,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return dm
}()

// boundedReader fails once more than n bytes were read since the last reset: one request is a few dozen bytes, so a
// peer that sends a frame announcing a huge string is cut off instead of making the keeper buffer it.
type boundedReader struct {
	r io.Reader
	n int
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, errors.New("keeper: frame too long")
	}
	if len(p) > b.n {
		p = p[:b.n]
	}
	n, err := b.r.Read(p)
	b.n -= n
	return n, err
}

type keeperRequest struct {
	op    string
	id    uint64
	token [32]byte
	pid   int
	sig   syscall.Signal
}

func parseKeeperRequest(frame []cbor.RawMessage) (keeperRequest, error) {
	var req keeperRequest
	if len(frame) < 3 {
		return req, errors.New("keeper: short request")
	}
	if err := keeperDec.Unmarshal(frame[0], &req.op); err != nil {
		return req, fmt.Errorf("keeper: op: %w", err)
	}
	if err := keeperDec.Unmarshal(frame[1], &req.id); err != nil {
		return req, fmt.Errorf("keeper: id: %w", err)
	}
	pid := func(raw cbor.RawMessage) error {
		var v uint64
		if err := keeperDec.Unmarshal(raw, &v); err != nil || v == 0 || v > 1<<22 {
			return errors.New("keeper: bad pid")
		}
		req.pid = int(v)
		return nil
	}
	switch req.op {
	case opStart:
		var tok []byte
		if len(frame) != 3 || keeperDec.Unmarshal(frame[2], &tok) != nil || len(tok) != len(req.token) {
			return req, errors.New("keeper: start takes exactly a 32-byte launch token")
		}
		copy(req.token[:], tok)
		clear(tok)
	case opSignal:
		var sig uint64
		if len(frame) != 4 || pid(frame[2]) != nil || keeperDec.Unmarshal(frame[3], &sig) != nil ||
			(sig != uint64(syscall.SIGTERM) && sig != uint64(syscall.SIGKILL) && sig != uint64(syscall.SIGHUP)) {
			return req, errors.New("keeper: signal takes a pid and SIGTERM, SIGKILL or SIGHUP")
		}
		req.sig = syscall.Signal(sig)
	case opWait:
		if len(frame) != 3 || pid(frame[2]) != nil {
			return req, errors.New("keeper: wait takes a pid")
		}
	default:
		return req, fmt.Errorf("keeper: unknown op %q", req.op)
	}
	return req, nil
}

// ServeKeeper is `dillad bot-keeper`: it serves the control socket ctl until it closes or misbehaves, then kills
// every child it started and returns. A clean close is nil; anything else (a protocol violation included) is an error.
func ServeKeeper(cfg KeeperConfig, ctl *os.File, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	// systemd's stop signals the whole cgroup (dilla.service has KillMode=mixed, so normally only serve): the keeper
	// stays until serve has stopped the Bot through it (SIGTERM, StopGrace, SIGKILL) and closed the control socket.
	// Handled, not ignored: an ignored disposition would pass through fork and exec to bot-launch and the Bot, and a
	// handled one is reset to the default there.
	stops := make(chan os.Signal, 1)
	signal.Notify(stops, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stops)
	go func() {
		for range stops { //nolint:revive // drained: the keeper outlives the stop signal on purpose
		}
	}()
	if err := narrowKeeperCapabilities(cfg.allowCgoNarrowing); err != nil {
		_ = ctl.Close()
		return fmt.Errorf("keeper: %w", err)
	}
	// A unit without CAP_KILL (a binary upgraded without the release's dilla.service) still starts the Bot; only its
	// signals fail. Said once, here, so the stop that then hangs has a reason in the log.
	if err := keeperKillCheck(cfg.SameUser); err != nil {
		log.Warn("bot keeper: signals to the Bot will fail", "err", err)
	}
	conn, err := net.FileConn(ctl)
	_ = ctl.Close()
	if err != nil {
		return fmt.Errorf("keeper: control socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	table := newChildren(cfg)
	var waits sync.WaitGroup
	// Deferred calls run last first: the children are killed, then the waits they unblock are answered.
	defer waits.Wait()
	defer table.killAll()
	var wmu sync.Mutex
	answer := func(id uint64, value any, err error) {
		msg := []any{id, uint64(0), value}
		if err != nil {
			msg = []any{id, uint64(1), err.Error()}
		}
		b, merr := cbor.Marshal(msg)
		if merr != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = conn.Write(b)
	}
	in := &boundedReader{r: conn}
	dec := keeperDec.NewDecoder(in)
	for {
		in.n = maxKeeperFrame
		var frame []cbor.RawMessage
		if err := dec.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("keeper: malformed request: %w", err)
		}
		req, err := parseKeeperRequest(frame)
		if err != nil {
			log.Error("bot keeper: refusing its control pipe", "err", err)
			// Close before the deferred kill: the client sees the pipe end, not an answer.
			_ = conn.Close()
			return err
		}
		switch req.op {
		case opStart:
			pid, err := table.start(req.token)
			clear(req.token[:])
			if err != nil {
				log.Error("bot keeper: start failed", "err", err)
			}
			answer(req.id, uint64(pid), err) //nolint:gosec // G115: a pid is never negative
		case opSignal:
			answer(req.id, uint64(0), table.signal(req.pid, req.sig))
		case opWait:
			waits.Add(1)
			go func(id uint64, pid int) {
				defer waits.Done()
				answer(id, table.wait(pid), nil)
			}(req.id, req.pid)
		}
	}
}

// PipeKeeper is the serving dillad's end of the keeper's control socket.
type PipeKeeper struct {
	conn    net.Conn
	connErr error
	once    sync.Once
	wmu     sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan keeperReply
	gone    error
}

type keeperReply struct {
	failed bool
	value  cbor.RawMessage
	err    error
}

// NewPipeKeeper wraps the control socket f (it takes f over). Its answers are read once the first call is made.
func NewPipeKeeper(f *os.File) *PipeKeeper {
	conn, err := net.FileConn(f)
	_ = f.Close()
	return &PipeKeeper{conn: conn, connErr: err, pending: map[uint64]chan keeperReply{}}
}

// Close closes the control socket; the keeper then kills the Bot and exits.
func (k *PipeKeeper) Close() error {
	if k.conn == nil {
		return k.connErr
	}
	return k.conn.Close()
}

func (k *PipeKeeper) readLoop() {
	dec := cbor.NewDecoder(k.conn)
	for {
		var msg []cbor.RawMessage
		err := dec.Decode(&msg)
		var id uint64
		var status uint64
		if err == nil && (len(msg) != 3 || cbor.Unmarshal(msg[0], &id) != nil || cbor.Unmarshal(msg[1], &status) != nil) {
			err = errors.New("keeper: malformed answer")
		}
		if err != nil {
			k.mu.Lock()
			k.gone = fmt.Errorf("%w: control socket: %w", ErrKeeperGone, err)
			for id, ch := range k.pending {
				ch <- keeperReply{err: k.gone}
				delete(k.pending, id)
			}
			k.mu.Unlock()
			return
		}
		k.mu.Lock()
		ch, ok := k.pending[id]
		delete(k.pending, id)
		k.mu.Unlock()
		if ok {
			ch <- keeperReply{failed: status != 0, value: msg[2]}
		}
	}
}

func (k *PipeKeeper) call(op string, args ...any) (keeperReply, error) {
	if k.conn == nil {
		return keeperReply{}, fmt.Errorf("%w: %w", ErrKeeperGone, k.connErr)
	}
	k.once.Do(func() { go k.readLoop() })
	ch := make(chan keeperReply, 1)
	k.mu.Lock()
	if k.gone != nil {
		err := k.gone
		k.mu.Unlock()
		return keeperReply{}, err
	}
	k.next++
	id := k.next
	k.pending[id] = ch
	k.mu.Unlock()
	b, err := cbor.Marshal(append([]any{op, id}, args...))
	if err != nil {
		return keeperReply{}, err
	}
	k.wmu.Lock()
	_, err = k.conn.Write(b)
	k.wmu.Unlock()
	if err != nil {
		k.mu.Lock()
		delete(k.pending, id)
		k.mu.Unlock()
		return keeperReply{}, fmt.Errorf("%w: %w", ErrKeeperGone, err)
	}
	r := <-ch
	if r.err != nil {
		return r, r.err
	}
	if r.failed {
		var msg string
		_ = cbor.Unmarshal(r.value, &msg)
		return r, errors.New(msg)
	}
	return r, nil
}

func (k *PipeKeeper) Start(spec LaunchSpec) (int, error) {
	r, err := k.call(opStart, spec.Token[:])
	if err != nil {
		return 0, err
	}
	var pid uint64
	if err := cbor.Unmarshal(r.value, &pid); err != nil || pid == 0 || pid > 1<<22 {
		return 0, errors.New("keeper: malformed pid")
	}
	return int(pid), nil
}

func (k *PipeKeeper) Signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return errNotChild
	}
	_, err := k.call(opSignal, uint64(pid), uint64(sig)) //nolint:gosec // G115: pid > 0 above; a signal number
	return err
}

func (k *PipeKeeper) Wait(pid int) string {
	if pid <= 0 {
		return notChildExit
	}
	r, err := k.call(opWait, uint64(pid))
	if err != nil {
		return err.Error()
	}
	var exit string
	if err := cbor.Unmarshal(r.value, &exit); err != nil {
		return "keeper: malformed exit"
	}
	return exit
}

var (
	_ Keeper = (*PipeKeeper)(nil)
	_ Keeper = (*LocalKeeper)(nil)
)
