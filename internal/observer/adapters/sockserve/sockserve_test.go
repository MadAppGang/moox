package sockserve_test

// The harness, and the tests about the SERVER: framing, the handshake, the
// verbs, the client cap, the stale socket, the pid file and --stop.
//
// The stream itself — deltas, filters, enrichment, backpressure, AC9 and
// AC11 — is in stream_test.go.
//
// Every test here is hermetic in the two ways P4's live defect 1 taught this
// repository to be: the state directory is a fresh temp directory (never
// ~/.local/state/tt), and no test constructs a path under the real home
// directory. TestNothingIsEverWrittenUnderTheRealHome asserts the second as a
// property of the whole package rather than as a convention.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/fakes"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// t0 is the one instant every fixture is built against. Every age and every
// timestamp any surface publishes derives from a snapshot's own instant, so a
// fixed t0 plus a fake clock is enough to make the whole suite deterministic.
var t0 = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

const testTT = "v0.4.0-test"

// readBound is how long a test waits for a frame that should already be on its
// way. It is a test-side deadline, not a production timer.
const readBound = 3 * time.Second

// ---------------------------------------------------------------------------
// temp paths
// ---------------------------------------------------------------------------

// tempStateDir is t.TempDir() with the ONE constraint a unix socket adds: the
// whole path must fit in sockaddr_un.sun_path, 104 bytes on darwin.
// t.TempDir() embeds the test's name, and this package's test names are long
// enough to blow that limit and fail with a bare "invalid argument" — which
// looks like a protocol bug and is not one.
func tempStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttobs")
	if err != nil {
		t.Fatalf("making a temp state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if n := len(filepath.Join(dir, sockserve.SocketName)); n >= 104 {
		t.Fatalf("the socket path is %d bytes, which sockaddr_un cannot hold", n)
	}
	return dir
}

// ---------------------------------------------------------------------------
// the core's stand-ins
// ---------------------------------------------------------------------------

// fakeCore is the Snapshotter. Its session set is mutable, which is how a test
// produces a delta: change the sessions, ask the loop to rebuild, and the diff
// between the two published snapshots is the event stream.
type fakeCore struct {
	mu       sync.Mutex
	sessions []observer.Session
	at       time.Time
	sources  map[string]observer.SourceReport
	err      error
	calls    int
}

func newFakeCore() *fakeCore {
	return &fakeCore{
		at: t0,
		sources: map[string]observer.SourceReport{
			observer.SourceRegistry: {
				ID:         observer.SourceRegistry,
				Role:       "authoritative",
				Path:       "/fixture/sessions",
				ObservedAt: ptr(t0),
				OK:         true,
				Counters:   map[string]int64{"records": 2},
				Labels:     map[string]string{},
				Advisories: []observer.Advisory{},
			},
			observer.SourceJobs: {
				ID:         observer.SourceJobs,
				Role:       "authoritative",
				Path:       "/fixture/jobs",
				ObservedAt: ptr(t0),
				OK:         true,
				Counters:   map[string]int64{"records": 0},
				Labels:     map[string]string{},
				Advisories: []observer.Advisory{},
			},
		},
	}
}

func (c *fakeCore) Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.mu.Lock()
	c.calls++
	sessions := make([]observer.Session, 0, len(c.sessions))
	for _, s := range c.sessions {
		if !observer.IncludedByDefault(s.Live, opts) || !observer.Visible(s, opts.Filter) {
			continue
		}
		sessions = append(sessions, s)
	}
	at, err := c.at, c.err
	// The SAME map on every call, deliberately. The Snapshotter port does not
	// promise a fresh one, and sharing it here is what caught attachWatch
	// mutating a map an already-published snapshot was exposing to a
	// subscriber's encoder — a race the detector found and no fixture would.
	sources := c.sources
	c.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &observer.Snapshot{
		ObservedAt: at,
		Counts:     countOf(sessions),
		Sources:    sources,
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}, nil
}

func (c *fakeCore) set(at time.Time, sessions ...observer.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
	c.sessions = sessions
}

func (c *fakeCore) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// countOf is the envelope tally over a session set. It mirrors what the core
// does so that a fixture snapshot is well-formed rather than half-filled —
// counts a consumer reads first must not be zero while sessions is not.
func countOf(sessions []observer.Session) observer.Counts {
	c := observer.NewCounts()
	for _, s := range sessions {
		c.Total++
		switch {
		case s.Live == nil:
			c.Unknown++
		case *s.Live:
			c.Live++
		default:
			c.Dead++
		}
		if s.Attention {
			c.Attention++
		}
		c.ByStatus[s.Status]++
		c.ByKind[s.Kind]++
	}
	return c
}

// emptyTree is the PathLister for a machine with nothing in it. The loop needs
// the port; these tests are about the socket, not about §6.3's fixed point,
// which P5 already pins over the real adapter.
type emptyTree struct{}

func (emptyTree) ListDirs(context.Context, string) ([]string, error)          { return nil, nil }
func (emptyTree) ListFiles(context.Context, string, string) ([]string, error) { return nil, nil }
func (emptyTree) Exists(context.Context, string) (bool, error)                { return false, nil }

// ledgerTimers is AC9's arm/disarm ledger, the same seam P5 used: "the idle
// steady state has ZERO armed timers" is a claim about arming discipline, and
// an AST allowlist cannot prove it.
type ledgerTimers struct {
	mu   sync.Mutex
	arms int
}

func (l *ledgerTimers) NewTimer(d time.Duration) observer.Timer {
	l.mu.Lock()
	l.arms++
	l.mu.Unlock()
	return &ledgerTimer{t: time.NewTimer(d)}
}

