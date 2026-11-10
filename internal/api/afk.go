package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// AFKBuiltinPosition is where the built-in AFK channel is written (R42): below every channel a client
// creates, which skips it when it computes max(position) + 1, and below maxChannelPosition.
const AFKBuiltinPosition = 1 << 30

// afkBuiltinName is the server-written name of the built-in AFK channel; a Go literal, not localised.
const afkBuiltinName = "AFK"

// afkBuiltinDetail is the audit detail of the system row that marks a channel as the built-in AFK.
const afkBuiltinDetail = "afk.builtin"

// afkBuiltinSettings is the built-in AFK channel's settings_json (R-AFK1): the marker the backfill
// reads to tell the built-in channel from a web3.8 pick. No route writes settings_json and it is not on
// the wire, so the marker cannot be lost or forged by a client.
const afkBuiltinSettings = `{"afk":"builtin"}`

// isBuiltinAFK reports whether ch carries the built-in marker.
func isBuiltinAFK(ch store.ChannelRow) bool { return string(ch.SettingsJSON) == afkBuiltinSettings }

// afkPendingTTL is the longest a device's pending cut lasts (R4, R33): it stays until an admission of
// the device clears it, through gateway disconnects and any number of snapshot reads, so a laptop
// that slept for hours still learns why it left its call. It is held in memory only: a restart ends
// every call anyway.
const afkPendingTTL = 24 * time.Hour

// The snapshot's pending reason codes (uint; 0 is none; a client that knows only 0|1 reads any
// non-zero as pending).
const (
	afkPendingSwept = 1 // the sweep moved the device (idle past the timeout)
	afkPendingChose = 2 // the user chose AFK: PUT …/afk/away cut the device
)

// afkDefaultTimeout is the timeout a community starts with, in seconds (5 minutes).
const afkDefaultTimeout = 300

// afkBackfillPage is one page of ListCommunities in EnsureAFKChannels.
const afkBackfillPage = 500

type afkTracked struct {
	community, channel, user id.ID
	room                     string
	lastActive               time.Time
}

// afkPendingEntry is a cut of one device (R33, R-AW6): the sweep's move (afkPendingSwept) or a
// chosen AFK (afkPendingChose). It is device-scoped: every community's snapshot reports it to that
// device, whose client only tells the user why it left, and never joins anything on it.
type afkPendingEntry struct {
	reason uint64
	at     time.Time
}

// setAFKPendingLocked records a pending cut of dev; a later one replaces an earlier. The caller
// holds afkMu.
func (h *Calls) setAFKPendingLocked(dev id.ID, reason uint64) {
	if h.afkPending == nil {
		h.afkPending = make(map[id.ID]afkPendingEntry)
	}
	h.afkPending[dev] = afkPendingEntry{reason: reason, at: h.clk.Now()}
}

// afkSnapshot is GET /v1/communities/{id}/afk: [channel_id|null, timeout_seconds, [], pending(uint
// 0|1), away([[user_id, since_ts]])]. The third element is the retired web3.8 occupant list, always
// empty, so a web3.8 parser still reads it; the dilla CBOR subset has no booleans (W-1). The fifth is
// the stored away roster (AW2), ordered by user id, at most afkAwaySnapshotMax entries; a client
// that reads four elements is unaffected.
type afkSnapshot struct {
	_              struct{} `cbor:",toarray"`
	ChannelID      *id.ID
	TimeoutSeconds uint64
	Retired        []struct{}
	Pending        uint64
	Away           []afkAwayEntry
}

// isAFKTimeout is the timeouts an administrator may pick: 1, 5, 15, 30 or 60 minutes.
func isAFKTimeout(seconds uint64) bool {
	switch seconds {
	case 60, 300, 900, 1800, 3600:
		return true
	}
	return false
}

