package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// Communities serves the community routes of interfaces.md §5.2: create, read,
// patch and delete a community, list and remove its members, join and leave.
// Every body is a fixed-position CBOR array (protocol/09 § Communities).
//
// Register mounts the handlers bare: the composition root wraps the mux in the
// session middleware, and each handler still refuses a request that carries no
// enrolled session, so a mis-mounted route fails closed.
type Communities struct {
	repo store.Repository
	dsvc DS
	clk  clock.Clock
	log  *slog.Logger

	// inviteLimiter meters POST /v1/communities/{id}/join on the ("invite",
	// client address) bucket GET /i/{code} is on, so a join is not a way round
	// the limit on guessing codes. Nil meters nothing: the handler-level tests
	// build Communities without one, and the composition root always sets it.
	inviteLimiter *server.RateLimiter
	trustedProxy  []netip.Prefix

	// calls cuts a kicked or departing user's connected call sessions after the commit
	// (syncAfterMembership); nil touches no call.
	calls *Calls

	// notify tells the members, after the commit, that a PATCH, join, leave or kick changed the
	// community, the user a leave or kick removed, and a deleted community's members (op 54);
	// nil tells nobody.
	notify *StructureNotifier
}

// WithCalls sets the call routes a kick or a leave cuts the user's live call sessions through, and
// returns c. The composition root sets it whenever it runs an SFU.
func (c *Communities) WithCalls(calls *Calls) *Communities {
	c.calls = calls
	return c
}

// WithNotifier sets where a community PATCH, a membership change and a delete are announced, and
// returns c.
func (c *Communities) WithNotifier(n *StructureNotifier) *Communities {
	c.notify = n
	return c
}

// NewCommunities takes the delivery service a membership change reaches: a
// kick or a leave removes the user's leaves from the community's groups, and a
// community delete closes them (membership.go).
func NewCommunities(repo store.Repository, dsvc DS, clk clock.Clock, log *slog.Logger) *Communities {
	return &Communities{repo: repo, dsvc: dsvc, clk: clk, log: log}
}

// WithInviteMeter puts POST /v1/communities/{id}/join on the invite bucket of l,
// keyed by the client address as server.RealIP resolves it behind trusted proxy
// prefixes. It returns c for chaining.
func (c *Communities) WithInviteMeter(l *server.RateLimiter, trustedProxies []netip.Prefix) *Communities {
	c.inviteLimiter, c.trustedProxy = l, trustedProxies
	return c
}

func (c *Communities) Register(mux *server.Mux) {
	mux.HandleFunc("POST /v1/communities", c.create)
	mux.HandleFunc("GET /v1/communities", c.mine)
	mux.HandleFunc("GET /v1/communities/{id}", c.get)
	mux.HandleFunc("PATCH /v1/communities/{id}", c.patch)
	mux.HandleFunc("DELETE /v1/communities/{id}", c.delete)
	mux.HandleFunc("GET /v1/communities/{id}/members", c.members)
	mux.HandleFunc("GET /v1/communities/{id}/members/{user_id}", c.member)
	mux.HandleFunc("GET /v1/communities/{id}/permissions", c.permissions)
	mux.HandleFunc("DELETE /v1/communities/{id}/members/{user_id}", c.removeMember)
	mux.HandleFunc("POST /v1/communities/{id}/join", c.join)
	mux.HandleFunc("POST /v1/communities/{id}/leave", c.leave)
	mux.HandleFunc("POST /v1/communities/{id}/owner", c.transfer)
	mux.HandleFunc("GET /v1/communities/{id}/afk/settings", c.afkSettings)
	mux.HandleFunc("PUT /v1/communities/{id}/afk/settings", c.putAFKSettings)
}

type afkSettingsBody struct {
	_              struct{} `cbor:",toarray"`
	ChannelID      *id.ID
	TimeoutSeconds uint64
}

func (c *Communities) afkSettings(w http.ResponseWriter, r *http.Request) {
	row, _, err := c.managerOnly(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	config, err := c.repo.GetCommunityAFK(r.Context(), row.ID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, afkSettingsBody{ChannelID: config.ChannelID, TimeoutSeconds: config.TimeoutSeconds}); err != nil {
		c.log.ErrorContext(r.Context(), "encoding AFK settings failed", "err", err)
	}
}

// putAFKSettings is PUT /v1/communities/{id}/afk/settings (R2): `[timeout_seconds]`, or the web3.8
// shape `[channel_id, timeout_seconds]` when channel_id is the built-in channel. The channel is never
// chosen here: the timeout is written beside the pointer as it stands, under the community row lock.
func (c *Communities) putAFKSettings(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.managerOnly(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	var named *id.ID
	var timeout uint64
	switch len(body) {
	case 1:
		if err := cborx.Unmarshal(body[0], &timeout); err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "timeout_seconds is a uint"))
			return
		}
	case 2:
		if err := cborx.Unmarshal(body[0], &named); err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "channel_id is a 16-byte id"))
			return
		}
		if err := cborx.Unmarshal(body[1], &timeout); err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "timeout_seconds is a uint"))
			return
		}
	default:
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the AFK channel is built in; send [timeout_seconds]"))
		return
	}
	if !isAFKTimeout(timeout) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "AFK timeout must be 1, 5, 15, 30 or 60 minutes"))
		return
	}
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), row.ID); err != nil {
			return notFound(err)
		}
		cfg, err := tx.GetCommunityAFK(r.Context(), row.ID)
		if err != nil {
			return err
		}
		if len(body) == 2 && (named == nil || cfg.ChannelID == nil || *named != *cfg.ChannelID) {
			return server.Errorf(server.CodeInvalidRequest, "the AFK channel is built in; send [timeout_seconds]")
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, &row.ID); err != nil {
			return err
		}
		return tx.SetCommunityAFK(r.Context(), row.ID, store.CommunityAFK{ChannelID: cfg.ChannelID, TimeoutSeconds: timeout})
	}); err != nil {
		c.fail(w, r, "put AFK settings", err)
		return
	}
	c.notify.Changed(afterCommit(r), row.ID)
	w.WriteHeader(http.StatusNoContent)
}

