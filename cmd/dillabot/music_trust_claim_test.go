//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
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
	mu         sync.Mutex
	binding    native.Binding
	user       id.ID
	cached     uint64
	active     bool
	pendingErr error
	trusts     int
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
func (c *blockedMusicTrustHistory) DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error) {
	return c.rows, nil
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
