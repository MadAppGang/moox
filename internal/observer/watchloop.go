package observer

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// This file is §6.3's arm-before-read protocol and §7.2's one serialized
// refresh-and-publish path.
//
// # The whole of it, in one invariant
//
//	INVARIANT A — no byte is read from any path whose containing watched vnode
//	was not already armed at the instant the read began.
//
// That is a CORRECTNESS property, not a style. kqueue does not replay vnode
// events that predate registration (§0.5 E2, measured: zero events), and the
// jobs root cannot recover the loss because state.json is its GRANDCHILD
// (§0.5 E4, measured: zero events). So a record created — or a terminal record
// replaced — after its directory was scanned and before that directory's vnode
// was armed is lost PERMANENTLY: there is no queued event, and the read has
// already happened.
//
// Why arming first is sufficient, as a two-case proof rather than an argument.
// For a mutation at m, an arm at a and a read at r, Invariant A gives a < r:
//
//	m < a : the read at r > a > m observes the mutation's result directly.
//	        Reading a file is not notification-dependent.
//	a < m : the kernel QUEUES the event (§0.5 E1 measured that post-registration
//	        events are retained even before the first kevent() call) and the next
//	        rebuild observes it.
//
// There is no third case. Reverse the order and r < m < a appears, which is
// unrecoverable — and that third case is exactly what round 3's CRITICAL 1 was.
//
// # Where the code puts it
//
// Rebuild runs three ARM-ONLY waves, each immediately before the read it
// protects, then an IDENTITY pass that re-arms any held target whose vnode was
// replaced (still before the read), and exactly ONE releasing call — the port's
// Release, which cannot arm — after the publish. Releases go last because
// releasing a target we are about to re-read would reopen the window we just
// closed — and because closing a descriptor discards every event the kernel
// had queued on it (§0.5 E5).
//
// An AST test asserts that ordering against this file's syntax tree, because
// the defect it prevents is one `Reconcile` call moved by one refactor.

// Degraded values for ArmReport.Degraded, named so a test and a report cannot
// disagree about the spelling.
const (
	DegradedFileWatchCap       = "file-watch-cap"
	DegradedJobDirCap          = "job-dir-cap"
	DegradedKqueueUnavailable  = "kqueue-unavailable"
	DegradedArmScanUnconverged = "arm-scan-unconverged"
)

// Watch modes for WatchReport.Mode.
const (
	WatchModeKqueue    = "kqueue"
	WatchModeOnDemand  = "on-demand"
	WatchModeAbsent    = "absent"
	WatchModeViaSocket = "via-socket"
)

// DefaultMaxReconcileIterations bounds §6.3's fixed point.
//
// The bound is honest rather than arbitrary. The loop is not a spin: the set it
// converges on is the set of directories that existed BEFORE wave 0 armed the
// jobs root, which is finite and cannot grow. Any directory created after wave
// 0 fires the root watch and is therefore already queued as a hint for the next
// rebuild whether this loop catches it or not. Steady state: 1 iteration. First
// rebuild of a process: 2 — one that arms, one that confirms.
//
// A bound BELOW 2 can never report convergence, because convergence is
// established by a scan that follows an arm. That is a property of the bound
// rather than a failure mode: a test injecting a cap to reach the degraded path
// must inject 2 or more.
const DefaultMaxReconcileIterations = 4

// maxConsecutiveRearms is how many times Run will re-arm a hint channel that
// closed without ever delivering a hint. One: the second consecutive failure is
// a watcher that cannot be revived, and the §6.6 on-demand answer is correct
// and free while a re-arm loop is neither.
const maxConsecutiveRearms = 1

// DefaultCoalesceWindow is how long a burst of hints is allowed to accumulate
// into one rebuild. The measured 13-seconds-apart double rewrite of one
// registry record collapses into one rebuild at this width.
const DefaultCoalesceWindow = 250 * time.Millisecond

// CorroborationKeepAlive is how often the loop re-reads the agents listing
// while a background job's liveness rests on it. Under the 30 s StaleAfterMs
// with room for the spawn, so the listing never ages past the line between
// two re-reads: without it a blocked job crossed rule 6 -> 6b on the clock
// alone and its row jumped between NEEDS YOU and UNCLEAR every time the
// machine went quiet (the user, 2026-10-21).
const CorroborationKeepAlive = 20 * time.Second

// Refresher is the explicit corroboration re-read. *Observer implements it.
type Refresher interface {
	RefreshSources(ctx context.Context, sources []string) RefreshOutcome
}

// DefaultSubscriberBuffer is how many events a subscriber may fall behind
// before it is resynced rather than waited for. The publisher NEVER blocks on a
// subscriber (§7.2): a slow consumer must not be able to delay the rebuild loop
// that every other consumer depends on.
const DefaultSubscriberBuffer = 64

// jobRecordName is the one file name that is a job record. M13: a job record is
// exactly ~/.claude/jobs/<shortId>/state.json, at depth 1, and nothing deeper is
// ever read as a record or armed as a target.
const jobRecordName = "state.json"

// Snapshotter is the narrow half of SessionSource this loop needs. It is
// consumer-defined on purpose: *Observer does not implement all four
// SessionSource methods until P6, and a loop that demanded the whole interface
// could not be wired at all today — nor could it be given a test double without
// three methods that lie.
type Snapshotter interface {
	Snapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error)
}

// PathLister is the enumeration the protocol needs: which immediate
// subdirectories a directory has, which of its files carry a suffix, and
// whether a path exists.
//
// It is a port rather than an os call in this file for the reason every other
// port exists — the core must not know what a filesystem is — and for one
// specific one: wave 1's fixed point is the hardest thing here to test, and a
// test needs to make the enumeration disagree with the armed set on demand.
//
// An absent directory is (nil, nil). §6.6 requires "Claude has never run here"
// to read as a fact rather than an error.
type PathLister interface {
	ListDirs(ctx context.Context, dir string) ([]string, error)
	ListFiles(ctx context.Context, dir, suffix string) ([]string, error)
	Exists(ctx context.Context, path string) (bool, error)
}

// Timer is a one-shot timer. TimerSource hands them out.
//
// The seam exists for AC9 and nothing else: "the idle steady state has ZERO
// armed timers" is a claim about arming discipline, and an AST allowlist cannot
// prove it. A test supplies a TimerSource that keeps an arm/disarm ledger and
// asserts the ledger reads zero over an idle interval.
type Timer interface {
	Stop() bool
	C() <-chan time.Time
}

// TimerSource creates one-shot timers.
type TimerSource interface {
	NewTimer(d time.Duration) Timer
}

// SystemTimers is the production TimerSource: one-shot time.Timers.
//
// time.NewTimer rather than time.After deliberately — §9.3a bans After and
// Sleep because they arm something nobody can then disarm, while a one-shot
// timer armed in response to an event and stopped on use satisfies all four
// clauses of the timer rule.
type SystemTimers struct{}

