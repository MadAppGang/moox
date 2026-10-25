package sockclient_test

// §7.5's contract, tested from both ends: against a real sockserve for the
// things that must hold end to end, and against a scripted raw server for the
// refusals — a version mismatch and a server that never sends a snapshot are
// states a healthy sockserve cannot be put into, and a test that could not
// produce them would be asserting nothing.
//
// Hermetic in the two ways this repository has learned to be: every socket is
// in a fresh temp directory (never ~/.local/state/tt), nothing is spawned, and
// every client and server is closed by a cleanup.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockclient"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

var t0 = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

const testTT = "v0.4.0-test"

// readBound is how long a test waits for something that should already be on
// its way. A test-side deadline, not a production timer.
const readBound = 3 * time.Second

// tempStateDir is t.TempDir() with the one constraint a unix socket adds: the
// whole path must fit in sockaddr_un.sun_path, 104 bytes on darwin, and this
// package's test names are long enough to blow that.
func tempStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttcli")
	if err != nil {
		t.Fatalf("making a temp state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func session(id string, tune func(*observer.Session)) observer.Session {
	pid := 4242
	since := t0.Add(-3 * time.Hour)
	age := int64(3 * 60 * 60 * 1000)
	s := observer.Session{
		SessionID:    id,
		PID:          &pid,
		Kind:         observer.KindInteractive,
		Name:         "session " + id,
		NameSource:   observer.SourceRegistry,
		FormerNames:  []string{},
		CWD:          "/home/user/dotfiles",
		Status:       observer.StatusIdle,
		RawStatus:    "idle",
		StatusSource: observer.SourceRegistry,
		StatusSince:  &since,
		StatusAgeMs:  &age,
		WaitReason:   observer.WaitReasonNone,
		Children:     []string{},
		Live:         boolPtr(true),
		LiveReason:   observer.LiveReasonProcPane,
		ListedBy:     []string{observer.SourceRegistry},
		Sources:      []string{observer.SourceRegistry},
		ObservedAt:   t0,
		Advisories:   []observer.Advisory{},
	}
	if tune != nil {
		tune(&s)
	}
	return s
}

func boolPtr(b bool) *bool { return &b }

func snapshotOf(at time.Time, sessions ...observer.Session) *observer.Snapshot {
	counts := observer.NewCounts()
	for _, s := range sessions {
		counts.Total++
		switch {
		case s.Live == nil:
			counts.Unknown++
		case *s.Live:
			counts.Live++
		default:
			counts.Dead++
		}
		if s.Attention {
			counts.Attention++
		}
		counts.ByStatus[s.Status]++
		counts.ByKind[s.Kind]++
	}
	return &observer.Snapshot{
		ObservedAt: at,
		Counts:     counts,
		Sources: map[string]observer.SourceReport{
			observer.SourceRegistry: {
				ID: observer.SourceRegistry, Role: "authoritative", Path: "/fixture/sessions",
				ObservedAt: &at, OK: true,
				Counters: map[string]int64{"records": int64(len(sessions))},
				Labels:   map[string]string{}, Advisories: []observer.Advisory{},
			},
		},
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}
}

// ---------------------------------------------------------------------------
// a real server, with the smallest possible event source behind it
// ---------------------------------------------------------------------------

// fakeLoop is sockserve's Subscriber and Rebuilder: it holds the published
// snapshot and computes the same observer.Diff the real watch loop computes, so
// the frames a client sees are produced by production code. What it does not do
// is watch anything.
type fakeLoop struct {
	mu     sync.Mutex
	snap   *observer.Snapshot
	subs   map[int]*loopSub
	nextID int
}

type loopSub struct {
	ch     chan observer.Event
	filter observer.Filter
}

func newFakeLoop() *fakeLoop { return &fakeLoop{subs: map[int]*loopSub{}} }

func (l *fakeLoop) Latest() *observer.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

func (l *fakeLoop) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	return l.Latest(), nil
}

