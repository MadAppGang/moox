package observer_test

// Code review round 2, A5: subscribing was not atomic with publishing.
//
// WatchLoop.publish captured its subscriber list under the mutex, RELEASED it,
// decorated the snapshot (attachWatch), and only then stored it. A subscriber
// that registered inside that window was not in the captured list AND loaded
// the previous published value — nil on the first publication — so it was
// either never given a baseline at all (a quiet machine: forever) or left one
// state behind until some unrelated publication.
//
// The stress test widens the window the old code left open (a snapshot with
// many source reports makes attachWatch's copy measurable) and lands a
// Subscribe at a spread of instants across it. The barrier tests pin the
// interleavings by name: a subscriber attached DURING the read of an EMPTY
// first snapshot with no later event, and one attached between publications.

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

// barrierCore blocks its read until released, and signals when the read has
// finished — the two instants the tests below need to aim at.
type barrierCore struct {
	mu       sync.Mutex
	gate     chan struct{} // nil: do not block
	readDone chan struct{} // closed as the read returns, then replaced
	sources  int
	sessions []observer.Session
	at       time.Time
}

func (c *barrierCore) Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.mu.Lock()
	gate, done := c.gate, c.readDone
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	c.mu.Lock()
	sources := make(map[string]observer.SourceReport, c.sources+2)
	for _, id := range []string{observer.SourceRegistry, observer.SourceJobs} {
		sources[id] = observer.SourceReport{ID: id, OK: true, Advisories: []observer.Advisory{}}
	}
	for i := 0; i < c.sources; i++ {
		id := fmt.Sprintf("extra-%04d", i)
		sources[id] = observer.SourceReport{ID: id, OK: true, Advisories: []observer.Advisory{}}
	}
	sessions := append([]observer.Session(nil), c.sessions...)
	c.mu.Unlock()
	if done != nil {
		close(done)
	}
	return &observer.Snapshot{
		ObservedAt: c.at,
		Counts:     observer.NewCounts(),
		Sources:    sources,
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}, nil
}

