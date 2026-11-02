package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

const initialSeedTemporary = "initial-slot.tmp"

// Only these two fixed names can be initial-copy metadata. This is not accepted
// update history: any other entry, type, alias or enumerator error holds setup.
func initialUpdateNames(l layout) (names map[string]bool, err error) {
	names = map[string]bool{}
	guard, err := desktoplaunch.PinOwnedDirectory(l.updates)
	if os.IsNotExist(err) {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, guard.Close()) }()
	directory, err := os.Open(l.updates) // #nosec G304 -- Fixed compiled-identity updates root and ancestors remain pinned.
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	for {
		entries, readErr := directory.ReadDir(3)
		for _, entry := range entries {
			name := entry.Name()
			if len(names) >= 2 || names[name] || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || (name != "windows-active-0.json" && name != initialSeedTemporary) {
				return nil, errors.New("other history prevents initial retry")
			}
			names[name] = true
		}
		if errors.Is(readErr, io.EOF) {
			return names, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func authorizeInitialStagingMetadata(l layout, expected []byte) error {
	if len(expected) == 0 {
		return errors.New("unprepared initial seed authority")
	}
	names, err := initialUpdateNames(l)
	if err != nil {
		return err
	}
	if names["windows-active-0.json"] && names[initialSeedTemporary] {
		return errors.New("final seed and temporary seed cannot coexist")
	}
	if names[initialSeedTemporary] {
		guard, err := desktoplaunch.PinOwnedFile(filepath.Join(l.updates, initialSeedTemporary))
		if err != nil {
			return err
		}
		err = guard.CompareInitialPrefix(bytes.NewReader(expected))
		return errors.Join(err, guard.Close())
	}
	return nil
}

// This entry is used only for a NEW explicit installer operation. It never
// converts current-attempt publication uncertainty into an automatic repair.
func classifyInitialInstallation(l layout, cfg buildConfig, payload string, pkg *initialPackage) (initialRetryDisposition, error) {
	if pkg == nil || len(pkg.seed) == 0 {
		return initialHold, errors.New("initial package is not prepared")
	}
	if err := incompleteInitial(l, cfg, payload); err != nil {
		return initialHold, err
	}
	names, err := initialUpdateNames(l)
	if err != nil {
		return initialHold, err
	}
	if names["windows-active-0.json"] && names[initialSeedTemporary] {
		return initialHold, errors.New("initial final and temporary seeds coexist")
	}
	var actual []byte
	readErr := os.ErrNotExist
	if names["windows-active-0.json"] {
		actual, readErr = readMetadata(filepath.Join(l.updates, "windows-active-0.json"), 4096)
		if readErr != nil {
			return initialHold, errors.New("observed initial seed cannot be read")
		}
	}
	complete := false
	if readErr == nil && bytes.Equal(actual, pkg.seed) {
		complete = verifyCompleteInitialProgram(l.stableRoot, pkg) == nil
	}
	result := classifyInitialSeed(pkg.seed, actual, readErr, complete, false)
	if result == initialHold {
		return result, errors.New("initial installation is not safely repairable")
	}
	return result, nil
}
