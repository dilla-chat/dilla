package api

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// readableBodyHeadroom is subtracted from the instance's ciphertext cap to give
// the body cap of a readable message. It is 0: the measured overhead of the
// message.plain frame (90 bytes, TestACapSizedReadableMessageFitsItsFanOutFrame) is
// below the 512 bytes the frame budget adds to the ciphertext cap, so a body at
// the cap still fans out. The test pins that; raise this, in steps of 256, if it
// ever fails.
const readableBodyHeadroom = 0

// minReadableBody is the floor: max_ciphertext_bytes cannot be set below 4096.
const minReadableBody = 4096

// readableBodyCap bounds POST and PATCH of a readable message and POST of a report
// that carries one. It follows the instance's max_ciphertext_bytes, so one number
// governs both channel modes: protocol/04's worst-case legal envelope (10
// attachments + 2 previews + a 4 000-byte body) is 131 643 bytes plus the request
// wrapper, which the 192 KiB default holds with room to spare. §5.3's 64 KiB for
// other CBOR routes is below it (P2-D28), and so was the old 96 KiB.
func readableBodyCap(ciphertextCap int64) int64 {
	if ciphertextCap <= 0 {
		ciphertextCap = defaultMaxCiphertextBytes
	}
	return max(ciphertextCap-readableBodyHeadroom, minReadableBody)
}

// readBody is the cap of the one small body on these routes, PUT read-state.
const readBody = 64 * 1024

// listPage and listMax bound GET /v1/channels/{id}/messages.
const (
	listPage = 50
	listMax  = 256
)

// Fanout is the gateway surface a readable channel needs (P2-D14): protocol/02's
// DeliverGroup, DeliverDevice and DeliverUser cannot reach a channel with no MLS
// group, so *gateway.Gateway gained a channel-keyed audience and a fan-out over
// it. The handler sets the audience from the store before every delivery.
type Fanout interface {
	SetChannelMembers(channelID id.ID, users []id.ID)
	DeliverChannel(channelID id.ID, f gateway.Frame)
}

// The composition root passes its *gateway.Gateway as the Fanout.
var _ Fanout = (*gateway.Gateway)(nil)

// Readable serves the server-readable channel routes of protocol/09 § Readable
// channels: the message stream, its edit and delete verbs, full-text search and
// the read position. Register mounts the handlers bare, as Channels does, and
// each handler requires an enrolled session itself.
type Readable struct {
	repo  store.Repository
	res   *Resolver
	fan   Fanout
	keys  FrankingKeys
	clk   clock.Clock
	log   *slog.Logger
	roles *Roles
	afk   AFKActivity
	// maxCiphertext is the instance's max_ciphertext_bytes; zero means the default.
	maxCiphertext int64
}

// NewReadable wires the routes. fan may be nil, which stores and answers
// without a live fan-out (a build with no gateway); keys may not.
func NewReadable(repo store.Repository, res *Resolver, fan Fanout, keys FrankingKeys, clk clock.Clock, log *slog.Logger) *Readable {
	return &Readable{repo: repo, res: res, fan: fan, keys: keys, clk: clk, log: log}
}

// WithRoles connects role-panel reaction processing and its post-commit
// membership/call synchronization to the readable message route.
func (h *Readable) WithRoles(roles *Roles) *Readable {
	h.roles = roles
	return h
}

// WithMaxCiphertextBytes sets the instance's max_ciphertext_bytes, from which the body cap of a
// readable message is derived, and returns h.
func (h *Readable) WithMaxCiphertextBytes(n int64) *Readable {
	h.maxCiphertext = n
	return h
}

// WithAFK sets where an accepted post reports the sender's activity (AW2, Q1: a message takes the
// sender out of the community's AFK list) and returns h.
func (h *Readable) WithAFK(a AFKActivity) *Readable {
	h.afk = a
	return h
}

// active reports user's activity in channel, when an AFK roster is wired.
func (h *Readable) active(ctx context.Context, user, channel id.ID) {
	if h.afk != nil {
		h.afk.AFKActiveInChannel(ctx, user, channel)
	}
}

