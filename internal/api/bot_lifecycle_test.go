//go:build linux

package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	botruntime "github.com/dilla-chat/dilla/internal/bot/runtime"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// This integration catches a coordinator that bootstraps only against test doubles, skips a
// mounted production route, or duplicates remote identity/publication after an accepted response
// is lost. Human owner/community setup uses the same API and step-up flow available to a human;
// every Bot credential, signed list, session, and KeyPackage is created by the lifecycle and the
// real native helper.
func TestManagedBotIdentityLifecycleUsesProductionCoordinator(t *testing.T) {
	root := repositoryRoot(t)
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "cargo", "build", "-p", "dilla-bot-core", "--bin", "dilla-bot-core")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native bot helper: %v\n%s", err, output)
	}
	for _, engine := range lifecycleEngines(t) {
		t.Run(engine.name, func(t *testing.T) {
			testManagedBotIdentityLifecycle(t, engine.repo, engine.removeDeviceList, filepath.Join(root, "target", "debug", "dilla-bot-core"))
		})
	}
}

type lifecycleEngine struct {
	name             string
	repo             store.Repository
	removeDeviceList func(context.Context, id.ID) error
}

// lifecycleEngines exposes a narrowly-scoped fixture mutation for the restore
// regression: after Ensure sets the encrypted helper's published bit, delete
// only that user's latest device-list row through the same SQLite/Postgres DB
// connection used by the API. This models a restore point before list publish
// while leaving helper state and all other server rows intact.
func lifecycleEngines(t *testing.T) []lifecycleEngine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot-lifecycle.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	sqliteRepo := sqlite.New(write, read)
	t.Cleanup(func() { _ = sqliteRepo.Close() })
	engines := []lifecycleEngine{{name: "sqlite", repo: sqliteRepo, removeDeviceList: func(ctx context.Context, user id.ID) error {
		_, err := write.ExecContext(ctx, `DELETE FROM device_lists WHERE user_id = ?`, user[:])
		return err
	}}}
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Log("DILLA_TEST_PG is unset: the Postgres leg runs in CI's service container")
		return engines
	}
	admin, err := postgres.Open(dsn, 1, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	name := "dilla_lifecycle_" + id.New().String()[:16]
	if _, err = admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := postgres.Open(u.String(), 4, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pgProvider, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pgProvider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	pgRepo := postgres.New(db)
	t.Cleanup(func() { _ = pgRepo.Close() })
	engines = append(engines, lifecycleEngine{name: "postgres", repo: pgRepo, removeDeviceList: func(ctx context.Context, user id.ID) error {
		_, err := db.ExecContext(ctx, `DELETE FROM device_lists WHERE user_id = $1`, user[:])
		return err
	}})
	return engines
}

func testManagedBotIdentityLifecycle(t *testing.T, repo store.Repository, removeDeviceList func(context.Context, id.ID) error, helperPath string) {
	t.Helper()
	var manager *botruntime.Manager
	var keyPackageAccepts atomic.Int32
	acceptedKeyPackages := make(chan [2]id.ID, 1)
	wasm, err := os.ReadFile(filepath.Join(repositoryRoot(t), "internal", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("read WASI core: %v", err)
	}
	deviceListRuntime, err := mlswasi.New(context.Background(), wasm, mlswasi.Options{PoolSize: 1, CacheDir: apiWasmCacheDir(t)})
	if err != nil {
		t.Fatalf("start WASI validators: %v", err)
	}
	t.Cleanup(func() { _ = deviceListRuntime.Close(context.Background()) })
	var diagnostics bytes.Buffer
	var delivery *ds.DS
	h, deps := newTestAPIFullWithRepoAndMount(t, repo, nil, func(d *api.Deps) {
		d.StepUps = api.NewStepUps(d.Clock, time.Minute)
		d.DeviceLists = ds.NewDeviceLists(d.Repo, deviceListRuntime)
		externalSenderSeed := make([]byte, ed25519.SeedSize)
		externalSenderSeed[0] = 1
		externalSenderPrivate := ed25519.NewKeyFromSeed(externalSenderSeed)
		d.ExternalSenderPub = append(ed25519.PublicKey(nil), externalSenderPrivate.Public().(ed25519.PublicKey)...)
		d.Log = slog.New(slog.NewTextHandler(&diagnostics, nil))
		manager = botruntime.New(d.Repo, d.Clock.Now)
		d.BotRuntime = manager
	}, func(mux *server.Mux, d api.Deps) {
		var keys ds.InstanceKeys
		keys.InstanceID = d.Instance.InstanceID
		keys.ExternalSenderPriv[0] = 1
		var constructErr error
		delivery, constructErr = ds.New(ds.Options{Store: d.Repo, Wasm: deviceListRuntime, Clock: clock.System(), Keys: keys, Policy: ds.DefaultPolicy()})
		if constructErr != nil {
			t.Fatalf("construct delivery service: %v", constructErr)
		}
		t.Cleanup(func() { _ = delivery.Shutdown(context.Background()) })
		groups := &api.Groups{DS: delivery, MaxBody: 1 << 21, AfterKeyPackages: func(_ context.Context, user, device id.ID) {
			keyPackageAccepts.Add(1)
			acceptedKeyPackages <- [2]id.ID{user, device}
		}}
		groups.RegisterBotDirectory(mux, d.Sessions)
	})
	// Serve with the harness clock as the authoritative HTTP Date. The production client uses that
	// response observation to judge claim expiry; using net/http's host clock would compare it to a
	// separate clock from the API's installed fake clock.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", deps.Clock.Now().UTC().Format(http.TimeFormat))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	owner, ownerDevice, humanToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	negotiate, err := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := lifecycleHTTPDo(t, srv, http.MethodPost, "/v1/sessions/negotiate", negotiate, humanToken); code != http.StatusOK {
		t.Fatalf("human negotiate wire 3 = %d (%x)", code, body)
	}
	ownerSession, err := deps.Sessions.Resolve(context.Background(), humanToken)
	if err != nil {
		t.Fatal(err)
	}
	step, _ := deps.StepUps.Issue(owner.ID, ownerSession.TokenHash)
	configureBody, err := cborx.Marshal([]any{id.New(), nil, uint64(1 << 5), step})
	if err != nil {
		t.Fatal(err)
	}
	enabledCode, enabledBody := lifecycleHTTPDo(t, srv, http.MethodPut, "/v1/communities/"+cid.String()+"/bot", configureBody, humanToken)
	if enabledCode != http.StatusCreated {
		t.Fatalf("human step-up Bot enable = %d (%x)", enabledCode, enabledBody)
	}
	var enabledValue []any
	if err := cborx.Unmarshal(enabledBody, &enabledValue); err != nil {
		t.Fatal(err)
	}
	installation := mustBotID(t, enabledValue[0])
	instance := deps.Instance.InstanceID
	var runtimeToken string
	if _, err := manager.Create(context.Background(), func(_ id.ID, token string) error { runtimeToken = token; return nil }); err != nil {
		t.Fatalf("create operator runtime credential: %v", err)
	}
	// At this point there is one human and no Bot identity. In particular, no test fixture has
	// written Bot credentials, a device list, or KeyPackages to the repository.
	if counts := countRepoUsersAndDevices(t, repo); counts != [2]int{1, 1} {
		t.Fatalf("pre-lifecycle user/device counts=%v, want only the human owner", counts)
	}
	if packages, err := repo.CountKeyPackages(context.Background(), ownerDevice.ID, deps.Clock.Now().Unix()); err != nil || packages != 0 {
		t.Fatalf("pre-lifecycle owner KeyPackages=%d err=%v, want an empty directory", packages, err)
	}

	transport := &lifecycleDropAcceptedPublication{base: srv.Client().Transport}
	httpClient, err := client.New(client.Config{BaseURL: srv.URL, HTTPClient: &http.Client{Transport: transport}, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatalf("construct pinned bot HTTP client: %v", err)
	}
	botRuntime, err := httpClient.Runtime(runtimeToken)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var storageKey [32]byte
	if _, err := rand.Read(storageKey[:]); err != nil {
		t.Fatal(err)
	}
	binding := native.Binding{Instance: instance, Community: cid, Installation: installation}
	startNative := func() *native.Client {
		n, err := native.Start(context.Background(), helper.Options{Executable: helperPath, StateDir: stateDir}, binding, append([]byte(nil), storageKey[:]...))
		if err != nil {
			t.Fatalf("start native helper: %v", err)
		}
		t.Cleanup(func() { _ = n.Close() })
		return n
	}
	createLifecycle := func(n *native.Client) *service.Lifecycle {
		lifecycle, err := service.NewIdentityLifecycle(instance, cid, installation, n, botRuntime, httpClient,
			service.LifecycleOptions{Now: deps.Clock.Now, DesiredPackages: 8})
		if err != nil {
			t.Fatalf("construct production identity lifecycle: %v", err)
		}
		return lifecycle
	}

	nativeClient := startNative()
	lifecycle := createLifecycle(nativeClient)
	var result service.LifecycleResult
	for attempt := 0; attempt < 3; attempt++ {
		result, err = lifecycle.Ensure(context.Background())
		if err == nil {
			break
		}
		var apiErr *client.APIError
		if attempt == 2 || !transport.enrollmentLost.Load() || !errors.As(err, &apiErr) || apiErr.Outcome != client.OutcomeUnknown {
			t.Fatalf("production Ensure attempt %d: %v; HTTP routes: %s", attempt+1, err, transport.routeSummary())
		}
	}
	if err != nil {
		t.Fatalf("production Ensure did not recover the lost enrollment response: %v", err)
	}
	if !transport.enrollmentLost.Load() || transport.enrollmentCount() < 1 || !transport.enrollmentRequestsMatch() {
		t.Fatal("lost enrollment response was not recovered using the same exact enrollment request")
	}
	if result.Phase != uint64(store.BotInstallationJoining) || result.Installation.State != uint64(store.BotInstallationJoining) {
		t.Fatalf("lifecycle phase/state = %d/%d, want joining; group readiness belongs to D2", result.Phase, result.Installation.State)
	}
	if result.Session == nil {
		t.Fatal("Ensure returned no scoped device session")
	}
	negotiated, err := result.Session.Negotiate(context.Background(), []uint64{3}, []uint64{1}, []uint64{1})
	if err != nil || negotiated != (client.Negotiated{Wire: 3, E2EE: 1, Media: 1}) {
		t.Fatalf("returned session negotiated %+v, %v", negotiated, err)
	}
	scoped, err := deps.Sessions.Resolve(context.Background(), result.Session.BearerToken())
	if err != nil || scoped.Scope != auth.ScopeEnrolled || result.Installation.BotUser == nil || result.Installation.BotDevice == nil || scoped.UserID != *result.Installation.BotUser || scoped.DeviceID != *result.Installation.BotDevice {
		t.Fatalf("returned bearer binding scope=%d user=%s device=%s err=%v", scoped.Scope, scoped.UserID, scoped.DeviceID, err)
	}
	identity, err := nativeClient.Identity(context.Background())
	if err != nil || identity.Phase != 2 || identity.User == nil || identity.Device == nil || *identity.User != scoped.UserID || *identity.Device != scoped.DeviceID || identity.Instance == nil || *identity.Instance != instance {
		t.Fatalf("native final identity=%+v err=%v", identity, err)
	}
	deviceList, err := result.Session.OwnDeviceList(context.Background(), scoped.UserID)
	if err != nil || len(deviceList) == 0 || !identity.DeviceListPublished {
		t.Fatalf("native signed device list is not published: bytes=%d published=%t err=%v", len(deviceList), identity.DeviceListPublished, err)
	}
	if result.Packages.Generation != deps.Instance.Generation || result.Packages.OrdinaryActive != 8 || !result.Packages.HasCurrentFallback {
		t.Fatalf("actual directory inventory=%+v, want generation %d, eight active and current fallback", result.Packages, deps.Instance.Generation)
	}
	if !transport.lost.Load() || transport.acceptedCount() != 1 {
		t.Fatalf("publication response loss/recovery did not exercise one accepted request: lost=%t uploads=%d", transport.lost.Load(), transport.acceptedCount())
	}
	select {
	case accepted := <-acceptedKeyPackages:
		if accepted != [2]id.ID{scoped.UserID, scoped.DeviceID} {
			t.Fatalf("server accepted KeyPackages for %v, want the enrolled Bot", accepted)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted native KeyPackage publication did not trigger the server reconciliation hook")
	}
	if keyPackageAccepts.Load() != 1 {
		t.Fatalf("accepted native KeyPackage publication hook count=%d, want one", keyPackageAccepts.Load())
	}
	firstInstallation, err := botRuntime.ListInstallations(context.Background())
	if err != nil || len(firstInstallation) != 1 || firstInstallation[0].ID != installation || firstInstallation[0].BotUser == nil || firstInstallation[0].BotDevice == nil {
		t.Fatalf("runtime installation listing after ensure=%+v err=%v", firstInstallation, err)
	}
	firstCounts := countRepoUsersAndDevices(t, repo)
	if firstCounts != [2]int{2, 2} {
		t.Fatalf("user/device counts after identity enrollment=%v, want one owner plus one Bot", firstCounts)
	}
	botRow, err := repo.GetDevice(context.Background(), scoped.DeviceID)
	if err != nil || len(botRow.CredentialBlob) == 0 {
		t.Fatalf("persisted native Bot device credential unavailable: %v", err)
	}
	if err = removeDeviceList(context.Background(), scoped.UserID); err != nil {
		t.Fatalf("fixture restore rollback of server device-list row: %v", err)
	}
	if _, err = repo.GetDeviceList(context.Background(), scoped.UserID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fixture rollback left device-list row present: %v", err)
	}
	// The helper still retains the exact current signed head and its published bit.
	// Ensure must observe the server rollback, republish only the valid next v1
	// head, verify it through the authenticated read route, and return a session.
	repaired, err := createLifecycle(nativeClient).Ensure(context.Background())
	if err != nil || repaired.Session == nil || repaired.Phase != uint64(store.BotInstallationJoining) {
		t.Fatalf("Ensure after server-list rollback did not repair and return joining: session=%v phase=%d err=%v", repaired.Session != nil, repaired.Phase, err)
	}
	restoredList, err := result.Session.OwnDeviceList(context.Background(), scoped.UserID)
	if err != nil || !bytes.Equal(restoredList, deviceList) {
		t.Fatalf("server-list rollback repair changed native head: equal=%v err=%v", bytes.Equal(restoredList, deviceList), err)
	}

	// A fresh native process opens the same protected database with the same key. Reconcile must
	// retain the account/device/list and accepted package batch rather than mint another identity.
	if err := nativeClient.Close(); err != nil {
		t.Fatalf("close first helper before reopen: %v", err)
	}
	nativeClient = startNative()
	defer nativeClient.Close()
	reopenedIdentity, err := nativeClient.Identity(context.Background())
	if err != nil || reopenedIdentity.User == nil || reopenedIdentity.Device == nil || *reopenedIdentity.User != scoped.UserID || *reopenedIdentity.Device != scoped.DeviceID {
		t.Fatalf("reopened helper identity=%+v err=%v", reopenedIdentity, err)
	}
	second, err := createLifecycle(nativeClient).Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure after protected-store reopen: %v", err)
	}
	if second.Installation.ID != result.Installation.ID || second.Installation.Revision != result.Installation.Revision || second.Installation.State != uint64(store.BotInstallationJoining) || second.Packages.OrdinaryActive != result.Packages.OrdinaryActive {
		t.Fatalf("reopened Ensure changed installation or package inventory: first=%+v second=%+v", result.Installation, second.Installation)
	}
	afterIdentity, err := nativeClient.Identity(context.Background())
	if err != nil || afterIdentity.User == nil || afterIdentity.Device == nil || *afterIdentity.User != scoped.UserID || *afterIdentity.Device != scoped.DeviceID || !afterIdentity.DeviceListPublished {
		t.Fatalf("identity after repeated Ensure=%+v err=%v", afterIdentity, err)
	}
	if counts := countRepoUsersAndDevices(t, repo); counts != firstCounts {
		t.Fatalf("reopened Ensure duplicated Bot account/device: before=%v after=%v", firstCounts, counts)
	}
	if transport.acceptedCount() != 1 || keyPackageAccepts.Load() != 1 {
		t.Fatalf("reopened Ensure duplicated KeyPackage publication: requests=%d hook=%d", transport.acceptedCount(), keyPackageAccepts.Load())
	}
	secondLists, err := botRuntime.ListInstallations(context.Background())
	if err != nil || len(secondLists) != 1 || secondLists[0].ID != installation || secondLists[0].Revision != result.Installation.Revision {
		t.Fatalf("runtime installation changed after replay: %+v err=%v", secondLists, err)
	}
	if got, err := result.Session.OwnDeviceList(context.Background(), scoped.UserID); err != nil || !bytes.Equal(got, deviceList) {
		t.Fatalf("repeated Ensure changed published signed device list: equal=%t err=%v", bytes.Equal(got, deviceList), err)
	}
	assertLifecycleDiagnosticsRedacted(t, diagnostics.Bytes(), []byte(humanToken), []byte(runtimeToken), []byte(result.Session.BearerToken()), botRow.CredentialBlob, deviceList, transport.uploadBytes())
}

func lifecycleHTTPDo(t *testing.T, srv *httptest.Server, method, path string, body []byte, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("HTTP %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read HTTP %s %s response: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

func assertLifecycleDiagnosticsRedacted(t *testing.T, logs []byte, secrets ...[]byte) {
	t.Helper()
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		if bytes.Contains(logs, secret) {
			t.Fatal("server diagnostics contained a bearer, credential, signed device list, or KeyPackage upload value")
		}
	}
}

type lifecycleDropAcceptedPublication struct {
	base             http.RoundTripper
	lost             atomic.Bool
	enrollmentLost   atomic.Bool
	mu               sync.Mutex
	uploads          [][]byte
	enrollmentBodies [][]byte
	routes           []string
}

func (rt *lifecycleDropAcceptedPublication) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	isPublication := req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/keypackages")
	isEnrollment := req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/enroll")
	if isPublication || isEnrollment {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := rt.base.RoundTrip(req)
	rt.mu.Lock()
	if err != nil {
		rt.routes = append(rt.routes, req.Method+" "+req.URL.Path+" transport-error")
	} else {
		rt.routes = append(rt.routes, req.Method+" "+req.URL.Path+" "+strconv.Itoa(resp.StatusCode))
	}
	rt.mu.Unlock()
	if err != nil || len(body) == 0 {
		return resp, err
	}
	rt.mu.Lock()
	if isPublication {
		rt.uploads = append(rt.uploads, append([]byte(nil), body...))
	} else if isEnrollment {
		rt.enrollmentBodies = append(rt.enrollmentBodies, append([]byte(nil), body...))
	}
	rt.mu.Unlock()
	if isEnrollment && rt.enrollmentLost.CompareAndSwap(false, true) || isPublication && rt.lost.CompareAndSwap(false, true) {
		_ = resp.Body.Close()
		return nil, errors.New("simulated response loss after server accepted lifecycle mutation")
	}
	return resp, nil
}

func (rt *lifecycleDropAcceptedPublication) routeSummary() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return strings.Join(rt.routes, "; ")
}

func (rt *lifecycleDropAcceptedPublication) acceptedCount() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.uploads)
}

func (rt *lifecycleDropAcceptedPublication) enrollmentCount() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.enrollmentBodies)
}

func (rt *lifecycleDropAcceptedPublication) enrollmentRequestsMatch() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.enrollmentBodies) == 0 {
		return false
	}
	for _, body := range rt.enrollmentBodies[1:] {
		if !bytes.Equal(body, rt.enrollmentBodies[0]) {
			return false
		}
	}
	return true
}

func (rt *lifecycleDropAcceptedPublication) uploadBytes() []byte {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.uploads) == 0 {
		return nil
	}
	return append([]byte(nil), rt.uploads[0]...)
}
