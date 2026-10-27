package tracker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"trains/internal/config"
	"trains/internal/store"
	vt "trains/internal/viaggiatreno"
)

// Monday 26 October 2026, Rome time.
func at(hhmm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-26 "+hhmm, vt.Rome)
	return t
}

type fakeSource struct {
	status func() (*vt.Status, error)
	runs   []vt.Run // what FindRuns returns; default one run from S01700
	calls  int
	asked  []string // origins of the runs whose status was asked
}

func (f *fakeSource) FindRuns(ctx context.Context, number int) ([]vt.Run, error) {
	if f.runs != nil {
		return f.runs, nil
	}
	return []vt.Run{{Number: number, Origin: "S01700", OriginName: "MILANO CENTRALE", Date: at("00:00")}}, nil
}

func (f *fakeSource) Status(ctx context.Context, run vt.Run) (*vt.Status, []byte, error) {
	f.calls++
	f.asked = append(f.asked, run.Origin)
	s, err := f.status()
	return s, []byte("{}"), err
}

func running(delay int) func() (*vt.Status, error) {
	return func() (*vt.Status, error) {
		return &vt.Status{Name: "REG 2034", Origin: "MILANO CENTRALE", Destination: "BERGAMO",
			DepartureMs: vt.Int(at("07:35").UnixMilli()), ArrivalMs: vt.Int(at("08:40").UnixMilli()),
			Delay: vt.Int(delay), LastSeenStation: "TREVIGLIO", LastSeenMs: vt.Int(at("07:15").UnixMilli())}, nil
	}
}

// inbox is an ntfy server that collects the messages it receives.
type inbox struct {
	mu   sync.Mutex
	msgs []string
}

func (b *inbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.msgs = append(b.msgs, r.Header.Get("Title")+"\n"+string(body))
	b.mu.Unlock()
}

type harness struct {
	t       *testing.T
	src     *fakeSource
	inbox   *inbox
	store   *store.Store
	cfg     *config.Config
	now     time.Time
	tracker *Tracker
}

func newHarness(t *testing.T, trainYAML string, status func() (*vt.Status, error)) *harness {
	h := &harness{t: t, src: &fakeSource{status: status}, inbox: &inbox{}}
	srv := httptest.NewServer(h.inbox)
	t.Cleanup(srv.Close)
	cfg, err := config.Parse([]byte("notifiers: [{type: ntfy, url: '" + srv.URL + "'}]\ntrains:\n" + trainYAML))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.store, err = store.Open(filepath.Join(t.TempDir(), "trains.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.store.Close() })
	if _, err := h.store.ImportTrains(context.Background(), cfg.Trains); err != nil {
		t.Fatal(err)
	}
	h.restart()
	return h
}

// restart replaces the tracker with a fresh one on the same database, as
// after a service restart.
func (h *harness) restart() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.tracker = New("", h.cfg, h.src, h.store, log)
	h.tracker.Now = func() time.Time { return h.now }
}

// run ticks once a minute from one time to another, inclusive.
func (h *harness) run(from, to string) {
	for h.now = at(from); !h.now.After(at(to)); h.now = h.now.Add(time.Minute) {
		h.tracker.Tick(context.Background())
	}
}

func (h *harness) messages() []string {
	h.inbox.mu.Lock()
	defer h.inbox.mu.Unlock()
	return append([]string(nil), h.inbox.msgs...)
}

const commuter = `  - {number: 2034, label: Commute, notify_at: "07:20"}`

func TestNotifiesOnceWhenLate(t *testing.T) {
	h := newHarness(t, commuter, running(12))
	h.run("07:00", "09:00")
	msgs := h.messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %q", len(msgs), msgs)
	}
	want := "🕒 Commute · REG 2034: +12 min\nMilano Centrale → Bergamo\nLast seen Treviglio at 07:15"
	if msgs[0] != want {
		t.Errorf("message\n%s\nwant\n%s", msgs[0], want)
	}
}

