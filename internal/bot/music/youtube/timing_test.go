package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerConnectTimingIsBoundedAndRedacted(t *testing.T) {
	for _, tc := range []struct{ host, class string }{{"www.youtube.com", "youtube"}, {"rr-secret.googlevideo.com", "googlevideo"}, {"i.ytimg.com", "other"}} {
		t.Run(tc.class, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := WithDiagnosticLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			a, b := net.Pipe()
			defer b.Close()
			calls := 0
			conn, err := dialPublicHTTPSWith(ctx, tc.host, func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111")}, {IP: net.ParseIP("8.8.8.8").To4()}}, nil
			}, func(context.Context, string, string) (net.Conn, error) {
				calls++
				if calls == 1 {
					return nil, errors.New("not reachable")
				}
				return a, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			var got map[string]any
			if err = json.Unmarshal(logs.Bytes(), &got); err != nil {
				t.Fatalf("missing CONNECT log: %s", logs.String())
			}
			if got["host_class"] != tc.class || got["family"] != "ipv4" || got["failed_addresses"] != float64(1) || got["dns_ms"] == nil || got["dial_ms"] == nil || strings.Contains(logs.String(), tc.host) || strings.Contains(logs.String(), "rr-secret") {
				t.Fatalf("CONNECT log=%s", logs.String())
			}
		})
	}
}
func TestBrokerTimingKeepsAllPublicAddressValidation(t *testing.T) {
	called := false
	_, err := dialPublicHTTPSWith(t.Context(), "youtube.com", func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8").To4()}, {IP: net.ParseIP("127.0.0.1").To4()}}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("unexpected dial")
	})
	if !errors.Is(err, errEgressPolicy) || called {
		t.Fatalf("mixed public/private answer dialed: err=%v dialed=%v", err, called)
	}
}
func TestWorkerWritesMonotonicStartMarksWithoutChangingPCM(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "egress.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ready := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			ready <- err
			return
		}
		defer c.Close()
		raw := make([]byte, len(egressReady))
		_, err = io.ReadFull(c, raw)
		if err == nil {
			_, err = c.Write([]byte(egressAck))
		}
		ready <- err
	}()
	yt := writeScript(t, dir, "yt", `printf encoded`)
	ff := writeScript(t, dir, "ff", `cat`)
	// Run the embedded worker with a private broker socket; no production mount or network is changed.
	worker := strings.ReplaceAll(sandboxWorker, "/run/egress.sock", socket)
	cmd := exec.CommandContext(t.Context(), python, "-I", "-c", worker, "pcm", yt, ff, "", "https://www.youtube.com/watch?v=abcdefghijk", "1024", "0.000")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("worker=%v stderr=%s", err, stderr.String())
	}
	if err = <-ready; err != nil {
		t.Fatal(err)
	}
	if string(out) != "encoded" {
		t.Fatalf("PCM changed: %q", out)
	}
	marks := map[string]int64{}
	for _, line := range strings.Split(stderr.String(), "\n") {
		var mark string
		var stamp int64
		if _, err := fmt.Sscanf(line, "DILLA_YT_TIMING %s %d", &mark, &stamp); err == nil {
			marks[mark] = stamp
		}
	}
	if marks["spawn"] <= 0 || marks["first_yt_dlp_byte"] < marks["spawn"] || marks["first_pcm"] < marks["first_yt_dlp_byte"] {
		t.Fatalf("missing or unordered marks: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "abcdefghijk") || strings.Contains(stderr.String(), "https://") {
		t.Fatalf("worker timing leaked source: %s", stderr.String())
	}
}

func TestWorkerTimingLogsOnlyValidatedMarks(t *testing.T) {
	var logs bytes.Buffer
	b := &sandboxStderr{ctx: t.Context(), logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for _, part := range []string{"DILLA_YT_TIMING first_yt_", "dlp_byte 123\n", "DILLA_YT_TIMING https://private.invalid/video 456\n", "DILLA_YT_TIMING first_pcm https://private.invalid/secret\n"} {
		if _, err := b.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(logs.String(), `"mark":"first_yt_dlp_byte"`) || !strings.Contains(logs.String(), `"monotonic_ns":123`) || strings.Contains(logs.String(), "private.invalid") {
		t.Fatalf("worker logs=%s", logs.String())
	}
}
