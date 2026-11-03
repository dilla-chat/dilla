package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// writeSecret writes n CSPRNG bytes as lowercase hex at mode 0600 and returns
// the path. init generates every secret file its own config references, because
// a dilla.toml that Validate refuses is not a bootstrap, it is a dead end.
func writeSecret(dir, name string, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// runInit creates the data directory (0700), writes the config to --config at
// 0600, generates the instance keys and the TURN and LiveKit secret files,
// CREATES AND MIGRATES the database (R35), inserts one bootstrap admin invite and
// prints its link exactly once. It refuses an existing --config unless
// --overwrite-config is given, and a data directory that already holds an
// instance (dilla.db, or a dilla.toml left by an older init) whatever the flag.
func runInit(args []string, stdout, stderr io.Writer) error {
	fs, cfgFlag := newFlagSet("init", stderr)
	cfgF := fs.Lookup("config")
	cfgF.Usage = "where init writes dilla.toml (default /etc/dilla/dilla.toml); an existing file is refused unless --overwrite-config"
	// The usage line already names the default; an empty DefValue stops the flag package printing it again.
	cfgF.DefValue = ""
	overwrite := fs.Bool("overwrite-config", false, "replace an existing file at --config")
	dataDir := fs.String("data-dir", "/var/lib/dilla", "state root")
	domain := fs.String("domain", "", "public DNS name (required)")
	publicIP := fs.String("public-ip", "", "public IP literal (required)")
	agreeTOS := fs.Bool("agree-tos", false, "accept the ACME CA's subscriber agreement on the operator's behalf")
	if err := parse(fs, args, stdout); err != nil {
		return err
	}
	if *domain == "" || *publicIP == "" {
		fmt.Fprintln(stderr, "dillad init: --domain and --public-ip are required")
		return exit.Usage
	}
	if !*agreeTOS {
		fmt.Fprintln(stderr, "dillad init: --agree-tos is required: tls.agreed records the operator's acceptance of the ACME CA's subscriber agreement, and dillad has no standing to accept it for them")
		return exit.Usage
	}
	cfgPath := *cfgFlag
	if _, err := os.Stat(cfgPath); err == nil && !*overwrite {
		return fmt.Errorf("init: %s already exists; pass --overwrite-config to replace it: %w", cfgPath, exit.CantCreate)
	}
	// An initialised data directory holds dilla.db, or a dilla.toml an older init left there.
	// The flag replaces a config file, never an instance.
	for _, name := range []string{"dilla.db", "dilla.toml"} {
		if _, err := os.Stat(filepath.Join(*dataDir, name)); err == nil {
			return fmt.Errorf("init: %s already holds an instance (%s); refusing to initialise it again: %w", *dataDir, name, exit.CantCreate)
		}
	}
	if err := mkdirAllMode(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("init: create %s: %w: %w", filepath.Dir(cfgPath), err, exit.CantCreate)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("init: create %s: %w: %w", *dataDir, err, exit.CantCreate)
	}
	// Even a directory that already existed: the umask no longer decides this.
	if err := os.Chmod(*dataDir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner-only
		return fmt.Errorf("init: chmod %s: %w: %w", *dataDir, err, exit.CantCreate)
	}

	c := config.Default()
	c.Instance.Domain = *domain
	addr, err := netip.ParseAddr(*publicIP)
	if err != nil {
		return fmt.Errorf("init: --public-ip %q: %w: %w", *publicIP, err, exit.Usage)
	}
	c.Instance.PublicIP = addr
	c.Instance.DataDir = *dataDir
	c.DB.Path = filepath.Join(*dataDir, "dilla.db")
	c.Blobs.Dir = filepath.Join(*dataDir, "blobs")
	c.TLS.Agreed = true // the operator said so with --agree-tos
	// Default() leaves turn.enabled and livekit.enabled true, and Validate then
	// requires turn.shared_secret_file, turn.relay_ip and livekit.api_secret_file.
	// Generate them here, or the file this verb writes is one no other verb can
	// read back.
	turnSecret, err := writeSecret(*dataDir, "turn.secret", 32)
	if err != nil {
		return fmt.Errorf("init: write turn secret: %w: %w", err, exit.CantCreate)
	}
	livekitSecret, err := writeSecret(*dataDir, "livekit.secret", 32)
	if err != nil {
		return fmt.Errorf("init: write livekit secret: %w: %w", err, exit.CantCreate)
	}
	c.TURN.SharedSecretFile = turnSecret
	// Not the public IP: on bridged Docker or a NATed LXC it is no local address, and every
	// Allocate would fail to bind there. serve resolves "auto" on the host it runs on.
	c.TURN.RelayIP = config.RelayIPAuto
	c.LiveKit.APISecretFile = livekitSecret
	c.Derive()

	write, err := sqlite.OpenWrite(c.DB.Path)
	if err != nil {
		return fmt.Errorf("init: %w: %w", err, exit.CantCreate)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		_ = write.Close()
		return fmt.Errorf("init: migrations: %w: %w", err, exit.Software)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		_ = write.Close()
		return fmt.Errorf("init: migrate: %w: %w", err, exit.Data)
	}
	read, err := sqlite.OpenRead(c.DB.Path)
	if err != nil {
		_ = write.Close()
		return fmt.Errorf("init: %w: %w", err, exit.CantCreate)
	}
	if err := secureFiles(c.DB.Path); err != nil {
		_ = write.Close()
		_ = read.Close()
		return fmt.Errorf("init: %w: %w", err, exit.CantCreate)
	}
	repo := sqlite.New(write, read)
	defer func() { _ = repo.Close() }() // closes BOTH pools; `defer write.Close()` would leak the read pool

	now := time.Now().Unix()
	// The two instance secrets of protocol/03 § Instance keys: the external
	// sender's Ed25519 signing key, whose public half goes into every text and
	// call group's external_senders extension, and K_frank, the 32-byte HMAC key
	// of protocol/04 § Franking. Both are generated once, here, and stored in
	// instances.key_history under the CBOR layout that section fixes.
	esPub, esPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("init: generate external sender key: %w: %w", err, exit.Software)
	}
	frankKey := make([]byte, 32)
	if _, err := rand.Read(frankKey); err != nil {
		return fmt.Errorf("init: generate franking key: %w: %w", err, exit.Software)
	}
	externalSenderKeyID, frankingKeyID := id.New(), id.New()
	keyHistory, err := cborx.Marshal([]any{
		uint64(1),
		[]any{
			// kind 0: external sender. The secret is the 32-byte Ed25519 seed,
			// which is ed25519.PrivateKey's first half.
			[]any{uint64(0), externalSenderKeyID, []byte(esPub), esPriv.Seed(), uint64(now), nil}, //nolint:gosec // G115: a unix second or row id this server wrote, never negative
			// kind 1: franking. No public half.
			[]any{uint64(1), frankingKeyID, []byte{}, frankKey, uint64(now), nil}, //nolint:gosec // G115: a unix second or row id this server wrote, never negative
		},
	})
	if err != nil {
		return fmt.Errorf("init: encode key history: %w: %w", err, exit.Software)
	}
	instance := store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: externalSenderKeyID, KeyHistory: keyHistory,
		FrankingKeyID: frankingKeyID, Generation: 1, PolicyVersion: 1, Created: now,
	}
	code, hash := auth.NewInviteCode()
	invite := store.InviteRow{
		ID: id.New(), CodeHash: hash, GrantsAdmin: 1, MaxUses: 1,
		Created: now, ExpiresAt: now + int64(24*time.Hour/time.Second),
	}
	if err := repo.Tx(context.Background(), func(tx store.Repository) error {
		if err := tx.CreateInstance(context.Background(), instance); err != nil {
			return err
		}
		return tx.CreateInvite(context.Background(), invite)
	}); err != nil {
		return fmt.Errorf("init: bootstrap: %w: %w", err, exit.Data)
	}

	sum := sha256.Sum256(hash)
	c.Registration.AdminInvite = hex.EncodeToString(sum[:4]) // the reference, never the code (NV12)
	if err := writeConfigFile(c, cfgPath, *overwrite); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "dillad init: wrote %s\n", cfgPath)
	fmt.Fprintf(stdout, "bootstrap invite (valid 24 hours, one use): https://%s/i/%s\n", *domain, code)
	fmt.Fprintln(stdout, "this link is printed once and is not stored anywhere; the database holds only its SHA-256")
	return nil
}

