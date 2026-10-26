package web

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trains/internal/config"
	"trains/internal/store"
	vt "trains/internal/viaggiatreno"
)

func newServer(t *testing.T, notifiers string) *httptest.Server {
	t.Helper()
	return newEnv(t, notifiers).srv
}

type env struct {
	srv     *httptest.Server
	st      *store.Store
	changed int
}

// fakeSource knows two trains numbered 652 today.
type fakeSource struct{}

func (fakeSource) FindRuns(ctx context.Context, number int) ([]vt.Run, error) {
	if number != 652 {
		return nil, vt.ErrNotFound
	}
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, vt.Rome)
	return []vt.Run{
		{Number: 652, Origin: "S01700", OriginName: "MILANO CENTRALE", Date: day},
		{Number: 652, Origin: "S09218", OriginName: "NAPOLI CENTRALE", Date: day},
	}, nil
}

func (fakeSource) Status(ctx context.Context, run vt.Run) (*vt.Status, []byte, error) {
	dest := map[string]string{"S01700": "TORINO PORTA NUOVA", "S09218": "ROMA TERMINI"}[run.Origin]
	return &vt.Status{Name: "REG 652", Origin: vt.String(run.OriginName), Destination: vt.String(dest),
		DepartureMs: vt.Int(run.Date.Add(8 * time.Hour).UnixMilli())}, nil, nil
}

func newEnv(t *testing.T, notifiers string) *env {
	t.Helper()
	cfg, err := config.Parse([]byte(notifiers + `
trains:
  - {number: 2034, label: Morning commute, notify_at: "07:20"}
  - {number: 2077, label: Evening, days: weekdays}
  - {number: 9544, days: weekends}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "trains.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.ImportTrains(context.Background(), cfg.Trains); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 26, 7, 40, 0, 0, vt.Rome)
	ctx := context.Background()
	add := func(key, date, state string, delay int, partial bool) {
		day, _ := time.ParseInLocation(store.DateLayout, date, vt.Rome)
		err := st.AddReading(ctx, &store.Reading{TrainKey: key, Number: 2034, Origin: "S01700", RunDate: date,
			FetchedAt: now.Add(-3 * time.Minute), State: state, Partial: partial, Delay: delay,
			LastSeenStation: "TREVIGLIO", LastSeenAt: day.Add(7*time.Hour + 18*time.Minute),
			ScheduledDeparture: day.Add(6*time.Hour + 45*time.Minute), ScheduledArrival: day.Add(7*time.Hour + 50*time.Minute),
			Name: "REG 2034", OriginName: "MILANO CENTRALE", DestinationName: "BERGAMO"})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("2034", "2026-10-26", "running", 12, false)
	if err := st.AddFetchError(ctx, "2077", "2026-10-26", now, errors.New("several trains have this number today, from A (S1), B (S2): set origin to choose one")); err != nil {
		t.Fatal(err)
	}
	add("2034", "2026-10-23", "arrived", 0, false)
	add("2034", "2026-10-22", "arrived", 7, false)
	add("2034", "2026-10-21", "cancelled", 0, false)
	add("2034", "2026-10-20", "arrived", 3, true)

	e := &env{st: st}
	s := &Server{Config: func() *config.Config { return cfg }, Store: st, Source: fakeSource{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
		Changed: func() { e.changed++ }, Version: "v9.9.9"}
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestPage(t *testing.T) {
	srv := newServer(t, "notifiers: [{type: ntfy, url: 'https://ntfy.sh/x'}]")
	body := []byte(get(t, srv.URL))
	page := html.UnescapeString(string(body))
	if out := os.Getenv("WRITE_PAGE"); out != "" {
		os.WriteFile(out, body, 0o644)
	}
	for _, want := range []string{
		"Morning commute · REG 2034", "+12 min", "Last seen Treviglio at 07:18",
		"Milano Centrale 06:45 → Bergamo 07:50", "Updated 3 min ago", "Notifies at 07:20 if 5+ min late",
		"Fri 23 Oct On time", "Wed 21 Oct Cancelled", "No data yet", "Not today", "trains v9.9.9",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestPageWithoutNotifiers(t *testing.T) {
	srv := newServer(t, "")
	page := html.UnescapeString(get(t, srv.URL))
	if !strings.Contains(page, "Would notify at 07:20, but no notifier is configured") {
		t.Error("page doesn't say notifications are off")
	}
}

func TestPageShowsProblem(t *testing.T) {
	srv := newServer(t, "")
	page := html.UnescapeString(get(t, srv.URL))
	if !strings.Contains(page, "several trains have this number") {
		t.Error("page doesn't show why train 2077 isn't tracked")
	}
}

func TestAPI(t *testing.T) {
	srv := newServer(t, "")
	resp, err := http.Get(srv.URL + "/api/trains")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Trains []Train }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Trains) != 3 || out.Trains[0].Today == nil || out.Trains[0].Today.Delay != 12 ||
		len(out.Trains[0].History) != 4 {
		t.Errorf("api = %+v", out)
	}
}

// post submits a form without following the redirect.
func post(t *testing.T, url string, form url.Values) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *env) keys(t *testing.T) string {
	trains, err := e.st.Trains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, tr := range trains {
		keys = append(keys, tr.Key()+":"+tr.Days.String()+":"+tr.NotifyAt)
	}
	return strings.Join(keys, " ")
}

func TestAddEditDelete(t *testing.T) {
	e := newEnv(t, "")
	resp := post(t, e.srv.URL+"/save", url.Values{"number": {"652"}, "origin": {"S09218"}, "label": {"Napoli"},
		"days": {"mon", "tue"}, "notify_at": {"08:00"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "./" {
		t.Fatalf("save: %s, Location %q", resp.Status, resp.Header.Get("Location"))
	}
	if got := e.keys(t); got != "2034::07:20 2077:mon,tue,wed,thu,fri: 9544:sat,sun: 652@S09218:mon,tue:08:00" {
		t.Errorf("after add: %s", got)
	}

	form := html.UnescapeString(get(t, e.srv.URL+"/edit?key=652@S09218"))
	for _, want := range []string{`value="Napoli"`, `value="08:00"`, `name="old_key" value="652@S09218"`} {
		if !strings.Contains(form, want) {
			t.Errorf("edit form is missing %s", want)
		}
	}

	post(t, e.srv.URL+"/save", url.Values{"old_key": {"652@S09218"}, "number": {"652"}, "origin": {"S01700"},
		"days": {"mon", "tue", "wed", "thu", "fri", "sat", "sun"}})
	if got := e.keys(t); !strings.HasSuffix(got, " 652@S01700::") {
		t.Errorf("after edit: %s", got)
	}

	post(t, e.srv.URL+"/delete", url.Values{"key": {"2077"}})
	if got := e.keys(t); strings.Contains(got, "2077") {
		t.Errorf("after delete: %s", got)
	}
	if e.changed != 3 {
		t.Errorf("tracker woken %d times, want 3", e.changed)
	}
}

func TestSaveErrors(t *testing.T) {
	e := newEnv(t, "")
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no number":    {url.Values{"days": {"mon"}}, "enter the train number"},
		"no days":      {url.Values{"number": {"1"}}, "choose at least one day"},
		"bad late":     {url.Values{"number": {"1"}, "days": {"mon"}, "late_after": {"0"}}, "at least 1"},
		"duplicate":    {url.Values{"number": {"2034"}, "days": {"mon"}}, "already in the list"},
		"bad notifyat": {url.Values{"number": {"1"}, "days": {"mon"}, "notify_at": {"7"}}, "is not HH:MM"},
	} {
		resp := post(t, e.srv.URL+"/save", tc.form)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: %s, want an error containing %q", name, resp.Status, tc.want)
		}
	}
	if e.changed != 0 {
		t.Error("tracker woken after failed saves")
	}
}

func TestLookup(t *testing.T) {
	e := newEnv(t, "")
	page := html.UnescapeString(get(t, e.srv.URL+"/edit?number=652&label=Mine&days=mon&lookup=1"))
	for _, want := range []string{
		"Milano Centrale 08:00 → Torino Porta Nuova", "Napoli Centrale 08:00 → Roma Termini",
		`value="S09218"`, `value="Mine"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("lookup page is missing %q", want)
		}
	}
	if page := get(t, e.srv.URL+"/edit?number=1&lookup=1"); !strings.Contains(page, "know train 1 today") {
		t.Error("unknown number isn't reported")
	}
}

