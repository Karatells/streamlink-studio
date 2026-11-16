package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Storage rules keep the recordings folder from filling the disk. They only
// ever touch the Recordings list: the Saved file manager is the place for
// things that must be kept.

const gib = int64(1) << 30

type CleanupItem struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"` // including the chat files
	ModTime time.Time `json:"mod_time"`
	Reason  string    `json:"reason"` // age | quota | low_disk
}

type CleanupPlan struct {
	Items     []CleanupItem `json:"items"`
	Count     int           `json:"count"`
	Size      int64         `json:"size"`
	LibSize   int64         `json:"lib_size"`   // all recordings
	LibCount  int           `json:"lib_count"`  //
	FreeAfter int64         `json:"free_after"` // free space once the plan ran (-1: unknown)
}

// CleanupRules are the thresholds a plan is made from (0 = rule off).
type CleanupRules struct {
	Days      int    `json:"days"`
	MaxGB     int    `json:"max_gb"`
	MinFreeGB int    `json:"min_free_gb"`
	Action    string `json:"min_free_action"` // warn | clean
}

func rulesFrom(s Settings) CleanupRules {
	a := s.MinFreeAction
	if a != "clean" {
		a = "warn"
	}
	return CleanupRules{Days: s.RetainDays, MaxGB: s.RetainMaxGB, MinFreeGB: s.MinFreeGB, Action: a}
}

func (r CleanupRules) any() bool {
	return r.Days > 0 || r.MaxGB > 0 || (r.MinFreeGB > 0 && r.Action == "clean")
}

// busyFile: being recorded, or read / written by a running job.
func (m *Manager) busyFile(name string) bool {
	if m.ActiveFile(name) {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if (j.Status == "running" || j.Status == "queued") && (j.Input == name || j.Output == name) {
			return true
		}
	}
	return false
}

// PlanCleanup works out which recordings the rules would delete right now.
func (m *Manager) PlanCleanup(r CleanupRules, now time.Time) CleanupPlan {
	type file struct {
		name string
		size int64
		mod  time.Time
		busy bool
	}
	var files []file
	var total int64
	es, _ := os.ReadDir(m.cfg.RecDir)
	for _, e := range es {
		n := e.Name()
		if e.IsDir() || !isMediaFile(n) || strings.HasPrefix(n, ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		size := fi.Size()
		for _, f := range chatFiles(m.cfg.RecDir, n) { // chat files go with their video
			if cfi, err := os.Stat(filepath.Join(m.cfg.RecDir, chatName(chatStem(n), f))); err == nil {
				size += cfi.Size()
			}
		}
		total += size
		files = append(files, file{n, size, fi.ModTime(), m.busyFile(n)})
	}
	plan := CleanupPlan{Items: []CleanupItem{}, LibSize: total, LibCount: len(files), FreeAfter: -1}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) }) // oldest first
	picked := map[string]bool{}
	var freed int64
	pick := func(f file, reason string) {
		picked[f.name] = true
		freed += f.size
		plan.Items = append(plan.Items, CleanupItem{Name: f.name, Size: f.size, ModTime: f.mod, Reason: reason})
	}
	if r.Days > 0 {
		cutoff := now.AddDate(0, 0, -r.Days)
		for _, f := range files {
			if !f.busy && f.mod.Before(cutoff) {
				pick(f, "age")
			}
		}
	}
	if r.MaxGB > 0 {
		limit := int64(r.MaxGB) * gib
		for _, f := range files {
			if total-freed <= limit {
				break
			}
			if !f.busy && !picked[f.name] {
				pick(f, "quota")
			}
		}
	}
	d := statDisk(m.cfg.RecDir)
	if d != nil {
		plan.FreeAfter = int64(d.Free)
		if r.MinFreeGB > 0 && r.Action == "clean" {
			need := int64(r.MinFreeGB)*gib - int64(d.Free)
			for _, f := range files {
				if need-freed <= 0 {
					break
				}
				if !f.busy && !picked[f.name] {
					pick(f, "low_disk")
				}
			}
		}
		plan.FreeAfter = int64(d.Free) + freed
	}
	for _, it := range plan.Items {
		plan.Count++
		plan.Size += it.Size
	}
	return plan
}

