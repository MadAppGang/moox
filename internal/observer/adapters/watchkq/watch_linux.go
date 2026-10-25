//go:build linux

package watchkq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/MadAppGang/moox/internal/observer"
)

// The inotify implementation of observer.Watcher — the same contract as
// watch_darwin.go, over a different kernel mechanism. The package keeps its
// kqueue name because the port, not the mechanism, is what callers depend on.
//
// Where the two differ, and why it does not show through the port:
//
//   - inotify watches an INODE by PATH through one descriptor, so an armed
//     target costs a watch descriptor (wd) inside the one inotify fd rather than
//     an open fd of its own. FDs() reports wds; nothing in the product reads
//     them, and the E5 tests only compare them across rebuilds — wds are handed
//     out increasingly and are not reused while the inotify fd lives, so an
//     unchanged number still means "untouched".
//   - Two paths naming one inode share one wd. The kernel would silently
//     REPLACE the first path's event mask with the second's, and removing either
//     would deafen both, so every add uses IN_MASK_ADD and a wd is only removed
//     when the last path holding it goes.
//   - A watch the kernel drops on its own (the inode was deleted) arrives as
//     IN_IGNORED. The target is marked dead so the next Reconcile re-arms it
//     even if a new file happens to reuse the old inode number — identity by
//     (dev, ino) alone could not tell.
//   - IN_Q_OVERFLOW means events were lost. That is still only "something
//     changed", and a rebuild reads the whole tree, so it becomes one hint.

// hintBuffer is how many coalesced hints may be in flight before the read loop
// starts dropping them. A drop is sound because a hint carries no information
// beyond "something changed": while the channel is non-empty a rebuild is
// already coming, and a rebuild reads the whole tree.
const hintBuffer = 64

// watch is one armed target.
type watch struct {
	wd   int
	kind observer.TargetKind
	// gen is the process-wide arming generation this target was armed at, as on
	// darwin: the E5 tests compare it across rebuilds.
	gen int64
	// dev and ino are the identity the path had when it was armed. A
	// rename-publish of a job record leaves the path in place and changes the
	// inode underneath it, and a watch on the old inode is deaf from that moment
	// on — so identity, not path, is what decides whether a re-arm is owed.
	dev uint64
	ino uint64
	// dead is set when the kernel reported IN_IGNORED for this wd: the watch is
	// gone even if the path's identity looks unchanged.
	dead bool
}

// Watcher is the inotify implementation of observer.Watcher.
type Watcher struct {
	caps Caps

	mu sync.Mutex
	// fd is the raw inotify descriptor, used for inotify_add_watch and
	// inotify_rm_watch under mu. file wraps the same descriptor for reading:
	// it was created non-blocking, so os.NewFile registers it with the runtime
	// poller, the read loop parks there with no timer, and Close wakes it by
	// closing the file. file.Fd() is never called — it would put the
	// descriptor back into blocking mode and lose exactly that wake-up.
	fd       int
	file     *os.File
	fdClosed bool
	armed    map[string]watch
	// byWD is every path holding a wd, so an event can be attributed and a wd
	// shared by two paths is only removed with the last of them.
	byWD   map[int]map[string]struct{}
	closed bool
	armGen int64

	hints   chan observer.Change
	started bool
	// done is closed by markDead once the read loop has stopped and the inotify
	// descriptor is released. Close waits on it, so "closed" means the reader
	// has stopped rather than merely been asked to.
	done chan struct{}

	events      int64
	dropped     int64
	lastEventAt time.Time
}

// DefaultCaps is §6.4's cap set for this process: min(1024, a quarter of the
// user's inotify watch budget) per-file watches, 64 job directories, 64 job
// transcripts. The budget is per user and shared with every other inotify
// client on the machine (editors, file managers), which is why only a quarter
// of it is claimed — the same margin darwin leaves of RLIMIT_NOFILE.
func DefaultCaps() Caps {
	caps := Caps{MaxFiles: MaxFilesCeiling, MaxJobDirs: MaxJobDirs, MaxJobTranscripts: MaxJobTranscripts}
	if b, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
			if quarter := n / 4; quarter < caps.MaxFiles {
				caps.MaxFiles = quarter
			}
		}
	}
	return caps
}