// maxCommunityNameBytes bounds the one free-text field a community owner controls.
// It is the same bound protocol/04 puts on an attachment mime (255), for the same
// reason: a TEXT column with no bound is a storage-exhaustion primitive.
const maxCommunityNameBytes = 255

// maxPolicyBytes bounds the community policy document.
const maxPolicyBytes = 16 * 1024

// maxMinAccountAgeSeconds bounds min_account_age_seconds at a century. The
// column is a signed 64-bit integer on both engines, so an unbounded uint64
// from a client would wrap negative in the store adapter and turn the gate off.
const maxMinAccountAgeSeconds = 100 * 365 * 24 * 3600

// maxMembersPage is one page of GET /v1/communities/{id}/members.
const maxMembersPage = 200

// everyoneRoleAllow is the permission base every new community starts from:
// view, send, attach, react, read history, connect, speak, video, screen share
// and create invite. The @everyone role's id, not its position, is what task
// 3's resolver reads as the base.
const everyoneRoleAllow = PermViewChannel | PermSendMessages | PermAttachFiles |
	PermAddReactions | PermReadHistory | PermConnect | PermSpeak | PermVideo |
	PermScreenShare | PermCreateInvite

// DefaultEveryoneAllow is everyoneRoleAllow for a caller outside the package that writes a
// community's rows itself (the test host's `channel … community=`), so the base it seeds is the
// one POST /v1/communities writes.
const DefaultEveryoneAllow = everyoneRoleAllow

type createCommunityReq struct {
	_                    struct{} `cbor:",toarray"`
	Name                 string
	Policy               []byte
	MinAccountAgeSeconds uint64
	RequireMod2FA        uint64
}

type createCommunityResp struct {
	_             struct{} `cbor:",toarray"`
	CommunityID   id.ID
	RoleEveryone  id.ID
	PolicyVersion uint64
}

func (c *Communities) create(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req createCommunityReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := validCommunityName(req.Name); err != nil {
		server.WriteError(w, err)
		return
	}
	twofa, err := requireMod2FAFlag(req.RequireMod2FA)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := validMinAccountAge(req.MinAccountAgeSeconds); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(req.Policy) == 0 {
		req.Policy = []byte("{}")
	}
	if _, err := ParseCommunityPolicy(req.Policy); err != nil {
		server.WriteError(w, err)
		return
	}
	nick, err := memberNick("")
	if err != nil {
		server.WriteError(w, err)
		return
	}

	now := c.clk.Now().Unix()
	out := createCommunityResp{CommunityID: id.New(), RoleEveryone: id.New(), PolicyVersion: 1}
	communityRow := store.CommunityRow{
		ID:                   out.CommunityID,
		Owner:                s.UserID,
		Name:                 req.Name,
		PolicyJSON:           req.Policy,
		PolicyVersion:        1,
		MinAccountAgeSeconds: req.MinAccountAgeSeconds,
		RequireMod2FA:        twofa,
		Created:              now,
	}
	var afk store.ChannelRow
	err = c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.CreateCommunity(r.Context(), communityRow); err != nil {
			return err
		}
		episode := id.New()
		if err := tx.PutMember(r.Context(), store.MemberOfCommunityRow{
			CommunityID: out.CommunityID, UserID: s.UserID, EpisodeID: episode, Joined: now, Nick: nick,
		}); err != nil {
			return err
		}
		if err := appendMemberEpisodeEvent(r.Context(), tx, protocol.EventMemberJoin, out.CommunityID, s.UserID, episode, now); err != nil {
			return err
		}
		if err := tx.PutRole(r.Context(), store.RoleRow{
			ID: out.RoleEveryone, CommunityID: out.CommunityID, Name: "@everyone",
			// store.RoleRow.Allow is uint64 (§4.2: roles.allow is a counter);
			// everyoneRoleAllow is api.Bits, a distinct named type.
			Position: 0, Allow: uint64(everyoneRoleAllow), Created: now,
		}); err != nil {
			return err
		}
		// R1: every community is born with its built-in AFK channel.
		var err error
		if afk, err = createBuiltinAFK(r.Context(), tx, communityRow, afkDefaultTimeout, now); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "community.create",
			Target: out.CommunityID.String(), Detail: req.Name, At: now,
		})
	})
	if err != nil {
		c.fail(w, r, "create community", err)
		return
	}
	if err := MaterialiseChannelMembers(afterCommit(r), c.repo, c.dsvc, afk, now); err != nil {
		c.log.ErrorContext(r.Context(), "materialise channel members", "channel", afk.ID, "err", err)
	}
	if err := server.EncodeBody(w, http.StatusCreated, out); err != nil {
		c.log.Error("encode create community", "err", err)
	}
}

type communityResp struct {
	_                    struct{} `cbor:",toarray"`
	CommunityID          id.ID
	Owner                id.ID
	Name                 string
	Policy               []byte
	PolicyVersion        uint64
	MinAccountAgeSeconds uint64
	RequireMod2FA        uint64
	Created              int64
}

func (c *Communities) get(w http.ResponseWriter, r *http.Request) {
	row, _, err := c.memberOnly(r)
	if err != nil {
		c.fail(w, r, "get community", err)
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, communityResp{
		CommunityID: row.ID, Owner: row.Owner, Name: row.Name, Policy: row.PolicyJSON,
		PolicyVersion: row.PolicyVersion, MinAccountAgeSeconds: row.MinAccountAgeSeconds,
		RequireMod2FA: uint64(row.RequireMod2FA), Created: row.Created,
	}); err != nil {
		c.log.Error("encode community", "err", err)
	}
}

