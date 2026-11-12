package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// RecMeta describes a recording independently of its file name, so changing the
// file-name template never affects which streamer / date a file belongs to.
type RecMeta struct {
	ChannelID string    `json:"channel_id"`
	Streamer  string    `json:"streamer"`
	Platform  string    `json:"platform"`
	Title     string    `json:"title,omitempty"`
	Quality   string    `json:"quality,omitempty"`
	Started   time.Time `json:"started"`
	// Compressed marks files produced (or already examined) by the compressor,
	// so the automatic rule never works on them twice.
	Compressed bool `json:"compressed,omitempty"`
}

// metaStore is a small JSON index: ref -> RecMeta (see files.go for refs).
type metaStore struct {
	mu   sync.Mutex
	path string
	data map[string]RecMeta
}

func openMeta(path string) *metaStore {
	ms := &metaStore{path: path, data: map[string]RecMeta{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &ms.data)
	}
	return ms
}

// parseNameList reads the legacy saved.json (a JSON array of file names).
func parseNameList(b []byte) []string {
	var names []string
	_ = json.Unmarshal(b, &names)
	return names
}

func (ms *metaStore) saveLocked() {
	b, err := json.Marshal(ms.data)
	if err != nil {
		return
	}
	tmp := ms.path + ".tmp"
	if os.WriteFile(tmp, b, 0o640) == nil {
		_ = os.Rename(tmp, ms.path)
	}
}

func (ms *metaStore) set(names []string, v RecMeta) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, n := range names {
		ms.data[n] = v
	}
	ms.saveLocked()
}

func (ms *metaStore) snapshot() map[string]RecMeta {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	out := make(map[string]RecMeta, len(ms.data))
	for k, v := range ms.data {
		out[k] = v
	}
	return out
}

func (ms *metaStore) copy(src, dst string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if v, ok := ms.data[src]; ok {
		ms.data[dst] = v
		ms.saveLocked()
	}
}

// copyPrefix duplicates every entry below one folder prefix ("saved/a/") under another.
func (ms *metaStore) copyPrefix(from, to string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	add := map[string]RecMeta{}
	for k, v := range ms.data {
		if rest, ok := strings.CutPrefix(k, from); ok {
			add[to+rest] = v
		}
	}
	for k, v := range add {
		ms.data[k] = v
	}
	if len(add) > 0 {
		ms.saveLocked()
	}
}

// rekey moves one entry to a new key (a renamed / moved file).
func (ms *metaStore) rekey(from, to string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if v, ok := ms.data[from]; ok && from != to {
		ms.data[to] = v
		delete(ms.data, from)
		ms.saveLocked()
	}
}

// rekeyPrefix moves every entry below a folder prefix ("saved/a/" -> "saved/b/").
func (ms *metaStore) rekeyPrefix(from, to string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	moved := map[string]RecMeta{}
	for k, v := range ms.data {
		if rest, ok := strings.CutPrefix(k, from); ok {
			moved[to+rest] = v
			delete(ms.data, k)
		}
	}
	for k, v := range moved {
		ms.data[k] = v
	}
	if len(moved) > 0 {
		ms.saveLocked()
	}
}

// removePrefix deletes the entry itself and everything below it.
func (ms *metaStore) removePrefix(key string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	changed := false
	for k := range ms.data {
		if k == key || strings.HasPrefix(k, key+"/") {
			delete(ms.data, k)
			changed = true
		}
	}
	if changed {
		ms.saveLocked()
	}
}

func (ms *metaStore) setCompressed(name string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	v := ms.data[name]
	v.Compressed = true
	ms.data[name] = v
	ms.saveLocked()
}

func (ms *metaStore) remove(names ...string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	changed := false
	for _, n := range names {
		if _, ok := ms.data[n]; ok {
			delete(ms.data, n)
			changed = true
		}
	}
	if changed {
		ms.saveLocked()
	}
}

// pruneMissing drops entries of files that no longer exist (called at startup).
func (ms *metaStore) pruneMissing(exists func(key string) bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	changed := false
	for k := range ms.data {
		if !exists(k) {
			delete(ms.data, k)
			changed = true
		}
	}
	if changed {
		ms.saveLocked()
	}
}

// ---------------------------------------------------------------- legacy

var dateInName = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})[ _T-](\d{2})[-.:](\d{2})[-.:](\d{2})`)
var dateOnlyInName = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)

// parseDateFromName extracts a start time from names like ..._2026-11-06_13-11-57...
func parseDateFromName(name string) (time.Time, int, bool) {
	if m := dateInName.FindStringSubmatchIndex(name); m != nil {
		s := name[m[2]:m[3]] + "-" + name[m[4]:m[5]] + "-" + name[m[6]:m[7]] + " " +
			name[m[8]:m[9]] + ":" + name[m[10]:m[11]] + ":" + name[m[12]:m[13]]
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
			return t, m[0], true
		}
	}
	if m := dateOnlyInName.FindStringIndex(name); m != nil {
		if t, err := time.ParseInLocation("2006-01-02", name[m[0]:m[1]], time.Local); err == nil {
			return t, m[0], true
		}
	}
	return time.Time{}, 0, false
}

func containsWord(hay, needle string) bool {
	hay, needle = strings.ToLower(hay), strings.ToLower(needle)
	if needle == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(needle)
		before := i == 0 || !isWordRune(rune(hay[i-1]))
		after := end >= len(hay) || !isWordRune(rune(hay[end]))
		if before && after {
			return true
		}
		from = i + 1
	}
}

func isWordRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127
}

var knownPlatforms = map[string]bool{"twitch": true, "youtube": true, "kick": true, "niconico": true,
	"tiktok": true, "trovo": true, "facebook": true, "vk": true, "bigo": true}

// guessLegacy infers streamer/started for files without metadata (recorded by
// older versions or copied in by hand). It does not rely on the streamer being
// the first part of the name, so any file-name template works.
func guessLegacy(name string, fallbackTime time.Time, channels []Channel) (streamer, platform string, started time.Time) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	started = fallbackTime
	cut := len(stem)
	if t, idx, ok := parseDateFromName(stem); ok {
		started, cut = t, idx
	}
	// 1) a known channel name appearing anywhere in the name (longest wins)
	best := -1
	for i, c := range channels {
		n := sanitizeFilename(streamerName(c))
		if containsWord(stem, n) && (best < 0 || len(n) > len(sanitizeFilename(streamerName(channels[best])))) {
			best = i
		}
	}
	if best >= 0 {
		c := channels[best]
		pl := c.Plugin
		if pl == "" {
			pl = detectPlatform(c.URL)
		}
		return streamerName(c), pl, started
	}
	// 2) whatever precedes the date, minus platform words
	var keep []string
	for _, tok := range strings.FieldsFunc(stem[:cut], func(r rune) bool { return r == '_' || r == ' ' }) {
		if knownPlatforms[strings.ToLower(tok)] {
			platform = strings.ToLower(tok)
			continue
		}
		keep = append(keep, tok)
	}
	if len(keep) == 0 { // nothing before the date: take the part after it
		for _, tok := range strings.FieldsFunc(stem[cut:], func(r rune) bool { return r == '_' || r == ' ' }) {
			if dateOnlyInName.MatchString(tok) || regexp.MustCompile(`^\d{2}[-.:]\d{2}[-.:]\d{2}$`).MatchString(tok) {
				continue
			}
			if knownPlatforms[strings.ToLower(tok)] {
				platform = strings.ToLower(tok)
				continue
			}
			keep = append(keep, tok)
		}
	}
	if len(keep) == 0 {
		return stem, platform, started
	}
	return strings.Join(keep, "_"), platform, started
}
