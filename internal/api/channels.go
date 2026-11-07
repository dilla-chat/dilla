package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Channel kinds, protocol/01 "Group kinds" for 0 and 1, interfaces.md §4.3 for
// the rest.
const (
	ChannelText     uint8 = 0
	ChannelVoice    uint8 = 1
	ChannelCategory uint8 = 2
	ChannelDM       uint8 = 3
	ChannelGroupDM  uint8 = 4
)

// Channel modes and visibilities, interfaces.md §4.3.
const (
	ModeE2EE     uint8 = 0
	ModeReadable uint8 = 1
)

const (
	VisPrivate      uint8 = 0
	VisInvite       uint8 = 1
	VisDiscoverable uint8 = 2
)

const (
	maxChannelNameBytes  = 100
	maxChannelTopicBytes = 1024
	maxSlowmodeSeconds   = 21600 // six hours
	// maxChannelPosition keeps a client's uint64 inside the signed column on
	// both engines: past 2^63 it would wrap negative in the adapter and sort
	// the channel in front of every other.
	maxChannelPosition = 1<<31 - 1
)

// TextGroupAllowed reports whether an MLS text group (protocol/01 kind 0) may be
// registered for this channel. The delivery service asks this question and
// answers 403 E_MODE_READABLE when it is false (interfaces.md §2.1).
func TextGroupAllowed(c store.ChannelRow) bool {
	if c.DeletedAt != nil {
		return false
	}
	switch c.Kind {
	case ChannelText, ChannelDM, ChannelGroupDM:
		return c.Mode == ModeE2EE
	default:
		return false
	}
}

// CallGroupAllowed reports whether a call group (kind 1) may be registered.
// protocol/01: "A call in any voice channel, including a voice channel of a
// community whose text channels are readable, always has a call group."
func CallGroupAllowed(c store.ChannelRow) bool {
	if c.DeletedAt != nil {
		return false
	}
	return c.Kind == ChannelVoice || c.Kind == ChannelDM || c.Kind == ChannelGroupDM
}

// Channels serves the channel routes of interfaces.md §5.2: create a channel in
// a community, read, patch and delete one. Every body is a fixed-position CBOR
// array (protocol/09 § Channels). Register mounts the handlers bare, as
// Communities does, and each handler requires an enrolled session itself.
// Every permission decision is the resolver's (perm.go).
type Channels struct {
	repo       store.Repository
	dsvc       DS
	clk        clock.Clock
	maxGroupDM int
	log        *slog.Logger
	res        *Resolver
	// calls cuts a removed group-DM participant's connected call session, and re-syncs a channel's
	// live call after a visibility change; nil touches no call.
	calls *Calls
	roles *Roles
	// notify tells the community's members, after the commit, that a channel was created,
	// updated or deleted (op 54); nil tells nobody.
	notify *StructureNotifier
	// botEvents hears Bot-channel unmarks a channel update or delete makes; nil hears nothing.
	botEvents BotChannelEvents
}

// WithCalls sets the call routes a membership change of a channel re-syncs, and returns c.
func (c *Channels) WithCalls(calls *Calls) *Channels {
	c.calls = calls
	return c
}

func (c *Channels) WithRoles(roles *Roles) *Channels { c.roles = roles; return c }

// WithBotChannelEvents sets who hears that a channel's Bot-channel mark went with an update or a
// delete (Bot v2 Task 9), and returns c. Nil hears nothing.
func (c *Channels) WithBotChannelEvents(e BotChannelEvents) *Channels {
	c.botEvents = e
	return c
}

func (c *Channels) botChannelEvents() BotChannelEvents {
	if c.botEvents == nil {
		return NoBotChannelEvents{}
	}
	return c.botEvents
}

// WithNotifier sets where a community channel's create, update and delete is announced, and
// returns c.
func (c *Channels) WithNotifier(n *StructureNotifier) *Channels {
	c.notify = n
	return c
}

// announce sends community_changed for the community row belongs to, after the commit. A DM
// belongs to none and is not announced.
func (c *Channels) announce(r *http.Request, row store.ChannelRow) {
	if row.CommunityID != nil {
		c.notify.Changed(afterCommit(r), *row.CommunityID)
	}
}

// NewChannels takes the delivery service that closes a deleted channel's
// groups (membership.go) and brings a group DM's groups in line with its
// participants (SyncGroupMembers), and maxGroupDM, the cap NewDMs enforces at
// creation, which the member routes enforce on every add.
func NewChannels(repo store.Repository, dsvc DS, clk clock.Clock, maxGroupDM int, log *slog.Logger) *Channels {
	return &Channels{repo: repo, dsvc: dsvc, clk: clk, maxGroupDM: maxGroupDM, log: log, res: NewResolver(repo)}
}

func (c *Channels) Register(mux *server.Mux) {
	mux.Socket("POST /v1/communities/{id}/channels", http.HandlerFunc(c.create))
	mux.Socket("GET /v1/communities/{id}/channels", http.HandlerFunc(c.list))
	mux.Socket("GET /v1/channels/{id}", http.HandlerFunc(c.get))
	mux.Socket("GET /v1/channels/{id}/permissions", http.HandlerFunc(c.permissions))
	mux.Socket("PATCH /v1/channels/{id}", http.HandlerFunc(c.patch))
	mux.Socket("DELETE /v1/channels/{id}", http.HandlerFunc(c.delete))
}

// permissions returns the caller's effective channel permissions after role
// and member overwrites. A channel the caller cannot view remains indistinguishable
// from an unknown channel through visible.
func (c *Channels) permissions(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.visible(r)
	if err != nil {
		c.fail(w, r, "read channel permissions", err)
		return
	}
	bits, err := c.res.Resolve(r.Context(), s.UserID, row)
	if err != nil {
		c.fail(w, r, "read channel permissions", err)
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, uint64(bits)); err != nil {
		c.log.Error("encode channel permissions", "err", err)
	}
}

