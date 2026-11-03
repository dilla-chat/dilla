package api_test

// web-3 task 3, fix round 1 (task-3-review.md I1, I2, M1, M2): every path that ends a call stops
// its rings; one caller can hold at most 8 ringing calls and track at most 1,024 live pairs, so no
// caller can spend a shared budget to silence another's rings or to forgive its own limited pair.

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// ringCounter counts the state-1 rings each user received.
type ringCounter struct {
	mu sync.Mutex
	n  map[id.ID]int
}

func (g *ringCounter) DeliverUser(u id.ID, f gateway.Frame) {
	if f.Op != gateway.OpCallRing {
		return
	}
	var raw []cbor.RawMessage
	var state uint64
	if cborx.Unmarshal(f.Payload, &raw) != nil || len(raw) != 5 || cborx.Unmarshal(raw[3], &state) != nil || state != 1 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n[u]++
}

func (g *ringCounter) count(u id.ID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n[u]
}

func newDM() store.ChannelRow { return store.ChannelRow{ID: id.New(), Kind: api.ChannelDM} }

// I1: a call ended through the queued path (a channel delete, the sweep or a cut finding its group
// closed) stops its rings like DELETE does; a restart of the same DM rings again, and a decline that
// names the ended call's ring tells the new call's caller nothing.
func TestAQueuedEndStopsTheRing(t *testing.T) {
	h := newDMCallHost(t)
	call := h.startCall(t, h.alice)
	ring1 := h.lastFrame(t, h.bob.devices[0]).Expires
	h.calls.EndChannelCalls(t.Context(), []id.ID{h.dm}, "test")
	for _, dev := range h.bob.devices {
		requireRing(t, h.lastFrame(t, dev), call, h.dm, h.alice.user, 0, int64(ring1))
	}

	h.e.Clk.Advance(16 * time.Second)
	h.register(t, h.dm, h.alice, h.bob)
	before := len(h.gw.rings(h.bob.devices[0]))
	h.startCall(t, h.alice)
	rings := h.gw.rings(h.bob.devices[0])
	if len(rings) != before+1 || rings[len(rings)-1].State != 1 {
		t.Fatalf("the restarted call's rings to bob = %+v (had %d), want one new state 1", rings, before)
	}
	aliceBefore := len(h.gw.rings(h.alice.devices[0]))
	if status, body := h.decline(t, call, h.bob, ring1); status != http.StatusNoContent {
		t.Fatalf("a stale decline = %d %x", status, body)
	}
	if got := len(h.gw.rings(h.alice.devices[0])); got != aliceBefore {
		t.Fatalf("a decline of the ended call's ring told the new call's caller: %+v", h.gw.rings(h.alice.devices[0]))
	}
}

// Attacker (I2): one caller starting call after call to fill the instance's ringing-call table, so
// that nobody else's call rings. Each caller holds at most 8 ringing calls; the 9th rings nobody,
// and every other caller still rings.
func TestOneCallerCannotSilenceAnothersRing(t *testing.T) {
	gw := &ringCounter{n: map[id.ID]int{}}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	r := api.NewRinger(gw, clk, nil).WithMaxUsers(25)
	mallory := id.New()
	var victims []id.ID
	for range 9 {
		ch, v := newDM(), id.New()
		victims = append(victims, v)
		r.Start(ch, ch.ID, mallory, []id.ID{v})
	}
	for i, v := range victims {
		want := 1
		if i == 8 {
			want = 0
		}
		if gw.count(v) != want {
			t.Fatalf("mallory's call %d rang its victim %d times, want %d (at most 8 ringing calls per caller)", i+1, gw.count(v), want)
		}
	}
	for range 4096 {
		ch := newDM()
		r.Start(ch, ch.ID, mallory, []id.ID{id.New()})
	}
	alice, bob := id.New(), id.New()
	ch := newDM()
	r.Start(ch, ch.ID, alice, []id.ID{bob})
	if gw.count(bob) != 1 {
		t.Fatalf("after mallory's 4,105 starts alice's ring reached bob %d times, want 1", gw.count(bob))
	}
}

