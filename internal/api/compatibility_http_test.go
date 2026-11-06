package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type unavailableBotInstallationRepo struct{ store.Repository }

func (r unavailableBotInstallationRepo) GetActiveBotInstallationByCommunity(context.Context, id.ID) (store.BotInstallationRow, error) {
	return store.BotInstallationRow{}, errors.New("injected installation lookup failure")
}

func TestCommunityCompatibilityGateUsesNegotiatedSessionAndPreservesMetadataReads(t *testing.T) {
	e := communityEnv(t)
	_, token := e.NewUser("owner")
	cid := createCommunity(t, e, token)
	installationID := setBotEnabled(t, e, cid, store.BotInstallationPending)
	setWireVersion(e, token, 2)

	status, body := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), token,
		[]any{"changed", nil, nil, nil})
	if status != http.StatusUpgradeRequired {
		t.Fatalf("wire-2 community mutation = %d %x, want 426 E_UPGRADE_REQUIRED", status, body)
	}
	if code := e.ErrCode(body); code != "E_UPGRADE_REQUIRED" {
		t.Fatalf("wire-2 community mutation code = %s, want E_UPGRADE_REQUIRED", code)
	}
	row, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil || row.Name == "changed" {
		t.Fatalf("community after refusal = %+v, %v; mutation must not happen", row, err)
	}
	if status, body = e.Do(http.MethodGet, "/v1/communities/"+cid.String(), token, nil); status != http.StatusOK {
		t.Fatalf("wire-2 metadata read = %d (%x), want 200", status, body)
	}
	encoded, err := cborx.Marshal([]any{"changed", nil, nil, nil})
	if err != nil {
		t.Fatal(err)
	}
	resp := e.Request(t, http.MethodPatch, "/v1/communities/"+cid.String(), token,
		map[string]string{"Content-Type": "application/cbor", "X-Wire-Version": "3"}, encoded)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("forged version header affected gate: = %d %s, want 426 E_UPGRADE_REQUIRED", resp.StatusCode, e.ErrCode(body))
	}
	installation, err := e.Repo.GetBotInstallation(t.Context(), installationID)
	if err != nil {
		t.Fatalf("GetBotInstallation: %v", err)
	}
	installation.State = store.BotInstallationDisabled
	installation.Revision++
	installation.Updated = e.Clk.Now().Unix()
	if err := e.Repo.UpdateBotInstallation(t.Context(), installation.ID, installation.Revision-1, installation); err != nil {
		t.Fatalf("disable installation: %v", err)
	}
	if status, body = e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), token,
		[]any{"disabled-allowed", nil, nil, nil}); status != http.StatusOK {
		t.Fatalf("disabled-community mutation = %d (%x), want 200", status, body)
	}
	e.sess[token] = sessionWithWire(e.sess[token], 3)
	if status, body = e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), token,
		[]any{"changed", nil, nil, nil}); status != http.StatusOK {
		t.Fatalf("wire-3 community mutation = %d (%x), want 200", status, body)
	}
}

func TestReadableContentGateIsScopedToEnabledCommunityAndRunsBeforeWrites(t *testing.T) {
	e := newReadableCompatibilityEnv(t)
	_, token := e.NewUser("owner")
	cid := createCommunity(t, e, token)
	ch := compatibilityReadableChannel(t, e, cid, token)
	other := createCommunity(t, e, token)
	otherCh := compatibilityReadableChannel(t, e, other, token)
	setBotEnabled(t, e, cid, store.BotInstallationError)
	setWireVersion(e, token, 2)

	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", token,
		[]any{compatibilityEnvelope0(t, "gated content")})
	if status != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("enabled-community content write = %d %s, want 426 E_UPGRADE_REQUIRED", status, e.ErrCode(body))
	}
	row, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil || row.Seq != 0 {
		t.Fatalf("gated channel sequence = %d, %v; refusal must not append", row.Seq, err)
	}
	status, body = e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/messages", token, nil)
	if status != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("enabled-community content read = %d %s, want 426 E_UPGRADE_REQUIRED", status, e.ErrCode(body))
	}
	status, body = e.Do(http.MethodPost, "/v1/channels/"+otherCh.String()+"/messages", token,
		[]any{compatibilityEnvelope0(t, "unaffected content")})
	if status != http.StatusOK {
		t.Fatalf("unaffected-community content write = %d (%x), want 200", status, body)
	}
}

