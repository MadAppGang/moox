// Package fakes provides a test double for every driven port the observer core
// defines, plus a controllable clock.
//
// # Why these are a package and not _test.go files
//
// The fakes are used from three places: the core's own tests, each adapter's
// tests, and the four-surface agreement test that wires a real socket server to
// a fake filesystem. A fake defined in observer's _test.go would be invisible
// to the other two. This package lives under internal/, so it is never part of
// anything tt publishes, and it is compiled into the binary only if the binary
// imports it — which it must not.
//
// # Why the fakes exist at all
//
// Five of the seven driven ports will plausibly have one real implementation
// forever, which is the textbook cost of over-applied ports and adapters. They
// are justified by exactly one thing: each has a SECOND implementation on day
// one, here, and the fixture suite is the validation strategy this feature was
// commissioned with. If the fixture strategy were ever dropped, those five
// ports should be deleted and inlined.
//
// # Concurrency
//
// Every fake is safe for concurrent use. That is not gratuitous: the core runs
// one serialized loop but spawns corroboration on its own goroutine, and the
// whole suite runs under -race. A fake that recorded call counts without a
// mutex would turn a real ordering bug into an intermittent race report
// somewhere else.
package fakes

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// Clock is a controllable clock. Time moves only when a test moves it, which is
// what makes the age derivations and the four clock-sensitive liveness rules
// assertable instead of flaky.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock reading t.
func NewClock(t time.Time) *Clock {
	return &Clock{now: observer.TruncateTime(t)}
}

// Now implements observer.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an absolute instant.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = observer.TruncateTime(t)
}

var _ observer.Clock = (*Clock)(nil)

// ---------------------------------------------------------------------------
// 1. RegistrySource
// ---------------------------------------------------------------------------

// Registry is a fake observer.RegistrySource.
type Registry struct {
	mu sync.Mutex
	// Result is what Read returns. Set it directly, or through SetRecords.
	Result observer.RegistryResult
	// Err is returned alongside Result. A source that errors AND returns
	// records is a real case — a partly readable directory — so both are
	// settable at once.
	Err   error
	calls int
}

// SetRecords replaces the records and stamps ObservedAt.
func (f *Registry) SetRecords(at time.Time, recs ...observer.RegistryRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Result.Records = recs
	f.Result.ObservedAt = observer.TruncateTime(at)
}

// Read implements observer.RegistrySource.
func (f *Registry) Read(ctx context.Context) (observer.RegistryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := ctx.Err(); err != nil {
		return observer.RegistryResult{}, err
	}
	return f.Result, f.Err
}

// Calls is how many times Read was called. The core reads the registry once per
// rebuild; a test that sees more has found a rebuild it did not expect.
func (f *Registry) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ observer.RegistrySource = (*Registry)(nil)

// ---------------------------------------------------------------------------
// 2. JobsSource
// ---------------------------------------------------------------------------

// Jobs is a fake observer.JobsSource.
type Jobs struct {
	mu     sync.Mutex
	Result observer.JobsResult
	Err    error
	calls  int
}

// SetRecords replaces the records, stamps ObservedAt, and derives the counts
// the real adapter would report so a test does not have to keep four numbers
// consistent by hand.
func (f *Jobs) SetRecords(at time.Time, recs ...observer.JobRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Result.Records = recs
	f.Result.ObservedAt = observer.TruncateTime(at)
	f.Result.Dirs = len(recs)
	f.Result.Entries = len(recs)
	f.Result.NonTerminal = 0
	for _, r := range recs {
		if st, _ := observer.MapJobState(r.State); !st.IsTerminal() {
			f.Result.NonTerminal++
		}
	}
}

// Read implements observer.JobsSource.
func (f *Jobs) Read(ctx context.Context) (observer.JobsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := ctx.Err(); err != nil {
		return observer.JobsResult{}, err
	}
	return f.Result, f.Err
}

// Calls is how many times Read was called.
func (f *Jobs) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ observer.JobsSource = (*Jobs)(nil)

// ---------------------------------------------------------------------------
// 3. TranscriptSource
// ---------------------------------------------------------------------------

