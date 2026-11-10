package service

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

type countingStateStore struct {
	musicStateStore
	mu    sync.Mutex
	loads int
	gate  chan struct{} // when set, a Load waits for it (or for its ctx)
}

func (s *countingStateStore) Load(ctx context.Context, channel id.ID) (persistence.Snapshot, error) {
	s.mu.Lock()
	s.loads++
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return persistence.Snapshot{}, ctx.Err()
		}
	}
	return s.musicStateStore.Load(ctx, channel)
}

func (s *countingStateStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func stateReadRig(t *testing.T) (*authzRig, *countingStateStore, *SealedDispatcher, MusicInteractions) {
	t.Helper()
	r := newAuthzRig(t)
	n := &playlistAcceptanceNative{binding: native.Binding{Instance: id.New(), Community: r.community, Installation: r.authority.InstallationID}, rows: map[string]native.StateValue{}}
	base, err := persistence.New(n, r.community)
	if err != nil {
		t.Fatal(err)
	}
	stateStore := &countingStateStore{musicStateStore: base}
	fixed := time.Now()
	reads := &MusicStateReads{now: func() time.Time { return fixed }, entries: map[id.ID]*musicStateRead{}}
	h := MusicInteractions{Store: stateStore, Authority: r.authority, InstallationID: r.authority.InstallationID, CommunityID: r.community,
		Resolver: staticMusicResolver{}, StateReads: reads, Now: func() time.Time { return fixed }}
	return r, stateStore, &SealedDispatcher{Community: r.community, Handlers: h.Handlers()}, h
}

func sealedMusic(t *testing.T, d *SealedDispatcher, r *authzRig, user id.ID, op protocol.Operation, args ...any) intake.Answer {
	t.Helper()
	req := protocol.SealedRequest{RequestID: id.New(), CommunityID: r.community, ChannelID: r.channel, Operation: uint64(op),
		Args: args, SentAt: uint64(time.Now().Unix()), UserID: user, DeviceID: id.New()}
	var last intake.Answer
	d.Dispatch(t.Context(), req, func(a intake.Answer) { last = a })
	return last
}

// Bot v2 Task 19 fix round 4 (Task 21b ruling 3): a call hint makes every member read music.state at once; the reads
// in one window share one call read and one state read, and each member is still authorised and answered on their own.
func TestMusicStateReadsCoalescePerCallAndStayPerMember(t *testing.T) {
	r, stateStore, d, h := stateReadRig(t)
	stranger := id.New() // not a member: cannot see the voice channel
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := r.listener
			if i%2 == 0 {
				user = r.dj
			}
			if a := sealedMusic(t, d, r, user, protocol.OpMusicState); a.Status != intake.StatusOK {
				t.Errorf("member read = %d", a.Status)
			}
		}(i)
	}
	wg.Wait()
	state := func(user id.ID) []byte {
		t.Helper()
		res, err := h.handle(withMusicCallMemo(t.Context()), InteractionActor{User: user}, protocol.Request{CommunityID: r.community, ChannelID: &r.channel}, protocol.OpMusicState)
		if err != nil || res.Status != 0 {
			t.Fatalf("state for %s = %+v, %v", user, res, err)
		}
		return res.Data
	}
	dj, listener := state(r.dj), state(r.listener)
	if r.session.callReads != 1 || stateStore.count() != 1 {
		t.Fatalf("22 reads in one window cost %d call reads and %d state reads, want 1 and 1", r.session.callReads, stateStore.count())
	}
	if bytes.Equal(dj, listener) {
		t.Fatal("the DJ and the listener got the same answer: canControl is per member")
	}
	if a := sealedMusic(t, d, r, stranger, protocol.OpMusicState); a.Status == intake.StatusOK {
		t.Fatal("someone who cannot see the voice channel read the shared state")
	}
}

// A committed change through the music handlers ends the call's window: the next read sees it.
func TestAMusicChangeEndsTheSharedStateWindow(t *testing.T) {
	r, stateStore, d, _ := stateReadRig(t)
	if a := sealedMusic(t, d, r, r.dj, protocol.OpMusicState); a.Status != intake.StatusOK {
		t.Fatalf("read = %d", a.Status)
	}
	before := stateStore.count()
	if a := sealedMusic(t, d, r, r.dj, protocol.OpMusicEnqueue, "song"); a.Status != intake.StatusOK {
		t.Fatalf("/play = %d", a.Status)
	}
	loadsAfterPlay := stateStore.count()
	if a := sealedMusic(t, d, r, r.listener, protocol.OpMusicState); a.Status != intake.StatusOK {
		t.Fatalf("read after = %d", a.Status)
	}
	if stateStore.count() != loadsAfterPlay+1 || loadsAfterPlay <= before {
		t.Fatalf("state reads %d → %d → %d, want the read after the change to read again", before, loadsAfterPlay, stateStore.count())
	}
}

// Fix round 5 (M4-3): a reader who may not see the voice channel is refused before the queue state is read for anyone.
func TestARefusedReaderTriggersNoSharedStateRead(t *testing.T) {
	r, stateStore, _, h := stateReadRig(t)
	res, err := h.handle(withMusicCallMemo(t.Context()), InteractionActor{User: id.New()}, protocol.Request{CommunityID: r.community, ChannelID: &r.channel}, protocol.OpMusicState)
	if err != nil || res.Status != interactionStatusForbidden {
		t.Fatalf("a stranger's read = %+v, %v, want forbidden", res, err)
	}
	if n := stateStore.count(); n != 0 {
		t.Fatalf("a refused read cost %d state reads, want 0", n)
	}
}

// Fix round 5 (M4-3): the shared computation runs apart from the reader that started it: that reader going away
// fails only its own read, and the others in the window are answered.
func TestAReaderWhoGoesAwayDoesNotFailTheOthers(t *testing.T) {
	r, stateStore, _, h := stateReadRig(t)
	gate := make(chan struct{})
	stateStore.mu.Lock()
	stateStore.gate = gate
	stateStore.mu.Unlock()
	read := func(ctx context.Context, user id.ID) (InteractionResult, error) {
		return h.handle(withMusicCallMemo(ctx), InteractionActor{User: user}, protocol.Request{CommunityID: r.community, ChannelID: &r.channel}, protocol.OpMusicState)
	}
	first, cancelFirst := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		_, err := read(first, r.dj)
		firstDone <- err
	}()
	for stateStore.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	otherDone := make(chan InteractionResult, 1)
	go func() {
		res, err := read(t.Context(), r.listener)
		if err != nil {
			t.Errorf("the other reader: %v", err)
		}
		otherDone <- res
	}()
	time.Sleep(20 * time.Millisecond)
	cancelFirst()
	if err := <-firstDone; err == nil {
		t.Fatal("the reader who went away was answered")
	}
	close(gate)
	if res := <-otherDone; res.Status != 0 || res.Data == nil {
		t.Fatalf("the other reader = %+v", res)
	}
	if n := stateStore.count(); n != 1 {
		t.Fatalf("state reads = %d, want the one shared", n)
	}
}
