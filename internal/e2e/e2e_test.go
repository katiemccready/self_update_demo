package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	dir, _ := os.Getwd()
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			t.Fatal("go.mod not found")
		}
	}
}

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

func readLog(path string) string { b, _ := os.ReadFile(path); return string(b) }

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSelfUpdateEndToEnd(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	h.publish("1.0.0", v1Dir)
	root := h.bootstrap("1.0.0", v1Dir, "install")

	logPath, _ := h.startLauncher(root, "agent.log", h.agentArgs("25s", "1s")...)
	waitFor(t, "v1 to start", 15*time.Second, func() bool {
		return strings.Contains(readLog(logPath), "agent starting version=1.0.0")
	})
	if !strings.Contains(readLog(logPath), "program files loaded") {
		t.Error("v1 did not report loading its data files")
	}

	h.publish("1.1.0", h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0")))

	waitFor(t, "restart into 1.1.0", 40*time.Second, func() bool {
		return strings.Contains(readLog(logPath), "agent starting version=1.1.0")
	})
	log := readLog(logPath)
	if !strings.Contains(log, "greeting=\"hello 1.1.0\"") {
		t.Errorf("1.1.0 started without its own data files:\n%s", log)
	}
	if !strings.Contains(log, "state saved; restarting into new version") {
		t.Error("restart did not go through the save-then-restart path")
	}
	if got := currentVersion(root); got != "1.1.0" {
		t.Errorf("current = %s", got)
	}

	if _, err := os.Stat(filepath.Join(root.Versions(), "1.0.0", "lib", "engine.dat")); err != nil {
		t.Errorf("the previous version is not intact: %v", err)
	}

	waitFor(t, "the new version to be confirmed", 15*time.Second, func() bool {
		return strings.Contains(readLog(logPath), "version confirmed healthy")
	})
}

func TestEveryInstantDuringAnUpdateShowsOneCompleteVersion(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	root := h.bootstrap("1.0.0", v1Dir, "install")
	h.publish("1.1.0", h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0")))

	var mu sync.Mutex
	var problems []string
	samples := 0
	stop := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	stopSampler := func() { stopOnce.Do(func() { close(stop); <-done }) }
	defer stopSampler()
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
			v, err := root.ReadPointer()
			if err != nil {
				continue
			}
			dir := root.VersionDir(v)
			read := func(rel string) string {
				b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
				if err != nil {
					mu.Lock()
					problems = append(problems, "pointer names "+v.String()+" but "+rel+" is unreadable: "+err.Error())
					mu.Unlock()
				}
				return strings.TrimSpace(string(b))
			}
			g, e := read("data/greeting"), read("lib/engine.dat")
			mu.Lock()
			samples++
			if g != "hello "+v.String() || e != "engine "+v.String() {
				problems = append(problems, "pointer names "+v.String()+" but greeting="+g+" engine="+e)
			}
			mu.Unlock()
		}
	}()

	logPath, _ := h.startLauncher(root, "agent.log", h.agentArgs("8s", "1s")...)
	waitFor(t, "the update to be installed", 40*time.Second, func() bool { return currentVersion(root) == "1.1.0" })
	waitFor(t, "restart into 1.1.0", 40*time.Second, func() bool {
		return strings.Contains(readLog(logPath), "agent starting version=1.1.0")
	})
	stopSampler()
	mu.Lock()
	defer mu.Unlock()
	if samples < 100 {
		t.Fatalf("only %d samples; the check proves nothing", samples)
	}
	if len(problems) > 0 {
		t.Fatalf("%d of %d samples saw a mixed or incomplete version, e.g. %s", len(problems), samples, problems[0])
	}
	t.Logf("%d consistent samples across the update", samples)
}

func TestInterruptedInstallLeavesTheOldVersionRunning(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	root := h.bootstrap("1.0.0", v1Dir, "install")

	partial := filepath.Join(root.Versions(), "1.1.0")
	if err := os.MkdirAll(filepath.Join(partial, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "data", "greeting"), []byte("hello 1.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root.Staging(), "unpack-crashed"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := runToExit(t, root, "-step", "50ms", "-run-for", "1s", "-state", filepath.Join(h.tmp, "s.json"))
	if code != 0 || !strings.Contains(out, "agent starting version=1.0.0") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := currentVersion(root); got != "1.0.0" {
		t.Errorf("current = %s", got)
	}
}
