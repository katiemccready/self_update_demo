package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfupdate/internal/update/install"

	"selfupdate/internal/update/manifest"
)

const badReleaseExitCode = 3

func TestCrashLoopingReleaseIsRolledBackAutomatically(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	root := h.bootstrap("1.0.0", v1Dir, "install")
	h.publish("1.1.0", h.programDir("1.1.0", h.build("./internal/e2e/testdata/badrelease", "1.1.0")))

	out, code := runToExit(t, root, h.agentArgs("20s", "1h")...)
	if code != badReleaseExitCode {
		t.Fatalf("first run: want the bad release to die with exit %d, got %d\n%s", badReleaseExitCode, code, out)
	}
	if got := currentVersion(root); got != "1.1.0" {
		t.Fatalf("after the first run the bad release should be current, got %q", got)
	}

	crashes, finalOut, rolledBack := 1, "", false
	for start := 2; start <= 8; start++ {
		out, code = runToExit(t, root, h.agentArgs("4s", "1h")...)
		if code == badReleaseExitCode && !strings.Contains(out, "agent starting") {
			crashes++
			continue
		}
		finalOut, rolledBack = out, true
		if code != 0 {
			t.Fatalf("start %d ended with exit %d after the rollback\n%s", start, code, out)
		}
		break
	}
	if !rolledBack {
		t.Fatalf("still crashing after %d starts: never rolled back", crashes)
	}
	t.Logf("rolled back after %d failing starts", crashes)

	if got := currentVersion(root); got != "1.0.0" {
		t.Errorf("current after rollback = %q, want 1.0.0", got)
	}
	if !strings.Contains(finalOut, "agent starting version=1.0.0") {
		t.Errorf("the previous version did not start after the rollback:\n%s", finalOut)
	}
	if strings.Contains(finalOut, "update installed") {
		t.Errorf("the rolled-back release was installed again:\n%s", finalOut)
	}
	st := install.Open(root, nil).Load()
	if !st.IsBad("1.1.0") || st.Pending != "" {
		t.Errorf("state should blacklist 1.1.0 with no trial pending: %+v", st)
	}

	time.Sleep(time.Second)
	if got := currentVersion(root); got != "1.0.0" {
		t.Errorf("current = %q a moment later", got)
	}
}

func TestTamperedDownloadIsRejected(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	root := h.bootstrap("1.0.0", v1Dir, "install")
	flipOneByte(t, h.publish("1.1.0", h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0"))))

	logPath, _ := h.startLauncher(root, "agent.log", h.agentArgs("30s", "1h")...)
	waitFor(t, "the tampered update to be refused", 40*time.Second, func() bool {
		return strings.Contains(readLog(logPath), "update failed")
	})
	log := readLog(logPath)
	if !strings.Contains(log, "failed verification") {
		t.Errorf("update was refused, but not by the integrity check:\n%s", log)
	}
	if strings.Contains(log, "update installed") || strings.Contains(log, "agent starting version=1.1.0") {
		t.Errorf("the tampered package was installed or run:\n%s", log)
	}
	if got := currentVersion(root); got != "1.0.0" {
		t.Errorf("current = %q, want it untouched at 1.0.0", got)
	}
	assertNothingStaged(t, root)

	if st := install.Open(root, nil).Load(); len(st.Rejected) == 0 {
		t.Errorf("tampered release should be recorded as rejected: %+v", st)
	}
}

func TestHostileSignedPackageIsRefused(t *testing.T) {
	cases := map[string][]hostile{
		"path traversal": {{name: "../../escaped", body: "pwned"}},
		"symlink":        {{name: "link", link: "/etc"}},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
			root := h.bootstrap("1.0.0", v1Dir, "install")
			good := h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0"))
			h.publishRaw("1.1.0", withHostileEntries(t, good, extra))

			logPath, _ := h.startLauncher(root, "agent.log", h.agentArgs("30s", "1h")...)
			waitFor(t, "the hostile package to be refused", 40*time.Second, func() bool {
				return strings.Contains(readLog(logPath), "unsafe archive")
			})
			log := readLog(logPath)
			if strings.Contains(log, "update installed") {
				t.Errorf("a hostile package was installed:\n%s", log)
			}
			if got := currentVersion(root); got != "1.0.0" {
				t.Errorf("current = %q", got)
			}
			assertNothingStaged(t, root)
			for _, p := range []string{
				filepath.Join(root.Dir, "escaped"), filepath.Join(filepath.Dir(root.Dir), "escaped"),
				filepath.Join(h.tmp, "escaped"),
			} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s was created by the package", p)
				}
			}
		})
	}
}

