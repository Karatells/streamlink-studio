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

// Job is a post-processing task (remux / trim / merge / compress) run by ffmpeg.
type Job struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"` // remux | trim
	Input    string    `json:"input"`
	Output   string    `json:"output"`
	Status   string    `json:"status"`   // queued | running | done | error
	Progress float64   `json:"progress"` // 0..1, -1 = unknown
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
}

// refLabel is the file name of a ref (job notifications show names, not paths).
func refLabel(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func (m *Manager) newJob(typ, in, out string) *Job { return m.newJobAs(typ, in, out, "running") }

// newJobAs creates a job; a "queued" job announces itself only when it starts.
func (m *Manager) newJobAs(typ, in, out, status string) *Job {
	j := &Job{ID: newID(), Type: typ, Input: in, Output: out, Status: status, Progress: -1, Started: time.Now()}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	if len(m.jobs) > 60 { // drop the oldest finished jobs
		keep := m.jobs[:0]
		drop := len(m.jobs) - 60
		for _, o := range m.jobs {
			if drop > 0 && o.Status != "running" && o.Status != "queued" {
				drop--
				continue
			}
			keep = append(keep, o)
		}
		m.jobs = keep
	}
	m.mu.Unlock()
	if status == "running" {
		m.jobEvent(j, "job_started")
	}
	return j
}

// jobEvent records a processing notification (Input/Output never change after creation).
func (m *Manager) jobEvent(j *Job, typ string) {
	p := map[string]string{"kind": j.Type, "input": refLabel(j.Input), "output": refLabel(j.Output)}
	if typ == "job_failed" {
		m.mu.Lock()
		p["error"] = j.Error
		m.mu.Unlock()
	}
	switch typ {
	case "job_done":
		m.cnt.Inc("streamlink_studio_jobs_total", "type", j.Type, "result", "done")
	case "job_failed":
		m.cnt.Inc("streamlink_studio_jobs_total", "type", j.Type, "result", "error")
		m.elog.Add(Channel{}, "job", "error", j.Type+" failed: "+j.Input, p["error"])
	}
	m.notes.Notify(typ, "", p, "")
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
		m.jobEvent(j, "job_failed")
		return err
	}
	if err := cmd.Start(); err != nil {
		m.setJob(j, func(j *Job) { j.Status, j.Error = "error", err.Error() })
		m.jobEvent(j, "job_failed")
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
		m.jobEvent(j, "job_failed")
		return fmt.Errorf("%s", msg)
	}
	m.setJob(j, func(j *Job) { j.Status, j.Progress = "done", 1 })
	m.jobEvent(j, "job_done")
	return nil
}

// outputTarget picks the directory a result file goes to: dest (a folder of the
// Saved tree, "" = its root) when given, otherwise next to the source ref.
func (m *Manager) outputTarget(ref string, dest *string) (dirRef, dirAbs string, err error) {
	if dest != nil {
		rel, err := cleanRel(*dest)
		if err != nil {
			return "", "", err
		}
		dirAbs, err = m.savedAbs(rel)
		if err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(dirAbs, 0o750); err != nil {
			return "", "", err
		}
		if rel == "" {
			return savedRoot, dirAbs, nil
		}
		return savedPrefix + rel, dirAbs, nil
	}
	dirRef, _ = splitRef(ref)
	dirAbs, err = m.dirRefPath(dirRef)
	return dirRef, dirAbs, err
}

// srcFile resolves a ref and checks that it can be processed.
func (m *Manager) srcFile(ref string) (string, error) {
	in, err := m.refPath(ref)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(in); err != nil || !fi.Mode().IsRegular() {
		return "", &httpError{404, "file not found"}
	}
	if m.ActiveFile(ref) {
		return "", &httpError{409, "file is being recorded"}
	}
	return in, nil
}

// Convert remuxes a recording to MP4 without re-encoding (async).
func (m *Manager) Convert(ref string) (*Job, error) {
	in, err := m.srcFile(ref)
	if err != nil {
		return nil, err
	}
	dirRef, dirAbs, err := m.outputTarget(ref, nil)
	if err != nil {
		return nil, err
	}
	_, name := splitRef(ref)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	outName, err := reserveName(dirAbs, base+".mp4", ".mp4")
	if err != nil {
		return nil, err
	}
	outRef := joinRef(dirRef, outName)
	m.meta.copy(ref, outRef)
	j := m.newJob("remux", ref, outRef)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = m.runFFmpeg(j, remuxArgs(in, filepath.Join(dirAbs, outName)), in, 0)
	}()
	return &snap, nil
}

