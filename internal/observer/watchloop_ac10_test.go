//go:build darwin

package observer_test

// AC10's end-to-end cases — 5, 6, 7, 8, 9 and the fd half of 10 — against a
// REAL kqueue watcher over a REAL scratch tree, with a REAL subscriber reading
// frames.
//
// Nothing here touches ~/.claude/. Every tree is t.TempDir().
//
// Cases 5 and 6 were round 2's C1 regression tests. Cases 7-9 exist because 5
// and 6 have the same blind spot one level down: both complete Reconcile before
// mutating anything, so they exercise only the benign `a < m` half of §6.3's
// two-case proof. A test that cannot fail on the defect it is named after is
// worse than no test, because it certifies. Cases 7-9 inject their mutation
// INSIDE the scan/arm window, and each ships with the paired control that
// performs the identical mutation just AFTER the arm — a case that passed only
// in the `m < a` ordering would mean the harness, not the protocol, was doing
// the work.
//
// WHAT IS TEST-LOCAL HERE, AND WHY. The background half of the join is P3's
// deliverable and did not exist when this was written, so the jobs reader and
// the jobs-to-session assembly below are test-local. They are deliberately thin
// and they use the DOMAIN's own mapping and liveness functions — MapJobState,
// Decide, IncludedByDefault — so what is being asserted is the ordering
// protocol, the watcher, the diff and the fan-out, all of which are P5's. When
// jobsfs lands, jobsCore.Snapshot becomes a call into the real background join
// and every assertion below stands unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

// ---------------------------------------------------------------------------
// The test-local jobs reader: depth 1, parse by key NAME, nothing deeper
// ---------------------------------------------------------------------------

// tinyJobs also honours §6.5's keep-previous rule, because the loop's
// behaviour on a torn read is only meaningful against a source that does: a
// parse failure means TORN, not dead, and the record keeps its previous known
// state. The REAL mechanism is jobsfs's (P3) — this is the port contract, held
// to by a stand-in so the loop's half of the bound can be asserted at all.
type tinyJobs struct {
	root string

	mu          sync.Mutex
	prev        map[string]observer.JobRecord
	parseErrors int
}

