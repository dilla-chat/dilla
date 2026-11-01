// Package desktoplaunch verifies the content used by the fixed Windows desktop launcher.
// Callers supply only roots and trust compiled into the native launcher, never renderer input.
package desktoplaunch

import (
	"bytes"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxDocument = 1_048_576
const maxSafeInteger = int64(9_007_199_254_740_991)
const maxFileSize = int64(4 * 1024 * 1024 * 1024)

type ManifestContext struct{ Version, Channel, Arch string }
type ProgramFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA512 string `json:"sha512"`
}
type ProgramManifest struct {
	Format   int           `json:"format"`
	Kind     string        `json:"kind"`
	Version  string        `json:"version"`
	Channel  string        `json:"channel"`
	Platform string        `json:"platform"`
	Arch     string        `json:"arch"`
	Files    []ProgramFile `json:"files"`
}

// VerifyManifest binds the exact published bytes to an already authenticated digest
// and the selected release identity. The manifest lives outside the program tree.
func VerifyManifest(raw []byte, expectedSHA512 string, ctx ManifestContext) (*ProgramManifest, error) {
	if len(raw) > maxDocument || !matchesDigest(raw, expectedSHA512) {
		return nil, errors.New("invalid program manifest digest or size")
	}
	if err := validateJSON(raw); err != nil {
		return nil, err
	}
	obj, err := exactObject(raw, "format", "kind", "version", "channel", "platform", "arch", "files")
	if err != nil {
		return nil, err
	}
	var m ProgramManifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Format != 1 || m.Kind != "dilla-desktop-program" || m.Platform != "win32" || m.Arch != "x64" || m.Version != ctx.Version || m.Channel != ctx.Channel || m.Arch != ctx.Arch || !validVersionChannel(m.Version, m.Channel) {
		return nil, errors.New("invalid program manifest identity")
	}
	var entries []json.RawMessage
	if err = json.Unmarshal(obj["files"], &entries); err != nil {
		return nil, err
	}
	if len(entries) < 1 || len(entries) > 16_384 {
		return nil, errors.New("invalid program manifest file count")
	}
	nodes := map[string]string{}
	files := map[string]bool{}
	var total int64
	last := ""
	for i, f := range m.Files {
		if _, err = exactObject(entries[i], "path", "size", "sha512"); err != nil {
			return nil, err
		}
		if !validProgramPath(f.Path) || (i > 0 && f.Path <= last) || f.Size < 0 || f.Size > maxFileSize {
			return nil, errors.New("invalid program manifest file")
		}
		if _, err = decodeDigest(f.SHA512); err != nil {
			return nil, err
		}
		total += f.Size
		if total > 8*1024*1024*1024 {
			return nil, errors.New("program tree exceeds size bound")
		}
		parts := strings.Split(f.Path, "/")
		for n := 1; n <= len(parts); n++ {
			name := strings.Join(parts[:n], "/")
			key := strings.ToLower(name)
			if old, ok := nodes[key]; ok && old != name {
				return nil, errors.New("case-aliased program path")
			}
			if files[key] {
				return nil, errors.New("program path file ancestor collision")
			}
			if n == len(parts) {
				if _, ok := nodes[key]; ok {
					return nil, errors.New("program path directory collision")
				}
				files[key] = true
			}
			nodes[key] = name
		}
		last = f.Path
	}
	return &m, nil
}

// TreeID uses a bounded directory name; the complete digest is always verified.
func TreeID(manifestSHA512 string) (string, error) {
	d, e := decodeDigest(manifestSHA512)
	if e != nil {
		return "", e
	}
	return "v-" + hex.EncodeToString(d)[:32], nil
}
func ManifestName(manifestSHA512 string) (string, error) {
	d, e := decodeDigest(manifestSHA512)
	if e != nil {
		return "", e
	}
	return hex.EncodeToString(d) + ".json", nil
}

func decodeDigest(value string) ([]byte, error) {
	if len(value) != 88 {
		return nil, errors.New("invalid SHA512 encoding")
	}
	d, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(d) != sha512.Size || base64.StdEncoding.EncodeToString(d) != value {
		return nil, errors.New("invalid SHA512 encoding")
	}
	return d, nil
}
func matchesDigest(data []byte, expected string) bool {
	d, e := decodeDigest(expected)
	if e != nil {
		return false
	}
	s := sha512.Sum512(data)
	return subtle.ConstantTimeCompare(s[:], d) == 1
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-beta\.(0|[1-9][0-9]*))?$`)

func versionParts(value string) ([4]int64, error) {
	var parts [4]int64
	m := versionPattern.FindStringSubmatch(value)
	if m == nil {
		return parts, errors.New("invalid desktop version")
	}
	for i := 0; i < 4; i++ {
		if i == 3 && m[i+1] == "" {
			parts[i] = maxSafeInteger
			continue
		}
		n, e := strconv.ParseInt(m[i+1], 10, 64)
		if e != nil || n > maxSafeInteger {
			return parts, errors.New("invalid desktop version")
		}
		parts[i] = n
	}
	return parts, nil
}
func validVersionChannel(version, channel string) bool {
	_, e := versionParts(version)
	return e == nil && ((channel == "stable" && !strings.Contains(version, "-")) || (channel == "beta" && strings.Contains(version, "-beta.")))
}
func validProgramPath(path string) bool {
	if len(path) == 0 || len(path) > 240 {
		return false
	}
	for _, c := range []byte(path) {
		if c < 0x20 || c > 0x7e || strings.ContainsRune(`<>:"\|?*`, rune(c)) {
			return false
		}
	}
	for _, p := range strings.Split(path, "/") {
		if len(p) == 0 || len(p) > 100 || p == "." || p == ".." || p[0] == ' ' || strings.HasSuffix(p, " ") || strings.HasSuffix(p, ".") {
			return false
		}
		base := strings.ToUpper(strings.TrimRight(strings.SplitN(p, ".", 2)[0], " "))
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && ((strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return false
		}
	}
	return true
}

