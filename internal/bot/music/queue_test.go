package music

import (
	"errors"
	"fmt"
	"testing"
)

func track(id string, provider Provider) Track {
	canonicalID := "abcdefghijk"
	if provider == ProviderSpotify {
		canonicalID = "spotify:track:0123456789012345678901"
	}
	return Track{ID: id, Provider: provider, CanonicalID: canonicalID, Title: "Title " + id, RequesterID: "member-1", DurationMS: 180_000}
}

func apply(t *testing.T, state State, op Operation) State {
	t.Helper()
	next, err := Apply(state, state.Revision, op)
	if err != nil {
		t.Fatalf("Apply(%v): %v", op.Kind, err)
	}
	return next
}

func TestPlaylistImportIsAtomicAtQueueLimit(t *testing.T) {
	state := InitialState()
	tracks := make([]Track, QueueLimit)
	for i := range tracks {
		tracks[i] = track(fmt.Sprintf("t%03d", i), ProviderYouTube)
	}
	state = apply(t, state, Operation{Kind: OpImportPlaylist, Tracks: tracks})
	if len(state.Queue) != QueueLimit || state.Revision != 1 {
		t.Fatalf("imported queue length/revision = %d/%d, want %d/1", len(state.Queue), state.Revision, QueueLimit)
	}
	state = apply(t, state, Operation{Kind: OpResume})
	if state.Current == nil || len(state.Queue) != QueueLimit-1 {
		t.Fatalf("starting a full queue did not preserve its total size: current=%v queue=%d", state.Current, len(state.Queue))
	}

	before := state
	_, err := Apply(state, state.Revision, Operation{Kind: OpImportPlaylist, Tracks: []Track{track("overflow", ProviderSpotify)}})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("overflow import error = %v, want ErrQueueFull", err)
	}
	if len(state.Queue) != len(before.Queue) || state.Revision != before.Revision || state.Queue[0] != before.Queue[0] || state.Queue[len(before.Queue)-1] != before.Queue[len(before.Queue)-1] {
		t.Fatal("rejected playlist import changed queue state")
	}
}

func TestPlaylistImportRejectsInvalidItemWithoutPartialMutation(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("first", ProviderYouTube)}})
	before := state
	bad := track("bad", ProviderYouTube)
	bad.Title = string(make([]byte, MaxMetadataBytes+1))
	_, err := Apply(state, state.Revision, Operation{Kind: OpImportPlaylist, Tracks: []Track{track("good", ProviderYouTube), bad}})
	if !errors.Is(err, ErrInvalidTrack) {
		t.Fatalf("invalid import error = %v, want ErrInvalidTrack", err)
	}
	if len(state.Queue) != len(before.Queue) || state.Revision != before.Revision {
		t.Fatal("rejected playlist import changed queue state")
	}
}

func TestStopEndsCurrentTrackAndLeavesRemainderPaused(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube), track("two", ProviderSpotify)}})
	state = apply(t, state, Operation{Kind: OpResume})
	if state.Current == nil || state.Paused {
		t.Fatal("resume did not start first queued track")
	}
	state.PositionMS = 42_000
	state = apply(t, state, Operation{Kind: OpStop})
	if state.Current != nil || state.PositionMS != 0 || !state.Paused || len(state.Queue) != 1 || state.Queue[0].ID != "two" {
		t.Fatalf("stop state = current %v position %d paused %v queue %#v", state.Current, state.PositionMS, state.Paused, state.Queue)
	}
}

func TestRepeatCurrentAffectsNaturalCompletionButSkipAdvancesOnce(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube), track("two", ProviderYouTube)}})
	state = apply(t, state, Operation{Kind: OpResume})
	state = apply(t, state, Operation{Kind: OpSetRepeat, Repeat: RepeatCurrent})
	state = apply(t, state, Operation{Kind: OpComplete})
	if state.Current == nil || state.Current.ID != "one" || len(state.Queue) != 1 {
		t.Fatalf("repeat-current completion current/queue = %v/%v", state.Current, state.Queue)
	}
	state = apply(t, state, Operation{Kind: OpSkip})
	if state.Current == nil || state.Current.ID != "two" || len(state.Queue) != 0 {
		t.Fatalf("skip under repeat-current current/queue = %v/%v", state.Current, state.Queue)
	}
}