func (l *ledgerTimers) Arms() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.arms
}

type ledgerTimer struct{ t *time.Timer }

func (lt *ledgerTimer) Stop() bool          { return lt.t.Stop() }
func (lt *ledgerTimer) C() <-chan time.Time { return lt.t.C }

// stubRefresher stands in for the corroboration path. The core owns the rate
// limit, so a test that wants a rate-limited answer says so here rather than
// trying to provoke §4.3's backoff through a socket.
type stubRefresher struct {
	mu      sync.Mutex
	outcome observer.RefreshOutcome
	calls   int
	asked   []string
}

func (r *stubRefresher) RefreshSources(_ context.Context, sources []string) observer.RefreshOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.asked = append(r.asked, sources...)
	return r.outcome
}

func (r *stubRefresher) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ---------------------------------------------------------------------------
// the harness
// ---------------------------------------------------------------------------

type harness struct {
	t       *testing.T
	dir     string
	core    *fakeCore
	loop    *observer.WatchLoop
	srv     *sockserve.Server
	timers  *ledgerTimers
	watcher *fakes.Watcher
	clock   *fakes.Clock
	enrich  *observer.Observer
	refresh *stubRefresher
	ctx     context.Context
	cancel  context.CancelFunc
	served  chan struct{}
	set     *fakes.Set
}

type options struct {
	subscriberBuffer int
	maxClients       int
	writeBuffer      int
	withRefresher    bool
	withEnricher     bool
	refreshOutcome   observer.RefreshOutcome
	watchErr         error
	claudeRoot       string
	dir              string
	// dontListen builds the server without binding, for the tests that drive
	// Listen themselves.
	dontListen bool
}

func newHarness(t *testing.T, tune func(*options)) *harness {
	t.Helper()

	o := options{}
	if tune != nil {
		tune(&o)
	}

	h := &harness{
		t:      t,
		dir:    o.dir,
		core:   newFakeCore(),
		timers: &ledgerTimers{},
		clock:  fakes.NewClock(t0),
		served: make(chan struct{}),
	}
	if h.dir == "" {
		h.dir = tempStateDir(t)
	}
	if o.claudeRoot == "" {
		// statedir requires a Claude tree to refuse (moox §2.4); a sibling
		// that does not exist keeps the refusal in force and never fires.
		o.claudeRoot = h.dir + "-claude-root"
	}

	deps, set := fakes.Deps(t0)
	h.set = set
	h.watcher = set.Watcher
	h.watcher.WatchErr = o.watchErr
	obs, err := observer.New(deps, observer.Config{})
	if err != nil {
		t.Fatalf("building the enrichment core: %v", err)
	}
	h.enrich = obs

	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:             h.core,
		Watch:            h.watcher,
		Tree:             emptyTree{},
		Clock:            h.clock,
		Timers:           h.timers,
		SubscriberBuffer: o.subscriberBuffer,
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}
	h.loop = loop

	deps2 := sockserve.Deps{Events: loop, Core: h.core, Rebuild: loop}
	if o.withEnricher {
		deps2.Enrich = obs
	}
	if o.withRefresher {
		h.refresh = &stubRefresher{outcome: o.refreshOutcome}
		deps2.Refresh = h.refresh
	}

	srv, err := sockserve.New(sockserve.Config{
		Dir:              h.dir,
		ClaudeRoot:       o.claudeRoot,
		TT:               testTT,
		PID:              os.Getpid(),
		MaxClients:       o.maxClients,
		WriteBufferBytes: o.writeBuffer,
		Clock:            h.clock,
		Deps:             deps2,
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	h.srv = srv

	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		h.cancel()
		<-h.served
	})

	if o.dontListen {
		close(h.served)
		return h
	}

	if err := srv.Listen(); err != nil {
		close(h.served)
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		defer close(h.served)
		if err := srv.Serve(h.ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	return h
}

// publish rebuilds once, synchronously, and returns the published snapshot. It
// is how these tests produce an event: the deltas are the difference between
// two published snapshots, exactly as they are in production.
func (h *harness) publish() *observer.Snapshot {
	h.t.Helper()
	snap, _, err := h.loop.Rebuild(context.Background(), "test")
	if err != nil {
		h.t.Fatalf("Rebuild: %v", err)
	}
	return snap
}

// ---------------------------------------------------------------------------
// the client
// ---------------------------------------------------------------------------

// client is a raw NDJSON reader. It is deliberately NOT a sockclient — that
// arrives with P8, and a test that used it would be asserting the protocol
// against its own second implementation.
type client struct {
	t  *testing.T
	nc net.Conn
	rd *bufio.Reader
	// lastLine is the raw bytes of the most recent frame. AC11 compares the
	// server's BYTES against the CLI's bytes, so a test needs the line as it
	// arrived and not only the decoded struct.
	lastLine []byte
}

func (h *harness) dial() *client {
	h.t.Helper()
	nc, err := net.Dial("unix", h.srv.SocketPath())
	if err != nil {
		h.t.Fatalf("dialling %s: %v", h.srv.SocketPath(), err)
	}
	h.t.Cleanup(func() { _ = nc.Close() })
	return &client{t: h.t, nc: nc, rd: bufio.NewReaderSize(nc, 1<<20)}
}

// line reads one raw frame, bounded.
func (c *client) line() []byte {
	c.t.Helper()
	if err := c.nc.SetReadDeadline(time.Now().Add(readBound)); err != nil {
		c.t.Fatalf("read deadline: %v", err)
	}
	data, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("reading a frame: %v", err)
	}
	c.lastLine = data
	return data
}

