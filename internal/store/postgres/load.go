package postgres

import (
	"context"
	"database/sql"
	"io"

	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres/pgdb"
)

// LoadDump replays a store.DumpPostgres stream into db (store.LoadPostgres) and
// runs then with a repository bound to the SAME transaction before it commits:
// the archive and everything then writes land together, or nothing changes.
// `dillad restore` arms invariant 11's heal in then, so the database never
// holds a restored archive without its generation bump and heal.
func LoadDump(ctx context.Context, db *sql.DB, dump io.Reader, then func(tx store.Repository) error) error {
	return store.LoadPostgres(ctx, db, dump, func(tx *sql.Tx) error {
		if then == nil {
			return nil
		}
		q := pgdb.New(tx)
		return then(&Repo{db: db, tx: tx, w: q, r: q, inTx: true, rawRead: db})
	})
}
