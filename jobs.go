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
	m.meta.copy(name, outName)
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
	m.meta.copy(name, outName)
	j := m.newJob("trim", name, outName)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = m.runFFmpeg(j, args, in, end-start)
	}()
	return &snap, nil
}

// concatQuote escapes a path for the ffmpeg concat demuxer list file.
func concatQuote(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }

// Merge joins recordings (already ordered oldest -> newest) into one file with
// the concat demuxer and stream copy - no re-encoding (async).
func (m *Manager) Merge(items []RecInfo, deleteSources bool) (*Job, error) {
	if len(items) < 2 {
		return nil, errBad("select at least two files to merge")
	}
	ext := items[0].Ext
	for _, it := range items {
		if it.Recording || m.ActiveFile(it.Name) {
			return nil, &httpError{409, "a selected file is being recorded: " + it.Name}
		}
		if it.Ext != ext {
			return nil, errBad("all files must have the same format (" + items[0].Ext + " / " + it.Ext + ") - convert them to MP4 first")
		}
	}
	streamer, platform := items[0].Streamer, items[0].Platform
	for _, it := range items {
		if it.Streamer != streamer {
			streamer, platform = "merged", ""
			break
		}
	}
	allSaved := true
	for _, it := range items {
		allSaved = allSaved && it.Saved
	}
	started := items[0].Started
	base := sanitizeFilename(streamer) + "_" + started.Format("2006-01-02_15-04-05") + "_merged"
	outName, err := reserveName(m.cfg.RecDir, base+"."+ext, "."+ext)
	if err != nil {
		return nil, err
	}
	out := filepath.Join(m.cfg.RecDir, outName)

	tmpDir := filepath.Join(m.cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		_ = os.Remove(out)
		return nil, err
	}
	list, err := os.CreateTemp(tmpDir, "concat-*.txt")
	if err != nil {
		_ = os.Remove(out)
		return nil, err
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
		fmt.Fprintf(list, "file %s\n", concatQuote(filepath.Join(m.cfg.RecDir, it.Name)))
	}
	list.Close()

	args := []string{"-f", "concat", "-safe", "0", "-i", list.Name(), "-map", "0:v?", "-map", "0:a?", "-c", "copy"}
	if ext == "mp4" || ext == "m4v" || ext == "mov" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, out)

	m.meta.set([]string{outName}, RecMeta{Streamer: streamer, Platform: platform, Title: items[0].Title, Started: started})
	if allSaved { // merging inside the Saved tab keeps the result there
		m.meta.setSaved([]string{outName}, true)
	}
	label := names[0]
	if len(names) > 1 {
		label = fmt.Sprintf("%s +%d", names[0], len(names)-1)
	}
	j := m.newJob("merge", label, outName)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer os.Remove(list.Name())
		var total float64
		for _, n := range names {
			total += m.probeDuration(filepath.Join(m.cfg.RecDir, n))
		}
		if err := m.runFFmpeg(j, args, "", total); err != nil {
			m.meta.remove(outName)
			return
		}
		if deleteSources { // only after a successful merge
			var gone []string
			for _, n := range names {
				if !m.ActiveFile(n) && os.Remove(filepath.Join(m.cfg.RecDir, n)) == nil {
					gone = append(gone, n)
				}
			}
			m.meta.remove(gone...)
		}
	}()
	return &snap, nil
}
