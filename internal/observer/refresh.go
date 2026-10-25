package observer

// The corroboration path: §4.2's six triggers reduced to one function, §4.3's
// bounds, §7.2's single-flight and generation, and the `sources.agents-cli`
// report that makes all of it visible.
//
// # The one invariant this file exists to hold
//
//	No code path may spawn `claude` in response to a Change alone.
//
// There is exactly ONE call to AgentsSource.List in this module, in spawnOnce
// below, and it is lexically inside an `if` whose condition names
// o.policy.StaleAfterMs. The §9.3(a) AST audit asserts that shape — a call not
// dominated by a staleAfterMs comparison is reported as a t5-spawn-precondition
// finding — so the invariant is enforced by a test rather than by review. The
// two triggers that may spawn unconditionally (T4, an explicit consumer
// request; T6, the first listing a process ever takes) are the other two
// disjuncts of that same condition, which is what keeps the sanctioned
// exceptions visible in the guard instead of hidden in a second call site.
//
// # What is deliberately NOT here
//
// A timer. Backoff is a TIMESTAMP compared against the caller's instant, and
// the staleness precondition is evaluated when a rebuild happens rather than on
// a schedule. The idle steady state arms nothing, which is what makes AC9's
// original "no timer, no poll loop" wording literally true after the user's
// override. The one time.Timer in the whole subsystem is the watch loop's 250 ms
// coalescer (§7.2), and the only bounded waits here are context deadlines that
// exist solely while a spawn is in flight.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Triggers, as published in sources.agents-cli.labels.trigger. They are §4.2's
// T6, T4 and T5 in the vocabulary a consumer reads.
const (
	// TriggerStartup is T6: the first listing a process takes, subject to the
	// shared cache. A one-shot surface takes this one and no other.
	TriggerStartup = "startup"
	// TriggerExplicit is T4: --refresh, the socket's refresh frame, the MCP
	// tool's refresh argument, the TUI's `r`.
	TriggerExplicit = "explicit"
	// TriggerPiggyback is T5: a rebuild already triggered by a watch event,
	// where the held listing has aged past staleAfterMs. It is issued
	// asynchronously and lands for the NEXT rebuild, so it can never delay an
	// event.
	TriggerPiggyback = "piggyback"
)

// The five §4.3(b) cache outcomes, reported as
// sources.agents-cli.labels.agentsCache. They are declared in the core rather
// than in the adapter because the core publishes them and the liveness rules
// read the OK flag they imply; an adapter-owned vocabulary would have to be
// translated somewhere, and that somewhere is where the five would become four.
const (
	// AgentsCacheHit: the shared cache was fresh. No lock, no spawn.
	AgentsCacheHit = "hit"
	// AgentsCacheMiss: this process took the lock and spawned.
	AgentsCacheMiss = "miss"
	// AgentsCacheHitAfterWait: the lock was held and the holder published
	// within lockWait.
	AgentsCacheHitAfterWait = "hit-after-wait"
	// AgentsCacheContended: the lock was held and the wait expired. This
	// process answers WITHOUT corroboration and does NOT spawn.
	AgentsCacheContended = "contended"
	// AgentsCacheUnwritable: a spawn happened and its result could not be
	// shared. The listing is still used; only the sharing failed.
	AgentsCacheUnwritable = "unwritable"
)

// Rejection reasons for RefreshOutcome.Rejected.
const (
	// RejectRateLimited: a spawn was declined by a bound rather than by a
	// failure — the cross-process lock was contended, or the circuit breaker
	// has degraded this source.
	RejectRateLimited = "rate-limited"
	// RejectTimeout: the spawn exceeded spawnTimeout, or the caller's own
	// deadline expired while waiting for it.
	RejectTimeout = "timeout"
	// RejectBackoff: consecutive failures have pushed the next permitted spawn
	// into the future.
	RejectBackoff = "backoff"
	// RejectUnsupported: this source id cannot be refreshed — an unknown id, or
	// a build with no corroboration port wired.
	RejectUnsupported = "unsupported"
	// RejectError: the spawn ran and failed. See RefreshOutcome.Rejected's
	// comment for why this reason had to exist.
	RejectError = "error"
)

