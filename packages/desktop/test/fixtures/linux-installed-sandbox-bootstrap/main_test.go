package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/packages/desktop/native/linux-sandbox/frontend"
)

func testELF(tag byte) []byte {
	b := make([]byte, 129)
	copy(b, []byte{127, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], 62)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint64(b[24:], 0x400080)
	binary.LittleEndian.PutUint64(b[32:], 64)
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[56:], 1)
	binary.LittleEndian.PutUint32(b[64:], 1)
	binary.LittleEndian.PutUint32(b[68:], 5)
	binary.LittleEndian.PutUint64(b[80:], 0x400000)
	binary.LittleEndian.PutUint64(b[88:], 0x400000)
	binary.LittleEndian.PutUint64(b[96:], 129)
	binary.LittleEndian.PutUint64(b[104:], 129)
	binary.LittleEndian.PutUint64(b[112:], 4096)
	b[128] = tag
	return b
}
func inputFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	pub, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	helper, manager := testELF(0xc3), testELF(0xf4)
	sha := sha256.Sum256(helper)
	now := time.Now().UTC()
	config := publicConfig{GoVersion: "go1.27.3", Generation: 1, PublishedAt: now.Add(-time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), PublicKeys: []string{base64.StdEncoding.EncodeToString(pub)}}
	config.Helper.Size = int64(len(helper))
	config.Helper.SHA512 = hash(helper)
	config.Helper.SHA256 = hex.EncodeToString(sha[:])
	config.Helper.Version = "ephemeral-ci"
	cfg, e := json.Marshal(config)
	if e != nil {
		t.Fatal(e)
	}
	manifest, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-linux-sandbox", "generation": 1, "platform": "linux", "arch": "x64", "compatibilityEpoch": 1, "sandboxApi": 1, "publishedAt": config.PublishedAt, "expiresAt": config.ExpiresAt, "maintainer": map[string]any{"size": len(manager), "sha512": hash(manager)}, "helper": map[string]any{"size": len(helper), "sha512": hash(helper)}})
	raw := make([]byte, 0, 32+len(manifest)+len(manager)+len(helper))
	raw = append(raw, make([]byte, 32)...)
	copy(raw, "DLSBOX1\x00")
	binary.LittleEndian.PutUint32(raw[8:], uint32(len(manifest)))
	binary.LittleEndian.PutUint64(raw[16:], uint64(len(manager)))
	binary.LittleEndian.PutUint64(raw[24:], uint64(len(helper)))
	raw = append(raw, manifest...)
	raw = append(raw, manager...)
	raw = append(raw, helper...)
	return raw, cfg
}
func TestExporterUsesExactProductionEmitterAndBoundedUnsignedBytes(t *testing.T) {
	raw, cfg := inputFixture(t)
	result, e := export(raw, cfg, hash(raw), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	expected, e := frontend.BootstrapArgv(result.Constants)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(result.Argv)
	b, _ := json.Marshal(expected)
	if !bytes.Equal(a, b) || result.CapsuleSHA512 != hash(raw) {
		t.Fatal("export did not use exact frontend argv")
	}
}
func TestExporterRejectsChangedOrUnboundCapsulesBeforeProducingAuthority(t *testing.T) {
	raw, cfg := inputFixture(t)
	for _, kind := range []string{"digest", "helper", "signature", "trailing", "config"} {
		t.Run(kind, func(t *testing.T) {
			changed := bytes.Clone(raw)
			config := bytes.Clone(cfg)
			expected := hash(raw)
			switch kind {
			case "digest":
				expected = hash([]byte("other"))
			case "helper":
				changed[len(changed)-1] ^= 1
				expected = hash(changed)
			case "signature":
				binary.LittleEndian.PutUint32(changed[12:], 64)
				expected = hash(changed)
			case "trailing":
				changed = append(changed, 0)
				expected = hash(changed)
			case "config":
				config = []byte(`{"generation":1}`)
			}
			if _, e := export(changed, config, expected, time.Now()); e == nil {
				t.Fatal("invalid initial capsule admitted")
			}
		})
	}
}
