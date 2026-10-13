package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"selfupdate/internal/appstate"
)

const jobSteps = 5

type appState struct {
	Completed int `json:"completed"`
	Step      int `json:"step"`
}

var stateSchema = appstate.Schema{
	Current: 1,
	Migrate: map[int]func(json.RawMessage) (json.RawMessage, error){
		0: func(d json.RawMessage) (json.RawMessage, error) { return d, nil },
	},
}

func (s *appState) sanitize(log *slog.Logger) {
	if s.Step < 0 || s.Step >= jobSteps {
		log.Warn("saved step out of range; restarting the job", "step", s.Step)
		s.Step = 0
	}
	if s.Completed < 0 {
		log.Warn("saved completed count out of range; resetting", "completed", s.Completed)
		s.Completed = 0
	}
}

type app struct {
	log       *slog.Logger
	statePath string
	stepDur   time.Duration

	saveMu sync.Mutex

	mu       sync.Mutex
	st       appState
	active   bool
	quiesced bool

	parent  context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

func newApp(log *slog.Logger, statePath string, stepDur time.Duration) (*app, error) {
	a := &app{log: log, statePath: statePath, stepDur: stepDur}
	err := appstate.LoadVersioned(statePath, stateSchema, &a.st)
	var newer *appstate.NewerSchemaError
	if errors.As(err, &newer) {
		from, rerr := appstate.RestoreFromBackup(statePath, stateSchema)
		if rerr != nil {
			return nil, fmt.Errorf("%w; and no usable backup: %v", err, rerr)
		}
		log.Error("saved state came from a newer build; restored the pre-upgrade backup "+
			"(newer state kept alongside as *.newer; changes made by the newer build are not visible)",
			"found", newer.Found, "restored_schema", from)
		a.st = appState{}
		err = appstate.LoadVersioned(statePath, stateSchema, &a.st)
	}
	if err != nil {
		if !errors.Is(err, appstate.ErrCorrupt) {
			return nil, fmt.Errorf("load saved state %s: %w", statePath, err)
		}
		log.Error("saved state was corrupt (kept aside), starting fresh", "err", err)
		a.st = appState{}
	} else {
		a.st.sanitize(log)
	}
	if a.st.Step > 0 {
		log.Info("resuming interrupted job", "step", a.st.Step, "of", jobSteps, "completed", a.st.Completed)
	}
	return a, nil
}

func (a *app) start(parent context.Context) {
	a.parent = parent
	a.startWorker()
}

func (a *app) startWorker() {
	a.mu.Lock()
	ctx, cancel := context.WithCancel(a.parent)
	a.cancel, a.stopped = cancel, false
	a.mu.Unlock()
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.run(ctx) }()
}

func (a *app) stop() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.stopped = true
	a.mu.Unlock()
	a.wg.Wait()
}

func (a *app) run(ctx context.Context) {
	for ctx.Err() == nil {
		if !a.begin() {
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		a.doJob(ctx)
		a.end()
	}
}

func (a *app) begin() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quiesced {
		return false
	}
	a.active = true
	return true
}

func (a *app) end() { a.mu.Lock(); a.active = false; a.mu.Unlock() }

func (a *app) doJob(ctx context.Context) {
	for {
		a.mu.Lock()
		step := a.st.Step
		a.mu.Unlock()
		if step >= jobSteps {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.stepDur):
		}
		a.mu.Lock()
		a.st.Step++
		a.mu.Unlock()
		a.log.Info("working", "step", step+1, "of", jobSteps)
	}
	a.mu.Lock()
	a.st.Completed++
	a.st.Step = 0
	n := a.st.Completed
	a.mu.Unlock()
	a.log.Info("job complete", "completed", n)
	if err := a.Save(); err != nil {
		a.log.Warn("save after job failed", "err", err)
	}
}

func (a *app) Quiesce(force bool) bool {
	a.mu.Lock()
	a.quiesced = true
	idle := !a.active
	a.mu.Unlock()
	if force {
		a.stop()
		return true
	}
	return idle
}

func (a *app) Save() error {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	snap := a.st
	a.mu.Unlock()
	return appstate.SaveVersioned(a.statePath, stateSchema, snap)
}

func (a *app) Resume() {
	a.mu.Lock()
	a.quiesced = false
	needWorker := a.stopped
	a.mu.Unlock()
	if needWorker {
		a.startWorker()
	}
}
