package viaggiatreno

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Status is the subset of an andamentoTreno response this project uses.
type Status struct {
	Number          Int    `json:"numeroTreno"`
	Category        String `json:"categoria"`
	Name            String `json:"compNumeroTreno"` // e.g. "REG 2034"
	Origin          String `json:"origine"`
	Destination     String `json:"destinazione"`
	DepartureMs     Int    `json:"orarioPartenza"`
	ArrivalMs       Int    `json:"orarioArrivo"`
	Delay           Int    `json:"ritardo"` // minutes; negative when early
	LastSeenStation String `json:"stazioneUltimoRilevamento"`
	LastSeenMs      Int    `json:"oraUltimoRilevamento"`
	NotDeparted     Bool   `json:"nonPartito"`
	Arrived         Bool   `json:"arrivato"`
	// TrainType is PG for a regular run, ST when the whole run is
	// cancelled, PP/SI/SF when part of it is, and DV when it is diverted.
	TrainType     String `json:"tipoTreno"`
	Provvedimento Int    `json:"provvedimento"`
	Stops         []Stop `json:"fermate"`
}

type Stop struct {
	Station     String `json:"stazione"`
	ID          String `json:"id"`
	ScheduledMs Int    `json:"programmata"`
	ActualMs    Int    `json:"effettiva"`
	Delay       Int    `json:"ritardo"`
}

type State string

const (
	NotDeparted State = "not_departed"
	Running     State = "running"
	Arrived     State = "arrived"
	Cancelled   State = "cancelled"
)

func (s *Status) State() State {
	switch {
	case strings.EqualFold(string(s.TrainType), "ST") || s.Provvedimento == 1:
		return Cancelled
	case bool(s.Arrived):
		return Arrived
	case len(s.Stops) > 0 && s.Stops[len(s.Stops)-1].ActualMs > 0:
		return Arrived
	case bool(s.NotDeparted):
		return NotDeparted
	}
	return Running
}

func (s *Status) PartiallyCancelled() bool {
	switch strings.ToUpper(string(s.TrainType)) {
	case "PP", "SI", "SF":
		return true
	}
	return s.Provvedimento == 2
}

func (s *Status) ScheduledDeparture() time.Time {
	if s.DepartureMs > 0 {
		return s.DepartureMs.Time()
	}
	if len(s.Stops) > 0 {
		return s.Stops[0].ScheduledMs.Time()
	}
	return time.Time{}
}

func (s *Status) ScheduledArrival() time.Time {
	if s.ArrivalMs > 0 {
		return s.ArrivalMs.Time()
	}
	if len(s.Stops) > 0 {
		return s.Stops[len(s.Stops)-1].ScheduledMs.Time()
	}
	return time.Time{}
}

// LastSeen returns where and when the train was last detected; the station
// is empty when it has not been detected yet.
func (s *Status) LastSeen() (string, time.Time) {
	station := strings.TrimSpace(string(s.LastSeenStation))
	if station == "--" {
		station = ""
	}
	return station, s.LastSeenMs.Time()
}

// Int decodes a JSON number, a numeric string or null.
type Int int64

func (n *Int) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
	if err != nil {
		*n = 0
		return nil
	}
	*n = Int(v)
	return nil
}

// Time reads n as Unix milliseconds; zero stays the zero time.
func (n Int) Time() time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(n)).In(Rome)
}

// String decodes a JSON string, number, bool or null.
type String string

func (s *String) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	switch v := v.(type) {
	case string:
		*s = String(v)
	case nil:
		*s = ""
	default:
		*s = String(strings.Trim(string(b), `"`))
	}
	return nil
}

// Bool decodes a JSON bool, "true"/"false" or null.
type Bool bool

func (v *Bool) UnmarshalJSON(b []byte) error {
	*v = Bool(strings.EqualFold(strings.Trim(string(b), `"`), "true"))
	return nil
}
