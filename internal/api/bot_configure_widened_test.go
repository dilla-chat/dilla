package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// botPollsRemindersMask is capability bits 8 (Polls) and 9 (Reminders).
const botPollsRemindersMask = 1<<8 | 1<<9

// Task 10c fix round 1 (I1): a configure request without ?v=2 comes from a web client that knows only
// the original capability byte. It must not clear Polls and Reminders it cannot see; a widened request
// sets them explicitly; a new installation from a legacy request starts with both on.
func TestConfigureBotLegacyRequestKeepsPollsAndReminders(t *testing.T) {
	f := newBotSettingsFixture(t, newEnv(t).Repo)
	ctx := context.Background()
	sess, err := f.deps.Sessions.Resolve(ctx, f.ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	stepUp := func() string {
		step, _ := f.deps.StepUps.Issue(sess.UserID, sess.TokenHash)
		return step
	}
	current := func() store.BotInstallationRow {
		t.Helper()
		row, err := f.deps.Repo.GetActiveBotInstallationByCommunity(ctx, f.cid)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	base := "/v1/communities/" + f.cid.String() + "/bot"

	// Legacy: switch Music off and Welcome on; Polls and Reminders stay on.
	revision := current().Revision
	if rec := putCBORAuth(f.h, base, mustCBOR(t, []any{id.New(), revision, uint64(1 << 1), stepUp()}), f.ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("legacy configure = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	if got := current().Capabilities; got != 1<<1|botPollsRemindersMask {
		t.Fatalf("after legacy configure caps = %#x, want %#x", got, 1<<1|botPollsRemindersMask)
	}

	// Widened: turn Reminders off explicitly.
	revision = current().Revision
	if rec := putCBORAuth(f.h, base+"?v=2", mustCBOR(t, []any{id.New(), revision, uint64(1<<1 | 1<<8), stepUp()}), f.ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("widened configure = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	if got := current().Capabilities; got != 1<<1|1<<8 {
		t.Fatalf("after widened configure caps = %#x, want %#x", got, 1<<1|1<<8)
	}

	// Legacy again: Reminders stays off, Polls stays on.
	revision = current().Revision
	if rec := putCBORAuth(f.h, base, mustCBOR(t, []any{id.New(), revision, uint64(1), stepUp()}), f.ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("second legacy configure = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	if got := current().Capabilities; got != 1|1<<8 {
		t.Fatalf("after second legacy configure caps = %#x, want %#x", got, 1|1<<8)
	}
}

func TestConfigureBotLegacyCreateStartsWithPollsAndReminders(t *testing.T) {
	f := newBotSettingsFixture(t, newEnv(t).Repo)
	ctx := context.Background()
	sess, err := f.deps.Sessions.Resolve(ctx, f.ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want uint16
	}{
		{"/bot", 1 | botPollsRemindersMask}, // legacy: both start on
		{"/bot?v=2", 1},                     // widened: exactly what was sent
	} {
		cid := seedBotCommunity(t, f.deps, sess.UserID) // a community with no installation yet
		step, _ := f.deps.StepUps.Issue(sess.UserID, sess.TokenHash)
		rec := putCBORAuth(f.h, "/v1/communities/"+cid.String()+tc.path, mustCBOR(t, []any{id.New(), nil, uint64(1), step}), f.ownerToken)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s create = %d (%x)", tc.path, rec.Code, rec.Body.Bytes())
		}
		created, err := f.deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
		if err != nil || created.Capabilities != tc.want {
			t.Fatalf("%s create caps = %#x (%v), want %#x", tc.path, created.Capabilities, err, tc.want)
		}
	}
}
