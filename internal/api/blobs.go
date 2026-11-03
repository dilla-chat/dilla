package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Blobs serves the attachment routes of protocol/09 § Blobs: a channel-scoped,
// content-addressed PUT and a GET (which also answers HEAD) over the blob store.
// The reference table is the ACL (gap-47 §4.1): a caller reads a blob only
// through a channel it may read that holds a reference to it. Register mounts
// the handlers bare, as Readable does, and each requires an enrolled session
// itself.
type Blobs struct {
	repo  store.Repository
	store *blob.Store
	res   *Resolver
	cfg   config.Blobs
	clk   clock.Clock
	log   *slog.Logger
	meter *UploadMeter // nil when neither upload limit is set
}

// NewBlobs wires the routes over one blob store and the [blobs] configuration,
// including the per-user upload meter uploads_per_minute and
// upload_bytes_per_day configure (UploadMeter).
func NewBlobs(repo store.Repository, bs *blob.Store, res *Resolver, cfg config.Blobs, clk clock.Clock, log *slog.Logger) *Blobs {
	return &Blobs{repo: repo, store: bs, res: res, cfg: cfg, clk: clk, log: log, meter: NewUploadMeter(clk, cfg)}
}

// WithUploadMeter makes the routes spend m, the meter the composition root also hands
// PUT /v1/backups (Deps.UploadMeter), so the two routes draw on one per-user budget. A nil m keeps
// the routes' own meter.
func (b *Blobs) WithUploadMeter(m *UploadMeter) *Blobs {
	if m != nil {
		b.meter = m
	}
	return b
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (b *Blobs) Register(mux *server.Mux) {
	mux.HandleFunc("PUT /v1/channels/{id}/blobs/{blob_id}", b.put)
	// ServeMux routes HEAD to the GET handler, so one registration answers both;
	// http.ServeContent writes no body for a HEAD and keeps the headers.
	mux.HandleFunc("GET /v1/channels/{id}/blobs/{blob_id}", b.get)
	mux.HandleFunc("DELETE /v1/channels/{id}/blobs/{blob_id}", b.delete)
	mux.HandleFunc("POST /v1/channels/{id}/blobs/{blob_id}/confirm", b.confirm)
}

// octetStream is the one media type a blob travels as, both ways: the server
// cannot know better, and must not guess from ciphertext.
const octetStream = "application/octet-stream"

// errQuota aborts the reference transaction when the new reference takes the
// uploader over blobs.quota_bytes_per_user.
var errQuota = errors.New("api: blob quota exceeded")

// errTombstoned aborts the reference transaction when an administrator purged
// the bytes while the upload was streaming.
var errTombstoned = errors.New("api: the blob was purged during the upload")

// channel parses {id} and {blob_id} — both before the database or the
// filesystem is touched (protocol/09 § Identifiers) — and loads the channel for
// a caller holding every bit of want. A caller who may not view the channel gets
// 404, as for an unknown one; a category holds no attachments (NV9: it has no
// messages to carry one).
func (b *Blobs) channel(r *http.Request, want Bits) (auth.Session, store.ChannelRow, []byte, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return s, store.ChannelRow{}, nil, err
	}
	chID, err := server.PathID(r, "id")
	if err != nil {
		return s, store.ChannelRow{}, nil, err
	}
	blobID, err := server.PathDigest(r, "blob_id")
	if err != nil {
		return s, store.ChannelRow{}, nil, err
	}
	ch, err := b.repo.GetChannel(r.Context(), chID)
	if err != nil {
		return s, store.ChannelRow{}, nil, notFound(err)
	}
	bits, err := b.res.Resolve(r.Context(), s.UserID, ch)
	if err != nil {
		return s, store.ChannelRow{}, nil, err
	}
	if !bits.Has(PermViewChannel) {
		return s, store.ChannelRow{}, nil, server.Errorf(server.CodeNotFound, "no such object")
	}
	if ch.Kind == ChannelCategory {
		return s, store.ChannelRow{}, nil, server.Errorf(server.CodeForbidden, "a category holds no attachments")
	}
	if !bits.Has(want) {
		return s, store.ChannelRow{}, nil, server.Errorf(server.CodeForbidden, "missing permission")
	}
	return s, ch, blobID, nil
}

