package moderation

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCheckpointRestoresRollingAndDedup(t *testing.T) {
	cfg := Config{Enabled: true, Actions: map[Rule]Action{RuleRate: ActionWarn}}
	e := New(cfg)
	now := time.Unix(1800000000, 0)
	for i := uint64(1); i <= 5; i++ {
		e.Evaluate(Input{CommunityID: "c", UserID: "u", SourceID: "s", Revision: i, ReceivedAt: now, Body: "different"}, now)
	}
	shards, err := e.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored := New(cfg)
	if err = restored.RestoreCheckpoint(shards); err != nil {
		t.Fatal(err)
	}
	dup := restored.Evaluate(Input{CommunityID: "c", UserID: "u", SourceID: "s", Revision: 5, ReceivedAt: now}, now)
	if !dup.DuplicateDelivery {
		t.Fatal("dedup lost")
	}
	d := restored.Evaluate(Input{CommunityID: "c", UserID: "u", SourceID: "s", Revision: 6, ReceivedAt: now}, now)
	if d.Action != ActionWarn {
		t.Fatalf("rolling counter lost: %+v", d)
	}
}

func TestCheckpointRejectsOversizedShardWithoutDiscardingObservations(t *testing.T) {
	e := New(Config{Enabled: true})
	now := time.Unix(1800000000, 0)
	for i := uint64(1); i <= 1000; i++ {
		e.Evaluate(Input{CommunityID: strings.Repeat("c", 128), UserID: strings.Repeat("u", 128), SourceID: fmt.Sprintf("%0128d", i), Revision: i, ReceivedAt: now, Body: "x"}, now)
	}
	if _, err := e.Checkpoint(); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("oversized checkpoint accepted: %v", err)
	}
	d := e.Evaluate(Input{CommunityID: strings.Repeat("c", 128), UserID: strings.Repeat("u", 128), SourceID: fmt.Sprintf("%0128d", 1000), Revision: 1000, ReceivedAt: now, Body: "x"}, now)
	if !d.DuplicateDelivery {
		t.Fatal("failed snapshot discarded evaluator")
	}
}

func TestCheckpointDoesNotAccumulateExpiredActorHistory(t *testing.T) {
	e := New(Config{Enabled: true})
	now := time.Unix(1800000000, 0)
	for i := uint64(1); i <= 3000; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		e.Evaluate(Input{CommunityID: "c", UserID: "u", SourceID: fmt.Sprintf("%0128d", i), Revision: i, ReceivedAt: at, Body: "x"}, at)
	}
	shards, err := e.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range shards {
		if len(raw) > 16<<10 {
			t.Fatalf("expired history retained: %d bytes", len(raw))
		}
	}
}
