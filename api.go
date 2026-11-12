package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type API struct {
	upd  *Updater
	m    *Manager
	auth *Auth
	tg   *Telegram
	ftp  *FTP
}

// ------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var he *httpError
	if errors.As(err, &he) {
		code = he.code
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errBad("invalid JSON: " + err.Error())
	}
	return nil
}

// ---------------------------------------------------------------- views

type ChannelView struct {
	ID               string `json:"id"`
	URL              string `json:"url"`
	Name             string `json:"name"`
	DisplayName      string `json:"display_name"`
	Plugin           string `json:"plugin"`
	Quality          string `json:"quality"`
	AutoRecord       bool   `json:"auto_record"`
	CheckInterval    int    `json:"check_interval"`
	FilenameTemplate string `json:"filename_template"`
	AuthMode         string `json:"auth_mode"`
	DisableAds       bool   `json:"disable_ads"`
	LowLatency       bool   `json:"low_latency"`
	Proxy            string `json:"proxy"`
	Headers          string `json:"headers"`
	Email            string `json:"email"`
	ExtraArgs        string `json:"extra_args"`
	HasToken         bool   `json:"has_token"`
	HasPassword      bool   `json:"has_password"`
	HasCookies       bool   `json:"has_cookies"`
	AuthActive       bool   `json:"auth_active"`

	Status           string     `json:"status"`
	Error            string     `json:"error,omitempty"`
	Title            string     `json:"title,omitempty"`
	LastCheck        *time.Time `json:"last_check,omitempty"`
	RecordingSince   *time.Time `json:"recording_since,omitempty"`
	RecordingQuality string     `json:"recording_quality,omitempty"`
	RecordingFile    string     `json:"recording_file,omitempty"`
}

func (a *API) view(ch Channel) ChannelView {
	v := ChannelView{
		ID: ch.ID, URL: ch.URL, Name: ch.Name, DisplayName: streamerName(ch), Plugin: ch.Plugin,
		Quality: ch.Quality, AutoRecord: ch.AutoRecord, CheckInterval: ch.CheckInterval,
		FilenameTemplate: ch.FilenameTemplate, AuthMode: ch.AuthMode, DisableAds: ch.DisableAds,
		LowLatency: ch.LowLatency, Proxy: ch.Proxy, Headers: ch.Headers, Email: ch.Email,
		ExtraArgs: ch.ExtraArgs, HasToken: ch.OAuthToken != "", HasPassword: ch.Password != "",
		HasCookies: ch.HasCookies, AuthActive: a.m.authActive(ch), Status: StatusOffline,
	}
	if v.Plugin == "" {
		v.Plugin = detectPlatform(ch.URL)
	}
	a.m.mu.Lock()
	if rt := a.m.rt[ch.ID]; rt != nil {
		v.Status, v.Error, v.Title = rt.Status, rt.Error, rt.Title
		if !rt.LastCheck.IsZero() {
			t := rt.LastCheck
			v.LastCheck = &t
		}
		if rt.rec != nil {
			t := rt.rec.Started
			v.RecordingSince = &t
			v.RecordingFile = rt.rec.TmpName
			v.RecordingQuality = rt.rec.Quality
		}
	}
	a.m.mu.Unlock()
	return v
}

type SettingsView struct {
	HasTwitchToken  bool     `json:"has_twitch_token"`
	HasCookies      bool     `json:"has_cookies"`
	Proxy           string   `json:"proxy"`
	DefaultQuality  string   `json:"default_quality"`
	DefaultInterval int      `json:"default_interval"`
	DefaultTemplate string   `json:"default_template"`
	RemuxAfter      bool     `json:"remux_after"`
	StreamlinkBin   string   `json:"streamlink_bin"`
	FFmpegBin       string   `json:"ffmpeg_bin"`
	UpdateChannel   string   `json:"update_channel"` // "" = installed channel
	UpdateAuto      bool     `json:"update_auto"`
	NotifyOff       []string `json:"notify_off"`
	GotifyOn        bool     `json:"gotify_on"`
	GotifyURL       string   `json:"gotify_url"`
	HasGotifyToken  bool     `json:"has_gotify_token"`
	GotifyTypes     []string `json:"gotify_types"`
	GotifyLang      string   `json:"gotify_lang"`

	TelegramOn       bool     `json:"telegram_on"`
	HasTelegramToken bool     `json:"has_telegram_token"`
	TelegramUsers    []int64  `json:"telegram_users"`
	TelegramControl  bool     `json:"telegram_control"`
	TelegramTypes    []string `json:"telegram_types"`
	TelegramLang     string   `json:"telegram_lang"`

	FTPOn         bool   `json:"ftp_on"`
	FTPPort       int    `json:"ftp_port"`
	FTPUser       string `json:"ftp_user"`
	HasFTPPass    bool   `json:"has_ftp_pass"`
	FTPReadOnly   bool   `json:"ftp_readonly"`
	FTPRequireTLS bool   `json:"ftp_require_tls"`
	FTPPassive    string `json:"ftp_passive"`
	FTPPublicIP   string `json:"ftp_public_ip"`

	CompressCodec    string `json:"compress_codec"`
	CompressLevel    string `json:"compress_level"`
	CompressHeight   int    `json:"compress_height"`
	CompressAutoDays int    `json:"compress_auto_days"`
}