// NewTimer arms a one-shot timer.
func (SystemTimers) NewTimer(d time.Duration) Timer { return &systemTimer{t: time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (s *systemTimer) Stop() bool          { return s.t.Stop() }
func (s *systemTimer) C() <-chan time.Time { return s.t.C }

// WatchLoopConfig is everything the loop needs. Core, Watch, Tree and Clock are
// required; Jobs is optional and its absence simply means no background target
// families are armed, which is the correct behaviour on a machine that has
// never run a background agent.
type WatchLoopConfig struct {
	Core  Snapshotter
	Watch Watcher
	Tree  PathLister
	Jobs  JobsSource
	Clock Clock
	Paths Paths

	// Refresh, when set, keeps the corroboration listing fresh while the
	// published snapshot holds a background job whose verdict depends on it
	// (CorroborationKeepAlive). Nil arms nothing.
	Refresh Refresher

	// Timers defaults to SystemTimers.
	Timers TimerSource
	// CoalesceWindow defaults to DefaultCoalesceWindow.
	CoalesceWindow time.Duration
	// MaxReconcileIterations defaults to DefaultMaxReconcileIterations.
	MaxReconcileIterations int
	// SubscriberBuffer defaults to DefaultSubscriberBuffer.
	SubscriberBuffer int
}

// WatchLoop owns the watch schedule, the diff and the subscriber fan-out.
//
// Every state change enters ONE path: Rebuild, serialized by rebuildMu. Two
// concurrent rebuilds could read different instants and publish out of order,
// leaving seq numerically monotonic while causal ordering was violated — and
// the race detector cannot see any of that, which is why the serialization is
// structural rather than a convention.
type WatchLoop struct {
	core    Snapshotter
	refresh Refresher
	watch   Watcher
	tree    PathLister
	jobs    JobsSource
	clock   Clock
	paths   Paths
	timers  TimerSource

	coalesceWindow time.Duration
	maxIterations  int
	subBuffer      int

	// rebuildMu serializes rebuilds. It is a separate lock from mu because a
	// rebuild takes hundreds of microseconds and reads the filesystem, and
	// holding the state lock across that would block every Latest() reader.
	rebuildMu sync.Mutex

	mu sync.Mutex
	// armed is the loop's model of what the adapter holds. It is kept here as
	// well as in the adapter because wave 1's fixed point compares the
	// enumeration against the ARMED set, and asking the adapter would make the
	// port answer questions about its own internals.
	armed map[string]WatchTarget
	// failedArms are targets this rebuild could not arm. They are excluded from
	// the fixed point's convergence test — otherwise a job directory that
	// disappeared between the scan and the arm would spin the loop to its cap
	// and report arm-scan-unconverged, which is the wrong diagnosis.
	failedArms map[string]bool

	subs    map[int64]*subscriber
	nextSub int64

	seq int64
	// degraded is set once, by Run, when the watch cannot push — it failed to
	// start, or it died. It is NOT the same as mode == on-demand, which is
	// also the state of a loop that has not been started yet: only a degraded
	// loop turns every pull into a rebuild (code review round 2, A8).
	degraded    bool
	lastArm     ArmReport
	lastErr     error
	lastTrigger string
	mode        string
	rebuilds    int64
	hints       int64
	lastHintAt  *time.Time
	unconverged []string
	// changedTranscripts accumulates the job transcripts whose watch fired
	// since the last rebuild took them — across the whole coalescing window,
	// which is what a burst of appends collapses into (code review round 2,
	// A3). The rebuild takes the set at its START, so a hint arriving during
	// the rebuild is kept for the next one rather than lost.
	changedTranscripts map[string]bool

	published atomic.Pointer[Snapshot]
}

// NewWatchLoop validates the ports this loop dereferences and returns it
// unstarted. A nil port is a programming error rather than a degraded mode: the
// degraded modes live inside the adapters, so "this machine has no kqueue" is a
// value in the snapshot instead of a nil check in here.
func NewWatchLoop(cfg WatchLoopConfig) (*WatchLoop, error) {
	switch {
	case cfg.Core == nil:
		return nil, errMissing("Snapshotter")
	case cfg.Watch == nil:
		return nil, errMissing("Watcher")
	case cfg.Tree == nil:
		return nil, errMissing("PathLister")
	case cfg.Clock == nil:
		return nil, errMissing("Clock")
	}

	l := &WatchLoop{
		core:           cfg.Core,
		refresh:        cfg.Refresh,
		watch:          cfg.Watch,
		tree:           cfg.Tree,
		jobs:           cfg.Jobs,
		clock:          cfg.Clock,
		paths:          cfg.Paths,
		timers:         cfg.Timers,
		coalesceWindow: cfg.CoalesceWindow,
		maxIterations:  cfg.MaxReconcileIterations,
		subBuffer:      cfg.SubscriberBuffer,
		armed:          map[string]WatchTarget{},
		failedArms:     map[string]bool{},
		subs:           map[int64]*subscriber{},
		mode:           WatchModeOnDemand,
	}
	if l.timers == nil {
		l.timers = SystemTimers{}
	}
	if l.coalesceWindow <= 0 {
		l.coalesceWindow = DefaultCoalesceWindow
	}
	if l.maxIterations <= 0 {
		l.maxIterations = DefaultMaxReconcileIterations
	}
	if l.subBuffer <= 0 {
		l.subBuffer = DefaultSubscriberBuffer
	}
	return l, nil
}

func errMissing(what string) error {
	return &missingPortError{what: what}
}

type missingPortError struct{ what string }

func (e *missingPortError) Error() string { return "observer: missing port: " + e.what }
func (e *missingPortError) Unwrap() error { return ErrMissingPort }

// Run arms the watch, publishes once, and then does nothing at all until
// something happens.
//
// "Nothing at all" is literal and is AC9: the select below has no default and
// no ticker, the coalescing timer is armed only while a burst is in flight, and
// there is no idle timeout of any kind (the user's override). An idle server
// blocks in select with ZERO armed timers and consumes no CPU, whether or not
// clients are attached.
//
// It returns nil on a clean context cancellation — a shutdown is not a failure,
// and a caller that had to distinguish context.Canceled from a real error would
// grow a check every surface has to get right.
func (l *WatchLoop) Run(ctx context.Context) error {
	// degrade is §6.6's one transition, written once: stop claiming to push,
	// tell every subscriber why, and then block until shutdown. It is a
	// function rather than three repeated lines because the mid-run death now
	// has four ways to be reached, and a death that forgot setMode is exactly
	// the defect this loop shipped — a watch that was dead while Mode() went on
	// reporting "kqueue". (The startup failure below stays spelled out: it
	// rebuilds BETWEEN setMode and the notification, so that the snapshot it
	// publishes already says "on-demand".)
	//
	// Code review round 2, A8: it now also REPUBLISHES, through the on-demand
	// rebuild, before telling anyone. A watch that died mid-run used to leave
	// the published snapshot — and every pull answered from it — describing a
	// pushing watch and the state of the moment it died, forever.
	degrade := func(reason string) {
		l.markDegraded()
		_, _, _ = l.Rebuild(ctx, "watch-degraded")
		l.notifyWatchDegraded(reason)
		<-ctx.Done()
	}

	hints, err := l.watch.Watch(ctx)
	if err != nil {
		// §6.6: the observer still answers every query correctly by re-reading
		// all sources. It simply cannot push.
		l.markDegraded()
		_, _, _ = l.Rebuild(ctx, "startup")
		l.notifyWatchDegraded(err.Error())
		<-ctx.Done()
		return nil
	}

	l.setMode(WatchModeKqueue)
	_, _, _ = l.Rebuild(ctx, "startup")

	var (
		coalesce Timer
		fire     <-chan time.Time
		// keep is the corroboration keep-alive: armed only while the latest
		// snapshot needs it, so the idle steady state still arms nothing.
		keep     Timer
		keepFire <-chan time.Time
		// rearms counts re-arms since the last hint actually arrived. A watch
		// that delivers is a watch that works, so this resets on every hint and
		// bounds only CONSECUTIVE re-arms that made no progress.
		rearms int
	)
	stop := func() {
		if coalesce != nil {
			coalesce.Stop()
			coalesce, fire = nil, nil
		}
	}
	defer stop()
	rearmKeep := func() {
		if keep == nil && l.refresh != nil && needsCorroboration(l.Latest()) {
			keep = l.timers.NewTimer(CorroborationKeepAlive)
			keepFire = keep.C()
		}
	}
	defer func() {
		if keep != nil {
			keep.Stop()
		}
	}()
	rearmKeep()

	for {
		select {
		case <-ctx.Done():
			return nil

		case c, ok := <-hints:
			if !ok {
				// The whole watch died mid-run: ONE re-arm attempt, and
				// anything short of a live, different channel is a death.
				//
				// The `next == hints` clause is the one that matters, and it is
				// not defensive programming. A watcher whose loop has died can
				// hand its own CLOSED channel straight back with a nil error —
				// the kqueue adapter did exactly that before markDead existed,
				// and sockserve's conn.go gets the same case right by nilling
				// the channel so the select blocks. Assigning it here re-selects
				// on a closed channel immediately, which measured 540,844
				// Watch() calls in 500 ms: one core pegged forever inside a
				// server that has no idle exit, no timer and no supervision,
				// while the health surface reported a healthy push watch.
				//
				// rearms bounds the same shape reached any other way — a fresh
				// channel that is already closed, or a watcher that re-arms and
				// dies again with nothing delivered in between.
				stop()
				next, rerr := l.watch.Watch(ctx)
				switch {
				case rerr != nil:
					degrade(rerr.Error())
					return nil
				case next == nil:
					degrade("the watch re-armed to a nil channel")
					return nil
				case next == hints:
					degrade("the watch handed back the same closed hint channel: it is dead")
					return nil
				case rearms >= maxConsecutiveRearms:
					degrade("the watch died again without delivering a hint")
					return nil
				}
				rearms++
				hints = next
				continue
			}
			rearms = 0
			l.noteHint(c)
			if coalesce == nil {
				coalesce = l.timers.NewTimer(l.coalesceWindow)
				fire = coalesce.C()
			}

		case <-fire:
			// Disarm before rebuilding, so that nothing re-arms the timer while
			// idle: the next arming needs another event.
			coalesce, fire = nil, nil
			_, _, _ = l.Rebuild(ctx, "watch")
			rearmKeep()

		case <-keepFire:
			keep, keepFire = nil, nil
			_ = l.refresh.RefreshSources(ctx, []string{SourceAgentsCLI})
			_, _, _ = l.Rebuild(ctx, "corroboration")
			rearmKeep()
		}
	}
}

// needsCorroboration reports whether a background job's verdict in snap rests
// on the agents listing's age: corroborated now (rule 6), or uncorroborated
// only because the listing aged (6b). A snapshot with neither needs no timer.
func needsCorroboration(snap *Snapshot) bool {
	if snap == nil {
		return false
	}
	for _, s := range snap.Sessions {
		if s.Kind != KindBackground {
			continue
		}
		switch s.LiveReason {
		case LiveReasonJobNonTerminalCorroborated, LiveReasonJobNonTerminalUncorroborated:
			return true
		}
	}
	return false
}

// Rebuild runs one whole cycle of the protocol. It is exported because a pull
// surface (and P4's refresh) needs to be able to ask for one, and because
// §9.2(b)'s case 10 asserts the arithmetic of the fixed point with no client
// attached at all.
//
// The order below is §6.3's and is fixed by correctness, not convenience.
func (l *WatchLoop) Rebuild(ctx context.Context, trigger string) (*Snapshot, ArmReport, error) {
	l.rebuildMu.Lock()
	defer l.rebuildMu.Unlock()

	if l.WatchDegraded() {
		return l.rebuildOnDemand(ctx, trigger)
	}

	rep := ArmReport{ByKind: map[TargetKind]int{}}
	changed := l.beginRebuild(trigger)

	// WAVE 0 — the stable spine. Arm before anything is enumerated. These paths
	// are created once and never replaced, so one arm holds for the process's
	// whole life and every later DIRECT-entry mutation is queued from here on.
	spine := l.armSpine(ctx, &rep)

	// WAVE 1 — the job-directory FIXED POINT, and the registry's one-level-
	// simpler equivalent. Arm every immediate jobs/*/ directory, re-enumerate,
	// and repeat until the armed set and the enumeration agree.
	jobDirs := l.armJobDirsToFixedPoint(ctx, &rep)
	regFiles := l.armRegistryFiles(ctx, &rep)

	// WAVE 3 — the targets the records NAME: each job's state.json and the
	// linkScanPath of each non-terminal record. Discovering them means reading
	// the records, and every directory they live in was armed in wave 1, so
	// Invariant A already covers this read.
	jobFiles, transcripts := l.armRecordNamedTargets(ctx, &rep)

	// THE IDENTITY PASS (code review round 2, A4). The waves above make no
	// call when every path they want is already armed with the same kind —
	// which says nothing about whether the VNODE under that path is the one the
	// descriptor holds. A registry record replaced by rename leaves its
	// descriptor on a dead inode, and a directory vnode gets nothing on the
	// in-place writes that follow. So every held target is handed back to the
	// adapter here, immediately before the read: unchanged descriptors are left
	// untouched (the port's contract, and §0.5 E5's reason for it), and a
	// replaced one is re-armed NOW, while the read it protects is still ahead.
	l.verifyArmedIdentities(ctx, &rep)

	// THE READ. Every path any source is about to open was armed above. This is
	// the line the whole protocol exists to protect, and it is why the rebuild
	// is whole-snapshot rather than incremental: wave 1 keeps no per-directory
	// record of "which ones still owe a read", so a newly armed directory is
	// read after its arm BY CONSTRUCTION and the invariant cannot be violated
	// by forgetting one.
	snap, err := l.core.Snapshot(ctx, l.snapshotOptions(changed))
	if err != nil {
		l.finishRebuild(rep, err)
		return nil, rep, err
	}

	// JOIN, liveness, diff, seq, publish — all inside publish, on this
	// goroutine, in this order.
	l.publish(snap, rep)

	// THE ONLY RELEASING CALL, and it is after the publish. It drops the watch
	// on every target that has disappeared and arms NOTHING — and that is now a
	// property of the OPERATION (the port's Release, which cannot open a
	// descriptor), not merely of its argument. It used to be the port's
	// Reconcile with a subset of the armed set, and Reconcile re-arms a
	// replaced vnode: code review round 2's A4 was exactly that re-arm, landing
	// after the read it should have protected.
	l.reconcileRelease(ctx, l.stillDesired(spine, jobDirs, regFiles, jobFiles, transcripts), &rep)

	l.finishRebuild(rep, nil)
	return snap, rep, nil
}

// rebuildOnDemand is Rebuild once the watch is degraded (§6.6, A8): read
// everything, publish, and arm nothing — there is nothing to arm with. Every
// job transcript is treated as changed, because no transcript event can arrive
// to say which ones did (A3's predicate has no input any more), and a cached
// timing read would otherwise stand for as long as the process lives.
//
// It is called with rebuildMu held, so a pull and a refresh are serialized
// with each other exactly as watch-driven rebuilds were.
func (l *WatchLoop) rebuildOnDemand(ctx context.Context, trigger string) (*Snapshot, ArmReport, error) {
	rep := ArmReport{ByKind: map[TargetKind]int{}, Degraded: DegradedKqueueUnavailable}
	_ = l.beginRebuild(trigger)
	opts := l.snapshotOptions(nil)
	opts.RereadTranscripts = true
	snap, err := l.core.Snapshot(ctx, opts)
	if err != nil {
		l.finishRebuild(rep, err)
		return nil, rep, err
	}
	l.publish(snap, rep)
	l.finishRebuild(rep, nil)
	return snap, rep, nil
}

// markDegraded records that the watch cannot push: on-demand mode, and an
// armed model emptied to match an adapter that holds nothing (a dead kqueue
// releases every descriptor; one that never started armed none).
func (l *WatchLoop) markDegraded() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.degraded = true
	l.mode = WatchModeOnDemand
	l.armed = map[string]WatchTarget{}
}

// WatchDegraded reports whether Run has given up on pushing. While it is true
// every Rebuild is a whole on-demand read, and a pull surface must rebuild
// rather than answer from the published snapshot (A8): nothing will ever
// update that snapshot on its own again.
func (l *WatchLoop) WatchDegraded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.degraded
}