type patchCommunityReq struct {
	_                    struct{} `cbor:",toarray"`
	Name                 *string
	Policy               []byte
	MinAccountAgeSeconds *uint64
	RequireMod2FA        *uint64
}

func (c *Communities) patch(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.managerOnly(r)
	if err != nil {
		c.fail(w, r, "patch community", err)
		return
	}
	var req patchCommunityReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	// Every field is validated before the transaction opens, so a refused
	// PATCH changes nothing.
	var changed []string
	if req.Name != nil {
		if err := validCommunityName(*req.Name); err != nil {
			server.WriteError(w, err)
			return
		}
		changed = append(changed, "name")
	}
	if len(req.Policy) > 0 {
		if _, err := ParseCommunityPolicy(req.Policy); err != nil {
			server.WriteError(w, err)
			return
		}
		changed = append(changed, "policy")
	}
	if req.MinAccountAgeSeconds != nil {
		if err := validMinAccountAge(*req.MinAccountAgeSeconds); err != nil {
			server.WriteError(w, err)
			return
		}
		changed = append(changed, "min_account_age_seconds")
	}
	var twofa *uint8
	if req.RequireMod2FA != nil {
		v, err := requireMod2FAFlag(*req.RequireMod2FA)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		twofa = &v
		changed = append(changed, "require_mod_2fa")
	}

	version := row.PolicyVersion
	if len(changed) > 0 {
		if err := requireCommunityWire(r.Context(), c.repo, s.WireVersion, &row.ID); err != nil {
			server.WriteError(w, err)
			return
		}
		err = c.repo.Tx(r.Context(), func(tx store.Repository) error {
			// Re-read inside the transaction: the row memberOnly returned is a
			// snapshot from before it, and the policy version must follow the
			// stored one, not that snapshot.
			cur, err := tx.GetCommunity(r.Context(), row.ID)
			if err != nil {
				return notFound(err)
			}
			version = cur.PolicyVersion
			if len(req.Policy) > 0 {
				version = cur.PolicyVersion + 1
				//nolint:gosec // G115: policy_version starts at 1 and moves by one per PATCH; it cannot reach 2^63
				if err := tx.UpdateCommunityPolicy(r.Context(), cur.ID, req.Policy, int64(version)); err != nil {
					return err
				}
			}
			if req.Name != nil || req.MinAccountAgeSeconds != nil || twofa != nil {
				name, age, flag := cur.Name, cur.MinAccountAgeSeconds, cur.RequireMod2FA
				if req.Name != nil {
					name = *req.Name
				}
				if req.MinAccountAgeSeconds != nil {
					age = *req.MinAccountAgeSeconds
				}
				if twofa != nil {
					flag = *twofa
				}
				if err := tx.UpdateCommunityMeta(r.Context(), cur.ID, name, age, flag); err != nil {
					return err
				}
			}
			return tx.Audit(r.Context(), store.AuditRow{
				Actor: &s.UserID, Action: "community.patch", Target: cur.ID.String(),
				Detail: strings.Join(changed, ","), At: c.clk.Now().Unix(),
			})
		})
		if err != nil {
			c.fail(w, r, "patch community", err)
			return
		}
		c.notify.Changed(afterCommit(r), row.ID)
	}
	if err := server.EncodeBody(w, http.StatusOK, []uint64{version}); err != nil {
		c.log.Error("encode patch community", "err", err)
	}
}

type joinReq struct {
	_ struct{} `cbor:",toarray"`
	// Invite is a community invite code as a person typed it, or null. An
	// invite-only community (policy join = "invite") admits a join only with a
	// live invite of its own; an open one spends the invite when it is given.
	Invite *string
}

// errInviteInvalid is the one refusal for an invite that is expired, exhausted,
// revoked, unknown or another community's: 410 E_INVITE_INVALID.
func errInviteInvalid(detail string) error {
	return server.Errorf(server.CodeInviteInvalid, "%s", detail)
}

func (c *Communities) join(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if c.inviteLimiter != nil {
		key := server.RateKey(server.RealIP(r, c.trustedProxy))
		if ok, wait := c.inviteLimiter.Allow(server.Class{
			Name:      classInvite,
			PerSecond: c.inviteLimiter.Config().InvitePerSecond,
			Burst:     c.inviteLimiter.Config().InviteBurst,
		}, key); !ok {
			server.WriteError(w, server.RateLimitedAfter(wait))
			return
		}
	}
	var req joinReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	// A malformed code is refused before the transaction opens.
	var hashes [][]byte
	if req.Invite != nil {
		var herr error
		if hashes, herr = auth.MatchInviteCode(*req.Invite); herr != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "malformed invite"))
			return
		}
	}
	nick, err := memberNick("")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// The gate and the membership write are one transaction under the community
	// row lock Bans.put also takes. Checked and written apart, a ban committing
	// between the two would find no member to delete and the join would then
	// write one: a banned member of the community, whom the Add and join ACL
	// treats as eligible.
	joined := false
	now := c.clk.Now().Unix()
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		row, err := tx.GetCommunity(r.Context(), cid)
		if err != nil {
			return notFound(err)
		}
		// Joining is idempotent: a member who joins again keeps the row, and
		// with it the nick and the join time, rather than being rewritten by the
		// upsert.
		switch _, err := tx.GetMember(r.Context(), cid, s.UserID); {
		case err == nil:
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if err := c.joinGate(r.Context(), tx, row, s.UserID); err != nil {
			return err
		}
		// The invite is spent AFTER the gate and in the same transaction as the
		// member row: a join the gate refuses burns no use, and a use is never
		// spent for a member row that was not written.
		if err := c.admitByInvite(r.Context(), tx, row, s.UserID, hashes); err != nil {
			return err
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, &row.ID); err != nil {
			return err
		}
		episode := id.New()
		if err := tx.PutMember(r.Context(), store.MemberOfCommunityRow{
			CommunityID: cid, UserID: s.UserID, EpisodeID: episode, Joined: now, Nick: nick,
		}); err != nil {
			return err
		}
		if err := appendMemberEpisodeEvent(r.Context(), tx, protocol.EventMemberJoin, cid, s.UserID, episode, now); err != nil {
			return err
		}
		joined = true
		return nil
	}); err != nil {
		c.fail(w, r, "join", err)
		return
	}
	// A new member enters channel_members of every group-less channel they may
	// view (materialiseJoiner), so a server-readable channel's live
	// message.plain reaches a member who joined after the channel existed.
	// After the commit, like every materialiser; a failure is logged, not
	// answered: the membership stands, and the next change to a channel
	// re-derives it.
	if joined {
		if err := materialiseJoiner(afterCommit(r), c.repo, cid, s.UserID, now); err != nil {
			c.log.ErrorContext(r.Context(), "materialise joiner", "community", cid, "user", s.UserID, "err", err)
		}
		c.notify.Changed(afterCommit(r), cid)
	}
	if err := server.EncodeBody(w, http.StatusOK, []id.ID{cid}); err != nil {
		c.log.Error("encode join", "err", err)
	}
}