func (l *fakeLoop) Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error) {
	l.mu.Lock()
	l.nextID++
	id := l.nextID
	s := &loopSub{ch: make(chan observer.Event, 64), filter: f}
	l.subs[id] = s
	snap := l.snap
	l.mu.Unlock()

	if snap != nil {
		s.ch <- observer.Event{Type: observer.EventSnapshot, Snapshot: snap, At: snap.ObservedAt}
	}
	go func() {
		<-ctx.Done()
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
		close(s.ch)
	}()
	return s.ch, nil
}

func (l *fakeLoop) Rebuild(context.Context, string) (*observer.Snapshot, observer.ArmReport, error) {
	return l.Latest(), observer.ArmReport{ByKind: map[observer.TargetKind]int{}}, nil
}

// publish replaces the published snapshot and delivers each subscriber its own
// deltas, which is exactly what the real loop does at the end of a rebuild.
func (l *fakeLoop) publish(snap *observer.Snapshot) {
	l.mu.Lock()
	prev := l.snap
	l.snap = snap
	subs := make([]*loopSub, 0, len(l.subs))
	for _, s := range l.subs {
		subs = append(subs, s)
	}
	l.mu.Unlock()

	for _, s := range subs {
		for _, ev := range observer.Diff(prev, snap, s.filter) {
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// stubRefresher is the corroboration path. The core owns the rate limit, so a
// test that wants a rate-limited answer says so here.
type stubRefresher struct {
	mu      sync.Mutex
	outcome observer.RefreshOutcome
	calls   int
}

func (r *stubRefresher) RefreshSources(context.Context, []string) observer.RefreshOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.outcome
}

func (r *stubRefresher) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type serverHarness struct {
	dir     string
	loop    *fakeLoop
	refresh *stubRefresher
	srv     *sockserve.Server
}

func newServer(t *testing.T, tune func(*sockserve.Deps, *stubRefresher)) *serverHarness {
	t.Helper()
	h := &serverHarness{
		dir:     tempStateDir(t),
		loop:    newFakeLoop(),
		refresh: &stubRefresher{},
	}
	h.loop.publish(snapshotOf(t0, session("s1", nil), session("s2", func(s *observer.Session) {
		s.Kind = observer.KindBackground
		s.Status = observer.StatusBlocked
		s.Attention = true
		s.Live = nil
		s.LiveReason = observer.LiveReasonJobBeyondRetention
		s.PID = nil
	})))

	deps := sockserve.Deps{Events: h.loop, Core: h.loop, Rebuild: h.loop}
	if tune != nil {
		tune(&deps, h.refresh)
	}

	srv, err := sockserve.New(sockserve.Config{
		Dir:        h.dir,
		ClaudeRoot: h.dir + "-claude-root",
		TT:         testTT,
		PID:        os.Getpid(),
		Deps:       deps,
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	h.srv = srv
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	return h
}

func (h *serverHarness) dial(t *testing.T, tune func(*sockclient.Config)) *sockclient.Client {
	t.Helper()
	cfg := sockclient.Config{Dir: h.dir, FirstSnapshot: readBound}
	if tune != nil {
		tune(&cfg)
	}
	c, err := sockclient.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------------------------------------------------------------------------
// a scripted raw server, for the states a healthy sockserve cannot produce
// ---------------------------------------------------------------------------

// rawServer answers one connection with whatever the script writes. It is the
// only way to test a version mismatch and a silent server, because sockserve
// by construction sends this build's schema and a baseline.
type rawServer struct {
	dir string
	ln  net.Listener
}

func newRawServer(t *testing.T, script func(t *testing.T, r *bufio.Scanner, w net.Conn)) *rawServer {
	t.Helper()
	dir := tempStateDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, sockserve.SocketName))
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	s := &rawServer{dir: dir, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = nc.Close() }()
		script(t, bufio.NewScanner(nc), nc)
	}()
	t.Cleanup(func() { <-done })
	return s
}

// writeFrame is how a scripted server emits one line. It builds the object as
// a map rather than through wire's constructors on purpose: these tests are
// about what the CLIENT does with bytes that are not this build's.
func writeFrame(t *testing.T, w net.Conn, obj map[string]any) {
	t.Helper()
	line, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("encoding a scripted frame: %v", err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Logf("writing a scripted frame: %v", err)
	}
}

// helloOf is a well-formed hello with the schema and protocol a test chooses.
func helloOf(schema, protocol int, capabilities []string) map[string]any {
	return map[string]any{
		"type": "hello", "seq": 1, "schema": schema, "protocol": protocol,
		"tt": testTT, "pid": 4242, "capabilities": capabilities,
		"notCapable": []string{"time-transitions"}, "sources": nil,
	}
}

// ---------------------------------------------------------------------------
// dialling, and the three refusals
// ---------------------------------------------------------------------------

// Nothing listening is a FACT about the machine, not a failure: nothing in tt
// ever auto-starts `tt observe`, so every surface falls back to its own reads.
func TestDiallingWithNoServerIsErrNoServer(t *testing.T) {
	_, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: tempStateDir(t)})
	if !errors.Is(err, sockclient.ErrNoServer) {
		t.Fatalf("got %v, want ErrNoServer", err)
	}
}

