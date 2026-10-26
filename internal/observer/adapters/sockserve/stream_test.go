package sockserve_test

// The stream: client-relative membership (H13), enrichment negotiation (M9),
// backpressure and resync, AC9's run-forever assertion, and AC11's
// CLI-equals-socket identity.
//
// The harness, the client and the fixtures are in sockserve_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// ---------------------------------------------------------------------------
// H13 — client-relative membership
// ---------------------------------------------------------------------------

// The assertion H13 exists for: a session LEAVING one client's filter is a
// removal FOR THAT CLIENT ONLY, and the same rebuild is a plain change for a
// client watching everything.
//
// Both clients are attached to the same server over the same two rebuilds, so
// the two answers cannot be explained by different inputs — they differ only by
// filter, which is the property a consumer that re-implemented the filter
// itself would get wrong.
func TestAFilteredSubscriberGetsClientRelativeAdditionsAndRemovals(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil), session("s2", nil))
	h.publish()

	everything := h.dial()
	everything.handshake()

	waiting := h.dial()
	waiting.handshake()
	waiting.send(wire.ClientFrame{
		Type:   wire.FrameSubscribe,
		ReqID:  ptr("r-w"),
		Filter: &wire.ClientFilter{Status: []string{string(observer.StatusWaiting)}},
	})
	base := waiting.expect(wire.FrameSnapshot)
	if got := len(base.Snapshot.Sessions); got != 0 {
		t.Fatalf("the filtered baseline carries %d sessions, want 0 — nothing is waiting yet", got)
	}

	// s2 starts waiting: it ENTERS the filtered view.
	h.core.set(t0.Add(time.Second),
		session("s1", nil),
		session("s2", func(s *observer.Session) {
			s.Status = observer.StatusWaiting
			s.RawStatus = "waiting"
			s.WaitReason = observer.WaitReasonInputNeeded
			s.Attention = true
		}),
	)
	h.publish()

	added := waiting.expect(wire.FrameSessionAdded)
	if added.SessionID != "s2" {
		t.Fatalf("session_added for %q, want s2", added.SessionID)
	}
	if added.Session == nil || added.Session.Status != string(observer.StatusWaiting) {
		t.Fatalf("session_added carries %+v, want a waiting s2", added.Session)
	}

	changed := everything.expect(wire.FrameSessionChanged)
	if changed.SessionID != "s2" {
		t.Fatalf("the unfiltered client saw a change for %q, want s2", changed.SessionID)
	}
	if !containsString(changed.Changed, "status") {
		t.Fatalf("changed[] = %v, want it to name status", changed.Changed)
	}

	// s2 stops waiting: it LEAVES the filtered view. Same rebuild, two
	// different truths.
	h.core.set(t0.Add(2*time.Second), session("s1", nil), session("s2", nil))
	h.publish()

	removed := waiting.expect(wire.FrameSessionRemoved)
	if removed.SessionID != "s2" || removed.Reason != observer.RemovedFilteredOut {
		t.Fatalf("session_removed = (%q, %q), want (s2, %q)",
			removed.SessionID, removed.Reason, observer.RemovedFilteredOut)
	}

	back := everything.expect(wire.FrameSessionChanged)
	if back.Type != wire.FrameSessionChanged || back.SessionID != "s2" {
		t.Fatalf("the unfiltered client saw %q for %q; a session leaving somebody else's filter is not a removal",
			back.Type, back.SessionID)
	}
}