func TestQuietWhenOnTime(t *testing.T) {
	h := newHarness(t, commuter, running(2))
	h.run("07:00", "09:00")
	if msgs := h.messages(); len(msgs) != 0 {
		t.Errorf("got messages %q, want none", msgs)
	}
	if checked, _ := h.store.Checked(context.Background(), "2034", "2026-10-26"); !checked {
		t.Error("the on-time check was not recorded")
	}
}

func TestCancelledAlwaysNotifies(t *testing.T) {
	h := newHarness(t, commuter, func() (*vt.Status, error) {
		return &vt.Status{Name: "REG 2034", TrainType: "ST"}, nil
	})
	h.run("07:00", "08:00")
	if msgs := h.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "cancelled") {
		t.Errorf("got %q, want one cancellation", msgs)
	}
}

func TestUnknownWhenSourceFails(t *testing.T) {
	h := newHarness(t, commuter, func() (*vt.Status, error) { return nil, errors.New("HTTP 503") })
	h.run("07:00", "09:00")
	msgs := h.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "status unknown") || !strings.Contains(msgs[0], "HTTP 503") {
		t.Errorf("got %q, want one 'status unknown' message", msgs)
	}
}

func TestNoLateNotificationAfterRestart(t *testing.T) {
	h := newHarness(t, commuter, running(12))
	h.run("07:00", "07:30")
	h.restart()
	h.run("07:31", "08:00")
	if msgs := h.messages(); len(msgs) != 1 {
		t.Errorf("got %d messages across a restart, want 1", len(msgs))
	}

	// Started after the window: the check was missed and is not made up.
	h2 := newHarness(t, commuter, running(12))
	h2.run("09:00", "09:30")
	if msgs := h2.messages(); len(msgs) != 0 {
		t.Errorf("started late: got %q, want nothing", msgs)
	}
}

func TestPollsOnlyAroundTheJourney(t *testing.T) {
	arrived := false
	h := newHarness(t, `  - {number: 2034}`, func() (*vt.Status, error) {
		s, _ := running(0)()
		s.Arrived = vt.Bool(arrived)
		return s, nil
	})
	h.run("00:00", "07:04")
	if h.src.calls != 1 {
		t.Errorf("before the journey: %d calls, want just the first one", h.src.calls)
	}
	h.run("07:05", "08:39") // from 30 min before departure, every 5 min
	if h.src.calls != 1+19 {
		t.Errorf("during the journey: %d calls, want 20", h.src.calls)
	}
	arrived = true
	h.run("08:40", "12:00")
	if h.src.calls != 21 {
		t.Errorf("after arrival: %d calls, want 21", h.src.calls)
	}
}

func TestTracksEveryDayNotifiesOnItsDays(t *testing.T) {
	h := newHarness(t, `  - {number: 2034, days: weekends, notify_at: "07:20"}`, running(30))
	h.run("06:00", "09:00") // a Monday
	if h.src.calls == 0 {
		t.Error("not tracked on a day it doesn't notify")
	}
	if msgs := h.messages(); len(msgs) != 0 {
		t.Errorf("notified on a Monday: %q", msgs)
	}
}

func TestNotRunningToday(t *testing.T) {
	h := newHarness(t, `  - {number: 2034}`, running(0))
	h.src.runs = []vt.Run{} // ViaggiaTreno lists no run today
	h.run("06:00", "09:00")
	msg, _, _ := h.store.LastFetchError(context.Background(), "2034", "2026-10-26")
	if !strings.HasPrefix(msg, "not running today") {
		t.Errorf("fetch error = %q, want it to start with 'not running today'", msg)
	}
}

var twin652 = []vt.Run{
	{Number: 652, Origin: "S01700", OriginName: "MILANO CENTRALE", Date: at("00:00")},
	{Number: 652, Origin: "S09218", OriginName: "NAPOLI CENTRALE", Date: at("00:00")},
	{Number: 652, Origin: "S08409", OriginName: "ROMA TERMINI", Date: at("00:00").AddDate(0, 0, -1)},
}

