package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// The "Saved" tab is a real directory tree (Config.SavedDir) managed like a
// file manager. Files are addressed by a *ref*:
//
//	"name.mp4"              a recording in the recordings directory
//	"saved/<rel>"           a file inside the Saved tree ("/" separated)
//
// Metadata (streamer, start time, ...) is keyed by the ref, so it follows
// files when they are renamed or moved.

const (
	savedPrefix = "saved/"
	savedRoot   = "saved"
)

func isSavedRef(ref string) bool { return strings.HasPrefix(ref, savedPrefix) }

// validName checks one path segment typed by the user (file or folder name).
func validName(n string) error {
	if n == "" || n != strings.TrimSpace(n) {
		return errBad("invalid name")
	}
	if len(n) > 255 || !utf8.ValidString(n) || n == "." || n == ".." || strings.HasPrefix(n, ".") ||
		strings.ContainsAny(n, "/\\\x00") || strings.HasSuffix(n, ".") {
		return errBad("invalid name")
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return errBad("invalid name")
		}
	}
	return nil
}

// cleanRel validates a relative path inside the Saved tree ("" = the root).
func cleanRel(rel string) (string, error) {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return "", nil
	}
	for _, seg := range strings.Split(rel, "/") {
		if validName(seg) != nil {
			return "", errBad("invalid path")
		}
	}
	return rel, nil
}

// safeJoin resolves rel below root and makes sure that neither the path nor a
// symlink in it leads outside root.
func safeJoin(root, rel string) (string, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	abs := root
	if rel != "" {
		abs = filepath.Join(root, filepath.FromSlash(rel))
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	// resolve the deepest existing ancestor
	p := abs
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
				return "", errBad("invalid path")
			}
			break
		}
		next := filepath.Dir(p)
		if next == p {
			return "", errBad("invalid path")
		}
		p = next
	}
	return abs, nil
}

func (m *Manager) savedAbs(rel string) (string, error) { return safeJoin(m.cfg.SavedDir, rel) }

