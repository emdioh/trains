// Package web serves the status page and its JSON twin.
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
	Log    *slog.Logger
	Now    func() time.Time

	page *template.Template
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
}

func (s *Server) Handler() http.Handler {
	s.page = template.Must(template.New("index.html").Funcs(template.FuncMap{
		"hhmm":     hhmm,
		"ago":      func(t time.Time) string { return ago(s.Now().Sub(t)) },
		"title":    tracker.Title,
		"position": tracker.Position,
		"delay":    delayText,
		"level":    level,
		"day":      day,
	}).ParseFS(templates, "templates/index.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/trains", s.api)
	return mux
}

func (s *Server) trains(ctx context.Context) ([]Train, error) {
	now := s.Now().In(vt.Rome)
	today := now.Format(store.DateLayout)
	var out []Train
	for _, c := range s.Config().Trains {
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
	var buf strings.Builder
	data := map[string]any{"Trains": trains, "Now": s.Now().In(vt.Rome),
		"Notify": len(s.Config().Notifiers) > 0}
	if err := s.page.Execute(&buf, data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