// put is PUT /v1/channels/{id}/blobs/{blob_id}: raw ciphertext in, and
// `201 [blob_id, size]` when the bytes were new or `200 [blob_id, size]` when
// they were already stored. Either way the caller's channel gains a reference, pending until its
// uploader confirms it (POST …/confirm) unless the channel already held one of the caller's; a
// reference another user made is 409 E_NOT_UPLOADER and stays as it is.
func (b *Blobs) put(w http.ResponseWriter, r *http.Request) {
	// SetReadDeadline must be the FIRST statement: "Setting the read deadline
	// after it has been exceeded will not extend it" (go doc
	// http.ResponseController.SetReadDeadline, Go 1.27), and a 100 MiB upload on
	// a homelab uplink outruns any whole-request ReadTimeout. The deadline is a
	// socket deadline, so it is wall-clock time, never the instance clock.
	if d := b.cfg.UploadTimeout.Value(); d > 0 {
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(d)); err != nil {
			b.log.Warn("set read deadline", "err", err)
		}
	}
	s, ch, blobID, err := b.channel(r, PermAttachFiles)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireCommunityWire(r.Context(), b.repo, s.WireVersion, ch.CommunityID); err != nil {
		server.WriteError(w, err)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != octetStream {
		server.WriteError(w, server.WithStatus(http.StatusUnsupportedMediaType,
			server.Errorf(server.CodeInvalidRequest, "Content-Type must be %s", octetStream)))
		return
	}
	if err := b.refuseTombstoned(r.Context(), blobID); err != nil {
		// Without this, content addressing undoes an admin purge: anyone holding
		// the ciphertext re-PUTs it and gets the same name back.
		server.WriteError(w, err)
		return
	}
	// The most this upload can write: its Content-Length when it sends one,
	// max_blob_bytes when it does not (a chunked body).
	announced := r.ContentLength
	reserve := b.cfg.MaxBlobBytes
	if announced >= 0 && announced < reserve {
		reserve = announced
	}
	// A blob the uploader already references is already on disk and already counted toward their
	// quota (each distinct blob counts once), so re-publishing it, a forward into a second
	// channel, adds no bytes: the byte pre-checks below would refuse it by its Content-Length
	// near a limit it cannot cross (M3). The reference-count and tombstone logic still run.
	owned, err := b.repo.UserReferencesBlob(r.Context(), s.UserID, blobID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if limit := b.cfg.StoreMaxBytes; limit > 0 && !owned {
		// blobs.store_max_bytes, before a byte is read. Every blob row counts,
		// unreferenced ones included: their files are on disk until collected.
		total, err := b.repo.InstanceBlobBytes(r.Context())
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if total+reserve > limit {
			server.WriteError(w, server.Errorf(server.CodeStorageFull, "the instance's attachment storage is full"))
			return
		}
	}
	quota := b.cfg.QuotaBytesPerUser
	if quota > 0 && !owned {
		// The cheap refusal, before a byte of the body is read, counting the
		// body the request announces. The exact check runs in the reference
		// transaction below.
		used, err := b.repo.UserBlobBytes(r.Context(), s.UserID)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if used >= quota || (announced > 0 && used+announced > quota) {
			server.WriteError(w, errStorageFull())
			return
		}
	}
	// blobs.uploads_per_minute and upload_bytes_per_day, before the body too.
	held, err := b.meter.begin(s.UserID, reserve)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	counted := &countingReader{r: http.MaxBytesReader(w, r.Body, b.cfg.MaxBlobBytes)}
	n, created, err := b.store.Put(r.Context(), blobID, counted, b.cfg.MaxBlobBytes)
	// Every byte read spends the day's budget, whatever happens to the upload next.
	b.meter.settle(s.UserID, held, counted.n)
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, blob.ErrTooLarge), errors.As(err, &mbe):
		server.WriteError(w, server.Errorf(server.CodeTooLarge, "at most %d bytes", b.cfg.MaxBlobBytes))
		return
	case errors.Is(err, blob.ErrHashMismatch):
		server.WriteError(w, server.WithStatus(http.StatusUnprocessableEntity,
			server.Errorf(server.CodeInvalidRequest, "the body does not hash to the requested blob_id")))
		return
	case err != nil:
		b.log.ErrorContext(r.Context(), "store blob", "err", err)
		server.WriteError(w, err)
		return
	}
	now := b.clk.Now().Unix()
	err = b.repo.Tx(r.Context(), func(tx store.Repository) error {
		// PutBlob is a no-op when the row exists, so a concurrent upload of the
		// same bytes lands on the same row whichever transaction commits first.
		if err := tx.PutBlob(r.Context(), store.BlobRow{
			BlobID: blobID, Size: uint64(n), //nolint:gosec // G115: n is a byte count io.Copy returned, never negative
			StorageRef: blob.StorageRef(b.cfg.Backend, blobID), Created: now,
		}); err != nil {
			return err
		}
		// The per-blob lock before the cross-table check below, the one the backup
		// route takes too (store.LockBlob), so the two routes cannot both find
		// the other table empty for the same bytes.
		if err := tx.LockBlob(r.Context(), blobID); err != nil {
			return err
		}
		// The file, after the row is claimed: a replaced state object's delete
		// (Deps.unlinkReplaced) claims the same row before it unlinks, so either it
		// saw this row and kept the file or the file is gone now and nothing is
		// recorded (the client sends the bytes again).
		if err := bytesPresent(b.store, blobID); err != nil {
			return err
		}
		// The tombstone again, now inside the transaction that would write the
		// reference: a purge that committed while the body was streaming must not
		// be undone by it (fix wave I9).
		tomb, err := tx.GetBlobTombstone(r.Context(), blobID)
		if err != nil {
			return err
		}
		if tomb {
			return errTombstoned
		}
		// Attachments and backup objects never share bytes (branch review
		// BACKUPS-RECOVERY-05): a channel reference to bytes a backup names would
		// be one the admin purge has to leave the bytes of, and the uploader
		// chooses every byte of an attachment's ciphertext.
		backup, err := tx.BackupRefersToBlob(r.Context(), blobID)
		if err != nil {
			return err
		}
		if backup {
			return errBackupBytes
		}
		// P2-D16, gap-47 §8.3: a reference created inside the grace window saves
		// the blob from the sweeper.
		if err := tx.ClearBlobUnreferenced(r.Context(), blobID); err != nil {
			return err
		}
		if err := tx.PutPendingBlobRef(r.Context(), blobID, ch.ID, s.DeviceID, "", now); err != nil {
			return err
		}
		// The channel's one reference has one owner, and the insert above keeps an existing row.
		// When that row is another user's, a 200 would leave the caller with no reference of their
		// own — one they could neither confirm nor delete, and which would vanish when its owner's
		// pending row expired (security review, reference squatting). Refuse instead: a forward
		// re-encrypts under a fresh key (protocol/09 § Blobs), so only the owner re-PUTs these bytes.
		if err := callerOwnsReference(r.Context(), tx, blobID, ch.ID, s); err != nil {
			return err
		}
		if quota <= 0 {
			return nil
		}
		// Exact, and inside the transaction: each distinct blob the user
		// references counts once, so re-publishing their own attachment into a
		// second channel costs nothing.
		used, err := tx.UserBlobBytes(r.Context(), s.UserID)
		if err != nil {
			return err
		}
		if used > quota {
			return errQuota
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errTombstoned) {
			// The purge's own unlink may have run before this upload wrote the
			// file; purged bytes stay removed, so they go now — unless a backups
			// row still names them (fix-wave review NEW-5, as 3caee12 on the
			// backup route): a purge of backup bytes keeps the file while the
			// backup is served, and this refused attachment must not unlink it.
			// After the tombstone no new backups row can name the bytes, so "not
			// named" is stable and the unlink is safe.
			named, nerr := b.repo.BackupRefersToBlob(r.Context(), blobID)
			if nerr != nil {
				b.log.ErrorContext(r.Context(), "check purged bytes against backups",
					"blob_id", hex.EncodeToString(blobID), "err", nerr)
			} else if !named {
				if derr := b.store.Delete(blobID); derr != nil {
					b.log.ErrorContext(r.Context(), "remove purged bytes an upload rewrote",
						"blob_id", hex.EncodeToString(blobID), "err", derr)
				}
			}
			server.WriteError(w, errPruned())
			return
		}
		if errors.Is(err, errBackupBytes) {
			// The bytes are a backup object's and stay as they are: nothing to orphan.
			server.WriteError(w, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest,
				"these bytes are a backup object; an attachment cannot share them")))
			return
		}
		if errors.Is(err, errOthersReference) {
			// The bytes are referenced here by their owner and stay as they are: nothing to orphan.
			server.WriteError(w, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeNotUploader,
				"this channel already holds these bytes under another uploader; re-encrypt and upload them anew")))
			return
		}
		if errors.Is(err, errBytesGone) {
			// Unlinked under the upload: no row may name a missing file, so nothing to orphan.
			server.WriteError(w, errBytesGoneRetry())
			return
		}
		if created {
			b.orphan(r.Context(), blobID, n, now)
		}
		if errors.Is(err, errQuota) {
			server.WriteError(w, errStorageFull())
			return
		}
		b.log.ErrorContext(r.Context(), "record blob reference", "err", err)
		server.WriteError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	if err := server.EncodeBody(w, status, []any{blobID, uint64(n)}); err != nil { //nolint:gosec // G115: n is a byte count io.Copy returned, never negative
		b.log.Error("encode blob put", "err", err)
	}
}

