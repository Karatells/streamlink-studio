package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startFTP(t *testing.T, mod func(*Settings)) (*API, string) {
	t.Helper()
	a, _ := testAPI(t)
	s := a.m.st.Settings()
	s.FTPOn, s.FTPUser, s.FTPPass = true, "alice", "s3cret-pass"
	if mod != nil {
		mod(&s)
	}
	if err := a.m.st.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	a.ftp = NewFTP(a)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); a.ftp.stop() })
	a.ftp.reconcile(ctx)
	addr := a.ftp.Addr()
	if addr == "" {
		t.Fatalf("ftp not listening: %s", a.ftp.lastErr)
	}
	_, port, _ := net.SplitHostPort(addr)
	return a, "127.0.0.1:" + port
}

type ftpc struct {
	t   *testing.T
	c   net.Conn
	r   *bufio.Reader
	tls bool
}

func dialFTP(t *testing.T, addr string) *ftpc {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := &ftpc{t: t, c: c, r: bufio.NewReader(c)}
	t.Cleanup(func() { c.Close() })
	if code, _ := f.read(); code != 220 {
		t.Fatal("greeting", code)
	}
	return f
}

// read returns the final reply of a (possibly multi-line) response.
func (f *ftpc) read() (int, string) {
	f.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		l, err := f.r.ReadString('\n')
		if err != nil {
			f.t.Fatalf("read: %v", err)
		}
		l = strings.TrimRight(l, "\r\n")
		if len(l) >= 4 && l[3] == ' ' {
			n, _ := strconv.Atoi(l[:3])
			return n, l[4:]
		}
	}
}

func (f *ftpc) cmd(format string, a ...any) (int, string) {
	fmt.Fprintf(f.c, format+"\r\n", a...)
	return f.read()
}

func (f *ftpc) want(code int, format string, a ...any) string {
	f.t.Helper()
	c, msg := f.cmd(format, a...)
	if c != code {
		f.t.Fatalf("%s: got %d %s, want %d", fmt.Sprintf(format, a...), c, msg, code)
	}
	return msg
}

func (f *ftpc) login(user, pass string) bool {
	if c, _ := f.cmd("USER %s", user); c != 331 {
		return false
	}
	c, _ := f.cmd("PASS %s", pass)
	return c == 230
}

var pasvRe = regexp.MustCompile(`\|\|\|(\d+)\|`)

func (f *ftpc) data() net.Conn {
	f.t.Helper()
	msg := f.want(229, "EPSV")
	port := pasvRe.FindStringSubmatch(msg)[1]
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 3*time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	if f.tls {
		return &lazyTLS{c: c, t: f.t}
	}
	return c
}

// lazyTLS performs the TLS handshake on first use: clients do it after the 150 reply.
type lazyTLS struct {
	net.Conn
	c  net.Conn
	t  *testing.T
	tc *tls.Conn
}

func (l *lazyTLS) conn() *tls.Conn {
	if l.tc == nil {
		l.tc = tls.Client(l.c, &tls.Config{InsecureSkipVerify: true})
		if err := l.tc.Handshake(); err != nil {
			l.t.Fatal(err)
		}
	}
	return l.tc
}
func (l *lazyTLS) Read(p []byte) (int, error)  { return l.conn().Read(p) }
func (l *lazyTLS) Write(p []byte) (int, error) { return l.conn().Write(p) }
func (l *lazyTLS) Close() error {
	if l.tc != nil {
		_ = l.tc.Close()
	}
	return l.c.Close()
}

func (f *ftpc) list(cmd string) string {
	f.t.Helper()
	d := f.data()
	f.want(150, "%s", cmd)
	b, _ := io.ReadAll(d)
	d.Close()
	if c, m := f.read(); c != 226 {
		f.t.Fatalf("list end: %d %s", c, m)
	}
	return string(b)
}

func (f *ftpc) put(name, body string) {
	f.t.Helper()
	d := f.data()
	f.want(150, "STOR %s", name)
	io.WriteString(d, body)
	d.Close()
	if c, m := f.read(); c != 226 {
		f.t.Fatalf("stor end: %d %s", c, m)
	}
}

