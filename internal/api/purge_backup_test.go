package api_test

// purge_backup_test.go pins the branch review's BACKUPS-RECOVERY-05: backup objects and attachments
// never share bytes, and the admin purge of bytes a backups row names still removes every channel
// reference. Before the fix a sender, who chooses an attachment's key and nonce and so every
// ciphertext byte, shaped an attachment as a well-formed state object, stored the same bytes as
// their backup, and the purge (security review F10) then refused the bytes outright: the channel
// references stayed and every member kept downloading content the operator meant to remove. The
// purge still never tombstones or unlinks a backup object (the root is written once, so that would
// end the account's recovery); it drops the references, audits, and keeps the bytes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// mountBackupPut adds PUT /v1/backups to a blob env, over its repository, blob store and clock.
func mountBackupPut(e *env) {
	d := api.Deps{Repo: e.Repo, Blobs: e.Blobs, Config: config.Default(), Clock: e.Clk, Log: slog.New(slog.DiscardHandler)}
	e.Mux.Handle("PUT /v1/backups/{kind}/{chunk_seq}", http.HandlerFunc(d.PutBackup))
}

// stateShaped is a well-formed state object, [1, nonce(12), ciphertext]: what a sender can make an
// attachment's ciphertext look like.
func stateShaped(t *testing.T, fill byte) []byte {
	t.Helper()
	return mustCBOR(t, []any{uint64(1), bytes.Repeat([]byte{fill}, 12), bytes.Repeat([]byte{fill ^ 0xff}, 64)})
}

func putAttachment(e *env, ch id.ID, tok string, payload []byte) (int, []byte) {
	sum := sha256.Sum256(payload)
	return e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload)
}

func putStateObject(e *env, tok string, object []byte) (int, []byte) {
	return e.Do(http.MethodPut, "/v1/backups/1/0", tok, []any{object})
}

// A backup PUT of bytes a channel already references is refused (409) inside the recording
// transaction, and records no backups row.
func TestABackupPutOfAttachmentBytesIsRefused(t *testing.T) {
	e, ch, tok := blobEnv(t)
	mountBackupPut(e)
	object := stateShaped(t, 0x21)
	if status, body := putAttachment(e, ch, tok, object); status != http.StatusCreated {
		t.Fatalf("the attachment PUT = %d %x, want 201", status, body)
	}
	status, body := putStateObject(e, tok, object)
	if status != http.StatusConflict || e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("a backup PUT of an attachment's bytes = %d %x, want 409 E_INVALID_REQUEST", status, body)
	}
	if rows, err := e.Repo.ListBackups(t.Context(), userOf(t, e, tok), 1); err != nil || len(rows) != 0 {
		t.Fatalf("state rows after the refusal = %+v, %v; want none", rows, err)
	}
	sum := sha256.Sum256(object)
	if n, err := e.Repo.CountBlobRefs(t.Context(), sum[:]); err != nil || n != 1 {
		t.Fatalf("references after the refusal = %d, %v; want the attachment's one", n, err)
	}
}

// An attachment upload or reference of bytes a backups row names is refused (409) inside the
// recording transaction, in any channel; once the state object is replaced the bytes are no backup's
// and upload as an attachment again.
func TestAnAttachmentOfBackupBytesIsRefused(t *testing.T) {
	e, ch, tok := blobEnv(t)
	mountBackupPut(e)
	other := secondChannel(t, e, ch, tok)
	object := stateShaped(t, 0x31)
	if status, body := putStateObject(e, tok, object); status != http.StatusCreated {
		t.Fatalf("the state PUT = %d %x, want 201", status, body)
	}
	sum := sha256.Sum256(object)
	for _, c := range []id.ID{ch, other} {
		status, body := putAttachment(e, c, tok, object)
		if status != http.StatusConflict || e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("an attachment of a backup object's bytes = %d %x, want 409 E_INVALID_REQUEST", status, body)
		}
	}
	if n, err := e.Repo.CountBlobRefs(t.Context(), sum[:]); err != nil || n != 0 {
		t.Fatalf("references after the refusals = %d, %v; want none", n, err)
	}
	if _, err := e.Blobs.Stat(sum[:]); err != nil {
		t.Fatalf("the backup object's file after the refusals: %v", err)
	}

	if status, body := putStateObject(e, tok, stateShaped(t, 0x32)); status != http.StatusOK {
		t.Fatalf("the replacing state PUT = %d %x, want 200", status, body)
	}
	if status, body := putAttachment(e, ch, tok, object); status != http.StatusCreated {
		t.Fatalf("an attachment of bytes no backup names any more = %d %x, want 201", status, body)
	}
}

