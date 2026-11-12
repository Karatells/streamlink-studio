package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var tgMenu = []string{"status", "channels", "recording", "recordings", "stats", "jobs", "log", "notifications", "health", "notify", "add", "help"}

const tgPageSize = 8

func esc(s string) string { return html.EscapeString(s) }

func (t *Telegram) tr(s Settings, key string, kv ...string) string {
	vars := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		vars[kv[i]] = kv[i+1]
	}
	return locs.Fill(s.TelegramLang, key, vars)
}

func tgDur(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm %02ds", m, sec)
}

func tgTime(ts time.Time) string { return ts.Local().Format("02 Jan 15:04") }

func (t *Telegram) allowed(s Settings, uid int64) bool {
	for _, u := range s.TelegramUsers {
		if u == uid {
			return true
		}
	}
	return false
}

func (t *Telegram) handle(u tgUpdate) {
	s := t.a.m.st.Settings()
	if !s.TelegramOn || s.TelegramToken == "" {
		return
	}
	if u.Message != nil {
		t.onMessage(s, u.Message)
	}
	if u.Callback != nil {
		t.onCallback(s, u.Callback)
	}
}

func splitCmd(text string) (cmd, arg string) {
	text = strings.TrimSpace(text)
	head, rest, _ := strings.Cut(text, " ")
	head = strings.TrimPrefix(head, "/")
	head, _, _ = strings.Cut(head, "@") // /status@MyBot
	return strings.ToLower(head), strings.TrimSpace(rest)
}

func (t *Telegram) onMessage(s Settings, m *tgMessage) {
	if m.From == nil || m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return // groups and channels are never served
	}
	uid := m.From.ID
	if !t.allowed(s, uid) {
		t.a.m.elog.Add(Channel{}, "system", "warn", fmt.Sprintf("telegram: access denied for user ID %d", uid), "")
		cmd, _ := splitCmd(m.Text)
		if cmd != "start" && cmd != "whoami" {
			return
		}
		t.mu.Lock()
		recent := time.Since(t.denied[uid]) < time.Minute
		if !recent {
			t.denied[uid] = time.Now()
		}
		t.mu.Unlock()
		if !recent {
			_ = t.send(m.Chat.ID, t.tr(s, "tg_denied", "id", strconv.FormatInt(uid, 10)), nil)
		}
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(m.Text), "/") {
		_ = t.send(m.Chat.ID, t.tr(s, "tg_unknown"), nil)
		return
	}
	cmd, arg := splitCmd(m.Text)
	text, k := t.run(s, m.Chat.ID, cmd, arg)
	_ = t.send(m.Chat.ID, text, k)
}

// run executes a command and returns the reply.
func (t *Telegram) run(s Settings, chat int64, cmd, arg string) (string, kbd) {
	switch cmd {
	case "start", "help":
		return t.viewHelp(s)
	case "whoami":
		return t.tr(s, "tg_whoami", "id", strconv.FormatInt(chat, 10)), nil
	case "status":
		return t.viewStatus(s)
	case "channels":
		return t.viewChannels(s)
	case "recording":
		return t.viewRecording(s)
	case "recordings":
		return t.viewRecordings(s, chat, arg, 0, true)
	case "stats":
		return t.viewStats(s)
	case "jobs":
		return t.viewJobs(s)
	case "log":
		return t.viewLog(s, arg)
	case "notifications":
		return t.viewNotifications(s)
	case "health":
		return t.viewHealth(s)
	case "notify":
		return t.viewNotify(s)
	case "add":
		if !s.TelegramControl {
			return t.tr(s, "tg_ro"), nil
		}
		return t.cmdAdd(s, arg)
	}
	return t.tr(s, "tg_unknown"), nil
}

// ------------------------------------------------------------ callbacks

