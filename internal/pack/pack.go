package pack

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrUnsafeArchive = errors.New("unsafe archive")

type Limits struct {
	MaxFiles   int
	MaxBytes   int64
	MaxPathLen int
}

var DefaultLimits = Limits{MaxFiles: 50_000, MaxBytes: 4 << 30, MaxPathLen: 240}

type Entry struct {
	Path   string
	Size   int64
	Exec   bool
	SHA256 string
}

type Tree struct {
	Files []Entry
	Dirs  []string
}

func (t Tree) find(p string) (Entry, bool) {
	for _, e := range t.Files {
		if e.Path == p {
			return e, true
		}
	}
	return Entry{}, false
}

func unsafef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafeArchive, fmt.Sprintf(format, a...))
}

var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func reservedDeviceName(element string) bool {
	stem, _, _ := strings.Cut(element, ".")
	stem = strings.ToUpper(stem)
	if windowsReserved[stem] {
		return true
	}
	return len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9'
}

func cleanName(name string, maxLen int) (string, error) {
	name = strings.TrimSuffix(name, "/")
	switch {
	case name == "":
		return "", unsafef("empty path")
	case len(name) > maxLen:
		return "", unsafef("path longer than %d bytes: %.40q...", maxLen, name)
	case !utf8.ValidString(name):
		return "", unsafef("path is not valid UTF-8: %q", name)
	case strings.HasPrefix(name, "/"):
		return "", unsafef("absolute path %q", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return "", unsafef("path %q contains a character that is not portable (%q)", name, r)
		}
	}
	for _, el := range strings.Split(name, "/") {
		switch {
		case el == "" || el == "." || el == "..":
			return "", unsafef("path %q has an empty, '.' or '..' element", name)
		case strings.HasSuffix(el, ".") || strings.HasSuffix(el, " "):
			return "", unsafef("path %q has an element ending in a dot or space", name)
		case reservedDeviceName(el):
			return "", unsafef("path %q uses a reserved device name", name)
		}
	}
	return name, nil
}

var execExtensions = map[string]bool{".exe": true, ".dll": true, ".bat": true, ".cmd": true, ".com": true, ".sh": true}

func isExec(mode int64, name, entry string) bool {
	return mode&0o111 != 0 || execExtensions[strings.ToLower(path.Ext(name))] || name == entry
}

type sink interface {
	dir(p string) error
	file(p string, exec bool, size int64, body io.Reader) error
}

func walk(r io.Reader, entry string, lim Limits, s sink) (Tree, error) {
	if lim.MaxFiles <= 0 || lim.MaxBytes <= 0 || lim.MaxPathLen <= 0 {
		lim = DefaultLimits
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Tree{}, unsafef("not a gzip stream: %v", err)
	}
	defer gz.Close()

	budget := lim.MaxBytes + int64(lim.MaxFiles)*2048 + 1<<20
	tr := tar.NewReader(&budgetReader{r: gz, left: budget})

	var tree Tree
	seen := map[string]bool{}
	isDir := map[string]bool{}
	var total int64
	count := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Tree{}, unsafef("%v", err)
		}
		if count++; count > lim.MaxFiles {
			return Tree{}, unsafef("more than %d entries", lim.MaxFiles)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return Tree{}, unsafef("%q is a %s; only regular files and directories are allowed", h.Name, typeName(h.Typeflag))
		}
		name, err := cleanName(h.Name, lim.MaxPathLen)
		if err != nil {
			return Tree{}, err
		}
		if err := claim(name, h.Typeflag == tar.TypeDir, seen, isDir); err != nil {
			return Tree{}, err
		}
		if h.Typeflag == tar.TypeDir {
			if err := s.dir(name); err != nil {
				return Tree{}, err
			}
			tree.Dirs = append(tree.Dirs, name)
			continue
		}
		if h.Size < 0 {
			return Tree{}, unsafef("%q has a negative size", name)
		}
		if total += h.Size; total > lim.MaxBytes {
			return Tree{}, unsafef("expands to more than %d bytes", lim.MaxBytes)
		}
		exec := isExec(h.Mode, name, entry)
		sum := sha256.New()
		if err := s.file(name, exec, h.Size, io.TeeReader(io.LimitReader(tr, h.Size), sum)); err != nil {
			return Tree{}, err
		}
		tree.Files = append(tree.Files, Entry{Path: name, Size: h.Size, Exec: exec, SHA256: hex.EncodeToString(sum.Sum(nil))})
	}

	if _, err := io.Copy(io.Discard, tr); err != nil {
		return Tree{}, unsafef("%v", err)
	}
	if _, err := io.Copy(io.Discard, &budgetReader{r: gz, left: budget}); err != nil {
		return Tree{}, unsafef("trailing data: %v", err)
	}
	if e, ok := tree.find(entry); !ok || e.Size == 0 {
		return Tree{}, unsafef("the package has no program %q (or it is empty)", entry)
	}
	slices.SortFunc(tree.Files, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(tree.Dirs)
	return tree, nil
}

func typeName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device node"
	case tar.TypeFifo:
		return "fifo"
	default:
		return fmt.Sprintf("entry of type %q", t)
	}
}

