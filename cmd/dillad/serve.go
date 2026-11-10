package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	postgresmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// secureFiles is sqlite.SecureFiles; tests swap it to make the chmod fail.
var secureFiles = sqlite.SecureFiles

// metricsTokenEnv carries the bearer token /metrics is guarded with when
// metrics.require_admin is set. It is an environment variable and not a
// dilla.toml key because dilla.toml is configuration an operator diffs and
// copies around, while this is a credential: systemd's EnvironmentFile= is
// where the unit already keeps them. An unset variable is not a way in —
// dillad.New substitutes an unguessable random token and logs that no scrape
// will succeed.
const metricsTokenEnv = "DILLA_METRICS_TOKEN" //nolint:gosec // G101: the name of an environment variable, not a credential

// openRepository opens the configured storage engine's pool(s) and wraps them
// in a store.Repository. The *sql.DB it also returns is the pool migrations,
// pragma checks and PingContext run against: the single write pool for
// SQLite (repo.Close closes both the write and the read pool), the one shared
// pool for Postgres.
func openRepository(c *config.Config) (store.Repository, *sql.DB, error) {
	switch c.DB.Driver {
	case "sqlite":
		write, err := sqlite.OpenWrite(c.DB.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("open database: %w: %w", err, exit.Unavailable)
		}
		// Also repairs an install whose -wal and -shm were made before OpenWrite pre-created the
		// file. A failure (EPERM on a file another user owns) is a warning, not a reason to refuse
		// to start: SQLite would have opened that file all the same.
		if err := secureFiles(c.DB.Path); err != nil {
			slog.Warn("could not restrict the database files to 0600; fix their owner or mode", "path", c.DB.Path, "err", err)
		}
		read, err := sqlite.OpenRead(c.DB.Path)
		if err != nil {
			_ = write.Close()
			return nil, nil, fmt.Errorf("open database: %w: %w", err, exit.Unavailable)
		}
		return sqlite.New(write, read), write, nil
	case "postgres":
		db, err := postgres.Open(c.DB.DSN, c.DB.MaxOpenConns, c.DB.ConnMaxLifetime.Value())
		if err != nil {
			return nil, nil, fmt.Errorf("open database: %w: %w", err, exit.Unavailable)
		}
		return postgres.New(db), db, nil
	default:
		// config.Validate already refuses any other value; this is unreachable
		// through Load but kept so openRepository is total on its own.
		return nil, nil, fmt.Errorf("db.driver %q is neither sqlite nor postgres: %w", c.DB.Driver, exit.Config)
	}
}

// sqliteMigrations is the SQLite migration set every verb builds its goose
// Provider over. It is a variable only so a test can hand serve a set with a
// failing migration: the embedded directory is fixed at compile time.
var sqliteMigrations fs.FS = sqlitemigrations.FS

// migrationProvider builds a goose Provider over db (as openRepository
// returned it), using the migration set for c's configured engine.
func migrationProvider(c *config.Config, db *sql.DB) (*goose.Provider, error) {
	switch c.DB.Driver {
	case "sqlite":
		p, err := goose.NewProvider(goose.DialectSQLite3, db, sqliteMigrations)
		if err != nil {
			return nil, fmt.Errorf("migrations: %w: %w", err, exit.Software)
		}
		return p, nil
	case "postgres":
		p, err := goose.NewProvider(goose.DialectPostgres, db, postgresmigrations.FS)
		if err != nil {
			return nil, fmt.Errorf("migrations: %w: %w", err, exit.Software)
		}
		return p, nil
	default:
		return nil, fmt.Errorf("db.driver %q is neither sqlite nor postgres: %w", c.DB.Driver, exit.Config)
	}
}

// newMetrics is dillad's metric surface as /metrics serves it: every dilla_* series on dillad's own
// registry, and LiveKit's — which registers on the default registry — through sfu.LiveKitGatherer,
// which drops the labels a call member writes (protocol_version, sdk, …), so no client can grow
// /metrics. A dilla_* series registered anywhere but on the returned Metrics' registry is never
// served (I-1 of the integration re-review).
func newMetrics() *obs.Metrics {
	reg := prometheus.NewRegistry()
	return obs.NewMetrics(reg, prometheus.Gatherers{reg, sfu.LiveKitGatherer(prometheus.DefaultGatherer)})
}