func TestRepeatQueueCyclesCompletedTrackToTail(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube), track("two", ProviderSpotify)}})
	state = apply(t, state, Operation{Kind: OpResume})
	state = apply(t, state, Operation{Kind: OpSetRepeat, Repeat: RepeatQueue})
	state = apply(t, state, Operation{Kind: OpComplete})
	if state.Current == nil || state.Current.ID != "two" || len(state.Queue) != 1 || state.Queue[0].ID != "one" {
		t.Fatalf("repeat-queue state current/queue = %v/%v", state.Current, state.Queue)
	}
}

func TestSkipWhilePausedAdvancesButKeepsQueuePaused(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube), track("two", ProviderYouTube)}})
	state = apply(t, state, Operation{Kind: OpResume})
	state = apply(t, state, Operation{Kind: OpPause})
	state = apply(t, state, Operation{Kind: OpSkip})
	if state.Current == nil || state.Current.ID != "two" || !state.Paused || len(state.Queue) != 0 {
		t.Fatalf("paused skip state = current %v paused %v queue %#v", state.Current, state.Paused, state.Queue)
	}
}

func TestSkipAfterStopDoesNotStartUpcomingTrack(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube), track("two", ProviderYouTube)}})
	state = apply(t, state, Operation{Kind: OpResume})
	state = apply(t, state, Operation{Kind: OpStop})
	state = apply(t, state, Operation{Kind: OpSkip})
	if state.Current != nil || !state.Paused || len(state.Queue) != 1 || state.Queue[0].ID != "two" {
		t.Fatalf("skip after stop state = current %v paused %v queue %#v", state.Current, state.Paused, state.Queue)
	}
}

func TestShuffleIsDeterministicForExplicitSeed(t *testing.T) {
	tracks := []Track{track("a", ProviderYouTube), track("b", ProviderYouTube), track("c", ProviderYouTube), track("d", ProviderYouTube)}
	left := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: tracks})
	right := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: tracks})
	left = apply(t, left, Operation{Kind: OpSetShuffle, Enabled: true, Seed: 1})
	right = apply(t, right, Operation{Kind: OpSetShuffle, Enabled: true, Seed: 1})
	for i := range left.Queue {
		if left.Queue[i].ID != right.Queue[i].ID {
			t.Fatalf("same seed produced different order: %v vs %v", left.Queue, right.Queue)
		}
	}
	if left.Queue[0].ID == "a" && left.Queue[1].ID == "b" && left.Queue[2].ID == "c" && left.Queue[3].ID == "d" {
		t.Fatal("shuffle left queue unchanged for seed 9182")
	}
}

func TestShuffleAppliesDeterministicallyToLaterEnqueueAndImport(t *testing.T) {
	initial := []Track{track("a", ProviderYouTube), track("b", ProviderYouTube)}
	added := []Track{track("c", ProviderYouTube), track("d", ProviderYouTube), track("e", ProviderYouTube), track("f", ProviderYouTube)}

	for _, kind := range []OperationKind{OpEnqueue, OpImportPlaylist} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			base := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: initial})
			base = apply(t, base, Operation{Kind: OpSetShuffle, Enabled: true, Seed: 13})
			left := apply(t, base, Operation{Kind: kind, Tracks: added, Seed: 42})
			right := apply(t, base, Operation{Kind: kind, Tracks: added, Seed: 42})
			if left.Queue[0].ID != "a" || left.Queue[1].ID != "b" {
				t.Fatalf("existing tracks moved when appending under shuffle: %v", left.Queue)
			}
			for i := range added {
				if left.Queue[i+2].ID != right.Queue[i+2].ID {
					t.Fatalf("same seed produced different appended order: %v vs %v", left.Queue, right.Queue)
				}
			}
			if left.Queue[2].ID == "c" && left.Queue[3].ID == "d" && left.Queue[4].ID == "e" && left.Queue[5].ID == "f" {
				t.Fatal("new tracks kept provider order while shuffle was enabled")
			}
		})
	}
}

func TestCanonicalIDsMustMatchProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider Provider
		id       string
	}{
		{"youtube rejects spotify URI", ProviderYouTube, "spotify:track:0123456789012345678901"},
		{"youtube rejects URL", ProviderYouTube, "https://youtube.com/watch?v=abcdefghijk"},
		{"youtube rejects path", ProviderYouTube, "/tmp/audio.ogg"},
		{"spotify rejects youtube ID", ProviderSpotify, "abcdefghijk"},
		{"spotify rejects URL", ProviderSpotify, "https://user:pass@open.spotify.com/track/0123456789012345678901"},
		{"spotify rejects playlist descriptor", ProviderSpotify, "spotify:playlist:0123456789012345678901"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := track("candidate", tc.provider)
			bad.CanonicalID = tc.id
			_, err := Apply(InitialState(), 0, Operation{Kind: OpEnqueue, Tracks: []Track{bad}})
			if !errors.Is(err, ErrInvalidTrack) {
				t.Fatalf("invalid canonical ID error = %v, want ErrInvalidTrack", err)
			}
		})
	}
}

func TestStaleRevisionConflictsWithoutMutation(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube)}})
	_, err := Apply(state, 0, Operation{Kind: OpClear})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale mutation error = %v, want ErrRevisionConflict", err)
	}
	if state.Revision != 1 || len(state.Queue) != 1 || state.Queue[0].ID != "one" {
		t.Fatal("stale mutation changed state")
	}
}

func TestReorderAndRemoveUseStableItemIDs(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("a", ProviderYouTube), track("b", ProviderYouTube), track("c", ProviderYouTube)}})
	state = apply(t, state, Operation{Kind: OpReorder, ItemID: "c", BeforeID: "b"})
	if got := []string{state.Queue[0].ID, state.Queue[1].ID, state.Queue[2].ID}; got[0] != "a" || got[1] != "c" || got[2] != "b" {
		t.Fatalf("reordered IDs = %v", got)
	}
	state = apply(t, state, Operation{Kind: OpRemove, ItemID: "c"})
	if len(state.Queue) != 2 || state.Queue[1].ID != "b" {
		t.Fatalf("queue after remove = %#v", state.Queue)
	}
}

func TestPauseSeekVolumeAndClearValidateBeforeRevision(t *testing.T) {
	state := apply(t, InitialState(), Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube)}})
	state = apply(t, state, Operation{Kind: OpResume})
	state = apply(t, state, Operation{Kind: OpPause})
	state = apply(t, state, Operation{Kind: OpSeek, PositionMS: 73_000})
	state = apply(t, state, Operation{Kind: OpSetVolume, Volume: 37})
	// Fix round 2: no provider applies a seek, so the playhead stays where it is (0 after the resume).
	if !state.Paused || state.PositionMS != 0 || state.Volume != 37 {
		t.Fatalf("pause/seek/volume state = %#v", state)
	}
	beforeRevision := state.Revision
	_, err := Apply(state, state.Revision, Operation{Kind: OpSetVolume, Volume: 101})
	if !errors.Is(err, ErrInvalidOperation) || state.Revision != beforeRevision {
		t.Fatalf("invalid volume error/revision = %v/%d", err, state.Revision)
	}
	state = apply(t, state, Operation{Kind: OpClear})
	if state.Current != nil || len(state.Queue) != 0 || !state.Paused || state.Revision != beforeRevision+1 {
		t.Fatalf("clear state = %#v", state)
	}
}

func TestRevisionedMutationsIncrementOnce(t *testing.T) {
	state := InitialState()
	state = apply(t, state, Operation{Kind: OpEnqueue, Tracks: []Track{track("one", ProviderYouTube), track("two", ProviderYouTube)}})
	if state.Revision != 1 {
		t.Fatalf("one playlist mutation incremented revision to %d", state.Revision)
	}
	state = apply(t, state, Operation{Kind: OpSetShuffle, Enabled: true, Seed: 1})
	if state.Revision != 2 {
		t.Fatalf("shuffle mutation revision = %d, want 2", state.Revision)
	}
}

