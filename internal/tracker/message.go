package tracker

import (
	"fmt"
	"strings"

	"trains/internal/config"
	"trains/internal/notify"
	"trains/internal/store"
	vt "trains/internal/viaggiatreno"
)

// Describe writes the notification for a reading, and reports whether it
// is worth sending: the train is cancelled, partly cancelled or late.
func Describe(train config.Train, r *store.Reading) (notify.Message, bool) {
	title := r.Name
	if train.Label != "" {
		title = train.Label + " · " + r.Name
	}
	var lines []string
	if r.OriginName != "" || r.DestinationName != "" {
		lines = append(lines, fmt.Sprintf("%s → %s", Title(r.OriginName), Title(r.DestinationName)))
	}

	late := r.Delay >= train.LateAfter
	switch {
	case vt.State(r.State) == vt.Cancelled:
		return notify.Message{Title: "❌ " + title + ": cancelled", Body: strings.Join(lines, "\n")}, true
	case r.Partial:
		title = "⚠️ " + title + ": partly cancelled"
		if r.Delay > 0 {
			title += fmt.Sprintf(", +%d min", r.Delay)
		}
	case late:
		title = fmt.Sprintf("🕒 %s: +%d min", title, r.Delay)
	default:
		title = fmt.Sprintf("✅ %s: on time", title)
	}
	lines = append(lines, Position(r))
	return notify.Message{Title: title, Body: strings.Join(lines, "\n")}, late || r.Partial
}

// Position describes where the train is, e.g. "Last seen Treviglio at 07:58".
func Position(r *store.Reading) string {
	switch vt.State(r.State) {
	case vt.NotDeparted:
		if !r.ScheduledDeparture.IsZero() {
			return "Not departed yet, scheduled " + r.ScheduledDeparture.In(vt.Rome).Format("15:04")
		}
		return "Not departed yet"
	case vt.Cancelled:
		return "Cancelled"
	}
	s := "No position yet"
	if r.LastSeenStation != "" {
		s = "Last seen " + Title(r.LastSeenStation)
		if !r.LastSeenAt.IsZero() {
			s += " at " + r.LastSeenAt.In(vt.Rome).Format("15:04")
		}
	}
	if vt.State(r.State) == vt.Arrived {
		s = "Arrived · " + s
	}
	return s
}

func unknown(train config.Train, err error) notify.Message {
	body := "No status from ViaggiaTreno around the notification time."
	if err != nil {
		body += "\n" + err.Error()
	}
	return notify.Message{Title: "❓ " + train.Name() + ": status unknown", Body: body}
}

// Title turns ViaggiaTreno's upper-case station names into "Milano Centrale".
func Title(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		parts := strings.Split(w, ".")
		for j, p := range parts {
			if p != "" {
				r := []rune(p)
				parts[j] = strings.ToUpper(string(r[0])) + string(r[1:])
			}
		}
		words[i] = strings.Join(parts, ".")
	}
	return strings.Join(words, " ")
}
