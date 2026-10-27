package viaggiatreno

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestParseRuns(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, Rome)
	body := "2034 - MILANO CENTRALE - 26/10/26|2034-S01700-1792969200000\n" +
		"2034 - TORINO PORTA NUOVA|2034-S00219\n" +
		"garbage line\n"
	runs := ParseRuns(body, now)
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if r := runs[0]; r.Number != 2034 || r.Origin != "S01700" || r.OriginName != "MILANO CENTRALE" ||
		!r.Date.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, Rome)) {
		t.Errorf("first run = %+v", r)
	}
	if r := runs[1]; r.Origin != "S00219" || !r.Date.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, Rome)) {
		t.Errorf("run without timestamp = %+v, want today's date", r)
	}
}

// The fixture is hand-written from the field names the API is known to
// use; it is not a recorded response.
func TestStatus(t *testing.T) {
	data, err := os.ReadFile("testdata/andamentoTreno.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != "REG 2034" || s.Delay != 12 || s.State() != Running || s.PartiallyCancelled() {
		t.Errorf("status = %+v, state %s", s, s.State())
	}
	station, at := s.LastSeen()
	if station != "TREVIGLIO" || at.Format("15:04") != "07:18" {
		t.Errorf("last seen %q at %s", station, at.Format("15:04"))
	}
	if got := s.ScheduledDeparture().Format("15:04"); got != "06:45" {
		t.Errorf("departure %s, want 06:45", got)
	}
}

func TestStatusLenient(t *testing.T) {
	var s Status
	body := `{"numeroTreno":"2034","ritardo":null,"stazioneUltimoRilevamento":"--",
		"oraUltimoRilevamento":null,"nonPartito":true,"tipoTreno":"ST","fermate":null,"extra":{"x":1}}`
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.Number != 2034 || s.Delay != 0 || s.State() != Cancelled {
		t.Errorf("status = %+v, state %s", s, s.State())
	}
	if station, at := s.LastSeen(); station != "" || !at.IsZero() {
		t.Errorf("last seen %q at %v, want nothing", station, at)
	}
}

func TestClient(t *testing.T) {
	fixture, _ := os.ReadFile("testdata/andamentoTreno.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cercaNumeroTrenoTrenoAutocomplete/2034":
			w.Write([]byte("2034 - MILANO CENTRALE - 26/10/26|2034-S01700-1792969200000\n"))
		case "/andamentoTreno/S01700/2034/1792969200000":
			w.Write(fixture)
		case "/cercaNumeroTrenoTrenoAutocomplete/9999":
			w.WriteHeader(http.StatusOK) // the API answers unknown trains with an empty body
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()
	runs, err := c.FindRuns(ctx, 2034)
	if err != nil || len(runs) != 1 {
		t.Fatalf("FindRuns = %v, %v", runs, err)
	}
	s, raw, err := c.Status(ctx, runs[0])
	if err != nil || s.Delay != 12 || len(raw) == 0 {
		t.Fatalf("Status = %+v, %d bytes, %v", s, len(raw), err)
	}
	if _, err := c.FindRuns(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown train: err = %v, want ErrNotFound", err)
	}
}

func TestStopDelays(t *testing.T) {
	min := func(m int) Int { return Int(1792969200000 + int64(m)*60000) }
	for name, tc := range map[string]struct {
		stop Stop
		want int
		ok   bool
	}{
		"arrived late":    {Stop{ScheduledArrivalMs: min(100), ActualArrivalMs: min(107)}, 7, true},
		"arrived early":   {Stop{ScheduledArrivalMs: min(100), ActualArrivalMs: min(98)}, -2, true},
		"origin departed": {Stop{ScheduledDepartureMs: min(10), ActualDepartureMs: min(13)}, 3, true},
		"not reached":     {Stop{ScheduledArrivalMs: min(100)}, 0, false},
		"reported only":   {Stop{ActualMs: min(5), Delay: 4}, 4, true},
	} {
		got, ok := tc.stop.DelayMinutes()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %d, %v; want %d, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}