func (j *tinyJobs) Read(ctx context.Context) (observer.JobsResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.prev == nil {
		j.prev = map[string]observer.JobRecord{}
	}
	res := observer.JobsResult{}
	entries, err := os.ReadDir(j.root)
	if err != nil {
		return res, nil // absent is a fact
	}
	for _, e := range entries {
		res.Entries++
		if !e.IsDir() {
			continue
		}
		res.Dirs++
		data, err := os.ReadFile(filepath.Join(j.root, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(data, &keys); err != nil {
			// §6.5: KEEP the previous known state for this record rather than
			// reporting it gone. "One malformed record blanking everything" is
			// a real prior failure in this repository.
			res.ParseErrors++
			j.parseErrors++
			if kept, ok := j.prev[e.Name()]; ok {
				res.Records = append(res.Records, kept)
			}
			continue
		}
		rec := observer.JobRecord{Dir: e.Name()}
		str := func(key string) string {
			raw, ok := keys[key]
			if !ok {
				return ""
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return ""
			}
			return s
		}
		rec.SessionID = str("sessionId")
		rec.State = str("state")
		rec.DaemonShort = str("daemonShort")
		rec.Name = str("name")
		rec.LinkScanPath = str("linkScanPath")
		if ts := str("updatedAt"); ts != "" {
			if parsed, err := observer.ParseTime(ts); err == nil {
				rec.UpdatedAt = &parsed
			}
		}
		if rec.SessionID == "" {
			res.ParseErrors++
			continue
		}
		j.prev[e.Name()] = rec
		res.Records = append(res.Records, rec)
		if st, _ := observer.MapJobState(rec.State); !st.IsTerminal() {
			res.NonTerminal++
		}
	}
	sort.Slice(res.Records, func(a, b int) bool { return res.Records[a].Dir < res.Records[b].Dir })
	return res, nil
}

// jobsCore is the Snapshotter: the REAL Observer for the interactive half, plus
// the background sessions the jobs tree describes.
type jobsCore struct {
	core   *observer.Observer
	jobs   observer.JobsSource
	clock  observer.Clock
	policy observer.Policy
}

func (c *jobsCore) Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	snap, err := c.core.Snapshot(ctx, opts)
	if err != nil {
		return nil, err
	}
	res, err := c.jobs.Read(ctx)
	if err != nil {
		return snap, nil
	}
	now := c.clock.Now()
	for _, rec := range res.Records {
		status, wait := observer.MapJobState(rec.State)
		verdict := observer.Decide(observer.Evidence{
			Kind:         observer.KindBackground,
			Status:       status,
			RawState:     rec.State,
			HasJobRecord: true,
		}, now, c.policy)
		if !observer.IncludedByDefault(verdict.Live, opts) {
			continue
		}
		short := rec.DaemonShort
		sess := observer.Session{
			SessionID:    rec.SessionID,
			ShortID:      &short,
			Kind:         observer.KindBackground,
			Name:         rec.Name,
			Status:       status,
			RawStatus:    rec.State,
			WaitReason:   wait,
			StatusSource: "jobs",
			StatusSince:  rec.UpdatedAt,
			Attention:    status.IsAttention(),
			Live:         verdict.Live,
			LiveReason:   verdict.Reason,
			ListedBy:     []string{"jobs"},
			Sources:      []string{"jobs"},
			ObservedAt:   snap.ObservedAt,
			Advisories:   []observer.Advisory{},
		}
		sess.StatusAgeMs = observer.AgeMs(snap.ObservedAt, rec.UpdatedAt)
		snap.Sessions = append(snap.Sessions, sess)
	}
	sort.Slice(snap.Sessions, func(a, b int) bool {
		return snap.Sessions[a].SessionID < snap.Sessions[b].SessionID
	})
	return snap, nil
}

// ---------------------------------------------------------------------------
// The armHook seam — §9.2(b)'s own design, verbatim in intent
// ---------------------------------------------------------------------------

// armHook wraps the real adapter and fires a test-supplied callback AFTER the
// core has computed the desired set and BEFORE the adapter arms it — i.e.
// precisely at scan < m < a.
//
// `when` matters: the waves are distinguishable by the target kinds they carry,
// and firing on the first Reconcile of a rebuild would land the mutation in
// WAVE 0's window, which proves nothing. This is the one place in the suite
// where hexagonal pays for itself in something other than tidiness — the window
// is a few hundred microseconds wide, it cannot be hit by sleeping, and there
// is no way to wedge a callback into it without a seam at the port.
type armHook struct {
	observer.Watcher
	once   sync.Once
	when   observer.TargetKind
	inside func([]observer.WatchTarget)
	// after runs immediately AFTER the real arm returns. It is the paired
	// control: the identical mutation in the benign ordering.
	after func([]observer.WatchTarget)

	mu    sync.Mutex
	fired bool
}

func (h *armHook) Reconcile(ctx context.Context, targets []observer.WatchTarget) (observer.ArmReport, error) {
	hit := false
	for _, t := range targets {
		if t.Kind == h.when {
			hit = true
			break
		}
	}
	if hit {
		h.once.Do(func() {
			h.mu.Lock()
			h.fired = true
			h.mu.Unlock()
			if h.inside != nil {
				h.inside(targets)
			}
		})
	}
	rep, err := h.Watcher.Reconcile(ctx, targets)
	if hit && h.after != nil {
		h.after(targets)
		h.after = nil
	}
	return rep, err
}

func (h *armHook) didFire() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fired
}

// ---------------------------------------------------------------------------
// The harness: a real watcher, a real tree, a real loop, a real subscriber
// ---------------------------------------------------------------------------

type e2e struct {
	t       *testing.T
	root    string
	reg     string
	jobs    string
	loop    *observer.WatchLoop
	watcher *watchkq.Watcher
	events  <-chan observer.Event
	clock   *fakes.Clock
	cancel  context.CancelFunc
	done    chan error
}

