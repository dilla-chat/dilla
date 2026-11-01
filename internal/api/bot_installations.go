package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strings"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/identity"
	botprotocol "github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

const (
	botCapMusic uint64 = 1 << iota
	botCapWelcome
	botCapRoles
	botCapAnnouncements
	botCapWarnings
	botCapRemoval
	botCapKick
	botCapBan
	// botCapPolls and botCapReminders (Task 10c) gate creating polls and reminders; existing
	// polls and reminders keep working with the bit off. They need no community permission.
	botCapPolls
	botCapReminders
	botKnownCaps = botCapMusic | botCapWelcome | botCapRoles | botCapAnnouncements |
		botCapWarnings | botCapRemoval | botCapKick | botCapBan | botCapPolls | botCapReminders
	// botLegacyCaps is the original capability byte: what a runtime that does not ask for the
	// widened installation shape (GET /v1/bot-runtime/installations without v=2) is shown.
	botLegacyCaps = botCapMusic | botCapWelcome | botCapRoles | botCapAnnouncements |
		botCapWarnings | botCapRemoval | botCapKick | botCapBan
	maxBotRuntimeBody      = 16 << 10
	botRestrictedBits Bits = PermAdministrator | PermManageRoles | PermManageCommunity
)

const (
	botOpConfigure uint8 = iota + 1
	botOpDisable
	botOpRemove
	botOpClaim
	botOpEnroll
	botOpCredential
	botOpReady
	botOpModeration
)

type botInstallationRequest struct {
	_                struct{} `cbor:",toarray"`
	RequestID        id.ID
	ExpectedRevision *uint64
	Capabilities     uint64
	StepUp           string
}

type botStateRequest struct {
	_                struct{} `cbor:",toarray"`
	RequestID        id.ID
	ExpectedRevision uint64
}

type botRuntimeClaimRequest struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
}

type botRuntimeEnrollRequest struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
	ClaimID   id.ID
	UMKPub    []byte
	SSKPub    []byte
	SigUMKSSK []byte
	Device    deviceRequest
}

type botCredentialRequest struct {
	_                struct{} `cbor:",toarray"`
	RequestID        id.ID
	ExpectedRevision uint64
	Credential       []byte
}

type botOperationPayload struct {
	Request      id.ID
	Revision     *uint64
	Capabilities uint64
	// Widened marks a configure request that sent the widened capability mask (PUT ...?v=2,
	// Task 10c fix round 1). It is omitted when false so a legacy request hashes as it always did.
	Widened bool `cbor:",omitempty"`
}

// botPollsReminders are the capability bits a legacy configure request (one without ?v=2) cannot
// see: such a request keeps them as they are, and a new installation starts with both on.
const botPollsReminders = botCapPolls | botCapReminders

// effectiveBotCapabilities is the mask a configure request stores. A widened request (?v=2) sets
// every bit itself. A legacy request (an older web client that knows only the original byte) sets
// the original eight and keeps Polls and Reminders as they are; a new installation starts with
// both on, as the prototype shows them.
func effectiveBotCapabilities(requested uint64, widened bool, current *store.BotInstallationRow) uint64 {
	if widened {
		return requested
	}
	if current == nil {
		return requested&botLegacyCaps | botPollsReminders
	}
	return requested&botLegacyCaps | uint64(current.Capabilities)&botPollsReminders
}

// appendBotInstallationEvent runs inside the installation mutation transaction.
// A replay returns the recorded operation before reaching this function.
func appendBotInstallationEvent(ctx context.Context, tx store.Repository, row store.BotInstallationRow) error {
	timestamp, ok := checkedUint64(row.Updated)
	if !ok {
		return store.ErrConflict
	}
	_, err := tx.AppendBotStructuralEvent(ctx, botprotocol.Event{
		ID: id.New(), CommunityID: row.CommunityID, Timestamp: timestamp,
		Kind: botprotocol.EventInstallation,
		Data: []any{row.ID, uint64(row.State), row.Revision},
	})
	return err
}

func registerBotInstallations(m *server.Mux, d Deps) {
	botSession := BotSessionRoute(d.Sessions, d.Limiter)
	m.Handle("GET /v1/communities/{community}/bot", botSession("GET /v1/communities/{community}/bot", http.HandlerFunc(d.GetBotInstallation)))
	m.Handle("PATCH /v1/communities/{community}/bot/profile", botSession("PATCH /v1/communities/{community}/bot/profile", http.HandlerFunc(d.PatchBotProfile)))
	m.Handle("PUT /v1/communities/{community}/bot", botSession("PUT /v1/communities/{community}/bot", http.HandlerFunc(d.ConfigureBotInstallation)))
	m.Handle("POST /v1/communities/{community}/bot/disable", botSession("POST /v1/communities/{community}/bot/disable", http.HandlerFunc(d.DisableBotInstallation)))
	m.Handle("DELETE /v1/communities/{community}/bot", botSession("DELETE /v1/communities/{community}/bot", http.HandlerFunc(d.RemoveBotInstallation)))
	commandStatusPath := "GET /v1/communities/{community}/bot/command-status"
	commandStatus := SessionRoute(d.Sessions, d.Limiter)(commandStatusPath, http.HandlerFunc(d.GetBotCommandStatus))
	m.Handle(commandStatusPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		commandStatus.ServeHTTP(w, r)
	}))
	m.Handle("GET /v1/communities/{community}/bot/actors/{actor}/authority", botSession("GET /v1/communities/{community}/bot/actors/{actor}/authority", http.HandlerFunc(d.GetBotActorAuthority)))
	channelAuthorityPath := "GET /v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority"
	channelAuthority := botSession(channelAuthorityPath, http.HandlerFunc(d.GetBotActorChannelAuthority))
	m.Handle(channelAuthorityPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		channelAuthority.ServeHTTP(w, r)
	}))
	m.Handle("POST /v1/communities/{community}/bot/join-role-grants", botSession("POST /v1/communities/{community}/bot/join-role-grants", http.HandlerFunc(d.GrantBotJoinRole)))
	m.Handle("GET /v1/communities/{community}/bot/members/{member}/episode", botSession("GET /v1/communities/{community}/bot/members/{member}/episode", http.HandlerFunc(d.GetBotMemberEpisode)))
	m.HandleFunc("GET /v1/bot-runtime/installations", d.ListRuntimeInstallations)
	m.HandleFunc("POST /v1/bot-runtime/installations/{installation}/claim", d.ClaimBotInstallation)
	m.HandleFunc("POST /v1/bot-runtime/installations/{installation}/enroll", d.EnrollBotInstallation)
	m.Handle("PUT /v1/bot-runtime/installations/{installation}/credential", botSession("PUT /v1/bot-runtime/installations/{installation}/credential", http.HandlerFunc(d.FinalizeBotCredential)))
	m.Handle("POST /v1/bot-runtime/installations/{installation}/ready", botSession("POST /v1/bot-runtime/installations/{installation}/ready", http.HandlerFunc(d.MarkBotReady)))
	m.Handle("POST /v1/bot-runtime/installations/{installation}/moderation/actions", botSession("POST /v1/bot-runtime/installations/{installation}/moderation/actions", http.HandlerFunc(d.AcceptBotModerationAction)))
	d.registerBotRemovalReceipt(m)
	d.registerBotWarningReceipt(m)
	d.registerBotInteractionAdmissions(m)
	registerBotSpotifyAttempt(m, d)
	registerBotSettings(m, d)
}

