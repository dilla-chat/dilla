//go:build linux

package helper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCallsAreSerializedAndCloseIsIdempotent(t *testing.T) {
	c := startFixture(t, "delay")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)}); err != nil {
				t.Errorf("Call: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCancellationOfActiveCallReapsChildAndReturnsUncertain(t *testing.T) {
	c := startFixture(t, "stall-after")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.Call(ctx, protocol.HelperStateTransaction, []any{fixtureID(1), []any{}})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("interrupted mutation error = %v, want ErrOutcomeUnknown", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled helper call did not terminate promptly")
	}
	transport := c.transport.(*linuxExchange)
	select {
	case <-transport.done:
	default:
		t.Fatal("cancelled helper child was not reaped")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close after abort: %v", err)
	}
}

func TestCleanupAfterChildAlreadyExitedUsesTrackedProcessHandle(t *testing.T) {
	c := startFixture(t, "")
	transport := c.transport.(*linuxExchange)
	if err := transport.cmd.Process.Kill(); err != nil {
		t.Fatalf("stop fixture child: %v", err)
	}
	select {
	case <-transport.done:
	case <-time.After(time.Second):
		t.Fatal("helper child did not exit after Open")
	}

	started := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close after child exit: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cleanup after child exit took %s", elapsed)
	}
}

func TestCallRejectsOperationOutsideFrozenRange(t *testing.T) {
	c := startFixture(t, "")
	for _, operation := range []protocol.HelperOperation{protocol.HelperShutdown, protocol.HelperGroupTransfer, 28} {
		if _, err := c.Call(context.Background(), operation, []any{}); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("operation %d error = %v, want ErrInvalidOperation", operation, err)
		}
	}
}

// fixtureCommandAnswer answers ops 21-24 in "command" mode. Op 22's `now` picks the answer:
// 1 a replay refusal, 2 an undefined refusal code, 3 an accepted request with trailing bytes,
// anything else the accepted request.
func fixtureCommandAnswer(request protocol.HelperRequest) ([]any, bool) {
	switch request.Operation {
	case protocol.HelperCommandKey:
		return []any{[]byte{0x87, 0x01}}, true
	case protocol.HelperCommandOpen:
		now, _ := request.Payload[3].(uint64)
		switch now {
		case 1:
			return []any{uint64(8)}, true
		case 2:
			return []any{uint64(9)}, true
		}
		replyKey := make([]byte, 32)
		replyKey[0] = 0xaa
		// Task 15: a refusal decided after the seal opened carries the reply key ([code, key]).
		switch now {
		case 4: // a verified replay: its reply key and its own request id (fix round 1 M-1)
			return []any{uint64(8), replyKey, idBytes(fixtureID(41))}, true
		case 8: // undefined: a replay without its request id
			return []any{uint64(8), replyKey}, true
		case 5:
			return []any{uint64(3), replyKey}, true
		case 6: // undefined: the seal of an unknown key never opened
			return []any{uint64(2), replyKey}, true
		case 7: // undefined: a short key
			return []any{uint64(8), replyKey[:31]}, true
		}
		body, _ := cborx.Marshal([]any{idBytes(fixtureID(40)), idBytes(fixtureID(2)), nil, uint64(7), []any{"hello", uint64(3), []byte{1}, nil}, uint64(1_800_000_000), replyKey})
		if now == 3 {
			body = append(body, 0)
		}
		return []any{uint64(0), body}, true
	case protocol.HelperReplySeal:
		return []any{[]byte{0x83, 0x01}}, true
	case protocol.HelperDeviceListIngest:
		// The body's first byte picks the state: 0x84 current, 1 held, 2 behind, 3 undefined.
		body, _ := request.Payload[1].([]byte)
		if now, ok := request.Payload[2].(uint64); !ok || now == 0 || len(body) == 0 {
			return nil, false
		}
		switch body[0] {
		case 1:
			return []any{uint64(1), uint64(7), uint64(1)}, true
		case 2:
			return []any{uint64(0), uint64(7), uint64(2)}, true
		case 3:
			return []any{uint64(0), uint64(7), uint64(3)}, true
		}
		return []any{uint64(1), uint64(7), uint64(0)}, true
	case protocol.HelperProbeSeal:
		// [community, request, now]: now 1 answers a zero handle (undefined).
		if now, _ := request.Payload[2].(uint64); now == 1 {
			return []any{[]byte{0x85}, uint64(0)}, true
		}
		return []any{[]byte{0x85, 0x01}, uint64(3)}, true
	case protocol.HelperProbeOpen:
		// [handle, seq, sealed, forget]: the plaintext is the sealed bytes after their first.
		sealed, _ := request.Payload[2].([]byte)
		if forget, _ := request.Payload[3].(uint64); forget > 1 || len(sealed) < 2 {
			return nil, false
		}
		return []any{sealed[1:]}, true
	}
	return nil, false
}

