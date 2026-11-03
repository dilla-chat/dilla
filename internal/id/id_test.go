package id_test

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestNewIsSixteenNonZeroBytesAndUnique(t *testing.T) {
	seen := make(map[id.ID]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		v := id.New()
		if v.IsZero() {
			t.Fatalf("draw %d is the zero id", i)
		}
		if _, dup := seen[v]; dup {
			t.Fatalf("draw %d repeated %s", i, v)
		}
		seen[v] = struct{}{}
	}
}

// A compile-time assertion, because len(id.New()) is a constant 16 for an
// array type and can never fail at run time.
var _ [16]byte = id.ID{}

func TestParseStringRoundTrip(t *testing.T) {
	v := id.New()
	got, err := id.Parse(v.String())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != v {
		t.Fatalf("round trip: %s != %s", got, v)
	}
}

func TestParseRejects(t *testing.T) {
	long := hex.EncodeToString(make([]byte, 17))
	for name, in := range map[string]string{
		"31 hex":     "0123456789abcdef0123456789abcde",
		"33 hex":     "0123456789abcdef0123456789abcdef0",
		"34 hex":     long,
		"upper case": "0123456789ABCDEF0123456789ABCDEF",
		"empty":      "",
		"non hex":    "0123456789abcdef0123456789abcdeg",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := id.Parse(in); err == nil {
				t.Fatalf("Parse(%q) accepted", in)
			}
		})
	}
}

func TestValueScanThroughSQL(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "id.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE t (k BLOB NOT NULL CHECK (length(k) = 16)) STRICT`); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := id.New()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO t (k) VALUES (?)`, want); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var got id.ID
	if err := db.QueryRowContext(t.Context(), `SELECT k FROM t`).Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != want {
		t.Fatalf("round trip through sql: %s != %s", got, want)
	}
}

func TestMarshalCBOREmitsBstr16(t *testing.T) {
	v := id.New()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) != 17 || b[0] != 0x50 {
		t.Fatalf("want 0x50 + 16 bytes, got %x", b)
	}
	if !bytes.Equal(b[1:], v[:]) {
		t.Fatalf("payload %x != %x", b[1:], v[:])
	}
	var back id.ID
	if err := cborx.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != v {
		t.Fatalf("cbor round trip: %s != %s", back, v)
	}
}

func TestUnmarshalCBORRejects(t *testing.T) {
	short := append([]byte{0x4f}, make([]byte, 15)...) // bstr 15
	array := []byte{0x81, 0x00}                        // [0]
	for name, in := range map[string][]byte{"15-byte bstr": short, "array": array} {
		t.Run(name, func(t *testing.T) {
			var v id.ID
			if err := cborx.Unmarshal(in, &v); err == nil {
				t.Fatalf("accepted %x", in)
			}
		})
	}
}
