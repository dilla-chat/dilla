//go:build linux && cgo

package frontend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var errProbeReport = errors.New("invalid sandbox probe response")

type probeReport struct {
	Format          int    `json:"format"`
	Kind            string `json:"kind"`
	Nonce           string `json:"nonce"`
	MainPID         int    `json:"mainPid"`
	RendererPID     int    `json:"rendererPid"`
	Sandboxed       bool   `json:"sandboxed"`
	ContextIsolated bool   `json:"contextIsolated"`
}

func parseProbeReport(raw []byte, nonce string, mainPID int) (probeReport, error) {
	var result probeReport
	if len(raw) < 2 || len(raw) > 4096 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 {
		return result, errProbeReport
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return result, errProbeReport
	}
	allowed := map[string]bool{"format": true, "kind": true, "nonce": true, "mainPid": true, "rendererPid": true, "sandboxed": true, "contextIsolated": true}
	seen := map[string]bool{}
	for d.More() {
		k, e := d.Token()
		key, ok := k.(string)
		if e != nil || !ok || !allowed[key] || seen[key] {
			return result, errProbeReport
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return result, errProbeReport
		}
	}
	token, e = d.Token()
	if e != nil || token != json.Delim('}') || len(seen) != len(allowed) {
		return result, errProbeReport
	}
	if _, e = d.Token(); e != io.EOF {
		return result, errProbeReport
	}
	if json.Unmarshal(raw, &result) != nil || result.Format != 1 || result.Kind != "dilla-sandbox-probe" || result.Nonce != nonce || result.MainPID != mainPID || result.RendererPID < 2 || result.RendererPID == mainPID || !result.Sandboxed || !result.ContextIsolated {
		return probeReport{}, errProbeReport
	}
	return result, nil
}
