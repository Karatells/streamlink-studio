package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Telegram widgets (https://core.telegram.org/bots/widgets): /dashboard sends a
// small live panel into the private chat - channel states with Record / Stop
// buttons, disk space and jobs. The HTML is fixed; the bot keeps the widget's
// chat state up to date with editMessageWidgetData and answers its requests
// (widget_query). Everything in a widget is visible to Telegram, so no secrets
// ever go into the HTML or the state - only names, states and counters.

const (
	widgetStateMax  = 900 // bytes; Telegram allows 1024 for the state and for an answer
	widgetHeartbeat = 5 * time.Minute
	widgetTick      = 5 * time.Second
	widgetMaxTrack  = 20
)

// widgetState is the compact chat_data of a dashboard widget:
// c rows are [id, name, state(0 off,1 live,2 recording,3 problem), recording-start unix],
// d is [free GB, total GB], j is [running, queued], x counts channels that did not fit.
type widgetStateT struct {
	V  int        `json:"v"`
	TS int64      `json:"ts,omitempty"`
	C  [][]any    `json:"c"`
	X  int        `json:"x,omitempty"`
	D  [2]float64 `json:"d"`
	J  [2]int     `json:"j"`
	RO int        `json:"ro,omitempty"`
	NQ int        `json:"nq,omitempty"` // the widget cannot send requests: no buttons
	M  string     `json:"m,omitempty"`  // one-off note for the widget that asked
}

func jsonCompact(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// widgetData returns the state; ts and note are left out when zero/empty (change detection).
func (t *Telegram) widgetData(s Settings, note string, ts int64, noQ bool) (string, int, int) {
	a := t.a
	type row struct {
		id, name string
		st       int
		since    int64
	}
	var rows []row
	live, rec := 0, 0
	for _, ch := range a.m.st.Channels() {
		v := a.view(ch)
		r := row{id: ch.ID, name: clipRunes(v.DisplayName, 14)}
		switch v.Status {
		case StatusRecording:
			r.st, live, rec = 2, live+1, rec+1
			if v.RecordingSince != nil {
				r.since = v.RecordingSince.Unix()
			}
		case StatusLive:
			r.st, live = 1, live+1
		case StatusAuthError, StatusError:
			r.st = 3
		}
		rows = append(rows, r)
	}
	rank := map[int]int{2: 0, 1: 1, 3: 2, 0: 3}
	sort.SliceStable(rows, func(i, j int) bool { return rank[rows[i].st] < rank[rows[j].st] })

	st := widgetStateT{V: 1, TS: ts, M: clipRunes(note, 60), C: [][]any{}}
	if !s.TelegramControl {
		st.RO = 1
	}
	if noQ {
		st.NQ = 1
	}
	if d := statDisk(a.m.cfg.RecDir); d != nil && d.Total > 0 {
		st.D = [2]float64{round1(float64(d.Free) / (1 << 30)), round1(float64(d.Total) / (1 << 30))}
	}
	for _, j := range a.m.Jobs() {
		switch j.Status {
		case "running":
			st.J[0]++
		case "queued":
			st.J[1]++
		}
	}
	for _, r := range rows {
		st.C = append(st.C, []any{r.id, r.name, r.st, r.since})
	}
	out := jsonCompact(st)
	for len(out) > widgetStateMax && len(st.C) > 0 { // drop the quietest channels until it fits
		st.C = st.C[:len(st.C)-1]
		st.X++
		out = jsonCompact(st)
	}
	return out, live, rec
}

var widgetLabelKeys = []string{"rec", "stop", "none", "free", "jobs", "upd", "err", "more", "ro"}

func (t *Telegram) widgetHTML(s Settings) string {
	labels := map[string]string{}
	for _, k := range widgetLabelKeys {
		labels[k] = locs.Tr(s.TelegramLang, "tg_w_"+k)
	}
	return strings.Replace(widgetTemplate, "__L__", jsonCompact(labels), 1)
}

// widgetTemplate has no external resources (widgets run without network access).
// All dynamic text goes through textContent, never innerHTML.
const widgetTemplate = `<style>
:root{--c:var(--tg-theme-text-color,#222);--h:var(--tg-theme-hint-color,#888);--a:var(--tg-theme-button-color,#2f80ed);--b:var(--tg-theme-button-text-color,#fff)}
*{box-sizing:border-box}
#w{color:var(--c);font:14px/1.35 system-ui,sans-serif;padding:2px 0}
.r{display:flex;align-items:center;gap:8px;padding:5px 0}
.d{width:10px;height:10px;border-radius:50%;flex:none;background:var(--h);opacity:.5}
.d.s1{background:#2ecc71;opacity:1}.d.s2{background:#e74c3c;opacity:1}.d.s3{background:#f39c12;opacity:1}
.n{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.t{color:var(--h);font-variant-numeric:tabular-nums;font-size:12px}
button{border:0;border-radius:8px;padding:4px 10px;background:var(--a);color:var(--b);font:inherit;font-size:12px;min-width:58px}
button.x{background:#e74c3c;color:#fff}button:disabled{opacity:.5}
.bar{height:6px;border-radius:3px;background:rgba(128,128,128,.25);margin:4px 0;overflow:hidden}
.bar i{display:block;height:100%;background:var(--a)}
.f{display:flex;justify-content:space-between;align-items:center;color:var(--h);font-size:12px;gap:8px}
.f button{background:none;color:var(--a);min-width:0;padding:2px 6px;font-size:16px}
</style><div id="w"></div><script>
var W=window.TelegramWidget,L=__L__,S=null,busy='',note='',tm=0;
function E(t,c,x){var e=document.createElement(t);if(c)e.className=c;if(x!=null)e.textContent=x;return e}
function P(n){return(n<10?'0':'')+n}
function D(s){s=Math.max(0,s|0);var h=s/3600|0;return(h?h+':':'')+P(s/60%60|0)+':'+P(s%60)}
function J(x){try{var o=JSON.parse(x);return o&&o.v===1?o:null}catch(e){return null}}
function go(d){if(busy)return;busy=d;draw();W.request(d).then(function(r){busy='';var o=r&&r.result?J(r.result):null;if(o){S=o;note=o.m||''}fin()},function(){busy='';note=L.err;fin()})}
function fin(){draw();clearTimeout(tm);if(note)tm=setTimeout(function(){note='';draw()},3500)}
function draw(){
var w=document.getElementById('w');w.textContent='';if(!S){w.appendChild(E('div','t','…'));return}
S.c.forEach(function(c){
var r=E('div','r');r.appendChild(E('span','d s'+c[2]));r.appendChild(E('span','n',c[1]));
if(c[2]==2){var t=E('span','t','');t.setAttribute('data-s',c[3]);r.appendChild(t)}
if(!S.ro&&!S.nq&&c[2]!=3){var q=c[2]==2?'stop':'rec',b=E('button',q=='stop'?'x':'',q=='stop'?L.stop:L.rec),k=q+':'+c[0];b.disabled=!!busy;b.onclick=function(){go(k)};r.appendChild(b)}
w.appendChild(r)});
if(!S.c.length)w.appendChild(E('div','t',L.none));
if(S.x)w.appendChild(E('div','t',L.more.replace('{n}',S.x)));
if(S.d[1]){var u=Math.max(0,Math.min(100,100-S.d[0]/S.d[1]*100)),bar=E('div','bar'),i=E('i');i.style.width=u+'%';bar.appendChild(i);w.appendChild(bar);
w.appendChild(E('div','f',L.free.replace('{free}',S.d[0]).replace('{total}',S.d[1])))}
if(S.j[0]+S.j[1])w.appendChild(E('div','t',L.jobs.replace('{run}',S.j[0]).replace('{q}',S.j[1])));
var f=E('div','f'),m=E('span','',note||(S.ro?L.ro+' · ':'')+(S.ts?L.upd+' '+new Date(S.ts*1000).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'}):''));
f.appendChild(m);if(!S.nq){var rb=E('button','','⟳');rb.disabled=!!busy;rb.onclick=function(){go('r')};f.appendChild(rb)}w.appendChild(f);tick()}
function tick(){var n=Date.now()/1000;document.querySelectorAll('[data-s]').forEach(function(e){e.textContent=D(n-e.getAttribute('data-s'))})}
S=J(W.chatData);draw();setInterval(tick,1000);
W.onEvent('chatDataChanged',function(){var o=J(W.chatData);if(o){S=o;draw()}});
</script>`

// ------------------------------------------------------------ tracking

type widgetRegistry struct {
	path string
	at   map[int64]int64 // chat -> message id of its live dashboard
	noq  map[int64]bool  // dashboards Telegram accepted only without allow_queries
	last string          // last pushed state (without ts)
	push time.Time
}

func (t *Telegram) widgetsLoad() {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.wr != nil {
		return
	}
	t.wr = &widgetRegistry{path: filepath.Join(t.a.m.cfg.DataDir, "telegram_widgets.json"), at: map[int64]int64{}, noq: map[int64]bool{}}
	if b, err := os.ReadFile(t.wr.path); err == nil {
		var f struct {
			At  map[string]int64 `json:"at"`
			NoQ map[string]bool  `json:"noq"`
		}
		if json.Unmarshal(b, &f) == nil {
			for k, v := range f.At {
				if id, err := strconv.ParseInt(k, 10, 64); err == nil {
					t.wr.at[id] = v
				}
			}
			for k, v := range f.NoQ {
				if id, err := strconv.ParseInt(k, 10, 64); err == nil && v {
					t.wr.noq[id] = true
				}
			}
		}
	}
}

func (t *Telegram) widgetsSave() { // t.wmu held
	f := struct {
		At  map[string]int64 `json:"at"`
		NoQ map[string]bool  `json:"noq,omitempty"`
	}{map[string]int64{}, map[string]bool{}}
	for k, v := range t.wr.at {
		f.At[strconv.FormatInt(k, 10)] = v
		if t.wr.noq[k] {
			f.NoQ[strconv.FormatInt(k, 10)] = true
		}
	}
	b, _ := json.Marshal(f)
	tmp := t.wr.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, t.wr.path)
	}
}

