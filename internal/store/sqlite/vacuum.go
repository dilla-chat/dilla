package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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
	log.Info("compacting the database",
		"page_count_before", pagesBefore, "freelist_count_before", freeBefore, "auto_vacuum_before", mode)
	started := time.Now()
	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("sqlite: set auto_vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("sqlite: compact: %w", err)
	}
	pagesAfter, freeAfter, err := pageCounts(ctx, conn)
	if err != nil {
		return err
	}
	log.Info("database compacted",
		"page_count_before", pagesBefore, "freelist_count_before", freeBefore,
		"page_count_after", pagesAfter, "freelist_count_after", freeAfter,
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
	return before - after, nil
}
