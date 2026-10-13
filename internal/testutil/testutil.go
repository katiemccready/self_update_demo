package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"selfupdate/internal/update/manifest"
)

func Quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ReadFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func Ver(t testing.TB, s string) manifest.Version {
	t.Helper()
	v, err := manifest.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func Ptr[T any](v T) *T { return &v }

func WriteTree(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type Member struct {
	Name, Body, Link string
	Type             byte
	Mode             int64
}

func Archive(t testing.TB, ms ...Member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range ms {
		typ, mode := m.Type, m.Mode
		if typ == 0 {
			typ = tar.TypeReg
		}
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: m.Name, Typeflag: typ, Mode: mode, Linkname: m.Link}
		if typ == tar.TypeReg {
			h.Size = int64(len(m.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(m.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
