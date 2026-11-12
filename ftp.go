package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A small FTP / FTPS (explicit TLS) server for the Saved file manager.
// Files uploaded, renamed, moved or deleted over FTP go through the same
// path checks and metadata handling as the web file manager.
// Only passive mode is supported (PORT/EPRT would allow FTP-bounce attacks).

const (
	ftpIdleControl = 10 * time.Minute
	ftpIdleData    = 2 * time.Minute
	ftpMaxFails    = 5
	ftpFailWindow  = 10 * time.Minute
)

var ftpRangeRe = regexp.MustCompile(`^(\d{4,5})-(\d{4,5})$`)

// parsePassiveRange returns lo, hi (0,0 = any port).
func parsePassiveRange(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	m := ftpRangeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, errors.New("passive ports must look like 50000-50100")
	}
	lo, _ := strconv.Atoi(m[1])
	hi, _ := strconv.Atoi(m[2])
	if lo < 1024 || hi > 65535 || lo > hi {
		return 0, 0, errors.New("passive port range must be within 1024-65535")
	}
	return lo, hi, nil
}

type failRec struct {
	n     int
	first time.Time
	until time.Time
}

type FTP struct {
	a *API

	mu       sync.Mutex
	ln       net.Listener
	sig      string
	cancel   context.CancelFunc
	lastErr  string
	sessions int
	total    int64
	cert     *tls.Certificate
	fp       string
	fails    map[string]*failRec
}

func NewFTP(a *API) *FTP { return &FTP{a: a, fails: map[string]*failRec{}} }

func ftpSig(s Settings) string {
	if !s.FTPOn {
		return ""
	}
	return fmt.Sprint(s.FTPPort, "|", s.FTPUser, "|", s.FTPPass, "|", s.FTPReadOnly, "|", s.FTPRequireTLS, "|", s.FTPPassive, "|", s.FTPPublicIP)
}

// Run keeps the listener in sync with the settings.
func (f *FTP) Run(ctx context.Context) {
	for ctx.Err() == nil {
		f.reconcile(ctx)
		sleepCtx(ctx, 2*time.Second)
	}
	f.stop()
}

func (f *FTP) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if f.ln != nil {
		_ = f.ln.Close()
		f.ln = nil
	}
	f.sig = ""
}

func (f *FTP) reconcile(ctx context.Context) {
	s := f.a.m.st.Settings()
	sig := ftpSig(s)
	f.mu.Lock()
	same := sig == f.sig && (sig == "" || f.ln != nil)
	f.mu.Unlock()
	if same {
		return
	}
	f.stop()
	f.mu.Lock()
	f.lastErr = ""
	f.mu.Unlock()
	if sig == "" {
		return
	}
	cert, err := f.loadCert()
	if err != nil {
		f.setErr("TLS certificate: " + err.Error())
		f.mu.Lock()
		f.sig = sig
		f.mu.Unlock()
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.FTPPort))
	f.mu.Lock()
	f.sig = sig
	if err != nil {
		f.lastErr = err.Error()
		f.mu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	f.ln, f.cancel, f.cert = ln, cancel, cert
	f.mu.Unlock()
	f.a.m.elog.Add(Channel{}, "ftp", "info", fmt.Sprintf("FTP server listening on %s", ln.Addr()), "")
	go f.accept(cctx, ln, s)
}

func (f *FTP) setErr(e string) { f.mu.Lock(); f.lastErr = e; f.mu.Unlock() }

// Addr is the actual listen address (for tests with port 0).
func (f *FTP) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return ""
	}
	return f.ln.Addr().String()
}

func (f *FTP) accept(ctx context.Context, ln net.Listener, s Settings) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			sess := &ftpSess{f: f, c: c, cfg: s, cwd: "/"}
			go func() { <-ctx.Done(); c.Close() }()
			sess.serve()
		}()
	}
}

// ---------------------------------------------------------------- certificate

func (f *FTP) loadCert() (*tls.Certificate, error) {
	dir := f.a.m.cfg.DataDir
	cp, kp := filepath.Join(dir, "ftp-cert.pem"), filepath.Join(dir, "ftp-key.pem")
	if c, err := tls.LoadX509KeyPair(cp, kp); err == nil {
		f.setFP(c)
		return &c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	host, _ := os.Hostname()
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Streamlink Studio FTP"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost", host}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return nil, err
	}
	c, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		return nil, err
	}
	f.setFP(c)
	return &c, nil
}

