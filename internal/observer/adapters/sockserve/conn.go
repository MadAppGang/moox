package sockserve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// One connection is TWO goroutines, and the split is the whole concurrency
// design:
//
//	read()  parses lines and posts requests. It touches no connection state.
//	serve() owns EVERYTHING else — the subscription, the filter, the enrich
//	        setting, the sequence number and the socket writes.
//
// So there is exactly one writer per connection and exactly one owner of each
// mutable field, which is what makes `seq` monotonic without a lock and makes
// "a re-subscribe replaces the stream" an ordinary assignment rather than a
// handshake. Work that can take a while — enrichment for a one-off snapshot,
// and a refresh that may spawn — runs on its own goroutine and posts a
// finished frame back to serve() through replies, so a slow refresh cannot
// stall the delta stream and a stalled delta stream cannot delay a refresh.
//
// Nothing here can block the rebuild loop. The only channel serve() can be
// slow to drain is its own subscription, and observer.WatchLoop answers a full
// subscriber queue with a resync rather than by waiting (§7.2).

// requestBuffer and replyBuffer are small on purpose: they exist to decouple
// two goroutines, not to accumulate work. A client that pipelines more
// requests than this blocks itself in the kernel, which is the correct place
// for its own backlog to sit.
const (
	requestBuffer = 8
	replyBuffer   = 8
)

// request is one parsed client line, or the reason it could not be parsed.
type request struct {
	frame wire.ClientFrame
	// err is non-nil for a malformed line. The connection SURVIVES it: one bad
	// line is not a reason to drop a subscriber that has 39 sessions' worth of
	// state, and a client that gets error{bad_request} back can fix itself.
	err error
	// fatal marks a line the framing cannot recover from — a line over
	// MaxFrameBytes leaves the reader mid-frame with no way to resynchronise,
	// so that one is answered and then closed.
	fatal bool
}

// conn is one attached consumer.
type conn struct {
	id  int64
	srv *Server
	nc  net.Conn
	enc *json.Encoder

	reqs    chan request
	replies chan wire.Frame

	// ctx and cancel are the connection's lifetime. Both are set by newConn,
	// which admit calls under s.mu BEFORE the conn is published into s.conns,
	// and neither is written again — so stop(), which stopConns reaches from
	// the Serve goroutine, reads a value whose write happens-before the read.
	// Code review MEDIUM 6: they used to be created inside serve(), on the
	// connection goroutine, and a shutdown landing between admit and serve's
	// first statement read c.cancel concurrently with its write.
	ctx    context.Context
	cancel context.CancelFunc

	// Owned by serve(), read by nobody else.
	seq    int64
	filter observer.Filter
	enrich bool
	events <-chan observer.Event
	// baseline is whether this subscription has sent a snapshot frame yet, and
	// baselineAt is the instant of the state that baseline described. Both are
	// reset by every (re)subscribe, and together they are the fix for the
	// defect P7's LIVE RUN found — see onEvent.
	baseline     bool
	baselineAt   time.Time
	subCancel    context.CancelFunc
	pendingReqID *string
	// inflight counts worker goroutines (a one-off snapshot, a refresh) that
	// owe this connection a reply. It is what lets a client that closes its
	// write half still be answered before the connection is released (A6).
	inflight int
}