func newE2E(t *testing.T, wrap func(observer.Watcher) observer.Watcher) *e2e {
	t.Helper()

	root := t.TempDir()
	reg := filepath.Join(root, "sessions")
	jobs := filepath.Join(root, "jobs")
	for _, d := range []string{reg, jobs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	deps, set := fakes.Deps(loopT0)
	clock := set.Clock
	// One interactive session with no tmux reference, so the pane gate is never
	// asked and no tmux command is issued by any test in this file. It is there
	// so the diff has to be selective rather than trivially empty.
	set.Proc.SetAlive(os.Getpid(), true)
	set.Registry.SetRecords(loopT0, observer.RegistryRecord{
		Path: filepath.Join("/scratch", "4242.json"), PID: os.Getpid(),
		SessionID: "aaaaaaaa-1111-2222-3333-444444444444",
		Status:    "idle", StatusUpdatedAt: timePtr(loopT0.Add(-time.Hour)),
		Name: "interactive-neighbour", NameSource: "user", CWD: root,
	})

	core, err := observer.New(deps, observer.Config{
		Paths: observer.Paths{ClaudeRoot: root, Registry: reg, Jobs: jobs, Projects: root},
	})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}

	w, err := watchkq.New(watchkq.Caps{})
	if err != nil {
		t.Skipf("no kqueue on this platform: %v", err)
	}

	var watcher observer.Watcher = w
	if wrap != nil {
		watcher = wrap(w)
	}

	// ONE jobs reader, shared by the core and by wave 3's discovery read, so
	// the keep-previous state is not duplicated.
	jobSource := &tinyJobs{root: jobs}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:  &jobsCore{core: core, jobs: jobSource, clock: clock, policy: core.Policy()},
		Watch: watcher,
		Tree:  watchkq.OSTree{},
		Jobs:  jobSource,
		Clock: clock,
		Paths: observer.Paths{ClaudeRoot: root, Registry: reg, Jobs: jobs, Projects: root},
	})
	if err != nil {
		t.Fatalf("NewWatchLoop: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Subscribe BEFORE the loop runs, so a mutation injected into the FIRST
	// rebuild's arm window is observable at all.
	//
	// The first publication reaches a subscriber as ONE baseline snapshot, not
	// as one session_added per session (the black-box fix: an empty first
	// publication used to reach it as nothing whatsoever). This harness expands
	// that baseline into the additions a client builds from it, so every case
	// below still asserts "the client learned of X" — whichever publication
	// carried X, and whether it arrived in the baseline or as a later delta.
	raw, err := loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	events := expandBaselines(ctx, raw)

	h := &e2e{
		t: t, root: root, reg: reg, jobs: jobs,
		loop: loop, watcher: w, events: events, clock: clock,
		cancel: cancel, done: make(chan error, 1),
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
		}
		_ = w.Close()
	})
	return h
}

// expandBaselines relays a subscription, turning each baseline snapshot into
// one session_added per session visible in it — what a client reconstructs
// from a baseline — and passing every other event through untouched. It exits
// when the subscription closes or ctx ends, and closes its output either way.
func expandBaselines(ctx context.Context, in <-chan observer.Event) <-chan observer.Event {
	out := make(chan observer.Event, 256)
	go func() {
		defer close(out)
		send := func(e observer.Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for e := range in {
			if e.Type != observer.EventSnapshot || e.Snapshot == nil {
				if !send(e) {
					return
				}
				continue
			}
			for i := range e.Snapshot.Sessions {
				s := e.Snapshot.Sessions[i]
				if !observer.Visible(s, observer.Filter{}) {
					continue
				}
				if !send(observer.Event{
					Type: observer.EventSessionAdded, SessionID: s.SessionID, Session: &s, At: e.At,
				}) {
					return
				}
			}
		}
	}()
	return out
}

// runWith starts the loop on a context the caller owns, and waits for the
// startup rebuild to have published.
func (h *e2e) runWith(ctx context.Context) {
	h.t.Helper()
	go func() { h.done <- h.loop.Run(ctx) }()
	waitFor(h.t, 3*time.Second, func() bool { return h.loop.Rebuilds() >= 1 })
}

// await returns the first event satisfying want, or fails.
func (h *e2e) await(within time.Duration, what string, want func(observer.Event) bool) observer.Event {
	h.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case e, ok := <-h.events:
			if !ok {
				h.t.Fatalf("the event channel closed while waiting for %s", what)
			}
			if want(e) {
				return e
			}
			h.t.Logf("(ignored %s %s)", e.Type, e.SessionID)
		case <-deadline:
			h.t.Fatalf("no %s within %v", what, within)
		}
	}
}