// The filtered baseline and the filtered deltas go through ONE predicate, so
// the snapshot a filtered client is given contains exactly the sessions its
// deltas will be about — and the envelope's counts describe that set rather
// than the whole machine.
func TestTheFilteredBaselineAndItsCountsDescribeOnlyTheFilteredView(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0,
		session("s1", nil),
		session("s2", func(s *observer.Session) {
			s.Kind = observer.KindBackground
			s.Status = observer.StatusBlocked
			s.RawStatus = "blocked"
			s.Attention = true
			s.PID = nil
			s.ShortID = ptr("s2short")
		}),
	)
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{
		Type:   wire.FrameSubscribe,
		ReqID:  ptr("r-bg"),
		Filter: &wire.ClientFilter{Kind: []string{string(observer.KindBackground)}},
	})

	snap := c.expect(wire.FrameSnapshot)
	if snap.ReqID == nil || *snap.ReqID != "r-bg" {
		t.Fatalf("the baseline did not echo the subscribe's reqId: %v", snap.ReqID)
	}
	if n := len(snap.Snapshot.Sessions); n != 1 || snap.Snapshot.Sessions[0].SessionID != "s2" {
		t.Fatalf("the filtered baseline carries %d sessions, want only s2", n)
	}
	counts := snap.Snapshot.Counts
	if counts.Total != 1 || counts.Attention != 1 {
		t.Fatalf("counts = %+v, want total 1 and attention 1 over the filtered view", counts)
	}
	if counts.ByKind[string(observer.KindInteractive)] != 0 {
		t.Fatalf("counts.byKind still counts the interactive session: %+v", counts.ByKind)
	}
	if counts.ByKind[string(observer.KindBackground)] != 1 {
		t.Fatalf("counts.byKind does not count the background session: %+v", counts.ByKind)
	}
	// Every known key is still present, zeros included.
	if len(counts.ByStatus) != len(observer.AllStatuses) {
		t.Fatalf("counts.byStatus has %d keys, want all %d", len(counts.ByStatus), len(observer.AllStatuses))
	}
}

// A re-subscribe replaces the stream. The old subscription's deltas must not
// leak into the new view — a client that changed its filter and then received
// events computed against the old one could not reconcile them.
func TestASecondSubscribeReplacesTheFilterAndTheStream(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil), session("s2", nil))
	h.publish()

	c := h.dial()
	c.handshake()

	c.send(wire.ClientFrame{
		Type:   wire.FrameSubscribe,
		ReqID:  ptr("r-1"),
		Filter: &wire.ClientFilter{CWDPrefix: "/nowhere"},
	})
	if n := len(c.expect(wire.FrameSnapshot).Snapshot.Sessions); n != 0 {
		t.Fatalf("the narrow baseline carries %d sessions, want 0", n)
	}

	c.send(wire.ClientFrame{Type: wire.FrameSubscribe, ReqID: ptr("r-2")})
	snap := c.expect(wire.FrameSnapshot)
	if n := len(snap.Snapshot.Sessions); n != 2 {
		t.Fatalf("the widened baseline carries %d sessions, want 2", n)
	}

	// And the deltas now follow the NEW filter: a change to s1 arrives, which
	// the previous subscription would have filtered out.
	h.core.set(t0.Add(time.Second),
		session("s1", func(s *observer.Session) { s.Status = observer.StatusBusy; s.RawStatus = "busy" }),
		session("s2", nil))
	h.publish()

	f := c.expect(wire.FrameSessionChanged)
	if f.SessionID != "s1" {
		t.Fatalf("session_changed for %q, want s1", f.SessionID)
	}
}

// ---------------------------------------------------------------------------
// M9 — enrichment is negotiated, both ways
// ---------------------------------------------------------------------------

// enrichedFixture wires the transcript fake so one session has facts to find.
func enrichedFixture(h *harness, sessionID string) {
	path := "/fixture/projects/-home-user/" + sessionID + ".jsonl"
	h.set.Transcripts.ByGlob[sessionID] = observer.TranscriptLocation{
		Found:     true,
		Path:      path,
		SizeBytes: 3_187_486,
		ModTime:   t0,
	}
	h.set.Transcripts.Facts[path] = observer.TranscriptFacts{
		Path:           path,
		SizeBytes:      3_187_486,
		TailBytes:      204_800,
		Truncated:      true,
		AgentColor:     "green",
		ColorFound:     true,
		APIErrors:      2,
		Subagents:      ptr(7),
		SubagentsKnown: true,
	}
}

