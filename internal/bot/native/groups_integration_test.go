//go:build linux

package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCompiledHelperNativeGroupsIntegration(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN and DILLA_BOT_HELPER_INTEGRATION=1 to run native integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{Instance: integrationID(81), Community: integrationID(82), Installation: integrationID(83)}
	key := make([]byte, 32)
	copy(key, "native-groups-integration-key")
	ctx := context.Background()
	c, err := Start(ctx, helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "groups")}, binding, append([]byte(nil), key...))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Bootstrap(ctx, [32]byte{0x77}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.CompleteEnrollment(ctx, integrationID(84), "groups integration", uint64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	identity, err := c.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if identity.User == nil || identity.Device == nil {
		t.Fatal("completed helper identity omitted Bot user/device")
	}
	groupID, channelID, operation := integrationID(85), integrationID(86), integrationID(87)
	externalSender := make([]byte, 32)
	externalSender[0] = 1
	created, err := c.CreateGroup(ctx, operation, groupID, channelID, 0, 1, externalSender)
	if err != nil || created.Group != groupID || created.Binding.Instance != binding.Instance || created.Binding.Community == nil || *created.Binding.Community != binding.Community || created.Binding.Target != channelID || created.Binding.Kind != 0 || len(created.GroupInfo) == 0 || len(created.RatchetTree) == 0 {
		t.Fatalf("native group create: %#v %v", created, err)
	}
	pending, err := c.GroupIntentResult(ctx, operation)
	if err != nil || pending == nil || pending.Kind != 2 || pending.Group != groupID || pending.State != 0 || len(pending.Result) == 0 {
		t.Fatalf("pending create intent: %#v %v", pending, err)
	}
	retried, err := c.CreateGroup(ctx, operation, groupID, channelID, 0, 1, externalSender)
	if err != nil || !reflect.DeepEqual(created, retried) {
		t.Fatalf("create intent retry changed bytes: %v", err)
	}
	if err = c.GroupRegistered(ctx, operation, groupID, 1); err != nil {
		t.Fatalf("register group: %v", err)
	}
	resolved, err := c.GroupIntentResult(ctx, operation)
	if err != nil || resolved == nil || resolved.State != 1 || resolved.Result != nil {
		t.Fatalf("resolved create intent: %#v %v", resolved, err)
	}

	// An empty proposal list exercises the real core self-update path. The pending result must
	// retain the five-field public CBOR value exactly across an operation retry.
	proposals := []byte{0x80}
	abortOperation := integrationID(89)
	built, err := c.BuildCommit(ctx, abortOperation, groupID, proposals)
	if err != nil || built.Epoch != 0 || !built.ResyncNull || len(built.Commit) == 0 || len(built.GroupInfo) == 0 || len(built.ResultBody) == 0 {
		t.Fatalf("build commit: %#v %v", built, err)
	}
	pending, err = c.GroupIntentResult(ctx, abortOperation)
	if err != nil || pending == nil || pending.Kind != 9 || pending.State != 0 || !reflect.DeepEqual(pending.Result, built.ResultBody) {
		t.Fatalf("pending commit intent bytes: %#v %v", pending, err)
	}
	retriedBuild, err := c.BuildCommit(ctx, abortOperation, groupID, proposals)
	if err != nil || !reflect.DeepEqual(built, retriedBuild) {
		t.Fatalf("build intent retry changed result: %v", err)
	}
	if err = c.AbortCommit(ctx, abortOperation, groupID); err != nil {
		t.Fatalf("abort staged commit: %v", err)
	}
	resolved, err = c.GroupIntentResult(ctx, abortOperation)
	if err != nil || resolved == nil || resolved.State != 2 || resolved.Result != nil {
		t.Fatalf("aborted commit intent: %#v %v", resolved, err)
	}

	confirmOperation := integrationID(90)
	confirmedBuild, err := c.BuildCommit(ctx, confirmOperation, groupID, proposals)
	if err != nil || !confirmedBuild.ResyncNull || len(confirmedBuild.ResultBody) == 0 {
		t.Fatalf("second build commit: %#v %v", confirmedBuild, err)
	}
	if err = c.ConfirmCommit(ctx, confirmOperation, groupID); err != nil {
		t.Fatalf("confirm staged commit: %v", err)
	}
	resolved, err = c.GroupIntentResult(ctx, confirmOperation)
	if err != nil || resolved == nil || resolved.State != 1 || resolved.Result != nil {
		t.Fatalf("confirmed commit intent: %#v %v", resolved, err)
	}
	groups, err := c.Groups(ctx)
	if err != nil || len(groups) != 1 || groups[0].ID != groupID || groups[0].Community == nil || *groups[0].Community != binding.Community || groups[0].State != 2 {
		t.Fatalf("group list: %#v %v", groups, err)
	}
	inspected, err := c.InspectGroup(ctx, groupID)
	if err != nil || !reflect.DeepEqual(groups[0], inspected) {
		t.Fatalf("group inspect: %#v %v", inspected, err)
	}
	cursor, err := c.GroupCursor(ctx, groupID)
	if err != nil || cursor != nil {
		t.Fatalf("new group cursor: %#v %v", cursor, err)
	}
	if _, err = encodeExpected([]GroupExpected{{Group: groupID, Community: integrationID(88), Channel: channelID}}, binding.Community); err == nil {
		t.Fatal("foreign community accepted in expected-group input")
	}
	page, err := c.ReadGroupEvents(ctx, 0, 10)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("event journal read: %#v %v", page, err)
	}
	if err = c.AcknowledgeGroupEvents(ctx, page.Next); err != nil {
		t.Fatalf("event journal ack: %v", err)
	}
}