// expectQuiet asserts that nothing arrives for d.
func (h *e2e) expectQuiet(d time.Duration) {
	h.t.Helper()
	select {
	case e, ok := <-h.events:
		if !ok {
			return
		}
		h.t.Fatalf("an unrelated frame arrived on a quiescent tree: %s %s %v",
			e.Type, e.SessionID, e.Changed)
	case <-time.After(d):
	}
}

func (h *e2e) mkJobDir(name string) string {
	h.t.Helper()
	dir := filepath.Join(h.jobs, name)
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	return dir
}

// publishRecord writes a job record the way the jobs tree actually publishes
// one: a temp file in the job directory, renamed over the record. Measured, not
// assumed: the job directory's mtime equals state.json's in 5 of 7 records and
// is never earlier, which an in-place rewrite cannot produce.
func (h *e2e) publishRecord(dir, sessionID, state string) {
	h.t.Helper()
	body := fmt.Sprintf(`{"sessionId":%q,"daemonShort":%q,"state":%q,"name":%q,"updatedAt":%q}`,
		sessionID, sessionID[:8], state, "job-"+sessionID[:4], observer.FormatTime(loopT0))
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "state.json")); err != nil {
		h.t.Fatal(err)
	}
}

// requireNeighbourPresent asserts the unrelated interactive session is in the
// published snapshot, so "no unrelated frame arrived" cannot be satisfied by
// the observer having lost track of it.
func (h *e2e) requireNeighbourPresent() {
	h.t.Helper()
	snap := h.loop.Latest()
	if snap == nil {
		h.t.Fatal("nothing published")
	}
	if _, ok := snap.SessionByID("aaaaaaaa-1111-2222-3333-444444444444"); !ok {
		h.t.Fatal("the unrelated interactive session vanished from the snapshot")
	}
}

func isAddOf(id string) func(observer.Event) bool {
	return func(e observer.Event) bool {
		return e.Type == observer.EventSessionAdded && e.SessionID == id
	}
}

func isRemoveOf(id string) func(observer.Event) bool {
	return func(e observer.Event) bool {
		return e.Type == observer.EventSessionRemoved && e.SessionID == id
	}
}

const (
	jobA = "bbbbbbbb-1111-2222-3333-444444444444"
	jobB = "cccccccc-1111-2222-3333-444444444444"
)

// ---------------------------------------------------------------------------
// Case 5 — C1a: a record-less directory gains its first record
// ---------------------------------------------------------------------------

func TestAC10Case5ARecordLessJobDirectoryGainingItsFirstRecordReachesTheClient(t *testing.T) {
	h := newE2E(t, nil)
	dir := h.mkJobDir("abcd1234") // only tmp/, no state.json — 7 of 14 on the live machine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	// Drain the startup publication.
	h.await(2*time.Second, "the startup frame for the interactive neighbour",
		isAddOf("aaaaaaaa-1111-2222-3333-444444444444"))

	start := time.Now()
	h.publishRecord(dir, jobA, "blocked")

	e := h.await(2*time.Second, "session_added for the first record", isAddOf(jobA))
	if e.Session == nil || e.Session.Status != observer.StatusBlocked {
		t.Fatalf("session_added carries %+v, want status blocked", e.Session)
	}
	t.Logf("client-visible latency for a first record in a record-less directory: %v",
		time.Since(start))
}

// ---------------------------------------------------------------------------
// Case 6 — C1b: terminal -> resumed, twice, which is the whole of C1
// ---------------------------------------------------------------------------

