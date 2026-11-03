//go:build linux

package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// This test joins and receives real MLS traffic from the Rust peer fixture. The HTTP source is a
// focused session double; the compiled helper and signed sender evidence remain real. The mounted
// delivery-service route path is covered separately by API acceptance tests.
func TestSourceSyncWithCompiledHelperAndRealRustPeer(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" || os.Getenv("DILLA_BOT_HELPER_INTEGRATION") != "1" {
		t.Skip("set DILLA_BOT_HELPER_BIN and DILLA_BOT_HELPER_INTEGRATION=1 to run source sync integration")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.Abs(binary)
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
	defer stopPeerFixture(cmd)
	waitPeerFile(t, filepath.Join(fixtureDir, "ready.cbor"))
	readyRaw, err := os.ReadFile(filepath.Join(fixtureDir, "ready.cbor"))
	if err != nil {
		t.Fatal(err)
	}
	var ready []any
	if err = cborx.Unmarshal(readyRaw, &ready); err != nil || len(ready) != 5 {
		t.Fatalf("peer fixture ready data: %v", err)
	}
	groupInfo, tree := ready[0].([]byte), ready[1].([]byte)
	fixtureHistory, innerHead := ready[2].([]byte), ready[3].([]byte)
	leaf, ok := ready[4].(uint64)
	if !ok {
		t.Fatal("peer fixture leaf is malformed")
	}
	var listRows [][]any
	if err = cborx.Unmarshal(fixtureHistory, &listRows); err != nil || len(listRows) != 1 {
		t.Fatalf("peer device list history: %v", err)
	}
	if !reflect.DeepEqual(innerHead, listRows[0][1]) {
		t.Fatal("peer fixture head does not equal the inner signed-list bytes")
	}
	outerHead, err := cborx.Marshal(listRows[0])
	if err != nil {
		t.Fatal(err)
	}

	fixedID := func(n byte) (v id.ID) {
		for i := range v {
			v[i] = n
		}
		return
	}
	instance, community, installation := fixedID(0x11), fixedID(0x22), fixedID(0x55)
	binding := native.Binding{Instance: instance, Community: community, Installation: installation}
	stateDir := filepath.Join(t.TempDir(), "helper-state")
	databaseKey := make([]byte, 32)
	copy(databaseKey, "source-sync-real-peer-key")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	openNative := func() *native.Client {
		c, startErr := native.Start(ctx, helper.Options{Executable: binary, StateDir: stateDir}, binding, append([]byte(nil), databaseKey...))
		if startErr != nil {
			t.Fatalf("start compiled helper: %v", startErr)
		}
		return c
	}
	n := openNative()
	if _, err = n.Bootstrap(ctx, [32]byte{0x77}); err != nil {
		t.Fatalf("bootstrap helper: %v", err)
	}
	if _, err = n.CompleteEnrollment(ctx, fixedID(0x56), "source sync peer", uint64(time.Now().Unix())); err != nil {
		t.Fatalf("complete helper identity: %v", err)
	}
	group, channel, operation := fixedID(0x44), fixedID(0x33), fixedID(0x57)
	joined, err := n.ExternalJoin(ctx, operation, group, channel, 0, 1, groupInfo, tree)
	if err != nil {
		t.Fatalf("join real Rust peer: %v", err)
	}
	joinAnswer := exchangeSourcePeer(t, fixtureDir, mustSourceCBOR(t, []any{uint64(1), mustSourceBytesCBOR(t, []any{joined.Commit, joined.GroupInfo})}))
	var joinedResult []any
	if err = cborx.Unmarshal(joinAnswer, &joinedResult); err != nil || len(joinedResult) != 1 {
		t.Fatalf("peer join response: %v", err)
	}
	if err = n.GroupJoined(ctx, operation, group, joinedResult[0].(uint64)); err != nil {
		t.Fatalf("persist peer join: %v", err)
	}
	messageAnswer := exchangeSourcePeer(t, fixtureDir, mustSourceCBOR(t, []any{uint64(2), mustSourceBytesCBOR(t, nil)}))
	var delivery []any
	if err = cborx.Unmarshal(messageAnswer, &delivery); err != nil || len(delivery) != 4 {
		t.Fatalf("peer delivery response: %v", err)
	}
	var handshakes, messages [][]any
	if err = cborx.Unmarshal(delivery[0].([]byte), &handshakes); err != nil {
		t.Fatalf("peer handshake rows: %v", err)
	}
	if err = cborx.Unmarshal(delivery[1].([]byte), &messages); err != nil {
		t.Fatalf("peer message rows: %v", err)
	}
	through, ok := delivery[2].(uint64)
	if !ok || through == 0 {
		t.Fatal("peer delivery head is malformed")
	}
	local, err := n.InspectGroup(ctx, group)
	if err != nil {
		t.Fatalf("inspect joined group: %v", err)
	}
	peerUser := fixedID(0xb2)
	currentOuter := outerHead
	session := &sourcePeerSession{group: group, epoch: local.Epoch, next: through + 1, handshakes: handshakes, messages: messages, outerHead: currentOuter, historyRows: listRows}
	// The membership coordinator owns the external-join cursor. Model its completed acceptance so
	// this pass's lost response is specifically after the message apply.
	joinCursor, err := n.GroupCursor(ctx, group)
	if err != nil {
		t.Fatalf("read external-join cursor: %v", err)
	}
	if joinCursor != nil {
		if err = session.AdvanceCursor(ctx, group, joinCursor.Sequence, joinCursor.Epoch); err != nil {
			t.Fatalf("accept external-join cursor: %v", err)
		}
		if err = n.AcknowledgeGroupCursor(ctx, group, *joinCursor); err != nil {
			t.Fatalf("ack external-join cursor: %v", err)
		}
	}
	session.cursorCalls, session.cursorAccepted = 0, false
	session.loseFirstCursorReply = true
	options := SourceSyncOptions{PageLimit: 256, PageBudget: 2}
	syncer, err := NewSourceSync(n, options)
	if err != nil {
		t.Fatal(err)
	}
	expected := sourceExpected(group, community, channel)
	if _, err = syncer.sync(ctx, session, expected); err == nil {
		t.Fatal("lost server cursor response did not stop the pass")
	}
	if !session.cursorAccepted || session.cursorCalls != 1 {
		t.Fatalf("cursor uncertainty fixture: calls=%d accepted=%v", session.cursorCalls, session.cursorAccepted)
	}
	acceptedSequence, acceptedEpoch := session.acceptedSequence, session.acceptedEpoch
	if err = n.Close(); err != nil {
		t.Fatalf("close helper after durable apply: %v", err)
	}
	n = openNative()
	defer n.Close()
	if !session.cursorAccepted || session.acceptedSequence != acceptedSequence || session.acceptedEpoch != acceptedEpoch {
		t.Fatalf("source server forgot accepted cursor after helper reopen: accepted=%v cursor=(%d,%d) want=(%d,%d)", session.cursorAccepted, session.acceptedSequence, session.acceptedEpoch, acceptedSequence, acceptedEpoch)
	}
	syncer, err = NewSourceSync(n, options)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := syncer.sync(ctx, session, expected)
	if err != nil || resumed.More {
		t.Fatalf("resume after helper reopen: %#v %v", resumed, err)
	}
	if session.cursorCalls != 2 || !session.cursorAccepted || session.acceptedSequence != acceptedSequence || session.acceptedEpoch != acceptedEpoch {
		t.Fatalf("server cursor was not retried idempotently: calls=%d accepted=%v cursor=(%d,%d) want=(%d,%d)", session.cursorCalls, session.cursorAccepted, session.acceptedSequence, session.acceptedEpoch, acceptedSequence, acceptedEpoch)
	}
	trust, err := NewSourceTrust(n)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := trust.trustSender(ctx, session, group, leaf, peerUser, 0)
	if err != nil || recovered < 2 {
		t.Fatalf("native trust/revalidation: recovered=%d err=%v", recovered, err)
	}
	page, err := n.ReadGroupEvents(ctx, 0, 10)
	if err != nil {
		t.Fatalf("read authenticated journal: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("peer authenticated messages were not journaled exactly once: got %d", len(page.Events))
	}
	if !reflect.DeepEqual([]string{valueOrEmpty(page.Events[0].Body), valueOrEmpty(page.Events[1].Body)}, []string{"native peer message one", "native peer message two"}) {
		t.Fatalf("unexpected authenticated peer messages: %#v", page)
	}
	if _, err = n.ReadGroupEvents(ctx, 0, 10); err != nil {
		t.Fatalf("repeat journal read: %v", err)
	}
}

type sourcePeerSession struct {
	group                id.ID
	epoch, next          uint64
	handshakes, messages [][]any
	outerHead            []byte
	historyRows          [][]any
	loseFirstCursorReply bool
	cursorCalls          int
	cursorAccepted       bool
	acceptedSequence     uint64
	acceptedEpoch        uint64
}

func (s *sourcePeerSession) GroupInfo(_ context.Context, group id.ID) (client.GroupInfo, error) {
	if group != s.group {
		return client.GroupInfo{}, errors.New("wrong group")
	}
	return client.GroupInfo{Epoch: s.epoch, NextSequence: s.next}, nil
}
func (s *sourcePeerSession) GroupPage(_ context.Context, group id.ID, kind string, from, limit uint64) (client.Page, error) {
	if group != s.group {
		return client.Page{}, errors.New("wrong group")
	}
	rows := s.handshakes
	if kind == "messages" {
		rows = s.messages
	}
	page := client.Page{Rows: make([][]any, 0, limit), Next: from}
	for _, row := range rows {
		seq := row[0].(uint64)
		if seq < from {
			continue
		}
		if uint64(len(page.Rows)) == limit {
			break
		}
		page.Rows = append(page.Rows, row)
		page.Next = seq + 1
	}
	return page, nil
}
func (s *sourcePeerSession) AdvanceCursor(_ context.Context, group id.ID, sequence, epoch uint64) error {
	if group != s.group || sequence >= s.next || epoch > mathMaxInt64 {
		return errors.New("invalid test cursor")
	}
	s.cursorCalls++
	if s.loseFirstCursorReply && s.cursorCalls == 1 {
		s.cursorAccepted = true
		s.acceptedSequence, s.acceptedEpoch = sequence, epoch
		return errors.New("server accepted cursor but reply was lost")
	}
	if s.cursorAccepted && (sequence != s.acceptedSequence || epoch != s.acceptedEpoch) {
		return errors.New("test cursor retry does not match the accepted cursor")
	}
	s.cursorAccepted = true
	s.acceptedSequence, s.acceptedEpoch = sequence, epoch
	return nil
}
func (s *sourcePeerSession) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	return append([]byte(nil), s.outerHead...), nil
}
func (s *sourcePeerSession) DeviceListHistory(_ context.Context, _ id.ID, after uint64) ([][]any, error) {
	rows := make([][]any, 0, len(s.historyRows))
	for _, row := range s.historyRows {
		if row[0].(uint64) > after {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

const mathMaxInt64 = uint64(1<<63 - 1)

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func mustSourceCBOR(t *testing.T, value any) []byte {
	t.Helper()
	b, err := cborx.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func mustSourceBytesCBOR(t *testing.T, value any) []byte {
	t.Helper()
	b, err := cborx.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exchangeSourcePeer(t *testing.T, dir string, request []byte) []byte {
	t.Helper()
	requestPath, responsePath := filepath.Join(dir, "request.cbor"), filepath.Join(dir, "response.cbor")
	if err := os.WriteFile(requestPath, request, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if response, err := os.ReadFile(responsePath); err == nil {
			if err = os.Remove(responsePath); err != nil {
				t.Fatal(err)
			}
			return response
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for real Rust peer fixture")
	return nil
}

func waitPeerFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for real Rust peer fixture startup")
}

func TestStopPeerFixtureReapsProcessGroup(t *testing.T) {
	const childMode = "DILLA_TEST_STUBBORN_PEER_CHILD"
	if os.Getenv(childMode) == "1" {
		pidFile := os.Getenv("DILLA_TEST_STUBBORN_PEER_PID_FILE")
		child := exec.CommandContext(t.Context(), "/bin/sh", "-c", `trap '' TERM; echo $$ > "$1"; while :; do sleep 1; done`, "stubborn-peer", pidFile)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Second)
		}
	}

	pidFile := filepath.Join(t.TempDir(), "stubborn-peer.pid")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStopPeerFixtureReapsProcessGroup$")
	cmd.Env = append(os.Environ(), childMode+"=1", "DILLA_TEST_STUBBORN_PEER_PID_FILE="+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cleanup subprocess: %v", err)
	}
	t.Cleanup(func() { stopPeerFixture(cmd) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pidRaw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("stubborn child did not start: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if err != nil {
		t.Fatalf("stubborn child pid is malformed: %q", pidRaw)
	}
	stopPeerFixture(cmd)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stubborn child %d survived process-group cleanup", childPID)
}

func stopPeerFixture(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	select {
	case <-waited:
		timer.Stop()
	case <-timer.C:
	}
	// Kill the whole group even when cargo exits promptly; descendants may still be alive.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-waited
}
