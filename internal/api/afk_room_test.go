package api_test

import (
	"log/slog"
	"maps"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// I-1 and m-2 of the SRV-POS review: the lowering of channels at 2^30 or above cannot fail, and it runs
// in the branch that creates the built-in channel too.

// putAFKTestChannel writes an ordinary voice channel of cid at pos under the store.
func putAFKTestChannel(t *testing.T, repo store.Repository, cid id.ID, name string, pos uint64) id.ID {
	t.Helper()
	ch := id.New()
	if err := repo.CreateChannel(t.Context(), store.ChannelRow{ID: ch, CommunityID: &cid, Kind: api.ChannelVoice,
		Name: name, Position: pos, SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: policyT0}); err != nil {
		t.Fatal(err)
	}
	return ch
}

// afkTestPositions reads the position of each channel in ids.
func afkTestPositions(t *testing.T, repo store.Repository, ids map[string]id.ID) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for name, ch := range ids {
		row, err := repo.GetChannel(t.Context(), ch)
		if err != nil {
			t.Fatalf("GetChannel %s: %v", name, err)
		}
		out[name] = row.Position
	}
	return out
}

// afkTestAuditCount counts the audit rows that target ch.
func afkTestAuditCount(t *testing.T, repo store.Repository, ch id.ID) int {
	t.Helper()
	rows, err := repo.ListAudit(t.Context(), 0, 10_000)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	n := 0
	for _, a := range rows {
		if a.Target == ch.String() {
			n++
		}
	}
	return n
}

func afkTestPointer(t *testing.T, repo store.Repository, cid id.ID) id.ID {
	t.Helper()
	cfg, err := repo.GetCommunityAFK(t.Context(), cid)
	if err != nil || cfg.ChannelID == nil {
		t.Fatalf("pointer of %s = %+v, %v", cid, cfg, err)
	}
	return *cfg.ChannelID
}

// I-1: with an ordinary channel at 2^30-1 there is no room after the current max, so every ordinary
// channel of the community is renumbered densely 0..n-1 in its current order, and the rest of the
// ensure (the settings reset, the overwrite strip) still commits. A second community with a channel at
// 2^30-1 and no stray is the control: nothing of it moves.
func TestEnsureAFKNoRoomRenumbersDensely(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			clk := newEnv(t).Clk
			log := slog.New(slog.DiscardHandler)
			builtin := uint64(api.AFKBuiltinPosition)
			cid, owner := seedAFKStoreCommunity(t, repo)
			control, _ := seedAFKStoreCommunity(t, repo)
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 2 {
				t.Fatalf("seeding run = %d, %v; want 2", n, err)
			}
			afk := afkTestPointer(t, repo, cid)
			ids := map[string]id.ID{
				"mid":   putAFKTestChannel(t, repo, cid, "mid", 5),
				"high":  putAFKTestChannel(t, repo, cid, "high", builtin-1),
				"stray": putAFKTestChannel(t, repo, cid, "stray", builtin+1),
			}
			controlIDs := map[string]id.ID{"high": putAFKTestChannel(t, repo, control, "high", builtin-1)}
			row, err := repo.GetChannel(ctx, afk)
			if err != nil {
				t.Fatal(err)
			}
			row.Mode, row.Visibility = api.ModeReadable, api.VisInvite
			if err := repo.UpdateChannel(ctx, row); err != nil {
				t.Fatal(err)
			}
			if err := repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: afk, TargetKind: 1, TargetID: owner,
				Allow: uint64(api.PermPinMessages)}); err != nil {
				t.Fatal(err)
			}
			controlAudit := afkTestAuditCount(t, repo, controlIDs["high"])
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 0 {
				t.Fatalf("no-room run = %d, %v; want 0 created", n, err)
			}
			want := map[string]uint64{"mid": 0, "high": 1, "stray": 2}
			if got := afkTestPositions(t, repo, ids); !maps.Equal(got, want) {
				t.Fatalf("positions after the ensure = %v, want %v (dense, in order)", got, want)
			}
			got, err := repo.GetChannel(ctx, afk)
			if err != nil || got.Position != builtin || got.Mode != api.ModeE2EE || got.Visibility != api.VisPrivate {
				t.Fatalf("AFK after the ensure = %+v, %v; want the built-in settings at 2^30", got, err)
			}
			if ow, err := repo.ListOverwrites(ctx, afk); err != nil || len(ow) != 0 {
				t.Fatalf("AFK overwrites after the ensure = %v, %v; want none", ow, err)
			}
			if got := afkTestPositions(t, repo, controlIDs); got["high"] != builtin-1 {
				t.Fatalf("control community = %v; want its channel left at 2^30-1", got)
			}
			if n := afkTestAuditCount(t, repo, controlIDs["high"]); n != controlAudit {
				t.Fatalf("control community audit rows %d -> %d; want none added", controlAudit, n)
			}
			audit := map[string]int{}
			for name, ch := range ids {
				audit[name] = afkTestAuditCount(t, repo, ch)
			}
			// Idempotent, also inside the transaction.
			if err := repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: afk, TargetKind: 1, TargetID: owner,
				Allow: uint64(api.PermPinMessages)}); err != nil {
				t.Fatal(err)
			}
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 0 {
				t.Fatalf("second run = %d, %v; want 0", n, err)
			}
			if got := afkTestPositions(t, repo, ids); !maps.Equal(got, want) {
				t.Fatalf("positions after the second run = %v, want %v", got, want)
			}
			for name, ch := range ids {
				if n := afkTestAuditCount(t, repo, ch); n != audit[name] {
					t.Fatalf("second run audited %s again: %d -> %d", name, audit[name], n)
				}
			}
		})
	}
}

