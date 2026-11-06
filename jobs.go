package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Job is a post-processing task (remux / trim) run by ffmpeg.
type Job struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"` // remux | trim
	Input    string    `json:"input"`
	Output   string    `json:"output"`
	Status   string    `json:"status"`   // running | done | error
	Progress float64   `json:"progress"` // 0..1, -1 = unknown
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
}

func (m *Manager) newJob(typ, in, out string) *Job {
	j := &Job{ID: newID(), Type: typ, Input: in, Output: out, Status: "running", Progress: -1, Started: time.Now()}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	if len(m.jobs) > 60 { // drop the oldest finished jobs
		keep := m.jobs[:0]
		drop := len(m.jobs) - 60
		for _, o := range m.jobs {
			if drop > 0 && o.Status != "running" {
				drop--
				continue
			}
			keep = append(keep, o)
		}
		m.jobs = keep
	}
	m.mu.Unlock()
	return j
}

// Jobs returns a snapshot, newest first.
func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for i := len(m.jobs) - 1; i >= 0; i-- {
		out = append(out, *m.jobs[i])
	}
	return out
}

func (m *Manager) setJob(j *Job, f func(*Job)) {
	m.mu.Lock()
	f(j)
	m.mu.Unlock()
}

func remuxArgs(in, out string) []string {
	return []string{"-i", in, "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart", out}
}

func (m *Manager) probeDuration(path string) float64 {
	b, err := exec.Command(m.ffprobeBin(), "-v", "error", "-show_entries", "format=duration",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return d
}

// runFFmpeg runs ffmpeg synchronously, updating the job's progress.
// expect is the expected output duration in seconds (0 = probe probeInput).
func (m *Manager) runFFmpeg(j *Job, args []string, probeInput string, expect float64) error {
	if expect <= 0 {
		expect = m.probeDuration(probeInput)
	}
	full := append([]string{"-hide_banner", "-nostdin", "-y", "-progress", "pipe:1", "-nostats"}, args...)
	cmd := exec.Command(m.ffmpegBin(), full...)
	tail := &tailBuf{}
	cmd.Stderr = tail
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.setJob(j, func(j *Job) { j.Status, j.Error = "error", err.Error() })
		return err
	}
	if err := cmd.Start(); err != nil {
		m.setJob(j, func(j *Job) { j.Status, j.Error = "error", err.Error() })
		return err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "out_time_us="); ok && expect > 0 {
			if us, err := strconv.ParseFloat(v, 64); err == nil {
				p := us / 1e6 / expect
				if p > 0.99 {
					p = 0.99
				}
				if p >= 0 {
					m.setJob(j, func(j *Job) { j.Progress = p })
				}
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		msg := lastLine(tail.String())
		if msg == "" {
			msg = err.Error()
		}
		_ = os.Remove(args[len(args)-1]) // partial output
		m.setJob(j, func(j *Job) { j.Status, j.Error = "error", msg })
		return fmt.Errorf("%s", msg)
	}
	m.setJob(j, func(j *Job) { j.Status, j.Progress = "done", 1 })
	return nil
}

// Convert remuxes a recording to MP4 without re-encoding (async).
func (m *Manager) Convert(name string) (*Job, error) {
	in := filepath.Join(m.cfg.RecDir, name)
	if _, err := os.Stat(in); err != nil {
		return nil, &httpError{404, "file not found"}
	}
	if m.ActiveFile(name) {
		return nil, &httpError{409, "file is being recorded"}
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	outName, err := reserveName(m.cfg.RecDir, base+".mp4", ".mp4")
	if err != nil {
		return nil, err
	}
	j := m.newJob("remux", name, outName)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = m.runFFmpeg(j, remuxArgs(in, filepath.Join(m.cfg.RecDir, outName)), in, 0)
	}()
	return &snap, nil
}

// Trim cuts [start,end] seconds out of a recording (async).
// mode "fast" copies streams (cuts on keyframes); "accurate" re-encodes.
func (m *Manager) Trim(name string, start, end float64, mode string) (*Job, error) {
	in := filepath.Join(m.cfg.RecDir, name)
	if _, err := os.Stat(in); err != nil {
		return nil, &httpError{404, "file not found"}
	}
	if m.ActiveFile(name) {
		return nil, &httpError{409, "file is being recorded"}
	}
	if start < 0 || end <= start {
		return nil, errBad("invalid range: OUT must be greater than IN")
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	outName, err := reserveName(m.cfg.RecDir, base+"_trim.mp4", ".mp4")
	if err != nil {
		return nil, err
	}
	out := filepath.Join(m.cfg.RecDir, outName)
	ss, to := fmt.Sprintf("%.3f", start), fmt.Sprintf("%.3f", end)
	var args []string
	if mode == "accurate" {
		args = []string{"-i", in, "-ss", ss, "-to", to, "-map", "0:v?", "-map", "0:a?",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-c:a", "aac", "-b:a", "160k",
			"-movflags", "+faststart", out}
	} else {
		args = []string{"-ss", ss, "-to", to, "-i", in, "-map", "0:v?", "-map", "0:a?", "-c", "copy",
			"-avoid_negative_ts", "make_zero", "-movflags", "+faststart", out}
	}
	j := m.newJob("trim", name, outName)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = m.runFFmpeg(j, args, in, end-start)
	}()
	return &snap, nil
}