func TestRunFailureThatIsTheMachinesFaultDoesNotRollBack(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	root := h.bootstrap("1.0.0", v1Dir, "install")
	v2Dir := h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0"))

	installTrialDir(t, root, "1.1.0", v2Dir)

	badState := filepath.Join(h.tmp, "state-is-a-dir")
	if err := os.MkdirAll(badState, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < install.MaxStartAttempts+2; i++ {
		out, code := runToExit(t, root, "-step", "50ms", "-state", badState, "-run-for", "2s")
		if code != 78 {
			t.Fatalf("run %d: exit %d, want 78 (environment)\n%s", i, code, out)
		}
	}
	if got := currentVersion(root); got != "1.1.0" {
		t.Errorf("current = %q: an environment failure rolled the release back", got)
	}
	if st := install.Open(root, nil).Load(); st.IsBad("1.1.0") {
		t.Errorf("1.1.0 was blacklisted: %+v", st)
	}
}

func TestReleaseWithMismatchedFilesRollsBack(t *testing.T) {
	h := newHarness(t)
	v1Dir := h.programDir("1.0.0", h.build("./cmd/agent", "1.0.0"))
	root := h.bootstrap("1.0.0", v1Dir, "install")

	broken := h.programDir("1.1.0", h.build("./cmd/agent", "1.1.0"))
	if err := os.WriteFile(filepath.Join(broken, "lib", "engine.dat"), []byte("engine 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installTrialDir(t, root, "1.1.0", broken)

	var finalOut string
	for start := 1; start <= 6; start++ {
		out, code := runToExit(t, root, "-step", "50ms", "-state", filepath.Join(h.tmp, "s.json"), "-run-for", "1s")
		if strings.Contains(out, "agent starting version=1.0.0") {
			finalOut = out
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			break
		}
		if !strings.Contains(out, "do not belong") {
			t.Fatalf("start %d: unexpected output (exit %d):\n%s", start, code, out)
		}
	}
	if finalOut == "" {
		t.Fatal("never rolled back")
	}
	if got := currentVersion(root); got != "1.0.0" {
		t.Errorf("current = %q", got)
	}
}

func installTrialDir(t *testing.T, root install.Root, v string, dir string) {
	t.Helper()
	s := install.Open(root, nil)
	staged, err := os.MkdirTemp(root.Staging(), "unpack-")
	if err != nil {
		t.Fatal(err)
	}
	if err := copyTree(dir, staged); err != nil {
		t.Fatal(err)
	}
	ver, _ := manifest.ParseVersion(v)
	if err := s.Install(staged, ver, false); err != nil {
		t.Fatal(err)
	}
}

func assertNothingStaged(t *testing.T, root install.Root) {
	t.Helper()
	entries, _ := os.ReadDir(root.Staging())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".partial-") {
			continue
		}
		if strings.HasPrefix(e.Name(), "unpack-") || strings.Contains(e.Name(), ".new-") {
			t.Errorf("leftover staging entry %q", e.Name())
		}
	}
	versions, _ := os.ReadDir(root.Versions())
	if len(versions) != 1 {
		t.Errorf("versions/ holds %d entries, want only the installed one", len(versions))
	}
}

type hostile struct {
	name, body, link string
}

func withHostileEntries(t *testing.T, dir string, extra []hostile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := walkFiles(dir, func(rel string, mode os.FileMode, content []byte) error {
		h := &tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: int64(mode.Perm()), Size: int64(len(content))}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range extra {
		h := &tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.body))}
		if e.link != "" {
			h = &tar.Header{Name: e.name, Typeflag: tar.TypeSymlink, Linkname: e.link, Mode: 0o777}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.link == "" {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func flipOneByte(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pos, b := fi.Size()/2, make([]byte, 1)
	if _, err := f.ReadAt(b, pos); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, pos); err != nil {
		t.Fatal(err)
	}
}