func (f *ftpc) get(name string) string {
	f.t.Helper()
	d := f.data()
	f.want(150, "RETR %s", name)
	b, _ := io.ReadAll(d)
	d.Close()
	if c, m := f.read(); c != 226 {
		f.t.Fatalf("retr end: %d %s", c, m)
	}
	return string(b)
}

func TestFTPFileOperations(t *testing.T) {
	a, addr := startFTP(t, nil)
	saved := a.m.cfg.SavedDir
	f := dialFTP(t, addr)
	if c, _ := f.cmd("LIST"); c != 530 {
		t.Fatal("commands must need a login", c)
	}
	if !f.login("alice", "s3cret-pass") {
		t.Fatal("login failed")
	}
	f.want(257, "MKD clips")
	f.want(250, "CWD clips")
	f.put("a.txt", "hello")
	if b, _ := os.ReadFile(filepath.Join(saved, "clips", "a.txt")); string(b) != "hello" {
		t.Fatalf("stored: %q", b)
	}
	if got := f.get("a.txt"); got != "hello" {
		t.Fatalf("retr: %q", got)
	}
	f.want(213, "SIZE a.txt")
	if l := f.list("LIST -la"); !strings.Contains(l, "a.txt") || strings.Contains(l, ".ftp-") {
		t.Fatalf("list: %q", l)
	}
	if l := f.list("MLSD"); !strings.Contains(l, "type=file;size=5;") {
		t.Fatalf("mlsd: %q", l)
	}
	// overwrite replaces the file
	f.put("a.txt", "second")
	if b, _ := os.ReadFile(filepath.Join(saved, "clips", "a.txt")); string(b) != "second" {
		t.Fatalf("overwrite: %q", b)
	}
	// metadata follows an FTP rename / move
	a.m.meta.set([]string{"saved/clips/a.txt"}, RecMeta{Streamer: "bob", Started: time.Now()})
	f.want(350, "RNFR a.txt")
	f.want(250, "RNTO b.txt")
	f.want(350, "RNFR b.txt")
	f.want(250, "RNTO /moved.txt")
	if a.m.meta.snapshot()["saved/moved.txt"].Streamer != "bob" {
		t.Fatalf("meta not moved: %+v", a.m.meta.snapshot())
	}
	f.want(350, "RNFR /moved.txt")
	f.want(550, "RNTO /clips")
	// delete
	f.want(550, "DELE /clips")
	f.want(250, "DELE /moved.txt")
	f.want(550, "RMD /clips/nothing")
	f.put("x.bin", "x")
	f.want(550, "RMD /clips") // not empty
	f.want(250, "DELE x.bin")
	f.want(250, "CDUP")
	f.want(250, "RMD clips")
	if _, err := os.Stat(filepath.Join(saved, "clips")); err == nil {
		t.Fatal("folder not removed")
	}
	f.want(257, "PWD")
	f.want(221, "QUIT")
}

