package desktopsandbox

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func digest(data []byte) string {
	h := sha512.Sum512(data)
	return base64.StdEncoding.EncodeToString(h[:])
}
func executable(tag byte) []byte {
	b := make([]byte, 129)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
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
	binary.LittleEndian.PutUint64(b[96:], uint64(len(b)))
	binary.LittleEndian.PutUint64(b[104:], uint64(len(b)))
	binary.LittleEndian.PutUint64(b[112:], 4096)
	b[128] = tag
	return b
}
func fixture(t *testing.T, generation int64) (Capsule, Trust, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := Capsule{Maintainer: executable(0xc3), Helper: executable(0xf4)}
	m := map[string]any{"format": 1, "kind": "dilla-linux-sandbox", "generation": generation, "platform": "linux", "arch": "x64", "compatibilityEpoch": 1, "sandboxApi": 1, "publishedAt": testNow.Add(-time.Hour).Format(time.RFC3339), "expiresAt": testNow.Add(24 * time.Hour).Format(time.RFC3339), "maintainer": map[string]any{"size": len(c.Maintainer), "sha512": digest(c.Maintainer)}, "helper": map[string]any{"size": len(c.Helper), "sha512": digest(c.Helper)}}
	c.Manifest, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	resign(&c, priv)
	return c, Trust{PublicKeys: []ed25519.PublicKey{pub}}, priv
}
func resign(c *Capsule, key ed25519.PrivateKey) {
	c.Signature = ed25519.Sign(key, append([]byte("Dilla Linux sandbox capsule v1\x00"), c.Manifest...))
}
func frame(c Capsule) []byte {
	var b bytes.Buffer
	b.WriteString("DLSBOX1\x00")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(c.Manifest)))
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(c.Signature)))
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(c.Maintainer)))
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(c.Helper)))
	b.Write(c.Manifest)
	b.Write(c.Signature)
	b.Write(c.Maintainer)
	b.Write(c.Helper)
	return b.Bytes()
}
func mutateManifest(t *testing.T, c *Capsule, key ed25519.PrivateKey, mutate func(map[string]any)) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	c.Manifest, _ = json.Marshal(m)
	resign(c, key)
}

func TestCapsuleExactFramingAndDedicatedSignature(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	raw := frame(c)
	parsed, err := ParseCapsule(bytes.NewReader(raw), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyCapsule(parsed, trust, Receipt{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Manifest().Generation != 1 || !bytes.Equal(verified.Helper(), c.Helper) || !bytes.Equal(verified.Maintainer(), c.Maintainer) {
		t.Fatal("verified different component bytes")
	}
	// Neither input aliases nor returned slices can alter admitted program bytes.
	parsed.Helper[128] ^= 1
	got := verified.Helper()
	got[128] ^= 1
	if !bytes.Equal(verified.Helper(), c.Helper) {
		t.Fatal("verified bytes remained mutable")
	}
	if verified.ManifestSHA512() != digest(c.Manifest) {
		t.Fatal("exact manifest bytes were not bound")
	}
}

func TestCapsuleRejectsIncompleteExcessOrInvalidDeclaredLengths(t *testing.T) {
	c, _, _ := fixture(t, 1)
	raw := frame(c)
	cases := map[string][]byte{"bad magic": append([]byte("NOTBOX1\x00"), raw[8:]...), "short header": raw[:31], "missing last byte": raw[:len(raw)-1], "trailing bytes": append(append([]byte(nil), raw...), 0)}
	for name, change := range map[string]func([]byte){"zero manifest": func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 0) }, "manifest too large": func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 65537) }, "signature63": func(b []byte) { binary.LittleEndian.PutUint32(b[12:], 63) }, "maintainer overflow": func(b []byte) { binary.LittleEndian.PutUint64(b[16:], ^uint64(0)) }, "helper too large": func(b []byte) { binary.LittleEndian.PutUint64(b[24:], 8*1024*1024+1) }} {
		b := append([]byte(nil), raw...)
		change(b)
		cases[name] = b
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCapsule(bytes.NewReader(b), Limits{}); err == nil {
				t.Fatal("accepted malformed framing")
			}
		})
	}
	if _, err := ParseCapsule(bytes.NewReader(raw), Limits{Manifest: 16}); err == nil {
		t.Fatal("ignored stricter caller bound")
	}
	if _, err := ParseCapsule(bytes.NewReader(raw), Limits{Manifest: 65537}); err == nil {
		t.Fatal("caller enlarged security limit")
	}
	failing := io.MultiReader(bytes.NewReader(raw), errorReader{})
	if _, err := ParseCapsule(failing, Limits{}); err == nil {
		t.Fatal("ignored terminal reader error")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("owned input failure") }

