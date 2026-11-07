package ds_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/ds"
)

// Bot v2 Task 12: OnEpoch hears a group's new epoch, with its member devices, only after the commit's
// transaction committed. The Bot's event feed builds its channel_group events from it, so a commit
// that rolled back must not reach the Bot.
func TestOnEpochHearsOnlyCommittedEpochs(t *testing.T) {
	type heard struct {
		mu  sync.Mutex
		got []ds.EpochChange
	}
	setup := func(t *testing.T) (*dsHarness, ds.RegisterResult, auth.Session, *heard) {
		t.Helper()
		h := newDSHarness(t)
		reg, session := h.mustRegister(t)
		on := &heard{}
		h.restartWithEpochHook(func(c ds.EpochChange) {
			on.mu.Lock()
			defer on.mu.Unlock()
			on.got = append(on.got, c)
		})
		return h, reg, session, on
	}
	selfUpdate := func(t *testing.T) ds.CommitRequest {
		return ds.CommitRequest{Epoch: 6, Commit: fixtureFile(t, "commits/09.mls"), GroupInfo: fixtureFile(t, "commits/09.group_info.mls")}
	}

	t.Run("committed", func(t *testing.T) {
		h, reg, session, on := setup(t)
		result, err := h.ds.Commit(context.Background(), session, reg.GroupID, selfUpdate(t))
		if err != nil {
			t.Fatalf("the self-update was refused: %v", err)
		}
		on.mu.Lock()
		defer on.mu.Unlock()
		if len(on.got) != 1 {
			t.Fatalf("OnEpoch heard %d epochs, want 1", len(on.got))
		}
		c := on.got[0]
		if c.GroupID != reg.GroupID || c.Epoch != result.Epoch || len(c.Devices) == 0 {
			t.Fatalf("OnEpoch heard %+v, want group %s at epoch %d with its devices", c, reg.GroupID, result.Epoch)
		}
	})
	t.Run("rolled back", func(t *testing.T) {
		h, reg, session, on := setup(t)
		h.failNextTx("TxCommit")
		if _, err := h.ds.Commit(context.Background(), session, reg.GroupID, selfUpdate(t)); !errors.Is(err, errInjected) {
			t.Fatalf("got %v, want the injected failure", err)
		}
		on.mu.Lock()
		defer on.mu.Unlock()
		if len(on.got) != 0 {
			t.Fatalf("a commit whose transaction rolled back reached OnEpoch: %+v", on.got)
		}
	})
}
