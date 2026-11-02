package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

type initialRetryDiagnostic struct {
	Format      int    `json:"format"`
	Stage       string `json:"stage"`
	Reason      string `json:"reason"`
	NativeError uint32 `json:"nativeError"`
}

func initialDiagnosticFailure(stage string, err error) initialRetryDiagnostic {
	reason := "other-validation"
	var native syscall.Errno
	_ = errors.As(err, &native)
	switch {
	case errors.Is(err, os.ErrNotExist):
		reason = "missing"
	case native != 0:
		reason = "native-error"
	case strings.HasPrefix(err.Error(), "program file digest mismatch:"):
		reason = "program-digest"
	case strings.HasPrefix(err.Error(), "program file size mismatch:"):
		reason = "program-size"
	case strings.HasPrefix(err.Error(), "unexpected program entry "):
		reason = "program-extra-entry"
	default:
		known := map[string]string{
			"initial marker does not match this package":                     "marker-mismatch",
			"unknown incomplete initial directory":                           "unknown-directory",
			"unknown file in incomplete initial installation":                "unknown-file",
			"initial file exceeds packaged bytes":                            "prefix-size",
			"initial file differs from interrupted package":                  "prefix-content",
			"active or historical update metadata prevents initial retry":    "update-history",
			"program entry has an untrusted owner":                           "untrusted-owner",
			"program entry has a null DACL":                                  "null-dacl",
			"program entry grants mutation rights to an untrusted principal": "untrusted-acl",
			"reparse, linked or wrong-type program entry":                    "linked-or-wrong-type",
			"incomplete program tree":                                        "program-incomplete",
			"invalid program manifest digest or size":                        "manifest-digest",
		}
		if value, ok := known[err.Error()]; ok {
			reason = value
		}
	}
	return initialRetryDiagnostic{1, stage, reason, uint32(native)}
}

// All validation calls below are shared shipping readers. This test-only
// decomposition identifies the first failing boundary without printing errors,
// file names, paths, public configuration, account bytes or cryptographic keys.
func diagnoseInitialRetry(l layout, cfg buildConfig, payload string) initialRetryDiagnostic {
	if err := maintenancePreflight(l, cfg, "initial", func(int64) (bool, error) { return false, nil }, int64(os.Getppid()), ""); err != nil && !errors.Is(err, errInitialExists) {
		return initialDiagnosticFailure("preflight", err)
	}
	if err := validateInitialMarker(l, cfg); err != nil {
		return initialDiagnosticFailure("marker", err)
	}
	name, err := desktoplaunch.ManifestName(cfg.InitialManifestSHA512)
	if err != nil {
		return initialDiagnosticFailure("manifest-identity", err)
	}
	id, err := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
	if err != nil {
		return initialDiagnosticFailure("manifest-identity", err)
	}
	raw, err := readMetadata(filepath.Join(payload, "manifests", name), 1_048_576)
	if err != nil {
		return initialDiagnosticFailure("payload-manifest-read", err)
	}
	manifest, err := desktoplaunch.VerifyManifest(raw, cfg.InitialManifestSHA512, desktoplaunch.ManifestContext{Version: cfg.InitialVersion, Channel: cfg.Channel, Arch: cfg.Arch})
	if err != nil {
		return initialDiagnosticFailure("payload-manifest-verify", err)
	}
	tree, err := desktoplaunch.VerifyTree(filepath.Join(payload, "versions", id), manifest)
	if err != nil {
		return initialDiagnosticFailure("payload-tree", err)
	}
	_ = tree.Close()
	if err = incompleteInitial(l, cfg, payload); err != nil {
		return initialDiagnosticFailure("owned-partial", err)
	}
	return initialRetryDiagnostic{1, "ready", "retry-authorized", 0}
}

