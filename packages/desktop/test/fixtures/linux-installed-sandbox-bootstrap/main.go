// Public-only CI exporter; it has no privileged execution or caller-path API.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/jonasthim/dilla/internal/desktopsandbox"
	"github.com/jonasthim/dilla/packages/desktop/native/linux-sandbox/frontend"
)

// Frozen by the unprivileged driver from the finalized package binding and
// its exact PUBLIC component configuration. No runtime environment override.
var expectedCapsuleSHA512, compiledPublicConfiguration string
var errExport = errors.New("invalid installed sandbox bootstrap inputs")

type publicConfig struct {
	GoVersion   string   `json:"goVersion"`
	Generation  int64    `json:"generation"`
	PublishedAt string   `json:"publishedAt"`
	ExpiresAt   string   `json:"expiresAt"`
	PublicKeys  []string `json:"publicKeys"`
	Helper      struct {
		Version string `json:"version"`
		Size    int64  `json:"size"`
		SHA512  string `json:"sha512"`
		SHA256  string `json:"sha256"`
	} `json:"helper"`
}
type exported struct {
	Format           int                     `json:"format"`
	CapsuleSHA512    string                  `json:"capsuleSha512"`
	ManifestSHA512   string                  `json:"manifestSha512"`
	MaintainerSHA512 string                  `json:"maintainerSha512"`
	HelperSHA512     string                  `json:"helperSha512"`
	Generation       int64                   `json:"generation"`
	Constants        frontend.BuildConstants `json:"constants"`
	Argv             []string                `json:"argv"`
}

func hash(b []byte) string { sum := sha512.Sum512(b); return base64.StdEncoding.EncodeToString(sum[:]) }
func export(raw, configuration []byte, expected string, now time.Time) (exported, error) {
	bad := func() (exported, error) { return exported{}, errExport }
	if len(raw) > desktopsandbox.MaxCapsule || len(configuration) == 0 || len(configuration) > 16384 || hash(raw) != expected {
		return bad()
	}
	var cfg publicConfig
	decoder := json.NewDecoder(bytes.NewReader(configuration))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF || !regexp.MustCompile(`^go[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(cfg.GoVersion) || cfg.Helper.Version == "" || cfg.Generation < 1 || len(cfg.PublicKeys) < 1 || len(cfg.PublicKeys) > 8 {
		return bad()
	}
	c, e := desktopsandbox.ParseCapsule(bytes.NewReader(raw), desktopsandbox.Limits{})
	if e != nil || len(c.Signature) != 0 {
		return bad()
	}
	sha := sha256.Sum256(c.Helper)
	if hex.EncodeToString(sha[:]) != cfg.Helper.SHA256 {
		return bad()
	}
	keys := make([]ed25519.PublicKey, len(cfg.PublicKeys))
	for i, value := range cfg.PublicKeys {
		key, e := base64.StdEncoding.Strict().DecodeString(value)
		if e != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != value {
			return bad()
		}
		keys[i] = ed25519.PublicKey(key)
	}
	trust := desktopsandbox.Trust{PublicKeys: keys, Initial: &desktopsandbox.InitialTrust{Generation: cfg.Generation, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: cfg.PublishedAt, ExpiresAt: cfg.ExpiresAt, Helper: desktopsandbox.Component{Size: cfg.Helper.Size, SHA512: cfg.Helper.SHA512}, SelfMaintainerSHA512: hash(c.Maintainer)}}
	verified, e := desktopsandbox.VerifyCapsule(c, trust, desktopsandbox.Receipt{}, now)
	if e != nil {
		return bad()
	}
	constants := frontend.BuildConstants{CapsuleSize: int64(len(raw)), CapsuleSHA512: expected, ManifestSHA512: hash(c.Manifest), MaintainerOffset: 32 + int64(binary.LittleEndian.Uint32(raw[8:12])), MaintainerSize: int64(len(c.Maintainer)), MaintainerSHA512: hash(c.Maintainer), Generation: verified.Manifest().Generation, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: cfg.PublishedAt, ExpiresAt: cfg.ExpiresAt}
	argv, e := frontend.BootstrapArgv(constants)
	if e != nil {
		return bad()
	}
	return exported{Format: 1, CapsuleSHA512: expected, ManifestSHA512: hash(c.Manifest), MaintainerSHA512: hash(c.Maintainer), HelperSHA512: hash(c.Helper), Generation: cfg.Generation, Constants: constants, Argv: argv}, nil
}
func main() {
	if os.Geteuid() == 0 || len(os.Args) != 1 {
		os.Exit(2)
	}
	configuration, e := base64.StdEncoding.Strict().DecodeString(compiledPublicConfiguration)
	if e != nil {
		os.Exit(2)
	}
	raw, e := io.ReadAll(io.LimitReader(os.Stdin, desktopsandbox.MaxCapsule+1))
	if e != nil {
		os.Exit(2)
	}
	result, e := export(raw, configuration, expectedCapsuleSHA512, time.Now())
	if e != nil {
		_, _ = os.Stderr.WriteString("Installed sandbox bootstrap input refused.\n")
		os.Exit(2)
	}
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		os.Exit(2)
	}
}
