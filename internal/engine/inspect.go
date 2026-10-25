package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// SnapshotInfo is `moox snapshot show` and doctor's view of a server's
// snapshot directory (§8.1): read-only, it never heals the pointer.
type SnapshotInfo struct {
	Dir     string
	Pointer string // what moox-last says ("" if missing or malformed)
	// Last is the snapshot a restore would use: the pointer's file, or the
	// heal scan's answer (Healed). LastErr is set when nothing validates.
	Last         string
	LastPath     string
	LastSnapshot *snapshot.Snapshot
	LastErr      error
	Healed       bool
	// Pins are the pinned snapshots (A16), with their absolute paths.
	Pins     []string
	PinPaths []string
	// RestoringFrom is a running (or dead) attempt's source.
	RestoringFrom string
}

// Inspect reads moox-last, the pins and restoring-from. It writes nothing
// but the directories store.Open creates if they are missing.
func (e *Engine) Inspect() (SnapshotInfo, error) {
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer func() { _ = st.Close() }()
	info := SnapshotInfo{Dir: st.SnapshotDir(), Pointer: st.Pointer()}
	last, err := st.Last()
	if err != nil {
		info.LastErr = err
	} else {
		info.Last, info.LastPath, info.LastSnapshot, info.Healed = last.Name, st.Path(last.Name), last.Snapshot, last.Healed
	}
	pins, err := st.Pins()
	if err != nil {
		return info, err
	}
	info.Pins = pins
	for _, p := range pins {
		info.PinPaths = append(info.PinPaths, st.Path(p))
	}
	if rf, ok := st.RestoringFrom(); ok {
		info.RestoringFrom = rf
	}
	return info, nil
}

// SnapshotAge is how old a snapshot is by its created_at ("" if unknown).
func SnapshotAge(s *snapshot.Snapshot, now time.Time) (time.Duration, bool) {
	if s == nil {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s.CreatedAt)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}

// ErrNotPinned: unpin named a snapshot that is not pinned.
var ErrNotPinned = errors.New("engine: not pinned")

// Unpin releases one pin (name, a basename or the absolute path of one in
// this server's snapshot dir) or all of them (name ""), under save.lock, and
// re-sets @moox-warning on the server if it answers (§12.4: unpin is one of
// the option's writers). It returns the pins left.
func (e *Engine) Unpin(ctx context.Context, name string) ([]string, error) {
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	if name != "" {
		pins, err := st.Pins()
		if err != nil {
			return nil, err
		}
		found := false
		for _, p := range pins {
			if p == name || st.Path(p) == name {
				name, found = p, true
				break
			}
		}
		if !found {
			return pins, fmt.Errorf("%w: %s", ErrNotPinned, name)
		}
	}
	l, err := st.TryLock(e.record())
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if err := st.Unpin(l, name); err != nil {
		return nil, err
	}
	left, err := st.Pins()
	if err != nil {
		return nil, err
	}
	w := store.WarningText(left)
	args := []string{"set-option", "-g", "@moox-warning", w}
	if w == "" {
		args = []string{"set-option", "-gu", "@moox-warning"}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, _ = e.tmux.Run(cctx, args...)
	cancel()
	e.logf("UNPIN %q; %d pinned", name, len(left))
	return left, nil
}
