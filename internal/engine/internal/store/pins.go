package store

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/statedir"
)

// Pins (A16, §6.2): a SET of snapshot basenames, released only by proof.
//
//   - During an attempt, restoring-from names its source. It is not a pin and
//     replaces nothing; retention keeps it while it exists.
//   - At the end, a passing attempt releases every pin whose snapshot has AT
//     MOST as many restored-as-pane rows as the attempt's source (a smaller
//     PASS cannot release a larger pin); a failing attempt adds its source.
//     Either way restoring-from is removed.
//   - A leftover restoring-from (the daemon died mid-restore) becomes a pin
//     (RecoverRestoringFrom), called by whoever clears the dead
//     @moox-restoring, or by the next gate.
//   - A pin whose snapshot cannot be read is kept: nothing proves it may go,
//     and `moox snapshot unpin` is the way out.

func contentsRef(b []byte) (string, bool) {
	snap, err := snapshot.Decode(b)
	if err != nil {
		return "", false
	}
	if snap.Contents == nil {
		return "", true
	}
	return snap.Contents.File, true
}

// Pins is the current set, sorted.
func (s *Store) Pins() ([]string, error) {
	b, _, err := s.state.ReadFile(PinsName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			set[n] = true
		}
	}
	return sorted(set), nil
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Store) writePins(l *Lock, pins []string) error {
	if err := l.check(); err != nil {
		return err
	}
	if len(pins) == 0 {
		err := s.state.Remove(PinsName, statedir.TypeRegular, nil)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.state.WriteFileSync(PinsName, []byte(strings.Join(pins, "\n")+"\n"), 0o600)
}

// AddPin adds name to the set.
func (s *Store) AddPin(l *Lock, name string) error {
	if p, ok := ParseName(name); !ok || p.Contents {
		return fmt.Errorf("store: cannot pin %q", name)
	}
	pins, err := s.Pins()
	if err != nil {
		return err
	}
	set := map[string]bool{name: true}
	for _, p := range pins {
		set[p] = true
	}
	return s.writePins(l, sorted(set))
}

// Unpin releases pins by hand (`moox snapshot unpin <file>|--all`); an empty
// name releases all of them.
func (s *Store) Unpin(l *Lock, name string) error {
	if name == "" {
		return s.writePins(l, nil)
	}
	pins, err := s.Pins()
	if err != nil {
		return err
	}
	var keep []string
	for _, p := range pins {
		if p != name {
			keep = append(keep, p)
		}
	}
	return s.writePins(l, keep)
}

// RestoringFrom is the running attempt's source, if any.
func (s *Store) RestoringFrom() (string, bool) {
	b, _, err := s.state.ReadFile(RestoringFromName)
	if err != nil {
		return "", false
	}
	n := strings.TrimSpace(string(b))
	if p, ok := ParseName(n); !ok || p.Contents {
		return "", false
	}
	return n, true
}

// BeginAttempt is step 0 of every restore: it records the source in
// restoring-from. Existing pins are untouched.
func (s *Store) BeginAttempt(l *Lock, source string) error {
	if p, ok := ParseName(source); !ok || p.Contents {
		return fmt.Errorf("store: %q is not a snapshot name", source)
	}
	if err := l.check(); err != nil {
		return err
	}
	return s.state.WriteFileSync(RestoringFromName, []byte(source+"\n"), 0o600)
}

func (s *Store) clearRestoringFrom(l *Lock) error {
	if err := l.check(); err != nil {
		return err
	}
	err := s.state.Remove(RestoringFromName, statedir.TypeRegular, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Attempt is the outcome of one restore, as the verdicts judged it.
type Attempt struct {
	Source string
	Boot   string // "PASS" | "INCOMPLETE" | "NOTHING"
	Misses int
	Launch string // "PASS" | "FAIL"
}

// Passed is §6.2's rule: (BOOT PASS, or BOOT NOTHING with zero misses) and
// LAUNCH PASS (zero judged rows is PASS).
func (a Attempt) Passed() bool {
	boot := a.Boot == "PASS" || (a.Boot == "NOTHING" && a.Misses == 0)
	return boot && a.Misses == 0 && a.Launch == "PASS"
}

// Settle applies an attempt's outcome to the pins and removes
// restoring-from. It returns the pins left.
func (s *Store) Settle(l *Lock, a Attempt) ([]string, error) {
	pins, err := s.Pins()
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, p := range pins {
		set[p] = true
	}
	if a.Passed() {
		src, err := s.Load(a.Source)
		if err == nil {
			have := src.RestoredPanes()
			for p := range set {
				snap, err := s.Load(p)
				if err == nil && snap.RestoredPanes() <= have {
					delete(set, p)
				}
			}
		}
	} else {
		set[a.Source] = true
	}
	left := sorted(set)
	if err := s.writePins(l, left); err != nil {
		return nil, err
	}
	return left, s.clearRestoringFrom(l)
}

// RecoverRestoringFrom turns a leftover restoring-from (a restore that died
// before its verdict) into a pin. ok reports whether there was one.
func (s *Store) RecoverRestoringFrom(l *Lock) (string, bool, error) {
	src, ok := s.RestoringFrom()
	if !ok {
		return "", false, nil
	}
	if err := s.AddPin(l, src); err != nil {
		return src, true, err
	}
	return src, true, s.clearRestoringFrom(l)
}

// WarningText is @moox-warning for a non-empty pins set ("" for none).
func WarningText(pins []string) string {
	if len(pins) == 0 {
		return ""
	}
	return fmt.Sprintf("restore INCOMPLETE: %d pinned — moox snapshot show", len(pins))
}
