package update

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"selfupdate/internal/update/manifest"
)

func (u *Updater) Run(ctx context.Context, interval time.Duration) (*manifest.Manifest, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("update interval must be positive, got %v", interval)
	}
	wait := jitter(interval)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = interval + jitter(interval)

		m, err := u.Check(ctx)
		if err != nil {
			u.log.Warn("update check failed", "err", err)
			continue
		}
		if m == nil {
			continue
		}
		u.log.Info("update available", "current", u.cur, "new", m.Version)
		if err := u.Apply(ctx, *m); err != nil {
			if errors.Is(err, ErrTrialPending) || errors.Is(err, ErrBusy) || errors.Is(err, ErrNotNewer) {
				u.log.Info("update deferred", "version", m.Version, "reason", err)
				continue
			}
			u.log.Error("update failed", "version", m.Version, "err", err)
			continue
		}
		return m, nil
	}
}

func jitter(interval time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(interval/10) + 1))
}
