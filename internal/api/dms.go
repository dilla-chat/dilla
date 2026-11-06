package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// dmIDLabel is the domain separator for the derived 1:1 DM id. It is a name,
// not a secret: both clients compute it so that dilla_binding.target_id
// (protocol/01, "dm_id for DMs") agrees without a round trip.
const dmIDLabel = "dilla dm v1"

// DMChannelID returns the deterministic channel id of a 1:1 DM (P2-D31): the
// first 16 bytes of SHA-256("dilla dm v1" || lo || hi) over the two user ids
// sorted bytewise. For any other number of users it returns the zero id, and
// the caller mints a random one: a group DM's membership is mutable, and a
// derived id would change when a member left.
func DMChannelID(users []id.ID) id.ID {
	if len(users) != 2 {
		return id.ID{}
	}
	lo, hi := users[0], users[1]
	if bytes.Compare(lo[:], hi[:]) > 0 {
		lo, hi = hi, lo
	}
	h := sha256.New()
	h.Write([]byte(dmIDLabel))
	h.Write(lo[:])
	h.Write(hi[:])
	var out id.ID
	copy(out[:], h.Sum(nil)[:16])
	return out
}

// Binding is the dilla_binding a group bound to a channel must carry
// (protocol/01 "dilla_binding"). It is the delivery service's own decoded
// shape, so the binding the api package computes and the one the delivery
// service compares a registration against are one type.
type Binding = ds.Binding

// The three fixed versions of the binding array, from protocol/07-versioning.md's
// change table: wire, e2ee and media all stand at 1, and a text group carries no
// media, so its media_version slot is 0. They are named constants rather than
// literals so that a version bump is one edit.
const (
	protocolBindingV uint64 = 1
	e2eeVersion      uint64 = 1
	mediaVersionText uint64 = 0
)

// ExpectedBinding returns the text-group binding for a channel. Call groups use
// Kind = GroupCall and MediaVersion = 1; task 16 mints those.
//
// instanceID is a parameter and not a package variable because protocol/01 rule 1
// makes instance_id the FIRST field a client and the delivery service compare
// (E_BINDING). A zero instance id would make the comparison either vacuous or
// always-false, depending on which side had it, and neither failure is visible in
// a test that checks only target_id.
func ExpectedBinding(instanceID id.ID, ch store.ChannelRow) Binding {
	return Binding{
		V:             protocolBindingV,
		InstanceID:    instanceID,
		CommunityID:   ch.CommunityID,
		TargetID:      ch.ID,
		Kind:          GroupText,
		PolicyVersion: ch.HostPolicyVersion,
		E2EEVersion:   e2eeVersion,
		MediaVersion:  mediaVersionText,
	}
}

// DMs serves POST /v1/dms and GET /v1/dms (protocol/09 § DMs). A DM (kind 3)
// or group DM (kind 4) is a channel row with no community, always end-to-end
// encrypted and private; its participants are channel_members, the only place
// that list is stored.
type DMs struct {
	repo       store.Repository
	dsvc       DS
	clk        clock.Clock
	instanceID id.ID
	maxGroupDM int
	log        *slog.Logger
}

// NewDMs takes the instance id the DM's binding names and maxGroupDM, the most
// participants a group DM may hold, the opener included (P2-6: the composition
// root passes livekit.max_voice_participants, so every participant fits in the
// DM's call).
func NewDMs(repo store.Repository, dsvc DS, clk clock.Clock, instanceID id.ID, maxGroupDM int, log *slog.Logger) *DMs {
	return &DMs{repo: repo, dsvc: dsvc, clk: clk, instanceID: instanceID, maxGroupDM: maxGroupDM, log: log}
}

// Register mounts the two DM routes bare, as Communities does; each handler
// requires an enrolled session itself.
func (d *DMs) Register(mux *server.Mux) {
	mux.Socket("POST /v1/dms", http.HandlerFunc(d.open))
	mux.Socket("GET /v1/dms", http.HandlerFunc(d.list))
}

type openDMReq struct {
	_          struct{} `cbor:",toarray"`
	Recipients []id.ID
}

