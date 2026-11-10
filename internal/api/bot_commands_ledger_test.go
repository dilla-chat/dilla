package api

import (
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// A device at its quota evicts its own oldest entry; another device's entries survive, however
// many devices fill the ledger (M1).
func TestDuplicateLedgerQuotaEvictsOnlyTheDevicesOwnEntries(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	l := newBotRequestLedger(clk, 1000, 4)
	community, victim, flooder := id.New(), id.New(), id.New()
	victimRequest := id.New()
	if _, err := l.admit(victimRequest, victim, community); err != nil {
		t.Fatal(err)
	}
	flood := make([]id.ID, 0, 50)
	for range 50 {
		r := id.New()
		flood = append(flood, r)
		if _, err := l.admit(r, flooder, community); err != nil {
			t.Fatal(err)
		}
	}
	if !l.relayed(victimRequest, victim, community) {
		t.Fatal("one device's flood evicted another device's entry")
	}
	if l.len() != 5 {
		t.Fatalf("ledger holds %d entries, want the victim's 1 and the flooder's quota of 4", l.len())
	}
	for i, r := range flood {
		if got, want := l.relayed(r, flooder, community), i >= 46; got != want {
			t.Fatalf("flood entry %d relayed = %v, want %v (only the newest 4 stay)", i, got, want)
		}
	}
	// The flooder still cannot claim the victim's request_id.
	if _, err := l.admit(victimRequest, flooder, community); err == nil {
		t.Fatal("the flooder claimed the victim's request_id")
	}
}

// The Bot may answer one request at most BotRepliesPerRequest times and BotReplyBytesPerRequest
// bytes in all (M4).
func TestReplyCapPerRequest(t *testing.T) {
	l := NewBotRequestLedger(clock.NewFake(time.Unix(1_700_000_000, 0)))
	request, device, community := id.New(), id.New(), id.New()
	if _, err := l.admit(request, device, community); err != nil {
		t.Fatal(err)
	}
	for i := range BotRepliesPerRequest {
		if err := l.reply(request, device, community, 10); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if err := l.reply(request, device, community, 10); ledgerCode(err) != "E_TOO_LARGE" {
		t.Fatalf("reply %d = %v, want E_TOO_LARGE", BotRepliesPerRequest+1, err)
	}
	big := id.New()
	if _, err := l.admit(big, device, community); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := l.reply(big, device, community, 64<<10); err != nil {
			t.Fatalf("64 KiB reply %d: %v", i, err)
		}
	}
	if err := l.reply(big, device, community, 1); ledgerCode(err) != "E_TOO_LARGE" {
		t.Fatalf("a reply past 256 KiB = %v, want E_TOO_LARGE", err)
	}
	if err := l.reply(id.New(), device, community, 1); ledgerCode(err) != "E_NOT_FOUND" {
		t.Fatalf("a reply to an unknown request = %v, want E_NOT_FOUND", err)
	}
}

// A same-device resend (Retry, or a carried redelivery) re-admits the request with a fresh reply
// budget: the Bot re-sends every kept answer to the new seal, so a k-answer command must survive more
// than BotRepliesPerRequest/k sends (final review I4).
func TestResendResetsTheReplyBudget(t *testing.T) {
	l := NewBotRequestLedger(clock.NewFake(time.Unix(1_700_000_000, 0)))
	request, device, community := id.New(), id.New(), id.New()
	const answers = 5
	for send := range 2*BotRepliesPerRequest/answers + 1 {
		if _, err := l.admit(request, device, community); err != nil {
			t.Fatal(err)
		}
		for i := range answers {
			if err := l.reply(request, device, community, 40<<10); err != nil {
				t.Fatalf("send %d answer %d: %v", send, i, err)
			}
		}
	}
}

func ledgerCode(err error) string {
	var e *server.Error
	if errors.As(err, &e) {
		return string(e.Code)
	}
	return ""
}

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
	l := newBotRequestLedger(clk, 3, 3)
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