func (f *FTP) setFP(c tls.Certificate) {
	sum := sha256.Sum256(c.Certificate[0])
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	f.mu.Lock()
	f.fp = strings.Join(parts, ":")
	f.mu.Unlock()
}

// ---------------------------------------------------------------- brute force guard

func (f *FTP) blocked(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.fails[ip]
	return r != nil && time.Now().Before(r.until)
}

func (f *FTP) fail(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.fails[ip]
	if r == nil || time.Since(r.first) > ftpFailWindow {
		r = &failRec{first: time.Now()}
		f.fails[ip] = r
	}
	r.n++
	if r.n >= ftpMaxFails {
		r.until = time.Now().Add(ftpFailWindow)
	}
	if len(f.fails) > 1000 { // forget old entries
		for k, v := range f.fails {
			if time.Since(v.first) > ftpFailWindow {
				delete(f.fails, k)
			}
		}
	}
}

func (f *FTP) clearFails(ip string) { f.mu.Lock(); delete(f.fails, ip); f.mu.Unlock() }

// ---------------------------------------------------------------- status API

func (a *API) ftpStatus(w http.ResponseWriter, r *http.Request) {
	f := a.ftp
	s := a.m.st.Settings()
	f.mu.Lock()
	defer f.mu.Unlock()
	port := 0
	if f.ln != nil {
		if ta, ok := f.ln.Addr().(*net.TCPAddr); ok {
			port = ta.Port
		}
	}
	writeJSON(w, 200, map[string]any{"enabled": s.FTPOn, "running": f.ln != nil, "port": port, "error": f.lastErr,
		"sessions": f.sessions, "logins": f.total, "fingerprint": f.fp})
}

// ---------------------------------------------------------------- session

type idleConn struct {
	net.Conn
	d time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.d))
	return c.Conn.Read(p)
}
func (c idleConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.d))
	return c.Conn.Write(p)
}

type ftpSess struct {
	f   *FTP
	c   net.Conn
	r   *bufio.Reader
	cfg Settings

	user   string
	authed bool
	tlsOn  bool
	protP  bool
	cwd    string
	rnfr   string // virtual rel path of the pending RNFR
	rest   int64
	pasv   net.Listener
	ip     string
}

func (s *ftpSess) reply(code int, msg string) {
	_ = s.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(s.c, "%d %s\r\n", code, msg)
}

func (s *ftpSess) replyMulti(code int, first string, lines []string, last string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%d-%s\r\n", code, first)
	for _, l := range lines {
		b.WriteString(" " + l + "\r\n")
	}
	fmt.Fprintf(&b, "%d %s\r\n", code, last)
	_ = s.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	io.WriteString(s.c, b.String())
}

func (s *ftpSess) serve() {
	defer s.c.Close()
	host, _, _ := net.SplitHostPort(s.c.RemoteAddr().String())
	s.ip = host
	if s.f.blocked(s.ip) {
		s.reply(421, "Too many failed logins, try again later")
		return
	}
	s.f.mu.Lock()
	s.f.sessions++
	s.f.mu.Unlock()
	defer func() { s.f.mu.Lock(); s.f.sessions--; s.f.mu.Unlock(); s.closePasv() }()
	s.r = bufio.NewReaderSize(s.c, 4096)
	s.reply(220, "Streamlink Studio FTP ready")
	for {
		_ = s.c.SetReadDeadline(time.Now().Add(ftpIdleControl))
		line, err := s.r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				s.reply(500, "Line too long")
			}
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimRight(string(line), "\r\n"), " ")
		cmd = strings.ToUpper(cmd)
		if !s.dispatch(cmd, arg) {
			return
		}
	}
}

func (s *ftpSess) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{*s.f.cert}, MinVersion: tls.VersionTLS12}
}

func sum(x string) [32]byte { return sha256.Sum256([]byte(x)) }

func (s *ftpSess) login(pass string) bool {
	if s.cfg.FTPUser == "" || s.cfg.FTPPass == "" {
		return false
	}
	u, p := sum(s.user), sum(pass)
	cu, cp := sum(s.cfg.FTPUser), sum(s.cfg.FTPPass)
	ok1 := subtle.ConstantTimeCompare(u[:], cu[:])
	ok2 := subtle.ConstantTimeCompare(p[:], cp[:])
	return ok1&ok2 == 1
}

