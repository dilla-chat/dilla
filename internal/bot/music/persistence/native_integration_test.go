//go:build linux

package persistence

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCompiledHelperQueueQuarantineUpgrade(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN to run compiled-helper queue upgrade integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stateDir := filepath.Join(t.TempDir(), "native")
	binding := native.Binding{Instance: testID(1), Community: testID(2), Installation: testID(3)}
	key := bytes.Repeat([]byte{0x37}, 32)
	open := func() *native.Client {
		c, err := native.Start(ctx, helper.Options{Executable: binary, StateDir: stateDir}, binding, append([]byte(nil), key...))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	defer func() { _ = c.Close() }()
	if _, err := c.Bootstrap(ctx, [32]byte{0x77}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompleteEnrollment(ctx, testID(4), "queue-upgrade-integration", uint64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, c, binding.Community)
	channel := testID(5)
	original := unsupportedStateValue(t, legacyQueueState())
	if _, err := c.StateTransaction(ctx, id.New(), []native.StateChange{{Area: stateArea, Key: s.key(channel), ExpectedRevision: 0, Value: original}}); err != nil {
		t.Fatal(err)
	}

	// Prove a CAS conflict on the second row rolls back the first archive write
	// using the real Rust helper and its encrypted SQLite transaction.
	if _, err := c.StateTransaction(ctx, id.New(), []native.StateChange{
		{Area: stateArea, Key: s.quarantineKey(channel, 1), ExpectedRevision: 0, Value: original},
		{Area: stateArea, Key: s.key(channel), ExpectedRevision: 0, Value: original},
	}); err == nil {
		t.Fatal("stale active-row CAS accepted")
	}
	archive, err := c.StateGet(ctx, stateArea, s.quarantineKey(channel, 1))
	if err != nil || archive.Revision != 0 {
		t.Fatalf("failed CAS partially archived row: %+v %v", archive, err)
	}
	active, err := c.StateGet(ctx, stateArea, s.key(channel))
	if err != nil || active.Revision != 1 || !bytes.Equal(active.Value, original) {
		t.Fatalf("failed CAS changed source: %+v %v", active, err)
	}

	// Two simultaneous readers must recover exactly one active revision and
	// one immutable original archive, including the helper's receipt replay.
	type result struct {
		snapshot Snapshot
		err      error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { snapshot, err := s.Load(ctx, channel); results <- result{snapshot, err} }()
	}
	for range 2 {
		r := <-results
		if r.err != nil || r.snapshot.Revision != 2 || !r.snapshot.State.Paused || len(r.snapshot.State.Queue) != 1 || r.snapshot.State.Queue[0].Provider != music.ProviderYouTube {
			t.Fatalf("concurrent recovery=%+v %v", r.snapshot, r.err)
		}
	}
	archive, err = c.StateGet(ctx, stateArea, s.quarantineKey(channel, 1))
	if err != nil || archive.Revision != 1 || !bytes.Equal(archive.Value, original) {
		t.Fatal("original archive differs after real helper recovery")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	s = newStore(t, c, binding.Community)
	reopened, err := s.Load(ctx, channel)
	if err != nil || reopened.Revision != 2 {
		t.Fatalf("reopened recovery=%+v %v", reopened, err)
	}
	archive, err = c.StateGet(ctx, stateArea, s.quarantineKey(channel, 1))
	if err != nil || archive.Revision != 1 || !bytes.Equal(archive.Value, original) {
		t.Fatal("encrypted original archive did not survive reopen")
	}
	cleared := applied(t, reopened.State, music.Operation{Kind: music.OpClear})
	if _, err := s.Save(ctx, channel, reopened.Revision, cleared); err != nil {
		t.Fatal(err)
	}
	archive, err = c.StateGet(ctx, stateArea, s.quarantineKey(channel, 1))
	if err != nil || !bytes.Equal(archive.Value, original) {
		t.Fatal("normal clear changed original archive")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("integration exceeded its deadline")
	}
}