// mkdirAllMode creates dir and every missing ancestor, and sets each one it created to mode with
// an explicit chmod: the process umask is 0077, so MkdirAll's mode alone would leave them 0700.
// A directory that already exists is left as it is.
func mkdirAllMode(dir string, mode os.FileMode) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	for _, d := range missing {
		if err := os.Chmod(d, mode); err != nil {
			return err
		}
	}
	return nil
}

// writeConfigFile writes c to path at 0600. Without overwrite it is O_EXCL, so a file that
// appeared since the check is refused. With overwrite it writes path.new and renames it over
// path, so a failed write never leaves a half-written config where a good one was.
func writeConfigFile(c *config.Config, path string, overwrite bool) error {
	target := path
	if overwrite {
		target = path + ".new"
		_ = os.Remove(target) // a stale one from an interrupted run is not ours to inherit
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's own --config path; O_EXCL refuses to overwrite
	if err != nil {
		return fmt.Errorf("init: write %s: %w: %w", target, err, exit.CantCreate)
	}
	if err := c.WriteConfig(f); err != nil {
		_ = f.Close()
		_ = os.Remove(target)
		return fmt.Errorf("init: %w: %w", err, exit.IOErr)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("init: write %s: %w: %w", target, err, exit.IOErr)
	}
	if overwrite {
		if err := os.Rename(target, path); err != nil {
			_ = os.Remove(target)
			return fmt.Errorf("init: replace %s: %w: %w", path, err, exit.CantCreate)
		}
	}
	return nil
}