// armSpine is wave 0.
//
// M12: ~/.claude itself is armed ONLY while jobs/ does not exist, and released
// the moment it does. Without it, a clean machine running a background agent for
// the first time while tt observe ran would never start detecting background
// sessions — the jobs directory's creation would fire nothing.
func (l *WatchLoop) armSpine(ctx context.Context, rep *ArmReport) []WatchTarget {
	var want []WatchTarget
	if l.paths.Registry != "" {
		want = append(want, WatchTarget{Kind: TargetRegistryDir, Path: l.paths.Registry})
	}
	jobsExists := false
	if l.paths.Jobs != "" {
		if ok, err := l.tree.Exists(ctx, l.paths.Jobs); err == nil {
			jobsExists = ok
		}
	}
	switch {
	case jobsExists:
		want = append(want, WatchTarget{Kind: TargetJobsRoot, Path: l.paths.Jobs})
	case l.paths.ClaudeRoot != "":
		want = append(want, WatchTarget{Kind: TargetClaudeRoot, Path: l.paths.ClaudeRoot})
	}
	l.reconcileArm(ctx, want, rep)
	return want
}

// armJobDirsToFixedPoint is wave 1: §6.3's explicit fixed point.
//
// It returns the job directories that are ACTUALLY ARMED, which is not always
// the same as the ones it saw. On non-convergence the unarmed remainder is
// deliberately left out of the desired set: including it would arm those
// directories in the releasing call, i.e. AFTER the read, which is the very
// ordering this protocol exists to forbid. They are recovered instead by the
// next rebuild, which the already-armed jobs root guarantees will happen — so
// the failure is bounded to LATENCY and never to a permanent miss.
func (l *WatchLoop) armJobDirsToFixedPoint(ctx context.Context, rep *ArmReport) []WatchTarget {
	if l.paths.Jobs == "" {
		rep.Iterations = 0
		return nil
	}

	var armedDirs []string
	iterations := 0
	pending := []string(nil)

	for {
		iterations++
		scan, err := l.tree.ListDirs(ctx, l.paths.Jobs)
		if err != nil {
			rep.Failures = append(rep.Failures, ArmFailure{Path: l.paths.Jobs, Error: err.Error()})
			break
		}

		var missing []string
		armedDirs = armedDirs[:0]
		for _, d := range scan {
			switch {
			case l.isArmed(d):
				armedDirs = append(armedDirs, d)
			case l.armFailed(d):
				// Counted, advised, and settled: retrying it here would spin
				// the loop to its cap and report the wrong degradation.
			default:
				missing = append(missing, d)
			}
		}
		if len(missing) == 0 {
			break // THE FIXED POINT: armed == scan.
		}
		if iterations >= l.maxIterations {
			pending = missing
			break
		}
		// Arm only, releasing nothing. A superset is free and safe: a redundant
		// EV_ADD on an already-armed descriptor preserves what the kernel has
		// queued on it (§0.5 E6).
		l.reconcileArm(ctx, targetsOf(TargetJobDir, scan), rep)
	}

	rep.Iterations = iterations
	l.setUnconverged(pending)
	if len(pending) > 0 {
		rep.Degraded = DegradedArmScanUnconverged
	}
	return targetsOf(TargetJobDir, armedDirs)
}

