//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"syscall"

	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
)

// runBotKeeper is `dillad bot-keeper --config <path>`. It reads [bot] from dilla.toml once, here, and serves its
// control socket (fd 3) until the serving process closes it.
func runBotKeeper(args []string, stdout, stderr io.Writer) error {
	fs, cfgPath := newFlagSet("bot-keeper", stderr)
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	kc, err := keeperConfig(cfg.Bot)
	if err != nil {
		return fmt.Errorf("bot-keeper: %w: %w", err, exit.Config)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(keeperControlFD, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return fmt.Errorf("bot-keeper: no control socket on fd %d; dillad serve starts the keeper: %w", keeperControlFD, exit.Usage)
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "bot-keeper")
	if err := botsupervisor.ServeKeeper(kc, os.NewFile(keeperControlFD, "keeper-control"), log); err != nil {
		return fmt.Errorf("bot-keeper: %w", err)
	}
	return nil
}

// startBotKeeper runs before serve opens any listener. With [bot] enabled it either starts `dillad bot-keeper`, which
// inherits CAP_SETUID and CAP_SETGID (the serving process drops them right after), or, with dev_same_user, keeps an
// in-process keeper that starts the Bot as dillad's own user. A process that cannot switch users and was not told
// dev_same_user refuses to start: running the Bot as the server's user unasked would hand it the Bot's keys.
func startBotKeeper(cfg *config.Config, cfgPath string) (*botKeeper, error) {
	b := cfg.Bot
	if !b.Enabled {
		return nil, nil
	}
	if !b.DevSameUser && !holdsSetuid() {
		return nil, errBotNeedsDevSameUser(b)
	}
	kc, err := keeperConfig(b)
	if err != nil {
		return nil, fmt.Errorf("serve: %w: %w", err, exit.Config)
	}
	if b.DevSameUser {
		local := botsupervisor.NewLocalKeeper(kc)
		return &botKeeper{keeper: local, close: local.Close, gid: -1}, nil
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("serve: bot keeper socket: %w: %w", err, exit.Software)
	}
	ours, theirs := os.NewFile(uintptr(pair[0]), "keeper-client"), os.NewFile(uintptr(pair[1]), "keeper-server")
	// /proc/self/exe: the keeper is this very binary, whatever happened to the file on disk.
	// Not CommandContext: the keeper lives as long as serve, and ends when its control socket closes.
	cmd := exec.Command("/proc/self/exe", "bot-keeper", "--config="+cfgPath) //nolint:noctx,gosec // see above; G204: this binary, with serve's own --config
	cmd.ExtraFiles = []*os.File{theirs}
	// No Pdeathsig: it would follow the forking thread, not this process. The keeper ends when its control socket
	// does, which is when serve exits, however it exits.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		_ = ours.Close()
		_ = theirs.Close()
		return nil, fmt.Errorf("serve: start the bot keeper: %w: %w", err, exit.Software)
	}
	_ = theirs.Close()
	pk := botsupervisor.NewPipeKeeper(ours)
	return &botKeeper{keeper: pk, gid: int(kc.GID), close: func() {
		// Closing the control socket ends the keeper, which kills whatever it still runs.
		_ = pk.Close()
		_ = cmd.Wait()
	}}, nil
}

// openBotSocket creates bot.socket_dir and listens on <socket_dir>/gateway.sock. The directory is 0750 and the socket
// 0660, both owned by dillad with the Bot user's primary group, so the Bot can connect and no other user can reach
// the socket at all (dillad must be a member of that group to set it; the installer adds it). With dev_same_user
// both are owner-only. A stale socket from an earlier run is replaced; anything else at the path is refused.
func openBotSocket(b config.Bot, gid int) (net.Listener, error) {
	dirMode, sockMode := os.FileMode(0o750), os.FileMode(0o660)
	if gid < 0 {
		dirMode, sockMode = 0o700, 0o600
	}
	if err := os.MkdirAll(b.SocketDir, dirMode); err != nil {
		return nil, err
	}
	info, err := os.Lstat(b.SocketDir)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not a directory this user owns", b.SocketDir)
	}
	if err := os.Chmod(b.SocketDir, dirMode); err != nil {
		return nil, err
	}
	if gid >= 0 {
		if err := os.Lchown(b.SocketDir, -1, gid); err != nil {
			return nil, fmt.Errorf("give %s to the Bot's group %d (dillad must be a member of it): %w", b.SocketDir, gid, err)
		}
	}
	path := b.SocketPath()
	if old, err := os.Lstat(path); err == nil {
		if old.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Listener, error) { _ = l.Close(); return nil, err }
	if err := os.Chmod(path, sockMode); err != nil {
		return fail(err)
	}
	if gid >= 0 {
		if err := os.Lchown(path, -1, gid); err != nil {
			return fail(err)
		}
	}
	return l, nil
}
