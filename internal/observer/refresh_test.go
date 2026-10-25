package observer_test

// P4's core-side contract: §4.2's triggers, §4.3's bounds, §7.2's single-flight
// and generation, the sources block, and H10's RefreshOutcome.
//
// Everything here runs against fakes and an injected clock. The spawn bound
// itself — the child, the kill, the cross-process lock — belongs to the adapter
// and is tested there; what this file is about is the POLICY: when a listing is
// taken at all, what survives a failure, and what a caller is told.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// tCorr is this file's reference instant. A fixed clock is what makes every age
// assertion below exact rather than approximately right.
var tCorr = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

const (
	bgSession  = "97030e53-528c-4452-bbae-535400039def"
	bgShortID  = "97030e53"
	intSession = "f350c44d-d22a-4c47-b3ef-00021713db7f"
)

type corrHarness struct {
	t     *testing.T
	obs   *observer.Observer
	set   *fakes.Set
	clock *fakes.Clock
	// release closes the current hold, if a test took one.
	release func()
}

type corrOpts struct {
	// noAgents drops the corroboration port, which is the P3 build and is what
	// a surface that forgets to wire it would produce.
	noAgents bool
	// jobRecord adds one blocked background record, updated at `now`, which is
	// the shape §3.5 rule 6 needs to be reachable at all.
	jobRecord bool
	policy    observer.Policy
}

func newCorr(t *testing.T, opts corrOpts) *corrHarness {
	t.Helper()
	deps, set := fakes.Deps(tCorr)
	if opts.noAgents {
		deps.Agents = nil
	}
	if opts.jobRecord {
		updated := tCorr
		short := bgShortID
		set.Jobs.SetRecords(tCorr, observer.JobRecord{
			Dir:         short,
			Path:        "/fixtures/jobs/" + short + "/state.json",
			DaemonShort: short,
			SessionID:   bgSession,
			State:       "blocked",
			Needs:       "confirm the plan",
			CWD:         "/home/user/dotfiles",
			Name:        "Drawing issue",
			CreatedAt:   &updated,
			UpdatedAt:   &updated,
		})
	}
	policy := opts.policy
	if policy == (observer.Policy{}) {
		policy = observer.DefaultPolicy()
	}

	obs, err := observer.New(deps, observer.Config{
		Paths: observer.Paths{
			ClaudeRoot: "/fixtures",
			Registry:   "/fixtures/sessions",
			Jobs:       "/fixtures/jobs",
			Projects:   "/fixtures/projects",
		},
		Policy: policy,
	})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return &corrHarness{t: t, obs: obs, set: set, clock: set.Clock}
}

// listing stamps the fake with a listing naming the given session ids.
func (h *corrHarness) listing(at time.Time, ids ...string) {
	h.t.Helper()
	entries := make([]observer.AgentsEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, observer.AgentsEntry{
			SessionID: id,
			ID:        id[:8],
			Kind:      "background",
			State:     "blocked",
		})
	}
	h.set.Agents.SetEntries(at, entries...)
}

func (h *corrHarness) snap() *observer.Snapshot {
	h.t.Helper()
	s, err := h.obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		h.t.Fatalf("Snapshot: %v", err)
	}
	return s
}

// settle waits for any in-flight piggyback so an assertion is never racing a
// goroutine, and so no test leaks one into the next.
func (h *corrHarness) settle() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.obs.AwaitCorroboration(ctx)
}

// hold makes the NEXT spawn block until the returned function is called. It is
// set before any spawn is in flight and never cleared afterwards: the fake
// reads the field under its own lock, so writing it while a spawn goroutine
// could be reading would be a real data race — closing the channel is the only
// mutation, and a closed channel makes every later List non-blocking.
func (h *corrHarness) hold() func() {
	ch := make(chan struct{})
	h.set.Agents.Block = ch
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	h.release = release
	return release
}

// releaseAndSettle lets any held spawn finish and then waits for it. The order
// is the point: settling before releasing would block for the whole
// AwaitCorroboration bound, which is how a test that "passes" starts taking
// five seconds.
func (h *corrHarness) releaseAndSettle() {
	if h.release != nil {
		h.release()
	}
	h.settle()
}