// Task 17 fix round 2: ops 25 and 26, the health probe's seal and the opening of its answer.
func TestProbeWrappersDecodeHelperAnswers(t *testing.T) {
	c := startFixture(t, "command")
	ctx := context.Background()
	sealed, handle, err := c.SealProbe(ctx, fixtureID(2), fixtureID(40), time.Unix(1_800_000_000, 0))
	if err != nil || handle != 3 || len(sealed) != 2 {
		t.Fatalf("SealProbe = %x %d %v", sealed, handle, err)
	}
	if _, _, err := c.SealProbe(ctx, fixtureID(2), fixtureID(40), time.Unix(1, 0)); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("a zero handle = %v, want an outcome-unknown error", err)
	}
	if _, _, err := c.SealProbe(ctx, fixtureID(2), id.ID{}, time.Unix(1_800_000_000, 0)); err == nil {
		t.Fatal("SealProbe accepted a zero request id")
	}
	body, err := c.OpenProbeReply(ctx, handle, 0, []byte{0x83, 0x18, 0xc8}, true)
	if err != nil || !bytes.Equal(body, []byte{0x18, 0xc8}) {
		t.Fatalf("OpenProbeReply = %x %v", body, err)
	}
	if _, err := c.OpenProbeReply(ctx, 0, 0, []byte{1, 2}, true); err == nil {
		t.Fatal("OpenProbeReply accepted handle 0")
	}
}

func TestCommandWrappersDecodeHelperAnswers(t *testing.T) {
	c := startFixture(t, "command")
	ctx := context.Background()
	if publication, err := c.CommandKey(ctx, fixtureID(2), time.Unix(1_800_000_000, 0)); err != nil || len(publication) != 2 {
		t.Fatalf("CommandKey = %x, %v", publication, err)
	}
	if _, err := c.CommandKey(ctx, id.ID{}, time.Unix(1_800_000_000, 0)); err == nil {
		t.Fatal("CommandKey accepted a zero community")
	}
	opened, err := c.OpenCommand(ctx, []byte{0x85}, fixtureID(5), fixtureID(6), time.Unix(1_800_000_000, 0))
	if err != nil || opened.Refusal != RefusalNone {
		t.Fatalf("OpenCommand = %#v, %v", opened, err)
	}
	r := opened.Request
	if r.RequestID != fixtureID(40) || r.CommunityID != fixtureID(2) || r.ChannelID != (id.ID{}) || r.Operation != 7 ||
		r.SentAt != 1_800_000_000 || r.ReplyKey[0] != 0xaa || r.UserID != fixtureID(5) || r.DeviceID != fixtureID(6) {
		t.Fatalf("opened request = %#v", r)
	}
	if len(r.Args) != 4 || r.Args[0] != "hello" || r.Args[1] != uint64(3) || string(r.Args[2].([]byte)) != "\x01" || r.Args[3] != nil {
		t.Fatalf("opened args = %#v", r.Args)
	}
	replay, err := c.OpenCommand(ctx, []byte{0x85}, fixtureID(5), fixtureID(6), time.Unix(1, 0))
	if err != nil || replay.Refusal != RefusalReplay || replay.ReplyKey != ([32]byte{}) {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	keyed, err := c.OpenCommand(ctx, []byte{0x85}, fixtureID(5), fixtureID(6), time.Unix(4, 0))
	if err != nil || keyed.Refusal != RefusalReplay || keyed.ReplyKey[0] != 0xaa || keyed.ReplayRequestID != fixtureID(41) || keyed.Request.RequestID != (id.ID{}) {
		t.Fatalf("replay with its reply key = %#v, %v", keyed, err)
	}
	signature, err := c.OpenCommand(ctx, []byte{0x85}, fixtureID(5), fixtureID(6), time.Unix(5, 0))
	if err != nil || signature.Refusal != RefusalBadSignature || signature.ReplyKey[0] != 0xaa {
		t.Fatalf("bad signature with its reply key = %#v, %v", signature, err)
	}
	if sealed, err := c.SealReply(ctx, [32]byte{1}, fixtureID(40), 2, []byte("ok")); err != nil || len(sealed) != 2 {
		t.Fatalf("SealReply = %x, %v", sealed, err)
	}
	if changed, version, err := c.IngestDeviceList(ctx, fixtureID(5), []byte{0x84}); err != nil || !changed || version != 7 {
		t.Fatalf("IngestDeviceList = %v %d %v", changed, version, err)
	}
	if changed, version, err := c.IngestDeviceList(ctx, fixtureID(5), []byte{1}); !errors.Is(err, ErrDeviceListHeld) || !changed || version != 7 {
		t.Fatalf("held IngestDeviceList = %v %d %v", changed, version, err)
	}
	if changed, version, err := c.IngestDeviceList(ctx, fixtureID(5), []byte{2}); !errors.Is(err, ErrDeviceListBehind) || changed || version != 7 {
		t.Fatalf("behind IngestDeviceList = %v %d %v", changed, version, err)
	}
	if _, _, err := c.IngestDeviceList(ctx, fixtureID(5), []byte{3}); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("undefined ingest state = %v", err)
	}
}