// frame reads and decodes one frame through the codec P8 must use.
func (c *client) frame() wire.Frame {
	c.t.Helper()
	raw := c.line()
	f, notes, err := wire.DecodeFrame(raw)
	if err != nil {
		c.t.Fatalf("decoding a frame: %v\n%s", err, raw)
	}
	if len(notes) > 0 {
		c.t.Fatalf("the server sent fields wire does not know: %v\n%s", notes, raw)
	}
	return f
}

// expect reads one frame and requires its type.
func (c *client) expect(typ string) wire.Frame {
	c.t.Helper()
	f := c.frame()
	if f.Type != typ {
		c.t.Fatalf("frame type %q, want %q", f.Type, typ)
	}
	return f
}

// until reads frames until one of the wanted type arrives, returning it and
// everything skipped on the way.
func (c *client) until(typ string) (wire.Frame, []wire.Frame) {
	c.t.Helper()
	var skipped []wire.Frame
	for i := 0; i < 64; i++ {
		f := c.frame()
		if f.Type == typ {
			return f, skipped
		}
		skipped = append(skipped, f)
	}
	c.t.Fatalf("no %s frame in 64 frames; saw %s", typ, typesOf(skipped))
	return wire.Frame{}, nil
}

// handshake reads hello and the unsolicited baseline snapshot, which is what
// every connection gets without writing a byte.
func (c *client) handshake() (hello, snapshot wire.Frame) {
	c.t.Helper()
	return c.expect(wire.FrameHello), c.expect(wire.FrameSnapshot)
}

func (c *client) send(f wire.ClientFrame) {
	c.t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		c.t.Fatalf("marshalling a client frame: %v", err)
	}
	c.writeRaw(append(data, '\n'))
}

func (c *client) writeRaw(data []byte) {
	c.t.Helper()
	if err := c.nc.SetWriteDeadline(time.Now().Add(readBound)); err != nil {
		c.t.Fatalf("write deadline: %v", err)
	}
	if _, err := c.nc.Write(data); err != nil {
		c.t.Fatalf("writing: %v", err)
	}
}

func typesOf(frames []wire.Frame) string {
	names := make([]string, 0, len(frames))
	for _, f := range frames {
		names = append(names, f.Type)
	}
	return strings.Join(names, ",")
}

func ptr[T any](v T) *T { return &v }

// session builds a minimal well-formed domain session.
func session(id string, tune func(*observer.Session)) observer.Session {
	s := observer.Session{
		SessionID:    id,
		Kind:         observer.KindInteractive,
		Name:         id,
		NameSource:   "user",
		FormerNames:  []string{},
		CWD:          "/home/user/dotfiles",
		Status:       observer.StatusIdle,
		RawStatus:    "idle",
		StatusSource: observer.SourceRegistry,
		StatusSince:  ptr(t0.Add(-5 * time.Minute)),
		StatusAgeMs:  ptr(int64(5 * 60 * 1000)),
		PID:          ptr(4242),
		Live:         ptr(true),
		LiveReason:   observer.LiveReasonProcOnly,
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

// ---------------------------------------------------------------------------
// the handshake and the framing
// ---------------------------------------------------------------------------

// A client that connects and never writes gets the full stream. That is §5.3's
// design target rather than a convenience: "consumers should not poll" must not
// be replaced by "consumers must implement a handshake".
func TestASilentClientGetsHelloAndTheWholeSnapshotWithoutWritingAnything(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil), session("s2", nil))
	h.publish()

	c := h.dial()
	hello, snap := c.handshake()

	if hello.Schema != wire.Schema || hello.Protocol != wire.Protocol {
		t.Fatalf("hello declares schema %d protocol %d, want %d and %d",
			hello.Schema, hello.Protocol, wire.Schema, wire.Protocol)
	}
	if hello.TT != testTT {
		t.Fatalf("hello.tt = %q, want %q", hello.TT, testTT)
	}
	if hello.PID != os.Getpid() {
		t.Fatalf("hello.pid = %d, want this process %d", hello.PID, os.Getpid())
	}
	if hello.Sources[observer.SourceRegistry].ID != observer.SourceRegistry {
		t.Fatalf("hello carries no registry source report: %+v", hello.Sources)
	}
	if snap.Snapshot == nil || len(snap.Snapshot.Sessions) != 2 {
		t.Fatalf("the baseline snapshot does not carry both sessions: %+v", snap.Snapshot)
	}
	if snap.ReqID != nil {
		t.Fatalf("an unsolicited snapshot carries reqId %v, want null", *snap.ReqID)
	}
}

// The capability list is derived from the ports that are wired. §7.5(f) has
// clients act on the ABSENCE of "enrich", so a capability declared for a nil
// port would be a promise the build cannot keep.
func TestHelloDeclaresOnlyTheCapabilitiesThePortsCanDeliver(t *testing.T) {
	t.Run("bare", func(t *testing.T) {
		h := newHarness(t, nil)
		h.publish()
		hello, _ := h.dial().handshake()
		// include-dead needs no optional port — the filter is the core's own
		// predicate — so it is declared by every build that honours it (code
		// review round 2, A10). include-spawned likewise: hiding launched
		// sessions is matchesFilter's rule, not a port's.
		assertCaps(t, hello.Capabilities,
			wire.CapSnapshot, wire.CapDeltas, wire.CapFilter, wire.CapPing, wire.CapIncludeDead,
			wire.CapIncludeSpawned)
	})

	t.Run("with both optional ports", func(t *testing.T) {
		h := newHarness(t, func(o *options) { o.withEnricher = true; o.withRefresher = true })
		h.publish()
		hello, _ := h.dial().handshake()
		assertCaps(t, hello.Capabilities,
			wire.CapSnapshot, wire.CapDeltas, wire.CapFilter, wire.CapPing, wire.CapIncludeDead,
			wire.CapIncludeSpawned, wire.CapRefresh, wire.CapEnrich)
	})
}

