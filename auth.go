package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cookieName      = "sls_session"
	rememberTTL     = 30 * 24 * time.Hour
	shortTTL        = 12 * time.Hour
	maxSessions     = 100
	maxFails        = 5
	lockDuration    = 5 * time.Minute
	failWindow      = 10 * time.Minute
	maxLoginBody    = 4 << 10
	sessionFileName = "sessions.json"
)

type session struct {
	Exp      time.Time `json:"exp"`
	Remember bool      `json:"remember"`
}

type failure struct {
	count int
	first time.Time
	until time.Time
}

// Auth implements the login page flow: a username/password configured via
// STUDIO_USER / STUDIO_PASS, server-side sessions persisted to disk (so they
// survive restarts) and an HttpOnly cookie. HTTP Basic is still accepted for
// scripts (curl, monitoring).
type Auth struct {
	user, pass string
	path       string
	metricsTok string // optional bearer token that may read /metrics only (STUDIO_METRICS_TOKEN)

	mu       sync.Mutex
	sessions map[string]session // key: sha256(token) hex
	fails    map[string]*failure
}

func NewAuth(user, pass, dataDir string) *Auth {
	a := &Auth{user: user, pass: pass, path: dataDir + "/" + sessionFileName,
		sessions: map[string]session{}, fails: map[string]*failure{}, metricsTok: os.Getenv("STUDIO_METRICS_TOKEN")}
	if b, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(b, &a.sessions)
	}
	a.pruneLocked()
	return a
}

func (a *Auth) Enabled() bool { return a.user != "" || a.pass != "" }

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (a *Auth) pruneLocked() {
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.Exp) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) > maxSessions { // keep the newest ones
		type kv struct {
			k string
			e time.Time
		}
		all := make([]kv, 0, len(a.sessions))
		for k, s := range a.sessions {
			all = append(all, kv{k, s.Exp})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].e.After(all[j].e) })
		for _, x := range all[maxSessions:] {
			delete(a.sessions, x.k)
		}
	}
}

func (a *Auth) saveLocked() {
	b, err := json.Marshal(a.sessions)
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, a.path)
	}
}

func (a *Auth) create(remember bool) (token string, exp time.Time) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	ttl := shortTTL
	if remember {
		ttl = rememberTTL
	}
	exp = time.Now().Add(ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[hashToken(token)] = session{Exp: exp, Remember: remember}
	a.pruneLocked()
	a.saveLocked()
	return token, exp
}

// check validates a token. For "remember me" sessions the lifetime is renewed
// once less than half of it is left (sliding expiry); renewed reports that.
func (a *Auth) check(token string) (ok bool, renewed *session) {
	if token == "" {
		return false, nil
	}
	k := hashToken(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	s, found := a.sessions[k]
	if !found || time.Now().After(s.Exp) {
		if found {
			delete(a.sessions, k)
			a.saveLocked()
		}
		return false, nil
	}
	if s.Remember && time.Until(s.Exp) < rememberTTL/2 {
		s.Exp = time.Now().Add(rememberTTL)
		a.sessions[k] = s
		a.saveLocked()
		return true, &s
	}
	return true, nil
}

func (a *Auth) destroy(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, hashToken(token))
	a.saveLocked()
	a.mu.Unlock()
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, exp time.Time, remember bool) {
	c := &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)}
	if remember { // persistent cookie; otherwise a browser-session cookie
		c.Expires = exp
		c.MaxAge = int(time.Until(exp).Seconds())
	}
	http.SetCookie(w, c)
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r)})
}

// clientIP uses X-Forwarded-For / X-Real-IP only when the direct peer is a
// local reverse proxy, otherwise the socket address.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	return host
}

// authed reports whether the request carries a valid session or Basic credentials.
func (a *Auth) authed(w http.ResponseWriter, r *http.Request) bool {
	if c, err := r.Cookie(cookieName); err == nil {
		if ok, renewed := a.check(c.Value); ok {
			if renewed != nil {
				setSessionCookie(w, r, c.Value, renewed.Exp, true)
			}
			return true
		}
	}
	if u, p, ok := r.BasicAuth(); ok && a.credsOK(u, p) {
		return true
	}
	return false
}

// metricsTokenOK lets Prometheus scrape /metrics with `Authorization: Bearer <token>`
// without a user password; it grants nothing else.
func (a *Auth) metricsTokenOK(r *http.Request) bool {
	if a.metricsTok == "" || r.URL.Path != "/metrics" {
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(tok), []byte(a.metricsTok)) == 1
}

func (a *Auth) credsOK(u, p string) bool {
	uo := subtle.ConstantTimeCompare([]byte(u), []byte(a.user))
	po := subtle.ConstantTimeCompare([]byte(p), []byte(a.pass))
	return uo&po == 1
}

// publicPath lists what the login page needs; everything else requires a session.
func publicPath(p string) bool {
	switch p {
	case "/healthz", "/login.html", "/login.js", "/style.css", "/languages.config.js",
		"/manifest.webmanifest", "/sw.js", "/pwa.js",
		"/api/login", "/api/logout", "/api/auth/status":
		return true
	}
	return strings.HasPrefix(p, "/locales/") || strings.HasPrefix(p, "/pwa/")
}

// sameOrigin blocks cross-site state-changing requests (CSRF) when the browser
// sends an Origin header.
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
			return
		}
		if publicPath(r.URL.Path) || a.metricsTokenOK(r) || a.authed(w, r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		http.Redirect(w, r, "/login.html", http.StatusFound)
	})
}

// ------------------------------------------------------------- handlers

func (a *Auth) status(w http.ResponseWriter, r *http.Request) {
	ok := !a.Enabled() || a.authed(w, r)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"auth_required": a.Enabled(), "authenticated": ok})
}

func (a *Auth) lockedFor(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f := a.fails[ip]; f != nil && time.Now().Before(f.until) {
		return time.Until(f.until)
	}
	return 0
}

func (a *Auth) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	f := a.fails[ip]
	if f == nil || now.Sub(f.first) > failWindow {
		f = &failure{first: now}
		a.fails[ip] = f
	}
	f.count++
	if f.count >= maxFails {
		f.until = now.Add(lockDuration)
		f.count = 0
		f.first = now
	}
	if len(a.fails) > 1000 { // bound memory
		for k, v := range a.fails {
			if now.After(v.until) && now.Sub(v.first) > failWindow {
				delete(a.fails, k)
			}
		}
	}
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	ip := clientIP(r)
	if d := a.lockedFor(ip); d > 0 {
		w.Header().Set("Retry-After", itoa(int(d.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too_many_attempts", "retry_after": int(d.Seconds()) + 1})
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad_request"})
		return
	}
	if !a.credsOK(in.Username, in.Password) {
		a.recordFail(ip)
		time.Sleep(400 * time.Millisecond) // slow down guessing
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
		return
	}
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
	// drop a stale session of this browser, if any
	if c, err := r.Cookie(cookieName); err == nil {
		a.destroy(c.Value)
	}
	token, exp := a.create(in.Remember)
	setSessionCookie(w, r, token, exp, in.Remember)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		a.destroy(c.Value)
	}
	clearSessionCookie(w, r)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