// armRegistryFiles is wave 1's registry equivalent, one level simpler.
//
// There is no fixed point to iterate because the records are DIRECT entries of
// a directory armed in wave 0: a file appearing there fires the vnode we have
// held since then, so the enumeration cannot be outrun in a way the event
// stream does not cover. The per-file arming is not an optimisation — the
// registry is rewritten IN PLACE, and a directory vnode receives nothing on an
// in-place write, so without these targets a status change fires nothing at all.
func (l *WatchLoop) armRegistryFiles(ctx context.Context, rep *ArmReport) []WatchTarget {
	if l.paths.Registry == "" {
		return nil
	}
	files, err := l.tree.ListFiles(ctx, l.paths.Registry, ".json")
	if err != nil {
		rep.Failures = append(rep.Failures, ArmFailure{Path: l.paths.Registry, Error: err.Error()})
		return nil
	}
	targets := targetsOf(TargetRegistryFile, files)
	l.reconcileArm(ctx, targets, rep)
	return l.onlyArmed(targets)
}

// armRecordNamedTargets is wave 3: the job records' own state.json files, and
// the linkScanPath target of every non-terminal record that has one.
//
// It reads the job records to find out what to arm. That read is safe because
// every directory it reads from was armed in wave 1 — which is the point of
// doing wave 1 first — and the answer the client sees is built by the read
// after this wave, so a transcript appended before the arm is seen by that read
// and one appended after it is queued (E1). Both cases are covered; there is no
// third.
func (l *WatchLoop) armRecordNamedTargets(ctx context.Context, rep *ArmReport) (jobFiles, transcripts []WatchTarget) {
	if l.jobs == nil || l.paths.Jobs == "" {
		return nil, nil
	}
	res, err := l.jobs.Read(ctx)
	if err != nil {
		rep.Failures = append(rep.Failures, ArmFailure{Path: l.paths.Jobs, Error: err.Error()})
		return nil, nil
	}

	var want []WatchTarget
	for _, rec := range res.Records {
		if rec.Dir == "" {
			continue
		}
		record := filepath.Join(l.paths.Jobs, rec.Dir, jobRecordName)
		if l.isArmed(filepath.Dir(record)) {
			// Belt-and-braces only, and only under an armed directory: a job
			// record is published by REPLACE (measured), so this descriptor
			// goes deaf after the first publish and the directory watch is what
			// actually carries the news. It costs one descriptor per record and
			// makes the design survive Claude changing the mechanism.
			want = append(want, WatchTarget{Kind: TargetJobFile, Path: record})
		}
		if rec.LinkScanPath == "" {
			continue
		}
		if st, _ := MapJobState(rec.State); st.IsTerminal() {
			continue
		}
		if ok, err := l.tree.Exists(ctx, rec.LinkScanPath); err != nil || !ok {
			continue
		}
		want = append(want, WatchTarget{Kind: TargetJobTranscript, Path: rec.LinkScanPath})
	}

	l.reconcileArm(ctx, want, rep)
	for _, t := range l.onlyArmed(want) {
		if t.Kind == TargetJobFile {
			jobFiles = append(jobFiles, t)
		} else {
			transcripts = append(transcripts, t)
		}
	}
	return jobFiles, transcripts
}