// RegisterMembers registers the three channel-membership routes of §5.2. They are
// separate from Register because channel_members is task 6's table: a build of
// tasks 1-5 alone must not advertise a route with no storage behind it.
func (c *Channels) RegisterMembers(mux *server.Mux) {
	mux.Socket("GET /v1/channels/{id}/members", http.HandlerFunc(c.members))
	mux.Socket("PUT /v1/channels/{id}/members/{user_id}", http.HandlerFunc(c.addMember))
	mux.Socket("DELETE /v1/channels/{id}/members/{user_id}", http.HandlerFunc(c.removeMember))
}

// members lists the channel's materialised membership, ordered by user id, one
// [user_id] row per member. On a community channel that is the set task 7
// derives from the resolver; on a DM or group DM it is the participant list,
// which is the only place it is stored.
func (c *Channels) members(w http.ResponseWriter, r *http.Request) {
	row, _, err := c.visible(r)
	if err != nil {
		c.fail(w, r, "list channel members", err)
		return
	}
	ids, err := c.repo.ListChannelMembers(r.Context(), row.ID)
	if err != nil {
		c.fail(w, r, "list channel members", err)
		return
	}
	out := make([][]id.ID, 0, len(ids))
	for _, u := range ids {
		out = append(out, []id.ID{u})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		c.log.Error("encode channel members", "err", err)
	}
}

// groupDM is visible restricted to a group DM, the one kind whose membership is
// written by hand, and the {user_id} the route names. A community channel's
// membership is derived from the permission resolver and is never written
// directly: a hand-written row would be overwritten by the next materialisation
// pass and the caller would never know. A 1:1 DM's pair is its id (P2-D31).
// Every participant may add and remove, because a group DM has no roles.
func (c *Channels) groupDM(r *http.Request) (store.ChannelRow, auth.Session, id.ID, error) {
	row, s, err := c.visible(r)
	if err != nil {
		return row, s, id.ID{}, err
	}
	if row.Kind != ChannelGroupDM {
		return row, s, id.ID{}, server.Errorf(server.CodeForbidden,
			"only a group DM's membership is set directly; a channel's follows its permissions")
	}
	target, err := server.PathID(r, "user_id")
	if err != nil {
		return row, s, id.ID{}, err
	}
	return row, s, target, nil
}

// addMember adds one participant to a group DM, then proposes the new
// participant's devices for the DM's text group.
func (c *Channels) addMember(w http.ResponseWriter, r *http.Request) {
	row, s, target, err := c.groupDM(r)
	if err != nil {
		c.fail(w, r, "add channel member", err)
		return
	}
	if err := mayReceiveDM(r.Context(), c.repo, target); err != nil {
		c.fail(w, r, "add channel member", err)
		return
	}
	now := c.clk.Now().Unix()
	// The count and the insert share one transaction, so two adds racing for
	// the last seat cannot both land on SQLite's single writer.
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		existing, err := tx.ListChannelMembers(r.Context(), row.ID)
		if err != nil {
			return err
		}
		if slices.Contains(existing, target) {
			return nil // already a participant: not growth, and not an error
		}
		if len(existing) >= c.maxGroupDM {
			return server.Errorf(server.CodeInvalidRequest,
				"a group DM holds at most %d members", c.maxGroupDM)
		}
		if err := tx.PutChannelMember(r.Context(), row.ID, target, now); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "channel.member.add", Target: row.ID.String(),
			Detail: target.String(), At: now,
		})
	}); err != nil {
		c.fail(w, r, "add channel member", err)
		return
	}
	// After the commit, never inside the transaction: SyncGroupMembers calls the
	// delivery service. See RemoveUserFromCommunityGroups on the single-writer
	// pool. A failure here leaves the row, and the next sync proposes the Adds.
	if err := SyncGroupMembers(afterCommit(r), c.repo, c.dsvc, row, now); err != nil {
		c.log.ErrorContext(r.Context(), "sync group members after add", "channel", row.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeMember is the mirror image: DeleteChannelMember, then SyncGroupMembers,
// which issues the Removes for every leaf the departing user held in the DM's
// text and call groups. A participant may always remove themself, and any
// participant may remove another.
func (c *Channels) removeMember(w http.ResponseWriter, r *http.Request) {
	row, s, target, err := c.groupDM(r)
	if err != nil {
		c.fail(w, r, "remove channel member", err)
		return
	}
	now := c.clk.Now().Unix()
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.DeleteChannelMember(r.Context(), row.ID, target); err != nil {
			return notFound(err)
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "channel.member.remove", Target: row.ID.String(),
			Detail: target.String(), At: now,
		})
	}); err != nil {
		c.fail(w, r, "remove channel member", err)
		return
	}
	if err := SyncGroupMembers(afterCommit(r), c.repo, c.dsvc, row, now); err != nil {
		c.log.ErrorContext(r.Context(), "sync group members after remove", "channel", row.ID, "err", err)
	}
	// The removed participant's connected session in the DM's call is cut immediately after, on the
	// call retry loop (the request queues it and never waits on the SFU).
	syncAfterMembership(afterCommit(r), c.calls, id.ID{}, target, &row.ID)
	w.WriteHeader(http.StatusNoContent)
}

type createChannelReq struct {
	_               struct{} `cbor:",toarray"`
	Kind            uint64
	Mode            uint64
	Visibility      uint64
	ParentID        *id.ID
	Name            string
	Topic           string
	Position        uint64
	SlowmodeSeconds uint64
}

