package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRunIncrementalVacuumFreesAndStops(t *testing.T) {
	db, err := sqlite.OpenWrite(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, body BLOB)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200) INSERT INTO t(id, body) SELECT i, zeroblob(4000) FROM n`,
		`DELETE FROM t`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	var buf lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runIncrementalVacuum(ctx, db, 10*time.Millisecond, slog.New(slog.NewTextHandler(&buf, nil)))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "database free pages returned") {
		if time.Now().After(deadline) {
			t.Fatal("no pass freed pages")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runIncrementalVacuum did not stop on cancel")
	}
}
