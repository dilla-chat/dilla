package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Runs one committed replacement after a handler's early lookup but before its mutation transaction.
type replacingBlobRepo struct {
	store.Repository
	beforeTx func() error
}

// Models a competing reference replacement at the authorization/mutation boundary.
// A LockBlob lets that transaction finish before the handler reads; without it,
// the replacement commits after the old reference is read and before the write.
type interleavingBlobRepo struct {
	store.Repository
	armed           bool
	current, queued store.BlobRefRow
	deleted         bool
}

func (r *interleavingBlobRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	if !r.armed {
		return r.Repository.Tx(ctx, fn)
	}
	return r.Repository.Tx(ctx, func(tx store.Repository) error {
		return fn(&interleavingBlobTx{Repository: tx, parent: r})
	})
}

type interleavingBlobTx struct {
	store.Repository
	parent *interleavingBlobRepo
	locked bool
}

func (tx *interleavingBlobTx) LockBlob(ctx context.Context, blobID []byte) error {
	// The competing transaction wins before this transaction acquires the row lock.
	tx.parent.current = tx.parent.queued
	if err := tx.Repository.LockBlob(ctx, blobID); err != nil {
		return err
	}
	tx.locked = true
	return nil
}

func TestBlobMutationLockClosesReadToWriteWindow(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			e, community, owner := channelEnv(t)
			bs, err := blob.Open(t.TempDir(), "fs")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bs.Close() })
			repo := &interleavingBlobRepo{Repository: e.Repo}
			api.NewBlobs(repo, bs, api.NewResolver(repo), config.Default().Blobs, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
			ch, _, status := newChannel(t, e, community, owner, uint64(api.ChannelText), uint64(api.ModeE2EE), uint64(api.VisPrivate), "files")
			if status != http.StatusCreated {
				t.Fatalf("channel = %d", status)
			}
			_, replacement := e.NewUser("replacement")
			joinChannel(t, e, ch, replacement)
			payload := []byte("shared encrypted bytes")
			sum := sha256.Sum256(payload)
			url := blobURL(ch, sum[:])
			if status, body := e.DoRaw(http.MethodPut, url, owner, "application/octet-stream", payload); status != http.StatusCreated {
				t.Fatalf("initial PUT = %d %s", status, body)
			}
			repo.current, err = e.Repo.GetBlobRef(t.Context(), sum[:], ch)
			if err != nil {
				t.Fatal(err)
			}
			repo.queued = repo.current
			repo.queued.UploaderDevice = e.sess[replacement].DeviceID
			repo.armed = true
			if method == http.MethodPost {
				url += "/confirm"
			}
			status, body := e.Do(method, url, owner, nil)
			if status != http.StatusForbidden || e.ErrCode(body) != "E_NOT_UPLOADER" {
				t.Fatalf("%s during replacement = %d %s, want 403 E_NOT_UPLOADER", method, status, body)
			}
			if repo.deleted || repo.current.Confirmed || repo.current.UploaderDevice != e.sess[replacement].DeviceID {
				t.Fatalf("%s changed replacement reference: %+v deleted=%v", method, repo.current, repo.deleted)
			}
		})
	}
}

func (tx *interleavingBlobTx) GetBlobRef(_ context.Context, _ []byte, _ id.ID) (store.BlobRefRow, error) {
	if tx.locked {
		return tx.parent.current, nil
	}
	// A bare read observes the old uploader; the competing transaction then commits.
	old := tx.parent.current
	tx.parent.current = tx.parent.queued
	return old, nil
}

func (tx *interleavingBlobTx) ConfirmBlobRef(_ context.Context, _ []byte, _ id.ID) error {
	tx.parent.current.Confirmed = true
	return nil
}

func (tx *interleavingBlobTx) DeleteBlobRef(_ context.Context, _ []byte, _ id.ID) error {
	tx.parent.deleted = true
	return nil
}

func (tx *interleavingBlobTx) MarkBlobUnreferenced(_ context.Context, _ []byte, _ int64) error {
	return nil
}

func (r *replacingBlobRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	if r.beforeTx != nil {
		hook := r.beforeTx
		r.beforeTx = nil
		if err := hook(); err != nil {
			return err
		}
	}
	return r.Repository.Tx(ctx, fn)
}

