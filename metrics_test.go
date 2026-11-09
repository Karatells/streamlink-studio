package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricsAndHealth(t *testing.T) {
	api, _ := testAPI(t)
	mkVideo(t, filepath.Join(api.m.cfg.RecDir, "v.mp4"), "red")
	api.m.cnt.Inc("streamlink_studio_checks_total", "result", "live")
	api.m.notes.Notify("stream_live", "twitch", map[string]string{"channel": "bob"}, "")
	api.m.lastTick.Store(time.Now().UnixNano())

	c, raw := call(t, api, "GET", "/metrics", nil)
	if c != 200 {
		t.Fatalf("metrics: %d", c)
	}
	body := string(raw)
	for _, want := range []string{
		"# TYPE streamlink_studio_build_info gauge", "streamlink_studio_library_files 1",
		`streamlink_studio_channels_by_status{status="offline"} 0`,
		`streamlink_studio_checks_total{result="live"} 1`,
		`streamlink_studio_notifications_total{type="stream_live"} 1`,
		"streamlink_studio_health_status ", "go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics miss %q:\n%s", want, body)
		}
	}
	// every TYPE line appears once per metric name (Prometheus rejects duplicates)
	seen := map[string]bool{}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "# TYPE ") {
			if seen[l] {
				t.Fatalf("duplicate %s", l)
			}
			seen[l] = true
		}
	}

	c, raw = call(t, api, "GET", "/api/health", nil)
	if c != 200 || !strings.Contains(string(raw), `"id":"rec_dir","status":"ok"`) || !strings.Contains(string(raw), `"id":"poller","status":"ok"`) {
		t.Fatalf("health: %d %s", c, raw)
	}
}

func TestHealthzStalls(t *testing.T) {
	api, _ := testAPI(t)
	rec := httptest.NewRecorder()
	api.healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("fresh process must be healthy: %d", rec.Code)
	}
	api.m.lastTick.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	rec = httptest.NewRecorder()
	api.healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("stalled poller must fail the probe: %d", rec.Code)
	}
}

func TestMetricsBearerToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STUDIO_METRICS_TOKEN", "scrape-me")
	a := NewAuth("admin", "pw", dir)
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("data")) }))
	do := func(path, authz string) int {
		req := httptest.NewRequest("GET", path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if do("/metrics", "") == 200 {
		t.Fatal("anonymous must be rejected")
	}
	if do("/metrics", "Bearer wrong") == 200 {
		t.Fatal("wrong token must be rejected")
	}
	if do("/metrics", "Bearer scrape-me") != 200 {
		t.Fatal("token should open /metrics")
	}
	if do("/api/state", "Bearer scrape-me") == 200 {
		t.Fatal("token must not open anything but /metrics")
	}
}

func TestEventLogCollapseFilterAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	l := OpenEventLog(path)
	a := Channel{ID: "a", URL: "https://twitch.tv/bob"}
	b := Channel{ID: "b", URL: "https://kick.com/al"}
	l.Add(a, "check", "warn", "check failed", "boom")
	l.Add(a, "check", "warn", "check failed", "boom again")
	l.Add(b, "check", "warn", "check failed", "x")
	l.Add(a, "record", "info", "recording started", "")
	l.Add(a, "check", "warn", "check failed", "boom") // not consecutive for a -> new entry
	if got := l.List("a", "info", 50); len(got) != 3 || got[2].Count != 2 || got[2].Detail != "boom again" {
		t.Fatalf("collapse wrong: %+v", got)
	}
	if got := l.List("", "warn", 50); len(got) != 3 {
		t.Fatalf("level filter wrong: %+v", got)
	}
	if got := l.List("b", "info", 50); len(got) != 1 || got[0].Platform != "kick" {
		t.Fatalf("channel filter wrong: %+v", got)
	}
	l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("log must be persisted on close")
	}
	l2 := OpenEventLog(path)
	defer l2.Close()
	if len(l2.List("", "info", 50)) != 4 { // 3 warn entries + the record entry
		t.Fatal("entries lost after reopen")
	}
	l2.Add(a, "check", "info", "again", "")
	if got := l2.List("", "info", 1); got[0].ID <= 4 {
		t.Fatalf("IDs must keep growing after reopen: %+v", got)
	}
}

func TestEventsAPIAndRecordingJournal(t *testing.T) {
	api, _ := testAPI(t)
	ch := Channel{ID: "c1", URL: "https://twitch.tv/zed"}
	api.m.elog.Add(ch, "record", "error", "recording failed", "streamlink: error: no plugin")
	c, raw := call(t, api, "GET", "/api/events?level=error", nil)
	if c != 200 || !strings.Contains(string(raw), "no plugin") || !strings.Contains(string(raw), `"platform":"twitch"`) {
		t.Fatalf("events: %d %s", c, raw)
	}
	if c, _ := call(t, api, "DELETE", "/api/events", nil); c != 204 {
		t.Fatalf("clear: %d", c)
	}
	_, raw = call(t, api, "GET", "/api/events", nil)
	if strings.Contains(string(raw), "no plugin") {
		t.Fatal("clear did not clear")
	}
}

func TestEventLogRedactsSecrets(t *testing.T) {
	l := OpenEventLog(filepath.Join(t.TempDir(), "e.json"))
	defer l.Close()
	l.Add(Channel{ID: "a", URL: "https://twitch.tv/x"}, "check", "error",
		"bad url https://x/y?token=SECRET1&sig=abc123&q=1", "Authorization: OAuth abcdefghijklmnop\npassword=hunter2 ok")
	e := l.List("", "info", 1)[0]
	for _, bad := range []string{"SECRET1", "abc123", "abcdefghijklmnop", "hunter2"} {
		if strings.Contains(e.Msg+e.Detail, bad) {
			t.Fatalf("%q leaked: %+v", bad, e)
		}
	}
	if !strings.Contains(e.Msg, "q=1") {
		t.Fatalf("harmless params must survive: %q", e.Msg)
	}
}
