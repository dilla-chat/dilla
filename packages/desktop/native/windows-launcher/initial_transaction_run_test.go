package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type initialTransactionFixture struct {
	root, program, updates, archive, oldUpdates string
	fault                                       string
	leased, created, visible                    bool
}

func (f *initialTransactionFixture) prepare() (initialRetryDisposition, error) {
	return initialRepair, nil
}
func (f *initialTransactionFixture) quarantineProgram() error { return os.Rename(f.program, f.archive) }
func (f *initialTransactionFixture) leaseArchive() error {
	if f.fault == "lease" {
		return errors.New("mapping conflict")
	}
	f.leased = true
	return nil
}
func (f *initialTransactionFixture) recheck() error {
	if f.fault == "late history" {
		return errors.New("new active history")
	}
	return nil
}
func (f *initialTransactionFixture) quarantineUpdates() error {
	return os.Rename(f.updates, f.oldUpdates)
}
func (f *initialTransactionFixture) copyAndVerify() error {
	if f.fault == "reappeared root" {
		if err := os.Mkdir(f.program, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(f.program, "foreign"), []byte("preserve"), 0600); err != nil {
			return err
		}
		return errors.New("canonical root reappeared")
	}
	if err := os.Mkdir(f.program, 0700); err != nil {
		return err
	}
	f.created = true
	if err := os.Mkdir(f.updates, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(f.program, "program"), []byte("new authenticated bytes"), 0600); err != nil {
		return err
	}
	if f.fault == "copy" {
		return errors.New("injected copy failure")
	}
	return nil
}
func (f *initialTransactionFixture) publish() (initialPublicationOutcome, error) {
	if f.fault == "before publish" || f.fault == "observation uncertain" {
		return initialPublicationNotInvoked, errors.New("preparation failed")
	}
	if f.fault == "publication uncertain" {
		return initialPublicationUnknown, errors.New("result unknown")
	}
	if err := os.WriteFile(filepath.Join(f.updates, "seed"), []byte("exact seed"), 0600); err != nil {
		return initialPublicationUnknown, err
	}
	f.visible = true
	if f.fault == "lost result" {
		return initialPublicationUnknown, errors.New("lost result")
	}
	return initialPublicationCompleted, nil
}
func (f *initialTransactionFixture) observe() (initialSeedObservation, error) {
	if f.fault == "observation uncertain" {
		return initialSeedAbsent, errors.New("observation failed")
	}
	if f.visible {
		return initialSeedExact, nil
	}
	return initialSeedAbsent, nil
}
func (f *initialTransactionFixture) flush() error {
	if f.fault == "flush" {
		return errors.New("flush failed")
	}
	return nil
}
func (f *initialTransactionFixture) closeCreated() error {
	if f.fault == "close" {
		return errors.New("close failed")
	}
	return nil
}
func (f *initialTransactionFixture) rollbackCreated() error {
	if !f.created {
		return nil
	}
	return errors.Join(os.RemoveAll(f.program), os.RemoveAll(f.updates))
}
func (f *initialTransactionFixture) closeArchive() error { f.leased = false; return nil }
func (f *initialTransactionFixture) restoreArchives() error {
	if f.leased {
		return errors.New("rollback before archive guards closed")
	}
	var result error
	for _, pair := range [][2]string{{f.oldUpdates, f.updates}, {f.archive, f.program}} {
		if _, err := os.Stat(pair[0]); os.IsNotExist(err) {
			continue
		}
		if _, err := os.Lstat(pair[1]); !os.IsNotExist(err) {
			result = errors.Join(result, errors.New("unexpected root; preserve archive"))
			continue
		}
		result = errors.Join(result, os.Rename(pair[0], pair[1]))
	}
	return result
}

func TestInitialTransactionFaultsPreserveAuthorityAndRestorePrecommit(t *testing.T) {
	for _, fault := range []string{"lease", "late history", "reappeared root", "copy", "before publish", "publication uncertain", "observation uncertain", "lost result", "flush", "close", "success"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			f := &initialTransactionFixture{root: root, program: filepath.Join(root, "program"), updates: filepath.Join(root, "updates"), archive: filepath.Join(root, "old-program"), oldUpdates: filepath.Join(root, "old-updates"), fault: fault}
			for _, path := range []string{f.program, f.updates} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			old := []byte("old prefix bytes")
			if err := os.WriteFile(filepath.Join(f.program, "program"), old, 0600); err != nil {
				t.Fatal(err)
			}
			account := filepath.Join(root, "account")
			if err := os.WriteFile(account, []byte("opaque account"), 0600); err != nil {
				t.Fatal(err)
			}
			state, err := runInitialRetryTransaction(f)
			precommit := fault == "lease" || fault == "late history" || fault == "copy" || fault == "before publish"
			if (err == nil) != (fault == "success") {
				t.Fatalf("unexpected outcome: %v %v", state, err)
			}
			if fault == "reappeared root" {
				if raw, e := os.ReadFile(filepath.Join(f.program, "foreign")); e != nil || string(raw) != "preserve" {
					t.Fatal("recreated root changed", e)
				}
				if raw, e := os.ReadFile(filepath.Join(f.archive, "program")); e != nil || !bytes.Equal(raw, old) {
					t.Fatal("quarantined source lost", e)
				}
				if _, e := os.Stat(f.updates); e != nil {
					t.Fatal("metadata did not restore", e)
				}
			} else if precommit {
				raw, e := os.ReadFile(filepath.Join(f.program, "program"))
				if e != nil || !bytes.Equal(raw, old) {
					t.Fatalf("source not restored: %q %v", raw, e)
				}
				if _, e = os.Stat(f.updates); e != nil {
					t.Fatal("updates not restored", e)
				}
			} else {
				raw, e := os.ReadFile(filepath.Join(f.program, "program"))
				if e != nil || string(raw) != "new authenticated bytes" {
					t.Fatal("new state lost", e)
				}
				if _, e = os.Stat(f.archive); e != nil {
					t.Fatal("original archive removed", e)
				}
				if fault == "publication uncertain" || fault == "observation uncertain" {
					if state != initialCommitUncertain {
						t.Fatal("uncertain state was not preserved")
					}
				} else if state != initialCommitted {
					t.Fatal("visible seed lost authority")
				}
			}
			if raw, e := os.ReadFile(account); e != nil || string(raw) != "opaque account" {
				t.Fatal("account changed")
			}
		})
	}
}

type finalizedInitialFixture struct{ *initialTransactionFixture }

func (f finalizedInitialFixture) prepare() (initialRetryDisposition, error) {
	return initialFinalizeOnly, nil
}
func TestInitialTransactionFinalizeOnlyDoesNotQuarantineOrCopy(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "program")
	if err := os.Mkdir(program, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(program, "sentinel")
	if err := os.WriteFile(path, []byte("complete authority"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := finalizedInitialFixture{&initialTransactionFixture{program: program}}
	state, err := runInitialRetryTransaction(fixture)
	if err != nil || state != initialCommitted {
		t.Fatalf("finalize: %v %v", state, err)
	}
	if raw, e := os.ReadFile(path); e != nil || string(raw) != "complete authority" {
		t.Fatal("finalization rewrote program", e)
	}
}