// reconcileArm is an ARM-ONLY wave: it passes the union of what is already
// armed with what is wanted, so the call cannot release anything, and it makes
// NO call at all when nothing is new.
//
// Making no call is not a micro-optimisation. §0.5 E5 measured that closing a
// descriptor discards every event already queued on it, so the fewer times the
// adapter is handed a set the fewer chances there are for a future
// implementation to treat a re-pass as a re-arm. It also makes "the steady
// state touches no descriptor" observable: a test compares fd numbers.
func (l *WatchLoop) reconcileArm(ctx context.Context, want []WatchTarget, rep *ArmReport) {
	if len(want) == 0 {
		return
	}
	l.mu.Lock()
	novel := false
	for _, t := range want {
		if cur, held := l.armed[t.Path]; !held || cur.Kind != t.Kind {
			novel = true
			break
		}
	}
	union := make([]WatchTarget, 0, len(l.armed)+len(want))
	for _, t := range l.armed {
		union = append(union, t)
	}
	l.mu.Unlock()

	if !novel {
		return
	}
	union = append(union, want...)
	l.applyReconcile(ctx, union, rep)
}

// verifyArmedIdentities hands every held target back to the port's Reconcile,
// so a target whose vnode was replaced is re-armed BEFORE the read. The set is
// exactly what is already held, so it arms nothing new and releases nothing;
// the only descriptors it can touch are ones that are already deaf.
func (l *WatchLoop) verifyArmedIdentities(ctx context.Context, rep *ArmReport) {
	l.mu.Lock()
	held := make([]WatchTarget, 0, len(l.armed))
	for _, t := range l.armed {
		held = append(held, t)
	}
	l.mu.Unlock()
	if len(held) == 0 {
		return
	}
	l.applyReconcile(ctx, held, rep)
}

// reconcileRelease is the one call that releases. It goes through the port's
// Release, which cannot arm anything — not a new target, and not a replacement
// vnode — so "no arm follows the read" holds by construction. Its argument is
// also a subset of what is already armed (see stillDesired), so a target that
// is wanted but not armed stays unarmed until the next rebuild's waves.
func (l *WatchLoop) reconcileRelease(ctx context.Context, keep []WatchTarget, rep *ArmReport) {
	got, err := l.watch.Release(ctx, keep)
	if err != nil {
		l.mu.Lock()
		l.lastErr = err
		l.mu.Unlock()
		rep.Failed++
		rep.Failures = append(rep.Failures, ArmFailure{Path: "", Error: err.Error()})
		return
	}

	l.mu.Lock()
	next := make(map[string]WatchTarget, len(keep))
	for _, t := range keep {
		if cur, held := l.armed[t.Path]; held && cur.Kind == t.Kind {
			next[t.Path] = cur
		}
	}
	l.armed = next
	l.mu.Unlock()

	rep.Released += got.Released
}

// armedByKindLocked is the armed set by family. It is what ArmReport.ByKind
// means (G3): the report travels with a publication, so it has to describe
// what is armed at that publication — not what the last arm wave happened to
// pass, which in the steady state is nothing at all. The caller holds l.mu.
func (l *WatchLoop) armedByKindLocked() map[TargetKind]int {
	out := make(map[TargetKind]int, len(AllTargetKinds))
	for _, t := range l.armed {
		out[t.Kind]++
	}
	return out
}

// applyReconcile is the single call site of the port's Reconcile, so the
// bookkeeping cannot diverge between the arm path and the release path.
func (l *WatchLoop) applyReconcile(ctx context.Context, targets []WatchTarget, rep *ArmReport) {
	got, err := l.watch.Reconcile(ctx, targets)
	if err != nil {
		l.mu.Lock()
		l.lastErr = err
		l.mu.Unlock()
		rep.Failed++
		rep.Failures = append(rep.Failures, ArmFailure{Path: "", Error: err.Error()})
		return
	}

	failed := make(map[string]bool, len(got.Failures))
	for _, f := range got.Failures {
		failed[f.Path] = true
	}

	l.mu.Lock()
	next := make(map[string]WatchTarget, len(targets))
	for _, t := range targets {
		if failed[t.Path] {
			l.failedArms[t.Path] = true
			continue
		}
		delete(l.failedArms, t.Path)
		next[t.Path] = t
	}
	l.armed = next
	l.mu.Unlock()

	rep.Armed += got.Armed
	rep.Released += got.Released
	rep.Failed += got.Failed
	rep.Failures = append(rep.Failures, got.Failures...)
	if got.Degraded != "" && rep.Degraded != DegradedArmScanUnconverged {
		rep.Degraded = got.Degraded
	}
	// ByKind is NOT taken from the adapter's answer: that counts the targets
	// this one call carried, and the arm-only waves make no call at all in the
	// steady state. It is derived from the armed set where the report is
	// stored (publish and finishRebuild) — code review round 2, G3.
}

