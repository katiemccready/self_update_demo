package launcher

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"selfupdate/internal/update/install"
)

const minRestartUptime = 10 * time.Second

type Config struct {
	Root install.Root
	Args []string
	Log  *slog.Logger

	Stdin          io.Reader
	Stdout, Stderr io.Writer

	Signals <-chan os.Signal

	InterruptDelay time.Duration

	StopGrace time.Duration

	now func() time.Time
}

func Run(cfg Config) int {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.InterruptDelay <= 0 {
		cfg.InterruptDelay = 3 * time.Second
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 30 * time.Second
	}
	entry, err := cfg.Root.ReadEntry()
	if err != nil {
		cfg.Log.Error("this is not a usable install root", "root", cfg.Root.Dir, "err", err)
		return 1
	}
	store := install.Open(cfg.Root, cfg.Log)

	for {
		plan, err := store.BeginStart(entry)
		if err != nil {
			cfg.Log.Error("cannot choose a version to start", "err", err)
			return 1
		}
		if plan.RolledBack {
			cfg.Log.Warn("started the previous version after a failed update", "version", plan.Version)
		}
		started := cfg.now()
		res := runOnce(cfg, plan)
		uptime := cfg.now().Sub(started)

		outcome := classify(res, store, plan, uptime)
		rollbackNext, err := store.EndStart(plan.Version, outcome)
		if err != nil {
			cfg.Log.Error("could not record how the run ended", "err", err)
		}
		switch {
		case res.stopped:
			return res.code
		case res.restart && outcome == install.Clean:
			cfg.Log.Info("restarting", "after", plan.Version)
		case rollbackNext:
			cfg.Log.Warn("the new version keeps failing; going back to the previous one", "version", plan.Version)
		case res.restart:
			cfg.Log.Error("the program keeps asking to restart without an update; giving up", "version", plan.Version)
			return 1
		default:
			return res.code
		}
	}
}

type result struct {
	code    int
	restart bool
	failed  bool
	stopped bool
}

func runOnce(cfg Config, plan install.Plan) result {
	cmd := exec.Command(plan.Entry, cfg.Args...)
	cmd.Env = append(os.Environ(), install.RootEnv+"="+cfg.Root.Dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Stdin, cfg.Stdout, cfg.Stderr
	if cfg.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cfg.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		cfg.Log.Error("cannot start the program", "version", plan.Version, "path", plan.Entry, "err", err)
		return result{code: 1, failed: true}
	}

	var stopped atomic.Bool
	done := make(chan struct{})
	finished := make(chan struct{})
	var timers []*time.Timer
	go func() {
		for {
			select {
			case sig := <-cfg.Signals:
				if !stopped.Swap(true) {
					timers = append(timers, time.AfterFunc(cfg.StopGrace, func() {
						cfg.Log.Warn("the program did not stop in time; killing it", "grace", cfg.StopGrace)
						_ = cmd.Process.Kill()
					}))
				}
				forward(cmd.Process, sig, cfg.Log)
				if sig == os.Interrupt {
					timers = append(timers, time.AfterFunc(cfg.InterruptDelay, func() { requestStop(cmd.Process, cfg.Log) }))
				}
			case <-done:
				for _, t := range timers {
					t.Stop()
				}
				finished <- struct{}{}
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	<-finished

	res := result{stopped: stopped.Load()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
		if res.code < 0 {
			res.code = 1
		}
	default:
		cfg.Log.Error("lost track of the program", "err", err)
		res.code, res.failed = 1, true
	}
	switch {
	case res.stopped:
	case res.code == install.RestartExitCode:
		res.restart = true
	case res.code == install.EnvironmentExitCode:
	case res.code != 0:
		res.failed = true
	}
	return res
}

func classify(res result, store *install.Store, plan install.Plan, uptime time.Duration) install.Outcome {
	switch {
	case res.stopped:
		return install.Clean
	case res.failed:
		return install.Failed
	case res.restart:
		if cur, err := store.Current(); err == nil && cur != plan.Version {
			return install.Clean
		}
		if uptime >= minRestartUptime {
			return install.Clean
		}
		return install.Failed
	}
	return install.Clean
}
