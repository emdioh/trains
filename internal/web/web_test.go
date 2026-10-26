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

	s := &Server{Config: func() *config.Config { return cfg }, Store: st,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
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
		"Fri 23 Oct On time", "Wed 21 Oct Cancelled", "No data yet", "Not today",
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
