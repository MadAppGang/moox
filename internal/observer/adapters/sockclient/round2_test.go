package sockclient_test

// Code review round 2 — the client-side halves of A5, A7, A8, A10, A12, A14
// and A15. Each test names the finding it pins.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockclient"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/fakes"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// scriptServer answers EVERY connection with the same script, for tests that
// dial many times.
type scriptServer struct {
	dir string
	ln  net.Listener
	wg  sync.WaitGroup
}

func newScriptServer(t *testing.T, script func(conn int, r *bufio.Scanner, w net.Conn)) *scriptServer {
	t.Helper()
	s := &scriptServer{dir: tempStateDir(t)}
	ln, err := net.Listen("unix", filepath.Join(s.dir, sockserve.SocketName))
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for n := 0; ; n++ {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func(n int, nc net.Conn) {
				defer s.wg.Done()
				defer func() { _ = nc.Close() }()
				sc := bufio.NewScanner(nc)
				sc.Buffer(make([]byte, 0, 4096), 1<<20)
				script(n, sc, nc)
			}(n, nc)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

// send writes one wire frame with the given seq.
func send(w net.Conn, seq int64, f wire.Frame) {
	f.Seq = seq
	line, _ := json.Marshal(f)
	_, _ = w.Write(append(line, '\n'))
}

func helloFrame(caps ...string) wire.Frame {
	if len(caps) == 0 {
		caps = []string{wire.CapSnapshot, wire.CapDeltas}
	}
	return wire.NewHello(wire.Options{TT: testTT}, 4242, caps, []string{"time-transitions"}, nil)
}

func snapshotFrame(reqID *string, snap *observer.Snapshot) wire.Frame {
	return wire.NewSnapshotFrame(reqID, wire.NewSnapshot(snap, snap.ObservedAt, wire.Options{TT: testTT}))
}

// fold replays a subscriber's events into the session set it describes.
func fold(state map[string]observer.Session, events []observer.Event) map[string]observer.Session {
	for _, e := range events {
		switch e.Type {
		case observer.EventSnapshot, observer.EventResync:
			state = map[string]observer.Session{}
			if e.Snapshot != nil {
				for _, s := range e.Snapshot.Sessions {
					state[s.SessionID] = s
				}
			}
		case observer.EventSessionAdded, observer.EventSessionChanged:
			if e.Session != nil {
				state[e.SessionID] = *e.Session
			}
		case observer.EventSessionRemoved:
			delete(state, e.SessionID)
		}
	}
	return state
}

func drain(ch <-chan observer.Event) []observer.Event {
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

// A5, client side. The reader folds a delta in (under the mutex), then
// delivers it (outside); Subscribe registered and captured the local snapshot
// (under the mutex), then delivered the baseline (outside). A Subscribe that
// captured the OLD state could have the new delta delivered BEFORE its older
// baseline, and a consumer that applies both in arrival order ends holding the
// old state — with nothing further coming to correct it.
func TestASubscriberRacingADeltaNeverEndsBehindTheClientsState(t *testing.T) {
	waiting := session("s1", func(s *observer.Session) {
		s.Status, s.RawStatus, s.Attention = observer.StatusWaiting, "waiting", true
		s.ObservedAt = t0.Add(time.Second)
	})
	srv := newScriptServer(t, func(_ int, r *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame())
		send(w, 2, snapshotFrame(nil, snapshotOf(t0, session("s1", nil))))
		send(w, 3, wire.NewSessionChanged(wire.NewSession(waiting), []string{"status", "rawStatus", "attention"}))
		for r.Scan() { // hold the connection until the client goes
		}
	})

	iterations := 1000
	if testing.Short() {
		iterations = 100
	}
	rng := rand.New(rand.NewSource(7))
	behind := 0
	for i := 0; i < iterations; i++ {
		c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		spin(time.Duration(rng.Intn(300)) * time.Microsecond)
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := c.Subscribe(ctx, observer.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		// Wait until the client itself holds the delta, then give the
		// synchronous delivery that follows it a moment.
		waitFor(t, func() bool {
			s, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
			return err == nil && len(s.Sessions) == 1 && s.Sessions[0].Status == observer.StatusWaiting
		}, "the client never applied the delta")
		time.Sleep(5 * time.Millisecond)

		state := fold(map[string]observer.Session{}, drain(ch))
		if got := state["s1"].Status; got != observer.StatusWaiting {
			behind++
			if behind <= 3 {
				t.Errorf("iteration %d: the subscriber ends holding s1=%q while the client holds "+
					"waiting, and nothing further will arrive", i, got)
			}
		}
		cancel()
		_ = c.Close()
	}
	if behind > 0 {
		t.Fatalf("%d of %d subscribers ended behind the client's own state", behind, iterations)
	}
}

// openFDs counts this process's open descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot list /dev/fd: %v", err)
	}
	return len(entries)
}

// Code review round 2, A15: on EOF the reader marked the client closed but
// neither closed its socket nor cancelled anything, and a later Close saw
// `closed` and returned early — so the descriptor was never released. The
// subscription's cleanup goroutine waited only on the CALLER's context, so a
// pane whose observer restarted kept the old client, its socket and a
// goroutine alive for as long as the pane lived. Server-initiated disconnects,
// repeated, must leave descriptors and goroutines where they started.
func TestServerInitiatedDisconnectsLeaveNoDescriptorsOrGoroutinesBehind(t *testing.T) {
	srv := newScriptServer(t, func(_ int, _ *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame())
		send(w, 2, snapshotFrame(nil, snapshotOf(t0, session("s1", nil))))
		// ...and, a moment later, the server goes away: the observer
		// restarting under a pane.
		time.Sleep(10 * time.Millisecond)
	})

	// A pane's context lives as long as the pane.
	paneCtx, paneDone := context.WithCancel(context.Background())
	defer paneDone()

	settle := func() { time.Sleep(20 * time.Millisecond); runtime.GC() }
	settle()
	fdsBefore, goBefore := openFDs(t), runtime.NumGoroutine()

	const cycles = 40
	for i := 0; i < cycles; i++ {
		c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
		if err != nil {
			t.Fatalf("cycle %d: Dial: %v", i, err)
		}
		ch, err := c.Subscribe(paneCtx, observer.Filter{})
		if errors.Is(err, sockclient.ErrClosed) {
			continue // it ended before the pane subscribed; nothing to wait for
		}
		if err != nil {
			t.Fatalf("cycle %d: Subscribe: %v", i, err)
		}
		// The pane notices the stream ended...
		deadline := time.After(readBound)
	wait:
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					break wait
				}
			case <-deadline:
				t.Fatalf("cycle %d: the subscription never ended after the server closed", i)
			}
		}
		// ...and drops this client for a fresh one, WITHOUT calling Close —
		// which is what a reconnect loop holding the client in a goroutine does.
	}
	time.Sleep(50 * time.Millisecond)
	settle()
	fdsAfter, goAfter := openFDs(t), runtime.NumGoroutine()

	if grew := fdsAfter - fdsBefore; grew > cycles/4 {
		t.Errorf("%d server-initiated disconnects left %d more open descriptors", cycles, grew)
	}
	if grew := goAfter - goBefore; grew > cycles/4 {
		t.Errorf("%d server-initiated disconnects left %d more goroutines", cycles, grew)
	}
}

