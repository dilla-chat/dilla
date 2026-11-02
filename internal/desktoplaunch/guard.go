package desktoplaunch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type DirectoryGuard struct{ guard *VerifiedTree }

// Stat snapshots metadata through the retained validated directory handle.
// On Windows this eagerly records volume/file identity, so later SameFile calls
// cannot resolve a renamed directory through its old pathname.
func (d *DirectoryGuard) Stat() (os.FileInfo, error) {
	if d == nil || d.guard == nil || len(d.guard.handles) == 0 {
		return nil, errors.New("closed owned directory guard")
	}
	return d.guard.handles[len(d.guard.handles)-1].Stat()
}

func (d *DirectoryGuard) Close() error {
	if d == nil || d.guard == nil {
		return nil
	}
	return d.guard.Close()
}

// FileGuard exposes bounded content reads only. Native callers derive its path
// from the compiled identity and fixed metadata layout, never CLI or renderer data.
type FileGuard struct {
	directory *DirectoryGuard
	file      *os.File
}

func (f *FileGuard) Close() error {
	if f == nil {
		return nil
	}
	var e error
	if f.file != nil {
		e = f.file.Close()
		f.file = nil
	}
	return errors.Join(e, f.directory.Close())
}
func (f *FileGuard) ReadBounded(limit int64) ([]byte, error) {
	if f == nil || f.file == nil || limit < 1 || limit > maxDocument {
		return nil, errors.New("invalid bounded metadata read")
	}
	info, e := f.file.Stat()
	if e != nil {
		return nil, e
	}
	if info.Size() > limit {
		return nil, errors.New("metadata exceeds bound")
	}
	if _, e = f.file.Seek(0, io.SeekStart); e != nil {
		return nil, e
	}
	data, e := io.ReadAll(io.LimitReader(f.file, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(data)) > limit {
		return nil, errors.New("metadata exceeds bound")
	}
	return data, nil
}

// PinOwnedDirectory verifies the selected directory's Windows owner and DACL,
// rejects reparse points in every ancestor, and retains handles against replacement.
// It does not create directories or alter permissions.
func PinOwnedDirectory(path string) (guard *DirectoryGuard, err error) {
	v := &VerifiedTree{}
	defer func() {
		if err != nil {
			_ = v.Close()
		}
	}()
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	v.handles, e = pinAncestors(abs)
	if e != nil {
		return nil, e
	}
	v.root, e = os.OpenRoot(abs)
	if e != nil {
		return nil, e
	}
	file, e := openPinned(abs, v.root, ".", true)
	if e != nil {
		return nil, e
	}
	v.handles = append(v.handles, file)
	return &DirectoryGuard{guard: v}, nil
}

var fixedManifestName = regexp.MustCompile(`^[a-f0-9]{128}\.json$`)

func PinOwnedFile(path string) (*FileGuard, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	base := filepath.Base(abs)
	if strings.Contains(base, "/") || (!validProgramPath(base) && !fixedManifestName.MatchString(base)) {
		return nil, errors.New("invalid fixed metadata filename")
	}
	directory, e := PinOwnedDirectory(filepath.Dir(abs))
	if e != nil {
		return nil, e
	}
	file, e := openPinned(filepath.Dir(abs), directory.guard.root, base, false)
	if e != nil {
		_ = directory.Close()
		return nil, e
	}
	return &FileGuard{directory: directory, file: file}, nil
}