// createOverwrite is one entry of a channel create's optional ninth element (protocol/09 §
// Channels): [kind(uint), target_id(bstr16), allow(uint), deny(uint)].
type createOverwrite struct {
	_      struct{} `cbor:",toarray"`
	Kind   uint64
	Target id.ID
	Allow  uint64
	Deny   uint64
}

// maxCreateOverwrites bounds a channel create's overwrites, and so the work of checking them.
const maxCreateOverwrites = 100

// decodeCreateChannel reads a channel create body: the eight fields of createChannelReq, then
// optionally the overwrites. A toarray struct is exact-arity, so the body is read as an array
// first; any length but 8 or 9 is refused exactly as a wrong-arity createChannelReq always was.
// overwrites is nil for an 8-element body.
func decodeCreateChannel(w http.ResponseWriter, r *http.Request) (createChannelReq, []createOverwrite, error) {
	var req createChannelReq
	var raw []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &raw); err != nil {
		return req, nil, err
	}
	shape := func(err error) error {
		return server.Errorf(server.CodeInvalidRequest, "body is not deterministic CBOR: %v", err)
	}
	fields := raw
	if len(raw) == 9 {
		fields = raw[:8]
	}
	// The fields are re-encoded as an array and decoded into the struct, so every field rule of
	// the 8-element body, and its refusal for any other length, holds unchanged.
	b, err := cborx.Marshal(fields)
	if err != nil {
		return req, nil, err
	}
	if err := cborx.Unmarshal(b, &req); err != nil {
		return req, nil, shape(err)
	}
	if len(raw) != 9 {
		return req, nil, nil
	}
	var overwrites []createOverwrite
	if err := cborx.Unmarshal(raw[8], &overwrites); err != nil {
		return req, nil, shape(err)
	}
	if len(overwrites) < 1 || len(overwrites) > maxCreateOverwrites {
		return req, nil, server.Errorf(server.CodeInvalidRequest, "overwrites must hold 1 to %d entries", maxCreateOverwrites)
	}
	return req, overwrites, nil
}

// createGate is every permission decision of a channel create, read from repo: the caller is a
// member with manage_channels community-wide, and, when overwrites is non-nil, holds manage_roles
// community-wide, the channel is not a category, and every entry follows putOverwrite's rules
// against the caller's community-wide bits. A kind-1 entry targeting the caller themself, allowing
// view_channel, send_messages or both (bits they must hold) and denying none, is exempt from the
// rank rule; it exists only here, never on a later PUT. Finally the caller must still
// view the channel with the overwrites applied. The handler runs it before the transaction, for
// the refusal's precedence, and again inside it under the community row lock, so a demotion that
// commits in between cannot let a create through on authority the caller no longer holds.
//
// It returns the overwrites as rows whose ChannelID the caller fills in; none for nil overwrites.
func createGate(ctx context.Context, repo store.Repository, cid, uid id.ID, kind uint8, overwrites []createOverwrite) ([]store.OverwriteRow, error) {
	if _, err := repo.GetMember(ctx, cid, uid); err != nil {
		// A non-member must not learn that the community exists.
		return nil, notFound(err)
	}
	// There is no channel yet, so there are no stored overwrites to apply: the community-wide
	// bits decide.
	snap, err := LoadSnapshot(ctx, repo, cid, uid, nil)
	if err != nil {
		return nil, notFound(err)
	}
	have := snap.Resolve(uid)
	if !have.Has(PermManageChannels) {
		return nil, server.Errorf(server.CodeForbidden, "manage channels")
	}
	if overwrites == nil {
		return nil, nil
	}
	if !have.Has(PermManageRoles) {
		return nil, server.Errorf(server.CodeForbidden, "manage roles")
	}
	if kind == ChannelCategory {
		return nil, server.Errorf(server.CodeInvalidRequest, "a category's overwrites have no effect")
	}
	top, _ := snap.Highest(uid)
	rows := make([]store.OverwriteRow, 0, len(overwrites))
	for _, o := range overwrites {
		row, err := createOverwriteRow(ctx, repo, cid, uid, have, top, snap.Owner, o)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	type target struct {
		kind uint8
		id   id.ID
	}
	seen := make(map[target]bool, len(rows))
	for _, row := range rows {
		k := target{row.TargetKind, row.TargetID}
		if seen[k] {
			return nil, server.Errorf(server.CodeInvalidRequest,
				"a target has at most one overwrite (%d:%s)", row.TargetKind, row.TargetID)
		}
		seen[k] = true
	}
	snap.Overwrite = rows
	if !snap.ResolveChannel(uid).Has(PermViewChannel) {
		return nil, server.Errorf(server.CodeInvalidRequest, "the channel would be hidden from you")
	}
	return rows, nil
}

// createOverwriteRow checks one create entry as putOverwrite checks a PUT, with the caller's
// community-wide bits (have) and highest role position (top), and returns it as a row whose
// channel id the caller fills in.
func createOverwriteRow(ctx context.Context, repo store.Repository, cid, uid id.ID, have Bits, top uint64, owner id.ID, o createOverwrite) (store.OverwriteRow, error) {
	var kind uint8
	switch o.Kind {
	case uint64(overwriteRole):
		kind = overwriteRole
	case uint64(overwriteUser):
		kind = overwriteUser
	default:
		return store.OverwriteRow{}, server.Errorf(server.CodeInvalidRequest, "kind is 0 (role) or 1 (user)")
	}
	var targetPos uint64
	switch kind {
	case overwriteRole:
		role, err := repo.GetRole(ctx, o.Target)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.OverwriteRow{}, err
		}
		if err != nil || role.CommunityID != cid {
			return store.OverwriteRow{}, server.Errorf(server.CodeNotFound, "no such role")
		}
		targetPos = role.Position
	case overwriteUser:
		if _, err := repo.GetMember(ctx, cid, o.Target); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return store.OverwriteRow{}, err
			}
			return store.OverwriteRow{}, server.Errorf(server.CodeNotFound, "not a member")
		}
		target, err := LoadSnapshot(ctx, repo, cid, o.Target, nil)
		if err != nil {
			return store.OverwriteRow{}, notFound(err)
		}
		targetPos, _ = target.Highest(o.Target)
	}
	allow, err := knownBits("allow", o.Allow)
	if err != nil {
		return store.OverwriteRow{}, err
	}
	deny, err := knownBits("deny", o.Deny)
	if err != nil {
		return store.OverwriteRow{}, err
	}
	if both := allow & deny; both != 0 {
		return store.OverwriteRow{}, server.Errorf(server.CodeInvalidRequest,
			"a bit is either allowed or denied, not both (%#x)", uint64(both))
	}
	if scoped := (allow | deny) & communityBits; scoped != 0 {
		return store.OverwriteRow{}, server.Errorf(server.CodeInvalidRequest,
			"a channel overwrite cannot carry a community-wide permission (%#x)", uint64(scoped))
	}
	if err := checkBits(have, allow, deny); err != nil {
		return store.OverwriteRow{}, err
	}
	// The one exception to the rank rule, at create only: the creator's own entry allowing
	// view_channel, send_messages or both, and denying nothing. It keeps a private channel visible
	// to them and lets them post in an announcement channel they made (R5); checkBits above has
	// already required that they hold every bit. Any other bit on oneself (manage_messages, pin,
	// mute…), or any deny, takes the rank rule: 403. putOverwrite has no such exception.
	self := kind == overwriteUser && o.Target == uid && deny == 0 && allow != 0 &&
		allow&^(PermViewChannel|PermSendMessages) == 0
	if !self && uid != owner && targetPos >= top {
		return store.OverwriteRow{}, server.Errorf(server.CodeForbidden,
			"an overwrite may target only a role or member below your highest role")
	}
	return store.OverwriteRow{TargetKind: kind, TargetID: o.Target, Allow: uint64(allow), Deny: uint64(deny)}, nil
}

