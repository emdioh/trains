// Package tracker polls ViaggiaTreno for the configured trains, records
// every reading and sends the notifications.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"trains/internal/config"
	"trains/internal/notify"
	"trains/internal/store"
	vt "trains/internal/viaggiatreno"
)

const (
	// Polling starts this long before the scheduled departure.
	leadTime = 30 * time.Minute
	// Polling gives up this long after the scheduled arrival if the train
	// never reports arriving.
	tailTime = 6 * time.Hour
	// A train ViaggiaTreno doesn't know today is retried this often.
	notFoundRetry = 30 * time.Minute
	// How long after notify_at a reading may still arrive. Past it, a
	// "couldn't check" notification is sent instead.
	notifyGrace = 30 * time.Minute
	// A forced refresh skips trains fetched more recently than this.
	refreshMinGap = 20 * time.Second
)

type Source interface {
	FindRuns(ctx context.Context, number int) ([]vt.Run, error)
	Status(ctx context.Context, run vt.Run) (*vt.Status, []byte, error)
}

type Tracker struct {
	ConfigPath string
	Source     Source
	Store      *store.Store
	Log        *slog.Logger
	Now        func() time.Time

	mu      sync.Mutex
	cfg     *config.Config
	cfgTime time.Time
	runs    map[string]*runState
	wake    chan struct{}
	refresh chan chan struct{}
}

// runState is what the tracker knows about today's run of one train.
type runState struct {
	date        string
	run         *vt.Run
	last        *store.Reading
	lastAttempt time.Time
	lastErr     error
	notFound    bool
	watching    bool // the notification window was open while running
	checked     bool // the notification check is done for today
}

func New(configPath string, cfg *config.Config, source Source, st *store.Store, log *slog.Logger) *Tracker {
	t := &Tracker{ConfigPath: configPath, Source: source, Store: st, Log: log, Now: time.Now,
		cfg: cfg, runs: map[string]*runState{}, wake: make(chan struct{}, 1), refresh: make(chan chan struct{})}
	if fi, err := os.Stat(configPath); err == nil {
		t.cfgTime = fi.ModTime()
	}
	return t
}

// Config returns the configuration currently in use.
func (t *Tracker) Config() *config.Config {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// Run ticks every minute, and when woken or asked to refresh, until ctx
// is done.
func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	t.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Tick(ctx)
		case <-t.wake:
			t.Tick(ctx)
		case done := <-t.refresh:
			t.tick(ctx, true)
			close(done)
		}
	}
}

// Refresh fetches every train running today now, outside the polling
// schedule, and returns once done. Trains fetched in the last few seconds
// are skipped.
func (t *Tracker) Refresh(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case t.refresh <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wake makes the tracker tick now, e.g. after the train list changed.
func (t *Tracker) Wake() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *Tracker) Tick(ctx context.Context) { t.tick(ctx, false) }

func (t *Tracker) tick(ctx context.Context, force bool) {
	t.reload()
	cfg := t.Config()
	trains, err := t.Store.Trains(ctx)
	if err != nil {
		t.Log.Error("reading the train list", "err", err)
		return
	}
	notifiers := notify.New(cfg.Notifiers)
	now := t.Now().In(vt.Rome)
	for _, train := range trains {
		if ctx.Err() != nil {
			return
		}
		t.tickTrain(ctx, cfg, cfg.WithDefaults(train), notifiers, now, force)
	}
}

// reload re-reads the configuration when the file changed. A file that
// doesn't parse is reported and the previous configuration kept.
func (t *Tracker) reload() {
	if t.ConfigPath == "" {
		return
	}
	fi, err := os.Stat(t.ConfigPath)
	if err != nil || fi.ModTime().Equal(t.cfgTime) {
		return
	}
	t.cfgTime = fi.ModTime()
	cfg, err := config.Load(t.ConfigPath)
	if err != nil {
		t.Log.Error("config not reloaded", "err", err)
		return
	}
	t.mu.Lock()
	t.cfg = cfg
	t.mu.Unlock()
	t.Log.Info("config reloaded", "notifiers", len(cfg.Notifiers))
	for _, w := range cfg.Warnings {
		t.Log.Warn(w)
	}
}

