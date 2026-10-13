package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfupdate/internal/testutil"
)

func programDir(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.WriteTree(t, dir, map[string]string{
		"agent":          "binary " + version,
		"data/greeting":  "hello " + version,
		"lib/engine.dat": "engine " + version,
	})
	return dir
}

func TestKeygenNeverOverwritesExistingKey(t *testing.T) {
	dir := t.TempDir()
	if err := keygen([]string{"-out", dir}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "private.key"))
	if err := keygen([]string{"-out", dir}); err == nil {
		t.Fatal("second keygen into the same dir must fail")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "private.key"))
	if string(before) != string(after) {
		t.Fatal("private key was modified")
	}
}

func TestKeygenKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := keygen([]string{"-out", dir}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "private.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("private key mode %o is group/world accessible", perm)
	}
}

func TestKeygenNeverLeavesALoneKeyBehind(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "public.key"), []byte("mine"), 0o644)
	if err := keygen([]string{"-out", dir}); err == nil {
		t.Fatal("keygen overwrote or ignored an existing public.key")
	}
	if _, err := os.Stat(filepath.Join(dir, "private.key")); !os.IsNotExist(err) {
		t.Error("a private.key was created even though the pair could not be completed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "public.key")); string(b) != "mine" {
		t.Error("existing public.key was modified")
	}
}

func TestMissingRequiredFlagIsReportedDeterministically(t *testing.T) {
	for i := 0; i < 50; i++ {
		err := publish(nil)
		if err == nil || err.Error() != "-key is required" {
			t.Fatalf("run %d: err = %v", i, err)
		}
	}
	if err := publish([]string{"-key", "k"}); err == nil || err.Error() != "-version is required" {
		t.Errorf("err = %v", err)
	}
	if err := publish([]string{"-key", "k", "-version", "1.0.0", "-os", "linux", "-arch", "amd64"}); err == nil || err.Error() != "-entry is required" {
		t.Errorf("err = %v", err)
	}
	if err := publish([]string{"-key", ""}); err == nil || err.Error() != "-key is required" {
		t.Errorf("empty value not treated as missing: %v", err)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	for _, f := range []func([]string) error{keygen, publish, refresh, packCmd, bootstrap, rollout} {
		if err := f([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("-h returned %v, want flag.ErrHelp", err)
		}
	}
}

func TestPublishThenRefreshEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := keygen([]string{"-out", dir}); err != nil {
		t.Fatal(err)
	}
	prog := programDir(t, "1.0.0")
	root, key := filepath.Join(dir, "releases"), filepath.Join(dir, "private.key")
	if err := publish([]string{"-key", key, "-root", root, "-version", "1.0.0", "-os", "linux", "-arch", "amd64", "-dir", prog, "-entry", "agent", "-valid-for", "24h"}); err != nil {
		t.Fatal(err)
	}
	if err := refresh([]string{"-key", key, "-root", root, "-os", "linux", "-arch", "amd64", "-valid-for", "48h"}); err != nil {
		t.Fatal(err)
	}
	if err := refresh([]string{"-key", key, "-root", root, "-arch", "amd64"}); err == nil || err.Error() != "-os is required" {
		t.Errorf("err = %v", err)
	}
}

func TestPublishStagedThenRolloutCommands(t *testing.T) {
	dir := t.TempDir()
	if err := keygen([]string{"-out", dir}); err != nil {
		t.Fatal(err)
	}
	prog := programDir(t, "1.0.0")
	root, key := filepath.Join(dir, "releases"), filepath.Join(dir, "private.key")
	target := []string{"-root", root, "-os", "linux", "-arch", "amd64"}
	rolloutFile := filepath.Join(root, "stable", "linux_amd64", "rollout.json")

	pub := append([]string{"-key", key, "-version", "1.0.0", "-dir", prog, "-entry", "agent", "-rollout", "5"}, target...)
	if err := publish(pub); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rolloutFile); !strings.Contains(string(b), `"percent": 5`) {
		t.Fatalf("publish -rollout 5 wrote %s", b)
	}

	if err := rollout(append([]string{"-percent", "40"}, target...)); err != nil {
		t.Fatal(err)
	}
	if err := rollout(append([]string{"-halt", "-note", "crash reports"}, target...)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(rolloutFile)
	if !strings.Contains(string(b), `"percent": 40`) || !strings.Contains(string(b), `"halt": true`) || !strings.Contains(string(b), "crash reports") {
		t.Errorf("rollout.json = %s", b)
	}
	if err := rollout(target); err == nil {
		t.Error("rollout with nothing to do succeeded")
	}
	if err := rollout([]string{"-halt", "-os", "linux"}); err == nil || err.Error() != "-arch is required" {
		t.Errorf("err = %v", err)
	}
}

func TestPackIsReproducible(t *testing.T) {
	prog := programDir(t, "1.0.0")
	out1, out2 := filepath.Join(t.TempDir(), "a.tar.gz"), filepath.Join(t.TempDir(), "b.tar.gz")
	for _, out := range []string{out1, out2} {
		if err := packCmd([]string{"-dir", prog, "-entry", "agent", "-out", out}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := os.ReadFile(out1)
	b, _ := os.ReadFile(out2)
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Error("two packs of the same directory differ")
	}
}

func TestPackRefusesAnUnusableDirectory(t *testing.T) {
	prog := programDir(t, "1.0.0")
	out := filepath.Join(t.TempDir(), "p.tar.gz")
	if err := packCmd([]string{"-dir", prog, "-entry", "missing", "-out", out}); err == nil {
		t.Error("packed a directory without its entry point")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a failed pack left a file behind")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(prog, "link")); err == nil {
		if err := packCmd([]string{"-dir", prog, "-entry", "agent", "-out", out}); err == nil {
			t.Error("packed a directory containing a symlink")
		}
	}
}

func TestPublishNeedsExactlyOneSource(t *testing.T) {
	dir := t.TempDir()
	keygen([]string{"-out", dir})
	base := []string{"-key", filepath.Join(dir, "private.key"), "-root", filepath.Join(dir, "r"),
		"-version", "1.0.0", "-os", "linux", "-arch", "amd64", "-entry", "agent"}
	if err := publish(base); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("neither: %v", err)
	}
	both := append([]string{"-dir", programDir(t, "1.0.0"), "-package", "x.tar.gz"}, base...)
	if err := publish(both); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("both: %v", err)
	}
}

func TestBootstrapCreatesARootAndRefusesToRepeat(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "p.tar.gz")
	if err := packCmd([]string{"-dir", programDir(t, "1.0.0"), "-entry", "agent", "-out", pkg}); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, "launcher-bin")
	os.WriteFile(launcher, []byte("L"), 0o755)
	root := filepath.Join(dir, "root")
	args := []string{"-root", root, "-launcher", launcher, "-package", pkg, "-version", "1.0.0", "-entry", "agent"}
	if err := bootstrap(args); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "current")); strings.TrimSpace(string(b)) != "1.0.0" {
		t.Errorf("current = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "versions", "1.0.0", "data", "greeting")); string(b) != "hello 1.0.0" {
		t.Errorf("data file = %q", b)
	}
	if err := bootstrap(args); err == nil {
		t.Error("bootstrapped over an existing root")
	}
}
