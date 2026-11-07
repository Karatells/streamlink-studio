package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSemver(t *testing.T) {
	lt := [][2]string{
		{"v1.0.0", "v1.0.1"}, {"v1.9.0", "v1.10.0"}, {"v1.0.0-beta.1", "v1.0.0"},
		{"v1.0.0-beta.2", "v1.0.0-beta.10"}, {"v1.0.0-beta.1", "v1.0.0-rc.1"}, {"v1.0.0-1", "v1.0.0-beta"},
	}
	for _, c := range lt {
		a, ok1 := parseSemver(c[0])
		b, ok2 := parseSemver(c[1])
		if !ok1 || !ok2 || compareSemver(a, b) >= 0 || compareSemver(b, a) <= 0 {
			t.Errorf("expected %s < %s", c[0], c[1])
		}
	}
	if _, ok := parseSemver("main-abc1234"); ok {
		t.Error("main-abc1234 must not parse")
	}
}

func TestInferChannel(t *testing.T) {
	for v, want := range map[string]string{"v1.2.3": "stable", "v1.2.3-beta.1": "beta", "main-abc1234": "nightly", "nightly-x": "nightly", "dev": "stable"} {
		if got := inferChannel(v); got != want {
			t.Errorf("inferChannel(%q)=%s want %s", v, got, want)
		}
	}
}

func TestSameCommit(t *testing.T) {
	if !sameCommit("abc1234", "abc1234def5678") || sameCommit("abc1234", "abc1235def") || sameCommit("abc", "abc") {
		t.Error("sameCommit")
	}
}

// fakeGitHub serves releases plus a downloadable archive.
type fakeGitHub struct {
	srv      *httptest.Server
	archive  []byte
	sum      string
	badSum   bool
	noSum    bool
	releases map[string]ghRelease
}

func mkTar(t *testing.T, script string) []byte {
	var out []byte
	w := &sliceWriter{&out}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "streamlink-studio", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	return out
}

type sliceWriter struct{ p *[]byte }

func (s *sliceWriter) Write(b []byte) (int, error) { *s.p = append(*s.p, b...); return len(b), nil }

func newFakeGitHub(t *testing.T, script string) *fakeGitHub {
	f := &fakeGitHub{archive: mkTar(t, script), releases: map[string]ghRelease{}}
	h := sha256.Sum256(f.archive)
	f.sum = hex.EncodeToString(h[:])
	mux := http.NewServeMux()
	mux.HandleFunc("/dl/archive", func(w http.ResponseWriter, r *http.Request) { w.Write(f.archive) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		s := f.sum
		if f.badSum {
			s = strings.Repeat("0", 64)
		}
		fmt.Fprintf(w, "%s  %s\n", s, assetName())
	})
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(f.releases["stable"]) })
	mux.HandleFunc("/repos/o/r/releases/tags/nightly", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(f.releases["nightly"]) })
	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		var l []ghRelease
		for _, v := range f.releases {
			l = append(l, v)
		}
		json.NewEncoder(w).Encode(l)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) rel(tag string, pre bool, commit string) ghRelease {
	g := ghRelease{TagName: tag, Name: tag, Body: "notes", HTMLURL: "http://x/" + tag, Prerelease: pre, TargetCommitish: commit, PublishedAt: time.Now()}
	g.Assets = []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{{assetName(), f.srv.URL + "/dl/archive"}}
	if !f.noSum {
		g.Assets = append(g.Assets, struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{"checksums.txt", f.srv.URL + "/dl/sums"})
	}
	return g
}

func newTestUpdater(t *testing.T, f *fakeGitHub) (*Updater, string) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{DataDir: t.TempDir(), RecDir: t.TempDir()}, st)
	dir := t.TempDir()
	exe := filepath.Join(dir, "streamlink-studio")
	os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755)
	u := NewUpdater(m)
	u.api, u.repo, u.exe, u.noRestart = f.srv.URL, "o/r", exe, true
	return u, exe
}

func setVersion(t *testing.T, v, c string) {
	ov, oc := version, commit
	version, commit = v, c
	t.Cleanup(func() { version, commit = ov, oc })
}

