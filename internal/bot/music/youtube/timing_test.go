package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
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
