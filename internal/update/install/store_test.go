package install

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"selfupdate/internal/testutil"
	"selfupdate/internal/update/manifest"
)

func TestBootstrapLaysOutARunnableRoot(t *testing.T) {
	s := newStore(t)
	if got := current(t, s); got != "1.0.0" {
		t.Fatalf("current = %s", got)
	}
	if got := testutil.ReadFile(t, s.Root.entryPath(testutil.Ver(t, "1.0.0"), testEntry)); got != "program 1.0.0" {
		t.Errorf("entry = %q", got)
	}
	if entry, err := s.Root.ReadEntry(); err != nil || entry != testEntry {
		t.Errorf("entry = %q, %v", entry, err)
	}
	if _, err := os.Stat(s.Root.Launcher()); err != nil {
		t.Errorf("no launcher: %v", err)
	}
	if st := s.Load(); st.LastGood != "1.0.0" || st.Pending != "" {
		t.Errorf("state = %+v", st)
	}
	if plan, err := s.BeginStart(testEntry); err != nil || plan.Version.String() != "1.0.0" {
		t.Errorf("BeginStart = %+v, %v", plan, err)
	}
}

func TestBootstrapRefusesAnExistingRoot(t *testing.T) {
	s := newStore(t)
	if err := Bootstrap(s.Root, "irrelevant", strings.NewReader(""), testutil.Ver(t, "2.0.0"), testEntry); err == nil {
		t.Fatal("bootstrapped over an existing root")
	}
}

func TestInstallSwitchesEveryFileAtOnce(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")

	if got := current(t, s); got != "1.1.0" {
		t.Fatalf("current = %s", got)
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		dir := s.Root.VersionDir(testutil.Ver(t, v))
		if testutil.ReadFile(t, filepath.Join(dir, testEntry)) != "program "+v || testutil.ReadFile(t, filepath.Join(dir, "data", "file.txt")) != "data "+v {
			t.Errorf("version %s is not intact", v)
		}
	}
	st := s.Load()
	if st.Pending != "1.1.0" || st.Previous != "1.0.0" || st.Attempts != 0 {
		t.Errorf("state = %+v", st)
	}
	if err := s.RequireNoTrial(); err == nil {
		t.Error("a second install would be allowed during a trial")
	}
}

func TestInstallRefusesTheCurrentVersion(t *testing.T) {
	s := newStore(t)
	if err := s.Install(stage(t, s, "1.0.0"), testutil.Ver(t, "1.0.0"), false); err == nil {
		t.Fatal("installed over the running version")
	}
	if got := testutil.ReadFile(t, s.Root.entryPath(testutil.Ver(t, "1.0.0"), testEntry)); got != "program 1.0.0" {
		t.Errorf("running version modified: %q", got)
	}
}

func TestInstallReplacesDebrisFromAnInterruptedInstall(t *testing.T) {
	s := newStore(t)
	debris := s.Root.VersionDir(testutil.Ver(t, "1.1.0"))
	os.MkdirAll(debris, 0o755)
	os.WriteFile(filepath.Join(debris, "leftover"), []byte("x"), 0o644)
	installTrial(t, s, "1.1.0")
	if _, err := os.Stat(filepath.Join(debris, "leftover")); err == nil {
		t.Error("debris survived into the installed version")
	}
}

func TestFailedSwitchRestoresStateAndRemovesTheDirectory(t *testing.T) {
	s := newStore(t)
	st := s.Load()
	st.Rejected = map[string]Rejection{"1.1.0": {Count: 2, Until: time.Now().Add(-time.Hour)}}
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}

	os.Remove(s.Root.pointer())
	os.MkdirAll(filepath.Join(s.Root.pointer(), "x"), 0o755)

	if err := s.Install(stage(t, s, "1.1.0"), testutil.Ver(t, "1.1.0"), false); err == nil {
		t.Fatal("install succeeded without a readable pointer")
	}
	if _, err := os.Stat(s.Root.VersionDir(testutil.Ver(t, "1.1.0"))); err == nil {
		t.Error("a version directory was left behind")
	}
	if r := s.Load().Rejected["1.1.0"]; r.Count != 2 {
		t.Errorf("backoff entry altered: %+v", r)
	}
}

