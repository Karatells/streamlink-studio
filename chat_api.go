package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// serveChat sends one chat file of a video (inline text, or a download with ?dl=1).
func serveChat(w http.ResponseWriter, r *http.Request, dir, video, format string) {
	if !validChatFormat(format) {
		writeErr(w, errBad("unknown chat format"))
		return
	}
	p := filepath.Join(dir, chatName(chatStem(video), format))
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		writeErr(w, &httpError{404, "no chat saved for this video"})
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	if format == "jsonl" {
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	if r.URL.Query().Get("dl") == "1" {
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(p)))
	}
	http.ServeFile(w, r, p)
}

func (a *API) recChat(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !safeRecordingName(name) {
		writeErr(w, errBad("invalid file name"))
		return
	}
	serveChat(w, r, a.m.cfg.RecDir, name, r.PathValue("fmt"))
}

func (a *API) savedChat(w http.ResponseWriter, r *http.Request) {
	p, ok := a.savedFile(w, r.PathValue("path"))
	if !ok {
		return
	}
	serveChat(w, r, filepath.Dir(p), filepath.Base(p), r.PathValue("fmt"))
}
