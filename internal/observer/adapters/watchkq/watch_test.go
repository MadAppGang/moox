//go:build darwin || linux

package watchkq_test

// AC10's mechanism cases, against a REAL kqueue watcher in t.TempDir().
//
// Nothing here goes near ~/.claude/. Every tree is scratch, created by the test
// and removed with it, which is also how §0.5's measurements were taken.
//
// Two of these tests are negative controls that must NOT see an event, and they
// are the point of the file: §6.3 establishes that the registry is rewritten in
// place and that a directory vnode receives nothing on an in-place write, so a
// directory-only watch "would never fire at all — the worse of the two failure
// modes, and one that passes a naive test". A suite without the negative
// controls certifies rather than tests.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
)

const (
	// mustArrive is AC10's own bound: "a status transition produces an event
	// within ~1 s".
	mustArrive = time.Second
	// mustNotArrive is deliberately LONGER than mustArrive. A negative control
	// that waited less than the positive case would pass by being impatient.
	mustNotArrive = 2 * time.Second
)

// newWatcher builds a watcher whose lifetime is the test's.
func newWatcher(t *testing.T) (*watchkq.Watcher, context.Context) {
	t.Helper()
	w, err := watchkq.New(watchkq.Caps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = w.Close()
	})
	return w, ctx
}

func watching(t *testing.T, w *watchkq.Watcher, ctx context.Context) <-chan observer.Change {
	t.Helper()
	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	return ch
}

// awaitChange waits for one hint and reports how long it took.
func awaitChange(t *testing.T, ch <-chan observer.Change, within time.Duration) (observer.Change, time.Duration, bool) {
	t.Helper()
	start := time.Now()
	select {
	case c, ok := <-ch:
		if !ok {
			return observer.Change{}, time.Since(start), false
		}
		return c, time.Since(start), true
	case <-time.After(within):
		return observer.Change{}, time.Since(start), false
	}
}

func reconcile(t *testing.T, w *watchkq.Watcher, ctx context.Context, targets ...observer.WatchTarget) observer.ArmReport {
	t.Helper()
	rep, err := w.Reconcile(ctx, targets)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return rep
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// rewriteInPlace reopens an existing file for writing WITHOUT truncating or
// replacing it and overwrites its bytes, which is the mechanism three
// independent lines of evidence say the registry uses. The caller asserts the
// inode is unchanged; otherwise the test is not testing what it claims.
func rewriteInPlace(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for in-place rewrite: %v", err)
	}
	if _, err := f.WriteAt([]byte(content), 0); err != nil {
		_ = f.Close()
		t.Fatalf("in-place write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func sameFile(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("%s was REPLACED, not rewritten in place: the test is not exercising the mechanism it names", path)
	}
}

// ---------------------------------------------------------------------------
// AC10 case 1 — the in-place rewrite, with the inode asserted unchanged
// ---------------------------------------------------------------------------

func TestAC10Case1AnInPlaceRewriteOfARegistryRecordFiresWithinOneSecond(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "4242.json")
	write(t, rec, `{"status":"idle"}`)

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec},
	)

	before, err := os.Stat(rec)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	rewriteInPlace(t, rec, `{"status":"busy"}`)
	sameFile(t, rec, before)

	c, latency, ok := awaitChange(t, ch, mustArrive)
	if !ok {
		t.Fatal("no Change within 1s for an in-place rewrite of an armed registry record")
	}
	if c.Kind != observer.TargetRegistryFile {
		t.Errorf("Change.Kind = %v, want registry-file", c.Kind)
	}
	if c.Ref != rec {
		t.Errorf("Change.Ref = %q, want %q", c.Ref, rec)
	}
	t.Logf("observed latency for an in-place rewrite: %v (bound %v)", latency, mustArrive)
}

// ---------------------------------------------------------------------------
// AC10 case 2 — the negative control that pins §6.3's central finding
// ---------------------------------------------------------------------------

func TestAC10Case2ADirectoryOnlyWatchNeverSeesAnInPlaceRewrite(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "4242.json")
	write(t, rec, `{"status":"idle"}`)

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	// The DIRECTORY only. This is revision 0's naive test, and it must fail to
	// see the transition.
	reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir})

	before, err := os.Stat(rec)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	rewriteInPlace(t, rec, `{"status":"busy"}`)
	sameFile(t, rec, before)

	if c, _, ok := awaitChange(t, ch, mustNotArrive); ok {
		t.Fatalf("a directory-only watch saw an in-place file rewrite (%+v) — either the "+
			"kernel changed or this test is not rewriting in place; the per-file arming "+
			"loop cannot be dropped on the strength of this passing", c)
	}
	t.Log("confirmed: the directory vnode receives nothing on an in-place write, so the " +
		"per-file registry watch is load-bearing and not redundancy")
}

