package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"time"

	botruntime "github.com/dilla-chat/dilla/internal/bot/runtime"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func adminBotRuntime(verb string, args []string, stdout, stderr io.Writer) error {
	name := "admin bot-runtime " + verb
	switch verb {
	case "create":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			outPath := fs.String("out", "", "new protected file for the credential")
			return adminAction{check: func() error { return validateBotRuntimeSecretPath(*outPath, stderr, name) }, run: func(ctx context.Context, e *adminEnv) error {
				manager := botruntime.New(e.repo, func() time.Time { return cliClock.Now() })
				runtimeID, err := createBotRuntimeSecret(ctx, manager, *outPath)
				if err != nil {
					if errors.Is(err, botruntime.ErrCommitUncertain) {
						return fmt.Errorf("create bot runtime %s: %w: %w", runtimeID, err, exit.Software)
					}
					return fmt.Errorf("create bot runtime: %w: %w", err, exit.Software)
				}
				fmt.Fprintf(e.out, "created bot runtime %s; secret written to protected file\n", runtimeID)
				return nil
			}}
		})
	case "rotate":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			idText := fs.String("id", "", "runtime ID")
			outPath := fs.String("out", "", "new protected file for the credential")
			var runtimeID id.ID
			return adminAction{check: func() error {
				parsed, err := parseID(stderr, name, "id", *idText)
				if err != nil {
					return err
				}
				runtimeID = parsed
				return validateBotRuntimeSecretPath(*outPath, stderr, name)
			}, run: func(ctx context.Context, e *adminEnv) error {
				manager := botruntime.New(e.repo, func() time.Time { return cliClock.Now() })
				err := rotateBotRuntimeSecret(ctx, manager, runtimeID, *outPath)
				if err != nil {
					if errors.Is(err, botruntime.ErrCommitUncertain) {
						return fmt.Errorf("rotate bot runtime %s: %w: %w", runtimeID, err, exit.Software)
					}
					if errors.Is(err, store.ErrNotFound) {
						return notFound("bot runtime", err)
					}
					return fmt.Errorf("rotate bot runtime: %w: %w", err, exit.Software)
				}
				fmt.Fprintf(e.out, "rotated bot runtime %s; secret written to protected file\n", runtimeID)
				return nil
			}}
		})
	case "revoke":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			idText := fs.String("id", "", "runtime ID")
			var runtimeID id.ID
			return adminAction{check: func() error {
				parsed, err := parseID(stderr, name, "id", *idText)
				if err != nil {
					return err
				}
				runtimeID = parsed
				return nil
			}, run: func(ctx context.Context, e *adminEnv) error {
				manager := botruntime.New(e.repo, func() time.Time { return cliClock.Now() })
				if err := manager.Revoke(ctx, runtimeID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return notFound("bot runtime", err)
					}
					return fmt.Errorf("revoke bot runtime: %w: %w", err, exit.Software)
				}
				fmt.Fprintf(e.out, "revoked bot runtime %s\n", runtimeID)
				return nil
			}}
		})
	case "list":
		return runVerb(name, args, stdout, stderr, func(*flag.FlagSet) adminAction {
			return adminAction{run: func(ctx context.Context, e *adminEnv) error {
				after := id.ID{}
				for {
					rows, err := e.repo.ListBotRuntimes(ctx, after, 100)
					if err != nil {
						return fmt.Errorf("list bot runtimes: %w: %w", err, exit.Software)
					}
					for _, row := range rows {
						status := "active"
						if row.RevokedAt != nil {
							status = "revoked"
						}
						fmt.Fprintf(e.out, "%s\t%s\tcreated=%d\trotated=%d", row.ID, status, row.Created, row.Rotated)
						if row.RevokedAt != nil {
							fmt.Fprintf(e.out, "\trevoked=%d", *row.RevokedAt)
						}
						fmt.Fprintln(e.out)
						after = row.ID
					}
					if len(rows) < 100 {
						break
					}
				}
				return nil
			}}
		})
	default:
		return unknownVerb("bot-runtime", verb, stderr)
	}
}

func createBotRuntimeSecret(ctx context.Context, manager *botruntime.Manager, path string) (id.ID, error) {
	var created *createdBotRuntimeSecret
	runtimeID, err := manager.Create(ctx, func(_ id.ID, token string) error {
		var writeErr error
		created, writeErr = writeBotRuntimeSecret(path, token)
		return writeErr
	})
	return runtimeID, finishBotRuntimeSecretFile(created, err)
}

func rotateBotRuntimeSecret(ctx context.Context, manager *botruntime.Manager, runtimeID id.ID, path string) error {
	var created *createdBotRuntimeSecret
	err := manager.Rotate(ctx, runtimeID, func(_ id.ID, token string) error {
		var writeErr error
		created, writeErr = writeBotRuntimeSecret(path, token)
		return writeErr
	})
	return finishBotRuntimeSecretFile(created, err)
}