func TestEnrichmentIsNegotiatedBothWays(t *testing.T) {
	t.Run("enrich absent means we did not look", func(t *testing.T) {
		h := newHarness(t, func(o *options) { o.withEnricher = true })
		enrichedFixture(h, "s1")
		h.core.set(t0, session("s1", nil))
		h.publish()

		_, snap := h.dial().handshake()
		got := snap.Snapshot.Sessions[0]
		if got.TranscriptCollected {
			t.Fatal("an unnegotiated subscription was enriched; §7.5(f) requires transcriptCollected:false")
		}
		if got.Transcript != nil {
			t.Fatalf("transcript = %+v, want null", got.Transcript)
		}
	})

	t.Run("enrich true means the facts survive the round trip", func(t *testing.T) {
		h := newHarness(t, func(o *options) { o.withEnricher = true })
		enrichedFixture(h, "s1")
		h.core.set(t0, session("s1", nil))
		h.publish()

		c := h.dial()
		c.handshake()
		c.send(wire.ClientFrame{Type: wire.FrameSubscribe, ReqID: ptr("r-e"), Enrich: ptr(true)})

		snap := c.expect(wire.FrameSnapshot)
		got := snap.Snapshot.Sessions[0]
		if !got.TranscriptCollected {
			t.Fatal("transcriptCollected is false on an enrich:true subscription")
		}
		if got.Transcript == nil {
			t.Fatal("the transcript facts did not survive the round trip")
		}
		if got.Transcript.AgentColor != "green" || got.Transcript.APIErrors != 2 {
			t.Fatalf("facts = %+v, want the fixture's colour and error count", got.Transcript)
		}
		if got.Transcript.Subagents == nil || *got.Transcript.Subagents != 7 {
			t.Fatalf("subagents = %v, want 7", got.Transcript.Subagents)
		}

		// And every subsequent delta for that connection carries them too: a
		// client told a session has a colour must not then be sent the same
		// session without one.
		h.core.set(t0.Add(time.Second),
			session("s1", func(s *observer.Session) { s.Status = observer.StatusBusy; s.RawStatus = "busy" }))
		h.publish()

		f := c.expect(wire.FrameSessionChanged)
		if f.Session == nil || !f.Session.TranscriptCollected || f.Session.Transcript == nil {
			t.Fatalf("a delta on an enriched subscription dropped the facts: %+v", f.Session)
		}
		if f.Session.Transcript.AgentColor != "green" {
			t.Fatalf("the delta's colour is %q, want green", f.Session.Transcript.AgentColor)
		}
	})

	t.Run("a server with no enricher declares it and does not enrich", func(t *testing.T) {
		h := newHarness(t, nil) // no Enricher port
		enrichedFixture(h, "s1")
		h.core.set(t0, session("s1", nil))
		h.publish()

		c := h.dial()
		hello, _ := c.handshake()
		if containsString(hello.Capabilities, wire.CapEnrich) {
			t.Fatal("hello declares enrich with no enricher wired")
		}

		c.send(wire.ClientFrame{Type: wire.FrameSubscribe, ReqID: ptr("r-e"), Enrich: ptr(true)})
		snap := c.expect(wire.FrameSnapshot)
		if snap.Snapshot.Sessions[0].TranscriptCollected {
			t.Fatal("a server with no enricher claimed it collected a transcript")
		}
	})
}

// A one-off enriched read does NOT change the subscription's setting, so it
// cannot silently make the whole stream expensive — and its inverse cannot
// silently disable a subscription's enrichment either.
func TestAOneOffEnrichedSnapshotDoesNotChangeTheSubscription(t *testing.T) {
	h := newHarness(t, func(o *options) { o.withEnricher = true })
	enrichedFixture(h, "s1")
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()

	c.send(wire.ClientFrame{Type: wire.FrameSnapshot, ReqID: ptr("r-1"), Enrich: ptr(true)})
	one := c.expect(wire.FrameSnapshot)
	if !one.Snapshot.Sessions[0].TranscriptCollected {
		t.Fatal("a one-off enrich:true snapshot was not enriched")
	}

	// The standing setting is untouched: the next delta is unenriched.
	h.core.set(t0.Add(time.Second),
		session("s1", func(s *observer.Session) { s.Status = observer.StatusBusy; s.RawStatus = "busy" }))
	h.publish()

	f := c.expect(wire.FrameSessionChanged)
	if f.Session.TranscriptCollected {
		t.Fatal("a one-off enriched read made the whole stream expensive")
	}
}

// ---------------------------------------------------------------------------
// backpressure
// ---------------------------------------------------------------------------

