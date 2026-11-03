package sqlite

import (
	"context"

	"github.com/dilla-chat/dilla/internal/store"
)

// searchSQL never aliases readable_messages_fts: `snippet(f, …)` and
// `WHERE f MATCH …` both fail with `no such column: f` (gap-69 §2.8). Column 0
// is body and column 1 is channel_hex, so the snippet takes 0 and bm25 weights
// channel_hex at zero.
//
// The ORDER BY is the reported bm25 expression itself, never the FTS5 `rank`
// column: rank is bm25 with every column weighted 1.0, and every hit matches its
// channel's hex token, whose IDF grows as the channel shrinks. Ordering by rank
// therefore favoured small channels, disagreed with the score each hit reports,
// and let LIMIT drop a better hit (TestHitsAreBestFirstByTheReportedScore). bm25
// is negative-is-better, so ascending is best first; ties break on the newer
// seq, as on Postgres.
const searchSQL = `
SELECT m.channel_id, m.seq, m.sender, m.created,
       snippet(readable_messages_fts, 0, '[', ']', '…', 12),
       bm25(readable_messages_fts, 1.0, 0.0)
FROM readable_messages_fts
JOIN readable_messages m ON m.id = readable_messages_fts.rowid
WHERE readable_messages_fts MATCH ?
  AND m.deleted IS NULL
  AND (? = 0 OR m.seq < ?)
ORDER BY bm25(readable_messages_fts, 1.0, 0.0), m.seq DESC
LIMIT ?`

// SearchReadable is hand-written database/sql: sqlc can type neither the FTS5
// MATCH nor the implicit rowid (gap-69 §5.1). Raw user input never reaches
// MATCH: the expression is rendered from the parsed query, which is safe by
// construction (gap-69 §4.2). store.ChunkedSearch handles the empty and the wide
// scope.
func (r *Repo) SearchReadable(ctx context.Context, q store.ReadableSearchQuery) ([]store.ReadableSearchHit, error) {
	return store.ChunkedSearch(ctx, q, r.searchChunk)
}

// searchChunk runs one statement over at most store.MaxSearchChannels channels.
func (r *Repo) searchChunk(ctx context.Context, q store.ReadableSearchQuery) ([]store.ReadableSearchHit, error) {
	hexes := make([]string, 0, len(q.ChannelIDs))
	for _, c := range q.ChannelIDs {
		hexes = append(hexes, c.String())
	}
	// The channel filter lives INSIDE the match expression: intersecting two
	// doclists is 20x faster than post-filtering a global match (gap-69 §2.6).
	match := q.Query.FTS5Match(hexes)
	// BeforeSeq is bounded above the store (the API's queryFrom saturates a
	// cursor at MaxInt64), so the conversion cannot wrap.
	before := int64(q.BeforeSeq)
	rows, err := r.read.QueryContext(ctx, searchSQL, match, before, before, q.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.ReadableSearchHit
	for rows.Next() {
		var h store.ReadableSearchHit
		var score float64
		if err := rows.Scan(&h.ChannelID, &h.Seq, &h.Sender, &h.Created, &h.Snippet, &score); err != nil {
			return nil, err
		}
		// bm25 is negative-is-better; the interface is higher-is-better on both
		// engines, so the sign is flipped here and nowhere else.
		h.Score = float32(-score)
		out = append(out, h)
	}
	return out, rows.Err()
}