// admitByInvite is the invite half of a join. A community whose policy says
// join = "invite" needs a live invite of its own (403 without one); an invite
// given to an open community is spent all the same. hashes are the candidate
// hashes of the typed code, nil when the request carried none.
//
// It resolves WHICH candidate exists before redeeming it. RedeemInvite is one
// guarded UPDATE that answers ErrExhausted whenever RowsAffected != 1, which is
// also what an unknown hash produces, so treating ErrExhausted as final would
// answer 410 for the first non-existent candidate of a legitimate code. A typed
// 1 is an I or an L, so a code with one has two candidates and one of them does
// not exist: that is the common case, not the corner one.
//
// The invite's community is checked BEFORE the redemption, so an invite minted
// for another community burns no use here.
//
// A person who registered with this very invite (POST /v1/accounts) already
// spent its use then, and the instance remembered it (invite_signups). Their
// first join with it takes that record instead of a second use, so a new person
// costs an invite one use and "10 uses" means ten people. The record is taken
// in the join's transaction, so the free join happens once; a revoked or
// expired invite still admits nobody. Anyone else, and the sign-up itself once
// its record is taken, redeems as before.
func (c *Communities) admitByInvite(ctx context.Context, repo store.Repository, row store.CommunityRow, userID id.ID, hashes [][]byte) error {
	policy, err := ParseCommunityPolicy(row.PolicyJSON)
	if err != nil {
		return err
	}
	if hashes == nil {
		if policy.InviteOnly() {
			return server.Errorf(server.CodeForbidden, "this community is invite only")
		}
		return nil
	}
	for _, h := range hashes {
		inv, err := repo.GetInviteByHash(ctx, h)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if inv.CommunityID == nil || *inv.CommunityID != row.ID {
			return errInviteInvalid("that invite is for another community")
		}
		now := c.clk.Now().Unix()
		switch err := repo.TakeInviteSignup(ctx, userID, inv.ID); {
		case err == nil:
			if inv.RevokedAt != nil || inv.ExpiresAt <= now { // a never-expiring invite stores the largest second
				return errInviteInvalid("the invite is expired, exhausted or revoked")
			}
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		// The row exists, so a refusal now is real: expired, exhausted or revoked.
		if _, err := repo.RedeemInvite(ctx, inv.CodeHash, now); err != nil {
			if errors.Is(err, store.ErrExhausted) || errors.Is(err, store.ErrNotFound) {
				return errInviteInvalid("the invite is expired, exhausted or revoked")
			}
			return err
		}
		return nil
	}
	return errInviteInvalid("the invite is expired, exhausted or revoked")
}

// joinGate enforces R17's stored gates: the account must not be banned from the
// community (a ban whose expiry has passed no longer gates), must be live, and
// at least min_account_age_seconds old. The screening flag is stored and
// served, never enforced (R17); invite-only communities are admitByInvite's.
// repo is the join's transaction, holding the
// community row lock, so the ban read here cannot go stale before the insert.
func (c *Communities) joinGate(ctx context.Context, repo store.Repository, row store.CommunityRow, userID id.ID) error {
	if ban, err := repo.GetBan(ctx, row.ID, userID); err == nil {
		if BanStands(ban, c.clk.Now().Unix()) {
			return server.Errorf(server.CodeForbidden, "banned from this community")
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	u, err := repo.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.DisabledAt != nil || u.DeletedAt != nil {
		return server.Errorf(server.CodeForbidden, "account disabled")
	}
	if row.MinAccountAgeSeconds > 0 {
		age := c.clk.Now().Unix() - u.Created
		// MinAccountAgeSeconds is bounded by maxMinAccountAgeSeconds on the way
		// in, so it fits an int64.
		if age < int64(min(row.MinAccountAgeSeconds, maxMinAccountAgeSeconds)) {
			return server.Errorf(server.CodeForbidden,
				"account must be at least %d seconds old", row.MinAccountAgeSeconds)
		}
	}
	return nil
}

// memberOnly resolves {id} and requires the session's user to be a member.
func (c *Communities) memberOnly(r *http.Request) (store.CommunityRow, auth.Session, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return store.CommunityRow{}, s, err
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		return store.CommunityRow{}, s, err
	}
	row, err := c.repo.GetCommunity(r.Context(), cid)
	if err != nil {
		return store.CommunityRow{}, s, notFound(err)
	}
	if _, err := c.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return store.CommunityRow{}, s, err
		}
		// A non-member must not learn that the community exists.
		return store.CommunityRow{}, s, server.Errorf(server.CodeNotFound, "no such object")
	}
	return row, s, nil
}

