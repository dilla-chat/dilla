package ds_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// restartCold is a whole-process restart as the gateway sees it: the harness's connections drop,
// and the delivery service AND the gateway are rebuilt over the same database, so the new
// gateway's registry starts empty. restartDS keeps h.gw, which is why no earlier test ever saw a
// cold registry (gap G5 evidence 7).
func (h *dsHarness) restartCold(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for device, c := range h.conns {
		_ = c.ws.CloseNow()
		delete(h.conns, device)
	}
	if err := h.ds.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := h.gw.Shutdown(ctx); err != nil {
		t.Fatalf("gateway Shutdown: %v", err)
	}
	botCapability := capability.New(h.repo)
	gw := gateway.New(gateway.Options{
		Clock: h.clk, Store: gatewayStore{h.repo}, Generation: 1, Auth: h.auth,
		Backoff: ds.DefaultPolicy().Backoff, BackoffJitter: ds.DefaultPolicy().BackoffJitter,
		ScopeResolver: gatewayScopeResolver{repo: h.repo}, CommunityCompatibility: botCapability,
	})
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: gw, Clock: h.clk, BotCapability: botCapability,
		Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), Channels: h.channels, ACL: h.acl,
	})
	if err != nil {
		t.Fatalf("ds.New after a cold restart: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.gw, h.ds = gw, d
}

// A group nobody commits in delivers message.ct again once the restarted instance has seeded
// its gateway: the member at leaf 0 listens, the member at leaf 1 uploads.
func TestARestartedInstanceDeliversAQuietGroupItSeeded(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	listener := h.memberSession(t, reg.GroupID, 0)
	uploader := h.memberSession(t, reg.GroupID, 1)
	h.sessions = map[id.ID]auth.Session{listener.DeviceID: listener}

	h.restartCold(t)
	if h.gw.Debug(listener.DeviceID, reg.GroupID).InMembers {
		t.Fatal("the rebuilt gateway already lists the group; the restart was not cold")
	}
	n, err := h.ds.SeedGateway(ctx)
	if err != nil {
		t.Fatalf("SeedGateway: %v", err)
	}
	if n != 1 {
		t.Fatalf("SeedGateway seeded %d groups, want the one open group", n)
	}

	h.online(listener.DeviceID)
	row, err := h.repo.GetGroup(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	out, err := h.ds.Upload(ctx, uploader, reg.GroupID, row.Epoch, privateMessage(t, 32, row.Epoch, 64), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	f := h.waitDeviceFrame(t, listener.DeviceID, "message.ct")
	var seq uint64
	if err := cborx.Unmarshal(f.payload[0], &seq); err != nil || seq != out.Seq {
		t.Fatalf("message.ct seq %d (%v), want %d", seq, err, out.Seq)
	}
	var from []byte
	if err := cborx.Unmarshal(f.payload[2], &from); err != nil || !bytes.Equal(from, uploader.DeviceID[:]) {
		t.Fatalf("message.ct uploader %x (%v), want %x", from, err, uploader.DeviceID[:])
	}
}

// The seeded list is the live member table: every live leaf with its index, a removed leaf absent.
func TestSeedGatewayMirrorsTheLiveMemberTable(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	row, err := h.repo.GetGroup(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) < 3 {
		t.Fatalf("the fixture has %d members, want at least 3", len(members))
	}
	first, removed, last := members[0], members[1], members[len(members)-1]
	h.removeLeafOfDevice(t, &dsMessageGroup{id: reg.GroupID, epoch: row.Epoch}, removed.DeviceID)

	h.restartCold(t)
	if _, err := h.ds.SeedGateway(ctx); err != nil {
		t.Fatalf("SeedGateway: %v", err)
	}
	for _, m := range []store.MemberRow{first, last} {
		got := h.gw.Debug(m.DeviceID, reg.GroupID)
		if !got.InMembers || got.Members != len(members)-1 || got.Leaf == nil || *got.Leaf != m.LeafIndex {
			t.Errorf("leaf %d: in list %v, list size %d, leaf %v; want true, %d, %d",
				m.LeafIndex, got.InMembers, got.Members, got.Leaf, len(members)-1, m.LeafIndex)
		}
	}
	if got := h.gw.Debug(removed.DeviceID, reg.GroupID); got.InMembers || got.Leaf != nil {
		t.Errorf("the removed leaf %d is seeded: in list %v, leaf %v", removed.LeafIndex, got.InMembers, got.Leaf)
	}
}

// A closed group is not seeded: it is not an open group, and nothing may be delivered in it.
func TestSeedGatewaySkipsAClosedGroup(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	member := h.memberSession(t, reg.GroupID, 0)
	if err := h.repo.CloseGroup(ctx, reg.GroupID, h.clk.Now().Unix()); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}
	h.restartCold(t)
	n, err := h.ds.SeedGateway(ctx)
	if err != nil {
		t.Fatalf("SeedGateway: %v", err)
	}
	if n != 0 {
		t.Fatalf("SeedGateway seeded %d groups, want 0: the only group is closed", n)
	}
	if got := h.gw.Debug(member.DeviceID, reg.GroupID); got.InMembers || got.Members != 0 {
		t.Fatalf("the closed group has a fan-out list of %d (member listed: %v)", got.Members, got.InMembers)
	}
}

// Start seeds before it returns, so the composition root's start-up is what reseeds a restart.
func TestStartSeedsTheGatewayBeforeItReturns(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	member := h.memberSession(t, reg.GroupID, 0)
	h.restartCold(t)
	if err := h.ds.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.gw.Debug(member.DeviceID, reg.GroupID); !got.InMembers || got.Leaf == nil || *got.Leaf != 0 {
		t.Fatalf("after Start the gateway lists the member: %v at leaf %v; want true at 0", got.InMembers, got.Leaf)
	}
}

// pagedOpenGroups is a store that answers only the two reads SeedGateway makes, over a synthetic
// instance of many open groups. The embedded Repository is nil: any other call panics, which is
// the assertion that seeding reads nothing else.
type pagedOpenGroups struct {
	store.Repository
	groups      []id.ID // ascending
	afters      []id.ID
	limits      []int32
	failList    error
	failMembers error
}

func (p *pagedOpenGroups) ListOpenGroups(_ context.Context, after id.ID, limit int32) ([]store.GroupRow, error) {
	p.afters = append(p.afters, after)
	p.limits = append(p.limits, limit)
	if p.failList != nil {
		return nil, p.failList
	}
	var out []store.GroupRow
	for _, g := range p.groups {
		if bytes.Compare(g[:], after[:]) > 0 && len(out) < int(limit) {
			out = append(out, store.GroupRow{GroupID: g})
		}
	}
	return out, nil
}

// ListMembers answers one live leaf (3, the group's own device) and one removed leaf (4, device
// 0xff…) whatever the SQL would have filtered, so SeedGateway's own RemovedEpoch rule is tested.
func (p *pagedOpenGroups) ListMembers(_ context.Context, g id.ID) ([]store.MemberRow, error) {
	if p.failMembers != nil {
		return nil, p.failMembers
	}
	gone := uint64(1)
	return []store.MemberRow{
		{GroupID: g, LeafIndex: 3, DeviceID: deviceOfGroup(g)},
		{GroupID: g, LeafIndex: 4, DeviceID: removedDevice, RemovedEpoch: &gone},
	}, nil
}

var removedDevice = id.ID{0xff}

// deviceOfGroup is a device id derived from the group id, distinct from every group id.
func deviceOfGroup(g id.ID) id.ID {
	d := g
	d[0] = 0xde
	return d
}

func syntheticGroups(n uint64) []id.ID {
	out := make([]id.ID, 0, n)
	for i := range n {
		var g id.ID
		binary.BigEndian.PutUint64(g[8:], i+1)
		out = append(out, g)
	}
	return out
}

func seedOnly(t *testing.T, p *pagedOpenGroups, withGateway bool) (*ds.DS, *gateway.Gateway) {
	t.Helper()
	var gw *gateway.Gateway
	o := ds.Options{Store: p}
	if withGateway {
		gw = gateway.New(gateway.Options{})
		t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
		o.Gateway = gw
	}
	d, err := ds.New(o)
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	return d, gw
}

// 600 open groups are walked in pages of 256 by group id, every one seeded.
func TestSeedGatewayPagesThroughEveryOpenGroup(t *testing.T) {
	p := &pagedOpenGroups{groups: syntheticGroups(600)}
	d, gw := seedOnly(t, p, true)
	n, err := d.SeedGateway(context.Background())
	if err != nil {
		t.Fatalf("SeedGateway: %v", err)
	}
	if n != 600 {
		t.Fatalf("seeded %d groups, want 600", n)
	}
	wantAfters := []id.ID{{}, p.groups[255], p.groups[511]}
	if len(p.afters) != len(wantAfters) {
		t.Fatalf("ListOpenGroups was called %d times, want 3 (pages of 256, 256, 88)", len(p.afters))
	}
	for i, want := range wantAfters {
		if p.afters[i] != want || p.limits[i] != 256 {
			t.Errorf("page %d: after %s limit %d, want after %s limit 256", i, p.afters[i], p.limits[i], want)
		}
	}
	for _, g := range []id.ID{p.groups[0], p.groups[255], p.groups[256], p.groups[599]} {
		got := gw.Debug(deviceOfGroup(g), g)
		if !got.InMembers || got.Members != 1 || got.Leaf == nil || *got.Leaf != 3 {
			t.Errorf("group %s: in list %v, size %d, leaf %v; want true, 1, 3", g, got.InMembers, got.Members, got.Leaf)
		}
		if removed := gw.Debug(removedDevice, g); removed.InMembers || removed.Leaf != nil {
			t.Errorf("group %s: the removed leaf is seeded", g)
		}
	}
}

// A store error aborts seeding and Start, and is not swallowed.
func TestASeedingStoreErrorAbortsStart(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *pagedOpenGroups
	}{
		{"listing the open groups", &pagedOpenGroups{groups: syntheticGroups(3), failList: errInjected}},
		{"reading a group's members", &pagedOpenGroups{groups: syntheticGroups(3), failMembers: errInjected}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, _ := seedOnly(t, c.p, true)
			if _, err := d.SeedGateway(context.Background()); !errors.Is(err, errInjected) {
				t.Fatalf("SeedGateway = %v, want the store's error", err)
			}
			if err := d.Start(context.Background()); !errors.Is(err, errInjected) {
				t.Fatalf("Start = %v, want the store's error", err)
			}
		})
	}
}

// Without a gateway there is nothing to seed and nothing is read.
func TestSeedGatewayWithoutAGatewayReadsNothing(t *testing.T) {
	p := &pagedOpenGroups{groups: syntheticGroups(3)}
	d, _ := seedOnly(t, p, false)
	n, err := d.SeedGateway(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("SeedGateway = %d, %v; want 0, nil", n, err)
	}
	if len(p.afters) != 0 {
		t.Fatalf("ListOpenGroups was called %d times without a gateway", len(p.afters))
	}
}