// Transcripts is a fake observer.TranscriptSource.
//
// It counts reads per path, which is how the "a colliding project directory is
// NEVER opened" and "no glob fallback is attempted after a refused path"
// assertions are made: those are claims about which files were touched, and the
// only way to test them is to count.
type Transcripts struct {
	mu sync.Mutex
	// ByGlob maps a session id to what FindByGlob returns.
	ByGlob map[string]observer.TranscriptLocation
	// ByPath maps an absolute path to what OpenPath returns. A path absent from
	// this map is "not found", which is also what a path outside the projects
	// root must look like.
	ByPath map[string]observer.TranscriptLocation
	// Timing and Facts map a path to what the two readers return.
	Timing map[string]observer.TimingFacts
	Facts  map[string]observer.TranscriptFacts

	// Err, when set, is returned by every method.
	Err error

	globCalls map[string]int
	pathCalls map[string]int
	readCalls map[string]int
}

// NewTranscripts returns an empty fake with its maps initialised.
func NewTranscripts() *Transcripts {
	return &Transcripts{
		ByGlob:    map[string]observer.TranscriptLocation{},
		ByPath:    map[string]observer.TranscriptLocation{},
		Timing:    map[string]observer.TimingFacts{},
		Facts:     map[string]observer.TranscriptFacts{},
		globCalls: map[string]int{},
		pathCalls: map[string]int{},
		readCalls: map[string]int{},
	}
}

// FindByGlob implements observer.TranscriptSource.
func (f *Transcripts) FindByGlob(ctx context.Context, sessionID string) (observer.TranscriptLocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.globCalls[sessionID]++
	if f.Err != nil {
		return observer.TranscriptLocation{}, f.Err
	}
	return f.ByGlob[sessionID], ctx.Err()
}

// OpenPath implements observer.TranscriptSource.
func (f *Transcripts) OpenPath(ctx context.Context, path string) (observer.TranscriptLocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pathCalls[path]++
	if f.Err != nil {
		return observer.TranscriptLocation{}, f.Err
	}
	return f.ByPath[path], ctx.Err()
}

// ReadTiming implements observer.TranscriptSource.
func (f *Transcripts) ReadTiming(ctx context.Context, loc observer.TranscriptLocation, tailBytes int) (observer.TimingFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls[loc.Path]++
	if f.Err != nil {
		return observer.TimingFacts{}, f.Err
	}
	t, ok := f.Timing[loc.Path]
	if !ok {
		t = observer.TimingFacts{TailBytes: tailBytes, ReachedStart: true}
	}
	return t, ctx.Err()
}

// ReadFacts implements observer.TranscriptSource.
func (f *Transcripts) ReadFacts(ctx context.Context, loc observer.TranscriptLocation, tailBytes int) (observer.TranscriptFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls[loc.Path]++
	if f.Err != nil {
		return observer.TranscriptFacts{}, f.Err
	}
	facts, ok := f.Facts[loc.Path]
	if !ok {
		facts = observer.TranscriptFacts{Path: loc.Path, TailBytes: tailBytes}
	}
	return facts, ctx.Err()
}

// GlobCalls, PathCalls and ReadCalls expose the per-key call counts.
func (f *Transcripts) GlobCalls(sessionID string) int { return f.count(f.globCalls, sessionID) }

// PathCalls is how many times OpenPath was called for a path.
func (f *Transcripts) PathCalls(path string) int { return f.count(f.pathCalls, path) }

// ReadCalls is how many times either reader touched a path — the number that
// proves an unchanged transcript was not re-read.
func (f *Transcripts) ReadCalls(path string) int { return f.count(f.readCalls, path) }

func (f *Transcripts) count(m map[string]int, k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[k]
}

var _ observer.TranscriptSource = (*Transcripts)(nil)

// ---------------------------------------------------------------------------
// 4. AgentsSource
// ---------------------------------------------------------------------------

// Agents is a fake observer.AgentsSource.
//
// Spawns counts List calls, which is the number the single-flight and
// cross-process-lock tests are about: "N simultaneous cold-cache callers
// produce at most one spawn" is a claim about this counter.
type Agents struct {
	mu     sync.Mutex
	Result observer.AgentsResult
	Err    error
	// Block, when non-nil, is waited on before List returns — how a test makes
	// a spawn slow enough to overlap with another.
	Block  <-chan struct{}
	spawns int
}

// SetEntries replaces the listing, stamps ObservedAt and marks it OK.
func (f *Agents) SetEntries(at time.Time, entries ...observer.AgentsEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Result.Entries = entries
	f.Result.ObservedAt = observer.TruncateTime(at)
	f.Result.OK = true
	f.Result.Command = "claude agents --json"
}