// dispatch handles one command; false closes the connection.
func (s *ftpSess) dispatch(cmd, arg string) bool {
	switch cmd {
	case "QUIT":
		s.reply(221, "Bye")
		return false
	case "NOOP":
		s.reply(200, "OK")
		return true
	case "SYST":
		s.reply(215, "UNIX Type: L8")
		return true
	case "FEAT":
		s.replyMulti(211, "Features:", []string{"UTF8", "MLST type*;size*;modify*;perm*;", "MDTM", "SIZE", "REST STREAM", "EPSV", "AUTH TLS", "PBSZ", "PROT"}, "End")
		return true
	case "OPTS":
		if strings.HasPrefix(strings.ToUpper(arg), "UTF8") {
			s.reply(200, "UTF8 is on")
		} else {
			s.reply(501, "Option not supported")
		}
		return true
	case "AUTH":
		if s.tlsOn {
			s.reply(503, "Already using TLS")
			return true
		}
		if u := strings.ToUpper(arg); u != "TLS" && u != "TLS-C" {
			s.reply(504, "Only AUTH TLS is supported")
			return true
		}
		s.reply(234, "Proceed with negotiation")
		tc := tls.Server(idleConn{s.c, ftpIdleControl}, s.tlsConfig())
		_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
		if err := tc.Handshake(); err != nil {
			return false
		}
		s.c, s.r, s.tlsOn = tc, bufio.NewReaderSize(tc, 4096), true
		return true
	case "PBSZ":
		if !s.tlsOn {
			s.reply(503, "Use AUTH TLS first")
		} else {
			s.reply(200, "PBSZ=0")
		}
		return true
	case "PROT":
		if !s.tlsOn {
			s.reply(503, "Use AUTH TLS first")
			return true
		}
		switch strings.ToUpper(arg) {
		case "P":
			s.protP = true
			s.reply(200, "Data channel will be protected")
		case "C":
			if s.cfg.FTPRequireTLS {
				s.reply(534, "TLS is required for data connections")
			} else {
				s.protP = false
				s.reply(200, "Data channel will be clear")
			}
		default:
			s.reply(504, "Unsupported protection level")
		}
		return true
	case "USER":
		if s.cfg.FTPRequireTLS && !s.tlsOn {
			s.reply(534, "TLS is required: use AUTH TLS first")
			return true
		}
		s.user, s.authed = arg, false
		s.reply(331, "Password required")
		return true
	case "PASS":
		if s.user == "" {
			s.reply(503, "Login with USER first")
			return true
		}
		if s.cfg.FTPRequireTLS && !s.tlsOn {
			s.reply(534, "TLS is required: use AUTH TLS first")
			return true
		}
		if s.login(arg) {
			s.authed = true
			s.f.clearFails(s.ip)
			s.f.mu.Lock()
			s.f.total++
			s.f.mu.Unlock()
			s.f.a.m.elog.Add(Channel{}, "ftp", "info", "FTP login: "+s.user+" from "+s.ip, "")
			s.reply(230, "Logged in")
			return true
		}
		time.Sleep(time.Second) // slow guessing down
		s.f.fail(s.ip)
		s.f.a.m.elog.Add(Channel{}, "ftp", "warn", "FTP login failed from "+s.ip, "")
		s.user = ""
		if s.f.blocked(s.ip) {
			s.reply(421, "Too many failed logins, try again later")
			return false
		}
		s.reply(530, "Login incorrect")
		return true
	}
	if !s.authed {
		s.reply(530, "Please log in first")
		return true
	}
	s.command(cmd, arg)
	return true
}

// ---------------------------------------------------------------- paths

// resolve maps a client path to a virtual absolute path and a Saved-relative one.
func (s *ftpSess) resolve(arg string) (virt, rel string, err error) {
	p := arg
	if !strings.HasPrefix(p, "/") {
		p = path.Join(s.cwd, p)
	}
	virt = path.Clean("/" + p)
	rel, err = cleanRel(strings.TrimPrefix(virt, "/"))
	return
}

func (s *ftpSess) abs(rel string) (string, error) { return s.f.a.m.savedAbs(rel) }

func stripOpts(arg string) string { // "LIST -la dir" -> "dir"
	for strings.HasPrefix(arg, "-") {
		_, rest, ok := strings.Cut(arg, " ")
		if !ok {
			return ""
		}
		arg = strings.TrimSpace(rest)
	}
	return arg
}

func (s *ftpSess) ro() bool {
	if s.cfg.FTPReadOnly {
		s.reply(550, "The server is read-only")
		return true
	}
	return false
}

