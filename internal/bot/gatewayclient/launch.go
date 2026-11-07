package gatewayclient

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
)

// LaunchToken is the single-use launch token dillad's supervisor hands the Bot it starts, on fd 3 (Bot v2 task 11).
// dillad admits the Bot's first identify over its Unix socket only with it, and spends it then; after that, every
// identify of the same child goes without. The Bot's per-installation connections share one LaunchToken: identifies
// that might carry it take turns, so two of them never present it at once, and an identify dillad refused leaves it
// usable (dillad spends it only on an identify it binds).
type LaunchToken struct {
	turn  chan struct{}
	mu    sync.Mutex
	value string
	spent bool
}

// NewLaunchToken holds raw (32 bytes) and clears the caller's copy.
func NewLaunchToken(raw []byte) (*LaunchToken, error) {
	if len(raw) != 32 {
		return nil, errors.New("gatewayclient: a launch token is 32 bytes")
	}
	t := &LaunchToken{turn: make(chan struct{}, 1), value: base64.RawURLEncoding.EncodeToString(raw)}
	clear(raw)
	return t, nil
}

// acquire returns what an identify's session_token carries: the token while it is unspent, then "". settle(true)
// records that the identify reached ready, which spent it. While an identify holds the token, others wait for it to
// settle. A nil LaunchToken (a network Bot) always answers "".
func (t *LaunchToken) acquire(ctx context.Context) (string, func(ready bool), error) {
	noop := func(bool) {}
	if t == nil {
		return "", noop, nil
	}
	t.mu.Lock()
	spent := t.spent
	t.mu.Unlock()
	if spent {
		return "", noop, nil
	}
	select {
	case t.turn <- struct{}{}:
	case <-ctx.Done():
		return "", noop, ctx.Err()
	}
	t.mu.Lock()
	if t.spent {
		t.mu.Unlock()
		<-t.turn
		return "", noop, nil
	}
	value := t.value
	t.mu.Unlock()
	var once sync.Once
	return value, func(ready bool) {
		once.Do(func() {
			if ready {
				t.mu.Lock()
				t.spent, t.value = true, ""
				t.mu.Unlock()
			}
			<-t.turn
		})
	}, nil
}
