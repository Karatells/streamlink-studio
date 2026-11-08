package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	HasTwitchToken  bool   `json:"has_twitch_token"`
	HasCookies      bool   `json:"has_cookies"`
	Proxy           string `json:"proxy"`
	DefaultQuality  string `json:"default_quality"`
	DefaultInterval int    `json:"default_interval"`
	DefaultTemplate string `json:"default_template"`
	RemuxAfter      bool   `json:"remux_after"`
	StreamlinkBin   string `json:"streamlink_bin"`
	FFmpegBin       string `json:"ffmpeg_bin"`
	UpdateChannel   string `json:"update_channel"` // "" = installed channel
	UpdateAuto      bool   `json:"update_auto"`
}

func settingsView(s Settings) SettingsView {
	return SettingsView{s.TwitchToken != "", s.HasCookies, s.Proxy, s.DefaultQuality, s.DefaultInterval,
		s.DefaultTemplate, s.RemuxAfter, s.StreamlinkBin, s.FFmpegBin, s.UpdateChannel, !s.UpdateCheckOff}
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
	Saved     bool      `json:"saved"`
}

// listRecordings builds the library. Streamer / platform / start time come from
// the metadata saved when the recording started, so they do not depend on the
// file-name template. Files without metadata fall back to guessLegacy.
func (a *API) listRecordings() []RecInfo {
	entries, _ := os.ReadDir(a.m.cfg.RecDir)
	channels := a.m.st.Channels()
	byID := map[string]Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
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
			Ext: strings.TrimPrefix(strings.ToLower(filepath.Ext(e.Name())), "."), Recording: a.m.ActiveFile(e.Name()), Saved: a.m.meta.isSaved(e.Name())}
		if md, ok := metas[e.Name()]; ok && md.Streamer != "" {
			ri.Streamer, ri.Platform, ri.Title, ri.Started = md.Streamer, md.Platform, md.Title, md.Started
			if c, ok := byID[md.ChannelID]; ok { // follow channel renames
				ri.Streamer = streamerName(c)
			}
		} else {
			ri.Streamer, ri.Platform, ri.Started = guessLegacy(e.Name(), fi.ModTime(), channels)
		}
		if ri.Started.IsZero() {
			ri.Started = fi.ModTime()
		}
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
	n, nSaved := 0, 0
	if es, err := os.ReadDir(a.m.cfg.RecDir); err == nil {
		for _, e := range es {
			if e.IsDir() || !isMediaFile(e.Name()) || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if a.m.meta.isSaved(e.Name()) {
				nSaved++
			} else {
				n++
			}
		}
	}
	upd := map[string]any{"available": false}
	if c, ok := a.upd.Cached(); ok && c.Available && c.Latest != nil {
		upd = map[string]any{"available": true, "version": c.Latest.Tag}
	}
	writeJSON(w, 200, map[string]any{"channels": views, "active": active, "jobs": a.m.Jobs(),
		"counts": map[string]int{"channels": len(views), "recordings": n, "saved": nSaved}, "update": upd})
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
		TwitchToken     *string `json:"twitch_token"` // nil = keep, "" = clear
		Proxy           *string `json:"proxy"`
		DefaultQuality  *string `json:"default_quality"`
		DefaultInterval *int    `json:"default_interval"`
		DefaultTemplate *string `json:"default_template"`
		RemuxAfter      *bool   `json:"remux_after"`
		StreamlinkBin   *string `json:"streamlink_bin"`
		FFmpegBin       *string `json:"ffmpeg_bin"`
		UpdateChannel   *string `json:"update_channel"`
		UpdateAuto      *bool   `json:"update_auto"`
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
		if !safeRecordingName(n) {
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

// saveRecs moves recordings to / from the "Saved" tab.
func (a *API) saveRecs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names []string `json:"names"`
		Saved *bool    `json:"saved"`
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
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(a.m.cfg.RecDir, n)); err != nil {
			writeErr(w, &httpError{404, "file not found: " + n})
			return
		}
	}
	a.m.meta.setSaved(names, in.Saved == nil || *in.Saved)
	w.WriteHeader(http.StatusNoContent)
}

// mergeRecs joins several recordings into one, oldest first, without re-encoding.
func (a *API) mergeRecs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names         []string `json:"names"`
		DeleteSources bool     `json:"delete_sources"`
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
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var items []RecInfo
	for _, ri := range a.listRecordings() { // sorted newest first
		if want[ri.Name] {
			items = append(items, ri)
		}
	}
	if len(items) != len(names) {
		writeErr(w, &httpError{404, "some files were not found"})
		return
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Started.Before(items[j].Started) }) // oldest first
	j, err := a.m.Merge(items, in.DeleteSources)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j)
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
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	j, err := a.m.Trim(r.PathValue("name"), in.Start, in.End, in.Mode)
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
	mux.HandleFunc("DELETE /api/recordings/{name}", a.deleteRec)
	mux.HandleFunc("POST /api/recordings/{name}/convert", a.convertRec)
	mux.HandleFunc("POST /api/recordings/{name}/trim", a.trimRec)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.m.Jobs()) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("/", static)
	return mux
}