func TestAC10Case6ATerminalJobResumingReachesTheClientEveryTime(t *testing.T) {
	h := newE2E(t, nil)
	dir := h.mkJobDir("abcd1234")
	// Start from a done record, so the session is absent from the client's view.
	h.publishRecord(dir, jobA, "done")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)
	h.await(2*time.Second, "the startup frame",
		isAddOf("aaaaaaaa-1111-2222-3333-444444444444"))

	if snap := h.loop.Latest(); snap != nil {
		if s, ok := snap.SessionByID(jobA); ok && observer.Visible(s, observer.Filter{}) {
			t.Fatal("a done job is visible to a subscriber before it resumes")
		}
	}

	for round := 1; round <= 2; round++ {
		// Resume.
		h.publishRecord(dir, jobA, "blocked")
		e := h.await(2*time.Second, fmt.Sprintf("session_added on resume %d", round), isAddOf(jobA))
		if e.Session == nil || e.Session.Status != observer.StatusBlocked {
			t.Fatalf("resume %d: session_added carries %+v, want status blocked", round, e.Session)
		}

		// And terminate again.
		h.publishRecord(dir, jobA, "done")
		r := h.await(2*time.Second, fmt.Sprintf("session_removed on round %d", round), isRemoveOf(jobA))
		if r.Reason != observer.RemovedJobTerminal {
			t.Errorf("round %d: removal reason %q, want %q", round, r.Reason, observer.RemovedJobTerminal)
		}
	}

	// The second round is the whole of C1: revision 1 released the watch on a
	// terminal directory and would have passed the first resume by luck if the
	// watch had not yet been reconciled, then failed the second.
	armed := h.loop.Armed()
	if kind, ok := armed[dir]; !ok || kind != observer.TargetJobDir {
		t.Fatalf("the job directory is not armed after two terminal transitions (%v): a "+
			"terminal job measurably resumes — 17 times across 6 of 7 timelines", armed)
	}
}

// ---------------------------------------------------------------------------
// Case 7 — C1a': the first record, injected INSIDE the arm window
// ---------------------------------------------------------------------------

func TestAC10Case7AFirstRecordInjectedInsideTheArmWindowStillReachesTheClient(t *testing.T) {
	var h *e2e
	var hook *armHook
	h = newE2E(t, func(real observer.Watcher) observer.Watcher {
		hook = &armHook{
			Watcher: real,
			when:    observer.TargetJobDir,
			inside: func([]observer.WatchTarget) {
				// After the enumeration that discovered the directory, before
				// the directory is armed. Without Invariant A this case hangs:
				// §0.5 E2 measured that the mutation fires nothing and the read
				// has already happened.
				h.publishRecord(filepath.Join(h.jobs, "abcd1234"), jobA, "blocked")
			},
		}
		return hook
	})
	h.mkJobDir("abcd1234")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	e := h.await(3*time.Second, "session_added for a record written inside the arm window",
		isAddOf(jobA))
	if e.Session == nil || e.Session.Status != observer.StatusBlocked {
		t.Fatalf("session_added carries %+v, want status blocked", e.Session)
	}
	if !hook.didFire() {
		t.Fatal("the hook never fired, so the mutation was not in the window and this test " +
			"proved nothing")
	}
	// Nothing else. The interactive neighbour arrived in the same publication
	// (await above logged and skipped it), so its presence is asserted against
	// the published snapshot rather than against the stream, and the stream is
	// then required to be silent.
	h.requireNeighbourPresent()
	h.expectQuiet(time.Second)
}

func TestAC10Case7TheControlPerformsTheSameMutationJustAfterTheArm(t *testing.T) {
	var h *e2e
	var hook *armHook
	h = newE2E(t, func(real observer.Watcher) observer.Watcher {
		hook = &armHook{
			Watcher: real,
			when:    observer.TargetJobDir,
			after: func([]observer.WatchTarget) {
				h.publishRecord(filepath.Join(h.jobs, "abcd1234"), jobA, "blocked")
			},
		}
		return hook
	})
	h.mkJobDir("abcd1234")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	// The benign ordering, `a < m`, must pass too — a case that passed only in
	// one ordering would mean the harness was doing the work.
	e := h.await(3*time.Second, "session_added in the m > a control", isAddOf(jobA))
	if e.Session == nil || e.Session.Status != observer.StatusBlocked {
		t.Fatalf("session_added carries %+v, want status blocked", e.Session)
	}
}

func TestAC10Case7TheQuiescentControlProducesNoFrameAtAll(t *testing.T) {
	var hook *armHook
	h := newE2E(t, func(real observer.Watcher) observer.Watcher {
		hook = &armHook{Watcher: real, when: observer.TargetJobDir, inside: func([]observer.WatchTarget) {}}
		return hook
	})
	h.mkJobDir("abcd1234")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	h.await(2*time.Second, "the interactive neighbour",
		isAddOf("aaaaaaaa-1111-2222-3333-444444444444"))
	// The hook is installed and mutates nothing. No further frame may arrive,
	// or the two cases above are measuring noise.
	h.expectQuiet(1500 * time.Millisecond)
	if !hook.didFire() {
		t.Error("the hook never fired even in the control, so `when` is wrong and cases 7-9 " +
			"are landing in the wrong wave")
	}
}