func TestBlobMutationChecksCurrentUploaderAfterReferenceReplacement(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			e, community, owner := channelEnv(t)
			bs, err := blob.Open(t.TempDir(), "fs")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bs.Close() })
			repo := &replacingBlobRepo{Repository: e.Repo}
			api.NewBlobs(repo, bs, api.NewResolver(repo), config.Default().Blobs, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
			ch, _, status := newChannel(t, e, community, owner, uint64(api.ChannelText), uint64(api.ModeE2EE), uint64(api.VisPrivate), "files")
			if status != http.StatusCreated {
				t.Fatalf("channel = %d", status)
			}
			_, replacement := e.NewUser("replacement")
			joinChannel(t, e, ch, replacement)
			payload := []byte("shared encrypted bytes")
			sum := sha256.Sum256(payload)
			url := blobURL(ch, sum[:])
			if status, body := e.DoRaw(http.MethodPut, url, owner, "application/octet-stream", payload); status != http.StatusCreated {
				t.Fatalf("initial PUT = %d %s", status, body)
			}
			newDevice := e.sess[replacement].DeviceID
			repo.beforeTx = func() error {
				return e.Repo.Tx(t.Context(), func(tx store.Repository) error {
					if err := tx.DeleteBlobRef(t.Context(), sum[:], ch); err != nil {
						return err
					}
					return tx.PutPendingBlobRef(t.Context(), sum[:], ch, newDevice, "", e.Clk.Now().Unix())
				})
			}
			if method == http.MethodPost {
				url += "/confirm"
			}
			status, body := e.Do(method, url, owner, nil)
			if status != http.StatusForbidden || e.ErrCode(body) != "E_NOT_UPLOADER" {
				t.Fatalf("%s after replacement = %d %s, want 403 E_NOT_UPLOADER", method, status, body)
			}
			ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch)
			if err != nil || ref.UploaderDevice != newDevice || ref.Confirmed {
				t.Fatalf("replacement reference = %+v, %v; want pending and owned by replacement", ref, err)
			}
		})
	}
}

// blobEnv mounts the blob routes over a real blob directory with the default
// limits, and returns an end-to-end encrypted text channel the owner created.
func blobEnv(t *testing.T) (*env, id.ID, string) {
	t.Helper()
	d := config.Default().Blobs
	return blobEnvWithLimits(t, d.MaxBlobBytes, d.QuotaBytesPerUser)
}

// blobEnvWithLimits is blobEnv with max_blob_bytes and quota_bytes_per_user set.
func blobEnvWithLimits(t *testing.T, maxBlob, quota int64) (*env, id.ID, string) {
	t.Helper()
	return blobEnvWithConfig(t, func(c *config.Blobs) { c.MaxBlobBytes, c.QuotaBytesPerUser = maxBlob, quota })
}

// blobEnvWithConfig is blobEnv with the [blobs] section adjusted by set.
func blobEnvWithConfig(t *testing.T, set func(*config.Blobs)) (*env, id.ID, string) {
	t.Helper()
	e, cid, tok := channelEnv(t)
	bs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	cfg := config.Default().Blobs
	set(&cfg)
	log := slog.New(slog.DiscardHandler)
	api.NewBlobs(e.Repo, bs, api.NewResolver(e.Repo), cfg, e.Clk, log).Register(e.Mux)
	api.NewAdmin(e.Repo, bs, e.Clk, log).Register(e.Mux)
	e.Blobs = bs
	ch, _, status := newChannel(t, e, cid, tok, uint64(api.ChannelText), uint64(api.ModeE2EE),
		uint64(api.VisPrivate), "files")
	if status != http.StatusCreated {
		t.Fatalf("create channel = %d", status)
	}
	return e, ch, tok
}

// secondChannel creates another text channel in ch's community.
func secondChannel(t *testing.T, e *env, ch id.ID, tok string) id.ID {
	t.Helper()
	row, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	other, _, status := newChannel(t, e, *row.CommunityID, tok, uint64(api.ChannelText), uint64(api.ModeE2EE),
		uint64(api.VisPrivate), "other-"+id.New().String()[:6])
	if status != http.StatusCreated {
		t.Fatalf("create second channel = %d", status)
	}
	return other
}