// And Close after the server already went away still releases everything and
// returns promptly.
func TestCloseAfterThePeerShutDownStillClosesTheSocket(t *testing.T) {
	srv := newScriptServer(t, func(_ int, _ *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame())
		send(w, 2, snapshotFrame(nil, snapshotOf(t0, session("s1", nil))))
	})
	settle := func() { time.Sleep(20 * time.Millisecond); runtime.GC() }
	settle()
	before := openFDs(t)
	// Held until after the count: a net.Conn nobody references is closed by
	// its finalizer at the next GC, which would hide a Close that does not.
	var keep []*sockclient.Client
	for i := 0; i < 20; i++ {
		c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		keep = append(keep, c)
		waitFor(t, func() bool {
			_, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
			return err != nil
		}, "the client never noticed the server went away")
		done := make(chan error, 1)
		go func() { done <- c.Close() }()
		select {
		case <-done:
		case <-time.After(readBound):
			t.Fatal("Close after a peer shutdown hung")
		}
	}
	settle()
	grew := openFDs(t) - before
	runtime.KeepAlive(keep)
	if grew > 5 {
		t.Fatalf("20 Close calls after peer shutdown left %d more open descriptors", grew)
	}
}

// A5, pinned structurally, because the client's window is sub-microsecond and a
// stress test can only catch it by luck (it did, once in 200, on the old code).
// Every delivery to a subscriber — the baseline in Subscribe, the fan-out in
// publish and emit — must happen while c.mu is held, which is what makes
// registration and publication one linear order.
func TestEveryDeliveryToASubscriberHappensUnderTheClientMutex(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sockclient.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, want := range []string{"Subscribe", "publish", "emit"} {
		var fn *ast.FuncDecl
		for _, d := range file.Decls {
			if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == want && f.Recv != nil {
				fn = f
			}
		}
		if fn == nil {
			t.Fatalf("(*Client).%s is gone from sockclient.go", want)
		}
		delivers := 0
		var bad []string
		var walk func(stmts []ast.Stmt, held bool) bool
		visit := func(n ast.Node, held *bool) {
			ast.Inspect(n, func(n ast.Node) bool {
				if _, ok := n.(*ast.DeferStmt); ok {
					return false // a deferred Unlock runs at return, not here
				}
				if _, ok := n.(*ast.FuncLit); ok {
					return false // a goroutine body is not this critical section
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Lock":
					*held = true
				case "Unlock":
					*held = false
				case "deliver":
					delivers++
					if !*held {
						bad = append(bad, fset.Position(call.Pos()).String())
					}
				}
				return true
			})
		}
		walk = func(stmts []ast.Stmt, held bool) bool {
			for _, st := range stmts {
				if ifs, ok := st.(*ast.IfStmt); ok && ifs.Else == nil && len(ifs.Body.List) > 0 {
					if _, ret := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt); ret {
						visit(ifs.Cond, &held)
						walk(ifs.Body.List, held)
						continue
					}
				}
				visit(st, &held)
			}
			return held
		}
		walk(fn.Body.List, false)
		if delivers == 0 {
			t.Errorf("(*Client).%s no longer delivers to a subscriber; this test is vacuous", want)
		}
		if len(bad) > 0 {
			t.Errorf("(*Client).%s delivers to a subscriber without holding c.mu at %v", want, bad)
		}
	}
}