func TestPointerWriteFailureAfterTheRenameUndoesEverything(t *testing.T) {
	s := newStore(t)

	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores directory permission bits, so the root cannot be made read-only this way")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	staged := stage(t, s, "1.1.0")

	os.Chmod(s.Root.Dir, 0o555)
	defer os.Chmod(s.Root.Dir, 0o755)
	err := s.Install(staged, testutil.Ver(t, "1.1.0"), false)
	os.Chmod(s.Root.Dir, 0o755)
	if err == nil {
		t.Fatal("install succeeded in a read-only root")
	}
	if got := current(t, s); got != "1.0.0" {
		t.Errorf("current = %s after a failed install", got)
	}
	if st := s.Load(); st.Pending != "" {
		t.Errorf("trial left recorded: %+v", st)
	}
}

func TestStaleTrialFromACrashedInstallIsDropped(t *testing.T) {
	s := newStore(t)
	st := s.Load()
	st.Pending, st.Previous, st.Attempts = "1.1.0", "1.0.0", 2
	s.save(st)
	plan, err := s.BeginStart(testEntry)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version.String() != "1.0.0" || plan.RolledBack {
		t.Fatalf("plan = %+v", plan)
	}
	if st := s.Load(); st.Pending != "" || st.Attempts != 0 {
		t.Errorf("stale trial kept: %+v", st)
	}
}

func TestUnconfirmedStartsAreCountedThenRolledBack(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")

	for i := 1; i <= MaxStartAttempts; i++ {
		plan, err := s.BeginStart(testEntry)
		if err != nil || plan.Version.String() != "1.1.0" || plan.RolledBack {
			t.Fatalf("start %d: %+v, %v", i, plan, err)
		}
		rollbackNext, err := s.EndStart(plan.Version, Failed)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == MaxStartAttempts; rollbackNext != want {
			t.Fatalf("after failure %d rollbackNext = %v", i, rollbackNext)
		}
	}
	plan, err := s.BeginStart(testEntry)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.RolledBack || plan.Version.String() != "1.0.0" || testutil.ReadFile(t, plan.Entry) != "program 1.0.0" {
		t.Fatalf("plan = %+v", plan)
	}
	st := s.Load()
	if !st.IsBad("1.1.0") || st.Pending != "" || st.Attempts != 0 {
		t.Errorf("state after rollback = %+v", st)
	}
	if got := current(t, s); got != "1.0.0" {
		t.Errorf("pointer = %s", got)
	}
}

func TestCleanExitsGiveTheStartBack(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	for i := 0; i < 3*MaxStartAttempts; i++ {
		plan, err := s.BeginStart(testEntry)
		if err != nil || plan.RolledBack {
			t.Fatalf("start %d: %+v, %v", i, plan, err)
		}
		if _, err := s.EndStart(plan.Version, Clean); err != nil {
			t.Fatal(err)
		}
	}
	if got := current(t, s); got != "1.1.0" {
		t.Errorf("rolled back a healthy release: %s", got)
	}
}

func TestConfirmEndsTheTrialAndCollectsOldVersions(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	if err := s.Confirm(testutil.Ver(t, "1.1.0")); err != nil {
		t.Fatal(err)
	}
	st := s.Load()
	if st.Pending != "" || st.LastGood != "1.1.0" || st.Previous != "1.0.0" {
		t.Errorf("state = %+v", st)
	}
	if err := s.RequireNoTrial(); err != nil {
		t.Error(err)
	}

	installTrial(t, s, "1.2.0")
	if err := s.Confirm(testutil.Ver(t, "1.2.0")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root.VersionDir(testutil.Ver(t, "1.0.0"))); err == nil {
		t.Error("a version two generations old was kept")
	}
	for _, v := range []string{"1.1.0", "1.2.0"} {
		if _, err := os.Stat(s.Root.VersionDir(testutil.Ver(t, v))); err != nil {
			t.Errorf("version %s should have been kept: %v", v, err)
		}
	}

	os.MkdirAll(filepath.Join(s.Root.Versions(), "keep-me"), 0o755)
	s.collectGarbage()
	if _, err := os.Stat(filepath.Join(s.Root.Versions(), "keep-me")); err != nil {
		t.Error("deleted a directory that is not a version")
	}
}