// managerOnly is memberOnly plus PermManageCommunity, which the owner always
// holds. It gates PATCH; deleting the community stays the owner's (ownerOnly).
func (c *Communities) managerOnly(r *http.Request) (store.CommunityRow, auth.Session, error) {
	row, s, err := c.memberOnly(r)
	if err != nil {
		return row, s, err
	}
	snap, err := LoadSnapshot(r.Context(), c.repo, row.ID, s.UserID, nil)
	if err != nil {
		return row, s, notFound(err)
	}
	if !snap.Resolve(s.UserID).Has(PermManageCommunity) {
		return row, s, server.Errorf(server.CodeForbidden, "manage community")
	}
	return row, s, nil
}

// ownerOnly is memberOnly plus ownership: deleting the community.
func (c *Communities) ownerOnly(r *http.Request) (store.CommunityRow, auth.Session, error) {
	row, s, err := c.memberOnly(r)
	if err != nil {
		return row, s, err
	}
	if row.Owner != s.UserID {
		return row, s, server.Errorf(server.CodeForbidden, "owner only")
	}
	return row, s, nil
}

type memberResp struct {
	_      struct{} `cbor:",toarray"`
	UserID id.ID
	Joined int64
	Nick   string
	Roles  []id.ID
	// Username, Display and Kind are L-HTTP-03's names (dilla-web-1 task 9): "", "", 0 when the user row is gone.
	Username string
	Display  string
	Kind     uint64
	// StatusMsg and Bio are the account's profile text (EOD E6); "" when the user row is gone.
	StatusMsg string
	Bio       string
}

func (c *Communities) members(w http.ResponseWriter, r *http.Request) {
	row, _, err := c.memberOnly(r)
	if err != nil {
		c.fail(w, r, "list members", err)
		return
	}
	var after id.ID
	if raw := r.URL.Query().Get("after"); raw != "" {
		if after, err = id.Parse(raw); err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "after: %v", err))
			return
		}
	}
	rows, err := c.repo.ListMembersOfCommunity(r.Context(), row.ID, after, maxMembersPage)
	if err != nil {
		c.fail(w, r, "list members", err)
		return
	}
	out := make([]memberResp, 0, len(rows))
	for _, m := range rows {
		roles, err := c.repo.ListMemberRoles(r.Context(), row.ID, m.UserID)
		if err != nil {
			c.fail(w, r, "list members", err)
			return
		}
		if roles == nil {
			roles = []id.ID{} // an array on the wire, never null
		}
		var username, display, statusMsg, bio string
		var kind uint64
		u, err := c.repo.GetUser(r.Context(), m.UserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			c.fail(w, r, "list members", err)
			return
		default:
			username, display, kind = u.Username, u.Display, uint64(u.Kind)
			statusMsg, bio = u.StatusMsg, u.Bio
		}
		out = append(out, memberResp{UserID: m.UserID, Joined: m.Joined, Nick: m.Nick, Roles: roles,
			Username: username, Display: display, Kind: kind, StatusMsg: statusMsg, Bio: bio})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		c.log.Error("encode members", "err", err)
	}
}

type currentMemberResp struct {
	_        struct{} `cbor:",toarray"`
	UserID   id.ID
	Nick     string
	Username string
	Display  string
}

// member returns only a current membership. The caller must also be a current
// community member, so absent targets and non-member callers cannot inspect a
// user's profile across communities.
func (c *Communities) member(w http.ResponseWriter, r *http.Request) {
	row, _, err := c.memberOnly(r)
	if err != nil {
		c.fail(w, r, "get member", err)
		return
	}
	uid, err := server.PathID(r, "user_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	m, err := c.repo.GetMember(r.Context(), row.ID, uid)
	if errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if err != nil {
		c.fail(w, r, "get member", err)
		return
	}
	u, err := c.repo.GetUser(r.Context(), uid)
	if errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "member profile temporarily unavailable"))
		return
	} else if err != nil {
		c.fail(w, r, "get member", err)
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, currentMemberResp{UserID: uid, Nick: m.Nick, Username: u.Username, Display: u.Display}); err != nil {
		c.log.Error("encode member", "err", err)
	}
}

// ownerTop is the Top a caller who owns the community reads: above every role
// position (2^31), so a client compares it with a target's position unchanged.
const ownerTop uint64 = 1 << 31

type permissionsResp struct {
	_    struct{} `cbor:",toarray"`
	Bits uint64
	Top  uint64
}

// permissions answers the caller's own community-wide permissions (no channel
// overwrite) and the position of their highest role: 0 for @everyone alone and
// ownerTop for the owner. A member only; anyone else gets 404.
func (c *Communities) permissions(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.memberOnly(r)
	if err != nil {
		c.fail(w, r, "read permissions", err)
		return
	}
	snap, err := LoadSnapshot(r.Context(), c.repo, row.ID, s.UserID, nil)
	if err != nil {
		c.fail(w, r, "read permissions", notFound(err))
		return
	}
	resp := permissionsResp{Bits: uint64(snap.Resolve(s.UserID))}
	if s.UserID == snap.Owner {
		resp.Top = ownerTop
	} else if top, held := snap.Highest(s.UserID); held {
		resp.Top = top
	}
	if err := server.EncodeBody(w, http.StatusOK, resp); err != nil {
		c.log.Error("encode permissions", "err", err)
	}
}