// A12, client side: a refresh's correlated sources reply was handed to the
// waiter and never applied, so the caller — which answers from the local
// snapshot right after — kept reporting the failure its own refresh had just
// cleared.
func TestARefreshReplyUpdatesTheSourcesTheCallerAnswersFrom(t *testing.T) {
	failing := snapshotOf(t0, session("s1", nil))
	msg := "the spawn failed"
	failing.Sources[observer.SourceAgentsCLI] = observer.SourceReport{ID: observer.SourceAgentsCLI,
		Role: "corroboration", OK: false, LastError: &msg, Advisories: []observer.Advisory{},
		Counters: map[string]int64{}, Labels: map[string]string{}}
	recovered := snapshotOf(t0, session("s1", nil))
	recovered.Sources[observer.SourceAgentsCLI] = observer.SourceReport{ID: observer.SourceAgentsCLI,
		Role: "corroboration", OK: true, Advisories: []observer.Advisory{},
		Counters: map[string]int64{}, Labels: map[string]string{}}

	srv := newScriptServer(t, func(_ int, r *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame(wire.CapSnapshot, wire.CapDeltas, wire.CapRefresh))
		send(w, 2, snapshotFrame(nil, failing))
		for r.Scan() {
			req, err := wire.DecodeClientFrame(r.Bytes())
			if err != nil || req.Type != wire.FrameRefresh {
				continue
			}
			send(w, 3, wire.NewSourcesFrame(req.ReqID, wire.NewSources(recovered.Sources), wire.CauseRefresh))
		}
	})
	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	snap, outcome, err := c.Refresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(outcome.Refreshed) != 1 {
		t.Fatalf("outcome %+v, want the source refreshed", outcome)
	}
	if src := snap.Sources[observer.SourceAgentsCLI]; !src.OK || src.LastError != nil {
		t.Fatalf("after a successful refresh the answer still carries %+v", src)
	}
}

