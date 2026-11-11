package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// fileJournalMode reads path's own journal mode on a connection that sets no
// pragmas.
//
// It must run BEFORE any pool is opened with the write DSN: modernc executes
// _pragma=journal_mode(WAL) on every new connection, so a database someone
// left in DELETE mode is silently converted back to WAL by the very act of
// opening it, and a read-back through that pool can never fail.
func fileJournalMode(ctx context.Context, path string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return "", err
	}
	return strings.ToLower(mode), nil
}

// doctorHTTPClient is the client the clock leg samples with. A variable so a
// test can point it elsewhere; five seconds bounds one peer.
var doctorHTTPClient = &http.Client{Timeout: 5 * time.Second}

// doctorWasi is the wasi leg's probe, a variable so a test can stand in for the
// artifact a `go test` binary never has beside it.
var doctorWasi = doctorWasiLeg

// peerList is a repeatable --clock-peer flag.
type peerList []string

func (p *peerList) String() string     { return strings.Join(*p, ",") }
func (p *peerList) Set(v string) error { *p = append(*p, v); return nil }

// runDoctor is the full check inventory, in this order: config (parse plus
// Validate), database (db.PingContext plus the goose schema version), pragmas,
// data_dir (mode and ownership), wasi (artifact load plus dilla_abi), clock
// (skew against doctor.clock_peers), certificate, turn, udp and blobs. It
// prints ops.Report's stable text, one line per leg, and exits exit.Unavailable
// if any leg is red; a yellow leg alone exits 0.
//
// doctor takes no data-directory lock, because it must be runnable against a
// live instance: it opens the database the way serve does and reads the
// certificate and the blob tree without writing to either.
func runDoctor(args []string, stdout, stderr io.Writer) error {
	flags, cfgPath := newFlagSet("doctor", stderr)
	var clockPeers peerList
	flags.Var(&clockPeers, "clock-peer", "an HTTPS origin to read the Date header from; repeatable, replaces doctor.clock_peers")
	quiet := flags.Bool("quiet", false, "print only the legs that are not OK (for a container health check)")
	if err := parse(flags, args, stdout); err != nil {
		return err
	}

	var report ops.Report
	add := func(l ops.Leg) { report.Legs = append(report.Legs, l) }
	fail := func(leg string, err error) { add(ops.Leg{Name: leg, Status: ops.Red, Detail: err.Error()}) }
	finish := func() error {
		out := report
		if *quiet {
			out = ops.Report{}
			for _, l := range report.Legs {
				if l.Status != ops.Green {
					out.Legs = append(out.Legs, l)
				}
			}
		}
		fmt.Fprint(stdout, out.Text())
		if !*quiet {
			fmt.Fprint(stdout, report.Summary())
		}
		if report.Worst() == ops.Red {
			return fmt.Errorf("doctor: one or more checks failed: %w", exit.Unavailable)
		}
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail("config", err)
		_ = finish()
		return err
	}
	add(ops.Leg{Name: "config", Status: ops.Green, Detail: "parsed and validated"})

	ctx := context.Background()

	// Pragma leg, half 1: the file's own journal mode, read on a mode=ro
	// connection that sets no pragmas of its own. This MUST run before any pool
	// is opened with the write DSN (the database leg, right below): modernc
	// executes _pragma=journal_mode(WAL) on every new connection, so opening
	// the file first would silently convert a database left in DELETE mode
	// back to WAL and this leg could then never fail. journal_mode is a
	// property of the file, not of any one connection (facts-storage §2.3).
	var pragmaErr error
	if cfg.DB.Driver == "sqlite" {
		mode, err := fileJournalMode(ctx, cfg.DB.Path)
		switch {
		case err != nil:
			pragmaErr = err
		case mode != "wal":
			pragmaErr = fmt.Errorf("journal_mode is %q, want wal", mode)
		}
	}

	// Leg 2: db.PingContext plus the goose schema version. Opening the
	// repository is what (harmlessly, now) converts the file back to WAL; the
	// pragma leg above already captured the value before that happened.
	repo, db, err := openRepository(cfg)
	if err != nil {
		fail("database", err)
	} else {
		defer func() { _ = repo.Close() }()
		if err := db.PingContext(ctx); err != nil {
			fail("database", err)
		} else if version, err := repo.SchemaVersion(ctx); err != nil {
			fail("database", err)
		} else if err := doctorMigrations(ctx, cfg, db); err != nil {
			fail("database", err)
		} else {
			add(ops.Leg{Name: "database", Status: ops.Green, Detail: fmt.Sprintf("schema version %d", version)})
		}

		// Pragma leg, half 2: busy_timeout, foreign_keys and synchronous are
		// per-connection and can only be checked on a connection that asked for
		// them, i.e. the pool openRepository just opened.
		if pragmaErr == nil && cfg.DB.Driver == "sqlite" {
			pragmaErr = sqlite.VerifyPragmas(ctx, db, sqlite.WantPragmas)
		}
	}

	switch {
	case cfg.DB.Driver != "sqlite":
		add(ops.Leg{Name: "pragmas", Status: ops.Green, Detail: "skipped (not sqlite)"})
	case pragmaErr != nil:
		fail("pragmas", pragmaErr)
	default:
		add(ops.Leg{Name: "pragmas", Status: ops.Green, Detail: "journal_mode, busy_timeout, foreign_keys and synchronous as required"})
	}

	// Leg 4: the data directory's mode and ownership. StateDirectoryMode=0700
	// (facts-ops.md §6.3) means group and world must have no access at all.
	if info, err := os.Stat(cfg.Instance.DataDir); err != nil {
		fail("data_dir", err)
	} else if perm := info.Mode().Perm(); perm&0o077 != 0 {
		fail("data_dir", fmt.Errorf("%s has mode %04o; group and world must have no access", cfg.Instance.DataDir, perm))
	} else {
		add(ops.Leg{Name: "data_dir", Status: ops.Green, Detail: fmt.Sprintf("%s mode %04o", cfg.Instance.DataDir, perm)})
	}

	// Leg 5: the wasi artifact load plus dilla_abi.
	if info, err := doctorWasi(ctx); err != nil {
		fail("wasi", err)
	} else {
		add(ops.Leg{Name: "wasi", Status: ops.Green, Detail: fmt.Sprintf("abi_version=%d core_version=%s", info.ABIVersion, info.CoreVersion)})
	}

	// Leg 6: clock skew, the median over doctor.clock_peers. serve never gates
	// on it: a booting LXC with no network yet must still start.
	peers := cfg.Doctor.ClockPeers
	if len(clockPeers) > 0 {
		peers = clockPeers
	}
	add(ops.ClockLeg(ctx, doctorHTTPClient, peers, cfg.Doctor.ClockSkewMax.Value()))

	add(doctorCertificateLeg(ctx, cfg))
	add(doctorTURNLeg(ctx, cfg))
	add(ops.UDPLeg(*cfg))
	add(ops.WebAuthnLeg(*cfg))
	add(doctorBlobsLeg(ctx, cfg, repo))
	add(doctorBotLeg(cfg))
	add(doctorBotHealthLeg(cfg))

	return finish()
}