func TestOpenCommandRefusesUndefinedAnswers(t *testing.T) {
	for _, now := range []int64{2, 3, 6, 7, 8} {
		c := startFixture(t, "command")
		if _, err := c.OpenCommand(context.Background(), []byte{0x85}, fixtureID(5), fixtureID(6), time.Unix(now, 0)); err == nil {
			t.Fatalf("answer %d accepted", now)
		}
	}
}

func TestCommandRefusalNumbersAreFixed(t *testing.T) {
	got := []CommandRefusal{RefusalNone, RefusalMalformed, RefusalUnknownKey, RefusalBadSignature, RefusalWrongDevice, RefusalDeviceUnlisted, RefusalDeviceRevoked, RefusalStale, RefusalReplay}
	for want, refusal := range got {
		if int(refusal) != want {
			t.Fatalf("refusal %d has number %d", want, refusal)
		}
	}
}

func TestCallAcceptsCallGroupJoinOperations(t *testing.T) {
	c := startFixture(t, "")
	for _, operation := range []protocol.HelperOperation{protocol.HelperCallJoinCheck, protocol.HelperCallGroupForget, protocol.HelperGroupForget} {
		if _, err := c.Call(context.Background(), operation, []any{fixtureID(9)}); err != nil {
			t.Fatalf("helper operation %d: %v", operation, err)
		}
	}
}

func TestHelperStatusIsDefinitiveAndSanitized(t *testing.T) {
	c := startFixture(t, "status")
	_, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)})
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 5 || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("helper rejection = %#v, want definitive status 5", err)
	}
}

func TestCancellationWhileWaitingDoesNotKillActiveRequest(t *testing.T) {
	c := startFixture(t, "delay")
	done := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)}); done <- err }()
	time.Sleep(15 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, protocol.HelperStateGet, []any{uint64(0)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call = %v, want context deadline", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("active call was interrupted by queued cancellation: %v", err)
	}
	if _, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)}); err != nil {
		t.Fatalf("client did not remain usable: %v", err)
	}
}

func TestOpenOwnsQueueAndCancellationAndCloseBehindItAreBounded(t *testing.T) {
	c := newFixture(t, "stall-open", 5*time.Second)
	openDone := make(chan error, 1)
	openKey := make([]byte, 32)
	openKey[0] = 1
	go func() {
		_, err := c.Open(context.Background(), fixtureID(1), fixtureID(2), fixtureID(3), openKey)
		openDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, protocol.HelperStateGet, []any{uint64(0)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call queued behind Open = %v, want deadline", err)
	}
	started := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close behind Open: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Close behind Open exceeded its bound")
	}
	if err := <-openDone; !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("stalled Open = %v, want uncertain outcome", err)
	}
	if openKey[0] != 0 {
		t.Fatal("Open did not clear the consumed key")
	}
	select {
	case <-c.transport.(*linuxExchange).done:
	default:
		t.Fatal("helper child remained after bounded Close")
	}
}

