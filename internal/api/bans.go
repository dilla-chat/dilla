package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bans serves the three ban routes of interfaces.md §5.2 (protocol/09 § Bans):
// ban a user from a community, lift a ban, and list a community's bans. A ban
// removes the membership row and, after that commits, the user's leaves from
// every text and call group of the community's channels through delivery-service
// Removes, and it gates every later join until it is lifted or lapses.
//
// Register mounts the handlers bare, as Communities does; each handler requires
// an enrolled session itself, so a mis-mounted route fails closed.
type Bans struct {
	repo store.Repository
	dsvc DS
	clk  clock.Clock
	log  *slog.Logger
	// calls cuts a banned user's connected call sessions after the commit; nil touches no call.
	calls *Calls
	// notify tells the members, and once the banned member, after the commit (op 54); nil sends nothing.
	notify *StructureNotifier
}

// WithNotifier sets where a ban is announced to the members and to the user it removed, and returns b.
func (b *Bans) WithNotifier(n *StructureNotifier) *Bans {
	b.notify = n
	return b
}

func NewBans(repo store.Repository, dsvc DS, clk clock.Clock, log *slog.Logger) *Bans {
	return &Bans{repo: repo, dsvc: dsvc, clk: clk, log: log}
}

// WithCalls sets the call routes a ban cuts the user's live call sessions through, and returns b.
func (b *Bans) WithCalls(calls *Calls) *Bans {
	b.calls = calls
	return b
}

func (b *Bans) Register(mux *server.Mux) {
	mux.HandleFunc("PUT /v1/communities/{id}/bans/{user_id}", b.put)
	mux.HandleFunc("DELETE /v1/communities/{id}/bans/{user_id}", b.delete)
	mux.HandleFunc("GET /v1/communities/{id}/bans", b.list)
}

type putBanReq struct {
	_       struct{} `cbor:",toarray"`
	Reason  string
	Expires *uint64
}

// maxBanReasonBytes bounds the one free-text field of a ban.
const maxBanReasonBytes = 512

// maxBanSeconds bounds how far in the future a ban's expiry may lie: a century,
// the bound min_account_age_seconds carries, so the unix second fits an int64
// with room to spare on both engines.
const maxBanSeconds = maxMinAccountAgeSeconds

