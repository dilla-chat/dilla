package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jonasthim/dilla/internal/bot/music"
)

func TestSDKLoggerFiltersLevelsAndRedactsEveryDerivedLogger(t *testing.T) {
	var logs bytes.Buffer
	ctx := music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("device_id", "device-1"))
	logger := newSDKLogger(ctx)
	logger.Debugw("private-debug", "url", "https://secret.invalid")
	logger.Infow("private-info", "token", "secret-token")
	if logs.Len() != 0 {
		t.Fatalf("SDK info/debug escaped filter: %s", logs.String())
	}
	derived := logger.WithValues("url", "https://secret.invalid").WithName("private-name").WithComponent("private-component").WithCallDepth(3).WithItemSampler().WithoutSampler()
	derived.Warnw("secret-token", errors.New("private error https://secret.invalid"), "sdp", "private-sdp")
	derived.WithUnlikelyValues("secret", "private-value").Errorw("private-error", errors.New("private cause"), "status", 403)
	deferred, _ := derived.WithDeferredValues()
	deferred.Warnw("private-deferred", nil)
	rows := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(rows) != 3 {
		t.Fatalf("warn/error results = %d: %s", len(rows), logs.String())
	}
	for i, line := range rows {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		wantLevel := []string{"WARN", "ERROR", "WARN"}[i]
		if row["level"] != wantLevel {
			t.Fatalf("SDK level = %v, want %s", row["level"], wantLevel)
		}
		if row["device_id"] != "device-1" {
			t.Fatalf("correlation lost: %s", line)
		}
	}
	for _, secret := range []string{"private", "secret-token", "secret.invalid"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("SDK leaked %s: %s", secret, logs.String())
		}
	}
}

func TestPoolSDKHTTPUpgradeFailureRetainsStatusAndRedactsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "private-response-token")
	}))
	defer server.Close()
	var logs bytes.Buffer
	ctx := music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
	p, _ := NewPool(time.Minute)
	defer p.Close()
	call, group := poolCall(46, 47)
	call.LiveKitURL = "ws" + strings.TrimPrefix(server.URL, "http")
	call.Token = "private-access-token"
	_, err := p.startSource(ctx, call, group, newPoolEpochs(), newPoolSource())
	if !errors.Is(err, ErrRoomConnection) || !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("HTTP SDK cause lost: %v", err)
	}
	if got := music.FailureClass(err); got != "http_403" {
		t.Fatalf("HTTP class = %s, want http_403", got)
	}
	for _, secret := range []string{"private-response-token", "private-access-token", server.URL, call.LiveKitURL} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("SDK leaked %s: %s", secret, logs.String())
		}
	}
}

func TestPoolRoomEndLogsOnceWithReasonAndCorrelation(t *testing.T) {
	for _, reason := range []string{"disconnect", "idle"} {
		t.Run(reason, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)).With("device_id", "device-2"))
			p, _ := NewPool(time.Minute)
			backend := &poolBackendStub{}
			p.backend = backend
			defer p.Close()
			call, group := poolCall(48, 49)
			s, err := p.startSource(ctx, call, group, newPoolEpochs(), newPoolSource())
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			if reason == "idle" {
				if err := p.LeaveIdle(); err != nil {
					t.Fatal(err)
				}
			} else {
				backend.disconnectRoom(0)
				backend.disconnectRoom(0)
			}
			count := 0
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var row map[string]any
				_ = json.Unmarshal([]byte(line), &row)
				if row["msg"] == "bot music room ended" {
					count++
					if row["reason"] != reason || row["device_id"] != "device-2" {
						t.Fatalf("room reason/correlation: %s", line)
					}
				}
			}
			if count != 1 {
				t.Fatalf("room end count = %d: %s", count, logs.String())
			}
		})
	}
}

func TestSDKLoggerDetachesAttemptContext(t *testing.T) {
	type attemptKey struct{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	_, group := poolCall(91, 92)
	original := music.WithPlaybackGroup(music.WithPlaybackLogger(context.WithValue(context.Background(), attemptKey{}, "private-attempt"), logger), group)
	ctx, cancel := context.WithCancel(original)
	sdk := newSDKLogger(ctx)
	cancel()
	if sdk.ctx.Err() != nil || sdk.ctx.Done() != nil {
		t.Fatal("SDK logger retained cancellation")
	}
	if sdk.ctx.Value(attemptKey{}) != nil {
		t.Fatal("SDK logger retained attempt values")
	}
	if music.PlaybackLogger(sdk.ctx) != logger || music.PlaybackGroup(sdk.ctx) != group {
		t.Fatal("room logging correlation lost")
	}
	sdk.Warnw("ignored", nil)
	if logs.Len() == 0 {
		t.Fatal("detached logger did not log")
	}
}
