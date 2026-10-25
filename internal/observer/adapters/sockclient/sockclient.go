package sockclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// DefaultFirstSnapshot is M11's bound: how long a freshly attached client
// waits for the baseline before giving up and letting its caller degrade.
//
// One second, from §7.5. P7's live fix guarantees a baseline precedes every
// delta even on a server whose first rebuild has not finished, so on a healthy
// server this never fires; it exists for the wedged one.
const DefaultFirstSnapshot = time.Second

// DefaultRefreshTimeout is §7.5(d)'s "spawnTimeout + 1s" with the production
// spawn bound of 5 s. A refresh that exceeds it is a DEGRADED ANSWER, never an
// error: the caller gets the current local snapshot plus a request-scoped
// refresh-rejected advisory.
const DefaultRefreshTimeout = 6 * time.Second

// DefaultSubscriberBuffer matches `tt observe`'s own per-subscriber queue
// depth, so a consumer sees the same backpressure behaviour whether it is
// attached to the loop directly or through a socket.
const DefaultSubscriberBuffer = 256

// maxFrameBytes matches sockserve.MaxFrameBytes: a client that could not read
// a frame the server is willing to send would resynchronise forever.
const maxFrameBytes = 1 << 20

// ErrNoServer means nothing is listening. It is a FACT about the machine, not
// a failure: nothing in tt ever auto-starts `tt observe` (§7.5), so "no server"
// is the ordinary case and every caller falls back to its own watch.
var ErrNoServer = errors.New("sockclient: no observer is listening")

// ErrNoSnapshot is M11's bound expiring: attached, but the server sent no
// snapshot within the bound.
var ErrNoSnapshot = errors.New("sockclient: the observer sent no snapshot within the bound")

// ErrClosed is returned by every method once the connection has ended, so a
// caller that held a client across a server restart learns it rather than
// being served the last state forever.
var ErrClosed = errors.New("sockclient: the connection to the observer has ended")

// SchemaMismatch is §7.5(e)'s refusal. It carries both version spaces because
// they are independent: the framing can change without the payload shape
// changing, and vice versa.
type SchemaMismatch struct {
	ServerSchema   int
	ServerProtocol int
	WantSchema     int
	WantProtocol   int
}

func (e *SchemaMismatch) Error() string {
	if e.ServerSchema != e.WantSchema {
		return fmt.Sprintf("server schema %d, expected %d", e.ServerSchema, e.WantSchema)
	}
	return fmt.Sprintf("server protocol %d, expected %d", e.ServerProtocol, e.WantProtocol)
}

// Config is what dialling needs.
type Config struct {
	// Dir is tt's state directory; the socket is SocketName inside it. Path
	// overrides it, which is what a test uses.
	Dir  string
	Path string
	// FirstSnapshot is M11's bound. Zero means DefaultFirstSnapshot.
	FirstSnapshot time.Duration
	// RefreshTimeout bounds one refresh round trip. Zero means
	// DefaultRefreshTimeout.
	RefreshTimeout time.Duration
	// SubscriberBuffer is the per-subscriber queue depth. Zero means
	// DefaultSubscriberBuffer.
	SubscriberBuffer int
	// Log receives the ONE line §7.5(e) requires on a refusal, and each
	// distinct unknown field seen on the wire. Nil discards.
	Log io.Writer
	// Clock is the CONSUMER's clock: every answer's ages and clock-dependent
	// liveness are derived from it at answer time (§7.5c, code review round 2
	// A7). Nil means observer.SystemClock{}.
	Clock observer.Clock
	// Policy is the liveness thresholds the answer-time derivation uses, as
	// the composition root resolved them (the retention window from the
	// vendor's settings). The corroboration threshold is taken from the
	// server's own agents-cli report when it carries one. A zero Policy means
	// observer.DefaultPolicy().
	Policy observer.Policy
}

// SocketPath is where a client looks, given a state directory. It is exported
// because three surfaces have to name it in a message before they dial.
func SocketPath(cfg Config) string {
	if cfg.Path != "" {
		return cfg.Path
	}
	if cfg.Dir == "" {
		return ""
	}
	return filepath.Join(cfg.Dir, sockserve.SocketName)
}

