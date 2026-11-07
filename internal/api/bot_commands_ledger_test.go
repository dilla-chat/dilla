package api

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// A reply is delivered only for a request relayed from that device into the replying Bot's own
// community: the same request_id in another community does not match.
func TestReplyCannotTargetAnotherCommunitysRequest(t *testing.T) {
	l := NewBotRequestLedger(clock.NewFake(time.Unix(1_700_000_000, 0)))
	request, device, mine, theirs := id.New(), id.New(), id.New(), id.New()
	if _, err := l.admit(request, device, theirs); err != nil {
		t.Fatal(err)
	}
	if l.relayed(request, device, mine) {
		t.Fatal("a request relayed into another community matched this Bot's reply")
	}
	if !l.relayed(request, device, theirs) {
		t.Fatal("the request is not recorded for its own community")
	}
	// The same device sending the id into another community is a duplicate too.
	if _, err := l.admit(request, device, mine); err == nil {
		t.Fatal("the same request_id into another community was admitted")
	}
}

func TestDuplicateLedgerIsBoundedAndEvictsTheOldest(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	l := newBotRequestLedger(clk, 3)
	device, community := id.New(), id.New()
	ids := []id.ID{id.New(), id.New(), id.New(), id.New()}
	for _, r := range ids[:3] {
		if _, err := l.admit(r, device, community); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	// A re-send refreshes ids[0], so ids[1] is now the oldest and the one the cap evicts.
	if inserted, err := l.admit(ids[0], device, community); err != nil || inserted {
		t.Fatalf("re-send = %v, %v; want refreshed, not inserted", inserted, err)
	}
	if _, err := l.admit(ids[3], device, community); err != nil {
		t.Fatal(err)
	}
	if l.len() != 3 {
		t.Fatalf("ledger holds %d entries, want the cap of 3", l.len())
	}
	if l.relayed(ids[1], device, community) {
		t.Fatal("the oldest entry survived the cap")
	}
	for _, r := range []id.ID{ids[0], ids[2], ids[3]} {
		if !l.relayed(r, device, community) {
			t.Fatalf("entry %s was evicted", r)
		}
	}
	// Every entry is gone five minutes after its last relay.
	clk.Advance(BotRequestTTL + time.Second)
	if l.relayed(ids[3], device, community) || l.len() != 0 {
		t.Fatalf("entries outlived BotRequestTTL: %d left", l.len())
	}
	// forget takes back only the device's own entry.
	if _, err := l.admit(ids[0], device, community); err != nil {
		t.Fatal(err)
	}
	l.forget(ids[0], id.New())
	if !l.relayed(ids[0], device, community) {
		t.Fatal("another device's forget removed the entry")
	}
	l.forget(ids[0], device)
	if l.relayed(ids[0], device, community) {
		t.Fatal("forget left the entry")
	}
}
