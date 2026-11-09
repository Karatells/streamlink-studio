package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAPI(t *testing.T) (*API, string) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "studio.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(dir, "rec")
	os.MkdirAll(rec, 0o755)
	m := NewManager(Config{DataDir: dir, RecDir: rec}, st)
	return &API{m: m, auth: NewAuth("", "", dir), upd: NewUpdater(m)}, rec
}

func call(t *testing.T, a *API, method, path string, body any) (int, []byte) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.Routes(http.NotFoundHandler()).ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func TestBulkDeleteAndSave(t *testing.T) {
	a, rec := testAPI(t)
	for _, n := range []string{"a.mp4", "b.mp4", "c.mp4"} {
		os.WriteFile(filepath.Join(rec, n), []byte("x"), 0o644)
	}
	if c, _ := call(t, a, "POST", "/api/recordings/save", map[string]any{"names": []string{"b.mp4"}}); c != 204 {
		t.Fatal("save", c)
	}
	var list []RecInfo
	_, b := call(t, a, "GET", "/api/recordings", nil)
	json.Unmarshal(b, &list)
	for _, r := range list {
		if r.Saved != (r.Name == "b.mp4") {
			t.Fatalf("saved flag wrong: %+v", r)
		}
	}
	// saved flag survives a restart
	if !openMeta(filepath.Join(filepath.Dir(rec), "recordings.json")).isSaved("b.mp4") {
		t.Fatal("saved not persisted")
	}
	if c, _ := call(t, a, "POST", "/api/recordings/delete", map[string]any{"names": []string{"../etc/passwd"}}); c != 400 {
		t.Fatal("path traversal accepted", c)
	}
	c, b := call(t, a, "POST", "/api/recordings/delete", map[string]any{"names": []string{"a.mp4", "b.mp4", "a.mp4"}})
	if c != 200 || !strings.Contains(string(b), `"deleted":["a.mp4","b.mp4"]`) {
		t.Fatal(c, string(b))
	}
	if _, err := os.Stat(filepath.Join(rec, "c.mp4")); err != nil {
		t.Fatal("c.mp4 must remain")
	}
	if a.m.meta.isSaved("b.mp4") {
		t.Fatal("saved flag not cleaned up")
	}
}

func mkVideo(t *testing.T, path, color string) {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c="+color+":s=160x120:r=25:d=2",
		"-f", "lavfi", "-i", "sine=d=2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg unavailable: %v %s", err, out)
	}
}

func waitJob(t *testing.T, a *API, id string) Job {
	for i := 0; i < 200; i++ {
		for _, j := range a.m.Jobs() {
			if j.ID == id && j.Status != "running" && j.Status != "queued" {
				return j
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job timeout")
	return Job{}
}

func TestMergeOrder(t *testing.T) {
	a, rec := testAPI(t)
	// file names deliberately sort opposite to their start times
	files := map[string]time.Time{"z_old.mp4": time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local), "m_mid.mp4": time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local), "a_new.mp4": time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)}
	colors := map[string]string{"z_old.mp4": "red", "m_mid.mp4": "green", "a_new.mp4": "blue"}
	for n, ts := range files {
		mkVideo(t, filepath.Join(rec, n), colors[n])
		a.m.meta.set([]string{n}, RecMeta{Streamer: "bob", Platform: "Twitch", Started: ts})
	}
	// client sends them in the "wrong" order; the server must order by start time
	c, b := call(t, a, "POST", "/api/recordings/merge", map[string]any{"names": []string{"a_new.mp4", "z_old.mp4", "m_mid.mp4"}})
	if c != 202 {
		t.Fatal(c, string(b))
	}
	var j Job
	json.Unmarshal(b, &j)
	if done := waitJob(t, a, j.ID); done.Status != "done" {
		t.Fatalf("job: %+v", done)
	}
	out := filepath.Join(rec, j.Output)
	if !strings.HasPrefix(j.Output, "bob_2026-11-01_10-00-00_merged") {
		t.Fatalf("name %s", j.Output)
	}
	// duration ~ 6s
	d := a.m.probeDuration(out)
	if d < 5.5 || d > 6.5 {
		t.Fatalf("duration %.2f", d)
	}
	// first second is red, middle green, last blue
	pix := func(sec string) string {
		raw, err := exec.Command("ffmpeg", "-v", "error", "-ss", sec, "-i", out, "-frames:v", "1", "-vf", "scale=1:1,format=rgb24", "-f", "rawvideo", "-").Output()
		if err != nil || len(raw) < 3 {
			t.Fatal("frame", err)
		}
		r, g, bl := raw[0], raw[1], raw[2]
		switch {
		case r > 150 && g < 100 && bl < 100:
			return "red"
		case g > 100 && r < 100 && bl < 100:
			return "green"
		case bl > 150 && r < 100 && g < 100:
			return "blue"
		}
		return "?"
	}
	if got := []string{pix("0.5"), pix("3"), pix("5.5")}; strings.Join(got, ",") != "red,green,blue" {
		t.Fatalf("order wrong: %v", got)
	}
	// metadata of the result: oldest start, streamer kept
	_, lb := call(t, a, "GET", "/api/recordings", nil)
	var list []RecInfo
	json.Unmarshal(lb, &list)
	for _, r := range list {
		if r.Name == j.Output && (r.Streamer != "bob" || !r.Started.Equal(files["z_old.mp4"])) {
			t.Fatalf("merged meta: %+v", r)
		}
	}
	// mixed formats are refused
	os.WriteFile(filepath.Join(rec, "x.ts"), []byte("x"), 0o644)
	if c, _ := call(t, a, "POST", "/api/recordings/merge", map[string]any{"names": []string{"x.ts", "z_old.mp4"}}); c != 400 {
		t.Fatalf("mixed formats must be 400, got %d", c)
	}
	if c, _ := call(t, a, "POST", "/api/recordings/merge", map[string]any{"names": []string{"z_old.mp4"}}); c != 400 {
		t.Fatal("single file must be 400")
	}
}

