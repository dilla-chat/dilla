package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

type buildConfig struct {
	Format                int      `json:"format"`
	AppID                 string   `json:"appId"`
	ProductName           string   `json:"productName"`
	Channel               string   `json:"channel"`
	Arch                  string   `json:"arch"`
	InitialVersion        string   `json:"initialVersion"`
	InitialManifestSHA512 string   `json:"initialManifestSha512"`
	PublicKeys            []string `json:"publicKeys"`
}
type layout struct{ stableRoot, userData, updates string }
type selection struct {
	slot       *desktoplaunch.Slot
	tree       *desktoplaunch.VerifiedTree
	executable string
}

func (s *selection) Close() error {
	if s == nil || s.tree == nil {
		return nil
	}
	return s.tree.Close()
}
func (cfg buildConfig) trust() desktoplaunch.Trust {
	return desktoplaunch.Trust{PublicKeys: cfg.PublicKeys, Channel: cfg.Channel, Arch: cfg.Arch, InitialVersion: cfg.InitialVersion, InitialManifestSHA512: cfg.InitialManifestSHA512}
}
func readMetadata(path string, limit int64) ([]byte, error) {
	f, e := desktoplaunch.PinOwnedFile(path)
	if e != nil {
		return nil, e
	}
	defer func() { _ = f.Close() }()
	return f.ReadBounded(limit)
}
func selectSlot(raw []byte, l layout, cfg buildConfig, now time.Time) (selected *selection, err error) {
	s, e := desktoplaunch.VerifySlot(raw, cfg.trust(), l.updates, now)
	if e != nil {
		return nil, e
	}
	return verifySelectedProgram(s, l, cfg)
}
func verifySelectedProgram(s *desktoplaunch.Slot, l layout, cfg buildConfig) (*selection, error) {
	name, e := desktoplaunch.ManifestName(s.ManifestSHA512)
	if e != nil {
		return nil, e
	}
	id, e := desktoplaunch.TreeID(s.ManifestSHA512)
	if e != nil {
		return nil, e
	}
	bytes, e := readMetadata(filepath.Join(l.stableRoot, "manifests", name), 1_048_576)
	if e != nil {
		return nil, e
	}
	m, e := desktoplaunch.VerifyManifest(bytes, s.ManifestSHA512, desktoplaunch.ManifestContext{Version: s.Version, Channel: cfg.Channel, Arch: cfg.Arch})
	if e != nil {
		return nil, e
	}
	fixedExecutable := false
	for _, file := range m.Files {
		if file.Path == "dilla-desktop.exe" {
			fixedExecutable = true
			break
		}
	}
	if !fixedExecutable {
		return nil, errors.New("manifest lacks fixed program executable")
	}
	root := filepath.Join(l.stableRoot, "versions", id)
	tree, e := desktoplaunch.VerifyTree(root, m)
	if e != nil {
		return nil, e
	}
	return &selection{slot: s, tree: tree, executable: filepath.Join(root, "dilla-desktop.exe")}, nil
}

// Commit metadata is selected independently of mutable program bytes. Once a
// higher signed generation is authenticated, damage to its program must hold
// startup; the previous version may be incompatible with migrated account data.
func selectCommitted(l layout, cfg buildConfig, now time.Time) (*selection, error) {
	return selectCommittedUsing(l, cfg, now, readMetadata)
}

// The reader seam exercises post-observation I/O failures without weakening the
// production reader's fixed paths, owner/DACL, reparse and bounded-read checks.
func selectCommittedUsing(l layout, cfg buildConfig, now time.Time, read func(string, int64) ([]byte, error)) (*selection, error) {
	slot, err := selectCommittedMetadataUsing(l, cfg, now, read)
	if err != nil {
		return nil, err
	}
	return verifySelectedProgram(slot, l, cfg)
}

// Explicit removal may authenticate terminal history without executing or
// requiring healthy program bytes. Normal launch always verifies its program.
func selectCommittedMetadata(l layout, cfg buildConfig, now time.Time) (*desktoplaunch.Slot, error) {
	return selectCommittedMetadataUsing(l, cfg, now, readMetadata)
}
func selectCommittedMetadataUsing(l layout, cfg buildConfig, now time.Time, read func(string, int64) ([]byte, error)) (*desktoplaunch.Slot, error) {
	var selected *desktoplaunch.Slot
	for _, name := range []string{"windows-active-0.json", "windows-active-1.json"} {
		path := filepath.Join(l.updates, name)
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		// Once a published slot was observed, every read/authentication error is
		// commit uncertainty. Even ENOENT during the later read must hold.
		raw, err := read(path, 1_048_576)
		if err != nil {
			return nil, err
		}
		candidate, err := desktoplaunch.VerifySlot(raw, cfg.trust(), l.updates, now)
		if err != nil {
			return nil, err
		}
		if selected != nil && candidate.Generation == selected.Generation && !reflect.DeepEqual(candidate, selected) {
			return nil, errors.New("conflicting committed slot generation")
		}
		if selected == nil || candidate.Generation > selected.Generation {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, errors.New("no verified committed program slot; uninstall Dilla in Windows Settings and reinstall with the official Dilla installer; your device profile is retained")
	}
	return selected, nil
}