// ---------------------------------------------------------------------------
// Case 8 — C1b': terminal -> resumed, injected INSIDE the arm window
// ---------------------------------------------------------------------------

func TestAC10Case8AResumeInjectedInsideTheArmWindowStillReachesTheClient(t *testing.T) {
	var h *e2e
	var hook *armHook
	h = newE2E(t, func(real observer.Watcher) observer.Watcher {
		hook = &armHook{
			Watcher: real,
			when:    observer.TargetJobDir,
			inside: func([]observer.WatchTarget) {
				h.publishRecord(filepath.Join(h.jobs, "abcd1234"), jobA, "blocked")
			},
		}
		return hook
	})
	dir := h.mkJobDir("abcd1234")
	// A done record, whose session is absent from the client's view, replaced
	// inside the window. This is round 2's measured resumption in the ordering
	// revision 2 could still lose it in.
	h.publishRecord(dir, jobA, "done")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	e := h.await(3*time.Second, "session_added for a resume inside the arm window", isAddOf(jobA))
	if e.Session == nil || e.Session.Status != observer.StatusBlocked {
		t.Fatalf("session_added carries %+v, want status blocked", e.Session)
	}
	if !hook.didFire() {
		t.Fatal("the hook never fired")
	}
	h.requireNeighbourPresent()
	h.expectQuiet(time.Second)
}

func TestAC10Case8TheControlResumesJustAfterTheArm(t *testing.T) {
	var h *e2e
	h = newE2E(t, func(real observer.Watcher) observer.Watcher {
		return &armHook{
			Watcher: real,
			when:    observer.TargetJobDir,
			after: func([]observer.WatchTarget) {
				h.publishRecord(filepath.Join(h.jobs, "abcd1234"), jobA, "blocked")
			},
		}
	})
	dir := h.mkJobDir("abcd1234")
	h.publishRecord(dir, jobA, "done")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	if e := h.await(3*time.Second, "session_added in the m > a control", isAddOf(jobA)); e.Session.Status != observer.StatusBlocked {
		t.Fatalf("status %q, want blocked", e.Session.Status)
	}
}

// ---------------------------------------------------------------------------
// Case 9 — C1c': a NEW directory whose record lands before the arm
// ---------------------------------------------------------------------------

func TestAC10Case9ANewDirectoryAndItsRecordBothInsideTheArmWindow(t *testing.T) {
	var h *e2e
	var hook *armHook
	h = newE2E(t, func(real observer.Watcher) observer.Watcher {
		hook = &armHook{
			Watcher: real,
			when:    observer.TargetJobDir,
			inside: func([]observer.WatchTarget) {
				// The DISCOVERY path rather than the startup path. §0.5 E4
				// measured that the root watch hears the mkdir and is DEAF to
				// the record, so this case fails for a different reason than 7
				// and 8 if the protocol is wrong.
				dir := h.mkJobDir("newdir00")
				h.publishRecord(dir, jobB, "blocked")
			},
		}
		return hook
	})
	h.mkJobDir("abcd1234") // one pre-existing directory, so wave 1 has a set to arm

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	h.runWith(ctx)

	e := h.await(3*time.Second, "session_added for a directory AND record created inside the window",
		isAddOf(jobB))
	if e.Session == nil || e.Session.Status != observer.StatusBlocked {
		t.Fatalf("session_added carries %+v, want status blocked", e.Session)
	}
	if !hook.didFire() {
		t.Fatal("the hook never fired")
	}
	// Both are passes; a timeout is the failure. The latency and the iteration
	// count together say which rebuild caught it.
	t.Logf("latency %v, fixed-point iterations on the rebuild that published it: %d "+
		"(1 or 2 = the same rebuild's fixed point caught it; a later rebuild = the root's "+
		"queued hint did)", time.Since(start), h.loop.LastArmReport().Iterations)

	armed := h.loop.Armed()
	if _, ok := armed[filepath.Join(h.jobs, "newdir00")]; !ok {
		t.Error("the new directory is not armed after the rebuild that reported its record")
	}
}