// Client is one attached consumer. It satisfies the incoming port and nothing
// wider.
type Client struct {
	cfg  Config
	conn net.Conn
	path string

	// hello's contents, immutable after the handshake.
	serverPID    int
	serverTT     string
	capabilities []string
	notCapable   []string

	mu     sync.Mutex
	local  *observer.Snapshot
	closed bool
	// awaitingBaseline is set by a resync: the local snapshot is discarded and
	// the next unsolicited snapshot frame becomes the new baseline (§7.5b).
	awaitingBaseline bool
	// standing is this connection's standing subscription as the client last
	// asked for it. Every subscribe frame carries ALL of it, because a
	// subscribe REPLACES the connection's filter and enrichment together: a
	// later subscribe that asked only for enrichment would otherwise silently
	// drop the dead population the handshake asked for, and vice versa.
	standing standingSubscription
	// degraded is set once the server's watch cannot push — a watch_degraded
	// frame, or a watch report that already says on-demand. From then on the
	// retained stream is not current by construction any more, and answers
	// are PULLED (code review round 2, A8).
	degraded    bool
	subs        map[int64]*subscriber
	nextSub     int64
	waiters     map[string]chan wire.Frame
	nextReq     int64
	unknown     []wire.Advisory
	seenUnknown map[string]bool

	done   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// releaseOnce guards the one path that releases resources — cancelling the
	// reader and closing the socket. "No longer usable" (closed, set under mu)
	// and "resources released" are separate states: code review round 2's A15
	// was a client whose reader saw EOF, set closed, and released nothing,
	// after which Close saw closed and returned early — so the descriptor was
	// never closed at all.
	releaseOnce sync.Once
}

var _ observer.SessionSource = (*Client)(nil)

// Dial attaches to a running observer, or reports why it did not.
//
// The three refusals are distinct values because the caller acts differently on
// each: ErrNoServer means "watch for yourself", *SchemaMismatch means "watch
// for yourself AND say which build is old", and ErrNoSnapshot means "the server
// is there but not answering", which is the one worth a message.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	path := SocketPath(cfg)
	if path == "" {
		return nil, fmt.Errorf("%w: no socket path", ErrNoServer)
	}

	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNoServer, path, err)
	}

	if cfg.Clock == nil {
		cfg.Clock = observer.SystemClock{}
	}
	c := &Client{
		cfg:         cfg,
		conn:        nc,
		path:        path,
		subs:        map[int64]*subscriber{},
		waiters:     map[string]chan wire.Frame{},
		seenUnknown: map[string]bool{},
		done:        make(chan struct{}),
	}

	rd := bufio.NewReaderSize(nc, 64*1024)
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 4096), maxFrameBytes)

	if err := c.handshake(sc); err != nil {
		_ = nc.Close()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.read(runCtx, sc)
	}()
	return c, nil
}

// handshake performs §7.5(b) and (e): read hello, check both version spaces,
// then wait — BOUNDED — for the first snapshot frame.
//
// The bound is a read deadline rather than a timer: it is armed by a read in
// flight and cleared by its completion, which is the same shape sockserve's
// write deadline has and the same four clauses of §9.3a's timer rule it
// satisfies. Nothing here is on a schedule.
func (c *Client) handshake(sc *bufio.Scanner) error {
	bound := c.cfg.FirstSnapshot
	if bound <= 0 {
		bound = DefaultFirstSnapshot
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(bound)); err != nil {
		return fmt.Errorf("sockclient: arming the handshake bound: %w", err)
	}
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	if !sc.Scan() {
		return fmt.Errorf("%w: no hello from %s", ErrNoServer, c.path)
	}
	hello, _, err := decodeLine(sc.Bytes())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoServer, err)
	}
	if hello.Type != wire.FrameHello {
		return fmt.Errorf("%w: the first frame was %q, not hello", ErrNoServer, hello.Type)
	}
	if hello.Schema != wire.Schema || hello.Protocol != wire.Protocol {
		mismatch := &SchemaMismatch{
			ServerSchema: hello.Schema, ServerProtocol: hello.Protocol,
			WantSchema: wire.Schema, WantProtocol: wire.Protocol,
		}
		c.logf("tt: not attaching to the observer at %s: %s", c.path, mismatch)
		return mismatch
	}
	c.serverPID = hello.PID
	c.serverTT = hello.TT
	c.capabilities = append([]string(nil), hello.Capabilities...)
	c.notCapable = append([]string(nil), hello.NotCapable...)
	// A server whose watch degraded before this client attached sent its
	// watch_degraded frame to somebody else; its own report says so here.
	c.degraded = wireSourcesOnDemand(hello.Sources)

	// Code review round 2, A10: the local state holds the WHOLE population —
	// dead sessions included — so an includeDead answer and an ENDED view can
	// be served from it. A server that declares the capability is asked for
	// it before the baseline is taken, and the baseline this client keeps is
	// the one that answers THAT request; the server's automatic default-view
	// baseline, if it came first, is superseded by it.
	//
	// The launched sessions (spawnedBy set) are asked for the same way and for
	// the same reason: `tt sessions --all` answered over the socket must be able
	// to list them, and a client whose local state the server had already
	// narrowed could not. A server without CapIncludeSpawned never hides them,
	// so its default baseline is already whole in that respect.
	wantReq := ""
	c.standing.includeDead = c.Capable(wire.CapIncludeDead)
	c.standing.includeSpawned = c.Capable(wire.CapIncludeSpawned)
	if c.standing.includeDead || c.standing.includeSpawned {
		wantReq = "baseline"
		if _, err := c.conn.Write(c.standing.line(&wantReq)); err != nil {
			return fmt.Errorf("%w: asking for the full population: %v", ErrNoServer, err)
		}
	}

	// §7.5(b): wait for the first snapshot frame and build the local state from
	// it. Anything else that arrives first is applied where it can be and
	// otherwise ignored — the baseline is what this loop is for.
	for sc.Scan() {
		f, notes, err := decodeLine(sc.Bytes())
		c.noteUnknown(notes)
		if err != nil {
			continue
		}
		if f.Type == wire.FrameWatchDegraded {
			c.degraded = true
			continue
		}
		if f.Type != wire.FrameSnapshot || f.Snapshot == nil {
			continue
		}
		if wantReq != "" && (f.ReqID == nil || *f.ReqID != wantReq) {
			continue
		}
		snap, _ := wire.ToDomain(*f.Snapshot)
		c.local = snap
		c.degraded = c.degraded || sourcesOnDemand(snap)
		return nil
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrNoSnapshot, err)
	}
	return ErrNoSnapshot
}