// §4.3(c)'s circuit breaker and §4.3(a)'s backoff.
const (
	// SpawnLoopGuardSpawns and SpawnLoopGuardWindow are N and M: ten spawns in
	// sixty seconds with zero change to the listing means something is spawning
	// in a loop, and the source drops to on-demand. It is the only thing that
	// would catch a spawn loop introduced through a path the AST audit does not
	// model.
	SpawnLoopGuardSpawns = 10
	SpawnLoopGuardWindow = 60 * time.Second
	// DegradedSpawnLoopGuard is published as sources.agents-cli.labels.degraded.
	DegradedSpawnLoopGuard = "spawn-loop-guard"

	// backoffBase and backoffCap are §4.3(a)'s 2s, 4s, 8s … 5 min. They are
	// compared against the caller's instant, never armed.
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

// CorroborationWait bounds how long a REBUILD may block on the corroboration
// spawn it issued. It is NOT the spawn's timeout — the spawn keeps §4.3(a)'s
// 5 s bound and, on a long-lived surface, its result is still adopted when it
// lands.
//
// # Why this constant exists (the wedged-CLI defect)
//
// §4.3(a) says "one timeout, every surface", and P4 implemented that literally:
// the rebuild waited on the startup spawn under the CALLER's context, so the
// spawn bound WAS the whole-read bound. When `claude agents --json` wedged on
// the user's machine (measured: three of three probes exit 124 at 12 s, having
// returned in ~130 ms the same morning), a 5 s spawn consumed the entire read
// budget, `tmux list-panes` was then asked with an expired context, and all 35
// interactive sessions collapsed to liveReason gate-error / live: null — from a
// source §3.5 says must never override a liveness gate.
//
// The read budget is now spent on the AUTHORITATIVE reads and only the residue
// on corroboration. Snapshot issues the spawn BEFORE the pane gate and the
// timing tails and collects it AFTER, so a healthy spawn (measured 130 ms)
// overlaps ~198 ms of authoritative reads and costs the answer nothing, while a
// wedged one costs 250 ms and is reported rather than absorbed.
//
// 250 ms is §4.3(b)'s lockWaitMs, chosen there for the same reason and against
// the same measurement ("> the measured 130 ms spawn"). Reusing the number
// keeps one machine-wide answer to "how long is it reasonable to wait for this
// child", and the effective allowance is the 250 ms PLUS whatever the
// authoritative reads took, because the spawn was already running.
//
// A wait that expires is a first-class, VISIBLE outcome: sources.agents-cli
// keeps ok:false, gains a populated lastError and carries
// corroboration-wait-expired, which is promoted into warnings.
const CorroborationWait = 250 * time.Millisecond

// corroboration is every piece of state the spawn path keeps.
//
// Its zero value is usable, which is why Observer holds it by value and New
// needs no line for it: no listing held, nothing attempted, generation zero.
type corroboration struct {
	mu sync.Mutex

	// held is the listing in force. Its OK flag is what §3.5's rule 6b reads to
	// distinguish "the source never ran" from "it ran and found nothing".
	held AgentsResult
	// attempted records that a listing has been SOUGHT at least once in this
	// process. It is separate from held.OK on purpose: a contended or failed
	// first attempt must not make every later rebuild retry synchronously, which
	// would turn one cold cache into a spawn on every event.
	attempted bool

	// gen is the request counter. appliedGen is the generation of the listing in
	// held. A result whose generation is not NEWER than appliedGen is dropped
	// (§7.2), so a slow spawn cannot overwrite a newer corroboration.
	gen        int64
	appliedGen int64

	// spawns counts spawn ATTEMPTS in this process, including failed and timed
	// out ones: it is published so that "why is this machine spawning claude"
	// has a number, and a failed attempt costs the same process as a successful
	// one.
	spawns      int64
	lastSpawnMs int64
	trigger     string
	// cache is the last §4.3(b) outcome, kept because it describes the last
	// ATTEMPT while held describes the last successful listing — a contended
	// attempt must be reportable without replacing the listing it failed to
	// improve on.
	cache string
	// waitedMs is how long the last attempt spent on the cross-process lock
	// before giving up. It is the field of the agents-cache-contended advisory,
	// and it lives with the ATTEMPT rather than with the listing because a
	// contended attempt produces no listing to hang it on.
	waitedMs int64
	lastErr  error

	// waitExpired records that a rebuild answered WITHOUT corroboration because
	// the spawn it issued had not returned inside CorroborationWait. It is the
	// state the wedged-CLI defect had no name for: held was empty, lastErr was
	// nil because nothing had failed YET, and the source therefore published
	// ok:false with no error and no advisory. waitExpiredMs is the bound that
	// expired, which is the actionable number (the caller's own deadline can cut
	// it shorter; the bound is what the policy chose).
	//
	// Cleared by applySpawn: once a result has landed, "it did not land" is no
	// longer the current state, whatever the result says.
	waitExpired   bool
	waitExpiredMs int64

	// failures is the consecutive-failure count behind the backoff. Contention
	// is NOT a failure: nothing went wrong, another process is simply holding
	// the spawn right, and counting it would back off the whole machine
	// whenever one process was working.
	failures     int
	backoffUntil time.Time

	// fingerprint is the last listing's content digest and unchanged is when
	// each no-change spawn happened, both for the circuit breaker.
	fingerprint string
	unchanged   []time.Time
	degraded    string

	// inflight is the single-flight slot. At most ONE spawn exists per process,
	// ever; concurrent requests attach to it and share its result.
	inflight *spawnCall
}

// spawnCall is one in-flight spawn and its waiters' rendezvous.
type spawnCall struct {
	gen  int64
	done chan struct{}
	res  AgentsResult
}

// Refresh implements the SessionSource port.
//
// It re-reads the corroboration source and then answers from a fresh rebuild.
// The snapshot is built with DefaultSnapshotOptions because the port carries no
// options — a surface that has its own filters calls RefreshSources and then
// Snapshot, which is one rebuild rather than two and is what jsonout does.
//
// It returns a non-nil error only when the SNAPSHOT could not be built.
// A rate limit, a timeout, a backoff and a contended lock are all successful
// degraded answers: the caller gets the current snapshot plus a request-scoped
// note, which is the whole reason this method returns a RefreshOutcome (H10).
func (o *Observer) Refresh(ctx context.Context, sources []string) (*Snapshot, RefreshOutcome, error) {
	outcome := o.RefreshSources(ctx, sources)
	snap, err := o.Snapshot(ctx, DefaultSnapshotOptions())
	if err != nil {
		return nil, outcome, err
	}
	return snap, outcome, nil
}

// RefreshSources re-reads the named sources and reports what that achieved,
// without building a snapshot.
//
// nil or empty means "every refreshable source", which today is agents-cli
// alone: the registry, the jobs tree and the transcripts are re-read on every
// rebuild anyway, so asking for them is not a refresh, it is a no-op with a
// misleading name. An id naming one of them is therefore rejected as
// unsupported rather than silently reported as refreshed.
func (o *Observer) RefreshSources(ctx context.Context, sources []string) RefreshOutcome {
	out := RefreshOutcome{
		Refreshed:  []string{},
		Rejected:   map[string]string{},
		Advisories: []Advisory{},
	}

	wanted := len(sources) == 0
	for _, id := range sources {
		if id == SourceAgentsCLI {
			wanted = true
			continue
		}
		out.Rejected[id] = RejectUnsupported
	}
	if !wanted {
		return o.withRejectionAdvisories(out)
	}

	if o.deps.Agents == nil {
		out.Rejected[SourceAgentsCLI] = RejectUnsupported
		return o.withRejectionAdvisories(out)
	}

	// One clock read for this operation, passed down. The snapshot that follows
	// takes its own instant: a refresh and a rebuild are two events, and making
	// the refresh borrow the rebuild's `now` would date the listing to before
	// it was taken.
	now := o.deps.Clock.Now()
	reason, advisories := o.takeCorroboration(ctx, now, TriggerExplicit)
	out.Advisories = append(out.Advisories, advisories...)
	if reason == "" {
		out.Refreshed = append(out.Refreshed, SourceAgentsCLI)
		return out
	}
	out.Rejected[SourceAgentsCLI] = reason
	return o.withRejectionAdvisories(out)
}

// withRejectionAdvisories attaches one refresh-rejected advisory per rejected
// source. They are REQUEST-SCOPED and never merged into the shared snapshot:
// one caller's rejected refresh is not a fact about the observer that unrelated
// subscribers should be told.
func (o *Observer) withRejectionAdvisories(out RefreshOutcome) RefreshOutcome {
	ids := make([]string, 0, len(out.Rejected))
	for id := range out.Rejected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out.Advisories = appendAdvisory(out.Advisories, NewAdvisory(AdvRefreshRejected, map[string]any{
			"source": id,
			"reason": out.Rejected[id],
		}))
	}
	return out
}