func (h *Readable) Register(mux *server.Mux) {
	mux.Socket("POST /v1/channels/{id}/messages", http.HandlerFunc(h.post))
	mux.Socket("GET /v1/channels/{id}/messages", http.HandlerFunc(h.list))
	// The canonical spelling of an edit and a delete. A type-1 or type-2
	// envelope on POST is the alias (P2-D15), which lands in the same
	// applyEdit/applyDelete.
	mux.Socket("PATCH /v1/channels/{id}/messages/{seq}", http.HandlerFunc(h.patch))
	mux.Socket("DELETE /v1/channels/{id}/messages/{seq}", http.HandlerFunc(h.delete))
	mux.Socket("GET /v1/channels/{id}/search", http.HandlerFunc(h.search))
	mux.Socket("PUT /v1/channels/{id}/read-state", http.HandlerFunc(h.readState))
}

// channel loads {id} for a caller holding every bit of want (view_channel is
// always implied), and refuses a channel that is not a server-readable text
// channel. The permission check comes first, so a caller who may not see the
// channel learns nothing about its mode: 404, as for an unknown channel.
func (h *Readable) channel(r *http.Request, want Bits) (auth.Session, store.ChannelRow, Bits, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	chID, err := server.PathID(r, "id")
	if err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	ch, err := h.repo.GetChannel(r.Context(), chID)
	if err != nil {
		return s, store.ChannelRow{}, 0, notFound(err)
	}
	bits, err := h.res.Resolve(r.Context(), s.UserID, ch)
	if err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	if !bits.Has(PermViewChannel) {
		return s, store.ChannelRow{}, 0, server.Errorf(server.CodeNotFound, "no such object")
	}
	if ch.Mode != ModeReadable || ch.Kind != ChannelText || ch.CommunityID == nil {
		// NOT E_MODE_READABLE: protocol/02 defines that code as "text groups are
		// not allowed for this channel, because the channel is server-readable",
		// the opposite condition. E_CHANNEL_MODE (403) is "the operation is not
		// allowed for this channel's mode" (P2-D30).
		return s, store.ChannelRow{}, 0, server.Errorf(server.CodeChannelMode,
			"this channel is not a server-readable text channel; an end-to-end encrypted channel's messages go through POST /v1/groups/{id}/message")
	}
	if !bits.Has(want) {
		return s, store.ChannelRow{}, 0, server.Errorf(server.CodeForbidden, "missing permission")
	}
	return s, ch, bits, nil
}

// envelopeRequest is the body of POST and PATCH: [envelope(bstr)].
type envelopeRequest struct {
	_        struct{} `cbor:",toarray"`
	Envelope []byte
}

// indexedBody is what readable_messages.body holds for an envelope: the text a
// message or an edit carries. A reaction's emoji and a pin's empty body are not
// search content.
//
// Every NUL is removed: Postgres TEXT cannot hold 0x00 (SQLSTATE 22021), so
// the same message would otherwise store on SQLite and fail with a 500 on
// Postgres (fix wave I8). The envelope itself, which franking covers, is stored
// as sent.
func indexedBody(typ uint8, body string) string {
	if typ == EnvMessage || typ == EnvEdit {
		return strings.ReplaceAll(body, "\x00", "")
	}
	return ""
}

type postResponse struct {
	_           struct{} `cbor:",toarray"`
	Seq         uint64
	FrankingTag []byte
	RecvTS      uint64
}

