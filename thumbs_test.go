package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func makeVideo(t *testing.T, path string) { makeVideoLen(t, path, 6) }

func makeVideoLen(t *testing.T, path string, secs int) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=160x90:rate=2:duration=%d", secs),
		"-pix_fmt", "yuv420p", path).CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
}

func TestThumbStripCachedAndShared(t *testing.T) {
	a, rec := testAPI(t)
	makeVideo(t, filepath.Join(rec, "v.mp4"))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // concurrent requests share one render
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, raw := call(t, a, "GET", "/api/thumb?ref=v.mp4", nil)
			if c != 200 || len(raw) < 500 || raw[0] != 0xFF || raw[1] != 0xD8 {
				t.Errorf("%d %d", c, len(raw))
			}
		}()
	}
	wg.Wait()
	es, _ := os.ReadDir(a.m.thumbDir())
	if len(es) != 1 || !strings.HasSuffix(es[0].Name(), ".jpg") {
		t.Fatalf("%v", es)
	}
	// a changed file gets a new strip
	makeVideo(t, filepath.Join(rec, "v.mp4"))
	ts := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(rec, "v.mp4"), ts, ts)
	if c, _ := call(t, a, "GET", "/api/thumb?ref=v.mp4", nil); c != 200 {
		t.Fatal(c)
	}
	if es, _ = os.ReadDir(a.m.thumbDir()); len(es) != 2 {
		t.Fatalf("%v", es)
	}
}

func TestThumbSavedAndRefusals(t *testing.T) {
	a, rec := testAPI(t)
	os.MkdirAll(filepath.Join(a.m.cfg.SavedDir, "clips"), 0o755)
	makeVideo(t, filepath.Join(a.m.cfg.SavedDir, "clips", "c.mp4"))
	if c, _ := call(t, a, "GET", "/api/thumb?ref="+url.QueryEscape("saved/clips/c.mp4"), nil); c != 200 {
		t.Fatal(c)
	}
	os.WriteFile(filepath.Join(rec, "junk.mp4"), []byte("not a video"), 0o644)
	for i := 0; i < 2; i++ { // second answer comes from the negative cache
		if c, _ := call(t, a, "GET", "/api/thumb?ref=junk.mp4", nil); c != 404 {
			t.Fatal(c)
		}
	}
	for _, bad := range []string{"", "../x.mp4", "saved/../../etc/passwd", "missing.mp4", "notes.txt"} {
		if c, _ := call(t, a, "GET", "/api/thumb?ref="+url.QueryEscape(bad), nil); c != 400 && c != 404 {
			t.Errorf("%q -> %d", bad, c)
		}
	}
	makeVideo(t, filepath.Join(rec, "live.mp4"))
	a.m.rt["c1"] = &Runtime{rec: &Rec{TmpName: "live.mp4", Final: "live.mp4"}}
	if c, _ := call(t, a, "GET", "/api/thumb?ref=live.mp4", nil); c != 404 {
		t.Fatal("active recording must not get a preview", c)
	}
}
