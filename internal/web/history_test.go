package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"trains/internal/store"
	vt "trains/internal/viaggiatreno"
)

// rawRun builds an andamentoTreno response for train 2034 on day with the
// given arrival delays (minutes) at Treviglio and Bergamo; nil means not
// reached.
func rawRun(day time.Time, treviglio, bergamo *int) string {
	ms := func(h, m int) int64 {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).UnixMilli()
	}
	actual := func(base int64, d *int) any {
		if d == nil {
			return nil
		}
		return base + int64(*d)*60000
	}
	stops := []map[string]any{
		{"stazione": "MILANO CENTRALE", "id": "S01700", "partenza_teorica": ms(6, 45), "partenzaReale": ms(6, 46),
			"binarioEffettivoPartenzaDescrizione": "14"},
		{"stazione": "TREVIGLIO", "id": "S01708", "arrivo_teorico": ms(7, 5), "arrivoReale": actual(ms(7, 5), treviglio)},
		{"stazione": "BERGAMO", "id": "S01529", "arrivo_teorico": ms(7, 50), "arrivoReale": actual(ms(7, 50), bergamo)},
	}
	b, _ := json.Marshal(map[string]any{"compNumeroTreno": "REG 2034", "fermate": stops})
	return string(b)
}

func addRun(t *testing.T, st *store.Store, date, state string, treviglio, bergamo *int) {
	t.Helper()
	day, _ := time.ParseInLocation(store.DateLayout, date, vt.Rome)
	delay := 0
	if bergamo != nil {
		delay = *bergamo
	}
	err := st.AddReading(context.Background(), &store.Reading{TrainKey: "2034", Number: 2034, Origin: "S01700",
		RunDate: date, FetchedAt: day.Add(8 * time.Hour), State: state, Delay: delay, Name: "REG 2034",
		Raw: rawRun(day, treviglio, bergamo)})
	if err != nil {
		t.Fatal(err)
	}
}

func ptr(v int) *int { return &v }

func historyEnv(t *testing.T) *env {
	e := newEnv(t, "")
	addRun(t, e.st, "2026-10-19", "arrived", ptr(2), ptr(4))
	addRun(t, e.st, "2026-10-20", "arrived", ptr(9), ptr(12))
	addRun(t, e.st, "2026-10-21", "cancelled", nil, nil)
	addRun(t, e.st, "2026-10-22", "arrived", ptr(-1), ptr(0))
	addRun(t, e.st, "2026-10-23", "running", ptr(3), nil)      // polling stopped before the end
	addRun(t, e.st, "2026-06-01", "arrived", ptr(30), ptr(30)) // outside 90 days
	if os.Getenv("WRITE_HISTORY") != "" {
		for i := 0; i < 60; i++ {
			day := time.Date(2026, 8, 20, 0, 0, 0, 0, vt.Rome).AddDate(0, 0, i)
			if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
				continue
			}
			a, b := (i*7)%11-2, (i*13)%23-3
			if i%17 == 5 {
				addRun(t, e.st, day.Format(store.DateLayout), "cancelled", nil, nil)
				continue
			}
			addRun(t, e.st, day.Format(store.DateLayout), "arrived", ptr(a), ptr(b))
		}
	}
	return e
}

func TestHistoryAPI(t *testing.T) {
	e := historyEnv(t)
	resp, err := http.Get(e.srv.URL + "/api/history?key=2034&stop=S01708")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Final  summary
		AtStop summary `json:"at_stop"`
		Runs   []historyRun
		Stops  []historyStop
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range out.Runs {
		v := func(p *int) string {
			if p == nil {
				return "-"
			}
			return fmt.Sprint(*p)
		}
		got = append(got, r.Date[5:]+":"+v(r.Final)+"/"+v(r.Stop))
	}
	if want := "10-19:4/2 10-20:12/9 10-21:-/- 10-22:0/-1 10-23:-/3 10-26:-/-"; strings.Join(got, " ") != want {
		t.Errorf("runs = %s, want %s", strings.Join(got, " "), want)
	}
	// Final: 4, 12, 0 → median 4, worst 12, 2 of 3 under 5 min.
	if f := out.Final; f.Runs != 3 || f.Median != 4 || f.Worst != 12 || f.OnTimePct != 67 || f.Cancelled != 1 {
		t.Errorf("final summary = %+v", f)
	}
	if s := out.AtStop; s.Runs != 4 || s.Worst != 9 {
		t.Errorf("stop summary = %+v", s)
	}
	if len(out.Stops) != 3 || out.Stops[1].Name != "Treviglio" {
		t.Errorf("stops = %+v", out.Stops)
	}
}