func newBarrierLoop(t *testing.T, core *barrierCore) *observer.WatchLoop {
	t.Helper()
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:  core,
		Watch: fakes.NewWatcher(4),
		Tree:  newFakeTree(nil),
		Clock: fakes.NewClock(loopT0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

// drainNow reads everything queued right now.
func drainNow(ch <-chan observer.Event) []observer.Event {
	var out []observer.Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func spin(d time.Duration) {
	for start := time.Now(); time.Since(start) < d; {
	}
}

func TestASubscriberThatRacesThePublicationIsNeverLeftWithoutItsState(t *testing.T) {
	iterations := 400
	if testing.Short() {
		iterations = 100
	}
	rng := rand.New(rand.NewSource(1))
	missed := 0
	for i := 0; i < iterations; i++ {
		core := &barrierCore{sources: 3000, at: loopT0, readDone: make(chan struct{})}
		loop := newBarrierLoop(t, core)
		ctx, cancel := context.WithCancel(context.Background())

		var ch <-chan observer.Event
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = loop.Rebuild(ctx, "startup")
		}()
		delay := time.Duration(rng.Intn(600)) * time.Microsecond
		go func() {
			defer wg.Done()
			<-core.readDone
			spin(delay)
			var err error
			ch, err = loop.Subscribe(ctx, observer.Filter{})
			if err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()

		// Both calls have returned, and both deliver synchronously, so what is
		// queued now is everything this subscriber will ever get: the machine
		// is quiet from here on.
		held := int64(0)
		for _, e := range drainNow(ch) {
			if e.Type == observer.EventSnapshot || e.Type == observer.EventResync {
				held = e.Seq
			}
		}
		if held != loop.Latest().Seq {
			missed++
			if missed <= 3 {
				t.Errorf("iteration %d (subscribed %v after the read): the subscriber holds "+
					"seq %d, the loop published seq %d, and nothing further will ever come",
					i, delay, held, loop.Latest().Seq)
			}
		}
		cancel()
	}
	if missed > 0 {
		t.Fatalf("%d of %d subscribers were left without the published state", missed, iterations)
	}
}

// Interleaving (a): attached while the FIRST read is in flight, the snapshot
// is EMPTY, and nothing ever happens afterwards. Exactly one event: a baseline
// of nothing.
func TestASubscriberAttachedDuringAnEmptyFirstReadGetsOneBaselineAndNothingElse(t *testing.T) {
	gate := make(chan struct{})
	core := &barrierCore{gate: gate, at: loopT0, readDone: make(chan struct{})}
	loop := newBarrierLoop(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rebuilt := make(chan struct{})
	go func() {
		_, _, _ = loop.Rebuild(ctx, "startup")
		close(rebuilt)
	}()
	ch, err := loop.Subscribe(ctx, observer.Filter{}) // the read is blocked on gate
	if err != nil {
		t.Fatal(err)
	}
	if got := drainNow(ch); len(got) != 0 {
		t.Fatalf("a subscriber got %v before anything was published", got)
	}
	close(gate)
	<-rebuilt

	got := drainNow(ch)
	if len(got) != 1 || got[0].Type != observer.EventSnapshot || got[0].Seq != 1 ||
		got[0].Snapshot == nil || len(got[0].Snapshot.Sessions) != 0 {
		t.Fatalf("got %+v, want exactly one baseline snapshot (seq 1) of an empty machine", got)
	}
	time.Sleep(50 * time.Millisecond)
	if more := drainNow(ch); len(more) != 0 {
		t.Fatalf("a quiet machine sent %d more events", len(more))
	}
}

// Interleaving (b): attached between two publications. The baseline is the
// first, the deltas of the second follow it, and the subscriber ends holding
// the second.
func TestASubscriberAttachedBetweenPublicationsEndsHoldingTheLatest(t *testing.T) {
	live := true
	core := &barrierCore{at: loopT0}
	loop := newBarrierLoop(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	ch, err := loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	core.mu.Lock()
	core.sessions = []observer.Session{{SessionID: "s1", Kind: observer.KindInteractive,
		Status: observer.StatusWaiting, Attention: true, Live: &live, ObservedAt: loopT0}}
	core.mu.Unlock()
	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatal(err)
	}
	got := drainNow(ch)
	if len(got) != 2 || got[0].Type != observer.EventSnapshot || got[0].Seq != 1 ||
		got[1].Type != observer.EventSessionAdded || got[1].SessionID != "s1" {
		t.Fatalf("got %+v, want baseline seq 1 then session_added s1", got)
	}
}

// Code review round 2, A13: a subscriber whose queue overflowed was marked for
// resync, and the resync was attempted only at the NEXT publication. A slow
// consumer that missed the last transition of a burst and then caught up on a
// quiet machine drained its stale deltas and was never told what it missed —
// with no timer by design, "never" is literal. Now the overflow itself replaces
// the stale queue with one resync carrying the current state.
func TestAnOverflowedSubscriberRecoversByDrainingAloneWithNoFurtherPublication(t *testing.T) {
	live := true
	core := &barrierCore{at: loopT0}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:             core,
		Watch:            fakes.NewWatcher(4),
		Tree:             newFakeTree(nil),
		Clock:            fakes.NewClock(loopT0),
		SubscriberBuffer: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	ch, err := loop.Subscribe(ctx, observer.Filter{}) // baseline: 1 of 2 slots
	if err != nil {
		t.Fatal(err)
	}

	// A burst of five transitions while the consumer is not reading.
	for i, st := range []observer.Status{observer.StatusBusy, observer.StatusIdle,
		observer.StatusWaiting, observer.StatusBusy, observer.StatusWaiting} {
		core.mu.Lock()
		core.sessions = []observer.Session{{SessionID: "s1", Kind: observer.KindInteractive,
			Status: st, Attention: st == observer.StatusWaiting, Live: &live, ObservedAt: loopT0,
			Name: fmt.Sprintf("step-%d", i)}}
		core.mu.Unlock()
		if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
			t.Fatal(err)
		}
	}

	// The machine goes quiet. The consumer catches up — and that is all.
	events := drainNow(ch)
	state := map[string]observer.Session{}
	var last observer.Event
	for _, e := range events {
		last = e
		switch e.Type {
		case observer.EventSnapshot, observer.EventResync:
			state = map[string]observer.Session{}
			for _, s := range e.Snapshot.Sessions {
				state[s.SessionID] = s
			}
		case observer.EventSessionAdded, observer.EventSessionChanged:
			state[e.SessionID] = *e.Session
		case observer.EventSessionRemoved:
			delete(state, e.SessionID)
		}
	}
	latest := loop.Latest()
	if got := state["s1"]; got.Name != "step-4" || got.Status != observer.StatusWaiting {
		t.Fatalf("after draining, the consumer holds %+v (from %d events, last %v seq %d); the "+
			"loop published seq %d with s1 waiting as step-4, and no further publication is "+
			"coming", got, len(events), last.Type, last.Seq, latest.Seq)
	}
}

// sourceFlipCore reports an empty machine whose corroboration source goes
// from failing to healthy — a change to SOURCES only.
type sourceFlipCore struct {
	mu sync.Mutex
	ok bool
}

func (c *sourceFlipCore) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.mu.Lock()
	ok := c.ok
	c.mu.Unlock()
	rep := observer.SourceReport{ID: observer.SourceAgentsCLI, Role: "corroboration", OK: ok,
		Advisories: []observer.Advisory{}}
	if !ok {
		msg := "the spawn failed"
		rep.LastError = &msg
	}
	return &observer.Snapshot{
		ObservedAt: loopT0,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{observer.SourceAgentsCLI: rep},
		Warnings:   []observer.Advisory{},
		Sessions:   []observer.Session{},
		NotCapable: observer.DefaultNotCapable(),
	}, nil
}

// Code review round 2, A12: the loop published only session diffs, so a
// change to a source's HEALTH with no session change never reached an
// existing subscriber — an MCP client that attached while corroboration was
// failing kept reporting the failure after it recovered.
func TestASourceOnlyHealthChangeReachesAnExistingSubscriber(t *testing.T) {
	core := &sourceFlipCore{}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:  core,
		Watch: fakes.NewWatcher(4),
		Tree:  newFakeTree(nil),
		Clock: fakes.NewClock(loopT0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	ch, err := loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	drainNow(ch)

	core.mu.Lock()
	core.ok = true
	core.mu.Unlock()
	if _, _, err := loop.Rebuild(ctx, "refresh"); err != nil {
		t.Fatal(err)
	}
	var got *observer.Snapshot
	for _, e := range drainNow(ch) {
		if e.Type == observer.EventSources {
			got = e.Snapshot
		}
	}
	if got == nil {
		t.Fatal("the corroboration source recovered and the subscriber was not told")
	}
	if src := got.Sources[observer.SourceAgentsCLI]; !src.OK || src.LastError != nil {
		t.Fatalf("the sources event carries %+v, want the recovered report", src)
	}

	// And an unchanged republication says nothing: health, not churn.
	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatal(err)
	}
	for _, e := range drainNow(ch) {
		if e.Type == observer.EventSources {
			t.Fatal("a publication with unchanged source health emitted a sources event")
		}
	}
}

// A10, the stream half: a subscriber that asks for the dead population is told
// that a session DIED (a change, which it keeps and can show as ended), and
// told it was REMOVED only when its record is gone. A default subscriber is
// told of the death as a removal, exactly as before.
func TestAnIncludeDeadSubscriberSeesADeathAsAChangeAndARemovalAsARemoval(t *testing.T) {
	live, dead := true, false
	core := &barrierCore{at: loopT0}
	running := observer.Session{SessionID: "job", Kind: observer.KindBackground, Status: observer.StatusWorking,
		Live: &live, LiveReason: observer.LiveReasonJobNonTerminalCorroborated, ObservedAt: loopT0}
	core.sessions = []observer.Session{running}
	loop := newBarrierLoop(t, core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	all, err := loop.Subscribe(ctx, observer.Filter{IncludeDead: true})
	if err != nil {
		t.Fatal(err)
	}
	def, err := loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	drainNow(all)
	drainNow(def)

	ended := running
	ended.Status, ended.Live, ended.LiveReason = observer.StatusDone, &dead, observer.LiveReasonJobTerminal
	core.mu.Lock()
	core.sessions = []observer.Session{ended}
	core.mu.Unlock()
	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatal(err)
	}
	got := drainNow(all)
	if len(got) != 1 || got[0].Type != observer.EventSessionChanged || got[0].Session.Live == nil || *got[0].Session.Live {
		t.Fatalf("includeDead subscriber got %+v on a death, want one session_changed to live:false", got)
	}
	if d := drainNow(def); len(d) != 1 || d[0].Type != observer.EventSessionRemoved || d[0].Reason != observer.RemovedJobTerminal {
		t.Fatalf("default subscriber got %+v, want session_removed(job-terminal)", d)
	}

	core.mu.Lock()
	core.sessions = []observer.Session{}
	core.mu.Unlock()
	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatal(err)
	}
	if got := drainNow(all); len(got) != 1 || got[0].Type != observer.EventSessionRemoved || got[0].Reason != observer.RemovedRecordRemoved {
		t.Fatalf("includeDead subscriber got %+v when the record went, want session_removed(record-removed)", got)
	}
}
