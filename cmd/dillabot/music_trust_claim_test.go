//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type sharedMusicTrustCore struct {
	musicJoinCoreFake
	mu             sync.Mutex
	binding        native.Binding
	user           id.ID
	cached         uint64
	active         bool
	pendingErr     error
	advanceOnClaim bool
	busyClaims     int
	trusts         int
}

func (c *sharedMusicTrustCore) Binding() native.Binding { return c.binding }
func (c *sharedMusicTrustCore) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return []native.GroupPendingLeaf{{Group: c.group, User: c.user, Leaf: 1, CachedVersion: c.cached}}, c.pendingErr
}
func (c *sharedMusicTrustCore) TryClaimTrust(user id.ID) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busyClaims > 0 {
		c.busyClaims--
		return nil, false
	}
	if c.advanceOnClaim {
		c.cached = 2
		c.advanceOnClaim = false
	}
	if c.active || user != c.user {
		return nil, false
	}
	c.active = true
	return func() { c.mu.Lock(); c.active = false; c.mu.Unlock() }, true
}
func (c *sharedMusicTrustCore) TrustSender(_ context.Context, _ id.ID, _ uint64, _ id.ID, history, _ []byte, _ uint64) (native.GroupTrustVerdict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var rows [][]any
	if cborx.Unmarshal(history, &rows) != nil || len(rows) == 0 || rows[0][0] != c.cached {
		return native.GroupTrustVerdict{}, errors.New("stale cached version")
	}
	c.cached = 2
	c.trusts++
	return native.GroupTrustVerdict{}, nil
}
func (*sharedMusicTrustCore) ProposedAdds(context.Context, id.ID, []byte) ([]native.GroupProposedAdd, error) {
	return nil, nil
}
func (*sharedMusicTrustCore) TrustProposedAdd(context.Context, id.ID, []byte, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error) {
	return native.GroupTrustVerdict{}, nil
}
func (*sharedMusicTrustCore) RevalidateGroup(context.Context, id.ID, uint64, uint64) (uint32, error) {
	return 0, nil
}

func TestIntakeTrustWarningIncludesPhaseAndBoundedCause(t *testing.T) {
	secret := "private history URL https://example.invalid/private"
	core := &sharedMusicTrustCore{binding: native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}, pendingErr: errors.New(secret)}
	trust, err := service.NewSourceTrust(core)
	if err != nil {
		t.Fatal(err)
	}
	_, phaseErr := trust.RefreshPendingLeaves(t.Context(), &client.Session{})
	var logs bytes.Buffer
	r := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer r.Close()
	pool := &commandWorkers{ctx: t.Context(), retry: make(map[interactionWork]time.Time)}
	job := commandJob{work: interactionWork{installation: id.New(), group: id.New()}}
	r.SetInteractionPoller(job.work.installation, func(context.Context, id.ID) error { return nil })
	r.finishCommandJob(pool, job, phaseErr, time.Now())
	if !bytes.Contains(logs.Bytes(), []byte(`"phase":"pending_leaves"`)) || !bytes.Contains(logs.Bytes(), []byte(`"cause_class":"unknown"`)) || bytes.Contains(logs.Bytes(), []byte(secret)) {
		t.Fatalf("trust diagnostic=%s", logs.String())
	}
}
