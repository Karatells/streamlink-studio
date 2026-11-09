package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func types(a *API) []string {
	items, _ := a.m.notes.List() // newest first
	out := make([]string, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		out = append(out, items[i].Type)
	}
	return out
}

func TestNotifierBasics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.json")
	var muted []string
	n := OpenNotifier(path, func() []string { return muted })
	n.Notify("job_done", "", map[string]string{"output": "a.mp4"}, "")
	n.Notify("rec_failed", "twitch", nil, "x|boom")
	n.Notify("rec_failed", "twitch", nil, "x|boom") // deduplicated
	muted = []string{"stream_live"}
	n.Notify("stream_live", "twitch", nil, "") // muted
	if s := n.Summary(); s["unread"] != 2 {
		t.Fatalf("summary %v", s)
	}
	n.MarkAllRead()
	if s := n.Summary(); s["unread"] != 0 || s["last_id"] != 2 {
		t.Fatalf("after read %v", s)
	}
	n.Notify("job_started", "", nil, "")
	if n.Summary()["unread"] != 1 {
		t.Fatal("new item must be unread")
	}
	// persisted, ids keep growing after a restart
	n2 := OpenNotifier(path, nil)
	if items, last := n2.List(); len(items) != 3 || last != 2 || items[0].Type != "job_started" {
		t.Fatalf("reload: %+v %d", items, last)
	}
	n2.Notify("job_done", "", nil, "")
	if items, _ := n2.List(); items[0].ID != 4 {
		t.Fatalf("id after restart: %d", items[0].ID)
	}
	n2.Clear()
	if items, _ := n2.List(); len(items) != 0 || n2.Summary()["unread"] != 0 {
		t.Fatal("clear")
	}
}

func TestNotifierCap(t *testing.T) {
	n := OpenNotifier(filepath.Join(t.TempDir(), "n.json"), nil)
	for i := 0; i < maxNotifications+25; i++ {
		n.Notify("job_done", "", nil, "")
	}
	if items, _ := n.List(); len(items) != maxNotifications {
		t.Fatal(len(items))
	}
}

func TestStreamEvents(t *testing.T) {
	a, _ := testAPI(t)
	ch := Channel{ID: "c1", URL: "https://www.twitch.tv/bob", Name: "bob"}
	a.m.st.PutChannel(ch)
	m := a.m
	check := func(r CheckResult) { m.mu.Lock(); m.rtLocked("c1").checking = true; m.mu.Unlock(); m.applyCheck(ch, r) }

	check(CheckResult{Live: true}) // already live when the app starts: no notification
	if got := types(a); len(got) != 0 {
		t.Fatalf("startup: %v", got)
	}
	check(CheckResult{}) // offline -> ended
	check(CheckResult{}) // still offline: nothing new
	check(CheckResult{Live: true})
	check(CheckResult{Live: true}) // still live: nothing new
	if got := strings.Join(types(a), ","); got != "stream_ended,stream_live" {
		t.Fatalf("events: %s", got)
	}
	items, _ := m.notes.List()
	if items[0].Platform != "twitch" || items[0].Params["channel"] != "bob" {
		t.Fatalf("params: %+v", items[0])
	}
	check(CheckResult{AuthError: true, Error: "401"})
	check(CheckResult{AuthError: true, Error: "401"}) // same state: one notification
	n := 0
	for _, x := range types(a) {
		if x == "auth_error" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("auth_error x%d", n)
	}
}

func TestNotificationAPI(t *testing.T) {
	a, _ := testAPI(t)
	a.m.notes.Notify("job_done", "", map[string]string{"output": "x.mp4"}, "")
	_, b := call(t, a, "GET", "/api/state", nil)
	var st struct {
		Notifications map[string]int64 `json:"notifications"`
	}
	json.Unmarshal(b, &st)
	if st.Notifications["unread"] != 1 {
		t.Fatalf("state: %s", b)
	}
	if c, _ := call(t, a, "POST", "/api/notifications/read", nil); c != 204 {
		t.Fatal(c)
	}
	_, b = call(t, a, "GET", "/api/notifications", nil)
	if !strings.Contains(string(b), `"last_read":1`) || !strings.Contains(string(b), `"job_done"`) {
		t.Fatal(string(b))
	}
	// mute through settings
	if c, _ := call(t, a, "PUT", "/api/settings", map[string]any{"notify_off": []string{"job_done"}}); c != 200 {
		t.Fatal(c)
	}
	a.m.notes.Notify("job_done", "", nil, "")
	if items, _ := a.m.notes.List(); len(items) != 1 {
		t.Fatal("muted type was stored")
	}
	if c, _ := call(t, a, "PUT", "/api/settings", map[string]any{"notify_off": []string{"bogus"}}); c != 400 {
		t.Fatal("unknown type accepted", c)
	}
	if c, _ := call(t, a, "DELETE", "/api/notifications", nil); c != 204 {
		t.Fatal(c)
	}
}

func TestJobNotifications(t *testing.T) {
	a, rec := testAPI(t)
	mkVideo(t, filepath.Join(rec, "v.mkv"), "red")
	a.m.meta.set([]string{"v.mkv"}, RecMeta{Streamer: "bob", Started: time.Now()})
	_, b := call(t, a, "POST", "/api/recordings/v.mkv/convert", nil)
	var j Job
	json.Unmarshal(b, &j)
	waitJob(t, a, j.ID)
	time.Sleep(100 * time.Millisecond)
	if got := strings.Join(types(a), ","); got != "job_started,job_done" {
		t.Fatalf("events: %s", got)
	}
	// a failing job
	os.WriteFile(filepath.Join(rec, "bad.mkv"), []byte("not a video"), 0o644)
	_, b = call(t, a, "POST", "/api/recordings/bad.mkv/convert", nil)
	json.Unmarshal(b, &j)
	waitJob(t, a, j.ID)
	time.Sleep(100 * time.Millisecond)
	items, _ := a.m.notes.List()
	if items[0].Type != "job_failed" || items[0].Params["error"] == "" || items[0].Level != "error" {
		t.Fatalf("failed event: %+v", items[0])
	}
}

func TestIcons(t *testing.T) {
	a, _ := testAPI(t)
	dir := filepath.Join(a.m.cfg.DataDir, "icons")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "twitch.svg"), []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), 0o644)
	os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("x"), 0o644)
	if c, _ := call(t, a, "GET", "/icons/twitch.svg", nil); c != 200 {
		t.Fatal("icon", c)
	}
	for _, p := range []string{"/icons/evil.txt", "/icons/..%2Fsettings.json", "/icons/Twitch.SVG", "/icons/missing.svg"} {
		if c, _ := call(t, a, "GET", p, nil); c == 200 {
			t.Fatalf("%s must not be served", p)
		}
	}
	_, b := call(t, a, "GET", "/api/state", nil)
	if !strings.Contains(string(b), `"icons":{"twitch":"twitch.svg"}`) {
		t.Fatalf("state icons: %s", b)
	}
}