// The property that matters: the rebuild loop is NEVER blocked by a client.
//
// The client below connects, reads its baseline, and then stops reading
// entirely while 200 rebuilds are driven through the loop. Every rebuild must
// complete — the assertion is on the count AND on the wall time of the slowest
// one — and the client must then be told it missed deltas with a resync
// followed by a fresh baseline.
func TestASlowConsumerIsResyncedAndTheRunLoopNeverBlocks(t *testing.T) {
	const rebuilds = 200

	h := newHarness(t, func(o *options) {
		o.subscriberBuffer = 2
		o.writeBuffer = 1024
	})
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()

	// THE PRIMER: one delta that provably reached the client's socket before
	// the burst. The writer goroutine is not scheduled in step with this one,
	// and a full queue DISCARDS what it holds (overflowLocked). CI (ubuntu,
	// tmux 3.8 job, v0.1.2) once ran the whole 200-rebuild burst — a few ms —
	// before the writer pulled a single event after the handshake: it found
	// only the resync, which is correct product behaviour for a consumer that
	// drained nothing, and this test failed with "no deltas before it".
	// Reproduced 5/5 by stalling the writer 50 ms after its baseline write.
	// One publish cannot overflow a queue of 2, so the event the writer sends
	// next is this delta; Peek waits until its bytes are in the socket
	// without consuming a frame.
	h.core.set(t0.Add(time.Second/2), session("s1", func(s *observer.Session) {
		s.Status = observer.StatusBusy
		s.RawStatus = string(observer.StatusBusy)
	}))
	h.publish()
	if err := c.nc.SetReadDeadline(time.Now().Add(readBound)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if _, err := c.rd.Peek(1); err != nil {
		t.Fatalf("the primer delta never reached the client: %v", err)
	}
	// From here the client reads NOTHING.

	slowest := time.Duration(0)
	for i := 0; i < rebuilds; i++ {
		status := observer.StatusBusy
		if i%2 == 0 {
			status = observer.StatusIdle
		}
		h.core.set(t0.Add(time.Duration(i+1)*time.Second),
			session("s1", func(s *observer.Session) {
				s.Status = status
				s.RawStatus = string(status)
			}))

		start := time.Now()
		h.publish()
		if d := time.Since(start); d > slowest {
			slowest = d
		}
	}

	// +2: the publish before the dial and the primer.
	if got := h.loop.Rebuilds(); got != rebuilds+2 {
		t.Fatalf("the loop performed %d rebuilds, want %d — it was blocked or skipped work",
			got, rebuilds+2)
	}
	if slowest > time.Second {
		t.Fatalf("the slowest rebuild took %s; a rebuild must never wait on a client", slowest)
	}

	// Now read. The resync is enqueued by the first delivery AFTER the
	// overflow that finds room in the queue, so the drain is interleaved with
	// further rebuilds — which is also exactly how it happens in production: a
	// client that starts reading again learns it missed deltas on the next
	// event, not retroactively.
	var (
		resync wire.Frame
		seen   []wire.Frame
		found  bool
	)
	for i := 0; i < 400 && !found; i++ {
		f := c.frame()
		if f.Type == wire.FrameResync {
			resync, found = f, true
			break
		}
		seen = append(seen, f)
		h.publish()
	}
	if !found {
		t.Fatalf("no resync in %d frames; a dropped queue must be reported", len(seen))
	}
	if resync.Reason != wire.ReasonSlowConsumer {
		t.Fatalf("resync.reason = %q, want %q", resync.Reason, wire.ReasonSlowConsumer)
	}
	if len(seen) == 0 {
		t.Fatal("the resync arrived with no deltas before it; the queue never filled")
	}
	if seen[0].Type != wire.FrameSessionChanged {
		t.Fatalf("the first frame after the baseline is %q, want the primer %q",
			seen[0].Type, wire.FrameSessionChanged)
	}
	snap := c.expect(wire.FrameSnapshot)
	if snap.Snapshot == nil || len(snap.Snapshot.Sessions) != 1 {
		t.Fatalf("the post-resync baseline is %+v, want the current single session", snap.Snapshot)
	}

	// seq stays monotonic per connection across the gap, which is what a
	// client checks, and the baseline is the frame right after the resync.
	if resync.Seq <= seen[len(seen)-1].Seq {
		t.Fatalf("seq went backwards: %d after %d", resync.Seq, seen[len(seen)-1].Seq)
	}
	if snap.Seq != resync.Seq+1 {
		t.Fatalf("the baseline's seq is %d, want %d — the frame after a resync", snap.Seq, resync.Seq+1)
	}
}

// ---------------------------------------------------------------------------
// §6.6 — a degraded watch is told to the client, not waited for
// ---------------------------------------------------------------------------

func TestADegradedWatchReachesAnAttachedClient(t *testing.T) {
	h := newHarness(t, func(o *options) { o.watchErr = errNoKqueue{} })
	h.core.set(t0, session("s1", nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.loop.Run(ctx)
	}()
	waitFor(t, func() bool { return h.loop.Latest() != nil })

	c := h.dial()
	c.handshake()

	// Run() publishes once and then notifies every subscriber that the stream
	// will not push. A subscriber that attaches after that gets it on the next
	// degradation, so this test drives a second one through a fresh
	// subscription.
	c.send(wire.ClientFrame{Type: wire.FrameSubscribe, ReqID: ptr("r-1")})
	c.until(wire.FrameSnapshot)

	cancel()
	<-done
}

type errNoKqueue struct{}

func (errNoKqueue) Error() string { return "kqueue: too many open files" }

// ---------------------------------------------------------------------------
// AC9 — the run-forever assertion
// ---------------------------------------------------------------------------

// Thirty days of fake clock, zero events, zero arms, still running.
//
// This is the direct test of the user's override — "that is one single server
// written in go with minimal resources. lets keep it run forever" — and the one
// that would catch a future edit reintroducing a scheduled wake-up. Four things
// are asserted, because "still running" alone would pass on a server that woke
// up every second and did nothing:
//
//	the timer ledger reads ZERO arms
//	exactly ONE rebuild has happened (the startup publish)
//	the attached client has received NOTHING since its baseline
//	the goroutine count has not grown, and a new client is still served
func TestAC9TheServerRunsForeverWithZeroArmedTimersAndPublishesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		if err := h.loop.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitFor(t, func() bool { return h.loop.Latest() != nil })

	c := h.dial()
	c.handshake()

	// Let everything settle before counting goroutines, so the baseline is the
	// steady state rather than a connection still being set up.
	waitFor(t, func() bool { return h.srv.Clients() == 1 })
	runtime.GC()
	before := runtime.NumGoroutine()

	// Thirty days, in 720 hourly steps. Nothing is watching the clock, which
	// is the whole point: if anything were, one of the four assertions below
	// would fail.
	for i := 0; i < 720; i++ {
		h.clock.Advance(time.Hour)
	}

	if arms := h.timers.Arms(); arms != 0 {
		t.Fatalf("the timer ledger records %d armings over 30 idle days, want 0", arms)
	}
	if got := h.loop.Rebuilds(); got != 1 {
		t.Fatalf("%d rebuilds over 30 idle days, want exactly the startup one", got)
	}
	if err := c.nc.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if extra, err := c.rd.ReadBytes('\n'); err == nil {
		t.Fatalf("the server published a frame while idle: %s", extra)
	}

	// Growth is a count that STAYS above the baseline. runtime.NumGoroutine is
	// a sum of counters the runtime itself documents as possibly inconsistent
	// (gcount: a goroutine anywhere in the process mid-creation or mid-exit
	// counts for an instant), and one -race -count=200 package run of v0.1.2's
	// gate saw 9 → 10 once in ~2,600 iterations (0 in 1,000 alone), with no
	// stack to say which. So the count is re-read until it is back at the
	// baseline, within readBound; a leaked goroutine never comes back and still
	// fails, now with every stack printed.
	runtime.GC()
	after := runtime.NumGoroutine()
	for settle := time.Now(); after > before && time.Since(settle) < readBound; {
		time.Sleep(time.Millisecond)
		runtime.GC()
		after = runtime.NumGoroutine()
	}
	if after > before {
		buf := make([]byte, 1<<20)
		t.Logf("every goroutine:\n%s", buf[:runtime.Stack(buf, true)])
		t.Fatalf("the goroutine count grew from %d to %d over 30 idle days", before, after)
	}

	// Still serving: a new client gets a hello and the same baseline.
	fresh := h.dial()
	hello, snap := fresh.handshake()
	if hello.PID == 0 || snap.Snapshot == nil {
		t.Fatalf("the server stopped serving after 30 days: hello=%+v", hello)
	}

	cancel()
	<-loopDone
}

// ---------------------------------------------------------------------------
// AC11 (partial) — the CLI and the socket are the same bytes
// ---------------------------------------------------------------------------

// The socket's snapshot payload and `tt sessions --json`'s stdout are compared
// BYTE FOR BYTE, from ONE domain snapshot, because both are produced by
// wire.NewSnapshot and by nothing else.
//
// §9.2(a)'s procedure is followed exactly, and the emphasis is load-bearing:
// ONE *observer.Snapshot, never re-read, driven through both surfaces with one
// explicit SnapshotOptions and one TT. The first version of this test let the
// CLI ask the core for its own snapshot, and it passed for the wrong reason —
// the two answers shared a map, so the watch block the server merged in
// appeared on both. With one snapshot and no re-read, the only way the bytes
// can agree is that the same encoder produced them.
//
// It is also why the snapshot frame nests its envelope instead of flattening
// it: a flattened frame would have to redeclare every envelope key, and this
// assertion would then compare two declarations rather than one value.
func TestTheSnapshotFrameAndTheCLIsJSONAreTheSameBytes(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0,
		session("s1", nil),
		session("s2", func(s *observer.Session) {
			s.Kind = observer.KindBackground
			s.Status = observer.StatusBlocked
			s.RawStatus = "blocked"
			s.Attention = true
			s.PID = nil
			s.ShortID = ptr("s2short")
			s.Needs = ptr("open a terminal and test `ck`")
			s.Live = nil
			s.LiveReason = observer.LiveReasonJobBeyondRetention
		}),
	)
	// THE one snapshot. It is the published one, so it carries the watch
	// accounting the loop merges at publication — which both surfaces must
	// therefore emit identically.
	published := h.publish()

	// The socket's bytes.
	c := h.dial()
	_, frame := c.handshake()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.lastLine, &raw); err != nil {
		t.Fatalf("decoding the snapshot frame: %v", err)
	}
	overSocket, ok := raw["snapshot"]
	if !ok {
		t.Fatalf("the snapshot frame carries no snapshot payload: %s", c.lastLine)
	}
	if frame.Snapshot == nil {
		t.Fatal("the decoded frame carries no snapshot")
	}

	// The CLI's bytes, from THE SAME snapshot — a source that answers with it
	// and never re-reads, which is §9.2(a)'s "never re-read" made structural.
	var stdout, stderr bytes.Buffer
	code := jsonout.Run(context.Background(), fixedSource{snap: published}, jsonout.Options{
		JSON:     true,
		TT:       testTT,
		Snapshot: observer.DefaultSnapshotOptions(),
	}, &stdout, &stderr)
	if code != jsonout.ExitOK {
		t.Fatalf("the CLI exited %d: %s", code, stderr.String())
	}
	overCLI := bytes.TrimSpace(stdout.Bytes())

	if !bytes.Equal(overSocket, overCLI) {
		t.Fatalf("the two surfaces disagree.\nsocket: %s\n   cli: %s", overSocket, overCLI)
	}

	// And the identity is not achieved by both being empty: the fields AC11
	// names are all there, including the watch block and notCapable.
	for _, want := range []string{
		`"notCapable":["time-transitions"]`,
		`"watch":{"mode":`,
		`"sessionId":"s1"`,
		`"sessionId":"s2"`,
		`"counts":{"total":2`,
	} {
		if !bytes.Contains(overCLI, []byte(want)) {
			t.Fatalf("the agreed bytes do not contain %s: %s", want, overCLI)
		}
	}
}

