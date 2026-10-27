package web

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"trains/internal/store"
	"trains/internal/tracker"
	vt "trains/internal/viaggiatreno"
)

// stopsDates is how many recorded runs the date menu offers.
const stopsDates = 120

type stopTime struct {
	Scheduled string // HH:MM, empty when the train doesn't arrive/leave here
	Actual    string // HH:MM, empty until it happens
	Delay     *int
	Level     string // ok, late or bad, for the delay's colour
}

type stopRow struct {
	Name      string
	Platform  string
	Cancelled bool
	LastSeen  bool // the last station where the train was detected
	Arrival   stopTime
	Departure stopTime
}

type dateOption struct {
	Date  string
	Label string
}

type stopsPage struct {
	Key      string
	Title    string
	Dates    []dateOption
	Date     string
	Route    string
	Position string
	Fetched  time.Time
	Note     string
	Rows     []stopRow
}

// stops lists every stop of one run with its scheduled and actual times,
// from the run's latest reading. Without ?date= it shows the most recent run.
func (s *Server) stops(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	train, err := s.findTrain(ctx, r.URL.Query().Get("key"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if train == nil {
		http.Error(w, "no such train", http.StatusNotFound)
		return
	}
	t := s.Config().WithDefaults(*train)
	p := &stopsPage{Key: t.Key(), Title: t.Name()}
	dates, err := s.Store.RunDates(ctx, t.Key(), stopsDates)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, d := range dates {
		p.Dates = append(p.Dates, dateOption{Date: d, Label: day(d)})
	}
	p.Date = r.URL.Query().Get("date")
	if !slices.Contains(dates, p.Date) && len(dates) > 0 {
		p.Date = dates[0]
	}
	if p.Date != "" {
		rd, err := s.Store.RunReading(ctx, t.Key(), p.Date)
		if err != nil {
			s.fail(w, err)
			return
		}
		if rd != nil {
			fillStops(p, rd, t.Label, t.LateAfter)
		}
	}
	s.render(w, http.StatusOK, "stops.html", p)
}

func fillStops(p *stopsPage, rd *store.Reading, label string, lateAfter int) {
	var st vt.Status
	json.Unmarshal([]byte(rd.Raw), &st)
	if rd.Name != "" {
		p.Title = rd.Name
		if label != "" {
			p.Title = label + " · " + rd.Name
		}
	}
	from, to := rd.OriginName, rd.DestinationName
	if len(st.Stops) > 0 {
		from = cmp.Or(from, string(st.Stops[0].Station))
		to = cmp.Or(to, string(st.Stops[len(st.Stops)-1].Station))
	}
	p.Route = tracker.Title(from) + " → " + tracker.Title(to)
	p.Position = tracker.Position(rd)
	p.Fetched = rd.FetchedAt
	if rd.Partial {
		p.Note = "Partly cancelled: cancelled stops are marked."
	}
	lastSeen := strings.ToUpper(strings.TrimSpace(rd.LastSeenStation))
	for _, stop := range st.Stops {
		row := stopRow{
			Name:      tracker.Title(string(stop.Station)),
			Platform:  stop.Platform(),
			Cancelled: stop.Cancelled() || vt.State(rd.State) == vt.Cancelled,
			LastSeen:  lastSeen != "" && strings.ToUpper(string(stop.Station)) == lastSeen,
		}
		arr, arrOK := stop.ArrivalDelayMinutes()
		row.Arrival = timeAt(stop.ScheduledArrivalMs, stop.ActualArrivalMs, arr, arrOK, lateAfter)
		dep, depOK := stop.DepartureDelayMinutes()
		row.Departure = timeAt(stop.ScheduledDepartureMs, stop.ActualDepartureMs, dep, depOK, lateAfter)
		p.Rows = append(p.Rows, row)
	}
}

func timeAt(scheduled, actual vt.Int, delay int, ok bool, lateAfter int) stopTime {
	var t stopTime
	if scheduled > 0 {
		t.Scheduled = hhmm(scheduled.Time())
	}
	if actual > 0 {
		t.Actual = hhmm(actual.Time())
	}
	if ok {
		t.Delay = &delay
		t.Level = level(&store.Reading{Delay: delay}, lateAfter)
	}
	return t
}