func agentsSource(t *testing.T, s *observer.Snapshot) observer.SourceReport {
	t.Helper()
	rep, ok := s.Sources[observer.SourceAgentsCLI]
	if !ok {
		t.Fatalf("no %s source block; sources = %v", observer.SourceAgentsCLI, sourceIDs(s))
	}
	return rep
}

// ---------------------------------------------------------------------------
// §4.2 — when a listing is taken at all
// ---------------------------------------------------------------------------

func TestTheFirstRebuildBootstrapsOnceAndAFreshOneSpawnsNothing(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)

	h.snap()
	h.settle()
	if got := h.set.Agents.Spawns(); got != 1 {
		t.Fatalf("spawns after the first rebuild = %d, want 1 (T6)", got)
	}

	// Two more rebuilds with the clock unmoved. The held listing is fresh, so
	// §4.2's invariant forbids a spawn: a Change alone may never cause one.
	h.snap()
	h.snap()
	h.settle()
	if got := h.set.Agents.Spawns(); got != 1 {
		t.Fatalf("spawns after three rebuilds = %d, want 1 — a rebuild with fresh "+
			"corroboration must not spawn", got)
	}
}

func TestAPiggybackFiresOnlyOnceStaleAndDoesNotDelayTheRebuild(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)
	h.snap()
	h.settle()

	// One millisecond inside the window: still fresh, still no spawn.
	h.clock.Advance(time.Duration(observer.DefaultStaleAfterMs) * time.Millisecond)
	h.snap()
	h.settle()
	if got := h.set.Agents.Spawns(); got != 1 {
		t.Fatalf("spawns at exactly staleAfterMs = %d, want 1: the comparison is strict", got)
	}

	// Past it. The piggyback is issued, and the ANSWER is still built from the
	// old listing — the spawn refreshes corroboration for the NEXT rebuild,
	// which is what makes it incapable of delaying an event.
	h.clock.Advance(time.Millisecond)
	stampAt := h.clock.Now()
	h.listing(stampAt, bgSession)
	snap := h.snap()

	rep := agentsSource(t, snap)
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(tCorr) {
		t.Fatalf("the rebuild answered from observedAt %v, want the OLD listing's %s: a "+
			"piggyback must not be waited on", rep.ObservedAt, tCorr)
	}

	h.settle()
	if got := h.set.Agents.Spawns(); got != 2 {
		t.Fatalf("spawns after the staleness crossing = %d, want 2 (T5)", got)
	}
	next := agentsSource(t, h.snap())
	if next.ObservedAt == nil || !next.ObservedAt.Equal(stampAt) {
		t.Fatalf("the next rebuild answered from %v, want the piggyback's %s",
			next.ObservedAt, stampAt)
	}
}

func TestABuildWithNoCorroborationPortSaysSoRatherThanPretending(t *testing.T) {
	h := newCorr(t, corrOpts{noAgents: true, jobRecord: true})

	snap := h.snap()
	if _, ok := snap.Sources[observer.SourceAgentsCLI]; ok {
		t.Fatal("a source block for a port that is not wired: a consumer would " +
			"compare counters that describe nothing")
	}

	outcome := h.obs.RefreshSources(context.Background(), nil)
	if outcome.Rejected[observer.SourceAgentsCLI] != observer.RejectUnsupported {
		t.Fatalf("rejected = %v, want unsupported", outcome.Rejected)
	}
	if len(outcome.Refreshed) != 0 {
		t.Fatalf("refreshed = %v with no port wired", outcome.Refreshed)
	}
	// The background job says why it cannot be vouched for, rather than being
	// reported live from the absence of disproof (§3.5 rule 6b(ii)).
	sess, ok := snap.SessionByID(bgSession)
	if !ok {
		t.Fatal("the background job is missing from the default set")
	}
	if sess.Live != nil {
		t.Fatalf("live = %v, want null with no corroboration available", *sess.Live)
	}
}

