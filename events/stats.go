package events

import (
	"context"
)

// EventTypeStat is one aggregate+type pair observed in the log, with its
// version range (empirical event types, for the catalog).
type EventTypeStat struct {
	Aggregate  string `json:"aggregate"`
	Type       string `json:"type"`
	Count      int64  `json:"count"`
	MinVersion int64  `json:"minVersion"`
	MaxVersion int64  `json:"maxVersion"`
}

// EventTypeStats returns every aggregate+type pair in the log, ordered by
// aggregate and first appearance.
func (s *Store) EventTypeStats(ctx context.Context) ([]EventTypeStat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT aggregate, type, COUNT(*), MIN(version), MAX(version)
		 FROM events GROUP BY aggregate, type ORDER BY aggregate, MIN(position)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventTypeStat
	for rows.Next() {
		var st EventTypeStat
		if err := rows.Scan(&st.Aggregate, &st.Type, &st.Count, &st.MinVersion, &st.MaxVersion); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Checkpoints returns every durable consumer checkpoint (name -> position).
func (s *Store) Checkpoints(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, position FROM projection_checkpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var pos int64
		if err := rows.Scan(&name, &pos); err != nil {
			return nil, err
		}
		out[name] = pos
	}
	return out, rows.Err()
}

// LogTotals returns the total event count and the number of distinct streams.
func (s *Store) LogTotals(ctx context.Context) (events int64, streams int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT aggregate || char(0x1f) || aggregate_id) FROM events`).
		Scan(&events, &streams)
	return events, streams, err
}

// MaxPosition returns the highest position in the log (0 when empty).
//
// This is the head of the log and the correct base for a consumer's
// behind-by: checkpoints record a POSITION, so they must be compared against
// a position. An event count is not interchangeable — position is
// AUTOINCREMENT, so any value that is allocated but never committed leaves
// the count permanently below the head.
func (s *Store) MaxPosition(ctx context.Context) (int64, error) {
	var max int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) FROM events`).Scan(&max)
	return max, err
}

// StreamCounts returns the number of distinct streams per aggregate.
func (s *Store) StreamCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT aggregate, COUNT(DISTINCT aggregate_id) FROM events GROUP BY aggregate ORDER BY aggregate`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var agg string
		var n int64
		if err := rows.Scan(&agg, &n); err != nil {
			return nil, err
		}
		out[agg] = n
	}
	return out, rows.Err()
}

// ReactorFlow is one observed reaction edge: a reactor (metadata.actor
// "reactor:<name>") produced target events in response to cause events
// (metadata.causationId). Empirical, from the log — reactions are events.
type ReactorFlow struct {
	Reactor         string `json:"reactor"`
	CauseAggregate  string `json:"causeAggregate"`
	CauseType       string `json:"causeType"`
	TargetAggregate string `json:"targetAggregate"`
	TargetType      string `json:"targetType"`
	Count           int64  `json:"count"`
}

// ReactorFlows aggregates the observed reactor mappings in the log.
func (s *Store) ReactorFlows(ctx context.Context) ([]ReactorFlow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT json_extract(e.metadata, '$.actor'),
		        c.aggregate, c.type, e.aggregate, e.type, COUNT(*)
		 FROM events e
		 JOIN events c ON c.id = json_extract(e.metadata, '$.causationId')
		 WHERE json_extract(e.metadata, '$.actor') LIKE 'reactor:%'
		 GROUP BY 1, 2, 3, 4, 5
		 ORDER BY 1, 2, 3, 4, 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReactorFlow
	for rows.Next() {
		var f ReactorFlow
		if err := rows.Scan(&f.Reactor, &f.CauseAggregate, &f.CauseType,
			&f.TargetAggregate, &f.TargetType, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
