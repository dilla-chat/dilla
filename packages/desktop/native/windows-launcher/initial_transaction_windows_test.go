//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

func initialNativeFixture(t *testing.T) (buildConfig, layout, string) {
	t.Helper()
	cfg, original := initialInstallation(t)
	base := t.TempDir()
	l := layout{stableRoot: filepath.Join(base, "Programs", cfg.ProductName), userData: filepath.Join(base, "Profile", cfg.ProductName)}
	l.updates = filepath.Join(l.userData, "updates")
	if err := os.MkdirAll(filepath.Dir(l.stableRoot), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.userData, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(l.stableRoot, os.DirFS(original.stableRoot)); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(l.updates, os.DirFS(original.updates)); err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err := os.CopyFS(payload, os.DirFS(l.stableRoot)); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{payload, l.stableRoot} {
		for name, raw := range map[string][]byte{"Dilla.exe": []byte("owned launcher bytes"), initialUninstallerName(cfg): []byte("private generated uninstaller")} {
			if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), seed[:12], 0600); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
	if err = os.WriteFile(filepath.Join(l.userData, "initial-install.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(l.userData, "account.sentinel"), []byte("opaque account/KEK/OPFS"), 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, l, payload
}

func TestWindowsInitialDirectoryIdentitySurvivesMoveAndRejectsRecreatedPath(t *testing.T) {
	parent := t.TempDir()
	original, archive := filepath.Join(parent, "original"), filepath.Join(parent, "archive")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := initialOwnedDirectoryInfo(original)
	if err != nil {
		t.Fatal(err)
	}
	var movedInfo os.FileInfo
	var moved bool
	if err = moveInitialDirectory(original, archive, &movedInfo, &moved); err != nil || !moved {
		t.Fatal("owned directory move did not preserve identity", err)
	}
	after, err := initialOwnedDirectoryInfo(archive)
	if err != nil || !os.SameFile(before, after) || !os.SameFile(movedInfo, after) {
		t.Fatal("pre-move snapshot lost archived identity", err)
	}
	if err = os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	replacement, err := initialOwnedDirectoryInfo(original)
	if err != nil || os.SameFile(before, replacement) || os.SameFile(movedInfo, replacement) {
		t.Fatal("recreated old path acquired the archived identity", err)
	}
}

func TestWindowsNativeInitialTransactionCopiesExactPackageAndPreservesAccount(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	state, err := executeNativeInitialRepair(l, cfg, payload)
	if err != nil || state != initialCommitted {
		t.Fatalf("native initial repair: %v %v", state, err)
	}
	pkg, err := loadInitialPackage(payload, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pkg.Close()
	if err = verifyCompleteInitialProgram(l.stableRoot, pkg); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
	if err != nil || !bytes.Equal(actual, pkg.seed) {
		t.Fatal("seed authority mismatch", err)
	}
	if got, e := os.ReadFile(filepath.Join(l.userData, "account.sentinel")); e != nil || string(got) != "opaque account/KEK/OPFS" {
		t.Fatal("account changed", e)
	}
	// A repeated explicit setup must finalize, not move a complete installation.
	before, err := initialOwnedDirectoryInfo(l.stableRoot)
	if err != nil {
		t.Fatal(err)
	}
	state, err = executeNativeInitialRepair(l, cfg, payload)
	if err != nil || state != initialCommitted {
		t.Fatal("finalize-only retry failed", err)
	}
	after, err := initialOwnedDirectoryInfo(l.stableRoot)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("complete authority was requarantined", err)
	}
}

func TestInitialNativeRaceChild(t *testing.T) {
	if os.Getenv("DILLA_TEST_INITIAL_NATIVE_CHILD") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("DILLA_TEST_INITIAL_NATIVE_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

type mappedInitialRetry struct {
	*nativeInitialRetry
	start func() error
}

func (n mappedInitialRetry) quarantineProgram() error {
	if err := n.start(); err != nil {
		return err
	}
	return n.nativeInitialRetry.quarantineProgram()
}

type lateInitialHistory struct {
	*nativeInitialRetry
	injectHistory func() error
}

func (n lateInitialHistory) quarantineUpdates() error {
	if err := n.injectHistory(); err != nil {
		return err
	}
	return n.nativeInitialRetry.quarantineUpdates()
}

func initialFixtureSnapshot(root string) (map[string]string, error) {
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[relative] = "directory"
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[relative] = hash(raw)
		return nil
	})
	return result, err
}

func TestWindowsNativeMappedAfterLastScanRestoresExactRootsWhileChildLives(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	manifest := desktoplaunch.ProgramManifest{Format: 1, Kind: "dilla-desktop-program", Version: cfg.InitialVersion, Channel: cfg.Channel, Platform: "win32", Arch: cfg.Arch, Files: []desktoplaunch.ProgramFile{{Path: "dilla-desktop.exe", Size: int64(len(image)), SHA512: hash(image)}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InitialManifestSHA512 = hash(raw)
	name, _ := desktoplaunch.ManifestName(cfg.InitialManifestSHA512)
	treeID, _ := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
	for _, root := range []string{payload, l.stableRoot} {
		for _, dir := range []string{"versions", "manifests"} {
			if err = os.RemoveAll(filepath.Join(root, dir)); err != nil {
				t.Fatal(err)
			}
		}
		if err = os.MkdirAll(filepath.Join(root, "versions", treeID), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(filepath.Join(root, "manifests"), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "manifests", name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "versions", treeID, "dilla-desktop.exe"), image, 0600); err != nil {
			t.Fatal(err)
		}
	}
	seed, _ := json.Marshal(desktoplaunch.Slot{Format: 1, Kind: "initial", Generation: 0, Version: cfg.InitialVersion, ManifestSHA512: cfg.InitialManifestSHA512})
	if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), seed[:12], 0600); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
	if err = os.WriteFile(filepath.Join(l.userData, "initial-install.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	beforeProgram, err := initialFixtureSnapshot(l.stableRoot)
	if err != nil {
		t.Fatal(err)
	}
	beforeProfile, err := initialFixtureSnapshot(l.userData)
	if err != nil {
		t.Fatal(err)
	}
	operation, acquired, err := acquireOperationMutex(cfg)
	if err != nil || !acquired {
		t.Fatalf("gate: %v %v", acquired, err)
	}
	defer operation.Close()
	retry := &nativeInitialRetry{l: l, cfg: cfg, payload: payload}
	defer func() { _ = retry.pkg.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	var child *exec.Cmd
	var exited chan error
	defer func() {
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
			if exited != nil {
				<-exited
			}
		}
	}()
	start := func() error {
		child = exec.CommandContext(ctx, filepath.Join(l.stableRoot, "versions", treeID, "dilla-desktop.exe"), "-test.run=^TestInitialNativeRaceChild$")
		child.Env = append(os.Environ(), "DILLA_TEST_INITIAL_NATIVE_CHILD=1", "DILLA_TEST_INITIAL_NATIVE_READY="+ready)
		if e := child.Start(); e != nil {
			return e
		}
		exited = make(chan error, 1)
		go func() { exited <- child.Wait() }()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if body, e := os.ReadFile(ready); e == nil && string(body) == "ready" {
				return nil
			}
			select {
			case e := <-exited:
				exited = nil
				return fmt.Errorf("owned race child exited: %w", e)
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("owned race child readiness timeout")
	}
	state, err := runInitialRetryTransaction(mappedInitialRetry{retry, start})
	if err == nil || state != initialPrecommit || !retry.programMoved || retry.programClaim != nil {
		t.Fatalf("mapped race was not refused by post-quarantine lease: %v %v", state, err)
	}
	select {
	case e := <-exited:
		exited = nil
		t.Fatalf("mapped child did not stay live through rollback: %v", e)
	default:
	}
	afterProgram, e := initialFixtureSnapshot(l.stableRoot)
	if e != nil || !reflect.DeepEqual(beforeProgram, afterProgram) {
		t.Fatal("program rollback changed bytes", e)
	}
	afterProfile, e := initialFixtureSnapshot(l.userData)
	if e != nil || !reflect.DeepEqual(beforeProfile, afterProfile) {
		t.Fatal("profile rollback changed bytes", e)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	exited = nil
	if child.ProcessState == nil || !child.ProcessState.Exited() {
		t.Fatal("owned race child exit was not verified")
	}
	child = nil
	operation.Close()
	state, err = executeNativeInitialRepair(l, cfg, payload)
	if err != nil || state != initialCommitted {
		t.Fatalf("retry after verified exit: %v %v", state, err)
	}
}

func TestWindowsNativeLateHistoryInMovedUpdatesIsPreservedBeforeCopy(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	auxiliaryCfg, auxiliary := initialInstallation(t)
	_, higher := committedUpgrade(t, &auxiliaryCfg, auxiliary)
	cfg.PublicKeys = auxiliaryCfg.PublicKeys
	before, err := initialFixtureSnapshot(l.stableRoot)
	if err != nil {
		t.Fatal(err)
	}
	operation, acquired, err := acquireOperationMutex(cfg)
	if err != nil || !acquired {
		t.Fatalf("gate: %v %v", acquired, err)
	}
	defer operation.Close()
	retry := &nativeInitialRetry{l: l, cfg: cfg, payload: payload}
	defer func() { _ = retry.pkg.Close() }()
	publish := func() error {
		nonce := strings.Repeat("a", 64)
		if e := os.CopyFS(filepath.Join(l.updates, "startup-"+nonce), os.DirFS(filepath.Join(auxiliary.updates, "startup-"+nonce))); e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(l.updates, "windows-active-1.json"), higher, 0600)
	}
	state, err := runInitialRetryTransaction(lateInitialHistory{retry, publish})
	if err == nil || state != initialPrecommit || !retry.updatesMoved || retry.programClaim != nil || retry.updatesClaim != nil {
		t.Fatalf("late history was not fenced before copy: %v %v", state, err)
	}
	after, e := initialFixtureSnapshot(l.stableRoot)
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("late history changed original program", e)
	}
	retained, e := os.ReadFile(filepath.Join(l.updates, "windows-active-1.json"))
	if e != nil || !bytes.Equal(retained, higher) {
		t.Fatal("published authority was removed/replaced", e)
	}
	if _, e = desktoplaunch.VerifySlot(retained, cfg.trust(), l.updates, time.Now()); e != nil {
		t.Fatal("restored higher authority no longer authenticates", e)
	}
	if body, e := os.ReadFile(filepath.Join(l.userData, "account.sentinel")); e != nil || string(body) != "opaque account/KEK/OPFS" {
		t.Fatal("account changed", e)
	}
}