func TestPickRun(t *testing.T) {
	for _, tc := range []struct {
		origin, want, err string
	}{
		{"S09218", "S09218", ""},
		{"s09218", "S09218", ""},
		{"Napoli Centrale", "S09218", ""},
		{"napoli", "S09218", ""},
		{"", "", "several trains have this number today, from Milano Centrale (S01700), Napoli Centrale (S09218)"},
		{"Centrale", "", "matches several runs"},
		{"Roma", "", `no run from "Roma" today`}, // Roma's run is yesterday's
	} {
		run, err := PickRun(twin652, tc.origin, at("09:00"))
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("origin %q: err = %v, want %q", tc.origin, err, tc.err)
		case tc.err == "" && (err != nil || run.Origin != tc.want):
			t.Errorf("origin %q: got %v, %v; want %s", tc.origin, run, err, tc.want)
		}
	}
	if _, err := PickRun(twin652[2:], "", at("09:00")); !errors.Is(err, vt.ErrNotFound) {
		t.Errorf("no run today: err = %v, want ErrNotFound", err)
	}
}

func TestAmbiguousNumberIsNotGuessed(t *testing.T) {
	h := newHarness(t, `  - {number: 652, notify_at: "07:20"}`, running(12))
	h.src.runs = twin652
	h.run("07:00", "08:00")
	if h.src.calls != 0 {
		t.Errorf("status fetched %d times for an ambiguous number", h.src.calls)
	}
	msg, _, _ := h.store.LastFetchError(context.Background(), "652", "2026-10-26")
	if !strings.Contains(msg, "set origin") {
		t.Errorf("fetch error = %q, want it to ask for origin", msg)
	}
	// The notification time passes without a status: say so rather than stay silent.
	if msgs := h.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "set origin") {
		t.Errorf("messages = %q, want one explaining the ambiguity", msgs)
	}
}

func TestOriginByName(t *testing.T) {
	h := newHarness(t, `  - {number: 652, origin: Napoli Centrale}`, running(0))
	h.src.runs = twin652
	h.run("07:00", "07:10")
	if len(h.src.asked) == 0 || h.src.asked[0] != "S09218" {
		t.Errorf("asked status of %v, want the Napoli run", h.src.asked)
	}
}

func TestForcedRefresh(t *testing.T) {
	h := newHarness(t, `  - {number: 2034}`, running(0))
	h.run("06:00", "06:00") // first reading: departs 07:35, so idle until 07:05
	h.now = at("06:30")
	h.tracker.tick(context.Background(), true)
	if h.src.calls != 2 {
		t.Errorf("forced refresh outside the window: %d calls, want 2", h.src.calls)
	}
	h.tracker.tick(context.Background(), true) // same instant: skipped
	if h.src.calls != 2 {
		t.Errorf("second forced refresh within a minute: %d calls, want 2", h.src.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.now = at("06:40")
	go h.tracker.Run(ctx)
	if n, err := h.tracker.Refresh(ctx); err != nil || n != 1 {
		t.Fatalf("Refresh = %d, %v; want 1 train fetched", n, err)
	}
	if h.src.calls < 3 {
		t.Errorf("Refresh: %d calls, want a new one", h.src.calls)
	}
}

func TestRefreshFetchesArrivedTrains(t *testing.T) {
	h := newHarness(t, `  - {number: 2034}`, func() (*vt.Status, error) {
		s, _ := running(0)()
		s.Arrived = true
		return s, nil
	})
	h.run("09:00", "09:30") // arrived on the first reading: no more polling
	if h.src.calls != 1 {
		t.Fatalf("after arrival: %d calls, want 1", h.src.calls)
	}
	h.now = at("21:00")
	if n := h.tracker.tick(context.Background(), true); n != 1 || h.src.calls != 2 {
		t.Errorf("refresh after arrival fetched %d (calls %d), want the train fetched again", n, h.src.calls)
	}
}
