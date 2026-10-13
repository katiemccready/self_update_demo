//go:build !windows

package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfupdate/internal/update/manifest"
)

func scriptVersion(t *testing.T, out string) (dir, program string) {
	t.Helper()
	dir = t.TempDir()
	program = filepath.Join(dir, entryName)
	script := "#!/bin/sh\n" + out + "\n"
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, program
}

func TestDefaultSmokeTestAcceptsTheAnnouncedVersion(t *testing.T) {
	dir, program := scriptVersion(t, `echo "v1.1.0"`)
	if err := runVersionCheck(context.Background(), dir, program, manifest.Manifest{Version: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultSmokeTestRejectsAnotherVersion(t *testing.T) {
	dir, program := scriptVersion(t, `echo 1.0.0`)
	err := runVersionCheck(context.Background(), dir, program, manifest.Manifest{Version: "1.1.0"})
	if err == nil || !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultSmokeTestRejectsABrokenProgram(t *testing.T) {
	for name, body := range map[string]string{"exits 1": "exit 1", "prints junk": "echo not-a-version"} {
		dir, program := scriptVersion(t, body)
		if err := runVersionCheck(context.Background(), dir, program, manifest.Manifest{Version: "1.1.0"}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDefaultSmokeTestRunsInTheVersionDirectoryWithAMinimalEnvironment(t *testing.T) {
	t.Setenv("SELFUPDATE_TEST_SECRET", "hunter2")
	dir, program := scriptVersion(t, `[ -z "$SELFUPDATE_TEST_SECRET" ] && [ -f ./`+entryName+` ] && echo 1.1.0`)
	if err := runVersionCheck(context.Background(), dir, program, manifest.Manifest{Version: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyWithTheRealSmokeTest(t *testing.T) {
	f, u := newFixture(t, "1.0.0", "1.1.0")
	u.cfg.Validate = runVersionCheck
	files := versionFiles("1.1.0")
	files[entryName] = "#!/bin/sh\necho 1.1.0\n"
	f.release("1.1.0", buildPackage(t, files))
	if err := u.Apply(context.Background(), f.check(u)); err != nil {
		t.Fatal(err)
	}
	if f.current() != "1.1.0" {
		t.Errorf("current = %s", f.current())
	}

	f2, u2 := newFixture(t, "1.0.0", "1.2.0")
	u2.cfg.Validate = runVersionCheck
	files = versionFiles("1.2.0")
	files[entryName] = "#!/bin/sh\necho 1.1.9\n"
	f2.release("1.2.0", buildPackage(t, files))
	if err := u2.Apply(context.Background(), f2.check(u2)); err == nil {
		t.Fatal("a program reporting the wrong version was installed")
	}
	if f2.current() != "1.0.0" {
		t.Errorf("current = %s", f2.current())
	}
}