func TestMountedCommunityActionRoutesGateAfterAuthorityWithoutSideEffects(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).Register(e.Mux)
	api.NewInvites(e.Repo, e.Clk, "https://dilla.example", log).Register(e.Mux)
	api.NewBans(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	_, token := e.NewUser("owner")
	_, joiner := e.NewUser("joiner")
	cid := createCommunity(t, e, token)
	setBotEnabled(t, e, cid, store.BotInstallationJoining)
	setWireVersion(e, token, 2)
	setWireVersion(e, joiner, 2)

	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"channel create", http.MethodPost, "/v1/communities/" + cid.String() + "/channels", []any{uint64(api.ChannelText), uint64(api.ModeReadable), uint64(api.VisPrivate), nil, "new-channel", "", uint64(0), uint64(0)}},
		{"role create", http.MethodPost, "/v1/communities/" + cid.String() + "/roles", []any{"moderator", uint64(0), uint64(1), uint64(0), uint64(0), uint64(0), uint64(0)}},
		{"invite create", http.MethodPost, "/v1/communities/" + cid.String() + "/invites", []any{uint64(1), uint64(3600), uint64(0)}},
		{"ban create", http.MethodPut, "/v1/communities/" + cid.String() + "/bans/" + e.sess[joiner].UserID.String(), []any{"reason", nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := e.Do(tc.method, tc.path, token, tc.body)
			if status != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
				t.Fatalf("%s = %d %s, want 426 E_UPGRADE_REQUIRED", tc.name, status, e.ErrCode(body))
			}
		})
	}
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", joiner, []any{nil})
	if status != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("wire-2 valid community join = %d %s, want 426 E_UPGRADE_REQUIRED", status, e.ErrCode(body))
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, e.sess[joiner].UserID); err == nil {
		t.Fatal("wire-2 join created membership")
	}
	if status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/channels", token, nil); status != http.StatusOK {
		t.Fatalf("community channel metadata read = %d (%x), want 200", status, body)
	}
	setWireVersion(e, token, 3)
	if ch, _, status := newChannel(t, e, cid, token, uint64(api.ChannelText), uint64(api.ModeE2EE), uint64(api.VisPrivate), "wire-three"); status != http.StatusCreated || ch.IsZero() {
		t.Fatalf("wire-3 channel create = %d, want 201", status)
	}
}

func TestAttachmentRoutesGateAuthorizedChannelBeforeBlobWritesAndKeepHiddenRefusals(t *testing.T) {
	e, ch, owner := blobEnv(t)
	channel, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil || channel.CommunityID == nil {
		t.Fatalf("GetChannel: %v", err)
	}
	setBotEnabled(t, e, *channel.CommunityID, store.BotInstallationPending)
	setWireVersion(e, owner, 2)
	payload := []byte("ciphertext")
	sum := sha256.Sum256(payload)
	url := blobURL(ch, sum[:])
	status, body := e.DoRaw(http.MethodPut, url, owner, "application/octet-stream", payload)
	if status != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("wire-2 attachment upload = %d %s, want 426 E_UPGRADE_REQUIRED", status, e.ErrCode(body))
	}
	if _, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("attachment reference after refusal = %v, want no reference", err)
	}
	_, stranger := e.NewUser("stranger")
	setWireVersion(e, stranger, 2)
	status, body = e.Do(http.MethodGet, url, stranger, nil)
	if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("hidden attachment read = %d %s, want existing 404", status, e.ErrCode(body))
	}
}

func TestCommunityCallAdmissionRequiresNegotiatedWireBeforeTokenOrCallState(t *testing.T) {
	e, ch, token, group, sfu := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, token), 3, nil)
	cid := ownerCommunityOf(t, e, ch)
	setBotEnabled(t, e, cid, store.BotInstallationReady)
	setWireVersion(e, token, 2)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", token, []any{})
	if status != http.StatusUpgradeRequired || e.ErrCode(body) != "E_UPGRADE_REQUIRED" {
		t.Fatalf("wire-2 community call admission = %d %s, want 426 E_UPGRADE_REQUIRED", status, e.ErrCode(body))
	}
	if _, err := e.Repo.GetVoiceSession(t.Context(), ch); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("voice session after call refusal = %v, want none", err)
	}
	if len(sfu.minted()) != 0 {
		t.Fatal("wire-2 call refusal minted an SFU token")
	}
	setWireVersion(e, token, 3)
	if status, body = e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", token, []any{}); status != http.StatusCreated {
		t.Fatalf("wire-3 community call admission = %d (%x), want 201", status, body)
	}
}