type createChannelResp struct {
	_          struct{} `cbor:",toarray"`
	ChannelID  id.ID
	Mode       uint64
	Visibility uint64
}

func (c *Channels) create(w http.ResponseWriter, r *http.Request) {
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
	req, overwrites, err := decodeCreateChannel(w, r)
	if err != nil {
		c.fail(w, r, "create channel", err)
		return
	}
	com, err := c.repo.GetCommunity(r.Context(), cid)
	if err != nil {
		c.fail(w, r, "create channel", notFound(err))
		return
	}
	// The member and manage_channels checks, read before the kind is known. The overwrite rules
	// follow once it is, and every check runs again inside the transaction.
	if _, err := createGate(r.Context(), c.repo, cid, s.UserID, ChannelText, nil); err != nil {
		c.fail(w, r, "create channel", err)
		return
	}
	// The three enums are checked as the client sent them, before they are
	// narrowed to a byte: 256 must not become 0.
	kind, err := enumByte(req.Kind, ChannelGroupDM, "kind must be 0..4")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	mode, err := enumByte(req.Mode, ModeReadable, "mode must be 0 or 1")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	visibility, err := enumByte(req.Visibility, VisDiscoverable, "visibility must be 0, 1 or 2")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	switch kind {
	case ChannelDM, ChannelGroupDM:
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"a DM is not created through a community"))
		return
	}
	if overwrites != nil {
		if _, err := createGate(r.Context(), c.repo, cid, s.UserID, kind, overwrites); err != nil {
			c.fail(w, r, "create channel", err)
			return
		}
	}
	row := store.ChannelRow{
		ID:                id.New(),
		CommunityID:       &cid,
		Kind:              kind,
		Mode:              mode,
		Visibility:        visibility,
		ParentID:          topLevelIfZero(req.ParentID),
		Name:              req.Name,
		Topic:             req.Topic,
		Position:          req.Position,
		SettingsJSON:      []byte("{}"),
		HostPolicyVersion: com.PolicyVersion,
		SlowmodeSeconds:   req.SlowmodeSeconds,
		Created:           c.clk.Now().Unix(),
	}
	// A visible channel is readable, and asking for e2ee is not an error: it is
	// how a client says "make this channel", and the label the user sees comes
	// from the stored mode. interfaces.md task 2: "forced to readable at
	// creation".
	if row.Visibility != VisPrivate {
		row.Mode = ModeReadable
	}
	// The parent is read and the row written in one transaction, so a category
	// deleted in between cannot end up with a live child. The channel and its
	// overwrites are written together: no reader, materialiser or frame ever sees
	// the channel without them, so it is never open to a member it excludes.
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		// Every permission decision is taken again under the community row lock,
		// so a demotion or role change that committed after the checks above
		// cannot let this create through on authority the caller no longer holds.
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		rows, err := createGate(r.Context(), tx, cid, s.UserID, kind, overwrites)
		if err != nil {
			return err
		}
		if err := requireParentVisible(r.Context(), tx, s.UserID, row); err != nil {
			return err
		}
		if err := validChannel(r.Context(), tx, row); err != nil {
			return err
		}
		// R-AFK8: 2^30 and above is the built-in AFK channel's, which no route creates.
		if row.Position >= AFKBuiltinPosition {
			return reservedPosition()
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, row.CommunityID); err != nil {
			return err
		}
		if err := tx.CreateChannel(r.Context(), row); err != nil {
			return err
		}
		if err := tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "channel.create", Target: row.ID.String(),
			Detail: cid.String(), At: row.Created,
		}); err != nil {
			return err
		}
		for _, o := range rows {
			o.ChannelID = row.ID
			if err := tx.PutOverwrite(r.Context(), o); err != nil {
				return err
			}
			if err := tx.Audit(r.Context(), store.AuditRow{
				Actor: &s.UserID, Action: "channel.overwrite.put", Target: row.ID.String(),
				Detail: fmt.Sprintf("%d:%s", o.TargetKind, o.TargetID), At: row.Created,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		c.fail(w, r, "create channel", err)
		return
	}
	// channel_members is derived from the resolver from the channel's first
	// moment, so GET /v1/channels/{id}/members answers for it before any group
	// exists. After the commit, never inside it (task 7). The channel has no
	// group yet, so nothing is proposed: registering its text group is what
	// populates that (SyncRegisteredGroup).
	if err := MaterialiseChannelMembers(afterCommit(r), c.repo, c.dsvc, row, row.Created); err != nil {
		c.log.ErrorContext(r.Context(), "materialise channel members", "channel", row.ID, "err", err)
	}
	c.announce(r, row)
	if err := server.EncodeBody(w, http.StatusCreated, createChannelResp{
		ChannelID: row.ID, Mode: uint64(row.Mode), Visibility: uint64(row.Visibility),
	}); err != nil {
		c.log.Error("encode create channel", "err", err)
	}
}

// enumByte narrows one of the three channel enums from the uint64 the client
// sent, refusing a value above maxV with 400 E_INVALID_REQUEST and msg.
func enumByte(v uint64, maxV uint8, msg string) (uint8, error) {
	if v > uint64(maxV) {
		return 0, server.Errorf(server.CodeInvalidRequest, "%s", msg)
	}
	return uint8(v), nil //nolint:gosec // G115: v <= maxV, a uint8, on this line
}

// topLevelIfZero reads a parent_id: null or the all-zero id is "no parent".
// The all-zero id is how a PATCH moves a channel to the top level, since null
// there means "leave the parent alone".
func topLevelIfZero(p *id.ID) *id.ID {
	if p == nil || *p == (id.ID{}) {
		return nil
	}
	parent := *p
	return &parent
}

// validChannel holds every structural rule the engine also enforces, and the
// field bounds, so that the client gets a reason code instead of a constraint
// violation. repo is the transaction the write will run in.
func validChannel(ctx context.Context, repo store.Repository, row store.ChannelRow) error {
	bad := func(format string, a ...any) error {
		return server.Errorf(server.CodeInvalidRequest, format, a...)
	}
	switch row.Kind {
	case ChannelText, ChannelVoice, ChannelCategory, ChannelDM, ChannelGroupDM:
	default:
		return bad("kind must be 0..4")
	}
	if row.Mode > ModeReadable {
		return bad("mode must be 0 or 1")
	}
	if row.Visibility > VisDiscoverable {
		return bad("visibility must be 0, 1 or 2")
	}
	if row.Visibility != VisPrivate && row.Mode != ModeReadable {
		return bad("an invite-visible or discoverable channel is server-readable")
	}
	if err := validChannelText("name", row.Name, 1, maxChannelNameBytes, false); err != nil {
		return err
	}
	if err := validChannelText("topic", row.Topic, 0, maxChannelTopicBytes, true); err != nil {
		return err
	}
	if row.Position > maxChannelPosition {
		return bad("position must be at most %d", maxChannelPosition)
	}
	if row.SlowmodeSeconds > maxSlowmodeSeconds {
		return bad("slowmode_seconds must be at most %d", maxSlowmodeSeconds)
	}
	if row.Kind == ChannelCategory && row.ParentID != nil {
		return bad("a category has no parent")
	}
	if row.ParentID != nil {
		if *row.ParentID == row.ID {
			return bad("a channel is not its own parent")
		}
		parent, err := repo.GetChannel(ctx, *row.ParentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return bad("parent_id names no live channel")
			}
			return err
		}
		if parent.Kind != ChannelCategory {
			return bad("parent_id must name a category")
		}
		if row.CommunityID == nil || parent.CommunityID == nil || *parent.CommunityID != *row.CommunityID {
			return bad("parent_id must name a category of the same community")
		}
	}
	return nil
}

