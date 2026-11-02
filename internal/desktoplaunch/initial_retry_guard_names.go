package desktoplaunch

import (
	"errors"
	"io"
	"os"
)

// HasFile reports membership in the already authenticated retained allowlist.
// It never reopens a path, so deletion/rename cannot bypass prefix validation.
func (g *InitialRetryTree) HasFile(relative string) bool { return g != nil && g.files[relative] != nil }

// VerifyRoot binds a quarantine lease to the directory identity observed by its
// no-replace move, rather than accepting a later object at the archive name.
func (g *InitialRetryTree) VerifyRoot(expected os.FileInfo) error {
	if g == nil || expected == nil || len(g.directories) == 0 {
		return errors.New("missing initial archive identity")
	}
	root := g.directories[0].guard.handles
	if len(root) == 0 {
		return errors.New("closed initial archive identity")
	}
	actual, err := root[len(root)-1].Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(expected, actual) {
		return errors.New("initial archive identity changed")
	}
	return nil
}

// ReadInitialSeed reads only the fixed initial authority name through its
// retained lease, bounded by the initial-slot format. No path reopen occurs.
func (g *InitialRetryTree) ReadInitialSeed() ([]byte, error) {
	if g == nil || g.files["windows-active-0.json"] == nil {
		return nil, errors.New("initial seed is not retained")
	}
	file := g.files["windows-active-0.json"]
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4096 {
		return nil, errors.New("initial seed exceeds bound")
	}
	return raw, nil
}
