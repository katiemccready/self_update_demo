package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"selfupdate/internal/testutil"
)

func good() []testutil.Member {
	return []testutil.Member{
		{Name: "app", Body: "#!/bin/sh\n", Mode: 0o755},
		{Name: "data/", Type: tar.TypeDir, Mode: 0o755},
		{Name: "data/greeting.txt", Body: "hello"},
	}
}

func extract(t *testing.T, a []byte) (string, Tree, error) {
	t.Helper()
	dst := t.TempDir()
	tree, err := Extract(bytes.NewReader(a), dst, "app", Limits{})
	return dst, tree, err
}

func TestExtractWritesTheWholeTree(t *testing.T) {
	dst, tree, err := extract(t, testutil.Archive(t, good()...))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "data", "greeting.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("data file = %q, %v", b, err)
	}
	if len(tree.Files) != 2 || tree.Files[0].Path != "app" || !tree.Files[0].Exec {
		t.Errorf("tree = %+v", tree)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dst, "app")); fi.Mode().Perm() != 0o755 {
			t.Errorf("app mode = %o, want 755 whatever the umask", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Join(dst, "data", "greeting.txt")); fi.Mode().Perm()&0o111 != 0 {
			t.Errorf("data file is executable: %o", fi.Mode().Perm())
		}
	}
	if err := tree.Verify(dst); err != nil {
		t.Errorf("a fresh extraction does not verify: %v", err)
	}
}

func TestHostileArchivesAreRefused(t *testing.T) {
	big := strings.Repeat("x", 2000)
	cases := map[string][]testutil.Member{
		"parent traversal":    {{Name: "app", Body: "x"}, {Name: "../escaped", Body: "x"}},
		"nested traversal":    {{Name: "app", Body: "x"}, {Name: "a/../../escaped", Body: "x"}},
		"absolute path":       {{Name: "app", Body: "x"}, {Name: "/etc/passwd", Body: "x"}},
		"backslash path":      {{Name: "app", Body: "x"}, {Name: `a\..\b`, Body: "x"}},
		"drive letter":        {{Name: "app", Body: "x"}, {Name: "C:/x", Body: "x"}},
		"alternate stream":    {{Name: "app", Body: "x"}, {Name: "app:stream", Body: "x"}},
		"dot element":         {{Name: "app", Body: "x"}, {Name: "a/./b", Body: "x"}},
		"trailing dot":        {{Name: "app", Body: "x"}, {Name: "x.", Body: "x"}},
		"device name":         {{Name: "app", Body: "x"}, {Name: "nul.txt", Body: "x"}},
		"control character":   {{Name: "app", Body: "x"}, {Name: "a\x01b", Body: "x"}},
		"symlink":             {{Name: "app", Body: "x"}, {Name: "link", Type: tar.TypeSymlink, Link: "/etc/passwd"}},
		"hard link":           {{Name: "app", Body: "x"}, {Name: "link", Type: tar.TypeLink, Link: "app"}},
		"device node":         {{Name: "app", Body: "x"}, {Name: "dev", Type: tar.TypeChar}},
		"fifo":                {{Name: "app", Body: "x"}, {Name: "pipe", Type: tar.TypeFifo}},
		"duplicate":           {{Name: "app", Body: "x"}, {Name: "app", Body: "y"}},
		"duplicate by case":   {{Name: "app", Body: "x"}, {Name: "Data.txt", Body: "x"}, {Name: "data.TXT", Body: "y"}},
		"file then directory": {{Name: "app", Body: "x"}, {Name: "a", Body: "x"}, {Name: "a/b", Body: "x"}},
		"no entry program":    {{Name: "other", Body: "x"}},
		"empty entry program": {{Name: "app"}},
		"entry is a dir":      {{Name: "app/", Type: tar.TypeDir}},
		"too long path":       {{Name: "app", Body: "x"}, {Name: strings.Repeat("a/", 200) + "b", Body: big}},
	}
	for name, ms := range cases {
		t.Run(name, func(t *testing.T) {
			dst, _, err := extract(t, testutil.Archive(t, ms...))
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("err = %v, want ErrUnsafeArchive", err)
			}

			parent := filepath.Dir(dst)
			for _, p := range []string{"escaped", "passwd", "link"} {
				if _, statErr := os.Lstat(filepath.Join(parent, p)); statErr == nil {
					t.Errorf("%s was created outside the destination", p)
				}
			}
		})
	}
}

func TestOversizedArchivesAreRefused(t *testing.T) {
	small := Limits{MaxFiles: 3, MaxBytes: 100, MaxPathLen: 50}
	cases := map[string][]testutil.Member{
		"too many entries": {{Name: "app", Body: "x"}, {Name: "a", Body: "x"}, {Name: "b", Body: "x"}, {Name: "c", Body: "x"}},
		"too many bytes":   {{Name: "app", Body: "x"}, {Name: "big", Body: strings.Repeat("z", 200)}},
	}
	for name, ms := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Extract(bytes.NewReader(testutil.Archive(t, ms...)), t.TempDir(), "app", small)
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDecompressionBombIsStopped(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "app", Typeflag: tar.TypeReg, Mode: 0o755, Size: 10})
	tw.Write([]byte("0123456789"))

	tw.Close()
	gz.Write(make([]byte, 64<<20))
	gz.Close()
	if buf.Len() > 1<<20 {
		t.Fatalf("test archive is not a bomb: %d bytes compressed", buf.Len())
	}
	_, err := Extract(&buf, t.TempDir(), "app", Limits{MaxFiles: 10, MaxBytes: 1 << 20, MaxPathLen: 100})
	if err == nil {
		t.Fatal("a 64 MiB trailer was accepted under a 1 MiB limit")
	}
}

