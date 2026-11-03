package ds_test

import (
	"context"
	"encoding/hex"
	"sync"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/store"
)

// fakeAuth is the other side of `gateway.Options.Auth`, which is an interface declared in the
// gateway package rather than *auth.Sessions (deviation B9). It is not a double for anything the
// delivery service's tests assert: the gateway's ready path needs SOME token resolver before a
// device can be online, and standing the whole account stack up here would test that stack.
type fakeAuth struct {
	mu       sync.Mutex
	sessions map[string]auth.Session
}

func newFakeAuth() *fakeAuth { return &fakeAuth{sessions: map[string]auth.Session{}} }

// token is the bearer this resolver answers for a session: the device id in hex, which is unique
// per device and needs no state to compute.
func (f *fakeAuth) token(s auth.Session) string { return hex.EncodeToString(s.DeviceID[:]) }

func (f *fakeAuth) add(s auth.Session) {
	f.mu.Lock()
	f.sessions[f.token(s)] = s
	f.mu.Unlock()
}

func (f *fakeAuth) Resolve(_ context.Context, bearer string) (auth.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[bearer]
	if !ok {
		return auth.Session{}, store.ErrNotFound
	}
	return s, nil
}