func TestFreshCorroborationIsWhatMakesABackgroundJobLive(t *testing.T) {
	// P3's log recorded that rule 6's live: true is UNREACHABLE without this
	// phase. This is the assertion that it is now reachable, and that it rests
	// on a listing rather than on the absence of disproof.
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)

	sess, ok := h.snap().SessionByID(bgSession)
	if !ok {
		t.Fatal("the background job is missing")
	}
	if sess.Live == nil || !*sess.Live {
		t.Fatalf("live = %v, want true from a fresh listing that names it", sess.Live)
	}
	if sess.LiveReason != observer.LiveReasonJobNonTerminalCorroborated {
		t.Fatalf("liveReason = %q, want %q", sess.LiveReason,
			observer.LiveReasonJobNonTerminalCorroborated)
	}
	if !containsID(sess.ListedBy, observer.SourceAgentsCLI) {
		t.Fatalf("listedBy = %v, want it to include %s", sess.ListedBy, observer.SourceAgentsCLI)
	}

	// The same record and the same listing, thirty seconds later: rule 6 is
	// clock-sensitive and nothing wakes up to publish the crossing.
	h.clock.Advance(time.Duration(observer.DefaultStaleAfterMs+1) * time.Millisecond)
	release := h.hold() // hold the piggyback open so the held listing stays old
	after, _ := h.snap().SessionByID(bgSession)
	if after.Live != nil {
		t.Fatalf("live = %v on a stale listing, want null", *after.Live)
	}
	release()
	h.settle()
}

// ---------------------------------------------------------------------------
// §4.3(a) / §7.2 — single-flight, timeout, backoff
// ---------------------------------------------------------------------------

func TestTwoConcurrentRefreshesProduceOneSpawn(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)

	release := h.hold()

	type result struct{ outcome observer.RefreshOutcome }
	done := make(chan result, 2)
	start := func() {
		go func() {
			done <- result{h.obs.RefreshSources(context.Background(), nil)}
		}()
	}

	// The ordering here is the whole test, and the first version of it was
	// wrong: releasing the hold before the second caller had reached the attach
	// point let the first spawn COMPLETE, which cleared the single-flight slot
	// and made the second caller start a legitimate second spawn. The test
	// failed and the code was right.
	//
	// So: start the first caller, wait until it is demonstrably inside the
	// spawn (the counter moved), then start the second. The first is still held,
	// so the slot cannot clear — a second spawn would therefore show up as a
	// second count immediately, and the assertion below is exact rather than
	// timing-dependent.
	start()
	// watchloop_test.go's waitFor: a bounded poll in a TEST, where the timer
	// rule does not apply — the production path has no polling of any kind.
	waitFor(t, 2*time.Second, func() bool { return h.set.Agents.Spawns() == 1 })
	start()
	time.Sleep(20 * time.Millisecond)
	if got := h.set.Agents.Spawns(); got != 1 {
		t.Fatalf("spawns = %d while the first spawn is still held: the second caller "+
			"started its own instead of attaching", got)
	}

	release()
	first, second := <-done, <-done

	if got := h.set.Agents.Spawns(); got != 1 {
		t.Fatalf("spawns = %d for two concurrent refreshes, want 1", got)
	}
	for _, r := range []result{first, second} {
		if len(r.outcome.Refreshed) != 1 || r.outcome.Refreshed[0] != observer.SourceAgentsCLI {
			t.Fatalf("refreshed = %v, want both callers to share the one result", r.outcome.Refreshed)
		}
		if len(r.outcome.Rejected) != 0 {
			t.Fatalf("rejected = %v, want none", r.outcome.Rejected)
		}
	}
}

func TestATimedOutRefreshKeepsThePreviousListingAndAnswersDegraded(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)
	h.snap() // bootstrap, held listing at tCorr
	h.settle()

	// The next spawn never answers. The caller's own deadline expires, which is
	// the shape a 5 s spawnTimeout has from the core's side.
	release := h.hold()
	h.clock.Advance(time.Duration(observer.DefaultStaleAfterMs+1) * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	outcome := h.obs.RefreshSources(ctx, nil)

	if outcome.Rejected[observer.SourceAgentsCLI] != observer.RejectTimeout {
		t.Fatalf("rejected = %v, want timeout", outcome.Rejected)
	}
	if len(outcome.Refreshed) != 0 {
		t.Fatalf("refreshed = %v after a timeout", outcome.Refreshed)
	}
	if !hasCode(outcome.Advisories, observer.AdvRefreshRejected) {
		t.Fatalf("advisories = %v, want a request-scoped %s", outcome.Advisories, observer.AdvRefreshRejected)
	}

	// THE PREVIOUS SNAPSHOT SURVIVES. The listing the liveness policy reads is
	// still the one taken at tCorr, and the failure is a value in the report.
	snap := h.snap()
	rep := agentsSource(t, snap)
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(tCorr) {
		t.Fatalf("observedAt = %v, want the surviving %s", rep.ObservedAt, tCorr)
	}
	if rep.Counters["entries"] != 1 {
		t.Fatalf("entries = %d, want the previous listing's 1", rep.Counters["entries"])
	}
	if _, ok := snap.SessionByID(bgSession); !ok {
		t.Fatal("the background job vanished because a corroboration spawn timed out")
	}

	release()
	h.settle()

	// And the failure is recorded: lastError, plus a backoff that is a
	// TIMESTAMP rather than an armed timer.
	after := agentsSource(t, h.snap())
	if after.LastError == nil {
		t.Fatal("no lastError after a failed spawn")
	}
	if after.Labels["backoffUntil"] == "" {
		t.Fatalf("labels = %v, want a backoffUntil", after.Labels)
	}
}

