package desktoplaunch

import (
	"errors"
	"path/filepath"
	"strings"
)

// InitialBootstrapFiles describes only the two fixed files supplied by the
// executing, marker-owning installer. Digests come from pinned private sources,
// never launcher arguments or renderer data.
type InitialBootstrapFiles struct {
	ProductName       string
	LauncherSize      int64
	LauncherSHA512    string
	UninstallerSize   int64
	UninstallerSHA512 string
}

type InitialCopyLayout struct {
	files                map[string]ProgramFile
	directories          map[string]bool
	manifestName, treeID string
}

func NewInitialCopyLayout(raw []byte, digest string, context ManifestContext, bootstrap InitialBootstrapFiles) (*InitialCopyLayout, error) {
	manifest, err := VerifyManifest(raw, digest, context)
	if err != nil {
		return nil, err
	}
	name, err := ManifestName(digest)
	if err != nil {
		return nil, err
	}
	treeID, err := TreeID(digest)
	if err != nil {
		return nil, err
	}
	if !validProgramPath(bootstrap.ProductName) || strings.Contains(bootstrap.ProductName, "/") {
		return nil, errors.New("invalid initial bootstrap identity")
	}
	for _, file := range []ProgramFile{{Size: bootstrap.LauncherSize, SHA512: bootstrap.LauncherSHA512}, {Size: bootstrap.UninstallerSize, SHA512: bootstrap.UninstallerSHA512}} {
		if file.Size < 1 || file.Size > maxFileSize {
			return nil, errors.New("invalid initial bootstrap size")
		}
		if _, err = decodeDigest(file.SHA512); err != nil {
			return nil, err
		}
	}
	layout := &InitialCopyLayout{files: map[string]ProgramFile{}, directories: map[string]bool{".": true}, manifestName: name, treeID: treeID}
	add := func(file ProgramFile) {
		layout.files[file.Path] = file
		for directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(file.Path))); directory != "."; directory = filepath.ToSlash(filepath.Dir(filepath.FromSlash(directory))) {
			layout.directories[directory] = true
		}
	}
	add(ProgramFile{Path: "Dilla.exe", Size: bootstrap.LauncherSize, SHA512: bootstrap.LauncherSHA512})
	add(ProgramFile{Path: "Uninstall " + bootstrap.ProductName + ".exe", Size: bootstrap.UninstallerSize, SHA512: bootstrap.UninstallerSHA512})
	add(ProgramFile{Path: "manifests/" + name, Size: int64(len(raw)), SHA512: digest})
	for _, file := range manifest.Files {
		file.Path = "versions/" + treeID + "/" + file.Path
		add(file)
	}
	if len(layout.files)+len(layout.directories) > maxDocument+16 {
		return nil, errors.New("initial layout exceeds authenticated manifest bound")
	}
	return layout, nil
}

func (l *InitialCopyLayout) expectedFile(relative string) (ProgramFile, bool) {
	if l == nil {
		return ProgramFile{}, false
	}
	file, found := l.files[relative]
	return file, found
}