func TestWireTwoHumanKeepsDMAttachmentAccessBesideEnabledCommunity(t *testing.T) {
	e, _ := dmEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).Register(e.Mux)
	_, token := e.NewUser("owner")
	bob, _ := e.NewUser("bob")
	community := createCommunity(t, e, token)
	if _, _, status := newChannel(t, e, community, token, uint64(api.ChannelText), uint64(api.ModeE2EE), uint64(api.VisPrivate), "private"); status != http.StatusCreated {
		t.Fatalf("create community channel = %d", status)
	}
	setBotEnabled(t, e, community, store.BotInstallationReady)
	dm, status := openDM(t, e, token, bob)
	if status != http.StatusCreated {
		t.Fatalf("open DM = %d", status)
	}
	blobs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	api.NewBlobs(e.Repo, blobs, api.NewResolver(e.Repo), config.Default().Blobs, e.Clk, log).Register(e.Mux)
	setWireVersion(e, token, 2)
	payload := []byte("dm ciphertext")
	sum := sha256.Sum256(payload)
	status, body := e.DoRaw(http.MethodPut, blobURL(dm, sum[:]), token, "application/octet-stream", payload)
	if status != http.StatusCreated {
		t.Fatalf("wire-2 DM attachment upload = %d (%x), want 201", status, body)
	}
}

func newReadableCompatibilityEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).Register(e.Mux)
	fan := &recordingFanout{}
	keys := api.NewStaticFrankingKeys(api.FrankingKey{ID: id.New(), Key: bytes.Repeat([]byte{0x09}, 32)})
	api.NewReadable(e.Repo, api.NewResolver(e.Repo), fan, keys, e.Clk, log).Register(e.Mux)
	return e
}

func compatibilityReadableChannel(t *testing.T, e *env, cid id.ID, token string) id.ID {
	t.Helper()
	ch, mode, status := newChannel(t, e, cid, token, uint64(api.ChannelText), 1, uint64(api.VisDiscoverable), "readable")
	if status != http.StatusCreated || mode != uint64(api.ModeReadable) {
		t.Fatalf("create readable channel = %d, mode %d", status, mode)
	}
	return ch
}

func compatibilityEnvelope0(t *testing.T, body string) []byte {
	t.Helper()
	encoded, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), nil, nil, body,
		[]any{}, []any{}, bytes.Repeat([]byte{0x06}, 32)})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return encoded
}

func TestCommunityCompatibilityGateRunsAfterCommunityAuthorization(t *testing.T) {
	e := communityEnv(t)
	_, ownerToken := e.NewUser("owner")
	_, strangerToken := e.NewUser("stranger")
	cid := createCommunity(t, e, ownerToken)
	setBotEnabled(t, e, cid, store.BotInstallationPending)
	setWireVersion(e, strangerToken, 2)

	status, body := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), strangerToken,
		[]any{"changed", nil, nil, nil})
	if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("unauthorized community mutation = %d %s, want existing 404", status, e.ErrCode(body))
	}
}

func TestCommunityCompatibilityLookupFailureFailsClosedAsUnavailable(t *testing.T) {
	e := newEnv(t)
	_, token := e.NewUser("owner")
	repo := unavailableBotInstallationRepo{Repository: e.Repo}
	api.NewCommunities(repo, e.DS, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	cid := createCommunity(t, e, token)
	setWireVersion(e, token, 2)
	status, body := e.Do(http.MethodPatch, "/v1/communities/"+cid.String(), token,
		[]any{"changed", nil, nil, nil})
	if status != http.StatusServiceUnavailable || e.ErrCode(body) != "E_UNAVAILABLE" {
		t.Fatalf("compatibility store failure = %d %s, want 503 E_UNAVAILABLE", status, e.ErrCode(body))
	}
	row, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil || row.Name == "changed" {
		t.Fatalf("community after unavailable refusal = %+v, %v; mutation must not happen", row, err)
	}
}

func setWireVersion(e *env, token string, version uint64) {
	s := e.sess[token]
	s.WireVersion = version
	e.sess[token] = s
}

func sessionWithWire(s auth.Session, version uint64) auth.Session {
	s.WireVersion = version
	return s
}

func setBotEnabled(t *testing.T, e *env, community id.ID, state uint8) id.ID {
	t.Helper()
	row, err := e.Repo.GetCommunity(t.Context(), community)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	installationID := id.New()
	if err := e.Repo.CreateBotInstallation(t.Context(), store.BotInstallationRow{
		ID: installationID, CommunityID: community, State: state, Revision: 1,
		ApprovedBy: row.Owner, ApprovedAt: e.Clk.Now().Unix(), Created: e.Clk.Now().Unix(), Updated: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateBotInstallation: %v", err)
	}
	return installationID
}
