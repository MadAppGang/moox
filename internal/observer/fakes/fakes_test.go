package fakes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

var refNow = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

// TestDepsWiresEverySevenDrivenPortsPlusTheClock. The compile-time assertions
// in fakes.go already prove each fake satisfies its interface; this proves the
// convenience constructor fills every slot, which is what stops a new port
// being added to observer.Deps with a nil fake nobody noticed until a test
// panicked three phases later.
func TestDepsWiresEverySevenDrivenPortsPlusTheClock(t *testing.T) {
	deps, set := Deps(refNow)

	if deps.Registry == nil || deps.Jobs == nil || deps.Transcripts == nil ||
		deps.Agents == nil || deps.Panes == nil || deps.Proc == nil ||
		deps.Watch == nil || deps.Clock == nil {
		t.Fatalf("Deps left a port nil: %+v", deps)
	}
	if set.Clock.Now() != refNow {
		t.Errorf("clock reads %v, want %v", set.Clock.Now(), refNow)
	}
	// The two push sources must be stamped, or every age derived from them
	// would be measured against the zero time.
	if got, _ := set.Registry.Read(context.Background()); !got.ObservedAt.Equal(refNow) {
		t.Errorf("registry observedAt = %v, want %v", got.ObservedAt, refNow)
	}
	if got, _ := set.Jobs.Read(context.Background()); !got.ObservedAt.Equal(refNow) {
		t.Errorf("jobs observedAt = %v, want %v", got.ObservedAt, refNow)
	}
}

// TestClockMovesOnlyWhenToldTo is the property that makes the four
// clock-sensitive liveness rules assertable rather than flaky.
func TestClockMovesOnlyWhenToldTo(t *testing.T) {
	c := NewClock(refNow)
	first := c.Now()
	time.Sleep(2 * time.Millisecond)
	if !c.Now().Equal(first) {
		t.Error("the fake clock advanced on its own")
	}
	c.Advance(90 * time.Minute)
	if got := c.Now(); !got.Equal(refNow.Add(90 * time.Minute)) {
		t.Errorf("after Advance, Now() = %v", got)
	}
	c.Set(refNow)
	if !c.Now().Equal(refNow) {
		t.Error("Set did not take")
	}
}

// TestProcDistinguishesDeadFromErrored. An errored gate is NOT evidence of
// death, so the fake has to be able to express all three outcomes or the
// gate-error path cannot be tested at all.
func TestProcDistinguishesDeadFromErrored(t *testing.T) {
	p := NewProc(78761)

	if alive, err := p.Alive(78761); !alive || err != nil {
		t.Errorf("a live pid: (%v, %v)", alive, err)
	}
	if alive, err := p.Alive(99999); alive || err != nil {
		t.Errorf("an absent pid should be dead with no error: (%v, %v)", alive, err)
	}

	boom := errors.New("kill: operation not permitted")
	p.SetError(4242, boom)
	alive, err := p.Alive(4242)
	if err == nil {
		t.Fatal("an errored gate returned no error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the injected one", err)
	}
	if alive {
		t.Error("an errored gate must not also claim the process is alive")
	}
	if p.Calls() != 3 {
		t.Errorf("Calls() = %d, want 3", p.Calls())
	}
}

// TestPanesReturnsACopy. A shared map would let the core mutate the fake, and
// under -race it would report a data race the production adapter cannot have.
func TestPanesReturnsACopy(t *testing.T) {
	f := NewPanes("madbench:@51.%107")
	got, err := f.ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	got["injected"] = true
	again, _ := f.ListPanes(context.Background())
	if again["injected"] {
		t.Error("mutating the returned map changed the fake's state")
	}
	if !again["madbench:@51.%107"] {
		t.Error("the pane id, sigils included, is not in the set")
	}
}

// TestWatcherRecordsReconcileOrderAndTargets. The correctness property of the
// Watcher port is about ORDER — an arm must precede the read it protects, and
// the only releasing call comes after the publish — and order cannot be
// asserted from a final state, so the fake records every call.
func TestWatcherRecordsReconcileOrderAndTargets(t *testing.T) {
	w := NewWatcher(4)
	ctx := context.Background()

	wave1 := []observer.WatchTarget{
		{Kind: observer.TargetRegistryDir, Path: "/c/sessions"},
		{Kind: observer.TargetJobsRoot, Path: "/c/jobs"},
	}
	wave2 := append(append([]observer.WatchTarget{}, wave1...),
		observer.WatchTarget{Kind: observer.TargetJobDir, Path: "/c/jobs/ea780079"})

	if _, err := w.Reconcile(ctx, wave1); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := w.Reconcile(ctx, wave2); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	calls := w.Reconciles()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(calls))
	}
	if len(calls[0]) != 2 || len(calls[1]) != 3 {
		t.Errorf("recorded target counts %d and %d, want 2 and 3", len(calls[0]), len(calls[1]))
	}

	// The recorded sets must be snapshots, not aliases: mutating the caller's
	// slice afterwards must not rewrite history.
	wave2[0].Path = "/mutated"
	if w.Reconciles()[1][0].Path == "/mutated" {
		t.Error("the fake aliased the caller's slice instead of copying it")
	}

	armed := w.Armed()
	if len(armed) != 3 {
		t.Errorf("armed set has %d entries, want 3: %v", len(armed), armed)
	}
}

