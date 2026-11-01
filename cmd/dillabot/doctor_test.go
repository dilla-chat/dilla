//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// This is the offline Bot half of the backup/restore acceptance slice. The
// server archive and generation reconciliation need a coordinated staging
// instance; this test exercises what can be proved from a cold Bot archive:
// key + encrypted database + SQLite recovery sidecars travel together, modes
// survive, and the restored private state passes the local doctor.
func TestColdBotArchiveRoundTripKeepsKeyAndSQLiteSidecarsDoctorReady(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	restored := filepath.Join(root, "restored")
	etc := filepath.Join(source, "etc", "dillabot")
	state := filepath.Join(source, "var", "lib", "dillabot")
	install := filepath.Join(state, "04040404040404040404040404040404")
	for _, dir := range []string{etc, install} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"database.key":     bytes.Repeat([]byte{0x42}, 32),
		"state.db":         []byte("opaque encrypted database fixture"),
		"state.db-wal":     []byte("opaque encrypted wal fixture"),
		"state.db-journal": []byte("opaque encrypted journal fixture"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(install, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files["state.db-shm"] = []byte("opaque encrypted shm fixture")
	if err := os.WriteFile(filepath.Join(install, "state.db-shm"), files["state.db-shm"], 0o600); err != nil {
		t.Fatal(err)
	}
	credentialText := "dbr_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	credential := filepath.Join(restored, "etc", "dillabot", "runtime.token")
	if err := os.WriteFile(filepath.Join(etc, "runtime.token"), []byte(credentialText), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(root, "identity.wasm")
	if err := os.WriteFile(module, []byte("fixture wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(restored, "etc", "dillabot", "bot.toml")
	config := "base_url = \"https://dilla.example\"\n" +
		"instance_id = \"01010101010101010101010101010101\"\n" +
		"runtime_credential_file = \"" + credential + "\"\n" +
		"helper_executable = \"" + helper + "\"\n" +
		"wasi_module = \"" + module + "\"\n" +
		"state_dir = \"" + filepath.Join(restored, "var", "lib", "dillabot") + "\"\n" +
		"reconcile_interval = \"30s\"\n"
	if err := os.WriteFile(filepath.Join(etc, "bot.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "bot-cold.tar.gz")
	if err := writeBotColdArchive(archive, source); err != nil {
		t.Fatalf("write cold archive: %v", err)
	}
	if info, err := os.Stat(archive); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cold archive mode/info = %v, %v; want 0600", info, err)
	}
	if err := extractBotColdArchive(archive, restored); err != nil {
		t.Fatalf("restore cold archive: %v", err)
	}
	restoredState := filepath.Join(restored, "var", "lib", "dillabot")
	for name, want := range files {
		path := filepath.Join(restoredState, filepath.Base(install), name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("restored %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("restored %s differs from cold snapshot", name)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("restored %s mode/info = %v, %v; want 0600", name, info, err)
		}
	}
	if info, err := os.Stat(restoredState); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("restored state directory mode/info = %v, %v; want 0700", info, err)
	}
	if info, err := os.Stat(filepath.Join(restoredState, filepath.Base(install))); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("restored installation directory mode/info = %v, %v; want 0700", info, err)
	}

	if _, err := os.Stat(filepath.Join(restored, "etc", "dillabot", "runtime.token")); err != nil {
		t.Fatalf("restored Bot credential: %v", err)
	}
	oldDoctorWASI := doctorWASI
	doctorWASI = func(context.Context, []byte) error { return nil }
	t.Cleanup(func() { doctorWASI = oldDoctorWASI })
	var stdout, stderr bytes.Buffer
	if code := runDoctorCommand([]string{"--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor rejected restored private state (exit %d): %s\n%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "state_directory OK") {
		t.Fatalf("doctor did not accept restored state directory:\n%s", stdout.String())
	}
}

func writeBotColdArchive(path, root string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range []string{
		"etc/dillabot",
		"etc/dillabot/bot.toml",
		"etc/dillabot/runtime.token",
		"var/lib/dillabot",
		"var/lib/dillabot/04040404040404040404040404040404",
		"var/lib/dillabot/04040404040404040404040404040404/database.key",
		"var/lib/dillabot/04040404040404040404040404040404/state.db",
		"var/lib/dillabot/04040404040404040404040404040404/state.db-wal",
		"var/lib/dillabot/04040404040404040404040404040404/state.db-shm",
		"var/lib/dillabot/04040404040404040404040404040404/state.db-journal",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Stat(full)
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = f.Close()
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			in, err := os.Open(full)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, in)
			closeErr := in.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

func extractBotColdArchive(path, root string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if filepath.IsAbs(hdr.Name) || filepath.Clean(hdr.Name) != hdr.Name || hdr.Name == ".." || strings.HasPrefix(hdr.Name, "../") {
			return errors.New("unsafe archive member")
		}
		dest := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dest, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return errors.New("unexpected archive member type")
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func TestDillabotDoctorChecksLocalDependenciesWithoutPrintingPathsOrSecrets(t *testing.T) {
	dir := t.TempDir()
	credentialText := "dbr_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	credential := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(credential, []byte(credentialText), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(dir, "identity.wasm")
	if err := os.WriteFile(module, []byte("fixture wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "bot.toml")
	body := "base_url = \"https://dilla.example\"\n" +
		"instance_id = \"01010101010101010101010101010101\"\n" +
		"runtime_credential_file = \"" + credential + "\"\n" +
		"helper_executable = \"" + helper + "\"\n" +
		"wasi_module = \"" + module + "\"\n" +
		"state_dir = \"" + state + "\"\n" +
		"reconcile_interval = \"30s\"\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDoctorWASI := doctorWASI
	doctorWASI = func(context.Context, []byte) error { return nil }
	t.Cleanup(func() { doctorWASI = oldDoctorWASI })
	oldDoctorLookPath := doctorLookPath
	doctorLookPath = func(name string) (string, error) {
		if name == "yt-dlp" {
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	}
	t.Cleanup(func() { doctorLookPath = oldDoctorLookPath })

	var stdout, stderr bytes.Buffer
	if code := runDoctorCommand([]string{"--config", config}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor exit code = %d, stderr %q, output:\n%s", code, stderr.String(), stdout.String())
	}
	for _, check := range []string{"runtime_credential OK", "native_helper OK", "wasi_identity_module OK", "state_directory OK"} {
		if !strings.Contains(stdout.String(), check) {
			t.Errorf("doctor output lacks %q:\n%s", check, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), "youtube_dependencies WARN") {
		t.Fatalf("normal doctor did not warn that YouTube is unavailable:\n%s", stdout.String())
	}
	stdout.Reset()
	if code := runDoctorCommand([]string{"--config", config, "--youtube"}, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "youtube_dependencies FAIL") {
		t.Fatalf("required YouTube check did not fail for missing yt-dlp (exit %d):\n%s", code, stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), credentialText) || strings.Contains(strings.ReplaceAll(stdout.String()+stderr.String(), config, ""), dir) {
		t.Fatal("doctor output disclosed secret material or configured filesystem paths")
	}
}

func TestDillabotDoctorYoutubeChecksTheQualifiedToolchain(t *testing.T) {
	dir := t.TempDir()
	for name, version := range map[string]string{
		"yt-dlp": "2026.10.21", "node": "v24.15.0", "bwrap": "bubblewrap 0.13.1",
		"python3": "Python 3.14.8", "ffmpeg": "ffmpeg version n9.0.2",
	} {
		path := filepath.Join(dir, name)
		body := "#!/bin/sh\nprintf '%s\\n' '" + version + "'\n"
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lookup := func(name string) (string, error) { return filepath.Join(dir, name), nil }
	var out bytes.Buffer
	if ok := checkYouTubeDependencies(&out, lookup); !ok {
		t.Fatalf("qualified toolchain was rejected: %s", out.String())
	}
	if !strings.Contains(out.String(), "yt-dlp 2026.10.21") || !strings.Contains(out.String(), "Node v24.15.0") {
		t.Fatalf("doctor omitted dependency evidence: %s", out.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "yt-dlp"), []byte("#!/bin/sh\necho 2026.8.19\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if ok := checkYouTubeDependencies(&out, lookup); ok || !strings.Contains(out.String(), "expected 2026.10.21") {
		t.Fatalf("an unqualified yt-dlp version was accepted: %s", out.String())
	}

	lookup = func(name string) (string, error) {
		if name == "yt-dlp" {
			return "", exec.ErrNotFound
		}
		return filepath.Join(dir, name), nil
	}
	out.Reset()
	if ok := checkYouTubeDependencies(&out, lookup); ok || !strings.Contains(out.String(), "yt-dlp unavailable") {
		t.Fatalf("missing yt-dlp was accepted or not identified: %s", out.String())
	}
}

func TestDillabotDoctorFailsWhenARequiredDependencyIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	credential := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(credential, []byte("dbr_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(dir, "identity.wasm")
	if err := os.WriteFile(module, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "bot.toml")
	body := "base_url = \"https://dilla.example\"\n" +
		"instance_id = \"01010101010101010101010101010101\"\n" +
		"runtime_credential_file = \"" + credential + "\"\n" +
		"helper_executable = \"" + filepath.Join(dir, "missing-helper") + "\"\n" +
		"wasi_module = \"" + module + "\"\n" +
		"state_dir = \"" + state + "\"\n" +
		"reconcile_interval = \"30s\"\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDoctorWASI := doctorWASI
	doctorWASI = func(context.Context, []byte) error { return errors.New("invalid module fixture") }
	t.Cleanup(func() { doctorWASI = oldDoctorWASI })

	var stdout, stderr bytes.Buffer
	if code := runDoctorCommand([]string{"--config", config}, &stdout, &stderr); code == 0 {
		t.Fatalf("doctor accepted an unavailable helper:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "native_helper FAIL") {
		t.Fatalf("doctor did not report helper failure:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "wasi_identity_module FAIL") || strings.Contains(stdout.String(), "invalid module fixture") {
		t.Fatalf("doctor did not report a sanitized module failure:\n%s", stdout.String())
	}
}

func TestDillabotDoctorRejectsUntrustedOrUnexecutableCodeFiles(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	module := filepath.Join(dir, "identity.wasm")
	if err := os.WriteFile(helper, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helper, 0o601); err != nil {
		t.Fatal(err)
	}
	if err := validateHelperExecutable(helper); err == nil && os.Geteuid() != 0 {
		t.Fatal("owner without execute permission was accepted via another user's execute bit")
	}
	if err := os.Chmod(helper, 0o722); err != nil {
		t.Fatal(err)
	}
	if err := validateHelperExecutable(helper); err == nil {
		t.Fatal("helper writable by another user was accepted")
	}
	if err := os.Chmod(helper, os.ModeSetuid|0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateHelperExecutable(helper); err == nil {
		t.Fatal("setuid helper was accepted")
	}
	if err := os.Chmod(module, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := readDoctorWASI(module); err == nil {
		t.Fatal("group-writable WASI module was accepted")
	}
	fifo := filepath.Join(dir, "blocking-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateHelperExecutable(fifo); err == nil {
		t.Fatal("FIFO was accepted as a helper")
	}
	if _, err := readDoctorWASI(fifo); err == nil {
		t.Fatal("FIFO was accepted as a WASI module")
	}
}

func TestDillabotDoctorDefaultsToTheInstalledConfigPath(t *testing.T) {
	if defaultConfigPath != "/etc/dillabot/bot.toml" {
		t.Fatalf("default config path = %q, want the path install-bot.sh writes and the unit reads", defaultConfigPath)
	}
	var stdout, stderr bytes.Buffer
	runDoctorCommand([]string{"--help"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), `(default "/etc/dillabot/bot.toml")`) {
		t.Fatalf("doctor usage does not show the installed default:\n%s", stderr.String())
	}
}

func writeDoctorTestConfig(t *testing.T, dir, token string) string {
	t.Helper()
	config := filepath.Join(dir, "bot.toml")
	body := "base_url = \"https://dilla.example\"\n" +
		"instance_id = \"01010101010101010101010101010101\"\n" +
		"runtime_credential_file = \"" + token + "\"\n" +
		"helper_executable = \"" + filepath.Join(dir, "helper") + "\"\n" +
		"wasi_module = \"" + filepath.Join(dir, "m.wasm") + "\"\n" +
		"state_dir = \"" + filepath.Join(dir, "state") + "\"\n" +
		"reconcile_interval = \"30s\"\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestDillabotDoctorDistinguishesMissingInvalidAndCredentialFailures(t *testing.T) {
	dir := t.TempDir()
	run := func(path string) string {
		var stdout, stderr bytes.Buffer
		if code := runDoctorCommand([]string{"--config", path}, &stdout, &stderr); code != 1 {
			t.Fatalf("doctor exit code = %d, want 1:\n%s", code, stdout.String())
		}
		return stdout.String()
	}
	missing := filepath.Join(dir, "absent", "bot.toml")
	out := run(missing)
	if !strings.Contains(out, "FAIL config: no such file at "+missing) || strings.Contains(out, "credential") {
		t.Fatalf("missing config message wrong:\n%s", out)
	}

	invalid := filepath.Join(dir, "invalid.toml")
	if err := os.WriteFile(invalid, []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = run(invalid)
	if !strings.Contains(out, "FAIL config: "+invalid+" is not a valid Bot configuration") || strings.Contains(out, "no such file") || strings.Contains(out, "credential") {
		t.Fatalf("invalid config message wrong:\n%s", out)
	}

	secret := "dbr_super-secret-value-that-must-not-print"
	token := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(token, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	out = run(writeDoctorTestConfig(t, dir, token))
	if !strings.Contains(out, "FAIL runtime_credential:") || strings.Contains(out, "no such file") || strings.Contains(out, secret) {
		t.Fatalf("credential message wrong or leaked the secret:\n%s", out)
	}
}

func TestDillabotDoctorNamesTheConfigAndSearchedPath(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(token, []byte("dbr_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	config := writeDoctorTestConfig(t, dir, token)
	t.Setenv("PATH", "/nonexistent-service-path")
	old := doctorLookPath
	doctorLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { doctorLookPath = old })
	var stdout, stderr bytes.Buffer
	runDoctorCommand([]string{"--config", config}, &stdout, &stderr)
	for _, want := range []string{"config OK: read " + config, "youtube_dependencies WARN", "PATH searched: /nonexistent-service-path"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("doctor output lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestDillabotDoctorYoutubeSandboxProbeReportsLaunchStderr(t *testing.T) {
	bwrap := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\nprintf 'namespace unavailable\\n'; printf 'namespace unavailable\\n' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	check := checkYouTubeSandbox(t.Context(), func(name string) (string, error) {
		if name == "bwrap" {
			return bwrap, nil
		}
		if name == "yt-dlp" {
			return exec.LookPath("true")
		}
		return exec.LookPath(name)
	})
	if check.name != "youtube_sandbox" || check.status != "FAIL: sandbox_unavailable: namespace unavailable" {
		t.Fatalf("check=%+v", check)
	}
}
