package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

type automodGrantFixture struct {
	handler                                          http.Handler
	deps                                             api.Deps
	community, group                                 id.ID
	human, humanDevice, bot, botDevice, installation id.ID
	humanToken, botToken                             string
	current                                          ds.CurrentBotInteractionGroupContext
	clk                                              *clock.Fake
}

func newAutoModGrantFixture(t *testing.T) *automodGrantFixture {
	t.Helper()
	f := &automodGrantFixture{community: id.New(), group: id.New(), humanToken: "grant-human", botToken: "grant-bot"}
	var current ds.CurrentBotInteractionGroupContext
	var currentErr error
	f.handler, f.deps = newTestAPIFull(t, nil, func(d *api.Deps) {
		f.clk = d.Clock.(*clock.Fake)
		d.StepUps = api.NewStepUps(d.Clock, api.StepUpTTL)
		d.CurrentBotInteractionGroupContext = func(_ context.Context, g id.ID) (ds.CurrentBotInteractionGroupContext, error) {
			if g != f.group || currentErr != nil {
				return ds.CurrentBotInteractionGroupContext{}, errors.New("no current context")
			}
			return current, nil
		}
	})
	f.human = id.New()
	f.humanDevice = id.New()
	f.bot = id.New()
	f.botDevice = id.New()
	f.installation = id.New()
	ctx := context.Background()
	now := f.deps.Clock.Now().Unix()
	verified := now
	owner := newAPITestUser(f.human, "automod-human", now)
	if err := f.deps.Repo.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create human: %v", err)
	}
	if err := f.deps.Repo.CreateDevice(ctx, newAPITestDevice(f.humanDevice, f.human, now)); err != nil {
		t.Fatalf("create human device: %v", err)
	}
	if err := f.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: f.community, Owner: f.human, Name: "grant-test", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatalf("create community: %v", err)
	}
	botUser := newAPITestUser(f.bot, "automod-bot", now)
	botUser.Kind = 1
	if err := f.deps.Repo.CreateUser(ctx, botUser); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	botDevice := newAPITestDevice(f.botDevice, f.bot, now)
	botDevice.DSKPub[0] = 1
	botDevice.VerifiedAt = &verified
	if err := f.deps.Repo.CreateDevice(ctx, botDevice); err != nil {
		t.Fatalf("create bot device: %v", err)
	}
	for _, uid := range []id.ID{f.human, f.bot} {
		if err := f.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: uid, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{0x51}, 32), Created: now, Rotated: now}
	if err := f.deps.Repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	verifiedAt := now
	installation := store.BotInstallationRow{ID: f.installation, CommunityID: f.community, RuntimeID: &runtime.ID, UserID: &f.bot, DeviceID: &f.botDevice, State: store.BotInstallationReady, Revision: 1, ApprovedBy: f.human, ApprovedAt: now, CredentialVerifiedAt: &verifiedAt, Created: now, Updated: now}
	if err := f.deps.Repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatalf("create installation: %v", err)
	}
	f.createSession(t, f.humanToken, f.human, f.humanDevice, nil)
	f.createSession(t, f.botToken, f.bot, f.botDevice, &f.installation)
	f.current = ds.CurrentBotInteractionGroupContext{Binding: ds.BotInteractionGroupBinding{InstallationID: f.installation, CommunityID: f.community, BotUserID: f.bot, ParticipantUserID: f.human, ParticipantDeviceID: f.humanDevice}, Roster: []ds.InteractionGroupLeaf{{UserID: f.bot, DeviceID: f.botDevice}, {UserID: f.human, DeviceID: f.humanDevice}}}
	current = f.current
	return f
}

