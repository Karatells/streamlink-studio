package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRepo      = "Karatells/streamlink-studio"
	defaultAPI       = "https://api.github.com"
	autoCheckEvery   = 6 * time.Hour
	manualCooldown   = 5 * time.Second
	maxArchiveBytes  = 300 << 20
	maxBinaryBytes   = 300 << 20
	maxNotesChars    = 4000
	binaryName       = "streamlink-studio"
	restartExitCode  = 3 // non-zero so systemd restarts us with Restart=on-failure as well as =always
	updateHTTPTimout = 20 * time.Second
)

// restartRequested is set after a successful self-update; main exits with
// restartExitCode once the graceful shutdown has finished.
var restartRequested atomic.Bool

// ------------------------------------------------------------- versions

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

type semver struct {
	major, minor, patch int
	pre                 []string
}

func parseSemver(s string) (semver, bool) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return semver{}, false
	}
	v := semver{}
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

// compareSemver returns -1, 0, 1. A pre-release is lower than its release.
func compareSemver(a, b semver) int {
	cmp := func(x, y int) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	if c := cmp(a.major, b.major); c != 0 {
		return c
	}
	if c := cmp(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmp(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xe := strconv.Atoi(x)
		yn, ye := strconv.Atoi(y)
		switch {
		case xe == nil && ye == nil:
			if c := cmp(xn, yn); c != 0 {
				return c
			}
		case xe == nil: // numeric identifiers sort before alphanumeric ones
			return -1
		case ye == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return cmp(len(a.pre), len(b.pre))
}

func sameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return a[:n] == b[:n]
}

func validChannel(c string) bool { return c == "stable" || c == "beta" || c == "nightly" }

// inferChannel guesses the release channel from a version string.
func inferChannel(v string) string {
	switch sv, ok := parseSemver(v); {
	case ok && len(sv.pre) > 0:
		return "beta"
	case ok:
		return "stable"
	case strings.HasPrefix(v, "main") || strings.HasPrefix(v, "nightly"):
		return "nightly"
	}
	return "stable"
}

// ------------------------------------------------------------- updater

type ReleaseInfo struct {
	Tag        string    `json:"tag"`
	Name       string    `json:"name"`
	Notes      string    `json:"notes"`
	URL        string    `json:"url"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published"`
	Commit     string    `json:"commit,omitempty"`
	assets     map[string]string
}

type UpdateCheck struct {
	CheckedAt time.Time    `json:"checked_at"`
	Channel   string       `json:"channel"`
	Latest    *ReleaseInfo `json:"latest,omitempty"`
	Available bool         `json:"available"`
	Error     string       `json:"error,omitempty"`
}

type ApplyState struct {
	State   string `json:"state"` // idle|downloading|verifying|installing|restarting|restart_manual|error
	Percent int    `json:"percent"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Updater struct {
	m         *Manager
	api, repo string
	exe       string // override of the running executable (tests)
	noRestart bool   // tests: do not signal ourselves
	client    *http.Client

	mu        sync.Mutex
	check     UpdateCheck
	haveCheck bool
	lastCheck time.Time
	apply     ApplyState
	notified  string // version already announced
}

func NewUpdater(m *Manager) *Updater {
	u := &Updater{m: m, api: defaultAPI, repo: defaultRepo, client: &http.Client{Timeout: updateHTTPTimout}}
	if v := os.Getenv("STUDIO_UPDATE_API"); v != "" { // for tests / GitHub Enterprise
		u.api = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("STUDIO_UPDATE_REPO"); v != "" {
		u.repo = v
	}
	u.apply.State = "idle"
	return u
}

// installedChannel is the channel the program was installed from
// (STUDIO_CHANNEL, written by install.sh), else inferred from the version.
func installedChannel() string {
	if c := strings.ToLower(strings.TrimSpace(os.Getenv("STUDIO_CHANNEL"))); validChannel(c) {
		return c
	}
	return inferChannel(version)
}

func (u *Updater) channel() string {
	if c := u.m.st.Settings().UpdateChannel; validChannel(c) {
		return c
	}
	return installedChannel()
}

type ghRelease struct {
	TagName         string    `json:"tag_name"`
	Name            string    `json:"name"`
	Body            string    `json:"body"`
	HTMLURL         string    `json:"html_url"`
	Draft           bool      `json:"draft"`
	Prerelease      bool      `json:"prerelease"`
	PublishedAt     time.Time `json:"published_at"`
	TargetCommitish string    `json:"target_commitish"`
	Assets          []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

var shaInBody = regexp.MustCompile(`@ ([0-9a-fA-F]{7,40})`)
var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func (g ghRelease) info() *ReleaseInfo {
	r := &ReleaseInfo{Tag: g.TagName, Name: g.Name, URL: g.HTMLURL, Prerelease: g.Prerelease,
		Published: g.PublishedAt, assets: map[string]string{}}
	notes := g.Body
	if rs := []rune(notes); len(rs) > maxNotesChars {
		notes = string(rs[:maxNotesChars]) + "…"
	}
	r.Notes = notes
	if fullSHA.MatchString(g.TargetCommitish) {
		r.Commit = strings.ToLower(g.TargetCommitish)
	} else if m := shaInBody.FindStringSubmatch(g.Body); m != nil {
		r.Commit = strings.ToLower(m[1])
	}
	for _, a := range g.Assets {
		r.assets[a.Name] = a.URL
	}
	return r
}

func (u *Updater) getJSON(ctx context.Context, p string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.api+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "streamlink-studio/"+version)
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("no release found (HTTP 404)")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("GitHub rate limit reached, try again later (HTTP %d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// latestFor finds the newest release of a channel.
func (u *Updater) latestFor(ctx context.Context, channel string) (*ReleaseInfo, error) {
	base := "/repos/" + u.repo + "/releases"
	switch channel {
	case "stable":
		var g ghRelease
		if err := u.getJSON(ctx, base+"/latest", &g); err != nil {
			return nil, err
		}
		return g.info(), nil
	case "nightly":
		var g ghRelease
		if err := u.getJSON(ctx, base+"/tags/nightly", &g); err != nil {
			return nil, err
		}
		return g.info(), nil
	}
	// beta: the newest version of all releases, pre-releases included (never "nightly")
	var list []ghRelease
	if err := u.getJSON(ctx, base+"?per_page=30", &list); err != nil {
		return nil, err
	}
	var best *ghRelease
	var bestV semver
	for i := range list {
		g := &list[i]
		if g.Draft || g.TagName == "nightly" {
			continue
		}
		v, ok := parseSemver(g.TagName)
		if !ok {
			continue
		}
		if best == nil || compareSemver(v, bestV) > 0 {
			best, bestV = g, v
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no beta release found")
	}
	return best.info(), nil
}

func buildCommit() string { return currentCommit() }

// isNewer decides whether latest should be offered for the running build.
func isNewer(channel string, latest *ReleaseInfo) bool {
	if version == "dev" { // development build: never nag
		return false
	}
	if channel == "nightly" {
		cur := buildCommit()
		if cur == "" || latest.Commit == "" {
			return false
		}
		return !sameCommit(cur, latest.Commit)
	}
	lv, ok := parseSemver(latest.Tag)
	if !ok {
		return false
	}
	cv, ok := parseSemver(version)
	if !ok { // e.g. a nightly build switching to stable/beta
		return true
	}
	return compareSemver(lv, cv) > 0
}

// Check queries GitHub for the effective channel and caches the result.
func (u *Updater) Check(ctx context.Context, force bool) UpdateCheck {
	u.mu.Lock()
	if !force && u.haveCheck && time.Since(u.lastCheck) < manualCooldown {
		c := u.check
		u.mu.Unlock()
		return c
	}
	u.mu.Unlock()

	ch := u.channel()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res := UpdateCheck{CheckedAt: time.Now(), Channel: ch}
	latest, err := u.latestFor(cctx, ch)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Latest = latest
		res.Available = isNewer(ch, latest)
	}
	u.mu.Lock()
	u.check, u.haveCheck, u.lastCheck = res, true, time.Now()
	u.mu.Unlock()
	u.mu.Lock()
	fresh := res.Available && res.Latest != nil && u.notified != res.Latest.Tag && !u.m.notes.Has("update_available", "version", res.Latest.Tag)
	if fresh {
		u.notified = res.Latest.Tag
	}
	u.mu.Unlock()
	if fresh {
		u.m.notes.Notify("update_available", "", map[string]string{"version": res.Latest.Tag, "channel": ch}, "")
	}
	return res
}

// Invalidate forgets the cached result (after the channel was changed).
func (u *Updater) Invalidate() {
	u.mu.Lock()
	u.haveCheck = false
	u.check = UpdateCheck{}
	u.mu.Unlock()
}

// Cached returns the last check without touching the network.
func (u *Updater) Cached() (UpdateCheck, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.check, u.haveCheck
}

// Run performs the periodic automatic check until ctx is cancelled.
func (u *Updater) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		u.mu.Lock()
		due := !u.haveCheck || time.Since(u.lastCheck) >= autoCheckEvery
		u.mu.Unlock()
		if due && !u.m.st.Settings().UpdateCheckOff {
			if c := u.Check(ctx, true); c.Error != "" {
				log.Printf("update check: %s", c.Error)
			} else if c.Available {
				log.Printf("update available on %s channel: %s", c.Channel, c.Latest.Tag)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ------------------------------------------------------------- applying

func (u *Updater) exePath() (string, error) {
	if u.exe != "" {
		return u.exe, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func assetName() string { return fmt.Sprintf("%s_linux_%s.tar.gz", binaryName, runtime.GOARCH) }

// canUpdate reports whether a one-click update is possible and, if not, why
// (a code the UI translates).
func (u *Updater) canUpdate() (bool, string) {
	if u.exe == "" && runtime.GOOS != "linux" {
		return false, "unsupported_platform"
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return false, "unsupported_platform"
	}
	if version == "dev" && u.exe == "" {
		return false, "dev_build"
	}
	exe, err := u.exePath()
	if err != nil {
		return false, "not_writable"
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".write-test-*")
	if err != nil {
		return false, "not_writable"
	}
	f.Close()
	_ = os.Remove(f.Name())
	return true, ""
}

func (u *Updater) setApply(f func(*ApplyState)) {
	u.mu.Lock()
	f(&u.apply)
	u.mu.Unlock()
}

func (u *Updater) fail(err error) {
	log.Printf("update failed: %v", err)
	u.setApply(func(a *ApplyState) { a.State, a.Error = "error", err.Error() })
}

// Apply starts the update in the background. Progress is read via Status.
func (u *Updater) Apply() error {
	if ok, reason := u.canUpdate(); !ok {
		return &httpError{409, "one-click update unavailable: " + reason}
	}
	u.mu.Lock()
	if s := u.apply.State; s == "downloading" || s == "verifying" || s == "installing" || s == "restarting" {
		u.mu.Unlock()
		return &httpError{409, "an update is already in progress"}
	}
	c, have := u.check, u.haveCheck
	u.mu.Unlock()
	if !have || c.Latest == nil || !c.Available {
		return &httpError{409, "no update available - check for updates first"}
	}
	rel := c.Latest
	u.setApply(func(a *ApplyState) { *a = ApplyState{State: "downloading", Version: rel.Tag} })
	go func() {
		if err := u.run(rel); err != nil {
			u.fail(err)
		}
	}()
	return nil
}

func (u *Updater) allowedDownload(raw string) bool {
	if u.api != defaultAPI { // custom API (tests): trust its URLs
		return true
	}
	return strings.HasPrefix(raw, "https://github.com/"+u.repo+"/releases/download/")
}

func (u *Updater) download(ctx context.Context, raw string, dst string, report func(done, total int64)) error {
	if !u.allowedDownload(raw) {
		return fmt.Errorf("refusing to download from %s", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "streamlink-studio/"+version)
	cl := &http.Client{} // no overall timeout: the archive may be large; ctx bounds it
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var done int64
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if done > maxArchiveBytes {
				return fmt.Errorf("archive is too large")
			}
			if report != nil {
				report(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func expectedHash(checksums, name string) string {
	for _, line := range strings.Split(checksums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// extractBinary pulls the executable out of the release archive.
func extractBinary(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("archive does not contain %q", binaryName)
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != binaryName {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxBinaryBytes+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if n > maxBinaryBytes {
			return fmt.Errorf("binary is too large")
		}
		return os.Chmod(dst, 0o755)
	}
}

func (u *Updater) run(rel *ReleaseInfo) error {
	exe, err := u.exePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	assetURL, sumURL := rel.assets[assetName()], rel.assets["checksums.txt"]
	if assetURL == "" {
		return fmt.Errorf("release %s has no asset %s", rel.Tag, assetName())
	}
	if sumURL == "" {
		return fmt.Errorf("release %s has no checksums.txt - refusing to install an unverified binary", rel.Tag)
	}
	tmp, err := os.MkdirTemp(dir, ".update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	archive := filepath.Join(tmp, assetName())
	last := -1
	err = u.download(ctx, assetURL, archive, func(done, total int64) {
		if total > 0 {
			if p := int(done * 100 / total); p != last {
				last = p
				u.setApply(func(a *ApplyState) { a.Percent = p })
			}
		}
	})
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	u.setApply(func(a *ApplyState) { a.State, a.Percent = "verifying", 100 })
	sumFile := filepath.Join(tmp, "checksums.txt")
	if err := u.download(ctx, sumURL, sumFile, nil); err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	sums, _ := os.ReadFile(sumFile)
	want := expectedHash(string(sums), assetName())
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", assetName())
	}
	got, err := fileSHA256(archive)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checksum mismatch for %s", assetName())
	}

	u.setApply(func(a *ApplyState) { a.State = "installing" })
	newBin := filepath.Join(tmp, binaryName)
	if err := extractBinary(archive, newBin); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	// the new binary must at least start and identify itself
	vctx, vcancel := context.WithTimeout(ctx, 15*time.Second)
	out, err := exec.CommandContext(vctx, newBin, "-version").CombinedOutput()
	vcancel()
	if err != nil || !strings.HasPrefix(string(out), binaryName) {
		return fmt.Errorf("the downloaded binary does not run on this system: %s", lastLine(string(out)))
	}

	// keep the current binary as .prev (rollback) and swap atomically
	staged := exe + ".new"
	if err := copyFile(newBin, staged, 0o755); err != nil {
		return err
	}
	prev := exe + ".prev"
	_ = os.Remove(prev)
	if err := copyFile(exe, prev, 0o755); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("replace binary: %w", err)
	}
	log.Printf("updated %s -> %s", version, rel.Tag)

	if u.noRestart {
		u.setApply(func(a *ApplyState) { a.State = "restart_manual" })
		return nil
	}
	if os.Getenv("INVOCATION_ID") == "" { // not started by systemd: nobody restarts us
		u.setApply(func(a *ApplyState) { a.State = "restart_manual" })
		return nil
	}
	u.setApply(func(a *ApplyState) { a.State = "restarting" })
	time.AfterFunc(1500*time.Millisecond, func() { // let the UI read the state first
		restartRequested.Store(true)
		requestShutdown()
	})
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// Status is what the UI shows on the Updates tab.
func (u *Updater) Status() map[string]any {
	can, reason := u.canUpdate()
	ch := u.channel()
	u.mu.Lock()
	check, have, apply := u.check, u.haveCheck, u.apply
	u.mu.Unlock()
	st := map[string]any{
		"current": map[string]any{"version": version, "commit": buildCommit(), "installed_channel": installedChannel(),
			"os": runtime.GOOS, "arch": runtime.GOARCH},
		"channel":    ch,
		"can_update": can,
		"reason":     reason,
		"apply":      apply,
		"command": fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sudo bash -s -- --update --channel=%s",
			defaultRepo, ch),
	}
	if have {
		st["check"] = check
	}
	return st
}

// Available reports whether the last check found a newer release.
func (u *Updater) Available() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.haveCheck && u.check.Available
}