func assertCaps(t *testing.T, got []string, want ...string) {
	t.Helper()
	g, w := append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("capabilities %v, want %v", g, w)
	}
}

// P0 left the omitempty-versus-narrow-frames question to P7 and P7 keeps the
// ban. This is the assertion that makes the answer structural: every frame
// carries EVERY key wire.Frame declares, so a consumer can read a field
// without asking which frame type it is on, and an absent payload is null
// rather than missing.
func TestEveryFrameCarriesEveryKeyAndTheSequenceIsMonotonicPerConnection(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	want := frameKeys(t)

	var seqs []int64
	for i := 0; i < 2; i++ { // hello, snapshot
		raw := c.line()
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decoding %s: %v", raw, err)
		}
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("frame %d keys\n got %v\nwant %v", i, keys, want)
		}
		var seq int64
		if err := json.Unmarshal(got["seq"], &seq); err != nil {
			t.Fatalf("decoding seq: %v", err)
		}
		seqs = append(seqs, seq)
	}
	// A third frame, this time an answer, so the counter is seen to advance
	// across frame kinds.
	c.send(wire.ClientFrame{Type: wire.FramePing, ReqID: ptr("r-1")})
	seqs = append(seqs, c.expect(wire.FramePong).Seq)

	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("sequence numbers %v; want 1,2,3 — monotonic per connection from 1", seqs)
		}
	}
}

// frameKeys is every json tag on wire.Frame, which is what "every key is
// present" is asserted against — derived by reflection so a field added later
// joins the assertion automatically.
func frameKeys(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf(wire.Frame{})
	keys := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("wire.Frame field %s has no json tag", typ.Field(i).Name)
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

// M10 — pong exists, echoes the reqId, and carries an instant.
//
// The instant is the STATE'S, not a fresh clock read: the clock is advanced an
// hour after the rebuild and the pong still reports the rebuild's instant. That
// is P7's deliberate divergence from §5.3's "measures clock skew", and it is
// the rule that three live defects in P2, P3 and P4 came from breaking.
func TestPongEchoesTheReqIdAndReportsTheStatesInstantNotAFreshClockRead(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()
	h.clock.Advance(time.Hour)

	c := h.dial()
	c.handshake()

	c.send(wire.ClientFrame{Type: wire.FramePing, ReqID: ptr("r-18")})
	pong := c.expect(wire.FramePong)
	if pong.ReqID == nil || *pong.ReqID != "r-18" {
		t.Fatalf("pong.reqId = %v, want r-18", pong.ReqID)
	}
	if pong.ObservedAt == nil {
		t.Fatal("pong carries no observedAt")
	}
	if want := observer.FormatTime(t0); *pong.ObservedAt != want {
		t.Fatalf("pong.observedAt = %q, want the snapshot's own instant %q", *pong.ObservedAt, want)
	}

	// A ping with no reqId is ANSWERED with reqId: null, not ignored.
	c.send(wire.ClientFrame{Type: wire.FramePing})
	if bare := c.expect(wire.FramePong); bare.ReqID != nil {
		t.Fatalf("a ping with no reqId was answered with reqId %q", *bare.ReqID)
	}
}

// notCapable is DECLARED, in hello, and it comes from the same core value the
// snapshot envelope carries — one source, so a consumer cannot see two answers
// to "will this server tell me when a threshold is crossed".
func TestNotCapableIsDeclaredInHelloAndMatchesTheEnvelopeFromOneSource(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	hello, snap := h.dial().handshake()
	want := observer.DefaultNotCapable()
	if !reflect.DeepEqual(hello.NotCapable, want) {
		t.Fatalf("hello.notCapable = %v, want %v", hello.NotCapable, want)
	}
	if snap.Snapshot == nil || !reflect.DeepEqual(snap.Snapshot.NotCapable, want) {
		t.Fatalf("the envelope's notCapable = %v, want %v", snap.Snapshot.NotCapable, want)
	}
	if len(want) != 1 || want[0] != "time-transitions" {
		t.Fatalf("notCapable = %v, want exactly [time-transitions]", want)
	}
}

// ---------------------------------------------------------------------------
// the verbs
// ---------------------------------------------------------------------------

// A one-off snapshot request is answered from the PUBLISHED snapshot. A
// client-triggered rebuild would make `snapshot` a poll with extra steps, and
// would let one consumer's polling drive the read cost of every other.
func TestASnapshotRequestIsAnsweredFromThePublishedStateWithoutRebuilding(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()
	before := h.core.reads()

	c.send(wire.ClientFrame{Type: wire.FrameSnapshot, ReqID: ptr("r-16")})
	snap := c.expect(wire.FrameSnapshot)
	if snap.ReqID == nil || *snap.ReqID != "r-16" {
		t.Fatalf("the answer's reqId = %v, want r-16", snap.ReqID)
	}
	if after := h.core.reads(); after != before {
		t.Fatalf("a snapshot request cost %d core reads; it must cost none", after-before)
	}
}

// §7.5(d)'s round trip: a refresh is answered by a sources frame with
// cause:"refresh" and the matching reqId, AFTER the refresh resolves.
func TestARefreshIsAnsweredByASourcesFrameWithTheMatchingReqId(t *testing.T) {
	h := newHarness(t, func(o *options) {
		o.withRefresher = true
		o.refreshOutcome = observer.RefreshOutcome{Refreshed: []string{observer.SourceAgentsCLI}}
	})
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{Type: wire.FrameRefresh, ReqID: ptr("r-17"), Sources: []string{observer.SourceAgentsCLI}})

	f, _ := c.until(wire.FrameSources)
	if f.ReqID == nil || *f.ReqID != "r-17" {
		t.Fatalf("the sources frame's reqId = %v, want r-17", f.ReqID)
	}
	if f.Cause != wire.CauseRefresh {
		t.Fatalf("cause = %q, want %q", f.Cause, wire.CauseRefresh)
	}
	if f.Sources[observer.SourceRegistry].ID == "" {
		t.Fatalf("the sources frame carries no source reports: %+v", f.Sources)
	}
	if h.refresh.Calls() != 1 {
		t.Fatalf("the corroboration source was refreshed %d times, want 1", h.refresh.Calls())
	}
}