// Close ends the connection and every subscription on it. It releases the
// socket whether or not the server already went away, and is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	err := c.release()
	<-c.done
	c.wg.Wait()
	return err
}

// release cancels the reader and closes the socket, exactly once, whichever
// of EOF, a read error or Close gets there first. The first caller gets the
// close error; later ones get nil.
func (c *Client) release() error {
	var err error
	c.releaseOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		err = c.conn.Close()
	})
	return err
}

// ServerPID is the pid hello reported, for a message that says what this
// process attached to.
func (c *Client) ServerPID() int { return c.serverPID }

// ServerTT is the attached server's tt version. A server one build behind is
// legitimate as long as the schema matches, and a surface that wants to say so
// needs the string.
func (c *Client) ServerTT() string { return c.serverTT }

// SocketPath is what this client is attached to.
func (c *Client) SocketPath() string { return c.path }

// NotCapable is the declared list from hello. It is the same value the
// envelope carries (P7's decision), from the same source.
func (c *Client) NotCapable() []string { return append([]string(nil), c.notCapable...) }

// Capable reports whether the server declared a capability. §7.5(f): a server
// without "enrich" is treated as enrich:false and the client reports
// transcriptCollected as it arrived rather than guessing.
func (c *Client) Capable(cap string) bool {
	for _, have := range c.capabilities {
		if have == cap {
			return true
		}
	}
	return false
}

// UnknownFields is every wire-unknown-field advisory this connection has seen,
// deduplicated. It is what makes P0 finding 1's requirement observable: a
// schema addition arriving from a newer server is reported here rather than
// silently dropped.
func (c *Client) UnknownFields() []wire.Advisory {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.Advisory(nil), c.unknown...)
}

// ---------------------------------------------------------------------------
// the incoming port
// ---------------------------------------------------------------------------

// Snapshot answers from the local state, filtered by opts.
//
// It NEVER asks the server to rebuild while the server is pushing: the local
// state is current by construction, and a pull that triggered a rebuild would
// make every MCP call a poll with extra steps. The one round trip it may take
// is §7.5(f)'s one-off enriched read, and only when the caller asked for
// transcripts and the server declared it can produce them.
//
// The answer is derived AT ANSWER TIME from this process's clock (§7.5c, code
// review round 2 A7): the observations are the server's, and every age and
// every clock-sensitive liveness verdict is re-derived from them at `now` by
// observer.AsOf — the same function a render-time consumer uses — so a quiet
// machine cannot freeze a blocked duration or keep a stalled job "live". It
// used to state "the clock is not read", which is exactly how those froze.
// Liveness is re-derived BEFORE the default filter runs, so a session that has
// crossed a threshold is filtered on its current verdict.
func (c *Client) Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	base, err := c.latest()
	if err != nil {
		return nil, err
	}
	enrich := opts.IncludeTranscripts && c.Capable(wire.CapEnrich)
	switch {
	case c.isDegraded():
		// The server's watch cannot push, so the retained stream is the state
		// of the moment it stopped (A8). Pull: a degraded server answers a
		// snapshot request with a fresh, serialized read. A pull that fails
		// leaves the retained state as the answer — no worse than before, and
		// its sources already say the watch is on-demand.
		if fresh, err := c.requestSnapshot(ctx, enrich); err == nil && fresh != nil {
			base = fresh
		}
	case enrich:
		if enriched, err := c.requestSnapshot(ctx, true); err == nil && enriched != nil {
			base = enriched
		}
		// An enrichment round trip that fails is not an error on this surface:
		// the status answer is complete without a colour, exactly as it is when
		// the local Enrich degrades.
	}
	return narrow(c.asOf(base), opts), nil
}

