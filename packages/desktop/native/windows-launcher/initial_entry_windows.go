//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

// All transaction authority is compiled identity + KnownFolders + this private
// extracted launcher directory. No argument/environment value supplies a path.
func initialInstallerContext(cfg buildConfig) (layout, string, error) {
	l, err := maintenanceLayout(cfg)
	if err != nil {
		return layout{}, "", err
	}
	self, err := os.Executable()
	if err != nil {
		return layout{}, "", err
	}
	if filepath.Base(self) != "Dilla.exe" {
		return layout{}, "", errors.New("invalid initial private launcher identity")
	}
	guard, err := desktoplaunch.PinOwnedFile(self)
	if err != nil {
		return layout{}, "", err
	}
	if err = guard.Close(); err != nil {
		return layout{}, "", err
	}
	return l, filepath.Dir(self), nil
}

func verifyNativeInitialComplete(l layout, cfg buildConfig, payload string) (pkg *initialPackage, err error) {
	pkg, err = loadInitialPackage(payload, cfg)
	if err != nil {
		return nil, err
	}
	disposition, err := classifyInitialInstallation(l, cfg, payload, pkg)
	if err != nil || disposition != initialFinalizeOnly {
		return nil, errors.Join(err, pkg.Close(), errors.New("initial installation is not complete"))
	}
	return pkg, nil
}

func runInitialInstallerMode(cfg buildConfig, mode string) error {
	l, payload, err := initialInstallerContext(cfg)
	if err != nil {
		return err
	}
	if mode == "retry-initial" {
		state, err := executeNativeInitialRepair(l, cfg, payload)
		if err != nil {
			return err
		}
		if state != initialCommitted {
			return errors.New("initial retry commit is not established")
		}
		return nil
	}
	// Mutations own both gates in this process; NSIS releases only its mutexes
	// while retaining the protected package before entering this handoff.
	if mode == "finalize-initial" {
		operation, acquired, gateErr := acquireOperationMutex(cfg)
		if gateErr != nil {
			return gateErr
		}
		if !acquired {
			return errors.New("initial finalization operation is busy")
		}
		defer operation.Close()
	}
	pkg, err := verifyNativeInitialComplete(l, cfg, payload)
	if err != nil {
		return err
	}
	defer func() { _ = pkg.Close() }()
	if mode == "initial-complete" {
		return nil
	}
	if mode != "finalize-initial" {
		return errors.New("invalid initial installer mode")
	}
	return finalizeNativeInitialMarker(l, cfg)
}

func finalizeNativeInitialMarker(l layout, cfg buildConfig) error {
	marker := filepath.Join(l.userData, "initial-install.json")
	raw, err := readMetadata(marker, 4096)
	if err != nil {
		return err
	}
	if err = validateInitialMarkerBytes(raw, cfg); err != nil {
		return err
	}
	return desktoplaunch.FinalizeInitialMarker(marker, raw)
}