// Ruling AI: the playhead is stamped with the Bot's time so a client can run it on.
func TestApplyStampsAndSettlesThePlayhead(t *testing.T) {
	track := Track{ID: "a", Provider: ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member", DurationMS: 10_000}
	state, err := Apply(InitialState(), 0, Operation{Kind: OpEnqueue, Tracks: []Track{track}, NowMS: 1_000})
	if err != nil || state.PositionAtMS != 1_000 {
		t.Fatalf("enqueue state=%+v err=%v", state, err)
	}
	state, err = Apply(state, state.Revision, Operation{Kind: OpResume, NowMS: 2_000})
	if err != nil || state.Paused || state.PositionMS != 0 || state.PositionAtMS != 2_000 {
		t.Fatalf("resume state=%+v err=%v", state, err)
	}
	if got := PlayheadMS(state, 5_500); got != 3_500 {
		t.Fatalf("playhead at 5.5s = %d, want 3500", got)
	}
	state, err = Apply(state, state.Revision, Operation{Kind: OpPause, NowMS: 6_000})
	if err != nil || state.PositionMS != 4_000 || state.PositionAtMS != 6_000 {
		t.Fatalf("pause must settle the reached playhead: %+v err=%v", state, err)
	}
	if got := PlayheadMS(state, 60_000); got != 4_000 {
		t.Fatalf("a paused playhead stays: %d", got)
	}
	state, err = Apply(state, state.Revision, Operation{Kind: OpResume, NowMS: 7_000})
	if err != nil || state.PositionMS != 0 || state.PositionAtMS != 7_000 {
		t.Fatalf("a resumed track restarts at 0 (providers have no offset) and restamps: %+v err=%v", state, err)
	}
	if got := PlayheadMS(state, 100_000); got != 10_000 {
		t.Fatalf("the playhead clamps to the duration: %d", got)
	}
	state, err = Apply(state, state.Revision, Operation{Kind: OpSeek, PositionMS: 1_234, NowMS: 8_000})
	if err != nil || state.PositionMS != 1_000 || state.PositionAtMS != 8_000 {
		t.Fatalf("a seek settles but does not move the playhead (no provider seeks): %+v err=%v", state, err)
	}
	unstamped, err := Apply(state, state.Revision, Operation{Kind: OpSetVolume, Volume: 50})
	if err != nil || unstamped.PositionMS != 1_000 || unstamped.PositionAtMS != 8_000 {
		t.Fatalf("an operation without NowMS leaves the playhead fields: %+v err=%v", unstamped, err)
	}
	if _, err := Apply(state, state.Revision, Operation{Kind: OpSetVolume, Volume: 50, NowMS: -1}); err == nil {
		t.Fatal("a negative NowMS is invalid")
	}
}

// Fix round 2 (task review): pause at 80 s, resume: the snapshot says 0 (the provider restarts the track), then it advances.
func TestResumeAfterPauseAt80sRestartsThePlayhead(t *testing.T) {
	track := Track{ID: "a", Provider: ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member", DurationMS: 200_000}
	state, _ := Apply(InitialState(), 0, Operation{Kind: OpEnqueue, Tracks: []Track{track}, NowMS: 1})
	state, _ = Apply(state, state.Revision, Operation{Kind: OpResume, NowMS: 10_000})
	state, err := Apply(state, state.Revision, Operation{Kind: OpPause, NowMS: 90_000})
	if err != nil || state.PositionMS != 80_000 {
		t.Fatalf("paused at %d, want 80000 (err %v)", state.PositionMS, err)
	}
	state, err = Apply(state, state.Revision, Operation{Kind: OpResume, NowMS: 100_000})
	if err != nil || state.PositionMS != 0 || PlayheadMS(state, 100_000) != 0 {
		t.Fatalf("resumed snapshot position %d, want 0 (err %v)", state.PositionMS, err)
	}
	if got := PlayheadMS(state, 105_000); got != 5_000 {
		t.Fatalf("then it advances: %d, want 5000", got)
	}
}
