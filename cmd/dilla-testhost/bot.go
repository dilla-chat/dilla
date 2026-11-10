package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
	"github.com/dilla-chat/dilla/internal/gateway"
)

// botOptions are the -bot flags: the test host runs the Bot as dillad does, a supervised child on a Unix socket, in
// dev_same_user mode (the specs' Bot runs as the test user). The specs start and stop it through /debug/bot/… on the
// control listener; it is not started with the host.
type botOptions struct {
	enabled                            bool
	executable, helper, wasi, stateDir string
}

// botConfig is [bot] as the host runs it: dev_same_user, the socket in <data-dir>/bot.
func (b botOptions) botConfig(dataDir string) (config.Bot, error) {
	u, err := user.Current()
	if err != nil {
		return config.Bot{}, err
	}
	state := b.stateDir
	if state == "" {
		state = filepath.Join(dataDir, "bot-state")
	}
	c := config.Bot{
		Enabled: true, Executable: b.executable, HelperExecutable: b.helper, WASIModule: b.wasi,
		StateDir: state, User: u.Username, SocketDir: filepath.Join(filepath.Dir(dataDir), filepath.Base(dataDir)+"-bot"), DevSameUser: true,
	}
	for name, p := range map[string]*string{"-bot-executable": &c.Executable, "-bot-helper": &c.HelperExecutable, "-bot-wasi": &c.WASIModule, "-bot-state-dir": &c.StateDir} {
		abs, err := filepath.Abs(*p)
		if *p == "" || err != nil {
			return config.Bot{}, fmt.Errorf("%s is required with -bot", name)
		}
		*p = abs
	}
	return c, nil
}

// hostBot is the running pieces: the supervisor (the host's launch gate), its runner and keeper, and the socket.
type hostBot struct {
	sup    *botsupervisor.Supervisor
	runner *dilladtest.BotRunner
	keeper *botsupervisor.LocalKeeper
	ln     net.Listener
	srv    *http.Server
}

// newHostBot builds the supervisor before the instance (the instance's gateway admits the Bot through it).
func newHostBot(b config.Bot, core string, log *slog.Logger) (*hostBot, error) {
	kc, err := botsupervisor.NewKeeperConfig(b, filepath.Dir(core))
	if err != nil {
		return nil, err
	}
	keeper := botsupervisor.NewLocalKeeper(kc)
	sup := botsupervisor.New(b, keeper, log)
	return &hostBot{sup: sup, runner: dilladtest.NewBotRunner(sup), keeper: keeper}, nil
}

// serve listens on <socket_dir>/gateway.sock (owner-only: dev_same_user) and serves the host's Bot surface behind the
// peer check.
func (hb *hostBot) serve(b config.Bot, host *dilladtest.Host) error {
	if err := os.MkdirAll(b.SocketDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(b.SocketDir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit
		return err
	}
	path := b.SocketPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	pl, err := gateway.BotPeerListener(ln, hb.sup)
	if err != nil {
		_ = ln.Close()
		return err
	}
	hb.ln = ln
	hb.srv = &http.Server{Handler: host.BotHandler(), ConnContext: gateway.BotConnContext, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hb.srv.Serve(pl) }()
	return nil
}

// close stops the Bot, then its socket.
func (hb *hostBot) close() {
	hb.runner.Stop()
	hb.keeper.Close()
	if hb.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hb.srv.Shutdown(ctx)
	}
	if hb.ln != nil {
		_ = hb.ln.Close()
	}
}
