package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dilla-chat/dilla/internal/exit"
)

// runNetns is a small, idle namespace holder for the Compose deployment. Keeping its container
// alive lets the API and media containers be replaced independently while their loopback link and
// published ports remain stable.
func runNetns(args []string, _, _ io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("netns takes no arguments: %w", exit.Usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}