func (d *DMs) open(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req openDMReq
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	users := append([]id.ID{s.UserID}, req.Recipients...)
	slices.SortFunc(users, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	users = slices.Compact(users)
	if len(users) < 2 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"a DM needs at least one recipient other than yourself"))
		return
	}
	if len(users) > d.maxGroupDM {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"a group DM holds at most %d members", d.maxGroupDM))
		return
	}
	for _, u := range users {
		if err := mayReceiveDM(r.Context(), d.repo, u); err != nil {
			d.fail(w, r, "open dm", err)
			return
		}
	}

	kind := ChannelGroupDM
	chID := id.New()
	if len(users) == 2 {
		kind = ChannelDM
		chID = DMChannelID(users)
		existing, err := d.repo.GetChannel(r.Context(), chID)
		switch {
		case err == nil:
			d.answer(w, http.StatusOK, existing.ID)
			return
		case !errors.Is(err, store.ErrNotFound):
			d.fail(w, r, "open dm", err)
			return
		}
	}

	now := d.clk.Now().Unix()
	row := store.ChannelRow{
		ID: chID, CommunityID: nil, Kind: kind, Mode: ModeE2EE, Visibility: VisPrivate,
		Name: "", Topic: "", SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: now,
	}
	err = d.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.CreateChannel(r.Context(), row); err != nil {
			return err
		}
		for _, u := range users {
			if err := tx.PutChannelMember(r.Context(), chID, u, now); err != nil {
				return err
			}
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &s.UserID, Action: "dm.create", Target: chID.String(), At: now,
		})
	})
	if kind == ChannelDM && errors.Is(err, store.ErrConflict) {
		// Both ends opened the same 1:1 DM at once and the other request's row
		// landed first: the derived id makes that row this DM.
		if existing, gerr := d.repo.GetChannel(r.Context(), chID); gerr == nil {
			d.answer(w, http.StatusOK, existing.ID)
			return
		}
	}
	if err != nil {
		d.fail(w, r, "open dm", err)
		return
	}
	d.answer(w, http.StatusCreated, chID)
}

// mayReceiveDM refuses an unknown account with 404 and a disabled or deleted
// one with 403. A disabled account is v1's block: interfaces.md §4.3 gives
// users a disabled_at column and no per-user block list.
func mayReceiveDM(ctx context.Context, repo store.Repository, userID id.ID) error {
	u, err := repo.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return server.Errorf(server.CodeNotFound, "no such user")
	}
	if err != nil {
		return err
	}
	if u.DisabledAt != nil || u.DeletedAt != nil {
		return server.Errorf(server.CodeForbidden, "that account cannot receive messages")
	}
	return nil
}

func (d *DMs) answer(w http.ResponseWriter, status int, chID id.ID) {
	if err := server.EncodeBody(w, status, []id.ID{chID}); err != nil {
		d.log.Error("encode dm", "err", err)
	}
}

type dmResp struct {
	_         struct{} `cbor:",toarray"`
	ChannelID id.ID
	Kind      uint64
	Members   []id.ID
}

func (d *DMs) list(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	rows, err := d.repo.ListChannelsForUser(r.Context(), s.UserID)
	if err != nil {
		d.fail(w, r, "list dms", err)
		return
	}
	out := make([]dmResp, 0, len(rows))
	for _, ch := range rows {
		members, err := d.repo.ListChannelMembers(r.Context(), ch.ID)
		if err != nil {
			d.fail(w, r, "list dms", err)
			return
		}
		out = append(out, dmResp{ChannelID: ch.ID, Kind: uint64(ch.Kind), Members: members})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		d.log.Error("encode dms", "err", err)
	}
}

// fail writes err as the client's refusal. Anything that is not a
// *server.Error is logged here and reaches the client as an empty E_INTERNAL.
func (d *DMs) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		d.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}