func blobURL(ch id.ID, sum []byte) string {
	return "/v1/channels/" + ch.String() + "/blobs/" + hex.EncodeToString(sum)
}

func TestPutRejectsAHashMismatchWithNoRow(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("ciphertext")
	wrong := sha256.Sum256([]byte("other"))
	status, body := e.DoRaw(http.MethodPut,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(wrong[:]), tok,
		"application/octet-stream", payload)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT with a wrong digest = %d (%s)", status, body)
	}
	if code := e.ErrCode(body); code != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s", code)
	}
	if _, err := e.Repo.GetBlob(t.Context(), wrong[:]); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a rejected upload wrote a row")
	}
}

func TestASecondPutOfTheSameBytesIs200AndAddsAReference(t *testing.T) {
	e, ch, tok := blobEnv(t)
	other := secondChannel(t, e, ch, tok)
	payload := bytes.Repeat([]byte("x"), 4096)
	sum := sha256.Sum256(payload)
	url := func(c id.ID) string {
		return "/v1/channels/" + c.String() + "/blobs/" + hex.EncodeToString(sum[:])
	}
	if status, _ := e.DoRaw(http.MethodPut, url(ch), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("first PUT was not 201")
	}
	if status, _ := e.DoRaw(http.MethodPut, url(other), tok, "application/octet-stream", payload); status != http.StatusOK {
		t.Fatal("second PUT was not 200")
	}
	n, err := e.Repo.CountBlobRefs(t.Context(), sum[:])
	if err != nil {
		t.Fatalf("CountBlobRefs: %v", err)
	}
	if n != 2 {
		t.Fatalf("references = %d, want 2", n)
	}
}

func TestRangeAndHeadCarryTheRightHeaders(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("0123456789abcdef")
	sum := sha256.Sum256(payload)
	url := "/v1/channels/" + ch.String() + "/blobs/" + hex.EncodeToString(sum[:])
	if status, _ := e.DoRaw(http.MethodPut, url, tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}

	head := e.Request(t, http.MethodHead, url, tok, nil, nil)
	defer head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD = %d", head.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":                 "application/octet-stream",
		"Content-Length":               "16",
		"Accept-Ranges":                "bytes",
		"ETag":                         `"` + hex.EncodeToString(sum[:]) + `"`,
		"Content-Disposition":          "attachment",
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "private, max-age=31536000, immutable",
	} {
		if got := head.Header.Get(k); got != want {
			t.Errorf("HEAD %s = %q, want %q", k, got, want)
		}
	}
	if head.Header.Get("Last-Modified") != "" {
		t.Error("Last-Modified must not be sent: the mtime is meaningless after a restore")
	}

	get := e.Request(t, http.MethodGet, url, tok, map[string]string{"Range": "bytes=4-7"}, nil)
	defer get.Body.Close()
	if get.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d", get.StatusCode)
	}
	b, _ := io.ReadAll(get.Body)
	if string(b) != "4567" {
		t.Fatalf("range body = %q", b)
	}
	if got := get.Header.Get("Content-Range"); got != "bytes 4-7/16" {
		t.Fatalf("Content-Range = %q", got)
	}
}

func TestOverMaxBlobBytesIs413(t *testing.T) {
	e, ch, tok := blobEnvWithLimits(t, 1024, 1<<30)
	payload := bytes.Repeat([]byte("x"), 4096)
	sum := sha256.Sum256(payload)
	status, body := e.DoRaw(http.MethodPut,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok,
		"application/octet-stream", payload)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT over max_blob_bytes = %d", status)
	}
	if code := e.ErrCode(body); code != "E_TOO_LARGE" {
		t.Fatalf("code = %s", code)
	}
}