func TestBackoffRefusesAndThenExpiresWithNothingArmed(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.set.Agents.Err = errors.New("claude: command not found")

	// A failed explicit refresh.
	outcome := h.obs.RefreshSources(context.Background(), nil)
	if outcome.Rejected[observer.SourceAgentsCLI] != observer.RejectError {
		t.Fatalf("rejected = %v, want %q for a spawn that ran and failed",
			outcome.Rejected, observer.RejectError)
	}
	spawnsAfterFirst := h.set.Agents.Spawns()

	// Inside the backoff window: refused without spawning.
	again := h.obs.RefreshSources(context.Background(), nil)
	if again.Rejected[observer.SourceAgentsCLI] != observer.RejectBackoff {
		t.Fatalf("rejected = %v, want backoff", again.Rejected)
	}
	if got := h.set.Agents.Spawns(); got != spawnsAfterFirst {
		t.Fatalf("spawns = %d during backoff, want %d", got, spawnsAfterFirst)
	}

	// Past it. Nothing was armed and nothing fired; the next request simply
	// succeeds, which is the whole point of comparing a timestamp.
	h.set.Agents.Err = nil
	h.listing(h.clock.Now(), bgSession)
	h.clock.Advance(3 * time.Second)
	third := h.obs.RefreshSources(context.Background(), nil)
	if len(third.Refreshed) != 1 {
		t.Fatalf("refreshed = %v after the backoff expired, rejected = %v",
			third.Refreshed, third.Rejected)
	}
}

func TestAContendedListingIsRateLimitedAndCarriesItsAdvisory(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	// What the adapter returns when another process holds the cross-process
	// lock: no listing, no spawn, and the wait it spent.
	h.set.Agents.Result = observer.AgentsResult{
		ObservedAt: tCorr,
		Command:    "claude agents --json",
		Cache:      observer.AgentsCacheContended,
		WaitedMs:   250,
		OK:         false,
	}

	outcome := h.obs.RefreshSources(context.Background(), nil)
	if outcome.Rejected[observer.SourceAgentsCLI] != observer.RejectRateLimited {
		t.Fatalf("rejected = %v, want rate-limited", outcome.Rejected)
	}
	if !hasCode(outcome.Advisories, observer.AdvAgentsCacheContended) {
		t.Fatalf("advisories = %v, want %s", outcome.Advisories, observer.AdvAgentsCacheContended)
	}

	snap := h.snap()
	rep := agentsSource(t, snap)
	if rep.Labels["agentsCache"] != observer.AgentsCacheContended {
		t.Fatalf("agentsCache = %q, want contended", rep.Labels["agentsCache"])
	}
	if !hasCode(rep.Advisories, observer.AdvAgentsCacheContended) {
		t.Fatalf("source advisories = %v, want %s", rep.Advisories, observer.AdvAgentsCacheContended)
	}
	// Source-scoped advisories are promoted into warnings exactly once.
	if !hasCode(snap.Warnings, observer.AdvAgentsCacheContended) {
		t.Fatalf("warnings = %v, want the promoted advisory", snap.Warnings)
	}
}

