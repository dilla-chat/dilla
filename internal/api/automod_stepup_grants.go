package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

const automodGrantTTL = time.Minute

type automodGrantArmRequest struct {
	_                struct{} `cbor:",toarray"`
	RequestID        id.ID
	CommunityID      id.ID
	ExpectedRevision uint64
	RequestExpires   uint64
	ConfigHash       []byte
	StepUp           string
}
type automodGrantArmResponse struct {
	_       struct{} `cbor:",toarray"`
	Token   string
	Expires uint64
}
type automodGrantRedeemRequest struct {
	_                                struct{} `cbor:",toarray"`
	Token                            string
	CommunityID, GroupID, RequestID  id.ID
	ExpectedRevision, RequestExpires uint64
	ActorUser, ActorDevice           id.ID
	ConfigHash                       []byte
}
type automodGrantRedeemResponse struct {
	_        struct{} `cbor:",toarray"`
	Accepted uint64
	Receipt  id.ID
}

func registerAutoModerationGrants(m *server.Mux, d Deps) {
	path := "POST /v1/groups/{group}/automod-grants/arm"
	m.SocketAuthed(path, d.socketChain(auth.ScopeEnrolled, dsClassWrite), http.HandlerFunc(d.armAutoModerationGrant))
	m.SocketAuthed("POST /v1/bot-runtime/installations/{installation}/automod-grants/redeem", botSocketChain(d.Sessions, d.Limiter, "POST /v1/bot-runtime/installations/{installation}/automod-grants/redeem"), http.HandlerFunc(d.redeemAutoModerationGrant))
}

func (d Deps) armAutoModerationGrant(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok || s.Bot != nil || s.Scope != auth.ScopeEnrolled {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	group, err := server.PathID(r, "group")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req automodGrantArmRequest
	if err = server.DecodeBody(w, r, 4096, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	now := d.Clock.Now()
	nowUnix, nowOK := checkedUint64(now.Unix())
	maximumRequestExpiry := now.Add(5 * time.Minute).Unix()
	maximumRequestExpiryUnix, maximumExpiryOK := checkedUint64(maximumRequestExpiry)
	expectedRevision, expectedRevisionOK := checkedInt64(req.ExpectedRevision)
	requestExpires, requestExpiryOK := checkedInt64(req.RequestExpires)
	if !nowOK || !maximumExpiryOK || !expectedRevisionOK || !requestExpiryOK || req.RequestID.IsZero() || req.CommunityID.IsZero() || req.ExpectedRevision > math.MaxInt64 || req.RequestExpires <= nowUnix || req.RequestExpires > maximumRequestExpiryUnix || len(req.ConfigHash) != sha256.Size {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid automatic moderation grant tuple"))
		return
	}
	if _, _, err = humanForBot(r, d.Repo, req.CommunityID, s); err != nil {
		server.WriteError(w, err)
		return
	}
	inst, err := d.Repo.GetActiveBotInstallationByCommunity(r.Context(), req.CommunityID)
	if err != nil || inst.State != store.BotInstallationReady || inst.UserID == nil || inst.DeviceID == nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "ready bot installation required"))
		return
	}
	if d.CurrentBotInteractionGroupContext == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "current interaction context unavailable"))
		return
	}
	current, err := d.CurrentBotInteractionGroupContext(r.Context(), group)
	if err != nil || current.Binding.CommunityID != req.CommunityID || current.Binding.InstallationID != inst.ID || current.Binding.BotUserID != *inst.UserID || current.Binding.ParticipantUserID != s.UserID || current.Binding.ParticipantDeviceID != s.DeviceID || !interactionRosterHas(current.Roster, *inst.UserID, *inst.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current participant interaction required"))
		return
	}
	if _, err = d.Repo.GetMember(r.Context(), req.CommunityID, s.UserID); err != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current community membership required"))
		return
	}
	if d.Clock.Now().Unix() < 0 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "clock unavailable"))
		return
	}
	if err = d.spendBotStepUp(req.StepUp, s); err != nil {
		server.WriteError(w, err)
		return
	}
	if d.Clock.Now().Unix() < 0 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "clock unavailable"))
		return
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "grant generation failed"))
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	expires := d.Clock.Now().Add(automodGrantTTL).Unix()
	expiresUnix, expiresOK := checkedUint64(expires)
	if !expiresOK {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	row := store.BotAutoModerationGrantRow{TokenHash: hash[:], CommunityID: req.CommunityID, InstallationID: inst.ID, GroupID: group, RequestID: req.RequestID, ActorUser: s.UserID, ActorDevice: s.DeviceID, ExpectedRevision: expectedRevision, RequestExpires: requestExpires, ConfigHash: req.ConfigHash, Expires: expires}
	if err = d.Repo.CreateBotAutoModerationGrant(r.Context(), row); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, automodGrantArmResponse{Token: token, Expires: expiresUnix})
}

