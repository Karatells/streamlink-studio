package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Chat capture: while a Twitch stream is being recorded, its chat is read over
// the public IRC interface (anonymous, read-only) and written next to the video:
//
//	<name>.chat.txt    readable log with times relative to the recording start
//	<name>.chat.jsonl  every message with all details (badges, colour, emotes)
//	<name>.chat.srt    subtitles, so players show the chat over the video
//
// Other platforms have no stable public chat interface, so they are not captured.

var chatFormats = []string{"txt", "jsonl", "srt"}

const (
	maxChatMem  = 300000 // messages kept for the .srt
	srtShowFor  = 6 * time.Second
	srtMaxLines = 4
)

var twitchLoginRe = regexp.MustCompile(`^[a-z0-9_]{1,25}$`)

func validChatFormat(f string) bool {
	for _, x := range chatFormats {
		if x == f {
			return true
		}
	}
	return false
}

// twitchLogin extracts the channel login from a twitch.tv URL.
func twitchLogin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) == 0 {
		return ""
	}
	l := strings.ToLower(seg[0])
	if l == "popout" && len(seg) > 1 {
		l = strings.ToLower(seg[1])
	}
	if !twitchLoginRe.MatchString(l) {
		return ""
	}
	return l
}

// chatWanted decides whether a recording of this channel captures chat.
func (m *Manager) chatWanted(ch Channel) (login string, formats []string) {
	plugin := ch.Plugin
	if plugin == "" {
		plugin = detectPlatform(ch.URL)
	}
	if plugin != "twitch" {
		return "", nil
	}
	s := m.st.Settings()
	switch ch.Chat {
	case "off":
		return "", nil
	case "on":
	default:
		if !s.ChatAll {
			return "", nil
		}
	}
	formats = s.ChatFormats
	if formats == nil {
		formats = []string{"txt"}
	}
	if len(formats) == 0 {
		return "", nil
	}
	return twitchLogin(ch.URL), formats
}

// chatStem is the file name without its extension; sidecars are "<stem>.chat.<fmt>".
func chatStem(name string) string { return strings.TrimSuffix(name, filepath.Ext(name)) }

func chatName(stem, format string) string { return stem + ".chat." + format }

func isChatSidecar(name string) bool {
	for _, f := range chatFormats {
		if strings.HasSuffix(name, ".chat."+f) {
			return true
		}
	}
	return false
}

// chatFiles lists the formats of the chat files that exist for a video.
func chatFiles(dir, videoName string) []string {
	stem := chatStem(videoName)
	var out []string
	for _, f := range chatFormats {
		if fi, err := os.Stat(filepath.Join(dir, chatName(stem, f))); err == nil && fi.Mode().IsRegular() {
			out = append(out, f)
		}
	}
	return out
}

// hasSiblingVideo reports whether another media file with the same stem is in dir.
func hasSiblingVideo(dir, videoName string) bool {
	stem := chatStem(videoName)
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if e.Name() != videoName && isMediaFile(e.Name()) && chatStem(e.Name()) == stem {
			return true
		}
	}
	return false
}

// moveChat moves (or copies) a video's chat files next to its new location.
func moveChat(srcDir, srcName, dstDir, dstName string, cp bool) {
	if !isMediaFile(srcName) {
		return
	}
	for _, f := range chatFiles(srcDir, srcName) {
		src := filepath.Join(srcDir, chatName(chatStem(srcName), f))
		dst := filepath.Join(dstDir, chatName(chatStem(dstName), f))
		if src == dst || exists(dst) {
			continue
		}
		if cp {
			_ = copyRegular(src, dst)
		} else {
			_ = moveEntry(src, dst)
		}
	}
}

// removeChat deletes a video's chat files unless another video still uses them.
func removeChat(dir, videoName string) {
	if !isMediaFile(videoName) || hasSiblingVideo(dir, videoName) {
		return
	}
	for _, f := range chatFiles(dir, videoName) {
		_ = os.Remove(filepath.Join(dir, chatName(chatStem(videoName), f)))
	}
}

// ------------------------------------------------------------------ messages

type ChatMsg struct {
	T      time.Time `json:"t"`
	Offset float64   `json:"offset"` // seconds since the recording started
	Kind   string    `json:"kind"`   // msg | action | event
	User   string    `json:"user,omitempty"`
	Name   string    `json:"name,omitempty"`
	Color  string    `json:"color,omitempty"`
	Badges []string  `json:"badges,omitempty"`
	Text   string    `json:"text"`
	Emotes string    `json:"emotes,omitempty"`
	ID     string    `json:"id,omitempty"`
}

func ircUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 's':
			b.WriteByte(' ')
		case ':':
			b.WriteByte(';')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func cleanChatText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
}

