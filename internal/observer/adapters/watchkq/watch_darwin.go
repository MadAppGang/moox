//go:build darwin

package watchkq

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/MadAppGang/moox/internal/observer"
)

// wakeIdent is the identifier of the EVFILT_USER event Watch registers on its
// own kqueue so that ctx cancellation can wake a BLOCKED kevent() call.
//
// This is why there is no poll timeout anywhere in this file, and it is what
// makes AC9 literally true for the watcher: kevent() is called with a nil
// timeout, so an idle process holds no timer, wakes for nothing, and burns no
// CPU. The alternative — a 1 s timeout so cancellation is noticed "soon" — is a
// poll loop with better manners.
const wakeIdent = 1

// hintBuffer is how many coalesced hints may be in flight before the kevent
// loop starts dropping them. A drop is sound because a hint carries no
// information beyond "something changed": while the channel is non-empty a
// rebuild is already coming, and a rebuild reads the whole tree.
const hintBuffer = 64

// watch is one armed target.
type watch struct {
	fd   int
	kind observer.TargetKind
	// gen is the process-wide arming generation this descriptor was opened at.
	// It exists because fd NUMBERS are reused: close(7) then open() hands back
	// 7, so "the fd number is unchanged" cannot by itself distinguish an
	// untouched descriptor from a closed-and-reopened one. A test that has to
	// tell those apart — and §0.5 E5 makes that the difference between working
	// and silently lossy — compares generations.
	gen int64
	// dev and ino are the identity the descriptor actually holds. A
	// rename-publish of a job record leaves the path in place and changes the
	// inode underneath it, and a descriptor on the old inode is deaf from that
	// moment on — so identity, not path, is what decides whether a re-arm is
	// owed.
	dev uint64
	ino uint64
}

// Watcher is the kqueue implementation of observer.Watcher.
type Watcher struct {
	caps Caps

	mu     sync.Mutex
	kq     int
	armed  map[string]watch
	closed bool
	armGen int64

	hints   chan observer.Change
	started bool
	// done is closed by markDead once the kevent loop has stopped and the
	// kqueue is released. Close waits on it, so "closed" means the reader has
	// stopped rather than merely been asked to (code review round 2, G1).
	done chan struct{}

	// Counters, for sources.<id>.watch. events counts delivered hints and
	// dropped counts the ones the buffer could not take; reporting both is what
	// makes a saturated consumer visible instead of silent.
	events      int64
	dropped     int64
	lastEventAt time.Time
}

// DefaultCaps is §6.4's cap set for this process: min(1024, RLIMIT_NOFILE/4)
// per-file watches, 64 job directories, 64 job transcripts.
func DefaultCaps() Caps {
	caps := Caps{MaxFiles: MaxFilesCeiling, MaxJobDirs: MaxJobDirs, MaxJobTranscripts: MaxJobTranscripts}
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err == nil && lim.Cur > 0 {
		if quarter := int(lim.Cur / 4); quarter < caps.MaxFiles {
			caps.MaxFiles = quarter
		}
	}
	return caps
}

// New creates the kqueue. A failure here is ErrUnsupported-wrapped rather than
// fatal: §6.6 requires the observer to keep answering on-demand when it cannot
// push, so the caller's job is to record watch.mode = "on-demand", not to exit.
func New(caps Caps) (*Watcher, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("%w: kqueue: %v", ErrUnsupported, err)
	}
	unix.CloseOnExec(kq)

	w := &Watcher{
		caps:  caps.withDefaults(),
		kq:    kq,
		armed: map[string]watch{},
		hints: make(chan observer.Change, hintBuffer),
		done:  make(chan struct{}),
	}
	// Register the wake-up channel before anything else, so Close can always
	// interrupt a blocked kevent().
	wake := unix.Kevent_t{
		Ident:  wakeIdent,
		Filter: unix.EVFILT_USER,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{wake}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("%w: register wakeup: %v", ErrUnsupported, err)
	}
	return w, nil
}

// Watch implements observer.Watcher. It starts the kevent loop on first call
// and returns the same channel on any later one, so a caller that re-Watches
// after a transient failure does not end up with two loops on one kqueue.
func (w *Watcher) Watch(ctx context.Context) (<-chan observer.Change, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, ErrUnsupported
	}
	first := !w.started
	w.started = true
	ch := w.hints
	w.mu.Unlock()

	if first {
		go w.loop()
		go func() {
			<-ctx.Done()
			_ = w.Close()
		}()
	}
	return ch, nil
}

