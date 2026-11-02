package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

var errInitialExists = errors.New("installation or active history already exists")

// The marker is atomically committed before any program/update-root creation.
// A matching marker alone is insufficient: only exact same-package byte prefixes
// and its initial generation-zero slot can be quarantined for a fresh retry.
func incompleteInitial(l layout, cfg buildConfig, payload string) error {
	if err := validateInitialMarker(l, cfg); err != nil {
		return err
	}
	digest := cfg.InitialManifestSHA512
	name, err := desktoplaunch.ManifestName(digest)
	if err != nil {
		return err
	}
	id, err := desktoplaunch.TreeID(digest)
	if err != nil {
		return err
	}
	manifestRaw, err := readMetadata(filepath.Join(payload, "manifests", name), 1_048_576)
	if err != nil {
		return err
	}
	manifest, err := desktoplaunch.VerifyManifest(manifestRaw, digest, desktoplaunch.ManifestContext{Version: cfg.InitialVersion, Channel: cfg.Channel, Arch: cfg.Arch})
	if err != nil {
		return err
	}
	tree, err := desktoplaunch.VerifyTree(filepath.Join(payload, "versions", id), manifest)
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	// A stable root can contain only files this exact interrupted installer owns.
	files := map[string]string{"Dilla.exe": filepath.Join(payload, "Dilla.exe"), "manifests/" + name: filepath.Join(payload, "manifests", name)}
	directories := map[string]bool{".": true, "versions": true, "versions/" + id: true, "manifests": true}
	for _, file := range manifest.Files {
		relative := "versions/" + id + "/" + file.Path
		files[relative] = filepath.Join(payload, filepath.FromSlash(relative))
		for directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative))); directory != "."; directory = filepath.ToSlash(filepath.Dir(filepath.FromSlash(directory))) {
			directories[directory] = true
		}
	}
	// Read bounded batches while each ancestor stays pinned. WalkDir sorts the
	// entire directory before its callback, which is unbounded for unknown trees.
	remaining := len(files) + len(directories) + 1 // Includes the fixed old uninstaller.
	var visit func(string, string, int) error
	visit = func(path, relative string, depth int) (visitErr error) {
		if depth > 128 || remaining < 1 {
			return errors.New("incomplete initial traversal exceeds package bounds")
		}
		remaining--
		if !directories[relative] {
			if relative == initialUninstallerName(cfg) {
				guard, e := desktoplaunch.PinOwnedFile(path)
				if e != nil {
					return e
				}
				return guard.Close()
			}
			source, found := files[relative]
			if !found {
				return errors.New("unknown file in incomplete initial installation")
			}
			return samePackagePrefix(source, path)
		}
		guard, e := desktoplaunch.PinOwnedDirectory(path)
		if e != nil {
			return e
		}
		defer func() { visitErr = errors.Join(visitErr, guard.Close()) }()
		directory, e := os.Open(path) // #nosec G304 -- Fixed root/authenticated child allowlist stays pinned.
		if e != nil {
			return e
		}
		defer func() { visitErr = errors.Join(visitErr, directory.Close()) }()
		seen := map[string]bool{}
		for {
			entries, readErr := directory.ReadDir(64)
			for _, entry := range entries {
				child := entry.Name()
				if relative != "." {
					child = relative + "/" + child
				}
				if seen[child] || entry.Type()&os.ModeSymlink != 0 || directories[child] != entry.IsDir() {
					return errors.New("unknown or linked incomplete initial entry")
				}
				seen[child] = true
				if e = visit(filepath.Join(path, entry.Name()), child, depth+1); e != nil {
					return e
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if _, err = os.Lstat(l.stableRoot); err == nil {
		if err = visit(l.stableRoot, ".", 0); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	slot, err := readMetadata(filepath.Join(payload, "initial-slot.json"), 4096)
	if err != nil {
		return err
	}
	if _, err = desktoplaunch.VerifySlot(slot, cfg.trust(), "", time.Now()); err != nil {
		return err
	}

	if err = authorizeInitialStagingMetadata(l, slot); err != nil {
		return err
	}
	names, err := initialUpdateNames(l)
	if err != nil {
		return err
	}
	if names["windows-active-0.json"] {
		return samePackagePrefix(filepath.Join(payload, "initial-slot.json"), filepath.Join(l.updates, "windows-active-0.json"))
	}
	return nil
}
func samePackagePrefix(source, destination string) error {
	guard, err := desktoplaunch.PinOwnedFile(destination)
	if err != nil {
		return err
	}
	defer func() { _ = guard.Close() }()
	expected, err := os.Open(source) // #nosec G304 -- Source is a fixed packaged file or an authenticated manifest allowlist entry.
	if err != nil {
		return err
	}
	defer func() { _ = expected.Close() }()
	return guard.CompareInitialPrefix(expected)
}

func validateInitialMarker(l layout, cfg buildConfig) error {
	raw, err := readMetadata(filepath.Join(l.userData, "initial-install.json"), 4096)
	if err != nil {
		return err
	}
	return validateInitialMarkerBytes(raw, cfg)
}
func validateInitialMarkerBytes(raw []byte, cfg buildConfig) error {
	fields, err := maintenanceJSON(raw, []string{"format", "kind", "appId", "manifestSha512"})
	if err != nil {
		return err
	}
	var format int
	var kind, app, digest string
	if json.Unmarshal(fields["format"], &format) != nil || json.Unmarshal(fields["kind"], &kind) != nil || json.Unmarshal(fields["appId"], &app) != nil || json.Unmarshal(fields["manifestSha512"], &digest) != nil || format != 1 || kind != "dilla-initial-install" || app != cfg.AppID || digest != cfg.InitialManifestSHA512 {
		return errors.New("initial marker does not match this package")
	}
	return nil
}