// New creates the inotify instance. A failure here is ErrUnsupported-wrapped
// rather than fatal: §6.6 requires the observer to keep answering on-demand
// when it cannot push, so the caller's job is to record watch.mode =
// "on-demand", not to exit. The usual cause is the per-user instance limit
// (fs.inotify.max_user_instances).
func New(caps Caps) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("%w: inotify_init1: %v", ErrUnsupported, err)
	}
	file := os.NewFile(uintptr(fd), "inotify")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: inotify: could not wrap descriptor", ErrUnsupported)
	}
	return &Watcher{
		caps:  caps.withDefaults(),
		fd:    fd,
		file:  file,
		armed: map[string]watch{},
		byWD:  map[int]map[string]struct{}{},
		hints: make(chan observer.Change, hintBuffer),
		done:  make(chan struct{}),
	}, nil
}

// Watch implements observer.Watcher. It starts the read loop on first call and
// returns the same channel on any later one, so a caller that re-Watches after
// a transient failure does not end up with two loops on one descriptor.
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

// loop is the only goroutine that reads the inotify descriptor. The read parks
// in the runtime poller — no timer, no poll interval — and fails once Close has
// closed the file, which is the loop's way out.
func (w *Watcher) loop() {
	defer w.markDead()

	// Room for many events per read; each is a header plus a NUL-padded name.
	buf := make([]byte, hintBuffer*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := w.file.Read(buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		w.deliver(buf[:n])
		if w.isClosed() {
			return
		}
	}
}

// markDead is the loop's one exit path, and it is what makes a dead watch SAY
// it is dead — the reasoning is watch_darwin.go's markDead, unchanged: set
// closed so Watch reports ErrUnsupported instead of handing back a closed
// channel, and release what was armed, because Close returns early once closed
// is set.
func (w *Watcher) markDead() {
	w.mu.Lock()
	w.closed = true
	w.armed = map[string]watch{}
	w.byWD = map[int]map[string]struct{}{}
	if !w.fdClosed {
		// Closing the inotify descriptor drops every watch it holds.
		_ = w.file.Close()
		w.fdClosed = true
	}
	w.mu.Unlock()
	close(w.hints)
	close(w.done)
}

// deliver turns one read's worth of kernel events into coarse hints: at most
// one per armed path per read. The event mask is deliberately NOT forwarded —
// the port promises only "something changed".
func (w *Watcher) deliver(buf []byte) {
	type hinted struct {
		kind observer.TargetKind
		path string
	}
	var out []hinted
	seen := map[string]bool{}

	w.mu.Lock()
	for off := 0; off+unix.SizeofInotifyEvent <= len(buf); {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		off += unix.SizeofInotifyEvent + int(ev.Len)

		if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
			// Events were lost, and with them any sense of which target. One
			// hint is enough: the rebuild it triggers reads everything.
			if !seen[""] {
				seen[""] = true
				out = append(out, hinted{kind: observer.TargetRegistryDir})
			}
			continue
		}
		wd := int(ev.Wd)
		for path := range w.byWD[wd] {
			a := w.armed[path]
			if ev.Mask&unix.IN_IGNORED != 0 {
				a.dead = true
				w.armed[path] = a
			}
			if !seen[path] {
				seen[path] = true
				out = append(out, hinted{kind: a.kind, path: path})
			}
		}
		if ev.Mask&unix.IN_IGNORED != 0 {
			// The kernel has already dropped the wd; forget it so a later
			// release does not remove a wd number the kernel may reissue.
			delete(w.byWD, wd)
		}
	}
	at := observer.TruncateTime(time.Now())
	w.mu.Unlock()

	for _, h := range out {
		c := observer.Change{Kind: h.kind, Ref: h.path, At: at}
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
}

// Reconcile implements observer.Watcher. RELEASE first, then arm, for the
// reason watch_darwin.go gives: a target leaving the desired set may share a
// path with one arriving, and releasing after arming would drop the new watch.
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

	// 1. Release what is gone.
	for path, a := range w.armed {
		if _, still := want[a.kind.String()+"\x00"+path]; still {
			continue
		}
		w.drop(path, a)
		rep.Released++
	}

	// 2. Arm what is new — and LEAVE UNTOUCHED what is already armed with the
	//    same identity (E5: dropping and re-adding a live watch discards what
	//    the kernel had queued on it).
	for _, t := range accepted {
		if cur, held := w.armed[t.Path]; held {
			if cur.kind == t.Kind && !cur.dead && sameIdentity(cur, t.Path) {
				continue
			}
			// The path's identity changed underneath the watch, or the kernel
			// dropped it. Drop then add, in that order, always.
			w.drop(t.Path, cur)
		}
		w.armGen++
		a, err := w.arm(t, w.armGen)
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

// Release implements observer.Watcher: drop every held target not in keep,
// and add NOTHING — not even a replacement for a kept target whose inode
// changed. That re-arm is Reconcile's.
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
		w.drop(path, a)
		rep.Released++
	}
	return rep, nil
}

