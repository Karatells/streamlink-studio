package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Telegram bot: long-polling client (no webhook, so it works behind NAT).
// Only the Telegram user IDs listed in the settings may talk to it; everybody
// else gets nothing (except their own ID in answer to /start, so the owner can
// find out what to enter in the allow-list).

var tgTokenRe = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{30,}$`)

type tgError struct {
	Code       int
	Desc       string
	RetryAfter int
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Desc) }

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}
type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      *tgUser `json:"from"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text"`
}
type tgCallback struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}
type tgUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *tgMessage  `json:"message"`
	Callback *tgCallback `json:"callback_query"`
}

type tgBtn struct{ T, D, U string }
type kbd [][]tgBtn

func (k kbd) markup() any {
	if len(k) == 0 {
		return nil
	}
	rows := make([][]map[string]string, 0, len(k))
	for _, r := range k {
		row := []map[string]string{}
		for _, b := range r {
			m := map[string]string{"text": b.T}
			if b.U != "" {
				m["url"] = b.U
			} else {
				m["callback_data"] = b.D
			}
			row = append(row, m)
		}
		rows = append(rows, row)
	}
	return map[string]any{"inline_keyboard": rows}
}

type pickList struct {
	names  []string
	filter string
	page   int
	at     time.Time
}

type Telegram struct {
	a        *API
	client   *http.Client
	base     string
	pollWait int // long-poll seconds
	q        chan Notification
	once     sync.Once

	mu        sync.Mutex
	username  string
	cmdLang   string
	lastOK    time.Time
	lastErr   string
	lastErrAt time.Time
	sent      uint64
	failed    uint64
	picks     map[int64]*pickList
	denied    map[int64]time.Time
}

func NewTelegram(a *API) *Telegram {
	base := os.Getenv("STUDIO_TELEGRAM_API")
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &Telegram{a: a, client: &http.Client{}, base: strings.TrimRight(base, "/"), pollWait: 25,
		q: make(chan Notification, 64), picks: map[int64]*pickList{}, denied: map[int64]time.Time{}}
}

// ------------------------------------------------------------ transport

func (t *Telegram) call(ctx context.Context, token, method string, params map[string]any, out any, timeout time.Duration) error {
	body, _ := json.Marshal(params)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, t.base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), token, "***")) // the token is part of the URL
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&r); err != nil {
		return &tgError{Code: resp.StatusCode, Desc: "unexpected response"}
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &tgError{Code: code, Desc: strings.ReplaceAll(r.Description, token, "***"), RetryAfter: r.Parameters.RetryAfter}
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (t *Telegram) record(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.lastOK, t.lastErr = time.Now(), ""
		return
	}
	t.lastErr, t.lastErrAt = err.Error(), time.Now()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// me validates a token and returns the bot's username.
func (t *Telegram) me(ctx context.Context, token string) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.call(ctx, token, "getMe", map[string]any{}, &me, 15*time.Second); err != nil {
		return "", err
	}
	return me.Username, nil
}

func (t *Telegram) setCommands(ctx context.Context, s Settings) {
	cmds := []map[string]string{}
	for _, c := range tgMenu {
		cmds = append(cmds, map[string]string{"command": c, "description": locs.Tr(s.TelegramLang, "tg_cmd_"+c)})
	}
	if err := t.call(ctx, s.TelegramToken, "setMyCommands", map[string]any{"commands": cmds}, nil, 15*time.Second); err == nil {
		t.mu.Lock()
		t.cmdLang = s.TelegramLang
		t.mu.Unlock()
	}
}

