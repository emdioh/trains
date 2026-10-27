package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"trains/internal/config"
	"trains/internal/store"
	"trains/internal/tracker"
	vt "trains/internal/viaggiatreno"
)

// form is the edit page's state. Fields stay strings so that invalid input
// is shown back as typed.
type form struct {
	OldKey    string // empty when adding a train
	Number    string
	Origin    string
	Label     string
	Days      map[string]bool
	NotifyAt  string
	LateAfter string
	Board     string        // boarding stop's code; empty for the starting station
	Stops     []historyStop // the stops a train can be boarded at, when known

	DefaultLate int
	Notify      bool
	DayKeys     []string
	Error       string

	Looked      bool        // a lookup was asked for
	Runs        []runChoice // today's trains with this number
	LookupError string
}

type runChoice struct {
	Origin      string
	Description string
}

var dayLabels = map[string]string{"mon": "Mon", "tue": "Tue", "wed": "Wed", "thu": "Thu",
	"fri": "Fri", "sat": "Sat", "sun": "Sun"}

func (f *form) DayLabel(k string) string { return dayLabels[k] }

func (s *Server) newForm() *form {
	cfg := s.Config()
	return &form{DefaultLate: cfg.LateAfter, Notify: len(cfg.Notifiers) > 0, DayKeys: config.DayKeys,
		Days: map[string]bool{}}
}

func formFromTrain(f *form, t config.Train) {
	f.OldKey = t.Key()
	f.Number = strconv.Itoa(t.Number)
	f.Origin, f.Label, f.NotifyAt, f.Board = t.Origin, t.Label, t.NotifyAt, t.Board
	if t.LateAfter > 0 {
		f.LateAfter = strconv.Itoa(t.LateAfter)
	}
	for _, k := range config.DayKeys {
		f.Days[k] = t.Days.String() == "" || strings.Contains(t.Days.String(), k)
	}
}

func formFromRequest(f *form, v url.Values) {
	f.OldKey = v.Get("old_key")
	f.Number = strings.TrimSpace(v.Get("number"))
	f.Origin = strings.TrimSpace(v.Get("origin"))
	f.Label = strings.TrimSpace(v.Get("label"))
	f.NotifyAt = strings.TrimSpace(v.Get("notify_at"))
	f.LateAfter = strings.TrimSpace(v.Get("late_after"))
	f.Board = v.Get("board")
	for _, d := range v["days"] {
		f.Days[d] = true
	}
}

// train turns the form into a train, or explains what is wrong with it.
func (f *form) train() (config.Train, error) {
	var t config.Train
	n, err := strconv.Atoi(f.Number)
	if err != nil || n <= 0 {
		return t, errors.New("enter the train number, e.g. 2034")
	}
	t.Number, t.Origin, t.Label, t.NotifyAt, t.Board = n, f.Origin, f.Label, f.NotifyAt, f.Board
	if f.LateAfter != "" {
		if t.LateAfter, err = strconv.Atoi(f.LateAfter); err != nil || t.LateAfter < 1 {
			return t, errors.New("minutes late must be a whole number, at least 1")
		}
	}
	var days []string
	for _, k := range config.DayKeys {
		if f.Days[k] {
			days = append(days, k)
		}
	}
	if len(days) == 0 {
		return t, errors.New("choose at least one day to notify on")
	}
	if t.Days, err = config.ParseDays(days); err != nil {
		return t, err
	}
	return t, t.Validate()
}

// edit shows the form: empty to add a train, filled in from ?key= to edit
// one, or from the query when the "Look up" button resubmits it.
func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	f := s.newForm()
	q := r.URL.Query()
	switch {
	case q.Has("number"):
		formFromRequest(f, q)
	case q.Get("key") != "":
		train, err := s.findTrain(r.Context(), q.Get("key"))
		if err != nil {
			s.fail(w, err)
			return
		}
		if train == nil {
			http.Error(w, "no such train", http.StatusNotFound)
			return
		}
		formFromTrain(f, *train)
	default:
		for _, k := range config.DayKeys {
			f.Days[k] = true
		}
	}
	if q.Has("lookup") {
		s.lookup(r.Context(), f)
	}
	if err := s.loadStops(r.Context(), f); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "edit.html", f)
}