// List implements observer.AgentsSource.
func (f *Agents) List(ctx context.Context) (observer.AgentsResult, error) {
	f.mu.Lock()
	block := f.Block
	f.spawns++
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return observer.AgentsResult{}, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return observer.AgentsResult{}, err
	}
	return f.Result, f.Err
}

// Spawns is how many times List was called.
func (f *Agents) Spawns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns
}

var _ observer.AgentsSource = (*Agents)(nil)

// ---------------------------------------------------------------------------
// 5. PanesLister
// ---------------------------------------------------------------------------

// Panes is a fake observer.PanesLister.
type Panes struct {
	mu    sync.Mutex
	set   map[string]bool
	Err   error
	calls int
}

// NewPanes returns a fake listing exactly the given pane ids, which must carry
// their sigils: "madbench:@51.%107", not "madbench:51.107".
func NewPanes(ids ...string) *Panes {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return &Panes{set: set}
}

// Set replaces the pane set.
func (f *Panes) Set(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = make(map[string]bool, len(ids))
	for _, id := range ids {
		f.set[id] = true
	}
}

// ListPanes implements observer.PanesLister. It returns a COPY: the core must
// never be able to mutate the fake's state, and a shared map under -race would
// report a data race that the production adapter cannot have.
func (f *Panes) ListPanes(ctx context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.Err != nil {
		return nil, f.Err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(f.set))
	for k, v := range f.set {
		out[k] = v
	}
	return out, nil
}

// Calls is how many times ListPanes was called. It must be once per rebuild,
// not once per session.
func (f *Panes) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ observer.PanesLister = (*Panes)(nil)

// ---------------------------------------------------------------------------
// 6. ProcLive
// ---------------------------------------------------------------------------

// Proc is a fake observer.ProcLive.
//
// Note the three distinguishable outcomes, which matter because an errored gate
// is NOT evidence of death: a pid in Alive, a pid not in Alive, and a pid in
// Errors.
type Proc struct {
	mu sync.Mutex
	// alive maps a pid to whether it is running. A pid absent from the map is
	// not running.
	alive map[int]bool
	// Errors maps a pid to an error the gate returns instead of an answer.
	Errors map[int]error
	calls  int

	// parents is the fake process tree: pid -> parent pid. A pid absent from
	// it ERRORS rather than answering 0 or 1, because that is what the real
	// lookup does for a pid it cannot see — and it keeps every test written
	// before ancestry existed on the safe side: no tree, nothing hidden.
	parents      map[int]int
	parentErrors map[int]error
	parentCalls  int
}

// errNoParent is what Parent answers for a pid the fake tree does not name.
var errNoParent = errors.New("fakes: no parent recorded for this pid")

// NewProc returns a fake reporting exactly the given pids alive.
func NewProc(alivePIDs ...int) *Proc {
	m := make(map[int]bool, len(alivePIDs))
	for _, p := range alivePIDs {
		m[p] = true
	}
	return &Proc{alive: m, Errors: map[int]error{}}
}

// SetAlive sets whether a pid is reported alive.
func (f *Proc) SetAlive(pid int, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alive == nil {
		f.alive = map[int]bool{}
	}
	f.alive[pid] = alive
}

// SetError makes the gate ERROR for a pid rather than answer.
func (f *Proc) SetError(pid int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Errors == nil {
		f.Errors = map[int]error{}
	}
	f.Errors[pid] = err
}

// Alive implements observer.ProcLive.
func (f *Proc) Alive(pid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err, ok := f.Errors[pid]; ok && err != nil {
		return false, err
	}
	return f.alive[pid], nil
}

// Calls is how many times Alive was called.
func (f *Proc) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// SetParent records ppid as pid's parent in the fake process tree.
func (f *Proc) SetParent(pid, ppid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parents == nil {
		f.parents = map[int]int{}
	}
	f.parents[pid] = ppid
}

// SetParentError makes the ancestry lookup ERROR for a pid rather than answer,
// which is the case the core must never read as evidence.
func (f *Proc) SetParentError(pid int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parentErrors == nil {
		f.parentErrors = map[int]error{}
	}
	f.parentErrors[pid] = err
}