// newConn builds a connection whose lifetime is a child of parent (the
// server's). The context is derived HERE, not in serve, for the reason on the
// ctx field.
func newConn(parent context.Context, id int64, srv *Server, nc net.Conn) *conn {
	ctx, cancel := context.WithCancel(parent)
	return &conn{
		id:      id,
		srv:     srv,
		nc:      nc,
		enc:     json.NewEncoder(nc),
		reqs:    make(chan request, requestBuffer),
		replies: make(chan wire.Frame, replyBuffer),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// stop asks the connection to say goodbye and close. It is safe from any
// goroutine and is how the server's shutdown reaches every client.
func (c *conn) stop() {
	c.cancel()
}

// serve runs the connection until its context is cancelled or the socket
// fails.
func (c *conn) serve() {
	ctx := c.ctx
	defer c.cancel()
	defer func() { _ = c.nc.Close() }()

	if n := c.srv.cfg.WriteBufferBytes; n > 0 {
		if u, ok := c.nc.(*net.UnixConn); ok {
			// Bounds how much a client that has stopped reading can hold in
			// kernel memory. Left at the OS default in production.
			_ = u.SetWriteBuffer(n)
		}
	}

	latest := c.srv.cfg.Deps.Events.Latest()
	hello := wire.NewHello(
		wire.Options{TT: c.srv.cfg.TT},
		c.srv.cfg.PID,
		c.srv.capabilities(),
		c.notCapable(latest),
		sourcesOf(latest),
	)
	if err := c.write(hello); err != nil {
		return
	}

	// Subscribe with NO filter and no enrichment, before reading anything from
	// the client. §5.3's client vocabulary is entirely optional: a consumer
	// that connects and never writes gets the full stream, because "should not
	// poll" must not be replaced by "must implement a handshake".
	if err := c.resubscribe(ctx, observer.Filter{}, false, nil); err != nil {
		return
	}

	// The reader's exit is an EVENT for this loop, not something it finds out
	// about on its next write (code review round 2, A6). It used to return on
	// EOF without a word, and this loop stayed subscribed and blocked in its
	// select: on an idle machine nothing is ever written, so no write deadline
	// could notice either, and every closed client kept its admission slot, its
	// subscription and two goroutines until some unrelated event. 128 such
	// clients locked out every new one.
	//
	// The lifecycle this defines: EOF on the read side — a full close, or a
	// client that shut down its write half after sending its requests — ends
	// the connection once the replies it is still owed have been written. A
	// client that half-closes gets its answers; it does not get a stream that
	// outlives its own interest in talking.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		c.read(ctx)
	}()
	draining := false

	for {
		select {
		case <-ctx.Done():
			c.farewell()
			return

		case <-readDone:
			readDone = nil
			if c.inflight == 0 {
				return
			}
			draining = true

		case ev, ok := <-c.events:
			if !ok {
				// The subscription ended (our own re-subscribe, or the loop
				// shutting down). A nil channel blocks in select forever,
				// which is the right thing: this connection keeps answering
				// requests rather than spinning on a closed channel.
				c.events = nil
				continue
			}
			if err := c.onEvent(ctx, ev); err != nil {
				return
			}

		case r := <-c.reqs:
			if err := c.onRequest(ctx, r); err != nil {
				return
			}

		case f := <-c.replies:
			c.inflight--
			if err := c.write(f); err != nil {
				return
			}
			if draining && c.inflight == 0 {
				return
			}
		}
	}
}

// notCapable is the declared list. It is taken from the published snapshot
// when there is one, so hello and every snapshot envelope report the SAME
// value from the same source, and falls back to the server's configured list
// before the first publication.
func (c *conn) notCapable(snap *observer.Snapshot) []string {
	if snap != nil && snap.NotCapable != nil {
		return snap.NotCapable
	}
	return c.srv.cfg.NotCapable
}

// farewell sends bye on the way out. Best effort and bounded by the write
// deadline: a client that cannot be written to cannot be told goodbye, and a
// client must handle a bare EOF anyway because a run-forever server can be
// killed at any time.
func (c *conn) farewell() {
	_ = c.write(wire.NewBye(wire.ReasonShutdown))
}

// read parses lines. It is the only goroutine that touches the socket for
// reading and it touches no connection state at all.
func (c *conn) read(ctx context.Context) {
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 0, 4096), MaxFrameBytes)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		f, err := wire.DecodeClientFrame(line)
		if !c.post(ctx, request{frame: f, err: err}) {
			return
		}
	}
	if err := sc.Err(); err != nil && errors.Is(err, bufio.ErrTooLong) {
		c.post(ctx, request{
			err:   fmt.Errorf("%w: a frame exceeded %d bytes", wire.ErrBadRequest, MaxFrameBytes),
			fatal: true,
		})
	}
}