// validateJSON rejects ambiguous duplicate fields before decoding any document,
// including signed documents. json.Unmarshal alone accepts duplicate keys.
func validateJSON(raw []byte) error {
	if len(raw) > maxDocument || !utf8.Valid(raw) {
		return errors.New("invalid JSON bytes")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting exceeds bound")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return errors.New("duplicate JSON key")
				}
				seen[key] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := value(depth + 1); e != nil {
					return e
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func exactObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(raw, &obj); e != nil {
		return nil, e
	}
	if len(obj) != len(keys) {
		return nil, errors.New("unexpected or missing document field")
	}
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || bytes.Equal(v, []byte("null")) {
			return nil, errors.New("missing document field")
		}
	}
	return obj, nil
}

// VerifiedTree retains every file and directory handle until the fixed executable
// is created. On Windows these handles deny write/delete sharing on the opened
// streams; stream enumeration also rejects named $DATA streams at verification.
// The default-stream handles do not prevent later ADS creation by a trusted
// current-user process. Drain authorized writers before verification and retain
// the ownership/DACL/path guards through process creation. Close only after
// CreateProcess returns; this is content verification, not a launch authorization API.
type VerifiedTree struct {
	handles []*os.File
	root    *os.Root
}

func (v *VerifiedTree) Close() error {
	var errs []error
	for i := len(v.handles) - 1; i >= 0; i-- {
		errs = append(errs, v.handles[i].Close())
	}
	v.handles = nil
	if v.root != nil {
		errs = append(errs, v.root.Close())
		v.root = nil
	}
	return errors.Join(errs...)
}
func VerifyTree(root string, manifest *ProgramManifest) (verified *VerifiedTree, err error) {
	if manifest == nil || len(manifest.Files) == 0 {
		return nil, errors.New("missing verified program manifest")
	}
	// Revalidate the supplied value so a caller cannot bypass path safety with a
	// constructed or modified ProgramManifest.
	raw, e := json.Marshal(manifest)
	if e != nil {
		return nil, e
	}
	if _, e = VerifyManifest(raw, func() string { s := sha512.Sum512(raw); return base64.StdEncoding.EncodeToString(s[:]) }(), ManifestContext{manifest.Version, manifest.Channel, manifest.Arch}); e != nil {
		return nil, e
	}
	v := &VerifiedTree{}
	defer func() {
		if err != nil {
			_ = v.Close()
		}
	}()
	abs, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	ancestors, e := pinAncestors(abs)
	if e != nil {
		return nil, e
	}
	v.handles = append(v.handles, ancestors...)
	v.root, e = os.OpenRoot(abs)
	if e != nil {
		return nil, e
	}
	expected := map[string]*ProgramFile{}
	for i := range manifest.Files {
		f := &manifest.Files[i]
		expected[f.Path] = f
		parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(f.Path)))
		for parent != "." {
			if _, ok := expected[parent]; !ok {
				expected[parent] = nil
			}
			parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent)))
		}
	}
	seen := map[string]bool{}
	var walk func(string) error
	walk = func(dir string) error {
		f, e := openPinned(abs, v.root, dir, true)
		if e != nil {
			return e
		}
		v.handles = append(v.handles, f)
		entries, e := f.ReadDir(-1)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			name := entry.Name()
			path := name
			if dir != "." {
				path = dir + "/" + name
			}
			want, ok := expected[path]
			if !ok || seen[path] || !validProgramPath(path) {
				return fmt.Errorf("unexpected program entry %q", path)
			}
			seen[path] = true
			if want == nil {
				if e = walk(path); e != nil {
					return e
				}
				continue
			}
			file, e := openPinned(abs, v.root, path, false)
			if e != nil {
				return e
			}
			v.handles = append(v.handles, file)
			info, e := file.Stat()
			if e != nil {
				return e
			}
			if info.Size() != want.Size {
				return fmt.Errorf("program file size mismatch: %s", path)
			}
			h := sha512.New()
			n, e := io.Copy(h, io.LimitReader(file, want.Size+1))
			if e != nil {
				return e
			}
			d, _ := decodeDigest(want.SHA512)
			if n != want.Size || subtle.ConstantTimeCompare(h.Sum(nil), d) != 1 {
				return fmt.Errorf("program file digest mismatch: %s", path)
			}
		}
		return nil
	}
	if e = walk("."); e != nil {
		return nil, e
	}
	if len(seen) != len(expected) {
		return nil, errors.New("incomplete program tree")
	}
	return v, nil
}

func pinAncestors(path string) (files []*os.File, err error) {
	var held []*os.File
	defer func() {
		if err != nil {
			for _, f := range held {
				_ = f.Close()
			}
		}
	}()
	var paths []string
	for p := path; ; p = filepath.Dir(p) {
		paths = append(paths, p)
		if p == filepath.Dir(p) {
			break
		}
	}
	for i := len(paths) - 1; i >= 0; i-- {
		f, e := openAbsolutePinned(paths[i], true)
		if e != nil {
			return nil, e
		}
		files = append(files, f)
		held = append(held, f)
	}
	return files, nil
}