// loop is the only goroutine that calls kevent() for reading. It blocks with a
// nil timeout — no timer, no poll — and returns when Close triggers the
// EVFILT_USER wake-up or the kqueue goes away.
func (w *Watcher) loop() {
	defer w.markDead()

	events := make([]unix.Kevent_t, hintBuffer)
	for {
		n, err := unix.Kevent(w.kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return
		}
		for i := 0; i < n; i++ {
			ev := events[i]
			if ev.Filter == unix.EVFILT_USER {
				return
			}
			w.deliver(int(ev.Ident))
		}
		if w.isClosed() {
			return
		}
	}
}

// markDead is the loop's one exit path, and it is what makes a dead watch SAY
// it is dead.
//
// Two things happen here, and the order matters.
//
// First, w.closed is set. The loop returns on ANY non-EINTR kevent() error, not
// only on the EVFILT_USER wake-up Close triggers, and in that case nothing else
// on the machine knows the watch has stopped. Without this line Watch() takes
// its `!w.closed` route, finds w.started already true so it starts no second
// loop, and hands the caller back the channel this function is about to CLOSE —
// with a nil error. A caller written to re-arm on failure then re-selects on a
// closed channel forever: a measured 1.08M Watch() calls per second, one core
// pegged, inside a server the user runs until reboot, while Mode() goes on
// reporting "kqueue". Setting closed turns that into ErrUnsupported, which is
// the §6.6 on-demand transition the caller already implements.
//
// Second, the armed descriptors are released. Close() returns early when closed
// is already set, so once this function claims the watcher it also owns the
// ~57 descriptors Close would otherwise have shut — and leaking those on the
// path that fires when the kqueue is already sick is the wrong direction.
//
// The kqueue descriptor is closed HERE rather than in Close, under the same
// mutex Reconcile holds: Close may be called while a Reconcile is in flight,
// and closing the kqueue out from under it would turn a shutdown into an EBADF
// the caller would read as an arm failure.
func (w *Watcher) markDead() {
	w.mu.Lock()
	w.closed = true
	for path, a := range w.armed {
		_ = unix.Close(a.fd)
		delete(w.armed, path)
	}
	_ = unix.Close(w.kq)
	w.kq = -1
	w.mu.Unlock()
	close(w.hints)
	close(w.done)
}

// deliver turns one kernel event into one coarse hint. The event's fflags are
// deliberately NOT forwarded: the port promises only "something changed", and
// a consumer that could read the kernel's opinion of WHAT changed would grow a
// dependence on it that a Linux implementation could not honour.
func (w *Watcher) deliver(fd int) {
	w.mu.Lock()
	var (
		path  string
		kind  observer.TargetKind
		found bool
	)
	for p, a := range w.armed {
		if a.fd == fd {
			path, kind, found = p, a.kind, true
			break
		}
	}
	if !found {
		// Released between the kernel queueing the event and us reading it.
		w.mu.Unlock()
		return
	}
	at := observer.TruncateTime(time.Now())
	w.mu.Unlock()

	c := observer.Change{Kind: kind, Ref: path, At: at}
	select {
	case w.hints <- c:
		w.mu.Lock()
		w.events++
		w.lastEventAt = at
		w.mu.Unlock()
	default:
		w.mu.Lock()
		w.dropped++
		w.mu.Unlock()
	}
}

