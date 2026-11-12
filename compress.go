package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Compression re-encodes recordings with x264/x265 to save disk space.
// Only one encode runs at a time; the rest wait as "queued" jobs.

type CompressOpts struct {
	Codec     string `json:"codec"`      // h264 | h265
	Level     string `json:"level"`      // light | balanced | strong
	MaxHeight int    `json:"max_height"` // 0 = keep
	Replace   bool   `json:"replace"`    // delete the original when the result is smaller
}

var compressCRF = map[string]map[string]int{
	"h264": {"light": 22, "balanced": 25, "strong": 28},
	"h265": {"light": 24, "balanced": 27, "strong": 30},
}

func (o *CompressOpts) normalize() error {
	if o.Codec == "" {
		o.Codec = "h264"
	}
	if o.Level == "" {
		o.Level = "balanced"
	}
	if _, ok := compressCRF[o.Codec]; !ok {
		return errBad("codec must be h264 or h265")
	}
	if _, ok := compressCRF[o.Codec][o.Level]; !ok {
		return errBad("level must be light, balanced or strong")
	}
	switch o.MaxHeight {
	case 0, 480, 540, 720, 1080:
	default:
		return errBad("max height must be 0, 480, 540, 720 or 1080")
	}
	return nil
}

func compressArgs(in, out string, o CompressOpts) []string {
	args := []string{"-i", in, "-map", "0:v:0", "-map", "0:a?"}
	if o.Codec == "h265" {
		args = append(args, "-c:v", "libx265", "-preset", "fast", "-tag:v", "hvc1")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "medium")
	}
	args = append(args, "-crf", fmt.Sprint(compressCRF[o.Codec][o.Level]), "-pix_fmt", "yuv420p")
	if o.MaxHeight > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=-2:'min(ih,%d)'", o.MaxHeight))
	}
	return append(args, "-c:a", "copy", "-movflags", "+faststart", out)
}

// jobBusy reports whether a queued/running job already uses this file as input.
func (m *Manager) jobBusy(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if (j.Status == "running" || j.Status == "queued") && j.Input == name {
			return true
		}
	}
	return false
}

// Compress queues one re-encode job per file ref (async).
func (m *Manager) Compress(refs []string, o CompressOpts) ([]Job, error) {
	if err := o.normalize(); err != nil {
		return nil, err
	}
	for _, n := range refs {
		if _, err := m.srcFile(n); err != nil {
			return nil, err
		}
		if m.jobBusy(n) {
			return nil, &httpError{409, "file is already being processed: " + n}
		}
	}
	var out []Job
	for _, n := range refs {
		j, err := m.startCompress(n, o)
		if err != nil {
			return out, err
		}
		out = append(out, *j)
	}
	return out, nil
}

func (m *Manager) startCompress(ref string, o CompressOpts) (*Job, error) {
	in, err := m.refPath(ref)
	if err != nil {
		return nil, err
	}
	dirRef, dirAbs, err := m.outputTarget(ref, nil)
	if err != nil {
		return nil, err
	}
	_, name := splitRef(ref)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	outName, err := reserveName(dirAbs, base+"_compressed.mp4", ".mp4")
	if err != nil {
		return nil, err
	}
	outRef := joinRef(dirRef, outName)
	out := filepath.Join(dirAbs, outName)
	m.meta.copy(ref, outRef)
	m.meta.setCompressed(outRef)
	j := m.newJobAs("compress", ref, outRef, "queued")
	snap := *j
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.enc <- struct{}{} // wait for our turn
		defer func() { <-m.enc }()
		if _, err := os.Stat(in); err != nil { // deleted while queued
			m.setJob(j, func(j *Job) { j.Status, j.Error = "error", "source file no longer exists" })
			m.meta.remove(outRef)
			_ = os.Remove(out)
			return
		}
		m.setJob(j, func(j *Job) { j.Status, j.Started = "running", time.Now() })
		m.jobEvent(j, "job_started")
		if err := m.runFFmpeg(j, compressArgs(in, out, o), in, 0); err != nil {
			m.meta.remove(outRef)
			return
		}
		m.meta.setCompressed(ref)
		m.finishCompress(j, ref, in, outRef, out, o)
	}()
	return &snap, nil
}

// finishCompress applies the "replace original" choice once the encode succeeded.
func (m *Manager) finishCompress(j *Job, ref, in, outRef, out string, o CompressOpts) {
	if !o.Replace {
		return
	}
	fi, e1 := os.Stat(in)
	fo, e2 := os.Stat(out)
	if e1 != nil || e2 != nil {
		return
	}
	if fo.Size() >= fi.Size() { // no gain: keep the original
		_ = os.Remove(out)
		m.meta.remove(outRef)
		m.setJob(j, func(j *Job) { j.Error = "result is not smaller - original kept" })
		return
	}
	if m.ActiveFile(ref) {
		return
	}
	if strings.EqualFold(filepath.Ext(in), ".mp4") { // same name: swap the file in place
		if os.Rename(out, in) == nil {
			m.meta.remove(outRef)
			m.setJob(j, func(j *Job) { j.Output = ref })
		}
		return
	}
	if os.Remove(in) == nil {
		m.meta.remove(ref)
	}
}

// RunAutoCompress applies the "compress files older than N days" rule.
func (m *Manager) RunAutoCompress(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.autoCompressOnce()
		t.Reset(30 * time.Minute)
	}
}

func (m *Manager) autoCompressOnce() {
	s := m.st.Settings()
	if s.CompressAutoDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.CompressAutoDays)
	metas := m.meta.snapshot()
	entries, _ := os.ReadDir(m.cfg.RecDir)
	o := CompressOpts{Codec: s.CompressCodec, Level: s.CompressLevel, MaxHeight: s.CompressHeight, Replace: true}
	if o.normalize() != nil {
		return
	}
	queued := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !safeRecordingName(n) {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.ModTime().After(cutoff) || metas[n].Compressed || m.ActiveFile(n) || m.jobBusy(n) {
			continue
		}
		if _, err := m.startCompress(n, o); err == nil {
			if queued++; queued >= 5 {
				return
			}
		}
	}
}