// ---------------------------------------------------------------------------
// AC10 case 3 — create, delete, rename, each with its post-state
// ---------------------------------------------------------------------------

func TestAC10Case3ARecordAppearingFiresTheDirectoryWatch(t *testing.T) {
	dir := t.TempDir()

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir})

	write(t, filepath.Join(dir, "7.json"), `{"status":"idle"}`)

	c, latency, ok := awaitChange(t, ch, mustArrive)
	if !ok {
		t.Fatal("no Change for a record appearing as a DIRECT entry of an armed directory")
	}
	if c.Kind != observer.TargetRegistryDir {
		t.Errorf("Change.Kind = %v, want registry-dir", c.Kind)
	}
	t.Logf("create latency: %v", latency)
}

func TestAC10Case3ADeletedRecordFiresAndThenDropsOutOfTheArmedSet(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "7.json")
	write(t, rec, `{"status":"idle"}`)

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	targets := []observer.WatchTarget{
		{Kind: observer.TargetRegistryDir, Path: dir},
		{Kind: observer.TargetRegistryFile, Path: rec},
	}
	reconcile(t, w, ctx, targets...)

	if err := os.Remove(rec); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, _, ok := awaitChange(t, ch, mustArrive); !ok {
		t.Fatal("no Change for a deleted armed record")
	}

	// Post-state: the core still wants it (it does not know yet), and the
	// adapter reports the arm failure rather than pretending it holds a watch.
	rep := reconcile(t, w, ctx, targets...)
	if rep.Failed != 1 || len(rep.Failures) != 1 {
		t.Fatalf("ArmReport.Failed = %d, Failures = %+v; want exactly one named failure", rep.Failed, rep.Failures)
	}
	if rep.Failures[0].Path != rec {
		t.Errorf("failure path = %q, want %q", rep.Failures[0].Path, rec)
	}
	if _, held := w.FDs()[rec]; held {
		t.Error("the descriptor for a deleted file is still held: on ENOENT the watch must be dropped")
	}
}

func TestAC10Case3ARenamedRecordIsReArmedUnderItsNewIdentity(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "7.json")
	write(t, rec, `{"status":"idle"}`)

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec},
	)
	genBefore := w.Generations()[rec]

	// Rename a DIFFERENT file over the watched path: the path survives, the
	// inode underneath it does not. This is the same shape as the jobs tree's
	// replace-publish and as a registry file being replaced.
	tmp := filepath.Join(dir, "7.json.tmp")
	write(t, tmp, `{"status":"busy"}`)
	if err := os.Rename(tmp, rec); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, _, ok := awaitChange(t, ch, mustArrive); !ok {
		t.Fatal("no Change when an armed record was replaced by rename")
	}

	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec},
	)
	// Compared by ARMING GENERATION rather than by fd number: closing fd 7 and
	// opening again hands back fd 7, so the fd number alone cannot tell an
	// untouched descriptor from a replaced one.
	if genAfter := w.Generations()[rec]; genAfter == genBefore {
		t.Fatal("the descriptor was NOT re-armed after the inode changed underneath it: " +
			"a descriptor on the unlinked inode is deaf from that moment on")
	}

	// And the re-armed descriptor actually works.
	rewriteInPlace(t, rec, `{"status":"idle"}`)
	if _, _, ok := awaitChange(t, ch, mustArrive); !ok {
		t.Fatal("the re-armed descriptor did not fire on a subsequent write")
	}
}

// ---------------------------------------------------------------------------
// AC10 case 4 — the jobs mirror, the opposite mechanism, and ITS negative
// ---------------------------------------------------------------------------

func TestAC10Case4AJobRecordPublishedByRenameFiresTheJobDirectoryWatch(t *testing.T) {
	jobs := t.TempDir()
	job := filepath.Join(jobs, "abcd1234")
	if err := os.MkdirAll(job, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetJobsRoot, Path: jobs},
		observer.WatchTarget{Kind: observer.TargetJobDir, Path: job},
	)

	tmp := filepath.Join(job, "state.json.tmp")
	write(t, tmp, `{"state":"blocked"}`)
	if err := os.Rename(tmp, filepath.Join(job, "state.json")); err != nil {
		t.Fatalf("rename-publish: %v", err)
	}

	c, latency, ok := awaitChange(t, ch, mustArrive)
	if !ok {
		t.Fatal("no Change for a rename-published job record in an armed job directory")
	}
	if c.Kind != observer.TargetJobDir {
		t.Errorf("Change.Kind = %v, want job-dir", c.Kind)
	}
	t.Logf("rename-publish latency: %v", latency)
}