func (f *automodGrantFixture) createSession(t *testing.T, token string, user, device id.ID, installation *id.ID) {
	t.Helper()
	ctx := context.Background()
	now := f.deps.Clock.Now().Unix()
	hash := auth.TokenHash(token)
	if err := f.deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: hash, DeviceID: device, UserID: user, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if installation != nil {
		if err := f.deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: hash, InstallationID: *installation, UserID: user, DeviceID: device, Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.deps.Sessions.RecordCapabilities(ctx, hash, 3, 1, 1); err != nil {
		t.Fatal(err)
	}
}

func (f *automodGrantFixture) call(t *testing.T, path, token string, body any) (int, []byte) {
	t.Helper()
	encoded, err := cborx.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/cbor")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (f *automodGrantFixture) arm(t *testing.T, step string, community, group, request id.ID, revision, expires uint64, hash []byte) (int, string) {
	t.Helper()
	status, body := f.call(t, "/v1/groups/"+group.String()+"/automod-grants/arm", f.humanToken, []any{request, community, revision, expires, hash, step})
	if status != http.StatusOK {
		return status, ""
	}
	var result []any
	if err := cborx.Unmarshal(body, &result); err != nil || len(result) != 2 {
		t.Fatalf("arm response %x (%v)", body, err)
	}
	token, ok := result[0].(string)
	if !ok || token == "" {
		t.Fatalf("arm token = %#v", result[0])
	}
	return status, token
}

func (f *automodGrantFixture) redeem(t *testing.T, token string, community, group, request, actor, device id.ID, revision, expires uint64, hash []byte) (int, id.ID) {
	t.Helper()
	status, body := f.call(t, "/v1/bot-runtime/installations/"+f.installation.String()+"/automod-grants/redeem", f.botToken, []any{token, community, group, request, revision, expires, actor, device, hash})
	if status != http.StatusOK {
		t.Logf("redeem refusal body: %x", body)
		return status, id.Zero
	}
	var result []any
	if err := cborx.Unmarshal(body, &result); err != nil || len(result) != 2 {
		t.Fatalf("redeem response %x (%v)", body, err)
	}
	accepted, ok := result[0].(uint64)
	if !ok || accepted != 1 {
		t.Fatalf("redeem accepted = %#v", result[0])
	}
	receiptBytes, ok := result[1].([]byte)
	if !ok || len(receiptBytes) != 16 {
		t.Fatalf("receipt = %#v", result[1])
	}
	var receipt id.ID
	copy(receipt[:], receiptBytes)
	return status, receipt
}

func TestAutoModerationGrantArmAndRedeemBindExactTupleAndRetry(t *testing.T) {
	f := newAutoModGrantFixture(t)
	request, hash := id.New(), bytes.Repeat([]byte{0x22}, 32)
	expires := uint64(f.deps.Clock.Now().Unix() + 240)
	if status, _ := f.arm(t, "", f.community, f.group, request, 0, expires, hash); status != http.StatusForbidden {
		t.Fatalf("missing step-up arm=%d", status)
	}
	step, _ := f.deps.StepUps.Issue(f.human, auth.TokenHash(f.humanToken))
	if status, _ := f.arm(t, step, f.community, id.New(), request, 0, expires, hash); status != http.StatusForbidden {
		t.Fatalf("wrong group arm=%d", status)
	}
	if status, _ := f.arm(t, step, f.community, f.group, request, 0, expires, []byte{1}); status != http.StatusBadRequest {
		t.Fatalf("short config hash arm=%d", status)
	}
	// The invalid attempts above do not consume the session-bound step-up.
	status, grant := f.arm(t, step, f.community, f.group, request, 0, expires, hash)
	if status != http.StatusOK {
		t.Fatalf("valid revision-0 arm=%d", status)
	}
	for name, tuple := range map[string]struct {
		c, g, r, a, d id.ID
		rev, exp      uint64
		hash          []byte
	}{
		"community":   {id.New(), f.group, request, f.human, f.humanDevice, 0, expires, hash},
		"group":       {f.community, id.New(), request, f.human, f.humanDevice, 0, expires, hash},
		"request":     {f.community, f.group, id.New(), f.human, f.humanDevice, 0, expires, hash},
		"revision":    {f.community, f.group, request, f.human, f.humanDevice, 1, expires, hash},
		"expiry":      {f.community, f.group, request, f.human, f.humanDevice, 0, expires + 1, hash},
		"actor":       {f.community, f.group, request, id.New(), f.humanDevice, 0, expires, hash},
		"device":      {f.community, f.group, request, f.human, id.New(), 0, expires, hash},
		"config hash": {f.community, f.group, request, f.human, f.humanDevice, 0, expires, bytes.Repeat([]byte{0x33}, 32)},
	} {
		t.Run(name, func(t *testing.T) {
			if got, _ := f.redeem(t, grant, tuple.c, tuple.g, tuple.r, tuple.a, tuple.d, tuple.rev, tuple.exp, tuple.hash); got != http.StatusForbidden {
				t.Fatalf("mismatched redemption=%d", got)
			}
		})
	}
	if status, receipt := f.redeem(t, grant, f.community, f.group, request, f.human, f.humanDevice, 0, expires, hash); status != http.StatusOK || receipt.IsZero() {
		t.Fatalf("redeem=%d receipt=%s", status, receipt)
	} else {
		// Re-register handlers with the persisted repository to simulate dillad restarting
		// after consuming the grant but before the bot received its receipt.
		mux := server.NewMux()
		api.Register(mux, f.deps)
		f.handler = mux
		if status2, receipt2 := f.redeem(t, grant, f.community, f.group, request, f.human, f.humanDevice, 0, expires, hash); status2 != http.StatusOK || receipt2 != receipt {
			t.Fatalf("post-restart exact retry=%d receipt %s; original %s", status2, receipt2, receipt)
		}
	}
	if status, _ := f.redeem(t, grant, f.community, f.group, id.New(), f.human, f.humanDevice, 0, expires, hash); status != http.StatusForbidden {
		t.Fatalf("spent changed request=%d", status)
	}
}

func TestAutoModerationGrantRefusesExpiredStepUpGrantAndRevokedInstallation(t *testing.T) {
	f := newAutoModGrantFixture(t)
	request, hash := id.New(), bytes.Repeat([]byte{0x44}, 32)
	stale, _ := f.deps.StepUps.Issue(f.human, auth.TokenHash(f.humanToken))
	f.clk.Advance(6 * time.Minute)
	if status, _ := f.arm(t, stale, f.community, f.group, request, 0, uint64(f.deps.Clock.Now().Unix()+240), hash); status != http.StatusForbidden {
		t.Fatalf("expired step-up arm=%d", status)
	}
	step, _ := f.deps.StepUps.Issue(f.human, auth.TokenHash(f.humanToken))
	request = id.New()
	expires := uint64(f.deps.Clock.Now().Unix() + 240)
	status, grant := f.arm(t, step, f.community, f.group, request, 0, expires, hash)
	if status != http.StatusOK {
		t.Fatalf("arm=%d", status)
	}
	f.clk.Advance(61 * time.Second)
	if status, _ := f.redeem(t, grant, f.community, f.group, request, f.human, f.humanDevice, 0, expires, hash); status != http.StatusForbidden {
		t.Fatalf("expired unspent grant=%d", status)
	}
	// A separately armed token is refused once its installation is disabled.
	step, _ = f.deps.StepUps.Issue(f.human, auth.TokenHash(f.humanToken))
	request = id.New()
	expires = uint64(f.deps.Clock.Now().Unix() + 240)
	status, grant = f.arm(t, step, f.community, f.group, request, 0, expires, hash)
	if status != http.StatusOK {
		t.Fatalf("second arm=%d", status)
	}
	if status, _ := f.call(t, "/v1/bot-runtime/installations/"+id.New().String()+"/automod-grants/redeem", f.botToken, []any{grant, f.community, f.group, request, uint64(0), expires, f.human, f.humanDevice, hash}); status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("cross-installation path=%d", status)
	}
	row, err := f.deps.Repo.GetBotInstallation(context.Background(), f.installation)
	if err != nil {
		t.Fatal(err)
	}
	row.State = store.BotInstallationDisabled
	row.Revision++
	row.Updated = f.deps.Clock.Now().Unix()
	if err := f.deps.Repo.UpdateBotInstallation(context.Background(), row.ID, row.Revision-1, row); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.redeem(t, grant, f.community, f.group, request, f.human, f.humanDevice, 0, expires, hash); status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Fatalf("revoked installation redemption=%d", status)
	}
}
