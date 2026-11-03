package gateway

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// The brief lists ticket_test.go among this task's files but gives no body for it; these are the
// three properties gap-38 states about POST /v1/gateway/ticket, written here so ticket.go is not
// the one file of the task that ships untested.

// A minted ticket resolves to the session token it was minted for, exactly once.
func TestATicketIsSingleUse(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	tk := NewTickets(clk)

	s, expires, err := tk.Mint(id.New(), "session-token")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if s == "" {
		t.Fatal("Mint returned an empty ticket")
	}
	if want := clk.Now().Add(30 * time.Second); !expires.Equal(want) {
		t.Errorf("expires = %v, want %v (a ticket lives 30 seconds)", expires, want)
	}

	got, ok := tk.Take(s)
	if !ok {
		t.Fatal("a fresh ticket was not accepted")
	}
	if got != "session-token" {
		t.Errorf("Take returned %q, want the session token it was minted for", got)
	}
	if _, ok := tk.Take(s); ok {
		t.Error("a ticket was accepted twice; the upgrade credential must be single-use")
	}
}

// A ticket older than its 30-second window is a miss, and the sweep does not resurrect it.
func TestAnExpiredTicketIsAMiss(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	tk := NewTickets(clk)

	s, _, err := tk.Mint(id.New(), "session-token")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	clk.Advance(31 * time.Second)
	if _, ok := tk.Take(s); ok {
		t.Error("a ticket older than 30 seconds was accepted")
	}
	if _, ok := tk.Take(s); ok {
		t.Error("an expired ticket was accepted on a second attempt")
	}
}

// Minting sweeps the expired entries, so a gateway nobody upgrades through does not grow a map of
// dead tickets for the life of the process.
func TestMintingSweepsExpiredTickets(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	tk := NewTickets(clk)

	for range 4 {
		if _, _, err := tk.Mint(id.New(), "stale"); err != nil {
			t.Fatalf("Mint: %v", err)
		}
	}
	clk.Advance(31 * time.Second)
	if _, _, err := tk.Mint(id.New(), "fresh"); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := tk.len(); got != 1 {
		t.Errorf("%d tickets held, want 1: Mint must sweep the expired entries", got)
	}
}

// An unknown ticket string is a miss, never a panic and never someone else's token.
func TestAnUnknownTicketIsAMiss(t *testing.T) {
	tk := NewTickets(clock.NewFake(time.Unix(1_700_000_000, 0)))
	if _, ok := tk.Take("not-a-ticket"); ok {
		t.Error("an unknown ticket was accepted")
	}
}