func TestFTPCannotEscapeOrTouchHidden(t *testing.T) {
	a, addr := startFTP(t, nil)
	os.WriteFile(filepath.Join(a.m.cfg.DataDir, "secret.txt"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(a.m.cfg.SavedDir, ".hidden"), []byte("h"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "o.txt"), []byte("o"), 0o644)
	_ = os.Symlink(outside, filepath.Join(a.m.cfg.SavedDir, "esc"))
	f := dialFTP(t, addr)
	if !f.login("alice", "s3cret-pass") {
		t.Fatal("login")
	}
	// ".." never leaves the root: the virtual path is clamped
	f.want(250, "CWD /../..")
	if msg := f.want(257, "PWD"); !strings.HasPrefix(msg, `"/"`) {
		t.Fatal(msg)
	}
	d := f.data()
	if c, _ := f.cmd("RETR ../secret.txt"); c != 550 {
		t.Fatal("traversal RETR", c)
	}
	d.Close()
	f.want(550, "RETR .hidden")
	f.want(550, "RETR esc/o.txt")
	f.want(550, "CWD esc")
	f.want(553, "STOR .evil")
	f.want(553, "STOR a\\b")
	f.want(550, "DELE esc/o.txt")
	if l := f.list("LIST"); strings.Contains(l, ".hidden") || strings.Contains(l, "esc") {
		t.Fatalf("hidden entries listed: %q", l)
	}
	if _, err := os.Stat(filepath.Join(outside, "o.txt")); err != nil {
		t.Fatal("file outside the root touched")
	}
	f.want(502, "PORT 127,0,0,1,4,1") // no active mode (FTP bounce)
}

func TestFTPAuthAndLockout(t *testing.T) {
	a, addr := startFTP(t, nil)
	f := dialFTP(t, addr)
	if f.login("alice", "wrong") {
		t.Fatal("wrong password accepted")
	}
	if f.login("bob", "s3cret-pass") {
		t.Fatal("wrong user accepted")
	}
	if !f.login("alice", "s3cret-pass") {
		t.Fatal("correct login refused")
	}
	// repeated failures lock the address out
	for i := 0; i < ftpMaxFails; i++ {
		a.ftp.fail("127.0.0.1")
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	l, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.HasPrefix(l, "421") {
		t.Fatalf("locked out client got %q", l)
	}
}

func TestFTPReadOnlyAndSettingsChange(t *testing.T) {
	a, addr := startFTP(t, func(s *Settings) { s.FTPReadOnly = true })
	os.WriteFile(filepath.Join(a.m.cfg.SavedDir, "r.txt"), []byte("r"), 0o644)
	f := dialFTP(t, addr)
	f.login("alice", "s3cret-pass")
	if got := f.get("r.txt"); got != "r" {
		t.Fatal(got)
	}
	f.want(550, "DELE r.txt")
	f.want(550, "MKD d")
	f.want(550, "STOR w.txt")
	f.want(550, "RNFR r.txt")
	// disabling the server drops running sessions and closes the port
	s := a.m.st.Settings()
	s.FTPOn = false
	a.m.st.SetSettings(s)
	a.ftp.reconcile(context.Background())
	if a.ftp.Addr() != "" {
		t.Fatal("still listening")
	}
	f.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	f.c.Write([]byte("NOOP\r\n"))
	if _, err := f.r.ReadString('\n'); err == nil {
		t.Fatal("session survived shutdown")
	}
}

func TestFTPTLSRequired(t *testing.T) {
	_, addr := startFTP(t, func(s *Settings) { s.FTPRequireTLS = true })
	f := dialFTP(t, addr)
	f.want(534, "USER alice") // plain text logins are refused
	f.want(234, "AUTH TLS")
	tc := tls.Client(f.c, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	f.c, f.r, f.tls = tc, bufio.NewReader(tc), true
	if !f.login("alice", "s3cret-pass") {
		t.Fatal("login over TLS failed")
	}
	f.want(200, "PBSZ 0")
	// clear data channel is refused, protected one works
	f.want(200, "PROT P")
	f.put("t.txt", "secret")
	if got := f.get("t.txt"); got != "secret" {
		t.Fatal(got)
	}
	f.want(534, "PROT C")
}

func TestFTPPassiveRangeAndSettingsValidation(t *testing.T) {
	for _, bad := range []string{"80-90", "5000-4000", "abc", "50000-70000"} {
		if _, _, err := parsePassiveRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if lo, hi, err := parsePassiveRange("50000-50010"); err != nil || lo != 50000 || hi != 50010 {
		t.Fatal(lo, hi, err)
	}
	a, _ := testAPI(t)
	if c, _ := call(t, a, "PUT", "/api/settings", map[string]any{"ftp_on": true}); c != 400 {
		t.Fatal("enabling FTP without credentials must fail", c)
	}
	if c, _ := call(t, a, "PUT", "/api/settings", map[string]any{"ftp_user": "u", "ftp_pass": "short"}); c != 400 {
		t.Fatal("short password accepted", c)
	}
	c, raw := call(t, a, "PUT", "/api/settings", map[string]any{"ftp_on": true, "ftp_user": "u", "ftp_pass": "longenough1", "ftp_passive": "50000-50010", "ftp_public_ip": "203.0.113.5"})
	if c != 200 || strings.Contains(string(raw), "longenough1") || !strings.Contains(string(raw), `"has_ftp_pass":true`) || !strings.Contains(string(raw), `"ftp_port":2121`) {
		t.Fatal(c, string(raw))
	}
	if c, _ := call(t, a, "PUT", "/api/settings", map[string]any{"ftp_public_ip": "not-an-ip"}); c != 400 {
		t.Fatal("bad ip", c)
	}
}
