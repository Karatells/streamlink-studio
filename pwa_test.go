package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pwaServer(t *testing.T) http.Handler {
	t.Helper()
	api, _ := testAPI(t)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuth("admin", "secret", t.TempDir())
	return auth.Middleware(api.Routes(pwaHeaders(http.FileServer(http.FS(sub)))))
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestPWAAssetsPublicAndTyped(t *testing.T) {
	h := pwaServer(t)
	for path, ctype := range map[string]string{
		"/manifest.webmanifest":     "application/manifest+json",
		"/sw.js":                    "javascript",
		"/pwa.js":                   "javascript",
		"/pwa/icon-192.png":         "image/png",
		"/pwa/icon-512.png":         "image/png",
		"/pwa/maskable-512.png":     "image/png",
		"/pwa/apple-touch-icon.png": "image/png",
		"/pwa/icon.svg":             "image/svg+xml",
	} {
		rec := get(h, path)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Fatalf("%s: %d %q (want %s) - install needs these without a session", path, rec.Code, rec.Header().Get("Content-Type"), ctype)
		}
	}
	if get(h, "/sw.js").Header().Get("Cache-Control") != "no-cache" || get(h, "/manifest.webmanifest").Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("sw.js and the manifest must be revalidated on every load")
	}
	// the app itself stays behind the login
	for _, p := range []string{"/", "/app.js", "/index.html", "/api/state"} {
		if rec := get(h, p); rec.Code == 200 {
			t.Fatalf("%s must still require a session", p)
		}
	}
}

func TestPWAManifestIsValid(t *testing.T) {
	h := pwaServer(t)
	var m struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Scope     string `json:"scope"`
		Display   string `json:"display"`
		Icons     []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
		Shortcuts []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"shortcuts"`
	}
	if err := json.Unmarshal(get(h, "/manifest.webmanifest").Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name == "" || m.ShortName == "" || m.Display != "standalone" || m.StartURL == "" || m.Scope != "./" {
		t.Fatalf("incomplete manifest: %+v", m)
	}
	// relative URLs only: Studio may live under a reverse-proxy sub-path
	for _, u := range []string{m.StartURL, m.Scope} {
		if strings.HasPrefix(u, "/") || strings.Contains(u, "://") {
			t.Fatalf("manifest URL %q must be relative", u)
		}
	}
	sizes := map[string]bool{}
	purposes := map[string]bool{}
	for _, ic := range m.Icons {
		if strings.HasPrefix(ic.Src, "/") {
			t.Fatalf("icon path %q must be relative", ic.Src)
		}
		rec := get(h, "/"+ic.Src)
		if rec.Code != 200 {
			t.Fatalf("icon %s missing", ic.Src)
		}
		if ic.Type == "image/png" {
			img, err := png.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Split(ic.Sizes, "x")
			if want[0] != itoa(img.Width) || want[1] != itoa(img.Height) {
				t.Fatalf("%s declares %s but is %dx%d", ic.Src, ic.Sizes, img.Width, img.Height)
			}
			sizes[ic.Sizes] = true
		}
		purposes[ic.Purpose] = true
	}
	if !sizes["192x192"] || !sizes["512x512"] || !purposes["maskable"] {
		t.Fatalf("installability needs 192, 512 and a maskable icon: %v %v", sizes, purposes)
	}
	for _, sc := range m.Shortcuts {
		if sc.Name == "" || !strings.HasPrefix(sc.URL, "./#/") {
			t.Fatalf("bad shortcut %+v", sc)
		}
	}
	// pages link the manifest
	for _, page := range []string{"index.html", "login.html"} {
		b, _ := fs.ReadFile(webFS, "web/"+page)
		if !bytes.Contains(b, []byte(`rel="manifest"`)) || !bytes.Contains(b, []byte("apple-touch-icon")) {
			t.Fatalf("%s must link manifest and apple icon", page)
		}
	}
}

func TestServiceWorkerNeverCachesPrivateData(t *testing.T) {
	b, err := fs.ReadFile(webFS, "web/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, must := range []string{`/^\/api\//`, `/^\/metrics/`, "range", "res.redirected", "method !== 'GET'"} {
		if !strings.Contains(src, must) {
			t.Fatalf("sw.js lost its guard %q", must)
		}
	}
}
