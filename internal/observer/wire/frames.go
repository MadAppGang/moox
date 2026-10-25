package wire

// The S3 protocol's vocabulary: the constructors for every server→client
// frame, the client→server frames, and the pid file.
//
// All of it is here rather than in sockserve for §5.1's reason — this package
// is the ONE place anything becomes JSON, and the ban on json tags outside it
// is enforced by an AST test. The pid file is in that set: it is a documented
// on-disk JSON contract that `tt observe --stop`, the doctor and §5.5's panel
// all read, so a second declaration of it in the adapter would be a second
// encoder of the same thing.
//
// What sockserve owns instead: the socket, the connections, the sequence
// numbers and the policy. It calls these constructors and assigns Seq.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
)

// Client frame types. §5.3 makes every one of them OPTIONAL: a client that
// connects and never writes gets the full stream, because "consumers should not
// poll" must not be replaced by "consumers must implement a handshake".
const (
	FrameSubscribe = "subscribe"
	FrameRefresh   = "refresh"
	FramePing      = "ping"
	// FrameSnapshot is in both directions: the client asks with it, the server
	// answers with it.
)

// Capabilities a server may declare in hello. The list is data so that a
// server which is missing a port declares one capability fewer rather than
// advertising a verb it will refuse (§7.5f: "a server without it is treated as
// enrich:false", which only works if the absence is visible).
const (
	CapSnapshot = "snapshot"
	CapDeltas   = "deltas"
	CapRefresh  = "refresh"
	CapFilter   = "filter"
	CapPing     = "ping"
	CapEnrich   = "enrich"
	// CapIncludeDead says the server honours filter.includeDead (code review
	// round 2, A10). A client talking to a server without it does not ask,
	// and keeps the old default-view behaviour.
	CapIncludeDead = "include-dead"
	// CapIncludeSpawned says the server honours filter.includeSpawned: it
	// hides sessions another session launched (spawnedBy set) unless asked.
	// A server one build behind neither hides them nor declares this, so a
	// client that sees it absent knows the population it holds is already
	// whole.
	CapIncludeSpawned = "include-spawned"
)

// Causes for a sources frame.
const (
	CauseRefresh       = "refresh"
	CausePiggyback     = "piggyback"
	CauseWatchDegraded = "watch-degraded"
	CauseSourceError   = "source-error"
)

// Error codes. §5.3 names exactly these three, and the server maps to them
// rather than inventing a fourth: a timeout or a failed spawn is a DEGRADED
// ANSWER (a sources frame whose source carries lastError), not a protocol
// error, which is the distinction §7.4 draws and §7.5(d) depends on.
const (
	CodeBadRequest  = "bad_request"
	CodeRateLimited = "rate_limited"
	CodeUnsupported = "unsupported"
)

// ReasonShutdown is bye's ONE reason. The user's override deleted the idle
// timeout, so a bye can only follow SIGTERM/SIGINT — and a client must still
// handle a bare EOF, because a run-forever server can be killed at any time.
const ReasonShutdown = "shutdown"

// ReasonSlowConsumer is the resync reason. A resync means "deltas were dropped
// for you"; the snapshot that follows is the new baseline.
const ReasonSlowConsumer = "slow-consumer"

// WatchDegradedOnDemand is the mode a degraded watch reports, matching
// sources.<id>.watch.mode.
const WatchDegradedOnDemand = observer.WatchModeOnDemand

// NewHello is the first frame on every connection.
//
// sources comes from the server's published snapshot rather than from a fresh
// read: a hello that triggered a rebuild would make connecting a way to poll.
func NewHello(o Options, pid int, capabilities, notCapable []string, sources map[string]Source) Frame {
	return Frame{
		Type:         FrameHello,
		Schema:       Schema,
		Protocol:     Protocol,
		TT:           o.TT,
		PID:          pid,
		Capabilities: copyStrings(capabilities),
		NotCapable:   copyStrings(notCapable),
		Sources:      sources,
	}
}

// NewSnapshotFrame wraps one encoded envelope. reqID is nil for the
// unsolicited baseline and echoes the request that asked for it otherwise.
func NewSnapshotFrame(reqID *string, s Snapshot) Frame {
	return Frame{Type: FrameSnapshot, ReqID: reqID, Snapshot: &s}
}

// NewSources converts the core's source reports for the frames that carry only
// them (hello and sources).
//
// It exists so that those two frames and the snapshot envelope go through the
// SAME converter: a second conversion of a SourceReport would be a second
// place for `sources.registry.watch.mode` to be spelled differently, and
// §9.2(a) puts that field on AC11's exclusion list precisely because it is the
// one legitimate per-surface difference. Everything else in the block has to
// match, which only holds if one function produces it.
func NewSources(in map[string]observer.SourceReport) map[string]Source {
	if in == nil {
		return nil
	}
	out := make(map[string]Source, len(in))
	for id, rep := range in {
		out[id] = newSource(rep)
	}
	return out
}

