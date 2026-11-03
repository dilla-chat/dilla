package ops_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// opsHarness is a migrated SQLite instance in a temp data directory: the
// instance row, one user with one device, one community channel holding two
// blobs, one group for application messages, one ACME account key in the
// certmagic storage directory, the write and read pools open (as serve holds
// them) and a fake clock.
type opsHarness struct {
	t       *testing.T
	Cfg     *config.Config
	Repo    store.Repository
	Clock   *clock.Fake
	Blobs   *blob.Store
	ACMEKey string

	write   *sql.DB
	group   id.ID
	blobIDs [][]byte
	nextSeq uint64
}

func newOpsHarness(t *testing.T) *opsHarness {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	c := config.Default()
	c.Instance.Domain = "dilla.test"
	c.Instance.DataDir = dir
	c.DB.Path = filepath.Join(dir, "dilla.db")
	c.Blobs.Dir = filepath.Join(dir, "blobs")
	c.TLS.Agreed = true
	c.Derive()

	write, err := sqlite.OpenWrite(c.DB.Path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	read, err := sqlite.OpenRead(c.DB.Path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { _ = repo.Close() })
	bs, err := blob.Open(c.Blobs.Dir, c.Blobs.Backend)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	clk := clock.NewFake(time.Unix(1_790_000_000, 0).UTC())
	h := &opsHarness{t: t, Cfg: c, Repo: repo, Clock: clk, Blobs: bs, write: write}
	now := clk.Now().Unix()

	if err := repo.CreateInstance(ctx, store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{0x82, 0x01, 0x80},
		FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	owner, device := id.New(), id.New()
	if err := repo.CreateUser(ctx, store.UserRow{
		ID: owner, Username: "owner", Display: "owner",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{
		ID: device, UserID: owner, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	community := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{
		ID: community, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	ch := store.ChannelRow{
		ID: id.New(), CommunityID: &community, Name: "files", SettingsJSON: []byte(`{}`),
		HostPolicyVersion: 1, Created: now,
	}
	if err := repo.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, payload := range [][]byte{bytes.Repeat([]byte("ciphertext one "), 80), []byte("ciphertext two")} {
		sum := sha256.Sum256(payload)
		n, _, err := bs.Put(ctx, sum[:], bytes.NewReader(payload), 1<<20)
		if err != nil {
			t.Fatalf("blob Put: %v", err)
		}
		if err := repo.Tx(ctx, func(tx store.Repository) error {
			if err := tx.PutBlob(ctx, store.BlobRow{
				BlobID: sum[:], Size: uint64(n), StorageRef: blob.StorageRef(c.Blobs.Backend, sum[:]), Created: now,
			}); err != nil {
				return err
			}
			return tx.PutBlobRef(ctx, sum[:], ch.ID, device, "", now)
		}); err != nil {
			t.Fatalf("record blob: %v", err)
		}
		h.blobIDs = append(h.blobIDs, sum[:])
	}
	h.group = id.New()
	if err := repo.CreateGroup(ctx, store.GroupRow{
		GroupID: h.group, Binding: []byte{0x80}, TargetID: ch.ID, Ciphersuite: 1,
		ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// certmagic's layout for an ACME account key (account.go, v0.25.4):
	// acme/<issuer>/users/<email>/<username>.key under the storage directory.
	h.ACMEKey = "-----BEGIN EC PRIVATE KEY-----\nharness\n-----END EC PRIVATE KEY-----\n"
	keyDir := filepath.Join(c.TLS.StorageDir, "acme", "acme-v02.api.letsencrypt.org-directory", "users", "ops@dilla.test")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "ops.key"), []byte(h.ACMEKey), 0o600); err != nil {
		t.Fatalf("write ACME key: %v", err)
	}
	return h
}

// SeedRows appends n application messages to the harness's group.
func (h *opsHarness) SeedRows(n int) {
	h.t.Helper()
	for range n {
		h.putMessage([]byte("ct"))
	}
}

func (h *opsHarness) putMessage(blobBytes []byte) {
	h.t.Helper()
	h.nextSeq++
	if err := h.Repo.PutAppMessage(h.t.Context(), store.AppMessageRow{
		GroupID: h.group, Seq: h.nextSeq, Epoch: 1, UploaderDevice: id.New(), Blob: blobBytes,
		FrankingTag: make([]byte, 32), Size: uint64(len(blobBytes)), Created: h.Clock.Now().Unix(),
	}); err != nil {
		h.t.Fatalf("PutAppMessage: %v", err)
	}
}

// SeedE2EEMessageWithPlaintext stores plaintext the way the delivery service
// stores every end-to-end-encrypted message: as ciphertext it cannot read,
// here AES-256-GCM under a key that is thrown away.
func (h *opsHarness) SeedE2EEMessageWithPlaintext(plaintext string) {
	h.t.Helper()
	key := make([]byte, 32)
	nonce := make([]byte, 12)
	_, _ = rand.Read(key)
	_, _ = rand.Read(nonce)
	block, err := aes.NewCipher(key)
	if err != nil {
		h.t.Fatalf("aes: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		h.t.Fatalf("gcm: %v", err)
	}
	h.putMessage(aead.Seal(nonce, nonce, []byte(plaintext), nil))
}

// SchemaVersion is the live database's goose version.
func (h *opsHarness) SchemaVersion(t *testing.T) int64 {
	t.Helper()
	v, err := h.Repo.SchemaVersion(t.Context())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	return v
}

func (h *opsHarness) Instance(t *testing.T) store.InstanceRow {
	t.Helper()
	row, err := h.Repo.GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return row
}

func (h *opsHarness) Generation(t *testing.T) uint64 { return h.Instance(t).Generation }

// ForgeSchemaVersion records a goose version no embedded migration has, as a
// database written by a newer dillad would carry.
func (h *opsHarness) ForgeSchemaVersion(t *testing.T, v int64) {
	t.Helper()
	if _, err := h.write.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v); err != nil {
		t.Fatalf("forge goose version: %v", err)
	}
}

// LoseOneBlob removes the first blob's file behind the database's back and
// returns the archive path that blob would have had.
func (h *opsHarness) LoseOneBlob() string {
	h.t.Helper()
	b := h.blobIDs[0]
	if err := h.Blobs.Delete(b); err != nil {
		h.t.Fatalf("Delete: %v", err)
	}
	x := hex.EncodeToString(b)
	return "dilla-backup/blobs/" + x[0:2] + "/" + x[2:4] + "/" + x
}

// archivedDB extracts db/dilla.sqlite into a temp file and opens it read-only.
func (h *opsHarness) archivedDB(t *testing.T, archive []byte) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored.sqlite")
	if err := os.WriteFile(path, tarMember(t, archive, "dilla-backup/db/dilla.sqlite"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	db, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("open the archived database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// RowsInArchivedDB is the number of application messages the archived
// database holds.
func (h *opsHarness) RowsInArchivedDB(t *testing.T, archive []byte) int {
	t.Helper()
	var n int
	if err := h.archivedDB(t, archive).QueryRowContext(t.Context(),
		`SELECT count(*) FROM mls_app_messages`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// ArchivedSetting reads one instance_settings value out of the archived database.
func (h *opsHarness) ArchivedSetting(t *testing.T, archive []byte, key string) string {
	t.Helper()
	var v []byte
	err := h.archivedDB(t, archive).QueryRowContext(t.Context(),
		`SELECT value FROM instance_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read setting: %v", err)
	}
	return string(v)
}

// member is one tar member as read back.
type member struct {
	hdr  *tar.Header
	body []byte
}

func readMembers(t *testing.T, archive []byte) []member {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out []member
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		out = append(out, member{hdr: hdr, body: body})
	}
}

// writeMembers re-packs members exactly as read, recomputing only each size.
func writeMembers(t *testing.T, ms []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range ms {
		hdr := *m.hdr
		hdr.Size = int64(len(m.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(m.body); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func tarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	var names []string
	for _, m := range readMembers(t, archive) {
		names = append(names, m.hdr.Name)
	}
	return names
}

func tarHeaders(t *testing.T, archive []byte) []*tar.Header {
	t.Helper()
	var hdrs []*tar.Header
	for _, m := range readMembers(t, archive) {
		hdrs = append(hdrs, m.hdr)
	}
	return hdrs
}

func tarMember(t *testing.T, archive []byte, name string) []byte {
	t.Helper()
	for _, m := range readMembers(t, archive) {
		if m.hdr.Name == name {
			return m.body
		}
	}
	t.Fatalf("the archive has no member %q", name)
	return nil
}

// countingReader counts the bytes read through it. archive/tar reads a header
// in whole blocks straight from its reader, so after Next the count is exactly
// the offset of that member's body.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// truncateLastMember cuts the uncompressed tar stream halfway through the last
// member's body, as an interrupted copy would, and recompresses it.
func truncateLastMember(t *testing.T) func([]byte) []byte {
	return func(archive []byte) []byte {
		t.Helper()
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		cr := &countingReader{r: bytes.NewReader(raw)}
		tr := tar.NewReader(cr)
		var start, size int64
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("tar: %v", err)
			}
			start, size = cr.n, hdr.Size
			if _, err := io.Copy(io.Discard, tr); err != nil {
				t.Fatalf("tar body: %v", err)
			}
		}
		var out bytes.Buffer
		w := gzip.NewWriter(&out)
		if _, err := w.Write(raw[:start+size/2]); err != nil {
			t.Fatalf("gzip: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip: %v", err)
		}
		return out.Bytes()
	}
}

// flipOneByteOfTheDatabase changes one byte in the middle of db/dilla.sqlite and
// re-packs the archive, leaving the manifest's digest as it was.
func flipOneByteOfTheDatabase(t *testing.T) func([]byte) []byte {
	return func(archive []byte) []byte {
		t.Helper()
		ms := readMembers(t, archive)
		for i := range ms {
			if ms[i].hdr.Name == "dilla-backup/db/dilla.sqlite" {
				ms[i].body[len(ms[i].body)/2] ^= 0xff
			}
		}
		return writeMembers(t, ms)
	}
}

// bumpManifestSchema rewrites MANIFEST.json's schema_version to one no binary
// has, as an archive from a newer dillad would carry.
func bumpManifestSchema(t *testing.T) func([]byte) []byte {
	return func(archive []byte) []byte {
		t.Helper()
		ms := readMembers(t, archive)
		var doc map[string]any
		if err := json.Unmarshal(ms[0].body, &doc); err != nil {
			t.Fatalf("manifest: %v", err)
		}
		doc["schema_version"] = 1_000_000
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("manifest: %v", err)
		}
		ms[0].body = body
		return writeMembers(t, ms)
	}
}
