package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jsonOf(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("no symlinks")
	}
	for _, bad := range []string{"../x", "a/../../x", "a//b/..", ".hidden", "a/.hidden/b", `a\b`, "link/x", "link"} {
		if _, err := safeJoin(root, bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	for _, good := range []string{"", "a", "a/b/c.mp4", "Папка/файл.mp4", "new/not/yet/there"} {
		if _, err := safeJoin(root, good); err != nil {
			t.Errorf("%q must be accepted: %v", good, err)
		}
	}
}

func TestFileManagerBasics(t *testing.T) {
	a, rec := testAPI(t)
	os.WriteFile(filepath.Join(rec, "a.mp4"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(rec, "b.mp4"), []byte("bbb"), 0o644)
	a.m.meta.set([]string{"a.mp4"}, RecMeta{Streamer: "bob", Platform: "Twitch", Started: time.Now()})

	// folders
	if c, b := call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Clips"}); c != 201 {
		t.Fatal(c, string(b))
	}
	if c, _ := call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Clips"}); c != 409 {
		t.Fatal("duplicate folder must conflict", c)
	}
	for _, bad := range []string{"..", "a/b", ".x", "", " "} {
		if c, _ := call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": bad}); c != 400 {
			t.Fatalf("name %q accepted: %d", bad, c)
		}
	}
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "Clips", "name": "Best"})

	// save a recording straight into a folder
	if c, b := call(t, a, "POST", "/api/recordings/save", map[string]any{"names": []string{"a.mp4"}, "dest": "Clips"}); c != 200 {
		t.Fatal(c, string(b))
	}
	if _, err := os.Stat(filepath.Join(rec, "a.mp4")); err == nil {
		t.Fatal("recording must have moved")
	}
	var l FileListing
	_, raw := call(t, a, "GET", "/api/files?path=Clips", nil)
	jsonOf(t, raw, &l)
	if len(l.Entries) != 2 || !l.Entries[0].Dir || l.Entries[1].Name != "a.mp4" || l.Entries[1].Streamer != "bob" || l.Entries[1].Size != 3 {
		t.Fatalf("listing: %+v", l)
	}

	// rename keeps metadata
	c, raw := call(t, a, "POST", "/api/files/rename", map[string]string{"path": "Clips/a.mp4", "name": "final.mp4"})
	if c != 200 {
		t.Fatal(c, string(raw))
	}
	if md := a.m.meta.snapshot()["saved/Clips/final.mp4"]; md.Streamer != "bob" {
		t.Fatalf("meta not renamed: %+v", a.m.meta.snapshot())
	}
	if c, _ := call(t, a, "POST", "/api/files/rename", map[string]string{"path": "Clips/final.mp4", "name": "Best"}); c != 409 {
		t.Fatal("renaming onto an existing name must conflict", c)
	}
	if c, _ := call(t, a, "POST", "/api/files/rename", map[string]string{"path": "Clips/Best", "name": "Top"}); c != 200 {
		t.Fatal("folder rename", c)
	}
}