func TestGroupApplyHasOneDeadlineAcrossAllTransferFrames(t *testing.T) {
	c := newFixture(t, "slow-transfer", time.Second)
	key := make([]byte, 32)
	key[0] = 1
	if _, err := c.Open(context.Background(), fixtureID(1), fixtureID(2), fixtureID(3), key); err != nil {
		t.Fatal(err)
	}
	// Process startup is unrelated to the staged transfer's deadline and can
	// exceed 150 ms on a busy race-detector runner.
	c.callTime = 150 * time.Millisecond
	started := time.Now()
	_, err := c.GroupApply(context.Background(), []any{uint64(0), make([]byte, 2<<20)})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("slow staged operation = %v, want uncertain outcome", err)
	}
	if time.Since(started) > 700*time.Millisecond {
		t.Fatal("staged operation exceeded its total deadline")
	}
}

func TestMalformedResponseClosesAndReapsHelper(t *testing.T) {
	for _, mode := range []string{"wrong-id", "wrong-version", "wrong-status", "oversized", "truncated", "crash"} {
		t.Run(mode, func(t *testing.T) {
			c := startFixture(t, mode)
			_, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)})
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("fault response = %v, want uncertain outcome", err)
			}
			select {
			case <-c.transport.(*linuxExchange).done:
			default:
				t.Fatal("protocol failure left helper child unreaped")
			}
			if _, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)}); !errors.Is(err, ErrClosed) {
				t.Fatalf("call after protocol failure = %v, want closed", err)
			}
		})
	}
}

func TestGroupApplyUsesStagedTransferAndChecksResultDigest(t *testing.T) {
	input := make([]byte, 2<<20)
	input[0], input[len(input)-1] = 7, 9
	c := startFixture(t, "transfer")
	got, err := c.GroupApply(context.Background(), []any{uint64(0), input})
	if err != nil {
		t.Fatalf("GroupApply: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("result values = %d, want 1", len(got))
	}
	result, ok := got[0].([]byte)
	if !ok || len(result) != 2<<20 || result[0] != 4 || result[len(result)-1] != 6 {
		t.Fatalf("large result does not match staged helper output")
	}
	c = startFixture(t, "transfer-bad-digest")
	_, err = c.GroupApply(context.Background(), []any{uint64(0), []byte("small payload")})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("bad result digest = %v, want uncertain outcome", err)
	}
	c = startFixture(t, "transfer-oversized-descriptor")
	_, err = c.GroupApply(context.Background(), []any{uint64(0), []byte("small payload")})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("oversized result descriptor = %v, want uncertain outcome", err)
	}
	c = startFixture(t, "transfer-wrong-offset")
	_, err = c.GroupApply(context.Background(), []any{uint64(0), []byte("small payload")})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("wrong upload offset acknowledgement = %v, want uncertain outcome", err)
	}
	c = startFixture(t, "transfer")
	tooLarge := make([]byte, maxLogicalTransfer+1)
	if _, err := c.GroupApply(context.Background(), []any{uint64(0), tooLarge}); err == nil {
		t.Fatal("accepted input above the 32 MiB logical cap")
	}
	if _, err := c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)}); err != nil {
		t.Fatalf("oversized input reached transport and damaged client: %v", err)
	}
}

func fixtureID(n byte) id.ID { var v id.ID; v[15] = n; return v }

// TestHelperProcess is a real exec child using fd3. Its protocol work is real;
// only the native helper implementation is replaced by a deterministic peer.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DILLA_HELPER_FIXTURE") != "1" {
		return
	}
	serveFixtureChild(os.Getenv("DILLA_HELPER_FIXTURE_MODE"))
	os.Exit(0)
}

func startFixture(t *testing.T, mode string) *Client {
	client := newFixture(t, mode, time.Second)
	key := make([]byte, 32)
	key[0] = 1
	if _, err := client.Open(context.Background(), fixtureID(1), fixtureID(2), fixtureID(3), key); err != nil {
		t.Fatalf("Open fixture: %v", err)
	}
	return client
}