func settingsView(s Settings) SettingsView {
	gt := append([]string{}, s.GotifyTypes...)
	if s.GotifyTypes == nil {
		gt = append([]string{}, notifyTypes...)
	}
	tt := append([]string{}, s.TelegramTypes...)
	if s.TelegramTypes == nil {
		tt = append([]string{}, notifyTypes...)
	}
	port := s.FTPPort
	if port == 0 {
		port = 2121
	}
	return SettingsView{FTPOn: s.FTPOn, FTPPort: port, FTPUser: s.FTPUser, HasFTPPass: s.FTPPass != "", FTPReadOnly: s.FTPReadOnly,
		FTPRequireTLS: s.FTPRequireTLS, FTPPassive: s.FTPPassive, FTPPublicIP: s.FTPPublicIP, TelegramOn: s.TelegramOn, HasTelegramToken: s.TelegramToken != "", TelegramUsers: append([]int64{}, s.TelegramUsers...),
		TelegramControl: s.TelegramControl, TelegramTypes: tt, TelegramLang: s.TelegramLang, HasTwitchToken: s.TwitchToken != "", HasCookies: s.HasCookies, Proxy: s.Proxy,
		DefaultQuality: s.DefaultQuality, DefaultInterval: s.DefaultInterval, DefaultTemplate: s.DefaultTemplate,
		RemuxAfter: s.RemuxAfter, StreamlinkBin: s.StreamlinkBin, FFmpegBin: s.FFmpegBin, UpdateChannel: s.UpdateChannel,
		UpdateAuto: !s.UpdateCheckOff, NotifyOff: append([]string{}, s.NotifyOff...),
		GotifyOn: s.GotifyOn, GotifyURL: s.GotifyURL, HasGotifyToken: s.GotifyToken != "", GotifyTypes: gt, GotifyLang: s.GotifyLang,
		CompressCodec: orDefault(s.CompressCodec, "h264"), CompressLevel: orDefault(s.CompressLevel, "balanced"),
		CompressHeight: s.CompressHeight, CompressAutoDays: s.CompressAutoDays}
}

// ----------------------------------------------------------- recordings

type RecInfo struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	Started   time.Time `json:"started"` // recording start (metadata > date in name > mtime)
	Streamer  string    `json:"streamer"`
	Platform  string    `json:"platform"`
	Title     string    `json:"title,omitempty"`
	Ext       string    `json:"ext"`
	Recording bool      `json:"recording"`
}

