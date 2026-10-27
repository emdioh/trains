package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"trains/internal/store"
	"trains/internal/tracker"
	vt "trains/internal/viaggiatreno"
)

// historyRanges are the time ranges offered, in days.
var historyRanges = []int{30, 90, 365}

const defaultRange = 90

// historyRun is one run on the history charts. A nil delay means there is
// no value to plot (cancelled, or the stop wasn't reached or recorded).
type historyRun struct {
	Date      string `json:"date"`
	Label     string `json:"label"`
	Short     string `json:"short"`
	Final     *int   `json:"final"`
	Stop      *int   `json:"stop"`
	Cancelled bool   `json:"cancelled"`
	Note      string `json:"note,omitempty"`
}

type historyStop struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// summary condenses one chart's values for the figures above it.
type summary struct {
	Runs      int `json:"runs"`      // runs with a value
	OnTimePct int `json:"on_time"`   // share of those below late_after
	Median    int `json:"median"`    // minutes
	Worst     int `json:"worst"`     // minutes
	Cancelled int `json:"cancelled"` // runs cancelled
}

type historyData struct {
	Runs      []historyRun `json:"runs"`
	LateAfter int          `json:"late_after"`
	YMin      int          `json:"y_min"`
	YMax      int          `json:"y_max"`
	Ticks     []int        `json:"ticks"`
}

type historyPage struct {
	Key      string
	Title    string
	Days     int
	Ranges   []int
	Stops    []historyStop
	Stop     string // selected stop's code, if any
	StopName string
	Terminus string
	Final    summary
	AtStop   summary
	Data     historyData
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	p, ok := s.historyPage(w, r)
	if ok {
		s.render(w, http.StatusOK, "history.html", p)
	}
}

func (s *Server) historyAPI(w http.ResponseWriter, r *http.Request) {
	p, ok := s.historyPage(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"train": p.Key, "days": p.Days, "stop": p.Stop,
		"stops": p.Stops, "final": p.Final, "at_stop": p.AtStop, "runs": p.Data.Runs})
}

// historyPage gathers the last reading of every run in the range and works
// out the delay at the end of the line and at the chosen stop. It writes
// the error response itself and reports false on failure.
func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) (*historyPage, bool) {
	q := r.URL.Query()
	train, err := s.findTrain(r.Context(), q.Get("key"))
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	if train == nil {
		http.Error(w, "no such train", http.StatusNotFound)
		return nil, false
	}
	t := s.Config().WithDefaults(*train)
	p := &historyPage{Key: t.Key(), Title: t.Name(), Days: defaultRange, Ranges: historyRanges, Stop: q.Get("stop")}
	if d, err := strconv.Atoi(q.Get("days")); err == nil && slices.Contains(historyRanges, d) {
		p.Days = d
	}
	now := s.Now().In(vt.Rome)
	today, since := now.Format(store.DateLayout), now.AddDate(0, 0, -p.Days).Format(store.DateLayout)
	readings, err := s.Store.FinalReadings(r.Context(), t.Key(), since)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}

	var finals, atStop []int
	for _, rd := range readings {
		var st vt.Status
		json.Unmarshal([]byte(rd.Raw), &st) // a reading without stops still gives its state
		run := historyRun{Date: rd.RunDate, Label: day(rd.RunDate), Short: shortDay(rd.RunDate)}
		if rd.Name != "" && t.Label != "" {
			p.Title = t.Label + " · " + rd.Name
		} else if rd.Name != "" {
			p.Title = rd.Name
		}
		if len(st.Stops) > 0 {
			p.Stops = stopsOf(st)
			p.Terminus = tracker.Title(string(st.Stops[len(st.Stops)-1].Station))
		}
		switch {
		case vt.State(rd.State) == vt.Cancelled:
			run.Cancelled, run.Note = true, "Cancelled"
			p.Final.Cancelled++
			p.AtStop.Cancelled++
		default:
			if rd.Partial {
				run.Note = "Partly cancelled"
			}
			if d, ok := finalDelay(rd, st); ok {
				run.Final = &d
				finals = append(finals, d)
			} else if run.Note == "" && rd.RunDate == today && !tracker.Finished(rd) {
				run.Note = "In progress"
			} else if run.Note == "" {
				run.Note = "Arrival not recorded"
			}
			if stop, ok := findStop(st, p.Stop); ok {
				if d, ok := stop.DelayMinutes(); ok {
					run.Stop = &d
					atStop = append(atStop, d)
				}
			}
		}
		p.Data.Runs = append(p.Data.Runs, run)
	}
	for _, st := range p.Stops {
		if st.ID == p.Stop {
			p.StopName = st.Name
		}
	}
	if p.StopName == "" {
		p.Stop = ""
	}
	p.Final = summarize(finals, t.LateAfter, p.Final.Cancelled)
	p.AtStop = summarize(atStop, t.LateAfter, p.AtStop.Cancelled)
	p.Data.LateAfter = t.LateAfter
	p.Data.YMin, p.Data.YMax, p.Data.Ticks = scale(append(finals, atStop...), t.LateAfter)
	return p, true
}

