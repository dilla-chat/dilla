package desktoplaunch

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureTrust(t *testing.T) (Trust, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return Trust{PublicKeys: []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}, Channel: "stable", Arch: "x64", InitialVersion: "1.0.0", InitialManifestSHA512: digest([]byte("initial manifest"))}, priv
}

func releaseFixture(t *testing.T, priv ed25519.PrivateKey, root string, mutate func(map[string]any)) ([]byte, string) {
	t.Helper()
	artifact := map[string]any{"platform": "win32", "arch": "x64", "url": "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.3/Dilla.exe", "size": len("installer"), "sha512": digest([]byte("installer")), "programManifest": map[string]any{"url": "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.3/Dilla.manifest.json", "size": len("manifest"), "sha512": digest([]byte("manifest"))}}
	doc := map[string]any{"format": 1, "version": "1.2.3", "channel": "stable", "publishedAt": "2026-10-01T00:00:00.000Z", "expiresAt": "2026-10-31T00:00:00.000Z", "artifacts": []any{artifact}, "source": map[string]any{"commit": "publisher metadata permitted"}}
	if mutate != nil {
		mutate(doc)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
	if root != "" {
		dir := filepath.Join(root, "startup-"+strings.Repeat("a", 64))
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, bytes := range map[string][]byte{"release.json": data, "release.json.sig": []byte(signature + "\n"), "update.exe": []byte("installer"), "journal.json": []byte(`{"format":1,"nonce":"` + strings.Repeat("a", 64) + `","phase":"complete","childPid":123,"version":"1.2.3"}`)} {
			if err = os.WriteFile(filepath.Join(dir, name), bytes, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return data, signature
}

// Losing exact signature, origin, time, target or monotonic version checks authorizes a different release.
func TestVerifyReleaseAuthenticatesDescriptorAndAcceptanceContext(t *testing.T) {
	trust, priv := fixtureTrust(t)
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	ctx := ReleaseContext{PublicKeys: trust.PublicKeys, Channel: "stable", Platform: "win32", Arch: "x64", CurrentVersion: "1.0.0", Now: now}
	data, sig := releaseFixture(t, priv, "", nil)
	if _, err := VerifyRelease(data, sig, ctx); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"expired":           func(m map[string]any) { m["expiresAt"] = "2026-10-14T00:00:00.000Z" },
		"future published":  func(m map[string]any) { m["publishedAt"] = "2026-10-15T00:05:01.000Z" },
		"long lifetime":     func(m map[string]any) { m["expiresAt"] = "2027-02-01T00:00:00.000Z" },
		"downgrade":         func(m map[string]any) { m["version"] = "0.9.0" },
		"bad channel":       func(m map[string]any) { m["channel"] = "beta" },
		"case alias format": func(m map[string]any) { delete(m, "format"); m["Format"] = 1 },
		"case alias URL": func(m map[string]any) {
			a := m["artifacts"].([]any)[0].(map[string]any)
			a["URL"] = a["url"]
			delete(a, "url")
		},
		"wrong target": func(m map[string]any) { m["artifacts"].([]any)[0].(map[string]any)["arch"] = "arm64" },
		"wrong origin": func(m map[string]any) {
			m["artifacts"].([]any)[0].(map[string]any)["url"] = "https://evil.invalid/Dilla.exe"
		},
		"wrong version URL": func(m map[string]any) {
			m["artifacts"].([]any)[0].(map[string]any)["url"] = "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/Dilla.exe"
		},
		"missing manifest": func(m map[string]any) { delete(m["artifacts"].([]any)[0].(map[string]any), "programManifest") },
		"manifest origin": func(m map[string]any) {
			m["artifacts"].([]any)[0].(map[string]any)["programManifest"].(map[string]any)["url"] = "https://evil.invalid/program.json"
		},
		"duplicate target": func(m map[string]any) { a := m["artifacts"].([]any); m["artifacts"] = append(a, a[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			b, s := releaseFixture(t, priv, "", mutate)
			if _, err := VerifyRelease(b, s, ctx); err == nil {
				t.Fatal("accepted invalid release")
			}
		})
	}
	if _, err := VerifyRelease(append(data, ' '), sig, ctx); err == nil {
		t.Fatal("accepted tampered signed bytes")
	}
	_, other := fixtureTrust(t)
	if _, err := VerifyRelease(data, base64.StdEncoding.EncodeToString(ed25519.Sign(other, data)), ctx); err == nil {
		t.Fatal("accepted untrusted key")
	}
	ctx.CurrentVersion = "1.2.3"
	if _, err := VerifyRelease(data, sig, ctx); err == nil {
		t.Fatal("accepted replay of current version")
	}
	duplicate := []byte(strings.Replace(string(data), `"format":1`, `"format":2,"format":1`, 1))
	ctx.CurrentVersion = "1.0.0"
	if _, err := VerifyRelease(duplicate, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, duplicate)), ctx); err == nil {
		t.Fatal("accepted signed duplicate JSON")
	}
}

// A committed version remains bootable after descriptor expiry, using its original acceptance time.
func TestVerifySlotBindsInitialTrustAndRetainedRelease(t *testing.T) {
	trust, priv := fixtureTrust(t)
	root := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	initial := map[string]any{"format": 1, "kind": "initial", "generation": 0, "version": trust.InitialVersion, "manifestSha512": trust.InitialManifestSHA512}
	initialBytes, _ := json.Marshal(initial)
	if s, err := VerifySlot(initialBytes, trust, root, now); err != nil || s.Generation != 0 {
		t.Fatalf("initial slot: %+v, %v", s, err)
	}
	initial["version"] = "1.2.3"
	bad, _ := json.Marshal(initial)
	if _, err := VerifySlot(bad, trust, root, now); err == nil {
		t.Fatal("accepted uncompiled initial version")
	}
	releaseFixture(t, priv, root, nil)
	slot := map[string]any{"format": 1, "kind": "release", "generation": 1, "version": "1.2.3", "manifestSha512": digest([]byte("manifest")), "nonce": strings.Repeat("a", 64), "acceptedAt": time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC).UnixMilli()}
	data, _ := json.Marshal(slot)
	if s, err := VerifySlot(data, trust, root, now); err != nil || s.Version != "1.2.3" {
		t.Fatalf("retained expired release: %+v, %v", s, err)
	}
	for name, field := range map[string]map[string]any{"wrong digest": {"manifestSha512": digest([]byte("wrong"))}, "wrong version": {"version": "1.2.4"}, "path nonce": {"nonce": "../../outside"}, "future acceptance": {"acceptedAt": now.Add(time.Hour).UnixMilli()}, "expired acceptance": {"acceptedAt": now.UnixMilli()}, "fraction generation": {"generation": 1.5}, "zero generation": {"generation": 0}, "extra command": {"command": "evil"}} {
		t.Run(name, func(t *testing.T) {
			candidate := map[string]any{}
			for k, v := range slot {
				candidate[k] = v
			}
			for k, v := range field {
				candidate[k] = v
			}
			bytes, _ := json.Marshal(candidate)
			if _, err := VerifySlot(bytes, trust, root, now); err == nil {
				t.Fatal("accepted invalid slot")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "startup-"+strings.Repeat("a", 64), "update.exe"), []byte("evil-----"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySlot(data, trust, root, now); err == nil {
		t.Fatal("accepted tampered retained installer")
	}
}

func TestReleaseSlotRequiresTheSameCompletedJournalBeforePublication(t *testing.T) {
	for _, fault := range []string{"missing", "installing", "failed", "wrong nonce", "wrong version", "missing PID", "bad PID", "valid historical PID"} {
		t.Run(fault, func(t *testing.T) {
			trust, priv := fixtureTrust(t)
			root := t.TempDir()
			releaseFixture(t, priv, root, nil)
			nonce := strings.Repeat("a", 64)
			journal := map[string]any{"format": 1, "nonce": nonce, "phase": "complete", "version": "1.2.3", "childPid": 123}
			switch fault {
			case "installing":
				journal["phase"] = "installing"
				delete(journal, "version")
			case "failed":
				journal["phase"] = "failed"
				journal["failure"] = "startup"
				delete(journal, "version")
			case "wrong nonce":
				journal["nonce"] = strings.Repeat("b", 64)
			case "wrong version":
				journal["version"] = "1.2.4"
			case "missing PID":
				delete(journal, "childPid")
			case "bad PID":
				journal["childPid"] = 1
			}
			path := filepath.Join(root, "startup-"+nonce, "journal.json")
			if fault == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, _ := json.Marshal(journal)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(Slot{Format: 1, Kind: "release", Generation: 1, Version: "1.2.3", ManifestSHA512: digest([]byte("manifest")), Nonce: nonce, AcceptedAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC).UnixMilli()})
			_, err := VerifySlot(raw, trust, root, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
			if (err == nil) != (fault == "valid historical PID") {
				t.Fatalf("fault %s: %v", fault, err)
			}
		})
	}
}