// splitRef returns the directory part ("" = recordings dir, "saved/a/b") and base name.
func splitRef(ref string) (dirRef, name string) {
	if isSavedRef(ref) {
		i := strings.LastIndex(ref, "/")
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

func joinRef(dirRef, name string) string {
	if dirRef == "" {
		return name
	}
	return dirRef + "/" + name
}

// refPath maps a ref to an absolute file path.
func (m *Manager) refPath(ref string) (string, error) {
	if isSavedRef(ref) {
		return m.savedAbs(strings.TrimPrefix(ref, savedPrefix))
	}
	if !safeRecordingName(ref) {
		return "", errBad("invalid file name")
	}
	return filepath.Join(m.cfg.RecDir, ref), nil
}

// dirRefPath maps a directory ref ("", "saved", "saved/a") to an absolute path.
func (m *Manager) dirRefPath(dirRef string) (string, error) {
	switch {
	case dirRef == "":
		return m.cfg.RecDir, nil
	case dirRef == savedRoot:
		return m.cfg.SavedDir, nil
	case isSavedRef(dirRef + "/"):
		return m.savedAbs(strings.TrimPrefix(dirRef, savedPrefix))
	}
	return "", errBad("invalid folder")
}

// ------------------------------------------------------------ file system helpers

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// uniqueTarget returns a name that does not exist in dir yet ("a.mp4" -> "a_2.mp4").
func uniqueTarget(dir, name string, isDir bool) string {
	if !exists(filepath.Join(dir, name)) {
		return name
	}
	ext := ""
	base := name
	if !isDir {
		ext = filepath.Ext(name)
		base = strings.TrimSuffix(name, ext)
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", base, i, ext)
		if !exists(filepath.Join(dir, cand)) {
			return cand
		}
	}
}

func copyRegular(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chtimes(tmp, fi.ModTime(), fi.ModTime())
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// copyTree copies a directory recursively (symlinks and hidden files are skipped).
func copyTree(src, dst string) error {
	if err := os.Mkdir(dst, 0o750); err != nil {
		return err
	}
	es, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".") || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			err = copyTree(s, d)
		} else {
			err = copyRegular(s, d)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// moveEntry renames src to dst, falling back to copy + delete across file systems.
func moveEntry(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := copyTree(src, dst); err != nil {
			_ = os.RemoveAll(dst)
			return err
		}
		return os.RemoveAll(src)
	}
	if err := copyRegular(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func within(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// ------------------------------------------------------------ listing

type FileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"` // relative to the Saved root
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	Items    int       `json:"items,omitempty"` // folders: entries inside
	ModTime  time.Time `json:"mod_time"`
	Ext      string    `json:"ext,omitempty"`
	Media    bool      `json:"media"`
	Streamer string    `json:"streamer,omitempty"`
	Platform string    `json:"platform,omitempty"`
	Title    string    `json:"title,omitempty"`
	Started  time.Time `json:"started"`
}

type FileListing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
	Files   int         `json:"files"`
	Folders int         `json:"folders"`
	Size    int64       `json:"size"`
}

// mediaInfo works out streamer / platform / title / start for a media file.
func mediaInfo(key, name string, mod time.Time, metas map[string]RecMeta, channels []Channel, byID map[string]Channel) (streamer, platform, title string, started time.Time) {
	if md, ok := metas[key]; ok && md.Streamer != "" {
		streamer, platform, title, started = md.Streamer, md.Platform, md.Title, md.Started
		if c, ok := byID[md.ChannelID]; ok { // follow channel renames
			streamer = streamerName(c)
		}
	} else {
		streamer, platform, started = guessLegacy(name, mod, channels)
	}
	if started.IsZero() {
		started = mod
	}
	return
}

func channelIndex(channels []Channel) map[string]Channel {
	byID := map[string]Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
	return byID
}

// ListSaved lists one folder of the Saved tree (folders first, then by name).
func (m *Manager) ListSaved(rel string) (*FileListing, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	abs, err := m.savedAbs(rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, &httpError{404, "folder not found"}
	}
	if !fi.IsDir() {
		return nil, errBad("not a folder")
	}
	es, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	channels := m.st.Channels()
	byID := channelIndex(channels)
	metas := m.meta.snapshot()
	out := &FileListing{Path: rel, Entries: []FileEntry{}}
	for _, e := range es {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := name
		if rel != "" {
			p = rel + "/" + name
		}
		fe := FileEntry{Name: name, Path: p, Dir: e.IsDir(), ModTime: info.ModTime(), Started: info.ModTime()}
		if e.IsDir() {
			if sub, err := os.ReadDir(filepath.Join(abs, name)); err == nil {
				for _, s := range sub {
					if !strings.HasPrefix(s.Name(), ".") {
						fe.Items++
					}
				}
			}
			out.Folders++
		} else {
			fe.Size = info.Size()
			fe.Ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
			fe.Media = isMediaFile(name)
			if fe.Media {
				fe.Streamer, fe.Platform, fe.Title, fe.Started = mediaInfo(savedPrefix+p, name, info.ModTime(), metas, channels, byID)
			}
			out.Files++
			out.Size += fe.Size
		}
		out.Entries = append(out.Entries, fe)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out, nil
}

type FolderNode struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Depth int    `json:"depth"`
}

// SavedTree lists every folder (depth first, alphabetical) for the folder picker.
func (m *Manager) SavedTree() []FolderNode {
	out := []FolderNode{}
	var walk func(abs, rel string, depth int)
	walk = func(abs, rel string, depth int) {
		if len(out) >= 3000 || depth > 20 {
			return
		}
		es, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		var names []string
		for _, e := range es {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Type()&fs.ModeSymlink == 0 {
				names = append(names, e.Name())
			}
		}
		sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
		for _, n := range names {
			p := n
			if rel != "" {
				p = rel + "/" + n
			}
			out = append(out, FolderNode{Path: p, Name: n, Depth: depth})
			walk(filepath.Join(abs, n), p, depth+1)
		}
	}
	walk(m.cfg.SavedDir, "", 0)
	return out
}

// ------------------------------------------------------------ operations

type MoveResult struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type FailResult struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}
type OpResult struct {
	Done   []MoveResult `json:"done"`
	Failed []FailResult `json:"failed"`
}

func (m *Manager) Mkdir(parent, name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := m.savedAbs(parent)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", &httpError{404, "folder not found"}
	}
	if exists(filepath.Join(dir, name)) {
		return "", &httpError{409, "a file or folder with this name already exists"}
	}
	if err := os.Mkdir(filepath.Join(dir, name), 0o750); err != nil {
		return "", err
	}
	p, _ := cleanRel(parent)
	if p != "" {
		p += "/"
	}
	return p + name, nil
}

// RenameSaved renames a file or folder in place (never overwrites).
func (m *Manager) RenameSaved(rel, newName string) (string, error) {
	rel, err := cleanRel(rel)
	if err != nil || rel == "" {
		return "", errBad("invalid path")
	}
	newName = strings.TrimSpace(newName)
	if err := validName(newName); err != nil {
		return "", err
	}
	src, err := m.savedAbs(rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return "", &httpError{404, "not found"}
	}
	dst := filepath.Join(filepath.Dir(src), newName)
	if dst == src {
		return rel, nil
	}
	if exists(dst) && !strings.EqualFold(filepath.Base(src), newName) { // case-only renames are fine
		return "", &httpError{409, "a file or folder with this name already exists"}
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	newRel := newName
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		newRel = rel[:i+1] + newName
	}
	m.rekey(fi.IsDir(), savedPrefix+rel, savedPrefix+newRel)
	return newRel, nil
}

func (m *Manager) rekey(isDir bool, from, to string) {
	if isDir {
		m.meta.rekeyPrefix(from+"/", to+"/")
	} else {
		m.meta.rekey(from, to)
	}
}

// MoveSaved moves files and folders inside the Saved tree to the folder dest.
func (m *Manager) MoveSaved(paths []string, dest string) (*OpResult, error) {
	return m.transferSaved(paths, dest, false)
}

// CopySaved copies files and folders to the folder dest (names are made unique).
func (m *Manager) CopySaved(paths []string, dest string) (*OpResult, error) {
	return m.transferSaved(paths, dest, true)
}

func (m *Manager) transferSaved(paths []string, dest string, cp bool) (*OpResult, error) {
	dest, err := cleanRel(dest)
	if err != nil {
		return nil, err
	}
	dstDir, err := m.savedAbs(dest)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dstDir); err != nil || !fi.IsDir() {
		return nil, &httpError{404, "destination folder not found"}
	}
	res := &OpResult{Done: []MoveResult{}, Failed: []FailResult{}}
	fail := func(p string, err error) { res.Failed = append(res.Failed, FailResult{p, err.Error()}) }
	for _, p := range paths {
		rel, err := cleanRel(p)
		if err != nil || rel == "" {
			fail(p, errBad("invalid path"))
			continue
		}
		src, err := m.savedAbs(rel)
		if err != nil {
			fail(p, err)
			continue
		}
		fi, err := os.Lstat(src)
		if err != nil {
			fail(p, errors.New("not found"))
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			fail(p, errors.New("not supported"))
			continue
		}
		if fi.IsDir() && within(src, dstDir) {
			fail(p, errors.New("cannot put a folder into itself"))
			continue
		}
		if !cp && filepath.Dir(src) == dstDir { // already there
			res.Done = append(res.Done, MoveResult{rel, rel})
			continue
		}
		name := uniqueTarget(dstDir, filepath.Base(src), fi.IsDir())
		dst := filepath.Join(dstDir, name)
		newRel := name
		if dest != "" {
			newRel = dest + "/" + name
		}
		if cp {
			if fi.IsDir() {
				err = copyTree(src, dst)
				if err != nil {
					_ = os.RemoveAll(dst)
				}
			} else {
				err = copyRegular(src, dst)
			}
			if err == nil {
				m.copyMeta(fi.IsDir(), savedPrefix+rel, savedPrefix+newRel)
			}
		} else {
			err = moveEntry(src, dst)
			if err == nil {
				m.rekey(fi.IsDir(), savedPrefix+rel, savedPrefix+newRel)
			}
		}
		if err != nil {
			fail(p, err)
			continue
		}
		res.Done = append(res.Done, MoveResult{rel, newRel})
	}
	return res, nil
}

