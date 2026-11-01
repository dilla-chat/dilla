//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
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

type blockedMusicTrustHistory struct {
	musicCallJoinReader
	entered, release chan struct{}
	head             []byte
	rows             [][]any
	once             sync.Once
}

func (c *blockedMusicTrustHistory) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.head, nil
}
func (c *blockedMusicTrustHistory) DeviceListHistory(_ context.Context, _ id.ID, after uint64) ([][]any, error) {
	var rows [][]any
	for _, row := range c.rows {
		if row[0].(uint64) > after {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func TestMusicJoinTrustAndIntakeTrustNeverFailEachOther(t *testing.T) {
	core := &sharedMusicTrustCore{musicJoinCoreFake: musicJoinCoreFake{group: id.New()}, binding: native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}, user: id.New(), cached: 1}
	rows := [][]any{{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)}, {uint64(2), []byte("two"), make([]byte, 64), make([]byte, 32)}}
	head, err := cborx.Marshal(rows[1])
	if err != nil {
		t.Fatal(err)
	}
	calls := &blockedMusicTrustHistory{entered: make(chan struct{}), release: make(chan struct{}), head: head, rows: rows}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []byte
		if r.URL.Path == "/v1/instance" {
			data, _ = cborx.Marshal([]any{[]any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}, core.binding.Instance, uint64(1), "test.local", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)})
		} else if r.URL.Query().Has("after") {
			selected := rows
			if r.URL.Query().Get("after") == "1" {
				selected = rows[1:]
			}
			data, _ = cborx.Marshal(selected)
		} else {
			data = head
		}
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: core.binding.Instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	session, err := api.Session("trust-test")
	if err != nil {
		t.Fatal(err)
	}
	trust, err := service.NewSourceTrust(core)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- refreshMusicCallJoinLeaves(t.Context(), calls, core, core.group) }()
	<-calls.entered
	refresh, intakeErr := trust.RefreshPendingLeaves(t.Context(), session)
	close(calls.release)
	joinErr := <-done
	if intakeErr != nil || !refresh.Busy[core.user] || joinErr != nil {
		t.Fatalf("intake busy=%v err=%v join=%v", refresh.Busy, intakeErr, joinErr)
	}
	// The claim was released, and a fresh pass uses the new cached anchor.
	refresh, err = trust.RefreshPendingLeaves(t.Context(), session)
	if err != nil || len(refresh.Busy) != 0 {
		t.Fatalf("claim leaked: refresh=%+v err=%v cause=%v", refresh, err, errors.Unwrap(err))
	}
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

func TestMusicJoinTrustReleasesClaimOnHistoryError(t *testing.T) {
	core := &sharedMusicTrustCore{musicJoinCoreFake: musicJoinCoreFake{group: id.New()}, user: id.New(), cached: 1}
	releaseHistory := make(chan struct{})
	close(releaseHistory)
	calls := &blockedMusicTrustHistory{entered: make(chan struct{}), release: releaseHistory, head: []byte{0}}
	if err := refreshMusicCallJoinLeaves(t.Context(), calls, core, core.group); err == nil {
		t.Fatal("malformed history accepted")
	}
	release, ok := core.TryClaimTrust(core.user)
	if !ok {
		t.Fatal("history failure leaked trust claim")
	}
	release()
}

func TestMusicJoinRetriesIntakeOwnedTrustClaim(t *testing.T) {
	calls := &claimRetryMusicCalls{musicJoinCallsFake: musicJoinCallsFake{channel: id.New(), group: id.New()}}
	core := &sharedMusicTrustCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}, user: id.New(), cached: 1, busyClaims: 1}
	var delays []time.Duration
	err := retryMusicCallGroupJoin(t.Context(), calls, core, id.New(), id.New(), calls.channel, 1, func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil })
	if err != nil || len(delays) != 1 || delays[0] != 250*time.Millisecond || core.trusts != 1 {
		t.Fatalf("busy join did not retry: err=%v delays=%v trusts=%d", err, delays, core.trusts)
	}
}

type claimRetryMusicCalls struct{ musicJoinCallsFake }

func (c *claimRetryMusicCalls) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	return cborx.Marshal([]any{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)})
}
func (c *claimRetryMusicCalls) DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error) {
	return [][]any{{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)}}, nil
}
func TestMusicJoinRereadsCachedAnchorAfterClaim(t *testing.T) {
	group, user := id.New(), id.New()
	core := &sharedMusicTrustCore{musicJoinCoreFake: musicJoinCoreFake{group: group}, user: user, cached: 1, advanceOnClaim: true}
	rows := [][]any{{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)}, {uint64(2), []byte("two"), make([]byte, 64), make([]byte, 32)}}
	head, err := cborx.Marshal(rows[1])
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	close(release)
	calls := &blockedMusicTrustHistory{entered: make(chan struct{}), release: release, head: head, rows: rows}
	if err = refreshMusicCallJoinLeaves(t.Context(), calls, core, group); err != nil || core.trusts != 1 {
		t.Fatalf("stale pre-claim anchor retained: err=%v trusts=%d", err, core.trusts)
	}
}

func TestMusicJoinBusyRetryIsBoundedAndCancelable(t *testing.T) {
	for _, cancelOnWait := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelOnWait), func(t *testing.T) {
			calls := &claimRetryMusicCalls{musicJoinCallsFake: musicJoinCallsFake{channel: id.New(), group: id.New()}}
			core := &sharedMusicTrustCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}, user: id.New(), cached: 1, busyClaims: 100}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waits := 0
			err := retryMusicCallGroupJoin(ctx, calls, core, id.New(), id.New(), calls.channel, 1, func(context.Context, time.Duration) error {
				waits++
				if cancelOnWait {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			if cancelOnWait {
				if !errors.Is(err, context.Canceled) || waits != 1 {
					t.Fatalf("cancellation ignored: err=%v waits=%d", err, waits)
				}
			} else if !errors.Is(err, service.ErrSourceTrustBusy) || waits != 3 || core.joins != 4 {
				t.Fatalf("busy retry unbounded: err=%v waits=%d attempts=%d", err, waits, core.joins)
			}
		})
	}
}