// Reconcile implements observer.Watcher.
//
// The order inside it is load-bearing: RELEASE first, then arm. A target that
// left the desired set may share a path with one that is arriving (a renamed
// registry file), and releasing after arming would close the descriptor just
// opened.
//
// There is one `defer`-free close path, per golang.md's "Defer in Hot Loops":
// with 65 targets, defers inside the loop would accumulate until the function
// returned.
func (w *Watcher) Reconcile(ctx context.Context, targets []observer.WatchTarget) (observer.ArmReport, error) {
	if err := ctx.Err(); err != nil {
		return observer.ArmReport{}, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return observer.ArmReport{Degraded: observer.DegradedKqueueUnavailable}, ErrUnsupported
	}

	accepted, degraded := prioritise(targets, w.caps)
	want := make(map[string]observer.TargetKind, len(accepted))
	for _, t := range accepted {
		want[t.Kind.String()+"\x00"+t.Path] = t.Kind
	}

	rep := observer.ArmReport{ByKind: map[observer.TargetKind]int{}, Degraded: degraded}
	for _, t := range accepted {
		rep.ByKind[t.Kind]++
	}

	// 1. Release what is gone. Closing here is correct precisely because the
	//    target is NOT in the desired set: nothing will read it again, so the
	//    events E5 says we are discarding are events nobody wanted.
	for path, a := range w.armed {
		if _, still := want[a.kind.String()+"\x00"+path]; still {
			continue
		}
		_ = unix.Close(a.fd)
		delete(w.armed, path)
		rep.Released++
	}

	// 2. Arm what is new — and LEAVE UNTOUCHED what is already armed with the
	//    same identity. This is the E5 half of §6.4's rule: a close-then-open
	//    of an unchanged target would discard whatever the kernel had queued on
	//    it, which is a data-loss bug dressed as hygiene.
	for _, t := range accepted {
		if cur, held := w.armed[t.Path]; held {
			if cur.kind == t.Kind && w.sameIdentity(cur, t.Path) {
				continue
			}
			// The path's identity changed underneath the descriptor — the jobs
			// tree's replace-publish, or a renamed registry file. The old
			// descriptor is deaf from here on, so this is the OTHER half of the
			// rule: close then open, in that order, always.
			_ = unix.Close(cur.fd)
			delete(w.armed, t.Path)
		}
		w.armGen++
		a, err := arm(w.kq, t, w.armGen)
		if err != nil {
			rep.Failed++
			rep.Failures = append(rep.Failures, observer.ArmFailure{Path: t.Path, Error: err.Error()})
			continue
		}
		w.armed[t.Path] = a
		rep.Armed++
	}
	return rep, nil
}

// Release implements observer.Watcher: close every held target not in keep,
// and open NOTHING — not even a replacement for a kept target whose vnode
// changed. That re-arm is Reconcile's, and it happens in the next rebuild's
// identity pass, before that rebuild's read (code review round 2, A4).
func (w *Watcher) Release(ctx context.Context, keep []observer.WatchTarget) (observer.ArmReport, error) {
	if err := ctx.Err(); err != nil {
		return observer.ArmReport{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return observer.ArmReport{Degraded: observer.DegradedKqueueUnavailable}, ErrUnsupported
	}

	want := make(map[string]bool, len(keep))
	for _, t := range keep {
		want[t.Kind.String()+"\x00"+t.Path] = true
	}
	rep := observer.ArmReport{ByKind: map[observer.TargetKind]int{}}
	for path, a := range w.armed {
		if want[a.kind.String()+"\x00"+path] {
			rep.ByKind[a.kind]++
			continue
		}
		_ = unix.Close(a.fd)
		delete(w.armed, path)
		rep.Released++
	}
	return rep, nil
}

// sameIdentity reports whether the descriptor still refers to what the path
// names now. A stat failure counts as "changed", which routes the target
// through the re-arm path and produces an honest ArmFailure if it is really
// gone.
func (w *Watcher) sameIdentity(a watch, path string) bool {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false
	}
	return uint64(st.Dev) == a.dev && st.Ino == a.ino
}