// finalDelay is the delay arriving at the last stop. A run whose last
// reading says it arrived but has no stop times falls back to the delay
// it reported then.
func finalDelay(rd *store.Reading, st vt.Status) (int, bool) {
	if len(st.Stops) > 0 {
		if d, ok := st.Stops[len(st.Stops)-1].ArrivalDelayMinutes(); ok {
			return d, true
		}
	}
	if vt.State(rd.State) == vt.Arrived {
		return rd.Delay, true
	}
	return 0, false
}

func findStop(st vt.Status, id string) (vt.Stop, bool) {
	if id == "" {
		return vt.Stop{}, false
	}
	for _, stop := range st.Stops {
		if string(stop.ID) == id {
			return stop, true
		}
	}
	return vt.Stop{}, false
}

func stopsOf(st vt.Status) []historyStop {
	var out []historyStop
	for _, stop := range st.Stops {
		out = append(out, historyStop{ID: string(stop.ID), Name: tracker.Title(string(stop.Station))})
	}
	return out
}

func summarize(values []int, lateAfter, cancelled int) summary {
	sm := summary{Runs: len(values), Cancelled: cancelled}
	if len(values) == 0 {
		return sm
	}
	sorted := slices.Sorted(slices.Values(values))
	onTime := 0
	for _, v := range sorted {
		if v < lateAfter {
			onTime++
		}
	}
	sm.OnTimePct = int(math.Round(100 * float64(onTime) / float64(len(sorted))))
	mid := len(sorted) / 2
	sm.Median = sorted[mid]
	if len(sorted)%2 == 0 {
		sm.Median = int(math.Round(float64(sorted[mid-1]+sorted[mid]) / 2))
	}
	sm.Worst = sorted[len(sorted)-1]
	return sm
}

// scale picks a y range and round tick values covering the delays and the
// late threshold, always including zero.
func scale(values []int, lateAfter int) (lo, hi int, ticks []int) {
	lo, hi = 0, lateAfter+1
	for _, v := range values {
		lo, hi = min(lo, v), max(hi, v)
	}
	step := 1
	for _, s := range []int{1, 2, 5, 10, 15, 30, 60, 120} {
		step = s
		if (hi-lo)/s <= 5 {
			break
		}
	}
	lo = -int(math.Ceil(float64(-lo)/float64(step))) * step
	hi = int(math.Ceil(float64(hi)/float64(step))) * step
	for v := lo; v <= hi; v += step {
		ticks = append(ticks, v)
	}
	return lo, hi, ticks
}

func shortDay(date string) string {
	t, err := time.ParseInLocation(store.DateLayout, date, vt.Rome)
	if err != nil {
		return date
	}
	return t.Format("2 Jan")
}

// minutes formats a delay for the summary figures.
func minutes(m int) string {
	switch {
	case m > 0:
		return fmt.Sprintf("+%d min", m)
	case m < 0:
		return fmt.Sprintf("%d min", m)
	}
	return "0 min"
}