func (m *Manager) copyMeta(isDir bool, from, to string) {
	if isDir {
		m.meta.copyPrefix(from+"/", to+"/")
	} else {
		m.meta.copy(from, to)
	}
}

// DeleteSaved removes files and folders (folders recursively).
func (m *Manager) DeleteSaved(paths []string) *OpResult {
	res := &OpResult{Done: []MoveResult{}, Failed: []FailResult{}}
	for _, p := range paths {
		rel, err := cleanRel(p)
		var abs string
		if err == nil && rel != "" {
			abs, err = m.savedAbs(rel)
		} else if err == nil {
			err = errBad("invalid path")
		}
		if err == nil {
			if exists(abs) {
				err = os.RemoveAll(abs)
			}
		}
		if err != nil {
			res.Failed = append(res.Failed, FailResult{p, err.Error()})
			continue
		}
		m.meta.removePrefix(savedPrefix + rel)
		res.Done = append(res.Done, MoveResult{From: rel})
	}
	return res
}

// SaveRecordings moves recordings into the Saved tree (folder dest, "" = root).
func (m *Manager) SaveRecordings(names []string, dest string) (*OpResult, error) {
	dest, err := cleanRel(dest)
	if err != nil {
		return nil, err
	}
	dstDir, err := m.savedAbs(dest)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dstDir); err != nil || !fi.IsDir() {
		return nil, &httpError{404, "destination folder not found"}
	}
	res := &OpResult{Done: []MoveResult{}, Failed: []FailResult{}}
	for _, n := range names {
		var err error
		switch {
		case !safeRecordingName(n):
			err = errBad("invalid file name")
		case m.ActiveFile(n):
			err = &httpError{409, "file is being recorded"}
		case m.jobBusy(n):
			err = &httpError{409, "file is being processed"}
		}
		if err == nil {
			src := filepath.Join(m.cfg.RecDir, n)
			if !exists(src) {
				err = &httpError{404, "file not found"}
			} else {
				name := uniqueTarget(dstDir, n, false)
				rel := name
				if dest != "" {
					rel = dest + "/" + name
				}
				if err = moveEntry(src, filepath.Join(dstDir, name)); err == nil {
					m.meta.rekey(n, savedPrefix+rel)
					res.Done = append(res.Done, MoveResult{n, rel})
					continue
				}
			}
		}
		res.Failed = append(res.Failed, FailResult{n, err.Error()})
	}
	return res, nil
}