func botInstallationValue(row store.BotInstallationRow) ([]any, error) {
	updated, ok := checkedUint64(row.Updated)
	if !ok {
		return nil, store.ErrConflict
	}
	var user, device any
	if row.UserID != nil {
		user = *row.UserID
	}
	if row.DeviceID != nil {
		device = *row.DeviceID
	}
	return []any{row.ID, row.CommunityID, uint64(row.State), row.Revision, user, device,
		uint64(row.Capabilities), updated}, nil
}

// writeRuntimeBotInstallation answers a Bot session's credential and ready mutations with the
// eight base fields and the original capability byte, the shape every runtime parses.
func (d Deps) writeRuntimeBotInstallation(w http.ResponseWriter, r *http.Request, status int, row store.BotInstallationRow) {
	value, err := botRuntimeInstallationValue(row, false)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, status, value)
}

func (d Deps) writeBotInstallation(w http.ResponseWriter, r *http.Request, status int, row store.BotInstallationRow) {
	value, err := botInstallationValue(row)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, status, value)
}

func humanForBot(r *http.Request, repo store.Repository, community id.ID, s auth.Session) (store.CommunityRow, Bits, error) {
	user, err := repo.GetUser(r.Context(), s.UserID)
	if err != nil {
		return store.CommunityRow{}, 0, notFound(err)
	}
	if user.Kind != kindHuman {
		return store.CommunityRow{}, 0, server.Errorf(server.CodeForbidden, "human session required")
	}
	if _, err := repo.GetMember(r.Context(), community, s.UserID); err != nil {
		return store.CommunityRow{}, 0, notFound(err)
	}
	com, err := repo.GetCommunity(r.Context(), community)
	if err != nil {
		return store.CommunityRow{}, 0, notFound(err)
	}
	snap, err := LoadSnapshot(r.Context(), repo, community, s.UserID, nil)
	if err != nil {
		return store.CommunityRow{}, 0, notFound(err)
	}
	have := snap.Resolve(s.UserID)
	if !have.Has(PermManageCommunity) {
		return store.CommunityRow{}, 0, server.Errorf(server.CodeForbidden, "manage community")
	}
	return com, have, nil
}

func (d Deps) GetBotInstallation(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if _, _, err = humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), cid)
	if errors.Is(err, store.ErrNotFound) {
		d.write(w, r, http.StatusOK, nil)
		return
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	// The manager's read is the widened shape (Task 10c): restart counters, the runtime's
	// version and the music defaults follow the eight base fields.
	value, err := botInstallationValueV2(row)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, value)
}

func (d Deps) ConfigureBotInstallation(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botInstallationRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	widened := r.URL.Query().Get("v") == "2"
	known := botKnownCaps
	if !widened {
		known = botLegacyCaps
	}
	if req.RequestID.IsZero() || req.Capabilities&^known != 0 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid request id or capability mask"))
		return
	}
	_, have, err := humanForBot(r, d.Repo, cid, s)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := checkBotCapabilityAuthority(have, req.Capabilities); err != nil {
		server.WriteError(w, err)
		return
	}
	semantic := botOperationPayload{Request: req.RequestID, Revision: req.ExpectedRevision, Capabilities: req.Capabilities, Widened: widened}
	digest := botPayloadHash(semantic)
	now := d.Clock.Now().Unix()
	var result store.BotInstallationRow
	created := false
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		lockedCommunity, lockedHave, err := humanForBot(r, tx, cid, s)
		if err != nil {
			return err
		}
		if err := checkBotCapabilityAuthority(lockedHave, req.Capabilities); err != nil {
			return err
		}
		prior, perr := tx.GetBotOperation(r.Context(), cid, req.RequestID)
		if perr == nil {
			if prior.ActorKind != 0 || prior.ActorID != s.UserID || prior.ActorDevice == nil || *prior.ActorDevice != s.DeviceID || prior.Operation != botOpConfigure || !bytes.Equal(prior.PayloadHash, digest) {
				return botConflict("request id was used for a different operation")
			}
			if err := requireBotWire(s.WireVersion); err != nil {
				return err
			}
			return decodeBotInstallationResult(prior.ResultCBOR, &result)
		}
		if !errors.Is(perr, store.ErrNotFound) {
			return perr
		}
		current, cerr := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if errors.Is(cerr, store.ErrNotFound) {
			if req.ExpectedRevision != nil {
				return botConflict("installation revision changed")
			}
			if err := requireBotWire(s.WireVersion); err != nil {
				return err
			}
			capabilities, ok := checkedUint16(effectiveBotCapabilities(req.Capabilities, widened, nil))
			if !ok {
				return server.Errorf(server.CodeInvalidRequest, "invalid capability mask")
			}
			current = store.BotInstallationRow{ID: id.New(), CommunityID: cid, State: store.BotInstallationPending, Revision: 1,
				Capabilities: capabilities, ApprovedBy: s.UserID, ApprovedAt: now, Created: now, Updated: now}
			created = true
			if err := d.spendBotStepUp(req.StepUp, s); err != nil {
				return err
			}
			if err := tx.CreateBotInstallation(r.Context(), current); err != nil {
				return err
			}
			if err := appendBotInstallationEvent(r.Context(), tx, current); err != nil {
				return err
			}
		} else if cerr != nil {
			return cerr
		} else {
			if req.ExpectedRevision == nil || *req.ExpectedRevision != current.Revision {
				return botConflict("installation revision changed")
			}
			capabilities, ok := checkedUint16(effectiveBotCapabilities(req.Capabilities, widened, &current))
			if !ok {
				return server.Errorf(server.CodeInvalidRequest, "invalid capability mask")
			}
			if current.State != store.BotInstallationDisabled && current.Capabilities == capabilities {
				if err := requireBotWire(s.WireVersion); err != nil {
					return err
				}
				result = current
				resultCBOR, err := encodeBotInstallationResult(current)
				if err != nil {
					return err
				}
				return tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: cid, RequestID: req.RequestID, InstallationID: current.ID,
					ActorKind: 0, ActorID: s.UserID, ActorDevice: &s.DeviceID, Operation: botOpConfigure, PayloadHash: digest, ResultCBOR: resultCBOR, Created: now})
			}
			if current.RemovedAt != nil {
				return botConflict("removed installation cannot be re-enabled")
			}
			var managedRole *store.RoleRow
			if current.RoleID != nil {
				role, err := tx.GetRole(r.Context(), *current.RoleID)
				if err != nil {
					return err
				}
				if s.UserID != lockedCommunity.Owner {
					snap, err := LoadSnapshot(r.Context(), tx, cid, s.UserID, nil)
					if err != nil {
						return err
					}
					highest := uint64(0)
					for _, role := range snap.Roles {
						if snap.Held[role.ID] && role.Position > highest {
							highest = role.Position
						}
					}
					if highest < 2 || role.Position >= highest {
						return server.Errorf(server.CodeForbidden, "no safe managed-role position below the configuring human")
					}
				}
				managedRole = &role
			}
			if err := requireBotWire(s.WireVersion); err != nil {
				return err
			}
			if err := d.spendBotStepUp(req.StepUp, s); err != nil {
				return err
			}
			if managedRole != nil {
				managedRole.Allow = uint64(botRoleBits(req.Capabilities, lockedHave) &^ moderatorBits)
				managedRole.Deny = 0
				if err := tx.PutRole(r.Context(), *managedRole); err != nil {
					return err
				}
			}
			current.Capabilities = capabilities
			current.ApprovedBy, current.ApprovedAt = s.UserID, now
			current.Updated, current.Revision = now, current.Revision+1
			if current.UserID != nil {
				current.State = store.BotInstallationJoining
			} else {
				current.State = store.BotInstallationPending
			}
			if err := tx.UpdateBotInstallation(r.Context(), current.ID, current.Revision-1, current); err != nil {
				return err
			}
			if err := appendBotInstallationEvent(r.Context(), tx, current); err != nil {
				return err
			}
		}
		result = current
		resultCBOR, err := encodeBotInstallationResult(current)
		if err != nil {
			return err
		}
		return tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: cid, RequestID: req.RequestID, InstallationID: current.ID,
			ActorKind: 0, ActorID: s.UserID, ActorDevice: &s.DeviceID, Operation: botOpConfigure, PayloadHash: digest, ResultCBOR: resultCBOR, Created: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	d.writeBotInstallation(w, r, status, result)
}

