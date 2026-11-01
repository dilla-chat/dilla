package dillad

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jonasthim/dilla/internal/blob"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/postgres"
	"github.com/jonasthim/dilla/internal/store/sqlite"
)

// WireVersion is the current production wire version (wire 5: ephemeral realtime extensions; wire 4: account-kind attestation, Task S1
// part 2). BotWireVersion is the version managed Bots negotiate; LegacyWireVersion remains available for
// existing clients that have not moved to the bot-capable protocol.
const (
	WireVersion       = 5
	BotWireVersion    = 3
	LegacyWireVersion = 2
)

// Options is everything New needs. Only Config is required; every other
// collaborator is built when it is nil, because part 1b's test harness calls
// New with Config and Clock alone and a composition root that insists on five
// pre-built collaborators forces every test to assemble them by hand — which
// is how a test path drifts from the production path.
type Options struct {
	Config      *config.Config   // required
	Repo        store.Repository // optional: opened from Config.DB when nil
	Clock       clock.Clock      // optional: clock.System()
	Log         *slog.Logger     // optional: obs.NewLogger(Config.Log, os.Stderr)
	Metrics     *obs.Metrics     // optional: over a fresh prometheus.NewRegistry()
	Health      *obs.Health      // optional: obs.NewHealth(Clock)
	ScrapeToken string

	// Extra runs after every route New mounts itself — Plan 1's and Plan 2's —
	// and gets the same *server.Mux (deviation ID14). The production binary
	// passes none; it is the seam a test mounts a route of its own through.
	Extra []func(*server.Mux)

	// Plan 2. Blobs is the attachment store the blob and admin routes read
	// and unlink from; nil means New opens blobs.dir itself and closes it on
	// Shutdown, while a store passed in stays the caller's (`dillad serve`
	// passes the one its garbage collector sweeps). SFU controls LiveKit when
	// livekit.enabled; nil means no SFU, so /rtc is
	// not mounted and a call that would open answers 501.
	Blobs *blob.Store
	SFU   SFU
	// Relay is the TURN relay's revocation state, shared with the relay `dillad serve` starts
	// (server.StartTURN): the call routes' cuts revoke a device's relay credentials and close its
	// relay sockets (dilla-media task 13 review I1). nil means no cut reaches a relay.
	Relay *server.RelayRevocations

	// Part 1b (deviation B17). Wasm is the wasi runtime the delivery service
	// validates every handshake in; nil means New compiles one from CorePath
	// and closes it on Shutdown, while a runtime passed in stays the caller's
	// (a test harness shares one across servers, because compiling the OpenMLS
	// module is the most expensive thing New can do).
	Wasm *mlswasi.Runtime
	// CorePath is the dilla_core_wasi.wasm New compiles when Wasm is nil. ""
	// means CoreFileName beside the dillad binary, the path `dillad doctor`
	// checks (deviation B35: dilla.toml has no [mls] table).
	CorePath string
	// ACL is invariant 4's eligibility source; nil means api.ResolverACL, the
	// permission resolver over the repository (Plan 2 task 3, NV-B6 closed).
	ACL ds.ACL
	// Channels is invariant 1's channel-mode source and the registration
	// ACL; nil means api.StructureChannels over the repository (Plan 2 task 2).
	Channels ds.Channels
	// Web is the built browser client dillad serves at its own origin, verified against its
	// dilla-manifest.json by web.New; nil means web.Embedded().
	Web fs.FS
	// LiveKitURL replaces the signalling URL the call routes hand a client ("wss://" plus the
	// instance's host); "" keeps it. `dillad serve` never sets it: the test host passes its own
	// ws:// listener, which a browser on loopback can reach (web-3 task 3).
	LiveKitURL string
	// WireVersions is the wire version list the gateway's hello offers and GET /v1/instance
	// advertises; nil defaults to [3,2]. `dillad serve` never sets it: the test
	// host's -wire flag does, so a browser test can meet an instance that offers another version.
	WireVersions  []uint64
	E2EEVersions  []uint64
	MediaVersions []uint64

	// closeRepo records that New opened the repository itself, so Shutdown
	// closes it. A caller that supplied its own keeps ownership of it: closing
	// a pool the caller still holds is how a harness that builds two servers
	// over one store starts failing at the second.
	closeRepo bool
}

// validate DEFAULTS rather than refuses. The one thing it cannot invent is the
// configuration.
func (o *Options) validate(ctx context.Context) error {
	if o.Config == nil {
		return errors.New("dillad: Options.Config is required")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = obs.NewLogger(o.Config.Log, os.Stderr)
	}
	if o.Metrics == nil {
		reg := prometheus.NewRegistry()
		o.Metrics = obs.NewMetrics(reg, reg)
	}
	if o.Health == nil {
		o.Health = obs.NewHealth(o.Clock)
	}
	if len(o.WireVersions) == 0 {
		o.WireVersions = []uint64{WireVersion, 4, BotWireVersion, LegacyWireVersion}
	}
	if len(o.E2EEVersions) == 0 {
		o.E2EEVersions = []uint64{1}
	}
	if len(o.MediaVersions) == 0 {
		o.MediaVersions = []uint64{1}
	}
	if o.Repo == nil {
		repo, err := openRepositoryFromConfig(ctx, o.Config)
		if err != nil {
			return err
		}
		o.Repo = repo
		o.closeRepo = true // New opened it, so Shutdown closes it
	}
	return nil
}

// openRepositoryFromConfig opens the engine named by db.driver and wraps its
// pool(s) in a store.Repository. It is the same three lines cmd/dillad's
// openRepository runs, without that function's exit-code wrapping: a library
// does not choose the process's exit status.
func openRepositoryFromConfig(_ context.Context, c *config.Config) (store.Repository, error) {
	switch c.DB.Driver {
	case "sqlite":
		write, err := sqlite.OpenWrite(c.DB.Path)
		if err != nil {
			return nil, fmt.Errorf("dillad: open database: %w", err)
		}
		read, err := sqlite.OpenRead(c.DB.Path)
		if err != nil {
			_ = write.Close()
			return nil, fmt.Errorf("dillad: open database: %w", err)
		}
		return sqlite.New(write, read), nil
	case "postgres":
		db, err := postgres.Open(c.DB.DSN, c.DB.MaxOpenConns, c.DB.ConnMaxLifetime.Value())
		if err != nil {
			return nil, fmt.Errorf("dillad: open database: %w", err)
		}
		return postgres.New(db), nil
	default:
		// config.Validate already refuses any other value; this keeps the
		// function total on its own.
		return nil, fmt.Errorf("dillad: db.driver %q is neither sqlite nor postgres", c.DB.Driver)
	}
}

// readSecretFile reads a 0600 secret file and trims the trailing newline an
// editor or a `echo … > file` leaves behind: a client secret with a stray \n is
// rejected by the identity provider with an error that names nothing useful.
func readSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("dillad: secret file %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("dillad: secret file %s has mode %04o; a secret file must be 0600", path, perm)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is a *_file setting the operator wrote in dilla.toml, checked for mode 0600 above
	if err != nil {
		return "", fmt.Errorf("dillad: secret file %s: %w", path, err)
	}
	return string(bytes.TrimRight(b, "\r\n")), nil
}
