package desktoplaunch

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digest(data []byte) string {
	sum := sha512.Sum512(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func manifestBytes(files []map[string]any) []byte {
	data, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-desktop-program", "version": "1.2.3", "channel": "stable", "platform": "win32", "arch": "x64", "files": files})
	return data
}

func fileEntry(path string, contents string) map[string]any {
	return map[string]any{"path": path, "size": len(contents), "sha512": digest([]byte(contents))}
}

var stableContext = ManifestContext{Version: "1.2.3", Channel: "stable", Arch: "x64"}

// Removing digest/context binding would authorize a manifest for another release.
func TestVerifyManifestBindsExactBytesAndIdentity(t *testing.T) {
	data := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program"), fileEntry("resources/app.asar", "app")})
	m, err := VerifyManifest(data, digest(data), stableContext)
	if err != nil || m.Version != "1.2.3" || len(m.Files) != 2 {
		t.Fatalf("valid manifest: %+v, %v", m, err)
	}
	for name, test := range map[string]struct {
		data []byte
		hash string
		ctx  ManifestContext
	}{
		"tamper":        {append(append([]byte{}, data...), ' '), digest(data), stableContext},
		"wrong digest":  {data, digest([]byte("wrong")), stableContext},
		"wrong version": {data, digest(data), ManifestContext{Version: "1.2.4", Channel: "stable", Arch: "x64"}},
		"wrong channel": {data, digest(data), ManifestContext{Version: "1.2.3", Channel: "beta", Arch: "x64"}},
		"wrong arch":    {data, digest(data), ManifestContext{Version: "1.2.3", Channel: "stable", Arch: "arm64"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyManifest(test.data, test.hash, test.ctx); err == nil {
				t.Fatal("accepted mismatching manifest")
			}
		})
	}
}

// Letting JSON silently overwrite keys or Windows alias paths makes verification ambiguous.
func TestManifestRejectsAmbiguousOrUnboundedInput(t *testing.T) {
	valid := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "a")})
	tests := map[string][]byte{
		"duplicate key":            []byte(strings.Replace(string(valid), `"format":1`, `"format":2,"format":1`, 1)),
		"unknown field":            []byte(strings.Replace(string(valid), `"format":1`, `"unknown":true,"format":1`, 1)),
		"trailing object":          append(append([]byte{}, valid...), []byte(` {}`)...),
		"oversized":                []byte(strings.Repeat(" ", 1_048_577)),
		"invalid UTF8":             append(append([]byte{}, valid...), 0xff),
		"escaped duplicate key":    []byte(strings.Replace(string(valid), `"format":1`, `"format":1,"for\u006dat":1`, 1)),
		"fractional size":          manifestBytes([]map[string]any{{"path": "a", "size": 1.5, "sha512": digest(nil)}}),
		"negative size":            manifestBytes([]map[string]any{{"path": "a", "size": -1, "sha512": digest(nil)}}),
		"excessive size":           manifestBytes([]map[string]any{{"path": "a", "size": 4*1024*1024*1024 + 1, "sha512": digest(nil)}}),
		"excessive total":          manifestBytes([]map[string]any{{"path": "a", "size": 4 * 1024 * 1024 * 1024, "sha512": digest(nil)}, {"path": "b", "size": 4 * 1024 * 1024 * 1024, "sha512": digest(nil)}, {"path": "c", "size": 1, "sha512": digest(nil)}}),
		"no files":                 manifestBytes(nil),
		"unsorted":                 manifestBytes([]map[string]any{fileEntry("b", "b"), fileEntry("a", "a")}),
		"case collision":           manifestBytes([]map[string]any{fileEntry("A", "a"), fileEntry("a", "a")}),
		"ancestor collision":       manifestBytes([]map[string]any{fileEntry("a", "a"), fileEntry("a/b", "b")}),
		"directory case collision": manifestBytes([]map[string]any{fileEntry("A/a", "a"), fileEntry("a/b", "b")}),
		"duplicate path":           manifestBytes([]map[string]any{fileEntry("a", "a"), fileEntry("a", "a")}),
	}
	for _, path := range []string{"../a", "a/../b", "./a", "a//b", "/a", "C:/a", `a\b`, "a:b", "a/CON.txt", "CON .txt", "nul", "COM1", "lpt9.foo", "a.", "a ", " a", "a/*", "a/é", strings.Repeat("a", 241)} {
		tests["path "+path] = manifestBytes([]map[string]any{fileEntry(path, "a")})
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyManifest(data, digest(data), stableContext); err == nil {
				t.Fatal("accepted malformed manifest")
			}
		})
	}
}

// Empty runtime assets are allowed; boundary sizes must not wrap integers.
func TestManifestAllowsEmptyAssetsAndBoundedSizes(t *testing.T) {
	for _, size := range []int64{0, 4 * 1024 * 1024 * 1024} {
		data := manifestBytes([]map[string]any{{"path": "dilla-desktop.exe", "size": size, "sha512": digest(nil)}})
		if _, e := VerifyManifest(data, digest(data), stableContext); e != nil {
			t.Fatalf("size %d: %v", size, e)
		}
	}
}

// Ignoring unlisted files or checking only expected sizes would allow injected executable content.
func TestVerifyTreeChecksCompleteContents(t *testing.T) {
	for _, fault := range []string{"valid", "tamper", "missing", "extra", "extra directory", "symlink", "ancestor symlink", "hardlink"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "resources"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "dilla-desktop.exe"), []byte("program"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "resources", "app.asar"), []byte("app"), 0600); err != nil {
				t.Fatal(err)
			}
			data := manifestBytes([]map[string]any{fileEntry("dilla-desktop.exe", "program"), fileEntry("resources/app.asar", "app")})
			m, err := VerifyManifest(data, digest(data), stableContext)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "resources", "app.asar")
			switch fault {
			case "tamper":
				err = os.WriteFile(target, []byte("bad"), 0600)
			case "missing":
				err = os.Remove(target)
			case "extra":
				err = os.WriteFile(filepath.Join(root, "injected.dll"), []byte("evil"), 0600)
			case "extra directory":
				err = os.Mkdir(filepath.Join(root, "extra"), 0700)
			case "symlink", "hardlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err = os.WriteFile(outside, []byte("app"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if fault == "symlink" {
					err = os.Symlink(outside, target)
				} else {
					err = os.Link(outside, target)
				}
				if err != nil {
					t.Skipf("links unavailable: %v", err)
				}
			case "ancestor symlink":
				outer := t.TempDir()
				linked := filepath.Join(outer, "linked")
				if err = os.Symlink(root, linked); err != nil {
					t.Skipf("links unavailable: %v", err)
				}
				root = linked
			}
			if err != nil {
				t.Fatal(err)
			}
			verified, err := VerifyTree(root, m)
			if verified != nil {
				defer verified.Close()
			}
			if fault == "valid" && err != nil {
				t.Fatalf("valid tree rejected: %v", err)
			}
			if fault != "valid" && err == nil {
				t.Fatalf("accepted %s tree", fault)
			}
		})
	}
}