func (c *Client) isDegraded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.degraded
}

// sourcesOnDemand reports whether a snapshot's watch reports say the server's
// watch has stopped pushing.
func sourcesOnDemand(snap *observer.Snapshot) bool {
	if snap == nil {
		return false
	}
	for _, src := range snap.Sources {
		if src.Watch != nil && src.Watch.Mode == observer.WatchModeOnDemand {
			return true
		}
	}
	return false
}

func wireSourcesOnDemand(sources map[string]wire.Source) bool {
	for _, src := range sources {
		if src.Watch != nil && src.Watch.Mode == observer.WatchModeOnDemand {
			return true
		}
	}
	return false
}

// asOf is observer.AsOf at this consumer's clock and policy.
func (c *Client) asOf(snap *observer.Snapshot) *observer.Snapshot {
	return observer.AsOf(snap, c.cfg.Clock.Now(), c.cfg.Policy)
}

// Enrich asks the server for §7.5(f)'s one-off enriched view and returns the
// sessions of `snap` as that view has them.
//
// It does NOT change the subscription's standing enrichment setting — that is
// the contract's point: a one-off enriched read must not silently make the
// whole stream expensive.
//
// A server with no "enrich" capability returns the snapshot unchanged, which is
// §7.5(f)'s "treated as enrich:false … rather than guessing".
func (c *Client) Enrich(ctx context.Context, snap *observer.Snapshot, ids []string) (*observer.Snapshot, error) {
	if snap == nil {
		return nil, errors.New("sockclient: Enrich on a nil snapshot")
	}
	if !c.Capable(wire.CapEnrich) {
		return snap, nil
	}
	enriched, err := c.requestSnapshot(ctx, true)
	if err != nil || enriched == nil {
		return snap, nil
	}

	// Keep the caller's session set: `snap` is what the caller's flags selected
	// and the enriched view is unfiltered, so returning the latter wholesale
	// would silently widen the answer.
	want := map[string]bool{}
	if len(ids) > 0 {
		for _, id := range ids {
			want[id] = true
		}
	} else {
		for i := range snap.Sessions {
			want[snap.Sessions[i].SessionID] = true
		}
	}
	out := *enriched
	kept := make([]observer.Session, 0, len(want))
	for i := range enriched.Sessions {
		if want[enriched.Sessions[i].SessionID] {
			kept = append(kept, enriched.Sessions[i])
		}
	}
	out.Sessions = kept
	// The caller's set was chosen above; View only recounts it, so it must
	// not re-apply the default liveness rule to a set that asked for the dead.
	return c.asOf(observer.View(&out, wholePopulation)), nil
}

// Refresh is the port's refresh: the round trip, then the answer from the
// updated local snapshot.
func (c *Client) Refresh(ctx context.Context, sources []string) (*observer.Snapshot, observer.RefreshOutcome, error) {
	outcome := c.RefreshSources(ctx, sources)
	snap, err := c.latest()
	if err != nil {
		return nil, outcome, err
	}
	return c.asOf(snap), outcome, nil
}