func checkBotCapabilityAuthority(have Bits, caps uint64) error {
	required := PermViewChannel | PermReadHistory | PermSendMessages | PermAddReactions
	if caps&botCapMusic != 0 {
		required |= PermConnect | PermSpeak
	}
	if !have.Has(required) {
		return server.Errorf(server.CodeForbidden, "bot baseline permissions exceed the configuring human's current permission ceiling")
	}
	checks := []struct {
		cap  uint64
		perm Bits
	}{{botCapRoles, PermManageRoles}, {botCapWarnings, PermModerateMembers}, {botCapRemoval, PermManageMessages}, {botCapKick, PermKickMembers}, {botCapBan, PermBanMembers}}
	for _, c := range checks {
		if caps&c.cap != 0 && !have.Has(c.perm) {
			return server.Errorf(server.CodeForbidden, "capability requires its corresponding community permission")
		}
	}
	return nil
}

func (d Deps) spendBotStepUp(token string, s auth.Session) error {
	if token == "" || d.StepUps == nil {
		return server.Errorf(server.CodeStepUp, "a fresh step-up is required")
	}
	user, ok := d.StepUps.Spend(token, s.TokenHash)
	if !ok || user != s.UserID {
		return server.Errorf(server.CodeStepUp, "a fresh step-up is required")
	}
	return nil
}

func botRoleBits(caps uint64, ceiling Bits) Bits {
	bits := PermViewChannel | PermReadHistory | PermSendMessages | PermAddReactions
	if caps&botCapMusic != 0 {
		bits |= PermConnect | PermSpeak
	}
	if caps&botCapWarnings != 0 {
		bits |= PermModerateMembers
	}
	if caps&botCapRemoval != 0 {
		bits |= PermManageMessages
	}
	if caps&botCapKick != 0 {
		bits |= PermKickMembers
	}
	if caps&botCapBan != 0 {
		bits |= PermBanMembers
	}
	return bits & ceiling &^ (PermAdministrator | PermManageRoles | PermManageCommunity)
}

func approvedBotModeratorBits(caps uint16) Bits {
	var bits Bits
	if uint64(caps)&botCapRemoval != 0 {
		bits |= PermManageMessages
	}
	if uint64(caps)&botCapWarnings != 0 {
		bits |= PermModerateMembers
	}
	if uint64(caps)&botCapKick != 0 {
		bits |= PermKickMembers
	}
	if uint64(caps)&botCapBan != 0 {
		bits |= PermBanMembers
	}
	return bits
}

func getManagedInstallation(ctx context.Context, repo store.Repository, communityID, roleID id.ID) (store.BotInstallationRow, bool, error) {
	row, err := repo.GetActiveBotInstallationByCommunity(ctx, communityID)
	if errors.Is(err, store.ErrNotFound) {
		return store.BotInstallationRow{}, false, nil
	}
	if err != nil {
		return store.BotInstallationRow{}, false, err
	}
	return row, row.RoleID != nil && *row.RoleID == roleID && row.RemovedAt == nil, nil
}

func managedRoleMayBeGranted(ctx context.Context, repo store.Repository, communityID, target, roleID id.ID) error {
	row, managed, err := getManagedInstallation(ctx, repo, communityID, roleID)
	if err != nil || !managed {
		return err
	}
	if row.State == store.BotInstallationDisabled || row.UserID == nil || *row.UserID != target {
		return server.Errorf(server.CodeForbidden, "managed bot role is restricted to its live installation account")
	}
	user, err := repo.GetUser(ctx, target)
	if err != nil {
		return err
	}
	if user.Kind != 1 {
		return server.Errorf(server.CodeForbidden, "managed bot role is restricted to its bot account")
	}
	return nil
}

func botPayloadHash(v any) []byte { b, _ := cborx.Marshal(v); sum := sha256.Sum256(b); return sum[:] }

