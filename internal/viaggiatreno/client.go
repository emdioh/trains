// Package viaggiatreno is a client for the unofficial JSON API behind
// viaggiatreno.it, which publishes RFI's real-time circulation data for
// every train on the Italian network.
//
// The API is undocumented and can change without notice, so decoding is
// deliberately lenient: fields that are missing or of an unexpected type
// decode to their zero value instead of failing the whole response.
package viaggiatreno

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://www.viaggiatreno.it/infomobilita/resteasy/viaggiatreno"

// ErrNotFound means ViaggiaTreno has no data for the train or run asked for.
var ErrNotFound = errors.New("train not found")

// Rome is the time zone every ViaggiaTreno date and time refers to.
var Rome = mustLoad("Europe/Rome")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

type Client struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
}

func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		HTTP:      &http.Client{Timeout: 20 * time.Second},
		UserAgent: "trains-tracker/1.0",
	}
}

// Run identifies one day's run of a train: the number alone is not unique,
// ViaggiaTreno also needs the origin station and the departure date.
type Run struct {
	Number     int
	Origin     string // station code, e.g. S01700
	OriginName string
	Date       time.Time // midnight, Rome time, of the departure day
}

// FindRuns lists the runs ViaggiaTreno knows for a train number. Usually
// there is one; several mean the number is used from different origins.
func (c *Client) FindRuns(ctx context.Context, number int) ([]Run, error) {
	body, err := c.get(ctx, "/cercaNumeroTrenoTrenoAutocomplete/"+strconv.Itoa(number))
	if err != nil {
		return nil, err
	}
	runs := ParseRuns(string(body), time.Now())
	if len(runs) == 0 {
		return nil, ErrNotFound
	}
	return runs, nil
}

// ParseRuns parses the autocomplete response. Each line looks like
//
//	2034 - MILANO CENTRALE - 26/10/26|2034-S01700-1761429600000
//
// Older responses leave out the date and the timestamp; those runs are
// taken to depart on the day of now.
func ParseRuns(body string, now time.Time) []Run {
	var runs []Run
	for _, line := range strings.Split(body, "\n") {
		display, key, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		parts := strings.Split(key, "-")
		if len(parts) < 2 {
			continue
		}
		number, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		run := Run{Number: number, Origin: parts[1]}
		if names := strings.Split(display, " - "); len(names) >= 2 {
			run.OriginName = strings.TrimSpace(names[1])
		}
		if len(parts) >= 3 {
			if ms, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				run.Date = midnight(time.UnixMilli(ms))
			}
		}
		if run.Date.IsZero() {
			run.Date = midnight(now)
		}
		runs = append(runs, run)
	}
	return runs
}

func midnight(t time.Time) time.Time {
	t = t.In(Rome)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, Rome)
}

// Status fetches the live status of a run. It also returns the raw body,
// which callers keep so that nothing the API said is lost.
func (c *Client) Status(ctx context.Context, run Run) (*Status, []byte, error) {
	path := fmt.Sprintf("/andamentoTreno/%s/%d/%d", run.Origin, run.Number, run.Date.UnixMilli())
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	var s Status
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, body, fmt.Errorf("decoding status: %w", err)
	}
	return &s, body, nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("viaggiatreno: %s: HTTP %d", path, resp.StatusCode)
	case len(strings.TrimSpace(string(body))) == 0:
		return nil, ErrNotFound
	}
	return body, nil
}