func (t *Telegram) widgetTrack(chat, msg int64, noQ bool) (old int64) {
	t.widgetsLoad()
	t.wmu.Lock()
	defer t.wmu.Unlock()
	old = t.wr.at[chat]
	t.wr.at[chat] = msg
	t.wr.noq[chat] = noQ
	for len(t.wr.at) > widgetMaxTrack { // forget somebody else's: the map is tiny, any entry will do
		for k := range t.wr.at {
			if k != chat {
				delete(t.wr.at, k)
				delete(t.wr.noq, k)
				break
			}
		}
	}
	t.wr.last = "" // push the first update soon
	t.widgetsSave()
	return old
}

func (t *Telegram) widgetUntrack(chat, msg int64) {
	t.widgetsLoad()
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.wr.at[chat] == msg {
		delete(t.wr.at, chat)
		delete(t.wr.noq, chat)
		t.widgetsSave()
	}
}

// widgetPushOne writes the state into one widget message.
func (t *Telegram) widgetPushOne(chat, msg int64, data string) {
	err := t.call(context.Background(), t.token(), "editMessageWidgetData", map[string]any{"chat_id": chat, "message_id": msg, "chat_data": data}, nil, 15*time.Second)
	var te *tgError
	if errors.As(err, &te) && te.Code == 400 {
		d := strings.ToLower(te.Desc)
		if strings.Contains(d, "not modified") {
			return
		}
		if strings.Contains(d, "not found") || strings.Contains(d, "message_id_invalid") || strings.Contains(d, "can't be edited") {
			t.widgetUntrack(chat, msg) // deleted by the user
		}
	}
}

