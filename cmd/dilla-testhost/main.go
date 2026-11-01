// Command dilla-testhost runs dillad with a second /debug control listener, for driving the Rust
// testkit outside `go test`. It is never built into a release: `cmd/dillad` is the only binary
// the release job builds, and this one imports internal/dillad/dilladtest, which that binary must
// not depend on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	botruntime "github.com/jonasthim/dilla/internal/bot/runtime"
	"github.com/jonasthim/dilla/internal/dillad"
	"github.com/jonasthim/dilla/internal/dillad/dilladtest"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store/sqlite"
)

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "dilla-testhost:", err)
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "dilla-testhost:", err)
		os.Exit(1)
	}
}

type options struct {
	public, control, dataDir, core, logLevel, webRoot string
	webauthnOrigin                                    string
	botRuntimeSecret                                  string
	productionACL                                     bool
	sfu                                               sfuOptions
	// wire is the one wire version the hello offers (-wire, web3.8 S1).
	wire uint64
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("dilla-testhost", flag.ContinueOnError)
	public := fs.String("listen", "127.0.0.1:8443", "public /v1 and /gateway listener")
	control := fs.String("control", "127.0.0.1:8444", "the /debug control listener")
	dataDir := fs.String("data-dir", "", "instance data directory (default: a temp directory)")
	core := fs.String("core", "", "the wasm32-wasip1 dilla_core_wasi.wasm (default: beside this binary)")
	logLevel := fs.String("log-level", "warn", "the instance log level: debug, info, warn or error")
	withSFU := fs.Bool("sfu", false, "start an in-process LiveKit beside the instance (browser media tests)")
	sfuPort := fs.Int("sfu-port", dilladtest.DefaultSFUPort, "the in-process LiveKit's signalling port")
	sfuUDPPort := fs.Int("sfu-udp-port", dilladtest.DefaultSFUUDPPort, "the in-process LiveKit's ICE/UDP port")
	sfuNoInternalIP := fs.Bool("sfu-no-internal-ip", false,
		"SP-27 only: render advertise_internal_ip: false (Firefox then cannot pair with the loopback node_ip)")
	sfuAV1 := fs.Bool("sfu-av1", false, "browser test only: enable AV1 in the real SFU")
	sfuDeparture := fs.Uint("sfu-departure-timeout", 0, "the in-process LiveKit's room departure_timeout in seconds (0: the instance default, 20)")
	webRoot := fs.String("web-root", "", "a built web client directory (with its dilla-manifest.json) served at the public listener's origin instead of the embedded placeholder")
	productionACL := fs.Bool("production-acl", false, "build the instance with production's ACL and channel seams (api.ResolverACL, api.StructureChannels) instead of the scenario harness's")
	webauthnOrigin := fs.String("webauthn-origin", "", "the origin a browser reaches this host from (for example http://localhost:9999): passkeys bind to its hostname and rp_origins is exactly it (empty: the instance domain's own relying party)")
	botRuntimeSecret := fs.String("bot-runtime-secret", "", "test only: create an operator runtime credential at this owner-only path")
	wire := fs.Uint64("wire", 0, "the one wire version the gateway's hello offers and GET /v1/instance advertises (version-reload e2e only); unset, the host offers dillad's production list (wire 4 for people, 3 for managed Bots, 2)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	// Wire versions start at 1: a hello offering 0 names no version any client speaks. An unset
	// flag (0) keeps dillad's own list, so people negotiate wire 4 and managed Bots wire 3.
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "wire" })
	if explicit && *wire < 1 {
		return options{}, fmt.Errorf("-wire %d: a wire version is 1 or more", *wire)
	}
	root := *webRoot
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return options{}, fmt.Errorf("-web-root %s: %w", root, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return options{}, fmt.Errorf("-web-root %s: %w", abs, err)
		}
		if !info.IsDir() {
			return options{}, fmt.Errorf("-web-root %s: not a directory", abs)
		}
		root = abs
	}
	return options{public: *public, control: *control, dataDir: *dataDir, core: *core,
		logLevel: *logLevel, webRoot: root, productionACL: *productionACL, webauthnOrigin: *webauthnOrigin,
		botRuntimeSecret: *botRuntimeSecret,
		wire:             *wire,
		sfu: sfuOptions{enabled: *withSFU, port: *sfuPort, udpPort: *sfuUDPPort,
			noInternalIP: *sfuNoInternalIP, av1: *sfuAV1, departure: uint32(min(*sfuDeparture, 3600))}}, nil
}

// sfuOptions are the -sfu flags: whether the host starts an in-process LiveKit, and on which ports.
type sfuOptions struct {
	enabled       bool
	port, udpPort int
	noInternalIP  bool
	av1           bool
	departure     uint32
}