// m-2: the branch that creates the built-in channel lowers strays too. A web3.8 community with no AFK
// channel and a channel at 2^30+1 gets its built-in channel at 2^30 and the stray below it; a web3.8 pick
// at 2^30+2 becomes an ordinary channel below 2^30 with its own settings kept. A second run does nothing.
func TestEnsureAFKCreateBranchLowersStrays(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			clk := newEnv(t).Clk
			log := slog.New(slog.DiscardHandler)
			builtin := uint64(api.AFKBuiltinPosition)
			none, _ := seedAFKStoreCommunity(t, repo)
			ids := map[string]id.ID{
				"lounge": putAFKTestChannel(t, repo, none, "lounge", 3),
				"stray":  putAFKTestChannel(t, repo, none, "stray", builtin+1),
			}
			legacy, _ := seedAFKStoreCommunity(t, repo)
			pick := id.New()
			if err := repo.CreateChannel(ctx, store.ChannelRow{ID: pick, CommunityID: &legacy, Kind: api.ChannelVoice,
				Mode: api.ModeReadable, Visibility: api.VisInvite, Name: "pick", Topic: "kept", Position: builtin + 2,
				SlowmodeSeconds: 5, SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: policyT0}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SetCommunityAFK(ctx, legacy, store.CommunityAFK{ChannelID: &pick, TimeoutSeconds: 900}); err != nil {
				t.Fatal(err)
			}
			ids["pick"] = pick
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 2 {
				t.Fatalf("EnsureAFKChannels = %d, %v; want 2", n, err)
			}
			want := map[string]uint64{"lounge": 3, "stray": 4, "pick": 0}
			if got := afkTestPositions(t, repo, ids); !maps.Equal(got, want) {
				t.Fatalf("positions = %v, want %v", got, want)
			}
			for _, cid := range []id.ID{none, legacy} {
				afk := afkTestPointer(t, repo, cid)
				row, err := repo.GetChannel(ctx, afk)
				if err != nil || afk == pick || row.Position != builtin || string(row.SettingsJSON) != builtinMarker {
					t.Fatalf("AFK of %s = %+v, %v; want a new built-in channel at 2^30", cid, row, err)
				}
			}
			old, err := repo.GetChannel(ctx, pick)
			if err != nil || old.Mode != api.ModeReadable || old.Visibility != api.VisInvite || old.Topic != "kept" ||
				old.SlowmodeSeconds != 5 {
				t.Fatalf("the old pick = %+v, %v; want its settings kept (it is an ordinary channel now)", old, err)
			}
			if cfg, err := repo.GetCommunityAFK(ctx, legacy); err != nil || cfg.TimeoutSeconds != 900 {
				t.Fatalf("legacy timeout = %+v, %v; want 900 kept", cfg, err)
			}
			audit := map[string]int{}
			for name, ch := range ids {
				audit[name] = afkTestAuditCount(t, repo, ch)
			}
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 0 {
				t.Fatalf("second run = %d, %v; want 0", n, err)
			}
			if got := afkTestPositions(t, repo, ids); !maps.Equal(got, want) {
				t.Fatalf("positions after the second run = %v, want %v", got, want)
			}
			for name, ch := range ids {
				if n := afkTestAuditCount(t, repo, ch); n != audit[name] {
					t.Fatalf("second run audited %s again: %d -> %d", name, audit[name], n)
				}
			}
		})
	}
}