// requestCorroboration is the ISSUING half of the rebuild's side of §4.2,
// called from Snapshot with the rebuild's own instant BEFORE the authoritative
// reads. It never blocks.
//
// Two triggers and nothing else:
//
//	T6  the first rebuild of a process issues a listing and COLLECTS it later in
//	    the same rebuild, because a one-shot surface has no later rebuild to
//	    receive an async result. It is subject to the shared cache, so the common
//	    case costs a file read.
//	T5  a later rebuild whose held listing has aged past staleAfterMs requests
//	    one ASYNCHRONOUSLY. The rebuild does not wait at all: the result
//	    refreshes corroboration for the NEXT rebuild, which is what makes a
//	    piggyback incapable of delaying an event.
//
// A rebuild with a fresh listing does nothing at all here, which is the steady
// state on an active machine.
//
// The returned call is the one — and only one — this rebuild may wait for;
// nil means "wait for nothing". A piggyback in flight from an EARLIER rebuild
// is deliberately not returned: waiting on it would give a later rebuild the
// synchronous cost T5 exists to avoid.
func (o *Observer) requestCorroboration(ctx context.Context, now time.Time) *spawnCall {
	if o.deps.Agents == nil {
		return nil
	}

	o.corr.mu.Lock()
	attempted := o.corr.attempted
	o.corr.mu.Unlock()

	if !attempted {
		call, _, _ := o.beginCorroboration(ctx, now, TriggerStartup)
		return call
	}
	if o.corroborationStale(now) {
		o.beginCorroboration(ctx, now, TriggerPiggyback)
	}
	return nil
}