// A13, client side: the same overflow rule as the loop's. A burst the
// consumer could not keep up with, then silence: draining must be enough.
func TestAClientSubscriberThatOverflowsRecoversByDrainingAlone(t *testing.T) {
	release := make(chan struct{})
	srv := newScriptServer(t, func(_ int, r *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame())
		send(w, 2, snapshotFrame(nil, snapshotOf(t0, session("s1", nil))))
		<-release
		for i := 0; i < 10; i++ {
			st := observer.StatusBusy
			if i%2 == 1 {
				st = observer.StatusWaiting
			}
			s := session("s1", func(s *observer.Session) {
				s.Status, s.RawStatus = st, string(st)
				s.Name = "step-" + string(rune('0'+i))
				s.ObservedAt = t0.Add(time.Duration(i+1) * time.Second)
			})
			send(w, int64(3+i), wire.NewSessionChanged(wire.NewSession(s), []string{"status", "name"}))
		}
		for r.Scan() {
		}
	})
	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir,
		FirstSnapshot: readBound, SubscriberBuffer: 2})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool {
		s, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
		return err == nil && len(s.Sessions) == 1 && s.Sessions[0].Name == "step-9"
	}, "the client never applied the burst")
	time.Sleep(20 * time.Millisecond)

	state := fold(map[string]observer.Session{}, drain(ch))
	if got := state["s1"].Name; got != "step-9" {
		t.Fatalf("after draining, the subscriber holds %q; the client holds step-9 and nothing "+
			"further will arrive", got)
	}
}

// staleWorld is a published snapshot whose instant is `observed`: an
// interactive session that started waiting a second before it, and a WORKING
// background job whose thread last moved a minute before it and which a fresh
// listing corroborated at that instant.
func staleWorld(observed time.Time) *observer.Snapshot {
	since := observed.Add(-time.Second)
	waiting := session("waiter", func(s *observer.Session) {
		s.Status, s.RawStatus, s.Attention = observer.StatusWaiting, "waiting", true
		s.WaitReason = observer.WaitReasonInputNeeded
		s.StatusSince = &since
		age := int64(1000)
		s.StatusAgeMs, s.BlockedForMs = &age, &age
		s.ObservedAt = observed
	})
	activity := observed.Add(-time.Minute)
	recordAge := int64(60_000)
	tempo := "active"
	job := session("worker", func(s *observer.Session) {
		s.Kind, s.PID = observer.KindBackground, nil
		s.Status, s.RawStatus = observer.StatusWorking, "working"
		s.StatusSource = observer.SourceJobs
		s.StatusSince, s.StatusAgeMs = &activity, &recordAge
		s.RecordUpdatedAt, s.RecordAgeMs = &activity, &recordAge
		s.LastActivityAt = &activity
		s.Tempo = &tempo
		s.Live, s.LiveReason = boolPtr(true), observer.LiveReasonJobNonTerminalCorroborated
		s.Sources = []string{observer.SourceJobs, observer.SourceAgentsCLI, observer.SourceTranscript}
		s.ListedBy = []string{observer.SourceJobs, observer.SourceAgentsCLI}
		s.TranscriptCollected = true
		s.ObservedAt = observed
	})
	snap := snapshotOf(observed, waiting, job)
	staleAfter := int64(30_000)
	agentsAge := int64(0)
	snap.Sources[observer.SourceAgentsCLI] = observer.SourceReport{
		ID: observer.SourceAgentsCLI, Role: "corroboration", OK: true,
		ObservedAt: &observed, AgeMs: &agentsAge, StaleAfterMs: &staleAfter,
		Advisories: []observer.Advisory{}, Counters: map[string]int64{},
		Labels: map[string]string{"crossChecksSuppressed": "false"},
	}
	return snap
}