// runServe loads the config, takes the data-directory lock (ops.AcquireServeLock),
// opens the repository, migrates it (when db.auto_migrate and a migration is
// pending, after a VACUUM INTO backup when db.pre_migration_backup, which is put
// back if the migration fails), refuses to start when the database's schema is
// newer than this binary's highest migration, and then serves
// internal/dillad's composition root — which finishes a pending `dillad
// restore` — until an interrupt drains it. Everything above the composition root is start-up
// order; every route, middleware and timeout lives in dillad.New, so the
// binary's surface and an end-to-end test's surface are one thing.
func runServe(args []string, stdout, stderr io.Writer) error {
	fs, cfgPath := newFlagSet("serve", stderr)
	desktopDownloads := fs.Bool("desktop-downloads", false, "serve desktop downloads for browser navigation (enable only after desktop acceptance)")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// The Bot's keeper starts first, while this process still holds CAP_SETUID, CAP_SETGID and CAP_KILL
	// (dilla.service's ambient set), and those leave this process right after, from every set of every thread, before
	// anything below opens a listener (Bot v2 task 11). From here on only the keeper can start a process as another
	// user, and only the configured Bot as the configured user; and only the keeper can signal it.
	keeper, err := startBotKeeper(cfg, *cfgPath)
	if err != nil {
		return err
	}
	if keeper != nil {
		defer keeper.close()
	}
	if err := botsupervisor.DropServingCapabilities(slog.Default(), cfg.Bot.Enabled && cfg.Bot.DevSameUser); err != nil {
		return fmt.Errorf("serve: drop the keeper's capabilities: %w: %w", err, exit.Software)
	}

	// The data-directory lock (Plan 2 task 13): dilla.serve.lock exclusively,
	// so a second serve is refused, and dilla.lock shared, so a backup may run
	// beside this serve and a restore may not. It is held for the life of the
	// process and covers the blob sweeper, which runs inside it.
	lock, err := ops.AcquireServeLock(cfg.Instance.DataDir)
	if err != nil {
		if errors.Is(err, ops.ErrLocked) {
			return fmt.Errorf("serve: %w: another dillad is serving this data directory, or a restore is running: %w",
				err, exit.TempFail)
		}
		return fmt.Errorf("serve: %w: %w", err, exit.CantCreate)
	}
	defer func() { _ = lock.Release() }()
	// A restore that stopped half-way through its swap left the directory in
	// two halves; serving either would serve the wrong instance.
	if err := ops.InterruptedRestore(cfg.Instance.DataDir); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	repo, db, err := openRepository(cfg)
	if err != nil {
		return err
	}
	repoClosed := false
	defer func() {
		if !repoClosed {
			_ = repo.Close()
		}
	}()

	ctx := context.Background()
	provider, err := migrationProvider(cfg, db)
	if err != nil {
		return err
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("serve: read schema version: %w: %w", err, exit.Data)
	}
	if current > target {
		return fmt.Errorf("serve: database schema %d is newer than this binary's highest migration %d: refusing to start: %w",
			current, target, exit.Data)
	}
	pending, err := migrationState(ctx, provider)
	if err != nil {
		return fmt.Errorf("serve: %w: %w", err, exit.Data)
	}
	var backupPath string
	if cfg.DB.AutoMigrate && pending {
		// R35: migrate at start, after the pre-migration backup — which is
		// taken only when there is a migration to run, so an ordinary restart
		// leaves no file behind. A restored database is migrated here too.
		if cfg.DB.PreMigrationBackup && cfg.DB.Driver == "sqlite" {
			backupPath = fmt.Sprintf("%s.pre-migration-%d", cfg.DB.Path, time.Now().Unix())
			if err := store.VacuumInto(ctx, db, backupPath); err != nil {
				return fmt.Errorf("serve: pre-migration backup: %w: %w", err, exit.CantCreate)
			}
		}
		if _, err := provider.Up(ctx); err != nil {
			if backupPath == "" {
				return fmt.Errorf("serve: migrate: %w: %w", err, exit.Software)
			}
			// "Migration failure restores the pre-migration backup and exits
			// non-zero with the heal protocol pending" (the spec's operational
			// rules). The mark is read before the pools close: it is what says
			// whether a restore's heal is still owed.
			pending, _ := repo.GetSetting(ctx, ds.RestorePendingKey)
			_ = repo.Close()
			repoClosed = true
			if rerr := ops.RestoreFile(backupPath, cfg.DB.Path); rerr != nil {
				return fmt.Errorf("serve: migration failed (%w) and restoring the pre-migration backup %s failed too: %w: %w",
					err, backupPath, rerr, exit.IOErr)
			}
			heal := "no heal is pending: the database is again what it was before this start"
			if len(pending) > 0 {
				heal = "the group heal protocol is still pending: the restore it belongs to finishes on the next successful start"
			}
			fmt.Fprintf(stderr, "dillad serve: migration to schema %d failed; restored the pre-migration backup %s over %s; %s\n",
				target, backupPath, cfg.DB.Path, heal)
			return fmt.Errorf("serve: migrate: %w; restored the pre-migration backup; %s: %w", err, heal, exit.Data)
		}
	}

	log := obs.NewLogger(cfg.Log, stderr)
	if backupPath != "" {
		// Only now, with the migration done: a failed one restores the newest
		// copy, so nothing may be removed before this point. A copy that will
		// not delete is logged and must not stop the server.
		if err := prunePreMigrationCopies(cfg.DB.Path, preMigrationKeep, backupPath, log); err != nil {
			log.Warn("could not prune old pre-migration copies", "err", err)
		}
	}
	metrics := newMetrics()
	health := obs.NewHealth(clock.System())

	// The blob store, its start-up sweep of interrupted uploads and the
	// garbage collector (Plan 2 task 11). SweepTemp runs before the listener
	// accepts an upload, so it can never remove one in flight.
	blobStore, err := blob.Open(cfg.Blobs.Dir, cfg.Blobs.Backend)
	if err != nil {
		return fmt.Errorf("serve: open blob store %s: %w: %w", cfg.Blobs.Dir, err, exit.CantCreate)
	}
	defer func() { _ = blobStore.Close() }()
	if n, err := blobStore.SweepTemp(); err != nil {
		return fmt.Errorf("serve: sweep temp uploads: %w: %w", err, exit.IOErr)
	} else if n > 0 {
		log.Info("removed interrupted uploads", "count", n)
	}
	sweeper := blob.NewSweeper(repo, blobStore, clock.System(), cfg.Blobs.GCGrace.Value(),
		cfg.Blobs.PendingTTL.Value(), cfg.Blobs.GCInterval.Value(), log).WithMetrics(metrics)

	// The in-process SFU when livekit.enabled (Plan 2 task 16). It starts before the composition
	// root, which builds the call routes over its token mint and proxies /rtc to it, and it stops
	// after the drain below — its deferred stop runs after front.close.
	// The relay's revocation state (dilla-media task 13 review I1), shared by the TURN relay and the
	// call routes: a cut device loses its relay allocations, and the relay asks the store, after
	// authenticating a request, whether a device another process revoked is barred.
	relayRev := server.NewRelayRevocations(max(cfg.TURN.MaxAllocationAge.Value(), cfg.TURN.CredentialTTL.Value()), clock.System()).
		WithBarred(api.BarredDevices{Repo: repo}, log).WithCredentialTTL(cfg.TURN.CredentialTTL.Value())
	fd := frontDeps{cfg: cfg, log: log, health: health, metrics: metrics, stdout: stdout, relay: relayRev}
	// The SFU's webhook listener is bound before the SFU starts, so LiveKit's webhook URL names the
	// port it got, and served once the composition root exists (dilla-media task 12).
	whln, err := listenWebhook(ctx, fd)
	if err != nil {
		return err
	}
	webhookServed := false
	defer func() {
		if whln != nil && !webhookServed {
			_ = whln.Close()
		}
	}()
	var sfuServer *sfu.Server
	var callSFU dillad.SFU
	var remoteSFU *sfu.Remote
	stopSFU := func() {}
	if cfg.LiveKit.Mode == "sidecar" && cfg.LiveKit.Enabled {
		body, readErr := os.ReadFile(cfg.LiveKit.APISecretFile)
		if readErr != nil {
			return fmt.Errorf("serve: livekit.api_secret_file: %w: %w", readErr, exit.Config)
		}
		remoteSFU = sfu.NewRemote(sfuConfig(cfg.LiveKit, string(body)))
		if probeErr := waitRemoteSFU(ctx, remoteSFU, 20*time.Second); probeErr != nil {
			return fmt.Errorf("serve: sidecar SFU at %s is unavailable: %w: %w", remoteSFU.HTTPURL(), probeErr, exit.Unavailable)
		}
		callSFU = remoteSFU
		health.Gate("livekit").Set(true, "sidecar SFU on "+remoteSFU.HTTPURL())
	} else {
		sfuServer, stopSFU, err = startSFU(ctx, fd)
		if err != nil {
			return err
		}
		if sfuServer != nil {
			callSFU = sfuServer
		}
	}
	defer stopSFU()

	// The composition root mounts every route of both plans. It shares serve's blob store, the one
	// the sweeper above collects from, and the SFU when there is one: a nil *sfu.Server must not
	// become a non-nil dillad.SFU.
	opts := dillad.Options{
		Config: cfg, Repo: repo, Clock: clock.System(), Log: log,
		Metrics: metrics, Health: health, ScrapeToken: os.Getenv(metricsTokenEnv),
		Blobs: blobStore, Relay: relayRev,
		DesktopDownloads: *desktopDownloads,
	}
	var bot *botsupervisor.Supervisor
	if keeper != nil {
		bot = botsupervisor.New(cfg.Bot, keeper.keeper, log.With("component", "bot"))
		opts.BotLaunch = bot
	}
	if callSFU != nil {
		opts.SFU = callSFU
	}
	srv, err := dillad.New(ctx, opts)
	if err != nil {
		return fmt.Errorf("serve: %w: %w", err, exit.Software)
	}
	// LiveKit's webhooks drive the call lifecycle from here on; the listener stops before the SFU.
	stopWebhook, err := serveWebhook(fd, whln, srv.CallEvents().Handle)
	if err != nil {
		_ = srv.Shutdown(context.Background())
		return err
	}
	webhookServed = true
	defer stopWebhook()

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if remoteSFU != nil {
		go watchRemoteSFU(runCtx, remoteSFU, health.Gate("livekit"))
	}

	botExit := make(chan error, 1)
	// The Bot: its Unix socket, then the child. The supervisor stops the child when runCtx ends, and serve waits for
	// that before the repository closes; the socket closes last.
	if bot != nil {
		botLn, err := openBotSocket(cfg.Bot, keeper.gid)
		if err != nil {
			_ = srv.Shutdown(context.Background())
			return fmt.Errorf("serve: bot socket %s: %w: %w", cfg.Bot.SocketPath(), err, exit.CantCreate)
		}
		botServed := make(chan struct{})
		go func() {
			defer close(botServed)
			if err := srv.ServeBot(botLn); err != nil {
				log.Error("bot socket", "err", err)
			}
		}()
		// Its own context: a serve that returns early (before runCtx ends) still stops the Bot before it waits for it.
		botCtx, stopBot := context.WithCancel(runCtx)
		botDone := make(chan struct{})
		go func() {
			defer close(botDone)
			// A keeper that is gone can start nothing again: end serve, and systemd restarts the unit with a new one.
			if err := bot.Run(botCtx); err != nil {
				botExit <- err
				stop()
			}
		}()
		defer func() {
			stopBot()
			<-botDone
			_ = botLn.Close()
			<-botServed
		}()
	}
	sfuExit := make(chan error, 1)
	if sfuServer != nil {
		died := watchSFU(runCtx, sfuServer.Done(), health.Gate("livekit"), log)
		go func() {
			if err, ok := <-died; ok {
				sfuExit <- err
				stop()
			}
		}()
	}

	// The listener tls.mode chooses (Plan 2 task 16): server.plain_listen
	// behind a proxy, or server.listen through the 443 TLS/STUN demux with
	// certmagic's certificate; and the TURN relay when turn.enabled.
	// front.close runs after the drain below has finished, and before the
	// repository closes.
	fr, err := openFront(ctx, runCtx, fd)
	if err != nil {
		_ = srv.Shutdown(context.Background())
		return err
	}
	defer fr.close()
	ln := fr.http

	// A second SIGINT/SIGTERM exits immediately rather than waiting out the
	// shutdown grace: NotifyContext only relays the first occurrence to runCtx.
	//
	// The registration and the goroutine last exactly as long as this run.
	// Left behind, they would make a later signal in the same process exit it
	// through a run that is already over — which is how an in-process test
	// that drives runServe twice loses its whole binary to the first run's
	// watcher.
	// The two signals are counted on this channel alone. Waiting on runCtx and
	// then draining forceExit with a non-blocking receive would be a race:
	// signal delivery to NotifyContext's channel and to this one is
	// independent, so the drain often runs before the FIRST signal has arrived
	// here, takes the default branch, and the blocking receive that follows
	// then treats that same first signal as the second and exits the process
	// in the middle of a perfectly ordinary drain. The buffer is 2 so a second
	// signal sent between the two receives is not dropped.
	forceExit := make(chan os.Signal, 2)
	signal.Notify(forceExit, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(forceExit)
	runOver := make(chan struct{})
	defer close(runOver)
	go func() {
		// The first signal is the one NotifyContext also relays to runCtx; it
		// starts the drain.
		select {
		case <-forceExit:
		case <-runOver:
			return
		}
		select {
		case <-forceExit:
		case <-runOver:
			return
		}
		fmt.Fprintln(stderr, "dillad: second signal received, exiting immediately")
		os.Exit(int(exit.Fail))
	}()

	// The sweeper runs for exactly as long as this serve, and runServe waits
	// for it before its deferred repo.Close and blobStore.Close, so a pass never
	// runs against a closed database. Its context is its own so that a Serve
	// that returns for a reason other than a signal still stops it.
	sweepCtx, stopSweep := context.WithCancel(runCtx)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweeper.Run(sweepCtx)
	}()
	defer func() {
		stopSweep()
		<-sweepDone
	}()

	// dilla_clock_skew_seconds, refreshed hourly from the same peers `dillad
	// doctor` reads. Nothing gates on it: a booting host with no network yet
	// must still start, so a round with no answer only leaves the gauge alone.
	clockCtx, stopClock := context.WithCancel(runCtx)
	clockDone := make(chan struct{})
	go func() {
		defer close(clockDone)
		ops.WatchClock(clockCtx, doctorHTTPClient, cfg.Doctor.ClockPeers, time.Hour, metrics.ClockSkewSeconds.Set)
	}()
	defer func() {
		stopClock()
		<-clockDone
	}()

	notifyReady()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.Value())
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown", "err", err)
		}
	}()
	// Serve returns nil on the graceful close the goroutine above triggers.
	if err := srv.Serve(runCtx, ln); err != nil {
		return fmt.Errorf("serve: %w: %w", err, exit.Unavailable)
	}
	// Shutdown closes the LISTENER first, so Serve returns ErrServerClosed —
	// and therefore nil — the instant the drain STARTS, not when it finishes.
	// Returning here would run `defer repo.Close()` on a database the requests
	// still in flight are reading, and exit the process inside shutdown_grace.
	// Waiting for the goroutine is what makes the grace real.
	select {
	case <-runCtx.Done():
		<-drained
	default:
		// Serve returned for a reason other than a signal; no drain was
		// started and the goroutine is still parked on runCtx.
	}
	select {
	case err := <-sfuExit:
		return fmt.Errorf("serve: the in-process SFU exited: %w: %w", err, exit.Unavailable)
	case err := <-botExit:
		return fmt.Errorf("serve: the Bot supervisor stopped: %w: %w", err, exit.Unavailable)
	default:
	}
	return nil
}

// notifyReady writes READY=1 to $NOTIFY_SOCKET by hand, over a unixgram
// socket: systemd's sd_notify protocol, with no dependency on
// coreos/go-systemd/v22 (Global Constraints: never added). A missing or
// unreachable socket is not an error: dillad runs the same way under a plain
// shell.
func notifyReady() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), "unixgram", sock)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("READY=1"))
}

// A sidecar can restart without the API restarting. Its readiness gate follows the actual
// RoomService connection, so an API that is up while media is down never reports ready.
func watchRemoteSFU(ctx context.Context, remote *sfu.Remote, gate *obs.Gate) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := remote.Rooms(probeCtx)
			cancel()
			if err != nil {
				gate.Set(false, "sidecar SFU unavailable: "+err.Error())
			} else {
				gate.Set(true, "sidecar SFU on "+remote.HTTPURL())
			}
		}
	}
}
