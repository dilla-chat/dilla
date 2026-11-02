//go:build windows

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var initialDiagnosticPayload = flag.String("dilla-initial-payload", "", "private acceptance payload")
var initialDiagnosticACLOnly = flag.Bool("dilla-acl-only", false, "private acceptance ACL probe")

// Invoked only in the compiled acceptance test binary, while the failing NSIS
// still holds its actual extracted payload, passed by its fixed fixture macro.
// No shipping argument, environment override or launcher ABI is introduced.
func TestInitialRetryNativeDiagnostic(t *testing.T) {
	request := os.Getenv("DILLA_INITIAL_RETRY_DIAGNOSTIC_REQUEST")
	if request == "" {
		t.Skip("private native acceptance diagnostic")
	}
	info, err := os.Lstat(request)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 131072 {
		t.Fatal("invalid private diagnostic request")
	}
	raw, err := os.ReadFile(request)
	if err != nil || len(raw) > 131072 {
		t.Fatal("cannot read private diagnostic request")
	}
	var input struct {
		Config json.RawMessage `json:"config"`
		Report string          `json:"report"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || !filepath.IsAbs(input.Report) || filepath.Dir(input.Report) != filepath.Dir(request) || filepath.Base(input.Report) != "initial-retry-native.json" {
		t.Fatal("invalid private diagnostic schema")
	}
	result := initialRetryDiagnostic{1, "request", "other-validation", 0}
	cfg, err := parseBuildConfig(base64.StdEncoding.EncodeToString(input.Config))
	if err == nil {
		if *initialDiagnosticACLOnly && filepath.IsAbs(*initialDiagnosticPayload) {
			result = diagnosePayloadACL(cfg, *initialDiagnosticPayload)
		} else {
			var l layout
			l, err = maintenanceLayout(cfg)
			if err != nil {
				result = initialDiagnosticFailure("layout", err)
			} else {
				if !filepath.IsAbs(*initialDiagnosticPayload) {
					result = initialRetryDiagnostic{1, "payload-directory", "other-validation", 0}
				} else {
					result = diagnoseInitialRetry(l, cfg, *initialDiagnosticPayload)
				}
			}
		}
	}
	// Exclusivity prevents this helper from replacing any unexpected file. Its
	// sole output is a fixed static classification in this private test directory.
	file, err := os.OpenFile(input.Report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("cannot create private diagnostic report")
	}
	defer func() { _ = file.Close() }()
	if json.NewEncoder(file).Encode(result) != nil {
		t.Fatal("cannot write private diagnostic report")
	}
}