// errOthersReference aborts the reference transaction when the channel's reference to the bytes
// belongs to another user (409 E_NOT_UPLOADER).
var errOthersReference = errors.New("api: the channel's reference is another user's")

// callerOwnsReference is nil when the channel's reference to blobID was made by one of the
// session user's devices, and errOthersReference otherwise — including a reference whose device row
// is gone, which nobody can prove to be theirs (the confirm and delete routes refuse it alike).
func callerOwnsReference(ctx context.Context, tx store.Repository, blobID []byte, channelID id.ID, s auth.Session) error {
	ref, err := tx.GetBlobRef(ctx, blobID, channelID)
	if err != nil {
		return err
	}
	if ref.UploaderDevice == s.DeviceID {
		return nil
	}
	dev, err := tx.GetDevice(ctx, ref.UploaderDevice)
	if errors.Is(err, store.ErrNotFound) {
		return errOthersReference
	}
	if err != nil {
		return err
	}
	if dev.UserID != s.UserID {
		return errOthersReference
	}
	return nil
}

// orphan records a file the refused transaction left without a row, marked
// unreferenced, so the sweeper (task 11) collects it after the grace window
// instead of it lingering on disk forever. The file is not unlinked here: an
// identical upload may be referencing the same bytes at this very moment, and
// MarkBlobUnreferenced leaves a referenced blob alone.
func (b *Blobs) orphan(ctx context.Context, blobID []byte, n, now int64) {
	// The request may be what failed (a client that went away mid-transaction),
	// so this bookkeeping must not die with it.
	ctx = context.WithoutCancel(ctx)
	err := b.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.PutBlob(ctx, store.BlobRow{
			BlobID: blobID, Size: uint64(n), //nolint:gosec // G115: n is a byte count io.Copy returned, never negative
			StorageRef: blob.StorageRef(b.cfg.Backend, blobID), Created: now,
		}); err != nil {
			return err
		}
		return tx.MarkBlobUnreferenced(ctx, blobID, now)
	})
	if err != nil {
		b.log.ErrorContext(ctx, "record an unreferenced upload", "blob_id", hex.EncodeToString(blobID), "err", err)
	}
}