func diagnosePayloadACL(cfg buildConfig, payload string) initialRetryDiagnostic {
	for _, item := range []struct{ stage, path string }{{"payload-root", payload}, {"payload-manifest-directory", filepath.Join(payload, "manifests")}} {
		guard, err := desktoplaunch.PinOwnedDirectory(item.path)
		if err != nil {
			return initialDiagnosticFailure(item.stage, err)
		}
		_ = guard.Close()
	}
	name, err := desktoplaunch.ManifestName(cfg.InitialManifestSHA512)
	if err != nil {
		return initialDiagnosticFailure("manifest-identity", err)
	}
	raw, err := readMetadata(filepath.Join(payload, "manifests", name), 1_048_576)
	if err != nil {
		return initialDiagnosticFailure("payload-manifest-read", err)
	}
	manifest, err := desktoplaunch.VerifyManifest(raw, cfg.InitialManifestSHA512, desktoplaunch.ManifestContext{Version: cfg.InitialVersion, Channel: cfg.Channel, Arch: cfg.Arch})
	if err != nil {
		return initialDiagnosticFailure("payload-manifest-verify", err)
	}
	id, _ := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
	tree, err := desktoplaunch.VerifyTree(filepath.Join(payload, "versions", id), manifest)
	if err != nil {
		return initialDiagnosticFailure("payload-tree", err)
	}
	_ = tree.Close()
	return initialRetryDiagnostic{1, "ready", "retry-authorized", 0}
}

func TestPayloadACLDiagnosticUsesUnchangedDirectoryFileAndTreeGuards(t *testing.T) {
	cfg, _, payload := diagnosticFixture(t)
	if result := diagnosePayloadACL(cfg, payload); result.Stage != "ready" {
		t.Fatalf("payload guard stage %s", result.Stage)
	}
	if err := os.RemoveAll(filepath.Join(payload, "manifests")); err != nil {
		t.Fatal(err)
	}
	if result := diagnosePayloadACL(cfg, payload); result.Stage != "payload-manifest-directory" {
		t.Fatalf("payload guard stage %s", result.Stage)
	}
}
func diagnosticFixture(t *testing.T) (buildConfig, layout, string) {
	t.Helper()
	cfg, l := initialInstallation(t)
	payload := t.TempDir()
	if err := os.CopyFS(payload, os.DirFS(l.stableRoot)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(payload, "Dilla.exe"), filepath.Join(l.stableRoot, "Dilla.exe")} {
		if err := os.WriteFile(path, []byte("same exact fixture launcher"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	slot, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), slot, 0600); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
	if err = os.WriteFile(filepath.Join(l.userData, "initial-install.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, l, payload
}
func TestInitialRetryDiagnosticUsesActualValidatorStages(t *testing.T) {
	for _, fault := range []string{"valid", "marker", "payload-tree", "owned-partial"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l, payload := diagnosticFixture(t)
			switch fault {
			case "marker":
				if err := os.Remove(filepath.Join(l.userData, "initial-install.json")); err != nil {
					t.Fatal(err)
				}
			case "payload-tree":
				id, _ := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
				if err := os.WriteFile(filepath.Join(payload, "versions", id, "dilla-desktop.exe"), []byte("wrong!!"), 0600); err != nil {
					t.Fatal(err)
				}
			case "owned-partial":
				if err := os.WriteFile(filepath.Join(l.stableRoot, "unknown.txt"), []byte("retain"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := diagnoseInitialRetry(l, cfg, payload)
			want := fault
			if fault == "valid" {
				want = "ready"
			}
			if got.Stage != want {
				t.Fatalf("stage=%s want=%s", got.Stage, want)
			}
		})
	}
}
func TestInitialRetryDiagnosticNeverPrintsPrivateErrorContent(t *testing.T) {
	for _, err := range []error{errors.New("private-key-and-account-path"), &os.PathError{Op: "private", Path: "private-key-and-account-path", Err: syscall.Errno(32)}} {
		got := initialDiagnosticFailure("owned-partial", err)
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "private") {
			t.Fatal("private diagnostic content leaked")
		}
		if got.Reason != "other-validation" && got.Reason != "native-error" {
			t.Fatalf("unexpected reason enum %s", got.Reason)
		}
	}
}
