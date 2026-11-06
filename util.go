package main

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tailBuf keeps the last few KB written to it (for error messages).
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// lastLine returns the last non-empty line of s, shortened.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				l = l[:300]
			}
			return l
		}
	}
	return ""
}

// normalizeURL makes sure the URL has a scheme and a host.
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errBad("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errBad("invalid url")
	}
	return u.String(), nil
}

// detectPlatform guesses the streamlink plugin name from the URL host.
func detectPlatform(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
	switch {
	case h == "twitch.tv" || strings.HasSuffix(h, ".twitch.tv"):
		return "twitch"
	case h == "youtube.com" || strings.HasSuffix(h, ".youtube.com") || h == "youtu.be":
		return "youtube"
	case h == "kick.com":
		return "kick"
	case h == "nicovideo.jp" || h == "live.nicovideo.jp":
		return "niconico"
	}
	parts := strings.Split(h, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return h
}

// streamerFromURL derives a display name from the URL path.
func streamerFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "stream"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s := strings.TrimPrefix(segs[i], "@")
		switch strings.ToLower(s) {
		case "", "live", "streams", "videos", "featured":
			continue
		}
		return s
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func streamerName(ch Channel) string {
	if strings.TrimSpace(ch.Name) != "" {
		return strings.TrimSpace(ch.Name)
	}
	return streamerFromURL(ch.URL)
}

var badChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]+`)

// sanitizeFilename removes characters that are unsafe in file names.
func sanitizeFilename(s string) string {
	s = badChars.ReplaceAllString(s, "_")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " .")
	r := []rune(s)
	if len(r) > 150 {
		s = string(r[:150])
	}
	if s == "" {
		s = "recording"
	}
	return s
}

func expandTemplate(tpl string, ch Channel, title string, now time.Time) string {
	if strings.TrimSpace(tpl) == "" {
		tpl = "{streamer}_{date}_{time}.mp4"
	}
	plugin := ch.Plugin
	if plugin == "" {
		plugin = detectPlatform(ch.URL)
	}
	r := strings.NewReplacer(
		"{streamer}", streamerName(ch),
		"{date}", now.Format("2006-01-02"),
		"{time}", now.Format("15-04-05"),
		"{platform}", plugin,
		"{title}", title,
	)
	name := sanitizeFilename(r.Replace(tpl))
	if filepath.Ext(name) == "" {
		name += ".mp4"
	}
	return name
}

// uniqueName returns name, or name_2, name_3... if it already exists in dir.
func uniqueName(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := name
	for i := 2; ; i++ {
		// the name is taken if the same base exists in any media container
		taken := false
		for _, e := range mediaExts {
			if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(cand, ext)+e)); err == nil {
				taken = true
				break
			}
		}
		if !taken {
			return cand
		}
		cand = base + "_" + strconv.Itoa(i) + ext
	}
}

var nameMu sync.Mutex

// reserveName picks a free name (see uniqueName) and atomically creates an empty
// placeholder file for it, so concurrent jobs can never pick the same name.
// The placeholder gets the extension placeholderExt (e.g. ".ts" for recordings).
func reserveName(dir, name, placeholderExt string) (string, error) {
	nameMu.Lock()
	defer nameMu.Unlock()
	n := uniqueName(dir, name)
	ph := strings.TrimSuffix(n, filepath.Ext(n)) + placeholderExt
	f, err := os.OpenFile(filepath.Join(dir, ph), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return "", err
	}
	return n, f.Close()
}

var mediaExts = []string{".mp4", ".ts", ".mkv", ".flv", ".webm", ".m4v", ".mov"}

func isMediaFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range mediaExts {
		if e == ext {
			return true
		}
	}
	return false
}

// safeRecordingName validates a single path segment coming from the client.
func safeRecordingName(name string) bool {
	return name != "" && name == filepath.Base(name) && !strings.HasPrefix(name, ".") &&
		!strings.ContainsAny(name, `/\`) && isMediaFile(name)
}

type cookie struct{ Domain, Name, Value string }

// parseNetscapeCookies parses a cookies.txt file and returns the cookies that
// apply to host.
func parseNetscapeCookies(path, host string) ([]cookie, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	host = strings.ToLower(host)
	var out []cookie
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) < 7 {
			continue
		}
		d := strings.ToLower(strings.TrimPrefix(p[0], "."))
		if host == d || strings.HasSuffix(host, "."+d) {
			out = append(out, cookie{d, p[5], p[6]})
		}
	}
	return out, sc.Err()
}

// validCookiesFile checks that the content looks like a Netscape cookies.txt.
func validCookiesFile(b []byte) bool {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#HttpOnly_"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(strings.Split(line, "\t")) >= 7 {
			n++
		}
	}
	return n > 0
}

// sortQualities orders names like 1080p60, 720p, 480p, audio_only, worst.
func sortQualities(q []string) {
	num := func(s string) int {
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		return n
	}
	sort.SliceStable(q, func(i, j int) bool {
		a, b := num(q[i]), num(q[j])
		if a != b {
			return a > b
		}
		return q[i] < q[j]
	})
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }
func errBad(msg string) error      { return &httpError{400, msg} }
