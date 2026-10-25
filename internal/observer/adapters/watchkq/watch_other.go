//go:build !darwin && !linux

package watchkq

import (
	"context"
	"sync"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// Watcher is the nowatch implementation: everything compiles, nothing is
// armed, and Watch says so with ErrUnsupported so the core falls straight into
// on-demand mode (§6.6) instead of waiting forever for an event that cannot
// come.
//
// It is a real type rather than a build-tagged absence because the composition
// root must not be conditional: a `tt sessions --json` built for Linux answers
// exactly as it does on darwin, by reading all sources on every query. The only
// thing missing is the push.
type Watcher struct {
	mu     sync.Mutex
	closed bool
	hints  chan observer.Change
}

// DefaultCaps returns the same guards the darwin build uses, so a cap-priority
// test is not platform-conditional.
func DefaultCaps() Caps {
	return Caps{MaxFiles: MaxFilesCeiling, MaxJobDirs: MaxJobDirs, MaxJobTranscripts: MaxJobTranscripts}
}

// New always succeeds. Refusing to construct would push a platform check into
// every caller; refusing to WATCH keeps the choice in one place.
func New(caps Caps) (*Watcher, error) {
	_ = caps
	return &Watcher{hints: make(chan observer.Change)}, nil
}

// Watch reports that this platform cannot push.
func (w *Watcher) Watch(ctx context.Context) (<-chan observer.Change, error) {
	_ = ctx
	return nil, ErrUnsupported
}

// Reconcile accounts for the desired set and arms nothing. It reports
// Degraded = "kqueue-unavailable" on every call, so sources.<id>.watch.degraded
// carries the reason in EVERY response rather than only at startup.
func (w *Watcher) Reconcile(ctx context.Context, targets []observer.WatchTarget) (observer.ArmReport, error) {
	if err := ctx.Err(); err != nil {
		return observer.ArmReport{}, err
	}
	accepted, _ := prioritise(targets, DefaultCaps())
	rep := observer.ArmReport{ByKind: map[observer.TargetKind]int{}, Degraded: observer.DegradedKqueueUnavailable}
	for _, t := range accepted {
		rep.ByKind[t.Kind]++
	}
	return rep, nil
}

// Release releases nothing, because nothing was armed, and arms nothing.
func (w *Watcher) Release(ctx context.Context, keep []observer.WatchTarget) (observer.ArmReport, error) {
	if err := ctx.Err(); err != nil {
		return observer.ArmReport{}, err
	}
	_ = keep
	return observer.ArmReport{ByKind: map[observer.TargetKind]int{}, Degraded: observer.DegradedKqueueUnavailable}, nil
}

// Close is idempotent and releases nothing, because nothing was armed.
func (w *Watcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.hints)
	}
	return nil
}

// FDs is empty on this platform.
func (w *Watcher) FDs() map[string]int { return map[string]int{} }

// Generations is empty on this platform. It exists so a test that compares
// arming generations compiles everywhere, even though nothing is ever armed
// here.
func (w *Watcher) Generations() map[string]int64 { return map[string]int64{} }

// Counters is all zeroes on this platform.
func (w *Watcher) Counters() (events, dropped int64, lastEventAt time.Time) {
	return 0, 0, time.Time{}
}

var _ observer.Watcher = (*Watcher)(nil)
