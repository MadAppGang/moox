package observer

import (
	"errors"
	"sync"
)

// Observer is the use case: it holds the driven ports, runs the
// refresh-and-publish loop, and implements the SessionSource incoming port so
// that every driving adapter talks to it through the same four methods a
// socket-attached client implements.
//
// The phase plan wires it up in order, and each piece arrives with the tests
// that pin it:
//
//	P1  New(), the registry half of the join, Snapshot()   <- DONE
//	P3  the jobs half, the background liveness inputs, the timing tail <- DONE
//	P4  Refresh(), the serialized single-flight spawn path, generations
//	P5  Run()/runLoop(), the watch reconciliation schedule, diff -> Subscribe()
//	P6  Enrich() and its (sessionId, path, size, mtime) cache  <- DONE
//
// What it must NOT become, recorded here because it is the failure mode
// hexagonal was chosen to prevent: a type that reaches for the filesystem,
// tmux or the claude binary directly. Every fact arrives through a port in
// ports.go. Every answer leaves through wire. If a later phase finds itself
// wanting an exec.Command in this file, the port is missing, not the rule.
type Observer struct {
	// Deps are the seven driven ports plus the clock. They are a value rather
	// than seven fields on Observer so that a test can build one literal and a
	// composition root can build one literal, and neither has to know the
	// order.
	deps Deps

	policy Policy
	paths  Paths
	// timingTailBytes is §8.3 item 9's injectable bound for the timing pass,
	// always resolved to a positive number by New. It is passed explicitly to
	// every ReadTiming call and published as
	// sources.transcripts.tailBytesTiming, so the bound a consumer is told
	// about is the bound that was used.
	timingTailBytes int
	// factsTailBytes is the same for the ENRICHMENT pass, and it is a SECOND
	// field rather than one shared bound because §7.3's split is two
	// measurements: 64 KiB for correctness, 200 KiB for enrichment. A test
	// asserts they differ, so collapsing them fails rather than silently
	// halving or tripling one of the two reads.
	factsTailBytes int
	// notCapable is declared, not derived: a consumer needs to know it will
	// never get a frame at the instant a time threshold is crossed, rather
	// than waiting for one that cannot come.
	notCapable []string

	// mu serialises rebuilds. §7.2 specifies ONE serialized
	// refresh-and-publish path, and P1 honours it with the simplest thing that
	// is that: Snapshot holds the lock for the whole build. The alternative —
	// concurrent rebuilds sharing previousState — would make the gate-error
	// rule's "previous value" depend on interleaving.
	mu   sync.Mutex
	prev previousState
	// enrichCache is §7.3's enrichment cache, one entry per session, keyed
	// internally on (sessionId, path, size, mtime) — see enrich.go. It is
	// guarded by mu, like prev, because Enrich reads prev.jobs for the
	// background locator and writes here.
	enrichCache map[string]enrichEntry

	// corr is P4's corroboration state: the held listing, the single-flight
	// slot, the generation counter, the backoff timestamp and the circuit
	// breaker (refresh.go). It is held by VALUE and its zero value is usable —
	// no listing, nothing attempted, generation zero — so New needs no line for
	// it and an Observer built by any surface starts in the same state.
	//
	// It is a separate lock from mu on purpose: a spawn outlives the rebuild
	// that requested it (a piggyback's result is for the NEXT rebuild), so the
	// goroutine that adopts it must not need the lock a rebuild holds.
	corr corroboration
}

// Paths are the locations the sources read, carried so the snapshot's source
// reports can name them. The core never opens any of them; the adapters do.
// They are here rather than discovered from the ports because a port that
// could be asked for its own path would be a port leaking its adapter's
// vocabulary.
type Paths struct {
	ClaudeRoot string
	Registry   string
	Jobs       string
	Projects   string
}

// DefaultNotCapable is what every surface declares it will not do. The one
// entry is the consequence of having no server-side timer: two of the liveness
// rules are clock-sensitive, so `live` can change between two answers with no
// filesystem event, and nothing wakes up to publish that crossing. The
// intended answer is the consumer's own render-time clock.
func DefaultNotCapable() []string { return []string{"time-transitions"} }