// listRecordings builds the library. Streamer / platform / start time come from
// the metadata saved when the recording started, so they do not depend on the
// file-name template. Files without metadata fall back to guessLegacy.
func (a *API) listRecordings() []RecInfo {
	entries, _ := os.ReadDir(a.m.cfg.RecDir)
	channels := a.m.st.Channels()
	byID := channelIndex(channels)
	metas := a.m.meta.snapshot()
	out := []RecInfo{}
	for _, e := range entries {
		if e.IsDir() || !isMediaFile(e.Name()) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		ri := RecInfo{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime(),
			Ext: strings.TrimPrefix(strings.ToLower(filepath.Ext(e.Name())), "."), Recording: a.m.ActiveFile(e.Name())}
		ri.Streamer, ri.Platform, ri.Title, ri.Started = mediaInfo(e.Name(), e.Name(), fi.ModTime(), metas, channels, byID)
		out = append(out, ri)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// -------------------------------------------------------------- handlers

func (a *API) state(w http.ResponseWriter, r *http.Request) {
	chs := a.m.st.Channels()
	views := make([]ChannelView, 0, len(chs))
	active := []ChannelView{}
	for _, c := range chs {
		v := a.view(c)
		views = append(views, v)
		if v.Status == StatusRecording {
			active = append(active, v)
		}
	}
	n := 0
	if es, err := os.ReadDir(a.m.cfg.RecDir); err == nil {
		for _, e := range es {
			if !e.IsDir() && isMediaFile(e.Name()) && !strings.HasPrefix(e.Name(), ".") {
				n++
			}
		}
	}
	nSaved := a.m.library().saved
	upd := map[string]any{"available": false}
	if c, ok := a.upd.Cached(); ok && c.Available && c.Latest != nil {
		upd = map[string]any{"available": true, "version": c.Latest.Tag}
	}
	writeJSON(w, 200, map[string]any{"channels": views, "active": active, "jobs": a.m.Jobs(),
		"counts": map[string]int{"channels": len(views), "recordings": n, "saved": nSaved}, "update": upd,
		"notifications": a.m.notes.Summary(), "icons": a.customIcons()})
}

type ChannelInput struct {
	ID               string  `json:"id"`
	URL              *string `json:"url"`
	Name             *string `json:"name"`
	Quality          *string `json:"quality"`
	AutoRecord       *bool   `json:"auto_record"`
	CheckInterval    *int    `json:"check_interval"`
	FilenameTemplate *string `json:"filename_template"`
	AuthMode         *string `json:"auth_mode"`
	OAuthToken       *string `json:"oauth_token"` // nil = keep, "" = clear
	DisableAds       *bool   `json:"disable_ads"`
	LowLatency       *bool   `json:"low_latency"`
	Proxy            *string `json:"proxy"`
	Headers          *string `json:"headers"`
	Email            *string `json:"email"`
	Password         *string `json:"password"` // nil = keep, "" = clear
	ExtraArgs        *string `json:"extra_args"`
}

func applyInput(ch *Channel, in ChannelInput) error {
	if in.URL != nil {
		u, err := normalizeURL(*in.URL)
		if err != nil {
			return err
		}
		if u != ch.URL {
			ch.Plugin = "" // re-detected on next check
		}
		ch.URL = u
	}
	str := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	flag := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	str(&ch.Name, in.Name)
	str(&ch.Quality, in.Quality)
	str(&ch.FilenameTemplate, in.FilenameTemplate)
	str(&ch.Proxy, in.Proxy)
	str(&ch.Email, in.Email)
	str(&ch.ExtraArgs, in.ExtraArgs)
	if in.Headers != nil {
		ch.Headers = strings.TrimSpace(*in.Headers)
	}
	if in.OAuthToken != nil {
		ch.OAuthToken = strings.TrimSpace(*in.OAuthToken)
	}
	if in.Password != nil {
		ch.Password = *in.Password
	}
	flag(&ch.AutoRecord, in.AutoRecord)
	flag(&ch.DisableAds, in.DisableAds)
	flag(&ch.LowLatency, in.LowLatency)
	if in.AuthMode != nil {
		if *in.AuthMode != "global" && *in.AuthMode != "custom" {
			return errBad("auth_mode must be global or custom")
		}
		ch.AuthMode = *in.AuthMode
	}
	if in.CheckInterval != nil {
		if *in.CheckInterval != 0 && *in.CheckInterval < 15 {
			return errBad("check interval must be at least 15 seconds")
		}
		ch.CheckInterval = *in.CheckInterval
	}
	if ch.AuthMode == "" {
		ch.AuthMode = "global"
	}
	return nil
}

func (a *API) createChannel(w http.ResponseWriter, r *http.Request) {
	var in ChannelInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	ch := Channel{ID: newID(), AuthMode: "global", AutoRecord: true, CreatedAt: time.Now()}
	if in.URL == nil {
		writeErr(w, errBad("url is required"))
		return
	}
	if err := applyInput(&ch, in); err != nil {
		writeErr(w, err)
		return
	}
	if ch.Plugin == "" {
		ch.Plugin = detectPlatform(ch.URL)
	}
	if err := a.m.st.PutChannel(ch); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, a.view(ch))
}

func (a *API) updateChannel(w http.ResponseWriter, r *http.Request) {
	ch, ok := a.m.st.Channel(r.PathValue("id"))
	if !ok {
		writeErr(w, &httpError{404, "channel not found"})
		return
	}
	var in ChannelInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := applyInput(&ch, in); err != nil {
		writeErr(w, err)
		return
	}
	if ch.Plugin == "" {
		ch.Plugin = detectPlatform(ch.URL)
	}
	if err := a.m.st.PutChannel(ch); err != nil {
		writeErr(w, err)
		return
	}
	a.m.ForceCheck(ch.ID)
	writeJSON(w, 200, a.view(ch))
}

func (a *API) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.m.st.Channel(id); !ok {
		writeErr(w, &httpError{404, "channel not found"})
		return
	}
	if err := a.m.DeleteChannel(id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testChannel checks a (possibly unsaved) channel configuration.
func (a *API) testChannel(w http.ResponseWriter, r *http.Request) {
	var in ChannelInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	ch := Channel{ID: "test", AuthMode: "global"}
	if in.ID != "" {
		stored, ok := a.m.st.Channel(in.ID)
		if !ok {
			writeErr(w, &httpError{404, "channel not found"})
			return
		}
		ch = stored
	}
	if err := applyInput(&ch, in); err != nil {
		writeErr(w, err)
		return
	}
	if ch.URL == "" {
		writeErr(w, errBad("url is required"))
		return
	}
	if ch.Plugin == "" {
		ch.Plugin = detectPlatform(ch.URL)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	res := a.m.check(ctx, ch)
	if res.Live { // show which stream would be recorded for the configured quality
		res.Requested = a.m.effectiveQuality(ch)
		res.Resolved = resolveQuality(res.Requested, res.Streams).Name
	}
	writeJSON(w, 200, res)
}

func (a *API) channelAction(act string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := a.m.st.Channel(id); !ok {
			writeErr(w, &httpError{404, "channel not found"})
			return
		}
		var err error
		switch act {
		case "record":
			err = a.m.StartRecording(id)
		case "stop":
			err = a.m.StopRecording(id)
		case "check":
			a.m.ForceCheck(id)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		ch, _ := a.m.st.Channel(id)
		writeJSON(w, 200, a.view(ch))
	}
}

func (a *API) putCookies(global bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := "global"
		if !global {
			id = r.PathValue("id")
			if _, ok := a.m.st.Channel(id); !ok {
				writeErr(w, &httpError{404, "channel not found"})
				return
			}
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			writeErr(w, errBad("body too large"))
			return
		}
		if !validCookiesFile(b) {
			writeErr(w, errBad("not a Netscape cookies.txt file"))
			return
		}
		_ = ensureDir(filepath.Join(a.m.cfg.DataDir, "cookies"))
		if err := os.WriteFile(cookiePath(a.m.cfg.DataDir, id), b, 0o600); err != nil {
			writeErr(w, err)
			return
		}
		a.setCookieFlag(id, true)
		a.cookiesResponse(w, id)
	}
}

func (a *API) deleteCookies(global bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := "global"
		if !global {
			id = r.PathValue("id")
			if _, ok := a.m.st.Channel(id); !ok {
				writeErr(w, &httpError{404, "channel not found"})
				return
			}
		}
		_ = os.Remove(cookiePath(a.m.cfg.DataDir, id))
		a.setCookieFlag(id, false)
		a.cookiesResponse(w, id)
	}
}

func (a *API) setCookieFlag(id string, v bool) {
	if id == "global" {
		s := a.m.st.Settings()
		s.HasCookies = v
		_ = a.m.st.SetSettings(s)
		return
	}
	if ch, ok := a.m.st.Channel(id); ok {
		ch.HasCookies = v
		_ = a.m.st.PutChannel(ch)
		a.m.ForceCheck(id)
	}
}

func (a *API) cookiesResponse(w http.ResponseWriter, id string) {
	if id == "global" {
		writeJSON(w, 200, settingsView(a.m.st.Settings()))
		return
	}
	ch, _ := a.m.st.Channel(id)
	writeJSON(w, 200, a.view(ch))
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, settingsView(a.m.st.Settings()))
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TwitchToken     *string   `json:"twitch_token"` // nil = keep, "" = clear
		Proxy           *string   `json:"proxy"`
		DefaultQuality  *string   `json:"default_quality"`
		DefaultInterval *int      `json:"default_interval"`
		DefaultTemplate *string   `json:"default_template"`
		RemuxAfter      *bool     `json:"remux_after"`
		StreamlinkBin   *string   `json:"streamlink_bin"`
		FFmpegBin       *string   `json:"ffmpeg_bin"`
		UpdateChannel   *string   `json:"update_channel"`
		UpdateAuto      *bool     `json:"update_auto"`
		NotifyOff       *[]string `json:"notify_off"`
		GotifyOn        *bool     `json:"gotify_on"`
		GotifyURL       *string   `json:"gotify_url"`
		GotifyToken     *string   `json:"gotify_token"` // nil = keep, "" = clear
		GotifyTypes     *[]string `json:"gotify_types"`
		GotifyLang      *string   `json:"gotify_lang"`

		TelegramOn      *bool     `json:"telegram_on"`
		TelegramToken   *string   `json:"telegram_token"` // nil = keep, "" = clear
		TelegramUsers   *[]int64  `json:"telegram_users"`
		TelegramControl *bool     `json:"telegram_control"`
		TelegramTypes   *[]string `json:"telegram_types"`
		TelegramLang    *string   `json:"telegram_lang"`
		FTPOn           *bool     `json:"ftp_on"`
		FTPPort         *int      `json:"ftp_port"`
		FTPUser         *string   `json:"ftp_user"`
		FTPPass         *string   `json:"ftp_pass"` // nil = keep, "" = clear
		FTPReadOnly     *bool     `json:"ftp_readonly"`
		FTPRequireTLS   *bool     `json:"ftp_require_tls"`
		FTPPassive      *string   `json:"ftp_passive"`
		FTPPublicIP     *string   `json:"ftp_public_ip"`

		CompressCodec    *string `json:"compress_codec"`
		CompressLevel    *string `json:"compress_level"`
		CompressHeight   *int    `json:"compress_height"`
		CompressAutoDays *int    `json:"compress_auto_days"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s := a.m.st.Settings()
	if in.TwitchToken != nil {
		s.TwitchToken = strings.TrimSpace(*in.TwitchToken)
	}
	if in.Proxy != nil {
		s.Proxy = strings.TrimSpace(*in.Proxy)
	}
	if in.DefaultQuality != nil && strings.TrimSpace(*in.DefaultQuality) != "" {
		s.DefaultQuality = strings.TrimSpace(*in.DefaultQuality)
	}
	if in.DefaultInterval != nil {
		if *in.DefaultInterval < 15 {
			writeErr(w, errBad("check interval must be at least 15 seconds"))
			return
		}
		s.DefaultInterval = *in.DefaultInterval
	}
	if in.DefaultTemplate != nil && strings.TrimSpace(*in.DefaultTemplate) != "" {
		s.DefaultTemplate = strings.TrimSpace(*in.DefaultTemplate)
	}
	if in.RemuxAfter != nil {
		s.RemuxAfter = *in.RemuxAfter
	}
	if in.StreamlinkBin != nil {
		s.StreamlinkBin = strings.TrimSpace(*in.StreamlinkBin)
	}
	if in.FFmpegBin != nil {
		s.FFmpegBin = strings.TrimSpace(*in.FFmpegBin)
	}
	if in.UpdateChannel != nil {
		c := strings.ToLower(strings.TrimSpace(*in.UpdateChannel))
		if c != "" && !validChannel(c) {
			writeErr(w, errBad("update channel must be stable, beta or nightly"))
			return
		}
		if c != s.UpdateChannel {
			s.UpdateChannel = c
			a.upd.Invalidate()
		}
	}
	if in.NotifyOff != nil {
		off := []string{}
		for _, t := range *in.NotifyOff {
			if !validNotifyType(t) {
				writeErr(w, errBad("unknown notification type: "+t))
				return
			}
			off = append(off, t)
		}
		s.NotifyOff = off
	}
	if in.GotifyURL != nil {
		u := strings.TrimSpace(*in.GotifyURL)
		if u != "" {
			c, err := cleanGotifyURL(u)
			if err != nil {
				writeErr(w, errBad(err.Error()))
				return
			}
			u = c
		}
		s.GotifyURL = u
	}
	if in.GotifyToken != nil {
		s.GotifyToken = strings.TrimSpace(*in.GotifyToken)
	}
	if in.GotifyLang != nil {
		l := strings.TrimSpace(*in.GotifyLang)
		if l != "" && !locs.Has(l) {
			writeErr(w, errBad("unknown language: "+l))
			return
		}
		s.GotifyLang = l
	}
	if in.GotifyTypes != nil {
		gt := []string{}
		for _, t := range *in.GotifyTypes {
			if !validNotifyType(t) {
				writeErr(w, errBad("unknown notification type: "+t))
				return
			}
			gt = append(gt, t)
		}
		s.GotifyTypes = gt
	}
	if in.GotifyOn != nil {
		s.GotifyOn = *in.GotifyOn
	}
	if s.GotifyOn && (s.GotifyURL == "" || s.GotifyToken == "") {
		writeErr(w, errBad("Gotify needs a server URL and an application token"))
		return
	}
	if in.TelegramToken != nil {
		tok := strings.TrimSpace(*in.TelegramToken)
		if tok != "" && !tgTokenRe.MatchString(tok) {
			writeErr(w, errBad("that does not look like a Telegram bot token (123456:ABC...)"))
			return
		}
		s.TelegramToken = tok
	}
	if in.TelegramUsers != nil {
		seen := map[int64]bool{}
		users := []int64{}
		for _, id := range *in.TelegramUsers {
			if id <= 0 {
				writeErr(w, errBad("Telegram user IDs are positive numbers"))
				return
			}
			if !seen[id] {
				seen[id] = true
				users = append(users, id)
			}
		}
		if len(users) > 50 {
			writeErr(w, errBad("at most 50 Telegram users"))
			return
		}
		s.TelegramUsers = users
	}
	if in.TelegramTypes != nil {
		tt := []string{}
		for _, ty := range *in.TelegramTypes {
			if !validNotifyType(ty) {
				writeErr(w, errBad("unknown notification type: "+ty))
				return
			}
			tt = append(tt, ty)
		}
		s.TelegramTypes = tt
	}
	if in.TelegramLang != nil {
		l := strings.TrimSpace(*in.TelegramLang)
		if l != "" && !locs.Has(l) {
			writeErr(w, errBad("unknown language: "+l))
			return
		}
		s.TelegramLang = l
	}
	if in.TelegramControl != nil {
		s.TelegramControl = *in.TelegramControl
	}
	if in.TelegramOn != nil {
		s.TelegramOn = *in.TelegramOn
	}
	if s.TelegramOn && s.TelegramToken == "" {
		writeErr(w, errBad("the Telegram bot needs a token"))
		return
	}
	if in.FTPPort != nil {
		if *in.FTPPort < 1 || *in.FTPPort > 65535 {
			writeErr(w, errBad("FTP port must be 1-65535"))
			return
		}
		s.FTPPort = *in.FTPPort
	}
	if in.FTPUser != nil {
		u := strings.TrimSpace(*in.FTPUser)
		if len(u) > 64 || strings.ContainsAny(u, " \t\r\n\x00") {
			writeErr(w, errBad("FTP user name: up to 64 characters, no spaces"))
			return
		}
		s.FTPUser = u
	}
	if in.FTPPass != nil {
		if p := *in.FTPPass; p != "" && (len(p) < 8 || len(p) > 128 || strings.ContainsAny(p, "\r\n\x00")) {
			writeErr(w, errBad("FTP password must be 8-128 characters"))
			return
		}
		s.FTPPass = *in.FTPPass
	}
	if in.FTPPassive != nil {
		if _, _, err := parsePassiveRange(*in.FTPPassive); err != nil {
			writeErr(w, errBad(err.Error()))
			return
		}
		s.FTPPassive = strings.TrimSpace(*in.FTPPassive)
	}
	if in.FTPPublicIP != nil {
		ip := strings.TrimSpace(*in.FTPPublicIP)
		if ip != "" && net.ParseIP(ip).To4() == nil {
			writeErr(w, errBad("public IP must be an IPv4 address"))
			return
		}
		s.FTPPublicIP = ip
	}
	if in.FTPReadOnly != nil {
		s.FTPReadOnly = *in.FTPReadOnly
	}
	if in.FTPRequireTLS != nil {
		s.FTPRequireTLS = *in.FTPRequireTLS
	}
	if in.FTPOn != nil {
		s.FTPOn = *in.FTPOn
	}
	if s.FTPOn {
		if s.FTPPort == 0 {
			s.FTPPort = 2121
		}
		if s.FTPUser == "" || s.FTPPass == "" {
			writeErr(w, errBad("the FTP server needs a user name and a password"))
			return
		}
	}
	if in.CompressCodec != nil {
		s.CompressCodec = *in.CompressCodec
	}
	if in.CompressLevel != nil {
		s.CompressLevel = *in.CompressLevel
	}
	if in.CompressHeight != nil {
		s.CompressHeight = *in.CompressHeight
	}
	if in.CompressAutoDays != nil {
		if *in.CompressAutoDays < 0 || *in.CompressAutoDays > 3650 {
			writeErr(w, errBad("auto-compress age must be between 0 and 3650 days"))
			return
		}
		s.CompressAutoDays = *in.CompressAutoDays
	}
	if in.CompressCodec != nil || in.CompressLevel != nil || in.CompressHeight != nil {
		chk := CompressOpts{Codec: s.CompressCodec, Level: s.CompressLevel, MaxHeight: s.CompressHeight}
		if err := chk.normalize(); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.UpdateAuto != nil {
		s.UpdateCheckOff = !*in.UpdateAuto
	}
	if err := a.m.st.SetSettings(s); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, settingsView(s))
}

func (a *API) version(w http.ResponseWriter, r *http.Request) {
	tool := func(bin string, args ...string) map[string]any {
		path, err := exec.LookPath(bin)
		if err != nil {
			return map[string]any{"found": false}
		}
		out, _ := exec.Command(path, args...).CombinedOutput()
		return map[string]any{"found": true, "version": lastLineFirst(string(out))}
	}
	writeJSON(w, 200, map[string]any{
		"version": version, "commit": currentCommit(), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"streamlink": tool(a.m.streamlinkBin(), "--version"),
		"ffmpeg":     tool(a.m.ffmpegBin(), "-version"),
	})
}

func lastLineFirst(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func (a *API) recordings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.listRecordings())
}

func (a *API) recPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !safeRecordingName(name) {
		writeErr(w, errBad("invalid file name"))
		return "", false
	}
	p := filepath.Join(a.m.cfg.RecDir, name)
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		writeErr(w, &httpError{404, "file not found"})
		return "", false
	}
	return p, true
}

func (a *API) streamRec(download bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.recPath(w, r)
		if !ok {
			return
		}
		if download {
			w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(p)))
		}
		http.ServeFile(w, r, p) // supports Range requests (seeking)
	}
}

func (a *API) deleteRec(w http.ResponseWriter, r *http.Request) {
	p, ok := a.recPath(w, r)
	if !ok {
		return
	}
	if a.m.ActiveFile(filepath.Base(p)) {
		writeErr(w, &httpError{409, "file is being recorded"})
		return
	}
	if err := os.Remove(p); err != nil {
		writeErr(w, err)
		return
	}
	a.m.meta.remove(filepath.Base(p))
	w.WriteHeader(http.StatusNoContent)
}

// recNames validates a list of file names coming from the client.
func (a *API) recNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, errBad("no files selected")
	}
	if len(names) > 200 {
		return nil, errBad("too many files selected")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if isSavedRef(n) {
			if rel, err := cleanRel(strings.TrimPrefix(n, savedPrefix)); err != nil || rel == "" {
				return nil, errBad("invalid file name")
			}
		} else if !safeRecordingName(n) {
			return nil, errBad("invalid file name")
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// bulkDeleteRecs deletes several recordings; files that are being recorded are skipped.
func (a *API) bulkDeleteRecs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names []string `json:"names"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	names, err := a.recNames(in.Names)
	if err != nil {
		writeErr(w, err)
		return
	}
	deleted, failed := []string{}, []string{}
	for _, n := range names {
		if a.m.ActiveFile(n) {
			failed = append(failed, n)
			continue
		}
		if err := os.Remove(filepath.Join(a.m.cfg.RecDir, n)); err != nil && !os.IsNotExist(err) {
			failed = append(failed, n)
			continue
		}
		deleted = append(deleted, n)
	}
	a.m.meta.remove(deleted...)
	writeJSON(w, 200, map[string]any{"deleted": deleted, "failed": failed})
}

