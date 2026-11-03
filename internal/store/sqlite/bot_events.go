package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func (r *Repo) AppendBotStructuralEvent(ctx context.Context, event protocol.Event) (protocol.Event, error) {
	if !r.inTx || event.ID.IsZero() || event.CommunityID.IsZero() || event.Sequence != 0 || event.Timestamp > math.MaxInt64 {
		return protocol.Event{}, store.ErrConflict
	}
	if _, err := protocol.EncodeEvent(protocol.Event{ID: event.ID, CommunityID: event.CommunityID, Sequence: 1, Timestamp: event.Timestamp, Kind: event.Kind, Data: event.Data}); err != nil {
		return protocol.Event{}, store.ErrConflict
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO bot_event_streams(community_id,high_water,floor) VALUES(?,0,0) ON CONFLICT(community_id) DO NOTHING`, event.CommunityID); err != nil {
		return protocol.Event{}, wrap(err)
	}
	var highWater int64
	if err := r.tx.QueryRowContext(ctx, `SELECT high_water FROM bot_event_streams WHERE community_id=?`, event.CommunityID).Scan(&highWater); err != nil {
		return protocol.Event{}, wrap(err)
	}
	if highWater == math.MaxInt64 {
		return protocol.Event{}, store.ErrExhausted
	}
	event.Sequence = uint64(highWater + 1)
	body, err := protocol.EncodeEvent(event)
	if err != nil {
		return protocol.Event{}, store.ErrConflict
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO bot_structural_events(event_id,community_id,sequence,occurred_at,body) VALUES(?,?,?,?,?)`, event.ID, event.CommunityID, int64(event.Sequence), int64(event.Timestamp), body); err != nil {
		return protocol.Event{}, wrap(err)
	}
	if _, err := r.tx.ExecContext(ctx, `UPDATE bot_event_streams SET high_water=? WHERE community_id=?`, int64(event.Sequence), event.CommunityID); err != nil {
		return protocol.Event{}, wrap(err)
	}
	return event, nil
}

func (r *Repo) ListBotStructuralEvents(ctx context.Context, community id.ID, after uint64, limit int32) (page store.BotStructuralEventPage, retErr error) {
	if community.IsZero() || after > math.MaxInt64 || limit < 1 || limit > 250 {
		return page, store.ErrConflict
	}
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}
	q = r.rawRead
	if r.tx != nil {
		q = r.tx
	}
	rows, err := q.QueryContext(ctx, `SELECT s.high_water,s.floor,e.event_id,e.sequence,e.occurred_at,e.body FROM bot_event_streams s LEFT JOIN bot_structural_events e ON e.community_id=s.community_id AND e.sequence>? WHERE s.community_id=? ORDER BY e.sequence LIMIT ?`, int64(after), community, limit)
	if err != nil {
		return page, wrap(err)
	}
	defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
	page.Events = make([]protocol.Event, 0, limit)
	for rows.Next() {
		var highWater, floor sql.NullInt64
		var eventID []byte
		var sequence, timestamp sql.NullInt64
		var body []byte
		if err := rows.Scan(&highWater, &floor, &eventID, &sequence, &timestamp, &body); err != nil {
			return store.BotStructuralEventPage{}, err
		}
		if highWater.Valid {
			page.HighWater = uint64(highWater.Int64)
		}
		if floor.Valid {
			page.Floor = uint64(floor.Int64)
		}
		if body == nil {
			continue
		}
		event, err := protocol.DecodeEvent(body)
		if err != nil || protocol.ValidateEventContext(event, community) != nil || !bytes.Equal(event.ID[:], eventID) || !sequence.Valid || event.Sequence != uint64(sequence.Int64) || !timestamp.Valid || event.Timestamp != uint64(timestamp.Int64) {
			return store.BotStructuralEventPage{}, store.ErrConflict
		}
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		return store.BotStructuralEventPage{}, err
	}
	page.NextSequence = after
	if len(page.Events) > 0 {
		page.NextSequence = page.Events[len(page.Events)-1].Sequence
	}
	return page, nil
}

func (r *Repo) PruneBotStructuralEvents(ctx context.Context, before int64) (int64, error) {
	if !r.inTx {
		return 0, sql.ErrTxDone
	}
	rows, err := r.tx.QueryContext(ctx, `SELECT e.community_id,e.sequence,e.occurred_at,s.floor FROM bot_structural_events e JOIN bot_event_streams s ON s.community_id=e.community_id WHERE e.sequence>s.floor ORDER BY e.community_id,e.sequence`)
	if err != nil {
		return 0, wrap(err)
	}
	type pruned struct {
		community id.ID
		through   int64
	}
	type progress struct {
		floor, through int64
		blocked        bool
	}
	progressByCommunity := make(map[id.ID]*progress)
	rowsErr := func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
		for rows.Next() {
			var community id.ID
			var sequence, occurredAt, floor int64
			if err := rows.Scan(&community, &sequence, &occurredAt, &floor); err != nil {
				return err
			}
			p := progressByCommunity[community]
			if p == nil {
				p = &progress{floor: floor, through: floor}
				progressByCommunity[community] = p
			}
			if p.blocked {
				continue
			}
			if sequence != p.through+1 || occurredAt >= before {
				p.blocked = true
				continue
			}
			p.through = sequence
		}
		return wrap(rows.Err())
	}()
	if rowsErr != nil {
		return 0, rowsErr
	}
	var floors []pruned
	for community, p := range progressByCommunity {
		if p.through > p.floor {
			floors = append(floors, pruned{community: community, through: p.through})
		}
	}
	for _, v := range floors {
		if _, err := r.tx.ExecContext(ctx, `UPDATE bot_event_streams SET floor=MAX(floor,?) WHERE community_id=?`, v.through, v.community); err != nil {
			return 0, wrap(err)
		}
	}
	var n int64
	for _, v := range floors {
		res, err := r.tx.ExecContext(ctx, `DELETE FROM bot_structural_events WHERE community_id=? AND sequence<=?`, v.community, v.through)
		if err != nil {
			return 0, wrap(err)
		}
		deleted, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		n += deleted
	}
	return n, nil
}

func (r *Repo) AckBotStructuralEvents(ctx context.Context, installation, community id.ID, through uint64) error {
	if !r.inTx || installation.IsZero() || community.IsZero() || through > math.MaxInt64 {
		return store.ErrConflict
	}
	res, err := r.tx.ExecContext(ctx, `INSERT INTO bot_event_acks(installation_id,community_id,through_sequence) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM bot_installations WHERE id=? AND community_id=?) AND EXISTS(SELECT 1 FROM bot_event_streams WHERE community_id=? AND high_water>=?) ON CONFLICT(installation_id,community_id) DO UPDATE SET through_sequence=excluded.through_sequence WHERE bot_event_acks.through_sequence<=excluded.through_sequence`, installation, community, int64(through), installation, community, community, int64(through))
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return store.ErrConflict
	}
	return nil
}

func (r *Repo) GetBotStructuralEventAck(ctx context.Context, installation, community id.ID) (uint64, error) {
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
	q = r.rawRead
	if r.tx != nil {
		q = r.tx
	}
	var through sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT through_sequence FROM bot_event_acks WHERE installation_id=? AND community_id=?`, installation, community).Scan(&through)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, wrap(err)
	}
	if !through.Valid {
		return 0, nil
	}
	return uint64(through.Int64), nil
}
