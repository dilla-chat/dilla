package desktoplaunch

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Trust struct {
	PublicKeys                                           []string
	Channel, Arch, InitialVersion, InitialManifestSHA512 string
}
type ReleaseContext struct {
	PublicKeys                              []string
	Channel, Platform, Arch, CurrentVersion string
	Now                                     time.Time
}
type PublishedFile struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA512 string `json:"sha512"`
}
type Artifact struct {
	Platform         string         `json:"platform"`
	Arch             string         `json:"arch"`
	URL              string         `json:"url"`
	Size             int64          `json:"size"`
	SHA512           string         `json:"sha512"`
	SparkleSignature string         `json:"sparkleSignature,omitempty"`
	ProgramManifest  *PublishedFile `json:"programManifest,omitempty"`
}
type Release struct {
	Format      int        `json:"format"`
	Version     string     `json:"version"`
	Channel     string     `json:"channel"`
	PublishedAt string     `json:"publishedAt"`
	ExpiresAt   string     `json:"expiresAt"`
	Artifacts   []Artifact `json:"artifacts"`
}
type Slot struct {
	Format         int    `json:"format"`
	Kind           string `json:"kind"`
	Generation     int64  `json:"generation"`
	Version        string `json:"version"`
	ManifestSHA512 string `json:"manifestSha512"`
	Nonce          string `json:"nonce,omitempty"`
	AcceptedAt     int64  `json:"acceptedAt,omitempty"`
}

// VerifyRelease authenticates bytes before parsing, using the same fixed origin,
// channel, version floor and acceptance-time bounds as the desktop updater.
// PublicKeys comes from the launcher's compiled trust, never local JSON.
func VerifyRelease(raw []byte, signature string, ctx ReleaseContext) (*Release, error) {
	if len(raw) > maxDocument || len(ctx.PublicKeys) == 0 {
		return nil, errors.New("no trusted release signature")
	}
	sig, e := decodeDigest(signature)
	if e != nil {
		return nil, e
	}
	valid := false
	for _, key := range ctx.PublicKeys {
		block, rest := pem.Decode([]byte(key))
		if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
			continue
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			continue
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if ok && ed25519.Verify(pub, raw, sig) {
			valid = true
			break
		}
	}
	if !valid {
		return nil, errors.New("invalid release signature")
	}
	if e = validateJSON(raw); e != nil {
		return nil, e
	}
	obj, e := knownObject(raw, []string{"format", "version", "channel", "publishedAt", "expiresAt", "artifacts"}, nil)
	if e != nil {
		return nil, e
	}
	var artifacts []json.RawMessage
	if e = json.Unmarshal(obj["artifacts"], &artifacts); e != nil {
		return nil, e
	}
	for _, rawArtifact := range artifacts {
		artifact, e := knownObject(rawArtifact, []string{"platform", "arch", "url", "size", "sha512"}, []string{"sparkleSignature", "programManifest"})
		if e != nil {
			return nil, e
		}
		if m, ok := artifact["programManifest"]; ok {
			if _, e = exactObject(m, "url", "size", "sha512"); e != nil {
				return nil, e
			}
		}
	}
	var r Release
	if e = json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.Format != 1 || r.Channel != ctx.Channel || !validVersionChannel(r.Version, r.Channel) {
		return nil, errors.New("invalid release identity")
	}
	a, e := versionParts(r.Version)
	if e != nil {
		return nil, e
	}
	b, e := versionParts(ctx.CurrentVersion)
	if e != nil {
		return nil, e
	}
	newer := false
	for i := 0; i < 4; i++ {
		if a[i] != b[i] {
			newer = a[i] > b[i]
			break
		}
	}
	if !newer {
		return nil, errors.New("release is not newer than trusted version floor")
	}
	published, e := parseReleaseDate(r.PublishedAt)
	if e != nil {
		return nil, e
	}
	expires, e := parseReleaseDate(r.ExpiresAt)
	if e != nil {
		return nil, e
	}
	if published.After(ctx.Now.Add(5*time.Minute)) || !expires.After(ctx.Now) || !expires.After(published) || expires.Sub(published) > 93*24*time.Hour {
		return nil, errors.New("expired or invalid release dates")
	}
	if len(r.Artifacts) < 1 || len(r.Artifacts) > 8 {
		return nil, errors.New("invalid release artifact count")
	}
	targets := map[string]bool{}
	for _, artifact := range r.Artifacts {
		if (artifact.Platform != "linux" && artifact.Platform != "win32" && artifact.Platform != "darwin") || (artifact.Arch != "x64" && artifact.Arch != "arm64") {
			return nil, errors.New("invalid release target")
		}
		id := artifact.Platform + "-" + artifact.Arch
		if targets[id] {
			return nil, errors.New("duplicate release target")
		}
		targets[id] = true
		extension := ".exe"
		if artifact.Platform == "linux" {
			extension = ".AppImage"
		}
		if artifact.Platform == "darwin" {
			extension = ".zip"
			if _, e = decodeDigest(artifact.SparkleSignature); e != nil {
				return nil, errors.New("invalid Sparkle archive signature")
			}
		}
		if !validPublishedFile(PublishedFile{artifact.URL, artifact.Size, artifact.SHA512}, r.Version, extension, maxFileSize) {
			return nil, errors.New("invalid release artifact")
		}
		if artifact.Platform == "win32" {
			if artifact.ProgramManifest == nil || !validPublishedFile(*artifact.ProgramManifest, r.Version, ".manifest.json", maxDocument) {
				return nil, errors.New("missing or invalid Windows program manifest")
			}
		} else if artifact.ProgramManifest != nil {
			return nil, errors.New("program manifest on unsupported target")
		}
	}
	if !targets[ctx.Platform+"-"+ctx.Arch] {
		return nil, errors.New("release has no selected target")
	}
	return &r, nil
}