// SyncUserDMs runs SyncGroupMembers over every live DM and group DM userID is a
// participant of. SyncGroupMembers proposes only a device that holds an
// available KeyPackage, so a device that had none when its DM's group was
// populated is left out until something re-syncs the DM; the composition root
// calls this after every accepted POST /v1/keypackages (Groups.AfterKeyPackages),
// which is that something. It MUST NOT run inside a Tx. Every DM is attempted;
// the refusals are returned together.
func SyncUserDMs(ctx context.Context, repo store.Repository, dsvc DS, userID id.ID, now int64) error {
	dms, err := repo.ListChannelsForUser(ctx, userID)
	if err != nil {
		return err
	}
	var errs []error
	for _, ch := range dms {
		if err := SyncGroupMembers(ctx, repo, dsvc, ch, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SyncGroupMembers brings the groups bound to ch in line with channel_members:
// in each open text group, every device of an eligible user that holds an
// available KeyPackage and is neither a live leaf nor the target of an
// outstanding instance Add is proposed for Add, and every live leaf whose user
// is no longer eligible, and that no outstanding instance Remove already
// targets, is proposed for Remove; in each open call group only the Removes
// are issued, because a call is joined by presence, not by an Add. It is the
// one function every membership change funnels through, and it is idempotent:
// a second call with nothing changed issues nothing.
//
// now is the caller's clk.Now().Unix(); CountKeyPackages takes it so an
// expired KeyPackage is not counted as available.
//
// It MUST NOT be called from inside a store.Repository.Tx, for the reason
// RemoveUserFromCommunityGroups gives: it calls the delivery service, which
// writes through the same single-connection write pool.
//
// Every group is attempted; the refusals are returned together.
func SyncGroupMembers(ctx context.Context, repo store.Repository, dsvc DS, ch store.ChannelRow, now int64) error {
	if dsvc == nil {
		return errNoDS
	}
	eligible, err := repo.ListChannelMembers(ctx, ch.ID)
	if err != nil {
		return err
	}
	var errs []error
	if TextGroupAllowed(ch) {
		groups, err := repo.GroupsForTarget(ctx, ch.ID, GroupText)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if err := syncGroup(ctx, repo, dsvc, g, eligible, now, true); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if CallGroupAllowed(ch) {
		groups, err := repo.GroupsForTarget(ctx, ch.ID, GroupCall)
		if err != nil {
			return err
		}
		if len(groups) == 0 {
			return errors.Join(errs...)
		}
		// channel_members is the view_channel set; a call group needs view_channel
		// AND connect (protocol/02 invariants 1 and 4), so a member who kept view
		// but lost connect is removed from the call groups (fix wave I4).
		callers, err := withCallBits(ctx, repo, ch, eligible)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, g := range groups {
			if err := syncGroup(ctx, repo, dsvc, g, callers, now, false); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// withCallBits narrows users to those whose resolved bits in ch carry
// callGroupBits (view_channel and connect), what ResolverACL requires of a call
// group's member.
func withCallBits(ctx context.Context, repo store.Repository, ch store.ChannelRow, users []id.ID) ([]id.ID, error) {
	res := NewResolver(repo)
	out := make([]id.ID, 0, len(users))
	for _, u := range users {
		bits, err := res.Resolve(ctx, u, ch)
		if err != nil {
			return nil, err
		}
		if bits.Has(callGroupBits) {
			out = append(out, u)
		}
	}
	return out, nil
}

// syncGroup is SyncGroupMembers for one group; adds is false for a call group.
func syncGroup(ctx context.Context, repo store.Repository, dsvc DS, g store.GroupRow,
	eligible []id.ID, now int64, adds bool) error {
	// First the Adds no commit could carry any more (a participant removed, or a role revoked,
	// while their Add was outstanding), so the Removes below land in a group that can commit.
	voidErr := dsvc.VoidIneligibleAdds(ctx, g.GroupID)
	leaves, err := repo.ListMembers(ctx, g.GroupID)
	if err != nil {
		return err
	}
	// The instance's outstanding proposals at the group's current epoch: an Add
	// or Remove already issued and not yet committed is not issued twice, which
	// would spend a second KeyPackage and put two leaves of one device in one
	// commit.
	outstanding, err := repo.ListProposals(ctx, g.GroupID, g.Epoch, false)
	if err != nil {
		return err
	}
	pendingAdd := map[id.ID]bool{}
	pendingRemove := map[uint32]bool{}
	for _, p := range outstanding {
		if p.Origin != 0 || p.VoidAt != nil {
			continue
		}
		switch {
		case p.Kind == uint8(mlswasi.ProposalAdd) && p.TargetDevice != nil:
			pendingAdd[*p.TargetDevice] = true
		case p.Kind == uint8(mlswasi.ProposalRemove) && p.TargetLeaf != nil:
			pendingRemove[*p.TargetLeaf] = true
		}
	}

	var errs []error
	if voidErr != nil {
		errs = append(errs, voidErr)
	}
	live := make(map[id.ID]bool, len(leaves))
	for _, m := range leaves {
		if m.RemovedEpoch != nil {
			continue
		}
		live[m.DeviceID] = true
		if slices.Contains(eligible, m.UserID) || pendingRemove[m.LeafIndex] {
			continue
		}
		// Each Remove carries its own action_id, and names the device it is for; see
		// RemoveUserFromChannelGroups.
		if err := dsvc.ProposeRemoveOf(ctx, g.GroupID, m.LeafIndex, m.DeviceID, id.New()); err != nil {
			errs = append(errs, err)
		}
	}
	if !adds {
		return errors.Join(errs...)
	}

	var want []id.ID
	for _, u := range eligible {
		devices, err := repo.ListDevicesByUser(ctx, u)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, dev := range devices {
			if dev.RevokedAt != nil || dev.QuarantinedAt != nil || live[dev.ID] || pendingAdd[dev.ID] {
				continue
			}
			n, err := repo.CountKeyPackages(ctx, dev.ID, now)
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
			if n == 0 {
				// The delivery service would refuse the Add and the round
				// would be wasted; the device is picked up by the next sync.
				continue
			}
			want = append(want, dev.ID)
		}
	}
	if len(want) > 0 {
		if err := dsvc.ProposeAddBatch(ctx, g.GroupID, want); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
