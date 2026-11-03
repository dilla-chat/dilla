package blob_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/blob"
)

func newStore(t *testing.T) (*blob.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := blob.Open(dir, "fs")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestPutStreamsAndVerifiesTheHash(t *testing.T) {
	s, dir := newStore(t)
	payload := bytes.Repeat([]byte("ciphertext"), 1000)
	sum := sha256.Sum256(payload)

	n, created, err := s.Put(t.Context(), sum[:], bytes.NewReader(payload), 1<<20)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !created || n != int64(len(payload)) {
		t.Fatalf("Put = (%d, %v)", n, created)
	}
	// The file lands at the two-level fan-out the storage_ref names.
	hex := blob.StorageRef("fs", sum[:])
	want := filepath.Join(dir, strings.TrimPrefix(hex, "fs:"))
	if fi, err := os.Stat(want); err != nil {
		t.Fatalf("stat %s: %v", want, err)
	} else if fi.Size() != int64(len(payload)) {
		t.Fatalf("size = %d", fi.Size())
	}
	// A second Put of the same bytes is not new.
	if _, created, err = s.Put(t.Context(), sum[:], bytes.NewReader(payload), 1<<20); err != nil || created {
		t.Fatalf("second Put = (%v, %v), want created=false", created, err)
	}
}

func TestPutRejectsAMismatchAndLeavesNothingBehind(t *testing.T) {
	s, dir := newStore(t)
	payload := []byte("actual bytes")
	wrong := sha256.Sum256([]byte("different bytes"))

	if _, _, err := s.Put(t.Context(), wrong[:], bytes.NewReader(payload), 1<<20); !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("Put = %v, want ErrHashMismatch", err)
	}
	var files []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("a rejected upload left %v behind", files)
	}
}

func TestPutRefusesOverTheLimitWithoutBufferingIt(t *testing.T) {
	s, _ := newStore(t)
	big := bytes.Repeat([]byte("x"), 4096)
	sum := sha256.Sum256(big)
	if _, _, err := s.Put(t.Context(), sum[:], bytes.NewReader(big), 1024); !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("Put over the limit = %v, want ErrTooLarge", err)
	}
}

func TestOsRootBlocksAnEscape(t *testing.T) {
	s, dir := newStore(t)
	outside := filepath.Join(filepath.Dir(dir), "escaped")
	// StorageRef is derived from the 32-byte id, so a traversal cannot be
	// spelled through the API; assert the guarantee os.Root gives anyway, since
	// it is what makes the derivation safe rather than merely tidy.
	if err := blob.WriteRelativeForTest(s, "../escaped", []byte("x")); err == nil {
		t.Fatal("os.Root allowed a write outside the root")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("a file appeared outside the root: %v", err)
	}
}

func TestGetReturnsASeekableFile(t *testing.T) {
	s, _ := newStore(t)
	payload := []byte("0123456789")
	sum := sha256.Sum256(payload)
	if _, _, err := s.Put(t.Context(), sum[:], bytes.NewReader(payload), 1<<20); err != nil {
		t.Fatalf("Put: %v", err)
	}
	f, size, err := s.Get(sum[:])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer f.Close()
	if size != int64(len(payload)) {
		t.Fatalf("size = %d", size)
	}
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(rest) != "456789" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestSweepTempRemovesACrashedUpload(t *testing.T) {
	s, dir := newStore(t)
	shard := filepath.Join(dir, "att", "aa", "bb")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	leftover := filepath.Join(shard, "aabb00.tmp-deadbeef")
	if err := os.WriteFile(leftover, []byte("half"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	n, err := s.SweepTemp()
	if err != nil {
		t.Fatalf("SweepTemp: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepTemp removed %d files, want 1", n)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("the temp file survived: %v", err)
	}
}

// gap-47 §6.2: the PUT is the proof of ownership. A Put of an id that is
// already stored must still hash the bytes it is given; answering "already
// present" without reading them would let anyone who has only seen a blob_id
// claim the object into a channel they can read, and download it from there.
func TestPutOfAStoredIDStillVerifiesTheBody(t *testing.T) {
	s, dir := newStore(t)
	payload := []byte("stored once")
	sum := sha256.Sum256(payload)
	if _, _, err := s.Put(t.Context(), sum[:], bytes.NewReader(payload), 1<<20); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, body := range [][]byte{nil, []byte("something else")} {
		if _, _, err := s.Put(t.Context(), sum[:], bytes.NewReader(body), 1<<20); !errors.Is(err, blob.ErrHashMismatch) {
			t.Fatalf("Put of a stored id with body %q = %v, want ErrHashMismatch", body, err)
		}
	}
	// The stored object is untouched and no temp file is left beside it.
	f, size, err := s.Get(sum[:])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || size != int64(len(payload)) || !bytes.Equal(got, payload) {
		t.Fatalf("stored object = %q (%d, %v)", got, size, err)
	}
	var files []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files under the root = %v, want the one object", files)
	}
}