// removeMember is the kick: it needs kick_members community-wide and a target
// strictly below the caller (the pair Bans uses), deletes the membership row,
// and then removes the user's leaves from the community's groups. It writes no
// ban row, so a kicked user may join again at once.
func (c *Communities) removeMember(w http.ResponseWriter, r *http.Request) {
	m, err := moderate(r, c.repo, PermKickMembers)
	if err != nil {
		c.fail(w, r, "kick", err)
		return
	}
	// A target who is not a member answers 404, before the rank check, so the
	// answer does not depend on who the caller is.
	if _, err := c.repo.GetMember(r.Context(), m.community, m.target); err != nil {
		c.fail(w, r, "kick", notFound(err))
		return
	}
	if err := outranks(r.Context(), c.repo, m.snap, m.session.UserID, m.target, m.community); err != nil {
		c.fail(w, r, "kick", err)
		return
	}
	if err := requireCommunityWire(r.Context(), c.repo, m.session.WireVersion, &m.community); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), m.community); err != nil {
			return notFound(err)
		}
		if err := stillModerates(r.Context(), tx, m, PermKickMembers); err != nil {
			return err
		}
		member, err := tx.GetMember(r.Context(), m.community, m.target)
		if err != nil {
			return err
		}
		if err := appendMemberEpisodeEvent(r.Context(), tx, protocol.EventMemberLeave, m.community, m.target, member.EpisodeID, c.clk.Now().Unix()); err != nil {
			return err
		}
		if err := tx.DeleteMember(r.Context(), m.community, m.target); err != nil {
			return err
		}
		if _, err := tx.DeleteUserOverwrites(r.Context(), m.community, m.target); err != nil {
			return err
		}
		if _, err := tx.DeleteCommunityChannelMembers(r.Context(), m.community, m.target); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &m.session.UserID, Action: "member.kick",
			Target: m.target.String(), Detail: m.community.String(), At: c.clk.Now().Unix(),
		})
	}); err != nil {
		c.fail(w, r, "kick", notFound(err))
		return
	}
	// After the commit, never inside it: see RemoveUserFromCommunityGroups.
	c.removeFromGroups(r, m.community, m.target)
	c.notify.Removed(afterCommit(r), m.community, m.target)
	c.notify.Changed(afterCommit(r), m.community)
	w.WriteHeader(http.StatusNoContent)
}

// removeFromGroups issues the delivery-service Removes of a membership change
// that has already committed. A failure is logged, not answered: the membership
// row is gone, so the Add and join ACL (ResolverACL) already refuses the user,
// and the change itself cannot be undone by a refused proposal. It runs on
// afterCommit's context, so a client that disconnects does not cut it short.
func (c *Communities) removeFromGroups(r *http.Request, cid, userID id.ID) {
	ctx := afterCommit(r)
	if err := RemoveUserFromCommunityGroups(ctx, c.repo, c.dsvc, cid, userID); err != nil {
		c.log.ErrorContext(ctx, "remove user from the community's groups",
			"community", cid, "user", userID, "err", err)
	}
	// The user holds nothing in the community any more: their connected call sessions are cut
	// immediately after, on the call retry loop, not when a member commits the Remove.
	syncAfterMembership(ctx, c.calls, cid, userID, nil)
}

func (c *Communities) leave(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.memberOnly(r)
	if err != nil {
		c.fail(w, r, "leave", err)
		return
	}
	if row.Owner == s.UserID {
		server.WriteError(w, server.Errorf(server.CodeForbidden,
			"the owner must transfer ownership or delete the community"))
		return
	}
	if err := requireCommunityWire(r.Context(), c.repo, s.WireVersion, &row.ID); err != nil {
		server.WriteError(w, err)
		return
	}
	// The membership and the user's channel_members rows for the community go in
	// one transaction, as for a kick.
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		// Re-checked under the community lock: a hand-over to the leaver may have committed
		// since the read above, and the owner must never leave.
		owner, err := lockedOwner(r.Context(), tx, row.ID)
		if err != nil {
			return err
		}
		if owner == s.UserID {
			return server.Errorf(server.CodeForbidden,
				"the owner must transfer ownership or delete the community")
		}
		member, err := tx.GetMember(r.Context(), row.ID, s.UserID)
		if err != nil {
			return err
		}
		if err := appendMemberEpisodeEvent(r.Context(), tx, protocol.EventMemberLeave, row.ID, s.UserID, member.EpisodeID, c.clk.Now().Unix()); err != nil {
			return err
		}
		if err := tx.DeleteMember(r.Context(), row.ID, s.UserID); err != nil {
			return err
		}
		if _, err := tx.DeleteUserOverwrites(r.Context(), row.ID, s.UserID); err != nil {
			return err
		}
		_, err = tx.DeleteCommunityChannelMembers(r.Context(), row.ID, s.UserID)
		return err
	}); err != nil {
		c.fail(w, r, "leave", notFound(err))
		return
	}
	// A member who leaves stops receiving the community's keys: the instance
	// removes their leaves exactly as for a kick, after the row is gone.
	c.removeFromGroups(r, row.ID, s.UserID)
	// The leaver's other tabs and devices learn it too, and the members see one fewer.
	c.notify.Removed(afterCommit(r), row.ID, s.UserID)
	c.notify.Changed(afterCommit(r), row.ID)
	w.WriteHeader(http.StatusNoContent)
}

// appendMemberEpisodeEvent writes the membership event beside the membership row change. Its
// payload is only the opaque account id and the episode id; profile data stays out.
func appendMemberEpisodeEvent(ctx context.Context, tx store.Repository, kind protocol.EventKind, community, user, episode id.ID, at int64) error {
	if episode.IsZero() || at < 0 {
		return store.ErrConflict
	}
	_, err := tx.AppendBotStructuralEvent(ctx, protocol.Event{
		ID: id.New(), CommunityID: community, Timestamp: uint64(at), Kind: kind,
		Data: []any{user, episode},
	})
	return err
}