// post appends one envelope: POST /v1/channels/{id}/messages [envelope] ->
// [seq, franking_tag, recv_ts]. A type-1 or type-2 envelope is the alias of
// PATCH or DELETE on the seq its reply_to names (P2-D15) and answers 204.
func (h *Readable) post(w http.ResponseWriter, r *http.Request) {
	s, ch, bits, err := h.channel(r, PermSendMessages)
	if err != nil {
		h.fail(w, r, "post readable message", err)
		return
	}
	var req envelopeRequest
	if err := server.DecodeBody(w, r, readableBodyCap(h.maxCiphertext), &req); err != nil {
		h.fail(w, r, "post readable message", err)
		return
	}
	env, err := ParseEnvelope(req.Envelope)
	if err != nil {
		h.fail(w, r, "post readable message", err)
		return
	}
	switch env.Type {
	case EnvMessage:
		// The ordinary append below.
	case EnvEdit, EnvDelete:
		// The ALIAS path. The canonical spelling is PATCH or DELETE
		// /v1/channels/{id}/messages/{seq}, which take the target from the path;
		// here the target is the seq reply_to carries. An edit is applied in
		// place, not appended: readable history is the server's own record, so
		// a second row would show the old text forever.
		seq, err := targetSeq(env)
		if err == nil {
			if env.Type == EnvEdit {
				err = h.applyEdit(r.Context(), ch, s, seq, env, req.Envelope)
			} else {
				err = h.applyDelete(r.Context(), ch, s, seq)
			}
		}
		if err != nil {
			h.fail(w, r, "post readable message", err)
			return
		}
		h.active(r.Context(), s.UserID, ch.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	case EnvPin, EnvUnpin:
		// Appended like a message; clients read the channel's pin set from
		// the envelope stream, as protocol/04 describes.
		if !bits.Has(PermPinMessages) {
			h.fail(w, r, "post readable message", server.Errorf(server.CodeForbidden, "missing permission"))
			return
		}
	case EnvReactionAdd, EnvReactionRemove:
		// Also appended; the client folds them.
		if !bits.Has(PermAddReactions) {
			h.fail(w, r, "post readable message", server.Errorf(server.CodeForbidden, "missing permission"))
			return
		}
	}
	if env.Type != EnvEdit && env.Type != EnvDelete {
		if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
			h.fail(w, r, "post readable message", err)
			return
		}
	}
	// protocol/04's C, from the envelope and the k_f it carries in the clear.
	// Task 17 recomputes exactly this when it verifies a report.
	c, err := Commitment(req.Envelope, env.KF)
	if err != nil {
		h.fail(w, r, "post readable message", err)
		return
	}
	now := h.clk.Now().Unix()
	keyID, key := h.keys.Current()
	var seq uint64
	var tag []byte
	var roleChanged bool
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		// The community lock serialises this write with moderator timeout changes.
		if err := tx.LockCommunity(r.Context(), *ch.CommunityID); err != nil {
			return notFound(err)
		}
		if err := requireNotTimedOut(r.Context(), tx, *ch.CommunityID, s.UserID, now); err != nil {
			return err
		}
		// NextChannelSeq comes first: it is `UPDATE channels … RETURNING seq`,
		// which serialises every post to this channel on its row (Postgres's
		// row lock; SQLite's single writer). The slowmode read after it
		// therefore sees any post that committed ahead of this one — under
		// Postgres READ COMMITTED each statement takes a fresh snapshot — so two
		// concurrent posts by one user cannot both pass. A refusal rolls the
		// bump back and spends no seq.
		var err error
		if seq, err = tx.NextChannelSeq(r.Context(), ch.ID); err != nil {
			return err
		}
		if err := h.slowmode(r.Context(), tx, ch, s.UserID, bits, now); err != nil {
			return err
		}
		tag = Tag(key, ch.ID, 0 /* no epoch on a readable channel */, seq, s.DeviceID, c, now)
		_, err = tx.PutReadableMessage(r.Context(), store.ReadableMessageRow{
			ChannelID: ch.ID, ChannelHex: ch.ID.String(), Seq: seq, Sender: s.UserID,
			Envelope: req.Envelope, Body: indexedBody(env.Type, env.Body), FrankingTag: tag,
			FrankingKeyID: keyID, MentionCount: uint64(env.MentionCount), Created: now, //nolint:gosec // G115: a count of distinct mentions, never negative
			// The rest of protocol/04's stored tuple (task 17): T binds the uploading
			// device, and a report's first equation is checked against C.
			UploaderDevice: s.DeviceID, CommitmentC: c,
		})
		if err != nil {
			return err
		}
		if h.roles != nil && (env.Type == EnvReactionAdd || env.Type == EnvReactionRemove) {
			roleChanged, err = h.roles.applyRolePanelReaction(r, tx, ch, s, env)
		}
		return err
	})
	if err != nil {
		h.fail(w, r, "post readable message", notFound(err))
		return
	}
	h.deliver(r.Context(), ch.ID, seq, s.UserID, req.Envelope, tag, 0, 0)
	if roleChanged {
		h.roles.reactionRoleChanged(afterCommit(r), *ch.CommunityID, s.UserID)
	}
	h.active(r.Context(), s.UserID, ch.ID)
	if err := server.EncodeBody(w, http.StatusOK, postResponse{Seq: seq, FrankingTag: tag, RecvTS: uint64(now)}); err != nil { //nolint:gosec // G115: a unix second from the instance clock
		h.log.Error("encode readable post", "err", err)
	}
}

