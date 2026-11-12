package main

import (
	"archive/zip"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxBatch = 500

func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	l, err := a.m.ListSaved(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, l)
}

func (a *API) filesTree(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"folders": a.m.SavedTree()})
}

type pathsIn struct {
	Paths []string `json:"paths"`
	Dest  string   `json:"dest"`
}

func readPaths(r *http.Request) (*pathsIn, error) {
	var in pathsIn
	if err := readJSON(r, &in); err != nil {
		return nil, err
	}
	if len(in.Paths) == 0 {
		return nil, errBad("nothing selected")
	}
	if len(in.Paths) > maxBatch {
		return nil, errBad("too many items selected")
	}
	return &in, nil
}

func (a *API) filesMkdir(w http.ResponseWriter, r *http.Request) {
	var in struct{ Path, Name string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.m.Mkdir(in.Path, in.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"path": p})
}

func (a *API) filesRename(w http.ResponseWriter, r *http.Request) {
	var in struct{ Path, Name string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.m.RenameSaved(in.Path, in.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": p})
}

func (a *API) filesTransfer(copyMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, err := readPaths(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		var res *OpResult
		if copyMode {
			res, err = a.m.CopySaved(in.Paths, in.Dest)
		} else {
			res, err = a.m.MoveSaved(in.Paths, in.Dest)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		a.m.lib.invalidate()
		writeJSON(w, 200, res)
	}
}

func (a *API) filesDelete(w http.ResponseWriter, r *http.Request) {
	in, err := readPaths(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	res := a.m.DeleteSaved(in.Paths)
	a.m.lib.invalidate()
	writeJSON(w, 200, res)
}

func (a *API) filesRestore(w http.ResponseWriter, r *http.Request) {
	in, err := readPaths(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	res := a.m.RestoreRecordings(in.Paths)
	a.m.lib.invalidate()
	writeJSON(w, 200, res)
}

// filesUpload stores the raw request body as a new file (name from the query).
func (a *API) filesUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if err := validName(name); err != nil {
		writeErr(w, err)
		return
	}
	dir, err := a.m.savedAbs(q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		writeErr(w, &httpError{404, "folder not found"})
		return
	}
	nameMu.Lock()
	final := uniqueTarget(dir, name, false)
	nameMu.Unlock()
	tmp, err := os.CreateTemp(dir, ".upload-*.part")
	if err != nil {
		writeErr(w, err)
		return
	}
	_, err = io.Copy(tmp, r.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		nameMu.Lock()
		final = uniqueTarget(dir, name, false)
		err = os.Rename(tmp.Name(), filepath.Join(dir, final))
		nameMu.Unlock()
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		writeErr(w, err)
		return
	}
	_ = os.Chmod(filepath.Join(dir, final), 0o640)
	a.m.lib.invalidate()
	rel, _ := cleanRel(q.Get("path"))
	if rel != "" {
		final = rel + "/" + final
	}
	writeJSON(w, 201, map[string]string{"path": final})
}

// savedFile resolves a regular, non-hidden file of the Saved tree.
func (a *API) savedFile(w http.ResponseWriter, rel string) (string, bool) {
	rel, err := cleanRel(rel)
	if err != nil || rel == "" {
		writeErr(w, errBad("invalid path"))
		return "", false
	}
	p, err := a.m.savedAbs(rel)
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		writeErr(w, &httpError{404, "file not found"})
		return "", false
	}
	return p, true
}

func (a *API) filesServe(download bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.savedFile(w, r.PathValue("path"))
		if !ok {
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox")
		if download || !isMediaFile(p) { // only videos are shown inline
			h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(p)))
		}
		http.ServeFile(w, r, p)
	}
}

// filesZip streams the selected files / folders as an uncompressed zip.
func (a *API) filesZip(w http.ResponseWriter, r *http.Request) {
	rels := r.URL.Query()["path"]
	if len(rels) == 0 {
		writeErr(w, errBad("nothing selected"))
		return
	}
	type item struct{ abs, name string }
	var roots []item
	for _, p := range rels {
		rel, err := cleanRel(p)
		if err != nil {
			writeErr(w, err)
			return
		}
		abs, err := a.m.savedAbs(rel)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !exists(abs) {
			writeErr(w, &httpError{404, "not found"})
			return
		}
		roots = append(roots, item{abs, filepath.Base(abs)})
	}
	zname := "saved.zip"
	if len(roots) == 1 {
		zname = roots[0].name + ".zip"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(zname))
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, root := range roots {
		parent := filepath.Dir(root.abs)
		_ = filepath.WalkDir(root.abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil || strings.HasPrefix(d.Name(), ".") || d.Type()&fs.ModeSymlink != 0 {
				if d != nil && d.IsDir() && err == nil {
					return fs.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(parent, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if _, e := zw.Create(rel + "/"); e != nil {
					return e
				}
				return nil
			}
			fi, e := d.Info()
			if e != nil {
				return nil
			}
			hdr, e := zip.FileInfoHeader(fi)
			if e != nil {
				return nil
			}
			hdr.Name, hdr.Method = rel, zip.Store
			wr, e := zw.CreateHeader(hdr)
			if e != nil {
				return e
			}
			f, e := os.Open(p)
			if e != nil {
				return nil
			}
			defer f.Close()
			_, e = io.Copy(wr, f)
			return e
		})
	}
}

func (a *API) filesTrim(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path  string  `json:"path"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Mode  string  `json:"mode"`
		Dest  *string `json:"dest"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if _, ok := a.savedFile(w, in.Path); !ok {
		return
	}
	j, err := a.m.Trim(savedPrefix+strings.Trim(in.Path, "/"), in.Start, in.End, in.Mode, in.Dest)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j)
}

func (a *API) filesConvert(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if _, ok := a.savedFile(w, in.Path); !ok {
		return
	}
	j, err := a.m.Convert(savedPrefix + strings.Trim(in.Path, "/"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, j)
}
