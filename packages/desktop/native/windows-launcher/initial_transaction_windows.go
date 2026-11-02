//go:build windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

type nativeInitialRetry struct {
	l                                layout
	cfg                              buildConfig
	payload                          string
	pkg                              *initialPackage
	programArchive, updatesArchive   string
	programOriginal, updatesOriginal os.FileInfo
	programMoved, updatesMoved       bool
	programLease, updatesLease       *desktoplaunch.InitialRetryTree
	programClaim, updatesClaim       *desktoplaunch.InitialRootClaim
}

// The public adapter supplies KnownFolders and implicit executable directory;
// tests supply isolated owned fixtures. No CLI argument selects these roots.
func executeNativeInitialRepair(l layout, cfg buildConfig, payload string) (state initialCommitState, err error) {
	operation, acquired, err := acquireOperationMutex(cfg)
	if err != nil {
		return initialPrecommit, err
	}
	if !acquired {
		return initialPrecommit, errors.New("initial installation operation is busy")
	}
	defer operation.Close()
	retry := &nativeInitialRetry{l: l, cfg: cfg, payload: payload}
	defer func() { err = errors.Join(err, retry.pkg.Close()) }()
	return runInitialRetryTransaction(retry)
}

func (n *nativeInitialRetry) prepare() (initialRetryDisposition, error) {
	var err error
	n.pkg, err = loadInitialPackage(n.payload, n.cfg)
	if err != nil {
		return initialHold, err
	}
	disposition, err := classifyInitialInstallation(n.l, n.cfg, n.payload, n.pkg)
	if err != nil || disposition != initialRepair {
		return disposition, err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return initialHold, err
	}
	nonce := hex.EncodeToString(random[:])
	n.programArchive = filepath.Join(filepath.Dir(n.l.stableRoot), filepath.Base(n.l.stableRoot)+"-incomplete-"+nonce)
	n.updatesArchive = filepath.Join(n.l.userData, "updates-incomplete-"+nonce)
	return disposition, nil
}

func initialOwnedDirectoryInfo(path string) (info os.FileInfo, err error) {
	guard, err := desktoplaunch.PinOwnedDirectory(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, guard.Close()) }()
	return os.Stat(path) // #nosec G304 -- Fixed KnownFolder root/archive is pinned against replacement.
}

func moveInitialDirectory(from, to string, original *os.FileInfo, moved *bool) error {
	info, err := initialOwnedDirectoryInfo(from)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	*original = info
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	destination, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	moveErr := windows.MoveFileEx(source, destination, windows.MOVEFILE_WRITE_THROUGH)
	actual, observeErr := initialOwnedDirectoryInfo(to)
	if observeErr == nil && os.SameFile(info, actual) {
		*moved = true
	}
	if moveErr != nil {
		return moveErr
	}
	if observeErr != nil {
		return observeErr
	}
	if !*moved {
		return errors.New("initial quarantine identity mismatch")
	}
	return nil
}

func (n *nativeInitialRetry) quarantineProgram() error {
	return moveInitialDirectory(n.l.stableRoot, n.programArchive, &n.programOriginal, &n.programMoved)
}

