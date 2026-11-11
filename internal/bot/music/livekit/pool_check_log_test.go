package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.2 Task 7: every failed call-group check of a playing track is logged with its attempt, error_class and
// stage, so the next journal names what ends a track 6 s after it was published.

func checkLogRows(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "bot music call group check failed" {
			rows = append(rows, row)
		}
	}
	return rows
}

func checkSession(t *testing.T) (*Session, *movingEpochs, id.ID) {
	t.Helper()
	group := testID(9)
	epochs := &movingEpochs{epoch: epochAt(4, 4, 1, 2), state: &counterStateStub{}}
	sender, err := senderForReservedEpoch(context.Background(), epochs, group, epochs.epoch)
	if err != nil {
		t.Fatal(err)
	}
	return &Session{sender: sender}, epochs, group
}

func TestCatchUpCheckFailureLogged(t *testing.T) {
	s, epochs, group := checkSession(t)
	var logs bytes.Buffer
	ctx := music.WithPlaybackItem(music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil))), "queue-item", false)
	guards := CallGuards{CatchUp: func(context.Context, id.ID) error { return fmt.Errorf("source sync: %w", context.Canceled) }}
	failures := 0
	var err error
	for check := 1; check <= maxCatchUpFailures; check++ {
		_, err = s.followEpoch(ctx, guards, group, epochs, epochs.epoch, &failures)
		if (err != nil) != (check == maxCatchUpFailures) {
			t.Fatalf("check %d: err=%v, want the track to stop only at the third failed check", check, err)
		}
	}
	rows := checkLogRows(t, &logs)
	if len(rows) != maxCatchUpFailures {
		t.Fatalf("check lines = %v, want one per failed check", rows)
	}
	for i, row := range rows {
		if row["attempt"] != float64(i+1) || row["error_class"] != "canceled" || row["stage"] != "catch_up" || row["item_id"] != "queue-item" {
			t.Fatalf("line %d = %v", i, row)
		}
	}

	// An epoch the Bot's own leaf left is a media_epoch failure with its reason.
	logs.Reset()
	epochs.set(native.CallMediaEpoch{}, fmt.Errorf("native helper returned an invalid result: %w", native.ErrCallEpochOwnLeafMissing))
	failures = 0
	if _, err := s.followEpoch(ctx, CallGuards{}, group, epochs, epochAt(4, 4, 1, 2), &failures); err == nil {
		t.Fatal("publishing went on without the Bot's leaf")
	}
	rows = checkLogRows(t, &logs)
	if len(rows) != 1 || rows[0]["stage"] != "media_epoch" || rows[0]["epoch_reason"] != "leaf_missing" || rows[0]["attempt"] != float64(1) {
		t.Fatalf("media_epoch check lines = %v", rows)
	}
}

func TestCatchUpClosedHelperEndsAtOnce(t *testing.T) {
	s, epochs, group := checkSession(t)
	var logs bytes.Buffer
	ctx := music.WithPlaybackLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
	// The catch-up resolves the installation's current helper on each check; a closed one is terminal.
	guards := CallGuards{CatchUp: func(context.Context, id.ID) error {
		return fmt.Errorf("%w: %w", ErrCallCatchUpTerminal, helper.ErrClosed)
	}}
	failures := 0
	_, err := s.followEpoch(ctx, guards, group, epochs, epochs.epoch, &failures)
	if !errors.Is(err, ErrCallBehind) || !errors.Is(err, helper.ErrClosed) || helper.Class(err) != "closed" {
		t.Fatalf("first check = %v, want the track ended at once with error_class=closed", err)
	}
	rows := checkLogRows(t, &logs)
	if len(rows) != 1 || rows[0]["error_class"] != "closed" || rows[0]["attempt"] != float64(1) {
		t.Fatalf("check lines = %v", rows)
	}
}

func TestStartEpochFailureNamesItsReason(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{fmt.Errorf("wrapped: %w", native.ErrCallEpochOwnLeafMissing), "leaf_missing"},
		{ErrLeafMissing, "leaf_missing"},
		{ErrEpochChanged, "epoch_stale"},
		{&helper.StatusError{Status: 7}, "group_unknown"},
	} {
		err := epochFailureOf(tc.err, true)
		if got := music.EpochReason(err); got != tc.reason {
			t.Fatalf("%v: reason %q, want %q", tc.err, got, tc.reason)
		}
		if !errors.Is(err, tc.err) {
			t.Fatalf("%v: the cause is lost", tc.err)
		}
	}
	if !errors.Is(epochFailureOf(native.ErrCallEpochOwnLeafMissing, true), ErrLeafMissing) {
		t.Fatal("the helper's own-leaf-missing epoch does not match ErrLeafMissing")
	}
}
