package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Addr    string
	DataDir string
	RecDir  string
}

const (
	StatusOffline   = "offline"
	StatusChecking  = "checking"
	StatusLive      = "live"      // live, but not recording
	StatusRecording = "recording" // live and recording
	StatusAuthError = "auth_error"
	StatusError     = "error"
)

// Rec describes a running recording.
type Rec struct {
	ChannelID string
	TmpName   string // file being written (always .ts)
	Final     string // final file name after remux
	Started   time.Time
	Quality   string
	cancel    context.CancelFunc
	done      chan struct{}
	stopped   bool
}

type Runtime struct {
	Status    string
	Error     string
	LastCheck time.Time
	Title     string
	checking  bool
	suppress  bool // manually stopped: do not auto-restart until the stream goes offline
	rec       *Rec
}

type CheckResult struct {
	Live      bool     `json:"live"`
	Plugin    string   `json:"plugin"`
	Streams   []string `json:"streams"`
	Title     string   `json:"title"`
	Error     string   `json:"error"`
	AuthError bool     `json:"auth_error"`
}

type Manager struct {
	cfg  Config
	st   *Store
	mu   sync.Mutex
	rt   map[string]*Runtime
	jobs []*Job
	wg   sync.WaitGroup
	ctx  context.Context
}

func NewManager(cfg Config, st *Store) *Manager {
	return &Manager{cfg: cfg, st: st, rt: map[string]*Runtime{}}
}

func (m *Manager) streamlinkBin() string {
	if s := m.st.Settings().StreamlinkBin; s != "" {
		return s
	}
	if e := os.Getenv("STREAMLINK_BIN"); e != "" {
		return e
	}
	return "streamlink"
}

func (m *Manager) ffmpegBin() string {
	if s := m.st.Settings().FFmpegBin; s != "" {
		return s
	}
	if e := os.Getenv("FFMPEG_BIN"); e != "" {
		return e
	}
	return "ffmpeg"
}

func (m *Manager) ffprobeBin() string {
	b := m.ffmpegBin()
	base := filepath.Base(b)
	if strings.Contains(base, "ffmpeg") {
		return filepath.Join(filepath.Dir(b), strings.Replace(base, "ffmpeg", "ffprobe", 1))
	}
	return "ffprobe"
}

// rtLocked returns (creating if needed) the runtime of a channel. m.mu must be held.
func (m *Manager) rtLocked(id string) *Runtime {
	r := m.rt[id]
	if r == nil {
		r = &Runtime{Status: StatusOffline}
		m.rt[id] = r
	}
	return r
}

// ---------------------------------------------------------------- poller

// Run is the channel poller: every couple of seconds it starts checks for
// channels whose interval has elapsed. It blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.ctx = ctx
	sem := make(chan struct{}, 4)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		m.tick(ctx, sem)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) tick(ctx context.Context, sem chan struct{}) {
	def := m.st.Settings().DefaultInterval
	for _, ch := range m.st.Channels() {
		iv := ch.CheckInterval
		if iv <= 0 {
			iv = def
		}
		if iv < 15 {
			iv = 15
		}
		m.mu.Lock()
		rt := m.rtLocked(ch.ID)
		skip := rt.rec != nil || rt.checking ||
			(!rt.LastCheck.IsZero() && time.Since(rt.LastCheck) < time.Duration(iv)*time.Second)
		if !skip {
			rt.checking = true
			rt.Status = StatusChecking
		}
		m.mu.Unlock()
		if skip {
			continue
		}
		m.wg.Add(1)
		go func(ch Channel) {
			defer m.wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			res := m.check(ctx, ch)
			m.applyCheck(ch, res)
		}(ch)
	}
}