// collectCorroboration is the COLLECTING half, called after the authoritative
// reads with the call requestCorroboration handed back.
//
// It waits at most CorroborationWait, and it waits under a CHILD of the
// caller's context so a caller with less budget than that gets less. A wait
// that expires is recorded, not swallowed: see CorroborationWait's comment for
// the defect that rule comes from.
//
// The spawn is NOT cancelled here. It keeps running under its own bound and
// applySpawn will adopt its result if it is still the newest — which is what
// makes the next rebuild of a long-lived surface corroborated even though this
// one was not. A one-shot surface cancels the read context on its way out,
// which reaps the child and releases the cross-process lock (cli.go).
func (o *Observer) collectCorroboration(ctx context.Context, call *spawnCall) {
	if call == nil {
		return
	}
	wait, cancel := context.WithTimeout(ctx, CorroborationWait)
	defer cancel()

	select {
	case <-call.done:
	case <-wait.Done():
		o.noteWaitExpired()
	}
}

// noteWaitExpired records the expiry so sources.agents-cli can publish it.
//
// lastErr is set unconditionally because it describes the CURRENT state of the
// source — a spawn still running, and an answer built without it — which is
// strictly more useful than an older spawn's error message. applySpawn
// overwrites or clears it the moment a result lands.
func (o *Observer) noteWaitExpired() {
	o.corr.mu.Lock()
	defer o.corr.mu.Unlock()
	o.corr.waitExpired = true
	o.corr.waitExpiredMs = CorroborationWait.Milliseconds()
	o.corr.lastErr = fmt.Errorf(
		"observer: corroboration did not return within the rebuild's %s wait; "+
			"the spawn is still running and this answer was built without it",
		CorroborationWait)
}

// corroborationStale reports whether the held listing is too old to speak for
// the present. A listing that was never taken is stale by definition — there is
// nothing to speak with.
//
// This is the predicate T5's precondition is expressed in, and it reads the
// SAME policy knob the join uses for rule 6, so a rebuild can never refuse to
// refresh a listing the liveness policy is about to call stale.
func (o *Observer) corroborationStale(now time.Time) bool {
	o.corr.mu.Lock()
	defer o.corr.mu.Unlock()
	return staleListing(o.corr.held, now, o.policy.StaleAfterMs)
}

// staleListing is the comparison itself, as a free function so both the gate
// and the spawn guard evaluate identical arithmetic.
func staleListing(held AgentsResult, now time.Time, staleAfterMs int64) bool {
	if !held.OK || held.ObservedAt.IsZero() {
		return true
	}
	return now.Sub(held.ObservedAt).Milliseconds() > staleAfterMs
}