// RefreshSources is §7.5(d)'s round trip, and it NEVER fails the caller.
//
//	sources{reqId}            -> Refreshed, answer from the updated local snapshot
//	error{rate_limited}       -> Rejected{…: rate-limited} + a request-scoped advisory
//	error{unsupported}        -> Rejected{…: unsupported}
//	timeout                   -> Rejected{…: timeout}
//
// The three shapes are the ones observer.RefreshOutcome already has, which is
// why the local and via-socket paths produce identical answers to a caller that
// cannot tell which source it holds.
func (c *Client) RefreshSources(ctx context.Context, sources []string) observer.RefreshOutcome {
	want := sources
	if len(want) == 0 {
		want = []string{observer.SourceAgentsCLI}
	}
	out := observer.RefreshOutcome{Rejected: map[string]string{}}

	bound := c.cfg.RefreshTimeout
	if bound <= 0 {
		bound = DefaultRefreshTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	reply, err := c.request(ctx, wire.FrameRefresh, func(id string) map[string]any {
		return map[string]any{"type": wire.FrameRefresh, "reqId": id, "sources": want}
	})
	switch {
	case err != nil:
		return rejectAll(out, want, observer.RejectTimeout)
	case reply.Type == wire.FrameError:
		switch reply.Code {
		case wire.CodeRateLimited:
			return rejectAll(out, want, observer.RejectRateLimited)
		case wire.CodeUnsupported:
			return rejectAll(out, want, observer.RejectUnsupported)
		default:
			return rejectAll(out, want, observer.RejectError)
		}
	default:
		out.Refreshed = append(out.Refreshed, want...)
		return out
	}
}

// rejectAll marks every requested source rejected for one reason and attaches
// §4.3(a)'s request-scoped advisory. Request-scoped is load-bearing: one
// caller's rejected refresh is not a fact about the observer, and merging it
// into the shared snapshot would leak it to unrelated subscribers.
func rejectAll(out observer.RefreshOutcome, want []string, reason string) observer.RefreshOutcome {
	for _, id := range want {
		out.Rejected[id] = reason
		out.Advisories = append(out.Advisories, observer.NewAdvisory(observer.AdvRefreshRejected, map[string]any{
			"source": id,
			"reason": reason,
		}))
	}
	return out
}

// Subscribe registers a local subscriber.
//
// The client subscribes to the SERVER without a filter (§7.5b) and filters
// here, through the same observer.View and observer.Diff the loop uses — so
// membership is client-relative by the one predicate rather than by a second
// implementation, and H13's client-relative delta translation is never
// exercised on this path.
func (c *Client) Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	buffer := c.cfg.SubscriberBuffer
	if buffer <= 0 {
		buffer = DefaultSubscriberBuffer
	}

	s := &subscriber{ch: make(chan observer.Event, buffer), filter: f}

	// Registration and the baseline are ONE critical section, the same one
	// publish replaces the local snapshot and fans out in (code review round
	// 2, A5). The baseline used to be delivered after the mutex was released,
	// so a delta folded in between could be delivered to this subscriber
	// BEFORE its older baseline — and a consumer applying both in order ended
	// holding the old state, with nothing further coming to correct it.
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.nextSub++
	id := c.nextSub
	c.subs[id] = s
	if view := observer.View(c.local, f); view != nil {
		s.deliver([]observer.Event{{
			Type:     observer.EventSnapshot,
			Seq:      view.Seq,
			Snapshot: view,
			At:       view.ObservedAt,
		}}, view)
	}
	c.mu.Unlock()

	// The cleanup ends on EITHER the caller's context or the client's own
	// shutdown (A15). Waiting on the caller alone kept a dead client — and its
	// socket — reachable for as long as a pane lived.
	go func() {
		select {
		case <-ctx.Done():
		case <-c.done:
		}
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
		s.close()
	}()
	return s.ch, nil
}

// ---------------------------------------------------------------------------
// the reader
// ---------------------------------------------------------------------------

// read owns the socket for reading and owns nothing else. Every state change
// goes through apply, under the mutex, so there is one writer of the local
// snapshot.
func (c *Client) read(ctx context.Context, sc *bufio.Scanner) {
	defer close(c.done)
	defer c.shutdown()

	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		f, notes, err := decodeLine(line)
		c.noteUnknown(notes)
		if err != nil {
			// Degrade silently on unknown data: a malformed line from a server
			// one build ahead is not a reason to drop a connection that is
			// otherwise serving 39 sessions' worth of state.
			continue
		}
		if id := reqIDOf(f); id != "" && c.hasWaiter(id) {
			// A correlated sources reply describes the server's CURRENT source
			// health, and the caller that asked will answer from the local
			// snapshot right after — so it is applied first (code review round
			// 2, A12). It used to be handed to the waiter and skipped here, and
			// a refresh that cleared a failure left the failure in place.
			if f.Type == wire.FrameSources {
				c.apply(f)
			}
			if c.deliverReply(id, f) {
				continue
			}
		}
		c.apply(f)
	}
}

// shutdown closes every subscription and marks the client done, so a consumer
// blocked on a subscription learns the server has gone rather than waiting
// forever. A run-forever server can still be killed at any time.
func (c *Client) shutdown() {
	// Resources first: EOF and a read error release the socket exactly as
	// Close does (A15). Marking the client closed is a separate statement
	// about usability.
	_ = c.release()

	c.mu.Lock()
	c.closed = true
	subs := make([]*subscriber, 0, len(c.subs))
	for id, s := range c.subs {
		subs = append(subs, s)
		delete(c.subs, id)
	}
	waiters := make([]chan wire.Frame, 0, len(c.waiters))
	for id, ch := range c.waiters {
		waiters = append(waiters, ch)
		delete(c.waiters, id)
	}
	c.mu.Unlock()

	for _, s := range subs {
		s.close()
	}
	for _, ch := range waiters {
		close(ch)
	}
}