func botConflict(detail string) error {
	return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "%s", detail))
}

func encodeBotInstallationResult(row store.BotInstallationRow) ([]byte, error) {
	value, err := botInstallationValue(row)
	if err != nil {
		return nil, err
	}
	return cborx.Marshal(value)
}
func decodeBotInstallationResult(raw []byte, row *store.BotInstallationRow) error {
	var v []any
	if err := cborx.Unmarshal(raw, &v); err != nil {
		return err
	}
	if len(v) != 8 {
		return store.ErrConflict
	}
	// Receipts are re-created from immutable operation rows only in this handler; decode identifiers and
	// primitive fields explicitly so malformed database bytes fail closed.
	getID := func(i int, dst *id.ID) error {
		b, ok := v[i].([]byte)
		if !ok || len(b) != 16 {
			return store.ErrConflict
		}
		copy(dst[:], b)
		return nil
	}
	if err := getID(0, &row.ID); err != nil {
		return err
	}
	if err := getID(1, &row.CommunityID); err != nil {
		return err
	}
	state, ok := v[2].(uint64)
	if !ok || state > 255 {
		return store.ErrConflict
	}
	row.State = uint8(state)
	rev, ok := v[3].(uint64)
	if !ok {
		return store.ErrConflict
	}
	row.Revision = rev
	if b, ok := v[4].([]byte); ok && len(b) == 16 {
		var x id.ID
		copy(x[:], b)
		row.UserID = &x
	} else if v[4] != nil {
		return store.ErrConflict
	}
	if b, ok := v[5].([]byte); ok && len(b) == 16 {
		var x id.ID
		copy(x[:], b)
		row.DeviceID = &x
	} else if v[5] != nil {
		return store.ErrConflict
	}
	caps, ok := v[6].(uint64)
	if !ok || caps&^botKnownCaps != 0 {
		return store.ErrConflict
	}
	row.Capabilities = uint16(caps) //nolint:gosec // G115: checked against botKnownCaps (10 bits) above
	updated, ok := v[7].(uint64)
	updatedInt64, inRange := checkedInt64(updated)
	if !ok || !inRange {
		return store.ErrConflict
	}
	row.Updated = updatedInt64
	return nil
}

func (d Deps) DisableBotInstallation(w http.ResponseWriter, r *http.Request) {
	d.setBotDisabled(w, r, false)
}

func (d Deps) RemoveBotInstallation(w http.ResponseWriter, r *http.Request) {
	d.setBotDisabled(w, r, true)
}

func (d Deps) setBotDisabled(w http.ResponseWriter, r *http.Request, remove bool) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	cid, err := server.PathID(r, "community")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botStateRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request id is required"))
		return
	}
	if _, _, err := humanForBot(r, d.Repo, cid, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	op := botOpDisable
	if remove {
		op = botOpRemove
	}
	digest := botPayloadHash(struct {
		Request  id.ID
		Revision uint64
		Remove   bool
	}{req.RequestID, req.ExpectedRevision, remove})
	now := d.Clock.Now().Unix()
	var result store.BotInstallationRow
	var changed bool
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := humanForBot(r, tx, cid, s); err != nil {
			return err
		}
		prior, perr := tx.GetBotOperation(r.Context(), cid, req.RequestID)
		if perr == nil {
			if prior.ActorKind != 0 || prior.ActorID != s.UserID || prior.ActorDevice == nil || *prior.ActorDevice != s.DeviceID || prior.Operation != op || !bytes.Equal(prior.PayloadHash, digest) {
				return botConflict("request id was used for a different operation")
			}
			return decodeBotInstallationResult(prior.ResultCBOR, &result)
		}
		if !errors.Is(perr, store.ErrNotFound) {
			return perr
		}
		row, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil {
			return notFound(err)
		}
		if req.ExpectedRevision != row.Revision {
			return botConflict("installation revision changed")
		}
		managedRoleID := row.RoleID
		if row.State == store.BotInstallationDisabled && (!remove || row.RemovedAt != nil) {
			result = row
		} else {
			if row.UserID != nil {
				changed = true
			}
			row.State, row.Updated = store.BotInstallationDisabled, now
			row.Revision++
			if remove {
				row.RemovedAt = &now
				// The installation's optional managed-role foreign key must be cleared in the
				// tombstone update before the role row is deleted below.
				row.RoleID = nil
			}
			if err := tx.UpdateBotInstallation(r.Context(), row.ID, req.ExpectedRevision, row); err != nil {
				return err
			}
			if err := appendBotInstallationEvent(r.Context(), tx, row); err != nil {
				return err
			}
			if row.RuntimeID != nil {
				if err := tx.DeleteBotClaim(r.Context(), row.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
			if row.UserID != nil {
				if _, err := tx.DeleteBotSessionBindingsByInstallation(r.Context(), row.ID); err != nil {
					return err
				}
				if row.DeviceID == nil {
					return store.ErrConflict
				}
				if _, err := tx.DeleteSessionsByDevice(r.Context(), *row.DeviceID); err != nil {
					return err
				}
				if remove {
					if _, err := tx.DeleteCommunityChannelMembers(r.Context(), cid, *row.UserID); err != nil {
						return err
					}
					member, err := tx.GetMember(r.Context(), cid, *row.UserID)
					if err == nil {
						if err := appendMemberEpisodeEvent(r.Context(), tx, botprotocol.EventMemberLeave, cid, *row.UserID, member.EpisodeID, now); err != nil {
							return err
						}
						if err := tx.DeleteMember(r.Context(), cid, *row.UserID); err != nil {
							return err
						}
					} else if !errors.Is(err, store.ErrNotFound) {
						return err
					}
					if managedRoleID != nil {
						if err := tx.DeleteRole(r.Context(), cid, *managedRoleID); err != nil && !errors.Is(err, store.ErrNotFound) {
							return err
						}
					}
				}
			}
			result = row
		}
		encoded, err := encodeBotInstallationResult(result)
		if err != nil {
			return err
		}
		return tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: cid, RequestID: req.RequestID, InstallationID: result.ID,
			ActorKind: 0, ActorID: s.UserID, ActorDevice: &s.DeviceID, Operation: op, PayloadHash: digest, ResultCBOR: encoded, Created: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if changed && d.BotRevoked != nil {
		d.BotRevoked(context.WithoutCancel(r.Context()), result, remove)
	}
	if remove {
		d.noContent(w, r)
		return
	}
	d.writeBotInstallation(w, r, http.StatusOK, result)
}

func runtimeBearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || len(value) <= len("Bearer ") || strings.Contains(value[len("Bearer "):], " ") {
		return ""
	}
	return value[len("Bearer "):]
}

func (d Deps) authenticateBotRuntime(w http.ResponseWriter, r *http.Request, write bool) (id.ID, bool) {
	if d.BotRuntime == nil {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return id.ID{}, false
	}
	if d.Limiter != nil {
		rate := d.Limiter.Config()
		cls := dsRateClass(rate, dsClassRead)
		if write {
			cls = dsRateClass(rate, dsClassWrite)
		}
		var trusted []netip.Prefix
		if d.Config != nil {
			trusted = d.Config.Server.TrustedProxyCIDRs
		}
		addr := server.RateKey(server.RealIP(r, trusted))
		if ok, wait := d.Limiter.Allow(server.Class{Name: "bot-runtime-source-" + cls.Name, PerSecond: cls.PerSecond, Burst: cls.Burst}, addr); !ok {
			server.WriteError(w, server.RateLimitedAfter(wait))
			return id.ID{}, false
		}
	}
	runtimeID, err := d.BotRuntime.Authenticate(r.Context(), runtimeBearer(r))
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return id.ID{}, false
	}
	if d.Limiter != nil {
		rate := d.Limiter.Config()
		cls := dsRateClass(rate, dsClassRead)
		if write {
			cls = dsRateClass(rate, dsClassWrite)
		}
		if ok, wait := d.Limiter.Allow(server.Class{Name: "bot-runtime-id-" + cls.Name, PerSecond: cls.PerSecond, Burst: cls.Burst}, runtimeID.String()); !ok {
			server.WriteError(w, server.RateLimitedAfter(wait))
			return id.ID{}, false
		}
	}
	return runtimeID, true
}