func TestTheCircuitBreakerDropsToOnDemandAfterTenUnchangedSpawns(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)

	// Ten spawns whose listing never changes, inside the 60 s window. That is
	// §4.3(c)'s signature of a loop: real activity brings a different listing.
	for i := 0; i < observer.SpawnLoopGuardSpawns+2; i++ {
		h.clock.Advance(time.Second)
		h.set.Agents.SetEntries(h.clock.Now(), observer.AgentsEntry{
			SessionID: bgSession, ID: bgShortID, Kind: "background", State: "blocked",
		})
		h.obs.RefreshSources(context.Background(), nil)
	}
	h.settle()

	rep := agentsSource(t, h.snap())
	if rep.Labels["degraded"] != observer.DegradedSpawnLoopGuard {
		t.Fatalf("degraded = %q, want %q after %d unchanged spawns",
			rep.Labels["degraded"], observer.DegradedSpawnLoopGuard, observer.SpawnLoopGuardSpawns)
	}
	if !hasCode(rep.Advisories, observer.AdvSpawnLoopGuard) {
		t.Fatalf("advisories = %v, want %s", rep.Advisories, observer.AdvSpawnLoopGuard)
	}

	// And it stops spawning: an explicit refresh is refused rather than served
	// from a mechanism we have just decided is misbehaving.
	before := h.set.Agents.Spawns()
	outcome := h.obs.RefreshSources(context.Background(), nil)
	if outcome.Rejected[observer.SourceAgentsCLI] != observer.RejectRateLimited {
		t.Fatalf("rejected = %v, want rate-limited once degraded", outcome.Rejected)
	}
	if got := h.set.Agents.Spawns(); got != before {
		t.Fatalf("spawns = %d after degrading, want %d", got, before)
	}
}

func TestAChangingListingDoesNotTripTheBreaker(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	for i := 0; i < observer.SpawnLoopGuardSpawns+4; i++ {
		h.clock.Advance(time.Second)
		state := "blocked"
		if i%2 == 0 {
			state = "working"
		}
		h.set.Agents.SetEntries(h.clock.Now(), observer.AgentsEntry{
			SessionID: bgSession, ID: bgShortID, Kind: "background", State: state,
		})
		h.obs.RefreshSources(context.Background(), nil)
	}
	h.settle()

	if got := agentsSource(t, h.snap()).Labels["degraded"]; got != "" {
		t.Fatalf("degraded = %q on a listing that kept changing", got)
	}
}

// ---------------------------------------------------------------------------
// §4.5(a) — the sources block, and the instant it is computed against
// ---------------------------------------------------------------------------

func TestTheSourcesBlockUsesTheRebuildsInstantAndNeverAFreshClockRead(t *testing.T) {
	// This is the assertion two live defects in this feature would have been
	// caught by: a negative ageMs from an adapter stamping its own clock read,
	// and an envelope whose observedAt disagreed with its payload. The identity
	// below is exact arithmetic, so a second clock read anywhere on the path
	// breaks it by however long the read took.
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)
	h.snap()
	h.settle()

	h.clock.Advance(7 * time.Second)
	snap := h.snap()
	rep := agentsSource(t, snap)

	if rep.AgeMs == nil || rep.ObservedAt == nil {
		t.Fatalf("ageMs = %v observedAt = %v, want both", rep.AgeMs, rep.ObservedAt)
	}
	want := snap.ObservedAt.Sub(*rep.ObservedAt).Milliseconds()
	if *rep.AgeMs != want {
		t.Fatalf("ageMs = %d, want %d = snapshot.observedAt - source.observedAt",
			*rep.AgeMs, want)
	}
	if *rep.AgeMs != 7_000 {
		t.Fatalf("ageMs = %d, want 7000 against the advanced clock", *rep.AgeMs)
	}
	if rep.StaleAfterMs == nil || *rep.StaleAfterMs != observer.DefaultStaleAfterMs {
		t.Fatalf("staleAfterMs = %v, want the policy's %d", rep.StaleAfterMs, observer.DefaultStaleAfterMs)
	}
	if rep.Stale {
		t.Fatal("stale at 7s against a 30s threshold")
	}
	if rep.Labels["crossChecksSuppressed"] != "false" {
		t.Fatalf("crossChecksSuppressed = %q, want \"false\"", rep.Labels["crossChecksSuppressed"])
	}

	// And it survives the encoder with the same identity, which is what AC11
	// rests on.
	out := wire.NewSnapshot(snap, snap.ObservedAt, wire.Options{TT: "v-test"})
	encoded := out.Sources[observer.SourceAgentsCLI]
	if encoded.AgeMs == nil || *encoded.AgeMs != want {
		t.Fatalf("encoded ageMs = %v, want %d", encoded.AgeMs, want)
	}
	if encoded.ObservedAt == nil || *encoded.ObservedAt != observer.FormatTime(*rep.ObservedAt) {
		t.Fatalf("encoded observedAt = %v", encoded.ObservedAt)
	}
}

