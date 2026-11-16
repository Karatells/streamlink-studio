package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Preview strips: one JPEG per video with thumbCount frames side by side. The UI
// shows the strip as a background image and slides it while the mouse moves, so
// a single request gives a scrubbing preview. Strips are cached on disk, keyed
// by file ref + size + mtime, and rendered at most thumbWorkers at a time.

const (
	thumbCount   = 8
	thumbW       = 160
	thumbH       = 90
	thumbWorkers = 2
)

type thumbCache struct {
	mu       sync.Mutex
	inflight map[string]chan struct{}
	sem      chan struct{}
	pruned   time.Time
}

func newThumbCache() *thumbCache {
	return &thumbCache{inflight: map[string]chan struct{}{}, sem: make(chan struct{}, thumbWorkers)}
}

func (m *Manager) thumbDir() string { return filepath.Join(m.cfg.DataDir, "thumbs") }

func thumbKey(ref string, size int64, mod time.Time) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d", ref, size, mod.UnixNano())))
	return hex.EncodeToString(h[:10])
}

func thumbArgs(in, out string, dur float64) []string {
	var args []string
	var fc, labels strings.Builder
	for i := 0; i < thumbCount; i++ {
		at := 0.0
		if dur > 0 {
			at = dur * (float64(i) + 0.5) / thumbCount
			if dur > 3 && at > dur-1 { // the last frame must exist, or ffmpeg writes nothing
				at = dur - 1
			}
		}
		args = append(args, "-ss", fmt.Sprintf("%.2f", at), "-i", in)
		fmt.Fprintf(&fc, "[%d:v:0]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1[v%d];", i, thumbW, thumbH, thumbW, thumbH, i)
		fmt.Fprintf(&labels, "[v%d]", i)
	}
	fmt.Fprintf(&fc, "%shstack=inputs=%d[o]", labels.String(), thumbCount)
	return append([]string{"-hide_banner", "-loglevel", "error", "-y"}, append(args, "-filter_complex", fc.String(), "-map", "[o]", "-frames:v", "1", "-q:v", "6", "-f", "image2", out)...)
}

// Thumb returns the path of the cached preview strip of ref, rendering it first
// when needed. A 404 error means the file has no usable video.
func (m *Manager) Thumb(ctx context.Context, ref string) (string, error) {
	in, err := m.srcFileLoose(ref)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(in)
	if err != nil {
		return "", &httpError{404, "file not found"}
	}
	if !isMediaFile(in) {
		return "", &httpError{404, "not a video"}
	}
	c := m.thumbs
	dir := m.thumbDir()
	key := thumbKey(ref, fi.Size(), fi.ModTime())
	jpg, none := filepath.Join(dir, key+".jpg"), filepath.Join(dir, key+".none")
	check := func() (string, error, bool) {
		if _, err := os.Stat(jpg); err == nil {
			return jpg, nil, true
		}
		if _, err := os.Stat(none); err == nil {
			return "", &httpError{404, "no preview"}, true
		}
		return "", nil, false
	}
	if p, err, ok := check(); ok {
		return p, err
	}
	c.mu.Lock()
	if ch, busy := c.inflight[key]; busy {
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if p, err, ok := check(); ok {
			return p, err
		}
		return "", &httpError{404, "no preview"}
	}
	done := make(chan struct{})
	c.inflight[key] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		close(done)
	}()

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if p, err, ok := check(); ok {
		return p, err
	}
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, key+".tmp.jpg")
	rc, cancel := context.WithTimeout(context.Background(), 40*time.Second) // keep rendering even if the client left
	defer cancel()
	cmd := exec.CommandContext(rc, m.ffmpegBin(), thumbArgs(in, tmp, m.probeDuration(in))...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		if fi, e := os.Stat(tmp); e != nil || fi.Size() == 0 {
			err = fmt.Errorf("ffmpeg wrote no frame")
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		if rc.Err() == nil { // ffmpeg itself said no: remember it
			_ = os.WriteFile(none, []byte(strings.TrimSpace(string(out))), 0o644)
		}
		return "", &httpError{404, "no preview"}
	}
	if err := os.Rename(tmp, jpg); err != nil {
		return "", err
	}
	m.pruneThumbs()
	return jpg, nil
}

// srcFileLoose is srcFile without the "being recorded" refusal's side effects:
// growing files get no preview, everything else resolves like a normal ref.
func (m *Manager) srcFileLoose(ref string) (string, error) {
	p, err := m.refPath(ref)
	if err != nil {
		return "", err
	}
	if m.ActiveFile(ref) {
		return "", &httpError{404, "file is being recorded"}
	}
	return p, nil
}

// pruneThumbs drops previews that were not requested for 30 days (at most hourly).
func (m *Manager) pruneThumbs() {
	c := m.thumbs
	c.mu.Lock()
	if time.Since(c.pruned) < time.Hour {
		c.mu.Unlock()
		return
	}
	c.pruned = time.Now()
	c.mu.Unlock()
	es, _ := os.ReadDir(m.thumbDir())
	cut := time.Now().AddDate(0, 0, -30)
	for _, e := range es {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cut) {
			_ = os.Remove(filepath.Join(m.thumbDir(), e.Name()))
		}
	}
}

// GET /api/thumb?ref=<name | saved/path>
func (a *API) thumb(w http.ResponseWriter, r *http.Request) {
	p, err := a.m.Thumb(r.Context(), r.URL.Query().Get("ref"))
	if err != nil {
		if r.Context().Err() == nil {
			writeErr(w, err)
		}
		return
	}
	if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
		now := time.Now()
		_ = os.Chtimes(p, now, now) // "used recently" for pruning
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, p)
}
