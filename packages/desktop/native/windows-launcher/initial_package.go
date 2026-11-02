package main

import (
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

type initialCopySource struct {
	path   string
	size   int64
	digest string
}
type initialPackage struct {
	files   map[string]initialCopySource
	entries map[string]bool
	seed    []byte
	layout  *desktoplaunch.InitialCopyLayout
	guards  []io.Closer
}

func (p *initialPackage) Close() error {
	if p == nil {
		return nil
	}
	var result error
	for i := len(p.guards) - 1; i >= 0; i-- {
		result = errors.Join(result, p.guards[i].Close())
	}
	p.guards = nil
	return result
}

func loadInitialPackage(payload string, cfg buildConfig) (pkg *initialPackage, err error) {
	pkg = &initialPackage{files: map[string]initialCopySource{}, entries: map[string]bool{".": true, "versions": true, "manifests": true}}
	defer func() {
		if err != nil {
			err = errors.Join(err, pkg.Close())
			pkg = nil
		}
	}()
	root, err := desktoplaunch.PinOwnedDirectory(payload)
	if err != nil {
		return pkg, err
	}
	pkg.guards = append(pkg.guards, root)
	name, err := desktoplaunch.ManifestName(cfg.InitialManifestSHA512)
	if err != nil {
		return pkg, err
	}
	treeID, err := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
	if err != nil {
		return pkg, err
	}
	manifestGuard, err := desktoplaunch.PinOwnedFile(filepath.Join(payload, "manifests", name))
	if err != nil {
		return pkg, err
	}
	pkg.guards = append(pkg.guards, manifestGuard)
	raw, err := manifestGuard.ReadBounded(1_048_576)
	if err != nil {
		return pkg, err
	}
	context := desktoplaunch.ManifestContext{Version: cfg.InitialVersion, Channel: cfg.Channel, Arch: cfg.Arch}
	manifest, err := desktoplaunch.VerifyManifest(raw, cfg.InitialManifestSHA512, context)
	if err != nil {
		return pkg, err
	}
	tree, err := desktoplaunch.VerifyTree(filepath.Join(payload, "versions", treeID), manifest)
	if err != nil {
		return pkg, err
	}
	pkg.guards = append(pkg.guards, tree)
	seedGuard, err := desktoplaunch.PinOwnedFile(filepath.Join(payload, "initial-slot.json"))
	if err != nil {
		return pkg, err
	}
	pkg.guards = append(pkg.guards, seedGuard)
	pkg.seed, err = seedGuard.ReadBounded(4096)
	if err != nil {
		return pkg, err
	}
	seedSlot, verifyErr := desktoplaunch.VerifySlot(pkg.seed, cfg.trust(), "", time.Now())
	if verifyErr != nil {
		return pkg, verifyErr
	}
	if seedSlot.Kind != "initial" || seedSlot.Generation != 0 {
		return pkg, errors.New("package seed is not initial generation zero")
	}
	add := func(relative string, source initialCopySource) {
		pkg.files[relative] = source
		pkg.entries[relative] = false
		for directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative))); directory != "."; directory = filepath.ToSlash(filepath.Dir(filepath.FromSlash(directory))) {
			pkg.entries[directory] = true
		}
	}
	add("manifests/"+name, initialCopySource{path: filepath.Join(payload, "manifests", name), size: int64(len(raw)), digest: cfg.InitialManifestSHA512})
	for _, file := range manifest.Files {
		relative := "versions/" + treeID + "/" + file.Path
		add(relative, initialCopySource{path: filepath.Join(payload, filepath.FromSlash(relative)), size: file.Size, digest: file.SHA512})
	}
	for _, relative := range []string{"Dilla.exe", initialUninstallerName(cfg)} {
		path := filepath.Join(payload, relative)
		guard, e := desktoplaunch.PinOwnedFile(path)
		if e != nil {
			return pkg, e
		}
		pkg.guards = append(pkg.guards, guard)
		source, e := measureInitialFile(path)
		if e != nil {
			return pkg, e
		}
		add(relative, source)
	}
	launcher := pkg.files["Dilla.exe"]
	uninstaller := pkg.files["Uninstall "+cfg.ProductName+".exe"]
	pkg.layout, err = desktoplaunch.NewInitialCopyLayout(raw, cfg.InitialManifestSHA512, context, desktoplaunch.InitialBootstrapFiles{ProductName: cfg.ProductName, LauncherSize: launcher.size, LauncherSHA512: launcher.digest, UninstallerSize: uninstaller.size, UninstallerSHA512: uninstaller.digest})
	return pkg, err
}

// The fixed source/destination was already authenticated and remains pinned.
// This read hashes bytes only; it grants no path-taking launcher authority.
func measureInitialFile(path string) (source initialCopySource, err error) {
	file, err := os.Open(path) // #nosec G304 -- Native caller derives fixed pinned package or installation allowlist paths.
	if err != nil {
		return source, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return source, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 4<<30 {
		return source, errors.New("invalid initial packaged file size/type")
	}
	hash := sha512.New()
	size, err := io.Copy(hash, io.LimitReader(file, info.Size()+1))
	if err != nil {
		return source, err
	}
	if size != info.Size() {
		return source, errors.New("initial packaged file changed size")
	}
	return initialCopySource{path: path, size: size, digest: base64.StdEncoding.EncodeToString(hash.Sum(nil))}, nil
}

func verifyCompleteInitialProgram(root string, pkg *initialPackage) error {
	for relative, source := range pkg.files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		guard, err := desktoplaunch.PinOwnedFile(path)
		if err != nil {
			return err
		}
		actual, readErr := measureInitialFile(path)
		closeErr := guard.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if actual.size != source.size || actual.digest != source.digest {
			return errors.New("initial installed file does not match complete package")
		}
	}
	// Unknown entries were rejected by the authenticated subset check first.
	return nil
}

func initialUninstallerName(cfg buildConfig) string {
	return "Uninstall " + cfg.ProductName + ".exe"
}