func TestFileManagerMoveCopyDelete(t *testing.T) {
	a, rec := testAPI(t)
	os.WriteFile(filepath.Join(rec, "a.mp4"), []byte("aaa"), 0o644)
	call(t, a, "POST", "/api/recordings/save", map[string]any{"names": []string{"a.mp4"}})
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "X"})
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "X", "name": "Y"})
	a.m.meta.set([]string{"saved/a.mp4"}, RecMeta{Streamer: "bob", Started: time.Now()})

	// move file; the name is made unique when taken
	os.WriteFile(filepath.Join(a.m.cfg.SavedDir, "X", "a.mp4"), []byte("other"), 0o644)
	c, raw := call(t, a, "POST", "/api/files/move", map[string]any{"paths": []string{"a.mp4"}, "dest": "X"})
	var res OpResult
	jsonOf(t, raw, &res)
	if c != 200 || len(res.Done) != 1 || res.Done[0].To != "X/a_2.mp4" {
		t.Fatal(c, string(raw))
	}
	if a.m.meta.snapshot()["saved/X/a_2.mp4"].Streamer != "bob" {
		t.Fatal("meta must follow the move")
	}

	// a folder cannot go into itself or its children
	_, raw = call(t, a, "POST", "/api/files/move", map[string]any{"paths": []string{"X"}, "dest": "X/Y"})
	jsonOf(t, raw, &res)
	if len(res.Failed) != 1 {
		t.Fatalf("folder into itself must fail: %s", raw)
	}
	if _, raw := call(t, a, "POST", "/api/files/move", map[string]any{"paths": []string{"X/a_2.mp4"}, "dest": "../.."}); !strings.Contains(string(raw), "invalid") {
		t.Fatalf("traversal must be refused: %s", raw)
	}

	// copy a folder: both exist, metadata duplicated
	_, raw = call(t, a, "POST", "/api/files/copy", map[string]any{"paths": []string{"X"}, "dest": ""})
	jsonOf(t, raw, &res)
	if len(res.Done) != 1 || res.Done[0].To != "X_2" {
		t.Fatal(string(raw))
	}
	if _, err := os.Stat(filepath.Join(a.m.cfg.SavedDir, "X_2", "a_2.mp4")); err != nil {
		t.Fatal("copied file missing")
	}
	if a.m.meta.snapshot()["saved/X_2/a_2.mp4"].Streamer != "bob" {
		t.Fatal("meta must be copied")
	}

	// move the folder: metadata keys are re-prefixed
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Z"})
	call(t, a, "POST", "/api/files/move", map[string]any{"paths": []string{"X_2"}, "dest": "Z"})
	if a.m.meta.snapshot()["saved/Z/X_2/a_2.mp4"].Streamer != "bob" {
		t.Fatalf("meta keys not re-prefixed: %+v", a.m.meta.snapshot())
	}

	// back to recordings
	_, raw = call(t, a, "POST", "/api/files/restore", map[string]any{"paths": []string{"Z/X_2/a_2.mp4"}})
	jsonOf(t, raw, &res)
	if len(res.Done) != 1 {
		t.Fatal(string(raw))
	}
	if _, err := os.Stat(filepath.Join(rec, res.Done[0].To)); err != nil {
		t.Fatal("not restored")
	}
	if a.m.meta.snapshot()[res.Done[0].To].Streamer != "bob" {
		t.Fatal("meta must follow back")
	}

	// delete folder recursively, metadata is dropped
	call(t, a, "POST", "/api/files/delete", map[string]any{"paths": []string{"X", "Z"}})
	if _, err := os.Stat(filepath.Join(a.m.cfg.SavedDir, "X")); err == nil {
		t.Fatal("folder not deleted")
	}
	for k := range a.m.meta.snapshot() {
		if strings.HasPrefix(k, "saved/") {
			t.Fatalf("stale meta %s", k)
		}
	}
	if c, _ := call(t, a, "POST", "/api/files/delete", map[string]any{"paths": []string{""}}); c != 200 {
		t.Fatal(c)
	}
	if _, err := os.Stat(a.m.cfg.SavedDir); err != nil {
		t.Fatal("the root must never be deleted")
	}
}

func rawReq(a *API, method, target string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.Routes(http.NotFoundHandler()).ServeHTTP(w, req)
	return w
}

func TestFileManagerUploadServeZip(t *testing.T) {
	a, _ := testAPI(t)
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Docs"})
	w := rawReq(a, "POST", "/api/files/upload?path=Docs&name=notes.html", []byte("<script>alert(1)</script>"))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w2 := rawReq(a, "POST", "/api/files/upload?path=Docs&name=notes.html", []byte("second"))
	if !strings.Contains(w2.Body.String(), "notes_2.html") {
		t.Fatalf("upload must not overwrite: %s", w2.Body.String())
	}
	if w := rawReq(a, "POST", "/api/files/upload?path=Docs&name=../evil", []byte("x")); w.Code != 400 {
		t.Fatal("bad upload name", w.Code)
	}
	if w := rawReq(a, "POST", "/api/files/upload?path=..&name=x", []byte("x")); w.Code != 400 {
		t.Fatal("bad upload path", w.Code)
	}
	// no temp files are left behind
	es, _ := os.ReadDir(filepath.Join(a.m.cfg.SavedDir, "Docs"))
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("leftover %s", e.Name())
		}
	}
	// non-video files are served as downloads, sandboxed
	w = rawReq(a, "GET", "/api/files/stream/Docs/notes.html", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if w := rawReq(a, "GET", "/api/files/stream/Docs", nil); w.Code != 404 {
		t.Fatal("folders are not streamable", w.Code)
	}
	if w := rawReq(a, "GET", "/api/files/download/..%2f..%2fstudio.json", nil); w.Code == 200 {
		t.Fatal("traversal served a file")
	}
	// zip of a folder
	w = rawReq(a, "GET", "/api/files/zip?path=Docs", nil)
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["Docs/notes.html"] || !names["Docs/notes_2.html"] {
		t.Fatalf("zip entries: %v", names)
	}
}

func TestSymlinksInSavedAreIgnored(t *testing.T) {
	a, _ := testAPI(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	if err := os.Symlink(outside, filepath.Join(a.m.cfg.SavedDir, "esc")); err != nil {
		t.Skip("no symlinks")
	}
	_, raw := call(t, a, "GET", "/api/files", nil)
	if strings.Contains(string(raw), "esc") {
		t.Fatalf("symlink listed: %s", raw)
	}
	if c, _ := call(t, a, "GET", "/api/files?path=esc", nil); c == 200 {
		t.Fatal("symlink followed")
	}
	if w := rawReq(a, "GET", "/api/files/stream/esc/secret.txt", nil); w.Code == 200 {
		t.Fatal("served a file through a symlink")
	}
	call(t, a, "POST", "/api/files/delete", map[string]any{"paths": []string{"esc/secret.txt"}})
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal("deleted a file outside the root")
	}
}

