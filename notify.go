package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Notification types. The UI builds the (translated) text from the type and params.
var notifyTypes = []string{"stream_live", "stream_ended", "rec_failed", "auth_error", "job_started", "job_done", "job_failed", "update_available"}

const (
	maxNotifications = 300
	dedupeWindow     = time.Hour
)

type Notification struct {
	ID       int64             `json:"id"`
	Time     time.Time         `json:"time"`
	Type     string            `json:"type"`
	Level    string            `json:"level"` // info | success | warn | error
	Platform string            `json:"platform,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
}

type notifyFile struct {
	Next     int64          `json:"next"`
	LastRead int64          `json:"last_read"`
	Items    []Notification `json:"items"` // oldest first
}

// Notifier keeps the event log shown on the Notifications page.
type Notifier struct {
	mu    sync.Mutex
	path  string
	d     notifyFile
	muted func() []string
	seen  map[string]time.Time // dedupe key -> last time
}

func OpenNotifier(path string, muted func() []string) *Notifier {
	n := &Notifier{path: path, muted: muted, seen: map[string]time.Time{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &n.d)
	}
	if n.d.Next < 1 {
		n.d.Next = 1
	}
	for _, it := range n.d.Items {
		if it.ID >= n.d.Next {
			n.d.Next = it.ID + 1
		}
	}
	return n
}

func (n *Notifier) saveLocked() {
	b, err := json.Marshal(n.d)
	if err != nil {
		return
	}
	tmp := n.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, n.path)
	}
}

func levelFor(typ string) string {
	switch typ {
	case "job_done":
		return "success"
	case "rec_failed", "job_failed":
		return "error"
	case "auth_error":
		return "warn"
	}
	return "info"
}

// Notify records an event unless its type is muted or it repeats within an hour.
// dedupe may be empty (never deduplicated).
func (n *Notifier) Notify(typ, platform string, params map[string]string, dedupe string) {
	if n == nil {
		return
	}
	if n.muted != nil {
		for _, t := range n.muted() {
			if t == typ {
				return
			}
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if dedupe != "" {
		key := typ + "|" + dedupe
		if t, ok := n.seen[key]; ok && time.Since(t) < dedupeWindow {
			return
		}
		n.seen[key] = time.Now()
	}
	n.d.Items = append(n.d.Items, Notification{ID: n.d.Next, Time: time.Now(), Type: typ, Level: levelFor(typ), Platform: platform, Params: params})
	n.d.Next++
	if len(n.d.Items) > maxNotifications {
		n.d.Items = n.d.Items[len(n.d.Items)-maxNotifications:]
	}
	n.saveLocked()
}

// Has reports whether an event of this type with params[key]==val is already logged.
func (n *Notifier) Has(typ, key, val string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, it := range n.d.Items {
		if it.Type == typ && it.Params[key] == val {
			return true
		}
	}
	return false
}

// Summary is what /api/state carries.
func (n *Notifier) Summary() map[string]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	var unread, last int64
	for _, it := range n.d.Items {
		if it.ID > n.d.LastRead {
			unread++
		}
		last = it.ID
	}
	return map[string]int64{"unread": unread, "last_id": last}
}

// List returns the log newest first plus the read watermark.
func (n *Notifier) List() ([]Notification, int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Notification, 0, len(n.d.Items))
	for i := len(n.d.Items) - 1; i >= 0; i-- {
		out = append(out, n.d.Items[i])
	}
	return out, n.d.LastRead
}

func (n *Notifier) MarkAllRead() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.d.Items) > 0 {
		n.d.LastRead = n.d.Items[len(n.d.Items)-1].ID
	}
	n.saveLocked()
}

func (n *Notifier) Clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.d.Items) > 0 {
		n.d.LastRead = n.d.Items[len(n.d.Items)-1].ID
	}
	n.d.Items = nil
	n.saveLocked()
}

// ------------------------------------------------------------- HTTP

func (a *API) listNotifications(w http.ResponseWriter, r *http.Request) {
	items, lastRead := a.m.notes.List()
	writeJSON(w, 200, map[string]any{"items": items, "last_read": lastRead, "types": notifyTypes})
}

func (a *API) readNotifications(w http.ResponseWriter, r *http.Request) {
	a.m.notes.MarkAllRead()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) clearNotifications(w http.ResponseWriter, r *http.Request) {
	a.m.notes.Clear()
	w.WriteHeader(http.StatusNoContent)
}

func validNotifyType(t string) bool {
	for _, x := range notifyTypes {
		if x == t {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- platform icons

var iconFileRe = regexp.MustCompile(`^[a-z0-9_-]{1,40}\.(svg|png|webp)$`)

// customIcons lists platform icons the user dropped into <data>/icons
// (e.g. twitch.svg); the UI falls back to a monogram badge for the rest.
func (a *API) customIcons() map[string]string {
	out := map[string]string{}
	es, err := os.ReadDir(filepath.Join(a.m.cfg.DataDir, "icons"))
	if err != nil {
		return out
	}
	for _, e := range es {
		if !e.IsDir() && iconFileRe.MatchString(e.Name()) {
			out[strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))] = e.Name()
		}
	}
	return out
}

func (a *API) serveIcon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !iconFileRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".svg") { // never let an SVG run script in our origin
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filepath.Join(a.m.cfg.DataDir, "icons", name))
}