func (n *nativeInitialRetry) leaseArchive() error {
	if !n.programMoved {
		return nil
	}
	var err error
	n.programLease, err = desktoplaunch.PinInitialRetryTree(n.programArchive, n.pkg.entries)
	if err != nil {
		return err
	}
	if err = n.programLease.VerifyRoot(n.programOriginal); err != nil {
		return err
	}
	for relative, source := range n.pkg.files {
		if relative == initialUninstallerName(n.cfg) || !n.programLease.HasFile(relative) {
			continue
		}
		file, err := os.Open(source.path) // #nosec G304 -- Exact package allowlist source remains authenticated and pinned.
		if err != nil {
			return err
		}
		compareErr := n.programLease.ComparePrefix(relative, file)
		closeErr := file.Close()
		if err = errors.Join(compareErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func (n *nativeInitialRetry) recheck() error {
	if err := validateInitialMarker(n.l, n.cfg); err != nil {
		return err
	}
	if _, err := os.Lstat(n.l.stableRoot); !os.IsNotExist(err) {
		return errors.New("canonical program root reappeared")
	}
	return n.checkMetadata(n.l)
}

func (n *nativeInitialRetry) checkMetadata(l layout) error {
	if err := authorizeInitialStagingMetadata(l, n.pkg.seed); err != nil {
		return err
	}
	names, err := initialUpdateNames(l)
	if err != nil {
		return err
	}
	if names["windows-active-0.json"] {
		guard, err := desktoplaunch.PinOwnedFile(filepath.Join(l.updates, "windows-active-0.json"))
		if err != nil {
			return err
		}
		actual, readErr := guard.ReadBounded(4096)
		closeErr := guard.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if classifyInitialSeed(n.pkg.seed, actual, nil, false, false) != initialRepair {
			return errors.New("initial final seed changed after repair classification")
		}
		return nil
	}
	return nil
}

func (n *nativeInitialRetry) quarantineUpdates() error {
	if err := moveInitialDirectory(n.l.updates, n.updatesArchive, &n.updatesOriginal, &n.updatesMoved); err != nil {
		return err
	}
	if !n.updatesMoved {
		return nil
	}
	archived := n.l
	archived.updates = n.updatesArchive
	// A legacy Local-only actor in another session can publish after recheck.
	// Validate the moved namespace before claiming/copying any canonical bytes.
	if err := n.checkMetadata(archived); err != nil {
		return err
	}
	var err error
	n.updatesLease, err = desktoplaunch.PinInitialRetryTree(n.updatesArchive, map[string]bool{".": true, "windows-active-0.json": false, initialSeedTemporary: false})
	if err != nil {
		return err
	}
	if err = n.updatesLease.VerifyRoot(n.updatesOriginal); err != nil {
		return err
	}
	return validateRetainedInitialMetadata(n.updatesLease, n.pkg.seed)
}

func validateRetainedInitialMetadata(lease *desktoplaunch.InitialRetryTree, expected []byte) error {
	final, temporary := lease.HasFile("windows-active-0.json"), lease.HasFile(initialSeedTemporary)
	if final && temporary {
		return errors.New("leased final and temporary seeds cannot coexist")
	}
	if temporary {
		if err := lease.ComparePrefix(initialSeedTemporary, bytes.NewReader(expected)); err != nil {
			return err
		}
	}
	if final {
		actual, err := lease.ReadInitialSeed()
		if err != nil {
			return err
		}
		// This attempt already chose Repair. An exact authority published later must
		// HOLD, never turn a committed/damaged program back into prefix repair.
		if classifyInitialSeed(expected, actual, nil, false, false) != initialRepair {
			return errors.New("leased final seed changed after repair classification")
		}
	}
	return nil
}

func (n *nativeInitialRetry) copyAndVerify() error {
	// Programs is a fixed shared KnownFolder child, never a caller-selected root.
	// It may be absent when setup stopped immediately after marker publication.
	if err := os.Mkdir(filepath.Dir(n.l.stableRoot), 0700); err != nil && !os.IsExist(err) {
		return err
	}
	var err error
	n.programClaim, err = desktoplaunch.ClaimInitialProgramRoot(n.l.stableRoot, n.pkg.layout)
	if err != nil {
		return err
	}
	n.updatesClaim, err = desktoplaunch.ClaimInitialUpdateRoot(n.l.updates)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(n.pkg.files))
	for relative := range n.pkg.files {
		names = append(names, relative)
	}
	sort.Strings(names)
	for _, relative := range names {
		file, err := os.Open(n.pkg.files[relative].path) // #nosec G304 -- Exact pinned authenticated private package source.
		if err != nil {
			return err
		}
		copyErr := n.programClaim.CopyVerifiedFile(relative, file)
		closeErr := file.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	return errors.Join(n.programClaim.VerifyLayout(), n.updatesClaim.VerifyLayout())
}

func (n *nativeInitialRetry) publish() (initialPublicationOutcome, error) {
	attempted, err := n.updatesClaim.PublishInitialSeed(n.pkg.seed)
	if !attempted {
		return initialPublicationNotInvoked, err
	}
	if err != nil {
		return initialPublicationUnknown, err
	}
	return initialPublicationCompleted, nil
}
func (n *nativeInitialRetry) observe() (initialSeedObservation, error) {
	exact, absent, err := n.updatesClaim.ObserveInitialSeed(n.pkg.seed)
	if exact {
		return initialSeedExact, err
	}
	if err != nil {
		return initialSeedUnknown, err
	}
	if absent {
		return initialSeedAbsent, nil
	}
	return initialSeedUnexpected, nil
}
func (n *nativeInitialRetry) flush() error { return n.updatesClaim.FlushInitialSeed() }
func (n *nativeInitialRetry) closeCreated() error {
	return errors.Join(n.updatesClaim.Close(), n.programClaim.Close())
}
func (n *nativeInitialRetry) rollbackCreated() error {
	return errors.Join(n.updatesClaim.Rollback(), n.programClaim.Rollback())
}
func (n *nativeInitialRetry) closeArchive() error {
	return errors.Join(n.updatesLease.Close(), n.programLease.Close())
}

func restoreInitialDirectory(archive, target string, original os.FileInfo, moved bool) error {
	if !moved {
		return nil
	}
	actual, err := initialOwnedDirectoryInfo(archive)
	if err != nil {
		return err
	}
	if original == nil || !os.SameFile(original, actual) {
		return errors.New("initial restoration archive identity mismatch")
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return errors.New("canonical root occupied; preserved initial archive")
	}
	source, err := syscall.UTF16PtrFromString(archive)
	if err != nil {
		return err
	}
	destination, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, destination, windows.MOVEFILE_WRITE_THROUGH)
}
func (n *nativeInitialRetry) restoreArchives() error {
	return errors.Join(restoreInitialDirectory(n.updatesArchive, n.l.updates, n.updatesOriginal, n.updatesMoved), restoreInitialDirectory(n.programArchive, n.l.stableRoot, n.programOriginal, n.programMoved))
}