// Config is everything the Observer needs that is not a port.
type Config struct {
	// Policy is the liveness and staleness thresholds. A zero Policy is
	// replaced by DefaultPolicy, because a zero SuspectBusyAfterMs would flag
	// every busy session as suspect and a zero TranscriptRetentionMs would
	// make every background record beyond retention.
	Policy Policy
	Paths  Paths
	// TimingTailBytes overrides the timing tail size. Zero or less means
	// DefaultTimingTailBytes. A test injects a small value to reach the
	// activity-since-truncated path, which is otherwise unreachable at 64 KiB
	// against a measured 1 912 B worst case.
	TimingTailBytes int
	// FactsTailBytes overrides the enrichment tail size. Zero or less means
	// DefaultFactsTailBytes. AC12's case — a colour record BEYOND the tail —
	// is unreachable at 200 KiB against the measured population (0 misses over
	// all 35 transcripts that carry one), so the injection is what makes the
	// fallback path testable at all.
	FactsTailBytes int
	// NotCapable overrides DefaultNotCapable. Nil means the default; an
	// explicitly empty non-nil slice means "capable of everything", which no
	// surface should claim but which a test may need.
	NotCapable []string
}

// ErrMissingPort is returned by New when a port the requested phase needs is
// nil. A nil port is a programming error, not a degraded mode: the degraded
// modes live INSIDE the adapters (a nowatch Watcher, an agents source
// reporting OK false), so "this machine has no kqueue" is a value in the
// snapshot rather than a nil check in the core.
var ErrMissingPort = errors.New("observer: missing port")

// New builds an Observer.
//
// It validates the ports the snapshot path actually dereferences. Each phase
// adds its own port to this check along with the code that uses it: P1 added
// Registry, Proc, Panes and Clock; P3 adds Jobs and Transcripts.
//
// Jobs and Transcripts are REQUIRED rather than optional-and-nil-checked, and
// that is the deliberate choice. A nil Jobs port would make the observer answer
// "there are no background agents on this machine" — a well-formed, confident,
// wrong answer that no test and no user could distinguish from the truth. A
// surface that forgets to wire them should fail to construct, loudly, at
// startup. Watch and Agents stay nil-able until P5 and P4 dereference them.
func New(deps Deps, cfg Config) (*Observer, error) {
	switch {
	case deps.Registry == nil:
		return nil, errors.Join(ErrMissingPort, errors.New("RegistrySource"))
	case deps.Jobs == nil:
		return nil, errors.Join(ErrMissingPort, errors.New("JobsSource"))
	case deps.Transcripts == nil:
		return nil, errors.Join(ErrMissingPort, errors.New("TranscriptSource"))
	case deps.Proc == nil:
		return nil, errors.Join(ErrMissingPort, errors.New("ProcLive"))
	case deps.Panes == nil:
		return nil, errors.Join(ErrMissingPort, errors.New("PanesLister"))
	case deps.Clock == nil:
		return nil, errors.Join(ErrMissingPort, errors.New("Clock"))
	}

	policy := cfg.Policy
	if policy == (Policy{}) {
		policy = DefaultPolicy()
	}
	notCapable := cfg.NotCapable
	if notCapable == nil {
		notCapable = DefaultNotCapable()
	}
	// Resolved here rather than left at 0 and delegated to the adapter: the
	// snapshot PUBLISHES the bound that was in force, and a published 0 would
	// tell a consumer diagnosing a null lastActivityAt that no bytes were read.
	timingTail := cfg.TimingTailBytes
	if timingTail <= 0 {
		timingTail = DefaultTimingTailBytes
	}
	factsTail := cfg.FactsTailBytes
	if factsTail <= 0 {
		factsTail = DefaultFactsTailBytes
	}

	return &Observer{
		deps:            deps,
		policy:          policy,
		paths:           cfg.Paths,
		timingTailBytes: timingTail,
		factsTailBytes:  factsTail,
		notCapable:      notCapable,
		prev:            newPreviousState(),
		enrichCache:     map[string]enrichEntry{},
	}, nil
}

// Policy returns the thresholds in force, so a caller can report them.
func (o *Observer) Policy() Policy { return o.policy }

// Deps is every port the Observer needs. One struct, one wiring site per
// surface — which is hexagonal's stated cost, paid explicitly rather than
// smeared across constructors.
//
// A nil port is a programming error, not a degraded mode: the degraded modes
// are inside the adapters (a nowatch Watcher, an agents source that reports
// OK false), so that "this machine has no kqueue" is a value in the snapshot
// rather than a nil check in the core.
type Deps struct {
	Registry    RegistrySource
	Jobs        JobsSource
	Transcripts TranscriptSource
	Agents      AgentsSource
	Panes       PanesLister
	// Members is the optional foreign-server rule (moox §4.5). Nil keeps the
	// pane-set-only behaviour.
	Members MembershipSource
	Proc    ProcLive
	Watch   Watcher
	Clock   Clock
}

// Compile-time proof that nothing has drifted: Observer is intended to satisfy
// SessionSource, and it will, in P1 through P6. The assertion is commented out
// rather than deleted so that the phase which adds the last method uncomments
// one line instead of discovering the intent from a design document.
//
//	var _ SessionSource = (*Observer)(nil)