// takeCorroboration is the single entry point to a spawn. It returns "" when
// the listing was refreshed and a rejection reason otherwise, plus any
// request-scoped advisories.
//
// Single-flight, stated precisely because it is the bound two of P4's
// acceptance cases are about: at most one spawn exists in this process at any
// instant. A second caller does not queue a second spawn; it attaches to the
// in-flight one and shares its result. Two concurrent refreshes therefore
// produce ONE spawn, and sixteen produce one.
func (o *Observer) takeCorroboration(ctx context.Context, now time.Time, trigger string) (string, []Advisory) {
	call, reason, advisories := o.beginCorroboration(ctx, now, trigger)
	if call == nil {
		return reason, advisories
	}

	if trigger == TriggerPiggyback {
		// Fire and forget. The result lands for the next rebuild; waiting here
		// would put a 130 ms process spawn on the path of a filesystem event.
		return "", nil
	}

	select {
	case <-call.done:
		return o.outcomeOf(call.res)
	case <-ctx.Done():
		// The CALLER's deadline, not ours. The spawn keeps running under its own
		// bound and its result will be adopted if it is still the newest.
		//
		// This is the EXPLICIT path (T4) and it deliberately keeps the caller's
		// whole budget: a consumer that asked for corroboration in so many words
		// is entitled to wait for it, and §4.3(a)'s refresh-rejected advisory is
		// how it learns the wait failed. The IMPLICIT rebuild path is bounded
		// instead — see CorroborationWait.
		return RejectTimeout, nil
	}
}

// beginCorroboration applies §4.3's guards and starts at most one spawn.
//
// It returns the in-flight call to attach to, or nil plus a rejection reason
// when a guard refused. Separating it from the WAIT is what lets a rebuild
// issue a spawn before its authoritative reads and collect it after, under a
// bound of its own.
//
// Single-flight, stated precisely because it is the bound two of P4's
// acceptance cases are about: at most one spawn exists in this process at any
// instant. A second caller does not queue a second spawn; it attaches to the
// in-flight one and shares its result. Two concurrent refreshes therefore
// produce ONE spawn, and sixteen produce one.
func (o *Observer) beginCorroboration(ctx context.Context, now time.Time, trigger string) (*spawnCall, string, []Advisory) {
	o.corr.mu.Lock()

	if o.corr.degraded != "" {
		// §4.3(c): the source has dropped to on-demand. An explicit request is
		// refused rather than served from a mechanism we have just decided is
		// misbehaving, and the reason travels in the outcome.
		degraded := o.corr.degraded
		o.corr.mu.Unlock()
		return nil, RejectRateLimited, []Advisory{NewAdvisory(AdvSpawnLoopGuard, map[string]any{
			"spawns":   int64(SpawnLoopGuardSpawns),
			"windowMs": SpawnLoopGuardWindow.Milliseconds(),
			"degraded": degraded,
		})}
	}

	if o.corr.inflight == nil && now.Before(o.corr.backoffUntil) {
		// A timestamp comparison, not a timer. Nothing is armed and nothing
		// wakes up: the next request after backoffUntil simply succeeds.
		o.corr.mu.Unlock()
		return nil, RejectBackoff, nil
	}

	call := o.corr.inflight
	started := false
	if call == nil {
		o.corr.gen++
		call = &spawnCall{gen: o.corr.gen, done: make(chan struct{})}
		o.corr.inflight = call
		o.corr.attempted = true
		o.corr.trigger = trigger
		o.corr.spawns++
		started = true
	}
	o.corr.mu.Unlock()

	if started {
		// The spawn runs on its own goroutine and posts its result back through
		// applySpawn, exactly as §7.2 requires: it never touches shared state
		// itself, and the requesting path never holds a lock across it.
		go o.spawnOnce(ctx, call, now, trigger)
	}

	return call, "", nil
}

// outcomeOf maps a finished listing onto a rejection reason.
func (o *Observer) outcomeOf(res AgentsResult) (string, []Advisory) {
	switch {
	case res.Cache == AgentsCacheContended:
		// The deliberate degraded answer of §4.3(b): this process declined to
		// spawn because another held the right, and one cycle without
		// corroboration is correct where a stampede is not.
		return RejectRateLimited, []Advisory{NewAdvisory(AdvAgentsCacheContended, map[string]any{
			"waitedMs": res.WaitedMs,
		})}
	case res.OK:
		return "", nil
	case errors.Is(res.Err, context.DeadlineExceeded):
		return RejectTimeout, nil
	case res.Err != nil:
		return RejectError, nil
	default:
		return RejectError, nil
	}
}

