package main

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/ops"
)

// internalVerbs are run by dillad itself, never by an operator, and are left out of the usage listing:
// `bot-keeper` (serve starts it before it opens any listener) and `bot-launch` (the keeper runs it as the Bot user).
func internalVerbs() map[string]verb {
	return map[string]verb{
		"bot-keeper": {"bot-keeper", "internal: start the Bot as its own user, on the serving process's request", runBotKeeper},
		"bot-launch": {"bot-launch", "internal: drop every privilege and exec the Bot", runBotLaunch},
	}
}

// holdsSetuid reports whether this process may switch users; a variable so a test can take the capability away.
var holdsSetuid = botsupervisor.HoldsSetuid

// keeperControlFD is the descriptor serve hands the keeper its control socket on.
const keeperControlFD = 3

// runBotLaunch is `dillad bot-launch …`: it returns only when it could not exec the Bot.
func runBotLaunch(args []string, _, _ io.Writer) error {
	return botsupervisor.LaunchMain(args)
}

// keeperConfig resolves [bot] against the wasi core beside this binary.
func keeperConfig(b config.Bot) (botsupervisor.KeeperConfig, error) {
	exe, err := os.Executable()
	if err != nil {
		return botsupervisor.KeeperConfig{}, err
	}
	return botsupervisor.NewKeeperConfig(b, filepath.Dir(exe))
}

// errBotNeedsDevSameUser is serve's refusal to run the Bot as dillad's own user unasked.
func errBotNeedsDevSameUser(b config.Bot) error {
	return fmt.Errorf("bot: cannot switch to user %s; set [bot] dev_same_user = true for development only: %w", b.User, exit.Config)
}

// botKeeper is what serve starts the Bot through, and how it lets go of it.
type botKeeper struct {
	keeper botsupervisor.Keeper
	close  func()
	gid    int
}

// lookupCurrentUser is this process's user name.
func lookupCurrentUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// doctorBotLeg checks [bot]: the user exists (not root), the executable, helper and wasi core exist and are root's
// and not writable by others, and the state directory exists. dev_same_user is a warning: the Bot's keys are then
// readable by the server's user.
func doctorBotLeg(cfg *config.Config) ops.Leg {
	const name = "bot"
	b := cfg.Bot
	if !b.Enabled {
		return ops.Leg{Name: name, Status: ops.Green, Detail: "disabled ([bot] enabled = false)"}
	}
	var problems []string
	if !b.DevSameUser {
		u, err := user.Lookup(b.User)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("user %s: %v", b.User, err))
		case u.Uid == "0" || u.Uid == strconv.Itoa(os.Getuid()):
			problems = append(problems, fmt.Sprintf("user %s has uid %s; the Bot needs a user of its own", b.User, u.Uid))
		}
	}
	core := b.WASIModule
	if core == "" {
		if exe, err := os.Executable(); err == nil {
			core = filepath.Join(filepath.Dir(exe), "dilla_core_wasi.wasm")
		}
	}
	for _, f := range []struct{ key, path string }{
		{"executable", b.Executable}, {"helper_executable", b.HelperExecutable}, {"wasi_module", core},
	} {
		if err := botsupervisor.CheckTrustedFile(f.path, !b.DevSameUser); err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", f.key, f.path, err))
		}
	}
	if info, err := os.Stat(b.StateDir); err != nil {
		problems = append(problems, fmt.Sprintf("state_dir %s: %v", b.StateDir, err))
	} else if !info.IsDir() {
		problems = append(problems, fmt.Sprintf("state_dir %s is not a directory", b.StateDir))
	}
	if len(problems) > 0 {
		detail := problems[0]
		for _, p := range problems[1:] {
			detail += "; " + p
		}
		return ops.Leg{Name: name, Status: ops.Red, Detail: detail,
			Fix: "create the Bot user and install the Bot (deploy/install.sh), or set [bot] enabled = false"}
	}
	if b.DevSameUser {
		return ops.Leg{Name: name, Status: ops.Yellow,
			Detail: "dev_same_user = true: the Bot runs as dillad's own user, which can read its keys; development only",
			Fix:    "set [bot] dev_same_user = false and give the Bot its own user"}
	}
	return ops.Leg{Name: name, Status: ops.Green, Detail: fmt.Sprintf("runs %s as user %s", b.Executable, b.User)}
}
