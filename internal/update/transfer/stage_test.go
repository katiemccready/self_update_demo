package transfer

import (
	"os"
	"path/filepath"
	"testing"

	"selfupdate/internal/testutil"
)

func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		in           string
		start, total int64
		ok           bool
	}{
		{"bytes 4-9/10", 4, 10, true},
		{"bytes 4-9/*", 4, -1, true},
		{"bytes */10", 0, 0, false},
		{"", 0, 0, false},
		{"items 4-9/10", 0, 0, false},
	} {
		start, _, total, ok := parseContentRange(tc.in)
		if ok != tc.ok || (ok && (start != tc.start || total != tc.total)) {
			t.Errorf("%q -> start=%d total=%d ok=%v", tc.in, start, total, ok)
		}
	}
}

func TestOpenStagingFileRefusesSymlink(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("O_NOFOLLOW is Unix-only")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".agent.partial-x")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openStagingFile(link); err == nil {
		f.Close()
		t.Fatal("opened through a symlink")
	}
	if testutil.ReadFile(t, target) != "precious" {
		t.Fatal("victim modified")
	}
	plain := filepath.Join(dir, ".agent.partial-y")
	f, err := openStagingFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestCheckStagingFileAcceptsAbsentAndOwnRegularFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	if err := CheckStagingFile(p); err != nil {
		t.Errorf("absent: %v", err)
	}
	os.WriteFile(p, []byte("mine"), 0o600)
	if err := CheckStagingFile(p); err != nil {
		t.Errorf("own regular file: %v", err)
	}
	if got := testutil.ReadFile(t, p); got != "mine" {
		t.Errorf("own file was disturbed: %q", got)
	}
}
