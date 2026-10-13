package install

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/update/manifest"
)

const MaxStartAttempts = 3

const stateLockTimeout = 10 * time.Second

var ErrTrialPending = errors.New("a previous update has not been confirmed healthy yet")

var ErrNotNewer = errors.New("not newer than the installed version")

type Store struct {
	Root Root
	log  *slog.Logger

	mu         sync.Mutex
	lock       *fsutil.FileLock
	lockWarned atomic.Bool
}

func Open(root Root, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{Root: root, log: log, lock: fsutil.NewFileLock(root.stateLock())}
}

func (s *Store) Current() (manifest.Version, error) { return s.Root.ReadPointer() }

func (s *Store) Load() State {
	var st State
	b, err := fsutil.ReadFile(s.Root.state())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("read update state", "err", err)
		}
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		quarantined, rerr := fsutil.Quarantine(s.Root.state())
		if rerr != nil {
			quarantined = "(could not preserve: " + rerr.Error() + ")"
		}
		s.log.Warn("corrupt update state, starting fresh", "err", err, "kept_at", quarantined)
		return State{}
	}
	return st
}

func (s *Store) save(st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	err = fsutil.WriteFileAtomic(s.Root.state(), b, 0o600)
	if errors.Is(err, fsutil.ErrDirSync) {
		s.log.Warn("update state written but directory fsync failed", "err", err)
	}
	return fsutil.IgnoreDirSync(err)
}

func (s *Store) with(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch err := s.lock.Lock(stateLockTimeout); {
	case err == nil:
		defer s.lock.Unlock()
	case errors.Is(err, fsutil.ErrLockHeld):
		return fmt.Errorf("another instance is holding the update state lock: %w", err)
	default:
		if s.lockWarned.CompareAndSwap(false, true) {
			s.log.Warn("cross-process locking unavailable; relying on a single running instance", "err", err)
		}
	}
	return fn()
}

func (s *Store) RequireNewerThanInstalled(nv manifest.Version) error {
	cur, err := s.Current()
	if err != nil {
		return fmt.Errorf("cannot tell which version is current: %w", err)
	}
	if nv.Compare(cur) <= 0 {
		return fmt.Errorf("%w: %s is installed, refusing %s", ErrNotNewer, cur, nv)
	}
	return nil
}

func (s *Store) RequireNoTrial() error {
	var pending string
	if err := s.with(func() error { pending = s.Load().Pending; return nil }); err != nil {
		return err
	}
	if pending != "" {
		return fmt.Errorf("%w (version %s)", ErrTrialPending, pending)
	}
	return nil
}

func (s *Store) Install(staged string, nv manifest.Version, allowUnsafeDir bool) error {
	return s.with(func() error {
		if err := CheckContext(s.Root.Dir, allowUnsafeDir); err != nil {
			return err
		}
		cur, err := s.Current()
		if err != nil {
			return fmt.Errorf("cannot tell which version is current: %w", err)
		}
		if nv.Compare(cur) <= 0 {
			return fmt.Errorf("%w: %s is installed, refusing %s", ErrNotNewer, cur, nv)
		}

		dst := s.Root.VersionDir(nv)
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("remove stale %s: %w", dst, err)
		}
		if err := os.MkdirAll(s.Root.Versions(), 0o755); err != nil {
			return err
		}
		if err := fsutil.Rename(staged, dst); err != nil {
			return fmt.Errorf("move the unpacked version into place: %w", err)
		}
		_ = fsutil.SyncDir(s.Root.Versions())

		st := s.Load()
		prev := st.clone()
		delete(st.Rejected, nv.String())
		st.Pending, st.Attempts, st.Previous = nv.String(), 0, cur.String()
		if err := s.save(st); err != nil {
			_ = os.RemoveAll(dst)
			return fmt.Errorf("save state: %w", err)
		}
		if err := s.Root.WritePointer(nv); err != nil {
			errs := fmt.Errorf("switch to %s: %w", nv, err)
			if serr := s.save(prev); serr != nil {
				s.log.Error("could not undo recorded update trial; further updates wait for the next start",
					"version", nv, "err", serr)
				errs = errors.Join(errs, fmt.Errorf("restore state: %w", serr))
			}
			_ = os.RemoveAll(dst)
			return errs
		}
		s.log.Info("update installed", "from", cur, "to", nv)
		return nil
	})
}

func (s *Store) Reject(nv manifest.Version, now time.Time) {
	err := s.with(func() error {
		st := s.Load()
		if st.Rejected == nil {
			st.Rejected = map[string]Rejection{}
		}
		r := st.Rejected[nv.String()]
		r.Count++
		r.Until = now.Add(rejectBackoff(r.Count))
		st.Rejected[nv.String()] = r
		return s.save(st)
	})
	if err != nil {
		s.log.Warn("could not record rejected version", "version", nv, "err", err)
	}
}

func (s *Store) Screen(issuedAt, buildTime time.Time, nv manifest.Version, now time.Time) (skip bool, err error) {
	err = s.with(func() error {
		st := s.Load()
		if issuedAt.Before(st.LastIssued) {
			return fmt.Errorf("%w: issued %s, already saw %s", manifest.ErrReplayedManifest,
				issuedAt.UTC().Format(time.RFC3339), st.LastIssued.UTC().Format(time.RFC3339))
		}
		if issuedAt.Before(buildTime) {
			return fmt.Errorf("%w: issued %s, before this program was built (%s)", manifest.ErrReplayedManifest,
				issuedAt.UTC().Format(time.RFC3339), buildTime.UTC().Format(time.RFC3339))
		}
		if issuedAt.After(st.LastIssued) {
			st.LastIssued = issuedAt
			if err := s.save(st); err != nil {
				s.log.Warn("could not record manifest issue time", "err", err)
			}
		}
		switch until, rejected := st.RejectedUntil(nv.String(), now); {
		case st.IsBad(nv.String()):
			s.log.Info("skipping version that previously failed", "version", nv)
			skip = true
		case rejected:
			s.log.Info("skipping version that failed verification; will retry later",
				"version", nv, "retry_after", until)
			skip = true
		}
		return nil
	})
	return skip, err
}

func (s *Store) ClientID() string {
	if id := s.Load().ClientID; id != "" {
		return id
	}
	var id string
	err := s.with(func() error {
		st := s.Load()
		if st.ClientID == "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			st.ClientID = hex.EncodeToString(b)
			if err := s.save(st); err != nil {
				return err
			}
		}
		id = st.ClientID
		return nil
	})
	if err != nil {
		s.log.Warn("cannot store the client ID; staged rollouts will treat this client as last", "err", err)
		return ""
	}
	return id
}
