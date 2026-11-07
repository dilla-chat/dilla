//go:build linux

package main

import "time"

// interactionSpacing is the least time between two interaction fast-path polls: signals inside
// the gap coalesce into one poll after it. interactionBurst is how many fast polls may follow one
// another at that spacing; each group refills at two tokens per second.
// Per-installation spacing and the worker cap also bound a flood across groups.
const (
	interactionSpacing = 250 * time.Millisecond
	interactionBurst   = 10
	interactionRefill  = time.Second / 2
)

// interactionBudget is one interaction group's token bucket for fast-path work. Each synced
// group spends its own token, so one member's flood does not slow another's commands, even in
// the same installation. It outlives one wait, so
// a flood cannot reset it by spanning a reconciliation pass.
type interactionBudget struct {
	tokens   float64
	at, last time.Time
	used     bool
}

func (b *interactionBudget) refill(now time.Time) {
	if !b.used {
		b.tokens, b.at, b.used = interactionBurst, now, true
		return
	}
	b.tokens += float64(now.Sub(b.at)) / float64(interactionRefill)
	if b.tokens > interactionBurst {
		b.tokens = interactionBurst
	}
	b.at = now
}

// delay is how long the next fast poll must wait: the spacing since the last one, and while the
// allowance is spent, until a token has refilled.
func (b *interactionBudget) delay(now time.Time) time.Duration {
	b.refill(now)
	d := interactionSpacing - now.Sub(b.last)
	if b.tokens < 1 {
		if refill := time.Duration((1 - b.tokens) * float64(interactionRefill)); refill > d {
			d = refill
		}
	}
	return d
}

// spend charges tokens for a poll that began at now. Each group sync spends one token.
func (b *interactionBudget) spend(now time.Time, groups int) {
	b.refill(now)
	b.tokens -= float64(groups)
	b.last = now
}