func TestConfirmOfAnotherVersionIsIgnored(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	if err := s.Confirm(testutil.Ver(t, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if st := s.Load(); st.Pending != "1.1.0" {
		t.Errorf("an old version's confirm ended the new version's trial: %+v", st)
	}
}

func TestReportBadRollsBackAtOnce(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	rolled, err := s.ReportBad(testutil.Ver(t, "1.1.0"), testEntry, os.ErrInvalid)
	if err != nil || !rolled {
		t.Fatalf("rolled=%v err=%v", rolled, err)
	}
	if got := current(t, s); got != "1.0.0" || !s.Load().IsBad("1.1.0") {
		t.Errorf("current=%s state=%+v", got, s.Load())
	}
	if rolled, err := s.ReportBad(testutil.Ver(t, "1.0.0"), testEntry, os.ErrInvalid); rolled || err != nil {
		t.Errorf("rolled=%v err=%v", rolled, err)
	}
}

func TestRollbackWithoutAUsablePreviousVersionEndsTheTrial(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	os.RemoveAll(s.Root.VersionDir(testutil.Ver(t, "1.0.0")))
	startFailing(t, s, MaxStartAttempts)
	plan, err := s.BeginStart(testEntry)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RolledBack || plan.Version.String() != "1.1.0" {
		t.Fatalf("plan = %+v", plan)
	}
	st := s.Load()
	if st.Pending != "" || !st.IsBad("1.1.0") {
		t.Errorf("state = %+v", st)
	}
}

func TestDamagedPointerOrVersionFallsBackToTheLastGoodVersion(t *testing.T) {
	cases := map[string]func(s *Store, broken manifest.Version){
		"pointer deleted":  func(s *Store, _ manifest.Version) { os.Remove(s.Root.pointer()) },
		"pointer garbage":  func(s *Store, _ manifest.Version) { os.WriteFile(s.Root.pointer(), []byte("not a version"), 0o644) },
		"program deleted":  func(s *Store, v manifest.Version) { os.Remove(s.Root.entryPath(v, testEntry)) },
		"version dir gone": func(s *Store, v manifest.Version) { os.RemoveAll(s.Root.VersionDir(v)) },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			installTrial(t, s, "1.1.0")
			if err := s.Confirm(testutil.Ver(t, "1.1.0")); err != nil {
				t.Fatal(err)
			}
			installTrial(t, s, "1.2.0")
			damage(s, testutil.Ver(t, "1.2.0"))
			plan, err := s.BeginStart(testEntry)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Version.String() != "1.1.0" {
				t.Fatalf("ran %s, want the last good 1.1.0", plan.Version)
			}
			if got := current(t, s); got != "1.1.0" {
				t.Errorf("pointer not repaired: %s", got)
			}
		})
	}
}

func TestNothingUsableIsAnErrorNotAGuess(t *testing.T) {
	s := newStore(t)
	os.RemoveAll(s.Root.Versions())
	if _, err := s.BeginStart(testEntry); err == nil {
		t.Fatal("started without any version")
	}
}

func TestConcurrentStartsAreCountedExactly(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	other := Open(s.Root, testutil.Quiet())
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			if _, err := st.BeginStart(testEntry); err != nil {
				t.Error(err)
			}
		}([]*Store{s, other}[i])
	}
	wg.Wait()
	if got := s.Load().Attempts; got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestScreenEnforcesReplayFloorAndSkipLists(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	nv := testutil.Ver(t, "1.1.0")
	if skip, err := s.Screen(now, time.Time{}, nv, now); skip || err != nil {
		t.Fatalf("fresh manifest: skip=%v err=%v", skip, err)
	}
	if _, err := s.Screen(now.Add(-time.Hour), time.Time{}, nv, now); err == nil {
		t.Error("an older manifest than one already seen was accepted")
	}
	if _, err := s.Screen(now.Add(time.Hour), now.Add(2*time.Hour), nv, now); err == nil {
		t.Error("a manifest older than the build time was accepted")
	}
	s.Reject(nv, now)
	if skip, _ := s.Screen(now, time.Time{}, nv, now); !skip {
		t.Error("a rejected version was not backed off")
	}
	if skip, _ := s.Screen(now, time.Time{}, nv, now.Add(rejectBackoff(1)+time.Minute)); skip {
		t.Error("backoff never ends")
	}
	st := s.Load()
	st.Bad = []string{"1.1.0"}
	s.save(st)
	if skip, _ := s.Screen(now, time.Time{}, nv, now.Add(100*24*time.Hour)); !skip {
		t.Error("a blacklisted version was offered again")
	}
}