// createBuiltinAFK writes the channel row (R1), its system audit row and the community_afk pointer
// (timeout kept, else 300) in tx. The caller holds the community row lock or created the community in tx.
func createBuiltinAFK(ctx context.Context, tx store.Repository, com store.CommunityRow, timeout uint64, now int64) (store.ChannelRow, error) {
	if !isAFKTimeout(timeout) {
		timeout = afkDefaultTimeout
	}
	cid := com.ID
	row := store.ChannelRow{
		ID:                id.New(),
		CommunityID:       &cid,
		Kind:              ChannelVoice,
		Mode:              ModeE2EE,
		Visibility:        VisPrivate,
		Name:              afkBuiltinName,
		Position:          AFKBuiltinPosition,
		SettingsJSON:      []byte(afkBuiltinSettings),
		HostPolicyVersion: com.PolicyVersion,
		Created:           now,
	}
	if err := tx.CreateChannel(ctx, row); err != nil {
		return store.ChannelRow{}, err
	}
	if err := tx.Audit(ctx, store.AuditRow{
		Actor: nil, Action: "channel.create", Target: row.ID.String(), Detail: afkBuiltinDetail, At: now,
	}); err != nil {
		return store.ChannelRow{}, err
	}
	if err := tx.SetCommunityAFK(ctx, cid, store.CommunityAFK{ChannelID: &row.ID, TimeoutSeconds: timeout}); err != nil {
		return store.ChannelRow{}, err
	}
	return row, nil
}

// ensureCommunityAFK is the per-community transaction of the backfill and of the lazy ensure. It
// takes the community row lock, reads the pointer and the channel it names as they stand under the
// lock, and asks done whether that channel will do. If it will not, a new built-in channel is written
// with the stored timeout kept; the old pointed channel is left as it is, and from this commit on it is
// an ordinary voice channel. If the pointed channel is the built-in one, its position, mode,
// visibility, topic and slow mode are put back as built (R-AFK8, M-2) and any overwrite on it is removed (R-AFK2: a rollback to web3.8 could
// have written one). Either way, every other channel at AFKBuiltinPosition or above is moved below it
// (R-AFK8, lowerStrayChannels). After the commit the members of the channel that was created or cleaned
// are materialised. created is true only when a channel was written.
func ensureCommunityAFK(ctx context.Context, repo store.Repository, dsvc DS, cid id.ID, now int64,
	done func(ch store.ChannelRow, live bool) bool, log *slog.Logger,
) (store.ChannelRow, bool, error) {
	var afk store.ChannelRow
	created, cleaned := false, false
	err := repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockCommunity(ctx, cid); err != nil {
			return err
		}
		com, err := tx.GetCommunity(ctx, cid)
		if err != nil {
			return err
		}
		cfg, err := tx.GetCommunityAFK(ctx, cid)
		if err != nil {
			return err
		}
		ch, live, err := livePointer(ctx, tx, cid, cfg)
		if err != nil {
			return err
		}
		if done(ch, live) {
			if !live || !isBuiltinAFK(ch) {
				return nil
			}
			var restored, stripped bool
			if afk, restored, err = restoreBuiltinAFK(ctx, tx, ch, now); err != nil {
				return err
			}
			if err := lowerStrayChannels(ctx, tx, cid, afk.ID, now); err != nil {
				return err
			}
			stripped, err = stripAFKOverwrites(ctx, tx, afk, now)
			cleaned = restored || stripped
			return err
		}
		afk, err = createBuiltinAFK(ctx, tx, com, cfg.TimeoutSeconds, now)
		if err != nil {
			return err
		}
		created = true
		return lowerStrayChannels(ctx, tx, cid, afk.ID, now)
	})
	if err != nil {
		return afk, false, err
	}
	if created || cleaned {
		if err := MaterialiseChannelMembers(ctx, repo, dsvc, afk, now); err != nil {
			log.ErrorContext(ctx, "materialise channel members", "channel", afk.ID, "err", err)
		}
	}
	return afk, created, nil
}