// apply folds one unsolicited frame into the local state and publishes the
// deltas that follow from it.
func (c *Client) apply(f wire.Frame) {
	switch f.Type {
	case wire.FrameSnapshot:
		if f.Snapshot == nil {
			return
		}
		snap, _ := wire.ToDomain(*f.Snapshot)
		c.publish(snap, nil)

	case wire.FrameSessionAdded, wire.FrameSessionChanged, wire.FrameSessionRemoved:
		c.mu.Lock()
		prev := c.local
		awaiting := c.awaitingBaseline
		c.mu.Unlock()
		if prev == nil || awaiting {
			// A delta with no baseline means nothing (§7.5b): the baseline is
			// what state is built from, and the server sends one before any
			// delta.
			return
		}
		next := copySnapshot(prev)
		if _, err := wire.ApplySessionFrame(next, f); err != nil {
			return
		}
		if at := frameInstant(f); !at.IsZero() {
			next.ObservedAt = at
		}
		// View only to recount: the local state is the WHOLE population the
		// server sends (dead sessions included when asked, A10), and each
		// subscriber and each answer narrows it for itself.
		c.publish(observer.View(next, wholePopulation), nil)

	case wire.FrameSources:
		if f.Sources == nil {
			return
		}
		c.mu.Lock()
		prev := c.local
		c.mu.Unlock()
		if prev == nil {
			return
		}
		next := copySnapshot(prev)
		next.Sources = toDomainSources(f.Sources)
		c.publish(next, &observer.Event{Type: observer.EventSources, Reason: f.Cause})

	case wire.FrameResync:
		// §7.5(b): discard and await a fresh snapshot. The resync is forwarded
		// so a consumer knows its deltas had a gap.
		c.mu.Lock()
		c.awaitingBaseline = true
		snap := c.local
		c.mu.Unlock()
		c.emit(observer.Event{
			Type:     observer.EventResync,
			Reason:   f.Reason,
			Snapshot: snap,
			At:       instantOf(snap),
		})

	case wire.FrameWatchDegraded:
		c.mu.Lock()
		c.degraded = true
		c.mu.Unlock()
		c.emit(observer.Event{Type: observer.EventWatchDegraded, Reason: f.Error})
	}
}

// publish replaces the local snapshot and delivers each subscriber its own
// deltas. A snapshot whose watch report says on-demand marks the client
// degraded (A8). extra is an envelope-level event (a sources change) that has no
// session deltas of its own.
//
// The replacement and the fan-out are one critical section with Subscribe's
// registration and baseline (A5): a subscriber is either registered before
// this publication and offered it, or registered after and handed it as its
// baseline. Delivery is a non-blocking send under the subscriber's own lock,
// so holding c.mu across it costs microseconds.
func (c *Client) publish(next *observer.Snapshot, extra *observer.Event) {
	if next == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.local
	c.local = next
	c.awaitingBaseline = false
	c.degraded = c.degraded || sourcesOnDemand(next)

	for _, s := range c.subs {
		view := observer.View(next, s.filter)
		if extra != nil {
			ev := *extra
			ev.Snapshot = view
			ev.At = next.ObservedAt
			s.deliver([]observer.Event{ev}, view)
			continue
		}
		if prev == nil {
			s.deliver([]observer.Event{{
				Type:     observer.EventSnapshot,
				Seq:      next.Seq,
				Snapshot: view,
				At:       next.ObservedAt,
			}}, view)
			continue
		}
		s.deliver(observer.Diff(prev, next, s.filter), view)
	}
}

// emit delivers one event that carries no state change, under the same lock
// as publish so the two cannot reorder for any subscriber.
func (c *Client) emit(ev observer.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		s.deliver([]observer.Event{ev}, ev.Snapshot)
	}
}

// ---------------------------------------------------------------------------
// request/reply
// ---------------------------------------------------------------------------