// TestWatcherReportsArmedAndReleased, and in particular that a GROWING set
// re-arms nothing. Closing and reopening an unchanged descriptor discards every
// event already queued on it, so a gratuitous re-arm is a data-loss bug, and
// the fake has to be able to show it did not happen.
func TestWatcherReportsArmedAndReleased(t *testing.T) {
	w := NewWatcher(4)
	ctx := context.Background()

	a := []observer.WatchTarget{{Kind: observer.TargetRegistryFile, Path: "/c/sessions/1.json"}}
	b := append(append([]observer.WatchTarget{}, a...),
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: "/c/sessions/2.json"})

	r1, _ := w.Reconcile(ctx, a)
	if r1.Armed != 1 || r1.Released != 0 {
		t.Errorf("first call: armed=%d released=%d, want 1/0", r1.Armed, r1.Released)
	}

	r2, _ := w.Reconcile(ctx, b)
	if r2.Armed != 1 {
		t.Errorf("growing the set armed %d, want exactly the 1 new target", r2.Armed)
	}
	if r2.Released != 0 {
		t.Errorf("growing the set released %d; it must release nothing", r2.Released)
	}

	r3, _ := w.Reconcile(ctx, a)
	if r3.Released != 1 {
		t.Errorf("shrinking the set released %d, want 1", r3.Released)
	}
	if r3.Armed != 0 {
		t.Errorf("shrinking the set armed %d, want 0", r3.Armed)
	}

	// Idempotence: the same set twice must arm and release nothing.
	r4, _ := w.Reconcile(ctx, a)
	if r4.Armed != 0 || r4.Released != 0 {
		t.Errorf("a repeated identical call armed=%d released=%d, want 0/0", r4.Armed, r4.Released)
	}
}

// TestWatcherArmHookFiresInsideReconcile. This is the seam that lets a test
// create a file in the window between the scan and the arm — the failure mode
// that loses an event permanently, with no later event to recover it, and which
// passes a naive test.
func TestWatcherArmHookFiresInsideReconcile(t *testing.T) {
	w := NewWatcher(4)
	fired := 0
	var sawTargets int
	w.ArmHook = func(targets []observer.WatchTarget) {
		fired++
		sawTargets = len(targets)
	}
	targets := []observer.WatchTarget{{Kind: observer.TargetJobsRoot, Path: "/c/jobs"}}
	if _, err := w.Reconcile(context.Background(), targets); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fired != 1 {
		t.Errorf("ArmHook fired %d times, want 1", fired)
	}
	if sawTargets != 1 {
		t.Errorf("ArmHook saw %d targets, want the desired set", sawTargets)
	}
}

