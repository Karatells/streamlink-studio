package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const tgTestToken = "123456:ABCdefGhIJKlmnoPQRstuVWxyz0123456789"

type tgCall struct {
	Method string
	P      map[string]any
}

type fakeTG struct {
	mu      sync.Mutex
	calls   []tgCall
	backlog []tgUpdate
	batches [][]tgUpdate
	srv     *httptest.Server
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if !strings.Contains(r.URL.Path, "/bot"+tgTestToken+"/") {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
			return
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.calls = append(f.calls, tgCall{method, p})
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"username": "studio_bot"}
		case "getUpdates":
			result = []tgUpdate{}
			if off, _ := p["offset"].(float64); off == -1 {
				result = f.backlog
			} else if len(f.batches) > 0 {
				result, f.batches = f.batches[0], f.batches[1:]
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) sent() []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tgCall
	for _, c := range f.calls {
		if c.Method == "sendMessage" || c.Method == "editMessageText" {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTG) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (f *fakeTG) lastText() string {
	s := f.sent()
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1].P["text"].(string)
}

func (f *fakeTG) waitSent(t *testing.T, n int) []tgCall {
	t.Helper()
	for i := 0; i < 150; i++ {
		if s := f.sent(); len(s) >= n {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d telegram messages, got %d", n, len(f.sent()))
	return nil
}

func setupTG(t *testing.T, control bool, users ...int64) (*API, *fakeTG) {
	t.Helper()
	api, _ := testAPI(t)
	f := newFakeTG(t)
	api.tg.base = f.srv.URL
	api.tg.pollWait = 0
	s := api.m.st.Settings()
	s.TelegramOn, s.TelegramToken, s.TelegramUsers, s.TelegramControl, s.TelegramLang = true, tgTestToken, users, control, "en"
	if err := api.m.st.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	return api, f
}

func msg(from, chat int64, typ, text string) tgUpdate {
	return tgUpdate{UpdateID: 1, Message: &tgMessage{MessageID: 7, From: &tgUser{ID: from}, Chat: tgChat{ID: chat, Type: typ}, Text: text}}
}

func cbq(from int64, data string) tgUpdate {
	return tgUpdate{UpdateID: 2, Callback: &tgCallback{ID: "cb1", From: tgUser{ID: from},
		Message: &tgMessage{MessageID: 9, Chat: tgChat{ID: from, Type: "private"}}, Data: data}}
}

func TestTelegramAccessControl(t *testing.T) {
	api, f := setupTG(t, true, 111)
	tg := api.tg

	tg.handle(msg(999, 999, "private", "/status"))
	if len(f.sent()) != 0 {
		t.Fatal("a stranger must get no answer to commands")
	}
	tg.handle(msg(999, 999, "private", "/start"))
	if got := f.lastText(); !strings.Contains(got, "999") || strings.Contains(got, "Streamlink Studio v") {
		t.Fatalf("stranger should only learn their own ID: %q", got)
	}
	n := len(f.sent())
	tg.handle(msg(999, 999, "private", "/start"))
	if len(f.sent()) != n {
		t.Fatal("repeated /start must be rate limited")
	}
	tg.handle(tgUpdate{Callback: &tgCallback{ID: "x", From: tgUser{ID: 999}, Data: "chl:", Message: &tgMessage{MessageID: 1, Chat: tgChat{ID: 999, Type: "private"}}}})
	if len(f.sent()) != n {
		t.Fatal("stranger callbacks must be ignored")
	}
	// allowed user in a group chat is ignored too
	tg.handle(msg(111, -100123, "supergroup", "/status"))
	if len(f.sent()) != n {
		t.Fatal("groups must never be served")
	}
	tg.handle(msg(111, 111, "private", "/status"))
	if !strings.Contains(f.lastText(), "Streamlink Studio") {
		t.Fatalf("allowed user must get the status: %q", f.lastText())
	}
	logs := api.m.elog.List("", "warn", 10)
	if len(logs) == 0 || !strings.Contains(logs[0].Msg, "access denied") {
		t.Fatalf("denied attempts must be logged: %+v", logs)
	}
	// disabled bot answers nobody
	s := api.m.st.Settings()
	s.TelegramOn = false
	_ = api.m.st.SetSettings(s)
	n = len(f.sent())
	tg.handle(msg(111, 111, "private", "/status"))
	if len(f.sent()) != n {
		t.Fatal("disabled bot must stay silent")
	}
}

func TestTelegramControlGating(t *testing.T) {
	api, f := setupTG(t, false, 111)
	tg := api.tg
	tg.handle(msg(111, 111, "private", "/add https://twitch.tv/newbie"))
	if !strings.Contains(f.lastText(), "read-only") || len(api.m.st.Channels()) != 0 {
		t.Fatalf("read-only mode must refuse /add: %q", f.lastText())
	}
	ch := Channel{ID: "c1", URL: "https://twitch.tv/bob", AuthMode: "global", AutoRecord: true}
	_ = api.m.st.PutChannel(ch)
	tg.handle(cbq(111, "rec:c1"))
	found := false
	for _, c := range f.calls {
		if c.Method == "answerCallbackQuery" && strings.Contains(c.P["text"].(string), "read-only") {
			found = true
		}
	}
	if !found {
		t.Fatal("record button must be refused in read-only mode")
	}

	s := api.m.st.Settings()
	s.TelegramControl = true
	_ = api.m.st.SetSettings(s)
	tg.handle(msg(111, 111, "private", "/add https://twitch.tv/newbie"))
	if len(api.m.st.Channels()) != 2 || !strings.Contains(f.lastText(), "newbie") {
		t.Fatalf("/add should create the channel: %q", f.lastText())
	}
	tg.handle(msg(111, 111, "private", "/add javascript:alert(1)"))
	if len(api.m.st.Channels()) != 2 {
		t.Fatal("invalid URLs must be rejected")
	}
}

func TestTelegramNotificationsFilteredAndLocalized(t *testing.T) {
	api, f := setupTG(t, true, 111, 222)
	s := api.m.st.Settings()
	s.TelegramTypes, s.TelegramLang = []string{"stream_live"}, "ru"
	_ = api.m.st.SetSettings(s)
	ch := Channel{ID: "c1", URL: "https://twitch.tv/bob", AuthMode: "global"}
	_ = api.m.st.PutChannel(ch)

	api.m.notes.Notify("rec_failed", "twitch", map[string]string{"channel": "bob", "error": "x"}, "")
	api.m.notes.Notify("stream_live", "twitch", map[string]string{"channel": "bob", "channel_id": "c1", "title": "Hi"}, "")
	sent := f.waitSent(t, 2)
	time.Sleep(150 * time.Millisecond)
	sent = f.sent()
	if len(sent) != 2 {
		t.Fatalf("want one message per allowed user for the one selected type, got %d", len(sent))
	}
	ids := map[float64]bool{}
	for _, c := range sent {
		ids[c.P["chat_id"].(float64)] = true
	}
	if !ids[111] || !ids[222] {
		t.Fatalf("both users must be notified: %v", ids)
	}
	txt := sent[0].P["text"].(string)
	if !strings.Contains(txt, "bob") || !strings.Contains(txt, "начал") {
		t.Fatalf("expected a Russian message: %q", txt)
	}
	kb, _ := json.Marshal(sent[0].P["reply_markup"])
	if !strings.Contains(string(kb), "rec:c1") || !strings.Contains(string(kb), "https://twitch.tv/bob") {
		t.Fatalf("live notification needs record + open buttons: %s", kb)
	}
	// no users configured -> nothing is queued
	s.TelegramUsers = nil
	_ = api.m.st.SetSettings(s)
	api.m.notes.Notify("stream_live", "twitch", map[string]string{"channel": "al"}, "")
	time.Sleep(150 * time.Millisecond)
	if len(f.sent()) != 2 {
		t.Fatal("without allowed users nothing may be sent")
	}
}

func TestTelegramRecordingsFlow(t *testing.T) {
	api, f := setupTG(t, true, 111)
	tg := api.tg
	rec := api.m.cfg.RecDir
	for _, n := range []string{"bob_a.mp4", "bob_b.mp4", "al_c.mp4"} {
		mkVideo(t, filepath.Join(rec, n), "red")
	}
	tg.handle(msg(111, 111, "private", "/recordings"))
	if !strings.Contains(f.lastText(), "3") {
		t.Fatalf("list should mention 3 files: %q", f.lastText())
	}
	tg.handle(msg(111, 111, "private", "/recordings zzz"))
	if !strings.Contains(strings.ToLower(f.lastText()), "no") {
		t.Fatalf("empty filter result: %q", f.lastText())
	}
	tg.handle(msg(111, 111, "private", "/recordings"))
	name, ok := tg.pick(111, "0")
	if !ok {
		t.Fatal("pick list missing")
	}
	tg.handle(cbq(111, "fs:0"))
	if _, err := os.Stat(filepath.Join(api.m.cfg.SavedDir, name)); err != nil {
		t.Fatal("file not moved to Saved")
	}
	tg.handle(msg(111, 111, "private", "/recordings"))
	if name, ok = tg.pick(111, "0"); !ok {
		t.Fatal("pick list missing after save")
	}
	tg.handle(cbq(111, "fd:0"))
	if !strings.Contains(f.lastText(), name) {
		t.Fatalf("delete must ask for confirmation first: %q", f.lastText())
	}
	if _, err := os.Stat(filepath.Join(rec, name)); err != nil {
		t.Fatal("file must survive until confirmed")
	}
	tg.handle(cbq(111, "fy:0"))
	if _, err := os.Stat(filepath.Join(rec, name)); err == nil {
		t.Fatal("confirmed delete did not remove the file")
	}
	tg.handle(cbq(111, "fy:99")) // stale / out of range index must not delete anything
	if len(api.listRecordings()) != 1 {
		t.Fatal("out-of-range index changed state")
	}
}

var leftoverTag = regexp.MustCompile(`<[^>]*>`)

func TestTelegramViewsAreComplete(t *testing.T) {
	for _, lang := range []string{"en", "ru", "uk"} {
		api, f := setupTG(t, true, 111)
		s := api.m.st.Settings()
		s.TelegramLang, s.TelegramTypes = lang, []string{} // keep async notifications out of the reply count
		_ = api.m.st.SetSettings(s)
		api.m.cnt.Inc("streamlink_studio_checks_total", "result", "live")
		_ = api.m.st.PutChannel(Channel{ID: "c1", URL: "https://twitch.tv/bob<b>", AuthMode: "global", AutoRecord: true})
		mkVideo(t, filepath.Join(api.m.cfg.RecDir, "bob_a.mp4"), "red")
		api.m.notes.Notify("stream_live", "twitch", map[string]string{"channel": "bob"}, "")
		api.m.elog.Add(Channel{ID: "c1", URL: "https://twitch.tv/bob"}, "check", "warn", "a <script> & b", "")
		api.m.lastTick.Store(time.Now().UnixNano())
		for _, cmd := range []string{"/help", "/status", "/channels", "/recording", "/recordings", "/stats", "/jobs", "/log all", "/notifications", "/health", "/notify", "/add", "/whoami", "/bogus"} {
			n := len(f.sent())
			api.tg.handle(msg(111, 111, "private", cmd))
			if len(f.sent()) != n+1 {
				t.Fatalf("[%s] %s: no reply", lang, cmd)
			}
			txt := f.lastText()
			plain := leftoverTag.ReplaceAllString(strings.NewReplacer("<b>", "", "</b>", "", "<i>", "", "</i>", "").Replace(txt), "")
			if strings.Contains(plain, "tg_") || strings.Contains(plain, "hc_") || strings.Contains(plain, "{") || strings.Contains(plain, "ntf_") || strings.Contains(plain, "job_") {
				t.Fatalf("[%s] %s has an untranslated key or placeholder:\n%s", lang, cmd, txt)
			}
			if strings.Contains(strings.NewReplacer("<b>", "", "</b>", "", "<i>", "", "</i>", "").Replace(txt), "<script>") {
				t.Fatalf("[%s] %s: HTML not escaped:\n%s", lang, cmd, txt)
			}
		}
		for _, cb := range []string{"ch:c1", "chl:", "r:status", "rp:0", "nt:stream_live", "nta:", "ntn:"} {
			n := len(f.sent())
			api.tg.handle(cbq(111, cb))
			if len(f.sent()) != n+1 {
				t.Fatalf("[%s] callback %s: no edit", lang, cb)
			}
			plain := leftoverTag.ReplaceAllString(strings.NewReplacer("<b>", "", "</b>", "", "<i>", "", "</i>", "").Replace(f.lastText()), "")
			if strings.Contains(plain, "tg_") || strings.Contains(plain, "{") {
				t.Fatalf("[%s] callback %s untranslated:\n%s", lang, cb, f.lastText())
			}
		}
	}
}

func TestTelegramNotifySelectionPersists(t *testing.T) {
	api, _ := setupTG(t, true, 111)
	api.tg.handle(cbq(111, "ntn:"))
	if got := api.m.st.Settings().TelegramTypes; got == nil || len(got) != 0 {
		t.Fatalf("none must persist as an empty list: %#v", got)
	}
	api.tg.handle(cbq(111, "nt:job_failed"))
	if got := api.m.st.Settings().TelegramTypes; len(got) != 1 || got[0] != "job_failed" {
		t.Fatalf("toggle failed: %v", got)
	}
	if !api.m.st.Settings().TelegramOn || api.m.st.Settings().TelegramToken == "" {
		t.Fatal("toggling must not clobber other settings")
	}
}

func TestTelegramRunLoopSkipsBacklog(t *testing.T) {
	api, f := setupTG(t, true, 111)
	f.backlog = []tgUpdate{{UpdateID: 10, Message: msg(111, 111, "private", "/add https://twitch.tv/old").Message}}
	f.batches = [][]tgUpdate{{{UpdateID: 11, Message: msg(111, 111, "private", "/status").Message}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { api.tg.Run(ctx); close(done) }()
	f.waitSent(t, 1)
	cancel()
	<-done
	if len(api.m.st.Channels()) != 0 {
		t.Fatal("commands queued while offline must not be replayed")
	}
	if f.count("getMe") == 0 || f.count("setMyCommands") == 0 {
		t.Fatal("bot should verify the token and publish its command menu")
	}
	if st := api.tg.Status(); st["username"] != "studio_bot" {
		t.Fatalf("status: %v", st)
	}
	var offs []float64
	for _, c := range f.calls {
		if c.Method == "getUpdates" {
			offs = append(offs, c.P["offset"].(float64))
		}
	}
	if len(offs) < 2 || offs[0] != -1 || offs[1] != 11 {
		t.Fatalf("offset handling wrong: %v", offs)
	}
}

func TestTelegramSettingsAPIAndSecrecy(t *testing.T) {
	api, _ := testAPI(t)
	f := newFakeTG(t)
	api.tg.base = f.srv.URL
	for _, body := range []map[string]any{
		{"telegram_token": "garbage"}, {"telegram_users": []int64{-5}}, {"telegram_on": true},
		{"telegram_types": []string{"bogus"}}, {"telegram_lang": "../x"},
	} {
		if c, _ := call(t, api, "PUT", "/api/settings", body); c != 400 {
			t.Fatalf("%v: want 400, got %d", body, c)
		}
	}
	c, raw := call(t, api, "PUT", "/api/settings", map[string]any{"telegram_token": tgTestToken, "telegram_users": []int64{5, 5, 7}, "telegram_control": true, "telegram_lang": "uk", "telegram_on": true})
	if c != 200 || strings.Contains(string(raw), tgTestToken) || !strings.Contains(string(raw), `"has_telegram_token":true`) || !strings.Contains(string(raw), `"telegram_users":[5,7]`) {
		t.Fatalf("put: %d %s", c, raw)
	}
	_, raw = call(t, api, "GET", "/api/settings", nil)
	if strings.Contains(string(raw), tgTestToken) {
		t.Fatal("token leaked in settings view")
	}
	c, raw = call(t, api, "POST", "/api/telegram/test", map[string]any{})
	if c != 200 || !strings.Contains(string(raw), "studio_bot") || !strings.Contains(string(raw), "[5,7]") {
		t.Fatalf("test: %d %s", c, raw)
	}
	bad := "999999:WRONGtokenWRONGtokenWRONGtokenWRONG1"
	c, raw = call(t, api, "POST", "/api/telegram/test", map[string]any{"token": bad})
	if c != 502 || strings.Contains(string(raw), bad) {
		t.Fatalf("wrong token: %d %s", c, raw)
	}
	_, raw = call(t, api, "GET", "/api/telegram", nil)
	if strings.Contains(string(raw), bad) || strings.Contains(string(raw), tgTestToken) {
		t.Fatalf("status leaks a token: %s", raw)
	}
	if c, _ := call(t, api, "PUT", "/api/settings", map[string]any{"telegram_token": "", "telegram_on": false}); c != 200 {
		t.Fatal("clearing the token must work")
	}
}
