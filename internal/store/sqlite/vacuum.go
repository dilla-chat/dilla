package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// autoVacuumIncremental is what PRAGMA auto_vacuum reads back for INCREMENTAL.
const autoVacuumIncremental = 2

// pageCounts reads page_count and freelist_count on conn.
func pageCounts(ctx context.Context, conn *sql.Conn) (pages, free int64, err error) {
	if err := conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, 0, fmt.Errorf("sqlite: read page_count: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return 0, 0, fmt.Errorf("sqlite: read freelist_count: %w", err)
	}
	return pages, free, nil
}

// checkpoint runs PRAGMA wal_checkpoint(mode) on conn. The pragma reports a busy checkpoint as a row, not an
// error, so busy is turned into one here.
func checkpoint(ctx context.Context, conn *sql.Conn, mode string) error {
	var busy, logFrames, done int
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &logFrames, &done); err != nil {
		return fmt.Errorf("sqlite: wal_checkpoint(%s): %w", mode, err)
	}
	if busy != 0 {
		return fmt.Errorf("sqlite: wal_checkpoint(%s) was blocked by a reader", mode)
	}
	return nil
}

// fileBytes is the main database file's size, or -1 when it cannot be read (an in-memory database, say).
func fileBytes(ctx context.Context, conn *sql.Conn) int64 {
	var seq int
	var name, file string
	if err := conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &file); err != nil || file == "" {
		return -1
	}
	st, err := os.Stat(file)
	if err != nil {
		return -1
	}
	return st.Size()
}

// EnsureIncrementalVacuum switches a database that still has auto_vacuum=0 to INCREMENTAL. The mode is a property
// of the file that only a VACUUM applies, so this runs that one VACUUM — which also returns every free page to the
// filesystem — and does nothing when the file is already incremental. It is meant for dillad's start, after the
// migrations and before it serves, and logs `compacting the database` first so a slow start on a large file is
// not mistaken for a hang.
//
// VACUUM cannot run in a transaction and the PRAGMA must reach the same connection, so both run on one pinned
// connection. A failure (a full disk, say) leaves the file exactly as it was, auto_vacuum still 0, and is returned
// for the caller to log; the next start tries again. The caller must not refuse to start because of it.
func EnsureIncrementalVacuum(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: compaction connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var mode int
	if err := conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return fmt.Errorf("sqlite: read auto_vacuum: %w", err)
	}
	if mode == autoVacuumIncremental {
		return nil
	}
	pagesBefore, freeBefore, err := pageCounts(ctx, conn)
	if err != nil {
		return err
	}
	bytesBefore := fileBytes(ctx, conn)
	log.Info("compacting the database",
		"page_count_before", pagesBefore, "freelist_count_before", freeBefore, "file_bytes_before", bytesBefore,
		"auto_vacuum_before", mode)
	started := time.Now()
	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("sqlite: set auto_vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("sqlite: compact: %w", err)
	}
	// In WAL mode the VACUUM's rewritten pages sit in the -wal file and the main file keeps its old size until a
	// checkpoint copies them back and truncates. Do it here so the space is returned now. The conversion is
	// already done, so a failed or busy checkpoint (a reader holding the WAL) is only a warning.
	if err := checkpoint(ctx, conn, "TRUNCATE"); err != nil {
		log.Warn("could not checkpoint the database after compacting; the file shrinks at the next checkpoint", "err", err)
	}
	pagesAfter, freeAfter, err := pageCounts(ctx, conn)
	if err != nil {
		return err
	}
	log.Info("database compacted",
		"page_count_before", pagesBefore, "freelist_count_before", freeBefore, "file_bytes_before", bytesBefore,
		"page_count_after", pagesAfter, "freelist_count_after", freeAfter, "file_bytes_after", fileBytes(ctx, conn),
		"duration", time.Since(started).Round(time.Millisecond))
	return nil
}

// IncrementalVacuum returns up to pages free pages to the filesystem and reports how many it freed. On a file that
// is not incremental it frees nothing.
func IncrementalVacuum(ctx context.Context, db *sql.DB, pages int) (freed int, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("sqlite: vacuum connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var before int
	if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&before); err != nil {
		return 0, fmt.Errorf("sqlite: read freelist_count: %w", err)
	}
	if before == 0 {
		return 0, nil
	}
	// incremental_vacuum yields one row per freed page; the statement does its work as the rows are stepped.
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages))
	if err != nil {
		return 0, fmt.Errorf("sqlite: incremental vacuum: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite: incremental vacuum: %w", err)
	}
	var after int
	if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&after); err != nil {
		return 0, fmt.Errorf("sqlite: read freelist_count: %w", err)
	}
	// The freed pages are in the WAL until a checkpoint copies them back. PASSIVE never waits for readers or
	// writers; what it cannot copy now it copies at a later pass. Its failure does not undo the freeing.
	_ = checkpoint(ctx, conn, "PASSIVE")
	return before - after, nil
}