// NewSessionAdded reports a session ENTERING this connection's view, for any
// reason — a new record, or a change that now matches the filter (H13).
func NewSessionAdded(s Session) Frame {
	return Frame{Type: FrameSessionAdded, SessionID: s.SessionID, Kind: s.Kind, Session: &s}
}

// NewSessionChanged carries the whole session as well as changed[]. The
// redundancy is deliberate: the alternative is every client implementing a
// merge, and at ~1 KB against three events per nine minutes redundancy is free
// while correctness is not.
func NewSessionChanged(s Session, changed []string) Frame {
	return Frame{
		Type:      FrameSessionChanged,
		SessionID: s.SessionID,
		Kind:      s.Kind,
		Session:   &s,
		Changed:   copyStrings(changed),
	}
}

// NewSessionRemoved reports a session LEAVING this connection's view. reason is
// one of observer's removal reasons; "filtered-out" is client-relative and says
// nothing about the session's existence. kind is the removed record's kind —
// the other half of its identity (A14).
func NewSessionRemoved(sessionID, kind, reason string) Frame {
	return Frame{Type: FrameSessionRemoved, SessionID: sessionID, Kind: kind, Reason: reason}
}

// NewSourcesFrame answers a refresh (with its reqId) or reports a change in
// what the sources know.
func NewSourcesFrame(reqID *string, sources map[string]Source, cause string) Frame {
	return Frame{Type: FrameSources, ReqID: reqID, Sources: sources, Cause: cause}
}

// NewWatchDegraded tells a subscriber immediately that the stream it is
// holding will not push, rather than leaving it waiting for an event that
// cannot come (§6.6).
func NewWatchDegraded(source, mode, errText string) Frame {
	return Frame{Type: FrameWatchDegraded, Source: source, Mode: mode, Error: errText}
}

// NewPong answers a ping (M10). observedAt is the instant of the state the
// server currently holds — NOT a fresh clock read.
//
// That is the whole of P7's divergence from §5.3's "a round-trip also measures
// clock skew", and it is deliberate: every timestamp any surface publishes is
// derived from the rebuild's own `now`, a rule three live defects in P2, P3 and
// P4 were each caused by breaking. A pong still measures liveness — the answer
// arriving at all proves the server's accept path, its reader and its writer
// are running — and it now also reports how old the state being served is,
// which is the more useful of the two numbers a monitor can act on. A client
// that wants skew compares its own clock against the snapshot's observedAt,
// which it already has.
func NewPong(reqID *string, observedAt *string) Frame {
	return Frame{Type: FramePong, ReqID: reqID, ObservedAt: observedAt}
}

// NewResync says deltas were dropped for this connection. A fresh snapshot
// follows it, and it is the only thing that may precede a gap in seq.
func NewResync(reason string) Frame {
	return Frame{Type: FrameResync, Reason: reason}
}

// NewError answers a request that cannot be served. rate_limited is an ANSWER
// rather than a failure: the client answers its own caller from the current
// snapshot with a refresh-rejected advisory (§5.4).
func NewError(reqID *string, code, detail string) Frame {
	return Frame{Type: FrameError, ReqID: reqID, Code: code, Detail: detail}
}

// NewBye is sent on SIGTERM/SIGINT.
func NewBye(reason string) Frame {
	return Frame{Type: FrameBye, Reason: reason}
}

// ---------------------------------------------------------------------------
// client -> server
// ---------------------------------------------------------------------------

// ClientFilter is §5.3's filter object.
//
// Statuses are NOT validated against a closed set, and that is B7's rule
// rather than laxity: status is an OBSERVED set, so a client filtering for a
// status this build has never seen must get an empty stream rather than a
// protocol error. Kinds ARE validated, because there are exactly three and a
// typo would otherwise select silently nothing.
type ClientFilter struct {
	Kind      []string `json:"kind"`
	Status    []string `json:"status"`
	CWDPrefix string   `json:"cwdPrefix"`
	// WaitingOnly is tt's own addition to §5.3's three keys: the core's Filter
	// carries it, S1 exposes it as --waiting, and a socket client that could
	// not ask for it would have to receive every session and re-derive
	// `attention` itself — which is the client-side filter divergence H13
	// exists to prevent.
	WaitingOnly bool `json:"waitingOnly"`
	// IncludeDead keeps live:false sessions in this connection's view. A
	// server one build behind ignores the key (unknown client fields are
	// ignored) and does not declare CapIncludeDead, so a client knows not to
	// rely on it.
	IncludeDead bool `json:"includeDead"`
	// IncludeSpawned keeps sessions another session launched in this
	// connection's view. Same compatibility story as IncludeDead: an older
	// server ignores the key and does not declare CapIncludeSpawned.
	IncludeSpawned bool `json:"includeSpawned"`
}