// slowmode refuses a send inside channels.slowmode_seconds of this user's
// previous message, unless they hold PermBypassSlowmode. A deleted message still
// counts, so delete-and-repost does not reset the gate.
func (h *Readable) slowmode(ctx context.Context, tx store.Repository, ch store.ChannelRow, userID id.ID, bits Bits, now int64) error {
	if ch.SlowmodeSeconds == 0 || bits.Has(PermBypassSlowmode) {
		return nil
	}
	last, err := tx.LastReadableMessageAt(ctx, ch.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return slowmodeWait(ch.SlowmodeSeconds, last, now)
}

// slowmodeWait is slow mode's arithmetic, shared by readable channels and the end-to-end encrypted
// post policy (web3.7 SB1): nil once seconds have passed since last, else 429 E_RATE_LIMITED with
// the remaining wait in retry_after_ms and the Detail "slowmode: N seconds left".
func slowmodeWait(seconds uint64, last, now int64) error {
	wait := last + int64(seconds) - now //nolint:gosec // G115: bounded by maxSlowmodeSeconds on write
	if wait <= 0 {
		return nil
	}
	e := server.RateLimited(uint64(wait) * 1000)
	e.Detail = "slowmode: " + strconv.FormatInt(wait, 10) + " seconds left"
	return e
}

// deliver sends one message.plain to the channel's audience: its
// channel_members that are still members of its community. The audience is
// read at delivery time, so a user who left or lost the channel is not sent it.
// A failure is logged: the message is stored, and a client catches up through
// GET /v1/channels/{id}/messages.
func (h *Readable) deliver(ctx context.Context, channelID id.ID, seq uint64, sender id.ID, envelope, tag []byte, edited, deleted uint64) {
	if h.fan == nil {
		return
	}
	users, err := h.repo.ListReadableAudience(ctx, channelID)
	if err != nil {
		h.log.ErrorContext(ctx, "readable audience", "channel", channelID, "err", err)
		return
	}
	payload, err := gateway.MessagePlainPayload(channelID, seq, sender, envelope, tag, edited, deleted)
	if err != nil {
		h.log.ErrorContext(ctx, "message.plain payload", "channel", channelID, "err", err)
		return
	}
	h.fan.SetChannelMembers(channelID, users)
	h.fan.DeliverChannel(channelID, gateway.Frame{
		Op: gateway.OpMessagePlain, Payload: payload, Replay: true,
		Scope: gateway.FrameScope{Channels: []id.ID{channelID}},
	})
}

type listItem struct {
	_           struct{} `cbor:",toarray"`
	Seq         uint64
	Sender      id.ID
	Envelope    []byte
	FrankingTag []byte
	Created     uint64
	Edited      *uint64
	Deleted     *uint64
}

// list is GET /v1/channels/{id}/messages?from=&limit= ->
// [[seq, sender, envelope, franking_tag, created, edited|null, deleted|null]].
// A deleted message is a tombstone: an empty envelope and its deleted time.
func (h *Readable) list(w http.ResponseWriter, r *http.Request) {
	s, ch, _, err := h.channel(r, PermReadHistory)
	if err != nil {
		h.fail(w, r, "list readable messages", err)
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		h.fail(w, r, "list readable messages", err)
		return
	}
	from := queryFrom(r, "from")
	limit := queryLimit(r, "limit", listPage)
	if limit <= 0 {
		limit = listPage
	}
	limit = min(limit, listMax)
	rows, err := h.repo.ListReadableMessages(r.Context(), ch.ID, from, limit)
	if err != nil {
		h.fail(w, r, "list readable messages", err)
		return
	}
	out := make([]listItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, listItem{
			Seq: row.Seq, Sender: row.Sender, Envelope: row.Envelope, FrankingTag: row.FrankingTag,
			Created: uint64(row.Created), Edited: uintPtr(row.Edited), Deleted: uintPtr(row.Deleted), //nolint:gosec // G115: unix seconds the instance wrote
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		h.log.Error("encode readable list", "err", err)
	}
}

func uintPtr(v *int64) *uint64 {
	if v == nil {
		return nil
	}
	u := uint64(*v) //nolint:gosec // G115: a unix second the instance wrote
	return &u
}

// seqFromPath parses {seq} as a decimal uint64 a store can hold. It is the
// canonical way an edit or a delete names its target.
func seqFromPath(r *http.Request) (uint64, error) {
	n, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil || n > math.MaxInt64 {
		return 0, server.Errorf(server.CodeInvalidRequest, "seq must be a decimal uint64")
	}
	return n, nil
}

// patch is PATCH /v1/channels/{id}/messages/{seq} [envelope] -> 204.
func (h *Readable) patch(w http.ResponseWriter, r *http.Request) {
	s, ch, _, err := h.channel(r, PermSendMessages)
	if err != nil {
		h.fail(w, r, "edit readable message", err)
		return
	}
	seq, err := seqFromPath(r)
	if err != nil {
		h.fail(w, r, "edit readable message", err)
		return
	}
	var req envelopeRequest
	if err := server.DecodeBody(w, r, readableBodyCap(h.maxCiphertext), &req); err != nil {
		h.fail(w, r, "edit readable message", err)
		return
	}
	env, err := ParseEnvelope(req.Envelope)
	if err != nil {
		h.fail(w, r, "edit readable message", err)
		return
	}
	if err := h.applyEdit(r.Context(), ch, s, seq, env, req.Envelope); err != nil {
		h.fail(w, r, "edit readable message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// delete is DELETE /v1/channels/{id}/messages/{seq} -> 204.
func (h *Readable) delete(w http.ResponseWriter, r *http.Request) {
	s, ch, _, err := h.channel(r, PermViewChannel)
	if err != nil {
		h.fail(w, r, "delete readable message", err)
		return
	}
	seq, err := seqFromPath(r)
	if err != nil {
		h.fail(w, r, "delete readable message", err)
		return
	}
	if err := h.applyDelete(r.Context(), ch, s, seq); err != nil {
		h.fail(w, r, "delete readable message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// target reads the live message at seq, or answers 404.
func (h *Readable) target(ctx context.Context, repo store.Repository, ch store.ChannelRow, seq uint64) (store.ReadableMessageRow, error) {
	rows, err := repo.ListReadableMessages(ctx, ch.ID, seq, 1)
	if err != nil {
		return store.ReadableMessageRow{}, err
	}
	if len(rows) == 0 || rows[0].Seq != seq || rows[0].Deleted != nil {
		return store.ReadableMessageRow{}, server.Errorf(server.CodeNotFound, "no such message")
	}
	return rows[0], nil
}

// applyEdit replaces the envelope and the indexed body of the author's own
// message. It is reached from PATCH /v1/channels/{id}/messages/{seq}
// (canonical) and from a type-1 envelope on POST (the P2-D15 alias); both give
// it the target seq and the already validated envelope.
//
// The franking tag is RECOMPUTED over the new bytes, by the editing device, at
// the edit's time (which the row records as edited), under the current key, and
// the key id, the editing device and the new C move with it (task 17), so the
// stored tuple still recomputes to the stored tag. Keeping the upload's tag
// would leave a tag that commits to text the instance no longer stores, so a
// report against an edited message could never verify.
func (h *Readable) applyEdit(ctx context.Context, ch store.ChannelRow, s auth.Session, seq uint64, env Envelope, envelope []byte) error {
	if env.Type != EnvMessage && env.Type != EnvEdit {
		return server.Errorf(server.CodeEnvelopeType, "an edit carries a type-0 or type-1 envelope")
	}
	if ch.CommunityID == nil {
		return server.Errorf(server.CodeForbidden, "missing permission")
	}
	var original store.ReadableMessageRow
	var tag []byte
	err := h.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockCommunity(ctx, *ch.CommunityID); err != nil {
			return notFound(err)
		}
		if err := requireNotTimedOut(ctx, tx, *ch.CommunityID, s.UserID, h.clk.Now().Unix()); err != nil {
			return err
		}
		row, err := h.target(ctx, tx, ch, seq)
		if err != nil {
			return err
		}
		original = row
		if row.Sender != s.UserID {
			return server.Errorf(server.CodeForbidden, "only the author may edit a message")
		}
		if err := requireCommunityWire(ctx, tx, s.WireVersion, ch.CommunityID); err != nil {
			return err
		}
		c, err := Commitment(envelope, env.KF)
		if err != nil {
			return err
		}
		now := h.clk.Now().Unix()
		keyID, key := h.keys.Current()
		tag = Tag(key, ch.ID, 0, seq, s.DeviceID, c, now)
		f := store.ReadableFranking{Tag: tag, KeyID: keyID, UploaderDevice: s.DeviceID, CommitmentC: c}
		if err := tx.EditReadableMessage(ctx, ch.ID, seq, envelope, indexedBody(env.Type, env.Body), f, now); err != nil {
			return notFound(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.deliver(ctx, ch.ID, seq, original.Sender, envelope, tag, 1, 0)
	return nil
}

func requireNotTimedOut(ctx context.Context, repo store.Repository, communityID, userID id.ID, now int64) error {
	_, err := repo.GetMemberTimeout(ctx, communityID, userID, now)
	if err == nil {
		return server.Errorf(server.CodeForbidden, "member is timed out")
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// applyDelete tombstones the row: the envelope and the body are emptied, so the
// message leaves the search index, while the franking tuple survives for the
// report path of task 17. Delete-for-everyone is the uploader's alone (R29):
// anyone else, manage_messages or not, is 403 E_NOT_UPLOADER. Moderator
// deletion needs a signed moderation event, which is follow-up card 2.
func (h *Readable) applyDelete(ctx context.Context, ch store.ChannelRow, s auth.Session, seq uint64) error {
	row, err := h.target(ctx, h.repo, ch, seq)
	if err != nil {
		return err
	}
	if row.Sender != s.UserID {
		return server.Errorf(server.CodeNotUploader, "only the author may delete a message")
	}
	if err := requireCommunityWire(ctx, h.repo, s.WireVersion, ch.CommunityID); err != nil {
		return err
	}
	var affected []id.ID
	err = h.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockCommunity(ctx, *ch.CommunityID); err != nil {
			return notFound(err)
		}
		if err := tx.DeleteReadableMessage(ctx, ch.ID, seq, h.clk.Now().Unix()); err != nil {
			return notFound(err)
		}
		var err error
		affected, err = tx.RemoveReactionRolePanelGrants(ctx, *ch.CommunityID, ch.ID, seq)
		if err != nil {
			return err
		}
		if err := tx.DeleteRolePanel(ctx, ch.ID, seq); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if h.roles != nil {
		for _, user := range affected {
			h.roles.reactionRoleChanged(ctx, *ch.CommunityID, user)
		}
	}
	h.deliver(ctx, ch.ID, seq, row.Sender, nil, row.FrankingTag, 0, 1)
	return nil
}

// targetSeq reads the target of a type-1 or type-2 envelope on the alias path.
// On a server-readable channel reply_to carries the target's channel seq as a
// big-endian uint64 in the low eight bytes, the high eight zero (P2-D15): the
// instance keys a readable message by (channel, seq) and holds no msg_id index.
// A missing reference, or one whose high bytes are set (a msg_id, which only an
// end-to-end encrypted group uses), is E_ENVELOPE_SHAPE.
func targetSeq(env Envelope) (uint64, error) {
	if env.ReplyTo == nil {
		return 0, server.Errorf(server.CodeEnvelopeShape, "an edit or a delete names its target's seq in reply_to")
	}
	ref := *env.ReplyTo
	if binary.BigEndian.Uint64(ref[:8]) != 0 {
		return 0, server.Errorf(server.CodeEnvelopeShape,
			"on a server-readable channel reply_to carries the target's seq in its low eight bytes, not a msg_id")
	}
	seq := binary.BigEndian.Uint64(ref[8:])
	if seq > math.MaxInt64 {
		return 0, server.Errorf(server.CodeNotFound, "no such message")
	}
	return seq, nil
}

type searchItem struct {
	_           struct{} `cbor:",toarray"`
	ChannelID   id.ID
	Seq         uint64
	Sender      id.ID
	Snippet     string
	ScoreMicros uint64
	Created     uint64
}

// search is GET /v1/channels/{id}/search?q=&limit=&before= ->
// [[channel_id, seq, sender, snippet, score_micros, created]]. The scope is every
// server-readable text channel of {id}'s community the caller may view and read
// the history of, passed whole to the store, which chunks it rather than
// dropping any. score travels as round(score x 1e6): deterministic CBOR carries
// no floats.
func (h *Readable) search(w http.ResponseWriter, r *http.Request) {
	s, ch, _, err := h.channel(r, PermReadHistory)
	if err != nil {
		h.fail(w, r, "search readable channels", err)
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		h.fail(w, r, "search readable channels", err)
		return
	}
	q, err := store.ParseQuery(r.URL.Query().Get("q"))
	if errors.Is(err, store.ErrEmptyQuery) {
		h.fail(w, r, "search readable channels", server.Errorf(server.CodeInvalidRequest, "q has no searchable term"))
		return
	}
	if err != nil {
		h.fail(w, r, "search readable channels", err)
		return
	}
	scope, err := h.searchScope(r.Context(), s.UserID, *ch.CommunityID)
	if err != nil {
		h.fail(w, r, "search readable channels", err)
		return
	}
	hits, err := h.repo.SearchReadable(r.Context(), store.ReadableSearchQuery{
		ChannelIDs: scope, Query: q, Limit: queryLimit(r, "limit", store.DefaultSearchLimit),
		BeforeSeq: queryFrom(r, "before"),
	})
	if err != nil {
		h.fail(w, r, "search readable channels", err)
		return
	}
	out := make([]searchItem, 0, len(hits))
	for _, hit := range hits {
		out = append(out, searchItem{
			ChannelID: hit.ChannelID, Seq: hit.Seq, Sender: hit.Sender, Snippet: hit.Snippet,
			ScoreMicros: scoreMicros(hit.Score), Created: uint64(hit.Created), //nolint:gosec // G115: a unix second the instance wrote
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		h.log.Error("encode readable search", "err", err)
	}
}

// scoreMicros is round(score x 1e6), and 0 for a score that is not positive.
func scoreMicros(score float32) uint64 {
	v := math.Round(float64(score) * 1e6)
	if !(v > 0) {
		return 0
	}
	return uint64(v)
}

// searchScope is the community's server-readable text channels the user may
// view and read the history of. The community, its roles and the user's grants
// are read once; per channel only its overwrites are.
func (h *Readable) searchScope(ctx context.Context, userID, communityID id.ID) ([]id.ID, error) {
	snap, err := LoadSnapshot(ctx, h.repo, communityID, userID, nil)
	if err != nil {
		return nil, notFound(err)
	}
	channels, err := h.repo.ListChannels(ctx, communityID)
	if err != nil {
		return nil, err
	}
	var scope []id.ID
	for _, c := range channels {
		if c.Mode != ModeReadable || c.Kind != ChannelText {
			continue
		}
		if snap.Overwrite, err = h.repo.ListOverwrites(ctx, c.ID); err != nil {
			return nil, err
		}
		if snap.ResolveChannel(userID).Has(PermViewChannel | PermReadHistory) {
			scope = append(scope, c.ID)
		}
	}
	return scope, nil
}

type readStateRequest struct {
	_           struct{} `cbor:",toarray"`
	LastReadSeq uint64
}

// readState is PUT /v1/channels/{id}/read-state [last_read_seq] -> 204. The
// position never passes the channel's newest seq and never moves backwards (the
// store keeps the greater), so neither a stale tab nor a client that marks
// "everything, forever" read can distort it.
func (h *Readable) readState(w http.ResponseWriter, r *http.Request) {
	s, ch, _, err := h.channel(r, PermViewChannel)
	if err != nil {
		h.fail(w, r, "put read state", err)
		return
	}
	var req readStateRequest
	if err := server.DecodeBody(w, r, readBody, &req); err != nil {
		h.fail(w, r, "put read state", err)
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		h.fail(w, r, "put read state", err)
		return
	}
	if err := h.repo.PutReadState(r.Context(), s.UserID, ch.ID, min(req.LastReadSeq, ch.Seq)); err != nil {
		h.fail(w, r, "put read state", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fail writes err as the client's refusal. Anything that is not a
// *server.Error is logged here and reaches the client as an empty E_INTERNAL.
func (h *Readable) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		h.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}
