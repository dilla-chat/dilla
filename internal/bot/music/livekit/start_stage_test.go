package livekit

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/sframe"
)

// Removing stage results or replacing an underlying error must fail these tests.
func TestPoolStartStageFailureRetainsCauseAndLogsOnce(t *testing.T) {
	for _, stage := range []string{"media_epoch", "counter_reserve", "room_connect", "track_publish"} {
		t.Run(stage, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)).With("request_id", "play-1"))
			cause := errors.New("private token secret-key https://example.invalid/private")
			epochs := &stageEpochs{poolEpochs: newPoolEpochs()}
			backend := &stageBackend{poolBackendStub: &poolBackendStub{}}
			switch stage {
			case "media_epoch":
				epochs.err = cause
			case "counter_reserve":
				epochs.reserveErr = cause
			case "room_connect":
				backend.connectErr = cause
			case "track_publish":
				backend.publishErr = cause
			}
			p, err := NewPool(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			p.backend = backend
			defer p.Close()
			call, group := poolCall(40, 41)
			_, err = p.startSource(ctx, call, group, epochs, newPoolSource())
			if !errors.Is(err, cause) {
				t.Fatalf("cause lost: %v", err)
			}
			if stage == "room_connect" && !errors.Is(err, ErrRoomConnection) {
				t.Fatalf("room sentinel lost: %v", err)
			}
			var start *music.StartError
			if !errors.As(err, &start) || start.Stage != stage {
				t.Fatalf("stage error = %v", err)
			}
			failures, results := 0, 0
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var row map[string]any
				if json.Unmarshal([]byte(line), &row) != nil {
					t.Fatalf("invalid log %q", line)
				}
				if row["stage"] == stage {
					results++
				}
				if row["stage"] == stage && row["level"] == "WARN" {
					failures++
					if row["request_id"] != "play-1" {
						t.Fatalf("missing correlation: %v", row)
					}
				}
			}
			if failures != 1 || results != 1 {
				t.Fatalf("stage %s failure count = %d, logs: %s", stage, failures, logs.String())
			}
			if strings.Contains(logs.String(), "secret-key") || strings.Contains(logs.String(), "example.invalid") {
				t.Fatalf("sensitive logs: %s", logs.String())
			}
		})
	}
}

func TestPoolStartStageSuccessLogsOneResultPerBoundary(t *testing.T) {
	var logs bytes.Buffer
	ctx := music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
	p, _ := NewPool(time.Minute)
	p.backend = &poolBackendStub{}
	defer p.Close()
	call, group := poolCall(42, 43)
	s, err := p.startSource(ctx, call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, stage := range []string{"media_epoch", "counter_reserve", "room_connect", "track_publish"} {
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var row map[string]any
			_ = json.Unmarshal([]byte(line), &row)
			if row["stage"] == stage && row["level"] == "INFO" && row["class"] == "ok" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("stage %s success count = %d: %s", stage, count, logs.String())
		}
	}
}

func TestRoomFailurePreservesTypedCauseAndClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		class string
	}{
		{"dns", &net.DNSError{Err: "private hostname", Name: "secret.invalid"}, "dns"},
		{"tcp", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private address")}, "tcp"},
		{"tls", tls.RecordHeaderError{Msg: "private handshake"}, "tls"},
		{"timeout", lksdk.ErrConnectionTimeout, "timeout"},
		{"ice", errors.New("ICE connection failed private candidate"), "ice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := NewPool(time.Minute)
			p.backend = &stageBackend{poolBackendStub: &poolBackendStub{}, connectErr: tc.cause}
			defer p.Close()
			call, group := poolCall(44, 45)
			_, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
			if !errors.Is(err, tc.cause) || !errors.Is(err, ErrRoomConnection) {
				t.Fatalf("cause/sentinel lost: %v", err)
			}
			if got := music.FailureClass(err); got != tc.class {
				t.Fatalf("class = %s, want %s", got, tc.class)
			}
			if tc.name == "dns" {
				var dns *net.DNSError
				if !errors.As(err, &dns) {
					t.Fatal("typed DNS cause lost")
				}
			}
		})
	}
}

type stageEpochs struct {
	*poolEpochs
	reserveErr error
}

func (e *stageEpochs) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	if e.reserveErr != nil {
		return native.StateValue{}, e.reserveErr
	}
	return e.poolEpochs.StateGet(ctx, area, key)
}

type stageBackend struct {
	*poolBackendStub
	connectErr, publishErr error
}

func (b *stageBackend) connect(ctx context.Context, call client.Call, disconnected func()) (poolRoom, error) {
	if b.connectErr != nil {
		return nil, b.connectErr
	}
	return b.poolBackendStub.connect(ctx, call, disconnected)
}
func (b *stageBackend) publish(room poolRoom, source sourceLifecycle, sender *sframe.Sender) (func() error, error) {
	if b.publishErr != nil {
		return nil, b.publishErr
	}
	return b.poolBackendStub.publish(room, source, sender)
}