// requireParentVisible is R44: a channel's parent must be a category its author can see. It is
// nil for a top-level row. A parent that names no live channel is left to validChannel's own 400;
// one the caller cannot view answers 404 E_NOT_FOUND, as the channel itself would, so a create or
// a move never confirms that a hidden category exists.
func requireParentVisible(ctx context.Context, repo store.Repository, uid id.ID, row store.ChannelRow) error {
	if row.ParentID == nil {
		return nil
	}
	parent, err := repo.GetChannel(ctx, *row.ParentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // validChannel answers it
		}
		return err
	}
	bits, err := NewResolver(repo).Resolve(ctx, uid, parent)
	if err != nil {
		return err
	}
	if !bits.Has(PermViewChannel) {
		return server.Errorf(server.CodeNotFound, "no such object")
	}
	return nil
}

// parentVisible reports whether row has a live parent that uid holds PermViewChannel in: false
// for a top-level row, a parent that names no live channel, and a category uid cannot view.
func parentVisible(ctx context.Context, repo store.Repository, uid id.ID, row store.ChannelRow) (bool, error) {
	if row.ParentID == nil {
		return false, nil
	}
	parent, err := repo.GetChannel(ctx, *row.ParentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	bits, err := NewResolver(repo).Resolve(ctx, uid, parent)
	if err != nil {
		return false, err
	}
	return bits.Has(PermViewChannel), nil
}

// validChannelText bounds a channel's name or topic in bytes, requires UTF-8,
// and refuses control characters: a NUL is legal in a SQLite TEXT column and
// refused by Postgres, so without the check one request would succeed on one
// engine and fail as a 500 on the other. A topic may carry line breaks and tabs.
func validChannelText(field, s string, minBytes, maxBytes int, multiline bool) error {
	if len(s) < minBytes || len(s) > maxBytes {
		return server.Errorf(server.CodeInvalidRequest, "%s must be %d..%d bytes", field, minBytes, maxBytes)
	}
	if !utf8.ValidString(s) {
		return server.Errorf(server.CodeInvalidRequest, "%s is not UTF-8", field)
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return server.Errorf(server.CodeInvalidRequest, "%s contains a control character", field)
		}
	}
	return nil
}