// ---------------------------------------------------------------- data connection

func (s *ftpSess) closePasv() {
	if s.pasv != nil {
		_ = s.pasv.Close()
		s.pasv = nil
	}
}

func (s *ftpSess) openPasv() (*net.TCPAddr, error) {
	s.closePasv()
	local, _ := s.c.LocalAddr().(*net.TCPAddr)
	if local == nil {
		return nil, errors.New("no local address")
	}
	lo, hi, _ := parsePassiveRange(s.cfg.FTPPassive)
	if lo == 0 {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: local.IP})
		if err != nil {
			return nil, err
		}
		s.pasv = ln
		return ln.Addr().(*net.TCPAddr), nil
	}
	n := hi - lo + 1
	start := mrand.Intn(n)
	for i := 0; i < n; i++ {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: local.IP, Port: lo + (start+i)%n})
		if err == nil {
			s.pasv = ln
			return ln.Addr().(*net.TCPAddr), nil
		}
	}
	return nil, errors.New("no free passive port")
}

func (s *ftpSess) dialData() (net.Conn, error) {
	ln := s.pasv
	if ln == nil {
		return nil, errors.New("use PASV or EPSV first")
	}
	s.pasv = nil
	defer ln.Close()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		return nil, errors.New("data connection timed out")
	}
	if h, _, _ := net.SplitHostPort(c.RemoteAddr().String()); h != s.ip { // only the client that logged in
		c.Close()
		return nil, errors.New("data connection from a different address refused")
	}
	if s.cfg.FTPRequireTLS && !s.protP {
		c.Close()
		return nil, errors.New("TLS is required for data connections: use PROT P")
	}
	return idleConn{c, ftpIdleData}, nil
}

// protect upgrades a data connection to TLS (done after the 150 reply, as clients expect).
func (s *ftpSess) protect(dc net.Conn) (net.Conn, error) {
	if !s.protP {
		return dc, nil
	}
	tc := tls.Server(dc, s.tlsConfig())
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		return nil, errors.New("TLS handshake failed")
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, nil
}

// transfer opens the data connection, sends 150, runs fn and sends 226 / 4xx.
func (s *ftpSess) transfer(fn func(w io.Writer) error) {
	dc, err := s.dialData()
	if err != nil {
		s.reply(425, err.Error())
		return
	}
	s.reply(150, "Opening data connection")
	if dc, err = s.protect(dc); err != nil {
		s.reply(425, err.Error())
		return
	}
	err = fn(dc)
	if tc, ok := dc.(*tls.Conn); ok {
		_ = tc.CloseWrite()
	}
	dc.Close()
	if err != nil {
		s.reply(426, "Transfer aborted")
		return
	}
	s.reply(226, "Transfer complete")
}

// ---------------------------------------------------------------- commands

func ftpTime(t time.Time) string { return t.UTC().Format("20060102150405") }

func lsLine(fi fs.FileInfo) string {
	mode := "-rw-r--r--"
	if fi.IsDir() {
		mode = "drwxr-xr-x"
	}
	t := fi.ModTime()
	ts := t.Format("Jan _2 15:04")
	if time.Since(t) > 180*24*time.Hour || t.After(time.Now().Add(24*time.Hour)) {
		ts = t.Format("Jan _2  2006")
	}
	return fmt.Sprintf("%s 1 ftp ftp %12d %s %s\r\n", mode, fi.Size(), ts, fi.Name())
}

func mlsxLine(fi fs.FileInfo, name string) string {
	typ, perm := "file", "radfw"
	if fi.IsDir() {
		typ, perm = "dir", "elcmpf"
	}
	return fmt.Sprintf("type=%s;size=%d;modify=%s;perm=%s; %s\r\n", typ, fi.Size(), ftpTime(fi.ModTime()), perm, name)
}

// entries lists a directory (hidden files and symlinks are never shown).
func entries(dir string) ([]fs.FileInfo, error) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []fs.FileInfo
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, fi)
		}
	}
	return out, nil
}

