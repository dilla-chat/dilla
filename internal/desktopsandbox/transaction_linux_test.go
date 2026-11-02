//go:build linux

package desktopsandbox

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func initialMaintainer(t *testing.T) (*Maintainer, fileLayout, Capsule, ed25519.PrivateKey) {
	t.Helper()
	f, l := fixtureFiles(t)
	if err := f.lock(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	c, trust, key := fixture(t, 1)
	c.Signature = nil
	trust.Initial = initialFor(t, c)
	capFile, e := f.writeNew(stageDir, "bootstrap-capsule", frame(c), 0o600)
	if e != nil {
		t.Fatal(e)
	}
	defer capFile.close()
	manager, e := f.writeNew(stageDir, "bootstrap-maintainer", c.Maintainer, 0o700)
	if e != nil {
		t.Fatal(e)
	}
	defer manager.close()
	a := BootstrapAnchor{Format: 1, Kind: "dilla-linux-sandbox-bootstrap", Operation: "initial", Generation: 1, AcceptedAt: testNow.UnixMilli(), ManifestSHA512: digest(c.Manifest), CompatibilityEpoch: 1, SandboxAPI: 1, Capsule: capFile.identity, Maintainer: manager.identity}
	raw, _ := json.Marshal(a)
	p, e := f.writeNew(stageDir, "bootstrap-anchor.json", raw, 0o600)
	if e != nil {
		t.Fatal(e)
	}
	p.close()
	return &Maintainer{files: f, trust: trust, now: func() time.Time { return testNow }}, l, c, key
}
func changedCandidate(t *testing.T, c Capsule, key ed25519.PrivateKey, generation int64) Capsule {
	t.Helper()
	next := copyCapsule(c)
	next.Maintainer = executable(0x90)
	next.Helper = executable(0x91)
	mutateManifest(t, &next, key, func(m map[string]any) {
		m["generation"] = generation
		m["maintainer"].(map[string]any)["sha512"] = digest(next.Maintainer)
		m["helper"].(map[string]any)["sha512"] = digest(next.Helper)
	})
	return next
}
func admitted(t *testing.T, c Capsule, trust Trust, now time.Time) VerifiedCapsule {
	t.Helper()
	v, e := VerifyCapsule(c, trust, Receipt{}, now)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func reopened(t *testing.T, old *Maintainer, l fileLayout, now time.Time) *Maintainer {
	t.Helper()
	trust := old.trust
	if e := old.Close(); e != nil {
		t.Fatal(e)
	}
	f, e := openFixedFiles(l, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.lock(t.Context(), false); e != nil {
		t.Fatal(e)
	}
	m := &Maintainer{files: f, trust: trust, now: func() time.Time { return now }}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestInitialRecoveryPublishesExactAuthenticatedComponentsAndCleansBootstrap(t *testing.T) {
	m, l, c, _ := initialMaintainer(t)
	r, e := m.Recover(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if r.MaximumGeneration != 1 || r.State != "active" || r.LastOperationAt < r.AcceptedAt {
		t.Fatal("incorrect committed receipt", r)
	}
	for _, entry := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"sandbox-maintainer", c.Maintainer, 0o755}, {"chrome-sandbox", c.Helper, 0o555}} {
		b, e := os.ReadFile(filepath.Join(l.root(), entry.name))
		if e != nil || !bytes.Equal(b, entry.data) {
			t.Fatal("published different component", entry.name, e)
		}
		s, e := os.Stat(filepath.Join(l.root(), entry.name))
		if e != nil || s.Mode().Perm() != entry.mode {
			t.Fatal("wrong component mode", entry.name, e)
		}
	}
	for _, name := range []string{"bootstrap-anchor.json", "bootstrap-capsule", "bootstrap-maintainer", "maintainer.next", "helper.next", "receipt.next"} {
		if _, e = os.Lstat(filepath.Join(l.root(), ".sandbox-staging", name)); !os.IsNotExist(e) {
			t.Fatal("retained bootstrap/retired program", name, e)
		}
	}
	if pending, e := m.NeedsRecovery(); e != nil || pending {
		t.Fatal("successful operation still pending", e)
	}
}

func TestHelperReplacementRevokesAndFlushesOldInodeBeforeExchangeAndRecoversForward(t *testing.T) {
	m, l, c, key := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	old, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
	if e != nil {
		t.Fatal(e)
	}
	defer old.close()
	next := changedCandidate(t, c, key, 2)
	m.now = func() time.Time { return testNow.Add(time.Hour) }
	v := admitted(t, next, m.trust, m.now())
	sawFlush := false
	m.files.fault = func(op string, after bool) error {
		if op == "sync-file:old-helper" && after {
			sawFlush = true
		}
		if op == "exchange:chrome-sandbox" && !after {
			var info unix.Stat_t
			if e := unix.Fstat(old.fd, &info); e != nil {
				t.Fatal(e)
			}
			if info.Mode&0o7777 != 0o755 || !sawFlush {
				t.Fatal("old inode not demoted and durable before exchange")
			}
			return unix.EIO
		}
		return nil
	}
	if _, e = m.Install(t.Context(), v); !errors.Is(e, unix.EIO) {
		t.Fatal("fixture did not interrupt helper exchange", e)
	}
	var info unix.Stat_t
	if e = unix.Fstat(old.fd, &info); e != nil || info.Mode&0o7777 != 0o755 {
		t.Fatal("failed operation left old privileged inode", e)
	}
	r, e := ParseReceipt(mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json")))
	if e != nil || r.MaximumGeneration != 1 {
		t.Fatal("published receipt before components", e)
	}
	m2 := reopened(t, m, l, testNow.Add(48*time.Hour))
	r, e = m2.Recover(t.Context())
	if e != nil {
		t.Fatal("historical exact recovery failed", e)
	}
	if r.MaximumGeneration != 2 || r.AcceptedAt != v.AcceptedAt().UnixMilli() || r.LastOperationAt != m2.now().UnixMilli() {
		t.Fatal("lost admitted/completion times", r)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(l.root(), "chrome-sandbox")), next.Helper) {
		t.Fatal("recovered old helper instead of forward generation")
	}
}

func TestRemovalRecoveryAfterUnlinkKeepsFloorAndDoesNotNeedExpiredHelper(t *testing.T) {
	m, l, _, _ := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	old, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
	if e != nil {
		t.Fatal(e)
	}
	defer old.close()
	m.now = func() time.Time { return testNow.Add(48 * time.Hour) }
	m.files.fault = func(op string, after bool) error {
		if op == "unlink:root:chrome-sandbox" && after {
			return unix.EIO
		}
		return nil
	}
	if e = m.Remove(t.Context()); !errors.Is(e, unix.EIO) {
		t.Fatal("remove interruption not reached", e)
	}
	if _, e = os.Lstat(filepath.Join(l.root(), "chrome-sandbox")); !os.IsNotExist(e) {
		t.Fatal("helper was not removed", e)
	}
	var info unix.Stat_t
	if e = unix.Fstat(old.fd, &info); e != nil || info.Mode&0o7777 != 0o755 {
		t.Fatal("open retired inode retains privilege", e)
	}
	m2 := reopened(t, m, l, testNow.Add(49*time.Hour))
	r, e := m2.Recover(t.Context())
	if e != nil || r.State != "removed" || r.MaximumGeneration != 1 || r.LastOperationAt != m2.now().UnixMilli() || r.AcceptedAt >= r.LastOperationAt {
		t.Fatal("lost tombstone/history", r, e)
	}
	if _, e = os.Lstat(filepath.Join(l.root(), "sandbox-maintainer")); e != nil {
		t.Fatal("removed shared repair entry", e)
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestProcessFinishesPriorAuthorityWithoutReadingNewInput(t *testing.T) {
	m, _, _, _ := initialMaintainer(t)
	called := false
	reader := readerFunc(func([]byte) (int, error) {
		called = true
		return 0, errors.New("must not read unrelated incoming input")
	})
	if _, e := m.Process(context.Background(), reader); e != nil {
		t.Fatal(e)
	}
	if called {
		t.Fatal("recovery consumed new input")
	}
}

type readerFunc func([]byte) (int, error)

func (fn readerFunc) Read(b []byte) (int, error) { return fn(b) }

func TestInstallStorageFaultsRecoverForwardWithoutReprivilegingOldFD(t *testing.T) {
	// Discover actual syscall boundaries from a completed, authenticated update.
	// Each subsequent case fails one real operation, then opens the same files
	// again with no new input. This checks externally observable recovery state.
	manager, _, capsule, key := initialMaintainer(t)
	if _, err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return testNow.Add(time.Hour) }
	next := changedCandidate(t, capsule, key, 2)
	operations := map[string]bool{}
	manager.files.fault = func(op string, _ bool) error { operations[op] = true; return nil }
	if _, err := manager.Install(t.Context(), admitted(t, next, manager.trust, manager.now())); err != nil {
		t.Fatal(err)
	}
	for op := range operations {
		for _, after := range []bool{false, true} {
			for _, fault := range []error{unix.ENOSPC, unix.EDQUOT, unix.EIO} {
				t.Run(fmt.Sprintf("%s/after=%v/%v", op, after, fault), func(t *testing.T) {
					m, l, c, k := initialMaintainer(t)
					if _, e := m.Recover(t.Context()); e != nil {
						t.Fatal(e)
					}
					old, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
					if e != nil {
						t.Fatal(e)
					}
					defer old.close()
					m.now = func() time.Time { return testNow.Add(time.Hour) }
					candidate := changedCandidate(t, c, k, 2)
					injected := false
					m.files.fault = func(actual string, did bool) error {
						if actual == op && did == after && !injected {
							injected = true
							return fault
						}
						return nil
					}
					_, e = m.Install(t.Context(), admitted(t, candidate, m.trust, m.now()))
					if !injected || !errors.Is(e, fault) {
						t.Fatal("fault boundary not reached", injected, e)
					}
					var held unix.Stat_t
					if e = unix.Fstat(old.fd, &held); e != nil {
						t.Fatal(e)
					}
					revoked := held.Mode&0o7777 == 0o755
					m2 := reopened(t, m, l, testNow.Add(2*time.Hour))
					pending, e := m2.NeedsRecovery()
					if e != nil {
						t.Fatal(e)
					}
					if pending {
						if _, e = m2.Recover(t.Context()); e != nil {
							t.Fatal("recovery failed", e)
						}
					} else {
						// No durable intent means the old program must be intact. The same
						// authenticated candidate may be retried as an ordinary fresh operation.
						if revoked {
							r, e := ParseReceipt(mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json")))
							if e != nil || r.MaximumGeneration != 2 || m2.current(r) != nil {
								t.Fatal("revoked old inode without terminal committed receipt", e)
							}
							return
						}
						if _, e = m2.Install(t.Context(), admitted(t, candidate, m2.trust, m2.now())); e != nil {
							t.Fatal("fresh retry failed", e)
						}
					}
					if e = unix.Fstat(old.fd, &held); e != nil || held.Mode&0o7777 != 0o755 {
						t.Fatal("old open inode regained privilege", e)
					}
					if !bytes.Equal(mustRead(t, filepath.Join(l.root(), "chrome-sandbox")), candidate.Helper) {
						t.Fatal("did not recover exact new bytes")
					}
				})
			}
		}
	}
}

func TestTerminalBootstrapCleanupDoesNotRequireAlreadyDeletedStages(t *testing.T) {
	m, l, _, _ := initialMaintainer(t)
	m.files.fault = func(op string, after bool) error {
		if op == "unlink:stage:bootstrap-capsule" && after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m.Recover(t.Context()); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	if _, e := os.Lstat(filepath.Join(l.root(), ".sandbox-staging", "bootstrap-capsule")); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	m2 := reopened(t, m, l, testNow.Add(48*time.Hour))
	if _, e := m2.Recover(t.Context()); e != nil {
		t.Fatal("committed cleanup required deleted stage", e)
	}
}
func TestMissingPrecommitStageAndNewerIntentNeverUseOldReceiptAsTerminal(t *testing.T) {
	m, l, c, k := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return testNow.Add(time.Hour) }
	m.files.fault = func(op string, after bool) error {
		if op == "publish:sandbox-transaction.json" && after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m.Install(t.Context(), admitted(t, changedCandidate(t, c, k, 2), m.trust, m.now())); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(l.root(), ".sandbox-staging", "helper.next")); e != nil {
		t.Fatal(e)
	}
	m2 := reopened(t, m, l, testNow.Add(2*time.Hour))
	if _, e := m2.Recover(t.Context()); e == nil {
		t.Fatal("missing protected pending bytes accepted")
	}
	r, e := ParseReceipt(mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json")))
	if e != nil || r.MaximumGeneration != 1 {
		t.Fatal("changed old receipt", e)
	}
}

func TestPublicStatusNeverCallsPendingOrCorruptProgramsReady(t *testing.T) {
	m, l, c, k := initialMaintainer(t)
	if got := statusFor(m); got.Status == "ready" {
		t.Fatal("bootstrap anchor called ready")
	}
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	if got := statusFor(m); got.Status != "ready" || got.Generation != 1 {
		t.Fatal(got)
	}
	m.now = func() time.Time { return testNow.Add(time.Hour) }
	m.files.fault = func(op string, after bool) error {
		if op == "exchange:chrome-sandbox" && !after {
			return unix.ENOSPC
		}
		return nil
	}
	if _, e := m.Install(t.Context(), admitted(t, changedCandidate(t, c, k, 2), m.trust, m.now())); !errors.Is(e, unix.ENOSPC) {
		t.Fatal(e)
	}
	if got := statusFor(m); got.Status != "maintenance" {
		t.Fatal("older receipt hid live pending authority", got)
	}
	m2 := reopened(t, m, l, testNow.Add(2*time.Hour))
	if _, e := m2.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(l.root(), "chrome-sandbox"), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(l.root(), "chrome-sandbox"), []byte("corrupt"), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(l.root(), "chrome-sandbox"), 0o555); e != nil {
		t.Fatal(e)
	}
	if got := statusFor(m2); got.Status != "unsafe" {
		t.Fatal("corrupt highest helper accepted", got)
	}
}
func TestUnexpectedNamespaceEntriesHoldWithoutDeletingThem(t *testing.T) {
	m, l, _, _ := initialMaintainer(t)
	path := filepath.Join(l.root(), "unrecognized-component")
	if e := os.WriteFile(path, []byte("leave untouched"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Recover(t.Context()); e == nil {
		t.Fatal("unexpected privileged namespace child accepted")
	}
	if b := mustRead(t, path); string(b) != "leave untouched" {
		t.Fatal("deleted unexpected child")
	}
}

func TestCompletionReceiptWriteAndFlushFaultsRemainRecoverable(t *testing.T) {
	for _, target := range []string{"write:stage:receipt.next", "sync-file:receipt.next"} {
		for _, fault := range []error{unix.ENOSPC, unix.EDQUOT, unix.EIO} {
			t.Run(target+fault.Error(), func(t *testing.T) {
				m, l, c, k := initialMaintainer(t)
				if _, e := m.Recover(t.Context()); e != nil {
					t.Fatal(e)
				}
				m.now = func() time.Time { return testNow.Add(time.Hour) }
				count := 0
				injected := false
				m.files.fault = func(op string, after bool) error {
					if op == target && !after {
						count++
						wanted := 2
						if target == "write:stage:receipt.next" {
							wanted = 3
						}
						if count == wanted {
							injected = true
							return fault
						}
					}
					return nil
				}
				candidate := changedCandidate(t, c, k, 2)
				if _, e := m.Install(t.Context(), admitted(t, candidate, m.trust, m.now())); !injected || !errors.Is(e, fault) {
					t.Fatal("completion fault not reached", count, e)
				}
				m2 := reopened(t, m, l, testNow.Add(48*time.Hour))
				r, e := m2.Recover(t.Context())
				if e != nil || r.MaximumGeneration != 2 {
					t.Fatal("completion write failure stranded recovery", e)
				}
			})
		}
	}
}
func TestPaddedCommittedReceiptIsReadByExactPythonBootstrapDecoder(t *testing.T) {
	m, l, _, _ := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	raw := mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json"))
	if len(raw) != recordLimit {
		t.Fatal("receipt reservation changed")
	}
	r, e := ParseReceipt(raw)
	if e != nil {
		t.Fatal(e)
	}
	_, source, _, _ := runtime.Caller(0)
	template, e := os.ReadFile(filepath.Join(filepath.Dir(source), "../../packages/desktop/native/linux-sandbox/frontend/bootstrap.py"))
	if e != nil {
		t.Fatal(e)
	}
	code := string(template)
	boundary := "\ntry:\n    run()\nexcept Exception:"
	if strings.Count(code, boundary) != 1 {
		t.Fatal("bootstrap boundary changed")
	}
	code = strings.Split(code, boundary)[0]
	code = strings.Replace(code, "CONFIG = __DILLA_CONFIGURATION__", "CONFIG = {}", 1)
	code = strings.Replace(code, "OWNER_UID = 0", fmt.Sprintf("OWNER_UID = %d", os.Geteuid()), 1)
	code = strings.Replace(code, "OWNER_GID = 0", fmt.Sprintf("OWNER_GID = %d", os.Getegid()), 1)
	quoted, _ := json.Marshal(l.root())
	expected, _ := json.Marshal(r.SHA512())
	code += "\nroot = os.open(" + string(quoted) + ", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)\nvalue = receipt(record(root, 'sandbox-receipt.json', MAX_RECORD))\nfd = pin_file(root, 'sandbox-receipt.json', 0o644, MAX_RECORD)\nrequire(base64.b64encode(hashlib.sha512(read_bytes(fd, MAX_RECORD)).digest()).decode('ascii') == " + string(expected) + ")\nrequire(value['lastOperationAt'] >= value['acceptedAt'])\n"
	command := exec.CommandContext(t.Context(), "/usr/bin/python3", "-I", "-S", "-c", code)
	if out, e := command.CombinedOutput(); e != nil {
		t.Fatalf("padded Go/Python record interop failed: %v %s", e, out)
	}
}

type crashFixture struct {
	Trust     Trust
	Capsule   Capsule
	Operation string
	After     bool
}

func TestOwnedTransactionCrashProcess(t *testing.T) {
	if len(os.Args) < 1 || os.Args[len(os.Args)-1] != "owned-transaction-crash" {
		return
	}
	anchor := os.Getenv("OWNED_TEST_ANCHOR")
	raw, e := os.ReadFile(filepath.Join(anchor, "crash-fixture.json"))
	if e != nil {
		t.Fatal(e)
	}
	var c crashFixture
	if e = json.Unmarshal(raw, &c); e != nil {
		t.Fatal(e)
	}
	l := fileLayout{anchor: anchor, parts: []string{"dilla", "appimage"}, uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), helperMode: 0o555}
	f, e := openFixedFiles(l, true)
	if e != nil {
		t.Fatal(e)
	}
	defer f.close()
	if e = f.lock(t.Context(), false); e != nil {
		t.Fatal(e)
	}
	m := &Maintainer{files: f, trust: c.Trust, now: func() time.Time { return testNow.Add(time.Hour) }}
	f.fault = func(op string, after bool) error {
		if op == c.Operation && after == c.After {
			if e := unix.Kill(os.Getpid(), unix.SIGKILL); e != nil {
				t.Fatal(e)
			}
		}
		return nil
	}
	_, e = m.Install(t.Context(), admitted(t, c.Capsule, c.Trust, m.now()))
	t.Fatal("owned crash boundary not reached", e)
}
func TestActualProcessDeathRecoversMixedPublicationAndTerminalCleanup(t *testing.T) {
	for _, step := range []struct {
		op    string
		after bool
	}{{"exchange:sandbox-maintainer", true}, {"sync-file:old-helper", true}, {"exchange:chrome-sandbox", false}, {"exchange:chrome-sandbox", true}, {"exchange:sandbox-receipt.json", true}, {"unlink:stage:helper.next", true}, {"unlink:root:sandbox-transaction.json", true}} {
		t.Run(step.op+fmt.Sprint(step.after), func(t *testing.T) {
			m, l, c, k := initialMaintainer(t)
			if _, e := m.Recover(t.Context()); e != nil {
				t.Fatal(e)
			}
			old, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
			if e != nil {
				t.Fatal(e)
			}
			defer old.close()
			candidate := changedCandidate(t, c, k, 2)
			config, _ := json.Marshal(crashFixture{m.trust, candidate, step.op, step.after})
			if e = os.WriteFile(filepath.Join(l.anchor, "crash-fixture.json"), config, 0o600); e != nil {
				t.Fatal(e)
			}
			if e = m.Close(); e != nil {
				t.Fatal(e)
			}
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOwnedTransactionCrashProcess$", "--", "owned-transaction-crash")
			command.Env = append(os.Environ(), "OWNED_TEST_ANCHOR="+l.anchor)
			out, e := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(e, &exited) || exited.Success() || !strings.Contains(e.Error(), "killed") {
				t.Fatalf("expected exact owned child SIGKILL: %v %s", e, out)
			}
			f, e := openFixedFiles(l, true)
			if e != nil {
				t.Fatal(e)
			}
			defer f.close()
			if e = f.lock(t.Context(), false); e != nil {
				t.Fatal(e)
			}
			m2 := &Maintainer{files: f, trust: m.trust, now: func() time.Time { return testNow.Add(48 * time.Hour) }}
			r, e := m2.Recover(t.Context())
			if e != nil || r.MaximumGeneration != 2 {
				t.Fatal("process death stranded forward recovery", e)
			}
			var info unix.Stat_t
			if e = unix.Fstat(old.fd, &info); e != nil || info.Mode&0o7777 != 0o755 {
				t.Fatal("held old inode retained privilege", e)
			}
		})
	}
}

func TestRemovalStorageFaultsKeepTombstoneFloorAndOpenInodeRevoked(t *testing.T) {
	m, _, _, _ := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return testNow.Add(48 * time.Hour) }
	operations := map[string]bool{}
	m.files.fault = func(op string, _ bool) error { operations[op] = true; return nil }
	if e := m.Remove(t.Context()); e != nil {
		t.Fatal(e)
	}
	for op := range operations {
		for _, after := range []bool{false, true} {
			for _, fault := range []error{unix.ENOSPC, unix.EDQUOT, unix.EIO} {
				t.Run(op+fmt.Sprint(after)+fault.Error(), func(t *testing.T) {
					manager, l, _, _ := initialMaintainer(t)
					if _, e := manager.Recover(t.Context()); e != nil {
						t.Fatal(e)
					}
					held, e := manager.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
					if e != nil {
						t.Fatal(e)
					}
					defer held.close()
					manager.now = func() time.Time { return testNow.Add(48 * time.Hour) }
					injected := false
					manager.files.fault = func(actual string, did bool) error {
						if actual == op && after == did && !injected {
							injected = true
							return fault
						}
						return nil
					}
					if e = manager.Remove(t.Context()); !injected || !errors.Is(e, fault) {
						t.Fatal("remove fault not reached", e)
					}
					manager2 := reopened(t, manager, l, testNow.Add(49*time.Hour))
					pending, e := manager2.NeedsRecovery()
					if e != nil {
						t.Fatal(e)
					}
					if pending {
						if _, e = manager2.Recover(t.Context()); e != nil {
							t.Fatal(e)
						}
					} else {
						if e = manager2.Remove(t.Context()); e != nil {
							t.Fatal(e)
						}
					}
					r, e := ParseReceipt(mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json")))
					if e != nil || r.State != "removed" || r.MaximumGeneration != 1 || r.AcceptedAt >= r.LastOperationAt {
						t.Fatal("removal lost floor/admission", e)
					}
					var info unix.Stat_t
					if e = unix.Fstat(held.fd, &info); e != nil || info.Mode&0o7777 != 0o755 {
						t.Fatal("removal left open privileged inode", e)
					}
				})
			}
		}
	}
}

func TestNewMaintainerWithoutPriorVerificationKeyHoldsMixedState(t *testing.T) {
	m, l, c, k := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return testNow.Add(time.Hour) }
	candidate := changedCandidate(t, c, k, 2)
	if _, e := m.Install(t.Context(), admitted(t, candidate, m.trust, m.now())); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return testNow.Add(2 * time.Hour) }
	candidate = changedCandidate(t, c, k, 3)
	m.files.fault = func(op string, after bool) error {
		if op == "exchange:sandbox-maintainer" && after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m.Install(t.Context(), admitted(t, candidate, m.trust, m.now())); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	m2 := reopened(t, m, l, testNow.Add(3*time.Hour))
	_, wrong, _ := fixture(t, 4)
	m2.trust.PublicKeys = wrong.PublicKeys
	if _, e := m2.Recover(t.Context()); !errors.Is(e, ErrAuthentication) {
		t.Fatal("missing prior key did not hold", e)
	}
	held, e := m2.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
	if e != nil || held == nil {
		t.Fatal("changed helper before prior receipt verification", e)
	}
	held.close()
	if pending, e := m2.NeedsRecovery(); e == nil || pending {
		t.Fatal("untrusted prior authority represented as ordinary usable state")
	}
}

func TestResidentCannotCreateInitialAuthorityFromMissingReceipt(t *testing.T) {
	f, l := fixtureFiles(t)
	if e := f.lock(t.Context(), false); e != nil {
		t.Fatal(e)
	}
	c, trust, _ := fixture(t, 2)
	trust.Initial = initialFor(t, c)
	m := &Maintainer{files: f, trust: trust, now: func() time.Time { return testNow }}
	if _, e := m.Install(t.Context(), admitted(t, c, trust, testNow)); e == nil {
		t.Fatal("resident invented initial authority")
	}
	if _, e := os.Lstat(filepath.Join(l.root(), "chrome-sandbox")); !os.IsNotExist(e) {
		t.Fatal("created privileged program without root initial anchor", e)
	}
}

func TestInitialPendingCannotLoseReferencedBootstrapStageBeforeCommit(t *testing.T) {
	m, l, _, _ := initialMaintainer(t)
	m.files.fault = func(op string, after bool) error {
		if op == "publish:sandbox-transaction.json" && after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m.Recover(t.Context()); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(l.root(), ".sandbox-staging", "bootstrap-capsule")); e != nil {
		t.Fatal(e)
	}
	m2 := reopened(t, m, l, testNow.Add(48*time.Hour))
	if _, e := m2.Recover(t.Context()); e == nil {
		t.Fatal("precommit missing referenced bootstrap stage accepted")
	}
}
func TestVisibleReceiptIsFlushedBeforeRetiringDurableIntent(t *testing.T) {
	m, l, c, k := initialMaintainer(t)
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return testNow.Add(time.Hour) }
	m.files.fault = func(op string, after bool) error {
		if op == "exchange:sandbox-receipt.json" && after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m.Install(t.Context(), admitted(t, changedCandidate(t, c, k, 2), m.trust, m.now())); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	m2 := reopened(t, m, l, testNow.Add(2*time.Hour))
	m2.files.fault = func(op string, after bool) error {
		if op == "sync:root" && !after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m2.Recover(t.Context()); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	if _, e := os.Lstat(filepath.Join(l.root(), "sandbox-transaction.json")); e != nil {
		t.Fatal("retired authority before receipt directory flush", e)
	}
}

func TestProtectedAdmissionTimeIsMutationClockFloor(t *testing.T) {
	m, l, c, k := initialMaintainer(t)
	m.now = func() time.Time { return testNow.Add(-time.Millisecond) }
	if _, e := m.Recover(t.Context()); !errors.Is(e, ErrTime) {
		t.Fatal("initial admission clock rollback mutated state", e)
	}
	m.now = func() time.Time { return testNow }
	if _, e := m.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return testNow.Add(time.Hour) }
	m.files.fault = func(op string, after bool) error {
		if op == "publish:sandbox-transaction.json" && after {
			return unix.EIO
		}
		return nil
	}
	if _, e := m.Install(t.Context(), admitted(t, changedCandidate(t, c, k, 2), m.trust, m.now())); !errors.Is(e, unix.EIO) {
		t.Fatal(e)
	}
	m2 := reopened(t, m, l, testNow.Add(time.Hour-time.Millisecond))
	if _, e := m2.Recover(t.Context()); !errors.Is(e, ErrTime) {
		t.Fatal("pending admission clock rollback mutated state", e)
	}
	old, e := m2.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
	if e != nil || old == nil {
		t.Fatal("revoked old helper before clock floor", e)
	}
	old.close()
}

func TestRecoveredVisibleComponentsRequirePriorDirectoryFlush(t *testing.T) {
	for _, boundary := range []string{"maintainer", "helper", "remove"} {
		t.Run(boundary, func(t *testing.T) {
			m, l, c, k := initialMaintainer(t)
			if _, e := m.Recover(t.Context()); e != nil {
				t.Fatal(e)
			}
			m.now = func() time.Time { return testNow.Add(time.Hour) }
			held, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o555)
			if e != nil {
				t.Fatal(e)
			}
			defer held.close()
			interrupt := "exchange:sandbox-maintainer"
			if boundary == "helper" {
				interrupt = "exchange:chrome-sandbox"
			}
			if boundary == "remove" {
				interrupt = "unlink:root:chrome-sandbox"
			}
			m.files.fault = func(op string, after bool) error {
				if op == interrupt && after {
					return unix.EIO
				}
				return nil
			}
			if boundary == "remove" {
				e = m.Remove(t.Context())
			} else {
				_, e = m.Install(t.Context(), admitted(t, changedCandidate(t, c, k, 2), m.trust, m.now()))
			}
			if !errors.Is(e, unix.EIO) {
				t.Fatal("rename/unlink interruption not reached", e)
			}
			oldReceipt := mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json"))
			m2 := reopened(t, m, l, testNow.Add(2*time.Hour))
			injected := false
			m2.files.fault = func(op string, after bool) error {
				if op == "sync:root" && !after {
					injected = true
					return unix.EDQUOT
				}
				return nil
			}
			if _, e = m2.Recover(t.Context()); !injected || !errors.Is(e, unix.EDQUOT) {
				t.Fatal("prepublication flush fault not reached", e)
			}
			if current := mustRead(t, filepath.Join(l.root(), "sandbox-receipt.json")); !bytes.Equal(current, oldReceipt) {
				t.Fatal("receipt advanced before recovered component directory flush")
			}
			if boundary == "maintainer" {
				var info unix.Stat_t
				if e = unix.Fstat(held.fd, &info); e != nil || info.Mode&0o7777 != 0o555 {
					t.Fatal("old helper revoked before recovered maintainer was durable", e)
				}
			}
			if _, e = os.Lstat(filepath.Join(l.root(), "sandbox-transaction.json")); e != nil {
				t.Fatal("retired pending authority after failed prior flush", e)
			}

			flushedRoot, flushedStage := false, false
			m2.files.fault = func(op string, after bool) error {
				if after {
					if op == "sync:root" {
						flushedRoot = true
					}
					if op == "sync:stage" {
						flushedStage = true
					}
					return nil
				}
				if boundary == "maintainer" && op == "chmod:old-helper" && (!flushedRoot || !flushedStage) {
					t.Fatal("recovered maintainer not durably published before revocation")
				}
				if op == "write:stage:receipt.next" && (!flushedRoot || (boundary != "remove" && !flushedStage)) {
					t.Fatal("recovered component change not durable before receipt write")
				}
				return nil
			}
			if _, e = m2.Recover(t.Context()); e != nil {
				t.Fatal("recovery did not finish after successful prior flush", e)
			}
		})
	}
}
