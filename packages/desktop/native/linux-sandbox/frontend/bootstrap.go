// Package frontend contains the unprivileged AppImage sandbox setup frontend.
// Privileged execution uses only fixed OS programs and compiled literal input.
package frontend

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/desktopsandbox"
)

type BuildConstants struct {
	CapsuleSize            int64
	CapsuleSHA512          string
	ManifestSHA512         string
	MaintainerOffset       int64
	MaintainerSize         int64
	MaintainerSHA512       string
	Generation             int64
	CompatibilityEpoch     int64
	SandboxAPI             int64
	PublishedAt, ExpiresAt string
}

//go:embed bootstrap.py
var bootstrapTemplate string

var errBuildConstants = errors.New("invalid sandbox bootstrap build constants")
var utcDate = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)

// BootstrapProgram freezes compile-time initial identities into the code argv,
// rather than executing or reopening a caller-owned program pathname as root.
func BootstrapProgram(c BuildConstants) (string, error) {
	if c.CapsuleSize < 35 || c.CapsuleSize > desktopsandbox.MaxCapsule || c.MaintainerSize < 1 || c.MaintainerSize > desktopsandbox.MaxMaintainer || c.MaintainerOffset < 32 || c.MaintainerOffset > 32+desktopsandbox.MaxManifest || c.MaintainerSize > c.CapsuleSize || c.MaintainerOffset > c.CapsuleSize-c.MaintainerSize || c.Generation < 1 || c.Generation > desktopsandbox.MaxSafeInteger || c.CompatibilityEpoch != 1 || c.SandboxAPI != 1 {
		return "", errBuildConstants
	}
	for _, hash := range []string{c.CapsuleSHA512, c.ManifestSHA512, c.MaintainerSHA512} {
		decoded, err := base64.StdEncoding.Strict().DecodeString(hash)
		if err != nil || len(decoded) != 64 || base64.StdEncoding.EncodeToString(decoded) != hash {
			return "", errBuildConstants
		}
	}
	pub, e1 := time.Parse(time.RFC3339Nano, c.PublishedAt)
	exp, e2 := time.Parse(time.RFC3339Nano, c.ExpiresAt)
	if !utcDate.MatchString(c.PublishedAt) || !utcDate.MatchString(c.ExpiresAt) || e1 != nil || e2 != nil || !exp.After(pub) || exp.Sub(pub) > 93*24*time.Hour {
		return "", errBuildConstants
	}
	encoded, err := json.Marshal(struct {
		CapsuleSize      int64  `json:"capsuleSize"`
		CapsuleSHA512    string `json:"capsuleSha512"`
		ManifestSHA512   string `json:"manifestSha512"`
		MaintainerOffset int64  `json:"maintainerOffset"`
		MaintainerSize   int64  `json:"maintainerSize"`
		MaintainerSHA512 string `json:"maintainerSha512"`
		Generation       int64  `json:"generation"`
		PublishedAt      string `json:"publishedAt"`
		ExpiresAt        string `json:"expiresAt"`
	}{c.CapsuleSize, c.CapsuleSHA512, c.ManifestSHA512, c.MaintainerOffset, c.MaintainerSize, c.MaintainerSHA512, c.Generation, c.PublishedAt, c.ExpiresAt})
	if err != nil || strings.Count(bootstrapTemplate, "__DILLA_CONFIGURATION__") != 1 {
		return "", errBuildConstants
	}
	return strings.Replace(bootstrapTemplate, "__DILLA_CONFIGURATION__", string(encoded), 1), nil
}

// BootstrapArgv is always passed directly to /usr/bin/pkexec, never a shell.
func BootstrapArgv(c BuildConstants) ([]string, error) {
	code, err := BootstrapProgram(c)
	if err != nil {
		return nil, err
	}
	return []string{"--disable-internal-agent", "/usr/bin/python3", "-I", "-S", "-c", code}, nil
}