// reservedPosition is the refusal of an ordinary channel at AFKBuiltinPosition or above (R-AFK8).
func reservedPosition() error {
	return server.Errorf(server.CodeInvalidRequest, "position must be below %d, which the AFK channel holds", AFKBuiltinPosition)
}

type patchChannelReq struct {
	_               struct{} `cbor:",toarray"`
	Name            *string
	Topic           *string
	Mode            *uint64
	Visibility      *uint64
	ParentID        *id.ID
	Position        *uint64
	SlowmodeSeconds *uint64
}

func (c *Channels) patch(w http.ResponseWriter, r *http.Request) {
	current, s, err := c.manageable(r, c.repo)
	if err != nil {
		c.fail(w, r, "patch channel", err)
		return
	}
	var req patchChannelReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	// Mode and visibility may not move in the same request: whichever is applied
	// first decides whether the other is legal, so the outcome would depend on
	// the server's evaluation order rather than on what the operator asked for.
	if req.Mode != nil && req.Visibility != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"change mode and visibility in separate requests"))
		return
	}
	var mode, visibility *uint8
	if req.Mode != nil {
		m, err := enumByte(*req.Mode, ModeReadable, "mode must be 0 or 1")
		if err != nil {
			server.WriteError(w, err)
			return
		}
		mode = &m
	}
	if req.Visibility != nil {
		v, err := enumByte(*req.Visibility, VisDiscoverable, "visibility must be 0, 1 or 2")
		if err != nil {
			server.WriteError(w, err)
			return
		}
		visibility = &v
	}
	now := c.clk.Now().Unix()
	var updated store.ChannelRow
	var toClose []id.ID
	unmarked := false
	// The row is read again inside the transaction and the request applied to
	// that, so two concurrent PATCHes of different fields both land.
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := c.lockManageable(r, tx, current); err != nil {
			return err
		}
		// R2: the built-in AFK channel may be renamed and moved to another category, and nothing else.
		// R-AFK8: it stays at AFKBuiltinPosition, last in its run, and every other channel stays below it.
		settings := req.Topic != nil || req.Mode != nil || req.Visibility != nil || req.SlowmodeSeconds != nil
		if settings || req.Position != nil {
			afk, err := tx.GetCommunityAFK(r.Context(), *current.CommunityID)
			if err != nil {
				return err
			}
			isAFK := afk.ChannelID != nil && *afk.ChannelID == current.ID
			if isAFK && settings {
				return server.Errorf(server.CodeForbidden, "the AFK channel keeps its kind, mode, visibility, topic and slow mode")
			}
			if isAFK && req.Position != nil && *req.Position != AFKBuiltinPosition {
				return server.Errorf(server.CodeForbidden, "the AFK channel keeps its position")
			}
			if !isAFK && req.Position != nil && *req.Position >= AFKBuiltinPosition {
				return reservedPosition()
			}
		}
		row, err := tx.GetChannel(r.Context(), current.ID)
		if err != nil {
			return notFound(err)
		}
		before := row
		if req.Name != nil {
			row.Name = *req.Name
		}
		if req.Topic != nil {
			row.Topic = *req.Topic
		}
		if mode != nil {
			row.Mode = *mode
		}
		if visibility != nil {
			row.Visibility = *visibility
			if row.Visibility != VisPrivate {
				row.Mode = ModeReadable
			}
		}
		if req.ParentID != nil {
			row.ParentID = topLevelIfZero(req.ParentID)
		}
		if req.Position != nil {
			row.Position = *req.Position
		}
		if req.SlowmodeSeconds != nil {
			row.SlowmodeSeconds = *req.SlowmodeSeconds
		}
		if req.ParentID != nil {
			if err := requireParentVisible(r.Context(), tx, s.UserID, row); err != nil {
				return err
			}
		}
		if err := validChannel(r.Context(), tx, row); err != nil {
			return err
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, row.CommunityID); err != nil {
			return err
		}
		if err := tx.UpdateChannel(r.Context(), row); err != nil {
			return notFound(err)
		}
		updated = row
		// A channel that stops being end-to-end encrypted is never allowed an MLS
		// text group, so its open text groups are closed after the commit. Only
		// the text kind: a voice channel's call group is untouched.
		// A Bot channel is an end-to-end text channel (Bot v2 Task 9): one that stops being one is
		// unmarked in the same transaction, and the Bot feed hears it after the commit.
		if row.CommunityID != nil && (row.Kind != ChannelText || row.Mode != ModeE2EE) {
			removed, err := tx.UnmarkBotChannel(r.Context(), *row.CommunityID, row.ID)
			if err != nil {
				return err
			}
			unmarked = removed
		}
		if TextGroupAllowed(before) && !TextGroupAllowed(row) {
			groups, err := tx.GroupsForTarget(r.Context(), row.ID, groupText)
			if err != nil {
				return err
			}
			for _, g := range groups {
				toClose = append(toClose, g.GroupID)
			}
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "channel.update", Target: row.ID.String(),
			Detail: row.Name, At: now,
		})
	}); err != nil {
		c.fail(w, r, "patch channel", err)
		return
	}
	if unmarked {
		c.botChannelEvents().BotChannelUnmarked(*updated.CommunityID, updated.ID)
	}
	if err := closeGroups(afterCommit(r), c.dsvc, toClose); err != nil {
		c.log.ErrorContext(r.Context(), "close the text group of a channel that left end-to-end encryption",
			"channel", updated.ID, "err", err)
	}
	// A visibility change re-derives who may be a leaf of the channel's groups
	// (task 7), after the commit and never inside it.
	if visibility != nil {
		if err := MaterialiseChannelMembers(afterCommit(r), c.repo, c.dsvc, updated, now); err != nil {
			c.log.ErrorContext(r.Context(), "materialise channel members", "channel", updated.ID, "err", err)
		}
		// A private channel's members are re-derived: every participant of its live call is
		// reconciled immediately after, on the call retry loop, so one who may no longer be in it is
		// cut; the request only queues it.
		if updated.CommunityID != nil {
			c.calls.RequestSync(afterCommit(r), *updated.CommunityID, nil, &updated.ID)
		}
	}
	c.announce(r, updated)
	w.WriteHeader(http.StatusNoContent)
}

