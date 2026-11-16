package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseIRCAndMessages(t *testing.T) {
	line := `@badges=moderator/1,subscriber/12;color=#FF0000;display-name=Cool\sGuy;emotes=25:0-4;id=abc;tmi-sent-ts=1700000005000 :coolguy!coolguy@coolguy.tmi.twitch.tv PRIVMSG #bob :Kappa hello: world` + "\r\n"
	tags, prefix, cmd, params := parseIRC(line)
	start := time.UnixMilli(1700000000000)
	m, ok := toMsg(tags, prefix, cmd, params, start)
	if !ok || m.Name != "Cool Guy" || m.User != "coolguy" || m.Text != "Kappa hello: world" || m.Color != "#FF0000" ||
		m.Offset != 5 || len(m.Badges) != 2 || m.Badges[0] != "moderator" || m.Emotes != "25:0-4" || m.Kind != "msg" {
		t.Fatalf("%+v", m)
	}
	// /me and control characters
	tags, prefix, cmd, params = parseIRC("@display-name=A :a!a@a PRIVMSG #bob :\x01ACTION waves\x01\r\n")
	if m, _ := toMsg(tags, prefix, cmd, params, start); m.Kind != "action" || m.Text != "waves" {
		t.Fatalf("%+v", m)
	}
	tags, prefix, cmd, params = parseIRC("@display-name=A :a!a@a PRIVMSG #bob :one\x07two\r\n")
	if m, _ := toMsg(tags, prefix, cmd, params, start); strings.ContainsRune(m.Text, 7) {
		t.Fatalf("control characters kept: %q", m.Text)
	}
	// sub events
	tags, prefix, cmd, params = parseIRC(`@msg-id=sub;system-msg=Neo\ssubscribed\sat\sTier\s1. :tmi.twitch.tv USERNOTICE #bob :thanks!` + "\r\n")
	if m, ok := toMsg(tags, prefix, cmd, params, start); !ok || m.Kind != "event" || !strings.Contains(m.Text, "Neo subscribed at Tier 1.") {
		t.Fatalf("%+v %v", m, ok)
	}
	// not chat
	_, _, cmd, _ = parseIRC(":tmi.twitch.tv 001 justinfan1 :Welcome, GLHF!\r\n")
	if cmd != "001" {
		t.Fatal(cmd)
	}
	for in, want := range map[string]string{"https://www.twitch.tv/Bob_1": "bob_1", "https://twitch.tv/popout/zed/chat": "zed", "https://twitch.tv/": "", "https://twitch.tv/../x y": ""} {
		if got := twitchLogin(in); got != want {
			t.Errorf("twitchLogin(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSRT(t *testing.T) {
	msgs := []ChatMsg{{Offset: 1, Name: "a", Text: "hi"}, {Offset: 2.5, Name: "b", Text: "yo"}, {Offset: 30, Name: "c", Text: "later"}}
	var b strings.Builder
	writeSRT(&b, msgs)
	out := b.String()
	if !strings.Contains(out, "1\n00:00:01,000 --> 00:00:07,000\na: hi\n\n") ||
		!strings.Contains(out, "2\n00:00:02,500 --> 00:00:08,500\na: hi\nb: yo\n\n") ||
		!strings.Contains(out, "3\n00:00:30,000 --> 00:00:36,000\nc: later\n\n") {
		t.Fatalf("%s", out)
	}
}

// fakeIRC accepts one client, replays the given lines after JOIN and keeps the line open.
func fakeIRC(t *testing.T, lines []string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(l, "JOIN ") {
						fmt.Fprint(c, "PING :tmi.twitch.tv\r\n")
						for _, x := range lines {
							fmt.Fprint(c, x+"\r\n")
						}
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

const fakeStreamlink = `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--json" ]; then echo '{"plugin":"twitch","streams":{"best":{},"720p":{}},"metadata":{"title":"T","author":"bob"}}'; exit 0; fi
done
out=""
while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done
head -c 5000 /dev/zero > "$out"
trap 'exit 0' INT TERM
sleep 30 >/dev/null 2>&1 &
wait
`

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}

func TestChatCapturedDuringRecording(t *testing.T) {
	a, rec := testAPI(t)
	sl := filepath.Join(t.TempDir(), "streamlink")
	os.WriteFile(sl, []byte(fakeStreamlink), 0o755)
	now := time.Now().UnixMilli()
	t.Setenv("STUDIO_TWITCH_IRC", fakeIRC(t, []string{
		fmt.Sprintf("@display-name=Ann;color=#00FF00;tmi-sent-ts=%d :ann!ann@ann PRIVMSG #bob :hello chat", now+1000),
		fmt.Sprintf("@display-name=Ben;badges=moderator/1;tmi-sent-ts=%d :ben!ben@ben PRIVMSG #bob :\x01ACTION cheers\x01", now+2000),
		":tmi.twitch.tv 001 x :Welcome",
		fmt.Sprintf(`@system-msg=Cy\sgifted\sa\ssub;tmi-sent-ts=%d :tmi.twitch.tv USERNOTICE #bob`, now+3000),
	}))
	s := a.m.st.Settings()
	s.StreamlinkBin = sl
	s.ChatFormats = []string{"txt", "jsonl", "srt"}
	s.RemuxAfter = false
	a.m.st.SetSettings(s)

	c, raw := call(t, a, "POST", "/api/channels", map[string]any{"url": "https://twitch.tv/bob", "chat": "on"})
	if c != 201 && c != 200 {
		t.Fatal(c, string(raw))
	}
	var ch ChannelView
	jsonOf(t, raw, &ch)
	if ch.Chat != "on" {
		t.Fatalf("%+v", ch)
	}
	if c, raw := call(t, a, "POST", "/api/channels/"+ch.ID+"/record", nil); c >= 300 {
		t.Fatal(c, string(raw))
	}
	waitFor(t, "3 messages", func() bool {
		a.m.mu.Lock()
		defer a.m.mu.Unlock()
		rt := a.m.rt[ch.ID]
		return rt != nil && rt.rec != nil && rt.rec.chat != nil && rt.rec.chat.Count() >= 3
	})
	// the channel card shows the live counter
	_, raw = call(t, a, "GET", "/api/state", nil)
	if !strings.Contains(string(raw), `"chat_messages":3`) {
		t.Fatalf("state lacks the chat counter: %s", raw)
	}
	if c, _ := call(t, a, "POST", "/api/channels/"+ch.ID+"/stop", nil); c >= 300 {
		t.Fatal(c)
	}
	waitFor(t, "recording to finish", func() bool {
		a.m.mu.Lock()
		defer a.m.mu.Unlock()
		rt := a.m.rt[ch.ID]
		return rt != nil && rt.rec == nil
	})
	if got := chatFilesIn(rec); len(got) != 3 {
		es, _ := os.ReadDir(rec)
		t.Fatalf("chat files: %v (dir: %v)", got, es)
	}

	var video string
	es, _ := os.ReadDir(rec)
	for _, e := range es {
		if isMediaFile(e.Name()) {
			video = e.Name()
		}
	}
	if video == "" {
		t.Fatal("no video")
	}
	stem := chatStem(video)
	txt, _ := os.ReadFile(filepath.Join(rec, stem+".chat.txt"))
	for _, want := range []string{"Ann: hello chat", "* Ben cheers", "* Cy gifted a sub"} {
		if !strings.Contains(string(txt), want) {
			t.Errorf("txt lacks %q:\n%s", want, txt)
		}
	}
	jl, _ := os.ReadFile(filepath.Join(rec, stem+".chat.jsonl"))
	if n := strings.Count(string(jl), "\n"); n != 3 || !strings.Contains(string(jl), `"badges":["moderator"]`) {
		t.Fatalf("jsonl: %s", jl)
	}
	srt, _ := os.ReadFile(filepath.Join(rec, stem+".chat.srt"))
	if !strings.Contains(string(srt), "Ann: hello chat") {
		t.Fatalf("srt: %s", srt)
	}

	// the listing reports the chat, the download works, and chat files follow the video
	_, raw = call(t, a, "GET", "/api/recordings", nil)
	if !strings.Contains(string(raw), `"chat":["txt","jsonl","srt"]`) {
		t.Fatalf("listing: %s", raw)
	}
	w := rawReq(a, "GET", "/api/recordings/"+video+"/chat/txt?dl=1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello chat") || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if w := rawReq(a, "GET", "/api/recordings/"+video+"/chat/exe", nil); w.Code != 400 {
		t.Fatal(w.Code)
	}

	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Best"})
	call(t, a, "POST", "/api/recordings/save", map[string]any{"names": []string{video}, "dest": "Best"})
	if len(chatFilesIn(rec)) != 0 {
		t.Fatal("chat files must move with the video")
	}
	_, raw = call(t, a, "GET", "/api/files?path=Best", nil)
	if strings.Contains(string(raw), ".chat.txt") || !strings.Contains(string(raw), `"chat":["txt","jsonl","srt"]`) {
		t.Fatalf("saved listing must hide chat files and report them: %s", raw)
	}
	if w := rawReq(a, "GET", "/api/files/chat/txt/Best/"+video, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "hello chat") {
		t.Fatalf("saved chat: %d", w.Code)
	}
	// rename + move + copy carry the chat along
	call(t, a, "POST", "/api/files/rename", map[string]string{"path": "Best/" + video, "name": "renamed.ts"})
	call(t, a, "POST", "/api/files/mkdir", map[string]string{"path": "", "name": "Other"})
	call(t, a, "POST", "/api/files/copy", map[string]any{"paths": []string{"Best/renamed.ts"}, "dest": "Other"})
	call(t, a, "POST", "/api/files/move", map[string]any{"paths": []string{"Best/renamed.ts"}, "dest": ""})
	for _, p := range []string{"renamed.chat.txt", "Other/renamed.chat.srt"} {
		if _, err := os.Stat(filepath.Join(a.m.cfg.SavedDir, p)); err != nil {
			t.Errorf("%s missing", p)
		}
	}
	// deleting the video deletes its chat
	call(t, a, "POST", "/api/files/delete", map[string]any{"paths": []string{"renamed.ts"}})
	if _, err := os.Stat(filepath.Join(a.m.cfg.SavedDir, "renamed.chat.txt")); err == nil {
		t.Fatal("orphan chat file left behind")
	}
	// and back in Recordings: delete cascades too
	os.WriteFile(filepath.Join(rec, "x.mp4"), []byte("v"), 0o644)
	os.WriteFile(filepath.Join(rec, "x.chat.txt"), []byte("c"), 0o644)
	call(t, a, "POST", "/api/recordings/delete", map[string]any{"names": []string{"x.mp4"}})
	if exists(filepath.Join(rec, "x.chat.txt")) {
		t.Fatal("chat not deleted with the recording")
	}
}

func chatFilesIn(dir string) []string {
	var out []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if isChatSidecar(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestChatNotCapturedWhenOffOrOtherPlatform(t *testing.T) {
	a, _ := testAPI(t)
	tw := Channel{URL: "https://twitch.tv/bob"}
	if l, _ := a.m.chatWanted(tw); l != "" {
		t.Fatal("must be off by default")
	}
	s := a.m.st.Settings()
	s.ChatAll = true
	a.m.st.SetSettings(s)
	if l, f := a.m.chatWanted(tw); l != "bob" || len(f) != 1 || f[0] != "txt" {
		t.Fatal(l, f)
	}
	tw.Chat = "off"
	if l, _ := a.m.chatWanted(tw); l != "" {
		t.Fatal("per-channel off must win")
	}
	if l, _ := a.m.chatWanted(Channel{URL: "https://youtube.com/@bob", Chat: "on"}); l != "" {
		t.Fatal("only Twitch is supported")
	}
	if c, _ := call(t, a, "PUT", "/api/settings", map[string]any{"chat_formats": []string{"docx"}}); c != 400 {
		t.Fatal("bad format accepted", c)
	}
	if c, _ := call(t, a, "POST", "/api/channels", map[string]any{"url": "https://twitch.tv/zed", "chat": "maybe"}); c != 400 {
		t.Fatal("bad chat mode accepted", c)
	}
}