func (m *Manager) applyCheck(ch Channel, res CheckResult) {
	// remember the detected plugin
	if res.Plugin != "" {
		if cur, ok := m.st.Channel(ch.ID); ok && cur.Plugin != res.Plugin {
			cur.Plugin = res.Plugin
			_ = m.st.PutChannel(cur)
		}
	}
	m.mu.Lock()
	rt := m.rtLocked(ch.ID)
	rt.checking = false
	rt.LastCheck = time.Now()
	rt.Error = ""
	if rt.rec != nil { // recording started manually while the check was running
		m.mu.Unlock()
		return
	}
	switch {
	case res.Live:
		rt.Title = res.Title
		rt.Status = StatusLive
		start := ch.AutoRecord && !rt.suppress
		m.mu.Unlock()
		if start {
			if err := m.StartRecording(ch.ID); err != nil {
				log.Printf("[%s] auto-record: %v", streamerName(ch), err)
			}
		}
		return
	case res.AuthError:
		rt.Status = StatusAuthError
		rt.Error = res.Error
	case res.Error != "":
		rt.Status = StatusError
		rt.Error = res.Error
	default:
		rt.Status = StatusOffline
		rt.Title = ""
		rt.suppress = false
	}
	m.mu.Unlock()
}

// ForceCheck makes the poller re-check the channel on its next tick.
func (m *Manager) ForceCheck(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt := m.rt[id]; rt != nil && !rt.checking {
		rt.LastCheck = time.Time{}
	}
}

// -------------------------------------------------------- streamlink args

var authHints = []string{"401", "403", "unauthorized", "forbidden", "login", "log in", "sign in",
	"authenticat", "oauth", "token", "members-only", "members only", "subscriber", "age-restricted", "age restricted"}

func looksLikeAuthError(s string) bool {
	s = strings.ToLower(s)
	for _, h := range authHints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// authActive reports whether any credential will be used for the channel.
func (m *Manager) authActive(ch Channel) bool {
	st := m.st.Settings()
	plugin := ch.Plugin
	if plugin == "" {
		plugin = detectPlatform(ch.URL)
	}
	if ch.HasCookies || st.HasCookies || ch.Email != "" {
		return true
	}
	if plugin == "twitch" {
		return ch.OAuthToken != "" && ch.AuthMode == "custom" || st.TwitchToken != "" && ch.AuthMode != "custom"
	}
	return false
}

// buildArgs returns the streamlink options for a channel. Secrets (tokens,
// cookies, passwords, proxy credentials) go into a private temporary config
// file passed with --config so they do not show up in the process list.
func (m *Manager) buildArgs(ch Channel) ([]string, func(), error) {
	st := m.st.Settings()
	noop := func() {}
	plugin := ch.Plugin
	if plugin == "" {
		plugin = detectPlatform(ch.URL)
	}
	var args, secret []string

	proxy := ch.Proxy
	if proxy == "" {
		proxy = st.Proxy
	}
	if proxy != "" {
		secret = append(secret, "http-proxy="+proxy)
	}
	for _, line := range strings.Split(ch.Headers, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") { // allow "Name: value"
			if i := strings.Index(line, ":"); i > 0 {
				line = strings.TrimSpace(line[:i]) + "=" + strings.TrimSpace(line[i+1:])
			} else {
				continue
			}
		}
		secret = append(secret, "http-header="+line)
	}

	if plugin == "twitch" {
		tok := st.TwitchToken
		if ch.AuthMode == "custom" {
			tok = ch.OAuthToken
		}
		if tok != "" {
			secret = append(secret, "twitch-api-header=Authorization=OAuth "+tok)
		}
		if ch.DisableAds {
			args = append(args, "--twitch-disable-ads")
		}
		if ch.LowLatency {
			args = append(args, "--twitch-low-latency")
		}
	}

	// Cookies: streamlink has no cookies.txt option, so the Netscape file is
	// converted to --http-cookie entries (only those matching the host).
	cpath := ""
	if ch.HasCookies {
		cpath = cookiePath(m.cfg.DataDir, ch.ID)
	} else if st.HasCookies {
		cpath = cookiePath(m.cfg.DataDir, "global")
	}
	if cpath != "" {
		host := ""
		if u, err := url.Parse(ch.URL); err == nil {
			host = u.Hostname()
		}
		if cs, err := parseNetscapeCookies(cpath, host); err == nil {
			for _, c := range cs {
				secret = append(secret, "http-cookie="+c.Name+"="+c.Value)
			}
		}
	}

	if ch.Email != "" && plugin != "" {
		secret = append(secret, plugin+"-email="+ch.Email)
		if ch.Password != "" {
			secret = append(secret, plugin+"-password="+ch.Password)
		}
	}
	args = append(args, strings.Fields(ch.ExtraArgs)...)

	if len(secret) == 0 {
		return args, noop, nil
	}
	tmpDir := filepath.Join(m.cfg.DataDir, "tmp")
	if err := ensureDir(tmpDir); err != nil {
		return nil, noop, err
	}
	f, err := os.CreateTemp(tmpDir, "run-*.conf")
	if err != nil {
		return nil, noop, err
	}
	_ = f.Chmod(0o600)
	for _, l := range secret {
		fmt.Fprintln(f, oneLine(l))
	}
	f.Close()
	cleanup := func() { _ = os.Remove(f.Name()) }
	return append([]string{"--config", f.Name()}, args...), cleanup, nil
}