func TestAC10Case4AWatchOnTheOldRecordInodeIsDeafToTheNewOne(t *testing.T) {
	jobs := t.TempDir()
	job := filepath.Join(jobs, "abcd1234")
	if err := os.MkdirAll(job, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := filepath.Join(job, "state.json")
	write(t, rec, `{"state":"working"}`)

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	// The FILE only — the mirror of case 2. No job-directory watch.
	reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetJobFile, Path: rec})

	tmp := filepath.Join(job, "state.json.tmp")
	write(t, tmp, `{"state":"blocked"}`)
	if err := os.Rename(tmp, rec); err != nil {
		t.Fatalf("rename-publish: %v", err)
	}
	// The unlink of the old inode may itself be reported; that is not the
	// claim. Drain whatever the replacement produced.
	if _, _, ok := awaitChange(t, ch, mustArrive); ok {
		t.Log("the old inode reported its own unlink, as expected; the claim is about what " +
			"happens NEXT")
	}

	// Now write to the NEW record. The armed descriptor points at the unlinked
	// inode, so it cannot see this — which is why the job-DIRECTORY watch is
	// the load-bearing one and case 4's positive half is not redundancy.
	rewriteInPlace(t, rec, `{"state":"failed!"}`)
	if c, _, ok := awaitChange(t, ch, mustNotArrive); ok {
		t.Fatalf("a file-only watch observed a write to the REPLACEMENT record (%+v): "+
			"either the publish was not a replace or the watcher re-armed itself behind "+
			"Reconcile's back", c)
	}
	t.Log("confirmed: a per-file watch goes deaf after a replace-publish, so neither watch " +
		"target alone is sufficient and the redundancy in §6.3 is load-bearing")
}

// ---------------------------------------------------------------------------
// §0.5 E1, E5 and E6 — the three measured properties the protocol rests on
// ---------------------------------------------------------------------------

func TestE1TheKernelQueuesEventsRegisteredBeforeTheFirstKeventCall(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "1.json")
	write(t, rec, `{"status":"idle"}`)

	w, ctx := newWatcher(t)
	// Armed BEFORE anyone calls Watch, i.e. before the first kevent() read.
	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec},
	)
	rewriteInPlace(t, rec, `{"status":"busy"}`)

	// Only now does the loop start reading. E1 says the event survived the gap,
	// and that is the single fact that makes "arm, then go away and scan for
	// 40 ms" safe.
	ch := watching(t, w, ctx)
	if _, latency, ok := awaitChange(t, ch, mustArrive); !ok {
		t.Fatal("an event that occurred after registration but before the first kevent() " +
			"call was LOST: the arm-before-read protocol's second case does not hold on " +
			"this kernel")
	} else {
		t.Logf("a pre-first-kevent event was retained and delivered in %v", latency)
	}
}

func TestE5ReconcileLeavesAnUnchangedTargetUntouchedAndKeepsItsQueuedEvent(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "1.json")
	write(t, rec, `{"status":"idle"}`)

	w, ctx := newWatcher(t)
	targets := []observer.WatchTarget{
		{Kind: observer.TargetRegistryDir, Path: dir},
		{Kind: observer.TargetRegistryFile, Path: rec},
	}
	reconcile(t, w, ctx, targets...)
	fdsBefore := w.FDs()
	gensBefore := w.Generations()

	// A mutation whose event is now queued on the descriptor.
	rewriteInPlace(t, rec, `{"status":"busy"}`)

	// A second, identical Reconcile — exactly what §6.3's arm-only waves do
	// when they re-pass a growing superset.
	rep := reconcile(t, w, ctx, targets...)
	if rep.Armed != 0 || rep.Released != 0 {
		t.Errorf("a re-passed identical set armed %d and released %d; want 0 and 0",
			rep.Armed, rep.Released)
	}
	for path, fd := range fdsBefore {
		if now := w.FDs()[path]; now != fd {
			t.Fatalf("%s was closed and reopened (fd %d -> %d) by a Reconcile that wanted "+
				"exactly what was already armed; §0.5 E5 measured that this DISCARDS every "+
				"queued event", path, fd, now)
		}
	}
	for path, gen := range gensBefore {
		if now := w.Generations()[path]; now != gen {
			t.Fatalf("%s was re-armed (generation %d -> %d) although nothing about it "+
				"changed", path, gen, now)
		}
	}

	ch := watching(t, w, ctx)
	if _, _, ok := awaitChange(t, ch, mustArrive); !ok {
		t.Fatal("the event queued before the redundant Reconcile was lost — which is the " +
			"data-loss bug §6.4's two-half rule exists to prevent")
	}
}