func (t *Tracker) tickTrain(ctx context.Context, cfg *config.Config, train config.Train, notifiers []notify.Notifier, now time.Time, force bool) {
	if !train.Days.Has(now.Weekday()) {
		return
	}
	key, today := train.Key(), now.Format(store.DateLayout)
	rs := t.runs[key]
	if rs == nil || rs.date != today {
		rs = &runState{date: today}
		t.runs[key] = rs
		var err error
		if rs.last, err = t.Store.LatestReading(ctx, key, today); err != nil {
			t.Log.Error("reading database", "train", key, "err", err)
		}
		if rs.checked, err = t.Store.Checked(ctx, key, today); err != nil {
			t.Log.Error("reading database", "train", key, "err", err)
		}
	}

	notifyAt, hasNotify := train.NotifyTime(now)
	if hasNotify && !rs.checked && !now.Before(notifyAt) && !now.After(notifyAt.Add(notifyGrace)) {
		rs.watching = true
	}
	// While the notification waits for a reading, retry every minute.
	needReading := rs.watching && !rs.checked && (rs.last == nil || rs.last.FetchedAt.Before(notifyAt))

	if needReading || t.due(rs, time.Duration(cfg.PollInterval), now, force) {
		if err := t.fetch(ctx, train, rs, now); err != nil {
			rs.lastErr = err
			t.Log.Warn("fetching status", "train", key, "err", err)
			if err := t.Store.AddFetchError(ctx, key, today, now, err); err != nil {
				t.Log.Error("writing database", "err", err)
			}
		} else {
			rs.lastErr = nil
		}
	}

	if rs.watching && !rs.checked {
		switch {
		case rs.last != nil && !rs.last.FetchedAt.Before(notifyAt):
			msg, late := Describe(train, rs.last)
			t.check(ctx, notifiers, key, today, now, late, msg)
			rs.checked = true
		case now.After(notifyAt.Add(notifyGrace)):
			t.check(ctx, notifiers, key, today, now, true, unknown(train, rs.lastErr))
			rs.checked = true
		}
	}
}

// due reports whether a train should be polled now: while it is Active,
// or on a forced refresh, unless it is over.
func (t *Tracker) due(rs *runState, interval time.Duration, now time.Time, force bool) bool {
	if force {
		return now.Sub(rs.lastAttempt) >= refreshMinGap && (rs.last == nil || !Finished(rs.last))
	}
	if rs.notFound {
		interval = max(interval, notFoundRetry)
	}
	if now.Sub(rs.lastAttempt) < interval {
		return false
	}
	return rs.last == nil || Active(rs.last, now) // without a reading the schedule isn't known yet
}

// Finished reports whether a run is over: arrived or cancelled.
func Finished(r *store.Reading) bool {
	switch vt.State(r.State) {
	case vt.Arrived, vt.Cancelled:
		return true
	}
	return false
}

// Active reports whether a run is worth following at now: from shortly
// before its scheduled departure until it arrives, is cancelled, or is
// long overdue.
func Active(r *store.Reading, now time.Time) bool {
	if Finished(r) {
		return false
	}
	if dep := r.ScheduledDeparture; !dep.IsZero() && now.Before(dep.Add(-leadTime)) {
		return false
	}
	if arr := r.ScheduledArrival; !arr.IsZero() && now.After(arr.Add(tailTime)) {
		return false
	}
	return true
}

func (t *Tracker) fetch(ctx context.Context, train config.Train, rs *runState, now time.Time) error {
	rs.lastAttempt = now
	rs.notFound = false
	if rs.run == nil {
		runs, err := t.Source.FindRuns(ctx, train.Number)
		if err != nil {
			rs.notFound = errors.Is(err, vt.ErrNotFound)
			return fmt.Errorf("finding today's run: %w", err)
		}
		if rs.run, err = PickRun(runs, train.Origin, now); err != nil {
			// Retrying soon won't help: a config change starts a new run state.
			rs.notFound = true
			return err
		}
	}
	status, raw, err := t.Source.Status(ctx, *rs.run)
	if err != nil {
		rs.notFound = errors.Is(err, vt.ErrNotFound)
		return err
	}
	r := toReading(train.Key(), *rs.run, status, raw, now)
	if err := t.Store.AddReading(ctx, r); err != nil {
		return fmt.Errorf("saving reading: %w", err)
	}
	rs.last = r
	return nil
}