// fixedSource answers with one snapshot, forever, and never reads anything.
// §9.2(a) requires the fixture snapshot to be built once and never re-read;
// this is that requirement as a type rather than as a convention.
type fixedSource struct{ snap *observer.Snapshot }

func (f fixedSource) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	return f.snap, nil
}

// ---------------------------------------------------------------------------
// the baseline, before anything is published
// ---------------------------------------------------------------------------

// A client that connects BEFORE the first publish still gets a snapshot frame
// first, never a burst of session_added.
//
// FOUND BY THE LIVE RUN. Every other test in this file publishes before it
// dials, and on a quiet machine that is what happens — but the real startup
// rebuild waits on the corroboration spawn's 5 s bound, and `claude agents
// --json` was measured at over a MINUTE while this was being written. A client
// that connects inside that window subscribes to a loop with nothing
// published, so the loop has no baseline to hand it and the first publish
// arrives as Diff(nil, snap): 37 session_added frames and no snapshot, for a
// client whose documented behaviour (§7.5b) is to wait for one and give up
// after M11's 1 s.
func TestAClientThatConnectsBeforeTheFirstPublishStillGetsABaselineFirst(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil), session("s2", nil))
	// NOTHING is published yet — the whole point.

	c := h.dial()
	hello := c.expect(wire.FrameHello)
	if hello.Sources != nil {
		t.Fatalf("hello carries sources before anything was read: %+v", hello.Sources)
	}

	h.publish()

	snap := c.expect(wire.FrameSnapshot)
	if snap.Snapshot == nil || len(snap.Snapshot.Sessions) != 2 {
		t.Fatalf("the first frame after the first publish is %+v, want a full baseline", snap.Snapshot)
	}

	// And from there it is an ordinary delta stream.
	h.core.set(t0.Add(time.Second),
		session("s1", func(s *observer.Session) { s.Status = observer.StatusBusy; s.RawStatus = "busy" }),
		session("s2", nil))
	h.publish()

	f := c.expect(wire.FrameSessionChanged)
	if f.SessionID != "s1" {
		t.Fatalf("session_changed for %q, want s1", f.SessionID)
	}
}

