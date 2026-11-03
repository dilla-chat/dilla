package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/sfu"
)

// runMedia keeps the SFU in its own process while dillad serve is replaced. It opens no database,
// API listener or web client. The API process owns the webhook receiver and the /rtc join gate.
func runMedia(args []string, stdout, stderr io.Writer) error {
	fs, cfgPath := newFlagSet("media", stderr)
	probe := fs.Bool("probe", false, "check whether the media sidecar accepts RoomService requests")
	rooms := fs.Bool("rooms", false, "list the sidecar's live rooms and their participants")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	if *probe && *rooms {
		return usageError(stderr, "media", "--probe and --rooms are exclusive")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *probe || *rooms {
		verb := "media probe"
		if *rooms {
			verb = "media rooms"
		}
		if !cfg.LiveKit.Enabled || cfg.LiveKit.Mode != "sidecar" {
			return fmt.Errorf("%s: livekit.enabled must be true and livekit.mode must be sidecar: %w", verb, exit.Config)
		}
		secret, err := os.ReadFile(cfg.LiveKit.APISecretFile)
		if err != nil {
			return fmt.Errorf("%s: livekit.api_secret_file: %w: %w", verb, err, exit.Config)
		}
		remote := sfu.NewRemote(sfuConfig(cfg.LiveKit, string(secret)))
		if *rooms {
			return mediaRooms(ctx, remote, stdout, 20*time.Second)
		}
		return mediaProbe(ctx, remote, stdout, 20*time.Second)
	}
	return serveMedia(ctx, cfg, stdout, stderr)
}

type roomLister interface {
	Rooms(context.Context) ([]string, error)
	Participants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error)
}

// mediaProbe waits for the sidecar and prints how many rooms are live.
func mediaProbe(ctx context.Context, remote roomLister, stdout io.Writer, deadline time.Duration) error {
	rooms, err := waitRemoteSFURooms(ctx, remote, deadline)
	if err != nil {
		return fmt.Errorf("media probe: %w: %w", err, exit.Unavailable)
	}
	_, err = fmt.Fprintf(stdout, "media probe: ok, %d live rooms\n", len(rooms))
	return err
}

// mediaRooms prints each live room (sorted) as "<room>\t<n> participants", then the participants'
// identities (sorted) indented by two spaces; an empty SFU prints "no live rooms".
func mediaRooms(ctx context.Context, remote roomLister, stdout io.Writer, deadline time.Duration) error {
	// One deadline covers the readiness wait and every Participants call.
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	rooms, err := waitRemoteSFURooms(ctx, remote, deadline)
	if err != nil {
		return fmt.Errorf("media rooms: %w: %w", err, exit.Unavailable)
	}
	if len(rooms) == 0 {
		_, err = fmt.Fprintln(stdout, "no live rooms")
		return err
	}
	sort.Strings(rooms)
	var buf bytes.Buffer // printed only when every room listed
	for _, room := range rooms {
		parts, err := remote.Participants(ctx, room)
		if err != nil {
			return fmt.Errorf("media rooms: %w: %w", err, exit.Unavailable)
		}
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			ids = append(ids, p.GetIdentity())
		}
		sort.Strings(ids)
		fmt.Fprintf(&buf, "%s\t%d participants\n", room, len(ids))
		for _, i := range ids {
			fmt.Fprintf(&buf, "  %s\n", i)
		}
	}
	_, err = stdout.Write(buf.Bytes())
	return err
}

func waitRemoteSFU(ctx context.Context, remote roomLister, deadline time.Duration) error {
	_, err := waitRemoteSFURooms(ctx, remote, deadline)
	return err
}

// A systemd Type=simple or Compose process can exist before LiveKit accepts requests. A
// successful authenticated RoomService call is the readiness signal shared by serve and install;
// it returns the rooms that call listed.
func waitRemoteSFURooms(ctx context.Context, remote interface {
	Rooms(context.Context) ([]string, error)
}, deadline time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var last error
	for {
		probeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		rooms, err := remote.Rooms(probeCtx)
		stop()
		if err == nil {
			return rooms, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("sidecar did not become ready: %w: %w", last, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func serveMedia(ctx context.Context, cfg *config.Config, stdout, stderr io.Writer) error {
	if !cfg.LiveKit.Enabled || cfg.LiveKit.Mode != "sidecar" {
		return fmt.Errorf("media: livekit.enabled must be true and livekit.mode must be sidecar: %w", exit.Config)
	}
	// startSFU's in_process check protects the ordinary serve path. This copy selects the same
	// validated SFU settings without changing the API process's sidecar configuration.
	mediaCfg := *cfg
	mediaCfg.LiveKit.Mode = "in_process"
	fd := frontDeps{cfg: &mediaCfg, log: obs.NewLogger(cfg.Log, stderr), health: obs.NewHealth(clock.System()), stdout: stdout}
	s, closeSFU, err := startSFU(ctx, fd)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer closeSFU()
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.Done():
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("media: SFU stopped: %w: %w", err, exit.Unavailable)
	}
}