func (b *Bans) put(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, b.repo, PermBanMembers)
	if err != nil {
		b.fail(w, r, "ban", err)
		return
	}
	var req putBanReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := validChannelText("reason", req.Reason, 0, maxBanReasonBytes, true); err != nil {
		server.WriteError(w, err)
		return
	}
	now := b.clk.Now().Unix()
	var expires *int64
	if req.Expires != nil {
		//nolint:gosec // G115: now is a unix second of the instance clock, far below 2^63 - maxBanSeconds
		if *req.Expires <= uint64(now) || *req.Expires > uint64(now)+maxBanSeconds {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
				"expires must be in the future and at most %d seconds away", uint64(maxBanSeconds)))
			return
		}
		v := int64(*req.Expires) //nolint:gosec // G115: bounded above by now + maxBanSeconds
		expires = &v
	}
	// The target must be an account of this instance; a ban may precede a join,
	// so membership is not required.
	if _, err := b.repo.GetUser(r.Context(), m.target); err != nil {
		b.fail(w, r, "ban", notFound(err))
		return
	}
	// A moderator may not ban someone at or above their own highest role, and
	// nobody may ban the owner.
	if err := outranks(r.Context(), b.repo, m.snap, m.session.UserID, m.target, m.community); err != nil {
		b.fail(w, r, "ban", err)
		return
	}
	// The rows first, in one transaction under the community row lock the join
	// takes too, so a join is either wholly before the ban (and its member row
	// is deleted here) or wholly after it (and its gate reads the ban) …
	// removed is whether a membership row was deleted: only a member is told they were removed.
	removed := false
	if err := b.repo.Tx(r.Context(), func(tx store.Repository) error {
		removed = false
		if err := tx.LockCommunity(r.Context(), m.community); err != nil {
			return notFound(err)
		}
		if err := stillModerates(r.Context(), tx, m, PermBanMembers); err != nil {
			return err
		}
		if err := requireCommunityWire(r.Context(), tx, m.session.WireVersion, &m.community); err != nil {
			return err
		}
		if err := tx.PutBan(r.Context(), store.BanRow{
			CommunityID: m.community, UserID: m.target, Reason: req.Reason,
			ByUser: m.session.UserID, Created: now, Expires: expires,
		}); err != nil {
			return err
		}
		member, memberErr := tx.GetMember(r.Context(), m.community, m.target)
		if memberErr == nil {
			if err := appendMemberEpisodeEvent(r.Context(), tx, protocol.EventMemberLeave, m.community, m.target, member.EpisodeID, now); err != nil {
				return err
			}
			if err := tx.DeleteMember(r.Context(), m.community, m.target); err != nil {
				return err
			}
			removed = true
		} else if !errors.Is(memberErr, store.ErrNotFound) {
			return memberErr
		}
		// The user's own channel overwrites and channel_members rows go with the
		// membership: an overwrite must not reopen a channel on a later rejoin, and
		// no channel may still list a banned user (the readable audience, GET
		// channel members, the sync).
		if _, err := tx.DeleteUserOverwrites(r.Context(), m.community, m.target); err != nil {
			return err
		}
		if _, err := tx.DeleteCommunityChannelMembers(r.Context(), m.community, m.target); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &m.session.UserID, Action: "ban.create", Target: m.target.String(),
			Detail: m.community.String() + " " + req.Reason, At: now,
		})
	}); err != nil {
		b.fail(w, r, "ban", err)
		return
	}
	// … and the delivery service AFTER the commit. See the comment on
	// RemoveUserFromCommunityGroups: the delivery service writes through the same
	// single-connection write pool, so calling it inside the transaction above is
	// a self-deadlock no timeout can break.
	//
	// A failure here is logged, not answered: the ban stands and gates every
	// join. A Remove that was not issued leaves the user's leaf in a group whose
	// Add and join ACL (ResolverACL) already refuses them.
	//
	// It runs on afterCommit's context: the ban has landed, and a moderator whose
	// client goes away must not leave the banned user a leaf in the groups the
	// loop had not reached yet.
	ctx := afterCommit(r)
	if err := RemoveUserFromCommunityGroups(ctx, b.repo, b.dsvc, m.community, m.target); err != nil {
		b.log.ErrorContext(ctx, "remove banned user from groups",
			"community", m.community, "user", m.target, "err", err)
	}
	syncAfterMembership(ctx, b.calls, m.community, m.target, nil)
	// A ban of someone who was not a member tells them nothing: they may never have known the
	// community.
	if removed {
		b.notify.Removed(ctx, m.community, m.target)
	}
	b.notify.Changed(ctx, m.community)
	w.WriteHeader(http.StatusNoContent)
}