// stillDesired is the releasing call's target set: everything currently armed
// that this rebuild still wants, and nothing else.
//
// Built as an INTERSECTION with the armed set rather than as a fresh desired
// set, which is the property that matters: a target that is wanted but not yet
// armed is not armed here, because arming after the read is the ordering defect
// this whole file exists to prevent. It will be armed by the next rebuild's
// waves, before the next read.
func (l *WatchLoop) stillDesired(groups ...[]WatchTarget) []WatchTarget {
	wanted := map[string]TargetKind{}
	for _, g := range groups {
		for _, t := range g {
			wanted[t.Path] = t.Kind
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	keep := make([]WatchTarget, 0, len(l.armed))
	for path, t := range l.armed {
		if kind, ok := wanted[path]; ok && kind == t.Kind {
			keep = append(keep, t)
		}
	}
	return keep
}

// publish assigns the sequence number, stores the snapshot and fans the deltas
// out. It is the only place any of the three happens.
//
// # Linearised with Subscribe (code review round 2, A5)
//
// It used to capture the subscriber list under l.mu, release it, decorate the
// snapshot, and only then store it — so a subscriber registering in between was
// neither in the captured list nor able to load the new snapshot, and was left
// with no baseline (on a quiet machine, forever) or one state behind. Now the
// store and the fan-out happen in ONE critical section, the same one Subscribe
// registers and takes its baseline in: every subscriber is either registered
// before this publication (and offered it) or after it (and handed it as its
// baseline), and there is no third case. Offers never block — each is a
// non-blocking send under the subscriber's own lock — so holding l.mu across
// them costs microseconds, not a slow consumer's latency.
func (l *WatchLoop) publish(snap *Snapshot, rep ArmReport) {
	l.mu.Lock()
	// Stored BEFORE attachWatch reads it back, so the watch block describes
	// THIS rebuild's arming rather than the previous one's — and with ByKind
	// taken from the armed set, so the report visible at publication is
	// complete (G3).
	rep.ByKind = l.armedByKindLocked()
	l.lastArm = rep
	l.rebuilds++
	l.mu.Unlock()

	// Decorated while still private: nothing else can see it until the store.
	l.attachWatch(snap)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	snap.Seq = l.seq
	prev := l.published.Load()
	l.published.Store(snap)
	for _, s := range l.subs {
		s.offer(prev, snap)
	}
}

// attachWatch merges the watch accounting into the snapshot's source reports,
// immediately before publication.
//
// FOUND BY P7's LIVE RUN, and by nothing else: every fixture test passed with
// `sources.registry.watch` null, and so did every surface, because the CORE
// builds the source reports and the core does not know what is armed. P5 built
// WatchReports() and Advisories() and left them "returned, never merged, so
// the phase that assembles sources stays the single writer" — and the phase
// that publishes over a socket is the first one for which the block is part of
// the contract (§5.2's envelope, §5.3's hello and sources frames, and AC11's
// exclusion allowlist, which excuses `watch.mode` from byte-identity precisely
// because it is expected to be THERE and to differ).
//
// Here rather than in the core because this is the only place that holds both
// the snapshot and the arm counts, and here rather than in each driving
// adapter because `tt mcp`'s local path and the TUI's own watch would each
// have to repeat it — the convention-over-structure failure §5.1 exists to
// prevent.
//
// It mutates the snapshot BEFORE the atomic store, which is the one window in
// which a snapshot is not yet shared. After the store it is immutable.
func (l *WatchLoop) attachWatch(snap *Snapshot) {
	if snap == nil || snap.Sources == nil {
		return
	}

	reports := l.WatchReports()
	advisories := l.Advisories()

	// Route the watch's own advisories to the source whose tree produced them.
	// A per-path advisory goes to the tree the path is in; the two aggregates
	// (an unconverged fixed point, a hit descriptor cap) go to the jobs tree,
	// because the fixed point is the jobs enumeration and the cap is reached by
	// arming its directories.
	bySource := map[string][]Advisory{}
	for _, a := range advisories {
		id := SourceJobs
		if path, ok := a.Fields["path"].(string); ok && !isUnder(path, l.paths.Jobs) {
			id = SourceRegistry
		}
		bySource[id] = append(bySource[id], a)
	}

	// COPIED BEFORE MUTATION, and the race detector is why this line exists.
	// The Snapshotter port does not promise a fresh map per call, and a source
	// map shared with an already-published snapshot would be written here
	// while a subscriber's encoder was reading it — §7.3's "snapshots are never
	// mutated after publish" broken by the one place that mutates one before.
	// A copy of at most five entries per rebuild makes the invariant hold
	// whatever the port returns.
	sources := make(map[string]SourceReport, len(snap.Sources))
	for id, src := range snap.Sources {
		sources[id] = src
	}
	snap.Sources = sources

	touched := false
	for id, rep := range reports {
		src, ok := snap.Sources[id]
		if !ok {
			// A source the core did not report cannot grow a watch block here:
			// inventing one would describe a read nobody performed, which is
			// the same rule that keeps the tmux and transcript blocks
			// conditional.
			continue
		}
		watch := rep
		src.Watch = &watch
		if extra := bySource[id]; len(extra) > 0 {
			merged := copyAdvisories(src.Advisories)
			for _, a := range extra {
				merged = appendAdvisory(merged, a)
			}
			src.Advisories = merged
		}
		snap.Sources[id] = src
		touched = true
	}
	if touched {
		// Recomputed rather than appended to: warnings ARE the promotion of
		// the source-scoped advisories, so deriving them again is what keeps
		// the two from disagreeing about the same fact.
		snap.Warnings = promoteSourceAdvisories(snap.Sources)
	}
}

// Latest returns the most recently published snapshot, or nil before the first
// publication. Readers load it through an atomic pointer and hold no lock:
// snapshots are never mutated after publication.
func (l *WatchLoop) Latest() *Snapshot { return l.published.Load() }

// Subscribe registers a subscriber whose FIRST event is always a snapshot, so
// the client has a baseline to apply deltas to rather than an undefined
// starting state.
//
// When something is already published that snapshot is delivered here, at
// once. When nothing is published yet it is delivered by the first
// publication — as a snapshot, never as a burst of additions (see offer). The
// second case is not a corner: `tt observe` binds its socket before its first
// rebuild has finished, and that rebuild waits on the corroboration spawn.
//
// The returned channel is closed when ctx is done. Membership is
// client-relative from here on: this subscriber is told about additions to and
// removals from ITS view of the world (H13).
func (l *WatchLoop) Subscribe(ctx context.Context, f Filter) (<-chan Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s := &subscriber{
		ch:     make(chan Event, l.subBuffer),
		filter: f,
	}

	// Registration and the baseline are ONE critical section, the same one
	// publish stores and fans out in (A5): the snapshot loaded here is exactly
	// the last one this subscriber was not offered.
	l.mu.Lock()
	l.nextSub++
	id := l.nextSub
	l.subs[id] = s
	if snap := l.published.Load(); snap != nil {
		s.baseline(snap)
	}
	l.mu.Unlock()

	go func() {
		<-ctx.Done()
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
		s.close()
	}()

	return s.ch, nil
}

// Subscribers is how many subscribers are attached.
func (l *WatchLoop) Subscribers() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.subs)
}

// notifyWatchDegraded tells every subscriber, once, that the stream it is
// holding will not push. §6.6: a subscriber must learn immediately rather than
// waiting forever for an event that cannot come.
func (l *WatchLoop) notifyWatchDegraded(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snap := l.published.Load()
	at := time.Time{}
	if snap != nil {
		at = snap.ObservedAt
	}
	for _, s := range l.subs {
		s.deliver([]Event{{
			Type:   EventWatchDegraded,
			Seq:    l.seq,
			Reason: reason,
			At:     at,
		}}, snap)
	}
}