// builtinAFKDrift lists what of the built-in AFK channel ch differs from how createBuiltinAFK writes it,
// as audit details naming the value it had: its position (R-AFK8: an earlier web3.9 build accepted a
// PATCH of it), and its mode, visibility, topic and slow mode (M-2: a web3.8 rollback accepts a PATCH of
// them, which web3.9 then refuses to undo). The name is the administrator's and is never reset.
func builtinAFKDrift(ch store.ChannelRow) []string {
	var out []string
	if ch.Position != AFKBuiltinPosition {
		out = append(out, fmt.Sprintf("afk.position:%d", ch.Position))
	}
	if ch.Mode != ModeE2EE {
		out = append(out, fmt.Sprintf("afk.mode:%d", ch.Mode))
	}
	if ch.Visibility != VisPrivate {
		out = append(out, fmt.Sprintf("afk.visibility:%d", ch.Visibility))
	}
	if ch.Topic != "" {
		out = append(out, "afk.topic")
	}
	if ch.SlowmodeSeconds != 0 {
		out = append(out, fmt.Sprintf("afk.slowmode:%d", ch.SlowmodeSeconds))
	}
	return out
}

// restoreBuiltinAFK puts every field builtinAFKDrift reports back to its built-in value, with one
// system audit row per field, and returns the row as it now stands and whether anything changed. The
// caller holds the community row lock.
func restoreBuiltinAFK(ctx context.Context, tx store.Repository, ch store.ChannelRow, now int64) (store.ChannelRow, bool, error) {
	drift := builtinAFKDrift(ch)
	if len(drift) == 0 {
		return ch, false, nil
	}
	ch.Position, ch.Mode, ch.Visibility, ch.Topic, ch.SlowmodeSeconds = AFKBuiltinPosition, ModeE2EE, VisPrivate, "", 0
	if err := tx.UpdateChannel(ctx, ch); err != nil {
		return store.ChannelRow{}, false, err
	}
	for _, d := range drift {
		if err := tx.Audit(ctx, store.AuditRow{
			Actor: nil, Action: "channel.update", Target: ch.ID.String(), Detail: d, At: now,
		}); err != nil {
			return store.ChannelRow{}, false, err
		}
	}
	return ch, true, nil
}

