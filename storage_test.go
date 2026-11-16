package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func aged(t *testing.T, path string, size int64, age time.Duration) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(size) // sparse: costs no disk space
	f.Close()
	ts := time.Now().Add(-age)
	os.Chtimes(path, ts, ts)
}

func TestCleanupByAgeKeepsBusyAndRemovesChat(t *testing.T) {
	a, rec := testAPI(t)
	day := 24 * time.Hour
	aged(t, filepath.Join(rec, "old.mp4"), 100, 40*day)
	aged(t, filepath.Join(rec, "old.chat.txt"), 10, 40*day)
	aged(t, filepath.Join(rec, "new.mp4"), 100, 2*day)
	aged(t, filepath.Join(rec, "live.ts"), 100, 50*day) // still being recorded
	aged(t, filepath.Join(rec, "busy.mp4"), 100, 50*day)
	a.m.rt["c1"] = &Runtime{rec: &Rec{TmpName: "live.ts", Final: "live.mp4"}}
	a.m.jobs = append(a.m.jobs, &Job{Status: "running", Input: "busy.mp4", Output: "busy_x.mp4"})
	a.m.meta.set([]string{"old.mp4"}, RecMeta{Streamer: "bob", Started: time.Now()})

	rules := CleanupRules{Days: 30}
	plan := a.m.PlanCleanup(rules, time.Now())
	if plan.Count != 1 || plan.Items[0].Name != "old.mp4" || plan.Items[0].Reason != "age" || plan.Size != 110 {
		t.Fatalf("plan: %+v", plan)
	}
	n, size := a.m.RunCleanup(rules)
	if n != 1 || size != 110 {
		t.Fatal(n, size)
	}
	for _, gone := range []string{"old.mp4", "old.chat.txt"} {
		if exists(filepath.Join(rec, gone)) {
			t.Errorf("%s not deleted", gone)
		}
	}
	for _, kept := range []string{"new.mp4", "live.ts", "busy.mp4"} {
		if !exists(filepath.Join(rec, kept)) {
			t.Errorf("%s must be kept", kept)
		}
	}
	if _, ok := a.m.meta.snapshot()["old.mp4"]; ok {
		t.Fatal("metadata not removed")
	}
}

func TestCleanupQuotaDeletesOldestFirst(t *testing.T) {
	a, rec := testAPI(t)
	day := 24 * time.Hour
	mb := int64(1 << 20)
	aged(t, filepath.Join(rec, "a.mp4"), 600*mb, 5*day)
	aged(t, filepath.Join(rec, "b.mp4"), 600*mb, 3*day)
	aged(t, filepath.Join(rec, "c.mp4"), 600*mb, 1*day)
	plan := a.m.PlanCleanup(CleanupRules{MaxGB: 1}, time.Now())
	if plan.Count != 2 || plan.Items[0].Name != "a.mp4" || plan.Items[1].Name != "b.mp4" || plan.Items[0].Reason != "quota" {
		t.Fatalf("%+v", plan)
	}
	// age and quota do not pick the same file twice
	plan = a.m.PlanCleanup(CleanupRules{Days: 4, MaxGB: 1}, time.Now())
	if plan.Count != 2 || plan.Items[0].Reason != "age" || plan.Items[1].Reason != "quota" {
		t.Fatalf("%+v", plan)
	}
	// nothing to do when everything fits
	if p := a.m.PlanCleanup(CleanupRules{MaxGB: 10}, time.Now()); p.Count != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestLowDiskWarnVersusClean(t *testing.T) {
	a, rec := testAPI(t)
	aged(t, filepath.Join(rec, "a.mp4"), 1000, 3*24*time.Hour)
	aged(t, filepath.Join(rec, "b.mp4"), 1000, 1*24*time.Hour)
	huge := 1_000_000 // GB: the disk is always "too full"
	if p := a.m.PlanCleanup(CleanupRules{MinFreeGB: huge, Action: "warn"}, time.Now()); p.Count != 0 {
		t.Fatal("warn mode must not delete")
	}
	p := a.m.PlanCleanup(CleanupRules{MinFreeGB: huge, Action: "clean"}, time.Now())
	if p.Count != 2 || p.Items[0].Reason != "low_disk" {
		t.Fatalf("%+v", p)
	}
}

func TestStorageBackgroundPassAndNotifications(t *testing.T) {
	a, rec := testAPI(t)
	lowNotified.at = time.Time{}
	aged(t, filepath.Join(rec, "old.mp4"), 100, 40*24*time.Hour)
	s := a.m.st.Settings()
	s.RetainDays, s.MinFreeGB, s.MinFreeAction = 30, 1_000_000, "warn"
	a.m.st.SetSettings(s)

	a.m.storageOnce() // auto is off: only the low-disk warning
	if !exists(filepath.Join(rec, "old.mp4")) {
		t.Fatal("deleted without RetainAuto")
	}
	items, _ := a.m.notes.List()
	if len(items) != 1 || items[0].Type != "disk_low" || items[0].Level != "warn" {
		t.Fatalf("%+v", items)
	}
	a.m.storageOnce()
	if items, _ = a.m.notes.List(); len(items) != 1 {
		t.Fatal("the low-disk warning must not repeat every pass")
	}
	s.RetainAuto = true
	a.m.st.SetSettings(s)
	a.m.storageOnce()
	if exists(filepath.Join(rec, "old.mp4")) {
		t.Fatal("not cleaned")
	}
	items, _ = a.m.notes.List()
	if items[0].Type != "cleanup_done" || items[0].Params["count"] != "1" {
		t.Fatalf("%+v", items[0])
	}
	// the texts exist in every language
	for _, lang := range []string{"en", "ru", "uk"} {
		title, msg := locs.Notification(lang, items[0])
		if strings.HasPrefix(title, "ntf_") || strings.Contains(msg, "{") || strings.HasPrefix(msg, "ntf_") {
			t.Errorf("%s: %q %q", lang, title, msg)
		}
		_, msg = locs.Notification(lang, Notification{Type: "disk_low", Params: map[string]string{"free": "1 GB", "limit": "5 GB"}})
		if strings.Contains(msg, "{") || strings.HasPrefix(msg, "ntf_") {
			t.Errorf("%s: %q", lang, msg)
		}
	}
}

func TestCleanupAPI(t *testing.T) {
	a, rec := testAPI(t)
	aged(t, filepath.Join(rec, "old.mp4"), 100, 40*24*time.Hour)
	c, raw := call(t, a, "GET", "/api/cleanup?days=30", nil)
	if c != 200 || !strings.Contains(string(raw), `"name":"old.mp4"`) || !strings.Contains(string(raw), `"count":1`) {
		t.Fatal(c, string(raw))
	}
	if c, _ := call(t, a, "POST", "/api/cleanup/run", nil); c != 400 {
		t.Fatal("run without rules must fail", c)
	}
	for _, bad := range []map[string]any{{"retain_days": -1}, {"retain_max_gb": 99999999}, {"min_free_action": "nuke"}} {
		if c, _ := call(t, a, "PUT", "/api/settings", bad); c != 400 {
			t.Fatalf("%v accepted", bad)
		}
	}
	if c, raw := call(t, a, "PUT", "/api/settings", map[string]any{"retain_days": 30, "retain_auto": true}); c != 200 || !strings.Contains(string(raw), `"retain_days":30`) {
		t.Fatal(c, string(raw))
	}
	c, raw = call(t, a, "POST", "/api/cleanup/run", nil)
	if c != 200 || !strings.Contains(string(raw), `"deleted":1`) || exists(filepath.Join(rec, "old.mp4")) {
		t.Fatal(c, string(raw))
	}
}