func (s *ftpSess) command(cmd, arg string) {
	m := s.f.a.m
	switch cmd {
	case "PWD", "XPWD":
		s.reply(257, fmt.Sprintf("%q is the current directory", s.cwd))
	case "CWD", "XCWD", "CDUP", "XCUP":
		if cmd == "CDUP" || cmd == "XCUP" {
			arg = ".."
		}
		virt, rel, err := s.resolve(arg)
		if err != nil {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		if fi, e := os.Stat(p); err != nil || e != nil || !fi.IsDir() {
			s.reply(550, "Not a directory")
			return
		}
		s.cwd = virt
		s.reply(250, "Directory changed to "+virt)
	case "TYPE":
		s.reply(200, "Type set")
	case "MODE":
		if strings.EqualFold(arg, "S") {
			s.reply(200, "Mode stream")
		} else {
			s.reply(504, "Only stream mode")
		}
	case "STRU":
		if strings.EqualFold(arg, "F") {
			s.reply(200, "Structure file")
		} else {
			s.reply(504, "Only file structure")
		}
	case "PORT", "EPRT":
		s.reply(502, "Active mode is disabled: use passive mode")
	case "PASV":
		a, err := s.openPasv()
		if err != nil {
			s.reply(425, "Cannot open passive port: "+err.Error())
			return
		}
		ip := net.ParseIP(s.cfg.FTPPublicIP)
		if ip == nil {
			ip = a.IP
		}
		ip4 := ip.To4()
		if ip4 == nil {
			s.closePasv()
			s.reply(425, "Use EPSV")
			return
		}
		s.reply(227, fmt.Sprintf("Entering Passive Mode (%d,%d,%d,%d,%d,%d)", ip4[0], ip4[1], ip4[2], ip4[3], a.Port>>8, a.Port&255))
	case "EPSV":
		a, err := s.openPasv()
		if err != nil {
			s.reply(425, "Cannot open passive port: "+err.Error())
			return
		}
		s.reply(229, fmt.Sprintf("Entering Extended Passive Mode (|||%d|)", a.Port))
	case "REST":
		n, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
		if err != nil || n < 0 {
			s.reply(501, "Bad offset")
			return
		}
		s.rest = n
		s.reply(350, "Restarting at "+arg)
	case "LIST", "NLST", "MLSD":
		virt, rel, err := s.resolve(stripOpts(arg))
		if err != nil {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		if err != nil {
			s.reply(550, "Invalid path")
			return
		}
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			s.reply(550, "No such file or directory")
			return
		}
		var list []fs.FileInfo
		if fi.IsDir() {
			if list, err = entries(p); err != nil {
				s.reply(550, "Cannot read directory")
				return
			}
		} else if cmd == "MLSD" {
			s.reply(501, "Not a directory")
			return
		} else {
			list = []fs.FileInfo{fi}
		}
		_ = virt
		s.transfer(func(w io.Writer) error {
			for _, e := range list {
				var line string
				switch cmd {
				case "LIST":
					line = lsLine(e)
				case "NLST":
					line = e.Name() + "\r\n"
				default:
					line = mlsxLine(e, e.Name())
				}
				if _, err := io.WriteString(w, line); err != nil {
					return err
				}
			}
			return nil
		})
	case "MLST":
		virt, rel, err := s.resolve(arg)
		if err != nil {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		fi, e := os.Lstat(p)
		if err != nil || e != nil || fi.Mode()&fs.ModeSymlink != 0 {
			s.reply(550, "No such file or directory")
			return
		}
		s.replyMulti(250, "Listing "+virt, []string{strings.TrimRight(mlsxLine(fi, virt), "\r\n")}, "End")
	case "SIZE", "MDTM":
		_, rel, err := s.resolve(arg)
		if err != nil {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		fi, e := os.Lstat(p)
		if err != nil || e != nil || !fi.Mode().IsRegular() {
			s.reply(550, "No such file")
			return
		}
		if cmd == "SIZE" {
			s.reply(213, strconv.FormatInt(fi.Size(), 10))
		} else {
			s.reply(213, ftpTime(fi.ModTime()))
		}
	case "RETR":
		_, rel, err := s.resolve(arg)
		if err != nil || rel == "" {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		if err != nil {
			s.reply(550, "Invalid path")
			return
		}
		fi, e := os.Lstat(p)
		if e != nil || !fi.Mode().IsRegular() {
			s.reply(550, "No such file")
			return
		}
		f, err := os.Open(p)
		if err != nil {
			s.reply(550, "Cannot open file")
			return
		}
		defer f.Close()
		off := s.rest
		s.rest = 0
		if off > 0 {
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				s.reply(554, "Bad restart offset")
				return
			}
		}
		s.transfer(func(w io.Writer) error { _, err := io.Copy(w, f); return err })
	case "STOR", "APPE":
		s.store(cmd, arg)
	case "DELE":
		if s.ro() {
			return
		}
		_, rel, err := s.resolve(arg)
		if err != nil || rel == "" {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		fi, e := os.Lstat(p)
		if err != nil || e != nil || fi.IsDir() {
			s.reply(550, "No such file")
			return
		}
		if res := m.DeleteSaved([]string{rel}); len(res.Failed) > 0 {
			s.reply(550, "Delete failed")
			return
		}
		m.lib.invalidate()
		s.reply(250, "Deleted")
	case "MKD", "XMKD":
		if s.ro() {
			return
		}
		virt, rel, err := s.resolve(arg)
		if err != nil || rel == "" {
			s.reply(550, "Invalid path")
			return
		}
		parent, name := path.Split(rel)
		if _, err := m.Mkdir(strings.TrimSuffix(parent, "/"), name); err != nil {
			s.reply(550, "Cannot create directory: "+err.Error())
			return
		}
		s.reply(257, fmt.Sprintf("%q created", virt))
	case "RMD", "XRMD":
		if s.ro() {
			return
		}
		_, rel, err := s.resolve(arg)
		if err != nil || rel == "" {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		fi, e := os.Lstat(p)
		if err != nil || e != nil || !fi.IsDir() {
			s.reply(550, "No such directory")
			return
		}
		if err := os.Remove(p); err != nil { // only empty directories
			s.reply(550, "Directory not empty")
			return
		}
		s.reply(250, "Directory removed")
	case "RNFR":
		if s.ro() {
			return
		}
		_, rel, err := s.resolve(arg)
		if err != nil || rel == "" {
			s.reply(550, "Invalid path")
			return
		}
		p, err := s.abs(rel)
		if _, e := os.Lstat(p); err != nil || e != nil {
			s.reply(550, "No such file or directory")
			return
		}
		s.rnfr = rel
		s.reply(350, "Ready for RNTO")
	case "RNTO":
		if s.ro() {
			return
		}
		from := s.rnfr
		s.rnfr = ""
		if from == "" {
			s.reply(503, "RNFR first")
			return
		}
		_, rel, err := s.resolve(arg)
		if err != nil || rel == "" {
			s.reply(553, "Invalid name")
			return
		}
		if err := m.MoveRenameSaved(from, rel); err != nil {
			s.reply(550, "Rename failed: "+err.Error())
			return
		}
		s.reply(250, "Renamed")
	case "SITE", "ABOR", "STAT", "ALLO", "ACCT", "REIN", "SMNT":
		if cmd == "ALLO" {
			s.reply(200, "OK")
		} else {
			s.reply(502, "Command not implemented")
		}
	default:
		s.reply(500, "Unknown command")
	}
}

func (s *ftpSess) store(cmd, arg string) {
	m := s.f.a.m
	if s.ro() {
		return
	}
	_, rel, err := s.resolve(arg)
	if err != nil || rel == "" {
		s.reply(553, "Invalid file name")
		return
	}
	dirRel, name := path.Split(rel)
	if validName(name) != nil {
		s.reply(553, "Invalid file name")
		return
	}
	dir, err := s.abs(strings.TrimSuffix(dirRel, "/"))
	if fi, e := os.Stat(dir); err != nil || e != nil || !fi.IsDir() {
		s.reply(550, "Folder does not exist")
		return
	}
	target := filepath.Join(dir, name)
	if fi, e := os.Lstat(target); e == nil && !fi.Mode().IsRegular() {
		s.reply(550, "Cannot overwrite a folder or special file")
		return
	}
	s.rest = 0
	var out *os.File
	tmp := ""
	if cmd == "APPE" {
		out, err = os.OpenFile(target, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	} else { // write beside the target and swap in at the end: a broken upload never replaces a good file
		out, err = os.CreateTemp(dir, ".ftp-*.part")
		if out != nil {
			tmp = out.Name()
		}
	}
	if err != nil {
		s.reply(550, "Cannot write file")
		return
	}
	s.transfer(func(w io.Writer) error {
		// the data connection is the *source* here
		src, _ := w.(io.Reader)
		_, err := io.Copy(out, src)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if tmp != "" {
			if err == nil {
				_ = os.Chmod(tmp, 0o640)
				err = os.Rename(tmp, target)
			}
			if err != nil {
				_ = os.Remove(tmp)
			}
		}
		return err
	})
	m.lib.invalidate()
}