// request writes one client frame carrying a fresh reqId and waits for the
// frame that echoes it.
func (c *Client) request(ctx context.Context, kind string, build func(id string) map[string]any) (wire.Frame, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return wire.Frame{}, ErrClosed
	}
	c.nextReq++
	id := "r-" + strconv.FormatInt(c.nextReq, 10)
	ch := make(chan wire.Frame, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()

	line, err := json.Marshal(build(id))
	if err != nil {
		return wire.Frame{}, fmt.Errorf("sockclient: encoding a %s request: %w", kind, err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return wire.Frame{}, fmt.Errorf("sockclient: sending a %s request: %w", kind, err)
	}

	select {
	case f, ok := <-ch:
		if !ok {
			return wire.Frame{}, ErrClosed
		}
		return f, nil
	case <-ctx.Done():
		return wire.Frame{}, ctx.Err()
	}
}

// requestSnapshot is §7.5(f)'s one-off read.
func (c *Client) requestSnapshot(ctx context.Context, enrich bool) (*observer.Snapshot, error) {
	f, err := c.request(ctx, wire.FrameSnapshot, func(id string) map[string]any {
		return map[string]any{"type": wire.FrameSnapshot, "reqId": id, "enrich": enrich}
	})
	if err != nil {
		return nil, err
	}
	if f.Type != wire.FrameSnapshot || f.Snapshot == nil {
		return nil, fmt.Errorf("sockclient: the observer answered a snapshot request with %q", f.Type)
	}
	snap, _ := wire.ToDomain(*f.Snapshot)
	return snap, nil
}

// hasWaiter reports whether a request with this id is waiting for its reply.
func (c *Client) hasWaiter(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.waiters[id]
	return ok
}

// deliverReply routes a frame that echoes a reqId to whoever is waiting for it.
// It reports whether it was consumed: an unmatched reqId is an unsolicited
// frame as far as the local state is concerned.
func (c *Client) deliverReply(id string, f wire.Frame) bool {
	c.mu.Lock()
	ch, ok := c.waiters[id]
	if ok {
		delete(c.waiters, id)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	ch <- f
	close(ch)
	return true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// latest returns the local snapshot, or says why there is none.
func (c *Client) latest() (*observer.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.local == nil {
		if c.closed {
			return nil, ErrClosed
		}
		return nil, ErrNoSnapshot
	}
	if c.closed {
		return nil, ErrClosed
	}
	return c.local, nil
}

// narrow applies SnapshotOptions to an already-built snapshot.
//
// It uses the SAME two predicates the core uses — IncludedByDefault for the
// liveness rule and Visible for the filter — and then lets observer.View
// recount, so the counts in a via-socket answer are produced by the same code
// that produced the server's. A second counting loop here is exactly how an
// envelope ends up describing 39 sessions above an array of three.
func narrow(snap *observer.Snapshot, opts observer.SnapshotOptions) *observer.Snapshot {
	if snap == nil {
		return nil
	}
	out := *snap
	kept := make([]observer.Session, 0, len(snap.Sessions))
	for i := range snap.Sessions {
		s := snap.Sessions[i]
		if !observer.IncludedByDefault(s.Live, opts) {
			continue
		}
		kept = append(kept, s)
	}
	out.Sessions = kept
	// The liveness rule was applied just above, with the caller's options;
	// View narrows by the caller's filter and recounts, and must not apply the
	// DEFAULT liveness rule a second time over a set that asked for the dead
	// (code review round 2, A10).
	f := opts.Filter
	f.IncludeDead = true
	return observer.View(&out, f)
}

// wholePopulation is the Filter for a View whose only job is to RECOUNT a set
// that has already been chosen — the local state, or a caller's set after
// enrichment. It keeps everything the default view would drop (the dead, A10,
// and the launched, spawnedBy) so that recounting never quietly re-narrows.
var wholePopulation = observer.Filter{IncludeDead: true, IncludeSpawned: true}

// copySnapshot copies the parts ApplySessionFrame mutates. The sessions slice
// is copied because a published snapshot is immutable (§7.3) and a subscriber
// may still be encoding the previous one.
func copySnapshot(s *observer.Snapshot) *observer.Snapshot {
	out := *s
	out.Sessions = append([]observer.Session(nil), s.Sessions...)
	return &out
}

// toDomainSources converts a sources frame's payload. It goes through
// wire.ToDomain rather than field by field, for H7's reason: one inverse.
func toDomainSources(in map[string]wire.Source) map[string]observer.SourceReport {
	env, _ := wire.ToDomain(wire.Snapshot{Sources: in})
	return env.Sources
}

// frameInstant is the instant a session delta describes — the session's own
// observedAt, which every delta of one rebuild shares with that rebuild's
// snapshot. It is READ FROM THE FRAME, never from a local clock.
func frameInstant(f wire.Frame) time.Time {
	if f.Session == nil {
		return time.Time{}
	}
	t, err := observer.ParseTime(f.Session.ObservedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

func instantOf(s *observer.Snapshot) time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.ObservedAt
}

// reqIDOf is the echoed request id, or "".
func reqIDOf(f wire.Frame) string {
	if f.ReqID == nil {
		return ""
	}
	return *f.ReqID
}

// decodeLine turns one NDJSON line into a frame.
//
// P0 finding 1 is why this function exists rather than a bare json.Unmarshal:
// an unknown field can only be reported where BYTES become a struct. The frame
// itself goes through wire.DecodeFrame, and a snapshot frame's NESTED envelope
// additionally goes through wire.DecodeSnapshot — so the envelope this client
// holds is the one DecodeSnapshot produced, and a vendor-driven addition inside
// it is named with the same `sessions[].x` spelling `tt sessions --json` would
// give it.
func decodeLine(line []byte) (wire.Frame, []wire.Advisory, error) {
	f, notes, err := wire.DecodeFrame(line)
	if err != nil {
		return wire.Frame{}, notes, err
	}
	if f.Type != wire.FrameSnapshot {
		return f, notes, nil
	}
	raw := nestedSnapshot(line)
	if raw == nil {
		return f, notes, nil
	}
	env, envNotes, err := wire.DecodeSnapshot(raw)
	if err != nil {
		return f, notes, nil
	}
	f.Snapshot = &env
	return f, mergeNotes(notes, envNotes), nil
}

// nestedSnapshot is the raw bytes of a snapshot frame's payload.
func nestedSnapshot(line []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	payload, ok := raw["snapshot"]
	if !ok || string(bytes.TrimSpace(payload)) == "null" {
		return nil
	}
	return payload
}

// mergeNotes concatenates two advisory lists without repeating a (code, field)
// pair, so the frame-level and envelope-level passes over the same bytes report
// each addition once.
func mergeNotes(a, b []wire.Advisory) []wire.Advisory {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]wire.Advisory, 0, len(a)+len(b))
	for _, adv := range append(append([]wire.Advisory(nil), a...), b...) {
		key := noteKey(adv)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, adv)
	}
	return out
}

func noteKey(a wire.Advisory) string {
	field, _ := a.Fields["field"].(string)
	return a.Code + "\x00" + field
}

// noteUnknown records each distinct unknown field once and logs it once.
func (c *Client) noteUnknown(notes []wire.Advisory) {
	if len(notes) == 0 {
		return
	}
	c.mu.Lock()
	fresh := make([]wire.Advisory, 0, len(notes))
	for _, a := range notes {
		key := noteKey(a)
		if c.seenUnknown[key] {
			continue
		}
		c.seenUnknown[key] = true
		c.unknown = append(c.unknown, a)
		fresh = append(fresh, a)
	}
	c.mu.Unlock()

	for _, a := range fresh {
		field, _ := a.Fields["field"].(string)
		c.logf("tt: the observer sent a field this build does not know: %s (%s)", field, a.Code)
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.cfg.Log == nil {
		return
	}
	fmt.Fprintf(c.cfg.Log, format+"\n", args...)
}

// ---------------------------------------------------------------------------
// subscribers
// ---------------------------------------------------------------------------

// subscriber is one local consumer's bounded queue. It is the same shape the
// watch loop's is, for the same reason: a consumer that stops reading costs
// itself its deltas and gets a resync, and never delays the reader.
type subscriber struct {
	ch     chan observer.Event
	filter observer.Filter

	mu     sync.Mutex
	closed bool
	resync bool
}

// deliver queues events without ever blocking. A full queue is REPLACED by one
// resync carrying snap (code review round 2, A13): the flag-and-wait version
// sent the resync only on a later delivery, so a consumer that caught up on a
// quiet machine drained stale deltas and never learned what it had missed.
func (s *subscriber) deliver(events []observer.Event, snap *observer.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.resync {
		s.overflowLocked(snap)
		if s.resync {
			return
		}
	}
	for _, e := range events {
		select {
		case s.ch <- e:
		default:
			s.overflowLocked(snap)
			return
		}
	}
}

// overflowLocked drains everything queued and enqueues one resync of snap. The
// subscriber is the channel's only sender, so after the drain there is room.
// A queued watch-degraded notice cannot be rebuilt from a snapshot, so it is
// re-queued after the resync when there is room for it.
func (s *subscriber) overflowLocked(snap *observer.Snapshot) {
	var keep []observer.Event
drain:
	for {
		select {
		case e := <-s.ch:
			if e.Type == observer.EventWatchDegraded {
				keep = append(keep, e)
			}
		default:
			break drain
		}
	}
	s.resync = true
	if snap != nil {
		select {
		case s.ch <- observer.Event{
			Type:     observer.EventResync,
			Reason:   wire.ReasonSlowConsumer,
			Snapshot: snap,
			At:       snap.ObservedAt,
		}:
			s.resync = false
		default:
		}
	}
	for _, e := range keep {
		select {
		case s.ch <- e:
		default:
		}
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// standingSubscription is what this connection's subscribe frames ask for.
type standingSubscription struct {
	includeDead    bool
	includeSpawned bool
	enrich         bool
}

// line encodes the subscribe frame for the standing subscription, with an
// optional reqId the server echoes on the baseline it answers with.
func (s standingSubscription) line(reqID *string) []byte {
	frame := map[string]any{"type": wire.FrameSubscribe, "enrich": s.enrich}
	filter := map[string]any{}
	if s.includeDead {
		filter["includeDead"] = true
	}
	if s.includeSpawned {
		filter["includeSpawned"] = true
	}
	if len(filter) > 0 {
		frame["filter"] = filter
	}
	if reqID != nil {
		frame["reqId"] = *reqID
	}
	line, _ := json.Marshal(frame)
	return append(line, '\n')
}