func TestMigrateLegacySavedList(t *testing.T) {
	a, rec := testAPI(t)
	os.WriteFile(filepath.Join(rec, "old.mp4"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(rec, "keep.mp4"), []byte("2"), 0o644)
	a.m.meta.set([]string{"old.mp4"}, RecMeta{Streamer: "bob", Started: time.Now()})
	list := filepath.Join(a.m.cfg.DataDir, "saved.json")
	os.WriteFile(list, []byte(`["old.mp4","../x.mp4","missing.mp4"]`), 0o644)
	a.m.MigrateSaved(list)
	if _, err := os.Stat(filepath.Join(a.m.cfg.SavedDir, "old.mp4")); err != nil {
		t.Fatal("not migrated")
	}
	if _, err := os.Stat(filepath.Join(rec, "keep.mp4")); err != nil {
		t.Fatal("unrelated file moved")
	}
	if a.m.meta.snapshot()["saved/old.mp4"].Streamer != "bob" {
		t.Fatal("meta not migrated")
	}
	if _, err := os.Stat(list); err == nil {
		t.Fatal("legacy list must be retired")
	}
}

func TestTrimIntoSavedFolderAndMergeInSaved(t *testing.T) {
	a, rec := testAPI(t)
	mkVideo(t, filepath.Join(rec, "v.mp4"), "red")
	mkVideo(t, filepath.Join(rec, "w.mp4"), "blue")
	a.m.meta.set([]string{"v.mp4"}, RecMeta{Streamer: "bob", Started: time.Now()})
	a.m.meta.set([]string{"w.mp4"}, RecMeta{Streamer: "bob", Started: time.Now().Add(time.Minute)})
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Clips"})

	c, raw := call(t, a, "POST", "/api/recordings/v.mp4/trim", map[string]any{"start": 0, "end": 1, "mode": "accurate", "dest": "Clips"})
	if c != 202 {
		t.Fatal(c, string(raw))
	}
	var j Job
	jsonOf(t, raw, &j)
	if j.Output != "saved/Clips/v_trim.mp4" {
		t.Fatalf("clip must land in Saved/Clips: %+v", j)
	}
	if r := waitJob(t, a, j.ID); r.Status != "done" {
		t.Fatalf("%+v", r)
	}
	if fi, err := os.Stat(filepath.Join(a.m.cfg.SavedDir, "Clips", "v_trim.mp4")); err != nil || fi.Size() == 0 {
		t.Fatal("clip missing")
	}
	if a.m.meta.snapshot()["saved/Clips/v_trim.mp4"].Streamer != "bob" {
		t.Fatal("clip lost its metadata")
	}
	if _, err := os.Stat(filepath.Join(rec, "v.mp4")); err != nil {
		t.Fatal("trim must not touch the source")
	}

	// trim a saved file in place (same folder)
	c, raw = call(t, a, "POST", "/api/files/trim", map[string]any{"path": "Clips/v_trim.mp4", "start": 0, "end": 0.5, "mode": "accurate"})
	if c != 202 {
		t.Fatal(c, string(raw))
	}
	jsonOf(t, raw, &j)
	if j.Output != "saved/Clips/v_trim_trim.mp4" {
		t.Fatalf("%+v", j)
	}
	waitJob(t, a, j.ID)

	// merge two recordings into a Saved folder
	c, raw = call(t, a, "POST", "/api/recordings/merge", map[string]any{"names": []string{"v.mp4", "w.mp4"}, "dest": "Clips"})
	if c != 202 {
		t.Fatal(c, string(raw))
	}
	jsonOf(t, raw, &j)
	if !strings.HasPrefix(j.Output, "saved/Clips/") {
		t.Fatalf("%+v", j)
	}
	if r := waitJob(t, a, j.ID); r.Status != "done" {
		t.Fatalf("%+v", r)
	}

	// compress a saved file in place
	c, raw = call(t, a, "POST", "/api/recordings/compress", map[string]any{"names": []string{"saved/Clips/v_trim.mp4"}, "level": "strong"})
	if c != 202 {
		t.Fatal(c, string(raw))
	}
	var jobs []Job
	jsonOf(t, raw, &jobs)
	if r := waitJob(t, a, jobs[0].ID); r.Status != "done" {
		t.Fatalf("%+v", r)
	}
	if c, _ := call(t, a, "POST", "/api/recordings/compress", map[string]any{"names": []string{"saved/../x.mp4"}}); c != 400 {
		t.Fatal("bad ref accepted", c)
	}
}