func claim(name string, dir bool, seen, isDir map[string]bool) error {
	key := strings.ToLower(name)
	parts := strings.Split(key, "/")
	for i := 1; i < len(parts); i++ {
		anc := strings.Join(parts[:i], "/")
		if seen[anc] && !isDir[anc] {
			return unsafef("%q is inside %q, which is a file", name, anc)
		}
		seen[anc], isDir[anc] = true, true
	}

	if seen[key] && !(dir && isDir[key]) {
		return unsafef("duplicate or conflicting path %q", name)
	}
	seen[key] = true
	isDir[key] = dir
	return nil
}

type budgetReader struct {
	r    io.Reader
	left int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("archive expands beyond the allowed size")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

type hashSink struct{}

func (hashSink) dir(string) error { return nil }
func (hashSink) file(_ string, _ bool, size int64, body io.Reader) error {
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		return err
	}
	if n != size {
		return unsafef("file ended after %d of %d bytes", n, size)
	}
	return nil
}

func Inspect(r io.Reader, entry string, lim Limits) (Tree, error) {
	return walk(r, entry, lim, hashSink{})
}

type diskSink struct{ dst string }

func (d diskSink) abs(p string) string { return filepath.Join(d.dst, filepath.FromSlash(p)) }

func (d diskSink) dir(p string) error { return os.MkdirAll(d.abs(p), 0o755) }

func (d diskSink) file(p string, exec bool, size int64, body io.Reader) (err error) {
	target := d.abs(p)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	n, err := io.Copy(f, body)
	if err != nil {
		return err
	}
	if n != size {
		return unsafef("%q ended after %d of %d bytes", p, n, size)
	}

	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return f.Sync()
}

func Extract(r io.Reader, dst, entry string, lim Limits) (Tree, error) {
	return walk(r, entry, lim, diskSink{dst})
}

func (t Tree) allowed() (files map[string]Entry, dirs map[string]bool) {
	files = map[string]Entry{}
	dirs = map[string]bool{".": true}
	for _, e := range t.Files {
		files[e.Path] = e
		for p := path.Dir(e.Path); p != "."; p = path.Dir(p) {
			dirs[p] = true
		}
	}
	for _, d := range t.Dirs {
		for p := d; p != "."; p = path.Dir(p) {
			dirs[p] = true
		}
	}
	return files, dirs
}

func (t Tree) Prune(dst string) error {
	files, dirs := t.allowed()
	var doomed []string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if !dirs[rel] {
				doomed = append(doomed, p)
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := files[rel]; !ok || !d.Type().IsRegular() {
			doomed = append(doomed, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range doomed {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func (t Tree) Verify(dst string) error {
	files, dirs := t.allowed()
	found := map[string]bool{}
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if !dirs[rel] {
				return fmt.Errorf("unexpected directory %q", rel)
			}
			return nil
		}
		want, ok := files[rel]
		switch {
		case !ok:
			return fmt.Errorf("unexpected file %q", rel)
		case !d.Type().IsRegular():
			return fmt.Errorf("%q is not a regular file", rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() != want.Size {
			return fmt.Errorf("%q has size %d, package says %d", rel, fi.Size(), want.Size)
		}
		if runtime.GOOS != "windows" && (fi.Mode().Perm()&0o111 != 0) != want.Exec {
			return fmt.Errorf("%q has mode %o, package says executable=%v", rel, fi.Mode().Perm(), want.Exec)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
			return fmt.Errorf("%q changed after it was verified", rel)
		}
		found[rel] = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("version directory does not match its package: %w", err)
	}
	for p := range files {
		if !found[p] {
			return fmt.Errorf("version directory does not match its package: %q is missing", p)
		}
	}
	return nil
}

var epoch = time.Unix(0, 0).UTC()

func Build(dir, entry string, w io.Writer) error {
	type item struct {
		rel, abs string
		dir      bool
		exec     bool
		size     int64
	}
	var items []item
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, err := cleanName(rel, DefaultLimits.MaxPathLen); err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			items = append(items, item{rel: rel, abs: p, dir: true})
		case fi.Mode().IsRegular():
			items = append(items, item{rel: rel, abs: p, exec: isExec(int64(fi.Mode().Perm()), rel, entry), size: fi.Size()})
		default:
			return unsafef("%q is a %s; packages may contain only regular files and directories", rel, fi.Mode().Type())
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.rel, b.rel) })
	if !slices.ContainsFunc(items, func(it item) bool { return !it.dir && it.rel == entry }) {
		return unsafef("the entry point %q is not a file in %s", entry, dir)
	}

	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, it := range items {
		h := &tar.Header{Name: it.rel, ModTime: epoch, Format: tar.FormatPAX}
		if it.dir {
			h.Name += "/"
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		} else {
			h.Typeflag, h.Size, h.Mode = tar.TypeReg, it.size, 0o644
			if it.exec {
				h.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if it.dir {
			continue
		}
		if err := copyFile(tw, it.abs, it.size); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func copyFile(w io.Writer, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while it was being packed", p)
	}
	return nil
}