// post hands a request to serve(), or gives up if the connection is going
// away. The select is what stops the reader leaking on a connection whose
// handler has already returned.
func (c *conn) post(ctx context.Context, r request) bool {
	select {
	case c.reqs <- r:
		return true
	case <-ctx.Done():
		return false
	}
}

// reply hands a finished frame to serve() from a worker goroutine.
func (c *conn) reply(ctx context.Context, f wire.Frame) {
	select {
	case c.replies <- f:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

// onEvent turns one client-relative delta into frames.
//
// The deltas are already client-relative: each connection subscribes with its
// OWN filter, so observer.Diff emits session_added when a session enters this
// connection's view for any reason (including a status change that now
// matches), session_removed with reason "filtered-out" when it leaves, and
// session_changed only for sessions in the view at both ends (H13). Nothing
// here re-filters, which is what keeps one predicate — observer.Visible — in
// charge of both the baseline and the deltas.
func (c *conn) onEvent(ctx context.Context, ev observer.Event) error {
	// A DELTA THE CLIENT HAS NO BASELINE FOR IS REPLACED BY THE BASELINE.
	//
	// FOUND BY THE LIVE RUN, and by nothing a fixture could have caught: every
	// test in this package published before it dialled. On the real machine
	// the first rebuild can take seconds — `claude agents --json` was measured
	// at over a MINUTE while this was being written, and the startup publish
	// waits on the corroboration spawn's 5 s bound — so a client that connects
	// inside that window subscribes to a loop with NOTHING published. The loop
	// then has no baseline to deliver, and the first publish arrives as
	// Diff(nil, snap): 37 session_added frames and never a snapshot.
	//
	// That breaks §7.5(b) — a client waits for the first snapshot frame and
	// builds its local state through wire.ToDomain — and M11's 1 s bound would
	// make P8 abandon a perfectly healthy server over it. So the first delta of
	// a baseline-less subscription is answered with the CURRENT state, and the
	// rest of that publication's deltas are dropped: the baseline is the
	// published state, which already contains everything a delta computed
	// against an earlier one would have said.
	//
	// "The rest of that publication" is identified by the event's instant,
	// which every delta of one rebuild shares with that rebuild's snapshot.
	// The one imprecision is two publications inside the same millisecond —
	// wire timestamps are millisecond-precision by §5.1 — which needs two
	// synchronous rebuilds back to back (a refresh racing a watch fire) inside
	// the window before a client's first baseline. The cost then is one
	// dropped delta batch for that one client, whose state is still correct as
	// of its baseline.
	//
	// SINCE THE BLACK-BOX FIX this path is defence in depth rather than the
	// mechanism: it could not help when the first publication had NO delta to
	// convert (an empty session set — hello, then silence, forever), so the
	// loop itself now makes every subscriber's first event a snapshot. It stays
	// because a Events implementation other than the loop owes this surface
	// nothing about ordering.
	if isDelta(ev.Type) {
		if !c.baseline {
			return c.writeSnapshot(ctx, c.srv.cfg.Deps.Events.Latest(), c.takePending())
		}
		if !ev.At.After(c.baselineAt) {
			return nil
		}
	}

	switch ev.Type {
	case observer.EventSnapshot:
		return c.writeSnapshot(ctx, ev.Snapshot, c.takePending())

	case observer.EventResync:
		// The queue for this connection overflowed: deltas were dropped. The
		// resync says so and the snapshot that follows is the new baseline —
		// which is both smaller than an unbounded queue and the only honest
		// thing to send a client that has missed deltas.
		reason := ev.Reason
		if reason == "" {
			reason = wire.ReasonSlowConsumer
		}
		if err := c.write(wire.NewResync(reason)); err != nil {
			return err
		}
		return c.writeSnapshot(ctx, ev.Snapshot, nil)

	case observer.EventSessionAdded:
		if ev.Session == nil {
			return nil
		}
		return c.write(wire.NewSessionAdded(wire.NewSession(c.enriched(ctx, *ev.Session, ev.At))))

	case observer.EventSessionChanged:
		if ev.Session == nil {
			return nil
		}
		s := wire.NewSession(c.enriched(ctx, *ev.Session, ev.At))
		return c.write(wire.NewSessionChanged(s, ev.Changed))

	case observer.EventSessionRemoved:
		return c.write(wire.NewSessionRemoved(ev.SessionID, string(ev.Kind), ev.Reason))

	case observer.EventSources:
		return c.write(wire.NewSourcesFrame(nil, sourcesOf(ev.Snapshot), wire.CausePiggyback))

	case observer.EventWatchDegraded:
		return c.write(wire.NewWatchDegraded("", wire.WatchDegradedOnDemand, ev.Reason))

	default:
		// Degrade silently on unknown data: a core that grows an event type
		// this build does not know must not kill the connection.
		return nil
	}
}

// isDelta reports whether an event is a per-session delta, i.e. one that only
// means something to a client that already holds a baseline.
func isDelta(t observer.EventType) bool {
	switch t {
	case observer.EventSessionAdded, observer.EventSessionChanged, observer.EventSessionRemoved:
		return true
	default:
		return false
	}
}

// takePending returns and clears the reqId a subscribe asked to have echoed on
// its next baseline snapshot.
func (c *conn) takePending() *string {
	id := c.pendingReqID
	c.pendingReqID = nil
	return id
}

// writeSnapshot sends one baseline.
func (c *conn) writeSnapshot(ctx context.Context, snap *observer.Snapshot, reqID *string) error {
	view := c.viewOf(ctx, snap, c.filter, c.enrich)
	if view == nil {
		return nil
	}
	if err := c.write(wire.NewSnapshotFrame(reqID, view.encode(c.srv.cfg.TT))); err != nil {
		return err
	}
	c.baseline, c.baselineAt = true, view.snap.ObservedAt
	return nil
}

// snapshotView is a filtered, optionally enriched snapshot on its way to the
// wire. It exists as a named type only so that the one encoding call is in one
// place: every surface must pass the SNAPSHOT'S OWN instant as `now`, never a
// fresh clock read, or §9.2(c)'s identity (statusAgeMs == observedAt −
// statusSince) becomes false by however long the read took — a defect that
// passes every frozen-clock fixture and fails only in production.
type snapshotView struct{ snap *observer.Snapshot }

func (v snapshotView) encode(tt string) wire.Snapshot {
	return wire.NewSnapshot(v.snap, v.snap.ObservedAt, wire.Options{TT: tt})
}

// viewOf projects a published snapshot onto one connection and enriches it if
// that connection asked for enrichment.
//
// Enrichment goes through the core's Enrich, over the sessions in THIS
// connection's filtered view, which is what §7.5(f) specifies. Filtering first
// and enriching second is not an optimisation: enriching first would read up
// to 200 KB per transcript for sessions this connection will never be shown.
func (c *conn) viewOf(ctx context.Context, snap *observer.Snapshot, f observer.Filter, enrich bool) *snapshotView {
	view := observer.View(snap, f)
	if view == nil {
		return nil
	}
	if enrich {
		if e := c.srv.cfg.Deps.Enrich; e != nil {
			if out, err := e.Enrich(ctx, view, nil); err == nil && out != nil {
				view = out
			}
			// An enrichment failure is not an error on this surface: Enrich
			// degrades per session and reports what happened in
			// sources.transcripts, and the status answer is complete without
			// a colour.
		}
	}
	return &snapshotView{snap: view}
}

// enriched returns one session with its transcript facts filled in, for a
// connection that asked for enrichment.
//
// M9 says to re-enrich "whenever it appears in changed[] with any
// transcript-relevant field, or whenever it is newly added". This enriches on
// EVERY delta for an enrich:true connection, which is a deliberate divergence
// with two reasons:
//
//  1. The conditional is not free of consequence — it is wrong. A
//     session_changed on a non-transcript field (a status flip, say) would
//     carry transcriptCollected:false for a session the client was already
//     told about with facts, and a consumer cannot distinguish "we did not
//     look this time" from "the transcript is gone".
//  2. The conditional is redundant: §7.3's cache is keyed on
//     (sessionId, path, size, mtime), so a session whose transcript has not
//     grown is answered from the cache without a read. The condition M9
//     describes is exactly the cache's miss condition, expressed twice.
func (c *conn) enriched(ctx context.Context, s observer.Session, at time.Time) observer.Session {
	if !c.enrich {
		return s
	}
	e := c.srv.cfg.Deps.Enrich
	if e == nil {
		return s
	}
	one := &observer.Snapshot{ObservedAt: at, Sessions: []observer.Session{s}}
	out, err := e.Enrich(ctx, one, nil)
	if err != nil || out == nil || len(out.Sessions) != 1 {
		return s
	}
	return out.Sessions[0]
}

// ---------------------------------------------------------------------------
// requests
// ---------------------------------------------------------------------------

// onRequest handles one client frame. A returned error closes the connection;
// everything a client can get wrong is answered instead.
func (c *conn) onRequest(ctx context.Context, r request) error {
	if r.err != nil {
		if err := c.write(wire.NewError(r.frame.ReqID, wire.CodeBadRequest, r.err.Error())); err != nil {
			return err
		}
		if r.fatal {
			return r.err
		}
		return nil
	}

	switch r.frame.Type {
	case wire.FrameSubscribe:
		return c.onSubscribe(ctx, r.frame)

	case wire.FrameSnapshot:
		// A one-off read. While the watch pushes it is answered from the
		// PUBLISHED snapshot, never by rebuilding: a client-triggered rebuild
		// would make `snapshot` a poll with extra steps, and the published
		// snapshot is current by construction. Once the watch has DEGRADED
		// that premise is gone, and the read is a serialized rebuild — see
		// answerSnapshot (code review round 2, A8).
		enrich := c.enrich
		if r.frame.Enrich != nil {
			enrich = *r.frame.Enrich
		}
		reqID, filter := r.frame.ReqID, c.filter
		c.inflight++
		go c.answerSnapshot(ctx, reqID, filter, enrich)
		return nil

	case wire.FrameRefresh:
		if c.srv.cfg.Deps.Refresh == nil {
			return c.write(wire.NewError(r.frame.ReqID, wire.CodeUnsupported,
				"this build has no corroboration source to refresh"))
		}
		reqID, sources := r.frame.ReqID, r.frame.Sources
		c.inflight++
		go c.answerRefresh(ctx, reqID, sources)
		return nil

	case wire.FramePing:
		// M10 — pong exists, echoes the reqId, and carries the instant of the
		// state the server holds. A ping with no reqId is answered with
		// reqId: null rather than ignored.
		return c.write(wire.NewPong(r.frame.ReqID, observedAtOf(c.srv.cfg.Deps.Events.Latest())))

	default:
		return c.write(wire.NewError(r.frame.ReqID, wire.CodeUnsupported,
			fmt.Sprintf("unknown frame type %q", r.frame.Type)))
	}
}

// onSubscribe replaces this connection's filter and enrichment setting.
//
// A new subscription delivers a fresh baseline snapshot, which is the answer to
// the subscribe: a client that changed its filter and received deltas computed
// against a view it no longer holds would have no way to reconcile them.
// Changing the standing enrichment setting requires this frame, exactly as
// §7.5(f) says — a one-off {"type":"snapshot","enrich":true} does not.
func (c *conn) onSubscribe(ctx context.Context, f wire.ClientFrame) error {
	filter, err := f.Filter.ToDomain()
	if err != nil {
		return c.write(wire.NewError(f.ReqID, wire.CodeBadRequest, err.Error()))
	}
	enrich := f.Enrich != nil && *f.Enrich
	if enrich && c.srv.cfg.Deps.Enrich == nil {
		// hello already declared no "enrich" capability, so the client has
		// been told. Accepting the subscription without enrichment beats
		// refusing it: the status answer is complete without a colour.
		enrich = false
	}
	return c.resubscribe(ctx, filter, enrich, f.ReqID)
}

// resubscribe swaps the subscription. Called from serve() only.
func (c *conn) resubscribe(ctx context.Context, f observer.Filter, enrich bool, reqID *string) error {
	if c.subCancel != nil {
		c.subCancel()
		c.subCancel = nil
	}
	c.events = nil

	subCtx, cancel := context.WithCancel(ctx)
	ch, err := c.srv.cfg.Deps.Events.Subscribe(subCtx, f)
	if err != nil {
		cancel()
		return c.write(wire.NewError(reqID, wire.CodeUnsupported,
			fmt.Sprintf("cannot subscribe: %v", err)))
	}

	c.subCancel = cancel
	c.filter = f
	c.enrich = enrich
	c.events = ch
	c.pendingReqID = reqID
	// A new subscription is a new view, so the client needs a new baseline
	// before any delta computed against that view means anything.
	c.baseline, c.baselineAt = false, time.Time{}

	if reqID != nil && c.srv.cfg.Deps.Events.Latest() == nil {
		// Nothing has been published yet, so the subscription delivered no
		// baseline and the reqId would never be answered. Answer from the core
		// instead — a reqId with no answer is the one failure mode a client
		// cannot recover from without a timeout of its own.
		c.pendingReqID = nil
		c.inflight++
		go c.answerSnapshot(ctx, reqID, f, enrich)
	}
	return nil
}

// answerSnapshot builds one snapshot frame off the serve goroutine.
//
// filter and enrich are ARGUMENTS rather than reads of c.filter/c.enrich: those
// fields belong to serve(), and reading them here would be a data race the
// race detector would find only when a subscribe raced a snapshot request.
func (c *conn) answerSnapshot(ctx context.Context, reqID *string, filter observer.Filter, enrich bool) {
	snap := c.srv.cfg.Deps.Events.Latest()
	if pullsMustRead(c.srv.cfg.Deps.Events) && c.srv.cfg.Deps.Rebuild != nil {
		// §6.6's on-demand answer: a degraded watch will never update the
		// published snapshot again, so a pull reads — THROUGH THE LOOP, so it
		// is serialized with every other rebuild and every subscriber sees the
		// same new state as deltas.
		if s, _, err := c.srv.cfg.Deps.Rebuild.Rebuild(ctx, "pull"); err == nil && s != nil {
			snap = s
		}
	}
	if snap == nil {
		// No publication yet: fall back to a direct read, which is what a pull
		// surface would have done anyway.
		opts := observer.DefaultSnapshotOptions()
		opts.Filter = filter
		s, err := c.srv.cfg.Deps.Core.Snapshot(ctx, opts)
		if err != nil {
			c.reply(ctx, wire.NewError(reqID, wire.CodeUnsupported,
				fmt.Sprintf("no snapshot is available: %v", err)))
			return
		}
		snap = s
	}

	view := c.viewOf(ctx, snap, filter, enrich)
	if view == nil {
		c.reply(ctx, wire.NewError(reqID, wire.CodeUnsupported, "no snapshot is available"))
		return
	}
	c.reply(ctx, wire.NewSnapshotFrame(reqID, view.encode(c.srv.cfg.TT)))
}

// answerRefresh performs §7.5(d)'s round trip.
//
//	sources{reqId, cause:"refresh"}  the spawn resolved (or the cache was warm)
//	error{code:"rate_limited"}       the core refused to spawn
//	error{code:"unsupported"}        no source could be refreshed at all
//
// A timeout or a failed spawn is NEITHER of the errors: it is a successful
// degraded answer whose sources block carries lastError and the advisories
// that describe it. §7.4 draws that line and §5.4 depends on it — a refresh
// must never fail a caller's request, it degrades the answer and says so.
func (c *conn) answerRefresh(ctx context.Context, reqID *string, sources []string) {
	want := sources
	if len(want) == 0 {
		want = []string{observer.SourceAgentsCLI}
	}

	outcome := c.srv.cfg.Deps.Refresh.RefreshSources(ctx, want)
	if code, detail, refused := refusal(want, outcome); refused {
		c.reply(ctx, wire.NewError(reqID, code, detail))
		return
	}

	// Rebuild THROUGH THE LOOP so every subscriber sees the corroborated state
	// as deltas, and this client's sources frame describes the same snapshot
	// they were just sent. Without this the requester would hold corroborated
	// source counters over an uncorroborated session set.
	snap := c.srv.cfg.Deps.Events.Latest()
	if rb := c.srv.cfg.Deps.Rebuild; rb != nil {
		if s, _, err := rb.Rebuild(ctx, wire.CauseRefresh); err == nil && s != nil {
			snap = s
		}
	}
	c.reply(ctx, wire.NewSourcesFrame(reqID, sourcesOf(snap), wire.CauseRefresh))
}

// refusal decides whether a RefreshOutcome is a protocol error or a degraded
// answer.
//
// It is a refusal only when EVERY requested source was rejected for the same
// class of reason — a partial refresh is an answer, and reporting it as an
// error would lose the half that worked.
func refusal(want []string, out observer.RefreshOutcome) (code, detail string, refused bool) {
	if len(out.Rejected) == 0 {
		return "", "", false
	}
	limited, unsupported := 0, 0
	for _, id := range want {
		switch out.Rejected[id] {
		case observer.RejectRateLimited, observer.RejectBackoff:
			limited++
		case observer.RejectUnsupported:
			unsupported++
		}
	}
	switch {
	case limited == len(want):
		return wire.CodeRateLimited, "the corroboration source was refreshed too recently", true
	case unsupported == len(want):
		return wire.CodeUnsupported, "no requested source can be refreshed", true
	default:
		return "", "", false
	}
}

// ---------------------------------------------------------------------------
// the wire
// ---------------------------------------------------------------------------

// write stamps the per-connection sequence number and writes one frame.
//
// seq is monotonic PER CONNECTION and is assigned here, by the one goroutine
// that writes. Using the snapshot's global seq instead would leak one
// consumer's view of the rebuild count to another and would show gaps that
// mean nothing to the receiver; a per-connection counter can only gap after a
// resync, which is what §5.3 promises.
func (c *conn) write(f wire.Frame) error {
	c.seq++
	f.Seq = c.seq
	return encodeFrame(c.nc, c.enc, f, c.srv.cfg.WriteTimeout)
}

// writeFrame writes one frame on a connection with no encoder of its own —
// the over-the-cap refusal, which never becomes a conn.
func writeFrame(nc net.Conn, f wire.Frame, timeout time.Duration) error {
	return encodeFrame(nc, json.NewEncoder(nc), f, timeout)
}

// encodeFrame is the one place bytes reach the socket.
//
// The deadline is the only time value in this package, and it is a WRITE
// deadline: armed by a write in flight, cleared by its completion, never armed
// while the server is idle. That satisfies all four clauses of §9.3a's timer
// rule and is not a schedule — nothing here wakes anything up, it only bounds
// how long a stuck client can hold a goroutine. time.Now is unavoidable for an
// absolute deadline and publishes nothing.
func encodeFrame(nc net.Conn, enc *json.Encoder, f wire.Frame, timeout time.Duration) error {
	if err := nc.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("sockserve: setting the write deadline: %w", err)
	}
	if err := enc.Encode(f); err != nil {
		return fmt.Errorf("sockserve: writing a %s frame: %w", f.Type, err)
	}
	return nil
}

// sourcesOf converts a snapshot's source reports for a frame that carries only
// them. A nil snapshot gives a nil map, which serialises as null — "we have
// not read anything yet", which is the truth before the first publication.
func sourcesOf(snap *observer.Snapshot) map[string]wire.Source {
	if snap == nil {
		return nil
	}
	return wire.NewSources(snap.Sources)
}

// observedAtOf is the instant of the state the server holds, for pong.
func observedAtOf(snap *observer.Snapshot) *string {
	if snap == nil {
		return nil
	}
	s := observer.FormatTime(snap.ObservedAt)
	return &s
}

// pullsMustRead reports whether the event source has stopped pushing for good,
// which is when a pull must be answered by reading rather than from the
// published snapshot (A8). A source that cannot say is assumed to push.
func pullsMustRead(ev Subscriber) bool {
	d, ok := ev.(interface{ WatchDegraded() bool })
	return ok && d.WatchDegraded()
}
