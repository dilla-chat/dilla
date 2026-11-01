//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

func TestCommandSkippedLoggerPreservesPerEventDeliveryAgeAndCause(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	installation := id.New()
	first := time.Unix(1800000000, 0).UTC()
	logSkippedCommandEvents(logger, installation, []service.SkippedGroupEvent{
		{DeliveryID: 41, FirstFailure: first, Cause: errors.New("reply stayed pending")},
		{DeliveryID: 42, Cause: errors.New("group permanently unavailable")},
	})
	decoder := json.NewDecoder(&output)
	for _, want := range []struct {
		delivery float64
		cause    string
		first    bool
	}{{41, "reply stayed pending", true}, {42, "group permanently unavailable", false}} {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["installation_id"] != installation.String() || event["delivery_id"] != want.delivery || event["cause"] != want.cause || event["level"] != "WARN" {
			t.Fatalf("skip diagnostic=%+v", event)
		}
		if want.first && event["first_failure"] != first.Format(time.RFC3339) {
			t.Fatalf("missing durable first-failure time: %+v", event)
		}
		if !want.first {
			if _, present := event["first_failure"]; present {
				t.Fatalf("permanent refusal acquired a fake failure age: %+v", event)
			}
		}
	}
	if decoder.More() {
		t.Fatal("extra skip diagnostics")
	}
}