// §7.5(e): a server whose schema is not this build's is NOT attached to. A
// long-lived `tt observe` from an older build is the EXPECTED case, and
// silently re-serving an older schema would turn §7.5 into a way to violate
// AC11 across processes.
func TestASchemaMismatchedServerIsRefusedAndLoggedOnce(t *testing.T) {
	s := newRawServer(t, func(t *testing.T, _ *bufio.Scanner, w net.Conn) {
		writeFrame(t, w, helloOf(wire.Schema+1, wire.Protocol, []string{"snapshot"}))
		// A baseline follows, and it must never be used: the client has
		// already refused.
		writeFrame(t, w, map[string]any{"type": "snapshot", "seq": 2, "snapshot": map[string]any{}})
	})

	var log strings.Builder
	_, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: s.dir, Log: &log})

	var mismatch *sockclient.SchemaMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %v, want a SchemaMismatch", err)
	}
	if mismatch.ServerSchema != wire.Schema+1 || mismatch.WantSchema != wire.Schema {
		t.Fatalf("the mismatch does not name both versions: %+v", mismatch)
	}
	if got := mismatch.Error(); got != "server schema 2, expected 1" {
		t.Fatalf("the message is %q; §7.5(e) specifies \"server schema N, expected M\"", got)
	}
	if !strings.Contains(log.String(), "not attaching") {
		t.Fatalf("the refusal was not logged: %q", log.String())
	}
}

// The protocol version is the OTHER version space, and it is checked
// independently: the framing can change without the payload shape changing.
func TestAProtocolMismatchedServerIsAlsoRefused(t *testing.T) {
	s := newRawServer(t, func(t *testing.T, _ *bufio.Scanner, w net.Conn) {
		writeFrame(t, w, helloOf(wire.Schema, wire.Protocol+7, nil))
	})
	_, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: s.dir})
	var mismatch *sockclient.SchemaMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %v, want a SchemaMismatch", err)
	}
	if !strings.Contains(mismatch.Error(), "protocol") {
		t.Fatalf("the message does not name the protocol: %q", mismatch.Error())
	}
}

