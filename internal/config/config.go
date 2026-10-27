// Package config loads the YAML configuration. The file is re-read while
// the service runs, so the train list and the notifiers can change without
// a restart.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Listen is the address of the status page. Changing it needs a restart.
	Listen string `yaml:"listen"`
	// Database is the SQLite file. Changing it needs a restart.
	Database     string           `yaml:"database"`
	PollInterval Duration         `yaml:"poll_interval"`
	LateAfter    int              `yaml:"late_after"` // minutes; default for trains that don't set it
	BaseURL      string           `yaml:"viaggiatreno_url"`
	Notifiers    []NotifierConfig `yaml:"notifiers"`
	Trains       []Train          `yaml:"trains"`

	// Warnings lists what was ignored, like a notifier without its token.
	Warnings []string `yaml:"-"`
}

// NotifierConfig holds the settings of one notification channel. Which
// fields apply depends on Type.
type NotifierConfig struct {
	Type   string `yaml:"type"` // telegram or ntfy
	Token  string `yaml:"token"`
	ChatID string `yaml:"chat_id"`
	URL    string `yaml:"url"`
}

type Train struct {
	Number int `yaml:"number"`
	// Origin picks the run when several trains share the number: a station
	// code (S01700) or part of the station's name (Milano Centrale).
	Origin string `yaml:"origin"`
	Label  string `yaml:"label"`
	Days   Days   `yaml:"days"`
	// NotifyAt is the time of day ("07:20") at which a notification is sent
	// if the train is late or cancelled. Empty means never.
	NotifyAt  string `yaml:"notify_at"`
	LateAfter int    `yaml:"late_after"`
}

// Key identifies a configured train in the database.
func (t Train) Key() string {
	if t.Origin != "" {
		return fmt.Sprintf("%d@%s", t.Number, t.Origin)
	}
	return strconv.Itoa(t.Number)
}

func (t Train) Name() string {
	if t.Label != "" {
		return t.Label
	}
	return strconv.Itoa(t.Number)
}

// Validate checks a train's own fields.
func (t Train) Validate() error {
	if t.Number <= 0 {
		return errors.New("the train number is required")
	}
	if t.NotifyAt != "" {
		if _, err := time.Parse("15:04", t.NotifyAt); err != nil {
			return fmt.Errorf("notification time %q is not HH:MM", t.NotifyAt)
		}
	}
	if t.LateAfter < 0 {
		return errors.New("late_after must be at least 1")
	}
	return nil
}

// WithDefaults fills in what a train leaves to the global settings.
func (c *Config) WithDefaults(t Train) Train {
	if t.LateAfter == 0 {
		t.LateAfter = c.LateAfter
	}
	return t
}

// NotifyTime returns the notification time on the given day, and false
// when the train has none.
func (t Train) NotifyTime(day time.Time) (time.Time, bool) {
	if t.NotifyAt == "" {
		return time.Time{}, false
	}
	hm, _ := time.Parse("15:04", t.NotifyAt)
	return time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, day.Location()), true
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a configuration. ${VAR} references are replaced with
// environment variables, so secrets can stay out of the file.
func Parse(data []byte) (*Config, error) {
	cfg := Config{
		Listen:       ":8080",
		Database:     "trains.db",
		PollInterval: Duration(5 * time.Minute),
		LateAfter:    5,
	}
	expanded := os.Expand(string(data), func(name string) string { return os.Getenv(name) })
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.PollInterval < Duration(time.Minute) {
		return errors.New("poll_interval must be at least 1m")
	}
	if c.LateAfter < 1 {
		return errors.New("late_after must be at least 1")
	}
	seen := map[string]bool{}
	for i, t := range c.Trains {
		if err := t.Validate(); err != nil {
			return fmt.Errorf("trains[%d]: %w", i, err)
		}
		if seen[t.Key()] {
			return fmt.Errorf("train %s is listed twice", t.Key())
		}
		seen[t.Key()] = true
	}
	// A notifier missing its settings (e.g. ${TELEGRAM_TOKEN} not set yet) is
	// left out rather than stopping the service: tracking and the status
	// page work without notifications.
	var notifiers []NotifierConfig
	for i, n := range c.Notifiers {
		var missing string
		switch n.Type {
		case "telegram":
			if n.Token == "" || n.ChatID == "" {
				missing = "token and chat_id"
			}
		case "ntfy":
			if n.URL == "" {
				missing = "url"
			}
		default:
			return fmt.Errorf("notifiers[%d]: unknown type %q", i, n.Type)
		}
		if missing != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf("notifiers[%d]: %s needs %s; it is disabled", i, n.Type, missing))
			continue
		}
		notifiers = append(notifiers, n)
	}
	c.Notifiers = notifiers
	return nil
}

// Duration decodes strings like "5m" or "90s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Days is the set of weekdays a train notifies on. It decodes a list of
// names ("mon", "tue", ...), ranges ("mon-fri"), "weekdays", "weekends" or
// "daily". An empty set means every day.
type Days map[time.Weekday]bool

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func (d *Days) UnmarshalYAML(n *yaml.Node) error {
	var items []string
	if n.Kind == yaml.SequenceNode {
		if err := n.Decode(&items); err != nil {
			return err
		}
	} else {
		items = strings.Split(n.Value, ",")
	}
	days, err := ParseDays(items)
	if err != nil {
		return err
	}
	*d = days
	return nil
}

// ParseDays reads day names and ranges, as in the YAML file.
func ParseDays(items []string) (Days, error) {
	days := Days{}
	for _, item := range items {
		item = strings.ToLower(strings.TrimSpace(item))
		switch item {
		case "daily", "":
			continue
		case "weekdays":
			item = "mon-fri"
		case "weekends":
			item = "sat-sun"
		}
		from, to, isRange := strings.Cut(item, "-")
		start, ok1 := dayNames[from]
		end, ok2 := dayNames[to]
		if !isRange {
			end, ok2 = start, ok1
		}
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("unknown day %q", item)
		}
		for wd := start; ; wd = (wd + 1) % 7 {
			days[wd] = true
			if wd == end {
				break
			}
		}
	}
	if len(days) == 7 {
		days = Days{}
	}
	return days, nil
}

// DayKeys are the day names in week order, Monday first.
var DayKeys = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// Describe is the days for people: "every day", "Mon–Fri", "Mon, Wed".
func (d Days) Describe() string {
	if d.String() == "" {
		return "every day"
	}
	var parts []string
	for i := 0; i < len(DayKeys); {
		if !d[dayNames[DayKeys[i]]] {
			i++
			continue
		}
		j := i
		for j+1 < len(DayKeys) && d[dayNames[DayKeys[j+1]]] {
			j++
		}
		name := func(k string) string { return strings.ToUpper(k[:1]) + k[1:] }
		switch {
		case j-i >= 2:
			parts = append(parts, name(DayKeys[i])+"–"+name(DayKeys[j]))
		case j > i:
			parts = append(parts, name(DayKeys[i]), name(DayKeys[j]))
		default:
			parts = append(parts, name(DayKeys[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// String lists the days, Monday first: "mon,tue,wed". Every day is "".
func (d Days) String() string {
	var names []string
	for _, k := range DayKeys {
		if d[dayNames[k]] {
			names = append(names, k)
		}
	}
	if len(names) == 7 {
		return ""
	}
	return strings.Join(names, ",")
}

func (d Days) Has(wd time.Weekday) bool {
	return len(d) == 0 || d[wd]
}
