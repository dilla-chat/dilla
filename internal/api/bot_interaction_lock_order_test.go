package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/capability"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

type interactionUploadContextKey struct{}
type interactionUploadBarrierRepo struct {
	store.Repository
	uploadTx chan struct{}
	managed  *interactionListBarrier
}

func (r interactionUploadBarrierRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	if ctx.Value(interactionUploadContextKey{}) == true {
		close(r.uploadTx)
	}
	return r.Repository.Tx(ctx, func(tx store.Repository) error {
		if ctx.Value(interactionUploadContextKey{}) == "managed" {
			if err := r.managed.wait(ctx); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

type interactionListBarrier struct {
	ds.DeviceLists
	once        sync.Once
	wasm        *mlswasi.Runtime
	startUpload chan struct{}
	uploadTx    <-chan struct{}
	cancel      func()
	afterScope  func() error
}

func (v *interactionListBarrier) wait(ctx context.Context) error {
	var barrierErr error
	v.once.Do(func() {
		// The private read already holds the real SQLite writer here. Before
		// starting Upload, probe whether it also reserved the sole WASI slot.
		// A bounded acquisition tests ownership; it does not schedule the race.
		probeCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		inst, err := v.wasm.Acquire(probeCtx)
		cancel()
		if err == nil {
			inst.Release()
			close(v.startUpload)
			// Real DS.Upload signals this only after taking WASI and immediately
			// before it attempts the same real writer transaction.
			select {
			case <-v.uploadTx:
			case <-ctx.Done():
				barrierErr = ctx.Err()
			}
		} else if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			// The corrected read owns WASI before SQL; Upload waits for its slot.
			close(v.startUpload)
		} else {
			barrierErr = err
		}
	})
	if barrierErr != nil {
		return barrierErr
	}
	return nil
}

func (v *interactionListBarrier) Verify(ctx context.Context, blob, ssk []byte, user id.ID) ([]mlswasi.DeviceListEntry, error) {
	if err := v.wait(ctx); err != nil {
		return nil, err
	}
	if v.cancel != nil {
		v.cancel()
	}
	return v.DeviceLists.Verify(ctx, blob, ssk, user)
}

func (v *interactionListBarrier) WithVerification(ctx context.Context, fn func(context.Context) error) error {
	if err := v.DeviceLists.WithVerification(ctx, fn); err != nil {
		return err
	}
	if v.afterScope != nil {
		return v.afterScope()
	}
	return nil
}

func TestBotInteractionPrivateReadAndUploadProgressWithOneWASIInstance(t *testing.T) {
	for _, mode := range []string{"private", "human", "managed", "cancelled", "human_changed_proof"} {
		t.Run(mode, func(t *testing.T) { testInteractionPoolAndSQLProgress(t, mode) })
	}
}
func testInteractionPoolAndSQLProgress(t *testing.T, mode string) {
	h, meta, fixture := newKind3HTTPAPI(t)
	ctx := t.Context()
	var requestID id.ID
	copy(requestID[:], fixture["request"])
	installation := idFromHex(t, meta.InstallationID)
	instance, err := h.deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	group := mustFixtureGroupID(t, mustReadKind3Metadata(t))
	if rec := h.do(t, http.MethodPost, "/v1/bot-runtime/installations/"+installation.String()+"/groups", string(fixture["token"]), mustCBOR(t, []any{id.New(), instance.Generation, fixture["registration"], requestID})); rec.Code != http.StatusCreated {
		t.Fatalf("register private fixture: %d %x", rec.Code, rec.Body.Bytes())
	}
	row, err := h.deps.Repo.GetBotInstallation(ctx, installation)
	if err != nil {
		t.Fatal(err)
	}
	revision := row.Revision
	row.State = store.BotInstallationReady
	row.Revision++
	if err := h.deps.Repo.UpdateBotInstallation(ctx, installation, revision, row); err != nil {
		t.Fatal(err)
	}
	// The unrelated ordinary group gives Upload a distinct local group mutex.
	h.mustCreate(t)
	members, err := h.deps.Repo.ListMembers(ctx, h.groupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("ordinary fixture members: %v", err)
	}
	epoch := h.groupEpoch(t)
	message := apiPrivateMessage(t, epoch, 64)
	if err := h.ds.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	wasm, err := mlswasi.New(ctx, h.fixture.wasm, mlswasi.Options{PoolSize: 1, CacheDir: apiWasmCacheDir(t), Now: h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	uploadTx := make(chan struct{})
	startUpload := make(chan struct{})
	barrier := &interactionListBarrier{DeviceLists: ds.NewDeviceLists(h.deps.Repo, wasm), wasm: wasm, startUpload: startUpload, uploadTx: uploadTx}
	repo := interactionUploadBarrierRepo{Repository: h.deps.Repo, uploadTx: uploadTx, managed: barrier}
	delivery, err := ds.New(ds.Options{Store: repo, Wasm: wasm, Gateway: h.gw, Clock: h.clk, Keys: h.keys, Policy: ds.DefaultPolicy(), Channels: openChannels{}, BotCapability: capability.New(h.deps.Repo), DeviceLists: ds.NewDeviceLists(h.deps.Repo, wasm)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = delivery.Shutdown(context.Background()) })
	h.ds = delivery
	h.groups.DS = delivery
	h.deps.DeviceLists = barrier
	h.groups.Deps = h.deps
	path := "/v1/bot-runtime/installations/" + installation.String() + "/interaction-groups/" + group.String() + "/info"
	token := string(fixture["token"])
	if mode == "human" || mode == "human_changed_proof" {
		path = "/v1/groups/" + group.String() + "/interaction-admission-context"
		token = "lock-order-human"
		now := h.deps.Clock.Now().Unix()
		hash := auth.TokenHash(token)
		if err := h.deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: hash, UserID: idFromHex(t, meta.ParticipantUserID), DeviceID: idFromHex(t, meta.ParticipantDeviceID), Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
			t.Fatal(err)
		}
		if err := h.deps.Sessions.RecordCapabilities(ctx, hash, 3, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRead()
	if mode == "cancelled" {
		barrier.cancel = cancelRead
	}
	if mode == "human_changed_proof" {
		barrier.afterScope = func() error {
			return h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
				receipt, err := tx.GetBotGroupRegistrationReceiptByGroup(ctx, group)
				if err != nil {
					return err
				}
				if err := tx.(store.BotGroupRegistrationReceipts).DeleteBotGroupRegistrationReceiptsByInstallation(ctx, installation); err != nil {
					return err
				}
				receipt.OperationID = id.New()
				return tx.CreateBotGroupRegistrationReceipt(ctx, receipt)
			})
		}
	}
	readDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		if mode == "managed" {
			result, err := delivery.AcceptBotMessage(context.WithValue(readCtx, interactionUploadContextKey{}, "managed"), ds.BotMessageRequest{
				Session:        auth.Session{UserID: *row.UserID, DeviceID: *row.DeviceID, Scope: auth.ScopeEnrolled, WireVersion: 3, Bot: &auth.BotSessionBinding{InstallationID: installation, CommunityID: row.CommunityID, State: store.BotInstallationReady}},
				InstallationID: installation, OperationID: id.New(), AttemptID: id.New(), CommunityID: row.CommunityID, GroupID: group, ExpectedGeneration: instance.Generation, Epoch: 1, Class: 1, Ciphertext: apiPrivateMessage(t, 1, 64),
			})
			if err != nil || !result.NewlyAccepted {
				rec.WriteHeader(500)
			} else {
				rec.WriteHeader(200)
			}
		} else {
			req := httptest.NewRequestWithContext(readCtx, http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			h.mux.ServeHTTP(rec, req)
		}
		readDone <- rec
	}()
	uploadCtx, cancelUpload := context.WithTimeout(ctx, 5*time.Second)
	defer cancelUpload()
	uploadDone := make(chan error, 1)
	go func() {
		select {
		case <-startUpload:
		case <-uploadCtx.Done():
			uploadDone <- uploadCtx.Err()
			return
		}
		_, err := delivery.Upload(context.WithValue(uploadCtx, interactionUploadContextKey{}, true), auth.Session{UserID: members[0].UserID, DeviceID: members[0].DeviceID}, h.groupID, epoch, message, 0)
		uploadDone <- err
	}()
	rec := <-readDone
	uploadErr := <-uploadDone
	wantCode := http.StatusOK
	if mode == "cancelled" {
		wantCode = http.StatusForbidden
	}
	if mode == "human_changed_proof" {
		wantCode = http.StatusServiceUnavailable
	}
	if rec.Code != wantCode || uploadErr != nil {
		t.Fatalf("private read/upload did not progress: read=%d body=%x upload=%v", rec.Code, rec.Body.Bytes(), uploadErr)
	}
	rows, err := h.deps.Repo.ListAppMessages(ctx, h.groupID, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("concurrent upload effects: rows=%d err=%v", len(rows), err)
	}
	if mode == "private" || mode == "human" {
		botDevice := idFromHex(t, meta.BotDeviceID)
		before, err := h.deps.Repo.GetCursor(ctx, botDevice, group)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.deps.Repo.RevokeDevice(ctx, idFromHex(t, meta.ParticipantDeviceID), h.deps.Clock.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		rec := h.do(t, http.MethodGet, path, token, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("revoked participant remained authorized: %d", rec.Code)
		}
		after, err := h.deps.Repo.GetCursor(ctx, botDevice, group)
		if err != nil || after != before {
			t.Fatalf("denied source changed cursor: before=%+v after=%+v err=%v", before, after, err)
		}
	}
	if mode != "managed" {
		privateRows, err := h.deps.Repo.ListAppMessages(ctx, group, 0, 10)
		if err != nil || len(privateRows) != 0 {
			t.Fatalf("read/refusal created private effects: rows=%d err=%v", len(privateRows), err)
		}
	}
	// Both requests must return the single native lease and writer connection.
	if err := h.deps.Repo.Tx(ctx, func(store.Repository) error { return nil }); err != nil {
		t.Fatal(err)
	}
	leaseCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lease, err := wasm.Acquire(leaseCtx)
	if err != nil {
		t.Fatal(fmt.Errorf("WASI lease leaked: %w", err))
	}
	lease.Release()
}
