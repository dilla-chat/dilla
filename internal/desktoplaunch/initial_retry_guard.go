package desktoplaunch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InitialRetryTree retains an authenticated archived initial-install namespace.
// It exposes no write, path-selection or handle-transfer operation. The caller
// constructs the exact entry allowlist from its compiled package manifest.
type InitialRetryTree struct {
	directories []*DirectoryGuard
	files       map[string]*os.File
}

func (g *InitialRetryTree) Close() error {
	if g == nil {
		return nil
	}
	var result error
	for name, file := range g.files {
		result = errors.Join(result, file.Close())
		delete(g.files, name)
	}
	for i := len(g.directories) - 1; i >= 0; i-- {
		result = errors.Join(result, g.directories[i].Close())
	}
	g.directories = nil
	return result
}

func PinInitialRetryTree(root string, expected map[string]bool) (guard *InitialRetryTree, err error) {
	guard = &InitialRetryTree{files: map[string]*os.File{}}
	defer func() {
		if err != nil {
			err = errors.Join(err, guard.Close())
			guard = nil
		}
	}()
	if !expected["."] {
		return nil, errors.New("missing initial root allowlist")
	}

	if len(expected) > maxDocument+16 {
		return nil, errors.New("initial retry allowlist exceeds manifest bound")
	}
	seen := map[string]bool{}
	var visit func(string, string, int) error
	visit = func(path, relative string, depth int) error {
		if depth > 128 || seen[relative] || len(seen) >= len(expected) {
			return errors.New("initial retry traversal exceeds authenticated bounds")
		}
		directory, found := expected[relative]
		if !found {
			return errors.New("unknown initial retry entry")
		}
		seen[relative] = true
		if directory {
			pinned, e := PinOwnedDirectory(path)
			if e != nil {
				return e
			}
			guard.directories = append(guard.directories, pinned)
			file := pinned.guard.handles[len(pinned.guard.handles)-1]
			return readInitialDirectory(file, func(entry os.DirEntry) error {
				child := entry.Name()
				if relative != "." {
					child = relative + "/" + child
				}
				wanted, ok := expected[child]
				if !ok || wanted != entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					return errors.New("unknown or linked initial retry entry")
				}
				if strings.Count(child, "/") > 128 {
					return errors.New("initial retry directory depth exceeds manifest bound")
				}
				return visit(filepath.Join(path, entry.Name()), child, depth+1)
			})
		}
		file, e := openInitialRetryFile(path)
		if e != nil {
			return e
		}
		guard.files[relative] = file
		return nil
	}
	err = visit(root, ".", 0)
	return guard, err
}

func (g *InitialRetryTree) ComparePrefix(relative string, expected io.Reader) error {
	file := g.files[relative]
	if file == nil {
		return errors.New("unlisted initial retry file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return compareInitialCopyPrefix(file, expected)
}

// CopyFile can expose a sequential prefix followed by preallocated zero bytes.
// Nothing permits unrelated nonzero data or a file longer than its package.
func compareInitialCopyPrefix(actual, expected io.Reader) error {
	a, b := make([]byte, 65536), make([]byte, 65536)
	zeroTail := false
	for {
		count, readErr := actual.Read(a)
		if count > 0 {
			if _, err := io.ReadFull(expected, b[:count]); err != nil {
				return errors.New("initial file exceeds packaged bytes")
			}
			for i := 0; i < count; i++ {
				if !zeroTail && a[i] == b[i] {
					continue
				}
				if a[i] != 0 {
					return errors.New("initial file differs from interrupted package")
				}
				zeroTail = true
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

// CompareInitialPrefix reads the already validated, retained destination handle.
// It cannot reopen or select another destination path.
func (f *FileGuard) CompareInitialPrefix(expected io.Reader) error {
	if f == nil || f.file == nil {
		return errors.New("closed initial prefix guard")
	}
	if _, err := f.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return compareInitialCopyPrefix(f.file, expected)
}

// readInitialDirectory uses a fresh handle/cursor and bounded batches. Unknown
// entries fail before traversal, so filesystem size cannot exceed the allowlist.
func readInitialDirectory(file *os.File, visit func(os.DirEntry) error) (err error) {
	enumeration, err := openInitialEnumeration(file)
	if err != nil {
		return fmt.Errorf("initial enumeration reopen: %w", err)
	}
	defer func() { err = errors.Join(err, enumeration.Close()) }()
	for {
		entries, readErr := enumeration.ReadDir(64)
		for _, entry := range entries {
			if err = visit(entry); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("initial enumeration read: %w", readErr)
		}
	}
}
