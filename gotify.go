package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Gotify delivery: every recorded notification (independent of the in-app mute
// list) whose type is ticked in the settings is POSTed to a Gotify server.

const gotifyQueue = 64

type Gotify struct {
	settings func() Settings
	client   *http.Client
	q        chan Notification
	retryGap time.Duration

	mu      sync.Mutex
	lastOK  time.Time
	lastErr string
	sent    uint64
	failed  uint64
	lastAt  time.Time
}

func NewGotify(settings func() Settings) *Gotify {
	g := &Gotify{settings: settings, client: &http.Client{Timeout: 10 * time.Second},
		q: make(chan Notification, gotifyQueue), retryGap: 2 * time.Second}
	go g.worker()
	return g
}

// gotifyTypeOn: nil (never configured) means every type.
func gotifyTypeOn(s Settings, typ string) bool {
	if s.GotifyTypes == nil {
		return true
	}
	for _, t := range s.GotifyTypes {
		if t == typ {
			return true
		}
	}
	return false
}

// Enqueue is the Notifier sink; it never blocks.
func (g *Gotify) Enqueue(n Notification) {
	s := g.settings()
	if !s.GotifyOn || s.GotifyURL == "" || s.GotifyToken == "" || !gotifyTypeOn(s, n.Type) {
		return
	}
	select {
	case g.q <- n:
	default: // queue full: drop rather than stall the manager
	}
}

func (g *Gotify) worker() {
	for n := range g.q {
		s := g.settings()
		if !s.GotifyOn || s.GotifyURL == "" || s.GotifyToken == "" {
			continue
		}
		title, msg := g.render(s.GotifyLang, n)
		err := g.post(s, title, msg, gotifyPriority(n.Level))
		if err != nil && !errors.Is(err, errGotifyPermanent) {
			time.Sleep(g.retryGap)
			err = g.post(s, title, msg, gotifyPriority(n.Level))
		}
		g.record(err)
	}
}

func (g *Gotify) record(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.lastOK, g.lastErr = time.Now(), ""
		g.sent++
		return
	}
	g.failed++
	g.lastErr, g.lastAt = err.Error(), time.Now()
}

func gotifyPriority(level string) int {
	switch level {
	case "warn":
		return 6
	case "error":
		return 8
	}
	return 4
}

var errGotifyPermanent = errors.New("gotify rejected the request")

// cleanGotifyURL validates and normalises the server address.
func cleanGotifyURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", errors.New("Gotify URL must look like http(s)://host[:port][/path]")
	}
	return raw, nil
}

func (g *Gotify) post(s Settings, title, msg string, prio int) error {
	base, err := cleanGotifyURL(s.GotifyURL)
	if err != nil {
		return fmt.Errorf("%w: %v", errGotifyPermanent, err)
	}
	body, _ := json.Marshal(map[string]any{"title": title, "message": msg, "priority": prio})
	req, err := http.NewRequest(http.MethodPost, base+"/message", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errGotifyPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", s.GotifyToken)
	resp, err := g.client.Do(req)
	if err != nil {
		return redactToken(err.Error(), s.GotifyToken)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	e := fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%w: %v", errGotifyPermanent, e)
	}
	return e
}

func redactToken(msg, token string) error {
	return errors.New(strings.ReplaceAll(msg, token, "***"))
}

// ------------------------------------------------------------ text

func (g *Gotify) render(lang string, n Notification) (title, msg string) {
	return locs.Notification(lang, n)
}

// Test sends a message synchronously with the given (possibly unsaved) settings.
func (g *Gotify) Test(s Settings) error {
	err := g.post(s, "Streamlink Studio", locs.Tr(s.GotifyLang, "gotify_test_msg"), 4)
	g.record(err)
	return err
}

func (g *Gotify) Status() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]any{"last_error": g.lastErr, "sent": g.sent, "failed": g.failed}
	if !g.lastOK.IsZero() {
		out["last_ok"] = g.lastOK
	}
	if g.lastErr != "" {
		out["last_error_at"] = g.lastAt
	}
	return out
}

// ------------------------------------------------------------ HTTP

func (a *API) gotifyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.m.gotify.Status())
}

// gotifyTest sends a test message using the posted values, falling back to the
// saved ones (so the stored token never has to travel to the browser).
func (a *API) gotifyTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL   *string `json:"url"`
		Token *string `json:"token"`
		Lang  *string `json:"lang"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s := a.m.st.Settings()
	if in.URL != nil {
		s.GotifyURL = strings.TrimSpace(*in.URL)
	}
	if in.Token != nil && strings.TrimSpace(*in.Token) != "" {
		s.GotifyToken = strings.TrimSpace(*in.Token)
	}
	if in.Lang != nil {
		s.GotifyLang = *in.Lang
	}
	if _, err := cleanGotifyURL(s.GotifyURL); err != nil {
		writeErr(w, errBad(err.Error()))
		return
	}
	if s.GotifyToken == "" {
		writeErr(w, errBad("Gotify application token is empty"))
		return
	}
	if err := a.m.gotify.Test(s); err != nil {
		writeErr(w, &httpError{502, err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