// parseIRC splits one IRC line into tags, command and parameters.
func parseIRC(line string) (tags map[string]string, prefix, cmd string, params []string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(line, "@") {
		t, rest, _ := strings.Cut(line[1:], " ")
		tags = map[string]string{}
		for _, kv := range strings.Split(t, ";") {
			k, v, _ := strings.Cut(kv, "=")
			tags[k] = ircUnescape(v)
		}
		line = strings.TrimLeft(rest, " ")
	}
	if strings.HasPrefix(line, ":") {
		p, rest, _ := strings.Cut(line[1:], " ")
		prefix = p
		line = strings.TrimLeft(rest, " ")
	}
	for line != "" {
		if strings.HasPrefix(line, ":") {
			params = append(params, line[1:])
			break
		}
		p, rest, _ := strings.Cut(line, " ")
		if cmd == "" {
			cmd = p
		} else {
			params = append(params, p)
		}
		line = strings.TrimLeft(rest, " ")
	}
	return
}

// toMsg converts a PRIVMSG / USERNOTICE into a ChatMsg (ok=false: not a chat line).
func toMsg(tags map[string]string, prefix, cmd string, params []string, start time.Time) (ChatMsg, bool) {
	at := time.Now()
	if ms, err := strconv.ParseInt(tags["tmi-sent-ts"], 10, 64); err == nil && ms > 0 {
		at = time.UnixMilli(ms)
	}
	msg := ChatMsg{T: at, ID: tags["id"], Color: tags["color"], Emotes: tags["emotes"]}
	if off := at.Sub(start).Seconds(); off > 0 {
		msg.Offset = off
	}
	if b := tags["badges"]; b != "" {
		for _, x := range strings.Split(b, ",") {
			name, _, _ := strings.Cut(x, "/")
			msg.Badges = append(msg.Badges, name)
		}
	}
	nick, _, _ := strings.Cut(prefix, "!")
	msg.User = nick
	msg.Name = tags["display-name"]
	if msg.Name == "" {
		msg.Name = nick
	}
	text := ""
	if len(params) >= 2 {
		text = params[1]
	}
	switch cmd {
	case "PRIVMSG":
		msg.Kind = "msg"
		if strings.HasPrefix(text, "\x01ACTION ") && strings.HasSuffix(text, "\x01") {
			msg.Kind, text = "action", strings.TrimSuffix(strings.TrimPrefix(text, "\x01ACTION "), "\x01")
		}
		msg.Text = cleanChatText(text)
		return msg, msg.Text != ""
	case "USERNOTICE": // subs, gifts, raids, ...
		msg.Kind = "event"
		sys := cleanChatText(tags["system-msg"])
		if sys == "" {
			return msg, false
		}
		msg.Text = sys
		if text != "" {
			msg.Text += " — " + cleanChatText(text)
		}
		return msg, true
	}
	return msg, false
}

