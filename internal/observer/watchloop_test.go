package observer_test

// §6.3's arm-before-read protocol and §7.2's serialized loop, driven through
// fakes so the ORDERING is asserted deterministically rather than by timing.
// The real kqueue watcher and the client-visible AC10 cases are in
// watchloop_ac10_test.go; this file is about the schedule.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

var loopT0 = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

// ---------------------------------------------------------------------------
// The test doubles the protocol needs
// ---------------------------------------------------------------------------

// callLog records what happened, in order. The correctness property of this
// whole file is an ORDER, and an order cannot be asserted from a final state.
type callLog struct {
	mu   sync.Mutex
	rows []string
}

func (l *callLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, fmt.Sprintf(format, args...))
}

func (l *callLog) rowsCopy() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.rows))
	copy(out, l.rows)
	return out
}

// indexOf is the first row whose prefix matches, or -1.
func (l *callLog) indexOf(prefix string) int {
	for i, r := range l.rowsCopy() {
		if strings.HasPrefix(r, prefix) {
			return i
		}
	}
	return -1
}

func (l *callLog) countOf(prefix string) int {
	n := 0
	for _, r := range l.rowsCopy() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// recordingWatcher wraps the fake watcher and logs every Reconcile with the
// kinds it carried, so a wave is identifiable by the families in its set — the
// same discriminator §9.2(b)'s armHook uses.
type recordingWatcher struct {
	*fakes.Watcher
	log *callLog
	// armed is what the watcher holds after each call, so a test can assert
	// the releasing call's set is a SUBSET of it.
	mu    sync.Mutex
	sets  [][]observer.WatchTarget
	holds []map[string]bool

	releaseSets  [][]observer.WatchTarget
	releaseHolds []map[string]bool
}

func (w *recordingWatcher) Reconcile(ctx context.Context, targets []observer.WatchTarget) (observer.ArmReport, error) {
	w.mu.Lock()
	held := map[string]bool{}
	for p := range w.Watcher.Armed() {
		held[p] = true
	}
	snapshot := make([]observer.WatchTarget, len(targets))
	copy(snapshot, targets)
	w.sets = append(w.sets, snapshot)
	w.holds = append(w.holds, held)
	w.mu.Unlock()

	w.log.add("reconcile %s", kindSummary(targets))
	return w.Watcher.Reconcile(ctx, targets)
}

// Release logs the post-publication call. It is a separate row prefix from
// "reconcile" because it is a separate operation: code review round 2's A4 was
// the releasing call being the ARMING one.
func (w *recordingWatcher) Release(ctx context.Context, keep []observer.WatchTarget) (observer.ArmReport, error) {
	w.mu.Lock()
	held := map[string]bool{}
	for p := range w.Watcher.Armed() {
		held[p] = true
	}
	snapshot := make([]observer.WatchTarget, len(keep))
	copy(snapshot, keep)
	w.releaseSets = append(w.releaseSets, snapshot)
	w.releaseHolds = append(w.releaseHolds, held)
	w.mu.Unlock()

	w.log.add("release %s", kindSummary(keep))
	return w.Watcher.Release(ctx, keep)
}

func (w *recordingWatcher) releaseCalls() ([][]observer.WatchTarget, []map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	sets := make([][]observer.WatchTarget, len(w.releaseSets))
	copy(sets, w.releaseSets)
	holds := make([]map[string]bool, len(w.releaseHolds))
	copy(holds, w.releaseHolds)
	return sets, holds
}

func (w *recordingWatcher) callSets() ([][]observer.WatchTarget, []map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	sets := make([][]observer.WatchTarget, len(w.sets))
	copy(sets, w.sets)
	holds := make([]map[string]bool, len(w.holds))
	copy(holds, w.holds)
	return sets, holds
}

func kindSummary(targets []observer.WatchTarget) string {
	seen := map[string]int{}
	for _, t := range targets {
		seen[t.Kind.String()]++
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, seen[k]))
	}
	return strings.Join(parts, ",")
}

// fakeTree is the enumeration. onList fires before every ListDirs answer, which
// is how a test makes the tree change DURING the fixed point.
type fakeTree struct {
	mu     sync.Mutex
	dirs   map[string][]string
	files  map[string][]string
	absent map[string]bool
	log    *callLog
	calls  int
	onList func(t *fakeTree, call int)
}

func newFakeTree(log *callLog) *fakeTree {
	return &fakeTree{
		dirs:   map[string][]string{},
		files:  map[string][]string{},
		absent: map[string]bool{},
		log:    log,
	}
}

func (f *fakeTree) setDirs(root string, dirs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[root] = dirs
}

func (f *fakeTree) addDir(root, dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[root] = append(f.dirs[root], dir)
}

func (f *fakeTree) setFiles(root string, files ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[root] = files
}

func (f *fakeTree) setAbsent(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.absent[path] = true
}

func (f *fakeTree) ListDirs(ctx context.Context, dir string) ([]string, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	hook := f.onList
	f.mu.Unlock()
	if hook != nil {
		hook(f, call)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.log != nil {
		f.log.add("listdirs %s", dir)
	}
	if f.absent[dir] {
		return nil, nil
	}
	out := make([]string, len(f.dirs[dir]))
	copy(out, f.dirs[dir])
	sort.Strings(out)
	return out, nil
}

func (f *fakeTree) ListFiles(ctx context.Context, dir, suffix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.log != nil {
		f.log.add("listfiles %s", dir)
	}
	if f.absent[dir] {
		return nil, nil
	}
	out := make([]string, len(f.files[dir]))
	copy(out, f.files[dir])
	sort.Strings(out)
	return out, nil
}

func (f *fakeTree) Exists(ctx context.Context, path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.absent[path] {
		return false, nil
	}
	return true, nil
}

// fakeCore is the Snapshotter. It logs the read, so "every arm precedes the
// read" is a property of the log rather than of a comment.
type fakeCore struct {
	mu       sync.Mutex
	sessions []observer.Session
	at       time.Time
	err      error
	calls    int
	opts     []observer.SnapshotOptions
	log      *callLog
}

func (c *fakeCore) Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.mu.Lock()
	c.calls++
	c.opts = append(c.opts, opts)
	sessions := make([]observer.Session, len(c.sessions))
	copy(sessions, c.sessions)
	at := c.at
	err := c.err
	c.mu.Unlock()

	if c.log != nil {
		c.log.add("read snapshot")
	}
	if err != nil {
		return nil, err
	}
	return &observer.Snapshot{
		ObservedAt: at,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}, nil
}