// M11: the hello → snapshot wait is BOUNDED. Revision 1 left it unbounded, so a
// wedged runLoop would hang an MCP list_sessions call or a TUI launch instead
// of degrading.
func TestTheWaitForTheFirstSnapshotIsBounded(t *testing.T) {
	if sockclient.DefaultFirstSnapshot != time.Second {
		t.Fatalf("the default bound is %s; M11 specifies 1s", sockclient.DefaultFirstSnapshot)
	}

	block := make(chan struct{})
	s := newRawServer(t, func(t *testing.T, _ *bufio.Scanner, w net.Conn) {
		writeFrame(t, w, helloOf(wire.Schema, wire.Protocol, []string{"snapshot"}))
		// Then nothing at all — a server whose first rebuild is wedged.
		<-block
	})
	t.Cleanup(func() { close(block) })

	start := time.Now()
	_, err := sockclient.Dial(context.Background(), sockclient.Config{
		Dir:           s.dir,
		FirstSnapshot: 150 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if !errors.Is(err, sockclient.ErrNoSnapshot) {
		t.Fatalf("got %v, want ErrNoSnapshot", err)
	}
	if elapsed > time.Second {
		t.Fatalf("the bound did not fire: waited %s for a 150ms bound", elapsed)
	}
}

// ---------------------------------------------------------------------------
// the local snapshot
// ---------------------------------------------------------------------------

// §7.5(b): the local state is built by wire from the server's baseline. What is
// asserted is that it round-tripped — the same sessions, the same instant, the
// same tri-states — because a client that decoded it itself would drift.
func TestTheLocalSnapshotIsBuiltFromTheServersBaseline(t *testing.T) {
	h := newServer(t, nil)
	// The consumer's clock is pinned at the server's instant: answers are
	// derived at answer time (code review round 2, A7), and at an equal
	// instant that derivation is the identity — so what is asserted here is
	// the baseline itself, travelling intact.
	c := h.dial(t, func(cfg *sockclient.Config) { cfg.Clock = stoppedClock{t0} })

	snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(snap.Sessions))
	}
	if !snap.ObservedAt.Equal(t0) {
		t.Fatalf("observedAt is %s, want the server's rebuild instant %s", snap.ObservedAt, t0)
	}
	if snap.Counts.Total != 2 || snap.Counts.Unknown != 1 || snap.Counts.Attention != 1 {
		t.Fatalf("the counts did not survive: %+v", snap.Counts)
	}
	// The tri-state that matters: live:null must not have become false.
	for _, s := range snap.Sessions {
		if s.SessionID == "s2" && s.Live != nil {
			t.Fatalf("live:null became %v", *s.Live)
		}
	}
	if c.ServerPID() == 0 || c.ServerTT() != testTT {
		t.Fatalf("hello did not travel: pid %d tt %q", c.ServerPID(), c.ServerTT())
	}
	if len(c.NotCapable()) != 1 {
		t.Fatalf("notCapable did not travel: %+v", c.NotCapable())
	}
}

// The options narrow the ANSWER, through the same two predicates the core uses,
// and the counts are recomputed over what is left rather than carried over.
func TestSnapshotNarrowsByTheCallersOptions(t *testing.T) {
	h := newServer(t, nil)
	c := h.dial(t, nil)

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeUnknownLiveness = false
	snap, err := c.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].SessionID != "s1" {
		t.Fatalf("the liveness rule was not applied: %+v", snap.Sessions)
	}
	if snap.Counts.Total != 1 || snap.Counts.Unknown != 0 {
		t.Fatalf("the counts were carried over rather than recomputed: %+v", snap.Counts)
	}

	opts = observer.DefaultSnapshotOptions()
	opts.Filter.Kinds = []observer.Kind{observer.KindBackground}
	snap, err = c.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].SessionID != "s2" {
		t.Fatalf("the filter was not applied: %+v", snap.Sessions)
	}
}