// ------------------------------------------------------------------ check

// check asks streamlink whether the channel is live (streamlink --json).
func (m *Manager) check(ctx context.Context, ch Channel) CheckResult {
	args, cleanup, err := m.buildArgs(ch)
	defer cleanup()
	if err != nil {
		return CheckResult{Error: err.Error()}
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	full := append([]string{"--json"}, args...)
	full = append(full, ch.URL)
	cmd := exec.CommandContext(cctx, m.streamlinkBin(), full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrNotFound) {
		return CheckResult{Error: "streamlink not found (install it or set STREAMLINK_BIN)"}
	}
	var j struct {
		Plugin   string                     `json:"plugin"`
		Error    string                     `json:"error"`
		Streams  map[string]json.RawMessage `json:"streams"`
		Metadata struct {
			Title  string `json:"title"`
			Author string `json:"author"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil {
		msg := lastLine(errb.String())
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = "unexpected streamlink output"
		}
		return CheckResult{Error: msg, AuthError: looksLikeAuthError(msg)}
	}
	res := CheckResult{Plugin: j.Plugin, Title: j.Metadata.Title}
	for name := range j.Streams {
		switch name {
		case "best", "worst", "best-unfiltered", "worst-unfiltered":
		default:
			res.Streams = append(res.Streams, name)
		}
	}
	sortQualities(res.Streams)
	if len(j.Streams) > 0 {
		res.Live = true
		return res
	}
	if j.Error != "" {
		low := strings.ToLower(j.Error)
		if strings.Contains(low, "no playable streams") {
			return res // simply offline
		}
		res.Error = j.Error
		res.AuthError = looksLikeAuthError(j.Error)
	}
	return res
}

// -------------------------------------------------------------- recording

func (m *Manager) effectiveQuality(ch Channel) string {
	if ch.Quality != "" {
		return ch.Quality
	}
	if q := m.st.Settings().DefaultQuality; q != "" {
		return q
	}
	return "best"
}

// StartRecording starts recording a channel (it must be live).
func (m *Manager) StartRecording(id string) error {
	ch, ok := m.st.Channel(id)
	if !ok {
		return &httpError{404, "channel not found"}
	}
	m.mu.Lock()
	rt := m.rtLocked(id)
	if rt.rec != nil {
		m.mu.Unlock()
		return &httpError{409, "already recording"}
	}
	rt.suppress = false
	title := rt.Title
	m.mu.Unlock()

	if err := ensureDir(m.cfg.RecDir); err != nil {
		return err
	}
	tpl := ch.FilenameTemplate
	if tpl == "" {
		tpl = m.st.Settings().DefaultTemplate
	}
	final, err := reserveName(m.cfg.RecDir, expandTemplate(tpl, ch, title, time.Now()), ".ts")
	if err != nil {
		return err
	}
	tmp := strings.TrimSuffix(final, filepath.Ext(final)) + ".ts"
	quality := m.effectiveQuality(ch)

	args, cleanup, err := m.buildArgs(ch)
	if err != nil {
		_ = os.Remove(filepath.Join(m.cfg.RecDir, tmp))
		return err
	}
	args = append(args, "--force", "-o", filepath.Join(m.cfg.RecDir, tmp), ch.URL, quality)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, m.streamlinkBin(), args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = 15 * time.Second
	tail := &tailBuf{}
	cmd.Stdout, cmd.Stderr = tail, tail
	if err := cmd.Start(); err != nil {
		cancel()
		cleanup()
		_ = os.Remove(filepath.Join(m.cfg.RecDir, tmp))
		return err
	}
	rec := &Rec{ChannelID: id, TmpName: tmp, Final: final, Started: time.Now(), Quality: quality,
		cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	rt = m.rtLocked(id)
	rt.rec = rec
	rt.Status = StatusRecording
	rt.Error = ""
	m.mu.Unlock()
	log.Printf("[%s] recording started -> %s (%s)", streamerName(ch), tmp, quality)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(rec.done)
		werr := cmd.Wait()
		cleanup()
		cancel()
		m.finishRecording(ch, rec, werr, tail.String())
	}()
	return nil
}

func (m *Manager) finishRecording(ch Channel, rec *Rec, werr error, output string) {
	tmpPath := filepath.Join(m.cfg.RecDir, rec.TmpName)
	size := int64(0)
	if fi, err := os.Stat(tmpPath); err == nil {
		size = fi.Size()
	}
	log.Printf("[%s] recording finished (%d bytes, err=%v)", streamerName(ch), size, werr)

	m.mu.Lock()
	rt := m.rtLocked(ch.ID)
	rt.rec = nil
	rt.LastCheck = time.Time{} // re-check right away
	switch {
	case rec.stopped:
		rt.Status = StatusOffline
		rt.suppress = true
	case size == 0 && werr != nil:
		msg := lastLine(output)
		if msg == "" {
			msg = werr.Error()
		}
		rt.Error = msg
		rt.Status = StatusError
		if looksLikeAuthError(msg) {
			rt.Status = StatusAuthError
		}
		rt.LastCheck = time.Now() // do not hammer a failing channel
	default:
		rt.Status = StatusOffline
	}
	m.mu.Unlock()

	if size == 0 {
		_ = os.Remove(tmpPath)
		return
	}
	if filepath.Ext(rec.Final) == ".ts" || !m.st.Settings().RemuxAfter {
		return // remux disabled (or .ts requested): keep the raw .ts file
	}
	// remux .ts -> final container, then drop the .ts
	j := m.newJob("remux", rec.TmpName, rec.Final)
	if err := m.runFFmpeg(j, remuxArgs(tmpPath, filepath.Join(m.cfg.RecDir, rec.Final)), tmpPath, 0); err == nil {
		_ = os.Remove(tmpPath)
	} else {
		log.Printf("[%s] remux failed, keeping %s: %v", streamerName(ch), rec.TmpName, err)
	}
}

// StopRecording gracefully stops a recording (SIGINT, then kill after 15s).
func (m *Manager) StopRecording(id string) error {
	m.mu.Lock()
	rt := m.rt[id]
	if rt == nil || rt.rec == nil {
		m.mu.Unlock()
		return &httpError{409, "not recording"}
	}
	rec := rt.rec
	rec.stopped = true
	m.mu.Unlock()
	rec.cancel()
	return nil
}

// ActiveFile reports whether a file name belongs to a running recording.
func (m *Manager) ActiveFile(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rt := range m.rt {
		if rt.rec != nil && (rt.rec.TmpName == name || rt.rec.Final == name) {
			return true
		}
	}
	return false
}

// Shutdown stops all recordings and waits for background work.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	for _, rt := range m.rt {
		if rt.rec != nil {
			rt.rec.stopped = true
			rt.rec.cancel()
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Println("shutdown: timed out waiting for background tasks")
	}
}

// DeleteChannel stops its recording and removes it with its cookies.
func (m *Manager) DeleteChannel(id string) error {
	_ = m.StopRecording(id)
	m.mu.Lock()
	delete(m.rt, id)
	m.mu.Unlock()
	_ = os.Remove(cookiePath(m.cfg.DataDir, id))
	return m.st.DeleteChannel(id)
}