func (c *fakeCore) setSessions(sessions ...observer.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = sessions
}

func (c *fakeCore) snapshotCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeCore) lastOptions() observer.SnapshotOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.opts) == 0 {
		return observer.SnapshotOptions{}
	}
	return c.opts[len(c.opts)-1]
}

// loggingJobs wraps the jobs fake so wave 3's discovery read is visible in the
// same ordered log as the arms.
type loggingJobs struct {
	*fakes.Jobs
	log *callLog
}

func (j *loggingJobs) Read(ctx context.Context) (observer.JobsResult, error) {
	j.log.add("read jobs")
	return j.Jobs.Read(ctx)
}

// ledgerTimers is AC9's arm/disarm ledger. "The idle steady state has zero
// armed timers" is a claim about arming discipline, and this is the only way to
// assert it: an AST allowlist cannot prove a one-shot property.
type ledgerTimers struct {
	mu      sync.Mutex
	arms    int
	stops   int
	windows []time.Duration
}

func (l *ledgerTimers) NewTimer(d time.Duration) observer.Timer {
	l.mu.Lock()
	l.arms++
	l.windows = append(l.windows, d)
	l.mu.Unlock()
	return &ledgerTimer{t: time.NewTimer(d), ledger: l}
}

func (l *ledgerTimers) Arms() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.arms
}

type ledgerTimer struct {
	t      *time.Timer
	ledger *ledgerTimers
}

func (lt *ledgerTimer) Stop() bool {
	lt.ledger.mu.Lock()
	lt.ledger.stops++
	lt.ledger.mu.Unlock()
	return lt.t.Stop()
}

func (lt *ledgerTimer) C() <-chan time.Time { return lt.t.C }

// loopHarness is one wired loop with every seam a test in this file needs.
type loopHarness struct {
	loop    *observer.WatchLoop
	watcher *recordingWatcher
	tree    *fakeTree
	core    *fakeCore
	jobs    *fakes.Jobs
	clock   *fakes.Clock
	timers  *ledgerTimers
	log     *callLog
	paths   observer.Paths
}

func newLoopHarness(t *testing.T, tune func(*observer.WatchLoopConfig)) *loopHarness {
	t.Helper()
	log := &callLog{}
	clock := fakes.NewClock(loopT0)
	tree := newFakeTree(log)
	core := &fakeCore{at: loopT0, log: log}
	jobs := &fakes.Jobs{}
	watcher := &recordingWatcher{Watcher: fakes.NewWatcher(64), log: log}
	timers := &ledgerTimers{}

	paths := observer.Paths{
		ClaudeRoot: "/scratch/.claude",
		Registry:   "/scratch/.claude/sessions",
		Jobs:       "/scratch/.claude/jobs",
		Projects:   "/scratch/.claude/projects",
	}

	cfg := observer.WatchLoopConfig{
		Core:   core,
		Watch:  watcher,
		Tree:   tree,
		Jobs:   &loggingJobs{Jobs: jobs, log: log},
		Clock:  clock,
		Paths:  paths,
		Timers: timers,
	}
	if tune != nil {
		tune(&cfg)
	}
	loop, err := observer.NewWatchLoop(cfg)
	if err != nil {
		t.Fatalf("NewWatchLoop: %v", err)
	}
	return &loopHarness{
		loop: loop, watcher: watcher, tree: tree, core: core,
		jobs: jobs, clock: clock, timers: timers, log: log, paths: paths,
	}
}

func jobDirPaths(root string, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(root, n))
	}
	return out
}

// ---------------------------------------------------------------------------
// The protocol's order
// ---------------------------------------------------------------------------