// The case the test above could not see: the FIRST publication carries no
// session at all. The fix above turned the first DELTA into a baseline, and an
// empty first publication has no delta — so the client got hello and then
// silence, forever, and never learned of the first job record either. FOUND BY
// THE INDEPENDENT BLACK-BOX SUITE (TestSocketDiscoversFirstJobRecord) against
// the real binary; the loop now makes an early subscriber's first event a
// snapshot, and this asserts it on the wire.
func TestAClientThatConnectsBeforeAnEmptyFirstPublishStillGetsABaseline(t *testing.T) {
	h := newHarness(t, nil)
	// No session anywhere, and nothing published yet.

	c := h.dial()
	if hello := c.expect(wire.FrameHello); hello.Sources != nil {
		t.Fatalf("hello carries sources before anything was read: %+v", hello.Sources)
	}

	h.publish()

	snap := c.expect(wire.FrameSnapshot)
	if snap.Snapshot == nil || len(snap.Snapshot.Sessions) != 0 {
		t.Fatalf("the first frame after an empty first publish is %+v, want an empty baseline", snap.Snapshot)
	}

	// And the first session to appear arrives as an ordinary addition.
	h.core.set(t0.Add(time.Second), session("s1", nil))
	h.publish()
	if f := c.expect(wire.FrameSessionAdded); f.Session == nil || f.Session.SessionID != "s1" {
		t.Fatalf("session_added carries %+v, want s1", f.Session)
	}
}

