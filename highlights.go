package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Smart moments: bursts of chat activity usually mark the funny / hype parts of
// a stream. The saved JSONL chat is turned into a per-second activity curve,
// summed over a sliding window, and the strongest well-separated peaks become
// suggested clips.

const (
	momentWindow  = 20 // seconds of chat summed for one activity sample
	momentLead    = 8  // clip starts this long before the burst began
	momentTail    = 10 // and ends this long after its peak
	momentMinGap  = 45 // peaks closer than this are one moment
	momentMaxN    = 30
	momentDefault = 8
)

type Moment struct {
	Start float64  `json:"start"` // clip range, seconds in the video
	End   float64  `json:"end"`
	Peak  float64  `json:"peak"`
	Count int      `json:"count"` // chat messages in the burst
	Score float64  `json:"score"` // burst strength relative to the average chat rate
	Top   []string `json:"top,omitempty"`
}

// sensitivity -> how far above the average a burst must be (in standard deviations).
func sensitivityK(s string) float64 {
	switch s {
	case "low":
		return 3
	case "high":
		return 1
	}
	return 2
}

func detectMoments(msgs []ChatMsg, duration float64, n int, sens string) []Moment {
	if n <= 0 {
		n = momentDefault
	}
	if n > momentMaxN {
		n = momentMaxN
	}
	var last float64
	for _, m := range msgs {
		if m.Offset > last {
			last = m.Offset
		}
	}
	if duration <= 0 || duration < last {
		duration = last + momentTail
	}
	size := int(math.Ceil(duration)) + 1
	if size < 2 || len(msgs) == 0 {
		return []Moment{}
	}
	bins := make([]float64, size)
	for _, m := range msgs {
		if m.Offset < 0 || int(m.Offset) >= size {
			continue
		}
		w := 1.0
		if m.Kind == "event" {
			w = 3 // subs, gifts and raids count more
		}
		bins[int(m.Offset)] += w
	}
	// win[s] = activity in the window that ends at second s
	win := make([]float64, size)
	var run float64
	for s := 0; s < size; s++ {
		run += bins[s]
		if s >= momentWindow {
			run -= bins[s-momentWindow]
		}
		win[s] = run
	}
	var sum, sq float64
	for _, v := range win {
		sum += v
	}
	mean := sum / float64(size)
	for _, v := range win {
		sq += (v - mean) * (v - mean)
	}
	std := math.Sqrt(sq / float64(size))
	threshold := math.Max(mean+sensitivityK(sens)*std, 6) // never call a handful of messages a "moment"

	taken := make([]bool, size)
	out := []Moment{}
	for len(out) < n {
		best, bv := -1, threshold
		for s, v := range win {
			if !taken[s] && v >= bv {
				best, bv = s, v
			}
		}
		if best < 0 {
			break
		}
		for s := max(0, best-momentMinGap); s <= min(size-1, best+momentMinGap); s++ {
			taken[s] = true
		}
		start := math.Max(0, float64(best-momentWindow-momentLead))
		end := math.Min(duration, float64(best+momentTail))
		if end-start < 5 {
			continue
		}
		out = append(out, Moment{Start: start, End: end, Peak: float64(best), Count: int(bv), Score: math.Round(bv/math.Max(mean, 1)*10) / 10,
			Top: topTokens(msgs, float64(best-momentWindow), float64(best), 3)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// topTokens returns the most repeated words (usually emotes like "KEKW") in a time range.
func topTokens(msgs []ChatMsg, from, to float64, n int) []string {
	cnt := map[string]int{}
	for _, m := range msgs {
		if m.Offset < from || m.Offset > to || m.Kind == "event" {
			continue
		}
		seen := map[string]bool{}
		for _, w := range strings.Fields(m.Text) {
			if len([]rune(w)) < 2 || strings.HasPrefix(w, "@") || strings.HasPrefix(w, "!") || seen[w] {
				continue
			}
			seen[w] = true
			cnt[w]++
		}
	}
	type kv struct {
		w string
		n int
	}
	var all []kv
	for w, c := range cnt {
		if c >= 3 {
			all = append(all, kv{w, c})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n || (all[i].n == all[j].n && all[i].w < all[j].w) })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].w)
	}
	return out
}

func readChatJSONL(path string) ([]ChatMsg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ChatMsg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m ChatMsg
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// GET /api/moments?ref=<name | saved/path>&n=8&sens=low|normal|high
func (a *API) moments(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	p, err := a.m.refPath(ref)
	if err != nil {
		writeErr(w, err)
		return
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		writeErr(w, &httpError{404, "file not found"})
		return
	}
	cp := filepath.Join(filepath.Dir(p), chatName(chatStem(filepath.Base(p)), "jsonl"))
	if fi, err := os.Lstat(cp); err != nil || !fi.Mode().IsRegular() {
		writeErr(w, &httpError{404, "no chat saved for this video (enable the JSONL format in the chat settings)"})
		return
	}
	msgs, err := readChatJSONL(cp)
	if err != nil {
		writeErr(w, err)
		return
	}
	dur := a.m.probeDuration(p)
	writeJSON(w, 200, map[string]any{"moments": detectMoments(msgs, dur, queryInt(r, "n", momentDefault), r.URL.Query().Get("sens")),
		"messages": len(msgs), "duration": dur})
}

// POST /api/moments/clip {ref, clips:[{start,end}], mode, dest}
func (a *API) momentClips(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ref   string `json:"ref"`
		Clips []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"clips"`
		Mode string  `json:"mode"`
		Dest *string `json:"dest"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if len(in.Clips) == 0 || len(in.Clips) > momentMaxN {
		writeErr(w, errBad("choose 1-"+strconv.Itoa(momentMaxN)+" clips"))
		return
	}
	jobs := []*Job{}
	for _, c := range in.Clips {
		label := "moment_" + clockLabel(c.Start)
		j, err := a.m.TrimNamed(in.Ref, c.Start, c.End, in.Mode, in.Dest, label)
		if err != nil {
			if len(jobs) == 0 {
				writeErr(w, err)
				return
			}
			break
		}
		jobs = append(jobs, j)
	}
	writeJSON(w, 202, jobs)
}

func clockLabel(sec float64) string {
	s := int(sec)
	return fmt.Sprintf("%02d-%02d-%02d", s/3600, s%3600/60, s%60)
}
