package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chat: 1 msg per 5s as background, with bursts at 300s and 900s (and a smaller one at 1500s)
func fakeChat() []ChatMsg {
	var msgs []ChatMsg
	for s := 0.0; s < 1800; s += 5 {
		msgs = append(msgs, ChatMsg{Offset: s, Kind: "msg", Text: "hello there"})
	}
	burst := func(at float64, n int, text string) {
		for i := 0; i < n; i++ {
			msgs = append(msgs, ChatMsg{Offset: at + float64(i)*0.2, Kind: "msg", Text: text})
		}
	}
	burst(300, 80, "KEKW so funny")
	burst(900, 120, "POG POG")
	burst(1500, 20, "ok")
	return msgs
}

func TestDetectMoments(t *testing.T) {
	ms := detectMoments(fakeChat(), 1800, 8, "normal")
	if len(ms) < 2 {
		t.Fatalf("%+v", ms)
	}
	for _, m := range ms {
		if m.Start < 0 || m.End > 1800 || m.End-m.Start < 5 {
			t.Errorf("bad range %+v", m)
		}
	}
	has := func(near float64) *Moment {
		for i := range ms {
			if ms[i].Start <= near && ms[i].End >= near {
				return &ms[i]
			}
		}
		return nil
	}
	if m := has(305); m == nil || len(m.Top) == 0 || m.Top[0] != "KEKW" {
		t.Fatalf("burst at 300 not found: %+v", ms)
	}
	if has(905) == nil {
		t.Fatalf("burst at 900 not found: %+v", ms)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Start < ms[i-1].Start || ms[i].Peak-ms[i-1].Peak < momentMinGap {
			t.Errorf("not sorted / too close: %+v", ms)
		}
	}
	if got := detectMoments(fakeChat(), 1800, 1, "normal"); len(got) != 1 || got[0].Peak < 900 || got[0].Peak > 925 {
		t.Fatalf("n=1 must keep the strongest burst: %+v", got)
	}
	// steady chat, empty chat, tiny chat -> nothing
	var flat []ChatMsg
	for s := 0.0; s < 600; s++ {
		flat = append(flat, ChatMsg{Offset: s, Kind: "msg", Text: "x"})
	}
	if got := detectMoments(flat, 600, 8, "normal"); len(got) != 0 {
		t.Fatalf("steady chat has no moments: %+v", got)
	}
	if len(detectMoments(nil, 100, 8, "normal")) != 0 || len(detectMoments(fakeChat()[:3], 100, 8, "normal")) != 0 {
		t.Fatal("tiny chat")
	}
	// events weigh more: a few raids make a moment where plain messages would not
	var ev []ChatMsg
	for s := 0.0; s < 600; s += 10 {
		ev = append(ev, ChatMsg{Offset: s, Kind: "msg", Text: "x"})
	}
	for i := 0; i < 4; i++ {
		ev = append(ev, ChatMsg{Offset: 300 + float64(i), Kind: "event", Text: "gift"})
	}
	if got := detectMoments(ev, 600, 8, "normal"); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

func writeJSONL(t *testing.T, path string, msgs []ChatMsg) {
	t.Helper()
	var b strings.Builder
	for _, m := range msgs {
		l, _ := json.Marshal(m)
		b.Write(l)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMomentsAPIAndClips(t *testing.T) {
	a, rec := testAPI(t)
	makeVideoLen(t, filepath.Join(rec, "s.mp4"), 240)
	if c, _ := call(t, a, "GET", "/api/moments?ref=s.mp4", nil); c != 404 {
		t.Fatal("no chat -> 404", c)
	}
	var msgs []ChatMsg
	for s := 0.0; s < 240; s += 4 {
		msgs = append(msgs, ChatMsg{Offset: s, Kind: "msg", Text: "hi"})
	}
	for i := 0; i < 60; i++ {
		msgs = append(msgs, ChatMsg{Offset: 120 + float64(i)*0.1, Kind: "msg", Text: "LUL LUL"})
	}
	writeJSONL(t, filepath.Join(rec, "s.chat.jsonl"), msgs)
	c, raw := call(t, a, "GET", "/api/moments?ref=s.mp4", nil)
	var res struct {
		Moments []Moment `json:"moments"`
	}
	json.Unmarshal(raw, &res)
	if c != 200 || len(res.Moments) != 1 || res.Moments[0].Start > 120 || res.Moments[0].End < 120 {
		t.Fatal(c, string(raw))
	}
	for _, bad := range []string{"", "../x", "missing.mp4"} {
		if c, _ := call(t, a, "GET", "/api/moments?ref="+bad, nil); c != 400 && c != 404 {
			t.Errorf("%q -> %d", bad, c)
		}
	}
	// clips go to a Saved folder
	os.MkdirAll(filepath.Join(a.m.cfg.SavedDir, "clips"), 0o755)
	dest := "clips"
	c, raw = call(t, a, "POST", "/api/moments/clip", map[string]any{"ref": "s.mp4", "dest": dest, "mode": "fast",
		"clips": []map[string]float64{{"start": 0, "end": 2}, {"start": 3, "end": 5}}})
	var jobs []Job
	json.Unmarshal(raw, &jobs)
	if c != 202 || len(jobs) != 2 {
		t.Fatal(c, string(raw))
	}
	for _, j := range jobs {
		if r := waitJob(t, a, j.ID); r.Status != "done" {
			t.Fatalf("%+v", r)
		}
	}
	es, _ := os.ReadDir(filepath.Join(a.m.cfg.SavedDir, "clips"))
	if len(es) != 2 || !strings.HasPrefix(es[0].Name(), "s_moment_00-00-00") && !strings.HasPrefix(es[1].Name(), "s_moment_00-00-00") {
		t.Fatalf("%v", es)
	}
	if c, _ := call(t, a, "POST", "/api/moments/clip", map[string]any{"ref": "s.mp4", "clips": []any{}}); c != 400 {
		t.Fatal(c)
	}
	if c, _ := call(t, a, "POST", "/api/moments/clip", map[string]any{"ref": "s.mp4", "clips": []map[string]float64{{"start": 5, "end": 2}}}); c != 400 {
		t.Fatal(c)
	}
}