func TestAStaleSourceSaysSoAndSuppressesTheCrossChecks(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)
	h.snap()
	h.settle()

	h.clock.Advance(time.Duration(observer.DefaultStaleAfterMs+5_000) * time.Millisecond)
	release := h.hold() // hold the piggyback so the held listing stays old
	snap := h.snap()
	rep := agentsSource(t, snap)

	if !rep.Stale {
		t.Fatalf("stale false at ageMs %v against %d", rep.AgeMs, observer.DefaultStaleAfterMs)
	}
	if rep.Labels["crossChecksSuppressed"] != "true" {
		t.Fatalf("crossChecksSuppressed = %q, want \"true\"", rep.Labels["crossChecksSuppressed"])
	}
	if !hasCode(rep.Advisories, observer.AdvSourceStale) {
		t.Fatalf("advisories = %v, want %s", rep.Advisories, observer.AdvSourceStale)
	}
	if !hasCode(snap.Warnings, observer.AdvSourceStale) {
		t.Fatalf("warnings = %v, want the promoted %s", snap.Warnings, observer.AdvSourceStale)
	}

	release()
	h.settle()
}

func TestTheSourcesBlockCountsBothPopulationsAndNamesTheCommand(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	pid := 78761
	h.set.Agents.SetEntries(tCorr,
		observer.AgentsEntry{SessionID: bgSession, ID: bgShortID, Kind: "background", State: "blocked"},
		observer.AgentsEntry{SessionID: intSession, PID: &pid, Kind: "interactive", Status: "idle"},
	)

	rep := agentsSource(t, h.snap())
	if rep.Role != "corroboration" {
		t.Fatalf("role = %q, want corroboration", rep.Role)
	}
	if rep.Counters["entries"] != 2 || rep.Counters["interactive"] != 1 || rep.Counters["background"] != 1 {
		t.Fatalf("counters = %v, want entries 2 / interactive 1 / background 1", rep.Counters)
	}
	if rep.Counters["spawns"] != 1 {
		t.Fatalf("spawns = %d, want 1", rep.Counters["spawns"])
	}
	if rep.Labels["command"] != "claude agents --json" {
		t.Fatalf("command = %q", rep.Labels["command"])
	}
	if rep.Labels["trigger"] != observer.TriggerStartup {
		t.Fatalf("trigger = %q, want %q on the first rebuild", rep.Labels["trigger"], observer.TriggerStartup)
	}
	if !rep.OK {
		t.Fatal("ok false on a successful listing")
	}
}

func TestAnExplicitRefreshIsLabelledExplicit(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)
	h.snap()
	h.settle()

	h.clock.Advance(time.Minute)
	h.listing(h.clock.Now(), bgSession)
	if out := h.obs.RefreshSources(context.Background(), nil); len(out.Refreshed) != 1 {
		t.Fatalf("refreshed = %v rejected = %v", out.Refreshed, out.Rejected)
	}
	if got := agentsSource(t, h.snap()).Labels["trigger"]; got != observer.TriggerExplicit {
		t.Fatalf("trigger = %q, want %q", got, observer.TriggerExplicit)
	}
}

func TestAnUnknownSourceIdIsRejectedAsUnsupported(t *testing.T) {
	h := newCorr(t, corrOpts{jobRecord: true})
	h.listing(tCorr, bgSession)

	out := h.obs.RefreshSources(context.Background(), []string{"registry", "nope"})
	if out.Rejected["registry"] != observer.RejectUnsupported {
		t.Fatalf("rejected = %v, want registry unsupported: it is re-read on every "+
			"rebuild, so refreshing it is a no-op with a misleading name", out.Rejected)
	}
	if out.Rejected["nope"] != observer.RejectUnsupported {
		t.Fatalf("rejected = %v, want the unknown id unsupported", out.Rejected)
	}
	if len(out.Refreshed) != 0 {
		t.Fatalf("refreshed = %v", out.Refreshed)
	}
	if got := h.set.Agents.Spawns(); got != 0 {
		t.Fatalf("spawns = %d for a request that never named agents-cli", got)
	}
}