// rate_limited is an ANSWER, not a failure: the client answers its own caller
// from the current snapshot with a refresh-rejected advisory (§5.4). The
// connection survives it.
func TestARateLimitedRefreshIsAnErrorAnswerAndTheStreamContinues(t *testing.T) {
	h := newHarness(t, func(o *options) {
		o.withRefresher = true
		o.refreshOutcome = observer.RefreshOutcome{
			Rejected: map[string]string{observer.SourceAgentsCLI: observer.RejectRateLimited},
		}
	})
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{Type: wire.FrameRefresh, ReqID: ptr("r-9")})

	f, _ := c.until(wire.FrameError)
	if f.Code != wire.CodeRateLimited {
		t.Fatalf("error code = %q, want %q", f.Code, wire.CodeRateLimited)
	}
	if f.ReqID == nil || *f.ReqID != "r-9" {
		t.Fatalf("the error's reqId = %v, want r-9", f.ReqID)
	}

	// Still alive.
	c.send(wire.ClientFrame{Type: wire.FramePing, ReqID: ptr("r-10")})
	if pong := c.expect(wire.FramePong); *pong.ReqID != "r-10" {
		t.Fatalf("the connection did not survive a rate-limited refresh")
	}
}

// A build with no corroboration port answers `unsupported` rather than
// pretending to refresh. hello already said so; this is the verb agreeing with
// the declaration.
func TestARefreshWithNoCorroborationPortIsUnsupported(t *testing.T) {
	h := newHarness(t, nil)
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{Type: wire.FrameRefresh, ReqID: ptr("r-1")})

	f := c.expect(wire.FrameError)
	if f.Code != wire.CodeUnsupported {
		t.Fatalf("error code = %q, want %q", f.Code, wire.CodeUnsupported)
	}
}

// Everything a client can get wrong is ANSWERED, and the subscription survives
// it: one bad line is not a reason to drop a subscriber holding 39 sessions'
// worth of state.
func TestMalformedAndUnknownClientFramesAreAnsweredAndTheConnectionSurvives(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()

	c.writeRaw([]byte("this is not json\n"))
	if f := c.expect(wire.FrameError); f.Code != wire.CodeBadRequest {
		t.Fatalf("a malformed line produced code %q, want %q", f.Code, wire.CodeBadRequest)
	}

	c.writeRaw([]byte(`{"type":"teleport","reqId":"r-2"}` + "\n"))
	f := c.expect(wire.FrameError)
	if f.Code != wire.CodeUnsupported {
		t.Fatalf("an unknown verb produced code %q, want %q", f.Code, wire.CodeUnsupported)
	}
	if f.ReqID == nil || *f.ReqID != "r-2" {
		t.Fatalf("the refusal did not echo the reqId: %v", f.ReqID)
	}

	// An unknown FIELD is ignored silently — degrade silently on unknown data,
	// so a newer client still gets its subscription.
	c.writeRaw([]byte(`{"type":"ping","reqId":"r-3","fromTheFuture":{"a":1}}` + "\n"))
	if pong := c.expect(wire.FramePong); *pong.ReqID != "r-3" {
		t.Fatalf("an unknown field was not ignored")
	}

	// A filter naming a kind that does not exist is a bad_request rather than
	// a silently empty stream: there are exactly three kinds, so a typo can be
	// diagnosed instead of guessed at.
	c.send(wire.ClientFrame{
		Type:   wire.FrameSubscribe,
		ReqID:  ptr("r-4"),
		Filter: &wire.ClientFilter{Kind: []string{"intercative"}},
	})
	if f := c.expect(wire.FrameError); f.Code != wire.CodeBadRequest {
		t.Fatalf("an unknown kind produced code %q, want %q", f.Code, wire.CodeBadRequest)
	}
}

// A status nobody has ever seen is NOT an error (B7: status is an observed
// set). It narrows to nothing, which is the honest answer.
func TestAFilterNamingAnUnseenStatusIsAcceptedAndMatchesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{
		Type:   wire.FrameSubscribe,
		ReqID:  ptr("r-5"),
		Filter: &wire.ClientFilter{Status: []string{"contemplating"}},
	})

	snap := c.expect(wire.FrameSnapshot)
	if snap.ReqID == nil || *snap.ReqID != "r-5" {
		t.Fatalf("the new baseline did not echo the subscribe's reqId: %v", snap.ReqID)
	}
	if n := len(snap.Snapshot.Sessions); n != 0 {
		t.Fatalf("an unseen status matched %d sessions, want 0", n)
	}
	if snap.Snapshot.Counts.Total != 0 {
		t.Fatalf("counts.total = %d over an empty view", snap.Snapshot.Counts.Total)
	}
}