func TestHistoryPage(t *testing.T) {
	e := historyEnv(t)
	body := get(t, e.srv.URL+"/history?key=2034&days=365&stop=S01708")
	if out := os.Getenv("WRITE_HISTORY"); out != "" {
		os.WriteFile(out, []byte(body), 0o644)
	}
	page := html.UnescapeString(body)
	for _, want := range []string{
		"Delay at the end of the line · Bergamo", "Delay at Treviglio", `<option value="S01708" selected>`,
		`<option value="365" selected>`, "Arrival not recorded", "In progress", "Cancelled",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("history page is missing %q", want)
		}
	}
	if !strings.Contains(get(t, e.srv.URL), `href="history?key=2034"`) {
		t.Error("status page has no link to the history")
	}
	if resp, _ := http.Get(e.srv.URL + "/history?key=nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown train: %s", resp.Status)
	}
}

func TestStopsPage(t *testing.T) {
	e := historyEnv(t)
	// The newest run (today's, from newEnv) is the default; it has no raw data.
	page := html.UnescapeString(get(t, e.srv.URL+"/stops?key=2034"))
	if !strings.Contains(page, `<option value="2026-10-26" selected>`) || !strings.Contains(page, "No stop times were recorded") {
		t.Error("stops page doesn't default to the most recent run")
	}

	body := get(t, e.srv.URL+"/stops?key=2034&date=2026-10-20")
	if out := os.Getenv("WRITE_STOPS"); out != "" {
		os.WriteFile(out, []byte(body), 0o644)
	}
	page = html.UnescapeString(body)
	for _, want := range []string{
		`<option value="2026-10-20" selected>`, "Milano Centrale", "Treviglio", "Bergamo",
		"07:14", "+9", "sched. 07:05",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("stops page is missing %q", want)
		}
	}
	if !strings.Contains(page, "08:02") || !strings.Contains(page, "+12") {
		t.Error("Bergamo arrival 08:02 (+12) missing")
	}
	if !strings.Contains(get(t, e.srv.URL), `href="stops?key=2034"`) {
		t.Error("status page has no link to the stops")
	}
	if resp, _ := http.Get(e.srv.URL + "/stops?key=nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown train: %s", resp.Status)
	}
}

func TestBoardingPlatform(t *testing.T) {
	raw := `{"fermate":[
		{"id":"S01700","stazione":"MILANO CENTRALE","binarioProgrammatoPartenzaDescrizione":"14","binarioEffettivoPartenzaDescrizione":"14"},
		{"id":"S01708","stazione":"TREVIGLIO","binarioProgrammatoPartenzaDescrizione":"3","binarioEffettivoPartenzaDescrizione":"5"},
		{"id":"S01530","stazione":"VERDELLO","binarioProgrammatoPartenzaDescrizione":"2"},
		{"id":"S01529","stazione":"BERGAMO"}]}`
	for board, want := range map[string]string{
		"":       "14||",
		"S01708": "5|changed from 3|Treviglio",
		"S01530": "2|planned|Verdello",
		"S01529": "||",   // the terminus has no departure platform
		"S99999": "14||", // an unknown stop falls back to the start
	} {
		p, note, at := boardingPlatform(raw, board)
		if got := p + "|" + note + "|" + at; got != want {
			t.Errorf("board %q: %s, want %s", board, got, want)
		}
	}
}

func TestPlatformOnStatusPageAndBoardChoice(t *testing.T) {
	e := historyEnv(t)
	ctx := context.Background()
	// Today's run, with platforms.
	err := e.st.AddReading(ctx, &store.Reading{TrainKey: "2034", Number: 2034, RunDate: "2026-10-26",
		FetchedAt: time.Date(2026, 10, 26, 7, 39, 0, 0, vt.Rome), State: "running", Name: "REG 2034",
		Raw: `{"fermate":[{"id":"S01700","stazione":"MILANO CENTRALE","binarioProgrammatoPartenzaDescrizione":"14"},
			{"id":"S01708","stazione":"TREVIGLIO","binarioProgrammatoPartenzaDescrizione":"3","binarioEffettivoPartenzaDescrizione":"5"},
			{"id":"S01529","stazione":"BERGAMO"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	page := html.UnescapeString(get(t, e.srv.URL))
	if !strings.Contains(page, "Platform <strong>14</strong> <span class=\"muted\">· planned</span>") {
		t.Error("status page doesn't show the starting station's planned platform")
	}

	form := html.UnescapeString(get(t, e.srv.URL+"/edit?key=2034"))
	if !strings.Contains(form, "Milano Centrale (starting station)") || !strings.Contains(form, `value="S01708"`) ||
		strings.Contains(form, `value="S01529"`) {
		t.Error("board choice should list the departure stops, not the terminus")
	}
	post(t, e.srv.URL+"/save", url.Values{"old_key": {"2034"}, "number": {"2034"}, "days": {"mon"},
		"notify_at": {"07:20"}, "board": {"S01708"}})
	page = html.UnescapeString(get(t, e.srv.URL))
	if !strings.Contains(page, "Platform <strong>5</strong> at Treviglio <span class=\"muted\">· changed from 3</span>") {
		t.Error("status page doesn't show the chosen stop's changed platform")
	}
}