// TestWatcherByKindCountsPerFamily so sources.<id>.watch can report per-family
// arm counts without the core keeping a parallel tally.
func TestWatcherByKindCountsPerFamily(t *testing.T) {
	w := NewWatcher(1)
	report, err := w.Reconcile(context.Background(), []observer.WatchTarget{
		{Kind: observer.TargetRegistryFile, Path: "/c/sessions/1.json"},
		{Kind: observer.TargetRegistryFile, Path: "/c/sessions/2.json"},
		{Kind: observer.TargetJobDir, Path: "/c/jobs/a"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := report.ByKind[observer.TargetRegistryFile]; got != 2 {
		t.Errorf("registry-file count = %d, want 2", got)
	}
	if got := report.ByKind[observer.TargetJobDir]; got != 1 {
		t.Errorf("job-dir count = %d, want 1", got)
	}
}

// TestWatcherEmitDeliversHints, and that a hint carries no kernel vocabulary —
// only "something under this kind changed", which is what lets the port stay
// coarse.
func TestWatcherEmitDeliversHints(t *testing.T) {
	w := NewWatcher(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	w.Emit(observer.Change{Kind: observer.TargetRegistryFile, Ref: "/c/sessions/1.json", At: refNow})

	select {
	case got := <-ch:
		if got.Kind != observer.TargetRegistryFile {
			t.Errorf("kind = %v", got.Kind)
		}
		if got.Ref != "/c/sessions/1.json" {
			t.Errorf("ref = %q", got.Ref)
		}
	case <-time.After(time.Second):
		t.Fatal("no hint delivered")
	}

	if w.WatchCalls() != 1 {
		t.Errorf("WatchCalls = %d, want 1", w.WatchCalls())
	}
}

// TestAgentsCountsSpawns is the counter behind "N simultaneous cold-cache
// callers produce at most one spawn".
func TestAgentsCountsSpawns(t *testing.T) {
	a := &Agents{}
	a.SetEntries(refNow, observer.AgentsEntry{SessionID: "s1"})
	for i := 0; i < 3; i++ {
		if _, err := a.List(context.Background()); err != nil {
			t.Fatalf("list: %v", err)
		}
	}
	if a.Spawns() != 3 {
		t.Errorf("Spawns = %d, want 3", a.Spawns())
	}
	res, _ := a.List(context.Background())
	if !res.OK {
		t.Error("SetEntries must mark the listing OK")
	}
	if !res.Lists("s1") {
		t.Error("Lists(s1) = false")
	}
	if res.Lists("nope") {
		t.Error("Lists reported a session that is not in the listing")
	}
}

// TestAgentsBlockRespectsContext. A spawn that hangs must be cancellable, or
// the timeout test would hang the whole suite instead of proving the timeout.
func TestAgentsBlockRespectsContext(t *testing.T) {
	a := &Agents{Block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.List(ctx); err == nil {
		t.Error("a blocked spawn ignored a cancelled context")
	}
}

// TestTranscriptsCountReadsPerPath. "The colliding project directory is never
// opened" and "no glob fallback is attempted after a refused path" are claims
// about which files were touched; counting is the only way to test them.
func TestTranscriptsCountReadsPerPath(t *testing.T) {
	f := NewTranscripts()
	f.ByGlob["s1"] = observer.TranscriptLocation{Found: true, Path: "/p/s1.jsonl"}
	f.Timing["/p/s1.jsonl"] = observer.TimingFacts{LastActivityAt: &refNow, TailBytes: 65536}

	ctx := context.Background()
	loc, err := f.FindByGlob(ctx, "s1")
	if err != nil {
		t.Fatalf("FindByGlob: %v", err)
	}
	if !loc.Found || loc.Path != "/p/s1.jsonl" {
		t.Fatalf("located %+v", loc)
	}
	if _, err := f.ReadTiming(ctx, loc, 65536); err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	if f.GlobCalls("s1") != 1 {
		t.Errorf("GlobCalls = %d, want 1", f.GlobCalls("s1"))
	}
	if f.ReadCalls("/p/s1.jsonl") != 1 {
		t.Errorf("ReadCalls = %d, want 1", f.ReadCalls("/p/s1.jsonl"))
	}

	// A path absent from ByPath is "not found" — which is also what a path
	// outside the projects root must look like, so the refusal path and the
	// missing-file path are indistinguishable to the core, as intended.
	missing, err := f.OpenPath(ctx, "/etc/passwd")
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	if missing.Found {
		t.Error("an unregistered path reported Found")
	}
	if f.PathCalls("/etc/passwd") != 1 {
		t.Errorf("PathCalls = %d, want 1", f.PathCalls("/etc/passwd"))
	}
	if f.GlobCalls("s1") != 1 {
		t.Error("OpenPath must not fall back to the glob locator")
	}
}

// TestJobsDerivesItsCounts so a test does not have to keep four reconcilable
// numbers consistent by hand.
func TestJobsDerivesItsCounts(t *testing.T) {
	j := &Jobs{}
	j.SetRecords(refNow,
		observer.JobRecord{Dir: "a", State: "blocked"},
		observer.JobRecord{Dir: "b", State: "working"},
		observer.JobRecord{Dir: "c", State: "done"},
		observer.JobRecord{Dir: "d", State: "failed"},
		observer.JobRecord{Dir: "e", State: "reaping"},
	)
	res, err := j.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Dirs != 5 {
		t.Errorf("Dirs = %d, want 5", res.Dirs)
	}
	// blocked, working and the unrecognised state are all non-terminal: only
	// done and failed are terminal, and an unrecognised state is "no opinion",
	// which must not be treated as finished.
	if res.NonTerminal != 3 {
		t.Errorf("NonTerminal = %d, want 3", res.NonTerminal)
	}
}

// TestSourcesReturnErrorsAsValues. A source failure is a value in the snapshot,
// not an error that kills the response, and the fakes have to be able to
// express "errored but still returned what was readable".
func TestSourcesReturnErrorsAsValues(t *testing.T) {
	boom := errors.New("open /c/sessions: permission denied")
	r := &Registry{Err: boom}
	r.SetRecords(refNow, observer.RegistryRecord{PID: 1, SessionID: "s1"})

	res, err := r.Read(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the injected one", err)
	}
	if len(res.Records) != 1 {
		t.Errorf("a partly readable source returned %d records, want 1", len(res.Records))
	}
}

// TestContextCancellationIsHonoured across the sources, so a timeout test is
// testing the core's timeout and not the fake's indifference to one.
func TestContextCancellationIsHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (&Registry{}).Read(ctx); err == nil {
		t.Error("registry ignored a cancelled context")
	}
	if _, err := (&Jobs{}).Read(ctx); err == nil {
		t.Error("jobs ignored a cancelled context")
	}
	if _, err := NewPanes().ListPanes(ctx); err == nil {
		t.Error("panes ignored a cancelled context")
	}
}