func fmtOffset(sec float64) string {
	s := int(sec)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

func (c ChatMsg) txtLine() string {
	switch c.Kind {
	case "event":
		return fmt.Sprintf("[%s] * %s\n", fmtOffset(c.Offset), c.Text)
	case "action":
		return fmt.Sprintf("[%s] * %s %s\n", fmtOffset(c.Offset), c.Name, c.Text)
	}
	return fmt.Sprintf("[%s] %s: %s\n", fmtOffset(c.Offset), c.Name, c.Text)
}

func srtTime(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// writeSRT turns messages into subtitles: each shows for a few seconds, and
// messages arriving close together are stacked (newest at the bottom).
func writeSRT(w io.Writer, msgs []ChatMsg) error {
	n := 0
	for i, m := range msgs {
		start := time.Duration(m.Offset * float64(time.Second))
		end := start + srtShowFor
		var lines []string
		for j := i; j >= 0 && len(lines) < srtMaxLines; j-- { // earlier messages still on screen
			pj := time.Duration(msgs[j].Offset * float64(time.Second))
			if start-pj >= srtShowFor {
				break
			}
			l := msgs[j].Name + ": " + msgs[j].Text
			if msgs[j].Kind == "event" {
				l = "★ " + msgs[j].Text
			}
			lines = append([]string{l}, lines...)
		}
		n++
		if _, err := fmt.Fprintf(w, "%d\n%s --> %s\n%s\n\n", n, srtTime(start), srtTime(end), strings.Join(lines, "\n")); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ recorder

type chatRecorder struct {
	m       *Manager
	login   string
	start   time.Time
	formats map[string]bool
	dir     string
	stem    string

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	txt    *os.File
	jsonl  *bufio.Writer
	jf     *os.File
	msgs   []ChatMsg // for the .srt
	count  atomic.Int64
	lastAt atomic.Int64
	err    string
}

func (m *Manager) ircAddr() (addr string, useTLS bool) {
	if v := os.Getenv("STUDIO_TWITCH_IRC"); v != "" { // tests / self-hosted relay: host:port, plain text
		return v, false
	}
	return "irc.chat.twitch.tv:6697", true
}

func (m *Manager) startChat(ch Channel, rec *Rec) *chatRecorder {
	login, formats := m.chatWanted(ch)
	if login == "" {
		return nil
	}
	cr := &chatRecorder{m: m, login: login, start: rec.Started, formats: map[string]bool{}, dir: m.cfg.RecDir,
		stem: chatStem(rec.Final), done: make(chan struct{})}
	for _, f := range formats {
		if validChatFormat(f) {
			cr.formats[f] = true
		}
	}
	var err error
	if cr.formats["txt"] {
		if cr.txt, err = os.OpenFile(filepath.Join(cr.dir, chatName(cr.stem, "txt")), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640); err == nil {
			fmt.Fprintf(cr.txt, "# Chat of %s (twitch.tv/%s), recording started %s\n# Times are relative to the start of the recording.\n\n",
				streamerName(ch), login, rec.Started.Format(time.RFC3339))
		}
	}
	if cr.formats["jsonl"] {
		if cr.jf, err = os.OpenFile(filepath.Join(cr.dir, chatName(cr.stem, "jsonl")), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640); err == nil {
			cr.jsonl = bufio.NewWriter(cr.jf)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cr.cancel = cancel
	go func() {
		defer close(cr.done)
		cr.run(ctx)
	}()
	m.elog.Add(ch, "chat", "info", "chat capture started: #"+login, "")
	return cr
}

func (cr *chatRecorder) add(msg ChatMsg) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.txt != nil {
		_, _ = cr.txt.WriteString(msg.txtLine())
	}
	if cr.jsonl != nil {
		if b, err := json.Marshal(msg); err == nil {
			cr.jsonl.Write(b)
			cr.jsonl.WriteByte('\n')
		}
	}
	if cr.formats["srt"] && len(cr.msgs) < maxChatMem {
		cr.msgs = append(cr.msgs, msg)
	}
	cr.count.Add(1)
	cr.lastAt.Store(time.Now().Unix())
	cr.m.cnt.Inc("streamlink_studio_chat_messages_total")
}

func (cr *chatRecorder) flush() {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.jsonl != nil {
		_ = cr.jsonl.Flush()
	}
}

func (cr *chatRecorder) run(ctx context.Context) {
	backoff := 2 * time.Second
	tick := time.NewTicker(5 * time.Second) // keep the jsonl reasonably fresh on disk
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				cr.flush()
			}
		}
	}()
	for ctx.Err() == nil {
		began := time.Now()
		err := cr.session(ctx)
		if ctx.Err() != nil {
			return
		}
		cr.mu.Lock()
		if err != nil {
			cr.err = err.Error()
		}
		cr.mu.Unlock()
		if time.Since(began) > time.Minute {
			backoff = 2 * time.Second
		}
		sleepCtx(ctx, backoff+time.Duration(rand.Intn(1000))*time.Millisecond)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session holds one IRC connection until it breaks.
func (cr *chatRecorder) session(ctx context.Context) error {
	addr, useTLS := cr.m.ircAddr()
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if useTLS {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()
	send := func(f string, a ...any) {
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		fmt.Fprintf(conn, f+"\r\n", a...)
	}
	send("CAP REQ :twitch.tv/tags twitch.tv/commands")
	send("PASS SCHMOOPIIE") // anonymous, read-only
	send("NICK justinfan%d", 10000+rand.Intn(80000))
	send("JOIN #%s", cr.login)
	r := bufio.NewReaderSize(conn, 16*1024)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute)) // Twitch pings every few minutes
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if len(line) > 8192 {
			continue
		}
		tags, prefix, cmd, params := parseIRC(line)
		switch cmd {
		case "PING":
			send("PONG :%s", strings.Join(params, " "))
		case "RECONNECT":
			return fmt.Errorf("server asked to reconnect")
		case "PRIVMSG", "USERNOTICE":
			if msg, ok := toMsg(tags, prefix, cmd, params, cr.start); ok {
				cr.add(msg)
			}
		}
	}
}

// Count is the number of messages saved so far.
func (cr *chatRecorder) Count() int64 {
	if cr == nil {
		return 0
	}
	return cr.count.Load()
}

// stop ends the capture and writes the final files. keep=false deletes them.
func (cr *chatRecorder) stop(ch Channel, keep bool) {
	cr.cancel()
	<-cr.done
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.jsonl != nil {
		_ = cr.jsonl.Flush()
	}
	if cr.txt != nil {
		cr.txt.Close()
	}
	if cr.jf != nil {
		cr.jf.Close()
	}
	n := cr.count.Load()
	if cr.formats["srt"] && keep && n > 0 {
		if f, err := os.Create(filepath.Join(cr.dir, chatName(cr.stem, "srt"))); err == nil {
			bw := bufio.NewWriter(f)
			_ = writeSRT(bw, cr.msgs)
			_ = bw.Flush()
			f.Close()
		}
	}
	if !keep || n == 0 { // nothing worth keeping
		for _, f := range chatFormats {
			_ = os.Remove(filepath.Join(cr.dir, chatName(cr.stem, f)))
		}
	}
	if keep && n > 0 {
		cr.m.elog.Add(ch, "chat", "info", fmt.Sprintf("chat saved: %d messages", n), "")
	} else if keep && cr.err != "" {
		cr.m.elog.Add(ch, "chat", "warn", "no chat messages were captured", cr.err)
	}
}
