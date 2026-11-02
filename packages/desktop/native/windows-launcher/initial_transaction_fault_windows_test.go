//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type initialPublicationFault struct {
	*nativeInitialRetry
	fault string
}

func (n initialPublicationFault) publish() (initialPublicationOutcome, error) {
	if n.fault == "before-publication" {
		attempted, err := n.updatesClaim.PublishInitialSeed(nil)
		if attempted {
			return initialPublicationUnknown, errors.New("empty seed unexpectedly attempted publication")
		}
		return initialPublicationNotInvoked, err
	}
	outcome, err := n.nativeInitialRetry.publish()
	if err != nil {
		return outcome, err
	}
	if n.fault == "lost-result" {
		return initialPublicationUnknown, errors.New("test lost native publication result")
	}
	return outcome, nil
}
func (n initialPublicationFault) flush() error {
	if n.fault == "flush-error" {
		return errors.New("test post-publication flush failure")
	}
	return n.nativeInitialRetry.flush()
}

func TestWindowsInitialNativePublicationFailuresPreserveAuthorityAndExplicitRetryPolicy(t *testing.T) {
	for _, fault := range []string{"before-publication", "lost-result", "flush-error"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l, payload := initialNativeFixture(t)
			beforeProgram, err := initialFixtureSnapshot(l.stableRoot)
			if err != nil {
				t.Fatal(err)
			}
			beforeProfile, err := initialFixtureSnapshot(l.userData)
			if err != nil {
				t.Fatal(err)
			}
			retry := &nativeInitialRetry{l: l, cfg: cfg, payload: payload}
			state, err := runInitialRetryTransaction(initialPublicationFault{retry, fault})
			if err == nil {
				_ = retry.pkg.Close()
				t.Fatal("fault was not reached")
			}
			if closeErr := retry.pkg.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if fault == "before-publication" {
				if state != initialPrecommit {
					t.Fatalf("definitive uninvoked+observed absent did not roll back: %v %v", state, err)
				}
				afterProgram, e := initialFixtureSnapshot(l.stableRoot)
				if e != nil || !reflect.DeepEqual(beforeProgram, afterProgram) {
					t.Fatal("precommit program rollback changed bytes", e)
				}
				afterProfile, e := initialFixtureSnapshot(l.userData)
				if e != nil || !reflect.DeepEqual(beforeProfile, afterProfile) {
					t.Fatal("precommit profile rollback changed bytes", e)
				}
			} else {
				if state != initialCommitted {
					t.Fatalf("exact visible seed was rolled back: %v %v", state, err)
				}
				pkg, e := verifyNativeInitialComplete(l, cfg, payload)
				if e != nil {
					t.Fatal("postcommit authority damaged", e)
				}
				_ = pkg.Close()
				before, e := initialOwnedDirectoryInfo(l.stableRoot)
				if e != nil {
					t.Fatal(e)
				}
				// Recovery is a NEW explicit setup; the current attempt never retries.
				next, e := executeNativeInitialRepair(l, cfg, payload)
				if e != nil || next != initialCommitted {
					t.Fatal("explicit finalize-only recovery failed", next, e)
				}
				after, e := initialOwnedDirectoryInfo(l.stableRoot)
				if e != nil || !os.SameFile(before, after) {
					t.Fatal("postcommit recovery requarantined program", e)
				}
			}
		})
	}
}

func TestWindowsInitialNativeFreshSetupUsesSameNoReplaceTransaction(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	if err := os.RemoveAll(l.stableRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(l.updates); err != nil {
		t.Fatal(err)
	}
	state, err := executeNativeInitialRepair(l, cfg, payload)
	if err != nil || state != initialCommitted {
		t.Fatal("fresh native setup", state, err)
	}
	pkg, err := verifyNativeInitialComplete(l, cfg, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = pkg.Close(); err != nil {
		t.Fatal(err)
	}
}

type initialReappearingRoot struct{ *nativeInitialRetry }

func (n initialReappearingRoot) copyAndVerify() error {
	if err := os.Mkdir(n.l.stableRoot, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(n.l.stableRoot, "foreign.sentinel"), []byte("never merge or delete"), 0600); err != nil {
		return err
	}
	return n.nativeInitialRetry.copyAndVerify()
}
func TestWindowsInitialReappearedCanonicalRootPreservesItAndOriginalArchive(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	original, err := initialFixtureSnapshot(l.stableRoot)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := initialFixtureSnapshot(l.userData)
	if err != nil {
		t.Fatal(err)
	}
	retry := &nativeInitialRetry{l: l, cfg: cfg, payload: payload}
	defer func() { _ = retry.pkg.Close() }()
	state, err := runInitialRetryTransaction(initialReappearingRoot{retry})
	if err == nil || state != initialPrecommit || !retry.programMoved || retry.programClaim != nil {
		t.Fatalf("canonical root was adopted: %v %v", state, err)
	}
	retained, e := initialFixtureSnapshot(retry.programArchive)
	if e != nil || !reflect.DeepEqual(original, retained) {
		t.Fatal("original archive not preserved", e)
	}
	canonical, e := initialFixtureSnapshot(l.stableRoot)
	if e != nil || len(canonical) != 2 {
		t.Fatal("foreign canonical root was merged or removed", e)
	}
	if raw, e := os.ReadFile(filepath.Join(l.stableRoot, "foreign.sentinel")); e != nil || string(raw) != "never merge or delete" {
		t.Fatal("foreign bytes changed", e)
	}
	after, e := initialFixtureSnapshot(l.userData)
	if e != nil || !reflect.DeepEqual(profile, after) {
		t.Fatal("profile changed", e)
	}
}