func (t *Telegram) onCallback(s Settings, cb *tgCallback) {
	if cb.Message == nil || cb.Message.Chat.Type != "private" || cb.Message.Chat.ID != cb.From.ID {
		return
	}
	if !t.allowed(s, cb.From.ID) {
		t.answer(cb.ID, "")
		return
	}
	chat, msg := cb.Message.Chat.ID, cb.Message.MessageID
	verb, arg, _ := strings.Cut(cb.Data, ":")
	show := func(text string, k kbd) { t.edit(chat, msg, text, k) }
	control := func() bool {
		if s.TelegramControl {
			return true
		}
		t.answer(cb.ID, t.tr(s, "tg_ro"))
		return false
	}
	switch verb {
	case "r": // refresh a read-only view
		text, k := t.run(s, chat, arg, "")
		t.answer(cb.ID, "")
		show(text, k)
	case "ch":
		t.answer(cb.ID, "")
		show(t.chCard(s, arg, ""))
	case "chl":
		t.answer(cb.ID, "")
		show(t.viewChannels(s))
	case "rec", "stop", "chk":
		if !control() {
			return
		}
		note := t.channelAction(s, verb, arg)
		t.answer(cb.ID, note)
		show(t.chCard(s, arg, note))
	case "rp": // recordings page
		page, _ := strconv.Atoi(arg)
		t.answer(cb.ID, "")
		show(t.viewRecordings(s, chat, "", page, false))
	case "rf":
		t.answer(cb.ID, "")
		show(t.fileCard(s, chat, arg, ""))
	case "fd":
		if !control() {
			return
		}
		name, ok := t.pick(chat, arg)
		if !ok {
			t.answer(cb.ID, t.tr(s, "tg_file_expired"))
			return
		}
		t.answer(cb.ID, "")
		show(t.tr(s, "tg_file_del_confirm", "name", esc(name)), kbd{{{T: t.tr(s, "tg_btn_confirm"), D: "fy:" + arg}, {T: t.tr(s, "tg_btn_cancel"), D: "rf:" + arg}}})
	case "fy":
		if !control() {
			return
		}
		name, ok := t.pick(chat, arg)
		if !ok {
			t.answer(cb.ID, t.tr(s, "tg_file_expired"))
			return
		}
		if err := t.deleteRecording(name); err != nil {
			t.answer(cb.ID, err.Error())
			return
		}
		t.answer(cb.ID, t.tr(s, "tg_file_deleted", "name", name))
		show(t.viewRecordings(s, chat, "", 0, false))
	case "fs":
		if !control() {
			return
		}
		name, ok := t.pick(chat, arg)
		if !ok {
			t.answer(cb.ID, t.tr(s, "tg_file_expired"))
			return
		}
		res, err := t.a.m.SaveRecordings([]string{name}, "")
		if err == nil && len(res.Failed) > 0 {
			err = errors.New(res.Failed[0].Error)
		}
		if err != nil {
			t.answer(cb.ID, err.Error())
			return
		}
		t.answer(cb.ID, t.tr(s, "tg_file_saved"))
		show(t.viewRecordings(s, chat, "", 0, false))
	case "nt", "nta", "ntn":
		if !control() {
			return
		}
		cur := map[string]bool{}
		for _, ty := range notifyTypes {
			cur[ty] = tgTypeOn(s, ty)
		}
		switch verb {
		case "nta":
			for k := range cur {
				cur[k] = true
			}
		case "ntn":
			for k := range cur {
				cur[k] = false
			}
		default:
			if validNotifyType(arg) {
				cur[arg] = !cur[arg]
			}
		}
		list := []string{}
		for _, ty := range notifyTypes {
			if cur[ty] {
				list = append(list, ty)
			}
		}
		s.TelegramTypes = list
		fresh := t.a.m.st.Settings() // do not overwrite unrelated settings changed meanwhile
		fresh.TelegramTypes = list
		if err := t.a.m.st.SetSettings(fresh); err != nil {
			t.answer(cb.ID, err.Error())
			return
		}
		t.answer(cb.ID, "")
		show(t.viewNotify(fresh))
	default:
		t.answer(cb.ID, "")
	}
}