// arm opens and registers one target. The open flags are the whole of the
// write-safety argument: O_RDONLY|O_EVTONLY|O_CLOEXEC is the flag set §9.4a's
// audit recognises as provably read-only, so no reviewer has to take this
// file's word for it.
func arm(kq int, t observer.WatchTarget, gen int64) (watch, error) {
	fd, err := unix.Open(t.Path, unix.O_RDONLY|unix.O_EVTONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return watch{}, fmt.Errorf("open %s: %w", t.Path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return watch{}, fmt.Errorf("fstat %s: %w", t.Path, err)
	}
	ev := unix.Kevent_t{
		Ident:  uint64(fd),
		Filter: unix.EVFILT_VNODE,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
		Fflags: fflagsFor(t.Kind),
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		_ = unix.Close(fd)
		return watch{}, fmt.Errorf("kevent add %s: %w", t.Path, err)
	}
	return watch{fd: fd, kind: t.Kind, gen: gen, dev: uint64(st.Dev), ino: st.Ino}, nil
}

// fflagsFor is §6.3's per-family event set.
//
// Directories ask for WRITE (their entry list changed) plus DELETE|RENAME (the
// directory itself went away, which is the only condition that releases a job
// directory watch).
//
// Files ask for WRITE|EXTEND|ATTRIB|DELETE|RENAME, because the registry is
// rewritten IN PLACE: a directory vnode receives nothing on an in-place write,
// which is the failure mode that passes a naive create-a-file test.
//
// Transcripts ask for WRITE|EXTEND only, which is what an append is.
func fflagsFor(kind observer.TargetKind) uint32 {
	switch kind {
	case observer.TargetClaudeRoot, observer.TargetRegistryDir, observer.TargetJobsRoot, observer.TargetJobDir:
		return unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME
	case observer.TargetJobTranscript:
		return unix.NOTE_WRITE | unix.NOTE_EXTEND
	default:
		return unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB | unix.NOTE_DELETE | unix.NOTE_RENAME
	}
}

// Close releases every descriptor, wakes the kevent loop and WAITS for it to
// stop. It is idempotent.
//
// Code review round 2, G1. The wake-up used to be posted AFTER w.mu was
// released, while the loop's markDead closes the kqueue under that same mutex:
// a loop that had exited on its own (any kevent() error) could close the kqueue
// in between, and Close then called kevent() on a closed — or, in a busy
// process, REUSED — descriptor. And Close returned as soon as it had posted,
// so "closed" did not mean the reader had stopped.
//
// Now the wake-up is posted while the mutex is held. markDead cannot run in
// that interval, and it is the only thing that closes the kqueue, so the
// descriptor is valid for the whole call; `closed` observed false under the
// mutex proves markDead has not run yet. Then Close waits on done, which
// markDead closes after it has released the kqueue and closed the hint
// channel. The loop never blocks on anything Close holds — deliver() takes the
// mutex only between kevent() calls, and its channel send has a default — so
// the wait is bounded by one kevent() wake-up.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	for path, a := range w.armed {
		_ = unix.Close(a.fd)
		delete(w.armed, path)
	}

	if !w.started {
		// No loop was ever started, so nothing else can touch the kqueue.
		err := unix.Close(w.kq)
		w.kq = -1
		w.mu.Unlock()
		return err
	}

	// Wake the blocked kevent() so the loop can exit, close the hint channel
	// and close the kqueue itself — from the one goroutine that was using it.
	trigger := unix.Kevent_t{
		Ident:  wakeIdent,
		Filter: unix.EVFILT_USER,
		Flags:  unix.EV_ENABLE,
		Fflags: unix.NOTE_TRIGGER,
	}
	_, err := unix.Kevent(w.kq, []unix.Kevent_t{trigger}, nil, nil)
	w.mu.Unlock()
	if err != nil {
		// The loop was not woken, so waiting for it could be forever. The
		// descriptor stays with the loop, whose own exit path closes it.
		return err
	}
	<-w.done
	return nil
}

func (w *Watcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// FDs returns the currently armed paths and their descriptor numbers.
//
// It exists for one test and says so: §0.5 E5 makes a gratuitous close-and-
// reopen a data-loss bug, and comparing fd NUMBERS across two rebuilds is the
// only way to notice one. Nothing in the product calls it.
func (w *Watcher) FDs() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.armed))
	for p, a := range w.armed {
		out[p] = a.fd
	}
	return out
}

// Generations returns the arming generation of every armed target. Two calls
// either side of a Reconcile that should have changed nothing must return the
// same numbers; a changed number is a close-and-reopen, which E5 measured
// discards every queued event.
func (w *Watcher) Generations() map[string]int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int64, len(w.armed))
	for p, a := range w.armed {
		out[p] = a.gen
	}
	return out
}

// Counters reports delivered and dropped hints and the last event instant, for
// sources.<id>.watch.
func (w *Watcher) Counters() (events, dropped int64, lastEventAt time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.events, w.dropped, w.lastEventAt
}

var _ observer.Watcher = (*Watcher)(nil)
