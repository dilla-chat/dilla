package ds_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
)

// `dillad restore` runs without a delivery service: it performs invariant 11's
// SQL itself and leaves ds.RestorePendingKey holding the generation it set.
// The next start finishes the restore through OnRestore, exactly once, without
// moving the generation a second time, and re-arms every heal deadline from
// the start: a window that opened while the instance was down would otherwise
// close groups nobody could have healed.
func TestFinishRestoreCompletesAPendingRestoreOnce(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()

	done, err := h.ds.FinishRestore(ctx)
	if err != nil || done {
		t.Fatalf("FinishRestore with nothing pending = %v, %v; want false, nil", done, err)
	}
	if h.epochUnknown(t, g.id) {
		t.Fatal("FinishRestore with nothing pending marked a group epoch-unknown")
	}

	gen := h.generation(t) + 1
	if err := h.repo.SetGeneration(ctx, gen); err != nil {
		t.Fatalf("SetGeneration: %v", err)
	}
	if err := h.repo.PutSetting(ctx, ds.RestorePendingKey, []byte(strconv.FormatUint(gen, 10)), h.clk.Now().Unix()); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	h.clk.Advance(3 * time.Hour)

	done, err = h.ds.FinishRestore(ctx)
	if err != nil || !done {
		t.Fatalf("FinishRestore with a restore pending = %v, %v; want true, nil", done, err)
	}
	if got := h.generation(t); got != gen {
		t.Fatalf("generation = %d, want %d (not bumped a second time)", got, gen)
	}
	row, err := h.repo.GetGroup(ctx, g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	want := h.clk.Now().Add(ds.DefaultPolicy().HealWindow).Unix()
	if !row.EpochUnknown || row.HealDeadline == nil || *row.HealDeadline != want {
		t.Fatalf("group after FinishRestore: epoch_unknown %v, deadline %v; want true, %d",
			row.EpochUnknown, row.HealDeadline, want)
	}
	mark, err := h.repo.GetSetting(ctx, ds.RestorePendingKey)
	if err != nil || len(mark) != 0 {
		t.Fatalf("%s after FinishRestore = %q, %v; want cleared", ds.RestorePendingKey, mark, err)
	}
	if done, err := h.ds.FinishRestore(ctx); err != nil || done {
		t.Fatalf("a second FinishRestore = %v, %v; want false, nil", done, err)
	}
}