func verifyLiveRuntime(ctx context.Context, tx store.Repository, runtimeID id.ID, bearer string) error {
	if err := tx.LockBotRuntime(ctx, runtimeID); err != nil {
		return err
	}
	runtime, err := tx.GetBotRuntime(ctx, runtimeID)
	if err != nil {
		return err
	}
	if runtime.RevokedAt != nil || !bytes.Equal(runtime.TokenHash, auth.TokenHash(bearer)) {
		return server.Errorf(server.CodeUnauthenticated, "")
	}
	return nil
}

func (d Deps) ListRuntimeInstallations(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := d.authenticateBotRuntime(w, r, false)
	if !ok {
		return
	}
	after := id.ID{}
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = id.Parse(raw)
		if err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "after must be a lowercase id"))
			return
		}
	}
	// v=2 (Task 10c) asks for the widened installation: the full capability mask plus the restart
	// counters, the reported version and the music defaults. Without it a runtime gets the eight
	// base fields with the original capability byte.
	widened := r.URL.Query().Get("v") == "2"
	rows, err := d.Repo.ListBotInstallationsForRuntime(r.Context(), runtimeID, after, 100)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	items := make([]any, len(rows))
	for i, row := range rows {
		value, err := botRuntimeInstallationValue(row, widened)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		items[i] = value
	}
	d.write(w, r, http.StatusOK, items)
}

