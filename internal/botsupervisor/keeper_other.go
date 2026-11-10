//go:build !linux

package botsupervisor

import (
	"errors"
	"log/slog"
	"os"
	"syscall"

	"github.com/dilla-chat/dilla/internal/config"
)

// errNotLinux is every keeper operation's answer off Linux: the Bot's sandbox (user switch, capability drop,
// bubblewrap) is Linux-only, as dillabot is.
var errNotLinux = errors.New("bot: the supervised Bot runs on Linux only")

// LaunchPATH is the Bot's PATH (see keeper_linux.go).
const LaunchPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// KeeperConfig is keeper_linux.go's; off Linux nothing can be started with it.
type KeeperConfig struct {
	Launcher, Executable, Helper, WASI, StateDir, Socket string
	UID, GID                                             uint32
	SameUser                                             bool
}

func NewKeeperConfig(config.Bot, string) (KeeperConfig, error) { return KeeperConfig{}, errNotLinux }

func ServeKeeper(KeeperConfig, *os.File, *slog.Logger) error { return errNotLinux }

func LaunchMain([]string) error { return errNotLinux }

func HoldsSetuid() bool { return false }

func CheckTrustedFile(string, bool) error { return errNotLinux }

func DropServingCapabilities(*slog.Logger, bool) error { return nil }

// PipeKeeper and LocalKeeper refuse every start off Linux.
type PipeKeeper struct{}

func NewPipeKeeper(f *os.File) *PipeKeeper { _ = f.Close(); return &PipeKeeper{} }

func (*PipeKeeper) Close() error                     { return nil }
func (*PipeKeeper) Start(LaunchSpec) (int, error)    { return 0, errNotLinux }
func (*PipeKeeper) Signal(int, syscall.Signal) error { return errNotLinux }
func (*PipeKeeper) Wait(int) string                  { return errNotLinux.Error() }

type LocalKeeper struct{}

func NewLocalKeeper(KeeperConfig) *LocalKeeper        { return &LocalKeeper{} }
func (*LocalKeeper) Close()                           {}
func (*LocalKeeper) Start(LaunchSpec) (int, error)    { return 0, errNotLinux }
func (*LocalKeeper) Signal(int, syscall.Signal) error { return errNotLinux }
func (*LocalKeeper) Wait(int) string                  { return errNotLinux.Error() }