func byID(snap *observer.Snapshot, id string) (observer.Session, bool) {
	for _, s := range snap.Sessions {
		if s.SessionID == id {
			return s, true
		}
	}
	return observer.Session{}, false
}

// Code review round 2, A7. The socket client returned its retained snapshot and
// MCP encoded it at that snapshot's own instant, so on a quiet machine every
// answer reported the ages and the clock-dependent liveness of the moment the
// server last rebuilt: a session waiting for an hour said blockedForMs ≈ 0, and
// a job whose thread had stopped moving stayed confidently live. §7.5(c): ages
// are derived at ANSWER time and the §3.5 policy is re-run there.
//
// Here the server's published instant is twenty minutes ago and NOTHING has
// happened since — zero server events. Only the consumer's clock has moved (it
// is the real one).
func TestSocketAnswersDeriveAgesAndLivenessFromTheConsumersClock(t *testing.T) {
	observed := observer.TruncateTime(time.Now().Add(-20 * time.Minute))
	h := newServer(t, nil)
	h.loop.publish(staleWorld(observed))
	c := h.dial(t, nil)

	snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	waiter, ok := byID(snap, "waiter")
	if !ok {
		t.Fatalf("no waiter in %v", snap.Sessions)
	}
	if waiter.BlockedForMs == nil || *waiter.BlockedForMs < (20*time.Minute).Milliseconds() {
		t.Errorf("blockedForMs = %v after twenty quiet minutes, want >= %d — frozen at the "+
			"server's last rebuild", deref(waiter.BlockedForMs), (20 * time.Minute).Milliseconds())
	}
	if waiter.StatusAgeMs == nil || waiter.BlockedForMs == nil || *waiter.StatusAgeMs != *waiter.BlockedForMs {
		t.Errorf("blockedForMs is not a view of statusAgeMs: %v vs %v", deref(waiter.StatusAgeMs), deref(waiter.BlockedForMs))
	}
	if !snap.ObservedAt.After(observed) {
		t.Errorf("the answer is still stamped %v, the server's instant", snap.ObservedAt)
	}
	worker, ok := byID(snap, "worker")
	if !ok {
		t.Fatalf("the worker vanished: %v", snap.Sessions)
	}
	if worker.Live != nil || worker.LiveReason != observer.LiveReasonJobWorkingStale {
		t.Errorf("a working job whose thread has not moved for 21 minutes answers live=%v (%s); "+
			"§3.5 rule 5 at the consumer's clock says null (job-working-stale)",
			derefBool(worker.Live), worker.LiveReason)
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

// A7 with an injected consumer clock: one publication, then ONLY the consumer's
// clock moves — no server event, no rebuild, no timer — and successive answers
// age with it, crossing rule 6's and rule 5's thresholds on the way.
func TestAdvancingOnlyTheConsumersClockAgesEveryAnswer(t *testing.T) {
	h := newServer(t, nil)
	h.loop.publish(staleWorld(t0))
	clock := fakes.NewClock(t0)
	c := h.dial(t, func(cfg *sockclient.Config) { cfg.Clock = clock })

	answer := func() (observer.Session, observer.Session) {
		t.Helper()
		snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		w, _ := byID(snap, "waiter")
		j, _ := byID(snap, "worker")
		return w, j
	}

	w, j := answer()
	if *w.BlockedForMs != 1000 || j.Live == nil || !*j.Live {
		t.Fatalf("at the server's instant: blockedForMs=%d worker live=%v", *w.BlockedForMs, derefBool(j.Live))
	}

	clock.Advance(45 * time.Second)
	w, j = answer()
	if *w.BlockedForMs != 46_000 {
		t.Errorf("45 s later blockedForMs = %d, want 46000", *w.BlockedForMs)
	}
	if j.Live != nil || j.LiveReason != observer.LiveReasonJobNonTerminalUncorroborated {
		t.Errorf("45 s later the 30 s-old listing still vouches: live=%v %s", derefBool(j.Live), j.LiveReason)
	}

	clock.Advance(20 * time.Minute)
	w, j = answer()
	if *w.BlockedForMs != 1000+45_000+20*60_000 {
		t.Errorf("blockedForMs = %d after 20 more minutes", *w.BlockedForMs)
	}
	if j.LiveReason != observer.LiveReasonJobWorkingStale {
		t.Errorf("a thread quiet for 21 minutes: %s, want job-working-stale", j.LiveReason)
	}
	if h.srv.Clients() != 1 {
		t.Fatalf("the test was supposed to involve one quiet connection")
	}
}

// degradedServer sends a baseline of `first`, then (if announce) a
// watch_degraded frame, and answers every snapshot REQUEST with `second` — the
// fresh read a degraded server now performs.
func degradedServer(t *testing.T, first, second *observer.Snapshot, announce bool) *scriptServer {
	return newScriptServer(t, func(_ int, r *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame())
		send(w, 2, snapshotFrame(nil, first))
		seq := int64(3)
		if announce {
			send(w, seq, wire.NewWatchDegraded("", wire.WatchDegradedOnDemand, "kqueue died"))
			seq++
		}
		for r.Scan() {
			req, err := wire.DecodeClientFrame(r.Bytes())
			if err != nil || req.Type != wire.FrameSnapshot {
				continue
			}
			send(w, seq, snapshotFrame(req.ReqID, second))
			seq++
		}
	})
}

// A8, client side: after the server's watch degrades, the stream it holds will
// not push, so an attached client must stop answering from it. It used to
// emit watch_degraded to its subscribers and go on serving the retained
// snapshot to every MCP call.
func TestAfterWatchDegradedTheClientAnswersFromAFreshPull(t *testing.T) {
	before := snapshotOf(t0, session("s1", nil))
	after := snapshotOf(t0.Add(time.Minute), session("s1", func(s *observer.Session) {
		s.Status, s.RawStatus, s.Attention = observer.StatusWaiting, "waiting", true
		s.ObservedAt = t0.Add(time.Minute)
	}))
	srv := degradedServer(t, before, after, true)
	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	waitFor(t, func() bool {
		snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
		return err == nil && len(snap.Sessions) == 1 && snap.Sessions[0].Status == observer.StatusWaiting
	}, "after watch_degraded the client kept answering from the stale stream")
}

// ...and a client that attaches to a server ALREADY degraded learns it from
// the baseline's own watch report, not only from a frame it arrived too late
// for.
func TestAClientAttachingToADegradedServerPullsFromTheStart(t *testing.T) {
	onDemand := func(snap *observer.Snapshot) *observer.Snapshot {
		src := snap.Sources[observer.SourceRegistry]
		src.Watch = &observer.WatchReport{Mode: observer.WatchModeOnDemand}
		snap.Sources[observer.SourceRegistry] = src
		return snap
	}
	before := onDemand(snapshotOf(t0, session("s1", nil)))
	after := onDemand(snapshotOf(t0.Add(time.Minute), session("s1", func(s *observer.Session) {
		s.Status, s.RawStatus, s.Attention = observer.StatusWaiting, "waiting", true
		s.ObservedAt = t0.Add(time.Minute)
	})))
	srv := degradedServer(t, before, after, false)
	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Sessions[0].Status != observer.StatusWaiting {
		t.Fatalf("attached to a server whose watch is on-demand, the client answered %q from "+
			"its baseline instead of pulling", snap.Sessions[0].Status)
	}
}

// A10: `View` applied the default liveness rule to every socket baseline, and
// the client narrowed again even after accepting IncludeDead — the protocol
// had no way to ask for the dead population. `tt mcp --local` could return an
// ended job for includeDead:true; the default socket-backed `tt mcp` could not.
func TestIncludeDeadWorksThroughTheSocketAsItDoesLocally(t *testing.T) {
	h := newServer(t, nil)
	dead := session("ended", func(s *observer.Session) {
		s.Kind, s.PID = observer.KindBackground, nil
		s.Status, s.RawStatus = observer.StatusDone, "done"
		s.Live, s.LiveReason = boolPtr(false), observer.LiveReasonJobTerminal
	})
	h.loop.publish(snapshotOf(t0, session("s1", nil), dead))
	c := h.dial(t, nil)

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = true
	snap, err := c.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, ok := byID(snap, "ended"); !ok {
		t.Fatalf("includeDead over the socket returned %d sessions without the ended job", len(snap.Sessions))
	}
	// And the default still hides it, exactly as locally.
	def, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, ok := byID(def, "ended"); ok {
		t.Fatal("the default view now includes a dead session")
	}
}

// A session another session launched (spawnedBy set) must be reachable over
// the socket exactly as it is locally: hidden by default, listed when the
// caller's Filter asks for IncludeSpawned (`tt sessions --all`). The client's
// local state must therefore hold it — which it does only if the standing
// subscription asks the server for it, and the client's own recounting views
// do not narrow it away again.
func TestIncludeSpawnedWorksThroughTheSocketAsItDoesLocally(t *testing.T) {
	h := newServer(t, nil)
	child := session("child", func(s *observer.Session) {
		launcher := "s1"
		s.SpawnedBy = &launcher
	})
	h.loop.publish(snapshotOf(t0, session("s1", nil), child))
	c := h.dial(t, nil)

	opts := observer.DefaultSnapshotOptions()
	opts.Filter.IncludeSpawned = true
	snap, err := c.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got, ok := byID(snap, "child")
	if !ok {
		t.Fatalf("includeSpawned over the socket returned %d sessions without the launched one", len(snap.Sessions))
	}
	if got.SpawnedBy == nil || *got.SpawnedBy != "s1" {
		t.Fatalf("spawnedBy did not survive the socket: %v", got.SpawnedBy)
	}

	def, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, ok := byID(def, "child"); ok {
		t.Fatal("the default view over the socket includes a launched session")
	}

	// A subscriber with the panel's filter (the dead, for ENDED rows) is still
	// not shown the child: IncludeDead must not widen into IncludeSpawned.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Subscribe(ctx, observer.Filter{IncludeDead: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ev := <-ch
	if ev.Snapshot == nil {
		t.Fatalf("first event %q carries no baseline", ev.Type)
	}
	if _, ok := byID(ev.Snapshot, "child"); ok {
		t.Fatal("an includeDead subscriber was handed a launched session")
	}
}

// A14, client side: a session_removed frame carries the KIND of the record it
// removes, and a client holding two records with one id removes that one. A
// removal from a server one build behind (no kind) still removes by id.
func TestAKindQualifiedRemovalRemovesThatRecordOnly(t *testing.T) {
	inter := session("X", nil)
	bg := session("X", func(s *observer.Session) {
		s.Kind, s.PID = observer.KindBackground, nil
		s.Status, s.RawStatus, s.Attention = observer.StatusBlocked, "blocked", true
	})
	srv := newScriptServer(t, func(_ int, r *bufio.Scanner, w net.Conn) {
		send(w, 1, helloFrame())
		send(w, 2, snapshotFrame(nil, snapshotOf(t0, bg, inter)))
		line, _ := json.Marshal(map[string]any{"type": "session_removed", "seq": 3, "reqId": nil,
			"sessionId": "X", "kind": "interactive", "reason": observer.RemovedRecordRemoved})
		_, _ = w.Write(append(line, '\n'))
		for r.Scan() {
		}
	})
	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: srv.dir, FirstSnapshot: readBound})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	waitFor(t, func() bool {
		s, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
		return err == nil && len(s.Sessions) == 1
	}, "the removal was never applied")
	s, _ := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if s.Sessions[0].Kind != observer.KindBackground {
		t.Fatalf("removing the INTERACTIVE X left the %s one", s.Sessions[0].Kind)
	}
}