func finishBotRuntimeSecretFile(created *createdBotRuntimeSecret, operationErr error) error {
	if errors.Is(operationErr, botruntime.ErrCommitUncertain) {
		created.release()
		return fmt.Errorf("database commit outcome uncertain; protected secret file retained: %w", botruntime.ErrCommitUncertain)
	}
	if operationErr != nil {
		if created != nil {
			if removeErr := created.cleanup(); removeErr != nil {
				return fmt.Errorf("operation failed and its new secret file could not be removed: %w", removeErr)
			}
		}
		return operationErr
	}
	created.release()
	return nil
}

func validateSecretParent(parent string) error {
	dir, err := openSecretParent(parent)
	if err != nil {
		return err
	}
	return dir.Close()
}

func verifySecretOwnerAndMode(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() || !sameOwner(info) {
		return fmt.Errorf("secret file must be owned, regular and mode 0600")
	}
	return nil
}

func sameOwner(info os.FileInfo) bool {
	current, err := user.Current()
	if err != nil {
		return false
	}
	sys := reflect.ValueOf(info.Sys())
	if sys.Kind() == reflect.Pointer {
		sys = sys.Elem()
	}
	if !sys.IsValid() || sys.Kind() != reflect.Struct {
		return false
	}
	uid := sys.FieldByName("Uid")
	if !uid.IsValid() {
		return false
	}
	switch uid.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprint(uid.Uint()) == current.Uid
	case reflect.String:
		return uid.String() == current.Uid
	default:
		return false
	}
}

func validateBotRuntimeSecretPath(path string, stderr io.Writer, command string) error {
	if path == "" {
		return usageError(stderr, command, "--out is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return usageError(stderr, command, "invalid --out path")
	}
	parent := filepath.Dir(abs)
	if err := validateSecretParent(parent); err != nil {
		return usageError(stderr, command, "--out parent must be an owned private 0700 directory with no symlink ancestors: %v", err)
	}
	if _, err := os.Lstat(abs); err == nil { //nolint:gosec // G703: admin CLI path has an owned private parent validated above; Lstat does not follow the final component.
		return usageError(stderr, command, "--out already exists")
	} else if !os.IsNotExist(err) {
		return usageError(stderr, command, "cannot inspect --out path")
	}
	return nil
}

// writeBotRuntimeSecret atomically creates a new 0600 file. O_EXCL also rejects
// an existing symlink without following it. A failed write removes only the
// inode this call created; successful callers own cleanup if the DB commit fails.
type createdBotRuntimeSecret struct {
	dir      *os.File
	file     *os.File
	name     string
	identity os.FileInfo
	cleaned  bool
}

func (s *createdBotRuntimeSecret) cleanup() error {
	if s == nil || s.cleaned {
		return nil
	}
	s.cleaned = true
	defer func() {
		_ = s.file.Close()
		_ = s.dir.Close()
	}()
	err := unlinkCreatedSecret(s.dir, s.name, s.identity)
	if err != nil {
		return err
	}
	return s.dir.Sync()
}

func (s *createdBotRuntimeSecret) release() {
	if s != nil && !s.cleaned {
		s.cleaned = true
		_ = s.file.Close()
		_ = s.dir.Close()
	}
}

func writeBotRuntimeSecret(path, token string) (*createdBotRuntimeSecret, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid secret path")
	}
	dir, err := openSecretParent(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("unsafe secret directory")
	}
	name := filepath.Base(absolute)
	f, err := openNewSecretFileAt(dir, name)
	if err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("cannot create new secret file")
	}
	identity, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = dir.Close()
		return nil, fmt.Errorf("cannot inspect secret file")
	}
	pinned, err := retainSecretInode(f)
	if err != nil {
		created := &createdBotRuntimeSecret{dir: dir, file: f, name: name, identity: identity}
		_ = created.cleanup()
		return nil, fmt.Errorf("cannot retain secret file identity")
	}
	created := &createdBotRuntimeSecret{dir: dir, file: pinned, name: name, identity: identity}
	sourceClosed := false
	closeSource := func() error {
		if sourceClosed {
			return nil
		}
		sourceClosed = true
		return f.Close()
	}
	cleanup := func() { _ = closeSource(); _ = created.cleanup() }
	if err := verifySecretOwnerAndMode(f); err != nil {
		cleanup()
		return nil, fmt.Errorf("secret file owner or mode is unsafe")
	}
	if _, err := io.WriteString(f, token+"\n"); err != nil {
		cleanup()
		return nil, fmt.Errorf("cannot write secret file")
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("cannot sync secret file")
	}
	if err := closeSource(); err != nil {
		_ = created.cleanup()
		return nil, fmt.Errorf("cannot close secret file")
	}
	if err := dir.Sync(); err != nil {
		_ = created.cleanup()
		return nil, fmt.Errorf("cannot sync secret directory")
	}
	return created, nil
}