func TestOverTheUserQuotaIs507(t *testing.T) {
	e, ch, tok := blobEnvWithLimits(t, 1<<20, 4096)
	for i, want := range []int{http.StatusCreated, http.StatusInsufficientStorage} {
		payload := bytes.Repeat([]byte{byte('a' + i)}, 3000)
		sum := sha256.Sum256(payload)
		status, body := e.DoRaw(http.MethodPut,
			"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok,
			"application/octet-stream", payload)
		if status != want {
			t.Fatalf("upload %d = %d, want %d (%s)", i, status, want, body)
		}
		if want == http.StatusInsufficientStorage {
			if code := e.ErrCode(body); code != "E_STORAGE_FULL" {
				t.Fatalf("code = %s", code)
			}
		}
	}
}

// M3: a blob the user already references counts once toward the quota and is already on disk, so
// re-publishing it (a forward into a second channel) near quota_bytes_per_user or
// store_max_bytes is not refused by the pre-body byte checks, which count the Content-Length.
// A NEW blob of the same size is still refused 507.
func TestAReUploadOfABlobTheUserHoldsDoesNotCountAgainstTheLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Blobs)
	}{
		{"per-user quota", func(c *config.Blobs) { c.MaxBlobBytes, c.QuotaBytesPerUser = 1<<20, 4000 }},
		{"store_max_bytes", func(c *config.Blobs) { c.MaxBlobBytes, c.StoreMaxBytes = 1<<20, 4000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ch, tok := blobEnvWithConfig(t, tc.set)
			other := secondChannel(t, e, ch, tok)
			held := bytes.Repeat([]byte{'a'}, 3000)
			heldSum := sha256.Sum256(held)
			if status, body := e.DoRaw(http.MethodPut, blobURL(ch, heldSum[:]), tok, "application/octet-stream", held); status != http.StatusCreated {
				t.Fatalf("first PUT = %d (%s)", status, body)
			}
			if status, body := e.DoRaw(http.MethodPut, blobURL(other, heldSum[:]), tok, "application/octet-stream", held); status != http.StatusOK {
				t.Fatalf("a re-upload of a held blob near the limit = %d, want 200 (%s)", status, body)
			}
			fresh := bytes.Repeat([]byte{'b'}, 3000)
			freshSum := sha256.Sum256(fresh)
			status, body := e.DoRaw(http.MethodPut, blobURL(ch, freshSum[:]), tok, "application/octet-stream", fresh)
			if status != http.StatusInsufficientStorage {
				t.Fatalf("a NEW blob near the limit = %d, want 507 (%s)", status, body)
			}
		})
	}
}

// An upload refused over the quota after its bytes were written leaves a blobs
// row marked unreferenced, so the sweeper collects the file after the grace
// window rather than it staying on disk with no row forever. The user is not
// charged for it.
func TestAnUploadRefusedOverTheQuotaIsLeftForTheSweeper(t *testing.T) {
	e, ch, tok := blobEnvWithLimits(t, 1<<20, 4096)
	first := bytes.Repeat([]byte{'a'}, 3000)
	firstSum := sha256.Sum256(first)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, firstSum[:]), tok, "application/octet-stream", first); status != http.StatusCreated {
		t.Fatal("first PUT failed")
	}
	second := bytes.Repeat([]byte{'b'}, 3000)
	sum := sha256.Sum256(second)
	// Sent with no Content-Length, so the pre-body quota check cannot see its size and the body
	// is read: this is the path that leaves a file for the sweeper (one with a Content-Length
	// over the quota is refused before a byte is read, TestTheQuotaPreCheckCountsTheBody).
	if status := putUnsized(t, e, ch, tok, sum[:], second); status != http.StatusInsufficientStorage {
		t.Fatalf("second PUT = %d, want 507", status)
	}
	row, err := e.Repo.GetBlob(t.Context(), sum[:])
	if err != nil {
		t.Fatalf("GetBlob of the refused upload: %v", err)
	}
	if row.UnrefSince == nil || *row.UnrefSince != e.Clk.Now().Unix() {
		t.Fatalf("unref_since = %v, want now", row.UnrefSince)
	}
	if n, err := e.Repo.CountBlobRefs(t.Context(), sum[:]); err != nil || n != 0 {
		t.Fatalf("references = %d (%v), want 0", n, err)
	}
	if used, err := e.Repo.UserBlobBytes(t.Context(), userOf(t, e, tok)); err != nil || used != 3000 {
		t.Fatalf("UserBlobBytes = %d (%v), want 3000", used, err)
	}
}