var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{3})?Z$`)

func parseReleaseDate(s string) (time.Time, error) {
	if !datePattern.MatchString(s) {
		return time.Time{}, errors.New("invalid ISO release date")
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return t, e
	}
	return t, nil
}

var assetNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

func validPublishedFile(f PublishedFile, version, extension string, maxSize int64) bool {
	prefix := "https://github.com/jonasthim/dilla/releases/download/desktop-v" + version + "/"
	if !strings.HasPrefix(f.URL, prefix) {
		return false
	}
	name := strings.TrimPrefix(f.URL, prefix)
	if !assetNamePattern.MatchString(name) || strings.Contains(name, "..") || !strings.HasSuffix(name, extension) || f.Size < 1 || f.Size > maxSize {
		return false
	}
	_, e := decodeDigest(f.SHA512)
	return e == nil
}

var noncePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// encoding/json matches struct fields case-insensitively. The JavaScript verifier
// uses exact property names, so reject spelling aliases while allowing publisher
// metadata outside the security-relevant fields.
func knownObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if e := json.Unmarshal(raw, &obj); e != nil {
		return nil, e
	}
	for _, key := range required {
		value, ok := obj[key]
		if !ok || bytes.Equal(value, []byte("null")) {
			return nil, errors.New("missing authenticated release field")
		}
	}
	known := append(append([]string{}, required...), optional...)
	for key, value := range obj {
		for _, name := range known {
			if strings.EqualFold(key, name) && (key != name || bytes.Equal(value, []byte("null"))) {
				return nil, errors.New("ambiguous authenticated release field")
			}
		}
	}
	return obj, nil
}

// VerifySlot authenticates one fixed slot. Callers validate every present slot
// before selecting the highest generation. Uncertain published metadata holds
// startup; it must never authorize older code after account-store migration.
func VerifySlot(raw []byte, trust Trust, provenanceRoot string, now time.Time) (*Slot, error) {
	if e := validateJSON(raw); e != nil {
		return nil, e
	}
	var s Slot
	if e := json.Unmarshal(raw, &s); e != nil {
		return nil, e
	}
	if s.Format != 1 || !validVersionChannel(s.Version, trust.Channel) {
		return nil, errors.New("invalid committed slot identity")
	}
	if _, e := decodeDigest(s.ManifestSHA512); e != nil {
		return nil, e
	}
	if s.Kind == "initial" {
		if _, e := exactObject(raw, "format", "kind", "generation", "version", "manifestSha512"); e != nil {
			return nil, e
		}
		if s.Generation != 0 || s.Version != trust.InitialVersion || s.ManifestSHA512 != trust.InitialManifestSHA512 {
			return nil, errors.New("initial slot does not match compiled trust")
		}
		return &s, nil
	}
	if s.Kind != "release" {
		return nil, errors.New("invalid committed slot kind")
	}
	if _, e := exactObject(raw, "format", "kind", "generation", "version", "manifestSha512", "nonce", "acceptedAt"); e != nil {
		return nil, e
	}
	if s.Generation < 1 || s.Generation > maxSafeInteger || !noncePattern.MatchString(s.Nonce) || s.AcceptedAt < 1 || s.AcceptedAt > maxSafeInteger || s.AcceptedAt > now.Add(5*time.Minute).UnixMilli() {
		return nil, errors.New("invalid committed slot bounds")
	}
	base, e := filepath.Abs(filepath.Join(provenanceRoot, "startup-"+s.Nonce))
	if e != nil {
		return nil, e
	}
	handles, e := pinAncestors(base)
	if e != nil {
		return nil, e
	}
	defer func() {
		for i := len(handles) - 1; i >= 0; i-- {
			_ = handles[i].Close()
		}
	}()
	root, e := os.OpenRoot(base)
	if e != nil {
		return nil, e
	}
	defer func() { _ = root.Close() }()
	guard, e := openPinned(base, root, ".", true)
	if e != nil {
		return nil, e
	}
	handles = append(handles, guard)
	read := func(name string, limit int64) ([]byte, error) {
		f, err := openPinned(base, root, name, false)
		if err != nil {
			return nil, err
		}
		handles = append(handles, f)
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() < 1 || info.Size() > limit {
			return nil, errors.New("invalid retained provenance size")
		}
		data, err := io.ReadAll(io.LimitReader(f, limit+1))
		if err != nil {
			return nil, err
		}
		if len(data) > int(limit) {
			return nil, errors.New("retained provenance exceeds bound")
		}
		return data, nil
	}
	descriptor, e := read("release.json", maxDocument)
	if e != nil {
		return nil, e
	}
	signature, e := read("release.json.sig", 1024)
	if e != nil {
		return nil, e
	}
	r, e := VerifyRelease(descriptor, strings.TrimSpace(string(signature)), ReleaseContext{PublicKeys: trust.PublicKeys, Channel: trust.Channel, Platform: "win32", Arch: trust.Arch, CurrentVersion: trust.InitialVersion, Now: time.UnixMilli(s.AcceptedAt)})
	if e != nil {
		return nil, e
	}
	if r.Version != s.Version {
		return nil, errors.New("slot version does not match authenticated release")
	}
	var artifact *Artifact
	for i := range r.Artifacts {
		if r.Artifacts[i].Platform == "win32" && r.Artifacts[i].Arch == trust.Arch {
			artifact = &r.Artifacts[i]
			break
		}
	}
	if artifact == nil || artifact.ProgramManifest == nil || artifact.ProgramManifest.SHA512 != s.ManifestSHA512 {
		return nil, errors.New("slot manifest does not match authenticated release")
	}
	// Slot publication happens only after this complete record was flushed.
	// A staged slot from a failed/interrupted transaction is not committed.
	journalBytes, e := read("journal.json", 1024)
	if e != nil {
		return nil, e
	}
	journal, e := ParseJournal(journalBytes, s.Nonce)
	if e != nil {
		return nil, e
	}
	if journal.Phase != "complete" || journal.Version != s.Version || journal.ChildPID == nil {
		return nil, errors.New("release slot has no matching completed journal")
	}
	f, e := openPinned(base, root, "update.exe", false)
	if e != nil {
		return nil, e
	}
	handles = append(handles, f)
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if info.Size() != artifact.Size {
		return nil, errors.New("retained installer size mismatch")
	}
	h := sha512.New()
	n, e := io.Copy(h, io.LimitReader(f, artifact.Size+1))
	if e != nil {
		return nil, e
	}
	expected, _ := decodeDigest(artifact.SHA512)
	if n != artifact.Size || subtle.ConstantTimeCompare(h.Sum(nil), expected) != 1 {
		return nil, errors.New("retained installer digest mismatch")
	}
	return &s, nil
}