func TestClientIDIsCreatedOnceAndSurvivesTheTrialLifecycle(t *testing.T) {
	s := newStore(t)
	id := s.ClientID()
	if len(id) != 32 || s.ClientID() != id {
		t.Fatalf("id = %q", id)
	}
	installTrial(t, s, "1.1.0")
	startFailing(t, s, MaxStartAttempts)
	s.BeginStart(testEntry)
	if got := s.Load().ClientID; got != id {
		t.Errorf("ID changed to %q across a rollback", got)
	}
}

func TestRollbacksAccumulateInTheSkipList(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	if _, err := s.ReportBad(testutil.Ver(t, "1.1.0"), testEntry, os.ErrInvalid); err != nil {
		t.Fatal(err)
	}
	installTrial(t, s, "1.2.0")
	if _, err := s.ReportBad(testutil.Ver(t, "1.2.0"), testEntry, os.ErrInvalid); err != nil {
		t.Fatal(err)
	}
	installTrial(t, s, "1.2.0")
	if _, err := s.ReportBad(testutil.Ver(t, "1.2.0"), testEntry, os.ErrInvalid); err != nil {
		t.Fatal(err)
	}
	if bad := s.Load().Bad; len(bad) != 2 || !s.Load().IsBad("1.1.0") || !s.Load().IsBad("1.2.0") {
		t.Errorf("Bad = %v, want 1.1.0 and 1.2.0 once each", bad)
	}
}

func TestFailedRollbackKeepsTheTrialOpen(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.1.0")
	os.Remove(s.Root.pointer())
	if err := os.Mkdir(s.Root.pointer(), 0o755); err != nil {
		t.Fatal(err)
	}
	rolled, err := s.ReportBad(testutil.Ver(t, "1.1.0"), testEntry, os.ErrInvalid)
	if err == nil || rolled {
		t.Fatalf("rolled=%v err=%v, want an error", rolled, err)
	}
	st := s.Load()
	if st.Pending != "1.1.0" || st.IsBad("1.1.0") || st.Attempts != MaxStartAttempts {
		t.Errorf("state = %+v, want the trial kept open and 1.1.0 not blamed yet", st)
	}
}

func TestPointerIsNotTouchedUntilTheVersionIsInPlace(t *testing.T) {
	s := newStore(t)
	missing := filepath.Join(s.Root.Staging(), "does-not-exist")
	if err := s.Install(missing, testutil.Ver(t, "1.1.0"), false); err == nil {
		t.Fatal("installed a directory that does not exist")
	}
	if got := current(t, s); got != "1.0.0" {
		t.Errorf("pointer = %s after a failed install", got)
	}
	if st := s.Load(); st.Pending != "" || st.Previous != "" {
		t.Errorf("state changed by a failed install: %+v", st)
	}
	if _, err := os.Stat(s.Root.VersionDir(testutil.Ver(t, "1.1.0"))); err == nil {
		t.Error("a version directory appeared")
	}
}

func TestInstallRefusesAnOlderOrEqualVersionThanThePointer(t *testing.T) {
	s := newStore(t)
	installTrial(t, s, "1.2.0")
	if err := s.Confirm(testutil.Ver(t, "1.2.0")); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.1.0", "1.2.0"} {
		err := s.Install(stage(t, s, v), testutil.Ver(t, v), false)
		if !errors.Is(err, ErrNotNewer) {
			t.Errorf("Install(%s) over 1.2.0 = %v, want ErrNotNewer", v, err)
		}
		if got := current(t, s); got != "1.2.0" {
			t.Errorf("pointer moved to %s", got)
		}
	}
	if err := s.RequireNewerThanInstalled(testutil.Ver(t, "1.1.0")); !errors.Is(err, ErrNotNewer) {
		t.Errorf("RequireNewerThanInstalled(1.1.0) = %v", err)
	}
	if err := s.RequireNewerThanInstalled(testutil.Ver(t, "1.3.0")); err != nil {
		t.Errorf("RequireNewerThanInstalled(1.3.0) = %v", err)
	}
}
