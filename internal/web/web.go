// Package web serves the status page, its JSON twin and the pages that
// edit the train list.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"trains/internal/config"
	"trains/internal/store"
	"trains/internal/tracker"
	vt "trains/internal/viaggiatreno"
)

//go:embed templates/*.html
var templates embed.FS

const historyRuns = 14

type Server struct {
	Config func() *config.Config
	Store  *store.Store
	Source tracker.Source // for looking up train numbers while editing
	Log    *slog.Logger
	Now    func() time.Time
	// Changed is called after the train list is edited.
	Changed func()
	// Version is shown at the bottom of the page.
	Version string
	// Refresh fetches fresh data for today's trains now.
	Refresh func(ctx context.Context) (int, error)

	pages *template.Template
}

type Train struct {
	Key       string `json:"key"`
	Label     string `json:"label,omitempty"`
	Number    int    `json:"number"`
	NotifyAt  string `json:"notify_at,omitempty"`
	LateAfter int    `json:"late_after"`
	Days      string `json:"days"` // when it notifies, e.g. "Mon–Fri"
	// Platform is the departure platform at the boarding stop, when known.
	Platform string `json:"platform,omitempty"`
	// PlatformNote says "planned" or "changed from 3".
	PlatformNote string `json:"platform_note,omitempty"`
	// BoardAt names the boarding stop when it isn't the starting station.
	BoardAt string `json:"board_at,omitempty"`
	// Week has one entry per day, Monday first: whether it notifies then,
	// and which day is today.
	Week    []weekDay        `json:"-"`
	Today   *store.Reading   `json:"today"`
	History []*store.Reading `json:"history"`
	// Problem is today's latest fetch error when there is no reading yet,
	// e.g. an ambiguous train number.
	Problem string `json:"problem,omitempty"`
	// boardOffset is how long after leaving its first station the train
	// leaves the boarding stop, and waiting is whether it is yet to leave
	// it today; both are known only for a boarding stop past the first.
	boardOffset time.Duration
	waiting     bool
}