func TestTruncatedArchiveIsRefused(t *testing.T) {
	a := testutil.Archive(t, good()...)
	if _, err := Inspect(bytes.NewReader(a[:len(a)-12]), "app", Limits{}); err == nil {
		t.Fatal("a truncated archive passed")
	}

	c := append([]byte(nil), a...)
	c[len(c)-6] ^= 0xff
	if _, err := Inspect(bytes.NewReader(c), "app", Limits{}); err == nil {
		t.Fatal("a corrupted gzip trailer passed")
	}
}

func TestPruneRemovesWhatTheCandidateLeftBehind(t *testing.T) {
	dst, tree, err := extract(t, testutil.Archive(t, good()...))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dst, "cache.log"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dst, "tmp", "deep"), 0o755)
	os.WriteFile(filepath.Join(dst, "tmp", "deep", "f"), []byte("x"), 0o644)
	if tree.Verify(dst) == nil {
		t.Fatal("Verify ignored extra files")
	}
	if err := tree.Prune(dst); err != nil {
		t.Fatal(err)
	}
	if err := tree.Verify(dst); err != nil {
		t.Fatalf("after Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "tmp")); err == nil {
		t.Error("extra directory survived Prune")
	}
}

func TestVerifyCatchesTampering(t *testing.T) {
	mutations := map[string]func(dst string){
		"changed content": func(dst string) { os.WriteFile(filepath.Join(dst, "data", "greeting.txt"), []byte("HELLO"), 0o644) },
		"deleted file":    func(dst string) { os.Remove(filepath.Join(dst, "data", "greeting.txt")) },
		"extra file":      func(dst string) { os.WriteFile(filepath.Join(dst, "evil.dll"), []byte("x"), 0o755) },
		"replaced by symlink": func(dst string) {
			p := filepath.Join(dst, "data", "greeting.txt")
			os.Remove(p)
			os.Symlink("/etc/hostname", p)
		},
	}
	if runtime.GOOS != "windows" {
		mutations["lost exec bit"] = func(dst string) { os.Chmod(filepath.Join(dst, "app"), 0o644) }
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			dst, tree, err := extract(t, testutil.Archive(t, good()...))
			if err != nil {
				t.Fatal(err)
			}
			mutate(dst)
			if err := tree.Verify(dst); err == nil {
				t.Fatal("Verify passed")
			}
		})
	}
}

func TestBuildIsDeterministicAndRoundTrips(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "assets", "empty"), 0o755)
	os.WriteFile(filepath.Join(src, "app"), []byte("program"), 0o755)
	os.WriteFile(filepath.Join(src, "assets", "a.txt"), []byte("A"), 0o600)
	os.WriteFile(filepath.Join(src, "lib.dll"), []byte("L"), 0o644)

	var first, second bytes.Buffer
	if err := Build(src, "app", &first); err != nil {
		t.Fatal(err)
	}

	os.Chtimes(filepath.Join(src, "app"), epoch.AddDate(40, 0, 0), epoch.AddDate(40, 0, 0))
	if err := Build(src, "app", &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("same directory produced different archives")
	}

	dst := t.TempDir()
	tree, err := Extract(bytes.NewReader(first.Bytes()), dst, "app", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Verify(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "assets", "empty")); err != nil {
		t.Errorf("empty directory lost: %v", err)
	}
	if e, _ := tree.find("lib.dll"); !e.Exec {
		t.Error(".dll should unpack as executable even when built where modes are absent")
	}
	if e, _ := tree.find("assets/a.txt"); e.Exec {
		t.Error("a data file became executable")
	}
}

func TestBuildRefusesWhatExtractWouldRefuse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "app"), []byte("x"), 0o755)
	os.Symlink("app", filepath.Join(src, "link"))
	if err := Build(src, "app", &bytes.Buffer{}); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("symlink: err = %v", err)
	}
	os.Remove(filepath.Join(src, "link"))
	os.WriteFile(filepath.Join(src, "CON"), []byte("x"), 0o644)
	if err := Build(src, "app", &bytes.Buffer{}); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("device Name: err = %v", err)
	}
	os.Remove(filepath.Join(src, "CON"))
	var b bytes.Buffer
	Build(src, "app", &b)
	if _, err := Inspect(&b, "missing", Limits{}); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("Inspect without the entry: %v", err)
	}
}

func TestCleanName(t *testing.T) {
	for name, valid := range map[string]bool{"bin/app": true, "../x": false, "a\\b": false} {
		got, err := cleanName(name, 100)
		if (err == nil) != valid || (valid && got != name) || (!valid && got != "") {
			t.Errorf("cleanName(%q) = %q, %v", name, got, err)
		}
	}
}

func TestExtractNeverWritesThroughAnExistingPath(t *testing.T) {
	a := testutil.Archive(t, good()...)
	dst := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(outside, []byte("original"), 0o644)
	if runtime.GOOS == "windows" {
		os.WriteFile(filepath.Join(dst, "app"), []byte("planted"), 0o644)
	} else if err := os.Symlink(outside, filepath.Join(dst, "app")); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(bytes.NewReader(a), dst, "app", Limits{}); err == nil {
		t.Fatal("extraction succeeded over a planted path")
	}
	if b, _ := os.ReadFile(outside); string(b) != "original" {
		t.Errorf("a planted symlink redirected the write: %q", b)
	}
}

func TestBuildRequiresTheEntryPoint(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	for _, entry := range []string{"missing", "app"} {
		var buf bytes.Buffer
		if err := Build(dir, entry, &buf); !errors.Is(err, ErrUnsafeArchive) {
			t.Errorf("entry %q: err = %v", entry, err)
		}
	}
}