// A line over the 1 MiB limit leaves the reader mid-frame with nothing to
// resynchronise against, so the connection ENDS. The server answers
// error{bad_request} first, best effort — but a peer that closes a socket with
// unread bytes still in its receive queue sends an RST, which can discard that
// answer in flight, so the assertion is on the close and the answer is
// accepted when it survives.
func TestAFrameOverTheLineLimitEndsTheConnection(t *testing.T) {
	h := newHarness(t, nil)
	h.publish()

	c := h.dial()
	c.handshake()

	huge := make([]byte, sockserve.MaxFrameBytes+16)
	for i := range huge {
		huge[i] = 'x'
	}
	huge[0], huge[len(huge)-1] = '"', '\n'
	// Write errors are expected: the server may close mid-write.
	_ = c.nc.SetWriteDeadline(time.Now().Add(readBound))
	_, _ = c.nc.Write(huge)

	if err := c.nc.SetReadDeadline(time.Now().Add(readBound)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	sawError := false
	for {
		raw, err := c.rd.ReadBytes('\n')
		if err != nil {
			break // EOF or RST: the connection ended, which is the contract.
		}
		f, _, derr := wire.DecodeFrame(raw)
		if derr == nil && f.Type == wire.FrameError {
			sawError = true
			if f.Code != wire.CodeBadRequest {
				t.Fatalf("code = %q, want %q", f.Code, wire.CodeBadRequest)
			}
		}
	}
	t.Logf("the oversized frame ended the connection (answer delivered: %v)", sawError)
}

// Over the cap, a connection gets an ANSWER and then a close, so a client
// learns why instead of seeing an unexplained EOF.
func TestTheClientCapIsAnAnswerThenAClose(t *testing.T) {
	h := newHarness(t, func(o *options) { o.maxClients = 1 })
	h.core.set(t0, session("s1", nil))
	h.publish()

	first := h.dial()
	first.handshake()

	second := h.dial()
	f := second.expect(wire.FrameError)
	if f.Code != wire.CodeUnsupported {
		t.Fatalf("code = %q, want %q", f.Code, wire.CodeUnsupported)
	}
	if !strings.Contains(f.Detail, "too many clients") {
		t.Fatalf("detail = %q, want it to name the cap", f.Detail)
	}

	// And the first connection is untouched.
	first.send(wire.ClientFrame{Type: wire.FramePing, ReqID: ptr("r-1")})
	first.expect(wire.FramePong)
}

// ---------------------------------------------------------------------------
// shutdown
// ---------------------------------------------------------------------------

// bye has exactly ONE reason now that there is no idle timeout, and EOF
// follows it. A client must handle both, because a run-forever server can be
// killed at any time.
func TestByeCarriesTheOnlyReasonAndIsFollowedByEOF(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()

	h.cancel()
	bye, _ := c.until(wire.FrameBye)
	if bye.Reason != wire.ReasonShutdown {
		t.Fatalf("bye.reason = %q, want %q", bye.Reason, wire.ReasonShutdown)
	}
	if _, err := c.rd.ReadBytes('\n'); err == nil {
		t.Fatal("the socket stayed open after bye")
	}
}

// The socket and the pid file are both gone after a clean shutdown. A
// leftover pair is the CRASHED state, which bind and Stop reconcile from two
// directions — so it must not be the state a clean exit leaves.
func TestACleanShutdownLeavesNeitherSocketNorPidFile(t *testing.T) {
	h := newHarness(t, nil)
	h.publish()

	if _, err := os.Stat(h.srv.SocketPath()); err != nil {
		t.Fatalf("the socket is not there while serving: %v", err)
	}
	if _, err := os.Stat(h.srv.PidPath()); err != nil {
		t.Fatalf("the pid file is not there while serving: %v", err)
	}

	h.cancel()
	<-h.served

	for _, p := range []string{h.srv.SocketPath(), h.srv.PidPath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived a clean shutdown (err = %v)", p, err)
		}
	}
	if left := dirEntries(t, h.dir); len(left) != 0 {
		t.Fatalf("the state directory still holds %v", left)
	}
}

// ---------------------------------------------------------------------------
// the pid file, the stale socket and --stop
// ---------------------------------------------------------------------------

func TestThePidFileDescribesTheRunningServer(t *testing.T) {
	h := newHarness(t, nil)
	h.publish()

	data, err := os.ReadFile(h.srv.PidPath())
	if err != nil {
		t.Fatalf("reading the pid file: %v", err)
	}
	rec, err := wire.DecodePidFile(data)
	if err != nil {
		t.Fatalf("decoding the pid file: %v", err)
	}
	if rec.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", rec.PID, os.Getpid())
	}
	if rec.Socket != h.srv.SocketPath() {
		t.Fatalf("socket = %q, want %q", rec.Socket, h.srv.SocketPath())
	}
	if rec.TT != testTT {
		t.Fatalf("tt = %q, want %q", rec.TT, testTT)
	}
	if rec.StartedAt != observer.FormatTime(t0) {
		t.Fatalf("startedAt = %q, want the injected clock's %q", rec.StartedAt, observer.FormatTime(t0))
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("the pid file is not newline-terminated")
	}
}