// drop forgets one armed path, and removes its wd from the kernel only when no
// other path still holds it. Called with mu held.
func (w *Watcher) drop(path string, a watch) {
	delete(w.armed, path)
	holders, ok := w.byWD[a.wd]
	if !ok {
		// Already dropped by the kernel (IN_IGNORED).
		return
	}
	delete(holders, path)
	if len(holders) == 0 {
		delete(w.byWD, a.wd)
		_, _ = unix.InotifyRmWatch(w.fd, uint32(a.wd))
	}
}

// sameIdentity reports whether the watch still refers to what the path names
// now. A stat failure counts as "changed", which routes the target through the
// re-arm path and produces an honest ArmFailure if it is really gone.
func sameIdentity(a watch, path string) bool {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false
	}
	return st.Dev == a.dev && st.Ino == a.ino
}

// arm adds one target. The identity is read BEFORE the watch is added: if the
// path is replaced in between, the recorded identity is the older one, the next
// Reconcile sees a mismatch and re-arms — a spare re-arm, never a deaf watch
// recorded as current. inotify opens nothing and writes nothing; the mask below
// is the whole of what is asked of the kernel.
func (w *Watcher) arm(t observer.WatchTarget, gen int64) (watch, error) {
	var st unix.Stat_t
	if err := unix.Stat(t.Path, &st); err != nil {
		return watch{}, fmt.Errorf("stat %s: %w", t.Path, err)
	}
	wd, err := unix.InotifyAddWatch(w.fd, t.Path, maskFor(t.Kind)|unix.IN_MASK_ADD)
	if err != nil {
		return watch{}, fmt.Errorf("inotify_add_watch %s: %w", t.Path, err)
	}
	if w.byWD[wd] == nil {
		w.byWD[wd] = map[string]struct{}{}
	}
	w.byWD[wd][t.Path] = struct{}{}
	return watch{wd: wd, kind: t.Kind, gen: gen, dev: st.Dev, ino: st.Ino}, nil
}

// maskFor is §6.3's per-family event set, in inotify's terms.
//
// Directories ask for entry-list changes (create, delete, either side of a
// rename) plus the directory itself going away. NOT IN_MODIFY: that would make
// a directory watch hear in-place writes to the files inside it, which kqueue
// cannot, and the core's per-file arming is built on directories staying deaf
// to them (AC10 case 2).
//
// Files ask for writes, attribute changes and the file itself going away,
// because the registry is rewritten IN PLACE.
//
// Transcripts ask for writes only, which is what an append is.
func maskFor(kind observer.TargetKind) uint32 {
	switch kind {
	case observer.TargetClaudeRoot, observer.TargetRegistryDir, observer.TargetJobsRoot, observer.TargetJobDir:
		return unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
			unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
	case observer.TargetJobTranscript:
		return unix.IN_MODIFY
	default:
		return unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
	}
}

// Close releases every watch, wakes the read loop and WAITS for it to stop. It
// is idempotent.
//
// The file is closed while mu is held: markDead cannot run in that interval,
// and Reconcile — the only other user of the raw descriptor — holds mu too, so
// no inotify_add_watch can land on a descriptor number the process has already
// reused.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.armed = map[string]watch{}
	w.byWD = map[int]map[string]struct{}{}
	err := w.file.Close()
	w.fdClosed = true
	started := w.started
	w.mu.Unlock()

	if started {
		// The read fails once the file is closed; markDead then closes the hint
		// channel and done.
		<-w.done
	}
	return err
}

func (w *Watcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// FDs returns the currently armed paths and their watch descriptors. It exists
// for the E5 tests, as on darwin; nothing in the product calls it.
func (w *Watcher) FDs() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.armed))
	for p, a := range w.armed {
		out[p] = a.wd
	}
	return out
}

// Generations returns the arming generation of every armed target.
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