// A delta folds into the local state through wire.ApplySessionFrame, and the
// subscriber sees it as an event. Both halves matter: a client whose local
// state was right but whose subscribers heard nothing would silently stop a
// panel updating.
func TestADeltaFoldsIntoTheLocalSnapshotAndReachesASubscriber(t *testing.T) {
	h := newServer(t, nil)
	c := h.dial(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// The baseline arrives first, always.
	first := receive(t, events)
	if first.Type != observer.EventSnapshot {
		t.Fatalf("the first event is %s, want a snapshot baseline", first.Type)
	}

	t1 := t0.Add(2 * time.Minute)
	h.loop.publish(snapshotOf(t1, session("s1", func(s *observer.Session) {
		s.Status = observer.StatusBusy
		s.RawStatus = "busy"
		s.ObservedAt = t1
	})))

	// s2 left and s1 changed, in whichever order they arrive.
	sawChange := false
	for i := 0; i < 2; i++ {
		ev := receive(t, events)
		if ev.Type == observer.EventSessionChanged && ev.Session != nil &&
			ev.Session.Status == observer.StatusBusy {
			sawChange = true
		}
	}
	if !sawChange {
		t.Fatal("the subscriber never saw the status change")
	}

	waitFor(t, func() bool {
		snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
		if err != nil {
			return false
		}
		for _, s := range snap.Sessions {
			if s.SessionID == "s1" && s.Status == observer.StatusBusy {
				return true
			}
		}
		return false
	}, "the local snapshot never took the delta")
}

// A filtered subscriber sees its OWN membership, through the same predicate the
// server uses for its clients — which is why the client subscribes unfiltered
// and filters here (§7.5b).
func TestASubscriberSeesItsOwnFilteredView(t *testing.T) {
	h := newServer(t, nil)
	c := h.dial(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx, observer.Filter{Kinds: []observer.Kind{observer.KindBackground}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	base := receive(t, events)
	if base.Snapshot == nil || len(base.Snapshot.Sessions) != 1 ||
		base.Snapshot.Sessions[0].SessionID != "s2" {
		t.Fatalf("the baseline is not this subscriber's view: %+v", base.Snapshot)
	}
	if base.Snapshot.Counts.Total != 1 {
		t.Fatalf("the filtered baseline's counts describe the whole machine: %+v", base.Snapshot.Counts)
	}
}

// ---------------------------------------------------------------------------
// refresh — §7.5(d)
// ---------------------------------------------------------------------------

// The round trip's happy path: the server refreshed, and the client says which
// source.
func TestARefreshThatSucceedsReportsTheSourceRefreshed(t *testing.T) {
	h := newServer(t, func(deps *sockserve.Deps, r *stubRefresher) {
		r.outcome = observer.RefreshOutcome{Rejected: map[string]string{}}
		deps.Refresh = r
	})
	c := h.dial(t, nil)

	out := c.RefreshSources(context.Background(), nil)
	if len(out.Rejected) != 0 {
		t.Fatalf("a successful refresh reported rejections: %+v", out.Rejected)
	}
	if len(out.Refreshed) != 1 || out.Refreshed[0] != observer.SourceAgentsCLI {
		t.Fatalf("the refreshed source was not named: %+v", out.Refreshed)
	}
	if h.refresh.Calls() != 1 {
		t.Fatalf("the server refreshed %d times", h.refresh.Calls())
	}
}

// rate_limited is an ANSWER: the outcome says so, the advisory is attached, and
// the snapshot is still served. §7.5(d) and §5.4 both turn on this — a refresh
// must never fail a caller's request.
func TestARateLimitedRefreshIsADegradedAnswerAndNeverAnError(t *testing.T) {
	h := newServer(t, func(deps *sockserve.Deps, r *stubRefresher) {
		r.outcome = observer.RefreshOutcome{
			Rejected: map[string]string{observer.SourceAgentsCLI: observer.RejectRateLimited},
		}
		deps.Refresh = r
	})
	c := h.dial(t, nil)

	snap, out, err := c.Refresh(context.Background(), nil)
	if err != nil {
		t.Fatalf("a rate-limited refresh returned an error: %v", err)
	}
	if snap == nil || len(snap.Sessions) != 2 {
		t.Fatalf("the degraded answer lost the snapshot: %+v", snap)
	}
	if out.Rejected[observer.SourceAgentsCLI] != observer.RejectRateLimited {
		t.Fatalf("the rejection was not reported: %+v", out.Rejected)
	}
	found := false
	for _, a := range out.Advisories {
		if a.Code == observer.AdvRefreshRejected && a.Fields["reason"] == observer.RejectRateLimited {
			found = true
		}
	}
	if !found {
		t.Fatalf("no request-scoped refresh-rejected advisory: %+v", out.Advisories)
	}
}

// A server with no corroboration source at all answers error{unsupported},
// which the client reports as the matching rejection rather than as a timeout —
// a diagnosis is wrong in exactly the place a consumer looks otherwise.
func TestARefreshAgainstAServerThatCannotIsUnsupported(t *testing.T) {
	h := newServer(t, nil) // no Refresh dependency
	c := h.dial(t, nil)

	out := c.RefreshSources(context.Background(), nil)
	if out.Rejected[observer.SourceAgentsCLI] != observer.RejectUnsupported {
		t.Fatalf("got %+v, want unsupported", out.Rejected)
	}
}

// A server that never answers the refresh degrades on the client's own bound,
// rather than hanging the caller.
func TestARefreshThatIsNeverAnsweredTimesOut(t *testing.T) {
	block := make(chan struct{})
	s := newRawServer(t, func(t *testing.T, r *bufio.Scanner, w net.Conn) {
		writeFrame(t, w, helloOf(wire.Schema, wire.Protocol, []string{"snapshot", "refresh"}))
		writeFrame(t, w, map[string]any{
			"type": "snapshot", "seq": 2,
			"snapshot": envelopeOf(t),
		})
		// Read the refresh and never answer it.
		r.Scan()
		<-block
	})
	t.Cleanup(func() { close(block) })

	c, err := sockclient.Dial(context.Background(), sockclient.Config{
		Dir:            s.dir,
		FirstSnapshot:  readBound,
		RefreshTimeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	start := time.Now()
	out := c.RefreshSources(context.Background(), nil)
	if out.Rejected[observer.SourceAgentsCLI] != observer.RejectTimeout {
		t.Fatalf("got %+v, want a timeout rejection", out.Rejected)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the refresh bound did not fire: %s", time.Since(start))
	}
}

// ---------------------------------------------------------------------------
// enrichment — §7.5(f)
// ---------------------------------------------------------------------------

// A server that does not declare "enrich" is treated as enrich:false and the
// client returns what it has, rather than waiting for facts that will not come.
func TestEnrichmentAgainstAServerWithoutTheCapabilityChangesNothing(t *testing.T) {
	h := newServer(t, nil) // sockserve declares "enrich" only with an Enricher
	c := h.dial(t, nil)
	if c.Capable(wire.CapEnrich) {
		t.Fatal("the server declared enrich without an enricher")
	}

	before, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	after, err := c.Enrich(context.Background(), before, nil)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if after != before {
		t.Fatal("Enrich against a server that cannot enrich returned a different snapshot")
	}
}

// A server that DOES declare it is asked with a one-off `snapshot` frame
// carrying enrich:true — and the standing subscription is not touched, which is
// what keeps a one-off enriched read from making the whole stream expensive.
func TestEnrichmentIsNegotiatedWithAOneOffRequest(t *testing.T) {
	asked := make(chan map[string]any, 4)
	s := newRawServer(t, func(t *testing.T, r *bufio.Scanner, w net.Conn) {
		writeFrame(t, w, helloOf(wire.Schema, wire.Protocol, []string{"snapshot", "enrich"}))
		writeFrame(t, w, map[string]any{"type": "snapshot", "seq": 2, "snapshot": envelopeOf(t)})
		for r.Scan() {
			var req map[string]any
			if err := json.Unmarshal(r.Bytes(), &req); err != nil {
				continue
			}
			asked <- req
			if req["type"] != "snapshot" {
				continue
			}
			env := envelopeOf(t)
			sessions := env["sessions"].([]any)
			one := sessions[0].(map[string]any)
			one["transcriptCollected"] = true
			one["transcript"] = map[string]any{
				"path": "/fixture/a.jsonl", "sizeBytes": 1024, "tailBytes": 200,
				"truncated": false, "agentColor": "cyan", "colorFound": true,
				"apiErrors": 0, "subagents": 2, "subagentsKnown": true, "sessionKind": nil,
			}
			writeFrame(t, w, map[string]any{
				"type": "snapshot", "seq": 3, "reqId": req["reqId"], "snapshot": env,
			})
		}
	})

	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: s.dir, FirstSnapshot: readBound})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !c.Capable(wire.CapEnrich) {
		t.Fatal("the enrich capability was not read from hello")
	}

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeTranscripts = true
	snap, err := c.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Sessions) == 0 || !snap.Sessions[0].TranscriptCollected ||
		snap.Sessions[0].Transcript == nil || snap.Sessions[0].Transcript.AgentColor != "cyan" {
		t.Fatalf("the enriched facts did not survive the round trip: %+v", snap.Sessions[0].Transcript)
	}

	select {
	case req := <-asked:
		if req["type"] != "snapshot" || req["enrich"] != true {
			t.Fatalf("the client asked %v, want a one-off snapshot with enrich:true", req)
		}
		if req["reqId"] == nil {
			t.Fatal("the one-off request carried no reqId, so its answer could never be matched")
		}
	default:
		t.Fatal("the client never asked for an enriched snapshot")
	}
}

// ---------------------------------------------------------------------------
// unknown fields — P0 finding 1's requirement, in production
// ---------------------------------------------------------------------------

// A field this build does not know is DROPPED AND NAMED, at both levels of the
// frame. This is the assertion that pins DecodeSnapshot/DecodeFrame: with
// json.Unmarshal + ToDomain the advisory is structurally unreachable, because
// encoding/json has already discarded the key by the time a struct exists.
func TestAnUnknownFieldOnTheWireIsNamedRatherThanSilentlyDropped(t *testing.T) {
	s := newRawServer(t, func(t *testing.T, r *bufio.Scanner, w net.Conn) {
		writeFrame(t, w, helloOf(wire.Schema, wire.Protocol, []string{"snapshot"}))
		env := envelopeOf(t)
		env["vendorExtension"] = "something new"
		env["sessions"].([]any)[0].(map[string]any)["quantumState"] = "superposed"
		writeFrame(t, w, map[string]any{"type": "snapshot", "seq": 2, "snapshot": env})
		r.Scan()
	})

	var log strings.Builder
	c, err := sockclient.Dial(context.Background(), sockclient.Config{
		Dir: s.dir, FirstSnapshot: readBound, Log: &log,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	waitFor(t, func() bool { return len(c.UnknownFields()) >= 2 }, "the unknown fields were never reported")

	named := map[string]bool{}
	for _, a := range c.UnknownFields() {
		if a.Code != observer.AdvWireUnknownField {
			t.Fatalf("an unknown field was reported as %q", a.Code)
		}
		field, _ := a.Fields["field"].(string)
		named[field] = true
	}
	if !named["vendorExtension"] && !named["snapshot.vendorExtension"] {
		t.Fatalf("the envelope's unknown field was not named: %+v", c.UnknownFields())
	}
	if !named["sessions[].quantumState"] {
		t.Fatalf("the session's unknown field was not named: %+v", c.UnknownFields())
	}
	if !strings.Contains(log.String(), "does not know") {
		t.Fatalf("nothing was logged: %q", log.String())
	}

	// And the snapshot is still usable: an addition is dropped, not fatal.
	snap, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil || len(snap.Sessions) != 1 {
		t.Fatalf("the snapshot did not survive the unknown fields: %v %+v", err, snap)
	}
}

// ---------------------------------------------------------------------------
// shutdown
// ---------------------------------------------------------------------------

// Close ends every subscription, so a consumer blocked on one learns the server
// has gone rather than waiting forever — a run-forever server can still be
// killed at any time.
func TestCloseEndsEverySubscription(t *testing.T) {
	h := newServer(t, nil)
	c := h.dial(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	receive(t, events) // the baseline

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-events:
		if ok {
			// One more event may be in flight; the channel must still close.
			select {
			case _, ok := <-events:
				if ok {
					t.Fatal("the subscription did not end")
				}
			case <-time.After(readBound):
				t.Fatal("the subscription never closed")
			}
		}
	case <-time.After(readBound):
		t.Fatal("the subscription never closed")
	}

	if _, err := c.Snapshot(context.Background(), observer.DefaultSnapshotOptions()); !errors.Is(err, sockclient.ErrClosed) {
		t.Fatalf("a closed client answered %v, want ErrClosed", err)
	}
	// Close is idempotent: a caller that closes twice must not panic.
	if err := c.Close(); err != nil {
		t.Fatalf("the second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// envelopeOf is a minimal but COMPLETE §5.2 envelope as a map, for the scripted
// server. It is built through wire so the keys cannot drift from the schema.
func envelopeOf(t *testing.T) map[string]any {
	t.Helper()
	env := wire.NewSnapshot(snapshotOf(t0, session("s1", nil)), t0, wire.Options{TT: testTT})
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encoding the fixture envelope: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("decoding the fixture envelope: %v", err)
	}
	return out
}

func receive(t *testing.T, ch <-chan observer.Event) observer.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the subscription closed before the event arrived")
		}
		return ev
	case <-time.After(readBound):
		t.Fatal("no event arrived")
		return observer.Event{}
	}
}

// waitFor polls a condition. It is a TEST-side poll of an asynchronous
// delivery, not a production timer.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(readBound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // test-only poll
	}
	t.Fatal(msg)
}

// stoppedClock is a consumer clock stopped at one instant.
type stoppedClock struct{ at time.Time }

func (c stoppedClock) Now() time.Time { return c.at }