type channelResp struct {
	_               struct{} `cbor:",toarray"`
	ChannelID       id.ID
	CommunityID     *id.ID
	Kind            uint64
	Mode            uint64
	Visibility      uint64
	ParentID        *id.ID
	Name            string
	Topic           string
	Position        uint64
	SlowmodeSeconds uint64
	Seq             uint64
	// TextGroupID is L-HTTP-02's text_group_id (textGroupID, discovery.go).
	TextGroupID *id.ID
}

// listedChannel is one element of GET /v1/communities/{id}/channels: channelResp without community_id, which the path already names. TextGroupID is L-HTTP-02's.
type listedChannel struct {
	_               struct{} `cbor:",toarray"`
	ChannelID       id.ID
	Kind            uint64
	Mode            uint64
	Visibility      uint64
	ParentID        *id.ID
	Name            string
	Topic           string
	Position        uint64
	SlowmodeSeconds uint64
	Seq             uint64
	TextGroupID     *id.ID
}

// list is GET /v1/communities/{id}/channels (fix wave I10): the community's live
// channels the caller may view, ordered by position then channel_id. A category is
// listed only when the caller can view it (R44): a visible child never reveals its
// parent, and a child whose category the caller cannot view is listed with
// parent_id null. A caller who is not a member, like an unknown or deleted
// community, gets 404.
func (c *Channels) list(w http.ResponseWriter, r *http.Request) {
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
	com, err := c.repo.GetCommunity(r.Context(), cid)
	if err != nil || com.DeletedAt != nil {
		c.fail(w, r, "list channels", server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if _, err := c.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		c.fail(w, r, "list channels", notFound(err))
		return
	}
	channels, err := c.repo.ListChannels(r.Context(), cid)
	if err != nil {
		c.fail(w, r, "list channels", err)
		return
	}
	// m-3: the AFK pointer is read once for the whole listing, not once per channel resolved.
	afk, err := c.repo.GetCommunityAFK(r.Context(), cid)
	if err != nil {
		c.fail(w, r, "list channels", err)
		return
	}
	resolveCtx := withAFKPointer(r.Context(), cid, afk)
	visible := make(map[id.ID]bool, len(channels))
	for _, ch := range channels {
		bits, err := c.res.Resolve(resolveCtx, s.UserID, ch)
		if err != nil {
			c.fail(w, r, "list channels", err)
			return
		}
		if bits.Has(PermViewChannel) {
			visible[ch.ID] = true
		}
	}
	out := make([]listedChannel, 0, len(channels))
	for _, ch := range channels {
		if !visible[ch.ID] {
			continue
		}
		tg, err := textGroupID(r.Context(), c.repo, ch)
		if err != nil {
			c.fail(w, r, "list channels", err)
			return
		}
		parent := ch.ParentID
		if parent != nil && !visible[*parent] {
			parent = nil // a category the caller cannot view is never named
		}
		out = append(out, listedChannel{
			ChannelID: ch.ID, Kind: uint64(ch.Kind), Mode: uint64(ch.Mode),
			Visibility: uint64(ch.Visibility), ParentID: parent, Name: ch.Name, Topic: ch.Topic,
			Position: ch.Position, SlowmodeSeconds: ch.SlowmodeSeconds, Seq: ch.Seq, TextGroupID: tg,
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		c.log.Error("encode channel list", "err", err)
	}
}

func (c *Channels) get(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.visible(r)
	if err != nil {
		c.fail(w, r, "get channel", err)
		return
	}
	// R44, as the listing: a visible child never names a category the caller cannot view.
	parent := row.ParentID
	seen, err := parentVisible(r.Context(), c.repo, s.UserID, row)
	if err != nil {
		c.fail(w, r, "get channel", err)
		return
	}
	if !seen {
		parent = nil
	}
	tg, err := textGroupID(r.Context(), c.repo, row)
	if err != nil {
		c.fail(w, r, "get channel", err)
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, channelResp{
		ChannelID: row.ID, CommunityID: row.CommunityID, Kind: uint64(row.Kind),
		Mode: uint64(row.Mode), Visibility: uint64(row.Visibility), ParentID: parent,
		Name: row.Name, Topic: row.Topic, Position: row.Position,
		SlowmodeSeconds: row.SlowmodeSeconds, Seq: row.Seq, TextGroupID: tg,
	}); err != nil {
		c.log.Error("encode channel", "err", err)
	}
}

func (c *Channels) delete(w http.ResponseWriter, r *http.Request) {
	row, s, err := c.manageable(r, c.repo)
	if err != nil {
		c.fail(w, r, "delete channel", err)
		return
	}
	now := c.clk.Now().Unix()
	var toClose []id.ID
	var roleAffected []id.ID
	unmarked := false
	if err := c.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := c.lockManageable(r, tx, row); err != nil {
			return err
		}
		// R2: the built-in AFK channel is never deleted. Read under the community row lock, so a
		// delete racing anything that moves the pointer still sees the pointer as it stands.
		afk, err := tx.GetCommunityAFK(r.Context(), *row.CommunityID)
		if err != nil {
			return err
		}
		if afk.ChannelID != nil && *afk.ChannelID == row.ID {
			return server.Errorf(server.CodeForbidden, "the AFK channel cannot be deleted")
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, row.CommunityID); err != nil {
			return err
		}
		// Read the channel's open groups inside the transaction; close them after it.
		if toClose, err = openChannelGroups(r.Context(), tx, []id.ID{row.ID}); err != nil {
			return err
		}
		if err := tx.DeleteChannel(r.Context(), row.ID, now); err != nil {
			return notFound(err)
		}
		// A deleted channel is no Bot channel (Bot v2 Task 9): the soft delete keeps its row, so the
		// mark is removed here and the Bot feed hears it after the commit.
		if unmarked, err = tx.UnmarkBotChannel(r.Context(), *row.CommunityID, row.ID); err != nil {
			return err
		}
		roleAffected, err = tx.RemoveReactionRoleChannelGrants(r.Context(), *row.CommunityID, row.ID)
		if err != nil {
			return err
		}
		// A deleted category's channels stay, at the top level: the tombstone
		// keeps its row, so ON DELETE SET NULL never fires, and a child left
		// naming it could never be edited again (its parent is not live).
		if row.Kind == ChannelCategory && row.CommunityID != nil {
			siblings, err := tx.ListChannels(r.Context(), *row.CommunityID)
			if err != nil {
				return err
			}
			for _, child := range siblings {
				if child.ParentID == nil || *child.ParentID != row.ID {
					continue
				}
				child.ParentID = nil
				if err := tx.UpdateChannel(r.Context(), child); err != nil {
					return err
				}
			}
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "channel.delete", Target: row.ID.String(),
			Detail: row.Name, At: now,
		})
	}); err != nil {
		c.fail(w, r, "delete channel", err)
		return
	}
	if unmarked {
		c.botChannelEvents().BotChannelUnmarked(*row.CommunityID, row.ID)
	}
	if c.roles != nil {
		for _, user := range roleAffected {
			c.roles.reactionRoleChanged(r.Context(), *row.CommunityID, user)
		}
	}
	// The channel's MLS groups are closed by the delivery service, which owns
	// mls_groups, AFTER the commit: ds.Close writes through the single-connection
	// write pool the transaction held (see RemoveUserFromCommunityGroups). A
	// group left open by a crash here is bound to a deleted channel, which the
	// Add and join ACL refuses.
	if err := closeGroups(afterCommit(r), c.dsvc, toClose); err != nil {
		c.log.ErrorContext(r.Context(), "close the groups of a deleted channel",
			"channel", row.ID, "err", err)
	}
	// Its live call ends with it (CALLS-1), after its call group is closed: ended in the record now,
	// so every gate refuses its room, and its room closed immediately after on the call retry loop.
	c.calls.EndChannelCalls(afterCommit(r), []id.ID{row.ID}, "channel_deleted")
	c.announce(r, row)
	w.WriteHeader(http.StatusNoContent)
}