// RestoreRecordings moves files from the Saved tree back to the recordings list.
func (m *Manager) RestoreRecordings(paths []string) *OpResult {
	res := &OpResult{Done: []MoveResult{}, Failed: []FailResult{}}
	for _, p := range paths {
		rel, err := cleanRel(p)
		var src string
		if err == nil {
			if rel == "" || !isMediaFile(rel) {
				err = errBad("only video files can go back to recordings")
			} else {
				src, err = m.savedAbs(rel)
			}
		}
		if err == nil {
			if fi, e := os.Lstat(src); e != nil || !fi.Mode().IsRegular() {
				err = &httpError{404, "file not found"}
			}
		}
		if err == nil {
			name := uniqueName(m.cfg.RecDir, filepath.Base(src))
			if err = moveEntry(src, filepath.Join(m.cfg.RecDir, name)); err == nil {
				m.meta.rekey(savedPrefix+rel, name)
				res.Done = append(res.Done, MoveResult{rel, name})
				continue
			}
		}
		res.Failed = append(res.Failed, FailResult{p, err.Error()})
	}
	return res
}

// MigrateSaved moves the files of the old "saved" flag list (saved.json) into
// the Saved tree. It runs once; the list is renamed afterwards.
func (m *Manager) MigrateSaved(listPath string) {
	b, err := os.ReadFile(listPath)
	if err != nil {
		return
	}
	names := parseNameList(b)
	if err := ensureDir(m.cfg.SavedDir); err != nil {
		return
	}
	for _, n := range names {
		if !safeRecordingName(n) {
			continue
		}
		src := filepath.Join(m.cfg.RecDir, n)
		if !exists(src) {
			continue
		}
		name := uniqueTarget(m.cfg.SavedDir, n, false)
		if moveEntry(src, filepath.Join(m.cfg.SavedDir, name)) == nil {
			m.meta.rekey(n, savedPrefix+name)
		}
	}
	_ = os.Rename(listPath, listPath+".migrated")
}
