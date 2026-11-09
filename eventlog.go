package main

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// EventLog is the troubleshooting journal: what streamlink / ffmpeg reported for
// each channel (checks that changed state, recording start/finish with the tail
// of streamlink's own output, failures). Unlike notifications it is verbose,
// never muted, and identical consecutive messages of a channel are collapsed.

const maxLogEntries = 1000

type LogEntry struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Last      time.Time `json:"last"`  // time of the latest repeat
	Count     int       `json:"count"` // how many times it happened in a row
	ChannelID string    `json:"channel_id,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Platform  string    `json:"platform,omitempty"`
	Kind      string    `json:"kind"`  // check | record | job | system
	Level     string    `json:"level"` // info | warn | error
	Msg       string    `json:"msg"`
	Detail    string    `json:"detail,omitempty"`
}

type logFile struct {
	Next  int64      `json:"next"`
	Items []LogEntry `json:"items"` // oldest first
}

type EventLog struct {
	mu    sync.Mutex
	path  string
	d     logFile
	dirty bool
	stop  chan struct{}
}

func OpenEventLog(path string) *EventLog {
	l := &EventLog{path: path, stop: make(chan struct{})}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &l.d)
	}
	if l.d.Next < 1 {
		l.d.Next = 1
	}
	for _, it := range l.d.Items {
		if it.ID >= l.d.Next {
			l.d.Next = it.ID + 1
		}
	}
	go func() { // debounced persistence: restarts must not erase the evidence
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				l.Flush()
			case <-l.stop:
				return
			}
		}
	}()
	return l
}

func (l *EventLog) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.dirty {
		return
	}
	b, err := json.Marshal(l.d)
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, l.path) == nil {
		l.dirty = false
	}
}

func (l *EventLog) Close() { close(l.stop); l.Flush() }

var secretRe = regexp.MustCompile(`(?i)\b(token|access_token|oauth_token|sig|signature|key|apikey|auth|authorization|password|passwd|pass|secret|cookie)=([^&\s"']+)`)
var bearerRe = regexp.MustCompile(`(?i)\b(oauth|bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)

// redactSecrets hides credentials that tools sometimes echo (query strings, headers).
func redactSecrets(s string) string {
	s = secretRe.ReplaceAllString(s, "$1=***")
	return bearerRe.ReplaceAllString(s, "$1 ***")
}

// Add records an entry; the channel may be the zero value for system events.
func (l *EventLog) Add(ch Channel, kind, level, msg, detail string) {
	if l == nil {
		return
	}
	msg, detail = redactSecrets(msg), redactSecrets(detail)
	if len(detail) > 3000 {
		detail = detail[len(detail)-3000:]
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.d.Items) - 1; i >= 0 && i >= len(l.d.Items)-50; i-- { // collapse a repeat
		it := &l.d.Items[i]
		if it.ChannelID != ch.ID {
			continue
		}
		if it.Kind == kind && it.Level == level && it.Msg == msg {
			it.Count++
			it.Last = now
			if detail != "" {
				it.Detail = detail
			}
			l.dirty = true
			return
		}
		break // the channel's latest entry differs: this is a new event
	}
	plat := ch.Plugin
	if plat == "" && ch.URL != "" {
		plat = detectPlatform(ch.URL)
	}
	name := ""
	if ch.ID != "" {
		name = streamerName(ch)
	}
	l.d.Items = append(l.d.Items, LogEntry{ID: l.d.Next, Time: now, Last: now, Count: 1, ChannelID: ch.ID, Channel: name,
		Platform: plat, Kind: kind, Level: level, Msg: msg, Detail: detail})
	l.d.Next++
	if len(l.d.Items) > maxLogEntries {
		l.d.Items = l.d.Items[len(l.d.Items)-maxLogEntries:]
	}
	l.dirty = true
}

// List returns entries newest first, filtered by channel / minimum level.
func (l *EventLog) List(channelID, minLevel string, limit int) []LogEntry {
	rank := map[string]int{"info": 0, "warn": 1, "error": 2}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []LogEntry{}
	for i := len(l.d.Items) - 1; i >= 0 && len(out) < limit; i-- {
		it := l.d.Items[i]
		if channelID != "" && it.ChannelID != channelID {
			continue
		}
		if rank[it.Level] < rank[minLevel] {
			continue
		}
		out = append(out, it)
	}
	return out
}

func (l *EventLog) Clear() {
	l.mu.Lock()
	l.d.Items = nil
	l.dirty = true
	l.mu.Unlock()
	l.Flush()
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > maxLogEntries {
		limit = 300
	}
	level := r.URL.Query().Get("level")
	if level != "warn" && level != "error" {
		level = "info"
	}
	chans := []map[string]string{}
	for _, c := range a.m.st.Channels() {
		chans = append(chans, map[string]string{"id": c.ID, "name": streamerName(c)})
	}
	writeJSON(w, 200, map[string]any{"items": a.m.elog.List(r.URL.Query().Get("channel"), level, limit), "channels": chans})
}

func (a *API) clearEvents(w http.ResponseWriter, r *http.Request) {
	a.m.elog.Clear()
	w.WriteHeader(http.StatusNoContent)
}
