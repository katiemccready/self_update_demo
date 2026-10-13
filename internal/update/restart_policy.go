package update

import (
	"context"
	"errors"
	"time"
)

type RestartPolicy struct {
	Quiesce   func(force bool) bool
	SaveState func() error
	Resume    func()

	Poll time.Duration

	MaxDeferral time.Duration
}

func (p RestartPolicy) withDefaults() (RestartPolicy, error) {
	if p.Quiesce == nil || p.SaveState == nil {
		return p, errors.New("RestartPolicy needs Quiesce and SaveState")
	}
	if p.Resume == nil {
		p.Resume = func() {}
	}
	if p.Poll <= 0 {
		p.Poll = time.Second
	}
	return p, nil
}

func waitFor(ctx context.Context, ch <-chan time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

func (u *Updater) RestartWhenIdle(ctx context.Context, p RestartPolicy) error {
	p, err := p.withDefaults()
	if err != nil {
		return err
	}
	start := u.now()
	tick := time.NewTicker(p.Poll)
	defer tick.Stop()
	saveFailures := 0

	for first := true; ; first = false {
		if !first {
			if err := waitFor(ctx, tick.C); err != nil {
				return err
			}
		}
		force := p.MaxDeferral > 0 && u.now().Sub(start) >= p.MaxDeferral
		if !p.Quiesce(force) {
			if first {
				u.log.Info("update installed; waiting for app to become idle before restart")
			}
			continue
		}
		if u.saveAndRestart(p, force) {
			return nil
		}

		saveFailures++
		if err := waitFor(ctx, time.After(saveRetryBackoff(p.Poll, saveFailures))); err != nil {
			return err
		}
	}
}

func (u *Updater) saveAndRestart(p RestartPolicy, force bool) (restarted bool) {
	if force {
		u.log.Warn("restart deferral limit reached; interrupting in-flight work", "after", p.MaxDeferral)
	}
	if err := p.SaveState(); err != nil {
		u.log.Error("saving state failed; restart aborted, will retry", "err", err)
		p.Resume()
		return false
	}
	u.log.Info("state saved; restarting into new version")
	u.Restart()
	return true
}

const maxSaveRetryBackoff = time.Minute

func saveRetryBackoff(poll time.Duration, failures int) time.Duration {
	d := poll
	for i := 0; i < failures && d < maxSaveRetryBackoff; i++ {
		d *= 2
	}
	return min(d, maxSaveRetryBackoff)
}
