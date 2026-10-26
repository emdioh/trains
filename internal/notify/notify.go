// Package notify sends messages to the configured channels. Each channel
// implements Notifier; adding one means adding a type and a case in New.
package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"trains/internal/config"
)

type Message struct {
	Title string
	Body  string
}

type Notifier interface {
	Notify(ctx context.Context, m Message) error
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// New builds the notifiers described by the configuration.
func New(cfgs []config.NotifierConfig) []Notifier {
	var out []Notifier
	for _, c := range cfgs {
		switch c.Type {
		case "telegram":
			out = append(out, &Telegram{Token: c.Token, ChatID: c.ChatID})
		case "ntfy":
			out = append(out, &Ntfy{URL: c.URL, Token: c.Token})
		}
	}
	return out
}

// All sends m to every notifier and returns the errors joined.
func All(ctx context.Context, ns []Notifier, m Message) error {
	var errs []error
	for _, n := range ns {
		if err := n.Notify(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Telegram sends through a bot made with @BotFather.
type Telegram struct {
	Token  string
	ChatID string
}

func (t *Telegram) Notify(ctx context.Context, m Message) error {
	form := url.Values{"chat_id": {t.ChatID}, "text": {m.Title + "\n" + m.Body}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+t.Token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(req, "telegram")
}

// Ntfy publishes to an ntfy topic URL, e.g. https://ntfy.sh/my-trains.
type Ntfy struct {
	URL   string
	Token string
}

func (n *Ntfy) Notify(ctx context.Context, m Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(m.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", m.Title)
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	return send(req, "ntfy")
}

func send(req *http.Request, name string) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		// The URL can carry a token (Telegram's does), so don't repeat it.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
