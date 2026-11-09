package main

import (
	"encoding/json"
	"strings"
	"sync"
)

// Locales gives the server (Gotify, Telegram) access to the same translation
// files the web UI uses, with English as the fallback.
type Locales struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

var locs = &Locales{m: map[string]map[string]string{}}

func validLangCode(l string) bool {
	if l == "" || len(l) > 10 {
		return false
	}
	for _, c := range l {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

func (l *Locales) get(lang string) map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m, ok := l.m[lang]; ok {
		return m
	}
	m := map[string]string{}
	if validLangCode(lang) {
		if b, err := webFS.ReadFile("web/locales/" + lang + ".json"); err == nil {
			_ = json.Unmarshal(b, &m)
		}
	}
	l.m[lang] = m
	return m
}

// Has reports whether a translation file exists for the language.
func (l *Locales) Has(lang string) bool { return len(l.get(lang)) > 0 }

func (l *Locales) Tr(lang, key string) string {
	if v := l.get(lang)[key]; v != "" {
		return v
	}
	if v := l.get("en")[key]; v != "" {
		return v
	}
	return key
}

func (l *Locales) Fill(lang, key string, vars map[string]string) string {
	s := l.Tr(lang, key)
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// Notification mirrors notifText()/ntf_type_* in the web UI.
func (l *Locales) Notification(lang string, n Notification) (title, msg string) {
	p := n.Params
	vars := map[string]string{"channel": p["channel"], "title": p["title"], "error": p["error"],
		"input": p["input"], "output": p["output"], "version": p["version"]}
	if p["kind"] != "" {
		vars["kind"] = l.Tr(lang, "job_"+p["kind"])
	}
	if p["channel"] != "" && n.Type == "update_available" {
		vars["channel_type"] = l.Tr(lang, "upd_ch_"+p["channel"])
		vars["channel"] = ""
	}
	msg = l.Fill(lang, "ntf_"+n.Type, vars)
	if n.Type == "stream_live" && p["recording"] != "" {
		msg += " · " + l.Tr(lang, "ntf_recording_started")
	}
	if n.Platform != "" {
		msg += " [" + n.Platform + "]"
	}
	return l.Tr(lang, "ntf_type_"+n.Type), msg
}