// RunWidgets keeps tracked dashboards fresh: on change, plus a heartbeat so the
// "updated at" time shows the bot is alive.
func (t *Telegram) RunWidgets(ctx context.Context) {
	t.widgetsLoad()
	tick := time.NewTicker(widgetTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s := t.a.m.st.Settings()
		if !s.TelegramOn || s.TelegramToken == "" {
			continue
		}
		t.widgetsSync(s)
	}
}

func (t *Telegram) widgetsSync(s Settings) {
	t.wmu.Lock()
	n := len(t.wr.at)
	t.wmu.Unlock()
	if n == 0 {
		return
	}
	plain, _, _ := t.widgetData(s, "", 0, false)
	t.wmu.Lock()
	if plain == t.wr.last && time.Since(t.wr.push) < widgetHeartbeat {
		t.wmu.Unlock()
		return
	}
	t.wr.last, t.wr.push = plain, time.Now()
	targets, noq := map[int64]int64{}, map[int64]bool{}
	for k, v := range t.wr.at {
		targets[k], noq[k] = v, t.wr.noq[k]
	}
	t.wmu.Unlock()
	now := time.Now().Unix()
	data, _, _ := t.widgetData(s, "", now, false)
	dataNQ, _, _ := t.widgetData(s, "", now, true)
	for chat, msg := range targets {
		if noq[chat] {
			t.widgetPushOne(chat, msg, dataNQ)
		} else {
			t.widgetPushOne(chat, msg, data)
		}
	}
}

