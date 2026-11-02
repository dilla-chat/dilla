package desktopsandbox

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProtectedRecordDecodersRejectAmbiguousUnsafeOrUnboundedState(t *testing.T) {
	c, _, _ := fixture(t, 1)
	unsigned := copyCapsule(c)
	unsigned.Signature = nil
	anchor := anchorFor(t, unsigned)
	receipt := receiptFor(t, c)
	pending := pendingFor(t, c, nil)
	removal := RemovalIntent{Format: 1, Operation: "remove", MaximumGeneration: 1, AcceptedAt: testNow.UnixMilli(), PreviousReceiptSHA512: receipt.SHA512(), CurrentMaintainer: identity(c.Maintainer, "12"), CurrentHelper: func() *FileIdentity { i := identity(c.Helper, "13"); return &i }()}
	cases := []struct {
		name   string
		value  any
		decode func([]byte) error
		limit  int
	}{
		{"anchor", anchor, func(b []byte) error { _, e := ParseBootstrapAnchor(b); return e }, 4096},
		{"receipt", receipt, func(b []byte) error { _, e := ParseReceipt(b); return e }, 128 << 10},
		{"install", pending, func(b []byte) error { _, e := ParseInstallIntent(b); return e }, 128 << 10},
		{"remove", removal, func(b []byte) error { _, e := ParseRemovalIntent(b); return e }, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.value)
			if err := tc.decode(raw); err != nil {
				t.Fatal(err)
			}
			for _, bad := range [][]byte{append([]byte(`{"PRIVATE_SECRET":1,`), raw[1:]...), bytes.Replace(raw, []byte(`"format":1`), []byte(`"Format":1`), 1), bytes.Replace(raw, []byte(`"format":1`), []byte(`"format":1,"\u0066ormat":1`), 1), bytes.Replace(raw, []byte(`"format":1`), []byte(`"format":1e0`), 1), bytes.Replace(raw, []byte(`"acceptedAt":`), []byte(`"acceptedAt":-`), 1), append(raw, []byte(` {}`)...), bytes.Repeat([]byte{' '}, tc.limit+1)} {
				if err := tc.decode(bad); err == nil {
					t.Fatal("accepted corrupt protected record")
				} else if strings.Contains(err.Error(), "PRIVATE_SECRET") {
					t.Fatal("record error leaked input")
				}
			}
		})
	}
	raw, _ := json.Marshal(anchor)
	for _, bad := range [][]byte{bytes.Replace(raw, []byte(`"inode":"100"`), []byte(`"inode":100`), 1), bytes.Replace(raw, []byte(`"inode":"100"`), []byte(`"inode":"0100"`), 1), bytes.Replace(raw, []byte(`"inode":"100"`), []byte(`"inode":"18446744073709551616"`), 1), bytes.Replace(raw, []byte(`"device":"1"`), []byte(`"device":"-1"`), 1)} {
		if _, e := ParseBootstrapAnchor(bad); e == nil {
			t.Fatal("accepted ambiguous inode identity")
		}
	}
}

func TestProtectedRecordsKeepExactSignedBytesAndInvalidateChangedRecordDigest(t *testing.T) {
	c, trust, key := fixture(t, 1)
	c.Manifest = append(c.Manifest, ' ', '\n')
	resign(&c, key)
	r := receiptFor(t, c)
	raw, _ := json.Marshal(r)
	parsed, err := ParseReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := base64.StdEncoding.DecodeString(parsed.Manifest)
	if !bytes.Equal(manifest, c.Manifest) {
		t.Fatal("signed bytes were compacted")
	}
	if parsed.SHA512() != digest(raw) {
		t.Fatal("wrong exact receipt digest")
	}
	if err := VerifyReceipt(parsed, trust, testNow); err != nil {
		t.Fatal(err)
	}
	parsed.MaximumGeneration = 2
	if parsed.SHA512() != "" {
		t.Fatal("stale protected-state digest survived mutation")
	}
}

func TestPendingReceiptChainBindsExactExistingBytes(t *testing.T) {
	old, trust, key := fixture(t, 1)
	r := receiptFor(t, old)
	next := withGeneration(t, old, key, 2)
	p := pendingFor(t, next, &r)
	tr := trust
	tr.Pending = p
	if _, err := VerifyCapsule(next, tr, r, testNow); err != nil {
		t.Fatal(err)
	}
	pp := *p
	wrong := digest([]byte("different receipt bytes"))
	pp.PreviousReceiptSHA512 = &wrong
	raw, _ := json.Marshal(pp)
	parsed, e := ParseInstallIntent(raw)
	if e != nil {
		t.Fatal(e)
	}
	tr.Pending = &parsed
	if _, err := VerifyCapsule(next, tr, r, testNow); err == nil {
		t.Fatal("pending record used unrelated previous receipt")
	}
}

func TestEmptyInitialSignatureHasExactStringEncodingNotNull(t *testing.T) {
	c, _, _ := fixture(t, 1)
	r := receiptFor(t, c)
	r.Authorization = "initial-digest"
	r.Signature = ""
	raw, _ := json.Marshal(r)
	if _, err := ParseReceipt(raw); err != nil {
		t.Fatal(err)
	}
	null := bytes.Replace(raw, []byte(`"signature":""`), []byte(`"signature":null`), 1)
	if _, err := ParseReceipt(null); err == nil {
		t.Fatal("null silently became empty unsigned signature")
	}
	p := pendingFor(t, c, nil)
	p.Authorization = "initial-anchor"
	p.Signature = ""
	raw, _ = json.Marshal(p)
	if _, err := ParseInstallIntent(raw); err != nil {
		t.Fatal(err)
	}
	null = bytes.Replace(raw, []byte(`"signature":""`), []byte(`"signature":null`), 1)
	if _, err := ParseInstallIntent(null); err == nil {
		t.Fatal("null silently selected initial-anchor signature mode")
	}
}

func TestRemovedReceiptKeepsHistoricalComponentAdmissionAndLaterOperationTime(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	r := receiptFor(t, c)
	raw, _ := json.Marshal(r)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	wire["state"] = "removed"
	wire["completedOperation"] = "remove"
	wire["lastOperationAt"] = testNow.Add(48 * time.Hour).UnixMilli()
	raw, _ = json.Marshal(wire)
	removed, err := ParseReceipt(raw)
	if err != nil {
		t.Fatal("removed receipt needs a later operation time without re-admitting expired code", err)
	}
	if removed.AcceptedAt != r.AcceptedAt {
		t.Fatal("component admission time changed during removal")
	}
	if err = VerifyReceipt(removed, trust, testNow.Add(48*time.Hour)); err != nil {
		t.Fatal("offline removal rechecked component expiry at operation time", err)
	}
	for _, bad := range []any{nil, float64(0), float64(r.AcceptedAt - 1)} {
		wire["lastOperationAt"] = bad
		raw, _ = json.Marshal(wire)
		if _, err = ParseReceipt(raw); err == nil {
			t.Fatal("invalid operation clock floor accepted")
		}
	}
	delete(wire, "lastOperationAt")
	raw, _ = json.Marshal(wire)
	if _, err = ParseReceipt(raw); err == nil {
		t.Fatal("old receipt silently lost operation clock floor")
	}
}
