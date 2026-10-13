//go:build !windows

package launcher

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"selfupdate/internal/pack"
	"selfupdate/internal/testutil"
	"selfupdate/internal/update/install"
)

const entry = "app"

func scriptDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, entry), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newRoot(t *testing.T, body string) (install.Root, *install.Store) {
	t.Helper()
	root := install.Root{Dir: t.TempDir()}
	var pkg bytes.Buffer
	if err := pack.Build(scriptDir(t, body), entry, &pkg); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "launcher")
	os.WriteFile(src, []byte("x"), 0o755)
	if err := install.Bootstrap(root, src, &pkg, testutil.Ver(t, "1.0.0"), entry); err != nil {
		t.Fatal(err)
	}
	return root, install.Open(root, testutil.Quiet())
}

func addTrial(t *testing.T, s *install.Store, v, body string) {
	t.Helper()
	os.MkdirAll(s.Root.Staging(), 0o755)
	staged, err := os.MkdirTemp(s.Root.Staging(), "unpack-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, entry), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(staged, testutil.Ver(t, v), false); err != nil {
		t.Fatal(err)
	}
}

type out struct{ bytes.Buffer }

func run(t *testing.T, root install.Root, args ...string) (int, string) {
	t.Helper()
	var stdout out
	code := Run(Config{Root: root, Args: args, Log: testutil.Quiet(), Stdout: &stdout, Stderr: io.Discard, Stdin: strings.NewReader("")})
	return code, stdout.String()
}

func TestRunStartsTheCurrentVersionWithArgsAndRoot(t *testing.T) {
	root, _ := newRoot(t, `echo "args=$* root=$SELFUPDATE_ROOT"; exit 0`)
	code, got := run(t, root, "a", "b c")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if want := "args=a b c root=" + root.Dir; !strings.Contains(got, want) {
		t.Errorf("output %q does not contain %q", got, want)
	}
}

func TestRunPassesTheProgramsExitStatusThrough(t *testing.T) {
	root, _ := newRoot(t, `exit 7`)
	if code, _ := run(t, root); code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
}

func TestInheritedRootIsReplaced(t *testing.T) {
	t.Setenv(install.RootEnv, "/somewhere/else")
	root, _ := newRoot(t, `echo $SELFUPDATE_ROOT`)
	_, got := run(t, root)
	if strings.Contains(got, "somewhere") || !strings.Contains(got, root.Dir) {
		t.Errorf("output = %q", got)
	}
}

func TestRunRefusesADirectoryThatIsNotARoot(t *testing.T) {
	if code, _ := run(t, install.Root{Dir: t.TempDir()}); code == 0 {
		t.Error("ran in an empty directory")
	}
}

func TestRestartCodeStartsTheNewVersion(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `echo "new version running"; exit 0`)

	if err := root.WritePointer(testutil.Ver(t, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	script := `echo "old version"; printf '1.1.0\n' > "$SELFUPDATE_ROOT/current"; exit 75`
	if err := os.WriteFile(filepath.Join(root.VersionDir(testutil.Ver(t, "1.0.0")), entry), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, got := run(t, root)
	if code != 0 || !strings.Contains(got, "old version") || !strings.Contains(got, "new version running") {
		t.Errorf("code=%d output=%q", code, got)
	}
}

func TestCrashOfAConfirmedVersionIsReportedNotRetried(t *testing.T) {
	root, _ := newRoot(t, `echo run; exit 3`)
	code, got := run(t, root)
	if code != 3 || strings.Count(got, "run") != 1 {
		t.Errorf("code=%d output=%q; want one run and its status", code, got)
	}
}

func TestTrialThatKeepsCrashingRollsBackWithinOneRun(t *testing.T) {
	root, s := newRoot(t, `echo "good 1.0.0"; exit 0`)
	addTrial(t, s, "1.1.0", `echo "bad 1.1.0"; exit 1`)
	for i := 1; i < install.MaxStartAttempts; i++ {
		if code, _ := run(t, root); code != 1 {
			t.Fatalf("start %d: code = %d, want 1", i, code)
		}
	}
	code, got := run(t, root)
	if code != 0 || !strings.Contains(got, "bad 1.1.0") || !strings.Contains(got, "good 1.0.0") {
		t.Fatalf("code=%d output=%q", code, got)
	}
	cur, _ := s.Current()
	if cur.String() != "1.0.0" || !s.Load().IsBad("1.1.0") {
		t.Errorf("current=%s state=%+v", cur, s.Load())
	}
}

func TestProgramThatCannotBeStartedCountsAsAFailedStart(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `exit 0`)
	v := testutil.Ver(t, "1.1.0")
	os.Chmod(filepath.Join(root.VersionDir(v), entry), 0o644)
	if code, _ := run(t, root); code == 0 {
		t.Error("an unstartable program reported success")
	}
	if st := s.Load(); st.Attempts != 1 {
		t.Errorf("attempts = %d, want the failed start counted", st.Attempts)
	}
}

func TestCleanExitsOfATrialAreNotHeldAgainstIt(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `exit 0`)
	for i := 0; i < install.MaxStartAttempts+2; i++ {
		if code, _ := run(t, root); code != 0 {
			t.Fatalf("run %d: code %d", i, code)
		}
	}
	cur, _ := s.Current()
	if cur.String() != "1.1.0" || s.Load().IsBad("1.1.0") {
		t.Errorf("current=%s state=%+v", cur, s.Load())
	}
}