func TestAFetchWithNoReferenceInThisChannelIs404(t *testing.T) {
	e, ch, tok := blobEnv(t)
	other := secondChannel(t, e, ch, tok)
	payload := []byte("shared")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut,
		"/v1/channels/"+ch.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok,
		"application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	// The bytes exist, but not in `other`. The answer must be 404, never 403:
	// the response must not tell a caller that a blob they cannot reach exists.
	status, body := e.Do(http.MethodGet,
		"/v1/channels/"+other.String()+"/blobs/"+hex.EncodeToString(sum[:]), tok, nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-channel GET = %d, want 404", status)
	}
	if code := e.ErrCode(body); code != "E_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
}

// gap-47 §6.2: the PUT is the proof of ownership. Knowing a blob_id is not
// enough to publish it into another channel: the body must hash to it even when
// the bytes are already stored, or anyone who saw the id could claim the object
// into a channel they read and download it from there.
func TestAPutOfAStoredBlobMustStillCarryItsBytes(t *testing.T) {
	e, ch, tok := blobEnv(t)
	other := secondChannel(t, e, ch, tok)
	payload := []byte("the ciphertext")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	status, body := e.DoRaw(http.MethodPut, blobURL(other, sum[:]), tok, "application/octet-stream", nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT of a stored id with an empty body = %d (%s), want 422", status, body)
	}
	if n, err := e.Repo.CountBlobRefs(t.Context(), sum[:]); err != nil || n != 1 {
		t.Fatalf("references = %d (%v), want 1: a claim by id alone added one", n, err)
	}
	if status, _ := e.Do(http.MethodGet, blobURL(other, sum[:]), tok, nil); status != http.StatusNotFound {
		t.Fatalf("GET in the claiming channel = %d, want 404", status)
	}
}

// The PUT answers [blob_id(bstr 32), size(uint)], and the object reads back
// byte for byte.
func TestPutAnswersTheIDAndSizeAndGetReturnsTheBytes(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := bytes.Repeat([]byte{0x5a}, 777)
	sum := sha256.Sum256(payload)
	status, body := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload)
	if status != http.StatusCreated {
		t.Fatalf("PUT = %d", status)
	}
	var out struct {
		_      struct{} `cbor:",toarray"`
		BlobID []byte
		Size   uint64
	}
	mustUnmarshalBody(t, body, &out)
	if !bytes.Equal(out.BlobID, sum[:]) || out.Size != uint64(len(payload)) {
		t.Fatalf("PUT answered %x/%d", out.BlobID, out.Size)
	}
	row, err := e.Repo.GetBlob(t.Context(), sum[:])
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if row.Size != uint64(len(payload)) || row.StorageRef != blob.StorageRef("fs", sum[:]) {
		t.Fatalf("blobs row = %+v", row)
	}
	ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch)
	if err != nil {
		t.Fatalf("GetBlobRef: %v", err)
	}
	if ref.UploaderDevice != sessionDevice(t, e, tok) {
		t.Fatalf("uploader_device = %v, want the session's device", ref.UploaderDevice)
	}
	get := e.Request(t, http.MethodGet, blobURL(ch, sum[:]), tok, nil, nil)
	defer get.Body.Close()
	got, _ := io.ReadAll(get.Body)
	if get.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("GET = %d, %d bytes", get.StatusCode, len(got))
	}
}

// §5.0: {blob_id} is 64 LOWERCASE hex, refused before the database or the
// filesystem is touched.
func TestAMalformedBlobIDIs400(t *testing.T) {
	e, ch, tok := blobEnv(t)
	sum := sha256.Sum256([]byte("x"))
	for _, bad := range []string{
		strings.ToUpper(hex.EncodeToString(sum[:])),
		hex.EncodeToString(sum[:31]),
		hex.EncodeToString(sum[:]) + "00",
	} {
		status, body := e.DoRaw(http.MethodPut, "/v1/channels/"+ch.String()+"/blobs/"+bad, tok,
			"application/octet-stream", []byte("x"))
		if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("PUT %s = %d", bad, status)
		}
	}
}

