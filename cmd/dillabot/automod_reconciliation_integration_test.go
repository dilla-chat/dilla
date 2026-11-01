//go:build linux

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestPreparedModerationDependenciesReconcileAfterPolicyStopsEnforcement(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("native integration requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("requires the compiled native helper, as the native package integration suite")
	}
	for _, disabled := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(map[bool]string{true: "disabled", false: "log-only"}[disabled]+map[bool]string{true: "-absent", false: "-accepted"}[missing], func(t *testing.T) {
				binding := native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}
				n, err := native.Start(t.Context(), helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "native")}, binding, bytes.Repeat([]byte{0x73}, 32))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = n.Close() })
				if _, err = n.Bootstrap(t.Context(), [32]byte{0x77}); err != nil {
					t.Fatal(err)
				}
				normal, lookups := 0, 0
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					w.Header().Set("Content-Type", "application/cbor")
					var raw []byte
					if req.URL.Path == "/v1/instance" {
						raw, _ = cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}, binding.Instance, uint64(1), "test", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)})
						_, _ = w.Write(raw)
						return
					}
					if req.URL.Query().Get("receipt_only") != "1" {
						normal++
						w.WriteHeader(503)
						raw, _ = cborx.Marshal([]any{"E_UNAVAILABLE", "uncertain reply", nil})
					} else {
						lookups++
						if missing {
							w.WriteHeader(404)
							raw, _ = cborx.Marshal([]any{"E_NOT_FOUND", "no receipt", nil})
						} else {
							raw, _ = cborx.Marshal([]any{id.New(), uint64(1), nil})
						}
					}
					_, _ = w.Write(raw)
				}))
				defer host.Close()
				c, err := client.New(client.Config{BaseURL: host.URL, AllowLoopbackHTTP: true, InstanceID: binding.Instance})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.Discover(t.Context()); err != nil {
					t.Fatal(err)
				}
				session, err := c.SessionForDevice("test-session", id.New())
				if err != nil {
					t.Fatal(err)
				}
				cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
				_, sink, err := prepareAutoModerationDependencies(n, session, binding.Community, cfg)
				if err != nil {
					t.Fatal(err)
				}
				authority := routedModerationAuthorityFunc(func(_ context.Context, _ native.GroupEvent) (bool, bool, error) { return false, false, nil })
				r := service.NewAutoModerationRuntime(cfg, authority, sink, time.Now)
				if err = r.AttachDurableState(t.Context(), n); err != nil {
					t.Fatal(err)
				}
				body := "test"
				now := time.Now()
				e := native.GroupEvent{DeliveryID: 1, Community: binding.Community, Group: id.New(), Channel: id.New(), SourceSequence: 1, ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Body: &body, Message: id.New()}
				mentions := []string{"1", "2", "3", "4", "5", "6"}
				if _, err = r.Handle(t.Context(), e, false, mentions); err == nil || normal != 1 {
					t.Fatalf("initial uncertain acceptance: calls=%d err=%v", normal, err)
				}
				next := moderation.Config{Enabled: !disabled, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionLog}}
				_, replacement, err := prepareAutoModerationDependencies(n, session, binding.Community, next)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.ActivateConfig(t.Context(), service.AutoModerationActivation{Config: next, Revision: 1, Sink: replacement, Authority: authority}, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				d, err := r.Handle(t.Context(), e, true, mentions)
				if err != nil || d.EnforcementRefused != missing || normal != 1 || lookups != 1 {
					t.Fatalf("reconciliation=%+v err=%v effects=%d lookups=%d", d, err, normal, lookups)
				}
			})
		}
	}
}
