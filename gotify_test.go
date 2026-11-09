package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gotifyMsg struct {
	Key, Title, Message string
	Priority            int
}

func fakeGotify(t *testing.T, status int) (*httptest.Server, func() []gotifyMsg) {
	t.Helper()
	var mu sync.Mutex
	var got []gotifyMsg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in struct {
			Title, Message string
			Priority       int
		}
		_ = json.Unmarshal(b, &in)
		if r.URL.Path != "/message" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if status != 200 {
			http.Error(w, "nope", status)
			return
		}
		mu.Lock()
		got = append(got, gotifyMsg{r.Header.Get("X-Gotify-Key"), in.Title, in.Message, in.Priority})
		mu.Unlock()
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []gotifyMsg { mu.Lock(); defer mu.Unlock(); return append([]gotifyMsg{}, got...) }
}

func waitMsgs(t *testing.T, get func() []gotifyMsg, n int) []gotifyMsg {
	t.Helper()
	for i := 0; i < 100; i++ {
		if g := get(); len(g) >= n {
			return g
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d gotify messages, got %d", n, len(get()))
	return nil
}

func TestGotifyFilterLangMuteAndPriority(t *testing.T) {
	api, _ := testAPI(t)
	srv, get := fakeGotify(t, 200)
	if c, _ := call(t, api, "PUT", "/api/settings", map[string]any{
		"gotify_on": true, "gotify_url": srv.URL + "/", "gotify_token": "tok123", "gotify_lang": "ru",
		"gotify_types": []string{"stream_live", "rec_failed"}, "notify_off": []string{"stream_live"}}); c != 200 {
		t.Fatalf("put settings: %d", c)
	}
	m := api.m
	m.notes.Notify("stream_ended", "twitch", map[string]string{"channel": "bob"}, "") // type not selected
	m.notes.Notify("stream_live", "twitch", map[string]string{"channel": "bob"}, "")  // muted in UI, still sent
	m.notes.Notify("rec_failed", "kick", map[string]string{"channel": "al", "error": "boom"}, "")
	msgs := waitMsgs(t, get, 2)
	time.Sleep(100 * time.Millisecond)
	if msgs = get(); len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %+v", msgs)
	}
	if msgs[0].Key != "tok123" || msgs[0].Title != "Стример начал трансляцию" || !strings.Contains(msgs[0].Message, "bob") || msgs[0].Priority != 4 {
		t.Fatalf("live msg wrong: %+v", msgs[0])
	}
	if !strings.Contains(msgs[1].Message, "boom") || msgs[1].Priority != 8 {
		t.Fatalf("fail msg wrong: %+v", msgs[1])
	}
	items, _ := m.notes.List()
	for _, it := range items {
		if it.Type == "stream_live" {
			t.Fatal("muted type must not appear in the in-app list")
		}
	}
}

func TestGotifyValidationAndSecrecy(t *testing.T) {
	api, _ := testAPI(t)
	for _, body := range []map[string]any{
		{"gotify_url": "ftp://x"}, {"gotify_url": "http://"}, {"gotify_types": []string{"bogus"}},
		{"gotify_lang": "../x"}, {"gotify_on": true}, // no url/token
	} {
		if c, _ := call(t, api, "PUT", "/api/settings", body); c != 400 {
			t.Fatalf("%v: want 400, got %d", body, c)
		}
	}
	call(t, api, "PUT", "/api/settings", map[string]any{"gotify_url": "http://h:8080/", "gotify_token": "sec"})
	_, raw := call(t, api, "GET", "/api/settings", nil)
	if strings.Contains(string(raw), "sec\"") || !strings.Contains(string(raw), `"has_gotify_token":true`) || !strings.Contains(string(raw), "http://h:8080\"") {
		t.Fatalf("settings view leaks or misses fields: %s", raw)
	}
	var v SettingsView
	_ = json.Unmarshal(raw, &v)
	if len(v.GotifyTypes) != len(notifyTypes) {
		t.Fatalf("default should be all types, got %v", v.GotifyTypes)
	}
	call(t, api, "PUT", "/api/settings", map[string]any{"gotify_types": []string{}})
	_, raw = call(t, api, "GET", "/api/settings", nil)
	_ = json.Unmarshal(raw, &v)
	if len(v.GotifyTypes) != 0 {
		t.Fatalf("empty selection must stay empty: %v", v.GotifyTypes)
	}
}

func TestGotifyTestEndpointAndRetry(t *testing.T) {
	api, _ := testAPI(t)
	srv, get := fakeGotify(t, 200)
	if c, _ := call(t, api, "POST", "/api/gotify/test", map[string]any{"url": srv.URL, "token": "k", "lang": "uk"}); c != 200 {
		t.Fatalf("test endpoint: %d", c)
	}
	if g := get(); len(g) != 1 || g[0].Key != "k" || g[0].Message == "gotify_test_msg" {
		t.Fatalf("bad test message: %+v", g)
	}
	bad, _ := fakeGotify(t, 401)
	c, raw := call(t, api, "POST", "/api/gotify/test", map[string]any{"url": bad.URL, "token": "zzz"})
	if c != 502 || strings.Contains(string(raw), "zzz") {
		t.Fatalf("want 502 w/o token, got %d %s", c, raw)
	}
	_, raw = call(t, api, "GET", "/api/gotify", nil)
	if !strings.Contains(string(raw), "401") {
		t.Fatalf("status should report error: %s", raw)
	}
	if c, _ := call(t, api, "POST", "/api/gotify/test", map[string]any{"url": "nonsense", "token": "k"}); c != 400 {
		t.Fatalf("bad url: %d", c)
	}

	// transient 5xx is retried once, a 4xx is not
	var hits atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "x", 503)
		}
	}))
	defer flaky.Close()
	g := NewGotify(func() Settings { return Settings{GotifyOn: true, GotifyURL: flaky.URL, GotifyToken: "k"} })
	g.retryGap = 10 * time.Millisecond
	g.Enqueue(Notification{Type: "job_done", Level: "success", Params: map[string]string{"kind": "merge", "output": "a.mp4"}})
	for i := 0; i < 100 && hits.Load() < 2; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected one retry, hits=%d", hits.Load())
	}
}