func (d Deps) ClaimBotInstallation(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := d.authenticateBotRuntime(w, r, true)
	if !ok {
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botRuntimeClaimRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request id is required"))
		return
	}
	bearer := runtimeBearer(r)
	now := d.Clock.Now().Unix()
	if now < 0 || now > math.MaxInt64-600 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is outside the claim range"))
		return
	}
	var claim store.BotClaimRow
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := verifyLiveRuntime(r.Context(), tx, runtimeID, bearer); err != nil {
			return err
		}
		initial, err := tx.GetBotInstallation(r.Context(), installationID)
		if err != nil {
			return err
		}
		if err := tx.LockBotCommunity(r.Context(), initial.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(r.Context(), installationID); err != nil {
			return err
		}
		row, err := tx.GetBotInstallation(r.Context(), installationID)
		if err != nil {
			return err
		}
		if row.RemovedAt != nil || row.State != store.BotInstallationPending || row.UserID != nil {
			return botConflict("installation is not awaiting enrollment")
		}
		if row.RuntimeID != nil && *row.RuntimeID != runtimeID {
			return server.Errorf(server.CodeForbidden, "installation is assigned to another runtime")
		}
		old, err := tx.GetBotClaim(r.Context(), installationID)
		if err == nil && old.Expires > now {
			if old.RuntimeID != runtimeID || old.RequestID != req.RequestID {
				return botConflict("installation already has a live claim")
			}
			claim = old
			return nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		claim = store.BotClaimRow{InstallationID: installationID, RuntimeID: runtimeID, ClaimID: id.New(), RequestID: req.RequestID, Expires: now + 600}
		return tx.PutBotClaim(r.Context(), claim)
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	expires, ok := checkedUint64(claim.Expires)
	if !ok {
		server.WriteError(w, d.storeError(r, store.ErrConflict))
		return
	}
	d.write(w, r, http.StatusOK, []any{claim.InstallationID, claim.ClaimID, expires})
}

func botHandle(repo store.Repository, ctx context.Context, installation id.ID) (string, error) {
	base := "bot_" + installation.String()[:20]
	for i := 0; i < 100; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s_%d", base, i)
		}
		normalized, err := auth.NormalizeHandle(candidate)
		if err != nil {
			return "", err
		}
		_, err = repo.GetUserByUsername(ctx, normalized)
		if errors.Is(err, store.ErrNotFound) {
			return normalized, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", store.ErrConflict
}

func botRuntimeEnrollmentHash(req botRuntimeEnrollRequest) []byte {
	return botPayloadHash(struct {
		Request, Claim id.ID
		UMK, SSK, Sig  []byte
		Device         deviceRequest
	}{req.RequestID, req.ClaimID, req.UMKPub, req.SSKPub, req.SigUMKSSK, req.Device})
}

func (d Deps) EnrollBotInstallation(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := d.authenticateBotRuntime(w, r, true)
	if !ok {
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botRuntimeEnrollRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() || req.ClaimID.IsZero() || len(req.UMKPub) != 32 || len(req.SSKPub) != 32 || len(req.SigUMKSSK) != 64 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid enrollment identity fields"))
		return
	}
	if err := req.Device.validate(); err != nil {
		server.WriteError(w, err)
		return
	}
	if d.BotIdentity == nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "bot identity verifier is not wired"))
		return
	}
	var expected identity.ExpectedNativeBot
	copy(expected.UMKPub[:], req.UMKPub)
	copy(expected.SSKPub[:], req.SSKPub)
	copy(expected.SigUMKSSK[:], req.SigUMKSSK)
	expected.DeviceID = req.Device.DeviceID
	expected.ExpectPlaceholder = true
	if _, err := d.BotIdentity.VerifyNativeBot(r.Context(), req.Device.Credential, req.Device.DSKPub, expected); err != nil {
		if errors.Is(err, identity.ErrUnavailable) {
			server.WriteError(w, server.Errorf(server.CodeInternal, "bot identity verifier unavailable"))
		} else {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "bot enrollment credential is invalid"))
		}
		return
	}
	bearer := runtimeBearer(r)
	now := d.Clock.Now().Unix()
	digest := botRuntimeEnrollmentHash(req)
	var result store.BotInstallationRow
	var replay bool
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := verifyLiveRuntime(r.Context(), tx, runtimeID, bearer); err != nil {
			return err
		}
		row, err := tx.GetBotInstallation(r.Context(), installationID)
		if err != nil {
			return err
		}
		if err := tx.LockBotCommunity(r.Context(), row.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(r.Context(), installationID); err != nil {
			return err
		}
		row, err = tx.GetBotInstallation(r.Context(), installationID)
		if err != nil {
			return err
		}
		prior, perr := tx.GetBotOperation(r.Context(), row.CommunityID, req.RequestID)
		if perr == nil {
			if prior.ActorKind != 1 || prior.ActorID != runtimeID || prior.InstallationID != installationID || prior.Operation != botOpEnroll || !bytes.Equal(prior.PayloadHash, digest) {
				return botConflict("request id was used for a different enrollment")
			}
			if row.RuntimeID == nil || *row.RuntimeID != runtimeID {
				return server.Errorf(server.CodeForbidden, "enrollment receipt belongs to another runtime")
			}
			if err := decodeBotInstallationResult(prior.ResultCBOR, &result); err != nil {
				return err
			}
			replay = true
			return nil
		}
		if !errors.Is(perr, store.ErrNotFound) {
			return perr
		}
		if row.RemovedAt != nil || row.State != store.BotInstallationPending || row.UserID != nil {
			return botConflict("installation is not awaiting enrollment")
		}
		claim, err := tx.GetBotClaim(r.Context(), installationID)
		if err != nil {
			return err
		}
		if claim.RuntimeID != runtimeID || claim.ClaimID != req.ClaimID || claim.Expires <= now {
			return server.Errorf(server.CodeForbidden, "claim is expired or does not match")
		}
		if row.RuntimeID != nil && *row.RuntimeID != runtimeID {
			return server.Errorf(server.CodeForbidden, "installation is assigned to another runtime")
		}
		handle, err := botHandle(tx, r.Context(), installationID)
		if err != nil {
			return err
		}
		userID := id.New()
		roleID := id.New()
		approver := row.ApprovedBy
		user := store.UserRow{ID: userID, Username: handle, Display: "Dilla Bot", Kind: 1, UMKPub: req.UMKPub, SSKPub: req.SSKPub, SigUMKSSK: req.SigUMKSSK, Created: now}
		device := store.DeviceRow{ID: req.Device.DeviceID, UserID: userID, DSKPub: req.Device.DSKPub, Tier: tierNative, SignerTier: tierNative, CredentialBlob: req.Device.Credential, LastSeen: now, Created: now}
		if err := tx.CreateUser(r.Context(), user); err != nil {
			return err
		}
		if err := tx.CreateBotDevice(r.Context(), device); err != nil {
			return err
		}
		if err := tx.PutMember(r.Context(), store.MemberOfCommunityRow{CommunityID: row.CommunityID, UserID: userID, Joined: now, Nick: "Dilla Bot"}); err != nil {
			return err
		}
		com, err := tx.GetCommunity(r.Context(), row.CommunityID)
		if err != nil {
			return err
		}
		snap, err := LoadSnapshot(r.Context(), tx, row.CommunityID, approver, nil)
		if err != nil {
			return err
		}
		have := snap.Resolve(approver)
		if !have.Has(PermManageCommunity) {
			return server.Errorf(server.CodeForbidden, "enabling actor lost community authority")
		}
		if err := checkBotCapabilityAuthority(have, uint64(row.Capabilities)); err != nil {
			return err
		}
		highest := uint64(0)
		for _, role := range snap.Roles {
			if snap.Held[role.ID] && role.Position > highest {
				highest = role.Position
			}
		}
		position := uint64(1)
		if approver != com.Owner {
			if highest < 2 {
				return server.Errorf(server.CodeForbidden, "no safe role position below the configuring human")
			}
			position = highest - 1
		}
		bits := botRoleBits(uint64(row.Capabilities), have) &^ moderatorBits
		role := store.RoleRow{ID: roleID, CommunityID: row.CommunityID, Name: "bot_" + installationID.String()[:8], Position: position, Allow: uint64(bits), Created: now}
		if err := tx.PutRole(r.Context(), role); err != nil {
			return err
		}
		if err := tx.PutMemberRole(r.Context(), row.CommunityID, userID, roleID); err != nil {
			return err
		}
		row.RuntimeID = &runtimeID
		row.UserID = &userID
		row.DeviceID = &device.ID
		row.RoleID = &roleID
		row.State = store.BotInstallationEnrolling
		row.Revision++
		row.Updated = now
		if err := tx.UpdateBotInstallation(r.Context(), row.ID, row.Revision-1, row); err != nil {
			return err
		}
		if err := appendBotInstallationEvent(r.Context(), tx, row); err != nil {
			return err
		}
		result = row
		encoded, err := encodeBotInstallationResult(row)
		if err != nil {
			return err
		}
		return tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: row.CommunityID, RequestID: req.RequestID, InstallationID: row.ID, ActorKind: 1, ActorID: runtimeID, Operation: botOpEnroll, PayloadHash: digest, ResultCBOR: encoded, Created: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if replay {
		d.write(w, r, http.StatusOK, []any{*result.UserID, *result.DeviceID, func() string {
			u, e := d.Repo.GetUser(r.Context(), *result.UserID)
			if e != nil {
				return ""
			}
			return u.Username
		}()})
		return
	}
	if d.BotEnrolled != nil {
		d.BotEnrolled(context.WithoutCancel(r.Context()), result)
	}
	d.write(w, r, http.StatusCreated, []any{*result.UserID, *result.DeviceID, func() string {
		u, e := d.Repo.GetUser(r.Context(), *result.UserID)
		if e != nil {
			return ""
		}
		return u.Username
	}()})
}

