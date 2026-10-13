package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"selfupdate/internal/pack"
	"selfupdate/internal/testutil"
)

const testEntry = "app"

func writeVersion(t *testing.T, dir, label string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testEntry), []byte("program "+label), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "file.txt"), []byte("data "+label), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	root := Root{Dir: t.TempDir()}
	src := t.TempDir()
	writeVersion(t, src, "1.0.0")
	var pkg bytes.Buffer
	if err := pack.Build(src, testEntry, &pkg); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "launcher-src")
	os.WriteFile(launcher, []byte("launcher"), 0o755)
	if err := Bootstrap(root, launcher, &pkg, testutil.Ver(t, "1.0.0"), testEntry); err != nil {
		t.Fatal(err)
	}
	return Open(root, testutil.Quiet())
}

func stage(t *testing.T, s *Store, version string) string {
	t.Helper()
	os.MkdirAll(s.Root.Staging(), 0o755)
	dir, err := os.MkdirTemp(s.Root.Staging(), "ex-")
	if err != nil {
		t.Fatal(err)
	}
	writeVersion(t, dir, version)
	return dir
}

func current(t *testing.T, s *Store) string {
	t.Helper()
	v, err := s.Current()
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

func installTrial(t *testing.T, s *Store, version string) {
	t.Helper()
	if err := s.Install(stage(t, s, version), testutil.Ver(t, version), false); err != nil {
		t.Fatal(err)
	}
}

func startFailing(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		plan, err := s.BeginStart(testEntry)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.EndStart(plan.Version, Failed); err != nil {
			t.Fatal(err)
		}
	}
}
