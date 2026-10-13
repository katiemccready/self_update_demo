package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"selfupdate/internal/appstate"

	"selfupdate/internal/testutil"
)

func testApp(t *testing.T, path string) *app {
	t.Helper()
	a, err := newApp(testutil.Quiet(), path, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func (a *app) snapshot() (appState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st, a.active
}

func TestQuiesceDrainsThenBlocksNewWork(t *testing.T) {
	a := testApp(t, filepath.Join(t.TempDir(), "s.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.start(ctx)
	waitFor(t, func() bool { _, act := a.snapshot(); return act })
	if a.Quiesce(false) {
		t.Fatal("reported idle while a job was in flight")
	}

	waitFor(t, func() bool { return a.Quiesce(false) })
	s1, _ := a.snapshot()
	if s1.Step != 0 || s1.Completed != 1 {
		t.Fatalf("job should have completed cleanly: %+v", s1)
	}

	time.Sleep(60 * time.Millisecond)
	if s2, act := a.snapshot(); act || s2 != s1 {
		t.Errorf("work continued while quiesced: %+v active=%v", s2, act)
	}
	a.stop()
}

func TestForcedQuiesceSavesProgressAndRestartResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a := testApp(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.start(ctx)
	waitFor(t, func() bool { s, _ := a.snapshot(); return s.Step >= 2 })

	if !a.Quiesce(true) {
		t.Fatal("forced quiesce refused")
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	want, _ := a.snapshot()
	if want.Step == 0 || want.Step >= jobSteps {
		t.Fatalf("expected mid-job state, got %+v", want)
	}

	var onDisk appState
	if err := appstate.LoadVersioned(path, stateSchema, &onDisk); err != nil || onDisk != want {
		t.Fatalf("on disk %+v (err %v), want %+v", onDisk, err, want)
	}
	b := testApp(t, path)
	if got, _ := b.snapshot(); got != want {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
	b.start(ctx)
	waitFor(t, func() bool { s, _ := b.snapshot(); return s.Completed == want.Completed+1 })
	b.stop()
}

func TestResumeAfterAbortedForcedRestart(t *testing.T) {
	a := testApp(t, filepath.Join(t.TempDir(), "s.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.start(ctx)
	waitFor(t, func() bool { s, _ := a.snapshot(); return s.Step >= 1 })
	a.Quiesce(true)
	a.Resume()
	waitFor(t, func() bool { s, _ := a.snapshot(); return s.Completed >= 1 })
	a.stop()
}

func TestNewAppRefusesToStartOnNonCorruptLoadError(t *testing.T) {
	log := testutil.Quiet()

	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(log, p, time.Millisecond)
	if err == nil || a != nil {
		t.Fatalf("newApp = %v, %v; want an error and no app", a, err)
	}
	if errors.Is(err, appstate.ErrCorrupt) {
		t.Errorf("I/O error misclassified as corrupt: %v", err)
	}
}

func TestNewAppStartsFreshOnCorruptStateAndKeepsTheFile(t *testing.T) {
	log := testutil.Quiet()
	p := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	a, err := newApp(log, p, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if a.st != (appState{}) {
		t.Errorf("state = %+v, want zero", a.st)
	}
	if b, err := os.ReadFile(p + ".corrupt"); err != nil || string(b) != "{not json" {
		t.Errorf("corrupt file not preserved: %q %v", b, err)
	}
}

func TestOutOfRangeSavedStateIsClamped(t *testing.T) {
	log := testutil.Quiet()
	for name, st := range map[string]appState{
		"negative step":   {Step: -1_000_000_000, Completed: 3},
		"step past end":   {Step: jobSteps + 4, Completed: 3},
		"negative count":  {Step: 2, Completed: -7},
		"valid is intact": {Step: 2, Completed: 3},
	} {
		p := filepath.Join(t.TempDir(), "state.json")
		if err := appstate.SaveVersioned(p, stateSchema, st); err != nil {
			t.Fatal(err)
		}
		a, err := newApp(log, p, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if a.st.Step < 0 || a.st.Step >= jobSteps || a.st.Completed < 0 {
			t.Errorf("%s: loaded %+v", name, a.st)
		}
		if name == "valid is intact" && a.st != st {
			t.Errorf("valid state altered: %+v", a.st)
		}
	}
}

func TestConcurrentSavesNeverLeaveAnOlderSnapshot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	a := testApp(t, p)
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.mu.Lock()
			a.st.Completed++
			a.mu.Unlock()
			if err := a.Save(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var got appState
	if err := appstate.LoadVersioned(p, stateSchema, &got); err != nil {
		t.Fatal(err)
	}
	if got.Completed != n {
		t.Errorf("file holds Completed=%d, want %d (an older snapshot overwrote a newer one)", got.Completed, n)
	}
}

func TestUpgradeFromLegacyUnversionedStateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"completed":6,"step":2}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	a := testApp(t, p)
	if got, _ := a.snapshot(); got != (appState{Completed: 6, Step: 2}) {
		t.Fatalf("legacy state not carried over: %+v", got)
	}
	if b, _ := os.ReadFile(p + ".v0.bak"); string(b) != legacy {
		t.Errorf("no pre-upgrade backup: %q", b)
	}
}

func TestRolledBackBuildRestoresBackupInsteadOfMisreadingNewerState(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(p, []byte(`{"completed":5,"step":1}`), 0o600)
	os.WriteFile(p+".v0.bak", []byte(`{"completed":5,"step":1}`), 0o600)
	newer := `{"schema":99,"data":{"jobs":[1,2,3]}}`
	os.WriteFile(p, []byte(newer), 0o600)

	a := testApp(t, p)
	if got, _ := a.snapshot(); got != (appState{Completed: 5, Step: 1}) {
		t.Fatalf("restored state = %+v", got)
	}
	if b, _ := os.ReadFile(p + ".v99.newer"); string(b) != newer {
		t.Errorf("newer state not preserved: %q", b)
	}
}

func TestNewerStateWithoutBackupRefusesToStart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	newer := `{"schema":99,"data":{}}`
	os.WriteFile(p, []byte(newer), 0o600)
	log := testutil.Quiet()
	if a, err := newApp(log, p, time.Millisecond); err == nil {
		t.Fatalf("started (%v) on state it cannot read", a)
	}
	if b, _ := os.ReadFile(p); string(b) != newer {
		t.Error("newer state was modified")
	}
}