func (t *Telegram) channelAction(s Settings, verb, id string) string {
	ch, ok := t.a.m.st.Channel(id)
	if !ok {
		return t.tr(s, "tg_not_found")
	}
	name := streamerName(ch)
	var err error
	switch verb {
	case "rec":
		if err = t.a.m.StartRecording(id); err == nil {
			t.a.m.elog.Add(ch, "system", "info", "recording started from Telegram", "")
			return t.tr(s, "tg_started", "name", name)
		}
	case "stop":
		if err = t.a.m.StopRecording(id); err == nil {
			return t.tr(s, "tg_stopped", "name", name)
		}
	case "chk":
		t.a.m.ForceCheck(id)
		return t.tr(s, "tg_checking", "name", name)
	}
	return t.tr(s, "tg_err", "error", err.Error())
}

func (t *Telegram) deleteRecording(name string) error {
	if !safeRecordingName(name) {
		return fmt.Errorf("invalid file name")
	}
	if t.a.m.ActiveFile(name) {
		return fmt.Errorf("file is being recorded")
	}
	if err := os.Remove(filepath.Join(t.a.m.cfg.RecDir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	t.a.m.meta.remove(name)
	t.a.m.elog.Add(Channel{}, "system", "info", "recording deleted from Telegram: "+name, "")
	return nil
}

// ------------------------------------------------------------ views

func (t *Telegram) viewHelp(s Settings) (string, kbd) {
	var b strings.Builder
	b.WriteString("<b>Streamlink Studio</b>\n")
	for _, c := range tgMenu {
		if c == "help" {
			continue
		}
		fmt.Fprintf(&b, "/%s — %s\n", c, esc(t.tr(s, "tg_cmd_"+c)))
	}
	if !s.TelegramControl {
		b.WriteString("\n" + esc(t.tr(s, "tg_ro")))
	}
	return b.String(), nil
}

func statusIcon(st string) string {
	switch st {
	case StatusRecording:
		return "🔴"
	case StatusLive:
		return "🟢"
	case StatusAuthError:
		return "🔑"
	case StatusError:
		return "⚠️"
	case StatusChecking:
		return "🔄"
	}
	return "⚪"
}

var healthIcon = map[string]string{"ok": "🟢", "warn": "🟡", "error": "🔴"}

func (t *Telegram) viewStatus(s Settings) (string, kbd) {
	a := t.a
	var live, rec, total int
	var recLines []string
	for _, ch := range a.m.st.Channels() {
		v := a.view(ch)
		total++
		switch v.Status {
		case StatusRecording:
			rec++
			live++
			d := ""
			if v.RecordingSince != nil {
				d = tgDur(time.Since(*v.RecordingSince))
			}
			recLines = append(recLines, "   • "+esc(v.DisplayName)+" · "+d)
		case StatusLive:
			live++
		}
	}
	h := a.runHealth()
	running, queued := 0, 0
	for _, j := range a.m.Jobs() {
		switch j.Status {
		case "running":
			running++
		case "queued":
			queued++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Streamlink Studio</b> %s\n", healthIcon[h.Status], esc(version))
	b.WriteString(esc(t.tr(s, "hc_overall_"+h.Status)) + "\n\n")
	b.WriteString(esc(t.tr(s, "tg_status_channels", "total", itoa(total), "live", itoa(live), "rec", itoa(rec))) + "\n")
	for _, l := range recLines {
		b.WriteString(l + "\n")
	}
	if running+queued > 0 {
		b.WriteString(esc(t.tr(s, "tg_status_jobs", "running", itoa(running), "queued", itoa(queued))) + "\n")
	}
	if d := statDisk(a.m.cfg.RecDir); d != nil && d.Total > 0 {
		b.WriteString(esc(t.tr(s, "tg_status_disk", "free", fmtBytes(int64(d.Free)), "total", fmtBytes(int64(d.Total)), "pct", fmt.Sprintf("%.0f", float64(d.Free)/float64(d.Total)*100))) + "\n")
	}
	if u := a.m.notes.Summary()["unread"]; u > 0 {
		b.WriteString(esc(t.tr(s, "tg_status_unread", "n", strconv.FormatInt(u, 10))) + "\n")
	}
	if a.upd != nil && a.upd.Available() {
		b.WriteString(esc(t.tr(s, "tg_status_update")) + "\n")
	}
	b.WriteString(esc(t.tr(s, "tg_status_uptime", "uptime", tgDur(time.Since(processStart)))))
	return b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:status"}}}
}

func (t *Telegram) viewChannels(s Settings) (string, kbd) {
	chs := t.a.m.st.Channels()
	if len(chs) == 0 {
		return t.tr(s, "tg_channels_none"), nil
	}
	var b strings.Builder
	b.WriteString("<b>" + esc(t.tr(s, "tg_channels_title")) + "</b>\n")
	var k kbd
	var row []tgBtn
	for i, ch := range chs {
		if i >= 40 {
			break
		}
		v := t.a.view(ch)
		fmt.Fprintf(&b, "%s %s <i>%s</i>\n", statusIcon(v.Status), esc(v.DisplayName), esc(v.Plugin))
		row = append(row, tgBtn{T: statusIcon(v.Status) + " " + v.DisplayName, D: "ch:" + ch.ID})
		if len(row) == 2 {
			k, row = append(k, row), nil
		}
	}
	if len(row) > 0 {
		k = append(k, row)
	}
	return b.String(), append(k, []tgBtn{{T: t.tr(s, "tg_btn_refresh"), D: "r:channels"}})
}

func (t *Telegram) chCard(s Settings, id, note string) (string, kbd) {
	ch, ok := t.a.m.st.Channel(id)
	if !ok {
		return t.tr(s, "tg_not_found"), nil
	}
	v := t.a.view(ch)
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b> <i>%s</i>\n", statusIcon(v.Status), esc(v.DisplayName), esc(v.Plugin))
	fmt.Fprintf(&b, "%s\n", esc(t.tr(s, "tg_st_"+v.Status)))
	if v.Title != "" {
		fmt.Fprintf(&b, "📝 %s\n", esc(v.Title))
	}
	ar := t.tr(s, "off")
	if v.AutoRecord {
		ar = t.tr(s, "on")
	}
	fmt.Fprintf(&b, "%s: <b>%s</b>", esc(t.tr(s, "auto_rec")), esc(ar))
	if v.Quality != "" {
		fmt.Fprintf(&b, " · %s", esc(v.Quality))
	}
	b.WriteString("\n")
	if v.RecordingSince != nil {
		fmt.Fprintf(&b, "⏺ %s · %s\n", esc(tgDur(time.Since(*v.RecordingSince))), esc(v.RecordingQuality))
	}
	if v.LastCheck != nil {
		fmt.Fprintf(&b, "🕒 %s\n", esc(tgTime(*v.LastCheck)))
	}
	if v.Error != "" {
		fmt.Fprintf(&b, "⚠️ %s\n", esc(v.Error))
	}
	if note != "" {
		fmt.Fprintf(&b, "\n<i>%s</i>", esc(note))
	}
	var act []tgBtn
	if s.TelegramControl {
		switch v.Status {
		case StatusRecording:
			act = append(act, tgBtn{T: t.tr(s, "tg_btn_stop"), D: "stop:" + id})
		case StatusLive:
			act = append(act, tgBtn{T: t.tr(s, "tg_btn_record"), D: "rec:" + id})
		}
		act = append(act, tgBtn{T: t.tr(s, "tg_btn_check"), D: "chk:" + id})
	}
	k := kbd{}
	if len(act) > 0 {
		k = append(k, act)
	}
	nav := []tgBtn{{T: t.tr(s, "tg_btn_back"), D: "chl:"}}
	if strings.HasPrefix(ch.URL, "http") {
		nav = append(nav, tgBtn{T: t.tr(s, "tg_btn_open"), U: ch.URL})
	}
	return b.String(), append(k, nav)
}

func (t *Telegram) viewRecording(s Settings) (string, kbd) {
	var b strings.Builder
	n := 0
	for _, ch := range t.a.m.st.Channels() {
		v := t.a.view(ch)
		if v.Status != StatusRecording {
			continue
		}
		n++
		var size int64
		if fi, err := os.Stat(filepath.Join(t.a.m.cfg.RecDir, v.RecordingFile)); err == nil {
			size = fi.Size()
		}
		d := ""
		if v.RecordingSince != nil {
			d = tgDur(time.Since(*v.RecordingSince))
		}
		fmt.Fprintf(&b, "🔴 <b>%s</b> <i>%s</i>\n   %s · %s · %s\n", esc(v.DisplayName), esc(v.Plugin), esc(d), esc(fmtBytes(size)), esc(v.RecordingQuality))
	}
	if n == 0 {
		return t.tr(s, "tg_rec_none"), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:recording"}}}
	}
	return "<b>" + esc(t.tr(s, "tg_rec_title")) + "</b>\n" + b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:recording"}}}
}

func (t *Telegram) setPicks(chat int64, p *pickList) {
	t.mu.Lock()
	t.picks[chat] = p
	for id, o := range t.picks { // forget stale lists
		if time.Since(o.at) > time.Hour {
			delete(t.picks, id)
		}
	}
	t.mu.Unlock()
}

func (t *Telegram) pick(chat int64, idx string) (string, bool) {
	i, err := strconv.Atoi(idx)
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.picks[chat]
	if err != nil || p == nil || time.Since(p.at) > 30*time.Minute || i < 0 || i >= len(p.names) {
		return "", false
	}
	return p.names[i], true
}

func (t *Telegram) viewRecordings(s Settings, chat int64, filter string, page int, fresh bool) (string, kbd) {
	var pl *pickList
	t.mu.Lock()
	pl = t.picks[chat]
	t.mu.Unlock()
	if fresh || pl == nil {
		pl = &pickList{filter: strings.ToLower(filter)}
	}
	var items []RecInfo
	for _, r := range t.a.listRecordings() {
		if pl.filter == "" || strings.Contains(strings.ToLower(r.Streamer), pl.filter) || strings.Contains(strings.ToLower(r.Name), pl.filter) {
			items = append(items, r)
		}
	}
	if len(items) == 0 {
		return t.tr(s, "tg_files_none"), nil
	}
	pages := (len(items) + tgPageSize - 1) / tgPageSize
	page = max(0, min(page, pages-1))
	pl.names, pl.page, pl.at = make([]string, len(items)), page, time.Now()
	var total int64
	for i, r := range items {
		pl.names[i] = r.Name
		total += r.Size
	}
	t.setPicks(chat, pl)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n", esc(t.tr(s, "tg_files_title", "count", itoa(len(items)), "size", fmtBytes(total), "page", itoa(page+1), "pages", itoa(pages))))
	var k kbd
	var row []tgBtn
	for i := page * tgPageSize; i < len(items) && i < (page+1)*tgPageSize; i++ {
		r := items[i]
		rc := ""
		if r.Recording {
			rc = "🔴 "
		}
		fmt.Fprintf(&b, "%d. %s%s%s\n    %s · %s · %s\n", i-page*tgPageSize+1, rc, "", esc(r.Streamer), esc(fmtBytes(r.Size)), esc(tgTime(r.Started)), esc(r.Ext))
		row = append(row, tgBtn{T: itoa(i - page*tgPageSize + 1), D: "rf:" + itoa(i)})
		if len(row) == 4 {
			k, row = append(k, row), nil
		}
	}
	if len(row) > 0 {
		k = append(k, row)
	}
	var nav []tgBtn
	if page > 0 {
		nav = append(nav, tgBtn{T: "◀", D: "rp:" + itoa(page-1)})
	}
	nav = append(nav, tgBtn{T: t.tr(s, "tg_btn_refresh"), D: "rp:" + itoa(page)})
	if page < pages-1 {
		nav = append(nav, tgBtn{T: "▶", D: "rp:" + itoa(page+1)})
	}
	return b.String(), append(k, nav)
}

func (t *Telegram) fileCard(s Settings, chat int64, idx, note string) (string, kbd) {
	name, ok := t.pick(chat, idx)
	if !ok {
		return t.tr(s, "tg_file_expired"), nil
	}
	var info *RecInfo
	for _, r := range t.a.listRecordings() {
		if r.Name == name {
			r := r
			info = &r
			break
		}
	}
	if info == nil {
		return t.tr(s, "tg_file_expired"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎞 <b>%s</b>\n%s", esc(info.Name), esc(info.Streamer))
	if info.Platform != "" {
		fmt.Fprintf(&b, " <i>%s</i>", esc(info.Platform))
	}
	fmt.Fprintf(&b, "\n%s · %s\n", esc(fmtBytes(info.Size)), esc(tgTime(info.Started)))
	if info.Title != "" {
		fmt.Fprintf(&b, "📝 %s\n", esc(info.Title))
	}
	if info.Recording {
		b.WriteString("🔴 " + esc(t.tr(s, "tg_file_busy")) + "\n")
	}
	var k kbd
	if s.TelegramControl && !info.Recording {
		save := t.tr(s, "tg_btn_save")
		k = append(k, []tgBtn{{T: save, D: "fs:" + idx}, {T: t.tr(s, "tg_btn_delete"), D: "fd:" + idx}})
	}
	t.mu.Lock()
	pg := 0
	if p := t.picks[chat]; p != nil {
		pg = p.page
	}
	t.mu.Unlock()
	return b.String(), append(k, []tgBtn{{T: t.tr(s, "tg_btn_back"), D: "rp:" + itoa(pg)}})
}

type tgStats struct {
	Disk    *diskStat `json:"disk"`
	Library struct {
		Count, SavedCount, RecordingNow int
		Size, SavedSize                 int64
	} `json:"library"`
	Streamers []groupStat `json:"streamers"`
	Platforms []groupStat `json:"platforms"`
	Largest   []struct {
		Name, Streamer string
		Size           int64
	} `json:"largest"`
	DaysLeft float64 `json:"days_left"`
	WeekSize int64   `json:"week_size"`
	Days     []dayStat
	System   struct {
		UptimeHost int64   `json:"uptime_host"`
		Load1      float64 `json:"load1"`
		CPUs       int     `json:"cpus"`
		MemTotal   uint64  `json:"mem_total"`
		MemAvail   uint64  `json:"mem_avail"`
		AppMem     uint64  `json:"app_mem"`
	} `json:"system"`
}

func (t *Telegram) viewStats(s Settings) (string, kbd) {
	var st tgStats
	raw, _ := json.Marshal(t.a.buildStats())
	_ = json.Unmarshal(raw, &st)
	var b strings.Builder
	line := func(key string, kv ...string) { b.WriteString(esc(t.tr(s, key, kv...)) + "\n") }
	b.WriteString("<b>📊 " + esc(t.tr(s, "tg_stats_title")) + "</b>\n\n")
	line("tg_stats_library", "count", itoa(st.Library.Count), "size", fmtBytes(st.Library.Size))
	line("tg_stats_saved", "count", itoa(st.Library.SavedCount), "size", fmtBytes(st.Library.SavedSize))
	line("tg_stats_week", "size", fmtBytes(st.WeekSize))
	if d := st.Disk; d != nil && d.Total > 0 {
		line("tg_stats_disk", "used", fmtBytes(int64(d.Used)), "total", fmtBytes(int64(d.Total)), "free", fmtBytes(int64(d.Free)))
		switch {
		case st.DaysLeft <= 0:
		case st.DaysLeft > 365:
			line("st_days_left_many")
		default:
			line("st_days_left", "days", fmt.Sprintf("%.0f", st.DaysLeft))
		}
	}
	list := func(titleKey string, gs []groupStat, n int) {
		if len(gs) == 0 {
			return
		}
		b.WriteString("\n<b>" + esc(t.tr(s, titleKey)) + "</b>\n")
		for i, g := range gs {
			if i >= n {
				break
			}
			fmt.Fprintf(&b, "• %s — %s (%d)\n", esc(g.Name), esc(fmtBytes(g.Size)), g.Count)
		}
	}
	list("st_by_streamer", st.Streamers, 5)
	list("st_by_platform", st.Platforms, 4)
	if len(st.Largest) > 0 {
		b.WriteString("\n<b>" + esc(t.tr(s, "st_largest")) + "</b>\n")
		for i, r := range st.Largest {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "• %s — %s\n", esc(r.Name), esc(fmtBytes(r.Size)))
		}
	}
	cnt := t.a.m.cnt
	b.WriteString("\n<b>" + esc(t.tr(s, "tg_stats_session")) + "</b>\n")
	line("tg_stats_recs", "started", itoa(int(cnt.Sum("streamlink_studio_recordings_started_total", ""))),
		"ok", itoa(int(cnt.Sum("streamlink_studio_recordings_finished_total", `"ok"`)+cnt.Sum("streamlink_studio_recordings_finished_total", `"stopped"`))),
		"failed", itoa(int(cnt.Sum("streamlink_studio_recordings_finished_total", `"failed"`))))
	line("tg_stats_checks", "total", itoa(int(cnt.Sum("streamlink_studio_checks_total", ""))), "errors", itoa(int(cnt.Sum("streamlink_studio_checks_total", `"error"`)+cnt.Sum("streamlink_studio_checks_total", `"auth_error"`))))
	line("tg_stats_jobs", "done", itoa(int(cnt.Sum("streamlink_studio_jobs_total", `"done"`))), "failed", itoa(int(cnt.Sum("streamlink_studio_jobs_total", `"error"`))))
	if st.System.MemTotal > 0 {
		b.WriteString("\n<b>" + esc(t.tr(s, "st_system")) + "</b>\n")
		line("tg_stats_server", "load", fmt.Sprintf("%.2f", st.System.Load1), "cpus", itoa(st.System.CPUs),
			"mem", fmtBytes(int64(st.System.MemTotal-st.System.MemAvail)), "memtotal", fmtBytes(int64(st.System.MemTotal)), "app", fmtBytes(int64(st.System.AppMem)))
	}
	return b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:stats"}}}
}

func (t *Telegram) viewJobs(s Settings) (string, kbd) {
	jobs := t.a.m.Jobs()
	if len(jobs) == 0 {
		return t.tr(s, "tg_jobs_none"), nil
	}
	var b strings.Builder
	b.WriteString("<b>⚙️ " + esc(t.tr(s, "tg_jobs_title")) + "</b>\n")
	for i, j := range jobs {
		if i >= 8 {
			break
		}
		icon := map[string]string{"queued": "⏳", "running": "⚙️", "done": "✅", "error": "❌"}[j.Status]
		pct := ""
		if j.Status == "running" && j.Progress >= 0 {
			pct = fmt.Sprintf(" %.0f%%", j.Progress*100)
		}
		fmt.Fprintf(&b, "%s <b>%s</b>%s\n    %s → %s\n", icon, esc(t.tr(s, "job_"+j.Type)), pct, esc(j.Input), esc(j.Output))
		if j.Error != "" {
			fmt.Fprintf(&b, "    <i>%s</i>\n", esc(j.Error))
		}
	}
	return b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:jobs"}}}
}

func (t *Telegram) viewLog(s Settings, arg string) (string, kbd) {
	level := "warn"
	if strings.EqualFold(arg, "all") {
		level = "info"
	}
	items := t.a.m.elog.List("", level, 10)
	if len(items) == 0 {
		return t.tr(s, "tg_log_none"), nil
	}
	var b strings.Builder
	b.WriteString("<b>🧾 " + esc(t.tr(s, "tg_log_title")) + "</b>\n")
	for _, e := range items {
		who := e.Channel
		if who == "" {
			who = t.tr(s, "log_system")
		}
		rep := ""
		if e.Count > 1 {
			rep = fmt.Sprintf(" ×%d", e.Count)
		}
		fmt.Fprintf(&b, "%s %s <b>%s</b>%s\n    %s\n", map[string]string{"info": "ℹ️", "warn": "⚠️", "error": "❌"}[e.Level], esc(tgTime(e.Last)), esc(who), rep, esc(e.Msg))
	}
	return b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:log"}}}
}

func (t *Telegram) viewNotifications(s Settings) (string, kbd) {
	items, _ := t.a.m.notes.List()
	if len(items) == 0 {
		return t.tr(s, "tg_notif_none"), nil
	}
	var b strings.Builder
	b.WriteString("<b>🔔 " + esc(t.tr(s, "tg_notif_title")) + "</b>\n")
	for i, n := range items {
		if i >= 10 {
			break
		}
		_, msg := locs.Notification(s.TelegramLang, n)
		fmt.Fprintf(&b, "%s %s\n    %s\n", notifEmoji[n.Type], esc(tgTime(n.Time)), esc(msg))
	}
	return b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:notifications"}}}
}

func (t *Telegram) viewHealth(s Settings) (string, kbd) {
	h := t.a.runHealth()
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>\n", healthIcon[h.Status], esc(t.tr(s, "hc_overall_"+h.Status)))
	for _, c := range h.Checks {
		label := t.tr(s, "hc_"+c.ID)
		if label == "hc_"+c.ID {
			label = c.ID
		}
		fmt.Fprintf(&b, "%s %s", healthIcon[c.Status], esc(label))
		if c.Detail != "" {
			fmt.Fprintf(&b, " — <i>%s</i>", esc(c.Detail))
		}
		b.WriteString("\n")
	}
	return b.String(), kbd{{{T: t.tr(s, "tg_btn_refresh"), D: "r:health"}}}
}

func (t *Telegram) viewNotify(s Settings) (string, kbd) {
	var k kbd
	for _, ty := range notifyTypes {
		mark := "⬜"
		if tgTypeOn(s, ty) {
			mark = "✅"
		}
		k = append(k, []tgBtn{{T: mark + " " + notifEmoji[ty] + " " + t.tr(s, "ntf_type_"+ty), D: "nt:" + ty}})
	}
	k = append(k, []tgBtn{{T: t.tr(s, "tg_notify_all"), D: "nta:"}, {T: t.tr(s, "tg_notify_none"), D: "ntn:"}})
	text := t.tr(s, "tg_notify_title")
	if !s.TelegramControl {
		text += "\n\n" + t.tr(s, "tg_ro")
	}
	return esc(text), k
}

func (t *Telegram) cmdAdd(s Settings, arg string) (string, kbd) {
	if arg == "" {
		return esc(t.tr(s, "tg_add_usage")), nil
	}
	ch := Channel{ID: newID(), AuthMode: "global", AutoRecord: true, CreatedAt: time.Now()}
	u := arg
	if err := applyInput(&ch, ChannelInput{URL: &u}); err != nil {
		return esc(t.tr(s, "tg_add_err", "error", err.Error())), nil
	}
	if ch.Plugin == "" {
		ch.Plugin = detectPlatform(ch.URL)
	}
	if err := t.a.m.st.PutChannel(ch); err != nil {
		return esc(t.tr(s, "tg_add_err", "error", err.Error())), nil
	}
	t.a.m.elog.Add(ch, "system", "info", "channel added from Telegram", "")
	return esc(t.tr(s, "tg_add_ok", "name", streamerName(ch))), kbd{{{T: streamerName(ch), D: "ch:" + ch.ID}}}
}