// removeRecording deletes a recording with everything that belongs to it.
func (m *Manager) removeRecording(name string) error {
	if err := os.Remove(filepath.Join(m.cfg.RecDir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeChat(m.cfg.RecDir, name)
	m.meta.remove(name)
	return nil
}

// RunCleanup deletes what the plan lists. Returns the number of files and bytes.
func (m *Manager) RunCleanup(r CleanupRules) (int, int64) {
	plan := m.PlanCleanup(r, time.Now())
	n, size := 0, int64(0)
	for _, it := range plan.Items {
		if m.busyFile(it.Name) { // became busy meanwhile
			continue
		}
		if m.removeRecording(it.Name) == nil {
			n++
			size += it.Size
		}
	}
	if n > 0 {
		m.lib.invalidate()
		m.cnt.Add("streamlink_studio_cleanup_files_total", float64(n))
		m.cnt.Add("streamlink_studio_cleanup_bytes_total", float64(size))
		m.elog.Add(Channel{}, "cleanup", "info", fmt.Sprintf("storage cleanup: %d recordings deleted, %s freed", n, fmtBytes(size)), "")
	}
	return n, size
}

var lowNotified struct {
	sync.Mutex
	at time.Time
}

// storageOnce is one pass of the background job.
func (m *Manager) storageOnce() {
	s := m.st.Settings()
	r := rulesFrom(s)
	if s.RetainAuto && r.any() {
		if n, size := m.RunCleanup(r); n > 0 {
			m.notes.Notify("cleanup_done", "", map[string]string{"count": strconv.Itoa(n), "size": fmtBytes(size)}, "")
		}
	}
	if r.MinFreeGB > 0 {
		if d := statDisk(m.cfg.RecDir); d != nil && int64(d.Free) < int64(r.MinFreeGB)*gib {
			lowNotified.Lock()
			due := time.Since(lowNotified.at) > 6*time.Hour
			if due {
				lowNotified.at = time.Now()
			}
			lowNotified.Unlock()
			if due {
				m.notes.Notify("disk_low", "", map[string]string{"free": fmtBytes(int64(d.Free)), "limit": fmt.Sprintf("%d GB", r.MinFreeGB)}, "")
			}
		}
	}
}

// RunStorage runs the storage rules in the background.
func (m *Manager) RunStorage(ctx context.Context) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.storageOnce()
		t.Reset(15 * time.Minute)
	}
}

// ------------------------------------------------------------------ HTTP

func queryInt(r *http.Request, k string, def int) int {
	if v := r.URL.Query().Get(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 1_000_000 {
			return n
		}
	}
	return def
}

// GET /api/cleanup[?days=&max_gb=&min_free_gb=&min_free_action=] previews the rules
// (the saved ones, or those passed in the query).
func (a *API) cleanupPreview(w http.ResponseWriter, r *http.Request) {
	rules := rulesFrom(a.m.st.Settings())
	rules.Days = queryInt(r, "days", rules.Days)
	rules.MaxGB = queryInt(r, "max_gb", rules.MaxGB)
	rules.MinFreeGB = queryInt(r, "min_free_gb", rules.MinFreeGB)
	if v := r.URL.Query().Get("min_free_action"); v == "warn" || v == "clean" {
		rules.Action = v
	}
	plan := a.m.PlanCleanup(rules, time.Now())
	lib := a.m.library()
	writeJSON(w, 200, map[string]any{"plan": plan, "rules": rules, "disk": statDisk(a.m.cfg.RecDir), "saved_size": lib.savedBytes, "saved_count": lib.saved})
}

// POST /api/cleanup/run applies the saved rules now.
func (a *API) cleanupRun(w http.ResponseWriter, r *http.Request) {
	rules := rulesFrom(a.m.st.Settings())
	if !rules.any() {
		writeErr(w, errBad("no cleanup rule is set"))
		return
	}
	n, size := a.m.RunCleanup(rules)
	writeJSON(w, 200, map[string]any{"deleted": n, "freed": size})
}