// delete lifts a ban. It issues no proposals: an unbanned user is not re-added
// to anything, they join again.
func (b *Bans) delete(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, b.repo, PermBanMembers)
	if err != nil {
		b.fail(w, r, "lift ban", err)
		return
	}
	now := b.clk.Now().Unix()
	if err := b.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), m.community); err != nil {
			return notFound(err)
		}
		if err := stillModerates(r.Context(), tx, m, PermBanMembers); err != nil {
			return err
		}
		if _, err := tx.GetBan(r.Context(), m.community, m.target); err != nil {
			return notFound(err)
		}
		if err := requireCommunityWire(r.Context(), tx, m.session.WireVersion, &m.community); err != nil {
			return err
		}
		if err := tx.DeleteBan(r.Context(), m.community, m.target); err != nil {
			return notFound(err)
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &m.session.UserID, Action: "ban.delete", Target: m.target.String(),
			Detail: m.community.String(), At: now,
		})
	}); err != nil {
		b.fail(w, r, "lift ban", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type banResp struct {
	_       struct{} `cbor:",toarray"`
	UserID  id.ID
	Reason  string
	ByUser  id.ID
	Created int64
	Expires *int64
	// Username and Display are "" when the account no longer exists.
	Username string
	Display  string
}

// list answers [[user_id, reason, by_user, created, expires|null, username,
// display]], newest first. A lapsed ban is listed: the row is the history,
// BanStands is the gate.
func (b *Bans) list(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, b.repo, PermBanMembers)
	if err != nil {
		b.fail(w, r, "list bans", err)
		return
	}
	rows, err := b.repo.ListBans(r.Context(), m.community)
	if err != nil {
		b.fail(w, r, "list bans", err)
		return
	}
	out := make([]banResp, 0, len(rows))
	for _, row := range rows {
		var username, display string
		switch u, err := b.repo.GetUser(r.Context(), row.UserID); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			b.fail(w, r, "list bans", err)
			return
		default:
			username, display = u.Username, u.Display
		}
		out = append(out, banResp{
			UserID: row.UserID, Reason: row.Reason, ByUser: row.ByUser,
			Created: row.Created, Expires: row.Expires,
			Username: username, Display: display,
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		b.log.Error("encode bans", "err", err)
	}
}

// fail writes err as the client's refusal; anything that is not a
// *server.Error is logged and reaches the client as an empty E_INTERNAL.
func (b *Bans) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		b.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}

// moderation is one resolved moderation request: the caller's session, the
// community {id}, the target {user_id} (zero on a route without one) and the
// caller's permission snapshot.
type moderation struct {
	session   auth.Session
	community id.ID
	target    id.ID
	snap      Snapshot
}

// moderate resolves {id} and {user_id}, requires the session's user to be a
// member of the community (a non-member gets the 404 an unknown community
// gets) and to hold want community-wide (403 otherwise).
func moderate(r *http.Request, repo store.Repository, want Bits) (moderation, error) {
	var m moderation
	s, err := enrolledSession(r)
	if err != nil {
		return m, err
	}
	m.session = s
	if m.community, err = server.PathID(r, "id"); err != nil {
		return m, err
	}
	if r.PathValue("user_id") != "" {
		if m.target, err = server.PathID(r, "user_id"); err != nil {
			return m, err
		}
	}
	if _, err := repo.GetCommunity(r.Context(), m.community); err != nil {
		return m, notFound(err)
	}
	if _, err := repo.GetMember(r.Context(), m.community, s.UserID); err != nil {
		return m, notFound(err)
	}
	if m.snap, err = LoadSnapshot(r.Context(), repo, m.community, s.UserID, nil); err != nil {
		return m, notFound(err)
	}
	if !m.snap.Resolve(s.UserID).Has(want) {
		return m, server.Errorf(server.CodeForbidden, "missing permission")
	}
	return m, nil
}

// stillModerates repeats moderate's permission check and outranks inside tx, which already holds
// the community row lock: a hand-over (POST …/owner takes the same lock) or a role change that
// committed after the checks before the transaction must not let a kick or ban remove the owner,
// or let an actor act with authority they no longer hold.
func stillModerates(ctx context.Context, tx store.Repository, m moderation, want Bits) error {
	snap, err := LoadSnapshot(ctx, tx, m.community, m.session.UserID, nil)
	if err != nil {
		return notFound(err)
	}
	if !snap.Resolve(m.session.UserID).Has(want) {
		return server.Errorf(server.CodeForbidden, "missing permission")
	}
	return outranks(ctx, tx, snap, m.session.UserID, m.target, m.community)
}

// outranks refuses a moderation action against the owner or against a user
// whose highest role is at or above the actor's; the owner outranks everyone
// else. It loads a SECOND snapshot for the target, because Snapshot.Held is one
// user's grants and Highest reads it. A target who holds no role sits below any
// actor who holds one, and an actor who holds none (a moderation bit granted to
// @everyone) outranks nobody.
func outranks(ctx context.Context, repo store.Repository, snap Snapshot, actor, target, cid id.ID) error {
	if target == snap.Owner {
		return server.Errorf(server.CodeForbidden, "the owner cannot be moderated")
	}
	if actor == snap.Owner {
		return nil
	}
	targetSnap, err := LoadSnapshot(ctx, repo, cid, target, nil)
	if err != nil {
		return notFound(err)
	}
	actorTop, actorHolds := snap.Highest(actor)
	targetTop, targetHolds := targetSnap.Highest(target)
	if !actorHolds || (targetHolds && targetTop >= actorTop) || target == actor {
		return server.Errorf(server.CodeForbidden, "the target holds a role at or above yours")
	}
	return nil
}