// saveRecs moves recordings into the Saved file manager (optionally into a folder).
func (a *API) saveRecs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names []string `json:"names"`
		Dest  string   `json:"dest"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	names, err := a.recNames(in.Names)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.m.SaveRecordings(names, in.Dest)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.m.lib.invalidate()
	writeJSON(w, 200, res)
}

// mergeRecs joins several files (recordings or Saved files) into one, oldest
// first, without re-encoding. dest (optional) is a Saved folder for the result.
func (a *API) mergeRecs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names         []string `json:"names"`
		DeleteSources bool     `json:"delete_sources"`
		Dest          *string  `json:"dest"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	names, err := a.recNames(in.Names)
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(names) < 2 {
		writeErr(w, errBad("select at least two files to merge"))
		return
	}
	items := make([]RecInfo, 0, len(names))
	for _, n := range names {
		ri, err := a.m.infoFor(n)
		if err != nil {
			writeErr(w, err)
			return
		}
		items = append(items, ri)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Started.Before(items[j].Started) }) // oldest first
	j, err := a.m.Merge(items, in.DeleteSources, in.Dest)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j)
}

// compressRecs queues re-encode jobs (x264/x265) for the selected recordings.
func (a *API) compressRecs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names []string `json:"names"`
		CompressOpts
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	names, err := a.recNames(in.Names)
	if err != nil {
		writeErr(w, err)
		return
	}
	jobs, err := a.m.Compress(names, in.CompressOpts)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, jobs)
}

