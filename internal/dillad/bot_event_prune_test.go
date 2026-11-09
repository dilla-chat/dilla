package dillad

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/auth"
	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// Bot v2 Task 23 fix round 1 (L5): nothing reads bot_structural_events any more (the Bot's v1 polling went; op 65 does
// not read them), so the maintenance tick prunes them past BotStructuralEventRetention. A warning's revision is the
// stream's high water (bot_event_streams), which pruning keeps, so the next sequence still follows the last.
func TestTheMaintenanceTickPrunesOldBotStructuralEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prune.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	clk := clock.NewFake(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	cfg := config.Default()
	s := &Server{
		o:        Options{Repo: repo, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		throttle: auth.NewThrottle(cfg.Limits.Rate, cfg.Auth.Lockout, clk),
		limiter:  server.NewRateLimiter(cfg.Limits.Rate, clk),
	}
	owner := store.UserRow{ID: id.New(), Username: "ada", Display: "Ada", UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
		SigUMKSSK: make([]byte, 64), Created: 1}
	if err := repo.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	community := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: owner.ID, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: 1}); err != nil {
		t.Fatal(err)
	}
	appendEvent := func() uint64 {
		var got botprotocol.Event
		if err := repo.Tx(ctx, func(tx store.Repository) error {
			var err error
			got, err = tx.AppendBotStructuralEvent(ctx, botprotocol.Event{ID: id.New(), CommunityID: community,
				Timestamp: uint64(clk.Now().Unix()), Kind: botprotocol.EventMemberJoin, Data: []any{id.New(), id.New()}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got.Sequence
	}
	held := func() int {
		page, err := repo.ListBotStructuralEvents(ctx, community, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Events)
	}
	appendEvent()
	appendEvent()
	s.sweep(ctx)
	if n := held(); n != 2 {
		t.Fatalf("fresh events after the sweep: %d, want 2", n)
	}
	clk.Advance(BotStructuralEventRetention + time.Hour)
	s.sweep(ctx)
	if n := held(); n != 0 {
		t.Fatalf("events past the retention after the sweep: %d, want 0", n)
	}
	if next := appendEvent(); next != 3 {
		t.Fatalf("the sequence after a prune = %d, want 3 (the high water is kept)", next)
	}
}