// ---------------------------------------------------------------------------
// H10 — RefreshOutcome's four cases, against two implementations
// ---------------------------------------------------------------------------

// refresher is the incoming port's refresh half. The four cases are asserted
// against every implementation of it, which in P4 is the local *Observer plus
// the via-wire proxy below.
type refresher interface {
	Refresh(ctx context.Context, sources []string) (*observer.Snapshot, observer.RefreshOutcome, error)
}

// wireProxy is P4's stand-in for P8's sockclient: it puts the outcome through
// the §7.5(d) round trip — a `sources` frame for a refresh that happened, an
// `error` frame with a code for one that was refused — and rebuilds the
// RefreshOutcome from the bytes.
//
// The reason to have it now rather than to wait for P8: the requirement is that
// the local and via-socket paths cannot DISAGREE, and the disagreement would be
// introduced by the serialisation, not by either implementation's logic. So the
// serialisation is what the second implementation has to exercise. P8 replaces
// the type name in oneRefresher below with the real client and the four cases
// carry over unchanged.
type wireProxy struct{ inner refresher }

func (p wireProxy) Refresh(ctx context.Context, sources []string) (*observer.Snapshot, observer.RefreshOutcome, error) {
	snap, outcome, err := p.inner.Refresh(ctx, sources)
	if err != nil {
		return nil, outcome, err
	}

	// server -> bytes
	frame := map[string]any{"type": wire.FrameSources, "reqId": "r-1"}
	if len(outcome.Refreshed) > 0 {
		frame["refreshed"] = outcome.Refreshed
	}
	if len(outcome.Rejected) > 0 {
		frame["type"] = wire.FrameError
		frame["rejected"] = outcome.Rejected
	}
	adv := make([]wire.Advisory, 0, len(outcome.Advisories))
	for _, a := range outcome.Advisories {
		adv = append(adv, wire.Advisory{Code: a.Code, Fields: a.Fields})
	}
	frame["advisories"] = adv

	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, observer.RefreshOutcome{}, err
	}

	// bytes -> client. Decoded by KEY NAME rather than into a tagged struct:
	// §5.1's ban on json tags outside `wire` covers test files too, and the
	// audit walks them — which is how this very proxy first failed the gate.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, observer.RefreshOutcome{}, err
	}
	out := observer.RefreshOutcome{
		Refreshed:  []string{},
		Rejected:   map[string]string{},
		Advisories: []observer.Advisory{},
	}
	if v, ok := decoded["refreshed"]; ok {
		if err := json.Unmarshal(v, &out.Refreshed); err != nil {
			return nil, observer.RefreshOutcome{}, err
		}
	}
	if v, ok := decoded["rejected"]; ok {
		if err := json.Unmarshal(v, &out.Rejected); err != nil {
			return nil, observer.RefreshOutcome{}, err
		}
	}
	if v, ok := decoded["advisories"]; ok {
		var list []wire.Advisory
		if err := json.Unmarshal(v, &list); err != nil {
			return nil, observer.RefreshOutcome{}, err
		}
		for _, a := range list {
			out.Advisories = append(out.Advisories, observer.Advisory{Code: a.Code, Fields: a.Fields})
		}
	}
	return snap, out, nil
}