// PickRun chooses today's run of a train. origin, when set, is a station
// code (S01700) or part of a station name (Milano Centrale) and must match
// exactly one run. Without it the number must be unambiguous: guessing
// would silently track the wrong train.
func PickRun(runs []vt.Run, origin string, now time.Time) (*vt.Run, error) {
	today := now.Format(store.DateLayout)
	var todays, matches []vt.Run
	for _, r := range runs {
		if r.Date.Format(store.DateLayout) != today {
			continue
		}
		todays = append(todays, r)
		if origin == "" || matchesOrigin(r, origin) {
			matches = append(matches, r)
		}
	}
	switch {
	case len(matches) == 1:
		return &matches[0], nil
	case len(todays) == 0:
		return nil, vt.ErrNotFound
	case len(matches) == 0:
		return nil, fmt.Errorf("no run from %q today; it runs from %s", origin, describeRuns(todays))
	case origin == "":
		return nil, fmt.Errorf("several trains have this number today, from %s: set origin to choose one", describeRuns(matches))
	}
	return nil, fmt.Errorf("origin %q matches several runs, from %s: be more specific", origin, describeRuns(matches))
}

func matchesOrigin(r vt.Run, origin string) bool {
	if strings.EqualFold(r.Origin, origin) {
		return true
	}
	want := normalizeStation(origin)
	return want != "" && strings.Contains(normalizeStation(r.OriginName), want)
}

// normalizeStation makes "Milano C.le" and "MILANO  C.LE" compare equal.
func normalizeStation(s string) string {
	return strings.Join(strings.Fields(strings.ToUpper(strings.ReplaceAll(s, ".", ". "))), " ")
}

func describeRuns(runs []vt.Run) string {
	var parts []string
	for _, r := range runs {
		parts = append(parts, fmt.Sprintf("%s (%s)", Title(r.OriginName), r.Origin))
	}
	return strings.Join(parts, ", ")
}

func toReading(key string, run vt.Run, s *vt.Status, raw []byte, now time.Time) *store.Reading {
	station, seenAt := s.LastSeen()
	name := string(s.Name)
	if name == "" {
		name = fmt.Sprint(run.Number)
	}
	return &store.Reading{
		TrainKey:           key,
		Number:             run.Number,
		Origin:             run.Origin,
		RunDate:            run.Date.Format(store.DateLayout),
		FetchedAt:          now,
		State:              string(s.State()),
		Partial:            s.PartiallyCancelled(),
		Delay:              int(s.Delay),
		LastSeenStation:    station,
		LastSeenAt:         seenAt,
		ScheduledDeparture: s.ScheduledDeparture(),
		ScheduledArrival:   s.ScheduledArrival(),
		Name:               name,
		OriginName:         string(s.Origin),
		DestinationName:    string(s.Destination),
		Raw:                string(raw),
	}
}

// check records the notification check and, when send is set, delivers
// the message.
func (t *Tracker) check(ctx context.Context, notifiers []notify.Notifier, key, date string, now time.Time, send bool, msg notify.Message) {
	var err error
	send = send && len(notifiers) > 0
	if send {
		err = notify.All(ctx, notifiers, msg)
		if err != nil {
			t.Log.Error("sending notification", "train", key, "err", err)
		} else {
			t.Log.Info("notification sent", "train", key, "title", msg.Title)
		}
	}
	if err := t.Store.AddCheck(ctx, key, date, now, send, msg.Title+"\n"+msg.Body, err); err != nil {
		t.Log.Error("writing database", "err", err)
	}
}