func (d Deps) FinalizeBotCredential(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok || s.Bot == nil {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botCredentialRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() || len(req.Credential) == 0 || len(req.Credential) > maxCredBlob {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid credential finalization"))
		return
	}
	row, err := d.Repo.GetBotInstallation(r.Context(), installationID)
	if err != nil || row.ID != s.Bot.InstallationID || row.UserID == nil || *row.UserID != s.UserID || row.DeviceID == nil || *row.DeviceID != s.DeviceID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "not found"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	digest := botPayloadHash(struct {
		Request, Installation id.ID
		Revision              uint64
		Credential            []byte
	}{req.RequestID, installationID, req.ExpectedRevision, req.Credential})
	now := d.Clock.Now().Unix()
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBotRuntime(r.Context(), *row.RuntimeID); err != nil {
			return err
		}
		runtime, err := tx.GetBotRuntime(r.Context(), *row.RuntimeID)
		if err != nil {
			return err
		}
		if runtime.RevokedAt != nil {
			return server.Errorf(server.CodeUnauthenticated, "")
		}
		if err := tx.LockBotCommunity(r.Context(), row.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(r.Context(), row.ID); err != nil {
			return err
		}
		cur, err := tx.GetBotInstallation(r.Context(), row.ID)
		if err != nil {
			return err
		}
		if cur.RemovedAt != nil || cur.RuntimeID == nil || row.RuntimeID == nil || *cur.RuntimeID != *row.RuntimeID ||
			cur.UserID == nil || *cur.UserID != s.UserID || cur.DeviceID == nil || *cur.DeviceID != s.DeviceID ||
			(cur.State != store.BotInstallationEnrolling && cur.State != store.BotInstallationJoining && cur.State != store.BotInstallationReady) {
			return server.Errorf(server.CodeUnauthenticated, "")
		}
		prior, perr := tx.GetBotOperation(r.Context(), row.CommunityID, req.RequestID)
		if perr == nil {
			if prior.ActorKind != 2 || prior.ActorID != s.UserID || prior.ActorDevice == nil || *prior.ActorDevice != s.DeviceID || prior.InstallationID != row.ID || prior.Operation != botOpCredential || !bytes.Equal(prior.PayloadHash, digest) {
				return botConflict("request id was used for a different credential")
			}
			return decodeBotInstallationResult(prior.ResultCBOR, &row)
		}
		if !errors.Is(perr, store.ErrNotFound) {
			return perr
		}
		if cur.UserID == nil || *cur.UserID != s.UserID || cur.DeviceID == nil || *cur.DeviceID != s.DeviceID || cur.Revision != req.ExpectedRevision {
			return botConflict("installation revision or identity changed")
		}
		if cur.CredentialVerifiedAt != nil {
			return botConflict("final credential is already bound")
		}
		if cur.State != store.BotInstallationEnrolling {
			return botConflict("installation is not enrolling")
		}
		user, err := tx.GetUser(r.Context(), s.UserID)
		if err != nil {
			return err
		}
		device, err := tx.GetDevice(r.Context(), s.DeviceID)
		if err != nil {
			return err
		}
		if d.BotIdentity == nil {
			return server.Errorf(server.CodeInternal, "bot identity verifier is not wired")
		}
		var expected identity.ExpectedNativeBot
		copy(expected.UMKPub[:], user.UMKPub)
		copy(expected.SSKPub[:], user.SSKPub)
		copy(expected.SigUMKSSK[:], user.SigUMKSSK)
		expected.DeviceID = device.ID
		expected.UserID = user.ID
		if _, err := d.BotIdentity.VerifyNativeBot(r.Context(), req.Credential, device.DSKPub, expected); err != nil {
			return server.Errorf(server.CodeInvalidRequest, "final bot credential is invalid")
		}
		if err := tx.FinalizeBotDeviceCredential(r.Context(), device.ID, req.Credential, now); err != nil {
			return err
		}
		verified := now
		cur.CredentialVerifiedAt = &verified
		cur.State = store.BotInstallationJoining
		cur.Revision++
		cur.Updated = now
		if err := tx.UpdateBotInstallation(r.Context(), cur.ID, req.ExpectedRevision, cur); err != nil {
			return err
		}
		if err := appendBotInstallationEvent(r.Context(), tx, cur); err != nil {
			return err
		}
		row = cur
		encoded, err := encodeBotInstallationResult(cur)
		if err != nil {
			return err
		}
		return tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: cur.CommunityID, RequestID: req.RequestID, InstallationID: cur.ID, ActorKind: 2, ActorID: s.UserID, ActorDevice: &s.DeviceID, Operation: botOpCredential, PayloadHash: digest, ResultCBOR: encoded, Created: now})
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.writeRuntimeBotInstallation(w, r, http.StatusOK, row)
}

