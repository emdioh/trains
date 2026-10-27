// Package store keeps every reading, notification and fetch error in a
// SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"trains/internal/config"
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

CREATE TABLE IF NOT EXISTS trains (
	key        TEXT    PRIMARY KEY,
	number     INTEGER NOT NULL,
	origin     TEXT    NOT NULL,
	label      TEXT    NOT NULL,
	days       TEXT    NOT NULL,
	notify_at  TEXT    NOT NULL,
	late_after INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

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

// RunReading returns the newest reading of a run with its raw response, or
// nil if there is none.
func (s *Store) RunReading(ctx context.Context, trainKey, runDate string) (*Reading, error) {
	var raw string
	row := s.db.QueryRowContext(ctx, `SELECT `+readingColumns+`, raw FROM readings
		WHERE train_key = ? AND run_date = ? ORDER BY id DESC LIMIT 1`, trainKey, runDate)
	r, err := scanReading(scanWith(row, &raw))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if r != nil {
		r.Raw = raw
	}
	return r, err
}

// RunDates lists the dates of a train's recorded runs, newest first.
func (s *Store) RunDates(ctx context.Context, trainKey string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT run_date FROM readings
		WHERE train_key = ? ORDER BY run_date DESC LIMIT ?`, trainKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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

// FinalReadings returns the last reading of each of the train's runs since
// the given run date, oldest first, with its raw API response.
func (s *Store) FinalReadings(ctx context.Context, trainKey, since string) ([]*Reading, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+readingColumns+`, raw FROM readings
		WHERE id IN (SELECT MAX(id) FROM readings WHERE train_key = ? AND run_date >= ?
			GROUP BY run_date)
		ORDER BY run_date`, trainKey, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Reading
	for rows.Next() {
		var raw string
		r, err := scanReading(scanWith(rows, &raw))
		if err != nil {
			return nil, err
		}
		r.Raw = raw
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanWith appends extra destinations after a reading's columns.
type scanWithExtra struct {
	row   interface{ Scan(...any) error }
	extra []any
}

func scanWith(row interface{ Scan(...any) error }, extra ...any) scanWithExtra {
	return scanWithExtra{row, extra}
}

func (s scanWithExtra) Scan(dest ...any) error { return s.row.Scan(append(dest, s.extra...)...) }

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

// ErrTrainExists means another train already has the same number and origin.
var ErrTrainExists = errors.New("that train is already in the list")

// Trains returns the tracked trains in the order they were added.
func (s *Store) Trains(ctx context.Context) ([]config.Train, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT number, origin, label, days, notify_at, late_after
		FROM trains ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []config.Train
	for rows.Next() {
		var t config.Train
		var days string
		if err := rows.Scan(&t.Number, &t.Origin, &t.Label, &days, &t.NotifyAt, &t.LateAfter); err != nil {
			return nil, err
		}
		if t.Days, err = config.ParseDays(strings.Split(days, ",")); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveTrain adds a train, or replaces the one stored under oldKey. A train
// keeps its place in the list when edited.
func (s *Store) SaveTrain(ctx context.Context, oldKey string, t config.Train) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trains WHERE key = ? AND key != ?`,
		t.Key(), oldKey).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrTrainExists
	}
	args := []any{t.Key(), t.Number, t.Origin, t.Label, t.Days.String(), t.NotifyAt, t.LateAfter}
	res, err := tx.ExecContext(ctx, `UPDATE trains SET key = ?, number = ?, origin = ?, label = ?,
		days = ?, notify_at = ?, late_after = ? WHERE key = ?`, append(args, oldKey)...)
	if err != nil {
		return err
	}
	if updated, _ := res.RowsAffected(); updated == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO trains (key, number, origin, label, days,
			notify_at, late_after) VALUES (?, ?, ?, ?, ?, ?, ?)`, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteTrain(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM trains WHERE key = ?`, key)
	return err
}

// ImportTrains copies the trains listed in the config file into the
// database, the first time only: from then on the list is edited on the
// status page. It reports whether the import happened.
func (s *Store) ImportTrains(ctx context.Context, trains []config.Train) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO meta (key, value) VALUES ('trains_imported', ?)`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	for _, t := range trains {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trains (key, number, origin, label,
			days, notify_at, late_after) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			t.Key(), t.Number, t.Origin, t.Label, t.Days.String(), t.NotifyAt, t.LateAfter); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
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