// Trim cuts [start,end] seconds out of a file (async). mode "fast" copies
// streams (cuts on keyframes); "accurate" re-encodes. dest (optional) is a
// folder of the Saved tree the clip is written to.
func (m *Manager) Trim(ref string, start, end float64, mode string, dest *string) (*Job, error) {
	in, err := m.srcFile(ref)
	if err != nil {
		return nil, err
	}
	if start < 0 || end <= start {
		return nil, errBad("invalid range: OUT must be greater than IN")
	}
	dirRef, dirAbs, err := m.outputTarget(ref, dest)
	if err != nil {
		return nil, err
	}
	_, name := splitRef(ref)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	outName, err := reserveName(dirAbs, base+"_trim.mp4", ".mp4")
	if err != nil {
		return nil, err
	}
	outRef := joinRef(dirRef, outName)
	out := filepath.Join(dirAbs, outName)
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
	m.meta.copy(ref, outRef)
	j := m.newJob("trim", ref, outRef)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = m.runFFmpeg(j, args, in, end-start)
	}()
	return &snap, nil
}

// infoFor builds the RecInfo of any ref (Name is the ref).
func (m *Manager) infoFor(ref string) (RecInfo, error) {
	p, err := m.refPath(ref)
	if err != nil {
		return RecInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return RecInfo{}, &httpError{404, "file not found: " + ref}
	}
	_, name := splitRef(ref)
	channels := m.st.Channels()
	st, pl, ti, started := mediaInfo(ref, name, fi.ModTime(), m.meta.snapshot(), channels, channelIndex(channels))
	return RecInfo{Name: ref, Size: fi.Size(), ModTime: fi.ModTime(), Started: started, Streamer: st, Platform: pl, Title: ti,
		Ext: strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), "."), Recording: m.ActiveFile(ref)}, nil
}

// concatQuote escapes a path for the ffmpeg concat demuxer list file.
func concatQuote(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }

// Merge joins recordings (already ordered oldest -> newest) into one file with
// the concat demuxer and stream copy - no re-encoding (async).
func (m *Manager) Merge(items []RecInfo, deleteSources bool, dest *string) (*Job, error) {
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
	dirRef, dirAbs, err := m.outputTarget(items[0].Name, dest)
	if err != nil {
		return nil, err
	}
	started := items[0].Started
	base := sanitizeFilename(streamer) + "_" + started.Format("2006-01-02_15-04-05") + "_merged"
	outName, err := reserveName(dirAbs, base+"."+ext, "."+ext)
	if err != nil {
		return nil, err
	}
	outRef := joinRef(dirRef, outName)
	out := filepath.Join(dirAbs, outName)
	paths := make([]string, len(items))
	for i, it := range items {
		if paths[i], err = m.refPath(it.Name); err != nil {
			_ = os.Remove(out)
			return nil, err
		}
	}

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
		fmt.Fprintf(list, "file %s\n", concatQuote(paths[i]))
	}
	list.Close()

	args := []string{"-f", "concat", "-safe", "0", "-i", list.Name(), "-map", "0:v?", "-map", "0:a?", "-c", "copy"}
	if ext == "mp4" || ext == "m4v" || ext == "mov" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, out)

	m.meta.set([]string{outRef}, RecMeta{Streamer: streamer, Platform: platform, Title: items[0].Title, Started: started})
	label := names[0]
	if len(names) > 1 {
		label = fmt.Sprintf("%s +%d", names[0], len(names)-1)
	}
	j := m.newJob("merge", label, outRef)
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer os.Remove(list.Name())
		var total float64
		for _, p := range paths {
			total += m.probeDuration(p)
		}
		if err := m.runFFmpeg(j, args, "", total); err != nil {
			m.meta.remove(outRef)
			return
		}
		if deleteSources { // only after a successful merge
			var gone []string
			for i, n := range names {
				if !m.ActiveFile(n) && os.Remove(paths[i]) == nil {
					gone = append(gone, n)
				}
			}
			m.meta.remove(gone...)
		}
	}()
	return &snap, nil
}