// The slot of a call whose rings ended is the caller's again.
func TestAnEndedRingFreesTheCallersSlot(t *testing.T) {
	gw := &ringCounter{n: map[id.ID]int{}}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	r := api.NewRinger(gw, clk, nil)
	mallory := id.New()
	var calls []id.ID
	for range 8 {
		ch := newDM()
		calls = append(calls, ch.ID)
		r.Start(ch, ch.ID, mallory, []id.ID{id.New()})
	}
	ninth, v := newDM(), id.New()
	r.Start(ninth, ninth.ID, mallory, []id.ID{v})
	if gw.count(v) != 0 {
		t.Fatal("a 9th ringing call rang")
	}
	r.End(calls[0])
	tenth, w := newDM(), id.New()
	r.Start(tenth, tenth.ID, mallory, []id.ID{w})
	if gw.count(w) != 1 {
		t.Fatal("a call after one of the 8 ended did not ring")
	}
}

// ringMany has caller ring every recipient in one call, then ends it.
func ringMany(r *api.Ringer, caller id.ID, recipients []id.ID) {
	ch := store.ChannelRow{ID: id.New(), Kind: api.ChannelGroupDM}
	r.Start(ch, ch.ID, caller, recipients)
	r.End(ch.ID)
}

func freshIDs(n int) []id.ID {
	out := make([]id.ID, n)
	for i := range out {
		out[i] = id.New()
	}
	return out
}

// Attacker (M1): mallory has spent her 20 rings of bob this hour and, with up to 63 accomplices,
// rings fresh users to push (mallory, bob) out of the pair table. Each caller tracks at most 1,024
// live pairs, so 64 callers never overflow the 65,536-pair table and the limited pair survives.
func TestALimitedPairSurvivesOtherCallersPairs(t *testing.T) {
	gw := &ringCounter{n: map[id.ID]int{}}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	r := api.NewRinger(gw, clk, nil)
	mallory, bob := id.New(), id.New()
	for range 20 {
		ringMany(r, mallory, []id.ID{bob})
		clk.Advance(16 * time.Second)
	}
	before := gw.count(bob)
	ringMany(r, mallory, []id.ID{bob})
	if gw.count(bob) != before {
		t.Fatal("the 21st ring within the hour rang")
	}
	callers := append([]id.ID{mallory}, freshIDs(63)...)
	for _, c := range callers {
		ringMany(r, c, freshIDs(2000))
	}
	ringMany(r, mallory, []id.ID{bob})
	if gw.count(bob) != before {
		t.Fatal("mallory rang bob again within the hour after 64 callers rang fresh users: the pair was evicted")
	}
}

// M1: a caller at its 1,024 live pairs fails closed for new pairs (for that caller only); once the
// hour passes its pairs expire and it rings new users again.
func TestACallersLivePairCapFailsClosedUntilThePairsExpire(t *testing.T) {
	gw := &ringCounter{n: map[id.ID]int{}}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	r := api.NewRinger(gw, clk, nil)
	mallory := id.New()
	first := freshIDs(1024)
	ringMany(r, mallory, first)
	for _, u := range first {
		if gw.count(u) != 1 {
			t.Fatal("one of mallory's first 1,024 pairs did not ring")
		}
	}
	extra := id.New()
	ringMany(r, mallory, []id.ID{extra})
	if gw.count(extra) != 0 {
		t.Fatal("mallory's 1,025th live pair rang")
	}
	alice, bob := id.New(), id.New()
	ringMany(r, alice, []id.ID{bob})
	if gw.count(bob) != 1 {
		t.Fatal("mallory's full share kept alice from ringing bob")
	}
	clk.Advance(time.Hour)
	later := id.New()
	ringMany(r, mallory, []id.ID{later})
	if gw.count(later) != 1 {
		t.Fatal("after her pairs' window passed mallory still could not ring a new user")
	}
}

// M2: two starts that both read the DM call as not live both answer 201, but only the one whose
// write opened the room rings; the other rings nobody.
func TestOnlyTheStartThatOpenedTheRoomRings(t *testing.T) {
	h := newDMCallHost(t)
	nested := false
	h.calls.SetStartHookForTest(func(stage string) {
		if stage != "put" || nested {
			return
		}
		nested = true
		// bob's start runs to the end inside alice's, between her read and her write.
		h.startIn(t, h.dm, h.bob, http.StatusCreated)
	})
	status, body := h.e.Do(http.MethodPost, "/v1/channels/"+h.dm.String()+"/calls", h.alice.tok, []any{})
	h.calls.SetStartHookForTest(nil)
	if status != http.StatusCreated {
		t.Fatalf("alice's start = %d %x", status, body)
	}
	if f := h.lastFrame(t, h.alice.devices[0]); f.State != 1 || f.User != h.bob.user {
		t.Fatalf("alice's ring = %+v, want bob's", f)
	}
	for _, dev := range h.bob.devices {
		h.requireNoFrame(t, dev)
	}
}
