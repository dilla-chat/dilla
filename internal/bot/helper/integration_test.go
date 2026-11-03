//go:build linux

package helper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCompiledHelperIntegration(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN to run the compiled dilla-bot-core integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "private-state")
	instance, community, installation := realID(1), realID(2), realID(3)
	key := make([]byte, 32)
	copy(key, []byte("database-key-not-in-args-or-environment"))
	keySentinel := string(key)

	first := startReal(t, binary, state)
	if _, err := first.Open(context.Background(), instance, community, installation, append([]byte(nil), key...)); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("helper state directory mode = %v, %v; want 0700", info, err)
	}
	for _, name := range []string{"state.db", "state.lock"} {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %v, %v; want 0600", name, info, err)
		}
	}

	wrongKey := append([]byte(nil), key...)
	wrongKey[0] ^= 0xff
	wrong := startReal(t, binary, state)
	_, err = wrong.Open(context.Background(), instance, community, installation, wrongKey)
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 1 {
		t.Fatalf("wrong database key result = %v, want sanitized invalid status", err)
	}
	if wrongKey[0] != 0 || wrongKey[31] != 0 {
		t.Fatal("Open did not best-effort clear its key buffer")
	}
	if err := wrong.Close(); err != nil {
		t.Fatalf("wrong-key helper shutdown: %v", err)
	}

	reopened := startReal(t, binary, state)
	openKey := append([]byte(nil), key...)
	if _, err := reopened.Open(context.Background(), instance, community, installation, openKey); err != nil {
		t.Fatalf("reopen after wrong key: %v", err)
	}
	if openKey[0] != 0 || openKey[31] != 0 {
		t.Fatal("Open did not clear its key buffer")
	}
	instanceKey := [32]byte{0x77}
	args := []any{instance, community, installation, instanceKey[:]}
	one, err := reopened.Call(context.Background(), protocol.HelperBootstrap, args)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	two, err := reopened.Call(context.Background(), protocol.HelperBootstrap, args)
	if err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	if !reflect.DeepEqual(one, two) {
		t.Fatal("bootstrap result changed across repeated request")
	}
	account := realID(4)
	now := uint64(time.Now().Unix())
	completed, err := reopened.Call(context.Background(), protocol.HelperEnrollComplete, []any{account, "local integration bot", now})
	if err != nil {
		t.Fatalf("complete local enrollment: %v", err)
	}
	if len(completed) != 2 {
		t.Fatalf("enrollment result has %d values, want credential and signed device list", len(completed))
	}
	nonce := make([]byte, 32)
	nonce[0], nonce[31] = 0xa5, 0x5a
	signArgs := []any{uint64(1), nonce, uint64(0)}
	signed, err := reopened.Call(context.Background(), protocol.HelperSession, signArgs)
	if err != nil || len(signed) != 1 {
		t.Fatalf("session signing result = %#v, %v", signed, err)
	}
	expires, idle := now+3600, now+1800
	if _, err := reopened.Call(context.Background(), protocol.HelperSession, []any{uint64(2), "integration-session-sentinel", expires, idle}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	stored, err := reopened.Call(context.Background(), protocol.HelperSession, []any{uint64(3)})
	if err != nil {
		t.Fatalf("read stored session: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	last := startReal(t, binary, state)
	if _, err := last.Open(context.Background(), instance, community, installation, append([]byte(nil), key...)); err != nil {
		t.Fatalf("reopen persisted state: %v", err)
	}
	process := last.transport.(*linuxExchange).cmd
	if strings.Contains(strings.Join(process.Args, "\x00"), keySentinel) || strings.Contains(strings.Join(process.Env, "\x00"), keySentinel) {
		t.Fatal("database key appeared in helper arguments or environment")
	}
	loaded, err := last.Call(context.Background(), protocol.HelperSession, []any{uint64(3)})
	if err != nil || !reflect.DeepEqual(stored, loaded) {
		t.Fatalf("persisted session = %#v (%v), before restart %#v", loaded, err, stored)
	}
	resigned, err := last.Call(context.Background(), protocol.HelperSession, signArgs)
	if err != nil || !reflect.DeepEqual(signed, resigned) {
		t.Fatalf("persisted identity signing changed: %#v (%v)", resigned, err)
	}
	groups, err := last.GroupApply(context.Background(), []any{uint64(0)})
	if err != nil || len(groups) != 1 || !reflect.DeepEqual(groups[0], []byte{0x80}) {
		t.Fatalf("empty enrolled Bot group list = %#v (%v), want an encoded empty CBOR array", groups, err)
	}
	if err := last.Close(); err != nil {
		t.Fatalf("final shutdown: %v", err)
	}
}

func startReal(t *testing.T, executable, state string) *Client {
	t.Helper()
	client, err := Start(Options{Executable: executable, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func realID(n byte) id.ID { var value id.ID; value[15] = n; return value }