// visible loads {id} for a caller who holds PermViewChannel in it. Anyone else,
// a non-member included, gets the 404 an unknown channel gets.
func (c *Channels) visible(r *http.Request) (store.ChannelRow, auth.Session, error) {
	return visibleIn(r, c.repo)
}

// visibleIn is visible reading through repo.
func visibleIn(r *http.Request, repo store.Repository) (store.ChannelRow, auth.Session, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return store.ChannelRow{}, s, err
	}
	chID, err := server.PathID(r, "id")
	if err != nil {
		return store.ChannelRow{}, s, err
	}
	row, err := repo.GetChannel(r.Context(), chID)
	if err != nil {
		return store.ChannelRow{}, s, notFound(err)
	}
	// Require answers 404 to a non-member and to a member an overwrite has
	// taken the channel from: neither learns that it exists. For a DM or group
	// DM the members are its participants (channel_members), which is what
	// makes a 1:1 DM's derived id a name rather than a capability (P2-D31).
	if err := NewResolver(repo).Require(r.Context(), s.UserID, row, PermViewChannel); err != nil {
		return store.ChannelRow{}, s, err
	}
	return row, s, nil
}

// manageable is visible plus PermManageChannels in that channel, overwrites
// applied, read through repo. patch and delete call it before their
// transaction, for the refusal's precedence, and again inside it under the
// community row lock.
func (c *Channels) manageable(r *http.Request, repo store.Repository) (store.ChannelRow, auth.Session, error) {
	row, s, err := visibleIn(r, repo)
	if err != nil {
		return row, s, err
	}
	if row.CommunityID == nil {
		return row, s, server.Errorf(server.CodeForbidden, "a DM has no manager")
	}
	if err := NewResolver(repo).Require(r.Context(), s.UserID, row, PermManageChannels); err != nil {
		return row, s, err
	}
	return row, s, nil
}

// lockManageable takes the community row lock in tx and runs manageable again through it, so a
// demotion that committed after a route's checks refuses the write. row is the channel the
// route checked before its transaction; it is a community channel (manageable refused a DM).
func (c *Channels) lockManageable(r *http.Request, tx store.Repository, row store.ChannelRow) error {
	if err := tx.LockCommunity(r.Context(), *row.CommunityID); err != nil {
		return notFound(err)
	}
	_, _, err := c.manageable(r, tx)
	return err
}

// fail writes err as the client's refusal. Anything that is not a
// *server.Error is logged here and reaches the client as an empty E_INTERNAL.
func (c *Channels) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		c.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}