// Run polls Telegram while the bot is enabled.
func (t *Telegram) Run(ctx context.Context) {
	t.startWorker()
	var offset int64
	synced := ""
	for ctx.Err() == nil {
		s := t.a.m.st.Settings()
		if !s.TelegramOn || s.TelegramToken == "" {
			synced = ""
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		if synced != s.TelegramToken {
			name, err := t.me(ctx, s.TelegramToken)
			if err != nil {
				t.record(err)
				sleepCtx(ctx, 15*time.Second)
				continue
			}
			t.mu.Lock()
			t.username = name
			t.mu.Unlock()
			t.setCommands(ctx, s)
			// skip whatever piled up while we were offline: replaying old
			// commands (/stop, delete confirmations) would be dangerous
			var ups []tgUpdate
			if t.call(ctx, s.TelegramToken, "getUpdates", map[string]any{"offset": -1, "timeout": 0}, &ups, 15*time.Second) == nil && len(ups) > 0 {
				offset = ups[len(ups)-1].UpdateID + 1
			}
			synced = s.TelegramToken
			t.record(nil)
		}
		t.mu.Lock()
		stale := t.cmdLang != s.TelegramLang
		t.mu.Unlock()
		if stale {
			t.setCommands(ctx, s)
		}
		var ups []tgUpdate
		err := t.call(ctx, s.TelegramToken, "getUpdates", map[string]any{"offset": offset, "timeout": t.pollWait,
			"allowed_updates": []string{"message", "callback_query"}}, &ups, time.Duration(t.pollWait+15)*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			t.record(err)
			var te *tgError
			switch {
			case errors.As(err, &te) && te.Code == 401:
				synced = ""
				sleepCtx(ctx, 30*time.Second)
			case errors.As(err, &te) && te.Code == 409: // another process polls with the same token
				sleepCtx(ctx, 10*time.Second)
			default:
				sleepCtx(ctx, 5*time.Second)
			}
			continue
		}
		t.record(nil)
		for _, u := range ups { // in order, so replies keep the order of the commands
			offset = u.UpdateID + 1
			t.safeHandle(u)
		}
		if t.pollWait == 0 {
			sleepCtx(ctx, 200*time.Millisecond)
		}
	}
}

func (t *Telegram) safeHandle(u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			t.a.m.elog.Add(Channel{}, "system", "error", "telegram handler crashed", fmt.Sprint(r))
		}
	}()
	t.handle(u)
}

// ------------------------------------------------------------ sending

func (t *Telegram) token() string { return t.a.m.st.Settings().TelegramToken }

func (t *Telegram) send(chat int64, text string, k kbd) error {
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	p := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if m := k.markup(); m != nil {
		p["reply_markup"] = m
	}
	return t.callRetry("sendMessage", p)
}