// Parent implements observer.ProcLive.
func (f *Proc) Parent(pid int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parentCalls++
	if err, ok := f.parentErrors[pid]; ok && err != nil {
		return 0, err
	}
	ppid, ok := f.parents[pid]
	if !ok {
		return 0, errNoParent
	}
	return ppid, nil
}

// ParentCalls is how many times Parent was called, so a test can hold the
// ancestry walk to its bound.
func (f *Proc) ParentCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parentCalls
}

var _ observer.ProcLive = (*Proc)(nil)

// ---------------------------------------------------------------------------
// 7. Watcher
// ---------------------------------------------------------------------------

// Watcher is a fake observer.Watcher.
//
// It records every Reconcile call IN ORDER, with the full target set each one
// carried. That is the whole point: the correctness property this port has is
// about ORDER — an arm must precede the read it protects, and the only
// releasing call must come after the publish — and order cannot be asserted
// from a final state.
//
// ArmHook, if set, runs inside Reconcile BEFORE the targets are recorded. It is
// the seam that lets a test create a file in the window between the scan and
// the arm, which is the failure mode that loses an event permanently with no
// later one to recover it.
type Watcher struct {
	mu sync.Mutex

	changes chan observer.Change
	// Report is returned by Reconcile. The per-kind counts are filled in
	// automatically from the targets unless Report.ByKind is already set.
	Report observer.ArmReport
	// WatchErr and ReconcileErr let a test drive the degraded paths.
	WatchErr     error
	ReconcileErr error
	// ArmHook runs inside every Reconcile, before recording.
	ArmHook func(targets []observer.WatchTarget)
	// ReleaseHook runs inside every Release, before recording. Release is the
	// post-publication operation (code review round 2, A4), so this is the
	// seam a test uses to act in that phase.
	ReleaseHook func(keep []observer.WatchTarget)
	// Reopen makes a Watch() after Close() hand back a FRESH channel, which is
	// a watcher that genuinely recovered. It is false by default because the
	// DEFAULT models the real adapter's worst case: a Watch() that returns the
	// same already-closed channel with a nil error, which is what turned a
	// dead watch into a 1.08M-calls-per-second spin. A fake that could only
	// recover could not reproduce the defect.
	Reopen bool

	reconciles [][]observer.WatchTarget
	releases   [][]observer.WatchTarget
	armed      map[string]observer.TargetKind
	watchCalls int
	buffer     int
	closed     bool
}

// NewWatcher returns a fake with a buffered change channel.
func NewWatcher(buffer int) *Watcher {
	return &Watcher{
		changes: make(chan observer.Change, buffer),
		armed:   map[string]observer.TargetKind{},
		buffer:  buffer,
	}
}

// Watch implements observer.Watcher.
func (f *Watcher) Watch(ctx context.Context) (<-chan observer.Change, error) {
	f.mu.Lock()
	f.watchCalls++
	err := f.WatchErr
	if f.closed && f.Reopen {
		f.changes = make(chan observer.Change, f.buffer)
		f.closed = false
	}
	ch := f.changes
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
	}()
	return ch, nil
}

