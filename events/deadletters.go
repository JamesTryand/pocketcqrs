package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// DeadLetter is a failed function delivery, captured for inspection and
// retry. Written by the functions runtime when an effect function fails on
// an event; retried through the deadletter CLI.
type DeadLetter struct {
	ID          int64     `json:"id"`
	Consumer    string    `json:"consumer"`
	EventPos    int64     `json:"eventPos"`
	Event       Event     `json:"event"`
	Error       string    `json:"error"`
	Attempts    int64     `json:"attempts"`
	FirstFailed string    `json:"firstFailed"`
	LastFailed  string    `json:"lastFailed"`
	Resolved    bool      `json:"resolved"`
}

const deadLettersSchema = `
CREATE TABLE IF NOT EXISTS dead_letters (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	consumer     TEXT NOT NULL,
	event_pos    INTEGER NOT NULL,
	event        TEXT NOT NULL,
	error        TEXT NOT NULL,
	attempts     INTEGER NOT NULL DEFAULT 1,
	first_failed TEXT NOT NULL,
	last_failed  TEXT NOT NULL,
	resolved     INTEGER NOT NULL DEFAULT 0
);
`

func nowString() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05.000Z")
}

// AddDeadLetter records a failed delivery of ev by consumer.
func (s *Store) AddDeadLetter(ctx context.Context, consumer string, ev Event, cause error) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	now := nowString()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO dead_letters (consumer, event_pos, event, error, first_failed, last_failed)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		consumer, ev.Position, string(raw), cause.Error(), now, now)
	return err
}

// DeadLetters lists dead letters, pending only unless includeResolved.
func (s *Store) DeadLetters(ctx context.Context, includeResolved bool) ([]DeadLetter, error) {
	query := `SELECT id, consumer, event_pos, event, error, attempts, first_failed, last_failed, resolved
	          FROM dead_letters`
	if !includeResolved {
		query += ` WHERE resolved = 0`
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeadLetter
	for rows.Next() {
		var dl DeadLetter
		var rawEvent string
		var resolved int
		if err := rows.Scan(&dl.ID, &dl.Consumer, &dl.EventPos, &rawEvent, &dl.Error,
			&dl.Attempts, &dl.FirstFailed, &dl.LastFailed, &resolved); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(rawEvent), &dl.Event); err != nil {
			return nil, err
		}
		dl.Resolved = resolved != 0
		out = append(out, dl)
	}
	return out, rows.Err()
}

// ResolveDeadLetter marks a dead letter resolved (retry succeeded or dismissed).
func (s *Store) ResolveDeadLetter(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE dead_letters SET resolved = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// FailDeadLetterRetry records a failed retry attempt.
func (s *Store) FailDeadLetterRetry(ctx context.Context, id int64, cause error) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dead_letters SET attempts = attempts + 1, last_failed = ?, error = ? WHERE id = ?`,
		nowString(), cause.Error(), id)
	return err
}
