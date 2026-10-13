//go:build !windows

package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"selfupdate/internal/update/manifest"
)

func processGone(pid int) bool {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] == 'Z'
		}
	}
	return syscall.Kill(pid, 0) != nil
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild %d survived", pid)
}

func candidate(t *testing.T, body string) (path, pidfile string) {
	t.Helper()
	dir := t.TempDir()
	path, pidfile = filepath.Join(dir, "cand"), filepath.Join(dir, "pid")
	script := "#!/bin/sh\nPIDFILE=" + pidfile + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func readPID(t *testing.T, pidfile string) int {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("candidate never recorded a child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("bad pid %q", b)
	}
	return pid
}

func TestVersionCheckTimeoutKillsGrandchildren(t *testing.T) {
	path, pidfile := candidate(t, `sleep 60 &
echo $! > "$PIDFILE"
wait`)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := runVersionCheck(ctx, filepath.Dir(path), path, manifest.Manifest{Version: "1.1.0"}); err == nil {
		t.Fatal("a hung candidate passed the smoke test")
	}
	waitGone(t, readPID(t, pidfile))
}

func TestVersionCheckSweepsDetachedChildAfterNormalExit(t *testing.T) {
	path, pidfile := candidate(t, `sleep 60 >/dev/null 2>&1 &
echo $! > "$PIDFILE"
echo 1.1.0`)
	if err := runVersionCheck(context.Background(), filepath.Dir(path), path, manifest.Manifest{Version: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	waitGone(t, readPID(t, pidfile))
}