func (c *Communities) delete(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.ownerOnly(r)
	if err != nil {
		c.fail(w, r, "delete community", err)
		return
	}
	if err := requireCommunityWire(r.Context(), c.repo, s.WireVersion, &row.ID); err != nil {
		server.WriteError(w, err)
		return
	}
	now := c.clk.Now().Unix()
	// Inside the transaction: read what will be closed, then tombstone.
	// ListChannels filters deleted_at IS NULL, so it runs BEFORE
	// DeleteChannelsOfCommunity or it returns nothing.
	var toClose, ids, members []id.ID
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		// Re-checked under the community lock: a hand-over may have committed since ownerOnly.
		owner, err := lockedOwner(r.Context(), tx, row.ID)
		if err != nil {
			return err
		}
		if owner != s.UserID {
			return server.Errorf(server.CodeForbidden, "owner only")
		}
		channels, err := tx.ListChannels(r.Context(), row.ID)
		if err != nil {
			return err
		}
		// The members at the moment of the delete, read under its transaction: after the
		// commit they are told (op 54), and nobody who was not one is.
		if members, err = communityMemberIDs(r.Context(), tx, row.ID); err != nil {
			return err
		}
		ids = make([]id.ID, 0, len(channels))
		for _, ch := range channels {
			ids = append(ids, ch.ID)
		}
		if toClose, err = openChannelGroups(r.Context(), tx, ids); err != nil {
			return err
		}
		// The community's channels go with it, in the same transaction.
		if _, err := tx.DeleteChannelsOfCommunity(r.Context(), row.ID, now); err != nil {
			return err
		}
		if err := tx.SoftDeleteCommunity(r.Context(), row.ID, now); err != nil {
			return err
		}
		// E8: the server's Giphy key goes with it; a secret must not outlive the server it belongs to.
		if err := tx.PutSetting(r.Context(), gifSettingKey(row.ID), []byte("{}"), now); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "community.delete", Target: row.ID.String(),
			Detail: "", At: now,
		})
	}); err != nil {
		c.fail(w, r, "delete community", notFound(err))
		return
	}
	// After the commit, for RemoveUserFromCommunityGroups' reason: ds.Close
	// writes through the single-connection write pool the transaction held. A
	// crash between the two leaves the community tombstoned and its groups open;
	// they carry no live channel, so the Add and join ACL refuses everyone.
	if err := closeGroups(afterCommit(r), c.dsvc, toClose); err != nil {
		c.log.ErrorContext(r.Context(), "close the groups of a deleted community",
			"community", row.ID, "err", err)
	}
	// Every live call of its channels ends with it (CALLS-1), as for a channel delete.
	c.calls.EndChannelCalls(afterCommit(r), ids, "community_deleted")
	c.notify.ChangedTo(afterCommit(r), row.ID, members)
	w.WriteHeader(http.StatusNoContent)
}

type transferReq struct {
	_      struct{} `cbor:",toarray"`
	UserID id.ID
}

// transfer is POST /v1/communities/{id}/owner [user_id]: 204. Only the owner hands the community
// over, to another member who is not a bot; the previous owner stays a member with their roles,
// and may then leave like anyone else. The new owner is read and written under the community row
// lock, with an audit row naming the previous owner, and the members are told after the commit.
func (c *Communities) transfer(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.memberOnly(r)
	if err != nil {
		c.fail(w, r, "transfer ownership", err)
		return
	}
	if row.Owner != s.UserID {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "only the owner can hand over a community"))
		return
	}
	var req transferReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.UserID == s.UserID {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the community is already yours"))
		return
	}
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), row.ID); err != nil {
			return notFound(err)
		}
		// The owner is re-read under the lock: a hand-over that raced this one has already moved it.
		locked, err := tx.GetCommunity(r.Context(), row.ID)
		if err != nil {
			return notFound(err)
		}
		if locked.Owner != s.UserID {
			return server.Errorf(server.CodeForbidden, "only the owner can hand over a community")
		}
		if _, err := tx.GetMember(r.Context(), row.ID, req.UserID); err != nil {
			return notFound(err)
		}
		u, err := tx.GetUser(r.Context(), req.UserID)
		if err != nil {
			return notFound(err)
		}
		if u.Kind != 0 {
			return server.Errorf(server.CodeInvalidRequest, "a bot account cannot own a community")
		}
		// As joinGate: an account that may not join a community may not own one.
		if u.DisabledAt != nil || u.DeletedAt != nil {
			return server.Errorf(server.CodeInvalidRequest, "that account can no longer own a community")
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, &row.ID); err != nil {
			return err
		}
		if err := tx.SetCommunityOwner(r.Context(), row.ID, req.UserID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "community.transfer", Target: req.UserID.String(),
			Detail: s.UserID.String(), At: c.clk.Now().Unix(),
		})
	}); err != nil {
		c.fail(w, r, "transfer ownership", err)
		return
	}
	c.notify.Changed(afterCommit(r), row.ID)
	w.WriteHeader(http.StatusNoContent)
}

// fail writes err as the client's refusal. A store.ErrConflict is the policy
// version guard: a concurrent PATCH got there first. Anything that is not a
// *server.Error is logged here and reaches the client as an empty E_INTERNAL.
func (c *Communities) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	switch {
	case errors.As(err, &se):
	case errors.Is(err, store.ErrConflict):
		err = server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest,
			"the community changed concurrently: read it again and retry"))
	default:
		c.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}

// enrolledSession is the session every community and role route requires:
// protocol/09 marks them all "E". The middleware already enforces the scope;
// this is the handler's own check, so a route mounted without it fails closed.
func enrolledSession(r *http.Request) (auth.Session, error) {
	s, ok := auth.SessionFrom(r.Context())
	if !ok {
		return s, server.Errorf(server.CodeUnauthenticated, "no device session")
	}
	if s.Scope != auth.ScopeEnrolled {
		return s, server.Errorf(server.CodeForbidden, "an enrolled session is required")
	}
	return s, nil
}

// lockedOwner takes the community row lock in tx and reads the owner under it, so a decision that
// depends on who owns the community cannot be overtaken by a hand-over (POST …/owner takes the
// same lock). An unknown or deleted community is 404.
func lockedOwner(ctx context.Context, tx store.Repository, cid id.ID) (id.ID, error) {
	if err := tx.LockCommunity(ctx, cid); err != nil {
		return id.ID{}, notFound(err)
	}
	row, err := tx.GetCommunity(ctx, cid)
	if err != nil {
		return id.ID{}, notFound(err)
	}
	return row.Owner, nil
}

func notFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return server.Errorf(server.CodeNotFound, "no such object")
	}
	return err
}

// validCommunityName is 1..255 bytes of UTF-8 with no control character. A
// NUL is legal in a Go string and in a SQLite TEXT column but refused by
// Postgres, so without the check the same request would succeed on one engine
// and fail as a 500 on the other.
func validCommunityName(name string) error {
	if name == "" || len(name) > maxCommunityNameBytes {
		return server.Errorf(server.CodeInvalidRequest, "name must be 1..%d bytes", maxCommunityNameBytes)
	}
	if !utf8.ValidString(name) {
		return server.Errorf(server.CodeInvalidRequest, "name is not UTF-8")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return server.Errorf(server.CodeInvalidRequest, "name contains a control character")
		}
	}
	return nil
}

func validMinAccountAge(v uint64) error {
	if v > maxMinAccountAgeSeconds {
		return server.Errorf(server.CodeInvalidRequest,
			"min_account_age_seconds is at most %d", maxMinAccountAgeSeconds)
	}
	return nil
}

func requireMod2FAFlag(v uint64) (uint8, error) {
	switch v {
	case 0:
		return 0, nil
	case 1:
		return 1, nil
	}
	return 0, server.Errorf(server.CodeInvalidRequest, "require_mod_2fa is 0 or 1")
}

// memberNick normalises a member nickname. R16: a nick is a display name, not a
// handle, so it takes auth.NormalizeDisplay's rules (free Unicode, NFC, at most
// 64 characters, trimmed, under the shared text rule of auth.NormalizeUserText:
// no control, bidi, zero-width or other invisible character) rather than the
// ASCII handle rules. Every members row this package writes goes through it.
func memberNick(s string) (string, error) {
	out, err := auth.NormalizeDisplay(s)
	if err != nil {
		return "", server.Errorf(server.CodeInvalidRequest, "nick: %v", err)
	}
	return out, nil
}

// CommunityPolicy is the community policy document: communities.policy_json,
// a JSON object recorded in protocol/09 § Communities. Every key is optional
// and an absent key is its default, so `{}` is an open, unscreened community
// that keeps its history indefinitely. It is stored byte for byte as the owner
// wrote it; this type is how the instance reads it.
type CommunityPolicy struct {
	// Join is "open" (the default, also spelled by omission) or "invite".
	// POST /v1/communities/{id}/join enforces "invite" (admitByInvite).
	Join string `json:"join"`
	// Screening is R17's membership-screening flag: stored and served, not
	// enforced by this version of the instance.
	Screening bool `json:"screening"`
	// RetentionDays is ARCHIVAL retention (R28, protocol/02 § Retention): how
	// long application messages every cursor has passed are kept. 0 means
	// indefinitely, the default.
	//
	// ENFORCED FOR ATTACHMENTS ONLY in this version (fix wave I5, controller
	// ruling): the blob sweeper expires blob references older than it
	// (protocol/09 § Blobs). No message, ciphertext or server-readable, is
	// deleted by it yet; that is the plan's follow-up card 9.
	RetentionDays uint64 `json:"retention_days"`
	// DeliveryRetentionDays shortens DELIVERY retention, the window a device
	// that was away can still fetch in. 0 means the instance's own 30 days; a
	// community may shorten it and never lengthen it, because protocol/02
	// forbids a handshake window past 30 days.
	//
	// STORED AND SERVED, NOT ENFORCED in this version (fix wave I5): delivery
	// retention is the instance's 30 days for every community until follow-up
	// card 9 lands.
	DeliveryRetentionDays uint64 `json:"delivery_retention_days"`
}

// The policy's two retention bounds, in days.
const (
	maxRetentionDays         = 36500 // a century: days * 86400 stays far inside an int64
	maxDeliveryRetentionDays = 30
)

// InviteOnly reports whether the community admits a join only with an invite.
func (p CommunityPolicy) InviteOnly() bool { return p.Join == "invite" }

// ParseCommunityPolicy decodes and validates a policy document. It refuses a
// document that is not bounded UTF-8 JSON, not exactly one object, carries a key
// this version does not know (a misspelt retention key silently ignored would
// keep history the owner asked to delete), or a value out of range. The error
// is a 400 E_INVALID_REQUEST.
func ParseCommunityPolicy(b []byte) (CommunityPolicy, error) {
	var p CommunityPolicy
	if len(b) > maxPolicyBytes {
		return p, server.Errorf(server.CodeInvalidRequest,
			"policy is %d bytes, at most %d", len(b), maxPolicyBytes)
	}
	if !utf8.Valid(b) {
		return p, server.Errorf(server.CodeInvalidRequest, "policy is not UTF-8")
	}
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return p, server.Errorf(server.CodeInvalidRequest, "policy must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return CommunityPolicy{}, server.Errorf(server.CodeInvalidRequest, "policy: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return CommunityPolicy{}, server.Errorf(server.CodeInvalidRequest,
			"policy must be exactly one JSON object")
	}
	switch p.Join {
	case "", "open", "invite":
	default:
		return CommunityPolicy{}, server.Errorf(server.CodeInvalidRequest,
			`policy join is "open" or "invite"`)
	}
	if p.RetentionDays > maxRetentionDays {
		return CommunityPolicy{}, server.Errorf(server.CodeInvalidRequest,
			"policy retention_days is at most %d", maxRetentionDays)
	}
	if p.DeliveryRetentionDays > maxDeliveryRetentionDays {
		return CommunityPolicy{}, server.Errorf(server.CodeInvalidRequest,
			"policy delivery_retention_days is at most %d: a community may shorten delivery retention, never lengthen it",
			maxDeliveryRetentionDays)
	}
	return p, nil
}