func (a *API) convertRec(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.recPath(w, r); !ok {
		return
	}
	j, err := a.m.Convert(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j)
}

func (a *API) trimRec(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.recPath(w, r); !ok {
		return
	}
	var in struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Mode  string  `json:"mode"`
		Dest  *string `json:"dest"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	j, err := a.m.Trim(r.PathValue("name"), in.Start, in.End, in.Mode, in.Dest)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j)
}

// Routes builds the HTTP handler (API + embedded UI).
func (a *API) Routes(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/status", a.auth.status)
	mux.HandleFunc("POST /api/login", a.auth.login)
	mux.HandleFunc("/api/logout", a.auth.logout)
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("GET /api/version", a.version)
	mux.HandleFunc("GET /api/stats", a.stats)
	mux.HandleFunc("GET /api/notifications", a.listNotifications)
	mux.HandleFunc("POST /api/notifications/read", a.readNotifications)
	mux.HandleFunc("DELETE /api/notifications", a.clearNotifications)
	mux.HandleFunc("GET /api/ftp", a.ftpStatus)
	mux.HandleFunc("GET /api/telegram", a.telegramStatus)
	mux.HandleFunc("POST /api/telegram/test", a.telegramTest)
	mux.HandleFunc("GET /api/gotify", a.gotifyStatus)
	mux.HandleFunc("POST /api/gotify/test", a.gotifyTest)
	mux.HandleFunc("GET /icons/{file}", a.serveIcon)
	mux.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.upd.Status()) })
	mux.HandleFunc("POST /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		a.upd.Check(r.Context(), true)
		writeJSON(w, 200, a.upd.Status())
	})
	mux.HandleFunc("POST /api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if err := a.upd.Apply(); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 202, a.upd.Status())
	})
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("PUT /api/settings", a.putSettings)
	mux.HandleFunc("PUT /api/settings/cookies", a.putCookies(true))
	mux.HandleFunc("DELETE /api/settings/cookies", a.deleteCookies(true))

	mux.HandleFunc("POST /api/channels", a.createChannel)
	mux.HandleFunc("POST /api/channels/test", a.testChannel)
	mux.HandleFunc("PUT /api/channels/{id}", a.updateChannel)
	mux.HandleFunc("DELETE /api/channels/{id}", a.deleteChannel)
	mux.HandleFunc("POST /api/channels/{id}/record", a.channelAction("record"))
	mux.HandleFunc("POST /api/channels/{id}/stop", a.channelAction("stop"))
	mux.HandleFunc("POST /api/channels/{id}/check", a.channelAction("check"))
	mux.HandleFunc("PUT /api/channels/{id}/cookies", a.putCookies(false))
	mux.HandleFunc("DELETE /api/channels/{id}/cookies", a.deleteCookies(false))

	mux.HandleFunc("GET /api/recordings", a.recordings)
	mux.HandleFunc("GET /api/recordings/{name}/stream", a.streamRec(false))
	mux.HandleFunc("GET /api/recordings/{name}/download", a.streamRec(true))
	mux.HandleFunc("POST /api/recordings/delete", a.bulkDeleteRecs)
	mux.HandleFunc("POST /api/recordings/save", a.saveRecs)
	mux.HandleFunc("POST /api/recordings/merge", a.mergeRecs)
	mux.HandleFunc("POST /api/recordings/compress", a.compressRecs)
	mux.HandleFunc("DELETE /api/recordings/{name}", a.deleteRec)
	mux.HandleFunc("POST /api/recordings/{name}/convert", a.convertRec)
	mux.HandleFunc("POST /api/recordings/{name}/trim", a.trimRec)
	mux.HandleFunc("GET /api/files", a.listFiles)
	mux.HandleFunc("GET /api/files/tree", a.filesTree)
	mux.HandleFunc("GET /api/files/zip", a.filesZip)
	mux.HandleFunc("GET /api/files/stream/{path...}", a.filesServe(false))
	mux.HandleFunc("GET /api/files/download/{path...}", a.filesServe(true))
	mux.HandleFunc("POST /api/files/mkdir", a.filesMkdir)
	mux.HandleFunc("POST /api/files/rename", a.filesRename)
	mux.HandleFunc("POST /api/files/move", a.filesTransfer(false))
	mux.HandleFunc("POST /api/files/copy", a.filesTransfer(true))
	mux.HandleFunc("POST /api/files/delete", a.filesDelete)
	mux.HandleFunc("POST /api/files/restore", a.filesRestore)
	mux.HandleFunc("POST /api/files/upload", a.filesUpload)
	mux.HandleFunc("POST /api/files/trim", a.filesTrim)
	mux.HandleFunc("POST /api/files/convert", a.filesConvert)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.m.Jobs()) })
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /api/events", a.listEvents)
	mux.HandleFunc("DELETE /api/events", a.clearEvents)
	mux.Handle("/", static)
	return mux
}