func TestE6ARedundantArmOnALiveDescriptorPreservesWhatIsQueued(t *testing.T) {
	jobs := t.TempDir()
	job := filepath.Join(jobs, "aaaa")
	if err := os.MkdirAll(job, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w, ctx := newWatcher(t)
	reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetJobDir, Path: job})
	write(t, filepath.Join(job, "state.json"), `{"state":"blocked"}`)

	// Growing superset, as wave 1 passes: the job dir is re-passed alongside a
	// newly discovered one.
	other := filepath.Join(jobs, "bbbb")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetJobDir, Path: job},
		observer.WatchTarget{Kind: observer.TargetJobDir, Path: other},
	)

	ch := watching(t, w, ctx)
	if _, _, ok := awaitChange(t, ch, mustArrive); !ok {
		t.Fatal("a growing superset dropped the event queued on an already-armed target")
	}
}

func TestReleasingATargetClosesItsDescriptor(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "1.json")
	b := filepath.Join(dir, "2.json")
	write(t, a, `{}`)
	write(t, b, `{}`)

	w, ctx := newWatcher(t)
	reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: a},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: b},
	)
	if n := len(w.FDs()); n != 2 {
		t.Fatalf("armed %d targets, want 2", n)
	}

	rep := reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: a})
	if rep.Released != 1 {
		t.Errorf("Released = %d, want 1", rep.Released)
	}
	if _, held := w.FDs()[b]; held {
		t.Error("a released target still holds a descriptor")
	}
	if _, held := w.FDs()[a]; !held {
		t.Error("the still-desired target was released too")
	}
}

func TestAnUnarmableTargetIsNamedRatherThanCountedAndTheRestStillArm(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "1.json")
	write(t, good, `{}`)
	missing := filepath.Join(dir, "nope.json")

	w, ctx := newWatcher(t)
	rep := reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: good},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: missing},
	)
	if rep.Armed != 2 {
		t.Errorf("Armed = %d, want 2 (the directory and the readable record)", rep.Armed)
	}
	if rep.Failed != 1 || len(rep.Failures) != 1 || rep.Failures[0].Path != missing {
		t.Fatalf("want exactly one failure naming %q, got Failed=%d %+v", missing, rep.Failed, rep.Failures)
	}
	if rep.Failures[0].Error == "" {
		t.Error("the failure carries no error text, so watch-target-missing would say nothing")
	}
}

func TestArmReportCountsPerKindSoEveryFamilyIsReportable(t *testing.T) {
	root := t.TempDir()
	reg := filepath.Join(root, "sessions")
	jobs := filepath.Join(root, "jobs")
	job := filepath.Join(jobs, "aaaa")
	if err := os.MkdirAll(reg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(job, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(reg, "1.json")
	state := filepath.Join(job, "state.json")
	transcript := filepath.Join(root, "t.jsonl")
	write(t, rec, `{}`)
	write(t, state, `{}`)
	write(t, transcript, "{}\n")

	w, ctx := newWatcher(t)
	rep := reconcile(t, w, ctx,
		observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: reg},
		observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec},
		observer.WatchTarget{Kind: observer.TargetJobsRoot, Path: jobs},
		observer.WatchTarget{Kind: observer.TargetJobDir, Path: job},
		observer.WatchTarget{Kind: observer.TargetJobFile, Path: state},
		observer.WatchTarget{Kind: observer.TargetJobTranscript, Path: transcript},
	)
	for _, kind := range observer.AllTargetKinds {
		if kind == observer.TargetClaudeRoot {
			continue // armed only while jobs/ is absent, and it is not
		}
		if rep.ByKind[kind] != 1 {
			t.Errorf("ByKind[%v] = %d, want 1", kind, rep.ByKind[kind])
		}
	}
	if rep.Armed != 6 {
		t.Errorf("Armed = %d, want 6", rep.Armed)
	}
}

func TestATranscriptAppendFires(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	write(t, transcript, "{\"type\":\"user\"}\n")

	w, ctx := newWatcher(t)
	ch := watching(t, w, ctx)
	reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetJobTranscript, Path: transcript})

	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString("{\"type\":\"assistant\"}\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	c, latency, ok := awaitChange(t, ch, mustArrive)
	if !ok {
		t.Fatal("an append to an armed transcript produced no hint, so lastActivityAt would " +
			"never publish")
	}
	if c.Kind != observer.TargetJobTranscript {
		t.Errorf("Change.Kind = %v, want job-transcript", c.Kind)
	}
	t.Logf("append latency: %v", latency)
}

func TestTheHintChannelClosesOnContextCancellationAndLeaksNoDescriptors(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "1.json")
	write(t, rec, `{}`)

	w, err := watchkq.New(watchkq.Caps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	reconcile(t, w, ctx, observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec})

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// A hint may race the cancellation; drain once and require closure.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("the hint channel kept delivering after cancellation")
				}
			case <-time.After(mustArrive):
				t.Fatal("the hint channel did not close after cancellation")
			}
		}
	case <-time.After(mustArrive):
		t.Fatal("the hint channel did not close after cancellation")
	}
	if n := len(w.FDs()); n != 0 {
		t.Errorf("%d descriptors still held after shutdown", n)
	}
}
