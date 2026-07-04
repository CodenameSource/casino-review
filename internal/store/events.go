package store

import (
	"context"
	"encoding/json"
)

// EventRow is one row of the events spine, read back for notification fan-out.
type EventRow struct {
	ID         int64
	Type       string
	Actor      string
	ContextRef string
	Payload    json.RawMessage
}

// EventsAfter reads events past a cursor, filtered by type prefix (e.g.
// "market." matches market.created, market.resolved, …).
//
// The 3s visibility grace matters: event IDs are allocated at INSERT inside
// money transactions, so a slow transaction can commit an event with a LOWER
// id after a reader already advanced past it. Only reading rows older than the
// grace keeps id-ordering an honest cursor (ledger transactions run in
// milliseconds; 3s is generous).
func (s *Store) EventsAfter(ctx context.Context, after int64, prefixes []string, limit int) ([]EventRow, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, event_type, actor, context_ref, payload FROM events
		 WHERE id > $1 AND event_type LIKE ANY($2) AND created_at <= now() - interval '3 seconds'
		 ORDER BY id LIMIT $3`, after, likePatterns(prefixes), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.ID, &e.Type, &e.Actor, &e.ContextRef, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxEventID returns the current tail of the events spine.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(id),0) FROM events`).Scan(&id)
	return id, err
}

func likePatterns(prefixes []string) []string {
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p + "%"
	}
	return out
}
