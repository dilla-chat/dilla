package postgres

import (
	"context"

	"github.com/dilla-chat/dilla/internal/store"
)

// searchSQL is hand-written for symmetry with SQLite, where sqlc cannot type the
// search at all (gap-69 §5.2). The ::text and ::float4 casts are load-bearing:
// without them ts_headline resolves to its json overload and ts_rank_cd's float4
// comes back as the wrong Go type.
//
// $1 is the text search configuration, a BOUND parameter and never a per-row
// column: a tsquery derived from a row degrades the plan to a Function Scan and
// the GIN index stops being used (gap-69 §3.5, measured). The tsquery is written
// inline, `to_tsquery($1, $2)` at each use, rather than as a function in FROM:
// that is gap-69 §3.5's measured form whose Index Cond carries it, and it leaves
// the plan no function RTE to scan at all. to_tsquery is immutable, so the
// repetition costs nothing. $3 is the channel scope as bytea[], which the
// composite GIN over (channel_id, body_tsv) serves.
const searchSQL = `
SELECT m.channel_id, m.seq, m.sender, m.created,
       ts_headline($1::regconfig, m.body, to_tsquery($1::regconfig, $2::text),
                   'StartSel=[,StopSel=],MaxWords=12,MinWords=3')::text,
       ts_rank_cd(m.body_tsv, to_tsquery($1::regconfig, $2::text))::float4
FROM readable_messages m
WHERE m.body_tsv @@ to_tsquery($1::regconfig, $2::text)
  AND m.channel_id = ANY($3::bytea[])
  AND m.deleted IS NULL
  AND ($4::bigint = 0 OR m.seq < $4::bigint)
ORDER BY 6 DESC, m.seq DESC
LIMIT $5::bigint`

// searchConfig is one configuration instance-wide in v1 (gap-69 §3.5): simple
// plus the unaccent dictionary, created by 00009_readable.sql.
const searchConfig = "dilla_simple"

// SearchReadable is hand-written database/sql. Raw user input never reaches
// to_tsquery: the tsquery is rendered from the parsed query, which is safe by
// construction, where to_tsquery on raw input is a syntax error (gap-69 §3.6).
// store.ChunkedSearch handles the empty and the wide scope.
func (r *Repo) SearchReadable(ctx context.Context, q store.ReadableSearchQuery) ([]store.ReadableSearchHit, error) {
	return store.ChunkedSearch(ctx, q, r.searchChunk)
}

// searchChunk runs one statement over at most store.MaxSearchChannels channels.
func (r *Repo) searchChunk(ctx context.Context, q store.ReadableSearchQuery) ([]store.ReadableSearchHit, error) {
	rows, err := r.rawRead.QueryContext(ctx, searchSQL, searchConfig, q.Query.TSQuery(),
		byteaArray(q), int64(q.BeforeSeq), int64(q.Limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.ReadableSearchHit
	for rows.Next() {
		var h store.ReadableSearchHit
		if err := rows.Scan(&h.ChannelID, &h.Seq, &h.Sender, &h.Created, &h.Snippet, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// byteaArray is the channel scope as a [][]byte, which pgx encodes as bytea[].
func byteaArray(q store.ReadableSearchQuery) [][]byte {
	out := make([][]byte, 0, len(q.ChannelIDs))
	for _, c := range q.ChannelIDs {
		out = append(out, append([]byte(nil), c[:]...))
	}
	return out
}