// NV9: a category has no messages and therefore no attachments.
func TestACategoryHoldsNoAttachments(t *testing.T) {
	e, ch, tok := blobEnv(t)
	row, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	cat, _, status := newChannel(t, e, *row.CommunityID, tok, uint64(api.ChannelCategory), uint64(api.ModeE2EE),
		uint64(api.VisPrivate), "cat")
	if status != http.StatusCreated {
		t.Fatalf("create category = %d", status)
	}
	payload := []byte("x")
	sum := sha256.Sum256(payload)
	status, body := e.DoRaw(http.MethodPut, blobURL(cat, sum[:]), tok, "application/octet-stream", payload)
	if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("PUT into a category = %d", status)
	}
}

// A caller who cannot view the channel learns nothing about it: 404, as for an
// unknown channel, on every verb.
func TestANonMemberGets404(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("members only")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	_, stranger := e.NewUser("stranger")
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), stranger, "application/octet-stream", payload); status != http.StatusNotFound {
		t.Fatalf("PUT by a non-member = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodGet, blobURL(ch, sum[:]), stranger, nil); status != http.StatusNotFound {
		t.Fatalf("GET by a non-member = %d, want 404", status)
	}
}

// An admin purge's tombstone refuses the same bytes again (gap-47 §7.3): content
// addressing would otherwise hand the purged name straight back.
func TestAPurgedBlobCannotBeUploadedAgain(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("purged")
	sum := sha256.Sum256(payload)
	if err := e.Repo.PutBlobTombstone(t.Context(), sum[:], "takedown", userOf(t, e, tok), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("PutBlobTombstone: %v", err)
	}
	status, body := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload)
	if status != http.StatusGone || e.ErrCode(body) != "E_PRUNED" {
		t.Fatalf("PUT of purged bytes = %d", status)
	}
	if _, err := e.Repo.GetBlob(t.Context(), sum[:]); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused upload wrote a row")
	}
}

// L-HTTP-81: a PUT writes a pending reference, which GET already serves; a second PUT into the same
// channel leaves the reference as it was.
func TestAPutWritesAPendingReferenceThatIsServed(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("pending until sent")
	sum := sha256.Sum256(payload)
	if status, body := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatalf("PUT = %d (%s)", status, body)
	}
	ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch)
	if err != nil || ref.Confirmed {
		t.Fatalf("after PUT: %+v, %v; want a pending reference", ref, err)
	}
	get := e.Request(t, http.MethodGet, blobURL(ch, sum[:]), tok, nil, nil)
	defer get.Body.Close()
	got, _ := io.ReadAll(get.Body)
	if get.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("GET of a pending reference = %d, %d bytes", get.StatusCode, len(got))
	}
}

// L-HTTP-82: the uploading user confirms, from any device and twice; another user who views the
// channel, a non-viewer and a channel without the reference are refused, and none of them changes
// the row. A PUT after the confirm leaves it confirmed.
func TestTheUploaderConfirmsAPendingReference(t *testing.T) {
	e, ch, tok := blobEnv(t)
	other := secondChannel(t, e, ch, tok)
	payload := []byte("a sent attachment")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	confirm := blobURL(ch, sum[:]) + "/confirm"

	_, otherTok := e.NewUser("other")
	joinChannel(t, e, ch, otherTok)
	if status, body := e.Do(http.MethodPost, confirm, otherTok, nil); status != http.StatusForbidden || e.ErrCode(body) != "E_NOT_UPLOADER" {
		t.Fatalf("confirm by another user = %d %s, want 403 E_NOT_UPLOADER", status, e.ErrCode(body))
	}
	_, stranger := e.NewUser("stranger")
	if status, body := e.Do(http.MethodPost, confirm, stranger, nil); status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("confirm by a non-viewer = %d %s, want 404 E_NOT_FOUND", status, e.ErrCode(body))
	}
	if status, body := e.Do(http.MethodPost, blobURL(other, sum[:])+"/confirm", tok, nil); status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("confirm in a channel without the reference = %d %s, want 404 E_NOT_FOUND", status, e.ErrCode(body))
	}
	if ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch); err != nil || ref.Confirmed {
		t.Fatalf("a refused confirm changed the reference: %+v, %v", ref, err)
	}

	user := userOf(t, e, tok)
	phone := seedDevices(t, e, user, 1)[0]
	phoneTok := "phone-" + user.String()
	e.sess[phoneTok] = sessionFor(user, phone)
	for _, by := range []string{phoneTok, tok} {
		status, body := e.Do(http.MethodPost, confirm, by, nil)
		if status != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("confirm by the uploading user = %d with %d body bytes, want 204 and none", status, len(body))
		}
	}
	if ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch); err != nil || !ref.Confirmed {
		t.Fatalf("after the confirm: %+v, %v; want confirmed", ref, err)
	}
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusOK {
		t.Fatal("a second PUT was not 200")
	}
	if ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch); err != nil || !ref.Confirmed {
		t.Fatalf("a PUT after the confirm made the reference pending again: %+v, %v", ref, err)
	}
}

