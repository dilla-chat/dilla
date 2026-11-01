package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

// Only the native adapter derives this layout from compiled identity and Windows
// known folders. No launcher argument selects any path. Preflight is read-only.
// NSIS holds the shared operation mutex across this check and its mutations.
// Owner/DACL checks trust the current user, Administrators and SYSTEM; this does
// not claim exclusion against a malicious process running as those principals.
func maintenancePreflight(l layout, cfg buildConfig, mode string, alive func(int64) (bool, error), installerPID int64, packageManifest string) error {
	if mode != "initial" && mode != "stage" && mode != "uninstall" {
		return errors.New("unknown maintenance preflight")
	}
	var guards []*desktoplaunch.DirectoryGuard
	defer func() {
		for _, g := range guards {
			_ = g.Close()
		}
	}()
	for _, root := range []string{l.stableRoot, l.userData, l.updates} {
		directory := root
		for {
			_, err := os.Lstat(directory)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) {
				return err
			}
			parent := filepath.Dir(directory)
			if parent == directory {
				return errors.New("missing known-folder ancestor")
			}
			directory = parent
		}
		guard, err := desktoplaunch.PinOwnedDirectory(directory)
		if err != nil {
			return err
		}
		guards = append(guards, guard)
	}
	if mode == "initial" {
		for _, path := range []string{l.stableRoot, l.updates} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				return errInitialExists
			}
		}
		return nil
	}
	// Removal is not authorization to execute a program. With no active
	// transaction, fixed root/entry ownership and kernel image guards authorize
	// explicit uninstall even if its only manifest, PE or ASAR is damaged.
	nonce, err := activeNonce(l)
	if err != nil {
		return err
	}
	if mode == "uninstall" {
		if nonce == "" {
			return nil
		}
		return uninstallTerminalMetadata(l, cfg, nonce, alive)
	}
	current, err := selectCommitted(l, cfg, time.Now())
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()
	pending, err := loadPending(l, cfg, time.Now())
	if err != nil {
		return err
	}
	if pending == nil {
		if mode == "stage" {
			return errors.New("no registered installation transaction")
		}
		return nil
	}
	defer func() { _ = pending.old.Close() }()
	journal := pending.journal
	if mode == "stage" {
		if journal.Phase != "installing" || journal.InstallerPID == nil || *journal.InstallerPID != installerPID {
			return errors.New("staging is not the registered installer")
		}
		_, err := stagingRelease(l, cfg, pending, current.slot.Version, packageManifest)
		return err
	}
	return errors.New("unknown installation maintenance mode")
}

// A terminal journal and independently authenticated committed/previous slot
// metadata authorize explicit removal. Their program bytes are not executed and
// may be corrupt or absent. Pending transactions still require recovery first.
func uninstallTerminalMetadata(l layout, cfg buildConfig, nonce string, alive func(int64) (bool, error)) error {
	journal, err := readJournal(l, nonce)
	if err != nil {
		return err
	}
	if journal.Phase != "complete" && journal.Phase != "failed" {
		return errors.New("update remains unresolved")
	}
	current, err := selectCommittedMetadata(l, cfg, time.Now())
	if err != nil {
		return err
	}
	raw, err := readMetadata(filepath.Join(l.updates, "startup-"+nonce, "old-slot.json"), 1_048_576)
	if err != nil {
		return err
	}
	old, err := desktoplaunch.VerifySlot(raw, cfg.trust(), l.updates, time.Now())
	if err != nil {
		return err
	}
	if journal.Phase == "complete" {
		if current.Kind != "release" || current.Nonce != nonce || current.Version != journal.Version || current.Generation != old.Generation+1 {
			return errors.New("completed update does not match committed metadata")
		}
	} else if !reflect.DeepEqual(current, old) {
		return errors.New("failed update does not retain previous committed metadata")
	}
	for _, pid := range []*int64{journal.ChildPID, journal.WatchdogPID, journal.InstallerActorPID, journal.InstallerPID} {
		if pid == nil {
			continue
		}
		running, err := alive(*pid)
		if err != nil {
			return err
		}
		if running {
			return errors.New("update process has not exited")
		}
	}
	ready, err := readMetadata(filepath.Join(l.updates, "startup-"+nonce, "watchdog-ready.json"), 4096)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		fields, err := maintenanceJSON(ready, []string{"nonce", "version", "pid"})
		if err != nil {
			return err
		}
		var nonce, version string
		var pid int64
		if json.Unmarshal(fields["nonce"], &nonce) != nil || json.Unmarshal(fields["version"], &version) != nil || json.Unmarshal(fields["pid"], &pid) != nil || nonce != journal.Nonce || version != old.Version || pid <= 1 || pid > 0xffffffff {
			return errors.New("invalid private readiness identity")
		}
		running, err := alive(pid)
		if err != nil {
			return err
		}
		if running {
			return errors.New("owned ready helper remains alive")
		}
	}
	return nil
}

// Reject duplicate top-level keys before reading bounded private status fields.
// Plan paths, keys, commands and roots are never taken from this record.
func maintenanceJSON(raw []byte, required []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid maintenance record")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("invalid maintenance key")
		}
		if _, found := fields[name]; found {
			return nil, errors.New("duplicate maintenance key")
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing maintenance JSON")
	}
	if required != nil {
		if len(fields) != len(required) {
			return nil, errors.New("unexpected maintenance fields")
		}
		for _, key := range required {
			if _, found := fields[key]; !found {
				return nil, errors.New("missing maintenance field")
			}
		}
	}
	return fields, nil
}
func stagingRelease(l layout, cfg buildConfig, pending *pendingUpdate, currentVersion, packageManifest string) (*desktoplaunch.Artifact, error) {
	directory := filepath.Join(l.updates, "startup-"+pending.nonce)
	plan, err := readMetadata(filepath.Join(directory, "plan.json"), 1_048_576)
	if err != nil {
		return nil, err
	}
	fields, err := maintenanceJSON(plan, nil)
	if err != nil {
		return nil, err
	}
	var accepted int64
	if json.Unmarshal(fields["verifiedAt"], &accepted) != nil || accepted < 1 || accepted > time.Now().Add(5*time.Minute).UnixMilli() {
		return nil, errors.New("invalid recorded acceptance time")
	}
	raw, err := readMetadata(filepath.Join(directory, "release.json"), 1_048_576)
	if err != nil {
		return nil, err
	}
	signature, err := readMetadata(filepath.Join(directory, "release.json.sig"), 1024)
	if err != nil {
		return nil, err
	}
	release, err := desktoplaunch.VerifyRelease(raw, strings.TrimSpace(string(signature)), desktoplaunch.ReleaseContext{PublicKeys: cfg.PublicKeys, Channel: cfg.Channel, Platform: "win32", Arch: cfg.Arch, CurrentVersion: currentVersion, Now: time.UnixMilli(accepted)})
	if err != nil {
		return nil, err
	}
	for _, artifact := range release.Artifacts {
		if artifact.Platform == "win32" && artifact.Arch == cfg.Arch && artifact.ProgramManifest != nil && artifact.ProgramManifest.SHA512 == packageManifest {
			return &artifact, nil
		}
	}
	return nil, errors.New("staged package does not match authenticated active transaction")
}
