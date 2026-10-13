package update

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu       sync.Mutex
	events   []string
	idleAt   int
	saveErrs int
	calls    int
}

func (r *recorder) add(e string) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }
func (r *recorder) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) policy() RestartPolicy {
	return RestartPolicy{
		Poll: time.Millisecond,
		Quiesce: func(force bool) bool {
			r.mu.Lock()
			r.calls++
			ok := force || r.calls >= r.idleAt
			r.mu.Unlock()
			if ok && force {
				r.add("quiesce-forced")
			} else if ok {
				r.add("quiesce")
			}
			return ok
		},
		SaveState: func() error {
			r.mu.Lock()
			fail := r.saveErrs > 0
			if fail {
				r.saveErrs--
			}
			r.mu.Unlock()
			if fail {
				r.add("save-failed")
				return errors.New("disk full")
			}
			r.add("save")
			return nil
		},
		Resume: func() { r.add("resume") },
	}
}

func restartUpdater(t *testing.T, r *recorder) *Updater {
	_, u := newFixture(t, "1.0.0", "1.1.0")
	u.exitFn = func(int) { r.add("restart") }
	return u
}

func TestRestartWaitsForIdleAndSavesFirst(t *testing.T) {
	r := &recorder{idleAt: 4}
	u := restartUpdater(t, r)
	if err := u.RestartWhenIdle(context.Background(), r.policy()); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log(), []string{"quiesce", "save", "restart"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if r.calls != 4 {
		t.Errorf("Quiesce called %d times, want 4", r.calls)
	}
}

func TestRestartAbortedWhenSaveFails(t *testing.T) {
	r := &recorder{idleAt: 1, saveErrs: 2}
	u := restartUpdater(t, r)
	if err := u.RestartWhenIdle(context.Background(), r.policy()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"quiesce", "save-failed", "resume",
		"quiesce", "save-failed", "resume",
		"quiesce", "save", "restart",
	}
	if got := r.log(); !slices.Equal(got, want) {
		t.Errorf("events = %v\nwant     %v", got, want)
	}
}

func TestRestartNeverHappensIfSaveAlwaysFails(t *testing.T) {
	r := &recorder{idleAt: 1, saveErrs: 1 << 30}
	u := restartUpdater(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := u.RestartWhenIdle(ctx, r.policy()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	for _, e := range r.log() {
		if e == "restart" {
			t.Fatal("restarted without saved state")
		}
	}
}

func TestMaxDeferralForcesRestart(t *testing.T) {
	r := &recorder{idleAt: 1 << 30}
	u := restartUpdater(t, r)
	p := r.policy()
	p.MaxDeferral = 20 * time.Millisecond
	if err := u.RestartWhenIdle(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log(), []string{"quiesce-forced", "save", "restart"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestUserQuitWhileBusyDoesNotRestart(t *testing.T) {
	r := &recorder{idleAt: 1 << 30}
	u := restartUpdater(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := u.RestartWhenIdle(ctx, r.policy()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if got := r.log(); len(got) != 0 {
		t.Errorf("unexpected events %v", got)
	}
}