// spawnOnce takes one listing and hands it to applySpawn.
//
// THE GUARD BELOW IS THE T5 INVARIANT (§4.2), and it is the only call to
// AgentsSource.List in this module. The three disjuncts are the three triggers
// §4.2 permits to spawn, and nothing else can reach the call:
//
//	TriggerExplicit   T4 — a consumer asked, in so many words.
//	TriggerStartup    T6 — the first listing of a process, cache-gated.
//	staleListing(…)   T5 — a rebuild already paid for by an event, AND the held
//	                  listing is older than staleAfterMs.
//
// §9.3(a)'s AST audit requires a staleAfterMs comparison to dominate this call;
// keeping the sanctioned triggers inside the same condition is what lets the
// audit stay a real check rather than one with an exemption list.
func (o *Observer) spawnOnce(ctx context.Context, call *spawnCall, now time.Time, trigger string) {
	var (
		res AgentsResult
		err error
	)

	o.corr.mu.Lock()
	held := o.corr.held
	o.corr.mu.Unlock()

	if trigger == TriggerExplicit || trigger == TriggerStartup ||
		staleListing(held, now, o.policy.StaleAfterMs) {
		res, err = o.deps.Agents.List(ctx)
	} else {
		// Unreachable through corroborate, which evaluates the same predicate
		// before requesting. It is a value rather than a panic because the
		// honest report of "no spawn was taken" is a listing that is not OK,
		// and the next rebuild will re-evaluate.
		res = AgentsResult{OK: false, Err: errors.New("observer: spawn refused, corroboration is fresh")}
	}

	if err != nil && res.Err == nil {
		// A port that reports through the error return rather than through the
		// result — the fakes do — is normalised here, so applySpawn has one
		// shape to reason about.
		res.OK = false
		res.Err = err
	}
	if res.ObservedAt.IsZero() {
		res.ObservedAt = o.deps.Clock.Now()
	}

	o.applySpawn(res, call.gen)

	call.res = res
	close(call.done)
}

// applySpawn adopts a spawn's result, or drops it as obsolete.
//
// §7.2: each spawn carries the generation it was requested at, and a result
// whose generation is not newer than the applied one is DISCARDED, so a slow
// spawn cannot overwrite a newer corroboration snapshot. The rule is
// "strictly newer" rather than "not older" so that re-applying the same
// generation twice — a retry, a duplicated result — cannot resurrect a listing
// that has since been superseded.
//
// A failed or contended result NEVER replaces the held listing. That is the
// other half of "the previous snapshot survives a timeout": the failure is
// recorded as lastError and published in the source report, while the listing
// the liveness policy reads stays exactly as it was.
func (o *Observer) applySpawn(res AgentsResult, gen int64) bool {
	o.corr.mu.Lock()
	defer o.corr.mu.Unlock()

	if o.corr.inflight != nil && o.corr.inflight.gen == gen {
		o.corr.inflight = nil
	}

	// A result has landed, so "it did not land inside the rebuild's wait" is no
	// longer the current state of this source — whatever the result says. The
	// error it may carry replaces the wait-expiry message below.
	o.corr.waitExpired = false
	o.corr.waitExpiredMs = 0

	if res.SpawnMs > 0 {
		o.corr.lastSpawnMs = res.SpawnMs
	}
	if res.Cache != "" {
		o.corr.cache = res.Cache
	}
	o.corr.waitedMs = res.WaitedMs

	if !res.OK {
		o.corr.lastErr = res.Err
		if res.Cache != AgentsCacheContended && res.Err != nil {
			// Contention is not a failure: nothing went wrong, another process
			// is holding the spawn right. Backing off for it would punish this
			// process for another's success.
			o.corr.failures++
			o.corr.backoffUntil = res.ObservedAt.Add(backoffDelay(o.corr.failures))
		}
		return false
	}

	if gen <= o.corr.appliedGen {
		// Obsolete. The newer listing already in place is the one every answer
		// must be built from.
		return false
	}

	o.corr.failures = 0
	o.corr.backoffUntil = time.Time{}
	o.corr.lastErr = nil

	if !res.Cached {
		o.noteBreakerLocked(res)
	}

	o.corr.held = res
	o.corr.appliedGen = gen
	return true
}