func TestRefreshOutcomesFourCasesAgainstBothImplementations(t *testing.T) {
	cases := []struct {
		name string
		// arrange puts the harness into the state the case is about.
		arrange func(*corrHarness) context.Context
		want    observer.RefreshOutcome
		// wantSnapshot asserts the answer is still an answer.
		wantSnapshot bool
	}{
		{
			name: "success — a spawn happened",
			arrange: func(h *corrHarness) context.Context {
				h.listing(tCorr, bgSession)
				return context.Background()
			},
			want: observer.RefreshOutcome{
				Refreshed: []string{observer.SourceAgentsCLI},
				Rejected:  map[string]string{},
			},
			wantSnapshot: true,
		},
		{
			name: "cache-hit — the shared cache answered, no child was started",
			arrange: func(h *corrHarness) context.Context {
				h.set.Agents.SetEntries(tCorr, observer.AgentsEntry{
					SessionID: bgSession, ID: bgShortID, Kind: "background", State: "blocked",
				})
				h.set.Agents.Result.Cached = true
				h.set.Agents.Result.Cache = observer.AgentsCacheHit
				return context.Background()
			},
			want: observer.RefreshOutcome{
				Refreshed: []string{observer.SourceAgentsCLI},
				Rejected:  map[string]string{},
			},
			wantSnapshot: true,
		},
		{
			name: "rate-limited — another process holds the cross-process lock",
			arrange: func(h *corrHarness) context.Context {
				h.set.Agents.Result = observer.AgentsResult{
					ObservedAt: tCorr,
					Cache:      observer.AgentsCacheContended,
					WaitedMs:   250,
				}
				return context.Background()
			},
			want: observer.RefreshOutcome{
				Refreshed: []string{},
				Rejected:  map[string]string{observer.SourceAgentsCLI: observer.RejectRateLimited},
			},
			wantSnapshot: true,
		},
		{
			name: "timeout — the spawn did not answer in time",
			arrange: func(h *corrHarness) context.Context {
				// The ADAPTER's spawnTimeout expiring, which is what §4.3(a)
				// specifies and what the hangs fixture proves for real. A
				// caller-side deadline is a different case — it also expires the
				// rebuild that has to answer — and has its own test.
				h.set.Agents.Err = fmt.Errorf(
					"claude agents --json: timed out after 5s: %w", context.DeadlineExceeded)
				return context.Background()
			},
			want: observer.RefreshOutcome{
				Refreshed: []string{},
				Rejected:  map[string]string{observer.SourceAgentsCLI: observer.RejectTimeout},
			},
			wantSnapshot: true,
		},
	}

	for _, tc := range cases {
		for _, impl := range []string{"local", "via-wire"} {
			t.Run(tc.name+"/"+impl, func(t *testing.T) {
				h := newCorr(t, corrOpts{jobRecord: true})
				ctx := tc.arrange(h)

				var src refresher = h.obs
				if impl == "via-wire" {
					src = wireProxy{inner: h.obs}
				}

				snap, outcome, err := src.Refresh(ctx, []string{observer.SourceAgentsCLI})
				if err != nil {
					t.Fatalf("Refresh returned an error; every one of these is a "+
						"successful degraded answer: %v", err)
				}
				if tc.wantSnapshot && snap == nil {
					t.Fatal("no snapshot: a refused refresh must still answer")
				}
				if !equalStrings(outcome.Refreshed, tc.want.Refreshed) {
					t.Fatalf("refreshed = %v, want %v", outcome.Refreshed, tc.want.Refreshed)
				}
				if len(outcome.Rejected) != len(tc.want.Rejected) {
					t.Fatalf("rejected = %v, want %v", outcome.Rejected, tc.want.Rejected)
				}
				for id, reason := range tc.want.Rejected {
					if outcome.Rejected[id] != reason {
						t.Fatalf("rejected[%s] = %q, want %q", id, outcome.Rejected[id], reason)
					}
				}
				if len(tc.want.Rejected) > 0 && !hasCode(outcome.Advisories, observer.AdvRefreshRejected) {
					t.Fatalf("advisories = %v, want a request-scoped %s",
						outcome.Advisories, observer.AdvRefreshRejected)
				}
				// Request-scoped means request-scoped: the shared snapshot must
				// not carry the caller's rejection.
				if snap != nil && hasCode(snap.Warnings, observer.AdvRefreshRejected) {
					t.Fatal("refresh-rejected leaked into the shared snapshot's warnings")
				}

				// No goroutine outlives the subtest: release the hold this case
				// may have taken, then wait for the spawn it was holding.
				h.releaseAndSettle()
			})
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// hasCode asks whether a LIST of advisories carries a code. ac_test.go's
// hasAdvisory asks the same question of a session; a refresh outcome, a source
// report and the warnings array are all bare lists, so this is the shape P4
// needs and the two are deliberately not merged — a helper taking `any` would
// turn a compile error into a runtime one.
func hasCode(list []observer.Advisory, code string) bool {
	for _, a := range list {
		if a.Code == code {
			return true
		}
	}
	return false
}

func containsID(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