// ------------------------------------------------------------ sending & requests

// widgetErrText turns Telegram's widget error codes into something readable.
func widgetErrText(err error) string {
	var te *tgError
	if errors.As(err, &te) {
		return te.Desc
	}
	return err.Error()
}

// cmdDashboard sends (or replaces) the dashboard widget of a chat. If Telegram
// refuses INPUT_MEDIA_WIDGET_NOT_ALLOWED, it is tried once more as a display-only
// widget (no allow_queries); if that fails too, the plain status is shown.
func (t *Telegram) cmdDashboard(s Settings, chat int64) {
	send := func(noQ bool) (tgMessage, error) {
		data, live, rec := t.widgetData(s, "", time.Now().Unix(), noQ)
		var res tgMessage
		err := t.call(context.Background(), t.token(), "sendWidget", map[string]any{
			"chat_id": chat, "html": t.widgetHTML(s), "chat_data": data, "allow_queries": !noQ,
			"caption": t.tr(s, "tg_w_caption", "live", itoa(live), "rec", itoa(rec)),
		}, &res, 20*time.Second)
		return res, err
	}
	noQ := false
	res, err := send(false)
	var te *tgError
	if errors.As(err, &te) && te.Code == 400 && strings.Contains(te.Desc, "WIDGET_NOT_ALLOWED") {
		t.a.m.elog.Add(Channel{}, "system", "warn", "telegram: widget with requests refused, retrying display-only", te.Desc)
		noQ = true
		res, err = send(true)
	}
	if err != nil {
		t.a.m.elog.Add(Channel{}, "system", "warn", "telegram: sendWidget failed", err.Error())
		text, k := t.viewStatus(s)
		_ = t.send(chat, esc(t.tr(s, "tg_w_unavail", "error", widgetErrText(err)))+"\n\n"+text, k)
		return
	}
	if old := t.widgetTrack(chat, res.MessageID, noQ); old != 0 && old != res.MessageID {
		_ = t.call(context.Background(), t.token(), "deleteMessage", map[string]any{"chat_id": chat, "message_id": old}, nil, 10*time.Second)
	}
}

type tgWidgetQuery struct {
	ID        string `json:"id"`
	From      tgUser `json:"from"`
	Chat      tgChat `json:"chat"`
	MessageID int64  `json:"message_id"`
	Data      string `json:"data"`
}

// onWidgetQuery answers a tap in the widget (within Telegram's 15 s limit).
func (t *Telegram) onWidgetQuery(s Settings, q *tgWidgetQuery) {
	answer := func(data string) {
		p := map[string]any{"widget_query_id": q.ID}
		if data != "" {
			p["data"] = data
		}
		_ = t.call(context.Background(), t.token(), "answerWidgetQuery", p, nil, 10*time.Second)
	}
	if q.Chat.Type != "private" || q.Chat.ID != q.From.ID || !t.allowed(s, q.From.ID) {
		answer("")
		return
	}
	verb, arg, _ := strings.Cut(q.Data, ":")
	note := ""
	switch verb {
	case "r":
	case "rec", "stop", "chk":
		if !s.TelegramControl {
			note = t.tr(s, "tg_ro")
		} else {
			note = t.channelAction(s, verb, arg)
			time.Sleep(300 * time.Millisecond) // let the manager publish the new state
		}
	default:
		answer("")
		return
	}
	data, _, _ := t.widgetData(s, note, time.Now().Unix(), false)
	answer(data)
	if note != "" && q.MessageID != 0 { // the stored state too, so reopening the chat shows it
		plain, _, _ := t.widgetData(s, "", time.Now().Unix(), false)
		t.widgetPushOne(q.Chat.ID, q.MessageID, plain)
	}
}