// Bytes an instance administrator purged are 410 on confirm as on GET, and the row is untouched.
func TestAConfirmOfPurgedBytesIs410(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("taken down while pending")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	if err := e.Repo.PutBlobTombstone(t.Context(), sum[:], "takedown", userOf(t, e, tok), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("PutBlobTombstone: %v", err)
	}
	if status, body := e.Do(http.MethodPost, blobURL(ch, sum[:])+"/confirm", tok, nil); status != http.StatusGone || e.ErrCode(body) != "E_PRUNED" {
		t.Fatalf("confirm of purged bytes = %d %s, want 410 E_PRUNED", status, e.ErrCode(body))
	}
	if ref, err := e.Repo.GetBlobRef(t.Context(), sum[:], ch); err != nil || ref.Confirmed {
		t.Fatalf("a refused confirm changed the reference: %+v, %v", ref, err)
	}
}

// The uploader may delete a pending reference as a confirmed one (L-HTTP-81).
func TestTheUploaderDeletesAPendingReference(t *testing.T) {
	e, ch, tok := blobEnv(t)
	payload := []byte("discarded before sending")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("PUT failed")
	}
	if status, _ := e.Do(http.MethodDelete, blobURL(ch, sum[:]), tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE of a pending reference = %d, want 204", status)
	}
	if n, err := e.Repo.CountBlobRefs(t.Context(), sum[:]); err != nil || n != 0 {
		t.Fatalf("references = %d (%v), want 0", n, err)
	}
}

// sweepPast advances the clock by d and runs one sweep with the default windows.
func sweepPast(t *testing.T, e *env, d time.Duration) {
	t.Helper()
	e.Clk.Advance(d)
	sw := blob.NewSweeper(e.Repo, e.Blobs, e.Clk, 24*time.Hour, 24*time.Hour, time.Hour, slog.New(slog.DiscardHandler))
	if _, err := sw.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
}

