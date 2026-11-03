package desktopsandbox

import (
	"bytes"
	"crypto/sha512"
	"crypto/subtle"
	"debug/elf"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"
)

var (
	ErrFraming        = errors.New("invalid Linux sandbox capsule framing")
	ErrManifest       = errors.New("invalid Linux sandbox manifest")
	ErrAuthentication = errors.New("linux sandbox component is not authenticated")
	ErrExecutable     = errors.New("incompatible Linux sandbox executable")
	ErrPolicy         = errors.New("linux sandbox generation policy rejected")
	ErrState          = errors.New("invalid Linux sandbox protected state")
	ErrTime           = errors.New("linux sandbox acceptance time rejected")
	ErrCompatibility  = errors.New("linux sandbox prerequisite is not compatible")
)

const signatureDomain = "Dilla Linux sandbox capsule v1\x00"

var magic = [8]byte{'D', 'L', 'S', 'B', 'O', 'X', '1', 0}

// ParseCapsule validates all declared sizes before allocation and reads exactly
// one framed capsule. A caller imposing an IO deadline remains responsible for
// a reader blocked waiting on its source (not a byte-bound parsing concern).
func ParseCapsule(r io.Reader, limits Limits) (Capsule, error) {
	bounds := Limits{MaxManifest, MaxMaintainer, MaxHelper, MaxCapsule}
	for _, pair := range [][2]*uint64{{&limits.Manifest, &bounds.Manifest}, {&limits.Maintainer, &bounds.Maintainer}, {&limits.Helper, &bounds.Helper}, {&limits.Total, &bounds.Total}} {
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		} else if *pair[0] > *pair[1] {
			return Capsule{}, ErrFraming
		}
	}
	var header [32]byte
	if r == nil {
		return Capsule{}, ErrFraming
	}
	if _, err := io.ReadFull(r, header[:]); err != nil || !bytes.Equal(header[:8], magic[:]) {
		return Capsule{}, ErrFraming
	}
	m, s, n, h := uint64(binary.LittleEndian.Uint32(header[8:])), uint64(binary.LittleEndian.Uint32(header[12:])), binary.LittleEndian.Uint64(header[16:]), binary.LittleEndian.Uint64(header[24:])
	if m == 0 || m > limits.Manifest || (s != 0 && s != 64) || n == 0 || n > limits.Maintainer || h == 0 || h > limits.Helper || 32+m+s+n+h > limits.Total {
		return Capsule{}, ErrFraming
	}
	payload := make([]byte, m+s+n+h)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Capsule{}, ErrFraming
	}
	var extra [1]byte
	if count, err := io.ReadFull(r, extra[:]); count != 0 || err != io.EOF {
		return Capsule{}, ErrFraming
	}
	return Capsule{payload[:m], payload[m : m+s], payload[m+s : m+s+n], payload[m+s+n:]}, nil
}

func capsuleShape(c Capsule) bool {
	return len(c.Manifest) > 0 && len(c.Manifest) <= MaxManifest && (len(c.Signature) == 0 || len(c.Signature) == 64) && len(c.Maintainer) > 0 && len(c.Maintainer) <= MaxMaintainer && len(c.Helper) > 0 && len(c.Helper) <= MaxHelper && 32+len(c.Manifest)+len(c.Signature)+len(c.Maintainer)+len(c.Helper) <= MaxCapsule
}
func copyCapsule(c Capsule) Capsule {
	return Capsule{bytes.Clone(c.Manifest), bytes.Clone(c.Signature), bytes.Clone(c.Maintainer), bytes.Clone(c.Helper)}
}
func capsuleDigest(c Capsule) string {
	manifestSize, signatureSize := len(c.Manifest), len(c.Signature)
	if manifestSize < 1 || manifestSize > MaxManifest || signatureSize < 0 || signatureSize > 64 {
		return ""
	}
	var header [32]byte
	copy(header[:], magic[:])
	binary.LittleEndian.PutUint32(header[8:], uint32(manifestSize))
	binary.LittleEndian.PutUint32(header[12:], uint32(signatureSize))
	binary.LittleEndian.PutUint64(header[16:], uint64(len(c.Maintainer)))
	binary.LittleEndian.PutUint64(header[24:], uint64(len(c.Helper)))
	hash := sha512.New()
	for _, p := range [][]byte{header[:], c.Manifest, c.Signature, c.Maintainer, c.Helper} {
		_, _ = hash.Write(p)
	}
	return base64.StdEncoding.EncodeToString(hash.Sum(nil))
}
func (v VerifiedCapsule) ManifestSHA512() string { return hashBytes(v.capsule.Manifest) }
func hashBytes(b []byte) string {
	h := sha512.Sum512(b)
	return base64.StdEncoding.EncodeToString(h[:])
}
func decodeBase64(s string, limit int) ([]byte, error) {
	if len(s) > base64.StdEncoding.EncodedLen(limit) {
		return nil, ErrManifest
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) > limit || base64.StdEncoding.EncodeToString(b) != s {
		return nil, ErrManifest
	}
	return b, nil
}
func validDigest(s string) bool {
	b, e := decodeBase64(s, sha512.Size)
	return e == nil && len(b) == sha512.Size
}
func matchesDigest(b []byte, s string) bool {
	expected, e := decodeBase64(s, sha512.Size)
	if e != nil || len(expected) != sha512.Size {
		return false
	}
	actual := sha512.Sum512(b)
	return subtle.ConstantTimeCompare(actual[:], expected) == 1
}