// The immediate delete of a replaced state object (BACKUPS-RECOVERY-01 as amended) is race-safe.
// Each round, two devices of one user replace the state object concurrently while the owner uploads
// the replaced object's bytes as an attachment. Whatever the interleaving: exactly one state row
// stays, naming the last committed object, whose blobs row and file exist; the losing object leaves
// no row and no file (no orphan); and the old bytes are either refused (409 while a backup still
// names them, 503 when they vanished under the upload, which the client retries) with no reference
// left, or accepted with a reference AND a blobs row AND a file holding exactly those bytes, never a
// reference to a file the delete unlinked.
func TestConcurrentStateReplacementsAndAnAttachmentOfTheOldBytes(t *testing.T) {
	e, ch, tok := blobEnv(t)
	mountBackupPut(e)
	ctx := t.Context()
	user := userOf(t, e, tok)
	second := id.New()
	if err := e.Repo.CreateDevice(ctx, newAPITestDevice(second, user, e.Clk.Now().Unix())); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	tok2 := "second-" + second.String()
	e.sess[tok2] = auth.Session{UserID: user, DeviceID: second, Scope: auth.ScopeEnrolled}

	exists := func(object []byte) (row, file bool) {
		sum := sha256.Sum256(object)
		_, rerr := e.Repo.GetBlob(ctx, sum[:])
		_, ferr := e.Blobs.Stat(sum[:])
		return rerr == nil, ferr == nil
	}
	old := stateShaped(t, 0x01)
	if status, body := putStateObject(e, tok, old); status != http.StatusCreated {
		t.Fatalf("the first state PUT = %d %x", status, body)
	}
	for round := 0; round < 60; round++ {
		e.Clk.Advance(time.Minute) // the attachment route's 20 uploads a minute
		a := mustCBOR(t, []any{uint64(1), bytes.Repeat([]byte{byte(round), 0xa}, 6), bytes.Repeat([]byte{0xaa}, 64)})
		b := mustCBOR(t, []any{uint64(1), bytes.Repeat([]byte{byte(round), 0xb}, 6), bytes.Repeat([]byte{0xbb}, 64)})
		var wg sync.WaitGroup
		var sa, sb, sx int
		wg.Add(3)
		go func() { defer wg.Done(); sa, _ = putStateObject(e, tok, a) }()
		go func() { defer wg.Done(); sb, _ = putStateObject(e, tok2, b) }()
		go func() { defer wg.Done(); sx, _ = putAttachment(e, ch, tok, old) }()
		wg.Wait()
		if sa != http.StatusOK || sb != http.StatusOK {
			t.Fatalf("round %d: the state PUTs = %d and %d, want 200 each", round, sa, sb)
		}
		rows, err := e.Repo.ListBackups(ctx, user, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("round %d: state rows = %+v, %v; want exactly one", round, rows, err)
		}
		live, lost := a, b
		if sum := sha256.Sum256(b); bytes.Equal(rows[0].BlobID, sum[:]) {
			live, lost = b, a
		}
		if row, file := exists(live); !row || !file {
			t.Fatalf("round %d: the live state has row %v file %v, want both", round, row, file)
		}
		if row, file := exists(lost); row || file {
			t.Fatalf("round %d: the replaced state has row %v file %v, want neither (an orphan)", round, row, file)
		}
		oldSum := sha256.Sum256(old)
		refs, err := e.Repo.CountBlobRefs(ctx, oldSum[:])
		if err != nil {
			t.Fatalf("CountBlobRefs: %v", err)
		}
		switch sx {
		case http.StatusCreated, http.StatusOK:
			row, file := exists(old)
			if refs != 1 || !row || !file {
				t.Fatalf("round %d: an accepted attachment of the old bytes has %d refs, row %v, file %v; want 1, true, true",
					round, refs, row, file)
			}
			rc, _, err := e.Blobs.Get(oldSum[:])
			if err != nil {
				t.Fatalf("round %d: read the accepted attachment: %v", round, err)
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || !bytes.Equal(got, old) {
				t.Fatalf("round %d: the accepted attachment's file holds %x (err %v), want the uploaded bytes", round, got, err)
			}
			// The bytes are an attachment now; the next round races fresh ones.
			old = live
			continue
		case http.StatusConflict, http.StatusServiceUnavailable:
			if refs != 0 {
				t.Fatalf("round %d: a refused attachment (%d) left %d references", round, sx, refs)
			}
			if row, file := exists(old); row || file {
				t.Fatalf("round %d: after a refused attachment (%d) the replaced bytes have row %v file %v, want neither",
					round, sx, row, file)
			}
		default:
			t.Fatalf("round %d: the attachment PUT = %d, want 201/200, 409 or 503", round, sx)
		}
		old = live
	}
}

// beforeFirstTx is the attachment route's repository with a hook run once, right before the route
// opens its first transaction: after the uploaded bytes are on disk, before they are recorded.
type beforeFirstTx struct {
	store.Repository
	mu   sync.Mutex
	hook func()
}

func (b *beforeFirstTx) Tx(ctx context.Context, fn func(store.Repository) error) error {
	b.mu.Lock()
	hook := b.hook
	b.hook = nil
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return b.Repository.Tx(ctx, fn)
}

// The deterministic interleaving of the race above: the owner uploads the state object's bytes as
// an attachment just after the state is replaced, and the replacement's delete commits and unlinks
// the file after the upload found the file present but before the upload records its reference.
// Before the fix the upload recorded a reference (200) to a file that was gone. Now the recording
// transaction finds the bytes gone and answers 503 with nothing recorded; the client retries and
// writes the bytes again.
func TestAnAttachmentWhoseBytesAreDeletedUnderItIsRefused(t *testing.T) {
	e, cid, tok := channelEnv(t)
	bs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	e.Blobs = bs
	hooked := &beforeFirstTx{Repository: e.Repo}
	log := slog.New(slog.DiscardHandler)
	api.NewBlobs(hooked, bs, api.NewResolver(e.Repo), config.Default().Blobs, e.Clk, log).Register(e.Mux)
	mountBackupPut(e)
	ch, _, status := newChannel(t, e, cid, tok, uint64(api.ChannelText), uint64(api.ModeE2EE), uint64(api.VisPrivate), "files")
	if status != http.StatusCreated {
		t.Fatalf("create channel = %d", status)
	}
	ctx := t.Context()

	old, next := stateShaped(t, 0x41), stateShaped(t, 0x42)
	if status, body := putStateObject(e, tok, old); status != http.StatusCreated {
		t.Fatalf("the first state PUT = %d %x", status, body)
	}
	// The attachment PUT of `old` first sees `old` named by the backup only if it records before
	// the replacement; here the replacement runs, whole, between its file write and its record.
	var replaced int
	hooked.hook = func() { replaced, _ = putStateObject(e, tok, next) }
	status, body := putAttachment(e, ch, tok, old)
	if replaced != http.StatusOK {
		t.Fatalf("the replacing state PUT inside the race = %d, want 200", replaced)
	}
	if status != http.StatusServiceUnavailable || e.ErrCode(body) != "E_UNAVAILABLE" {
		t.Fatalf("an attachment whose bytes were deleted under it = %d %x, want 503 E_UNAVAILABLE", status, body)
	}
	sum := sha256.Sum256(old)
	if n, err := e.Repo.CountBlobRefs(ctx, sum[:]); err != nil || n != 0 {
		t.Fatalf("references = %d, %v; want none", n, err)
	}
	if _, err := e.Repo.GetBlob(ctx, sum[:]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the deleted bytes' blobs row: %v, want ErrNotFound (no orphan row without a file)", err)
	}
	// The client's retry writes the bytes again and is accepted.
	if status, body := putAttachment(e, ch, tok, old); status != http.StatusCreated {
		t.Fatalf("the retried attachment = %d %x, want 201", status, body)
	}
	if _, err := e.Blobs.Stat(sum[:]); err != nil {
		t.Fatalf("the retried attachment's file: %v", err)
	}
}

// Backup objects and attachments stay disjoint under concurrency: an attachment PUT and a backup PUT
// of the same fresh bytes, run at once a hundred times, never both succeed. Each recording
// transaction takes the per-blob lock (store.LockBlob: the blobs row FOR UPDATE on Postgres; on
// SQLite the one writer already serialises the two transactions, which is what this run exercises)
// before its cross-table check, so exactly one wins and the other is 409, every time.
func TestAnAttachmentAndABackupOfTheSameBytesNeverBothWin(t *testing.T) {
	e, ch, tok := blobEnv(t)
	mountBackupPut(e)
	ctx := t.Context()
	user := userOf(t, e, tok)
	second := id.New()
	if err := e.Repo.CreateDevice(ctx, newAPITestDevice(second, user, e.Clk.Now().Unix())); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	tok2 := "second-" + second.String()
	e.sess[tok2] = auth.Session{UserID: user, DeviceID: second, Scope: auth.ScopeEnrolled}
	ok := func(status int) bool { return status == http.StatusOK || status == http.StatusCreated }
	for round := 0; round < 100; round++ {
		e.Clk.Advance(time.Minute) // the attachment route's 20 uploads a minute
		object := mustCBOR(t, []any{uint64(1), bytes.Repeat([]byte{byte(round), byte(round >> 8), 0x5}, 4), bytes.Repeat([]byte{0x55}, 64)})
		var wg sync.WaitGroup
		var sa, sb int
		wg.Add(2)
		go func() { defer wg.Done(); sa, _ = putAttachment(e, ch, tok, object) }()
		go func() { defer wg.Done(); sb, _ = putStateObject(e, tok2, object) }()
		wg.Wait()
		if ok(sa) == ok(sb) {
			t.Fatalf("round %d: attachment %d and backup %d: want exactly one to win", round, sa, sb)
		}
		loser := sa
		if ok(sa) {
			loser = sb
		}
		if loser != http.StatusConflict {
			t.Fatalf("round %d: the loser answered %d, want 409", round, loser)
		}
		sum := sha256.Sum256(object)
		refs, err := e.Repo.CountBlobRefs(ctx, sum[:])
		if err != nil {
			t.Fatalf("CountBlobRefs: %v", err)
		}
		backup, err := e.Repo.BackupRefersToBlob(ctx, sum[:])
		if err != nil {
			t.Fatalf("BackupRefersToBlob: %v", err)
		}
		if (refs > 0) == backup {
			t.Fatalf("round %d: %d references and backup %v: the bytes must be exactly one kind's", round, refs, backup)
		}
	}
}

// The purge of bytes a backups row names (what a database written before the disjointness rule can
// hold) deletes every channel reference, writes its audit row AND its tombstone, and skips only the
// unlink (and keeps the blobs row): the backup object stays readable, and the tombstone refuses any
// new attachment or backup of those bytes, so the takedown holds once the backup lets them go; then
// the state replacement unlinks them and the tombstone stays. Changed by BACKUPS-RECOVERY-05 (it was
// a 409 that wrote nothing and left every reference in place), and again by the coordinator's
// amendment (it first skipped the tombstone too, so a replaced backup freed the bytes for re-upload).
func TestThePurgeOfBackupBytesDropsEveryReferenceAndKeepsTheBytes(t *testing.T) {
	e, ch, tok := blobEnv(t)
	mountBackupPut(e)
	other := secondChannel(t, e, ch, tok)
	adminTok := e.NewInstanceAdmin("root")
	ctx := t.Context()
	user := userOf(t, e, tok)
	device := e.sess[tok].DeviceID
	now := e.Clk.Now().Unix()
	stored := func(kind uint64, fill byte, channels ...id.ID) []byte {
		t.Helper()
		object := bytes.Repeat([]byte{fill}, 103)
		sum := sha256.Sum256(object)
		if _, _, err := e.Blobs.Put(ctx, sum[:], bytes.NewReader(object), int64(len(object))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := e.Repo.PutBlob(ctx, store.BlobRow{BlobID: sum[:], Size: uint64(len(object)), StorageRef: "fs:x", Created: now}); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		if err := e.Repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: kind, DeviceID: id.ID{}, BlobID: sum[:], Created: now}); err != nil {
			t.Fatalf("PutBackup: %v", err)
		}
		for _, c := range channels {
			if err := e.Repo.PutBlobRef(ctx, sum[:], c, device, "", now); err != nil {
				t.Fatalf("PutBlobRef: %v", err)
			}
		}
		return sum[:]
	}
	purge := func(blobID []byte) (int, []byte) {
		return e.Do(http.MethodDelete, "/v1/admin/blobs/"+hex.EncodeToString(blobID), adminTok, []any{"takedown"})
	}
	kept := func(what string, blobID []byte) {
		t.Helper()
		if tomb, err := e.Repo.GetBlobTombstone(ctx, blobID); err != nil || !tomb {
			t.Fatalf("%s: tombstone %v (err %v), want one", what, tomb, err)
		}
		if _, err := e.Repo.GetBlob(ctx, blobID); err != nil {
			t.Fatalf("%s: the blobs row is gone: %v", what, err)
		}
		if _, err := e.Blobs.Stat(blobID); err != nil {
			t.Fatalf("%s: the file is gone: %v", what, err)
		}
		if n, err := e.Repo.CountBlobRefs(ctx, blobID); err != nil || n != 0 {
			t.Fatalf("%s: %d references (err %v), want none", what, n, err)
		}
	}
	audits := func(want int) {
		t.Helper()
		rows, err := e.Repo.ListAudit(ctx, 0, 10)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		n := 0
		for _, row := range rows {
			if row.Action == "blob.purge" {
				n++
			}
		}
		if n != want {
			t.Fatalf("blob.purge audit rows = %d (%+v), want %d", n, rows, want)
		}
	}

	shared := stored(1, 0x6b, ch, other)
	if status, body := purge(shared); status != http.StatusNoContent {
		t.Fatalf("purge of a state object's bytes two channels reference = %d %x, want 204", status, body)
	}
	kept("the state object after the purge", shared)
	audits(1)
	sharedBytes := bytes.Repeat([]byte{0x6b}, 103)
	if status, body := putAttachment(e, ch, tok, sharedBytes); status != http.StatusGone || e.ErrCode(body) != "E_PRUNED" {
		t.Fatalf("an attachment of the purged backup bytes = %d %x, want 410 E_PRUNED", status, body)
	}

	root := stored(0, 0x5a)
	if status, body := purge(root); status != http.StatusNoContent {
		t.Fatalf("purge of a root object's bytes = %d %x, want 204", status, body)
	}
	kept("the root object after the purge", root)
	audits(2)

	// The state object is replaced: no backups row names the purged bytes any more, so the
	// replacement unlinks them, and the tombstone stays and keeps refusing them.
	if status, body := putStateObject(e, tok, stateShaped(t, 0x7c)); status != http.StatusOK {
		t.Fatalf("the replacing state PUT = %d %x, want 200", status, body)
	}
	if _, err := e.Repo.GetBlob(ctx, shared); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the purged bytes' row after the replacement: %v, want ErrNotFound", err)
	}
	if _, err := e.Blobs.Stat(shared); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the purged bytes' file after the replacement: %v, want blob.ErrNotFound", err)
	}
	if tomb, err := e.Repo.GetBlobTombstone(ctx, shared); err != nil || !tomb {
		t.Fatalf("the tombstone after the replacement: %v (err %v), want it kept", tomb, err)
	}
	if status, body := putAttachment(e, ch, tok, sharedBytes); status != http.StatusGone {
		t.Fatalf("an attachment of the purged bytes after the replacement = %d %x, want 410", status, body)
	}
	audits(2)
}