// L-HTTP-82's attacker statement (security review lead "reference squatting"): a channel holds one
// reference per blob and it has one owner, so another user's PUT of the same ciphertext must not
// answer 200 over that owner's row — that user would hold no reference of their own, could neither
// confirm nor delete it, and would lose the attachment when the owner's pending row expired. It is
// 409 E_NOT_UPLOADER and the row is untouched; the uploader's own re-PUT stays idempotent; and the
// refused user's re-encrypted upload survives the sweep whatever the squatter does.
func TestAPutOverAnotherUsersReferenceIs409(t *testing.T) {
	e, ch, _ := blobEnv(t)
	_, squat := e.NewUser("squatter")
	joinChannel(t, e, ch, squat)
	_, victim := e.NewUser("victim")
	joinChannel(t, e, ch, victim)
	held := []byte("ciphertext the squatter already holds")
	x := sha256.Sum256(held)
	if status, body := e.DoRaw(http.MethodPut, blobURL(ch, x[:]), squat, "application/octet-stream", held); status != http.StatusCreated {
		t.Fatalf("squatter PUT = %d (%s)", status, body)
	}
	before, err := e.Repo.GetBlobRef(t.Context(), x[:], ch)
	if err != nil {
		t.Fatalf("GetBlobRef: %v", err)
	}
	e.Clk.Advance(23 * time.Hour)
	status, body := e.DoRaw(http.MethodPut, blobURL(ch, x[:]), victim, "application/octet-stream", held)
	if status != http.StatusConflict || e.ErrCode(body) != "E_NOT_UPLOADER" {
		t.Fatalf("a PUT over another user's reference = %d %s, want 409 E_NOT_UPLOADER", status, e.ErrCode(body))
	}
	if after, err := e.Repo.GetBlobRef(t.Context(), x[:], ch); err != nil || after.UploaderDevice != before.UploaderDevice ||
		after.Created != before.Created || after.Confirmed {
		t.Fatalf("the refused PUT changed the reference: %+v -> %+v, %v", before, after, err)
	}
	if status, body := e.DoRaw(http.MethodPut, blobURL(ch, x[:]), squat, "application/octet-stream", held); status != http.StatusOK {
		t.Fatalf("the uploader's own re-PUT = %d (%s), want 200", status, body)
	}
	// The reference is the user's, not the device's: their other device's re-PUT is 200 too.
	squatter := userOf(t, e, squat)
	phone := seedDevices(t, e, squatter, 1)[0]
	phoneTok := "phone-" + squatter.String()
	e.sess[phoneTok] = sessionFor(squatter, phone)
	if status, body := e.DoRaw(http.MethodPut, blobURL(ch, x[:]), phoneTok, "application/octet-stream", held); status != http.StatusOK {
		t.Fatalf("the uploader's re-PUT from another device = %d (%s), want 200", status, body)
	}
	// protocol/09's forward rule: the refused user re-encrypts under a fresh key and uploads new bytes.
	fresh := []byte("the same attachment under a fresh key")
	y := sha256.Sum256(fresh)
	if status, body := e.DoRaw(http.MethodPut, blobURL(ch, y[:]), victim, "application/octet-stream", fresh); status != http.StatusCreated {
		t.Fatalf("victim PUT of fresh bytes = %d (%s)", status, body)
	}
	if status, body := e.Do(http.MethodPost, blobURL(ch, y[:])+"/confirm", victim, nil); status != http.StatusNoContent {
		t.Fatalf("victim confirm = %d %s", status, e.ErrCode(body))
	}
	if status, body := e.DoRaw(http.MethodPut, blobURL(ch, y[:]), squat, "application/octet-stream", fresh); status != http.StatusConflict {
		t.Fatalf("squatter PUT over the victim's confirmed reference = %d (%s), want 409", status, body)
	}
	if status, body := e.Do(http.MethodDelete, blobURL(ch, y[:]), squat, nil); status != http.StatusForbidden || e.ErrCode(body) != "E_NOT_UPLOADER" {
		t.Fatalf("squatter DELETE of the victim's reference = %d %s, want 403 E_NOT_UPLOADER", status, e.ErrCode(body))
	}
	sweepPast(t, e, 48*time.Hour)
	r := e.Request(t, http.MethodGet, blobURL(ch, y[:]), victim, nil, nil)
	got, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK || !bytes.Equal(got, fresh) {
		t.Fatalf("the victim's confirmed upload after the sweep = %d; nobody else's action may make it expire", r.StatusCode)
	}
}

// The same ciphertext in ANOTHER channel is that channel's own reference: a second user's PUT there
// is the usual 200 and gives them a row they confirm.
func TestAnotherUsersPutIntoAnotherChannelIsTheirOwnReference(t *testing.T) {
	e, ch, tok := blobEnv(t)
	other := secondChannel(t, e, ch, tok)
	_, peer := e.NewUser("peer")
	joinChannel(t, e, ch, peer)
	joinChannel(t, e, other, peer)
	payload := []byte("published in two channels by two users")
	sum := sha256.Sum256(payload)
	if status, _ := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", payload); status != http.StatusCreated {
		t.Fatal("first PUT was not 201")
	}
	if status, body := e.DoRaw(http.MethodPut, blobURL(other, sum[:]), peer, "application/octet-stream", payload); status != http.StatusOK {
		t.Fatalf("a second user's PUT into another channel = %d (%s), want 200", status, body)
	}
	if status, body := e.Do(http.MethodPost, blobURL(other, sum[:])+"/confirm", peer, nil); status != http.StatusNoContent {
		t.Fatalf("their confirm = %d %s, want 204", status, e.ErrCode(body))
	}
}