// validateJSON rejects duplicate decoded keys, invalid UTF-8, deep nesting and
// noncanonical/unsafe numeric encodings BEFORE Go's case-insensitive unmarshaler.
func validateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrManifest
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return ErrManifest
		}
		token, err := d.Token()
		if err != nil {
			return ErrManifest
		}
		switch v := token.(type) {
		case json.Delim:
			switch v {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, e := d.Token()
					name, ok := key.(string)
					if e != nil || !ok || seen[name] {
						return ErrManifest
					}
					seen[name] = true
					if e = value(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim('}') {
					return ErrManifest
				}
			case '[':
				for d.More() {
					if e := value(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim(']') {
					return ErrManifest
				}
			default:
				return ErrManifest
			}
		case json.Number:
			if !integerPattern.MatchString(string(v)) {
				return ErrManifest
			}
			n, e := strconv.ParseInt(string(v), 10, 64)
			if e != nil || n > MaxSafeInteger {
				return ErrManifest
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrManifest
	}
	return nil
}

var integerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func exactObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil || len(obj) != len(keys) {
		return nil, ErrManifest
	}
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			return nil, ErrManifest
		}
	}
	return obj, nil
}
func parseManifest(raw []byte) (Manifest, error) {
	if len(raw) == 0 || len(raw) > MaxManifest || validateJSON(raw) != nil {
		return Manifest{}, ErrManifest
	}
	obj, err := exactObject(raw, "format", "kind", "generation", "platform", "arch", "compatibilityEpoch", "sandboxApi", "publishedAt", "expiresAt", "maintainer", "helper")
	if err != nil {
		return Manifest{}, err
	}
	for _, key := range []string{"maintainer", "helper"} {
		if _, err = exactObject(obj[key], "size", "sha512"); err != nil {
			return Manifest{}, err
		}
	}
	var m Manifest
	if json.Unmarshal(raw, &m) != nil || m.Format != 1 || m.Kind != "dilla-linux-sandbox" || m.Generation < 1 || m.Generation > MaxSafeInteger || m.Platform != "linux" || m.Arch != "x64" || m.CompatibilityEpoch != 1 || m.SandboxAPI != 1 || m.Maintainer.Size < 1 || m.Maintainer.Size > MaxMaintainer || m.Helper.Size < 1 || m.Helper.Size > MaxHelper || !validDigest(m.Maintainer.SHA512) || !validDigest(m.Helper.SHA512) {
		return Manifest{}, ErrManifest
	}
	if _, _, err = manifestTimes(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

var utcPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)

func manifestTimes(m Manifest) (time.Time, time.Time, error) {
	if !utcPattern.MatchString(m.PublishedAt) || !utcPattern.MatchString(m.ExpiresAt) {
		return time.Time{}, time.Time{}, ErrManifest
	}
	pub, e1 := time.Parse(time.RFC3339Nano, m.PublishedAt)
	exp, e2 := time.Parse(time.RFC3339Nano, m.ExpiresAt)
	if e1 != nil || e2 != nil || !exp.After(pub) || exp.Sub(pub) > 93*24*time.Hour {
		return time.Time{}, time.Time{}, ErrManifest
	}
	return pub, exp, nil
}
func verifyELF(raw []byte) error {
	if len(raw) < 64 || binary.LittleEndian.Uint16(raw[52:]) != 64 {
		return ErrExecutable
	}
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return ErrExecutable
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Version != elf.EV_CURRENT || f.Machine != elf.EM_X86_64 || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return ErrExecutable
	}
	executableEntry := false
	for _, p := range f.Progs {
		if p.Filesz > uint64(len(raw)) || p.Off > uint64(len(raw))-p.Filesz || p.Memsz < p.Filesz {
			return ErrExecutable
		}
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 && p.Filesz > 0 && f.Entry >= p.Vaddr && f.Entry-p.Vaddr < p.Memsz {
			executableEntry = true
		}
	}
	if !executableEntry {
		return ErrExecutable
	}
	return nil
}
