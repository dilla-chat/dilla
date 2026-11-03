// Command dilla-loadrig is dilla's own capacity rig (SP-29, DEV-64): Go participants on lksdk that
// publish and decrypt dilla-sframe/1 media through one LiveKit, and a sampler that reads the SFU
// host's counters across each cell. It publishes only what it measured. It is a test tool: the
// release job builds cmd/dillad only.
//
// It runs against an in-process LiveKit (-local-sfu, the dev-box loopback run) or against a
// dilla-testhost started with -sfu on the remote host, reached through an ssh tunnel to its
// loopback (-testhost). Never against a production dillad: its call routes sweep every room a call
// did not open, so a cell's room would be deleted under it within 30 s.
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
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dilla-chat/dilla/internal/sfu"
)

// stunSentinel is the rtc.stun_servers entry of the in-process LiveKit: a loopback port nothing
// serves, as dillad's own instance-domain sentinel is. Without it LiveKit hands every participant
// its public list (Google's and Twilio's STUN hosts) and the rig contacts them (RIGS-06).
const stunSentinel = "127.0.0.1:3478"

// localSFUConfig is the -local-sfu LiveKit.
func localSFUConfig() sfu.Config {
	cfg := sfu.DefaultConfig()
	cfg.APISecret = "dilla-loadrig-local-secret-0123456789"
	cfg.STUNServers = []string{stunSentinel}
	return cfg
}

// openOut opens -out for appending and reports whether the table header still has to be written:
// only into a file that is new or empty, so a re-run appends rows under the existing header.
func openOut(path string) (*os.File, bool, error) {
	// #nosec G304 -- the operator names -out.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, false, err
	}
	return f, info.Size() == 0, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dilla-loadrig:", err)
		os.Exit(1)
	}
}

func run() error {
	localSFU := flag.Bool("local-sfu", false, "start LiveKit in this process on 127.0.0.1:7880/7882 and measure it (the dev-box loopback run)")
	testHost := flag.String("testhost", "", "the dilla-testhost control listener through the ssh tunnel, e.g. http://127.0.0.1:8444 (its debug rooms are hidden from the call-room sweep)")
	metricsURL := flag.String("metrics", "", "the test host's /metrics URL through the ssh tunnel, e.g. http://127.0.0.1:8443/metrics")
	metricsToken := flag.String("metrics-token", "", "the test host's scrape token, sent as a Bearer token and never logged; empty reads DILLA_METRICS_TOKEN (prefer the environment: argv is readable by every local user)")
	iface := flag.String("iface", "lo", "the SFU host interface whose /proc/net/dev bytes are the wire numbers")
	sshTarget := flag.String("ssh", "", "read /proc on the SFU host through `ssh <target> cat`; empty reads locally")
	pid := flag.Int("dillad-pid", 0, "the SFU process's pid on the SFU host: dilla-testhost's (with -local-sfu: this process)")
	voice := flag.String("voice", "", "the ≥ 90 s Ogg Opus voice fixture")
	shareQ := flag.String("share-q", "", "the ≥ 90 s VP8 IVF at 480×270, 150 kbit/s")
	shareH := flag.String("share-h", "", "the ≥ 90 s VP8 IVF at 960×540, 625 kbit/s")
	shareF := flag.String("share-f", "", "the ≥ 90 s VP8 IVF at 1920×1080, 2.5 Mbit/s")
	cellsFlag := flag.String("cells", "all", `"all" or a comma list of cell names`)
	warmup := flag.Duration("warmup", 15*time.Second, "time after the last publish before sampling")
	hold := flag.Duration("hold", 60*time.Second, "sampling window per cell (at least 30 s)")
	location := flag.String("location", "", `where the SFU runs, e.g. "founder LXC, LAN"`)
	commit := flag.String("commit", "", "the measured commit")
	outPath := flag.String("out", "", "append the table rows to this file as well (the header only when the file is new)")
	flag.Parse()

	if *hold < MinHold {
		return errors.New("-hold must be at least 30s: the counter moves in 5 s steps and the card reads rate over [30s]")
	}
	cells, err := Select(*cellsFlag)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	o := Options{Iface: *iface, DilladPID: *pid, VoiceFile: *voice,
		ShareFiles: map[Layer]string{LayerQ: *shareQ, LayerH: *shareH, LayerF: *shareF}, Warmup: *warmup, Hold: *hold, Files: LocalReader}
	if *sshTarget != "" {
		o.Files = SSHReader(*sshTarget)
	}
	switch {
	case *localSFU && *testHost != "":
		return errors.New("-local-sfu and -testhost exclude each other")
	case *localSFU:
		cfg := localSFUConfig()
		srv, err := sfu.Start(ctx, cfg)
		if err != nil {
			return err
		}
		defer func() { _ = srv.Stop(context.Background()) }()
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		ms := &http.Server{Handler: promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = ms.Serve(ln) }()
		defer func() { _ = ms.Close() }()
		o.Rooms = LocalRooms{URL: srv.URL(), HTTPURL: srv.HTTPURL(), APIKey: cfg.APIKey, APISecret: cfg.APISecret}
		o.Metrics, o.DilladPID, o.Files = HTTPMetrics("http://"+ln.Addr().String()+"/metrics", ""), os.Getpid(), LocalReader
	default:
		if *testHost == "" || *metricsURL == "" || *pid == 0 {
			return errors.New("without -local-sfu, -testhost, -metrics and -dillad-pid are required: the remote legs run against dilla-testhost, never a production dillad")
		}
		token := *metricsToken
		if token == "" {
			token = os.Getenv("DILLA_METRICS_TOKEN")
		}
		o.Rooms = TestHostRooms{Control: *testHost}
		o.Metrics = HTTPMetrics(*metricsURL, token)
	}

	var out io.Writer
	header := false
	if *outPath != "" {
		f, fresh, err := openOut(*outPath)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		out, header = f, fresh
	}
	emit := func(line string, toFile bool) {
		fmt.Println(line)
		if out != nil && toFile {
			fmt.Fprintln(out, line)
		}
	}
	emit(fmt.Sprintf("<!-- egress = %s; ingress = %s; wire = /proc/net/dev %s tx -->", PromQLEgress, PromQLIngress, *iface), header)
	emit(TableHeader, header)
	date := time.Now().UTC().Format("2006-01-02")
	for _, c := range cells {
		res, err := RunCell(ctx, o, c)
		if res.Measured {
			// The row is real even when the cell failed its checks; the error says why.
			emit(res.Row(*location, date, *commit), true)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}