func TestCompiledHelperWithRealRustPeerExternalJoinTrustAndJournal(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" || os.Getenv("DILLA_BOT_HELPER_INTEGRATION") != "1" {
		t.Skip("set DILLA_BOT_HELPER_BIN and DILLA_BOT_HELPER_INTEGRATION=1 to run native integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	fixtureDir := filepath.Join(t.TempDir(), "peer")
	cmd := exec.CommandContext(t.Context(), "cargo", "test", "-p", "dilla-core", "--test", "native_bot_group_peer_fixture", "--", "--exact", "bridge_public_cbor_peer", "--nocapture")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DILLA_NATIVE_GROUP_FIXTURE_DIR="+fixtureDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		t.Fatalf("start Rust peer fixture: %v", err)
	}
	defer killProcessGroupAndWait(cmd)
	waitForFile(t, filepath.Join(fixtureDir, "ready.cbor"))
	readyBytes, err := os.ReadFile(filepath.Join(fixtureDir, "ready.cbor"))
	if err != nil {
		t.Fatal(err)
	}
	var ready []any
	if err = cborx.Unmarshal(readyBytes, &ready); err != nil || len(ready) != 5 {
		t.Fatalf("peer fixture ready shape: %v", err)
	}
	groupInfo, tree, history, heads := ready[0].([]byte), ready[1].([]byte), ready[2].([]byte), ready[3].([]byte)
	leaf := ready[4].(uint64)

	fillID := func(n byte) (v id.ID) {
		for i := range v {
			v[i] = n
		}
		return
	}
	binding := Binding{Instance: fillID(0x11), Community: fillID(0x22), Installation: fillID(0x55)}
	key := make([]byte, 32)
	copy(key, "native-group-peer-integration")
	ctx := context.Background()
	c, err := Start(ctx, helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "state")}, binding, key)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Bootstrap(ctx, [32]byte{0x77}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.CompleteEnrollment(ctx, fillID(0x56), "peer integration", uint64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	group, channel, operation := fillID(0x44), fillID(0x33), fillID(0x57)
	joined, err := c.ExternalJoin(ctx, operation, group, channel, 0, 1, groupInfo, tree)
	if err != nil {
		t.Fatalf("external join: %v", err)
	}
	input, err := cborx.Marshal([]any{uint64(1), mustBytesCBOR(t, []any{joined.Commit, joined.GroupInfo})})
	if err != nil {
		t.Fatal(err)
	}
	response := exchangePeerFixture(t, fixtureDir, input)
	var merged []any
	if err = cborx.Unmarshal(response, &merged); err != nil || len(merged) != 1 {
		t.Fatalf("external join response: %v", err)
	}
	if err = c.GroupJoined(ctx, operation, group, merged[0].(uint64)); err != nil {
		t.Fatalf("group joined: %v", err)
	}

	response = exchangePeerFixture(t, fixtureDir, mustCBOR(t, []any{uint64(2), mustBytesCBOR(t, nil)}))
	var delivery []any
	if err = cborx.Unmarshal(response, &delivery); err != nil || len(delivery) != 4 {
		t.Fatalf("peer delivery shape: %v", err)
	}
	through := delivery[2].(uint64)
	if _, err = c.ApplyGroup(ctx, group, delivery[0].([]byte), delivery[1].([]byte), through); err != nil {
		t.Fatalf("apply stale peer messages: %v", err)
	}
	verdict, err := c.TrustSender(ctx, group, leaf, fillID(0xb2), history, heads, uint64(time.Now().Unix()))
	if err != nil {
		t.Fatalf("trust real peer: %#v %v", verdict, err)
	}
	recovered, err := c.RevalidateGroup(ctx, group, leaf, 1)
	if err != nil || recovered != 2 {
		t.Fatalf("revalidate real peer messages: count=%d err=%v", recovered, err)
	}
	page, err := c.ReadGroupEvents(ctx, 0, 10)
	if err != nil || len(page.Events) < 2 {
		t.Fatalf("nonempty real peer journal: events=%d err=%v", len(page.Events), err)
	}
	if err = c.AcknowledgeGroupEvents(ctx, page.Next); err != nil {
		t.Fatalf("ack real peer journal: %v", err)
	}
	remaining, err := c.ReadGroupEvents(ctx, page.Next, 10)
	if err != nil || len(remaining.Events) != 0 {
		t.Fatalf("journal after ack: %#v %v", remaining, err)
	}

	// Publish one real helper KeyPackage to a second Rust peer and apply its MLS Welcome.
	kpIntent := protocol.KeyPackageIntent{OperationID: fillID(0x58), Count: 1, ExpectedGeneration: 0}
	kp, err := c.KeyPackageIntent(ctx, kpIntent)
	if err != nil {
		t.Fatalf("key package intent: %v", err)
	}
	var upload []any
	if err = cborx.Unmarshal(kp.UploadBody, &upload); err != nil || len(upload) != 2 {
		t.Fatalf("key package upload shape: %v", err)
	}
	packages := upload[0].([]any)
	if len(packages) != 1 {
		t.Fatalf("key package count %d", len(packages))
	}
	welcomeBytes := exchangePeerFixture(t, fixtureDir, mustCBOR(t, []any{uint64(3), mustCBOR(t, packages[0])}))
	var welcomeRow []any
	if err = cborx.Unmarshal(welcomeBytes, &welcomeRow); err != nil || len(welcomeRow) != 7 {
		t.Fatalf("welcome fixture shape: %v", err)
	}
	var treeHash [32]byte
	if hash, ok := welcomeRow[6].([]byte); !ok || len(hash) != 32 {
		t.Fatalf("welcome tree hash shape")
	} else {
		copy(treeHash[:], hash)
	}
	welcome := GroupWelcome{ID: welcomeRow[0].(uint64), Group: fillID(0x45), Epoch: welcomeRow[2].(uint64), CommitSequence: welcomeRow[3].(uint64), Welcome: welcomeRow[4].([]byte), RatchetTree: welcomeRow[5].([]byte), TreeHash: treeHash}
	welcomeChannel := fillID(0x34)
	outcomes, err := c.ApplyWelcomes(ctx, []GroupWelcome{welcome}, []GroupExpected{{Group: welcome.Group, Community: binding.Community, Channel: welcomeChannel, Kind: 0, Policy: 1}})
	if err != nil || len(outcomes) != 1 || outcomes[0].ID != welcome.ID || outcomes[0].Group != welcome.Group || outcomes[0].Outcome != 0 {
		t.Fatalf("apply real Welcome: %#v %v", outcomes, err)
	}
	resolution := protocol.KeyPackageResolution{OperationID: kpIntent.OperationID, PackageRefs: kp.PackageRefs, Outcome: 0, Now: uint64(time.Now().Unix()), CurrentGeneration: 0}
	if err = c.ResolveKeyPackageIntent(ctx, resolution); err != nil {
		t.Fatalf("resolve accepted key package: %v", err)
	}
}

func killProcessGroupAndWait(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Cargo starts the Rust test as a child. Killing only Cargo can leave that
	// child polling the fixture directory until its own timeout, so signal the
	// process group created for this command before reaping Cargo.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
}

func TestKillProcessGroupAndWaitStopsChildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-finished")
	started := marker + ".started"
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `sh -c 'touch "$1"; sleep 0.3; touch "$2"' sh "$1" "$2" & wait`, "sh", started, marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			killProcessGroupAndWait(cmd)
		}
	}()

	waitForFile(t, started)
	killProcessGroupAndWait(cmd)
	cleaned = true
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child survived process-group cleanup; marker stat error: %v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for fixture file %s", path)
}

func exchangePeerFixture(t *testing.T, dir string, request []byte) []byte {
	t.Helper()
	requestPath := filepath.Join(dir, "request.cbor")
	responsePath := filepath.Join(dir, "response.cbor")
	if err := os.WriteFile(requestPath, request, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, responsePath)
	response, err := os.ReadFile(responsePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(responsePath); err != nil {
		t.Fatal(err)
	}
	return response
}

func mustCBOR(t *testing.T, value any) []byte {
	t.Helper()
	b, err := cborx.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustBytesCBOR(t *testing.T, value any) []byte { return mustCBOR(t, value) }
