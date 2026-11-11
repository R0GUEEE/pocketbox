package buildkitclient

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one file created by an instruction.
type Entry struct {
	// Path is relative to the container root, without a leading slash.
	Path     string
	Mode     fs.FileMode
	Uid, Gid int
	Content  []byte
	Link     string // symlink target
	Dir      bool
	ModTime  time.Time
	// Whiteout removes Path instead of creating it.
	Whiteout bool
}

// Layer accumulates the changes of one instruction.
type Layer struct {
	Entries []Entry
}

// AddFile adds or replaces a regular file.
func (l *Layer) AddFile(p string, mode fs.FileMode, content []byte) {
	l.Entries = append(l.Entries, Entry{Path: clean(p), Mode: mode, Content: content, ModTime: time.Unix(0, 0).UTC()})
}

// AddDir adds a directory.
func (l *Layer) AddDir(p string, mode fs.FileMode) {
	l.Entries = append(l.Entries, Entry{Path: clean(p), Mode: mode | fs.ModeDir, Dir: true, ModTime: time.Unix(0, 0).UTC()})
}

// AddSymlink adds a symlink.
func (l *Layer) AddSymlink(p, target string) {
	l.Entries = append(l.Entries, Entry{Path: clean(p), Mode: fs.ModeSymlink | 0o777, Link: target, ModTime: time.Unix(0, 0).UTC()})
}

// Remove records a whiteout for p.
func (l *Layer) Remove(p string) {
	l.Entries = append(l.Entries, Entry{Path: clean(p), Whiteout: true})
}

// Empty reports whether the layer has no changes.
func (l *Layer) Empty() bool { return len(l.Entries) == 0 }