// lowerStrayChannels moves every live channel of community cid other than the AFK channel afkID that sits
// at AFKBuiltinPosition or above (R-AFK8: a rolled-back web3.8 numbers a new channel max(position) + 1)
// down to max(position below 2^30) + 1, + 2, … in their current order (position, then channel_id), each
// with a null-actor channel.update audit row whose detail names the position it left (afk.reserved:<old>).
// When they do not fit there (I-1: a channel already at or near 2^30-1), every channel other than afkID is
// renumbered densely 0..n-1 in that order instead, each one that moves with an afk.renumber:<old> row, so
// the lowering itself never fails and the rest of the ensure still commits. The caller holds the
// community row lock.
func lowerStrayChannels(ctx context.Context, tx store.Repository, cid, afkID id.ID, now int64) error {
	all, err := tx.ListChannels(ctx, cid) // ordered by position, then channel_id
	if err != nil {
		return err
	}
	var next uint64
	var ordinary, stray []store.ChannelRow
	for _, c := range all {
		switch {
		case c.ID == afkID:
			continue
		case c.Position < AFKBuiltinPosition:
			next = max(next, c.Position+1)
		default:
			stray = append(stray, c)
		}
		ordinary = append(ordinary, c)
	}
	if len(stray) == 0 {
		return nil
	}
	moves, detail := stray, "afk.reserved:%d"
	if next+uint64(len(stray)) > AFKBuiltinPosition {
		moves, detail, next = ordinary, "afk.renumber:%d", 0
	}
	for _, c := range moves {
		was := c.Position
		c.Position = next
		next++
		if was == c.Position {
			continue
		}
		if err := tx.UpdateChannel(ctx, c); err != nil {
			return err
		}
		if err := tx.Audit(ctx, store.AuditRow{
			Actor: nil, Action: "channel.update", Target: c.ID.String(),
			Detail: fmt.Sprintf(detail, was), At: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// stripAFKOverwrites deletes every overwrite on the built-in AFK channel ch, with a system audit row
// each (R-AFK2), and reports whether there was one. The caller holds the community row lock.
func stripAFKOverwrites(ctx context.Context, tx store.Repository, ch store.ChannelRow, now int64) (bool, error) {
	rows, err := tx.ListOverwrites(ctx, ch.ID)
	if err != nil {
		return false, err
	}
	for _, o := range rows {
		if err := tx.DeleteOverwrite(ctx, ch.ID, o.TargetKind, o.TargetID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if err := tx.Audit(ctx, store.AuditRow{
			Actor: nil, Action: "channel.overwrite.delete", Target: ch.ID.String(),
			Detail: fmt.Sprintf("%d:%s", o.TargetKind, o.TargetID), At: now,
		}); err != nil {
			return false, err
		}
	}
	return len(rows) > 0, nil
}

// livePointer reports whether cfg names a live channel of community cid.
func livePointer(ctx context.Context, repo store.Repository, cid id.ID, cfg store.CommunityAFK) (store.ChannelRow, bool, error) {
	if cfg.ChannelID == nil {
		return store.ChannelRow{}, false, nil
	}
	ch, err := repo.GetChannel(ctx, *cfg.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return ch, false, nil
	}
	if err != nil {
		return ch, false, err
	}
	return ch, ch.CommunityID != nil && *ch.CommunityID == cid, nil
}

// builtinDone is the backfill's question (R1, R-AFK1): the pointer names a live channel of the
// community that carries the built-in marker.
func builtinDone(ch store.ChannelRow, live bool) bool { return live && isBuiltinAFK(ch) }

// EnsureAFKChannels backfills every live community that is not done (R1). Never inside a request.
//
// Done means the community's pointer names a live channel of the community whose settings_json is
// the built-in marker (R-AFK1), decided inside the community's own transaction under its row lock, so
// two processes that run it at once against one database still write one channel per community. Every
// other live community (no row, a NULL pointer, or a web3.8 pointer at an administrator's pick) gets
// a new built-in channel. A built-in channel that carries an overwrite loses it (R-AFK2). A failure
// for one community is logged and the others go on; the answer is how many channels were created.
func EnsureAFKChannels(ctx context.Context, repo store.Repository, dsvc DS, clk clock.Clock, log *slog.Logger) (int, error) {
	n := 0
	var after id.ID
	for {
		page, err := repo.ListCommunities(ctx, after, afkBackfillPage)
		if err != nil {
			return n, err
		}
		for _, com := range page {
			skip, err := afkSettled(ctx, repo, com.ID)
			if err != nil {
				log.ErrorContext(ctx, "afk backfill", "community", com.ID, "err", err)
				continue
			}
			if skip {
				continue
			}
			_, created, err := ensureCommunityAFK(ctx, repo, dsvc, com.ID, clk.Now().Unix(), builtinDone, log)
			if err != nil {
				log.ErrorContext(ctx, "afk backfill", "community", com.ID, "err", err)
				continue
			}
			if created {
				n++
			}
		}
		if len(page) < afkBackfillPage {
			break
		}
		after = page[len(page)-1].ID
	}
	if n > 0 {
		log.Info("afk backfill", "created", n)
	}
	return n, nil
}

// afkSettled is the backfill's read before any lock: the pointer already names the community's
// built-in channel, with its built-in settings (builtinAFKDrift) and no overwrite, and no other channel
// of the community sits at AFKBuiltinPosition or above, so there is nothing to write. Anything else goes
// on to the transaction, which decides again under the lock.
func afkSettled(ctx context.Context, repo store.Repository, cid id.ID) (bool, error) {
	cfg, err := repo.GetCommunityAFK(ctx, cid)
	if err != nil {
		return false, err
	}
	ch, live, err := livePointer(ctx, repo, cid, cfg)
	if err != nil || !builtinDone(ch, live) || len(builtinAFKDrift(ch)) > 0 {
		return false, err
	}
	ow, err := repo.ListOverwrites(ctx, ch.ID)
	if err != nil || len(ow) > 0 {
		return false, err
	}
	all, err := repo.ListChannels(ctx, cid)
	if err != nil {
		return false, err
	}
	for _, c := range all {
		if c.ID != ch.ID && c.Position >= AFKBuiltinPosition {
			return false, nil
		}
	}
	return true, nil
}

// ensureAFK is the lazy ensure of GET /v1/communities/{id}/afk (R1): the backfill's per-community
// transaction, for a pointer that is NULL or names no live channel.
func (h *Calls) ensureAFK(ctx context.Context, cid id.ID) (store.CommunityAFK, error) {
	_, created, err := ensureCommunityAFK(ctx, h.repo, h.dsvc, cid, h.clk.Now().Unix(),
		func(_ store.ChannelRow, live bool) bool { return live }, h.log)
	if err != nil {
		return store.CommunityAFK{}, err
	}
	if created {
		h.afkNotify.Changed(context.WithoutCancel(ctx), cid)
	}
	return h.repo.GetCommunityAFK(ctx, cid)
}

// inAFK reports whether row is a call of its community's AFK channel (R34). A read error counts as
// AFK: the grant paths that ask fail closed, to listen-only.
func (h *Calls) inAFK(ctx context.Context, row store.VoiceSessionRow) bool {
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return false // a deleted channel's call is over; every gate refuses it anyway
	}
	if err != nil {
		return true
	}
	if ch.CommunityID == nil {
		return false
	}
	cfg, err := h.repo.GetCommunityAFK(ctx, *ch.CommunityID)
	if err != nil {
		return true
	}
	return cfg.ChannelID != nil && *cfg.ChannelID == ch.ID
}

// An SFU admission starts the device's timer even if its browser never gets to send
// the first activity report. A signalling resume keeps the original timer. The AFK channel's own
// calls are never tracked, and neither is a device that reported itself exempt for this room (R5).
func (h *Calls) trackAFKAdmission(ctx context.Context, row store.VoiceSessionRow, dev id.ID, resume bool) {
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if err != nil || ch.CommunityID == nil || ch.Kind != ChannelVoice {
		return
	}
	afk := h.inAFK(ctx, row)
	dv, err := h.repo.GetDevice(ctx, dev)
	if err != nil {
		return
	}
	h.afkMu.Lock()
	defer h.afkMu.Unlock()
	if room, exempt := h.afkExempt[dev]; exempt {
		if room == row.LivekitRoom {
			return
		}
		if !resume {
			delete(h.afkExempt, dev)
		}
	}
	if afk {
		delete(h.afkTracked, dev)
		return
	}
	if current, exists := h.afkTracked[dev]; resume && exists && current.room == row.LivekitRoom {
		return
	}
	if h.afkTracked == nil {
		h.afkTracked = make(map[id.ID]afkTracked)
	}
	h.afkTracked[dev] = afkTracked{community: *ch.CommunityID, channel: ch.ID, user: dv.UserID,
		room: row.LivekitRoom, lastActive: h.clk.Now()}
}

// WithAFKNotify sets where a sweep's move is announced (op 54 to the moved user's live connections)
// and returns h.
func (h *Calls) WithAFKNotify(notify *StructureNotifier) *Calls {
	h.afkNotify = notify
	return h
}

func (h *Calls) registerAFK(mux *server.Mux) {
	mux.Socket("GET /v1/communities/{id}/afk", http.HandlerFunc(h.afkSnapshot))
	mux.Socket("POST /v1/communities/{id}/afk/activity", http.HandlerFunc(h.afkActivity))
	// AW2 (R-AW3): choosing AFK. There is no DELETE: only the server clears away (Q3).
	mux.Socket("PUT /v1/communities/{id}/afk/away", http.HandlerFunc(h.afkAway))
	// The web3.8 manual move and leave (POST and DELETE /afk) were retired in web3.9 and answered a mapped
	// refusal to a web3.8 tab (R41). Bot v2 keeps no compatibility with old clients, so they are not registered.
}

func (h *Calls) afkMember(r *http.Request) (id.ID, id.ID, id.ID, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return id.ID{}, id.ID{}, id.ID{}, err
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		return cid, s.UserID, s.DeviceID, err
	}
	if _, err := h.repo.GetCommunity(r.Context(), cid); err != nil {
		return cid, s.UserID, s.DeviceID, notFound(err)
	}
	if _, err := h.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		return cid, s.UserID, s.DeviceID, notFound(err)
	}
	return cid, s.UserID, s.DeviceID, nil
}

func (h *Calls) afkDestination(ctx context.Context, cid, user id.ID) (store.CommunityAFK, error) {
	cfg, err := h.repo.GetCommunityAFK(ctx, cid)
	if err != nil || cfg.ChannelID == nil {
		return cfg, err
	}
	ch, err := h.repo.GetChannel(ctx, *cfg.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		cfg.ChannelID = nil
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if ch.CommunityID == nil || *ch.CommunityID != cid || ch.Kind != ChannelVoice {
		cfg.ChannelID = nil
		return cfg, nil
	}
	bits, err := h.res.Resolve(ctx, user, ch)
	if err != nil {
		return cfg, err
	}
	if !bits.Has(PermViewChannel) || !bits.Has(PermConnect) {
		cfg.ChannelID = nil
	}
	return cfg, nil
}

// afkSnapshot is GET /v1/communities/{id}/afk (R33): the built-in channel, the timeout and whether
// the caller's own device has a pending move in this community.
func (h *Calls) afkSnapshot(w http.ResponseWriter, r *http.Request) {
	cid, user, dev, err := h.afkMember(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	stored, err := h.repo.GetCommunityAFK(r.Context(), cid)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, live, err := livePointer(r.Context(), h.repo, cid, stored); err != nil {
		server.WriteError(w, err)
		return
	} else if !live {
		if _, err := h.ensureAFK(afterCommit(r), cid); err != nil {
			h.log.ErrorContext(r.Context(), "ensure the AFK channel", "community", cid, "err", err)
			h.writeAFKSnapshot(w, r, afkSnapshot{TimeoutSeconds: stored.TimeoutSeconds, Retired: []struct{}{},
				Away: []afkAwayEntry{}})
			return
		}
	}
	cfg, err := h.afkDestination(r.Context(), cid, user)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// The roster goes only with a channel the caller may reach, the audience op 55 goes to (M1).
	away := []afkAwayEntry{}
	if cfg.ChannelID != nil {
		if away, err = h.awaySnapshot(r.Context(), cid); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	out := afkSnapshot{ChannelID: cfg.ChannelID, TimeoutSeconds: cfg.TimeoutSeconds, Retired: []struct{}{}, Away: away}
	h.afkMu.Lock()
	// Device-scoped and not consumed: a client may read again (a reload on wake) and must see the
	// same reason until an admission of the device clears it.
	if p, ok := h.afkPending[dev]; ok && h.clk.Since(p.at) < afkPendingTTL {
		out.Pending = p.reason
	}
	h.afkMu.Unlock()
	h.writeAFKSnapshot(w, r, out)
}

func (h *Calls) writeAFKSnapshot(w http.ResponseWriter, r *http.Request, out afkSnapshot) {
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		h.log.WarnContext(r.Context(), "write AFK snapshot", "err", err)
	}
}

// decodeAFKActivity reads [channel_id] or [channel_id, exempt(uint 0|1)] (R5, W-1).
func decodeAFKActivity(w http.ResponseWriter, r *http.Request) (id.ID, bool, error) {
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		return id.ID{}, false, err
	}
	if len(body) != 1 && len(body) != 2 {
		return id.ID{}, false, server.Errorf(server.CodeInvalidRequest, "the body is [channel_id] or [channel_id, exempt]")
	}
	var ch id.ID
	if err := cborx.Unmarshal(body[0], &ch); err != nil {
		return id.ID{}, false, server.Errorf(server.CodeInvalidRequest, "channel_id is a 16-byte id")
	}
	if len(body) == 1 {
		return ch, false, nil
	}
	var flag uint64
	if err := cborx.Unmarshal(body[1], &flag); err != nil || flag > 1 {
		return id.ID{}, false, server.Errorf(server.CodeInvalidRequest, "exempt is 0 or 1")
	}
	return ch, flag == 1, nil
}

func (h *Calls) afkActivity(w http.ResponseWriter, r *http.Request) {
	cid, user, dev, err := h.afkMember(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	chID, exempt, err := decodeAFKActivity(w, r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ch, err := h.repo.GetChannel(r.Context(), chID)
	if err != nil || ch.CommunityID == nil || *ch.CommunityID != cid || ch.Kind != ChannelVoice {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "activity must name a community voice channel"))
		return
	}
	cfg, err := h.afkDestination(r.Context(), cid, user)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if cfg.ChannelID == nil || *cfg.ChannelID == ch.ID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	row, live, err := h.liveCallOf(r.Context(), ch, nil)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if !live || row.Ended != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, &cid); err != nil {
		server.WriteError(w, err)
		return
	}
	h.afkMu.Lock()
	if exempt {
		// R5: a device that cannot be idle-detected (a phone) is never moved out of this call. The
		// report may come before the admission; trackAFKAdmission honours it then too.
		if h.afkExempt == nil {
			h.afkExempt = make(map[id.ID]string)
		}
		h.afkExempt[dev] = row.LivekitRoom
		delete(h.afkTracked, dev)
	} else if tracked, ok := h.afkTracked[dev]; ok && tracked.community == cid && tracked.channel == ch.ID &&
		tracked.user == user && tracked.room == row.LivekitRoom {
		tracked.lastActive = h.clk.Now()
		h.afkTracked[dev] = tracked
	}
	h.afkMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// moveAFK cuts an idle device from its room (R4). The server decides the move and the client carries
// it out: the server cannot add a device to an E2EE call group, so after the cut it records a pending
// move for that device (R33), which the device's own snapshot reports, and tells the user's live
// connections (op 54). A failed cut changes nothing, so the next sweep retries.
func (h *Calls) moveAFK(ctx context.Context, dev id.ID, tracked afkTracked) error {
	cfg, err := h.afkDestination(ctx, tracked.community, tracked.user)
	if err != nil {
		return err
	}
	if cfg.ChannelID == nil {
		return server.Errorf(server.CodeInvalidRequest, "AFK is missing or inaccessible")
	}
	if tracked.channel == *cfg.ChannelID {
		return server.Errorf(server.CodeInvalidRequest, "already in AFK")
	}
	ch, err := h.repo.GetChannel(ctx, tracked.channel)
	if err != nil || ch.CommunityID == nil || *ch.CommunityID != tracked.community || ch.Kind != ChannelVoice {
		return server.Errorf(server.CodeInvalidRequest, "source must be a community voice channel")
	}
	row, live, err := h.liveCallOf(ctx, ch, nil)
	if err != nil {
		return err
	}
	if !live || h.sfu == nil {
		return server.Errorf(server.CodeInvalidRequest, "device is not in a live call")
	}
	if tracked.room != row.LivekitRoom {
		return server.Errorf(server.CodeInvalidRequest, "call changed since AFK timer started")
	}
	checkCtx, cancel := sfuCtx(ctx)
	parts, err := h.sfu.Participants(checkCtx, row.LivekitRoom)
	cancel()
	if err != nil {
		return err
	}
	present := false
	for _, p := range parts {
		base, _, _ := strings.Cut(p.GetIdentity(), "#")
		if base == dev.String() {
			present = true
			break
		}
	}
	if !present {
		return server.Errorf(server.CodeInvalidRequest, "device is not in this call")
	}
	h.afkMu.Lock()
	current, ok := h.afkTracked[dev]
	if !ok || current.channel != tracked.channel || current.room != tracked.room || !current.lastActive.Equal(tracked.lastActive) {
		h.afkMu.Unlock()
		return nil // fresh activity, an exempt report or a different call won the race
	}
	// The away mark below is decided here: an admission of the user after this point wins (R-AW7).
	decided := h.afkAdmitSeq
	// The reason goes in BEFORE the removal (R3-n1), so the cut client's first read already says
	// "moved"; a removal that fails puts back what was there.
	prev, hadPrev := h.afkPending[dev]
	h.setAFKPendingLocked(dev, afkPendingSwept)
	mine := h.afkPending[dev]
	h.afkMu.Unlock()
	h.afkStage("sweep-pending")
	removeCtx, cancel := sfuCtx(context.WithoutCancel(ctx))
	err = h.sfu.RemoveParticipants(removeCtx, row.LivekitRoom, dev)
	cancel()
	if err != nil {
		h.afkMu.Lock()
		if cur, ok := h.afkPending[dev]; ok && cur.reason == mine.reason && cur.at.Equal(mine.at) {
			if hadPrev {
				h.afkPending[dev] = prev
			} else {
				delete(h.afkPending, dev)
			}
		}
		h.afkMu.Unlock()
		return err
	}
	h.revokeRelay(dev)
	h.afkMu.Lock()
	delete(h.afkTracked, dev)
	h.afkMu.Unlock()
	// R-AW3: the cut marks the user away when no other device of theirs is in a call.
	h.sweepMarkAway(ctx, tracked, dev, decided)
	h.afkNotify.ChangedTo(context.WithoutCancel(ctx), tracked.community, []id.ID{tracked.user})
	return nil
}

// SweepAFK enforces inactivity in the server even if the browser's timers stop, and drops pending
// moves older than afkPendingTTL.
func (h *Calls) SweepAFK(ctx context.Context) {
	h.afkMu.Lock()
	for dev, p := range h.afkPending {
		if h.clk.Since(p.at) >= afkPendingTTL {
			delete(h.afkPending, dev)
		}
	}
	h.pruneAFKAdmitsLocked()
	tracked := make(map[id.ID]afkTracked, len(h.afkTracked))
	for dev, row := range h.afkTracked {
		tracked[dev] = row
	}
	h.afkMu.Unlock()
	for dev, p := range tracked {
		cfg, err := h.afkDestination(ctx, p.community, p.user)
		if err != nil {
			h.afkMu.Lock()
			delete(h.afkTracked, dev)
			h.afkMu.Unlock()
			continue
		}
		if cfg.ChannelID == nil {
			continue // an active admission may outlive a missing destination
		}
		if h.clk.Since(p.lastActive) < afkTimeout(cfg.TimeoutSeconds) {
			continue
		}
		if err := h.moveAFK(ctx, dev, p); err != nil {
			// A departed caller cannot be moved. Keep only errors worth retrying; a new
			// interaction starts a fresh timer if this device joins again.
			var se *server.Error
			if errors.As(err, &se) {
				h.afkMu.Lock()
				delete(h.afkTracked, dev)
				h.afkMu.Unlock()
			}
		}
	}
}

func afkTimeout(seconds uint64) time.Duration {
	switch seconds {
	case 60:
		return time.Minute
	case 900:
		return 15 * time.Minute
	case 1800:
		return 30 * time.Minute
	case 3600:
		return time.Hour
	default:
		return 5 * time.Minute
	}
}