// doctorBotHealthLeg prints, from the failing-since file the running dillad keeps in its data directory (Bot v2 Task 17
// fix round 2), "commands failing since <time>" for each community whose Bot dillad judged degraded, by dillad's own
// clock. Yellow, not red: a degraded Bot is not a fault of the server, and a container health check on doctor --quiet
// must not restart dillad for it.
func doctorBotHealthLeg(cfg *config.Config) ops.Leg {
	const name = "bot_health"
	if !cfg.Bot.Enabled {
		return ops.Leg{Name: name, Status: ops.Green, Detail: "disabled ([bot] enabled = false)"}
	}
	failing, err := api.ReadBotHealthStates(filepath.Join(cfg.Instance.DataDir, api.BotHealthFile))
	if err != nil {
		return ops.Leg{Name: name, Status: ops.Yellow, Detail: err.Error(), Fix: "dillad rewrites the file on its next judgement"}
	}
	if len(failing) == 0 {
		return ops.Leg{Name: name, Status: ops.Green, Detail: "no Bot's commands are failing"}
	}
	communities := make([]id.ID, 0, len(failing))
	for community := range failing {
		communities = append(communities, community)
	}
	slices.SortFunc(communities, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	lines := make([]string, 0, len(communities))
	for _, community := range communities {
		line := fmt.Sprintf("commands failing since %s (community %s)", failing[community].Since.UTC().Format(time.RFC3339), community.String())
		if n := failing[community].FailedCommands; n > 0 {
			line += fmt.Sprintf(": %d failed commands", n)
		}
		lines = append(lines, line)
	}
	return ops.Leg{Name: name, Status: ops.Yellow, Detail: strings.Join(lines, "; "),
		Fix: "read the Bot's lines in the service journal (journalctl -u dilla); stage and error_class name the failing step"}
}

// doctorCertificateLeg looks at the certificate certmagic holds on disk. It
// never asks certmagic to obtain one, so it is safe beside a live serve; the
// last renewal error lives in that process's memory and is not visible here.
func doctorCertificateLeg(ctx context.Context, cfg *config.Config) ops.Leg {
	if cfg.BehindProxy() {
		return ops.Leg{Name: "certificate", Status: ops.Green, Detail: "tls.mode is behind_proxy: the proxy terminates TLS"}
	}
	name := cfg.Instance.Domain
	if cfg.TLS.Mode == config.TLSModeACMEIP {
		name = cfg.Instance.PublicIP.String()
	}
	return ops.CertificateLeg(ctx, name, &tls.Config{
		GetCertificate: ops.CertificateFromStorage(cfg.TLS.StorageDir, name),
		MinVersion:     tls.VersionTLS12,
	}, "")
}

// doctorTURNLeg allocates against the TURN listener the configuration names:
// turn.listen in behind_proxy mode, where it is its own plain TCP listener, and
// otherwise the instance's own 443, where TURN shares the port behind the TLS
// demux and is reached over TLS.
func doctorTURNLeg(ctx context.Context, cfg *config.Config) ops.Leg {
	const name = "turn"
	switch {
	case !cfg.TURN.Enabled:
		// Yellow, not red: turn.enabled = false is a legitimate topology (a LAN
		// plus one forwarded UDP port), not a fault.
		return ops.Leg{Name: name, Status: ops.Yellow,
			Detail: "turn.enabled is false: no relay is offered, so voice needs direct UDP reachability"}
	case !cfg.Doctor.TURNProbe:
		return ops.Leg{Name: name, Status: ops.Green, Detail: "skipped: doctor.turn_probe is false"}
	case cfg.TURN.ProxyProtocol:
		return ops.Leg{Name: name, Status: ops.Yellow,
			Detail: "turn.proxy_protocol is true: the listener refuses a connection without a PROXY header, so doctor cannot probe it directly",
			Fix:    "allocate through the proxy from another network"}
	}
	secret, err := ops.ReadSecret(cfg.TURN.SharedSecretFile)
	if err != nil {
		return ops.Leg{Name: name, Status: ops.Red, Detail: "turn.shared_secret_file: " + err.Error(),
			Fix: "run dillad init, or create the file with 32 random bytes at mode 0600"}
	}
	listen := cfg.TURN.Listen
	if !cfg.BehindProxy() {
		host := cfg.Instance.Domain
		if cfg.TLS.Mode == config.TLSModeACMEIP {
			host = cfg.Instance.PublicIP.String()
		}
		_, port, err := net.SplitHostPort(cfg.Server.Listen)
		if err != nil || port == "" {
			port = "443"
		}
		listen = "tls://" + net.JoinHostPort(host, port)
	}
	return ops.TURNLeg(ctx, listen, cfg.TURN.Realm, secret)
}

// doctorBlobsLeg cross-checks the blob rows against the blob tree. It never
// creates the blob directory: a directory serve has not made yet is an
// instance that has not stored an attachment.
func doctorBlobsLeg(ctx context.Context, cfg *config.Config, repo store.Repository) ops.Leg {
	const name = "blobs"
	if repo == nil {
		return ops.Leg{Name: name, Status: ops.Yellow, Detail: "skipped: the database did not open"}
	}
	if _, err := os.Stat(cfg.Blobs.Dir); errors.Is(err, fs.ErrNotExist) {
		return ops.Leg{Name: name, Status: ops.Yellow, Detail: cfg.Blobs.Dir + " does not exist yet: serve creates it"}
	}
	bs, err := blob.Open(cfg.Blobs.Dir, cfg.Blobs.Backend)
	if err != nil {
		return ops.Leg{Name: name, Status: ops.Red, Detail: err.Error()}
	}
	defer func() { _ = bs.Close() }()
	return ops.BlobConsistencyLeg(ctx, repo, bs)
}

// doctorWasiLeg loads dilla-core-wasi from the conventional path next to the
// dillad binary and calls dilla_abi. Nothing in dillad yet configures this
// path — the wasi ABI is wired into serve only from task 14 onward — so this
// leg is red until the artifact is placed there.
func doctorWasiLeg(ctx context.Context) (mlswasi.ABIInfo, error) {
	exe, err := os.Executable()
	if err != nil {
		return mlswasi.ABIInfo{}, fmt.Errorf("locate the dillad binary: %w", err)
	}
	path := filepath.Join(filepath.Dir(exe), "dilla_core_wasi.wasm")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the running binary's own directory joined with a constant file name
	if err != nil {
		return mlswasi.ABIInfo{}, fmt.Errorf("read %s: %w", path, err)
	}
	rt, err := mlswasi.New(ctx, raw, mlswasi.Options{})
	if err != nil {
		return mlswasi.ABIInfo{}, fmt.Errorf("load %s: %w", path, err)
	}
	defer func() { _ = rt.Close(ctx) }()
	return rt.ABI(ctx)
}

// doctorMigrations fails the database leg when the database skipped a migration (see migrationState).
func doctorMigrations(ctx context.Context, cfg *config.Config, db *sql.DB) error {
	provider, err := migrationProvider(cfg, db)
	if err != nil {
		return err
	}
	if current, target, err := provider.GetVersions(ctx); err != nil {
		return err
	} else if current > target {
		return fmt.Errorf("database schema %d is newer than this binary's highest migration %d; this dillad will not serve it", current, target)
	}
	_, err = migrationState(ctx, provider)
	return err
}
