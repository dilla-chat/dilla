package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Tickets mints the single-use 30-second upgrade tickets of POST /v1/gateway/ticket, for browsers
// that cannot set an Authorization header on a WebSocket upgrade and must pass the credential in
// Sec-WebSocket-Protocol instead (gap-38).
type Tickets struct {
	mu    sync.Mutex
	clk   clock.Clock
	ttl   time.Duration
	items map[string]ticket
}

type ticket struct {
	deviceID id.ID
	token    string
	expires  time.Time
}

func NewTickets(clk clock.Clock) *Tickets {
	return &Tickets{clk: clk, ttl: 30 * time.Second, items: map[string]ticket{}}
}

// Mint returns the ticket string and its expiry.
func (t *Tickets) Mint(deviceID id.ID, sessionToken string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	s := base64.RawURLEncoding.EncodeToString(raw)
	expires := t.clk.Now().Add(t.ttl)
	t.mu.Lock()
	t.items[s] = ticket{deviceID: deviceID, token: sessionToken, expires: expires}
	t.sweepLocked()
	t.mu.Unlock()
	return s, expires, nil
}

// Take consumes a ticket. A ticket is valid once; a second use is a miss.
func (t *Tickets) Take(s string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	item, ok := t.items[s]
	if !ok {
		return "", false
	}
	delete(t.items, s)
	if t.clk.Now().After(item.expires) {
		return "", false
	}
	return item.token, true
}

func (t *Tickets) sweepLocked() {
	now := t.clk.Now()
	for k, v := range t.items {
		if now.After(v.expires) {
			delete(t.items, k)
		}
	}
}

// len is the number of tickets outstanding. It exists for ticket_test.go's sweep assertion; the
// gateway itself never asks.
func (t *Tickets) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}