func TestMergeDeleteSources(t *testing.T) {
	a, rec := testAPI(t)
	for i, n := range []string{"one.mp4", "two.mp4"} {
		mkVideo(t, filepath.Join(rec, n), "red")
		a.m.meta.set([]string{n}, RecMeta{Streamer: "bob", Started: time.Now().Add(time.Duration(i) * time.Hour)})
	}
	_, b := call(t, a, "POST", "/api/recordings/merge", map[string]any{"names": []string{"one.mp4", "two.mp4"}, "delete_sources": true})
	var j Job
	json.Unmarshal(b, &j)
	waitJob(t, a, j.ID)
	time.Sleep(200 * time.Millisecond)
	for _, n := range []string{"one.mp4", "two.mp4"} {
		if _, err := os.Stat(filepath.Join(rec, n)); err == nil {
			t.Fatalf("%s should be deleted after a successful merge", n)
		}
	}
	if _, err := os.Stat(filepath.Join(rec, j.Output)); err != nil {
		t.Fatal("merged file missing")
	}
}

func TestConcatQuote(t *testing.T) {
	if got := concatQuote("/a/it's.mp4"); got != `'/a/it'\''s.mp4'` {
		t.Fatal(got)
	}
}

func TestStats(t *testing.T) {
	a, rec := testAPI(t)
	os.WriteFile(filepath.Join(rec, "a.mp4"), make([]byte, 1000), 0o644)
	os.WriteFile(filepath.Join(rec, "b.mp4"), make([]byte, 3000), 0o644)
	a.m.meta.set([]string{"a.mp4"}, RecMeta{Streamer: "bob", Platform: "Twitch", Started: time.Now()})
	a.m.meta.set([]string{"b.mp4"}, RecMeta{Streamer: "bob", Platform: "Twitch", Started: time.Now().AddDate(0, 0, -2)})
	a.m.meta.setSaved([]string{"b.mp4"}, true)
	c, b := call(t, a, "GET", "/api/stats", nil)
	if c != 200 {
		t.Fatal(c, string(b))
	}
	var st struct {
		Disk    *diskStat `json:"disk"`
		Library struct {
			Count, Size, SavedCount, SavedSize int64
		} `json:"library"`
		Streamers []groupStat `json:"streamers"`
		Days      []dayStat   `json:"days"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	// field names are snake_case in JSON; re-read the ones we care about
	var raw map[string]map[string]any
	json.Unmarshal(b, &raw)
	if raw["library"]["count"].(float64) != 2 || raw["library"]["size"].(float64) != 4000 || raw["library"]["saved_size"].(float64) != 3000 {
		t.Fatalf("library: %v", raw["library"])
	}
	if st.Disk == nil || st.Disk.Total == 0 || st.Disk.Used > st.Disk.Total {
		t.Fatalf("disk: %+v", st.Disk)
	}
	if len(st.Streamers) != 1 || st.Streamers[0].Count != 2 || st.Streamers[0].Size != 4000 {
		t.Fatalf("streamers: %+v", st.Streamers)
	}
	if len(st.Days) != 14 || st.Days[13].Count != 1 || st.Days[11].Count != 1 {
		t.Fatalf("days: %+v", st.Days)
	}
}
