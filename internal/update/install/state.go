package install

import (
	"maps"
	"slices"
	"time"

	"selfupdate/internal/update/manifest"
)

type State struct {
	Pending  string               `json:"pending,omitempty"`
	Attempts int                  `json:"attempts,omitempty"`
	Bad      []string             `json:"bad,omitempty"`
	Rejected map[string]Rejection `json:"rejected,omitempty"`

	LastIssued time.Time `json:"last_issued,omitempty"`

	Previous string `json:"previous,omitempty"`
	LastGood string `json:"last_good,omitempty"`

	ClientID string `json:"client_id,omitempty"`
}

type Rejection struct {
	Count int       `json:"count"`
	Until time.Time `json:"until"`
}

const (
	rejectBase = 6 * time.Hour
	rejectMax  = 7 * 24 * time.Hour
)

func rejectBackoff(count int) time.Duration {
	d := rejectBase
	for i := 1; i < count && d < rejectMax; i++ {
		d *= 2
	}
	return min(d, rejectMax)
}

func (s State) clone() State {
	s.Bad = slices.Clone(s.Bad)
	s.Rejected = maps.Clone(s.Rejected)
	return s
}

func (s *State) prune(cur manifest.Version) {
	stale := func(v string) bool {
		pv, err := manifest.ParseVersion(v)
		return err == nil && pv.Compare(cur) <= 0
	}
	s.Bad = slices.DeleteFunc(s.Bad, stale)
	for v := range s.Rejected {
		if stale(v) {
			delete(s.Rejected, v)
		}
	}
	if len(s.Rejected) == 0 {
		s.Rejected = nil
	}
}

func (s State) RejectedUntil(v string, now time.Time) (time.Time, bool) {
	r, ok := s.Rejected[v]
	return r.Until, ok && now.Before(r.Until)
}

func (s State) IsBad(v string) bool { return slices.Contains(s.Bad, v) }