func (o options) hostOptions(dataDir, core, scrapeToken string) dilladtest.HostOptions {
	ho := dilladtest.HostOptions{
		DataDir: dataDir, CorePath: core, LogLevel: o.logLevel,
		SFU: o.sfu.enabled, SFUPort: o.sfu.port, SFUUDPPort: o.sfu.udpPort,
		SFUNoInternalIP: o.sfu.noInternalIP, SFUEnableAV1: o.sfu.av1, SFUDepartureTimeout: o.sfu.departure,
		ScrapeToken: scrapeToken, WebRoot: o.webRoot, ProductionACL: o.productionACL,
		WebAuthnOrigin: o.webauthnOrigin,
	}
	if o.wire != 0 {
		ho.WireVersions = []uint64{o.wire}
	}
	// A browser test (a web root) that calls (an SFU) reaches LiveKit through this host's own /rtc
	// proxy over plain ws://: the instance domain's wss:// URL resolves to nothing on loopback.
	if o.sfu.enabled && o.webRoot != "" {
		ho.LiveKitURL = "ws://" + o.public
	}
	return ho
}

func run(o options) error {
	dataDir, core := o.dataDir, o.core
	if dataDir == "" {
		dir, err := os.MkdirTemp("", "dilla-testhost-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		dataDir = dir
	}
	if core == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		core = filepath.Join(filepath.Dir(exe), dillad.CoreFileName)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A fake clock, because advance_clock is the whole reason this binary exists: five of the
	// fifteen scenarios cross a TTL, retention or heal boundary and none of them can wait for it.
	// /metrics accepts DILLA_METRICS_TOKEN as its bearer token, exactly as dillad does, so a load
	// rig can scrape this host (docs/spikes/2026-10-capacity.md); unset, every scrape is refused.
	host, err := dilladtest.NewHost(ctx, o.hostOptions(dataDir, core, os.Getenv("DILLA_METRICS_TOKEN")))
	if err != nil {
		return err
	}
	defer func() { _ = host.Close(context.Background()) }()
	if o.botRuntimeSecret != "" {
		if err := createBotRuntimeSecret(ctx, filepath.Join(dataDir, "dilla.db"), o.botRuntimeSecret); err != nil {
			return fmt.Errorf("create test bot runtime credential: %w", err)
		}
	}

	publicLn, err := (&net.ListenConfig{}).Listen(ctx, "tcp", o.public)
	if err != nil {
		return err
	}
	controlLn, err := (&net.ListenConfig{}).Listen(ctx, "tcp", o.control)
	if err != nil {
		return err
	}

	publicSrv := &http.Server{Handler: host.Handler(), ReadHeaderTimeout: 10 * time.Second}
	controlSrv := &http.Server{
		Handler:           dilladtest.ControlHandler(host),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 2)
	go func() { errs <- publicSrv.Serve(publicLn) }()
	go func() { errs <- controlSrv.Serve(controlLn) }()

	fmt.Printf("public  http://%s\n", publicLn.Addr())
	fmt.Printf("control http://%s\n", controlLn.Addr())
	if o.webRoot != "" {
		fmt.Printf("web     %s\n", o.webRoot)
	}
	if s := host.SFU(); s != nil {
		fmt.Printf("sfu     %s\n", s.URL())
	}
	fmt.Printf("run a scenario with:\n  DILLA_TESTKIT_CONTROL=http://%s DILLA_TESTKIT_INVITE=%s \\\n"+
		"    dilla-testkit run testkit/scenarios/<name>.scn --ds http://%s\n",
		controlLn.Addr(), host.Invite(), publicLn.Addr())

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = publicSrv.Shutdown(shutdownCtx)
	_ = controlSrv.Shutdown(shutdownCtx)
	return nil
}

// createBotRuntimeSecret registers a real runtime credential through the production
// runtime manager. It gives the browser acceptance harness a credential for its
// separate dillabot process without manufacturing an installation row.
func createBotRuntimeSecret(ctx context.Context, database, path string) error {
	if !filepath.IsAbs(database) || !filepath.IsAbs(path) || path == database {
		return errors.New("database and credential paths must be distinct absolute paths")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G302: this is a directory and needs owner search permission.
		return err
	}
	write, err := sqlite.OpenWrite(database)
	if err != nil {
		return err
	}
	read, err := sqlite.OpenRead(database)
	if err != nil {
		_ = write.Close()
		return err
	}
	repo := sqlite.New(write, read)
	defer func() { _ = repo.Close() }()
	manager := botruntime.New(repo, nil)
	_, err = manager.Create(ctx, func(_ id.ID, token string) error {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: local testhost CLI supplies this absolute credential path; exclusive owner-only creation prevents overwrite.
		if err != nil {
			return err
		}
		cleanup := true
		defer func() {
			_ = file.Close()
			if cleanup {
				_ = os.Remove(path)
			}
		}()
		if _, err := io.WriteString(file, token+"\n"); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		cleanup = false
		return nil
	})
	return err
}
