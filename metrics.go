package main

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prometheus exposition (GET /metrics) and the library-size cache it needs.

var metricHelp = map[string]string{
	"streamlink_studio_checks_total":              "Stream checks by result.",
	"streamlink_studio_recordings_started_total":  "Recordings started since the process began.",
	"streamlink_studio_recordings_finished_total": "Recordings finished by result (ok, stopped, interrupted, failed).",
	"streamlink_studio_jobs_total":                "Finished processing jobs by type and result.",
	"streamlink_studio_notifications_total":       "Notification events by type (including muted ones).",
}

type libStats struct {
	files, saved int
	bytes        int64
	savedBytes   int64
	at           time.Time
}

type libCache struct {
	mu sync.Mutex
	v  libStats
}

func (c *libCache) invalidate() {
	c.mu.Lock()
	c.v.at = time.Time{}
	c.mu.Unlock()
}

// library scans the recordings directory at most every 30 seconds.
func (m *Manager) library() libStats {
	m.lib.mu.Lock()
	defer m.lib.mu.Unlock()
	if time.Since(m.lib.v.at) < 30*time.Second {
		return m.lib.v
	}
	v := libStats{at: time.Now()}
	es, _ := os.ReadDir(m.cfg.RecDir)
	for _, e := range es {
		if e.IsDir() || !isMediaFile(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			v.files++
			v.bytes += fi.Size()
		}
	}
	_ = filepath.WalkDir(m.cfg.SavedDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") && p != m.cfg.SavedDir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				v.saved++
				v.savedBytes += fi.Size()
			}
		}
		return nil
	})
	m.lib.v = v
	return v
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	m := a.m
	var p promWriter
	p.sample("streamlink_studio_build_info", "gauge", "Build information.", promLabels("version", version, "commit", currentCommit(), "go", runtime.Version()), 1)
	p.sample("streamlink_studio_uptime_seconds", "gauge", "Seconds since the process started.", "", time.Since(processStart).Seconds())

	channels := m.st.Channels()
	p.sample("streamlink_studio_channels", "gauge", "Configured channels.", "", float64(len(channels)))
	byStatus := map[string]int{}
	for _, s := range []string{StatusOffline, StatusChecking, StatusLive, StatusRecording, StatusAuthError, StatusError} {
		byStatus[s] = 0
	}
	type row struct {
		name, plat string
		live, rec  bool
		since      time.Time
		last       time.Time
	}
	rows := []row{}
	m.mu.Lock()
	active := 0
	for _, ch := range channels {
		rt := m.rt[ch.ID]
		plat := ch.Plugin
		if plat == "" {
			plat = detectPlatform(ch.URL)
		}
		rw := row{name: streamerName(ch), plat: plat}
		st := StatusOffline
		if rt != nil {
			st = rt.Status
			rw.live = st == StatusLive || st == StatusRecording
			rw.last = rt.LastCheck
			if rt.rec != nil {
				rw.rec, rw.since = true, rt.rec.Started
				active++
			}
		}
		byStatus[st]++
		rows = append(rows, rw)
	}
	var running, queued int
	for _, j := range m.jobs {
		switch j.Status {
		case "running":
			running++
		case "queued":
			queued++
		}
	}
	m.mu.Unlock()

	statuses := make([]string, 0, len(byStatus))
	for s := range byStatus {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		p.sample("streamlink_studio_channels_by_status", "gauge", "Channels per status.", promLabels("status", s), float64(byStatus[s]))
	}
	for _, rw := range rows {
		l := promLabels("channel", rw.name, "platform", rw.plat)
		p.sample("streamlink_studio_channel_live", "gauge", "1 while the channel is live.", l, b2f(rw.live))
	}
	for _, rw := range rows {
		l := promLabels("channel", rw.name, "platform", rw.plat)
		p.sample("streamlink_studio_channel_recording", "gauge", "1 while the channel is being recorded.", l, b2f(rw.rec))
	}
	for _, rw := range rows {
		if rw.rec {
			p.sample("streamlink_studio_channel_recording_seconds", "gauge", "Duration of the running recording.", promLabels("channel", rw.name, "platform", rw.plat), time.Since(rw.since).Seconds())
		}
	}
	for _, rw := range rows {
		if !rw.last.IsZero() {
			p.sample("streamlink_studio_channel_last_check_timestamp_seconds", "gauge", "Unix time of the last check.", promLabels("channel", rw.name, "platform", rw.plat), float64(rw.last.Unix()))
		}
	}
	p.sample("streamlink_studio_recordings_active", "gauge", "Recordings running right now.", "", float64(active))

	lib := m.library()
	p.sample("streamlink_studio_library_files", "gauge", "Recording files on disk.", "", float64(lib.files))
	p.sample("streamlink_studio_library_saved_files", "gauge", "Files in the Saved file manager.", "", float64(lib.saved))
	p.sample("streamlink_studio_library_saved_bytes", "gauge", "Total size of the Saved file manager.", "", float64(lib.savedBytes))
	p.sample("streamlink_studio_library_bytes", "gauge", "Total size of recording files.", "", float64(lib.bytes))
	if d := statDisk(m.cfg.RecDir); d != nil {
		p.sample("streamlink_studio_disk_total_bytes", "gauge", "Size of the recordings file system.", "", float64(d.Total))
		p.sample("streamlink_studio_disk_free_bytes", "gauge", "Free space on the recordings file system.", "", float64(d.Free))
		p.sample("streamlink_studio_disk_used_bytes", "gauge", "Used space on the recordings file system.", "", float64(d.Used))
	}
	p.sample("streamlink_studio_jobs_running", "gauge", "Processing jobs running.", "", float64(running))
	p.sample("streamlink_studio_jobs_queued", "gauge", "Processing jobs waiting.", "", float64(queued))

	unread := m.notes.Summary()["unread"]
	p.sample("streamlink_studio_notifications_unread", "gauge", "Unread notifications.", "", float64(unread))
	p.sample("streamlink_studio_update_available", "gauge", "1 when a newer release exists.", "", b2f(a.upd.Available()))

	g := m.gotify.Status()
	p.sample("streamlink_studio_gotify_sent_total", "counter", "Messages delivered to Gotify.", "", float64(g["sent"].(uint64)))
	p.sample("streamlink_studio_gotify_failed_total", "counter", "Gotify deliveries that failed.", "", float64(g["failed"].(uint64)))

	health := a.runHealth()
	p.sample("streamlink_studio_health_status", "gauge", "Overall health: 0 ok, 1 warning, 2 error.", "", float64(healthRank(health.Status)))

	p.counters(m.cnt, metricHelp)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p.sample("go_goroutines", "gauge", "Number of goroutines.", "", float64(runtime.NumGoroutine()))
	p.sample("go_memstats_alloc_bytes", "gauge", "Bytes of allocated heap objects.", "", float64(ms.Alloc))
	p.sample("go_memstats_sys_bytes", "gauge", "Bytes obtained from the system.", "", float64(ms.Sys))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(p.b.String()))
}