// noteHint records one coalesced hint. It does no work beyond counting: T5's
// precondition and everything else that could spawn or read is evaluated inside
// the rebuild, so a burst of 50 hints that coalesces into one rebuild also
// produces at most one of everything else.
func (l *WatchLoop) noteHint(c Change) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hints++
	at := c.At
	if at.IsZero() {
		at = l.clock.Now()
	}
	l.lastHintAt = &at
	// Which transcript fired is the one thing a hint carries that the rebuild
	// cannot rediscover by reading: an append changes neither the job record
	// nor anything Invariant B's other four predicates compare (A3).
	if c.Kind == TargetJobTranscript && c.Ref != "" {
		if l.changedTranscripts == nil {
			l.changedTranscripts = map[string]bool{}
		}
		l.changedTranscripts[c.Ref] = true
	}
}

// Hints is how many change hints have been received.
func (l *WatchLoop) Hints() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hints
}

// Rebuilds is how many rebuilds have published.
func (l *WatchLoop) Rebuilds() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rebuilds
}

// LastArmReport is the arm accounting of the most recent rebuild.
func (l *WatchLoop) LastArmReport() ArmReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastArm
}

// Mode is the watch mode: "kqueue" or "on-demand".
func (l *WatchLoop) Mode() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode
}

func (l *WatchLoop) setMode(mode string) {
	l.mu.Lock()
	l.mode = mode
	l.mu.Unlock()
}

// Unconverged is the job directories wave 1 saw and could not arm within its
// iteration cap. Empty in every state but the degraded one.
func (l *WatchLoop) Unconverged() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.unconverged))
	copy(out, l.unconverged)
	return out
}

func (l *WatchLoop) setUnconverged(dirs []string) {
	l.mu.Lock()
	l.unconverged = dirs
	l.mu.Unlock()
}

// Advisories is the source-scoped advisories the watch itself has to report:
// one per unarmable target, and one for a fixed point that did not converge.
//
// They are returned rather than merged into the snapshot because the sources
// block is assembled by whoever owns the source reports; this keeps the two
// from disagreeing about the same fact.
func (l *WatchLoop) Advisories() []Advisory {
	rep := l.LastArmReport()
	out := []Advisory{}
	for _, f := range rep.Failures {
		out = append(out, Advisory{
			Code:   AdvWatchTargetMissing,
			Fields: map[string]any{"path": f.Path, "error": f.Error},
		})
	}
	if pending := l.Unconverged(); len(pending) > 0 {
		out = append(out, Advisory{
			Code: AdvWatchArmUnconverged,
			Fields: map[string]any{
				"iterations":  int64(rep.Iterations),
				"pendingDirs": int64(len(pending)),
			},
		})
	}
	if rep.Degraded == DegradedFileWatchCap || rep.Degraded == DegradedJobDirCap {
		out = append(out, Advisory{
			Code:   AdvFileWatchCap,
			Fields: map[string]any{"armed": int64(rep.Armed), "cap": rep.Degraded},
		})
	}
	return out
}

// WatchReports is the per-source watch accounting for sources.<id>.watch. The
// two source ids that have a watch are the registry and the jobs tree; the
// transcript targets are reported with the jobs tree, because they exist only
// because a job record named them.
func (l *WatchLoop) WatchReports() map[string]WatchReport {
	l.mu.Lock()
	defer l.mu.Unlock()

	reg := WatchReport{Mode: l.mode, Events: l.hints, LastEventAt: l.lastHintAt}
	jobs := WatchReport{Mode: l.mode, Events: l.hints, LastEventAt: l.lastHintAt}
	jobs.ArmIterations = l.lastArm.Iterations
	if l.lastArm.Degraded != "" {
		degraded := l.lastArm.Degraded
		jobs.Degraded = &degraded
		if degraded == DegradedKqueueUnavailable || degraded == DegradedFileWatchCap {
			reg.Degraded = &degraded
		}
	}

	for _, t := range l.armed {
		switch t.Kind {
		case TargetRegistryDir, TargetClaudeRoot:
			reg.ArmedDirs++
		case TargetRegistryFile:
			reg.ArmedFiles++
		case TargetJobsRoot, TargetJobDir:
			jobs.ArmedDirs++
		case TargetJobFile:
			jobs.ArmedFiles++
		case TargetJobTranscript:
			jobs.ArmedTranscripts++
		}
	}
	if l.paths.Registry == "" {
		reg.Mode = WatchModeAbsent
	}
	if l.paths.Jobs == "" {
		jobs.Mode = WatchModeAbsent
	}
	for path := range l.failedArms {
		if isUnder(path, l.paths.Jobs) {
			jobs.ArmFailures++
			continue
		}
		reg.ArmFailures++
	}
	return map[string]WatchReport{SourceRegistry: reg, SourceJobs: jobs}
}

// snapshotOptions is what the loop asks the core for: EVERYTHING it knows.
//
// The published snapshot is the full set — dead included — and each
// subscriber's view is narrowed by Visible at delta time. That is what makes a
// removal reason derivable: a session that left a client's view is either still
// in the full set (and we can say which rule ended it) or gone from it
// entirely.
//
// Built by overriding DefaultSnapshotOptions rather than by a struct literal,
// because IncludeUnknownLiveness defaults to FALSE in a literal and would
// silently drop every live: null session — the exact behaviour M7 forbids.
func (l *WatchLoop) snapshotOptions(changedTranscripts []string) SnapshotOptions {
	opts := DefaultSnapshotOptions()
	opts.IncludeDead = true
	// Launched sessions too, for the same reason as the dead: the published
	// set is the whole population and each subscriber's Filter narrows it. A
	// loop that dropped them here would make `tt sessions --all` over the
	// socket — and every includeSpawned subscriber — unable to see them at all.
	opts.Filter.IncludeSpawned = true
	opts.ChangedTranscripts = changedTranscripts
	return opts
}

// beginRebuild records the trigger and TAKES the transcripts that fired since
// the last rebuild (A3). Taking them here, at the start, is what makes the
// hand-off lossless: a hint that lands while this rebuild is reading is kept
// for the next one, whose read will see the append either way.
func (l *WatchLoop) beginRebuild(trigger string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastTrigger = trigger
	l.failedArms = map[string]bool{}
	if len(l.changedTranscripts) == 0 {
		return nil
	}
	out := make([]string, 0, len(l.changedTranscripts))
	for p := range l.changedTranscripts {
		out = append(out, p)
	}
	l.changedTranscripts = nil
	return out
}

func (l *WatchLoop) finishRebuild(rep ArmReport, err error) {
	l.mu.Lock()
	rep.ByKind = l.armedByKindLocked()
	l.lastArm = rep
	l.lastErr = err
	l.mu.Unlock()
}

// LastTrigger names what caused the most recent rebuild: "startup", "watch" or
// whatever a pull surface passed.
func (l *WatchLoop) LastTrigger() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastTrigger
}

// LastError is the most recent rebuild's error, or nil.
func (l *WatchLoop) LastError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

