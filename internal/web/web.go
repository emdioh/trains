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
	// Refresh fetches fresh data for today's trains now.
	Refresh func(ctx context.Context) error

	pages *template.Template
}

type Train struct {
	Key       string           `json:"key"`
	Label     string           `json:"label,omitempty"`
	Number    int              `json:"number"`
	NotifyAt  string           `json:"notify_at,omitempty"`
	LateAfter int              `json:"late_after"`
	RunsToday bool             `json:"runs_today"`
	Today     *store.Reading   `json:"today"`
	History   []*store.Reading `json:"history"`
	// Problem is today's latest fetch error when there is no reading yet,
	// e.g. an ambiguous train number.
	Problem string `json:"problem,omitempty"`
}

func (s *Server) Handler() http.Handler {
	s.pages = template.Must(template.New("").Funcs(template.FuncMap{
		"hhmm":     hhmm,
		"ago":      func(t time.Time) string { return ago(s.Now().Sub(t)) },
		"title":    tracker.Title,
		"position": tracker.Position,
		"delay":    delayText,
		"level":    level,
		"day":      day,
	}).ParseFS(templates, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/trains", s.api)
	mux.HandleFunc("GET /edit", s.edit)
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
			LateAfter: c.LateAfter, RunsToday: c.Days.Has(now.Weekday())}
		history, err := s.Store.History(ctx, c.Key(), historyRuns+1)
		if err != nil {
			return nil, err
		}
		if len(history) > 0 && history[0].RunDate == today {
			t.Today, history = history[0], history[1:]
		}
		t.History = history[:min(len(history), historyRuns)]
		if t.Today == nil && t.RunsToday {
			if t.Problem, _, err = s.Store.LastFetchError(ctx, c.Key(), today); err != nil {
				return nil, err
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
	s.render(w, http.StatusOK, "index.html", map[string]any{"Trains": trains, "Now": now.In(vt.Rome),
		"Notify": len(s.Config().Notifiers) > 0, "Active": active, "Updated": updated,
		"Reload": int(reloadEvery.Seconds())})
}

// reloadEvery is how often the page reloads while trains run, matching the
// tracker's default polling.
const reloadEvery = 5 * time.Minute

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if s.Refresh != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if err := s.Refresh(ctx); err != nil {
			s.Log.Warn("refresh did not finish", "err", err)
		}
	}
	backToList(w)
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