func (t *Telegram) edit(chat, msg int64, text string, k kbd) {
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	p := map[string]any{"chat_id": chat, "message_id": msg, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if m := k.markup(); m != nil {
		p["reply_markup"] = m
	}
	var te *tgError
	if err := t.callRetry("editMessageText", p); err != nil && !(errors.As(err, &te) && strings.Contains(te.Desc, "not modified")) {
		_ = t.send(chat, text, k) // the old message is gone: send a fresh one
	}
}

func (t *Telegram) answer(id, text string) {
	p := map[string]any{"callback_query_id": id}
	if text != "" {
		p["text"] = text
	}
	_ = t.call(context.Background(), t.token(), "answerCallbackQuery", p, nil, 10*time.Second)
}

// callRetry retries once after a flood-wait or a transient failure.
func (t *Telegram) callRetry(method string, p map[string]any) error {
	tok := t.token()
	err := t.call(context.Background(), tok, method, p, nil, 15*time.Second)
	var te *tgError
	switch {
	case err == nil:
	case errors.As(err, &te) && te.Code == 429:
		time.Sleep(time.Duration(min(max(te.RetryAfter, 1), 30)) * time.Second)
		err = t.call(context.Background(), tok, method, p, nil, 15*time.Second)
	case errors.As(err, &te) && te.Code >= 400 && te.Code < 500:
		// permanent (blocked bot, bad request): no retry
	default:
		time.Sleep(2 * time.Second)
		err = t.call(context.Background(), tok, method, p, nil, 15*time.Second)
	}
	t.mu.Lock()
	if err == nil {
		t.sent++
	} else {
		t.failed++
	}
	t.mu.Unlock()
	if method == "sendMessage" {
		t.record(err)
	}
	return err
}

// ------------------------------------------------------------ notifications

func (t *Telegram) startWorker() { t.once.Do(func() { go t.worker() }) }

func tgTypeOn(s Settings, typ string) bool {
	if s.TelegramTypes == nil {
		return true
	}
	for _, x := range s.TelegramTypes {
		if x == typ {
			return true
		}
	}
	return false
}

// Enqueue is a Notifier sink; it never blocks.
func (t *Telegram) Enqueue(n Notification) {
	s := t.a.m.st.Settings()
	if !s.TelegramOn || s.TelegramToken == "" || len(s.TelegramUsers) == 0 || !tgTypeOn(s, n.Type) {
		return
	}
	t.startWorker()
	select {
	case t.q <- n:
	default:
	}
}

var notifEmoji = map[string]string{"stream_live": "🔴", "stream_ended": "⏹", "rec_failed": "⚠️", "auth_error": "🔑",
	"job_started": "⚙️", "job_done": "✅", "job_failed": "❌", "update_available": "⬆️"}

func (t *Telegram) worker() {
	for n := range t.q {
		s := t.a.m.st.Settings()
		if !s.TelegramOn || s.TelegramToken == "" {
			continue
		}
		title, msg := locs.Notification(s.TelegramLang, n)
		text := notifEmoji[n.Type] + " <b>" + html.EscapeString(title) + "</b>\n" + html.EscapeString(msg)
		var k kbd
		if n.Type == "stream_live" {
			if ch, ok := t.a.m.st.Channel(n.Params["channel_id"]); ok {
				var row []tgBtn
				if s.TelegramControl && n.Params["recording"] == "" {
					row = append(row, tgBtn{T: locs.Tr(s.TelegramLang, "tg_btn_record"), D: "rec:" + ch.ID})
				}
				if strings.HasPrefix(ch.URL, "http") {
					row = append(row, tgBtn{T: locs.Tr(s.TelegramLang, "tg_btn_open"), U: ch.URL})
				}
				if len(row) > 0 {
					k = kbd{row}
				}
			}
		}
		for _, uid := range s.TelegramUsers {
			_ = t.send(uid, text, k)
		}
	}
}

// ------------------------------------------------------------ status / API

func (t *Telegram) Status() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{"username": t.username, "last_error": t.lastErr, "sent": t.sent, "failed": t.failed}
	if !t.lastOK.IsZero() {
		out["last_ok"] = t.lastOK
	}
	if t.lastErr != "" {
		out["last_error_at"] = t.lastErrAt
	}
	return out
}

func (a *API) telegramStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.tg.Status())
}

// telegramTest checks the token (posted or saved) and greets every allowed user.
func (a *API) telegramTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token *string `json:"token"`
		Lang  *string `json:"lang"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s := a.m.st.Settings()
	if in.Token != nil && strings.TrimSpace(*in.Token) != "" {
		s.TelegramToken = strings.TrimSpace(*in.Token)
	}
	if in.Lang != nil {
		s.TelegramLang = *in.Lang
	}
	if !tgTokenRe.MatchString(s.TelegramToken) {
		writeErr(w, errBad("that does not look like a Telegram bot token (123456:ABC...)"))
		return
	}
	name, err := a.tg.me(r.Context(), s.TelegramToken)
	if err != nil {
		a.tg.record(err)
		writeErr(w, &httpError{502, err.Error()})
		return
	}
	a.tg.mu.Lock()
	a.tg.username = name
	a.tg.mu.Unlock()
	a.tg.record(nil)
	res := map[string]any{"ok": true, "username": name}
	sentTo, failedTo := []int64{}, []int64{}
	for _, uid := range s.TelegramUsers {
		p := map[string]any{"chat_id": uid, "parse_mode": "HTML", "text": "✅ " + html.EscapeString(locs.Tr(s.TelegramLang, "tg_test_msg"))}
		if a.tg.call(r.Context(), s.TelegramToken, "sendMessage", p, nil, 15*time.Second) == nil {
			sentTo = append(sentTo, uid)
		} else {
			failedTo = append(failedTo, uid)
		}
	}
	res["sent_to"], res["failed_to"] = sentTo, failedTo
	writeJSON(w, 200, res)
}