// ClientFrame is one NDJSON line from a client.
//
// Enrich is a *bool so that "absent" and "false" stay distinguishable. On a
// `subscribe` they mean the same thing (§7.5f: absent or false ⇒
// transcriptCollected false on every session). On a one-off `snapshot` they do
// not: absent means "whatever this subscription is set to", and an explicit
// false means "not this time" — which is what keeps a one-off enriched read
// from silently making the whole stream expensive, and its inverse from
// silently disabling a subscription's enrichment.
type ClientFrame struct {
	Type    string        `json:"type"`
	ReqID   *string       `json:"reqId"`
	Filter  *ClientFilter `json:"filter"`
	Enrich  *bool         `json:"enrich"`
	Sources []string      `json:"sources"`
}

// ErrBadRequest is what a malformed client frame produces. The server answers
// error{code:"bad_request"} and keeps the connection: one bad line is not a
// reason to drop a subscriber that may have 39 sessions' worth of state.
var ErrBadRequest = fmt.Errorf("wire: bad request")

// DecodeClientFrame parses one client line.
//
// Unknown FIELDS are ignored silently — "degrade silently on unknown data",
// and a newer client sending a key this build does not know should still get
// its subscription. An unknown TYPE is not ignored: the server answers
// error{unsupported}, because a client that asked for something and got
// nothing back cannot tell the difference between "refused" and "lost".
func DecodeClientFrame(data []byte) (ClientFrame, error) {
	var f ClientFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return ClientFrame{}, fmt.Errorf("%w: %s", ErrBadRequest, err)
	}
	if strings.TrimSpace(f.Type) == "" {
		return ClientFrame{}, fmt.Errorf("%w: no type", ErrBadRequest)
	}
	return f, nil
}

// ToDomain converts a client filter to the core's Filter.
//
// An empty filter is the zero Filter, which matches everything — so
// `{"type":"subscribe"}` with no filter widens rather than narrows.
func (f *ClientFilter) ToDomain() (observer.Filter, error) {
	if f == nil {
		return observer.Filter{}, nil
	}
	out := observer.Filter{
		CWDPrefix:      f.CWDPrefix,
		WaitingOnly:    f.WaitingOnly,
		IncludeDead:    f.IncludeDead,
		IncludeSpawned: f.IncludeSpawned,
	}
	for _, k := range f.Kind {
		kind, ok := parseKind(k)
		if !ok {
			return observer.Filter{}, fmt.Errorf("%w: unknown kind %q", ErrBadRequest, k)
		}
		out.Kinds = append(out.Kinds, kind)
	}
	for _, s := range f.Status {
		out.Statuses = append(out.Statuses, observer.Status(strings.TrimSpace(s)))
	}
	return out, nil
}

// parseKind maps the wire spelling of a kind to the domain value.
func parseKind(s string) (observer.Kind, bool) {
	switch observer.Kind(strings.ToLower(strings.TrimSpace(s))) {
	case observer.KindInteractive:
		return observer.KindInteractive, true
	case observer.KindBackground:
		return observer.KindBackground, true
	case observer.KindUnknown:
		return observer.KindUnknown, true
	default:
		return "", false
	}
}

// ---------------------------------------------------------------------------
// the pid file
// ---------------------------------------------------------------------------

// PidFile is ~/.local/state/tt/observer.pid, written once at bind and removed
// at exit.
//
// It is what answers "who notices when it lingers" now that the server runs
// forever: a running server is discoverable, `tt observe --stop` has a pid to
// signal, and a CRASHED server leaves this file behind for the stale-socket
// rule to reconcile against the socket.
type PidFile struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	Socket    string `json:"socket"`
	TT        string `json:"tt"`
	// Role is which command bound the socket: RoleServe for the moox daemon
	// (`tt serve --foreground`, observer AND saver), RoleObserve for a plain
	// `tt observe`. Empty in a pid file from a binary that predates roles.
	// `tt serve --ensure` keeps only a RoleServe process as the daemon
	// (G2-2-H1): the pid file is the one thing both write.
	Role string `json:"role"`
}

// The pid file roles.
const (
	RoleServe   = "serve"
	RoleObserve = "observe"
)

// EncodePidFile renders the pid file, newline-terminated so `cat` of it reads
// like every other line-oriented thing tt writes.
func EncodePidFile(p PidFile) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("wire: encode pid file: %w", err)
	}
	return append(b, '\n'), nil
}

// DecodePidFile parses the pid file. A file that will not parse is reported as
// an error rather than as pid 0: signalling pid 0 would signal the whole
// process group.
func DecodePidFile(data []byte) (PidFile, error) {
	var p PidFile
	if err := json.Unmarshal(data, &p); err != nil {
		return PidFile{}, fmt.Errorf("wire: decode pid file: %w", err)
	}
	if p.PID <= 0 {
		return PidFile{}, fmt.Errorf("wire: decode pid file: pid %d is not a process", p.PID)
	}
	return p, nil
}