// A live socket is never unlinked. The second server reports the first one's
// path and pid and its caller exits 0 — a user asking for an observer and
// finding one is not a failure.
func TestALiveSocketIsReportedAndNeverUnlinked(t *testing.T) {
	first := newHarness(t, nil)
	first.publish()

	second := newHarness(t, func(o *options) {
		o.dir = first.dir
		o.dontListen = true
	})
	err := second.srv.Listen()

	var running *sockserve.ErrAlreadyRunning
	if !errors.As(err, &running) {
		t.Fatalf("Listen returned %v, want ErrAlreadyRunning", err)
	}
	if running.PID != os.Getpid() {
		t.Fatalf("the report names pid %d, want the first server's %d", running.PID, os.Getpid())
	}
	if running.Path != first.srv.SocketPath() {
		t.Fatalf("the report names %q, want %q", running.Path, first.srv.SocketPath())
	}

	// The first server is untouched: still listening, still answering.
	c := first.dial()
	c.handshake()
}

// A socket file whose process is gone is unlinked and rebound ONCE. This is
// the crashed-server state, produced here the way a crash produces it: the
// file outlives the listener.
func TestAStaleSocketIsUnlinkedAndRebound(t *testing.T) {
	dir := tempStateDir(t)
	path := filepath.Join(dir, sockserve.SocketName)

	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("binding the doomed listener: %v", err)
	}
	// Leave the file behind on close, which is exactly what a killed process
	// does.
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket was not left behind: %v", err)
	}

	h := newHarness(t, func(o *options) { o.dir = dir })
	h.publish()
	h.dial().handshake()
}

// A path that exists, is in the way, and cannot be dialled for an UNKNOWN
// reason is left alone. Unlinking it is how a healthy server gets silently
// displaced; refusing produces a message the user can act on.
func TestANonSocketInTheWayIsRefusedRatherThanUnlinked(t *testing.T) {
	dir := tempStateDir(t)
	path := filepath.Join(dir, sockserve.SocketName)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	h := newHarness(t, func(o *options) { o.dir = dir; o.dontListen = true })
	err := h.srv.Listen()
	if err == nil {
		t.Fatal("Listen succeeded over a regular file")
	}
	var running *sockserve.ErrAlreadyRunning
	if errors.As(err, &running) {
		t.Fatalf("a regular file was reported as a live server: %v", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("the file in the way was removed: %v", serr)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the error does not name the path: %v", err)
	}
}

// --stop signals the pid in the pid file. The pid is a CHILD process, not this
// one: a test that signalled itself would kill the test binary.
//
// The record's startedAt is NOW, because the child really did start now: with
// no socket to dial, the start instant is the evidence that this pid is the
// process the record was written about. That is the second gate, and it is the
// one that survives a socket having been removed underneath a live server.
func TestStopSignalsThePidInThePidFile(t *testing.T) {
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start a child to signal: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	dir := tempStateDir(t)
	writePidFileStartedAt(t, dir, child.Process.Pid,
		filepath.Join(dir, sockserve.SocketName), time.Now())

	out, err := sockserve.Stop(dir, dir+"-claude-root")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !out.Signalled || out.PID != child.Process.Pid {
		t.Fatalf("Stop reported %+v, want a signal to pid %d", out, child.Process.Pid)
	}
	if out.Corroborated == "" {
		t.Error("a signal went out with no evidence recorded for why the pid was believed")
	}

	state, werr := child.Process.Wait()
	if werr != nil {
		t.Fatalf("waiting for the child: %v", werr)
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("the child ended as %v, want a SIGTERM", state)
	}
}

// The defect this pair of tests pins: a pid file survives crashes and reboots,
// pids restart low after a reboot and climb back through the same range, and
// the live Claude pids on this machine (measured 1061-98693) fully overlap it.
// Stop used to SIGTERM whatever integer it found and print "sent SIGTERM to pid
// N"; the likely victim of a stale record was one of the user's own Claude
// sessions, losing its in-flight work — delivered by the one command in a
// read-only tool that sends a signal.
//
// Here the pid is a live process that is NOT the observer, and the record was
// written long before that process existed. Nothing may be signalled, and —
// just as important — nothing may be removed, because a refusal the user can
// read beats both a kill and a silent cleanup.
func TestStopRefusesAPidItCannotCorroborate(t *testing.T) {
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start a child to signal: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	dir := tempStateDir(t)
	// The reboot signature: the record predates the process wearing its pid.
	writePidFileStartedAt(t, dir, child.Process.Pid,
		filepath.Join(dir, sockserve.SocketName), time.Now().Add(-48*time.Hour))

	out, err := sockserve.Stop(dir, dir+"-claude-root")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if out.Signalled {
		t.Fatalf("Stop signalled pid %d on the word of a stale pid file", out.PID)
	}
	if !out.Refused || out.Reason == "" {
		t.Fatalf("Stop reported %+v, want a refusal with a reason", out)
	}
	if out.StaleCleaned {
		t.Error("Stop removed the files of a pid it could not identify")
	}
	// The child must still be alive and unsignalled.
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the innocent process is gone: %v", err)
	}
	if left := dirEntries(t, dir); len(left) != 1 {
		t.Errorf("the state directory holds %v, want the pid file left alone", left)
	}
}

// Stopping an observer that is not running is a FACT, not a failure — like
// "no Claude is running" on S1 — so a script that stops one twice succeeds
// twice.
func TestStopWithNoPidFileIsANoOp(t *testing.T) {
	dir := tempStateDir(t)
	out, err := sockserve.Stop(dir, dir+"-claude-root")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if out.Found || out.Signalled || out.StaleCleaned {
		t.Fatalf("Stop reported %+v over an empty directory", out)
	}
}