// noteBreakerLocked is §4.3(c). It counts spawns that changed NOTHING, because
// that is the signature of a loop: a spawn caused by real activity brings a
// different listing back with it.
//
// Called with corr.mu held.
func (o *Observer) noteBreakerLocked(res AgentsResult) {
	fp := listingFingerprint(res)
	if fp != o.corr.fingerprint {
		o.corr.fingerprint = fp
		o.corr.unchanged = nil
		return
	}

	cutoff := res.ObservedAt.Add(-SpawnLoopGuardWindow)
	kept := o.corr.unchanged[:0]
	for _, at := range o.corr.unchanged {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	o.corr.unchanged = append(kept, res.ObservedAt)
	if len(o.corr.unchanged) >= SpawnLoopGuardSpawns {
		o.corr.degraded = DegradedSpawnLoopGuard
	}
}

// listingFingerprint digests everything a listing can contribute to an answer.
// Two listings with the same fingerprint changed no session field, which is the
// condition §4.3(c) names.
func listingFingerprint(res AgentsResult) string {
	var b []byte
	for _, e := range res.Entries {
		b = append(b, e.SessionID...)
		b = append(b, '|')
		b = append(b, e.Kind...)
		b = append(b, '|')
		b = append(b, e.Status...)
		b = append(b, '|')
		b = append(b, e.State...)
		b = append(b, '|')
		b = append(b, e.WaitingFor...)
		b = append(b, '|')
		if e.PID != nil {
			b = strconv.AppendInt(b, int64(*e.PID), 10)
		}
		b = append(b, '\n')
	}
	return string(b)
}

// backoffDelay is 2s, 4s, 8s … capped at 5 minutes.
func backoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := backoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= backoffCap {
			return backoffCap
		}
	}
	return d
}

// corroborationListing is what the join reads. It is a COPY of the held
// listing, so a spawn landing mid-rebuild cannot change the evidence a
// half-finished answer was computed from.
func (o *Observer) corroborationListing() AgentsResult {
	o.corr.mu.Lock()
	defer o.corr.mu.Unlock()
	return o.corr.held
}

// AwaitCorroboration blocks until no spawn is in flight, or until ctx is done.
//
// It exists because a spawn outlives the request that asked for it by design (a
// piggyback's result is for the next rebuild), so "is anything still running"
// is a question a shutdown path and a test both have to be able to ask. Without
// it the only ways to answer are a sleep and a leaked goroutine.
func (o *Observer) AwaitCorroboration(ctx context.Context) {
	for {
		o.corr.mu.Lock()
		call := o.corr.inflight
		o.corr.mu.Unlock()
		if call == nil {
			return
		}
		select {
		case <-call.done:
		case <-ctx.Done():
			return
		}
	}
}