func (s *Server) Handler() http.Handler {
	s.pages = template.Must(template.New("").Funcs(template.FuncMap{
		"hhmm":     hhmm,
		"ago":      func(t time.Time) string { return ago(s.Now().Sub(t)) },
		"title":    tracker.Title,
		"position": tracker.Position,
		"delay":    delayText,
		"level":    level,
		"minutes":  minutes,
		"delaySign": func(m int) string {
			if m > 0 {
				return fmt.Sprintf("+%d", m)
			}
			return fmt.Sprint(m)
		},
	}).ParseFS(templates, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/trains", s.api)
	mux.HandleFunc("GET /edit", s.edit)
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("GET /stops", s.stops)
	mux.HandleFunc("GET /api/history", s.historyAPI)
	mux.HandleFunc("POST /save", s.save)
	mux.HandleFunc("POST /delete", s.delete)
	mux.HandleFunc("POST /refresh", s.refresh)
	// Links and forms are relative, so the pages also work under a path
	// prefix (behind the dashboard's proxy). Cross-site form posts are
	// refused.
	return http.NewCrossOriginProtection().Handler(mux)
}

func (s *Server) trains(ctx context.Context) ([]Train, error) {
	now := s.Now().In(vt.Rome)
	today := now.Format(store.DateLayout)
	cfg := s.Config()
	list, err := s.Store.Trains(ctx)
	if err != nil {
		return nil, err
	}
	var out []Train
	for _, c := range list {
		c = cfg.WithDefaults(c)
		t := Train{Key: c.Key(), Label: c.Label, Number: c.Number, NotifyAt: c.NotifyAt,
			LateAfter: c.LateAfter, Days: c.Days.Describe(), Week: week(c.Days, now.Weekday())}
		history, err := s.Store.History(ctx, c.Key(), historyRuns+1)
		if err != nil {
			return nil, err
		}
		if len(history) > 0 && history[0].RunDate == today {
			t.Today, history = history[0], history[1:]
		}
		t.History = history[:min(len(history), historyRuns)]
		if t.Today != nil {
			rd, err := s.Store.RunReading(ctx, c.Key(), today)
			if err != nil {
				return nil, err
			}
			if rd != nil {
				t.Platform, t.PlatformNote, t.BoardAt = boardingPlatform(rd.Raw, c.Board)
				t.boardOffset, t.waiting = boardingTime(rd.Raw, c.Board)
			}
		} else if c.Board != "" && len(history) > 0 {
			// Not running yet today: the last run says when it gets there.
			rd, err := s.Store.RunReading(ctx, c.Key(), history[0].RunDate)
			if err != nil {
				return nil, err
			}
			if rd != nil {
				t.boardOffset, _ = boardingTime(rd.Raw, c.Board)
			}
		}
		if t.Today == nil {
			if t.Problem, _, err = s.Store.LastFetchError(ctx, c.Key(), today); err != nil {
				return nil, err
			}
			// No run listed today isn't a problem worth a warning.
			if strings.HasPrefix(t.Problem, tracker.ErrNotRunning.Error()) {
				t.Problem = ""
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	trains, err := s.trains(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	byNextDeparture(trains, s.Now())
	// The page reloads itself only while a train is running; the rest of
	// the day nothing changes until the next departure.
	now := s.Now()
	var updated time.Time
	active := false
	for _, t := range trains {
		if t.Today != nil {
			active = active || tracker.Active(t.Today, now)
			if t.Today.FetchedAt.After(updated) {
				updated = t.Today.FetchedAt
			}
		}
	}
	wantsNotify := slices.ContainsFunc(trains, func(t Train) bool { return t.NotifyAt != "" })
	s.render(w, http.StatusOK, "index.html", map[string]any{"Trains": trains, "Now": now.In(vt.Rome),
		"WantsNotify": wantsNotify,
		"Notify":      len(s.Config().Notifiers) > 0, "Active": active, "Updated": updated,
		"Reload": int(reloadEvery.Seconds()), "Version": s.Version,
		"Notice": r.URL.Query().Get("notice")})
}

// reloadEvery is how often the page reloads while trains run, matching the
// tracker's default polling.
const reloadEvery = 5 * time.Minute

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	notice := "nothing to refresh"
	if s.Refresh != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		switch n, err := s.Refresh(ctx); {
		case err != nil:
			s.Log.Warn("refresh did not finish", "err", err)
			notice = "refresh failed"
		case n == 1:
			notice = "refreshed 1 train"
		case n > 1:
			notice = fmt.Sprintf("refreshed %d trains", n)
		default:
			notice = "nothing to refresh: no train runs today, or all were just updated"
		}
	}
	w.Header().Set("Location", "./?notice="+url.QueryEscape(notice))
	w.WriteHeader(http.StatusSeeOther)
}
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf strings.Builder
	if err := s.pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, buf.String())
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	trains, err := s.trains(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"trains": trains})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("serving page", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// byNextDeparture puts the trains on their way right now (gone from the
// boarding stop, not yet arrived or cancelled) at the top, earliest first,
// then the others by when they next leave the boarding stop: today if they
// haven't yet, otherwise tomorrow at the time of their last known run.
// Trains never seen running come last, in their order.
func byNextDeparture(trains []Train, now time.Time) {
	now = now.In(vt.Rome)
	gone := func(t Train) bool {
		return t.Today != nil && vt.State(t.Today.State) == vt.Running && !t.waiting
	}
	next := func(t Train) time.Time {
		if t.Today != nil && (vt.State(t.Today.State) == vt.NotDeparted || t.waiting) {
			return t.Today.ScheduledDeparture.Add(t.boardOffset)
		}
		var dep time.Time
		if t.Today != nil {
			dep = t.Today.ScheduledDeparture
		} else if len(t.History) > 0 {
			dep = t.History[0].ScheduledDeparture
		}
		if dep.IsZero() {
			return dep
		}
		dep = dep.Add(t.boardOffset).In(vt.Rome)
		at := time.Date(now.Year(), now.Month(), now.Day(), dep.Hour(), dep.Minute(), 0, 0, vt.Rome)
		if t.Today != nil || at.Before(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at
	}
	slices.SortStableFunc(trains, func(a, b Train) int {
		switch ga, gb := gone(a), gone(b); {
		case ga && gb:
			return a.Today.ScheduledDeparture.Add(a.boardOffset).Compare(b.Today.ScheduledDeparture.Add(b.boardOffset))
		case ga:
			return -1
		case gb:
			return 1
		}
		switch na, nb := next(a), next(b); {
		case na.IsZero() && nb.IsZero():
			return 0
		case na.IsZero():
			return 1
		case nb.IsZero():
			return -1
		default:
			return na.Compare(nb)
		}
	})
}

// boardingTime reads, from a raw status, how long after leaving its first
// station the train is due to leave the boarding stop board, and whether
// it has yet to leave it. Both are zero when board is empty, the first
// station, or not among the stops.
func boardingTime(raw, board string) (offset time.Duration, waiting bool) {
	var st vt.Status
	if board == "" || json.Unmarshal([]byte(raw), &st) != nil || len(st.Stops) < 2 {
		return 0, false
	}
	first := st.Stops[0]
	for _, s := range st.Stops[1:] {
		if string(s.ID) != board {
			continue
		}
		from, to := first.ScheduledDepartureMs, s.ScheduledDepartureMs
		if from == 0 {
			from = first.ScheduledMs
		}
		if to == 0 {
			to = s.ScheduledMs
		}
		if from == 0 || to == 0 {
			return 0, false
		}
		return time.Duration(to-from) * time.Millisecond, s.ActualDepartureMs == 0 && s.ActualMs == 0
	}
	return 0, false
}

// boardingPlatform reads the departure platform at the boarding stop (the
// first stop unless board names another) from a raw status. The actual
// platform wins over the planned one; a change is noted.
func boardingPlatform(raw, board string) (platform, note, at string) {
	var st vt.Status
	if json.Unmarshal([]byte(raw), &st) != nil || len(st.Stops) == 0 {
		return "", "", ""
	}
	stop := st.Stops[0]
	if board != "" {
		for i, s := range st.Stops {
			if string(s.ID) == board {
				stop = s
				if i > 0 {
					at = tracker.Title(string(s.Station))
				}
				break
			}
		}
	}
	planned := strings.TrimSpace(string(stop.PlannedDeparturePlatform))
	actual := strings.TrimSpace(string(stop.ActualDeparturePlatform))
	switch {
	case actual != "" && planned != "" && actual != planned:
		return actual, "changed from " + planned, at
	case actual != "":
		return actual, "", at
	case planned != "":
		return planned, "planned", at
	}
	return "", "", ""
}

type weekDay struct {
	On    bool
	Today bool
}

func week(days config.Days, today time.Weekday) []weekDay {
	out := make([]weekDay, 7)
	for i := range out {
		wd := time.Weekday((i + 1) % 7) // Monday first
		out[i] = weekDay{On: days.Has(wd), Today: wd == today}
	}
	return out
}

func hhmm(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	return t.In(vt.Rome).Format("15:04")
}

func day(date string) string {
	t, err := time.ParseInLocation(store.DateLayout, date, vt.Rome)
	if err != nil {
		return date
	}
	return t.Format("Mon 2 Jan")
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func delayText(r *store.Reading) string {
	switch {
	case vt.State(r.State) == vt.Cancelled:
		return "Cancelled"
	case r.Delay > 0:
		return fmt.Sprintf("+%d min", r.Delay)
	case r.Delay < 0:
		return fmt.Sprintf("%d min", r.Delay)
	}
	return "On time"
}

// level grades a reading for colouring: ok, late or bad.
func level(r *store.Reading, lateAfter int) string {
	switch {
	case vt.State(r.State) == vt.Cancelled || r.Delay >= 3*lateAfter:
		return "bad"
	case r.Delay >= lateAfter || r.Partial:
		return "late"
	}
	return "ok"
}