// A pid file naming a process that no longer exists is the crashed state from
// the other direction, and Stop reconciles it: both files go.
//
// The socket is a REAL stale socket — bound, then abandoned without an unlink,
// as a crashed server leaves it. This fixture used to be a regular file named
// observer.sock, and Stop deleting it was the very defect code review round 2
// (A1) found: removal now requires the entry to BE a socket, and
// TestStopRemovesOnlyASocketAtTheSocketName pins the regular-file case.
func TestStopCleansUpAStalePidFileAndItsSocket(t *testing.T) {
	dir := tempStateDir(t)
	sock := filepath.Join(dir, sockserve.SocketName)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatalf("seeding the socket: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
	// A pid the kernel cannot ever have allocated: darwin's pid ceiling is
	// 99998, so this is ESRCH by construction rather than by luck. Picking a
	// recycled pid instead would risk signalling one of the user's own
	// processes.
	writePidFile(t, dir, 1<<30, sock)

	out, err := sockserve.Stop(dir, dir+"-claude-root")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !out.StaleCleaned || out.Signalled {
		t.Fatalf("Stop reported %+v, want a stale cleanup", out)
	}
	if left := dirEntries(t, dir); len(left) != 0 {
		t.Fatalf("the state directory still holds %v", left)
	}
}

// ---------------------------------------------------------------------------
// where it may write — §9.4's runtime half
// ---------------------------------------------------------------------------

// The static audit is keyed on the package because an AST pass cannot evaluate
// a path built at runtime. This is the other half: a state directory inside the
// Claude tree is REFUSED, and nothing is created.
func TestAStateDirectoryInsideTheClaudeTreeIsRefused(t *testing.T) {
	claudeRoot := tempStateDir(t)
	inside := filepath.Join(claudeRoot, "sneaky", "state")

	h := newHarness(t, func(o *options) {
		o.dir = inside
		o.claudeRoot = claudeRoot
		o.dontListen = true
	})
	err := h.srv.Listen()
	if !errors.Is(err, sockserve.ErrOutsideStateDir) {
		t.Fatalf("Listen returned %v, want ErrOutsideStateDir", err)
	}
	if _, serr := os.Stat(inside); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the refused directory was created anyway (err = %v)", serr)
	}
}

// A `..` escape is refused for the same reason and by the same rule.
func TestADirectoryEscapingIntoTheClaudeTreeIsRefused(t *testing.T) {
	root := tempStateDir(t)
	claudeRoot := filepath.Join(root, "claude")
	escape := filepath.Join(root, "state", "..", "claude", "state")

	h := newHarness(t, func(o *options) {
		o.dir = escape
		o.claudeRoot = claudeRoot
		o.dontListen = true
	})
	if err := h.srv.Listen(); !errors.Is(err, sockserve.ErrOutsideStateDir) {
		t.Fatalf("Listen returned %v, want ErrOutsideStateDir", err)
	}
}

// A whole cycle — bind, serve, subscribe, enrich, refresh, stop — writes
// EXACTLY two files, both inside the injected directory.
func TestAWholeCycleWritesOnlyTheSocketAndThePidFile(t *testing.T) {
	h := newHarness(t, func(o *options) {
		o.withEnricher = true
		o.withRefresher = true
		o.refreshOutcome = observer.RefreshOutcome{Refreshed: []string{observer.SourceAgentsCLI}}
	})
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{Type: wire.FrameSubscribe, Enrich: ptr(true), ReqID: ptr("r-1")})
	c.until(wire.FrameSnapshot)
	c.send(wire.ClientFrame{Type: wire.FrameRefresh, ReqID: ptr("r-2")})
	c.until(wire.FrameSources)

	got := dirEntries(t, h.dir)
	want := []string{sockserve.PidName, sockserve.SocketName}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the state directory holds %v, want exactly %v", got, want)
	}
}

// The hermeticity rule itself, as a property of the package rather than a
// convention: P4's tests once published fixture listings into the user's real
// ~/.local/state/tt cache, and the lesson was that "the tests use t.TempDir()"
// has to be asserted somewhere.
func TestNothingIsEverWrittenUnderTheRealHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to protect: %v", err)
	}
	dir := tempStateDir(t)
	if strings.HasPrefix(dir, filepath.Join(home, ".local")) {
		t.Fatalf("the temp state dir %s is inside the real state tree", dir)
	}
	h := newHarness(t, func(o *options) { o.dir = dir })
	h.publish()
	for _, p := range []string{h.srv.SocketPath(), h.srv.PidPath()} {
		if !strings.HasPrefix(p, dir) {
			t.Fatalf("the server writes %s, which is outside the injected %s", p, dir)
		}
	}
}

func writePidFile(t *testing.T, dir string, pid int, socket string) {
	t.Helper()
	writePidFileStartedAt(t, dir, pid, socket, t0)
}

// writePidFileStartedAt is writePidFile with the startedAt the record carries
// spelled out, because --stop now CORROBORATES a live pid against it: a record
// whose startedAt predates the process it names is the recycled-pid signature,
// and two tests need to be on opposite sides of that line.
func writePidFileStartedAt(t *testing.T, dir string, pid int, socket string, startedAt time.Time) {
	t.Helper()
	data, err := wire.EncodePidFile(wire.PidFile{
		PID:       pid,
		StartedAt: observer.FormatTime(startedAt),
		Socket:    socket,
		TT:        testTT,
	})
	if err != nil {
		t.Fatalf("encoding a pid file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sockserve.PidName), data, 0o600); err != nil {
		t.Fatalf("writing a pid file: %v", err)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}
