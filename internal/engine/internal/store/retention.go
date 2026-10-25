package store

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/statedir"
)

// Policy is the retention policy (§6.2): keep the newest Count snapshots,
// plus the newest snapshot of each of the last Days days, plus whatever
// moox-last names, plus every snapshot in pins and restoring-from.
type Policy struct {
	Count int
	Days  int
}

// DefaultPolicy is "20 14d".
var DefaultPolicy = Policy{Count: 20, Days: 14}

// ParsePolicy reads @moox-retention ("20 14d"); a missing or malformed part
// keeps its default.
func ParsePolicy(opt string) Policy {
	p := DefaultPolicy
	for _, w := range strings.Fields(opt) {
		if d, ok := strings.CutSuffix(w, "d"); ok {
			if n, err := strconv.Atoi(d); err == nil && n >= 0 {
				p.Days = n
			}
			continue
		}
		if n, err := strconv.Atoi(w); err == nil && n >= 1 {
			p.Count = n
		}
	}
	return p
}

// Retain applies the policy under the lock and returns what it removed. Only
// moox names are ranked or deleted (TestRetention_RanksOnlyMooxNames). A
// contents file is deleted only when no retained snapshot references it; a
// retained snapshot that does not decode keeps every contents file of its own
// stamp (it cannot say which one it names, so nothing is guessed away).
func (s *Store) Retain(l *Lock, p Policy, now time.Time) ([]string, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	names, err := s.names()
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	if ptr := s.Pointer(); ptr != "" {
		keep[ptr] = true
	}
	pins, err := s.Pins()
	if err != nil {
		return nil, err
	}
	for _, n := range pins {
		keep[n] = true
	}
	if rf, ok := s.RestoringFrom(); ok {
		keep[rf] = true
	}
	count := 0
	days := map[string]bool{}
	cutoff := now.Add(-time.Duration(p.Days) * 24 * time.Hour)
	for _, n := range names { // newest first
		if n.Contents {
			continue
		}
		if count < p.Count {
			keep[n.Base] = true
			count++
		}
		if p.Days > 0 && !n.Stamp.Before(cutoff) {
			day := n.Stamp.Format("20060102")
			if !days[day] {
				days[day] = true
				keep[n.Base] = true
			}
		}
	}

	referenced := map[string]bool{}
	for _, n := range names {
		if n.Contents || !keep[n.Base] {
			continue
		}
		b, _, err := s.snaps.ReadFile(n.Base)
		if err != nil {
			continue // a retained name that is gone references nothing
		}
		if ref, ok := contentsRef(b); ok {
			if ref != "" {
				referenced[ref] = true
			}
		} else {
			stamp := strings.TrimSuffix(n.Base, ".json")
			referenced[stamp+".contents.gz"] = true
		}
	}

	var removed []string
	var errs []error
	for _, n := range names {
		if (n.Contents && referenced[n.Base]) || (!n.Contents && keep[n.Base]) {
			continue
		}
		if err := s.snaps.Remove(n.Base, statedir.TypeRegular, nil); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		removed = append(removed, n.Base)
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("store: retention: %w", errors.Join(errs...))
	}
	return removed, nil
}
