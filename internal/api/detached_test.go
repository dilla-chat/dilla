package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// cancellingDS is the moderator's client going away mid-request: its first Remove cancels the
// request's context, and from then on it answers every call on a done context the way the real
// delivery service does, with the context's error.
type cancellingDS struct {
	recordingDS
	cancel context.CancelFunc
	once   sync.Once
}

func (d *cancellingDS) ProposeRemove(ctx context.Context, g id.ID, leaf uint32, a id.ID) error {
	d.once.Do(d.cancel)
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.recordingDS.ProposeRemove(ctx, g, leaf, a)
}

func (d *cancellingDS) ProposeRemoveOf(ctx context.Context, g id.ID, leaf uint32, dev, a id.ID) error {
	d.once.Do(d.cancel)
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.recordingDS.ProposeRemoveOf(ctx, g, leaf, dev, a)
}

func (d *cancellingDS) ProposeRemoveOfMember(ctx context.Context, g id.ID, leaf uint32, dev id.ID, added uint64, a id.ID) error {
	d.once.Do(d.cancel)
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.recordingDS.ProposeRemoveOfMember(ctx, g, leaf, dev, added, a)
}

func (d *cancellingDS) ProposeRemoveDevice(ctx context.Context, g, dev, a id.ID) error {
	d.once.Do(d.cancel)
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.recordingDS.ProposeRemoveDevice(ctx, g, dev, a)
}

func (d *cancellingDS) VoidIneligibleAdds(ctx context.Context, g id.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.recordingDS.VoidIneligibleAdds(ctx, g)
}

// serveCancellable drives one request straight into a mux the test builds around dsvc, on a
// context dsvc can cancel, as the same session the env's token authenticates.
func serveCancellable(t *testing.T, e *env, dsvc *cancellingDS, register func(*server.Mux),
	method, path, tok string, body any) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dsvc.cancel = cancel
	mux := server.NewMux()
	register(mux)
	var raw io.Reader = http.NoBody
	if body != nil {
		b, err := cborx.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		raw = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(auth.WithSession(ctx, e.sess[tok]), method, path, raw)
	if body != nil {
		req.Header.Set("Content-Type", "application/cbor")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

// C3 (fix wave): a kick, ban or leave commits first and issues the delivery-service Removes
// afterwards. Those Removes must not run on the request's context: a moderator whose client
// disconnects inside the first Remove would otherwise leave every later group with no Remove,
// and the removed user keeps a current leaf, decrypting and sending, in each of them. The
// user's channel_members rows for the community go in the same transaction as the membership.
func TestRemovalsSurviveADisconnectedClient(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		name    string
		act     func(t *testing.T, e *env, dsvc *cancellingDS, cid, target id.ID, ownerTok, targetTok string) int
		wantHTS int
	}{
		{"ban", func(t *testing.T, e *env, dsvc *cancellingDS, cid, target id.ID, ownerTok, _ string) int {
			return serveCancellable(t, e, dsvc, func(m *server.Mux) { api.NewBans(e.Repo, dsvc, e.Clk, log).Register(m) },
				http.MethodPut, "/v1/communities/"+cid.String()+"/bans/"+target.String(), ownerTok, []any{"spam", nil})
		}, http.StatusNoContent},
		{"kick", func(t *testing.T, e *env, dsvc *cancellingDS, cid, target id.ID, ownerTok, _ string) int {
			return serveCancellable(t, e, dsvc, func(m *server.Mux) { api.NewCommunities(e.Repo, dsvc, e.Clk, log).Register(m) },
				http.MethodDelete, "/v1/communities/"+cid.String()+"/members/"+target.String(), ownerTok, nil)
		}, http.StatusNoContent},
		{"leave", func(t *testing.T, e *env, dsvc *cancellingDS, cid, _ id.ID, _, targetTok string) int {
			return serveCancellable(t, e, dsvc, func(m *server.Mux) { api.NewCommunities(e.Repo, dsvc, e.Clk, log).Register(m) },
				http.MethodPost, "/v1/communities/"+cid.String()+"/leave", targetTok, []any{})
		}, http.StatusNoContent},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, cid, ownerTok := channelEnv(t)
			target, targetTok := e.NewUser("target")
			// The readable channel's channel_members is its live audience, which the join writes.
			readable, _, status := newChannel(t, e, cid, ownerTok, 0, 1, 2, "open")
			if status != http.StatusCreated {
				t.Fatalf("readable channel = %d", status)
			}
			joinCommunity(t, e, cid, targetTok)
			if members, _ := e.Repo.ListChannelMembers(t.Context(), readable); !slices.Contains(members, target) {
				t.Fatalf("the join did not add the target to the readable channel's audience: %x", members)
			}
			var groups []id.ID
			for i := range 4 {
				ch, _, status := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret"+string(rune('a'+i)))
				if status != http.StatusCreated {
					t.Fatalf("channel %d = %d", i, status)
				}
				g := seedTextGroup(t, e, ch, cid)
				seedMember(t, e, g, target, 3)
				groups = append(groups, g)
			}

			dsvc := &cancellingDS{}
			if got := c.act(t, e, dsvc, cid, target, ownerTok, targetTok); got != c.wantHTS {
				t.Fatalf("%s = %d, want %d", c.name, got, c.wantHTS)
			}
			removed := dsvc.removes()
			for _, g := range groups {
				if !slices.ContainsFunc(removed, func(r struct {
					Group id.ID
					Leaf  uint32
				}) bool {
					return r.Group == g && r.Leaf == 3
				}) {
					t.Fatalf("groups=%d removes issued=%d (%+v): group %s got no Remove after the client went away",
						len(groups), len(removed), removed, g)
				}
			}
			if members, _ := e.Repo.ListChannelMembers(t.Context(), readable); slices.Contains(members, target) {
				t.Fatalf("after the %s the target is still in the readable channel's audience", c.name)
			}
		})
	}
}
