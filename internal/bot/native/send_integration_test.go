//go:build linux

package native

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestCompiledHelperNativeSendIntentIntegration(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN and DILLA_BOT_HELPER_INTEGRATION=1 to run native send integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := filepath.Join(root, "send")
	restoredState := filepath.Join(root, "restored-send")
	binding := Binding{Instance: integrationID(111), Community: integrationID(112), Installation: integrationID(113)}
	key := make([]byte, 32)
	copy(key, "native-send-integration-key")
	ctx := context.Background()
	open := func() (*Client, error) {
		return Start(ctx, helper.Options{Executable: binary, StateDir: state}, binding, append([]byte(nil), key...))
	}
	c, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Bootstrap(ctx, [32]byte{0x77}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.CompleteEnrollment(ctx, integrationID(114), "send integration", uint64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	group, channel, createOperation := integrationID(115), integrationID(116), integrationID(117)
	externalSender := make([]byte, 32)
	externalSender[0] = 1
	if _, err = c.CreateGroup(ctx, createOperation, group, channel, 0, 1, externalSender); err != nil {
		t.Fatal(err)
	}
	if err = c.GroupRegistered(ctx, createOperation, group, 1); err != nil {
		t.Fatal(err)
	}
	request, err := cborx.Marshal([]any{uint64(0), nil, "compiled helper send", []any{}, nil})
	if err != nil {
		t.Fatal(err)
	}
	operation, attempt := integrationID(118), integrationID(119)
	originalNow, generation := uint64(time.Now().Unix()), uint64(1)
	first, err := c.PrepareSend(ctx, operation, attempt, group, request, originalNow, generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore the real helper-produced encrypted SQLite state into a fresh
	// directory, as the Bot archive does. Copy only files that SQLite actually
	// created; sidecars are intentionally optional and must not be fabricated.
	if err := os.Mkdir(restoredState, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unexpected directory in native state: %s", entry.Name())
		}
		source := filepath.Join(state, entry.Name())
		dest := filepath.Join(restoredState, entry.Name())
		in, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = in.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(out, in)
		closeOutErr := out.Close()
		closeInErr := in.Close()
		if copyErr != nil || closeOutErr != nil || closeInErr != nil {
			t.Fatalf("copy native state %s: copy=%v close destination=%v close source=%v", entry.Name(), copyErr, closeOutErr, closeInErr)
		}
	}
	openRestored := func() (*Client, error) {
		return Start(ctx, helper.Options{Executable: binary, StateDir: restoredState}, binding, append([]byte(nil), key...))
	}
	c, err = openRestored()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	replayed, err := c.PrepareSend(ctx, operation, attempt, group, request, originalNow, generation)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("compiled helper send retry changed bytes: %v", err)
	}
	hash := sha256.Sum256(first.Ciphertext)
	receipt, err := protocol.EncodeReceipt(protocol.SendReceipt{
		OperationID: operation, AttemptID: attempt, CommunityID: binding.Community, GroupID: group,
		ExpectedGeneration: generation, CiphertextHash: hash[:], Class: first.Class,
		Outcome: protocol.ReceiptClosedNotAccepted, Epoch: first.Epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ConfirmSendReceipt(ctx, operation, receipt); err != nil {
		t.Fatalf("compiled helper send receipt confirmation: %v", err)
	}
}