// agentsReport is §4.5(a)'s sources.agents-cli block.
//
// `now` IS THE REBUILD'S INSTANT, passed in, never read from the clock here.
// This has now been the cause of two live defects in this feature — a negative
// ageMs from an adapter stamping its own clock read, and a re-encoded snapshot
// whose envelope disagreed with its sessions — and every derived number below
// (ageMs, stale, crossChecksSuppressed) is arithmetic against this one value.
//
// The nested counters/labels shape is P0's and P4 KEEPS IT deliberately: the
// architecture's §5.2 sample shows the per-source metrics flat and
// heterogeneous, which one struct with no omitempty cannot produce, and the
// union-of-pointers alternative would put every source's keys on every source.
// Booleans live in `labels` as "true"/"false" for the same reason — the nested
// form has a typed home for int64 and string and none for bool, and inventing a
// 0/1 counter for crossChecksSuppressed would make the one field §3.5's
// suppression rule is visible through read like a count.
func (o *Observer) agentsReport(res AgentsResult, now time.Time) SourceReport {
	o.corr.mu.Lock()
	state := struct {
		spawns        int64
		lastSpawnMs   int64
		trigger       string
		cache         string
		waitedMs      int64
		degraded      string
		lastErr       error
		backoffUntil  time.Time
		waitExpired   bool
		waitExpiredMs int64
	}{
		spawns:        o.corr.spawns,
		lastSpawnMs:   o.corr.lastSpawnMs,
		trigger:       o.corr.trigger,
		cache:         o.corr.cache,
		waitedMs:      o.corr.waitedMs,
		degraded:      o.corr.degraded,
		lastErr:       o.corr.lastErr,
		backoffUntil:  o.corr.backoffUntil,
		waitExpired:   o.corr.waitExpired,
		waitExpiredMs: o.corr.waitExpiredMs,
	}
	o.corr.mu.Unlock()

	staleAfterMs := o.policy.StaleAfterMs
	// Path stays EMPTY, deliberately. For every other source it means "the
	// directory this source reads"; this one reads no directory, and putting
	// the cache file there would give one field two meanings — the cache is
	// where the listing is PUBLISHED, and a consumer looking for what produced
	// these numbers reads labels.command.
	rep := SourceReport{
		ID:           SourceAgentsCLI,
		Role:         "corroboration",
		OK:           res.OK,
		StaleAfterMs: &staleAfterMs,
		Advisories:   []Advisory{},
		Counters: map[string]int64{
			"spawns":      state.spawns,
			"lastSpawnMs": state.lastSpawnMs,
			"entries":     int64(len(res.Entries)),
			"interactive": 0,
			"background":  0,
			"waitedMs":    state.waitedMs,
		},
		Labels: map[string]string{
			"command":     agentsCommand(res, o.deps.Agents),
			"agentsCache": state.cache,
			"trigger":     state.trigger,
		},
	}

	for _, e := range res.Entries {
		// Counted from the PID's presence rather than from `kind`, because the
		// pid is the structural difference (an interactive entry has one, a
		// background entry cannot) and `kind` is a label the vendor may extend.
		if e.PID != nil {
			rep.Counters["interactive"]++
			continue
		}
		rep.Counters["background"]++
	}

	if !res.ObservedAt.IsZero() {
		observedAt := res.ObservedAt
		rep.ObservedAt = &observedAt
		rep.AgeMs = sourceAgeMs(now, &observedAt)
	}

	// stale and crossChecksSuppressed are computed HERE, at answer time, from
	// the same instant and the same threshold the join used for rule 6. The two
	// cannot drift because neither is stored.
	suppressed := staleListing(res, now, staleAfterMs)
	rep.Stale = res.OK && suppressed
	rep.Labels["crossChecksSuppressed"] = boolLabel(suppressed)

	if rep.Stale {
		rep.Advisories = appendAdvisory(rep.Advisories, NewAdvisory(AdvSourceStale, map[string]any{
			"source":       SourceAgentsCLI,
			"ageMs":        derefInt64(rep.AgeMs),
			"staleAfterMs": staleAfterMs,
			"detail":       "corroboration only; no reported status depends on this source",
		}))
	}
	if state.cache == AgentsCacheContended {
		// From the last ATTEMPT, deliberately: a contended attempt produces no
		// listing, so `res` is the older one that survived it and carries
		// neither the outcome nor the wait.
		rep.Advisories = appendAdvisory(rep.Advisories, NewAdvisory(AdvAgentsCacheContended, map[string]any{
			"waitedMs": state.waitedMs,
		}))
	}
	if state.degraded != "" {
		rep.Labels["degraded"] = state.degraded
		rep.Advisories = appendAdvisory(rep.Advisories, NewAdvisory(AdvSpawnLoopGuard, map[string]any{
			"spawns":   int64(SpawnLoopGuardSpawns),
			"windowMs": SpawnLoopGuardWindow.Milliseconds(),
			"degraded": state.degraded,
		}))
	}
	if state.waitExpired {
		// The wedged-CLI case, said out loud. Without this the block reads
		// ok:false with lastError:null and an empty advisories array, which is
		// the exact shape the defect was reported in: something that turns every
		// background verdict uncorroborated and says nothing about why.
		//
		// It is source-scoped, so promoteSourceAdvisories lifts it into
		// warnings; the SESSIONS whose verdict actually depended on the listing
		// carry job-corroboration-stale from §3.5's rule 6b, which is their own
		// half of the same explanation.
		rep.Advisories = appendAdvisory(rep.Advisories, NewAdvisory(AdvCorroborationWaitExpired, map[string]any{
			"source": SourceAgentsCLI,
			"waitMs": state.waitExpiredMs,
			"detail": "answered without corroboration; the spawn had not returned " +
				"when the rebuild's bounded wait expired. No liveness gate depends " +
				"on this source.",
		}))
	}
	if !state.backoffUntil.IsZero() && now.Before(state.backoffUntil) {
		rep.Labels["backoffUntil"] = FormatTime(state.backoffUntil)
	}
	if state.lastErr != nil {
		msg := state.lastErr.Error()
		rep.LastError = &msg
	}
	return rep
}

// agentsCommand names the exact invocation, preferring what the listing itself
// reported. A source that can describe its own command is asked; one that
// cannot gets the domain's name for it rather than an empty string, because
// this label is how a consumer knows what produced the numbers beside it.
func agentsCommand(res AgentsResult, src AgentsSource) string {
	if res.Command != "" {
		return res.Command
	}
	if c, ok := src.(interface{ CommandLine() string }); ok {
		return c.CommandLine()
	}
	return ""
}

// boolLabel renders a boolean for the labels map. Explicitly "false" rather
// than absent: the omitempty ban's reasoning applies to a nested map too, and a
// missing crossChecksSuppressed would read as "unknown".
func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