func newFixture(t *testing.T, mode string, callTimeout time.Duration) *Client {
	t.Helper()
	client, err := Start(Options{Executable: os.Args[0], StateDir: t.TempDir(), CallTimeout: callTimeout, ShutdownTimeout: 100 * time.Millisecond, KillTimeout: time.Second,
		launcher: func(cmd *exec.Cmd) error {
			cmd.Env = []string{"DILLA_HELPER_FIXTURE=1", "DILLA_HELPER_FIXTURE_MODE=" + mode}
			cmd.Args = []string{cmd.Path, "-test.run=^TestHelperProcess$"}
			return cmd.Start()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func serveFixtureChild(mode string) {
	f := os.NewFile(3, "fixture-socket")
	raw, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		os.Exit(91)
	}
	conn := raw.(*net.UnixConn)
	defer conn.Close()
	var uploaded []byte
	transferID := fixtureID(77)
	result := make([]byte, 2<<20)
	result[0], result[len(result)-1] = 4, 6
	resultCBOR, _ := cborx.Marshal([]any{result})
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(prefix[:])
		if n == 0 || n > 1<<20 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		frame := append(append([]byte(nil), prefix[:]...), body...)
		request, err := protocol.DecodeHelperRequestFrame(frame)
		clear(body)
		if err != nil {
			return
		}
		if mode == "stall-open" && request.Operation == protocol.HelperOpen {
			time.Sleep(5 * time.Second)
			return
		}
		if request.Operation == protocol.HelperOpen || request.Operation == protocol.HelperShutdown {
			writeFixtureResponse(conn, request.RequestID, 0, []any{})
			if request.Operation == protocol.HelperShutdown {
				return
			}
			continue
		}
		if mode == "stall" || mode == "stall-after" {
			time.Sleep(5 * time.Second)
			return
		}
		if mode == "delay" || mode == "slow-transfer" {
			time.Sleep(100 * time.Millisecond)
		}
		if mode == "wrong-id" {
			writeFixtureResponse(conn, fixtureID(222), 0, []any{})
			return
		}
		if mode == "wrong-version" {
			body, _ := cborx.Marshal([]any{uint64(2), request.RequestID, uint64(0), []byte{0x80}})
			writeFixtureRaw(conn, body)
			return
		}
		if mode == "wrong-status" {
			body, _ := cborx.Marshal([]any{uint64(1), request.RequestID, uint64(10), []byte{0x80}})
			writeFixtureRaw(conn, body)
			return
		}
		if mode == "oversized" {
			var oversized [4]byte
			binary.BigEndian.PutUint32(oversized[:], (1<<20)+1)
			_, _ = conn.Write(oversized[:])
			return
		}
		if mode == "truncated" || mode == "crash" {
			_, _ = conn.Write([]byte{0, 0, 0, 20, 0x84, 0x01})
			return
		}
		if mode == "command" {
			if payload, ok := fixtureCommandAnswer(request); ok {
				writeFixtureResponse(conn, request.RequestID, 0, payload)
				continue
			}
		}
		status := uint64(0)
		if mode == "status" {
			status = 5
		}
		if request.Operation == protocol.HelperGroupTransfer && (mode == "transfer" || mode == "transfer-bad-digest" || mode == "transfer-oversized-descriptor" || mode == "transfer-wrong-offset" || mode == "slow-transfer") {
			command := request.Payload
			if len(command) == 0 {
				return
			}
			sub, _ := command[0].(uint64)
			var payload []any
			switch sub {
			case 0:
				payload = []any{transferID[:]}
			case 1:
				offset := command[2].(uint64)
				chunk := command[3].([]byte)
				if int(offset) != len(uploaded) {
					return
				}
				uploaded = append(uploaded, chunk...)
				ack := uint64(len(uploaded))
				if mode == "transfer-wrong-offset" {
					ack++
				}
				payload = []any{ack}
			case 2:
				digest := sha256.Sum256(resultCBOR)
				if mode == "transfer-bad-digest" {
					digest[0] ^= 1
				}
				length := uint64(len(resultCBOR))
				if mode == "transfer-oversized-descriptor" {
					length = uint64(maxLogicalTransfer + 1)
				}
				payload = []any{uint64(0), length, digest[:]}
			case 3:
				offset, size := command[2].(uint64), command[3].(uint64)
				payload = []any{resultCBOR[offset : offset+size]}
			case 4:
				payload = []any{}
			default:
				return
			}
			writeFixtureResponse(conn, request.RequestID, 0, payload)
			continue
		}
		writeFixtureResponse(conn, request.RequestID, status, []any{})
	}
}

func writeFixtureResponse(conn net.Conn, request id.ID, status uint64, payload []any) {
	response, err := protocol.EncodeHelperResponse(protocol.HelperResponse{RequestID: request, Status: status, Payload: payload})
	if err == nil {
		_, _ = conn.Write(response)
	}
}

func writeFixtureRaw(conn net.Conn, body []byte) {
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	_, _ = conn.Write(frame)
}

func idBytes(v id.ID) []byte { return v[:] }