func clean(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// Tar writes the layer as an uncompressed tar stream. Whiteouts are emitted
// first, deepest paths before their parents, so extraction cannot resurrect a
// file through a directory entry.
func (l *Layer) Tar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := append([]Entry(nil), l.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Whiteout != entries[j].Whiteout {
			return entries[i].Whiteout
		}
		return strings.Count(entries[i].Path, "/") > strings.Count(entries[j].Path, "/")
	})
	seenDirs := map[string]bool{}
	for _, e := range entries {
		if e.Whiteout {
			dir, base := path.Split(e.Path)
			name := path.Join(dir, ".wh."+base)
			if err := writeHeader(tw, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0, Size: 0, ModTime: e.ModTime}); err != nil {
				return nil, err
			}
			continue
		}
		// Implicit parents keep the image well formed for runtimes that walk
		// tar headers instead of the filesystem.
		for d := path.Dir(e.Path); d != "." && d != "/" && !seenDirs[d]; d = path.Dir(d) {
			seenDirs[d] = true
			if err := writeHeader(tw, &tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: e.ModTime}); err != nil {
				return nil, err
			}
		}
		switch {
		case e.Dir:
			if err := writeHeader(tw, &tar.Header{Name: e.Path + "/", Typeflag: tar.TypeDir, Mode: int64(e.Mode.Perm()), ModTime: e.ModTime}); err != nil {
				return nil, err
			}
		case e.Mode&fs.ModeSymlink != 0:
			if err := writeHeader(tw, &tar.Header{Name: e.Path, Typeflag: tar.TypeSymlink, Linkname: e.Link, Mode: 0o777, ModTime: e.ModTime}); err != nil {
				return nil, err
			}
		default:
			h := &tar.Header{Name: e.Path, Typeflag: tar.TypeReg, Mode: int64(e.Mode.Perm()), Size: int64(len(e.Content)), ModTime: e.ModTime}
			if err := writeHeader(tw, h); err != nil {
				return nil, err
			}
			if _, err := tw.Write(e.Content); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeHeader(tw *tar.Writer, h *tar.Header) error {
	if h.ModTime.IsZero() {
		h.ModTime = time.Unix(0, 0).UTC()
	}
	return tw.WriteHeader(h)
}

// Gzip compresses b.
func Gzip(b []byte) ([]byte, error) {
	var out bytes.Buffer
	gw, err := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := gw.Write(b); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DiffID returns the digest of the uncompressed tar, which is what an image
// config lists for the layer.
func DiffID(tarBytes []byte) string {
	sum := sha256.Sum256(tarBytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// BuildContext is a Dockerfile build context read from a directory or archive.
type BuildContext struct {
	// Dir is the extracted context directory.
	Dir string
	// Ignore holds .dockerignore patterns (already split into lines).
	Ignore []string
}

// ReadFile reads a path from the context, rejecting traversal.
func (c *BuildContext) ReadFile(p string) ([]byte, error) {
	cleanPath := clean(p)
	if strings.HasPrefix(cleanPath, "..") {
		return nil, fmt.Errorf("context: path %q escapes the context", p)
	}
	full := filepath.Join(c.Dir, filepath.FromSlash(cleanPath))
	rel, err := filepath.Rel(c.Dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("context: path %q escapes the context", p)
	}
	return os.ReadFile(full)
}

// Walk visits context files that are not excluded by .dockerignore.
func (c *BuildContext) Walk(fn func(rel string, info fs.FileInfo) error) error {
	return filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.Dir, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if c.Excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(rel, info)
	})
}

// Excluded evaluates the context's .dockerignore patterns. Matching follows
// Docker: patterns are anchored at the context root, `*` does not cross a
// slash, and a directory match excludes everything below it.
func (c *BuildContext) Excluded(rel string, isDir bool) bool {
	excluded := false
	for _, line := range c.Ignore {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		neg := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		line = strings.TrimSuffix(strings.TrimPrefix(line, "/"), "/")
		if line == "" {
			continue
		}
		matched := ignoreMatch(line, rel)
		if !matched && strings.Contains(line, "/") {
			// A slash-anchored pattern also covers the directory it names:
			// `app/static/` excludes every file below `app/static`.
			matched = rel == line || strings.HasPrefix(rel, line+"/")
		} else if !matched {
			// Bare patterns match at any depth, like .gitignore.
			for p := rel; p != "." && p != "/"; p = path.Dir(p) {
				if ignoreMatch(line, p) {
					matched = true
					break
				}
			}
		}
		if matched {
			excluded = !neg
		}
	}
	return excluded
}

func ignoreMatch(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.Contains(pattern, "**") {
		return matchDoubleStar(pattern, name)
	}
	if ok, err := path.Match(pattern, name); err == nil && ok {
		return true
	}
	if ok, err := path.Match(pattern, path.Base(name)); err == nil && ok && !strings.Contains(pattern, "/") {
		return true
	}
	return false
}

func matchDoubleStar(pattern, name string) bool {
	parts := strings.Split(pattern, "**")
	if len(parts) != 2 {
		return false
	}
	head := strings.TrimSuffix(parts[0], "/")
	tail := strings.TrimPrefix(parts[1], "/")
	if head != "" && !strings.HasPrefix(name, head) {
		return false
	}
	if tail == "" {
		return true
	}
	ok, _ := path.Match(tail, path.Base(name))
	return ok
}

// CopyToLayer copies context paths into a layer, following Docker's COPY
// semantics for files, directories and trailing slashes.
func (c *BuildContext) CopyToLayer(l *Layer, sources []string, dest string, includeAll bool) error {
	if len(sources) == 0 {
		return fmt.Errorf("COPY: no source")
	}
	destIsDir := strings.HasSuffix(dest, "/") || len(sources) > 1
	for _, src := range sources {
		trimmed := strings.TrimPrefix(src, "/")
		full := filepath.Join(c.Dir, filepath.FromSlash(trimmed))
		// .dockerignore patterns are written relative to the context root, so
		// the source keeps its context-relative name while being copied.
		ctxPath := filepath.ToSlash(trimmed)
		if src == "." || trimmed == "" {
			ctxPath = ""
		}
		info, err := os.Stat(full)
		if err != nil {
			if strings.ContainsAny(src, "*?[") {
				matches, _ := filepath.Glob(filepath.Join(c.Dir, filepath.FromSlash(trimmed)))
				for _, m := range matches {
					sub, _ := filepath.Rel(c.Dir, m)
					if err := copyOneFrom(c, l, m, dest, destIsDir, includeAll, filepath.ToSlash(sub)); err != nil {
						return err
					}
				}
				continue
			}
			return fmt.Errorf("COPY: %w", err)
		}
		_ = info
		if err := copyOneFrom(c, l, full, dest, destIsDir, includeAll, ctxPath); err != nil {
			return err
		}
	}
	return nil
}

// copyOneFrom copies one path; ctxPrefix is the path the source had inside the
// build context, so .dockerignore patterns and traversal checks see it.
func copyOneFrom(c *BuildContext, l *Layer, full, dest string, destIsDir, includeAll bool, ctxPrefix string) error {
	rel, err := filepath.Rel(c.Dir, full)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	root := rel
	if ctxPrefix != "" {
		root = ctxPrefix
	}
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		target := dest
		if destIsDir {
			target = path.Join(dest, path.Base(rel))
		}
		l.AddFile(target, info.Mode().Perm(), data)
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("COPY: %s is not a regular file or directory", rel)
	}
	// Directory sources: `/dest` receives the source's contents, `/dest/`
	// receives the source directory itself, matching Docker.
	base := path.Base(rel)
	if !destIsDir {
		base = ""
	}
	var walkErr error
	err = filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, _ := filepath.Rel(full, p)
		sub = filepath.ToSlash(sub)
		if sub != "." && !includeAll && c.Excluded(path.Join(root, sub), d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := path.Join(dest, base, sub)
		if sub == "." {
			target = path.Join(dest, base)
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case st.IsDir():
			l.AddDir(target, st.Mode().Perm())
		case st.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			l.AddSymlink(target, link)
		case st.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			l.AddFile(target, st.Mode().Perm(), data)
		}
		return nil
	})
	if walkErr == nil {
		walkErr = err
	}
	return walkErr
}

// UnpackTar extracts a context archive (tar, possibly gzip) into dir.
func UnpackTar(r io.Reader, dir string) error {
	br := &peekReader{r: r}
	f, err := br.peek(2)
	if err != nil && err != io.EOF {
		return err
	}
	var rd io.Reader = br
	if len(f) == 2 && f[0] == 0x1f && f[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		rd = gz
	}
	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + hdr.Name)
		target := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(name, "/")))
		if !strings.HasPrefix(target, dir) {
			return fmt.Errorf("context: entry %q escapes the directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
}

type peekReader struct {
	r   io.Reader
	buf []byte
}

func (p *peekReader) peek(n int) ([]byte, error) {
	for len(p.buf) < n {
		b := make([]byte, n-len(p.buf))
		k, err := p.r.Read(b)
		p.buf = append(p.buf, b[:k]...)
		if err != nil {
			return p.buf, err
		}
	}
	return p.buf, nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}
	return p.r.Read(b)
}