// loadStops lists the stops the train departs from, from its most recent
// recorded run, for choosing where to board. A new train has none yet.
func (s *Server) loadStops(ctx context.Context, f *form) error {
	if f.OldKey == "" {
		return nil
	}
	dates, err := s.Store.RunDates(ctx, f.OldKey, 1)
	if err != nil || len(dates) == 0 {
		return err
	}
	rd, err := s.Store.RunReading(ctx, f.OldKey, dates[0])
	if err != nil || rd == nil {
		return err
	}
	var st vt.Status
	json.Unmarshal([]byte(rd.Raw), &st)
	if len(st.Stops) > 1 {
		f.Stops = stopsOf(st)[:len(st.Stops)-1] // no departure from the last stop
	}
	return nil
}

func (s *Server) findTrain(ctx context.Context, key string) (*config.Train, error) {
	trains, err := s.Store.Trains(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range trains {
		if t.Key() == key {
			return &t, nil
		}
	}
	return nil, nil
}

// lookup lists today's trains with the form's number, so the right one can
// be picked when several share it.
func (s *Server) lookup(ctx context.Context, f *form) {
	f.Looked = true
	n, err := strconv.Atoi(f.Number)
	if err != nil || n <= 0 {
		f.LookupError = "Enter a train number to look up."
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	runs, err := s.Source.FindRuns(ctx, n)
	if errors.Is(err, vt.ErrNotFound) {
		f.LookupError = fmt.Sprintf("ViaggiaTreno doesn't know train %d today.", n)
		return
	}
	if err != nil {
		f.LookupError = "Couldn't reach ViaggiaTreno: " + err.Error()
		return
	}
	today := s.Now().In(vt.Rome).Format(store.DateLayout)
	for _, run := range runs {
		if run.Date.Format(store.DateLayout) != today || len(f.Runs) == 8 {
			continue
		}
		c := runChoice{Origin: run.Origin, Description: "from " + tracker.Title(run.OriginName)}
		if st, _, err := s.Source.Status(ctx, run); err == nil {
			c.Description = fmt.Sprintf("%s · %s %s → %s %s", st.Name,
				tracker.Title(string(st.Origin)), hhmm(st.ScheduledDeparture()),
				tracker.Title(string(st.Destination)), hhmm(st.ScheduledArrival()))
		}
		f.Runs = append(f.Runs, c)
	}
	if len(f.Runs) == 0 {
		f.LookupError = fmt.Sprintf("Train %d doesn't run today, so it can't be looked up now; it can still be added.", n)
	}
}

func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	f := s.newForm()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	formFromRequest(f, r.PostForm)
	train, err := f.train()
	if err == nil {
		err = s.Store.SaveTrain(r.Context(), f.OldKey, train)
	}
	if err != nil {
		f.Error = err.Error()
		if err := s.loadStops(r.Context(), f); err != nil {
			s.fail(w, err)
			return
		}
		s.render(w, http.StatusBadRequest, "edit.html", f)
		return
	}
	s.Log.Info("train saved", "train", train.Key(), "was", f.OldKey)
	s.changed()
	backToList(w)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	if err := s.Store.DeleteTrain(r.Context(), key); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("train deleted", "train", key)
	s.changed()
	backToList(w)
}

// backToList redirects to the status page with a relative URL, which also
// resolves correctly under the dashboard's path prefix.
func backToList(w http.ResponseWriter) {
	w.Header().Set("Location", "./")
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) changed() {
	if s.Changed != nil {
		s.Changed()
	}
}
