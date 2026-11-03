package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// Review minor 1: removing restrictUmask() from dispatch must fail a test. Not parallel.
func TestDispatchRestrictsTheUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	if _, _, err := run(t, "--help"); err != nil {
		t.Fatalf("help: %v", err)
	}
	if got := syscall.Umask(0o022); got != 0o077 {
		t.Fatalf("umask after dispatch = %04o, want 0077", got)
	}
}

// Review minor 4: the flag package appends "(default ...)" itself, so the usage text must not
// leave it a second time.
func TestInitHelpShowsTheConfigDefaultOnce(t *testing.T) {
	out, _, err := run(t, "init", "--help")
	if err != nil {
		t.Fatalf("init --help: %v", err)
	}
	if n := strings.Count(out, `(default "/etc/dilla/dilla.toml")`); n != 0 {
		t.Fatalf("init --help appends a second default after the brief's own:\n%s", out)
	}
	if !strings.Contains(out, "where init writes dilla.toml (default /etc/dilla/dilla.toml); an existing file is refused unless --overwrite-config") {
		t.Fatalf("init --help lost the brief's --config line:\n%s", out)
	}
}

// oldInstanceConfig is an install made before OpenWrite pre-created the file: a database and a
// live -wal at 0644. It is a snapshot taken while a connection holds the database open (a crash
// image), so the -wal carries a row and survives the next open's recovery.
func oldInstanceConfig(t *testing.T) *config.Config {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	src := filepath.Join(t.TempDir(), "src.db")
	db, err := sqlite.OpenWrite(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.DB.Driver = "sqlite"
	c.DB.Path = filepath.Join(t.TempDir(), "dilla.db")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.DB.Path+suffix, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(c.DB.Path+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// Review minor 2: serve's open path repairs an old install's 0644 database and -wal.
func TestOpenRepositoryRepairsAnOldInstallsModes(t *testing.T) {
	c := oldInstanceConfig(t)
	repo, _, err := openRepository(c)
	if err != nil {
		t.Fatalf("openRepository: %v", err)
	}
	defer func() { _ = repo.Close() }()
	for _, p := range []string{c.DB.Path, c.DB.Path + "-wal"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %04o, want 0600", p, perm)
		}
	}
}

// Review minor 3: a chmod that fails at serve is a warning, at init it is fatal. Not parallel:
// it swaps the package hook and the default logger.
func TestSecureFilesFailureWarnsAtServeAndStopsInit(t *testing.T) {
	old := secureFiles
	secureFiles = func(string) error { return errors.New("chmod: operation not permitted") }
	t.Cleanup(func() { secureFiles = old })

	var logs bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })

	repo, _, err := openRepository(oldInstanceConfig(t))
	if err != nil {
		t.Fatalf("openRepository failed on a chmod error, want a warning: %v", err)
	}
	_ = repo.Close()
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "operation not permitted") {
		t.Fatalf("no warning logged: %q", logs.String())
	}

	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "dilla.toml")
	_, _, err = run(t, initArgs(cfg, filepath.Join(tmp, "data"))...)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("init err = %v, want the chmod failure to be fatal", err)
	}
	if _, statErr := os.Stat(cfg); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("init wrote a config after a fatal chmod failure")
	}
}

func initArgs(cfg, data string, extra ...string) []string {
	return append([]string{"init", "--config=" + cfg, "--data-dir=" + data, "--agree-tos",
		"--domain=chat.example", "--public-ip=203.0.113.7"}, extra...)
}

func wantCantCreate(t *testing.T, err error, substr string) {
	t.Helper()
	var code exit.Code
	if !errors.As(err, &code) || code != exit.CantCreate {
		t.Fatalf("err = %v, want exit.CantCreate (73)", err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %q, want it to contain %q", err, substr)
	}
}

// R16 (9a): init writes the config where --config says, and nowhere in the data directory.
func TestInitWritesTheConfigPath(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "etc", "dilla.toml")
	data := filepath.Join(tmp, "data")
	out, _, err := run(t, initArgs(cfg, data)...)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("no config at --config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "dilla.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("init left a dilla.toml in the data directory (stat err %v)", err)
	}
	if !strings.Contains(out, "dillad init: wrote "+cfg) {
		t.Fatalf("stdout does not name the config path: %q", out)
	}
}

func TestInitRefusesAnExistingConfig(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "dilla.toml")
	if err := os.WriteFile(cfg, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(tmp, "data")
	_, _, err := run(t, initArgs(cfg, data)...)
	wantCantCreate(t, err, cfg+" already exists; pass --overwrite-config to replace it")
	if got, _ := os.ReadFile(cfg); string(got) != "precious" {
		t.Fatalf("the refused config was changed: %q", got)
	}

	// Control: the same call with the flag replaces it.
	if _, _, err := run(t, initArgs(cfg, data, "--overwrite-config")...); err != nil {
		t.Fatalf("init --overwrite-config: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if string(got) == "precious" || !strings.Contains(string(got), "chat.example") {
		t.Fatalf("the config was not replaced: %q", got)
	}
	if _, err := os.Stat(cfg + ".new"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PATH.new was left behind (stat err %v)", err)
	}

	// An initialised data directory is refused with the flag too.
	_, _, err = run(t, initArgs(cfg, data, "--overwrite-config")...)
	wantCantCreate(t, err, data+" already holds an instance (dilla.db); refusing to initialise it again")
}

func TestInitRefusesALegacyDataDirectory(t *testing.T) {
	tmp := t.TempDir()
	data := filepath.Join(tmp, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "dilla.toml"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, initArgs(filepath.Join(tmp, "etc.toml"), data, "--overwrite-config")...)
	wantCantCreate(t, err, "already holds an instance (dilla.toml)")
}

// R17 (9b): whatever the caller's umask, every file init leaves is 0600 and every directory 0700.
// Not parallel: the umask is per process.
func TestInitFilesAreOwnerOnlyUnderUmask022(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "etc", "dilla.toml")
	data := filepath.Join(tmp, "data")
	// A data directory that already exists at the umask's mode is tightened too.
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, initArgs(cfg, data)...); err != nil {
		t.Fatalf("init: %v", err)
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		seen[filepath.Base(p)] = true
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if perm := info.Mode().Perm(); perm != want {
			t.Errorf("%s has mode %04o, want %04o", p, perm, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dilla.db", "turn.secret", "livekit.secret"} {
		if !seen[name] {
			t.Errorf("init left no %s in the data directory", name)
		}
	}
	for _, c := range []struct {
		path string
		want os.FileMode
	}{{cfg, 0o600}, {filepath.Dir(cfg), 0o755}} {
		info, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != c.want {
			t.Errorf("%s has mode %04o, want %04o", c.path, perm, c.want)
		}
	}
}
