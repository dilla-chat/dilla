package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestManagedKeyPackageRoutesCheckAuthorityBeforeWireAndIgnoreForgedHeaders(t *testing.T) {
	h := newGroupsAPI(t)
	deps := h.deps
	ctx := context.Background()
	now := deps.Clock.Now().Unix()
	userID, deviceID := id.New(), id.New()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateUser(ctx, store.UserRow{ID: userID, Username: "bot" + userID.String()[:10], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	verified := now
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: deviceID, UserID: userID, DSKPub: pub, Tier: 0, SignerTier: 0, CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	community := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: userID, Name: "bot-keypackages", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: userID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: now, Rotated: now}
	runtime.TokenHash[0] = 1
	if err := deps.Repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &userID, DeviceID: &deviceID,
		State: store.BotInstallationJoining, Revision: 1, ApprovedBy: userID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := deps.Repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := deps.Sessions.Challenge(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	preimage := auth.SessionPreimage(deps.Sessions.InstanceID(), deviceID, nonce, auth.PurposeSession)
	session, err := deps.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: deviceID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: ed25519.Sign(private, preimage)})
	if err != nil {
		t.Fatal(err)
	}
	token := session.Token
	negotiate, _ := cborx.Marshal([]any{[]any{uint64(2)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h.mux, "/v1/sessions/negotiate", negotiate, token); rec.Code != http.StatusOK {
		t.Fatalf("managed Bot wire-2 negotiation = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	upload, err := cborx.Marshal([]any{[]any{[]byte{1}}, nil})
	if err != nil {
		t.Fatal(err)
	}
	publication, err := cborx.Marshal([]any{id.New(), uint64(1), upload})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/bot-runtime/installations/" + installation.ID.String() + "/keypackages"
	inventoryPath := path
	inventoryReq := httptest.NewRequestWithContext(ctx, http.MethodGet, inventoryPath, nil)
	inventoryReq.Header.Set("Authorization", "Bearer "+token)
	inventoryRec := httptest.NewRecorder()
	h.mux.ServeHTTP(inventoryRec, inventoryReq)
	if inventoryRec.Code != http.StatusBadRequest || refusalCode(t, inventoryRec) != "E_VERSION" {
		t.Fatalf("authorized wire-2 inventory with forged wire header absent = %d (%x), want 400 E_VERSION", inventoryRec.Code, inventoryRec.Body.Bytes())
	}
	assertNegotiationVersionError(t, inventoryRec, [][]uint64{{3}, {1}, {1}})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(publication))
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Dilla-Wire-Version", "3")
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || refusalCode(t, recorder) != "E_VERSION" {
		t.Fatalf("authorized wire-2 publication with forged wire header = %d (%x), want 400 E_VERSION", recorder.Code, recorder.Body.Bytes())
	}
	assertNegotiationVersionError(t, recorder, [][]uint64{{3}, {1}, {1}})

	unknown := id.New()
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/bot-runtime/installations/" + unknown.String() + "/keypackages"},
		{http.MethodGet, "/v1/bot-runtime/installations/" + unknown.String() + "/keypackages"},
		{http.MethodGet, "/v1/bot-runtime/installations/" + unknown.String() + "/keypackages/" + id.New().String()},
	} {
		req := httptest.NewRequestWithContext(ctx, tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FORBIDDEN" {
			t.Fatalf("%s %s unknown installation = %d (%x), want authority refusal before E_VERSION", tc.method, tc.path, rec.Code, rec.Body.Bytes())
		}
	}

	// A newly negotiated, current Bot session gets the empty inventory tuple only after its
	// installation authority is established. This checks the mounted GET with a real session
	// token and the cache policy, independent of the publication body refusal above.
	nonce, _, err = deps.Sessions.Challenge(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	newSession, err := deps.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: deviceID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: ed25519.Sign(private, auth.SessionPreimage(deps.Sessions.InstanceID(), deviceID, nonce, auth.PurposeSession))})
	if err != nil {
		t.Fatal(err)
	}
	if rec := postCBORAuth(h.mux, "/v1/sessions/negotiate", mustCBOR(t, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}), newSession.Token); rec.Code != http.StatusOK {
		t.Fatalf("managed Bot wire-3 negotiation = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	get := httptest.NewRequestWithContext(ctx, http.MethodGet, inventoryPath, nil)
	get.Header.Set("Authorization", "Bearer "+newSession.Token)
	got := httptest.NewRecorder()
	h.mux.ServeHTTP(got, get)
	if got.Code != http.StatusOK || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("current managed inventory = %d cache=%q body=%x", got.Code, got.Header().Get("Cache-Control"), got.Body.Bytes())
	}
	var tuple []any
	if err := cborx.Unmarshal(got.Body.Bytes(), &tuple); err != nil || len(tuple) != 4 {
		t.Fatalf("inventory tuple=%v err=%v, want four fields", tuple, err)
	}
	instance, err := deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gen, genOK := tuple[0].(uint64)
	serverTime, timeOK := tuple[1].(uint64)
	ordinary, ordinaryOK := tuple[2].(uint64)
	fallback, fallbackOK := tuple[3].(uint64)
	if !genOK || gen != instance.Generation || !timeOK || serverTime != uint64(h.clk.Now().Unix()) || !ordinaryOK || ordinary != 0 || !fallbackOK || fallback != 0 {
		t.Fatalf("empty inventory tuple=%v", tuple)
	}
}