// The same hole through a re-subscribe: a client that changes its filter before
// anything is published has no baseline for the NEW view either, and a delta
// must not be the first thing it hears.
//
// The assertion is on ORDER alone. How many snapshots arrive, and in which
// order the subscribe and the publication are processed, is deliberately not
// asserted: the server may still be emitting frames from before it read the
// subscribe, and a test that pinned that would be pinning a race rather than
// the contract.
func TestAResubscribeBeforeTheFirstPublishAlsoGetsItsBaselineFirst(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	// Again: nothing published yet.

	c := h.dial()
	c.expect(wire.FrameHello)
	c.send(wire.ClientFrame{
		Type:   wire.FrameSubscribe,
		ReqID:  ptr("r-1"),
		Filter: &wire.ClientFilter{Kind: []string{string(observer.KindInteractive)}},
	})
	h.publish()

	seenBaseline, echoed := false, false
	for i := 0; i < 8 && !echoed; i++ {
		f := c.frame()
		switch f.Type {
		case wire.FrameSnapshot:
			seenBaseline = true
			if f.ReqID != nil && *f.ReqID == "r-1" {
				echoed = true
			}
		case wire.FrameSessionAdded, wire.FrameSessionChanged, wire.FrameSessionRemoved:
			if !seenBaseline {
				t.Fatalf("a %s arrived before any baseline", f.Type)
			}
		default:
			t.Fatalf("unexpected frame %q", f.Type)
		}
	}
	if !echoed {
		t.Fatal("the subscribe's reqId was never echoed on a baseline")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// waitFor polls a condition with a bound. It is a test-side wait, not a
// production one: the thing it waits for is another goroutine's first
// publication, which has no channel to block on from out here.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(readBound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the condition never became true")
}

// ---------------------------------------------------------------------------
// the watch accounting, over the wire
// ---------------------------------------------------------------------------

// The watch block reaches BOTH frames that carry source reports. This is the
// assertion for the defect P7's live run found: `sources.registry.watch` was
// null on every surface, and no fixture noticed because the block is merged at
// publication rather than built by the core.
func TestTheWatchAccountingReachesTheWireInHelloAndInTheSnapshot(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	hello, snap := h.dial().handshake()

	helloWatch := hello.Sources[observer.SourceRegistry].Watch
	if helloWatch == nil {
		t.Fatal("hello's registry source carries no watch block")
	}
	if helloWatch.Mode == "" {
		t.Fatalf("hello's watch block names no mode: %+v", helloWatch)
	}

	envWatch := snap.Snapshot.Sources[observer.SourceRegistry].Watch
	if envWatch == nil {
		t.Fatal("the snapshot envelope's registry source carries no watch block")
	}
	if !reflect.DeepEqual(helloWatch, envWatch) {
		t.Fatalf("hello and the envelope disagree about the watch:\n%+v\n%+v", helloWatch, envWatch)
	}
	if snap.Snapshot.Sources[observer.SourceJobs].Watch == nil {
		t.Fatal("the jobs source carries no watch block")
	}
}

// transcriptCollected is PASSED THROUGH, never rewritten by this surface.
//
// FOUND BY THE LIVE RUN. §7.5(f) says an unnegotiated connection gets
// "transcriptCollected:false on every session, always" — and on the real
// machine three background jobs arrive with it already TRUE, because P3's
// timing read (§3.4's Invariant B, a correctness input to statusSince) opened
// their transcripts before any enrichment was asked for. Forcing false here
// would make the field say "we did not look" about a file the server did look
// at, which is precisely the distinction the field exists to carry. What the
// negotiation governs is the FACTS: transcript stays null until enrichment is
// asked for.
func TestTheServerNeverRewritesTranscriptCollected(t *testing.T) {
	h := newHarness(t, func(o *options) { o.withEnricher = true })
	h.core.set(t0,
		session("s1", nil),
		// The background shape the live run produced: looked, found nothing.
		session("s2", func(s *observer.Session) {
			s.Kind = observer.KindBackground
			s.PID = nil
			s.ShortID = ptr("s2short")
			s.TranscriptCollected = true
			s.Transcript = nil
			s.LastActivityAt = ptr(t0.Add(-2 * time.Hour))
		}),
	)
	h.publish()

	_, snap := h.dial().handshake()
	byID := map[string]wire.Session{}
	for _, s := range snap.Snapshot.Sessions {
		byID[s.SessionID] = s
	}
	if byID["s1"].TranscriptCollected {
		t.Fatal("an interactive session was reported as collected on an unnegotiated connection")
	}
	if !byID["s2"].TranscriptCollected {
		t.Fatal("the server rewrote a background job's transcriptCollected to false; it looked, and the field says so")
	}
	if byID["s2"].Transcript != nil {
		t.Fatalf("unnegotiated facts leaked: %+v", byID["s2"].Transcript)
	}
	if byID["s2"].LastActivityAt == nil {
		t.Fatal("the timing read's result was dropped")
	}
}