func TestAC10Case9TheControlCreatesBothJustAfterTheArm(t *testing.T) {
	var h *e2e
	h = newE2E(t, func(real observer.Watcher) observer.Watcher {
		return &armHook{
			Watcher: real,
			when:    observer.TargetJobDir,
			after: func([]observer.WatchTarget) {
				dir := h.mkJobDir("newdir00")
				h.publishRecord(dir, jobB, "blocked")
			},
		}
	})
	h.mkJobDir("abcd1234")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)

	if e := h.await(3*time.Second, "session_added in the m > a control", isAddOf(jobB)); e.Session.Status != observer.StatusBlocked {
		t.Fatalf("status %q, want blocked", e.Session.Status)
	}
}

// ---------------------------------------------------------------------------
// Case 10's fd half — against the real adapter, because nothing else notices
// ---------------------------------------------------------------------------

func TestAC10Case10ASecondRebuildClosesAndReopensNothing(t *testing.T) {
	h := newE2E(t, nil)
	for i := 0; i < 14; i++ {
		dir := h.mkJobDir(fmt.Sprintf("job%02d", i))
		h.publishRecord(dir, fmt.Sprintf("%08d-1111-2222-3333-444444444444", i), "blocked")
	}
	if err := os.WriteFile(filepath.Join(h.reg, "4242.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, first, err := h.loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	} else if first.Iterations != 2 {
		t.Errorf("first rebuild took %d iterations, want 2", first.Iterations)
	}

	fdsBefore := h.watcher.FDs()
	gensBefore := h.watcher.Generations()
	if len(fdsBefore) != 1+1+14+14+1 {
		t.Fatalf("armed %d targets, want 1 registry dir + 1 registry record + 14 job dirs + "+
			"14 job records + 1 jobs root: %v", len(fdsBefore), fdsBefore)
	}

	if _, second, err := h.loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	} else if second.Iterations != 1 {
		t.Errorf("the steady state took %d iterations, want 1", second.Iterations)
	}

	for path, fd := range fdsBefore {
		if now, held := h.watcher.FDs()[path]; !held || now != fd {
			t.Errorf("%s: fd %d -> %v across an unchanged rebuild", path, fd, now)
		}
	}
	for path, gen := range gensBefore {
		if now := h.watcher.Generations()[path]; now != gen {
			t.Errorf("%s was re-armed (generation %d -> %d) although nothing changed; "+
				"§0.5 E5 measured that close(fd) discards every queued event, and nothing "+
				"else in this suite would notice", path, gen, now)
		}
	}
	t.Logf("%d descriptors held across two rebuilds, none closed and reopened", len(fdsBefore))
}

// ---------------------------------------------------------------------------
// §6.5's torn-read bound, at the jobs tree's unlink-then-create window
// ---------------------------------------------------------------------------

func TestATornJobRecordMeansTornAndNeverMeansGone(t *testing.T) {
	h := newE2E(t, nil)
	dir := h.mkJobDir("abcd1234")
	h.publishRecord(dir, jobA, "blocked")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx)
	h.await(2*time.Second, "the job's first frame", isAddOf(jobA))

	// A half-written record: the exact shape an unlink-then-create publish
	// leaves behind, and the shape an in-place rewrite leaves mid-write.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"sessionId":"bbbb`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return h.loop.Rebuilds() >= 2 })

	// A parse failure is TORN, not dead. The session must not be removed: "one
	// malformed record blanking everything" is a real prior failure, and
	// removal requires positive evidence.
	select {
	case e := <-h.events:
		if e.Type == observer.EventSessionRemoved && e.SessionID == jobA {
			t.Fatal("an unparseable record removed the session; removal requires POSITIVE " +
				"evidence — a record gone AND a dead pid, or a terminal state")
		}
	case <-time.After(500 * time.Millisecond):
	}

	// And the watch on the directory is still held, so the next good record is
	// still seen.
	if _, ok := h.loop.Armed()[dir]; !ok {
		t.Fatal("the job directory watch was released while its record was unreadable")
	}
	h.publishRecord(dir, jobA, "working")
	h.await(3*time.Second, "the recovery frame once the record parses again", func(e observer.Event) bool {
		return e.SessionID == jobA &&
			(e.Type == observer.EventSessionChanged || e.Type == observer.EventSessionAdded)
	})
}
