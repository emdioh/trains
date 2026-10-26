// Package store keeps every reading, notification and fetch error in a
// SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS readings (
	id                  INTEGER PRIMARY KEY,
	train_key           TEXT    NOT NULL,
	number              INTEGER NOT NULL,
	origin              TEXT    NOT NULL,
	run_date            TEXT    NOT NULL,
	fetched_at          INTEGER NOT NULL,
	state               TEXT    NOT NULL,
	partial             INTEGER NOT NULL,
	delay               INTEGER NOT NULL,
	last_seen_station   TEXT    NOT NULL,
	last_seen_at        INTEGER NOT NULL,
	scheduled_departure INTEGER NOT NULL,
	scheduled_arrival   INTEGER NOT NULL,
	name                TEXT    NOT NULL,
	origin_name         TEXT    NOT NULL,
	destination_name    TEXT    NOT NULL,
	raw                 TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS readings_run ON readings (train_key, run_date, id);

CREATE TABLE IF NOT EXISTS notifications (
	id        INTEGER PRIMARY KEY,
	train_key TEXT    NOT NULL,
	run_date  TEXT    NOT NULL,
	at        INTEGER NOT NULL,
	sent      INTEGER NOT NULL,
	message   TEXT    NOT NULL,
	error     TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS notifications_run ON notifications (train_key, run_date);

CREATE TABLE IF NOT EXISTS fetch_errors (
	id        INTEGER PRIMARY KEY,
	train_key TEXT    NOT NULL,
	run_date  TEXT    NOT NULL,
	at        INTEGER NOT NULL,
	error     TEXT    NOT NULL
);
`

// DateLayout is how run dates are stored.
const DateLayout = "2006-01-02"

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Reading struct {
	TrainKey           string    `json:"-"`
	Number             int       `json:"number"`
	Origin             string    `json:"origin"`
	RunDate            string    `json:"run_date"`
	FetchedAt          time.Time `json:"fetched_at"`
	State              string    `json:"state"`
	Partial            bool      `json:"partially_cancelled"`
	Delay              int       `json:"delay"`
	LastSeenStation    string    `json:"last_seen_station,omitempty"`
	LastSeenAt         time.Time `json:"last_seen_at"`
	ScheduledDeparture time.Time `json:"scheduled_departure"`
	ScheduledArrival   time.Time `json:"scheduled_arrival"`
	Name               string    `json:"name"`
	OriginName         string    `json:"origin_name"`
	DestinationName    string    `json:"destination_name"`
	Raw                string    `json:"-"`
}

func (s *Store) AddReading(ctx context.Context, r *Reading) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO readings (train_key, number, origin, run_date,
		fetched_at, state, partial, delay, last_seen_station, last_seen_at, scheduled_departure,
		scheduled_arrival, name, origin_name, destination_name, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TrainKey, r.Number, r.Origin, r.RunDate, unix(r.FetchedAt), r.State, r.Partial, r.Delay,
		r.LastSeenStation, unix(r.LastSeenAt), unix(r.ScheduledDeparture), unix(r.ScheduledArrival),
		r.Name, r.OriginName, r.DestinationName, r.Raw)
	return err
}

const readingColumns = `train_key, number, origin, run_date, fetched_at, state, partial, delay,
	last_seen_station, last_seen_at, scheduled_departure, scheduled_arrival, name,
	origin_name, destination_name`

// LatestReading returns the newest reading of a run, or nil if there is none.
func (s *Store) LatestReading(ctx context.Context, trainKey, runDate string) (*Reading, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+readingColumns+` FROM readings
		WHERE train_key = ? AND run_date = ? ORDER BY id DESC LIMIT 1`, trainKey, runDate)
	r, err := scanReading(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// History returns the last reading of each of the train's most recent
// runs, newest first.
func (s *Store) History(ctx context.Context, trainKey string, runs int) ([]*Reading, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+readingColumns+` FROM readings
		WHERE id IN (SELECT MAX(id) FROM readings WHERE train_key = ?
			GROUP BY run_date ORDER BY run_date DESC LIMIT ?)
		ORDER BY run_date DESC`, trainKey, runs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Reading
	for rows.Next() {
		r, err := scanReading(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanReading(row interface{ Scan(...any) error }) (*Reading, error) {
	var r Reading
	var fetched, lastSeen, dep, arr int64
	err := row.Scan(&r.TrainKey, &r.Number, &r.Origin, &r.RunDate, &fetched, &r.State, &r.Partial,
		&r.Delay, &r.LastSeenStation, &lastSeen, &dep, &arr, &r.Name, &r.OriginName, &r.DestinationName)
	if err != nil {
		return nil, err
	}
	r.FetchedAt, r.LastSeenAt = fromUnix(fetched), fromUnix(lastSeen)
	r.ScheduledDeparture, r.ScheduledArrival = fromUnix(dep), fromUnix(arr)
	return &r, nil
}

// AddCheck records the outcome of a train's notification check: sent
// tells whether a message went out, and sendErr is any delivery error.
func (s *Store) AddCheck(ctx context.Context, trainKey, runDate string, at time.Time, sent bool, message string, sendErr error) error {
	errText := ""
	if sendErr != nil {
		errText = sendErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO notifications (train_key, run_date, at, sent, message, error)
		VALUES (?, ?, ?, ?, ?, ?)`, trainKey, runDate, unix(at), sent, message, errText)
	return err
}

// Checked reports whether a run's notification check already happened.
func (s *Store) Checked(ctx context.Context, trainKey, runDate string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications
		WHERE train_key = ? AND run_date = ?`, trainKey, runDate).Scan(&n)
	return n > 0, err
}

func (s *Store) AddFetchError(ctx context.Context, trainKey, runDate string, at time.Time, fetchErr error) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO fetch_errors (train_key, run_date, at, error)
		VALUES (?, ?, ?, ?)`, trainKey, runDate, unix(at), fetchErr.Error())
	return err
}

// LastFetchError returns the newest fetch error of a run, or "" if there
// is none.
func (s *Store) LastFetchError(ctx context.Context, trainKey, runDate string) (string, time.Time, error) {
	var msg string
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT error, at FROM fetch_errors
		WHERE train_key = ? AND run_date = ? ORDER BY id DESC LIMIT 1`, trainKey, runDate).Scan(&msg, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return msg, fromUnix(at), err
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}
