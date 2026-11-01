package native

import (
	"sync"

	"github.com/jonasthim/dilla/internal/id"
)

// TryClaimSource prevents overlapping history synchronization for one group
// across service wrappers sharing this installation's native client. The caller
// holds the claim, but no client mutex, throughout its HTTP/helper work.
func (c *Client) TryClaimSource(group id.ID) (func(), bool) {
	return c.tryClaim(group, &c.sources)
}

// TryClaimTrust prevents overlapping trust refreshes for one user across
// service wrappers sharing this installation's native client.
func (c *Client) TryClaimTrust(user id.ID) (func(), bool) {
	return c.tryClaim(user, &c.trust)
}

func (c *Client) tryClaim(key id.ID, claims *map[id.ID]bool) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.usable || key.IsZero() || (*claims)[key] {
		return nil, false
	}
	if *claims == nil {
		*claims = make(map[id.ID]bool)
	}
	(*claims)[key] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			delete(*claims, key)
			c.mu.Unlock()
		})
	}, true
}
