package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
)

// runMigrate applies or inspects schema migrations: `up` (the default),
// `status`, and `down` (gated behind --i-know-what-i-am-doing). The sub-verb
// is popped BEFORE fs.Parse, because Go's flag package stops at the first
// non-flag argument: given ["status", "--config=/tmp/.../dilla.toml"] it would
// see "status", stop, leave --config unparsed and fall back to
// /etc/dilla/dilla.toml — reading the operator's real production file on a dev
// box.
func runMigrate(args []string, stdout, stderr io.Writer) error {
	sub := "up"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "up", "status", "down":
	default:
		fmt.Fprintf(stderr, "dillad migrate: unknown sub-verb %q; want up, status or down\n", sub)
		return exit.Usage
	}

	fs, cfgPath := newFlagSet("migrate "+sub, stderr)
	to := fs.Int64("to", 0, "migrate to this version instead of the latest")
	force := fs.Bool("i-know-what-i-am-doing", false, "permit `migrate down`")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	if sub == "down" && !*force {
		fmt.Fprintln(stderr, "dillad migrate down: --i-know-what-i-am-doing is required")
		return exit.Usage
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	repo, db, err := openRepository(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	provider, err := migrationProvider(cfg, db)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if sub == "status" || sub == "up" {
		if _, err := migrationState(ctx, provider); err != nil {
			return fmt.Errorf("migrate %s: %w: %w", sub, err, exit.Data)
		}
	}
	switch sub {
	case "status":
		current, target, err := provider.GetVersions(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w: %w", err, exit.Data)
		}
		fmt.Fprintf(stdout, "current %d, target %d\n", current, target)
		return nil
	case "up":
		if *to > 0 {
			if _, err := provider.UpTo(ctx, *to); err != nil {
				return fmt.Errorf("migrate up --to=%d: %w: %w", *to, err, exit.Software)
			}
		} else if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("migrate up: %w: %w", err, exit.Software)
		}
	case "down":
		if *to > 0 {
			if _, err := provider.DownTo(ctx, *to); err != nil {
				return fmt.Errorf("migrate down --to=%d: %w: %w", *to, err, exit.Software)
			}
		} else if _, err := provider.Down(ctx); err != nil {
			return fmt.Errorf("migrate down: %w: %w", err, exit.Software)
		}
	}

	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("migrate %s: %w: %w", sub, err, exit.Data)
	}
	fmt.Fprintf(stdout, "migrated to %d (target %d)\n", current, target)
	return nil
}