func TestEveryArmPrecedesTheReadAndTheOnlyReleasingCallFollowsThePublish(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.tree.setDirs(h.paths.Jobs, jobDirPaths(h.paths.Jobs, "aaaa", "bbbb")...)
	h.tree.setFiles(h.paths.Registry, filepath.Join(h.paths.Registry, "100.json"))
	h.jobs.SetRecords(loopT0, observer.JobRecord{
		Dir: "aaaa", SessionID: "job-aaaa", State: "blocked",
		LinkScanPath: "/scratch/.claude/projects/-s/aaaa.jsonl",
	})

	if _, _, err := h.loop.Rebuild(context.Background(), "test"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	rows := h.log.rowsCopy()
	t.Logf("the rebuild, in order:\n  %s", strings.Join(rows, "\n  "))

	read := h.log.indexOf("read snapshot")
	if read < 0 {
		t.Fatal("the rebuild never read")
	}
	// Every Reconcile — every ARMING call — comes before the read; after it
	// there is exactly one call, and it is the port's Release, which cannot
	// arm. Until code review round 2 (A4) the releasing call was itself a
	// Reconcile, and Reconcile re-arms a replaced vnode: an arm after the read.
	armsAfter, releasesAfter := 0, 0
	for i, r := range rows {
		switch {
		case strings.HasPrefix(r, "reconcile") && i > read:
			armsAfter++
		case strings.HasPrefix(r, "release") && i > read:
			releasesAfter++
		}
	}
	if armsAfter != 0 {
		t.Errorf("%d Reconcile (arming) calls follow the read; none may", armsAfter)
	}
	if releasesAfter != 1 || h.log.countOf("release") != 1 {
		t.Errorf("%d Release calls follow the read (%d in all); §7.2 permits exactly one, "+
			"after the publish", releasesAfter, h.log.countOf("release"))
	}
	if before := h.log.countOf("reconcile"); before < 3 {
		t.Errorf("only %d arm-only waves ran before the read; §6.3 specifies three",
			before)
	}

	// Wave 1 arms the job directories before wave 3 READS the records, which is
	// the specific ordering §0.5 E4 measured a root watch cannot save us from.
	armJobDirs := -1
	for i, r := range rows {
		if strings.HasPrefix(r, "reconcile") && strings.Contains(r, "job-dir=") {
			armJobDirs = i
			break
		}
	}
	readJobs := h.log.indexOf("read jobs")
	if armJobDirs < 0 {
		t.Fatal("no Reconcile ever carried a job directory")
	}
	if readJobs < 0 || armJobDirs > readJobs {
		t.Errorf("the job directories were armed at row %d and the records read at row %d: "+
			"a record written in that window fires nothing and is lost forever",
			armJobDirs, readJobs)
	}
	if readJobs > read {
		t.Error("the discovery read of the job records came after the snapshot read")
	}

	// And the spine is armed before anything is enumerated at all.
	if spine := h.log.indexOf("reconcile jobs-root"); spine >= 0 {
		if first := h.log.indexOf("listdirs"); first >= 0 && spine > first {
			t.Error("the jobs root was enumerated before it was armed: a directory created " +
				"in that window fires nothing")
		}
	}
}

func TestTheReleasingCallArmsNothingBecauseItsSetIsAlreadyArmed(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.tree.setDirs(h.paths.Jobs, jobDirPaths(h.paths.Jobs, "aaaa", "bbbb")...)
	h.tree.setFiles(h.paths.Registry, filepath.Join(h.paths.Registry, "100.json"))

	if _, _, err := h.loop.Rebuild(context.Background(), "test"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	sets, holds := h.watcher.releaseCalls()
	if len(sets) != 1 {
		t.Fatalf("%d Release calls, want exactly the one releasing call", len(sets))
	}
	final := sets[len(sets)-1]
	heldBefore := holds[len(holds)-1]
	for _, target := range final {
		if !heldBefore[target.Path] {
			t.Errorf("the releasing call would ARM %s, which was not armed before it: "+
				"arming after the read is precisely the ordering defect this protocol "+
				"exists to forbid", target.Path)
		}
	}
	t.Logf("the releasing call carried %d targets, all of them already armed", len(final))
}

func TestTheLoopAsksTheCoreForTheFullSetAndNarrowsPerSubscriber(t *testing.T) {
	h := newLoopHarness(t, nil)
	if _, _, err := h.loop.Rebuild(context.Background(), "test"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	opts := h.core.lastOptions()
	if !opts.IncludeDead {
		t.Error("the loop did not ask for the dead set; without it a removal cannot say " +
			"WHY a session left, only that it did")
	}
	if !opts.IncludeUnknownLiveness {
		t.Error("IncludeUnknownLiveness is false: the options were built as a struct " +
			"literal instead of from DefaultSnapshotOptions, which silently drops every " +
			"live: null session (M7)")
	}
	if opts.IncludeTranscripts {
		t.Error("a watch-driven rebuild asked for the 200 KB enrichment pass")
	}
}

// ---------------------------------------------------------------------------
// AC10 case 10 — the fixed point's own arithmetic, with no client
// ---------------------------------------------------------------------------

func TestAC10Case10TheFixedPointTakesTwoIterationsThenOne(t *testing.T) {
	h := newLoopHarness(t, nil)
	names := make([]string, 0, 14)
	for i := 0; i < 14; i++ {
		names = append(names, fmt.Sprintf("job%02d", i))
	}
	h.tree.setDirs(h.paths.Jobs, jobDirPaths(h.paths.Jobs, names...)...)

	_, first, err := h.loop.Rebuild(context.Background(), "startup")
	if err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if first.Iterations != 2 {
		t.Errorf("first rebuild took %d iterations, want 2 — one that arms 14 directories "+
			"and one that confirms", first.Iterations)
	}
	if first.Degraded != "" {
		t.Errorf("Degraded = %q on a convergent tree", first.Degraded)
	}

	_, second, err := h.loop.Rebuild(context.Background(), "watch")
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if second.Iterations != 1 {
		t.Errorf("the steady state took %d iterations, want 1: the enumeration returns "+
			"exactly the armed set and nothing is re-armed", second.Iterations)
	}
	if n := len(h.loop.Armed()); n != 14+2 {
		t.Errorf("armed %d targets, want 14 job dirs + the registry dir + the jobs root", n)
	}
}

func TestAC10Case10TheSteadyStateMakesNoArmingCallAtAll(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.tree.setDirs(h.paths.Jobs, jobDirPaths(h.paths.Jobs, "aaaa")...)
	h.tree.setFiles(h.paths.Registry, filepath.Join(h.paths.Registry, "1.json"))

	if _, _, err := h.loop.Rebuild(context.Background(), "startup"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	firstCalls := len(h.watcher.Reconciles())

	if _, _, err := h.loop.Rebuild(context.Background(), "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	secondCalls := len(h.watcher.Reconciles()) - firstCalls
	// Exactly one: the identity pass before the read (code review round 2,
	// A4), which hands back only what is already held and so arms nothing in
	// the steady state. The releasing call is no longer a Reconcile at all.
	if secondCalls != 1 {
		t.Errorf("the second rebuild made %d Reconcile calls, want exactly 1 (the identity "+
			"pass). Every extra call is a chance for a future implementation to treat a "+
			"re-pass as a re-arm, which §0.5 E5 measured discards queued events",
			secondCalls)
	}
	calls := h.watcher.Reconciles()
	armedBefore := map[string]bool{}
	for _, t := range calls[firstCalls-1] {
		armedBefore[t.Path] = true
	}
	for _, target := range calls[len(calls)-1] {
		if !armedBefore[target.Path] {
			t.Errorf("the steady-state identity pass carried %s, which was not armed: it armed "+
				"something", target.Path)
		}
	}
}

func TestAC10Case10NonConvergencePublishesAnywayAndTheNextRebuildCatchesUp(t *testing.T) {
	// The cap is injected down to 2 rather than 1. Convergence is established
	// by a scan that FOLLOWS an arm, so a cap of 1 can never report it at all —
	// which is a property of the bound, not a failure mode worth testing. The
	// production bound is 4.
	h := newLoopHarness(t, func(cfg *observer.WatchLoopConfig) {
		cfg.MaxReconcileIterations = 2
	})
	h.tree.setDirs(h.paths.Jobs, jobDirPaths(h.paths.Jobs, "aaaa")...)
	// A tree a goroutine is adding directories to: every enumeration sees one
	// more than the last.
	added := 0
	h.tree.onList = func(tree *fakeTree, call int) {
		if added >= 3 {
			return
		}
		added++
		tree.addDir(h.paths.Jobs, filepath.Join(h.paths.Jobs, fmt.Sprintf("new%02d", added)))
	}

	snap, rep, err := h.loop.Rebuild(context.Background(), "startup")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap == nil {
		t.Fatal("a non-convergent fixed point must PUBLISH ANYWAY: the failure is bounded " +
			"to latency, never to a permanent miss")
	}
	if rep.Degraded != observer.DegradedArmScanUnconverged {
		t.Fatalf("Degraded = %q, want %q", rep.Degraded, observer.DegradedArmScanUnconverged)
	}
	if rep.Iterations != 2 {
		t.Errorf("Iterations = %d, want the injected cap of 2", rep.Iterations)
	}
	if len(h.loop.Unconverged()) == 0 {
		t.Error("nothing was reported as pending, so the advisory would carry pendingDirs: 0")
	}

	advisories := h.loop.Advisories()
	found := false
	for _, a := range advisories {
		if a.Code != observer.AdvWatchArmUnconverged {
			continue
		}
		found = true
		if _, ok := a.Fields["iterations"].(int64); !ok {
			t.Errorf("iterations field is %T, want int64 (it round-trips through JSON)",
				a.Fields["iterations"])
		}
		if _, ok := a.Fields["pendingDirs"].(int64); !ok {
			t.Errorf("pendingDirs field is %T, want int64", a.Fields["pendingDirs"])
		}
	}
	if !found {
		t.Fatalf("no %s advisory; the degradation would be invisible: %+v",
			observer.AdvWatchArmUnconverged, advisories)
	}

	// The claim that non-convergence costs LATENCY and not correctness: stop
	// the churn, rebuild again, and every directory that was ever added is
	// armed. The jobs root was armed in wave 0, so the hint that triggers this
	// rebuild exists by construction.
	h.tree.onList = nil
	if _, next, err := h.loop.Rebuild(context.Background(), "watch"); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	} else if next.Degraded != "" {
		t.Errorf("the second rebuild is still degraded: %q", next.Degraded)
	}
	armed := h.loop.Armed()
	for i := 1; i <= added; i++ {
		path := filepath.Join(h.paths.Jobs, fmt.Sprintf("new%02d", i))
		if _, ok := armed[path]; !ok {
			t.Errorf("%s was added during the unconverged loop and is STILL not armed after "+
				"the next rebuild", path)
		}
	}
	if len(h.loop.Unconverged()) != 0 {
		t.Errorf("still pending after convergence: %v", h.loop.Unconverged())
	}
}

func TestAnUnarmableJobDirectoryDoesNotSpinTheFixedPointToItsCap(t *testing.T) {
	// A directory that disappears between the scan and the arm is a
	// watch-target-missing, not a non-convergence: reporting the wrong
	// degradation would send a reader looking for a churning tree.
	h := newLoopHarness(t, nil)
	gone := filepath.Join(h.paths.Jobs, "gone")
	h.tree.setDirs(h.paths.Jobs, gone)
	h.watcher.Watcher.Report = observer.ArmReport{
		Failures: []observer.ArmFailure{{Path: gone, Error: "no such file or directory"}},
	}

	_, rep, err := h.loop.Rebuild(context.Background(), "startup")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rep.Degraded == observer.DegradedArmScanUnconverged {
		t.Error("an unarmable directory was reported as a non-convergent fixed point")
	}
	if rep.Iterations > 2 {
		t.Errorf("Iterations = %d: the failed target was retried instead of being settled",
			rep.Iterations)
	}
	var codes []string
	for _, a := range h.loop.Advisories() {
		codes = append(codes, a.Code)
	}
	if len(codes) == 0 || codes[0] != observer.AdvWatchTargetMissing {
		t.Errorf("advisories = %v, want %s first", codes, observer.AdvWatchTargetMissing)
	}
}

// ---------------------------------------------------------------------------
// §6.6 — the degraded paths
// ---------------------------------------------------------------------------

func TestM12ClaudeRootIsArmedWhileJobsIsAbsentAndReleasedWhenItAppears(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.tree.setAbsent(h.paths.Jobs)

	if _, _, err := h.loop.Rebuild(context.Background(), "startup"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	armed := h.loop.Armed()
	if kind, ok := armed[h.paths.ClaudeRoot]; !ok || kind != observer.TargetClaudeRoot {
		t.Fatalf("~/.claude is not armed while jobs/ is absent (armed: %v). On a clean "+
			"machine running a background agent for the first time, the jobs directory's "+
			"creation would fire nothing and background detection would never start", armed)
	}
	if _, ok := armed[h.paths.Jobs]; ok {
		t.Error("the absent jobs root is armed")
	}

	// It appears.
	h.tree.mu.Lock()
	delete(h.tree.absent, h.paths.Jobs)
	h.tree.mu.Unlock()
	h.tree.setDirs(h.paths.Jobs, filepath.Join(h.paths.Jobs, "aaaa"))

	if _, _, err := h.loop.Rebuild(context.Background(), "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	armed = h.loop.Armed()
	if _, ok := armed[h.paths.ClaudeRoot]; ok {
		t.Error("~/.claude is STILL armed after jobs/ appeared: it costs one descriptor in " +
			"one rare state and must be released the moment it is not needed")
	}
	if kind, ok := armed[h.paths.Jobs]; !ok || kind != observer.TargetJobsRoot {
		t.Error("the jobs root was not armed once it existed")
	}
}

func TestAnAbsentRegistryIsAFactAndTheLoopStillAnswers(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.tree.setAbsent(h.paths.Registry)
	h.tree.setAbsent(h.paths.Jobs)

	snap, rep, err := h.loop.Rebuild(context.Background(), "startup")
	if err != nil {
		t.Fatalf("an absent tree must not be an error: %v", err)
	}
	if snap == nil {
		t.Fatal("no snapshot was published for a machine where Claude has never run")
	}
	if rep.Failed != 0 {
		t.Errorf("Failed = %d for absent directories; absence is a fact, not a failure", rep.Failed)
	}
}

func TestAnUnavailableKqueueFallsIntoOnDemandAndTellsEverySubscriber(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.watcher.Watcher.WatchErr = errors.New("kqueue: too many open files")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := h.loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	sawDegraded := false
	for !sawDegraded {
		select {
		case e := <-events:
			if e.Type == observer.EventWatchDegraded {
				sawDegraded = true
				if e.Reason == "" {
					t.Error("watch_degraded carries no reason")
				}
			}
		case <-deadline:
			t.Fatal("no watch_degraded frame: a subscriber would wait forever for an event " +
				"that cannot come")
		}
	}
	if h.loop.Mode() != observer.WatchModeOnDemand {
		t.Errorf("Mode = %q, want %q", h.loop.Mode(), observer.WatchModeOnDemand)
	}
	if h.loop.Latest() == nil {
		t.Error("a degraded observer must still be a correct on-demand observer, and it " +
			"published nothing at all")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on a clean shutdown", err)
	}
}

func TestAWatchThatDiesMidRunIsReArmedOnce(t *testing.T) {
	h := newLoopHarness(t, nil)
	// Reopen is what "the re-arm SUCCEEDED" means: a watcher that recovered
	// hands back a fresh, live channel. Without it the fake models the dead
	// watcher instead — the same closed channel, nil error — which is the case
	// the test below pins.
	h.watcher.Watcher.Reopen = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()

	// Wait for the startup rebuild, then kill the channel the way a dying
	// watcher would.
	waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })
	h.watcher.Watcher.Close()

	// One re-arm attempt: Watch is called a second time and succeeds, so the
	// loop keeps its mode.
	waitFor(t, 2*time.Second, func() bool { return h.watcher.Watcher.WatchCalls() >= 2 })
	if h.loop.Mode() != observer.WatchModeKqueue {
		t.Errorf("Mode = %q after a successful re-arm, want %q",
			h.loop.Mode(), observer.WatchModeKqueue)
	}
	cancel()
	<-done
}

// A watcher whose loop has died can hand its own CLOSED channel back with a nil
// error. Run's recovery code was written for `rerr != nil`, which by
// construction never happened on that path, so the closed channel was assigned
// straight back and re-selected immediately: 540,844 Watch() calls in 500 ms,
// one core pegged forever inside a server with no idle exit — while Mode() went
// on reporting "kqueue" to the doctor and the panel footer.
//
// This is the failure class this machine has repeatedly suffered (61 orphaned
// probe scripts across 12.5 of 18 cores; 226 idle processes holding 17.3 GB),
// so the assertion is on all three halves: the call count stays BOUNDED, the
// health surface stops claiming a push watch, and the subscriber is told.
func TestAClosedHintChannelHandedBackDegradesInsteadOfSpinning(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := h.loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

	// The fake's default IS the dead adapter: Close() closes the channel, and
	// every later Watch() returns that same closed channel with a nil error.
	h.watcher.Watcher.Close()

	// §6.6's transition must happen, and it must happen without a spin. The
	// bound is deliberately generous — the defect produced ~1.08M calls per
	// second, so anything in single digits is a loop that stopped.
	waitFor(t, 2*time.Second, func() bool { return h.loop.Mode() == observer.WatchModeOnDemand })
	if calls := h.watcher.Watcher.WatchCalls(); calls > 4 {
		t.Errorf("Watch was called %d times over one death: the loop is spinning on a closed "+
			"channel, which is one core burned forever on a server that never exits", calls)
	}

	// The degradation must be OBSERVABLE, or a subscriber waits forever for an
	// event that cannot come and the doctor reports a healthy push watch.
	deadline := time.After(2 * time.Second)
	sawDegraded := false
	for !sawDegraded {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("the event stream closed without a watch_degraded frame")
			}
			if e.Type == observer.EventWatchDegraded {
				sawDegraded = true
				if e.Reason == "" {
					t.Error("watch_degraded carries no reason")
				}
			}
		case <-deadline:
			t.Fatal("no watch_degraded frame: the watch is dead and nothing said so")
		}
	}

	// Settle, then prove the call count is not merely small but STABLE: a spin
	// that degraded and kept calling would still be a spin.
	before := h.watcher.Watcher.WatchCalls()
	time.Sleep(200 * time.Millisecond)
	if after := h.watcher.Watcher.WatchCalls(); after != before {
		t.Errorf("Watch calls went %d -> %d after the transition: the loop is still re-arming",
			before, after)
	}
	if h.loop.Latest() == nil {
		t.Error("a degraded observer must still be a correct on-demand observer, and it " +
			"published nothing at all")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on a clean shutdown", err)
	}
}

// The second half of the bound: a watcher that keeps handing back FRESH but
// already-dead channels must not be re-armed forever either. The identity check
// cannot see this one — every channel is a different channel — so it is the
// consecutive-re-arm budget that has to stop it.
func TestAWatchThatDiesAgainWithoutDeliveringStopsReArming(t *testing.T) {
	h := newLoopHarness(t, nil)
	h.watcher.Watcher.Reopen = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

	// Die, recover, die again — with no hint delivered in between.
	h.watcher.Watcher.Close()
	waitFor(t, 2*time.Second, func() bool { return h.watcher.Watcher.WatchCalls() >= 2 })
	h.watcher.Watcher.Close()

	waitFor(t, 2*time.Second, func() bool { return h.loop.Mode() == observer.WatchModeOnDemand })
	if calls := h.watcher.Watcher.WatchCalls(); calls > 4 {
		t.Errorf("Watch was called %d times over two deaths, want the re-arm budget to bite", calls)
	}
	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// AC9 — idle cost is proven, not asserted
// ---------------------------------------------------------------------------

func TestAC9aTheIdleArmCountIsZeroWithNoClientsAndWithTwo(t *testing.T) {
	for _, clients := range []int{0, 2} {
		t.Run(fmt.Sprintf("%d clients", clients), func(t *testing.T) {
			h := newLoopHarness(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			for i := 0; i < clients; i++ {
				if _, err := h.loop.Subscribe(ctx, observer.Filter{}); err != nil {
					t.Fatalf("Subscribe: %v", err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- h.loop.Run(ctx) }()
			waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

			// Ten minutes of the fake clock, then thirty days of it, with no
			// events at all. Nothing in the loop reads the clock on a schedule,
			// so this is instantaneous — which is the point.
			h.clock.Advance(10 * time.Minute)
			h.clock.Advance(30 * 24 * time.Hour)

			if arms := h.timers.Arms(); arms != 0 {
				t.Errorf("%d timers were armed over an idle interval; the idle steady state "+
					"must hold ZERO", arms)
			}
			if n := h.loop.Rebuilds(); n != 1 {
				t.Errorf("%d rebuilds with no events, want the one startup rebuild", n)
			}
			if h.loop.Subscribers() != clients {
				t.Errorf("Subscribers = %d, want %d", h.loop.Subscribers(), clients)
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run returned %v", err)
			}
		})
	}
}

func TestAC9aARunForeverObserverArmsNothingAndGrowsNoGoroutines(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

	before := runtime.NumGoroutine()
	publishedBefore := h.loop.Rebuilds()
	h.clock.Advance(30 * 24 * time.Hour)
	// Give anything that WOULD wake up a chance to. There is nothing to wait
	// for, which is exactly what is being asserted.
	time.Sleep(200 * time.Millisecond)

	if arms := h.timers.Arms(); arms != 0 {
		t.Errorf("%d timers armed over 30 days of idleness", arms)
	}
	if now := h.loop.Rebuilds(); now != publishedBefore {
		t.Errorf("published %d more snapshots with no events", now-publishedBefore)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines grew from %d to %d while idle", before, after)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v; the user's override is that tt observe runs until it is "+
			"killed, with no idle timeout of any kind", err)
	default:
	}
	cancel()
	<-done
}

func TestAC9aABurstOfFiftyHintsProducesOneRebuildAndOneArming(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

	for i := 0; i < 50; i++ {
		h.watcher.Watcher.Emit(observer.Change{
			Kind: observer.TargetRegistryFile,
			Ref:  fmt.Sprintf("/scratch/.claude/sessions/%d.json", i),
			At:   loopT0,
		})
	}

	waitFor(t, 3*time.Second, func() bool { return h.loop.Rebuilds() >= 2 })
	time.Sleep(700 * time.Millisecond) // two more coalescing windows

	if n := h.loop.Rebuilds(); n != 2 {
		t.Errorf("%d rebuilds for one burst of 50 hints, want 2 (startup + the coalesced "+
			"burst)", n)
	}
	if arms := h.timers.Arms(); arms != 1 {
		t.Errorf("the coalescer was armed %d times for one burst, want 1: it is armed on the "+
			"FIRST hint of a burst and never re-armed while idle", arms)
	}
	if got := h.loop.Hints(); got != 50 {
		t.Errorf("Hints = %d, want 50 — every hint is counted even though they coalesce", got)
	}
	cancel()
	<-done
}

func TestAC9bAnIdleLoopCostsNoCPUAndAConstantGoroutineCount(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

	g0 := runtime.NumGoroutine()
	cpu0 := cpuMillis(t)
	time.Sleep(2 * time.Second)
	g1 := runtime.NumGoroutine()
	cpu1 := cpuMillis(t)

	if g1 != g0 {
		t.Errorf("goroutine count moved from %d to %d over a quiet 2 s", g0, g1)
	}
	spent := cpu1 - cpu0
	t.Logf("CPU over a quiet 2 s: %.3f ms (bound 10 ms); goroutines constant at %d", spent, g0)
	if spent > 10 {
		t.Errorf("%.3f ms of CPU over a quiet interval, want under 10 ms", spent)
	}
	cancel()
	<-done
}

func TestAC9cEightObserversOverOneTransitionSpawnNothing(t *testing.T) {
	const observers = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hs := make([]*loopHarness, 0, observers)
	for i := 0; i < observers; i++ {
		h := newLoopHarness(t, nil)
		hs = append(hs, h)
		go func() { _ = h.loop.Run(ctx) }()
	}
	for _, h := range hs {
		waitFor(t, 3*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })
	}

	cpu0 := cpuMillis(t)
	for _, h := range hs {
		h.watcher.Watcher.Emit(observer.Change{
			Kind: observer.TargetRegistryFile,
			Ref:  "/scratch/.claude/sessions/100.json",
			At:   loopT0,
		})
	}
	for _, h := range hs {
		waitFor(t, 3*time.Second, func() bool { return h.loop.Rebuilds() >= 2 })
	}
	cpu1 := cpuMillis(t)

	for i, h := range hs {
		if n := h.loop.Rebuilds(); n != 2 {
			t.Errorf("observer %d rebuilt %d times, want 2", i, n)
		}
		// Nothing spawns on a Change alone (§4.2 T5). The loop holds no agents
		// source at all, which is the structural version of the same claim.
		if h.core.snapshotCalls() != 2 {
			t.Errorf("observer %d read %d times, want 2", i, h.core.snapshotCalls())
		}
	}
	spent := cpu1 - cpu0
	t.Logf("aggregate CPU for %d observers over one transition: %.3f ms (bound 40 ms)",
		observers, spent)
	if spent > 40 {
		t.Errorf("%.3f ms of CPU for one transition across %d observers, want under 40 ms",
			spent, observers)
	}
}

// ---------------------------------------------------------------------------
// Subscribers
// ---------------------------------------------------------------------------

func TestASlowSubscriberIsResyncedAndNeverBlocksTheLoop(t *testing.T) {
	h := newLoopHarness(t, func(cfg *observer.WatchLoopConfig) { cfg.SubscriberBuffer = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := h.loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Never read from events. Each rebuild adds a session, so each would emit a
	// delta into a buffer of one.
	for i := 0; i < 5; i++ {
		s := baseSession()
		s.SessionID = fmt.Sprintf("0000000%d-2222-3333-4444-555555555555", i)
		existing, _, err := h.loop.Rebuild(ctx, "test")
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		_ = existing
		h.core.setSessions(append(currentSessions(h), s)...)
	}

	// The loop is still healthy: it published every time.
	if n := h.loop.Rebuilds(); n != 5 {
		t.Fatalf("Rebuilds = %d, want 5 — a slow consumer must not be able to delay the "+
			"rebuild loop every other consumer depends on", n)
	}

	// And the subscriber is told to resync rather than handed a hole.
	drained := 0
	sawResync := false
	for {
		select {
		case e := <-events:
			drained++
			if e.Type == observer.EventResync {
				sawResync = true
				if e.Snapshot == nil {
					t.Error("a resync carries no snapshot, so the client cannot recover")
				}
			}
			if drained > 10 {
				t.Fatal("more events than could fit in a buffer of one")
			}
			continue
		default:
		}
		break
	}
	if !sawResync {
		// One more rebuild gives the resync somewhere to go now the buffer has
		// been drained.
		if _, _, err := h.loop.Rebuild(ctx, "test"); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		select {
		case e := <-events:
			if e.Type != observer.EventResync {
				t.Fatalf("event %q, want resync", e.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("a subscriber that fell behind was never resynced")
		}
	}
}

func TestASubscriberGetsASnapshotBaselineThenDeltas(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.core.setSessions(baseSession())
	if _, _, err := h.loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	events, err := h.loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	first := <-events
	if first.Type != observer.EventSnapshot || first.Snapshot == nil {
		t.Fatalf("the first frame is %q; a subscriber needs a baseline to apply deltas to",
			first.Type)
	}
	if first.Seq == 0 {
		t.Error("the baseline carries no sequence number")
	}

	changed := baseSession()
	changed.Status = observer.StatusBusy
	h.core.setSessions(changed)
	if _, _, err := h.loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	select {
	case e := <-events:
		if e.Type != observer.EventSessionChanged {
			t.Fatalf("event %q, want session_changed", e.Type)
		}
		if len(e.Changed) == 0 || e.Changed[0] != "status" {
			t.Errorf("Changed = %v, want [status ...]", e.Changed)
		}
	case <-time.After(time.Second):
		t.Fatal("no delta after a status change")
	}
}

// The black-box suite's TestSocketDiscoversFirstJobRecord, at the loop: a
// subscriber that attached BEFORE the first publication, on a machine with no
// visible session, used to receive nothing at all — Diff(nil, empty) is empty —
// so it never had a baseline, and every consumer that waits for one (a socket
// client, the TUI panel) waited forever. The first event is now a snapshot
// whatever the set holds.
func TestASubscriberAttachedBeforeTheFirstPublicationGetsASnapshotEvenOfNothing(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := h.loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("an event (%s) arrived before anything was published", e.Type)
	default:
	}

	// The first publication, of an EMPTY set.
	if _, _, err := h.loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	select {
	case e := <-events:
		if e.Type != observer.EventSnapshot || e.Snapshot == nil {
			t.Fatalf("the first event is %q, want the baseline snapshot", e.Type)
		}
		if len(e.Snapshot.Sessions) != 0 {
			t.Fatalf("the baseline carries %d sessions, want 0", len(e.Snapshot.Sessions))
		}
	case <-time.After(time.Second):
		t.Fatal("no baseline: a subscriber attached before the first publication of an empty " +
			"set was sent nothing, and would wait for one forever")
	}

	// And from that baseline the next publication is an ordinary delta.
	h.core.setSessions(baseSession())
	if _, _, err := h.loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	select {
	case e := <-events:
		if e.Type != observer.EventSessionAdded || e.SessionID != baseSession().SessionID {
			t.Fatalf("event %q for %q, want session_added for the new session", e.Type, e.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("no delta for the session that appeared after the baseline")
	}
}

// The same rule with sessions present: the first publication reaches an early
// subscriber as ONE snapshot, not as one session_added per session. The panel
// drops every delta that precedes a baseline, so additions-first left the TUI's
// own-watch panel empty.
func TestAnEarlySubscribersFirstEventIsASnapshotRatherThanABurstOfAdditions(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := h.loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.core.setSessions(baseSession())
	if _, _, err := h.loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	first := <-events
	if first.Type != observer.EventSnapshot || first.Snapshot == nil || len(first.Snapshot.Sessions) != 1 {
		t.Fatalf("first event %q (%+v), want a one-session baseline", first.Type, first.Snapshot)
	}
	select {
	case e := <-events:
		t.Fatalf("a %s followed the baseline of the same publication; the baseline already "+
			"says everything it would", e.Type)
	default:
	}
}

func TestSequenceNumbersAreMonotonicAndAssignedOnlyByThePublisher(t *testing.T) {
	h := newLoopHarness(t, nil)
	var last int64
	for i := 0; i < 5; i++ {
		snap, _, err := h.loop.Rebuild(context.Background(), "test")
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		if snap.Seq <= last {
			t.Fatalf("seq went %d -> %d", last, snap.Seq)
		}
		last = snap.Seq
	}
	if got := h.loop.Latest(); got == nil || got.Seq != last {
		t.Fatalf("Latest carries seq %v, want %d", got, last)
	}
}

func TestAnUnsubscribedChannelIsClosedAndDropsOutOfTheFanOut(t *testing.T) {
	h := newLoopHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	subCtx, subCancel := context.WithCancel(ctx)
	defer cancel()

	events, err := h.loop.Subscribe(subCtx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if h.loop.Subscribers() != 1 {
		t.Fatalf("Subscribers = %d, want 1", h.loop.Subscribers())
	}
	subCancel()

	select {
	case _, ok := <-events:
		if ok {
			t.Error("the channel delivered after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel was not closed on unsubscribe")
	}
	waitFor(t, time.Second, func() bool { return h.loop.Subscribers() == 0 })
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func currentSessions(h *loopHarness) []observer.Session {
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	out := make([]observer.Session, len(h.core.sessions))
	copy(out, h.core.sessions)
	return out
}

// waitFor polls a condition. It is in a test, where the timer rule does not
// apply; the production code has no polling of any kind, which is what AC9
// asserts.
func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", within)
}

// cpuMillis is this process's cumulative user+system CPU time.
func cpuMillis(t *testing.T) float64 {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Skipf("getrusage unavailable: %v", err)
	}
	ms := func(tv syscall.Timeval) float64 {
		return float64(tv.Sec)*1000 + float64(tv.Usec)/1000
	}
	return ms(ru.Utime) + ms(ru.Stime)
}

var _ = os.Getpid

// manualTimers hands out timers that fire only when the test says so, and
// records each one's duration.
type manualTimers struct {
	mu     sync.Mutex
	timers []*manualTimer
}

type manualTimer struct {
	d time.Duration
	c chan time.Time
}

func (m *manualTimers) NewTimer(d time.Duration) observer.Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &manualTimer{d: d, c: make(chan time.Time, 1)}
	m.timers = append(m.timers, t)
	return t
}

func (t *manualTimer) Stop() bool          { return true }
func (t *manualTimer) C() <-chan time.Time { return t.c }

func (m *manualTimers) armed() []*manualTimer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*manualTimer(nil), m.timers...)
}

type countingRefresher struct {
	mu    sync.Mutex
	calls int
}

func (r *countingRefresher) RefreshSources(context.Context, []string) observer.RefreshOutcome {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return observer.RefreshOutcome{}
}

func (r *countingRefresher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestABackgroundJobOnTheListingKeepsTheListingFresh: while a background job's
// verdict rests on the agents listing, the loop re-reads it before it can go
// stale and republishes, so the row does not flip NEEDS YOU -> UNCLEAR on the
// clock alone. With no such job it arms nothing (AC9 still holds).
func TestABackgroundJobOnTheListingKeepsTheListingFresh(t *testing.T) {
	for _, tc := range []struct {
		reason observer.LiveReason
		want   bool
	}{
		{observer.LiveReasonJobNonTerminalCorroborated, true},
		{observer.LiveReasonJobNonTerminalUncorroborated, true},
		{observer.LiveReasonJobTranscriptGone, false},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			timers := &manualTimers{}
			ref := &countingRefresher{}
			h := newLoopHarness(t, func(c *observer.WatchLoopConfig) {
				c.Timers = timers
				c.Refresh = ref
			})
			h.core.sessions = []observer.Session{{
				SessionID:  "1ce7fef1-99c7-4309-97b5-6a7995b7434d",
				Kind:       observer.KindBackground,
				Status:     observer.StatusBlocked,
				LiveReason: tc.reason,
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- h.loop.Run(ctx) }()
			waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })

			if !tc.want {
				if n := len(timers.armed()); n != 0 {
					t.Fatalf("%d timers armed for a job that does not rest on the listing", n)
				}
				return
			}
			waitFor(t, 2*time.Second, func() bool { return len(timers.armed()) == 1 })
			first := timers.armed()[0]
			if first.d != observer.CorroborationKeepAlive {
				t.Errorf("keep-alive armed for %v, want %v", first.d, observer.CorroborationKeepAlive)
			}
			if observer.CorroborationKeepAlive.Milliseconds() >= observer.DefaultStaleAfterMs {
				t.Errorf("keep-alive %v does not beat the %d ms stale line",
					observer.CorroborationKeepAlive, observer.DefaultStaleAfterMs)
			}
			first.c <- loopT0
			waitFor(t, 2*time.Second, func() bool { return h.loop.Rebuilds() >= 2 && len(timers.armed()) == 2 })
			if ref.count() != 1 {
				t.Errorf("listing re-read %d times on one keep-alive, want 1", ref.count())
			}
			cancel()
			<-done
		})
	}
}
