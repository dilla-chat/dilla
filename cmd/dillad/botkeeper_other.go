//go:build !linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
)

// errBotNotLinux: the supervised Bot (user switch, capability drop, bubblewrap) runs on Linux only, as dillabot does.
var errBotNotLinux = errors.New("bot: the supervised Bot runs on Linux only")

func runBotKeeper([]string, io.Writer, io.Writer) error { return errBotNotLinux }

func startBotKeeper(cfg *config.Config, _ string) (*botKeeper, error) {
	if !cfg.Bot.Enabled {
		return nil, nil
	}
	return nil, fmt.Errorf("serve: %w: %w", errBotNotLinux, exit.Config)
}

func openBotSocket(config.Bot, int) (net.Listener, error) { return nil, errBotNotLinux }