func TestRestartRequestThatChangesNothingIsNotLoopedForever(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `echo again; exit 75`)
	code, got := run(t, root)
	if code == 0 || strings.Count(got, "again") > install.MaxStartAttempts {
		t.Errorf("code=%d, %d runs", code, strings.Count(got, "again"))
	}
	if st := s.Load(); st.Attempts == 0 && !st.IsBad("1.1.0") {
		t.Errorf("a pointless restart request was held against nobody: %+v", st)
	}
}

func TestStopSignalIsForwardedAndNotHeldAgainstTheVersion(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `trap 'echo got-term; exit 0' TERM
echo ready
while :; do sleep 0.05; done`)
	signals := make(chan os.Signal, 1)
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Run(Config{Root: root, Log: testutil.Quiet(), Stdout: pw, Stderr: io.Discard, Stdin: strings.NewReader(""), Signals: signals})
		pw.Close()
	}()
	buf := make([]byte, 64)
	n, _ := pr.Read(buf)
	if !strings.Contains(string(buf[:n]), "ready") {
		t.Fatalf("read %q", buf[:n])
	}
	signals <- syscall.SIGTERM
	rest, _ := io.ReadAll(pr)
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(string(rest), "got-term") {
			t.Errorf("code=%d output=%q", code, rest)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not stop")
	}
	if st := s.Load(); st.Attempts != 0 {
		t.Errorf("a requested stop was counted as a failed start: %+v", st)
	}
}

func TestEnvironmentFailuresAreNotHeldAgainstTheVersion(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `exit 78`)
	for i := 0; i < install.MaxStartAttempts+2; i++ {
		if code, _ := run(t, root); code != install.EnvironmentExitCode {
			t.Fatalf("run %d: code = %d", i, code)
		}
	}
	cur, _ := s.Current()
	st := s.Load()
	if cur.String() != "1.1.0" || st.IsBad("1.1.0") || st.Attempts != 0 {
		t.Errorf("current=%s state=%+v", cur, st)
	}
}

func TestInterruptThatNeverReachedTheProgramIsEscalatedToTerm(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `trap 'echo got-term; exit 0' TERM
echo ready
while :; do sleep 0.05; done`)
	signals := make(chan os.Signal, 1)
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Run(Config{Root: root, Log: testutil.Quiet(), Stdout: pw, Stderr: io.Discard, Stdin: strings.NewReader(""),
			Signals: signals, InterruptDelay: 200 * time.Millisecond})
		pw.Close()
	}()
	buf := make([]byte, 64)
	pr.Read(buf)
	signals <- syscall.SIGINT
	rest, _ := io.ReadAll(pr)
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(string(rest), "got-term") {
			t.Errorf("code=%d output=%q", code, rest)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not stop after SIGINT")
	}
}

func TestProgramThatIgnoresTheStopSignalIsKilled(t *testing.T) {
	root, s := newRoot(t, `exit 0`)
	addTrial(t, s, "1.1.0", `trap '' TERM
echo ready
while :; do sleep 0.05; done`)
	signals := make(chan os.Signal, 1)
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Run(Config{Root: root, Log: testutil.Quiet(), Stdout: pw, Stderr: io.Discard, Stdin: strings.NewReader(""),
			Signals: signals, StopGrace: 300 * time.Millisecond})
		pw.Close()
	}()
	buf := make([]byte, 64)
	pr.Read(buf)
	signals <- syscall.SIGTERM
	go io.Copy(io.Discard, pr)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a program that ignores SIGTERM kept the launcher alive")
	}
	if st := s.Load(); st.Attempts != 0 {
		t.Errorf("a requested stop was counted as a failed start: %+v", st)
	}
}