func (d Deps) redeemAutoModerationGrant(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok || s.Bot == nil || s.Scope != auth.ScopeEnrolled || s.Bot.State != store.BotInstallationReady {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "ready bot runtime session required"))
		return
	}
	iid, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req automodGrantRedeemRequest
	if err = server.DecodeBody(w, r, 8192, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	if req.Token == "" || len(req.ConfigHash) != sha256.Size || req.CommunityID.IsZero() || req.GroupID.IsZero() || req.RequestID.IsZero() || req.ExpectedRevision > math.MaxInt64 || req.RequestExpires == 0 || req.RequestExpires > math.MaxInt64 || req.ActorUser.IsZero() || req.ActorDevice.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid automatic moderation grant tuple"))
		return
	}
	inst, err := d.Repo.GetBotInstallation(r.Context(), iid)
	if err != nil || s.Bot.InstallationID != iid || s.Bot.CommunityID != req.CommunityID || s.UserID.IsZero() || s.DeviceID.IsZero() || inst.CommunityID != req.CommunityID || inst.State != store.BotInstallationReady || inst.RemovedAt != nil || inst.UserID == nil || inst.DeviceID == nil || *inst.UserID != s.UserID || *inst.DeviceID != s.DeviceID {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current ready installation required"))
		return
	}
	if d.CurrentBotInteractionGroupContext == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "current interaction context unavailable"))
		return
	}
	current, err := d.CurrentBotInteractionGroupContext(r.Context(), req.GroupID)
	if err != nil || current.Binding.CommunityID != req.CommunityID || current.Binding.InstallationID != iid || current.Binding.ParticipantUserID != req.ActorUser || current.Binding.ParticipantDeviceID != req.ActorDevice || current.Binding.BotUserID != s.UserID || !interactionRosterHas(current.Roster, s.UserID, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current interaction actor required"))
		return
	}
	user, userErr := d.Repo.GetUser(r.Context(), req.ActorUser)
	if userErr != nil || user.Kind != kindHuman || user.DisabledAt != nil || user.DeletedAt != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current human actor required"))
		return
	}
	if _, err = d.Repo.GetMember(r.Context(), req.CommunityID, req.ActorUser); err != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current actor membership required"))
		return
	}
	snapshot, err := LoadSnapshot(r.Context(), d.Repo, req.CommunityID, req.ActorUser, nil)
	if err != nil || !snapshot.Resolve(req.ActorUser).Has(PermManageCommunity) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current manage_community permission required"))
		return
	}
	if d.Clock.Now().Unix() < 0 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "clock unavailable"))
		return
	}
	tokenHash := sha256.Sum256([]byte(req.Token))
	grant, err := d.Repo.GetBotAutoModerationGrant(r.Context(), tokenHash[:])
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "grant unavailable"))
		return
	}
	if grant.CommunityID != req.CommunityID || grant.InstallationID != iid || grant.GroupID != req.GroupID || grant.RequestID != req.RequestID || grant.ActorUser != req.ActorUser || grant.ActorDevice != req.ActorDevice || grant.ExpectedRevision != int64(req.ExpectedRevision) || grant.RequestExpires != int64(req.RequestExpires) || !bytes.Equal(grant.ConfigHash, req.ConfigHash) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "grant tuple mismatch"))
		return
	}
	nowUnix, nowOK := checkedUint64(d.Clock.Now().Unix())
	if !nowOK {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	if grant.ConsumedAt == 0 && req.RequestExpires <= nowUnix {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "automatic moderation request expired"))
		return
	}
	requestBytes, err := cborx.Marshal([]any{req.CommunityID, req.GroupID, req.RequestID, uint64(63), req.ExpectedRevision, req.RequestExpires, req.ActorUser, req.ActorDevice, req.ConfigHash})
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid grant tuple"))
		return
	}
	requestHash := sha256.Sum256(requestBytes)
	receipt := id.New()
	now := d.Clock.Now().Unix()
	accepted, retry, err := d.Repo.AcceptBotAutoModerationGrant(r.Context(), store.BotAutoModerationGrantRow{TokenHash: tokenHash[:], CommunityID: req.CommunityID, InstallationID: iid, GroupID: req.GroupID, RequestID: req.RequestID, ActorUser: req.ActorUser, ActorDevice: req.ActorDevice, ExpectedRevision: int64(req.ExpectedRevision), RequestExpires: int64(req.RequestExpires), ConfigHash: req.ConfigHash, ConsumedAt: now, RequestHash: requestHash[:], Receipt: receipt[:]})
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrExhausted) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "grant already spent or expired"))
		return
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if accepted.ConsumedAt > grant.Expires || accepted.ConsumedAt == 0 {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "grant expired"))
		return
	}
	_ = retry
	var receiptID id.ID
	copy(receiptID[:], accepted.Receipt)
	d.write(w, r, http.StatusOK, automodGrantRedeemResponse{Accepted: 1, Receipt: receiptID})
}

func interactionRosterHas(roster []ds.InteractionGroupLeaf, user, device id.ID) bool {
	for _, leaf := range roster {
		if leaf.UserID == user && leaf.DeviceID == device {
			return true
		}
	}
	return false
}