// Armed returns the loop's model of the armed set.
func (l *WatchLoop) Armed() map[string]TargetKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]TargetKind, len(l.armed))
	for p, t := range l.armed {
		out[p] = t.Kind
	}
	return out
}

func (l *WatchLoop) isArmed(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.armed[path]
	return ok
}

func (l *WatchLoop) armFailed(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failedArms[path]
}

// onlyArmed filters a wanted set down to what is actually armed, so a target
// that could not be opened never reaches the releasing call's desired set and
// never reads as armed in a report.
func (l *WatchLoop) onlyArmed(want []WatchTarget) []WatchTarget {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]WatchTarget, 0, len(want))
	for _, t := range want {
		if cur, held := l.armed[t.Path]; held && cur.Kind == t.Kind {
			out = append(out, t)
		}
	}
	return out
}

func targetsOf(kind TargetKind, paths []string) []WatchTarget {
	if len(paths) == 0 {
		return nil
	}
	out := make([]WatchTarget, 0, len(paths))
	for _, p := range paths {
		out = append(out, WatchTarget{Kind: kind, Path: p})
	}
	return out
}

func isUnder(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// subscriber is one attached consumer.
//
// The publisher never blocks on it. When the buffer is full, everything it has
// queued is replaced by ONE resync event carrying the current snapshot
// (overflowLocked) — which is both smaller than an unbounded queue and the only
// honest thing to send to a client that has missed deltas.
type subscriber struct {
	ch     chan Event
	filter Filter

	mu     sync.Mutex
	resync bool
	closed bool
	// held is the Seq of the published snapshot whose state this subscriber
	// has been brought to — by a baseline, by a resync, or by the deltas of a
	// publication. 0 means it has been given NO baseline yet. Published
	// sequence numbers start at 1, so 0 is never a real publication.
	held int64
}

// snapshotEvent is a baseline: the whole published state.
func snapshotEvent(snap *Snapshot) Event {
	return Event{Type: EventSnapshot, Seq: snap.Seq, Snapshot: snap, At: snap.ObservedAt}
}

// resyncEvent is a baseline sent because deltas were dropped.
func resyncEvent(snap *Snapshot) Event {
	return Event{Type: EventResync, Seq: snap.Seq, Snapshot: snap, At: snap.ObservedAt}
}

// baseline hands a subscriber the snapshot that was already published when it
// subscribed. A publication that raced ahead of it and already brought the
// subscriber to a NEWER state wins, and this older one is dropped rather than
// delivered after it.
func (s *subscriber) baseline(snap *Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || snap == nil || (s.held != 0 && snap.Seq <= s.held) {
		return
	}
	select {
	case s.ch <- snapshotEvent(snap):
		s.held = snap.Seq
	default:
		s.overflowLocked(snap)
	}
}

// offer hands one publication to this subscriber: a snapshot when it has no
// baseline the deltas could apply to, the deltas otherwise — and, after them,
// one sources event when a source's HEALTH changed (A12).
//
// FOUND BY THE INDEPENDENT BLACK-BOX SUITE (TestSocketDiscoversFirstJobRecord),
// and by nothing in-repo. A subscriber that attached before the FIRST
// publication used to be handed Diff(nil, snap) — one addition per visible
// session — and nothing else. On a machine with sessions sockserve turned the
// first addition into a snapshot frame, so every test passed. On a machine
// with NO visible session (a clean install, or an observer started while only
// a record-less job directory exists) Diff(nil, snap) is empty, so the
// subscriber was sent nothing at all: `tt observe` sent hello and then
// silence, and a client waiting for its baseline (§7.5b) waited forever — and
// never saw the first job record that arrived afterwards either. The TUI
// panel, which subscribes to a loop it has just started and drops every delta
// that precedes a baseline, had the same hole with sessions present.
//
// So the baseline decision is the LOOP's, made once, for every consumer: the
// first event any subscriber receives is a snapshot. held also covers a
// subscriber whose held state is not the previous publication: it gets a fresh
// snapshot instead of deltas computed against a state it never saw.
func (s *subscriber) offer(prev, snap *Snapshot) {
	if snap == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.held != 0 && snap.Seq <= s.held {
		return // it already holds this state, or a newer one
	}

	if s.resync {
		// Only reachable if an overflow could not even enqueue its resync.
		s.overflowLocked(snap)
		return
	}

	var prevSeq int64
	if prev != nil {
		prevSeq = prev.Seq
	}
	if s.held == 0 || s.held != prevSeq {
		select {
		case s.ch <- snapshotEvent(snap):
			s.held = snap.Seq
		default:
			s.overflowLocked(snap)
		}
		return
	}

	events := Diff(prev, snap, s.filter)
	if SourceHealthChanged(prev, snap) {
		// Code review round 2, A12: a change to what the sources report about
		// themselves — a corroboration failure that cleared, a watch that
		// degraded, a source that went stale — reaches an existing subscriber
		// even when no session changed. It carries the whole snapshot, so a
		// consumer takes the source reports from the same state as the deltas
		// before it.
		events = append(events, Event{Type: EventSources, Seq: snap.Seq, Snapshot: snap, At: snap.ObservedAt})
	}
	for _, e := range events {
		select {
		case s.ch <- e:
		default:
			s.overflowLocked(snap)
			return
		}
	}
	s.held = snap.Seq
}

// overflowLocked is what a full queue means: everything this subscriber has
// queued is DISCARDED and replaced by one resync carrying snap. Caller holds
// s.mu.
//
// Code review round 2, A13. Overflow used to set a flag and wait for the NEXT
// publication to send the resync. A slow consumer that missed the last
// transition of a burst, then caught up while the machine went quiet, drained
// its stale deltas and was never told what it had missed — and with no timer of
// any kind in this design, nothing was ever going to publish again. Replacing
// the queue here makes recovery depend only on the consumer draining it.
//
// The subscriber is the channel's only sender, so after the drain there is
// room (the buffer is at least one). A queued watch-degraded notice is not
// session state and cannot be reconstructed from a snapshot, so it survives the
// drain when there is room for it after the resync.
func (s *subscriber) overflowLocked(snap *Snapshot) {
	var keep []Event
drain:
	for {
		select {
		case e := <-s.ch:
			if e.Type == EventWatchDegraded {
				keep = append(keep, e)
			}
		default:
			break drain
		}
	}
	s.resync = true
	if snap != nil {
		select {
		case s.ch <- resyncEvent(snap):
			s.resync = false
			s.held = snap.Seq
		default:
		}
	}
	for _, e := range keep {
		select {
		case s.ch <- e:
		default:
		}
	}
}

// deliver sends events that are not about session state — a watch
// degradation — honouring a pending resync the same way offer does.
func (s *subscriber) deliver(events []Event, snap *Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.resync {
		s.overflowLocked(snap)
	}
	for _, e := range events {
		select {
		case s.ch <- e:
		default:
			// Full: replace the stale queue with a resync, then retry this
			// event once — it is the one thing a resync cannot carry.
			s.overflowLocked(snap)
			select {
			case s.ch <- e:
			default:
			}
		}
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}
