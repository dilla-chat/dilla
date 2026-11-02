//go:build windows

package desktoplaunch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsInitialClaimUsesCreateNewAndIdentityDisposition(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "program")
	claim, err := ClaimInitialProgramRoot(root, initialLayoutFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = copyInitialLayoutFixture(claim); err != nil {
		_ = claim.Rollback()
		t.Fatal(err)
	}
	if err = claim.VerifyLayout(); err != nil {
		_ = claim.Rollback()
		t.Fatal(err)
	}
	if other, e := ClaimInitialProgramRoot(root, initialLayoutFixture(t)); e == nil {
		_ = other.Rollback()
		t.Fatal("adopted an existing root")
	}
	if err = claim.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("created root remained after owned disposition", err)
	}
}

func TestWindowsInitialClaimNeverDeletesUnknownChildren(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "program")
	claim, err := ClaimInitialProgramRoot(root, initialLayoutFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "foreign.txt")
	if err = os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		_ = claim.Close()
		t.Fatal(err)
	}
	if err = claim.VerifyLayout(); err == nil {
		_ = claim.Close()
		t.Fatal("accepted unknown child")
	}
	if err = claim.Rollback(); err == nil {
		t.Fatal("claimed deletion of a nonempty foreign namespace")
	}
	if raw, e := os.ReadFile(sentinel); e != nil || string(raw) != "preserve" {
		t.Fatalf("foreign object changed: %q %v", raw, e)
	}
}

func TestWindowsInitialSeedPublicationRetainsExactHandleAndNeverReplaces(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "updates")
	claim, err := ClaimInitialUpdateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"format":1,"kind":"initial"}`)
	attempted, err := claim.PublishInitialSeed(seed)
	if err != nil || !attempted {
		_ = claim.Close()
		t.Fatalf("publish: %v %v", attempted, err)
	}
	exact, absent, err := claim.ObserveInitialSeed(seed)
	if err != nil || !exact || absent {
		_ = claim.Close()
		t.Fatalf("observe held seed: %v %v %v", exact, absent, err)
	}
	if err = claim.Close(); err != nil {
		t.Fatal(err)
	}
	if got, e := os.ReadFile(filepath.Join(root, "windows-active-0.json")); e != nil || !bytes.Equal(got, seed) {
		t.Fatal("published bytes changed", e)
	}
	// A second root claim cannot adopt/replace committed authority.
	if other, e := ClaimInitialUpdateRoot(root); e == nil {
		_ = other.Rollback()
		t.Fatal("adopted committed update root")
	}
}

func TestWindowsInitialLayoutRejectsMissingRenamedOrReplacedTrackedChild(t *testing.T) {
	for _, fault := range []string{"missing", "renamed", "replaced"} {
		t.Run(fault, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "program")
			claim, err := ClaimInitialProgramRoot(root, initialLayoutFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			holding, err := ClaimInitialProgramRoot(filepath.Join(parent, "holding"), initialLayoutFixture(t))
			if err != nil {
				_ = claim.Rollback()
				t.Fatal(err)
			}
			defer func() { _ = claim.Rollback(); _ = holding.Rollback() }()
			if err = copyInitialLayoutFixture(claim); err != nil {
				t.Fatal(err)
			}
			if err = claim.VerifyLayout(); err != nil {
				t.Fatal(err)
			}
			destination := holding.entries["."].file
			if fault == "renamed" {
				destination = claim.entries["versions/"+claim.layout.treeID].file
			}
			if err = renameInitialClaim(claim.entries["versions/"+claim.layout.treeID+"/image.exe"].file, destination, "moved.exe"); err != nil {
				t.Fatal("controlled held-child namespace mutation did not execute", err)
			}
			if fault == "replaced" {
				if err = os.WriteFile(filepath.Join(root, "versions", claim.layout.treeID, "image.exe"), []byte("foreign replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = claim.VerifyLayout(); err == nil {
				t.Fatal("accepted missing/renamed/replaced tracked identity")
			}
			if fault == "replaced" {
				if err = claim.Rollback(); err == nil {
					t.Fatal("deleted a foreign replacement")
				}
				if got, e := os.ReadFile(filepath.Join(root, "versions", claim.layout.treeID, "image.exe")); e != nil || string(got) != "foreign replacement" {
					t.Fatal("foreign replacement changed", e)
				}
			}
		})
	}
}

func TestWindowsInitialLayoutRestartsEnumerationOnEveryCheck(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program")
	claim, err := ClaimInitialUpdateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Close() }()
	if err = claim.VerifyLayout(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "foreign"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = claim.VerifyLayout(); err == nil {
		t.Fatal("exhausted enumeration hid a new foreign child")
	}
}

func copyInitialLayoutFixture(claim *InitialRootClaim) error {
	raw := manifestBytes([]map[string]any{fileEntry("image.exe", "program"), fileEntry("resources/"+strings.Repeat("a", 90)+"/"+strings.Repeat("b", 90)+"/image.exe", "nested")})
	sources := map[string][]byte{digest([]byte("go!")): []byte("go!"), digest([]byte("unins")): []byte("unins"), digest(raw): raw, digest([]byte("program")): []byte("program"), digest([]byte("nested")): []byte("nested")}
	for relative, file := range claim.layout.files {
		if err := claim.CopyVerifiedFile(relative, bytes.NewReader(sources[file.SHA512])); err != nil {
			return err
		}
	}
	return nil
}

func TestWindowsInitialSeedObservationBeforePreparationIsPositiveEvidence(t *testing.T) {
	claim, err := ClaimInitialUpdateRoot(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Rollback() }()
	exact, absent, err := claim.ObserveInitialSeed([]byte("expected seed"))
	if err != nil || exact || !absent {
		t.Fatalf("unprepared absent observation: %v %v %v", exact, absent, err)
	}
	if err = os.WriteFile(filepath.Join(claim.entries["."].file.Name(), "windows-active-0.json"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	exact, absent, err = claim.ObserveInitialSeed([]byte("expected seed"))
	if err == nil || exact || absent {
		t.Fatalf("unheld final must remain unknown: %v %v %v", exact, absent, err)
	}
}

func TestWindowsInitialSeedFailedPublishKeepsOnlyFixedAuthenticatedTemporary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "updates")
	claim, err := ClaimInitialUpdateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Rollback() }()
	final := filepath.Join(root, "windows-active-0.json")
	if err = os.WriteFile(final, []byte("preexisting authority"), 0600); err != nil {
		t.Fatal(err)
	}
	attempted, err := claim.PublishInitialSeed([]byte("prepared package seed"))
	if !attempted || err == nil {
		t.Fatalf("no-replace publication: %v %v", attempted, err)
	}
	if err = claim.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unexpected publication residue count: %d", len(entries))
	}
	temporary, err := os.ReadFile(filepath.Join(root, "initial-slot.tmp"))
	if err != nil || string(temporary) != "prepared package seed" {
		t.Fatal("fixed same-package residue missing", err)
	}
	actual, err := os.ReadFile(final)
	if err != nil || string(actual) != "preexisting authority" {
		t.Fatal("replaced existing authority", err)
	}
}