func TestCrossSiteFormIsRefused(t *testing.T) {
	e := newEnv(t, "")
	req, _ := http.NewRequest("POST", e.srv.URL+"/delete", strings.NewReader("key=2034"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(e.keys(t), "2034") {
		t.Errorf("cross-site delete: %s", resp.Status)
	}
}

func TestRefreshAndAutoReload(t *testing.T) {
	e := newEnv(t, "")
	page := get(t, e.srv.URL)
	// 2034 departed 06:45 and is running at 07:40.
	if !strings.Contains(page, `http-equiv="refresh" content="300"`) || !strings.Contains(page, "Updated 07:37") {
		t.Error("running train: page doesn't reload every 5 minutes or lacks the update time")
	}
	resp := post(t, e.srv.URL+"/refresh", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("refresh: %s", resp.Status)
	}
	if err := e.st.AddReading(context.Background(), &store.Reading{TrainKey: "2034", RunDate: "2026-10-26",
		FetchedAt: time.Date(2026, 10, 26, 7, 39, 0, 0, vt.Rome), State: "arrived"}); err != nil {
		t.Fatal(err)
	}
	page = get(t, e.srv.URL)
	if strings.Contains(page, `http-equiv="refresh"`) || !strings.Contains(page, "no train running now") {
		t.Error("nothing running: the page still reloads itself")
	}
}

func TestDeleteFromList(t *testing.T) {
	e := newEnv(t, "")
	page := html.UnescapeString(get(t, e.srv.URL))
	if !strings.Contains(page, `action="delete"`) || !strings.Contains(page, `name="key" value="9544"`) {
		t.Fatal("the list has no delete button per train")
	}
	post(t, e.srv.URL+"/delete", url.Values{"key": {"9544"}})
	if strings.Contains(e.keys(t), "9544") {
		t.Error("train still listed after delete")
	}
}
