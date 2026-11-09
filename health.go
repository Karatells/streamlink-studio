package main

import (
	"net/http"
	"os"
	"os/exec"
	"time"
)

type HealthCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | warn | error
	Detail string `json:"detail,omitempty"`
}

type Health struct {
	Status string        `json:"status"`
	Checks []HealthCheck `json:"checks"`
	Uptime float64       `json:"uptime_seconds"`
}

func healthRank(s string) int {
	switch s {
	case "error":
		return 2
	case "warn":
		return 1
	}
	return 0
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".health-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// runHealth evaluates everything that can silently stop recordings.
func (a *API) runHealth() Health {
	m := a.m
	var cs []HealthCheck
	add := func(id, st, detail string) { cs = append(cs, HealthCheck{id, st, detail}) }

	if t := m.lastTick.Load(); t == 0 {
		add("poller", "warn", "not started yet")
	} else if age := time.Since(time.Unix(0, t)); age > 30*time.Second {
		add("poller", "error", "last tick "+age.Round(time.Second).String()+" ago")
	} else {
		add("poller", "ok", "")
	}
	for _, d := range []struct{ id, dir string }{{"data_dir", m.cfg.DataDir}, {"rec_dir", m.cfg.RecDir}} {
		if err := writable(d.dir); err != nil {
			add(d.id, "error", err.Error())
		} else {
			add(d.id, "ok", d.dir)
		}
	}
	if d := statDisk(m.cfg.RecDir); d == nil {
		add("disk", "warn", "cannot read disk usage")
	} else {
		pct := float64(d.Free) / float64(d.Total) * 100
		detail := fmtBytes(int64(d.Free)) + " free"
		switch {
		case d.Free < 1<<30 || pct < 2:
			add("disk", "error", detail)
		case d.Free < 5<<30 || pct < 5:
			add("disk", "warn", detail)
		default:
			add("disk", "ok", detail)
		}
	}
	for _, t := range []struct{ id, bin string }{{"streamlink", m.streamlinkBin()}, {"ffmpeg", m.ffmpegBin()}} {
		if p, err := exec.LookPath(t.bin); err != nil {
			add(t.id, "error", "not found: "+t.bin)
		} else {
			add(t.id, "ok", p)
		}
	}
	bad := 0
	m.mu.Lock()
	for _, rt := range m.rt {
		if rt.Status == StatusError || rt.Status == StatusAuthError {
			bad++
		}
	}
	m.mu.Unlock()
	if bad > 0 {
		add("channels", "warn", itoa(bad)+" channel(s) with errors")
	} else {
		add("channels", "ok", "")
	}
	if s := m.st.Settings(); s.GotifyOn {
		if e, _ := m.gotify.Status()["last_error"].(string); e != "" {
			add("gotify", "warn", e)
		} else {
			add("gotify", "ok", "")
		}
	}
	h := Health{Checks: cs, Status: "ok", Uptime: time.Since(processStart).Seconds()}
	for _, c := range cs {
		if healthRank(c.Status) > healthRank(h.Status) {
			h.Status = c.Status
		}
	}
	return h
}

func (a *API) health(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.runHealth()) }

// healthz is the public liveness probe: it only fails when the poller stalls.
func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	if t := a.m.lastTick.Load(); t != 0 && time.Since(time.Unix(0, t)) > 60*time.Second {
		http.Error(w, "stalled", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}