func (d Deps) MarkBotReady(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok || s.Bot == nil {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botStateRequest
	if err := server.DecodeBody(w, r, maxBotRuntimeBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.RequestID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "request id is required"))
		return
	}
	row, err := d.Repo.GetBotInstallation(r.Context(), installationID)
	if err != nil || row.ID != s.Bot.InstallationID || row.UserID == nil || *row.UserID != s.UserID || row.DeviceID == nil || *row.DeviceID != s.DeviceID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "not found"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	digest := botPayloadHash(struct {
		Request, Installation id.ID
		Revision              uint64
	}{req.RequestID, installationID, req.ExpectedRevision})
	now := d.Clock.Now().Unix()
	var missingChannels []id.ID
	err = d.withDeviceListTransaction(r.Context(), func(ctx context.Context, tx store.Repository) error {
		r := r.WithContext(ctx)
		if row.RuntimeID == nil {
			return botConflict("installation has no runtime")
		}
		if err := tx.LockBotRuntime(r.Context(), *row.RuntimeID); err != nil {
			return err
		}
		runtime, err := tx.GetBotRuntime(r.Context(), *row.RuntimeID)
		if err != nil {
			return err
		}
		if runtime.RevokedAt != nil {
			return server.Errorf(server.CodeUnauthenticated, "")
		}
		if err := tx.LockBotCommunity(r.Context(), row.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(r.Context(), row.ID); err != nil {
			return err
		}
		cur, err := tx.GetBotInstallation(r.Context(), row.ID)
		if err != nil {
			return err
		}
		if cur.RemovedAt != nil || cur.RuntimeID == nil || row.RuntimeID == nil || *cur.RuntimeID != *row.RuntimeID ||
			cur.UserID == nil || *cur.UserID != s.UserID || cur.DeviceID == nil || *cur.DeviceID != s.DeviceID ||
			(cur.State != store.BotInstallationJoining && cur.State != store.BotInstallationReady) {
			return server.Errorf(server.CodeUnauthenticated, "")
		}
		prior, perr := tx.GetBotOperation(r.Context(), cur.CommunityID, req.RequestID)
		if perr == nil {
			if prior.ActorKind != 2 || prior.ActorID != s.UserID || prior.ActorDevice == nil || *prior.ActorDevice != s.DeviceID || prior.InstallationID != row.ID || prior.Operation != botOpReady || !bytes.Equal(prior.PayloadHash, digest) {
				return botConflict("request id was used for a different readiness operation")
			}
			return decodeBotInstallationResult(prior.ResultCBOR, &row)
		}
		if !errors.Is(perr, store.ErrNotFound) {
			return perr
		}
		if cur.UserID == nil || *cur.UserID != s.UserID || cur.DeviceID == nil || *cur.DeviceID != s.DeviceID || cur.State != store.BotInstallationJoining || cur.Revision != req.ExpectedRevision {
			return botConflict("installation revision or state changed")
		}
		ready, missing, err := d.botReadiness(r.Context(), tx, cur)
		if err != nil {
			return err
		}
		if !ready {
			missingChannels = missing
			return botConflict("installation prerequisites are still pending")
		}
		approver, err := tx.GetUser(r.Context(), cur.ApprovedBy)
		if err != nil || approver.Kind != kindHuman || approver.DisabledAt != nil || approver.DeletedAt != nil {
			return server.Errorf(server.CodeForbidden, "the approving human is no longer eligible")
		}
		approverSession := auth.Session{UserID: cur.ApprovedBy}
		community, have, err := humanForBot(r, tx, cur.CommunityID, approverSession)
		if err != nil {
			return err
		}
		if err := checkBotCapabilityAuthority(have, uint64(cur.Capabilities)); err != nil {
			return err
		}
		if cur.RoleID == nil {
			return botConflict("managed role is missing")
		}
		managedRole, err := tx.GetRole(r.Context(), *cur.RoleID)
		if err != nil {
			return err
		}
		if cur.ApprovedBy != community.Owner {
			snap, err := LoadSnapshot(r.Context(), tx, cur.CommunityID, cur.ApprovedBy, nil)
			if err != nil {
				return err
			}
			highest := uint64(0)
			for _, role := range snap.Roles {
				if snap.Held[role.ID] && role.Position > highest {
					highest = role.Position
				}
			}
			if highest < 2 || managedRole.Position >= highest {
				return server.Errorf(server.CodeForbidden, "managed role no longer sits below the approving human")
			}
		}
		managedRole.Allow = uint64(botRoleBits(uint64(cur.Capabilities), have))
		managedRole.Deny = 0
		if err := tx.PutRole(r.Context(), managedRole); err != nil {
			return err
		}
		cur.State = store.BotInstallationReady
		cur.Revision++
		cur.Updated = now
		if err := tx.UpdateBotInstallation(r.Context(), cur.ID, req.ExpectedRevision, cur); err != nil {
			return err
		}
		if err := appendBotInstallationEvent(r.Context(), tx, cur); err != nil {
			return err
		}
		row = cur
		encoded, err := encodeBotInstallationResult(cur)
		if err != nil {
			return err
		}
		return tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: cur.CommunityID, RequestID: req.RequestID, InstallationID: cur.ID, ActorKind: 2, ActorID: s.UserID, ActorDevice: &s.DeviceID, Operation: botOpReady, PayloadHash: digest, ResultCBOR: encoded, Created: now})
	})
	if err != nil {
		if len(missingChannels) > 0 && d.BotMembershipPending != nil {
			// SyncGroupMembers writes through the delivery service. Run it only
			// after the readiness transaction has released its write lock.
			d.BotMembershipPending(context.WithoutCancel(r.Context()), row, missingChannels)
		}
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.writeRuntimeBotInstallation(w, r, http.StatusOK, row)
}

func (d Deps) botReadiness(ctx context.Context, repo store.Repository, row store.BotInstallationRow) (bool, []id.ID, error) {
	if row.CredentialVerifiedAt == nil || row.DeviceID == nil || row.UserID == nil || d.DeviceLists == nil {
		return false, nil, nil
	}
	user, err := repo.GetUser(ctx, *row.UserID)
	if err != nil {
		return false, nil, err
	}
	list, err := repo.GetDeviceList(ctx, *row.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	entries, err := d.DeviceLists.Verify(ctx, list.Blob, user.SSKPub, *row.UserID)
	if err != nil {
		return false, nil, nil //nolint:nilerr // An invalid signed device list must fail readiness closed.
	}
	if len(entries) != 1 || entries[0].Revoked || !bytes.Equal(entries[0].DeviceID, row.DeviceID[:]) {
		return false, nil, nil
	}
	device, err := repo.GetDevice(ctx, *row.DeviceID)
	if err != nil {
		return false, nil, err
	}
	if !bytes.Equal(entries[0].DSKPub, device.DSKPub) || entries[0].Tier != tierNative {
		return false, nil, nil
	}
	count, err := repo.CountKeyPackages(ctx, *row.DeviceID, d.Clock.Now().Unix())
	if err != nil {
		return false, nil, err
	}
	if count == 0 {
		return false, nil, nil
	}
	channels, err := repo.ListChannels(ctx, row.CommunityID)
	if err != nil {
		return false, nil, err
	}
	groups, err := repo.GroupsForDevice(ctx, *row.DeviceID)
	if err != nil {
		return false, nil, err
	}
	joined := make(map[id.ID]bool)
	for _, gid := range groups {
		g, err := repo.GetGroup(ctx, gid)
		if err != nil {
			continue
		}
		if g.ClosedAt != nil || g.CommunityID == nil || *g.CommunityID != row.CommunityID || g.Kind != GroupText {
			continue
		}
		members, err := repo.ListMembers(ctx, gid)
		if err != nil {
			return false, nil, err
		}
		for _, m := range members {
			if m.UserID == *row.UserID && m.DeviceID == *row.DeviceID && m.RemovedEpoch == nil {
				joined[g.TargetID] = true
			}
		}
	}
	var missing []id.ID
	for _, channel := range channels {
		if channel.DeletedAt != nil || channel.Kind != ChannelText || channel.Mode != ModeE2EE {
			continue
		}
		resolver := NewResolver(repo)
		allowed, err := resolver.Resolve(ctx, *row.UserID, channel)
		if err != nil {
			return false, nil, err
		}
		if allowed.Has(PermViewChannel) && !joined[channel.ID] {
			missing = append(missing, channel.ID)
		}
	}
	if len(missing) > 0 {
		return false, missing, nil
	}
	return true, nil, nil
}