func TestCheckChannels(t *testing.T) {
	f := newFakeGitHub(t, "#!/bin/sh\necho streamlink-studio v0.2.0\n")
	f.releases["stable"] = f.rel("v0.2.0", false, "main")
	f.releases["beta1"] = f.rel("v0.3.0-beta.1", true, "main")
	f.releases["old"] = f.rel("v0.1.0", false, "main")
	f.releases["nightly"] = f.rel("nightly", true, strings.Repeat("a", 40))
	u, _ := newTestUpdater(t, f)
	setVersion(t, "v0.1.0", "")
	ctx := context.Background()

	if c := u.Check(ctx, true); c.Error != "" || !c.Available || c.Latest.Tag != "v0.2.0" {
		t.Fatalf("stable: %+v", c)
	}
	s := u.m.st.Settings()
	s.UpdateChannel = "beta"
	u.m.st.SetSettings(s)
	if c := u.Check(ctx, true); !c.Available || c.Latest.Tag != "v0.3.0-beta.1" {
		t.Fatalf("beta: %+v", c)
	}
	s.UpdateChannel = "nightly"
	u.m.st.SetSettings(s)
	setVersion(t, "main-aaaaaaa", strings.Repeat("a", 40))
	if c := u.Check(ctx, true); c.Available {
		t.Fatalf("nightly same commit must be up to date: %+v", c)
	}
	setVersion(t, "main-bbbbbbb", strings.Repeat("b", 40))
	if c := u.Check(ctx, true); !c.Available {
		t.Fatalf("nightly different commit must be available: %+v", c)
	}
	setVersion(t, "v0.2.0", "")
	s.UpdateChannel = "stable"
	u.m.st.SetSettings(s)
	if c := u.Check(ctx, true); c.Available {
		t.Fatalf("same version must be up to date: %+v", c)
	}
	setVersion(t, "dev", "")
	if c := u.Check(ctx, true); c.Available {
		t.Fatal("dev build must not be offered updates")
	}
}

func waitApply(t *testing.T, u *Updater) ApplyState {
	for i := 0; i < 100; i++ {
		u.mu.Lock()
		a := u.apply
		u.mu.Unlock()
		if a.State == "error" || a.State == "restart_manual" {
			return a
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("apply timeout")
	return ApplyState{}
}

func TestApply(t *testing.T) {
	f := newFakeGitHub(t, "#!/bin/sh\necho streamlink-studio v0.2.0\n")
	f.releases["stable"] = f.rel("v0.2.0", false, "main")
	u, exe := newTestUpdater(t, f)
	setVersion(t, "v0.1.0", "")
	if err := u.Apply(); err == nil {
		t.Fatal("apply without check must fail")
	}
	u.Check(context.Background(), true)
	if err := u.Apply(); err != nil {
		t.Fatal(err)
	}
	if a := waitApply(t, u); a.State != "restart_manual" {
		t.Fatalf("state %+v", a)
	}
	b, _ := os.ReadFile(exe)
	if !strings.Contains(string(b), "v0.2.0") {
		t.Fatalf("binary not replaced: %q", b)
	}
	if p, _ := os.ReadFile(exe + ".prev"); !strings.Contains(string(p), "old") {
		t.Fatal(".prev missing")
	}
	if es, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".update-*")); len(es) != 0 {
		t.Fatal("temp dir left behind")
	}
}

func TestApplyRefusals(t *testing.T) {
	for name, mod := range map[string]func(*fakeGitHub){
		"bad checksum": func(f *fakeGitHub) { f.badSum = true },
		"no checksums": func(f *fakeGitHub) { f.noSum = true },
	} {
		f := newFakeGitHub(t, "#!/bin/sh\necho streamlink-studio v0.2.0\n")
		mod(f)
		f.releases["stable"] = f.rel("v0.2.0", false, "main")
		u, exe := newTestUpdater(t, f)
		setVersion(t, "v0.1.0", "")
		u.Check(context.Background(), true)
		if err := u.Apply(); err != nil {
			t.Fatal(err)
		}
		if a := waitApply(t, u); a.State != "error" {
			t.Fatalf("%s: state %+v", name, a)
		}
		if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "old") {
			t.Fatalf("%s: binary was replaced", name)
		}
	}
	// a binary that does not identify itself is rejected
	f := newFakeGitHub(t, "#!/bin/sh\nexit 1\n")
	f.releases["stable"] = f.rel("v0.2.0", false, "main")
	u, exe := newTestUpdater(t, f)
	setVersion(t, "v0.1.0", "")
	u.Check(context.Background(), true)
	u.Apply()
	if a := waitApply(t, u); a.State != "error" {
		t.Fatalf("broken binary accepted: %+v", a)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "old") {
		t.Fatal("binary replaced by a broken one")
	}
}

func TestDownloadAllowList(t *testing.T) {
	u := &Updater{api: defaultAPI, repo: defaultRepo}
	if u.allowedDownload("https://evil.example/x") || !u.allowedDownload("https://github.com/"+defaultRepo+"/releases/download/v1/x") {
		t.Fatal("allow list")
	}
}
