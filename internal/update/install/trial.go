package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"selfupdate/internal/update/manifest"
)

type Plan struct {
	Version    manifest.Version
	Entry      string
	RolledBack bool
}

func (s *Store) BeginStart(entry string) (Plan, error) {
	var plan Plan
	err := s.with(func() error {
		st := s.Load()
		cur, ok := s.resolve(&st, entry)
		if !ok {
			return errors.New("no usable version is installed")
		}

		switch {
		case st.Pending == "":
		case st.Pending != cur.String():
			st.Pending, st.Attempts = "", 0
		case st.Attempts >= MaxStartAttempts:
			s.log.Error("new version never became healthy, rolling back",
				"version", st.Pending, "starts", st.Attempts, "to", st.Previous)
			rolled, err := s.rollbackLocked(&st, entry)
			if err != nil {
				return err
			}
			if rolled {
				plan.RolledBack = true
				cur, _ = s.Root.ReadPointer()
			}
		default:
			st.Attempts++
		}
		if err := s.save(st); err != nil {
			return fmt.Errorf("record the start: %w", err)
		}
		plan.Version = cur
		plan.Entry = s.Root.entryPath(cur, entry)
		return nil
	})
	return plan, err
}

func (s *Store) resolve(st *State, entry string) (manifest.Version, bool) {
	cur, err := s.Root.ReadPointer()
	if err == nil && s.Root.usable(cur, entry) {
		return cur, true
	}
	s.log.Error("the current version cannot be used; looking for a good one", "pointer_err", err)
	for _, cand := range []string{st.LastGood, st.Previous} {
		v, perr := manifest.ParseVersion(cand)
		if cand == "" || perr != nil || !s.Root.usable(v, entry) {
			continue
		}
		if werr := s.Root.WritePointer(v); werr != nil {
			s.log.Warn("could not repair the pointer", "err", werr)
		}
		if err == nil && st.Pending == cur.String() {
			st.Bad = append(st.Bad, st.Pending)
			st.Pending, st.Attempts = "", 0
		}
		s.log.Warn("fell back to a previous version", "version", v)
		return v, true
	}
	return manifest.Version{}, false
}

func (s *Store) rollbackLocked(st *State, entry string) (rolledBack bool, err error) {
	bad := st.Pending
	if !slices.Contains(st.Bad, bad) {
		st.Bad = append(st.Bad, bad)
	}
	st.Pending, st.Attempts = "", 0
	target, perr := manifest.ParseVersion(st.Previous)
	if st.Previous == "" || perr != nil || !s.Root.usable(target, entry) {
		s.log.Error("no previous version to roll back to; staying on the failed one", "version", bad)
		return false, nil
	}
	if err := s.Root.WritePointer(target); err != nil {
		st.Pending, st.Attempts = bad, MaxStartAttempts
		st.Bad = slices.DeleteFunc(st.Bad, func(v string) bool { return v == bad })
		return false, fmt.Errorf("point back at %s: %w", target, err)
	}
	return true, nil
}

type Outcome int

const (
	Clean Outcome = iota

	Failed
)

func (s *Store) EndStart(v manifest.Version, o Outcome) (rollbackNext bool, err error) {
	err = s.with(func() error {
		st := s.Load()
		if st.Pending != v.String() {
			return nil
		}
		if o == Clean {
			if st.Attempts > 0 {
				st.Attempts--
				return s.save(st)
			}
			return nil
		}
		rollbackNext = st.Attempts >= MaxStartAttempts
		return nil
	})
	return rollbackNext, err
}

func (s *Store) Confirm(v manifest.Version) error {
	var prune bool
	err := s.with(func() error {
		st := s.Load()
		if st.Pending != v.String() {
			return nil
		}
		st.Pending, st.Attempts = "", 0
		st.LastGood = v.String()
		st.prune(v)
		if err := s.save(st); err != nil {
			return err
		}
		prune = true
		s.log.Info("version confirmed healthy", "version", v)
		return nil
	})
	if err == nil && prune {
		s.collectGarbage()
	}
	return err
}

func (s *Store) ReportBad(v manifest.Version, entry string, reason error) (rolledBack bool, err error) {
	err = s.with(func() error {
		st := s.Load()
		if st.Pending != v.String() {
			return nil
		}
		s.log.Error("new version reports itself unusable, rolling back", "version", v, "reason", reason)
		var rerr error
		rolledBack, rerr = s.rollbackLocked(&st, entry)
		if serr := s.save(st); serr != nil && rerr == nil {
			rerr = serr
		}
		return rerr
	})
	return rolledBack, err
}

func (s *Store) collectGarbage() {
	keep := map[string]bool{}
	if cur, err := s.Root.ReadPointer(); err == nil {
		keep[cur.String()] = true
	}
	st := s.Load()
	for _, v := range []string{st.Previous, st.LastGood} {
		if v != "" {
			keep[v] = true
		}
	}
	entries, err := os.ReadDir(s.Root.Versions())
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, err := manifest.ParseVersion(e.Name()); err != nil || keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Root.Versions(), e.Name())); err != nil {
			s.log.Debug("could not delete an old version yet", "version", e.Name(), "err", err)
		}
	}
}
