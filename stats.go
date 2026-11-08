package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var processStart = time.Now()

type diskStat struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

type groupStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Size  int64  `json:"size"`
}

type dayStat struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
	Size  int64  `json:"size"`
}

func statDisk(path string) *diskStat {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return nil
	}
	bs := uint64(s.Bsize)
	total, free := s.Blocks*bs, s.Bavail*bs
	used := (s.Blocks - s.Bfree) * bs
	return &diskStat{Path: path, Total: total, Used: used, Free: free}
}

// readProcFloat returns the first number of a /proc file ("12.3 4.5" -> 12.3).
func readProcFloat(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func readMeminfo() (total, avail uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func topGroups(m map[string]*groupStat, limit int) []groupStat {
	out := make([]groupStat, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// buildStats gathers disk, library and system numbers for the Statistics tab.
func (a *API) buildStats() map[string]any {
	recs := a.listRecordings()
	var total, savedSize int64
	var savedCount, recording int
	streamers, platforms := map[string]*groupStat{}, map[string]*groupStat{}
	add := func(m map[string]*groupStat, key string, size int64) {
		if key == "" {
			key = "—"
		}
		g := m[key]
		if g == nil {
			g = &groupStat{Name: key}
			m[key] = g
		}
		g.Count++
		g.Size += size
	}
	days := make([]dayStat, 14)
	idx := map[string]int{}
	now := time.Now()
	for i := range days {
		d := now.AddDate(0, 0, -(13 - i))
		days[i].Date = ymdGo(d)
		idx[days[i].Date] = i
	}
	largest := make([]RecInfo, 0, len(recs))
	var week int64
	for _, r := range recs {
		total += r.Size
		if r.Saved {
			savedCount++
			savedSize += r.Size
		}
		if r.Recording {
			recording++
		}
		add(streamers, r.Streamer, r.Size)
		add(platforms, r.Platform, r.Size)
		if i, ok := idx[ymdGo(r.Started)]; ok {
			days[i].Count++
			days[i].Size += r.Size
			if i >= 7 {
				week += r.Size
			}
		}
		largest = append(largest, r)
	}
	sort.Slice(largest, func(i, j int) bool { return largest[i].Size > largest[j].Size })
	if len(largest) > 5 {
		largest = largest[:5]
	}
	type big struct {
		Name     string `json:"name"`
		Streamer string `json:"streamer"`
		Size     int64  `json:"size"`
	}
	bigs := make([]big, 0, len(largest))
	for _, r := range largest {
		bigs = append(bigs, big{r.Name, r.Streamer, r.Size})
	}

	// the app's own files (database, sessions, cookies, ...), without the recordings
	dataSize := dirSize(a.m.cfg.DataDir)
	if strings.HasPrefix(a.m.cfg.RecDir, a.m.cfg.DataDir) {
		dataSize -= dirSize(a.m.cfg.RecDir)
	}

	disk := statDisk(a.m.cfg.RecDir)
	// days until the disk is full at the pace of the last 7 days (0 = unknown)
	var daysLeft float64
	if disk != nil && week > 0 {
		daysLeft = float64(disk.Free) / (float64(week) / 7)
	}
	memTotal, memAvail := readMeminfo()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	active := 0
	for _, c := range a.m.st.Channels() {
		if v := a.view(c); v.Status == StatusRecording {
			active++
		}
	}
	return map[string]any{
		"disk": disk,
		"library": map[string]any{
			"count": len(recs), "size": total, "saved_count": savedCount, "saved_size": savedSize,
			"recording_now": recording, "recordings_dir": a.m.cfg.RecDir,
			"data_size": dataSize,
		},
		"streamers": topGroups(streamers, 8),
		"platforms": topGroups(platforms, 6),
		"days":      days,
		"largest":   bigs,
		"days_left": daysLeft,
		"week_size": week,
		"channels":  map[string]int{"total": len(a.m.st.Channels()), "recording": active},
		"system": map[string]any{
			"uptime_app": int64(time.Since(processStart).Seconds()), "uptime_host": int64(readProcFloat("/proc/uptime")),
			"load1": readProcFloat("/proc/loadavg"), "cpus": runtime.NumCPU(),
			"mem_total": memTotal, "mem_avail": memAvail, "app_mem": ms.Sys, "goroutines": runtime.NumGoroutine(),
		},
	}
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.buildStats()) }

func ymdGo(t time.Time) string { return t.Local().Format("2006-01-02") }