func TestCapsuleAuthenticatesExactManifestAndBothPrograms(t *testing.T) {
	for _, kind := range []string{"manifest", "maintainer", "helper", "signature", "other key", "wrong domain", "unsigned resident"} {
		t.Run(kind, func(t *testing.T) {
			c, trust, key := fixture(t, 2)
			switch kind {
			case "manifest":
				c.Manifest = append(c.Manifest, ' ')
			case "maintainer":
				c.Maintainer[128] ^= 1
			case "helper":
				c.Helper[128] ^= 1
			case "signature":
				c.Signature[0] ^= 1
			case "other key":
				other, _, _ := ed25519.GenerateKey(rand.Reader)
				trust.PublicKeys = []ed25519.PublicKey{other}
			case "wrong domain":
				c.Signature = ed25519.Sign(key, c.Manifest)
			case "unsigned resident":
				c.Signature = nil
			}
			if _, err := VerifyCapsule(c, trust, Receipt{}, testNow); err == nil {
				t.Fatal("accepted unauthorized component")
			}
		})
	}
}

func TestManifestStrictSchemaAndRuntimeIdentity(t *testing.T) {
	changes := map[string]func(map[string]any){"wrong platform": func(m map[string]any) { m["platform"] = "win32" }, "arm64 unsupported": func(m map[string]any) { m["arch"] = "arm64" }, "future epoch": func(m map[string]any) { m["compatibilityEpoch"] = 2 }, "future API": func(m map[string]any) { m["sandboxApi"] = 2 }, "negative generation": func(m map[string]any) { m["generation"] = -1 }, "unsafe integer": func(m map[string]any) { m["generation"] = float64(9007199254740992) }, "unknown key": func(m map[string]any) { m["destination"] = "/owned/forbidden" }, "case alias": func(m map[string]any) { m["Format"] = m["format"]; delete(m, "format") }, "component unknown": func(m map[string]any) { m["helper"].(map[string]any)["path"] = "/owned/forbidden" }, "wrong size": func(m map[string]any) { m["helper"].(map[string]any)["size"] = 1 }, "noncanonical digest": func(m map[string]any) {
		m["helper"].(map[string]any)["sha512"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB=="
	}}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c, trust, key := fixture(t, 1)
			mutateManifest(t, &c, key, change)
			if _, err := VerifyCapsule(c, trust, Receipt{}, testNow); err == nil {
				t.Fatal("accepted invalid signed manifest")
			}
		})
	}
	for _, raw := range []string{`{"format":1,"format":1}`, `{"helper":{"size":1,"size":1}}`, string([]byte{0xff, '{', '}'})} {
		t.Run("duplicate-or-invalid-JSON", func(t *testing.T) {
			c, trust, key := fixture(t, 1)
			c.Manifest = []byte(raw)
			resign(&c, key)
			if _, err := VerifyCapsule(c, trust, Receipt{}, testNow); err == nil {
				t.Fatal("accepted ambiguous JSON")
			}
		})
	}
}

func TestRejectsSignedWrongELFArchitectureTypeAndBounds(t *testing.T) {
	for _, target := range []string{"maintainer", "helper"} {
		for _, field := range []string{"class", "machine", "type", "program range", "non ELF"} {
			t.Run(target+"/"+field, func(t *testing.T) {
				c, trust, key := fixture(t, 1)
				b := c.Helper
				if target == "maintainer" {
					b = c.Maintainer
				}
				switch field {
				case "class":
					b[4] = 1
				case "machine":
					binary.LittleEndian.PutUint16(b[18:], 183)
				case "type":
					binary.LittleEndian.PutUint16(b[16:], 1)
				case "program range":
					binary.LittleEndian.PutUint64(b[96:], ^uint64(0))
				case "non ELF":
					b[0] = 0
				}
				mutateManifest(t, &c, key, func(m map[string]any) { m[target].(map[string]any)["sha512"] = digest(b) })
				if _, err := VerifyCapsule(c, trust, Receipt{}, testNow); err == nil {
					t.Fatal("accepted a signed incompatible executable")
				}
			})
		}
	}
}

func TestUnauthenticatedInputNeverReachesExecutableDecoder(t *testing.T) {
	c, trust, key := fixture(t, 1)
	c.Helper[0] = 0
	mutateManifest(t, &c, key, func(m map[string]any) { m["helper"].(map[string]any)["sha512"] = digest(c.Helper) })
	c.Signature[0] ^= 1
	if _, err := VerifyCapsule(c, trust, Receipt{}, testNow); !errors.Is(err, ErrAuthentication) {
		t.Fatal("unauthorized executable reached parser", err)
	}
}
