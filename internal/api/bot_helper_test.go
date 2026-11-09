//go:build linux

package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	botruntime "github.com/dilla-chat/dilla/internal/bot/runtime"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// This is an explicit integration test of the production helper process. It builds the native
// binary when needed and sends its private-identity operations over the same inherited fd 3
// transport used by the external runtime. No private credential or signing key leaves that
// process; only its protocol responses are submitted to the HTTP API.
func TestManagedBotReachesReadyWithNativeHelperCredentialListAndKeyPackages(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			testManagedBotReachesReady(t, repo)
		})
	}
}

func testManagedBotReachesReady(t *testing.T, repo store.Repository) {
	t.Helper()
	root := repositoryRoot(t)
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "cargo", "build", "-p", "dilla-bot-core", "--bin", "dilla-bot-core")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native bot helper: %v\n%s", err, output)
	}
	helper := startNativeBotHelper(t, filepath.Join(root, "target", "debug", "dilla-bot-core"))
	defer helper.close()

	var manager *botruntime.Manager
	wasm, err := os.ReadFile(filepath.Join(root, "internal", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("read WASI core for signed device-list verification: %v", err)
	}
	deviceListRuntime, err := mlswasi.New(context.Background(), wasm, mlswasi.Options{PoolSize: 1, CacheDir: apiWasmCacheDir(t)})
	if err != nil {
		t.Fatalf("start WASI device-list verifier: %v", err)
	}
	t.Cleanup(func() { _ = deviceListRuntime.Close(context.Background()) })
	var diagnostics bytes.Buffer
	var afterKeyPackages = make(chan [2]id.ID, 1)
	var pendingMembership []id.ID
	var delivery *ds.DS
	h, deps := newTestAPIFullWithRepoAndMount(t, repo, nil, func(d *api.Deps) {
		d.StepUps = api.NewStepUps(d.Clock, time.Minute)
		d.DeviceLists = ds.NewDeviceLists(d.Repo, deviceListRuntime)
		d.Log = slog.New(slog.NewTextHandler(&diagnostics, nil))
		manager = botruntime.New(d.Repo, d.Clock.Now)
		d.BotRuntime = manager
		d.BotMembershipPending = func(_ context.Context, _ store.BotInstallationRow, channels []id.ID) {
			pendingMembership = append(pendingMembership, channels...)
		}
	}, func(mux *server.Mux, d api.Deps) {
		var keys ds.InstanceKeys
		keys.InstanceID = d.Instance.InstanceID
		keys.ExternalSenderPriv[0] = 1
		var err error
		delivery, err = ds.New(ds.Options{Store: d.Repo, Wasm: deviceListRuntime, Clock: clock.System(), Keys: keys, Policy: ds.DefaultPolicy()})
		if err != nil {
			t.Fatalf("construct delivery service for managed routes: %v", err)
		}
		t.Cleanup(func() { _ = delivery.Shutdown(context.Background()) })
		groups := &api.Groups{
			DS: delivery, MaxBody: 1 << 21,
			AfterKeyPackages: func(_ context.Context, userID, deviceID id.ID) { afterKeyPackages <- [2]id.ID{userID, deviceID} },
		}
		groups.RegisterBotDirectory(mux, d.Sessions)
	})
	// The routes that moved to the gateway socket are sent as request frames.
	h = viaSocket(t, h, deps)
	owner, ownerDevice, humanToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID) // no accessible text channels are required here
	negotiate, _ := cborx.Marshal([]any{[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, humanToken); rec.Code != http.StatusOK {
		t.Fatalf("human negotiate wire 3 = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	ownerSession, err := deps.Sessions.Resolve(context.Background(), humanToken)
	if err != nil {
		t.Fatal(err)
	}
	step, _ := deps.StepUps.Issue(owner.ID, ownerSession.TokenHash)
	const removalCapability = uint64(1 << 5)
	configureBody, _ := cborx.Marshal([]any{id.New(), nil, removalCapability, step})
	enabled := putCBORAuth(h, "/v1/communities/"+cid.String()+"/bot", configureBody, humanToken)
	if enabled.Code != http.StatusCreated {
		t.Fatalf("enable = %d (%x)", enabled.Code, enabled.Body.Bytes())
	}
	var enabledValue []any
	if err := cborx.Unmarshal(enabled.Body.Bytes(), &enabledValue); err != nil {
		t.Fatal(err)
	}
	installationID := mustBotID(t, enabledValue[0])
	instanceID := deps.Instance.InstanceID
	var runtimeID id.ID
	var runtimeToken string
	if _, err := manager.Create(context.Background(), func(got id.ID, token string) error {
		runtimeID, runtimeToken = got, token
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = runtimeID // creation is asserted here through authenticated runtime use, not a response field
	claimBody, _ := cborx.Marshal([]any{id.New()})
	claim := doBotCBOR(h, http.MethodPost, "/v1/bot-runtime/installations/"+installationID.String()+"/claim", claimBody, "Bearer "+runtimeToken)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim = %d (%x)", claim.Code, claim.Body.Bytes())
	}
	var claimValue []any
	if err := cborx.Unmarshal(claim.Body.Bytes(), &claimValue); err != nil {
		t.Fatal(err)
	}
	claimID := mustBotID(t, claimValue[1])
	var stateKey [32]byte
	if _, err := rand.Read(stateKey[:]); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stateKey[:], make([]byte, len(stateKey))) {
		t.Fatal("random helper storage key is zero")
	}
	if got := helper.call(t, botprotocol.HelperOpen, []any{instanceID, cid, installationID, stateKey[:]}); len(got) != 2 {
		t.Fatalf("open result has %d fields, want [version,identity]", len(got))
	}
	instanceKey := [32]byte{0x77}
	registration := helper.call(t, botprotocol.HelperBootstrap, []any{instanceID, cid, installationID, instanceKey[:]})
	if len(registration) != 4 {
		t.Fatalf("native registration has %d fields, want [umk,ssk,sig,device]", len(registration))
	}
	device, ok := registration[3].([]any)
	if !ok || len(device) != 5 {
		t.Fatalf("native registration device = %T %v", registration[3], registration[3])
	}
	enrollBody, _ := cborx.Marshal([]any{id.New(), claimID,
		registration[0], registration[1], registration[2], device})
	enrolled := doBotCBOR(h, http.MethodPost, "/v1/bot-runtime/installations/"+installationID.String()+"/enroll", enrollBody, "Bearer "+runtimeToken)
	if enrolled.Code != http.StatusCreated {
		t.Fatalf("enroll native helper identity = %d (%x)", enrolled.Code, enrolled.Body.Bytes())
	}
	var enrollment []any
	if err := cborx.Unmarshal(enrolled.Body.Bytes(), &enrollment); err != nil {
		t.Fatal(err)
	}
	userID := mustBotID(t, enrollment[0])
	deviceID := mustBotID(t, enrollment[1])
	communityRoles, err := deps.Repo.ListRoles(context.Background(), cid)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range communityRoles {
		if role.Position == 0 {
			role.Allow = uint64(api.PermAll)
			if err := deps.Repo.PutRole(context.Background(), role); err != nil {
				t.Fatal(err)
			}
		}
	}
	joiningSnapshot, err := api.LoadSnapshot(context.Background(), deps.Repo, cid, userID, nil)
	if err != nil {
		t.Fatalf("load joining bot permissions: %v", err)
	}
	if joining := joiningSnapshot.Resolve(userID); joining.Has(api.PermAdministrator | api.PermManageRoles | api.PermManageCommunity |
		api.PermManageMessages | api.PermKickMembers | api.PermBanMembers | api.PermModerateMembers) {
		t.Fatalf("joining bot inherited elevated @everyone permissions: %b", joining)
	}
	username, ok := enrollment[2].(string)
	if !ok || username == "" {
		t.Fatalf("enrollment username = %T %v", enrollment[2], enrollment[2])
	}
	completed := helper.call(t, botprotocol.HelperEnrollComplete, []any{userID, username, uint64(deps.Clock.Now().Unix())})
	if len(completed) != 2 {
		t.Fatalf("native completion has %d fields, want [credential,device_list]", len(completed))
	}
	credential, ok := completed[0].([]byte)
	if !ok || len(credential) == 0 {
		t.Fatalf("final credential = %T", completed[0])
	}

	nonce, _, err := deps.Sessions.Challenge(context.Background(), deviceID)
	if err != nil {
		t.Fatalf("challenge for bot: %v", err)
	}
	signed := helper.call(t, botprotocol.HelperSession, []any{uint64(1), nonce, uint64(auth.PurposeSession)})
	if len(signed) != 1 {
		t.Fatalf("helper session signature response has %d fields", len(signed))
	}
	sessionBody, ok := signed[0].([]byte)
	if !ok {
		t.Fatalf("helper session body is %T", signed[0])
	}
	var sessionProof []any
	if err := cborx.Unmarshal(sessionBody, &sessionProof); err != nil || len(sessionProof) != 5 {
		t.Fatalf("decode helper session proof: %v; fields=%d", err, len(sessionProof))
	}
	signature, ok := sessionProof[2].([]byte)
	if !ok || len(signature) != ed25519.SignatureSize {
		t.Fatalf("helper session proof signature is %T", sessionProof[2])
	}
	deviceRow, err := deps.Repo.GetDevice(context.Background(), deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(deviceRow.DSKPub, device[1].([]byte)) || instanceID != deps.Sessions.InstanceID() {
		t.Fatalf("helper binding differs from enrolled/session binding (dsk=%v instance=%v)", bytes.Equal(deviceRow.DSKPub, device[1].([]byte)), instanceID == deps.Sessions.InstanceID())
	}
	if !ed25519.Verify(ed25519.PublicKey(deviceRow.DSKPub), auth.SessionPreimage(deps.Sessions.InstanceID(), deviceID, nonce, auth.PurposeSession), signature) {
		t.Fatal("native helper signature does not match the server session preimage")
	}
	sess, err := deps.Sessions.Establish(context.Background(), auth.EstablishRequest{
		DeviceID: deviceID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: signature,
	})
	if err != nil {
		t.Fatalf("establish helper-signed bot session: %v", err)
	}
	botToken := sess.Token
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", negotiate, botToken); rec.Code != http.StatusOK {
		t.Fatalf("bot negotiate wire 3 = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	credentialBody, _ := cborx.Marshal([]any{id.New(), uint64(2), credential})
	credentialPath := "/v1/bot-runtime/installations/" + installationID.String() + "/credential"
	if rec := putCBORAuth(h, credentialPath, credentialBody, botToken); rec.Code != http.StatusOK {
		t.Fatalf("publish final helper credential = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	listBody, ok := completed[1].([]byte)
	if !ok || len(listBody) == 0 {
		t.Fatalf("signed list body = %T", completed[1])
	}
	listPath := "/v1/users/" + userID.String() + "/device-list"
	if rec := putCBORAuth(h, listPath, listBody, botToken); rec.Code != http.StatusNoContent {
		t.Fatalf("publish helper-signed own device list = %d (%x)", rec.Code, rec.Body.Bytes())
	}
	instance, err := deps.Repo.GetInstance(context.Background())
	if err != nil {
		t.Fatalf("read current generation for helper KeyPackage intent: %v", err)
	}
	operationID := id.New()
	intentRaw, err := botprotocol.EncodeKeyPackageIntent(botprotocol.KeyPackageIntent{
		OperationID: operationID, Count: 1, ExpectedGeneration: instance.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	var intentPayload []any
	if err := cborx.Unmarshal(intentRaw, &intentPayload); err != nil {
		t.Fatal(err)
	}
	packages := helper.call(t, botprotocol.HelperKeyPackages, intentPayload)
	encodedIntentResult, err := cborx.Marshal(packages)
	if err != nil {
		t.Fatal(err)
	}
	intentResult, err := botprotocol.DecodeKeyPackageIntentResult(encodedIntentResult)
	if err != nil {
		t.Fatalf("decode native KeyPackage intent result: %v", err)
	}
	inst, err := deviceListRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire WASI KeyPackage verifier: %v", err)
	}
	var packageInfo mlswasi.KeyPackageInfo
	var verifiedPackageCount int
	for _, keyPackage := range func() [][]byte {
		var upload struct {
			_          struct{} `cbor:",toarray"`
			Packages   [][]byte
			LastResort []byte
		}
		if err := cborx.Unmarshal(intentResult.UploadBody, &upload); err != nil {
			inst.Release()
			t.Fatalf("decode actual helper package upload: %v", err)
		}
		if len(upload.Packages) != 1 || upload.LastResort != nil {
			inst.Release()
			t.Fatalf("native helper upload has packages=%d last_resort=%t", len(upload.Packages), upload.LastResort != nil)
		}
		return upload.Packages
	}() {
		packageInfo, err = inst.ValidateKeyPackage(context.Background(), keyPackage)
		if err != nil {
			break
		}
		verifiedPackageCount++
	}
	inst.Release()
	if err != nil || verifiedPackageCount != 1 || !bytes.Equal(packageInfo.DeviceID, deviceID[:]) || !bytes.Equal(packageInfo.UserID, userID[:]) || !bytes.Equal(packageInfo.Credential, credential) {
		t.Fatalf("native helper KeyPackage binding: info=%+v err=%v", packageInfo, err)
	}
	if !bytes.Equal(packageInfo.SignatureKey, deviceRow.DSKPub) {
		t.Fatalf("native helper KeyPackage signature key does not match registered DSK (signature=%x registered=%x)", packageInfo.SignatureKey, deviceRow.DSKPub)
	}
	if int64(packageInfo.NotAfter)-time.Now().Unix() > int64(ds.DefaultPolicy().MaxKeyPackageLifetime.Seconds()) {
		t.Fatalf("native helper KeyPackage expiry is outside delivery lifetime: not_after=%d now=%d delta=%s", packageInfo.NotAfter, time.Now().Unix(), time.Duration(int64(packageInfo.NotAfter)-time.Now().Unix())*time.Second)
	}
	publicationBody, err := botprotocol.EncodeKeyPackagePublication(botprotocol.KeyPackagePublication{
		OperationID: operationID, ExpectedGeneration: instance.Generation, UploadBody: intentResult.UploadBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	keyPackagePath := "/v1/bot-runtime/installations/" + installationID.String() + "/keypackages"
	if published := postCBORAuth(h, keyPackagePath, publicationBody, botToken); published.Code != http.StatusCreated {
		t.Fatalf("publish native helper KeyPackage intent: %d (%x); diagnostics=%s", published.Code, published.Body.Bytes(), diagnostics.String())
	}
	select {
	case accepted := <-afterKeyPackages:
		if accepted != [2]id.ID{userID, deviceID} {
			t.Fatalf("managed publication reconciliation hook received %v, want (%s,%s)", accepted, userID, deviceID)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted managed publication did not trigger membership reconciliation")
	}
	receiptPath := keyPackagePath + "/" + operationID.String()
	receiptResponse := doBotCBOR(h, http.MethodGet, receiptPath, nil, "Bearer "+botToken)
	if receiptResponse.Code != http.StatusOK {
		t.Fatalf("read native helper publication receipt: %d (%x)", receiptResponse.Code, receiptResponse.Body.Bytes())
	}
	receipt, err := botprotocol.DecodeKeyPackagePublicationReceipt(receiptResponse.Body.Bytes())
	if err != nil || receipt.ExpectedGeneration != instance.Generation || receipt.UploadSHA256 != sha256.Sum256(intentResult.UploadBody) || receipt.Count != 1 {
		t.Fatalf("native helper publication receipt=%+v err=%v", receipt, err)
	}
	resolutionRaw, err := botprotocol.EncodeKeyPackageResolution(botprotocol.KeyPackageResolution{
		OperationID: operationID, PackageRefs: intentResult.PackageRefs, Outcome: 0,
		Now: uint64(deps.Clock.Now().Unix()), CurrentGeneration: instance.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	var resolutionPayload []any
	if err := cborx.Unmarshal(resolutionRaw, &resolutionPayload); err != nil {
		t.Fatal(err)
	}
	if resolved := helper.call(t, botprotocol.HelperKeyPackagesUploaded, resolutionPayload); len(resolved) != 0 {
		t.Fatalf("native helper resolution returned %d fields", len(resolved))
	}
	readyRequestID := id.New()
	readyBody, _ := cborx.Marshal([]any{readyRequestID, uint64(5)})
	readyPath := "/v1/bot-runtime/installations/" + installationID.String() + "/ready"
	staleApprover := store.UserRow{
		ID: id.New(), Username: "stale" + id.New().String()[:8], Display: "Former approver",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: deps.Clock.Now().Unix(),
	}
	if err := deps.Repo.CreateUser(context.Background(), staleApprover); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(context.Background(), store.MemberOfCommunityRow{CommunityID: cid, UserID: staleApprover.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	installBeforeReady, err := deps.Repo.GetBotInstallation(context.Background(), installationID)
	if err != nil || installBeforeReady.Revision != 3 {
		t.Fatalf("installation before stale-approver check = %+v, %v", installBeforeReady, err)
	}
	installBeforeReady.ApprovedBy = staleApprover.ID
	installBeforeReady.Revision++
	if err := deps.Repo.UpdateBotInstallation(context.Background(), installationID, 3, installBeforeReady); err != nil {
		t.Fatalf("fixture stale approving human: %v", err)
	}
	staleReadyBody, _ := cborx.Marshal([]any{readyRequestID, uint64(4)})
	if stale := postCBORAuth(h, readyPath, staleReadyBody, botToken); stale.Code != http.StatusForbidden {
		t.Fatalf("ready using human without current manage-community/capability authority = %d (%x), want 403", stale.Code, stale.Body.Bytes())
	}
	installBeforeReady.ApprovedBy = owner.ID
	priorRevision := installBeforeReady.Revision
	installBeforeReady.Revision++
	if err := deps.Repo.UpdateBotInstallation(context.Background(), installationID, priorRevision, installBeforeReady); err != nil {
		t.Fatalf("restore ready approver: %v", err)
	}
	// A valid identity with a missing text-group leaf must ask the composition
	// root to repair that exact channel after the Ready transaction rolls back.
	channelID, groupID := id.New(), id.New()
	communityID := cid
	if err := deps.Repo.CreateChannel(context.Background(), store.ChannelRow{ID: channelID, CommunityID: &communityID, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "pending-bot", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateGroup(context.Background(), store.GroupRow{GroupID: groupID, Binding: []byte{0x80}, Kind: api.GroupText, CommunityID: &communityID, TargetID: channelID, Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutChannelMember(context.Background(), channelID, userID, deps.Clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// Bot v2 Task 22: only a Bot channel's group is owed before ready (the Bot is proposed into no other).
	if _, err := deps.Repo.MarkBotChannel(context.Background(), store.BotChannelRow{ChannelID: channelID, CommunityID: communityID, MarkedAt: deps.Clock.Now().Unix(), MarkedBy: userID}); err != nil {
		t.Fatal(err)
	}
	if pending := postCBORAuth(h, readyPath, readyBody, botToken); pending.Code != http.StatusConflict {
		t.Fatalf("ready before required MLS join = %d (%x), want 409", pending.Code, pending.Body.Bytes())
	}
	if len(pendingMembership) != 1 || pendingMembership[0] != channelID {
		t.Fatalf("pending membership repair channels = %v, want %s", pendingMembership, channelID)
	}
	if err := deps.Repo.ReplaceMembers(context.Background(), groupID, 0, []store.MemberRow{
		{GroupID: groupID, UserID: owner.ID, DeviceID: ownerDevice.ID, LeafIndex: 0, SignatureKey: ownerDevice.DSKPub},
		{GroupID: groupID, UserID: userID, DeviceID: deviceID, LeafIndex: 1, SignatureKey: deviceRow.DSKPub},
	}); err != nil {
		t.Fatal(err)
	}
	ready := postCBORAuth(h, readyPath, readyBody, botToken)
	if ready.Code != http.StatusOK {
		t.Fatalf("ready after native credential/list/key package = %d (%x)", ready.Code, ready.Body.Bytes())
	}
	var readyValue []any
	if err := cborx.Unmarshal(ready.Body.Bytes(), &readyValue); err != nil {
		t.Fatal(err)
	}
	if got := readyValue[2].(uint64); got != uint64(store.BotInstallationReady) {
		t.Fatalf("ready state = %d, want %d", got, store.BotInstallationReady)
	}
	readySnapshot, err := api.LoadSnapshot(context.Background(), deps.Repo, cid, userID, nil)
	if err != nil {
		t.Fatalf("load ready bot permissions: %v", err)
	}
	readyPermissions := readySnapshot.Resolve(userID)
	if !readyPermissions.Has(api.PermManageMessages) {
		t.Fatal("Ready bot did not receive its explicitly approved removal capability")
	}
	for _, bit := range []api.Bits{api.PermAdministrator, api.PermManageRoles, api.PermManageCommunity,
		api.PermKickMembers, api.PermBanMembers, api.PermModerateMembers} {
		if readyPermissions.Has(bit) {
			t.Errorf("Ready removal-only bot inherited unapproved permission %b", bit)
		}
	}
	install, err := deps.Repo.GetActiveBotInstallationByCommunity(context.Background(), cid)
	if err != nil || install.Revision != 6 || install.State != store.BotInstallationReady {
		t.Fatalf("persisted installation = %+v, %v; want ready revision 6", install, err)
	}
	if install.RoleID == nil {
		t.Fatal("ready installation missing managed role")
	}
	managedRoleID := *install.RoleID
	if _, err := deps.Repo.GetRole(context.Background(), managedRoleID); err != nil {
		t.Fatalf("managed role before removal: %v", err)
	}
	if retry := putCBORAuth(h, credentialPath, credentialBody, botToken); retry.Code != http.StatusOK {
		t.Fatalf("matching final credential retry after Ready = %d (%x)", retry.Code, retry.Body.Bytes())
	}
	if retry := postCBORAuth(h, readyPath, readyBody, botToken); retry.Code != http.StatusOK {
		t.Fatalf("matching ready retry after phase advance = %d (%x)", retry.Code, retry.Body.Bytes())
	}
	afterRetry, err := deps.Repo.GetActiveBotInstallationByCommunity(context.Background(), cid)
	if err != nil || afterRetry.Revision != install.Revision || afterRetry.State != install.State {
		t.Fatalf("ready retry changed phase/revision: %+v, %v", afterRetry, err)
	}
	changedReadyBody, _ := cborx.Marshal([]any{readyRequestID, uint64(6)})
	if changed := postCBORAuth(h, readyPath, changedReadyBody, botToken); changed.Code != http.StatusConflict {
		t.Fatalf("same ready request id with changed revision = %d (%x), want 409", changed.Code, changed.Body.Bytes())
	}
	disableBody, _ := cborx.Marshal([]any{id.New(), uint64(6)})
	disablePath := "/v1/communities/" + cid.String() + "/bot/disable"
	if disabled := postCBORAuth(h, disablePath, disableBody, humanToken); disabled.Code != http.StatusOK {
		t.Fatalf("human disable = %d (%x)", disabled.Code, disabled.Body.Bytes())
	}
	if retry := postCBORAuth(h, readyPath, readyBody, botToken); retry.Code != http.StatusUnauthorized {
		t.Fatalf("disabled bot session ready replay = %d (%x), want 401", retry.Code, retry.Body.Bytes())
	}
	if retry := putCBORAuth(h, credentialPath, credentialBody, botToken); retry.Code != http.StatusUnauthorized {
		t.Fatalf("disabled bot session credential replay = %d (%x), want 401", retry.Code, retry.Body.Bytes())
	}
	removeBody, _ := cborx.Marshal([]any{id.New(), uint64(7)})
	removePath := "/v1/communities/" + cid.String() + "/bot"
	if removed := doBotCBOR(h, http.MethodDelete, removePath, removeBody, "Bearer "+humanToken); removed.Code != http.StatusNoContent {
		t.Fatalf("human remove = %d (%x); diagnostics=%s", removed.Code, removed.Body.Bytes(), diagnostics.String())
	}
	if retry := postCBORAuth(h, readyPath, readyBody, botToken); retry.Code != http.StatusUnauthorized {
		t.Fatalf("removed bot session ready replay = %d (%x), want 401", retry.Code, retry.Body.Bytes())
	}
	removedRow, err := deps.Repo.GetBotInstallation(context.Background(), installationID)
	if err != nil || removedRow.RemovedAt == nil || removedRow.State != store.BotInstallationDisabled {
		t.Fatalf("removed installation = %+v, %v", removedRow, err)
	}
	if _, err := deps.Repo.GetRole(context.Background(), managedRoleID); err == nil {
		t.Fatal("managed role still exists after uninstall")
	}
	if err := helper.stop(t); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve bot helper integration test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

type nativeBotHelper struct {
	cmd    *exec.Cmd
	stream *net.UnixConn
	cancel context.CancelFunc
	closed bool
}

func startNativeBotHelper(t *testing.T, binary string) *nativeBotHelper {
	t.Helper()
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientFile := os.NewFile(uintptr(fds[0]), "bot-helper-client")
	child := os.NewFile(uintptr(fds[1]), "bot-helper-child")
	clientConn, err := net.FileConn(clientFile)
	_ = clientFile.Close()
	if err != nil {
		_ = child.Close()
		t.Fatalf("open helper client socket: %v", err)
	}
	client, ok := clientConn.(*net.UnixConn)
	if !ok {
		_ = clientConn.Close()
		_ = child.Close()
		t.Fatalf("helper client socket is %T, want *net.UnixConn", clientConn)
	}
	processCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(processCtx, binary, "--state-dir", state)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	cmd.ExtraFiles = []*os.File{child}
	if err := cmd.Start(); err != nil {
		_ = client.Close()
		cancel()
		_ = child.Close()
		t.Fatal(err)
	}
	_ = child.Close()
	return &nativeBotHelper{cmd: cmd, stream: client, cancel: cancel}
}

func (h *nativeBotHelper) call(t *testing.T, operation botprotocol.HelperOperation, payload []any) []any {
	t.Helper()
	frame, err := botprotocol.EncodeHelperRequest(botprotocol.HelperRequest{RequestID: id.New(), Operation: operation, Payload: payload})
	if err != nil {
		t.Fatalf("encode helper operation %d: %v", operation, err)
	}
	if err := h.stream.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set helper operation %d deadline: %v", operation, err)
	}
	if _, err := h.stream.Write(frame); err != nil {
		t.Fatalf("write helper operation %d: %v", operation, err)
	}
	var prefix [4]byte
	if _, err := io.ReadFull(h.stream, prefix[:]); err != nil {
		t.Fatalf("read helper operation %d prefix: %v", operation, err)
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 || length > botprotocol.MaxHelperBytes {
		t.Fatalf("helper operation %d response length %d is outside bounds", operation, length)
	}
	responseFrame := make([]byte, int(length)+4)
	copy(responseFrame[:4], prefix[:])
	if _, err := io.ReadFull(h.stream, responseFrame[4:]); err != nil {
		t.Fatalf("read helper operation %d response: %v", operation, err)
	}
	response, err := botprotocol.DecodeHelperResponseFrame(responseFrame)
	if err != nil || response.Status != 0 {
		t.Fatalf("helper operation %d status=%d err=%v", operation, response.Status, err)
	}
	return response.Payload
}

func (h *nativeBotHelper) close() {
	if !h.closed {
		h.cancel()
		_ = h.stream.Close()
		_ = h.cmd.Wait()
		h.closed = true
	}
}

func (h *nativeBotHelper) stop(t *testing.T) error {
	t.Helper()
	if h.closed {
		return nil
	}
	if got := h.call(t, botprotocol.HelperShutdown, []any{}); len(got) != 0 {
		t.Fatalf("helper shutdown returned %d fields", len(got))
	}
	_ = h.stream.Close()
	h.closed = true
	err := h.cmd.Wait()
	h.cancel()
	return err
}
