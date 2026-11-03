package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	botruntime "github.com/dilla-chat/dilla/internal/bot/runtime"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestProtectedRuntimeSecretFilesAndAdminLifecycle(t *testing.T) {
	h := newCLIHarness(t)
	secretDir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(secretDir, "runtime.token")
	code, stdout, stderr := h.Run3(t, "admin", "bot-runtime", "create", "--config="+h.ConfigPath, "--out="+secretPath)
	if code != 0 {
		t.Fatalf("create exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout == "" || strings.Contains(stdout+stderr, "dbr_") {
		t.Fatalf("credential appeared in command output: stdout=%q stderr=%q", stdout, stderr)
	}
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %o", info.Mode().Perm())
	}
	contents, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(contents))
	if !strings.HasPrefix(token, "dbr_") {
		t.Fatalf("secret file does not contain a dbr_ token")
	}
	fields := strings.Fields(stdout)
	if len(fields) < 4 {
		t.Fatalf("created runtime id missing from stdout %q", stdout)
	}
	runtimeID, err := id.Parse(strings.TrimSuffix(fields[3], ";"))
	if err != nil {
		t.Fatalf("created runtime id missing from stdout %q: %v", stdout, err)
	}
	manager := botruntime.New(h.Repo(t), func() time.Time { return h.clk.Now() })
	if got, err := manager.Authenticate(context.Background(), token); err != nil || got != runtimeID {
		t.Fatalf("authenticate created token: %s %v", got, err)
	}
	if got, err := h.Repo(t).GetBotRuntime(context.Background(), runtimeID); err != nil {
		t.Fatalf("runtime persistence: %+v %v", got, err)
	} else if hash := sha256.Sum256([]byte(token)); !bytes.Equal(got.TokenHash, hash[:]) {
		t.Fatal("database does not contain the hash of the complete token")
	}
	listCode, listed := h.Run(t, "admin", "bot-runtime", "list", "--config="+h.ConfigPath)
	if listCode != 0 || strings.Contains(listed, token) {
		t.Fatalf("list leaked credential or failed: %q", listed)
	}

	rotatePath := filepath.Join(secretDir, "rotated.token")
	code, stdout, stderr = h.Run3(t, "admin", "bot-runtime", "rotate", "--config="+h.ConfigPath, "--id="+runtimeID.String(), "--out="+rotatePath)
	if code != 0 || strings.Contains(stdout+stderr, "dbr_") {
		t.Fatalf("rotate exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	newBytes, err := os.ReadFile(rotatePath)
	if err != nil {
		t.Fatal(err)
	}
	newToken := strings.TrimSpace(string(newBytes))
	if _, err := manager.Authenticate(context.Background(), token); !errors.Is(err, botruntime.ErrUnauthorized) {
		t.Fatalf("old credential remains active: %v", err)
	}
	if got, err := manager.Authenticate(context.Background(), newToken); err != nil || got != runtimeID {
		t.Fatalf("new credential failed: %v", err)
	}
	code, stdout, stderr = h.Run3(t, "admin", "bot-runtime", "revoke", "--config="+h.ConfigPath, "--id="+runtimeID.String())
	if code != 0 || strings.Contains(stdout+stderr, "dbr_") {
		t.Fatalf("revoke exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := manager.Authenticate(context.Background(), newToken); !errors.Is(err, botruntime.ErrUnauthorized) {
		t.Fatalf("revoked credential remains active: %v", err)
	}
}

func TestProtectedRuntimeSecretWriterRefusesUnsafeDestinations(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "secret")
	created, err := writeBotRuntimeSecret(path, "dbr_sentinel")
	if err != nil || created == nil {
		t.Fatalf("write: created=%v err=%v", created != nil, err)
	}
	created.release()
	if created, err := writeBotRuntimeSecret(path, "dbr_replacement"); err == nil || created != nil {
		t.Fatalf("existing file accepted: created=%v err=%v", created != nil, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "dbr_sentinel\n" {
		t.Fatalf("existing contents changed: %q %v", got, err)
	}
	if err := os.Chmod(private, 0o750); err != nil {
		t.Fatal(err)
	}
	if created, err := writeBotRuntimeSecret(filepath.Join(private, "wrong-mode"), "dbr_other"); err == nil || created != nil {
		t.Fatalf("wrong-mode directory accepted: created=%v err=%v", created != nil, err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if created, err := writeBotRuntimeSecret(filepath.Join(link, "linked"), "dbr_other"); err == nil || created != nil {
		t.Fatalf("symlink ancestor accepted: created=%v err=%v", created != nil, err)
	}
	writable := filepath.Join(root, "writable")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(writable, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if created, err := writeBotRuntimeSecret(filepath.Join(writable, "private", "secret"), "dbr_other"); err == nil || created != nil {
		t.Fatalf("non-sticky writable ancestor accepted: created=%v err=%v", created != nil, err)
	}
	if err := os.Chmod(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(private, "replacement")
	handle, err := writeBotRuntimeSecret(replacementPath, "dbr_first")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(replacementPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handle.cleanup(); err == nil {
		t.Fatal("cleanup removed/reported success for a replacement inode")
	}
	got, err = os.ReadFile(replacementPath)
	if err != nil || string(got) != "keep me" {
		t.Fatalf("replacement file was removed or changed: %q %v", got, err)
	}
}

type lostRuntimeCommitAck struct{ store.Repository }

func (r lostRuntimeCommitAck) Tx(ctx context.Context, fn func(store.Repository) error) error {
	if err := r.Repository.Tx(ctx, fn); err != nil {
		return err
	}
	return errors.New("simulated lost commit acknowledgement")
}

type failRuntimeRotation struct{ store.Repository }

func (r failRuntimeRotation) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return r.Repository.Tx(ctx, func(tx store.Repository) error { return fn(failRuntimeRotationTx{Repository: tx}) })
}

type failRuntimeRotationTx struct{ store.Repository }

func (failRuntimeRotationTx) RotateBotRuntime(context.Context, id.ID, []byte, []byte, int64) error {
	return store.ErrConflict
}

func TestProtectedRuntimeSecretCommitUncertaintyRetainsSecret(t *testing.T) {
	h := newCLIHarness(t)
	base := h.Repo(t)
	defer base.Close()
	ctx := context.Background()
	initialPath := filepath.Join(t.TempDir(), "initial.token")
	manager := botruntime.New(base, func() time.Time { return h.clk.Now() })
	runtimeID, err := createBotRuntimeSecret(ctx, manager, initialPath)
	if err != nil {
		t.Fatal(err)
	}
	initialBytes, err := os.ReadFile(initialPath)
	if err != nil {
		t.Fatal(err)
	}
	initialToken := strings.TrimSpace(string(initialBytes))

	knownFailurePath := filepath.Join(filepath.Dir(initialPath), "known-failure.token")
	knownManager := botruntime.New(failRuntimeRotation{Repository: base}, func() time.Time { return h.clk.Now() })
	if err := rotateBotRuntimeSecret(ctx, knownManager, runtimeID, knownFailurePath); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("known CAS failure = %v, want conflict", err)
	}
	if _, err := os.Lstat(knownFailurePath); !os.IsNotExist(err) {
		t.Fatalf("known CAS failure secret file remains: %v", err)
	}
	if got, err := manager.Authenticate(ctx, initialToken); err != nil || got != runtimeID {
		t.Fatalf("old token after known rollback = %s, %v", got, err)
	}

	uncertainPath := filepath.Join(filepath.Dir(initialPath), "uncertain.token")
	uncertainManager := botruntime.New(lostRuntimeCommitAck{Repository: base}, func() time.Time { return h.clk.Now() })
	uncertainErr := rotateBotRuntimeSecret(ctx, uncertainManager, runtimeID, uncertainPath)
	if !errors.Is(uncertainErr, botruntime.ErrCommitUncertain) {
		t.Fatalf("lost commit acknowledgement = %v", uncertainErr)
	}
	uncertainBytes, err := os.ReadFile(uncertainPath)
	if err != nil {
		t.Fatalf("secret file was removed after uncertain commit: %v", err)
	}
	if strings.Contains(uncertainErr.Error(), "dbr_") {
		t.Fatalf("error leaked credential: %v", uncertainErr)
	}
	newToken := strings.TrimSpace(string(uncertainBytes))
	if _, err := manager.Authenticate(ctx, initialToken); !errors.Is(err, botruntime.ErrUnauthorized) {
		t.Fatalf("old token after committed but uncertain rotation = %v", err)
	}
	if got, err := manager.Authenticate(ctx, newToken); err != nil || got != runtimeID {
		t.Fatalf("retained token after uncertain commit = %s, %v", got, err)
	}
}
