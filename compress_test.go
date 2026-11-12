package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkBigVideo(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=25:d=3",
		"-f", "lavfi", "-i", "sine=d=3", "-c:v", "libx264", "-crf", "8", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg unavailable: %v %s", err, out)
	}
}

func TestCompressArgs(t *testing.T) {
	o := CompressOpts{Codec: "h265", Level: "strong", MaxHeight: 720}
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	a := strings.Join(compressArgs("in.ts", "out.mp4", o), " ")
	for _, want := range []string{"libx265", "-crf 30", "hvc1", "scale=-2:'min(ih,720)'", "-c:a copy"} {
		if !strings.Contains(a, want) {
			t.Fatalf("args miss %q: %s", want, a)
		}
	}
	for _, bad := range []CompressOpts{{Codec: "vp9"}, {Level: "insane"}, {MaxHeight: 123}} {
		if bad.normalize() == nil {
			t.Fatalf("%+v should be rejected", bad)
		}
	}
}

func TestCompressReplaceKeepAndNoGain(t *testing.T) {
	api, _ := testAPI(t)
	rec := api.m.cfg.RecDir
	mkBigVideo(t, filepath.Join(rec, "a.mp4"))
	mkBigVideo(t, filepath.Join(rec, "b.mp4"))
	orig, _ := os.Stat(filepath.Join(rec, "a.mp4"))

	if c, _ := call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"a.mp4"}, "codec": "vp9"}); c != 400 {
		t.Fatalf("bad codec: %d", c)
	}
	if c, _ := call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"nope.mp4"}}); c != 404 {
		t.Fatalf("missing file: %d", c)
	}

	// replace=true: same name, smaller file, no leftovers
	c, raw := call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"a.mp4"}, "level": "strong", "replace": true})
	if c != 202 {
		t.Fatalf("compress: %d %s", c, raw)
	}
	var jobs []Job
	_ = json.Unmarshal(raw, &jobs)
	j := waitJob(t, api, jobs[0].ID)
	if j.Status != "done" || j.Error != "" {
		t.Fatalf("job: %+v", j)
	}
	now, _ := os.Stat(filepath.Join(rec, "a.mp4"))
	if now.Size() >= orig.Size() {
		t.Fatalf("not smaller: %d >= %d", now.Size(), orig.Size())
	}
	if _, err := os.Stat(filepath.Join(rec, "a_compressed.mp4")); err == nil {
		t.Fatal("temporary output must be gone after replace")
	}
	if !api.m.meta.snapshot()["a.mp4"].Compressed {
		t.Fatal("file should be marked compressed")
	}

	// compressing the result again gains nothing -> original is kept and the job says so
	_, raw = call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"a.mp4"}, "level": "light", "replace": true})
	_ = json.Unmarshal(raw, &jobs)
	j = waitJob(t, api, jobs[0].ID)
	if !strings.Contains(j.Error, "not smaller") {
		t.Fatalf("expected no-gain note: %+v", j)
	}
	if again, _ := os.Stat(filepath.Join(rec, "a.mp4")); again.Size() != now.Size() {
		t.Fatal("original must be untouched on no gain")
	}

	// replace=false keeps both files
	_, raw = call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"b.mp4"}, "codec": "h264", "level": "strong"})
	_ = json.Unmarshal(raw, &jobs)
	j = waitJob(t, api, jobs[0].ID)
	if j.Status != "done" || j.Output != "b_compressed.mp4" {
		t.Fatalf("job: %+v", j)
	}
	for _, n := range []string{"b.mp4", "b_compressed.mp4"} {
		if _, err := os.Stat(filepath.Join(rec, n)); err != nil {
			t.Fatalf("%s missing", n)
		}
	}
}

func TestCompressQueuesOneAtATime(t *testing.T) {
	api, _ := testAPI(t)
	rec := api.m.cfg.RecDir
	mkBigVideo(t, filepath.Join(rec, "x.mp4"))
	mkBigVideo(t, filepath.Join(rec, "y.mp4"))
	c, raw := call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"x.mp4", "y.mp4"}, "level": "strong"})
	if c != 202 {
		t.Fatalf("%d %s", c, raw)
	}
	var jobs []Job
	_ = json.Unmarshal(raw, &jobs)
	if len(jobs) != 2 || jobs[1].Status != "queued" {
		t.Fatalf("second job should wait in the queue: %+v", jobs)
	}
	if c, _ := call(t, api, "POST", "/api/recordings/compress", map[string]any{"names": []string{"x.mp4"}}); c != 409 {
		t.Fatalf("busy file should conflict, got %d", c)
	}
	for _, j := range jobs {
		if r := waitJob(t, api, j.ID); r.Status != "done" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestAutoCompressPicksOldUnsavedOnly(t *testing.T) {
	api, _ := testAPI(t)
	rec := api.m.cfg.RecDir
	for _, n := range []string{"old.mp4", "fresh.mp4", "keep.mp4"} {
		mkBigVideo(t, filepath.Join(rec, n))
	}
	old := timeDaysAgo(10)
	for _, n := range []string{"old.mp4", "keep.mp4"} {
		_ = os.Chtimes(filepath.Join(rec, n), old, old)
	}
	if res, err := api.m.SaveRecordings([]string{"keep.mp4"}, ""); err != nil || len(res.Failed) > 0 {
		t.Fatal(err, res)
	}
	call(t, api, "PUT", "/api/settings", map[string]any{"compress_auto_days": 5, "compress_level": "strong"})
	api.m.autoCompressOnce()
	if len(api.m.Jobs()) != 1 || api.m.Jobs()[0].Input != "old.mp4" {
		t.Fatalf("only old.mp4 should be queued: %+v", api.m.Jobs())
	}
	waitJob(t, api, api.m.Jobs()[0].ID)
	api.m.autoCompressOnce() // already compressed -> nothing new
	if len(api.m.Jobs()) != 1 {
		t.Fatalf("must not recompress: %+v", api.m.Jobs())
	}
}

func timeDaysAgo(d int) time.Time { return time.Now().AddDate(0, 0, -d) }
