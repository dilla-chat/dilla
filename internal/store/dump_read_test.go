package store_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/store"
)

// copyPayload is a PostgreSQL binary COPY payload of rows tuples, each of two
// fields: a three-byte value and a NULL.
func copyPayload(rows int) []byte {
	var b bytes.Buffer
	b.WriteString("PGCOPY\n\xff\r\n\x00")
	_ = binary.Write(&b, binary.BigEndian, int32(0)) // flags
	_ = binary.Write(&b, binary.BigEndian, int32(0)) // header extension length
	for range rows {
		_ = binary.Write(&b, binary.BigEndian, int16(2))
		_ = binary.Write(&b, binary.BigEndian, int32(3))
		b.WriteString("abc")
		_ = binary.Write(&b, binary.BigEndian, int32(-1))
	}
	_ = binary.Write(&b, binary.BigEndian, int16(-1))
	return b.Bytes()
}

type record struct {
	table   string
	payload []byte
}

func dumpStream(records ...record) []byte {
	var b bytes.Buffer
	b.WriteString(store.DumpMagic)
	_ = binary.Write(&b, binary.BigEndian, store.DumpVersion)
	for _, r := range records {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(r.table)))
		b.WriteString(r.table)
		_ = binary.Write(&b, binary.BigEndian, uint64(len(r.payload)))
		b.Write(r.payload)
	}
	return b.Bytes()
}

// ReadDump is DumpPostgres's reader, and CountCopyRows is what a dry run of a
// Postgres restore reports per table without a database.
func TestReadDumpWalksTheRecordsAndCountsTheirRows(t *testing.T) {
	stream := dumpStream(record{"users", copyPayload(2)}, record{"audit_log", copyPayload(0)}, record{"devices", copyPayload(5)})
	got := map[string]int64{}
	var order []string
	err := store.ReadDump(bytes.NewReader(stream), func(table string, payload io.Reader) error {
		n, err := store.CountCopyRows(payload)
		if err != nil {
			return err
		}
		got[table] = n
		order = append(order, table)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadDump: %v", err)
	}
	if strings.Join(order, ",") != "users,audit_log,devices" {
		t.Fatalf("order = %v", order)
	}
	if got["users"] != 2 || got["audit_log"] != 0 || got["devices"] != 5 {
		t.Fatalf("counts = %v", got)
	}
	// A callback that reads nothing still leaves the stream at the next record.
	var names []string
	if err := store.ReadDump(bytes.NewReader(stream), func(table string, _ io.Reader) error {
		names = append(names, table)
		return nil
	}); err != nil || len(names) != 3 {
		t.Fatalf("ReadDump skipping payloads = %v, %v", names, err)
	}
}

func TestReadDumpRefusesAForeignOrDamagedStream(t *testing.T) {
	good := dumpStream(record{"users", copyPayload(1)})
	for name, tc := range map[string]struct {
		stream []byte
		want   string
	}{
		"wrong magic":          {append([]byte("NOTADUMP"), good[8:]...), "not a DILLADMP stream"},
		"a table not dilla's":  {dumpStream(record{"pg_authid", copyPayload(1)}), "not a dilla table"},
		"a short payload":      {good[:len(good)-3], "short"},
		"a truncated header":   {good[:14], "record header"},
		"an oversized name":    {dumpStream(record{strings.Repeat("x", 65), nil}), "table of 65 bytes"},
		"another dump version": {append(append([]byte(store.DumpMagic), 0, 0, 0, 9), good[12:]...), "dump version 9"},
	} {
		t.Run(name, func(t *testing.T) {
			err := store.ReadDump(bytes.NewReader(tc.stream), func(string, io.Reader) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadDump = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := store.CountCopyRows(strings.NewReader("not a copy payload")); err == nil {
		t.Fatal("CountCopyRows accepted a payload with no signature")
	}
	if _, err := store.CountCopyRows(bytes.NewReader(copyPayload(3)[:30])); err == nil {
		t.Fatal("CountCopyRows accepted a payload with no trailer")
	}
}
