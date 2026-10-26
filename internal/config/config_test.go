package config

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Setenv("TG_TOKEN", "secret")
	cfg, err := Parse([]byte(`
late_after: 3
notifiers:
  - type: telegram
    token: ${TG_TOKEN}
    chat_id: "42"
trains:
  - number: 2034
    days: mon-fri
    notify_at: "07:20"
  - number: 9544
    origin: S01700
    days: [sat, sun]
    late_after: 10
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifiers[0].Token != "secret" {
		t.Errorf("token = %q, want it read from the environment", cfg.Notifiers[0].Token)
	}
	if time.Duration(cfg.PollInterval) != 5*time.Minute || cfg.Listen != ":8080" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	a, b := cfg.Trains[0], cfg.Trains[1]
	if a.Key() != "2034" || b.Key() != "9544@S01700" {
		t.Errorf("keys %q, %q", a.Key(), b.Key())
	}
	if a.LateAfter != 3 || b.LateAfter != 10 {
		t.Errorf("late_after %d, %d; want 3, 10", a.LateAfter, b.LateAfter)
	}
	if !a.Days.Has(time.Friday) || a.Days.Has(time.Saturday) || !b.Days.Has(time.Sunday) || b.Days.Has(time.Monday) {
		t.Errorf("days %v, %v", a.Days, b.Days)
	}
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if at, ok := a.NotifyTime(day); !ok || at.Format("15:04") != "07:20" {
		t.Errorf("notify time %v, %v", at, ok)
	}
	if _, ok := b.NotifyTime(day); ok {
		t.Error("train without notify_at has a notify time")
	}
}

func TestDays(t *testing.T) {
	cfg, err := Parse([]byte("trains:\n  - {number: 1, days: fri-mon}\n  - {number: 2, days: weekdays}\n  - {number: 3}"))
	if err != nil {
		t.Fatal(err)
	}
	wrap, weekdays, every := cfg.Trains[0].Days, cfg.Trains[1].Days, cfg.Trains[2].Days
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		wantWrap := wd == time.Friday || wd == time.Saturday || wd == time.Sunday || wd == time.Monday
		wantWeek := wd != time.Saturday && wd != time.Sunday
		if wrap.Has(wd) != wantWrap || weekdays.Has(wd) != wantWeek || !every.Has(wd) {
			t.Errorf("%s: fri-mon %v, weekdays %v, unset %v", wd, wrap.Has(wd), weekdays.Has(wd), every.Has(wd))
		}
	}
}

func TestParseErrors(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown field":   "trainz: []",
		"bad notify time": "trains: [{number: 1, notify_at: '7.20'}]",
		"duplicate train": "trains: [{number: 1}, {number: 1}]",
		"bad day":         "trains: [{number: 1, days: [funday]}]",
		"bad notifier":    "notifiers: [{type: pigeon}]",
		"missing token":   "notifiers: [{type: telegram, chat_id: '1'}]",
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