// Reconcile implements observer.Watcher.
func (f *Watcher) Reconcile(ctx context.Context, targets []observer.WatchTarget) (observer.ArmReport, error) {
	if f.ArmHook != nil {
		f.ArmHook(targets)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	snapshot := make([]observer.WatchTarget, len(targets))
	copy(snapshot, targets)
	f.reconciles = append(f.reconciles, snapshot)

	if f.ReconcileErr != nil {
		return observer.ArmReport{}, f.ReconcileErr
	}
	if err := ctx.Err(); err != nil {
		return observer.ArmReport{}, err
	}

	// Track the armed set the way the real adapter must: arm the new, release
	// what is gone, leave the rest untouched. "Untouched" is asserted by tests
	// through Armed() staying stable across calls, because closing and
	// reopening a descriptor discards the events already queued on it.
	want := make(map[string]observer.TargetKind, len(targets))
	for _, t := range targets {
		want[t.Path] = t.Kind
	}
	report := f.Report
	if report.ByKind == nil {
		report.ByKind = map[observer.TargetKind]int{}
		for _, t := range targets {
			report.ByKind[t.Kind]++
		}
	}
	for path := range want {
		if _, held := f.armed[path]; !held {
			report.Armed++
		}
	}
	for path := range f.armed {
		if _, still := want[path]; !still {
			report.Released++
		}
	}
	f.armed = want
	return report, nil
}

// Release implements observer.Watcher: it drops every armed target not in keep
// and arms nothing — a target in keep that is not armed stays unarmed, which
// is the whole difference from Reconcile.
func (f *Watcher) Release(ctx context.Context, keep []observer.WatchTarget) (observer.ArmReport, error) {
	if f.ReleaseHook != nil {
		f.ReleaseHook(keep)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	snapshot := make([]observer.WatchTarget, len(keep))
	copy(snapshot, keep)
	f.releases = append(f.releases, snapshot)

	if f.ReconcileErr != nil {
		return observer.ArmReport{}, f.ReconcileErr
	}
	if err := ctx.Err(); err != nil {
		return observer.ArmReport{}, err
	}
	want := make(map[string]observer.TargetKind, len(keep))
	for _, t := range keep {
		want[t.Path] = t.Kind
	}
	report := observer.ArmReport{ByKind: map[observer.TargetKind]int{}}
	next := make(map[string]observer.TargetKind, len(f.armed))
	for path, kind := range f.armed {
		if k, still := want[path]; still && k == kind {
			next[path] = kind
			report.ByKind[kind]++
			continue
		}
		report.Released++
	}
	f.armed = next
	return report, nil
}

// Releases returns every Release call's keep set, in call order.
func (f *Watcher) Releases() [][]observer.WatchTarget {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]observer.WatchTarget, len(f.releases))
	for i, r := range f.releases {
		out[i] = append([]observer.WatchTarget(nil), r...)
	}
	return out
}

// Emit queues a change hint, as the kernel would. The channel is read under
// the mutex because Reopen can replace it.
func (f *Watcher) Emit(c observer.Change) {
	f.mu.Lock()
	ch := f.changes
	f.mu.Unlock()
	ch <- c
}

// Close closes the change channel, the way a watcher whose loop has died does.
// It is idempotent, because a test that kills a watch twice is testing the
// caller rather than the fake.
func (f *Watcher) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.changes)
}

// Reconciles returns every Reconcile call's target set, in call order.
func (f *Watcher) Reconciles() [][]observer.WatchTarget {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]observer.WatchTarget, len(f.reconciles))
	copy(out, f.reconciles)
	return out
}

// Armed returns the currently armed paths and their kinds.
func (f *Watcher) Armed() map[string]observer.TargetKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]observer.TargetKind, len(f.armed))
	for k, v := range f.armed {
		out[k] = v
	}
	return out
}

// WatchCalls is how many times Watch was called. More than once per observer is
// a leak.
func (f *Watcher) WatchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchCalls
}

var _ observer.Watcher = (*Watcher)(nil)

// ---------------------------------------------------------------------------
// Convenience
// ---------------------------------------------------------------------------

// Deps builds an observer.Deps with a fake in every slot and a clock reading
// `now`. It returns the struct and the fakes, so a test can hand the struct to
// the core and keep the handles for assertions.
//
// There is one of these rather than eight constructor calls per test for a
// reason that is not convenience: a new port added to observer.Deps must be
// wired up HERE, once, and every existing test keeps compiling. Without it,
// adding the eighth port would mean touching every test file.
func Deps(now time.Time) (observer.Deps, *Set) {
	set := &Set{
		Clock:       NewClock(now),
		Registry:    &Registry{},
		Jobs:        &Jobs{},
		Transcripts: NewTranscripts(),
		Agents:      &Agents{},
		Panes:       NewPanes(),
		Proc:        NewProc(),
		Watcher:     NewWatcher(16),
	}
	set.Registry.Result.ObservedAt = observer.TruncateTime(now)
	set.Jobs.Result.ObservedAt = observer.TruncateTime(now)
	return observer.Deps{
		Registry:    set.Registry,
		Jobs:        set.Jobs,
		Transcripts: set.Transcripts,
		Agents:      set.Agents,
		Panes:       set.Panes,
		Proc:        set.Proc,
		Watch:       set.Watcher,
		Clock:       set.Clock,
	}, set
}

// Set holds every fake built by Deps, for assertions.
type Set struct {
	Clock       *Clock
	Registry    *Registry
	Jobs        *Jobs
	Transcripts *Transcripts
	Agents      *Agents
	Panes       *Panes
	Proc        *Proc
	Watcher     *Watcher
}
