package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func (f *fakeTG) last(method string) *tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			c := f.calls[i]
			return &c
		}
	}
	return nil
}

func wq(from int64, data string) tgUpdate {
	return tgUpdate{UpdateID: 3, Widget: &tgWidgetQuery{ID: "wq1", From: tgUser{ID: from}, Chat: tgChat{ID: from, Type: "private"}, MessageID: 4242, Data: data}}
}

func TestWidgetHTMLLimits(t *testing.T) {
	api, _ := setupTG(t, true, 111)
	for _, lang := range []string{"en", "uk", "ru"} {
		s := api.m.st.Settings()
		s.TelegramLang = lang
		h := api.tg.widgetHTML(s)
		var z bytes.Buffer
		w, _ := zlib.NewWriterLevel(&z, 9)
		w.Write([]byte(h))
		w.Close()
		if len(h) > 32768 || z.Len() > 4096 || strings.ContainsRune(h, 0) || strings.Contains(h, "__L__") || strings.Contains(h, "tg_w_") {
			t.Errorf("%s: html %d bytes, %d compressed", lang, len(h), z.Len())
		}
		if strings.Contains(h, "innerHTML") || strings.Contains(h, "fetch(") {
			t.Error("widget must not use innerHTML or the network")
		}
	}
}

func TestWidgetStateFitsAndIsPublicSafe(t *testing.T) {
	api, _ := setupTG(t, true, 111)
	for i := 0; i < 80; i++ {
		_ = api.m.st.PutChannel(Channel{ID: fmt.Sprintf("%012d", i), URL: fmt.Sprintf("https://twitch.tv/some_long_streamer_name_%d", i), OAuthToken: "SECRET-TOKEN", AuthMode: "global"})
	}
	data, _, _ := api.tg.widgetData(api.m.st.Settings(), "note", 123)
	if len(data) > 1024 {
		t.Fatalf("state is %d bytes", len(data))
	}
	var st widgetStateT
	if err := json.Unmarshal([]byte(data), &st); err != nil || st.V != 1 || st.X == 0 || len(st.C)+st.X != 80 {
		t.Fatalf("%v %+v", err, st)
	}
	if strings.Contains(data, "SECRET") || strings.Contains(data, "twitch.tv") {
		t.Fatal("state must contain neither tokens nor URLs")
	}
}

func TestWidgetDashboardCommand(t *testing.T) {
	api, f := setupTG(t, true, 111)
	_ = api.m.st.PutChannel(Channel{ID: "c1", URL: "https://twitch.tv/bob", AuthMode: "global"})
	api.tg.handle(msg(999, 999, "private", "/dashboard"))
	api.tg.handle(msg(111, -100123, "supergroup", "/dashboard"))
	if f.count("sendWidget") != 0 {
		t.Fatal("strangers and groups must not get a widget")
	}
	api.tg.handle(msg(111, 111, "private", "/dashboard"))
	c := f.last("sendWidget")
	if c == nil || c.P["allow_queries"] != true || !strings.Contains(c.P["caption"].(string), "Streamlink Studio") ||
		!strings.Contains(c.P["html"].(string), "TelegramWidget") || !strings.Contains(c.P["chat_data"].(string), `"bob"`) {
		t.Fatalf("%+v", c)
	}
	b, _ := os.ReadFile(api.tg.wr.path)
	if !strings.Contains(string(b), "4242") {
		t.Fatalf("not tracked: %s", b)
	}
	// a second dashboard replaces the first one
	api.tg.handle(msg(111, 111, "private", "/dashboard"))
	if api.tg.wr.at[111] != 4242 || f.count("deleteMessage") != 0 {
		t.Log("same message id from the fake server: nothing to delete")
	}
}

func TestWidgetDashboardFallsBackWhenUnsupported(t *testing.T) {
	api, f := setupTG(t, true, 111)
	api.tg.base = "http://127.0.0.1:1" // every call fails
	api.tg.handle(msg(111, 111, "private", "/dashboard"))
	_ = f
	if len(api.m.elog.List("", "warn", 10)) == 0 {
		t.Fatal("the failure must be logged")
	}
}

func TestWidgetQueries(t *testing.T) {
	api, f := setupTG(t, true, 111)
	_ = api.m.st.PutChannel(Channel{ID: "c1", URL: "https://twitch.tv/bob", AuthMode: "global"})
	tg := api.tg

	tg.handle(wq(999, "r"))
	c := f.last("answerWidgetQuery")
	if c == nil || c.P["widget_query_id"] != "wq1" || c.P["data"] != nil {
		t.Fatalf("a stranger gets an empty answer: %+v", c)
	}
	tg.handle(wq(111, "bogus:x"))
	if c = f.last("answerWidgetQuery"); c.P["data"] != nil {
		t.Fatal("unknown request")
	}
	tg.handle(wq(111, "r"))
	c = f.last("answerWidgetQuery")
	data, _ := c.P["data"].(string)
	var st widgetStateT
	if json.Unmarshal([]byte(data), &st) != nil || st.V != 1 || len(st.C) != 1 || st.M != "" || len(data) > 1024 {
		t.Fatalf("%q", data)
	}
	tg.handle(wq(111, "rec:nope"))
	c = f.last("answerWidgetQuery")
	json.Unmarshal([]byte(c.P["data"].(string)), &st)
	if st.M == "" {
		t.Fatal("an unknown channel must come back as a note")
	}
	if f.count("editMessageWidgetData") != 1 {
		t.Fatal("the stored state must be refreshed after an action")
	}

	// read-only bot: control requests are refused with a note, nothing happens
	s := api.m.st.Settings()
	s.TelegramControl = false
	api.m.st.SetSettings(s)
	tg.handle(wq(111, "rec:c1"))
	c = f.last("answerWidgetQuery")
	json.Unmarshal([]byte(c.P["data"].(string)), &st)
	if st.RO != 1 || st.M == "" || api.m.ActiveFile("") {
		t.Fatalf("%+v", st)
	}
	if api.view(Channel{ID: "c1"}).Status == StatusRecording {
		t.Fatal("must not record in read-only mode")
	}
}

func TestWidgetSyncOnChangeAndHeartbeat(t *testing.T) {
	api, f := setupTG(t, true, 111)
	tg := api.tg
	tg.handle(msg(111, 111, "private", "/dashboard"))
	s := api.m.st.Settings()
	tg.widgetsSync(s) // first push after a new widget
	if f.count("editMessageWidgetData") != 1 {
		t.Fatal(f.count("editMessageWidgetData"))
	}
	tg.widgetsSync(s)
	if f.count("editMessageWidgetData") != 1 {
		t.Fatal("unchanged state must not be pushed again")
	}
	_ = api.m.st.PutChannel(Channel{ID: "c9", URL: "https://twitch.tv/new", AuthMode: "global"})
	tg.widgetsSync(s)
	if f.count("editMessageWidgetData") != 2 {
		t.Fatal("a change must be pushed")
	}
	tg.wmu.Lock()
	tg.wr.push = time.Now().Add(-widgetHeartbeat - time.Second)
	tg.wmu.Unlock()
	tg.widgetsSync(s)
	if f.count("editMessageWidgetData") != 3 {
		t.Fatal("heartbeat")
	}
	p := f.last("editMessageWidgetData").P
	if p["chat_id"] != float64(111) || p["message_id"] != float64(4242) || !strings.Contains(p["chat_data"].(string), `"ts"`) {
		t.Fatalf("%+v", p)
	}
}
