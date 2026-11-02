package frontend

import (
	"crypto/sha512"
	"encoding/base64"
	"strings"
	"testing"
)

func testHash(data string) string {
	sum := sha512.Sum512([]byte(data))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func validConstants() BuildConstants {
	return BuildConstants{CapsuleSize: 4096, CapsuleSHA512: testHash("capsule"), ManifestSHA512: testHash("manifest"), MaintainerOffset: 512, MaintainerSize: 1024, MaintainerSHA512: testHash("maintainer"), Generation: 1, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: "2026-11-02T00:00:00Z", ExpiresAt: "2026-11-03T00:00:00Z"}
}

func TestBootstrapFreezesOnlyFixedSystemProgramAndLiteralData(t *testing.T) {
	code, err := BootstrapProgram(validConstants())
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`/usr/lib/dilla/appimage`, `OWNER_UID = 0`, `OWNER_GID = 0`, `INPUT_TIMEOUT = 30.0`, `os.execve`, `bootstrap-anchor.json`, `bootstrap-capsule`, `bootstrap-maintainer`, `os.fsync`, `os.O_NOFOLLOW`} {
		if !strings.Contains(code, required) {
			t.Fatalf("missing fixed bootstrap contract %q", required)
		}
	}
	argv, err := BootstrapArgv(validConstants())
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"--disable-internal-agent", "/usr/bin/python3", "-I", "-S", "-c", code}
	if len(argv) != len(expected) {
		t.Fatal("unexpected bootstrap arguments")
	}
	for i := range expected {
		if argv[i] != expected[i] {
			t.Fatal("changed fixed bootstrap argv")
		}
	}
	if strings.Contains(code, "getenv(") || strings.Contains(code, "eval(") || strings.Contains(code, "subprocess") || strings.Contains(code, "--no-sandbox") {
		t.Fatal("bootstrap introduced a dynamic execution/trust boundary")
	}
}

func TestBootstrapRejectsInvalidBuildConstantsBeforeEmittingPrivilegedCode(t *testing.T) {
	for _, kind := range []string{"capsule size", "offset overflow", "maintainer bound", "digest", "generation", "epoch", "api", "date", "non UTC", "too long"} {
		t.Run(kind, func(t *testing.T) {
			c := validConstants()
			switch kind {
			case "capsule size":
				c.CapsuleSize = 49 << 20
			case "offset overflow":
				c.MaintainerOffset = 9223372036854775807
			case "maintainer bound":
				c.MaintainerSize = 33 << 20
			case "digest":
				c.CapsuleSHA512 = "PRIVATE_SECRET"
			case "generation":
				c.Generation = 0
			case "epoch":
				c.CompatibilityEpoch = 2
			case "api":
				c.SandboxAPI = 2
			case "date":
				c.ExpiresAt = "2026-02-30T00:00:00Z"
			case "non UTC":
				c.PublishedAt = "2026-11-02T00:00:00+00:00"
			case "too long":
				c.ExpiresAt = "2027-11-03T00:00:00Z"
			}
			if _, err := BootstrapProgram(c); err == nil || strings.Contains(err.Error(), "PRIVATE_SECRET") {
				t.Fatal("invalid constants escaped into bootstrap")
			}
		})
	}
}