// refuseTombstoned is 410 E_PRUNED for bytes an instance admin purged, on PUT
// and on GET alike: the purge removed every reference, and the answer says why
// rather than a bare 404.
func (b *Blobs) refuseTombstoned(ctx context.Context, blobID []byte) error {
	return refuseTombstoned(ctx, b.repo, blobID)
}

// refuseTombstoned is the tombstone gate every route that stores bytes under their hash applies:
// the blob routes and PUT /v1/backups alike.
func refuseTombstoned(ctx context.Context, repo store.Repository, blobID []byte) error {
	tomb, err := repo.GetBlobTombstone(ctx, blobID)
	if err != nil {
		return err
	}
	if tomb {
		return errPruned()
	}
	return nil
}

func errPruned() *server.Error {
	return server.Errorf(server.CodePruned, "these bytes were removed by the server operator")
}

func errStorageFull() *server.Error {
	return server.Errorf(server.CodeStorageFull, "your attachment quota is exhausted")
}

// get is GET (and HEAD) /v1/channels/{id}/blobs/{blob_id}. The answer is 404
// whenever this channel holds no reference, even if the bytes exist: the
// response must not tell a caller that a blob they cannot reach exists
// (gap-47 §4.2).
func (b *Blobs) get(w http.ResponseWriter, r *http.Request) {
	s, ch, blobID, err := b.channel(r, PermReadHistory)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := b.refuseTombstoned(r.Context(), blobID); err != nil {
		server.WriteError(w, err)
		return
	}
	if _, err := b.repo.GetBlobRef(r.Context(), blobID, ch.ID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if err := requireCommunityWire(r.Context(), b.repo, s.WireVersion, ch.CommunityID); err != nil {
		server.WriteError(w, err)
		return
	}
	f, _, err := b.store.Get(blobID)
	if errors.Is(err, blob.ErrNotFound) {
		// A reference with no file is a storage fault (an operator rm, a restore
		// that skipped the blob directory). The client holds the key and can
		// re-PUT the identical bytes (gap-47 §8.5), so 404 is the useful answer.
		b.log.WarnContext(r.Context(), "blob referenced but missing on disk", "blob_id", hex.EncodeToString(blobID))
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if err != nil {
		server.WriteError(w, err)
		return
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	h := w.Header()
	// Content-Type BEFORE ServeContent, so it never sniffs ciphertext.
	h.Set("Content-Type", octetStream)
	h.Set("Content-Disposition", "attachment")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	// The content hash IS the strong validator, and ServeContent uses it for
	// If-Match, If-None-Match and If-Range.
	h.Set("ETag", `"`+hex.EncodeToString(blobID)+`"`)
	// Repr-Digest (RFC 9530), not Content-Digest: it describes the selected
	// representation, which is still the blob on a 416 whose body is an error
	// string (gap-47 §19.3).
	h.Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(blobID)+":")
	// name "" infers nothing from an extension; the zero modtime sends no
	// Last-Modified, which is meaningless after a restore. This one call answers
	// 206, 416, If-Range and 304.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// confirm is POST /v1/channels/{id}/blobs/{blob_id}/confirm (L-HTTP-82): the uploading user says
// the reference is in use, so the pending sweep (blobs.pending_ttl) leaves it alone. No body is read.
func (b *Blobs) confirm(w http.ResponseWriter, r *http.Request) {
	s, ch, blobID, err := b.channel(r, PermViewChannel)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := b.refuseTombstoned(r.Context(), blobID); err != nil {
		server.WriteError(w, err)
		return
	}
	err = b.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBlob(r.Context(), blobID); err != nil {
			return err
		}
		ref, err := tx.GetBlobRef(r.Context(), blobID, ch.ID)
		if err != nil {
			return err
		}
		dev, err := tx.GetDevice(r.Context(), ref.UploaderDevice)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err != nil || dev.UserID != s.UserID {
			return server.Errorf(server.CodeNotUploader, "only the uploading user may confirm this object")
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, ch.CommunityID); err != nil {
			return err
		}
		return tx.ConfirmBlobRef(r.Context(), blobID, ch.ID)
	})
	if errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if err != nil {
		server.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// delete is DELETE /v1/channels/{id}/blobs/{blob_id}: it removes this channel's
// REFERENCE, never the bytes (gap-47 INV-B3). When it was the last reference
// anywhere the blob is marked unreferenced in the same transaction, and the
// sweeper unlinks the file once blobs.gc_grace has passed. Deleting a reference
// that is already gone is a success, so a retry is harmless.
func (b *Blobs) delete(w http.ResponseWriter, r *http.Request) {
	s, ch, blobID, err := b.channel(r, PermViewChannel)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// R29 and R34: at v1 only the uploading user may delete, from any of their
	// devices. There is NO moderator fallback — a channel's manage-messages holder
	// is refused here like anyone else. Moderator deletion of an attachment needs a
	// signed moderation event, which is follow-up card 2; letting PermManageMessages
	// drop another user's reference would ship exactly the capability R29 defers,
	// with no signed event and no audit row.
	now := b.clk.Now().Unix()
	if err := b.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockBlob(r.Context(), blobID); err != nil {
			return err
		}
		ref, err := tx.GetBlobRef(r.Context(), blobID, ch.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} // An absent reference is already deleted.
		if err != nil {
			return err
		}
		dev, err := tx.GetDevice(r.Context(), ref.UploaderDevice)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// A reference whose device row is gone has no provable uploader; only the admin purge can remove it.
		if err != nil || dev.UserID != s.UserID {
			return server.Errorf(server.CodeNotUploader, "only the uploading user may delete this object")
		}
		if err := requireCommunityWire(r.Context(), tx, s.WireVersion, ch.CommunityID); err != nil {
			return err
		}
		if err := tx.DeleteBlobRef(r.Context(), blobID, ch.ID); err != nil {
			return err
		}
		// Sets unref_since only when no reference is left; the statement's own
		// NOT EXISTS makes it safe to call unconditionally.
		return tx.MarkBlobUnreferenced(r.Context(), blobID, now)
	}); err != nil {
		server.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
