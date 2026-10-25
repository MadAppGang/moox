// Package wire is the ONE place anything in the observer becomes JSON, and the
// ONE place anything comes back.
//
// # Why this package exists
//
// Four machine-readable surfaces — the CLI's --json, the MCP server's
// structuredContent, the socket stream's frames, and a socket client's
// re-derived local snapshot — must agree field for field. The conventional way
// to achieve that is for each adapter to build the same JSON, which drifts on
// the first field added. The structural way is this package: there is exactly
// one struct per shape, one set of json tags, one constructor and one inverse,
// and an AST test bans json-tagged structs anywhere outside it. No adapter owns
// a DTO.
//
// # The three rules
//
//  1. omitempty is BANNED. "subagents": 0 versus null, and "blockedForMs": 0
//     versus null, are whole acceptance criteria. The one deliberate
//     non-pointer exception is waitReason, where "" means "not applicable" and
//     "unknown" means "waiting, reason undetermined", so a third null state
//     would be meaningless.
//  2. Timestamps are RFC3339 with milliseconds, UTC. Derived ages are
//     PRE-COMPUTED so no consumer does clock arithmetic against our clock.
//  3. The tt version has one source — observer.ResolveVersion, passed in
//     through Options. Never hardcoded in an encoder, and orthogonal to Schema.
//
// # The inverse, and what it guarantees
//
// domain -> wire -> domain is the identity on every field of a session. A
// tri-state round-trips as a tri-state: a null never becomes a zero and a zero
// never becomes a null. An advisory the decoder does not recognise is
// PRESERVED, because advisory codes may be added in a minor version and a
// client one build behind must still be able to show them. An unknown FIELD is
// dropped, with a wire-unknown-field advisory naming it.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/moox/internal/observer"
)

// Schema is the JSON shape version. It is bumped only on a breaking wire
// change, which is why an added field arrives silently and why the decoder has
// a rule for unknown fields.
const Schema = 1

// Protocol is the socket framing version, independent of Schema: the framing
// can change without the payload shape changing, and vice versa.
const Protocol = 1

// Options is what the encoder needs that a snapshot does not carry.
//
// Deliberately minimal: filtering is the CORE's job, on
// observer.SnapshotOptions, not the encoder's. Having both filter would give
// two places to disagree about which sessions exist, and the four-surface
// byte-identity property would then depend on every adapter passing identical
// encoder options — which is exactly the convention-over-structure failure this
// package exists to avoid.
type Options struct {
	// TT is tt's own version string, from observer.ResolveVersion.
	TT string
}

// ---------------------------------------------------------------------------
// Advisory — flattened, because that is what consumers read
// ---------------------------------------------------------------------------

// Advisory encodes as a flat object: the code plus its fields as siblings, e.g.
// {"code":"transcript-absent","path":"/..."}. Flat rather than nested because
// an advisory is read by humans in a terminal as often as by code, and
// {"code":"x","fields":{...}} costs a level of indirection on every read.
type Advisory struct {
	Code   string
	Fields map[string]any
}

// MarshalJSON writes the code and its fields as siblings. A field literally
// named "code" would collide with the code itself; it is dropped rather than
// allowed to overwrite it, because an advisory whose code is a field value is
// unreadable by anything.
func (a Advisory) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(a.Fields)+1)
	for k, v := range a.Fields {
		if k == "code" {
			continue
		}
		out[k] = v
	}
	out["code"] = a.Code
	return json.Marshal(out)
}

// UnmarshalJSON reads the code and treats every other key as a field.
//
// Numbers are normalised to int64 where they are integral and float64
// otherwise, and homogeneous string arrays to []string, so that a decoded
// advisory compares equal to the one the constructors produce. Without that,
// every integer field would come back as a float64 and the round-trip identity
// would fail on advisories alone.
func (a *Advisory) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("advisory: %w", err)
	}
	a.Code = ""
	a.Fields = nil
	if c, ok := raw["code"]; ok {
		if err := json.Unmarshal(c, &a.Code); err != nil {
			return fmt.Errorf("advisory code: %w", err)
		}
	}
	if len(raw) <= 1 {
		return nil
	}
	fields := make(map[string]any, len(raw)-1)
	for k, v := range raw {
		if k == "code" {
			continue
		}
		val, err := decodeJSONValue(v)
		if err != nil {
			return fmt.Errorf("advisory field %q: %w", k, err)
		}
		fields[k] = val
	}
	a.Fields = fields
	return nil
}

// decodeJSONValue decodes one advisory field value into the normalised Go
// types the domain uses: int64 for integral numbers, float64 otherwise,
// []string for a homogeneous string array, []any otherwise.
func decodeJSONValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytesReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normalizeJSONValue(v), nil
}

func normalizeJSONValue(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, err := t.Float64()
		if err != nil {
			return t.String()
		}
		return f
	case []any:
		allStrings := len(t) > 0
		for _, e := range t {
			if _, ok := e.(string); !ok {
				allStrings = false
				break
			}
		}
		if allStrings {
			out := make([]string, 0, len(t))
			for _, e := range t {
				out = append(out, e.(string))
			}
			return out
		}
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, normalizeJSONValue(e))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalizeJSONValue(e)
		}
		return out
	default:
		return v
	}
}

// ---------------------------------------------------------------------------
// Session and its parts
// ---------------------------------------------------------------------------

// Tmux is a pane reference with its sigils intact.
type Tmux struct {
	Raw     string `json:"raw"`
	Session string `json:"session"`
	Window  string `json:"window"`
	Pane    string `json:"pane"`
}

// Transcript is the bounded, closed set of facts allowed to leave a transcript.
// No message content, no prompt text, no tool output.
type Transcript struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	TailBytes int    `json:"tailBytes"`
	Truncated bool   `json:"truncated"`
	// AgentColor is "" when the tail held no colour record; ColorFound says
	// whether that is "this file has none" or "we did not read far enough".
	AgentColor string `json:"agentColor"`
	ColorFound bool   `json:"colorFound"`
	APIErrors  int    `json:"apiErrors"`
	// Subagents is null when the subagents directory does not exist and 0 when
	// it exists and is empty. That distinction is why this is a pointer.
	Subagents      *int    `json:"subagents"`
	SubagentsKnown bool    `json:"subagentsKnown"`
	SessionKind    *string `json:"sessionKind"`
	// ContextTokens is null when the context is not measurable (no usable
	// usage in the tail, or a compaction that recorded no postTokens). Model
	// is a bounded id, "" when absent or refused. Compactions counts
	// compaction boundaries within the tail only. All three are additive:
	// an older decoder ignores them, and an older frame decodes to null, ""
	// and 0.
	ContextTokens *int64 `json:"contextTokens"`
	Model         string `json:"model"`
	Compactions   int    `json:"compactions"`
}

// Session is the only Session-shaped struct with json tags in the tree. Field
// order here is the field order on the wire, and it matches the documented
// schema so a diff against the spec is a visual one.
type Session struct {
	SessionID   string   `json:"sessionId"`
	ShortID     *string  `json:"shortId"`
	PID         *int     `json:"pid"`
	Kind        string   `json:"kind"`
	RawKind     *string  `json:"rawKind"`
	Name        string   `json:"name"`
	NameSource  string   `json:"nameSource"`
	FormerNames []string `json:"formerNames"`
	CWD         string   `json:"cwd"`
	StartedAt   *string  `json:"startedAt"`

	Status                string  `json:"status"`
	RawStatus             string  `json:"rawStatus"`
	StatusSource          string  `json:"statusSource"`
	StatusSince           *string `json:"statusSince"`
	StatusAgeMs           *int64  `json:"statusAgeMs"`
	StatusSinceLowerBound *string `json:"statusSinceLowerBound"`
	BlockedForMs          *int64  `json:"blockedForMs"`
	BlockedForUnavailable *string `json:"blockedForUnavailable"`
	// WaitReason is the ONE allowlisted non-pointer among the nullable-looking
	// fields. It is never null on the wire.
	WaitReason     string  `json:"waitReason"`
	RawWaitingFor  *string `json:"rawWaitingFor"`
	Attention      bool    `json:"attention"`
	LastActivityAt *string `json:"lastActivityAt"`

	Needs           *string  `json:"needs"`
	Detail          *string  `json:"detail"`
	Tempo           *string  `json:"tempo"`
	Tokens          *int64   `json:"tokens"`
	Children        []string `json:"children"`
	ResumeSessionID *string  `json:"resumeSessionId"`
	RecordUpdatedAt *string  `json:"recordUpdatedAt"`
	RecordAgeMs     *int64   `json:"recordAgeMs"`
	ReapedMidWorkAt *string  `json:"reapedMidWorkAt"`

	Tmux       *Tmux    `json:"tmux"`
	PaneAlive  *bool    `json:"paneAlive"`
	ProcAlive  *bool    `json:"procAlive"`
	Live       *bool    `json:"live"`
	LiveReason string   `json:"liveReason"`
	ListedBy   []string `json:"listedBy"`

	ClaudeVersion       string  `json:"claudeVersion"`
	MessagingSocketPath *string `json:"messagingSocketPath"`
	// SpawnedBy is the launching session's id, or null. Additive: an older
	// decoder drops it with a wire-unknown-field advisory, and an older frame
	// decodes to null — "no launcher found", which is the safe direction.
	SpawnedBy *string `json:"spawnedBy"`

	TranscriptCollected bool        `json:"transcriptCollected"`
	Transcript          *Transcript `json:"transcript"`

	Sources    []string   `json:"sources"`
	Stale      bool       `json:"stale"`
	ObservedAt string     `json:"observedAt"`
	Advisories []Advisory `json:"advisories"`
}

// ---------------------------------------------------------------------------
// Envelope
// ---------------------------------------------------------------------------

// Counts is the envelope tally. ByStatus and ByKind always carry every known
// key, including zeros: the omitempty ban covers struct tags, and a map needs
// its own rule or the first implementation's incidental behaviour becomes the
// contract.
type Counts struct {
	Total     int            `json:"total"`
	Live      int            `json:"live"`
	Dead      int            `json:"dead"`
	Unknown   int            `json:"unknown"`
	Attention int            `json:"attention"`
	ByStatus  map[string]int `json:"byStatus"`
	ByKind    map[string]int `json:"byKind"`
}

// Watch is the per-source watch accounting.
type Watch struct {
	Mode             string  `json:"mode"`
	ArmedDirs        int     `json:"armedDirs"`
	ArmedFiles       int     `json:"armedFiles"`
	ArmedTranscripts int     `json:"armedTranscripts"`
	ArmFailures      int     `json:"armFailures"`
	ArmIterations    int     `json:"armIterations"`
	Degraded         *string `json:"degraded"`
	LastEventAt      *string `json:"lastEventAt"`
	Events           int64   `json:"events"`
}

// Source is what one source reports about itself.
//
// DEVIATION FROM THE SPEC, recorded because a later phase owns this block and
// must decide deliberately: the architecture's §5.2 sample shows the per-source
// metrics FLATTENED and HETEROGENEOUS — registry carries records/parseErrors/
// ignoredKeyFiles, jobs carries entries/dirs/records/nonTerminal, agents-cli
// carries spawns/lastSpawnMs and the string labels command/agentsCache/trigger,
// tmux carries panes. One struct with no omitempty and a total inverse cannot
// produce that: either every source emits every key (a union of pointers), or
// the metrics are nested. P0 nests them under "counters" and "labels", which
// keeps the inverse exact and keeps the struct honest about what it knows.
// P4, which owns the sources block, should either keep this or switch to the
// union-of-pointers form — both satisfy the rules; the flat heterogeneous form
// in the sample does not.
type Source struct {
	ID         string  `json:"id"`
	Role       string  `json:"role"`
	Path       string  `json:"path"`
	ObservedAt *string `json:"observedAt"`
	// AgeMs is clock-derived and excluded from delta comparison.
	AgeMs        *int64            `json:"ageMs"`
	OK           bool              `json:"ok"`
	Stale        bool              `json:"stale"`
	StaleAfterMs *int64            `json:"staleAfterMs"`
	LastError    *string           `json:"lastError"`
	Advisories   []Advisory        `json:"advisories"`
	Watch        *Watch            `json:"watch"`
	Counters     map[string]int64  `json:"counters"`
	Labels       map[string]string `json:"labels"`
}

// Snapshot is the envelope every machine surface emits.
type Snapshot struct {
	Schema int    `json:"schema"`
	TT     string `json:"tt"`
	// ObservedAt is the encoder's `now`. It is the single reference instant
	// every derived number in the payload was computed against.
	ObservedAt string            `json:"observedAt"`
	Counts     Counts            `json:"counts"`
	Sources    map[string]Source `json:"sources"`
	Warnings   []Advisory        `json:"warnings"`
	Sessions   []Session         `json:"sessions"`
	NotCapable []string          `json:"notCapable"`
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

// Frame type names, server to client and client to server.
const (
	FrameHello          = "hello"
	FrameSnapshot       = "snapshot"
	FrameSessionAdded   = "session_added"
	FrameSessionChanged = "session_changed"
	FrameSessionRemoved = "session_removed"
	FrameSources        = "sources"
	FrameWatchDegraded  = "watch_degraded"
	FramePong           = "pong"
	FrameResync         = "resync"
	FrameError          = "error"
	FrameBye            = "bye"
)

// Frame is one NDJSON line on the socket.
//
// P0 defined the header plus the three session-delta payloads, which is what
// ApplySessionFrame needs. P7 added the hello, sources, watch_degraded, pong,
// error and bye payloads, and SETTLED the two questions P0 left open. Both
// answers are the conservative one, and both are structural rather than
// stylistic:
//
//	THE OMITEMPTY BAN WINS OVER §5.3's SAMPLES. Every frame carries every key;
//	an absent payload is null. §5.3 shows each sample carrying only its own
//	keys, which is omitempty behaviour, and §5.1 bans omitempty across this
//	package with one allowlisted exception. The ban is the rule AC11's
//	byte-identity assertion and TestNoOmitemptyAnywhereInWire rest on, so a
//	frame-only exemption would need a second marshaller per frame type — which
//	is precisely the "second encoder of the domain" §5.1 exists to prevent. The
//	cost is ~200 bytes of nulls on a frame whose measured rate is three per
//	nine minutes.
//
//	THE SNAPSHOT PAYLOAD STAYS NESTED under "snapshot" rather than flattened
//	into the frame. §5.3's sample flattens observedAt/counts/sources/sessions
//	to the frame's top level, which would require this struct to redeclare
//	every envelope key — a second declaration of the S1 envelope, differing
//	from it by construction the first time one of the two changes. Nested, the
//	payload IS a wire.Snapshot encoded by the one encoder, so "the CLI's bytes
//	and the socket's bytes are the same bytes" is a property of the value
//	rather than a comparison somebody has to maintain. AC11's assertion is
//	written against exactly that: frame.snapshot == the CLI's stdout line.
type Frame struct {
	Type  string  `json:"type"`
	Seq   int64   `json:"seq"`
	ReqID *string `json:"reqId"`

	// Session delta payloads.
	SessionID string `json:"sessionId"`
	// Kind is the other half of a session delta's identity (code review round
	// 2, A14): an interactive session and a background job may share a
	// sessionId, and a removal has no session to carry its kind. "" on every
	// other frame. ADDITIVE: Schema and Protocol are unchanged — a client one
	// build behind reports it once as an unknown field and removes by id, as it
	// always did; a client reading a server one build behind sees "" and falls
	// back to the id alone.
	Kind    string   `json:"kind"`
	Session *Session `json:"session"`
	Changed []string `json:"changed"`
	Reason  string   `json:"reason"`

	// Snapshot payload, nested for the reason stated above.
	Snapshot   *Snapshot         `json:"snapshot"`
	Sources    map[string]Source `json:"sources"`
	ObservedAt *string           `json:"observedAt"`

	// hello payload. Schema and Protocol are the two independent version
	// spaces §7.5(e) makes a client check before it attaches.
	Schema       int      `json:"schema"`
	Protocol     int      `json:"protocol"`
	TT           string   `json:"tt"`
	PID          int      `json:"pid"`
	Capabilities []string `json:"capabilities"`
	// NotCapable is declared, not omitted (§4.4 residual 3): a subscriber that
	// needs a frame at the instant a threshold is crossed reads here that it
	// will never get one, instead of waiting for it.
	NotCapable []string `json:"notCapable"`

	// Cause is the sources frame's "refresh" | "piggyback" | "watch-degraded" |
	// "source-error".
	Cause string `json:"cause"`

	// watch_degraded payload. Source is "" when the degradation is the whole
	// watch rather than one family — the Watcher port is coarse by design
	// (§6.2), so a per-source attribution would be invented rather than
	// observed.
	Source string `json:"source"`
	Mode   string `json:"mode"`
	Error  string `json:"error"`

	// error payload.
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// AttentionSession is the projection behind the session_counts tool: the answer
// to "is anyone else blocked, and on what?" without shipping the full record
// set.
//
// The spec calls this "eight fields" and then lists ten in its own example. The
// example wins — it is the one a consumer would have been written against — so
// all ten are here: Needs, because for a blocked background agent that string
// IS the answer; and Live, because this projection is the surface most damaged
// by confusing membership with liveness, so an agent this list names should say
// whether we can actually vouch for it.
type AttentionSession struct {
	SessionID    string  `json:"sessionId"`
	Name         string  `json:"name"`
	Kind         string  `json:"kind"`
	Status       string  `json:"status"`
	WaitReason   string  `json:"waitReason"`
	BlockedForMs *int64  `json:"blockedForMs"`
	Needs        *string `json:"needs"`
	Live         *bool   `json:"live"`
	Tmux         *Tmux   `json:"tmux"`
	CWD          string  `json:"cwd"`
}

// ToolResult is the MCP tool payload shape: a compact human summary alongside
// the structured answer, never the full record set in the text.
type ToolResult struct {
	Counts     Counts             `json:"counts"`
	Attention  []AttentionSession `json:"attention"`
	Sources    map[string]Source  `json:"sources"`
	Warnings   []Advisory         `json:"warnings"`
	NotCapable []string           `json:"notCapable"`
}

// ---------------------------------------------------------------------------
// domain -> wire
// ---------------------------------------------------------------------------

// NewSnapshot is the only constructor. Every machine surface calls it, which is
// what makes the four-surface agreement structural rather than conventional.
//
// now becomes the envelope's observedAt. It does NOT overwrite each session's
// own ObservedAt: a session's observedAt is when that session was OBSERVED, and
// silently replacing it with encode time would make the field a lie in exactly
// the case it matters, a snapshot re-encoded by an attached client. In the
// normal path the core sets both from the same rebuild instant, so they agree.
//
// NewSnapshot does not filter. Filtering is the core's job.
func NewSnapshot(s *observer.Snapshot, now time.Time, o Options) Snapshot {
	out := Snapshot{
		Schema:     Schema,
		TT:         o.TT,
		ObservedAt: observer.FormatTime(now),
		Counts:     newCounts(s),
	}
	if s == nil {
		return out
	}
	if s.Sources != nil {
		out.Sources = make(map[string]Source, len(s.Sources))
		for id, src := range s.Sources {
			out.Sources[id] = newSource(src)
		}
	}
	out.Warnings = newAdvisories(s.Warnings)
	out.NotCapable = copyStrings(s.NotCapable)
	if s.Sessions != nil {
		out.Sessions = make([]Session, 0, len(s.Sessions))
		for i := range s.Sessions {
			out.Sessions = append(out.Sessions, NewSession(s.Sessions[i]))
		}
	}
	return out
}

func newCounts(s *observer.Snapshot) Counts {
	c := Counts{ByStatus: map[string]int{}, ByKind: map[string]int{}}
	// Every known key, always, including zeros.
	for _, st := range observer.AllStatuses {
		c.ByStatus[string(st)] = 0
	}
	for _, k := range observer.AllKinds {
		c.ByKind[string(k)] = 0
	}
	if s == nil {
		return c
	}
	c.Total = s.Counts.Total
	c.Live = s.Counts.Live
	c.Dead = s.Counts.Dead
	c.Unknown = s.Counts.Unknown
	c.Attention = s.Counts.Attention
	for st, n := range s.Counts.ByStatus {
		c.ByStatus[string(st)] = n
	}
	for k, n := range s.Counts.ByKind {
		c.ByKind[string(k)] = n
	}
	return c
}

func newSource(s observer.SourceReport) Source {
	out := Source{
		ID:           s.ID,
		Role:         s.Role,
		Path:         s.Path,
		ObservedAt:   formatTimePtr(s.ObservedAt),
		AgeMs:        copyInt64Ptr(s.AgeMs),
		OK:           s.OK,
		Stale:        s.Stale,
		StaleAfterMs: copyInt64Ptr(s.StaleAfterMs),
		LastError:    copyStringPtr(s.LastError),
		Advisories:   newAdvisories(s.Advisories),
	}
	if s.Counters != nil {
		out.Counters = make(map[string]int64, len(s.Counters))
		for k, v := range s.Counters {
			out.Counters[k] = v
		}
	}
	if s.Labels != nil {
		out.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			out.Labels[k] = v
		}
	}
	if s.Watch != nil {
		out.Watch = &Watch{
			Mode:             s.Watch.Mode,
			ArmedDirs:        s.Watch.ArmedDirs,
			ArmedFiles:       s.Watch.ArmedFiles,
			ArmedTranscripts: s.Watch.ArmedTranscripts,
			ArmFailures:      s.Watch.ArmFailures,
			ArmIterations:    s.Watch.ArmIterations,
			Degraded:         copyStringPtr(s.Watch.Degraded),
			LastEventAt:      formatTimePtr(s.Watch.LastEventAt),
			Events:           s.Watch.Events,
		}
	}
	return out
}

// NewSession encodes one domain session.
func NewSession(s observer.Session) Session {
	out := Session{
		SessionID:   s.SessionID,
		ShortID:     copyStringPtr(s.ShortID),
		PID:         copyIntPtr(s.PID),
		Kind:        string(s.Kind),
		RawKind:     copyStringPtr(s.RawKind),
		Name:        s.Name,
		NameSource:  s.NameSource,
		FormerNames: copyStrings(s.FormerNames),
		CWD:         s.CWD,
		StartedAt:   formatTimePtr(s.StartedAt),

		Status:                string(s.Status),
		RawStatus:             s.RawStatus,
		StatusSource:          s.StatusSource,
		StatusSince:           formatTimePtr(s.StatusSince),
		StatusAgeMs:           copyInt64Ptr(s.StatusAgeMs),
		StatusSinceLowerBound: formatTimePtr(s.StatusSinceLowerBound),
		BlockedForMs:          copyInt64Ptr(s.BlockedForMs),
		BlockedForUnavailable: copyStringPtr(s.BlockedForUnavailable),
		WaitReason:            string(s.WaitReason),
		RawWaitingFor:         copyStringPtr(s.RawWaitingFor),
		Attention:             s.Attention,
		LastActivityAt:        formatTimePtr(s.LastActivityAt),

		Needs:           copyStringPtr(s.Needs),
		Detail:          copyStringPtr(s.Detail),
		Tempo:           copyStringPtr(s.Tempo),
		Tokens:          copyInt64Ptr(s.Tokens),
		Children:        copyStrings(s.Children),
		ResumeSessionID: copyStringPtr(s.ResumeSessionID),
		RecordUpdatedAt: formatTimePtr(s.RecordUpdatedAt),
		RecordAgeMs:     copyInt64Ptr(s.RecordAgeMs),
		ReapedMidWorkAt: formatTimePtr(s.ReapedMidWorkAt),

		PaneAlive:  copyBoolPtr(s.PaneAlive),
		ProcAlive:  copyBoolPtr(s.ProcAlive),
		Live:       copyBoolPtr(s.Live),
		LiveReason: string(s.LiveReason),
		ListedBy:   copyStrings(s.ListedBy),

		ClaudeVersion:       s.ClaudeVersion,
		MessagingSocketPath: copyStringPtr(s.MessagingSocketPath),
		SpawnedBy:           copyStringPtr(s.SpawnedBy),

		TranscriptCollected: s.TranscriptCollected,

		Sources:    copyStrings(s.Sources),
		Stale:      s.Stale,
		ObservedAt: observer.FormatTime(s.ObservedAt),
		Advisories: newAdvisories(s.Advisories),
	}
	if s.Tmux != nil {
		out.Tmux = &Tmux{
			Raw:     s.Tmux.Raw,
			Session: s.Tmux.Session,
			Window:  s.Tmux.Window,
			Pane:    s.Tmux.Pane,
		}
	}
	if s.Transcript != nil {
		t := s.Transcript
		out.Transcript = &Transcript{
			Path:           t.Path,
			SizeBytes:      t.SizeBytes,
			TailBytes:      t.TailBytes,
			Truncated:      t.Truncated,
			AgentColor:     t.AgentColor,
			ColorFound:     t.ColorFound,
			APIErrors:      t.APIErrors,
			Subagents:      copyIntPtr(t.Subagents),
			SubagentsKnown: t.SubagentsKnown,
			SessionKind:    copyStringPtr(t.SessionKind),
			ContextTokens:  copyInt64Ptr(t.ContextTokens),
			Model:          t.Model,
			Compactions:    t.Compactions,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// the free-text job fields
// ---------------------------------------------------------------------------

// JobTextWithheldPrefix begins the placeholder a withheld free-text job field
// carries instead of its content: "[withheld: 63 chars]".
//
// It is exported so a consumer can TELL, rather than guess, that it is holding
// a placeholder — and so a test can assert the content is gone without
// depending on the exact wording.
const JobTextWithheldPrefix = "[withheld: "

// RedactJobText replaces the free-text job fields — `needs` and `detail` — with
// a presence-and-length placeholder, in place, on every session of an envelope.
//
// # Why this exists
//
// jobsfs.NeverRead excludes `intent`, `output` and `suggestedReply` because
// they carry the user's own prompt and the agent's replies. `needs` and
// `detail` come out of the same record, and on this machine `needs` measured as
// "open new terminal and test `ck` command; report if it works or breaks" — a
// verbatim restatement of something the user typed. So the line the design drew
// did not survive contact with the data it was drawn about.
//
// The line is redrawn by AUDIENCE rather than by field, which is the
// distinction the data actually has:
//
//	local surfaces  (tt sessions, the TUI panel, the socket on a 0600 path in
//	                 a 0700 directory) show the user their own machine. Full
//	                 content. Withholding it there protects nobody from
//	                 anything.
//	the MCP surface hands data to OTHER AGENTS. Every Claude session on the
//	                 machine can reach `tt mcp`, so a free-text `needs` there
//	                 is a summary of what the user asked every other agent to
//	                 do, published to all of them.
//
// What survives redaction is everything an agent needs for "this session needs
// you": status, waitReason, blockedForMs, live, tmux, cwd — and the placeholder
// itself, which still says a need exists and how long it is. What does not
// survive is the sentence.
//
// One switch turns it off: `tt mcp --publish-job-text`.
func (s *Snapshot) RedactJobText() {
	if s == nil {
		return
	}
	for i := range s.Sessions {
		s.Sessions[i].Needs = WithheldJobText(s.Sessions[i].Needs)
		s.Sessions[i].Detail = WithheldJobText(s.Sessions[i].Detail)
	}
}

// WithheldJobText is RedactJobText on one value.
//
// The three cases are deliberately distinct on the wire, because a consumer
// acts differently on each: nil stays nil ("the record has no such field"),
// empty stays empty ("it has one and it is blank"), and anything else becomes
// "[withheld: N chars]" ("it has one, this long, and you are not being given
// it"). Collapsing the first two into the third would make every session look
// like it needed something.
func WithheldJobText(v *string) *string {
	if v == nil || *v == "" {
		return copyStringPtr(v)
	}
	out := fmt.Sprintf("%s%d chars]", JobTextWithheldPrefix, utf8.RuneCountInString(*v))
	return &out
}

// NewAttention is the only projection. It reads a wire.Session rather than a
// domain one so that the projection cannot show a value the JSON surfaces do
// not have.
func NewAttention(s Session) AttentionSession {
	return AttentionSession{
		SessionID:    s.SessionID,
		Name:         s.Name,
		Kind:         s.Kind,
		Status:       s.Status,
		WaitReason:   s.WaitReason,
		BlockedForMs: copyInt64Ptr(s.BlockedForMs),
		Needs:        copyStringPtr(s.Needs),
		Live:         copyBoolPtr(s.Live),
		Tmux:         s.Tmux,
		CWD:          s.CWD,
	}
}

func newAdvisories(in []observer.Advisory) []Advisory {
	if in == nil {
		return nil
	}
	out := make([]Advisory, 0, len(in))
	for _, a := range in {
		w := Advisory{Code: a.Code}
		if len(a.Fields) > 0 {
			w.Fields = make(map[string]any, len(a.Fields))
			for k, v := range a.Fields {
				w.Fields[k] = v
			}
		}
		out = append(out, w)
	}
	return out
}

// ---------------------------------------------------------------------------
// wire -> domain (H7's inverse)
// ---------------------------------------------------------------------------

// ErrFrameUnsupported is returned by ApplySessionFrame for a frame type it does
// not handle. It is a distinct error so a caller can tell "this frame is not a
// session delta" from "this frame is malformed".
var ErrFrameUnsupported = errors.New("wire: frame type is not a session delta")

// ToDomain is the inverse of NewSnapshot. A socket client hands frames here
// rather than decoding them itself: a second decoder of the domain, in reverse,
// is precisely the drift the one-encoder rule exists to prevent — and it would
// be the load-bearing component of the very agreement test it undermines.
//
// The returned advisories are decode-scoped notes about the DECODE, not about
// any session. Today the decoder produces none from an already-parsed struct
// (see DecodeSnapshot, which is where unknown fields can still be seen); the
// slice is part of the signature so that a future lossy case has somewhere to
// report itself instead of being silent.
func ToDomain(s Snapshot) (*observer.Snapshot, []Advisory) {
	out := &observer.Snapshot{
		Counts:     observer.NewCounts(),
		Warnings:   toDomainAdvisories(s.Warnings),
		NotCapable: copyStrings(s.NotCapable),
	}
	var notes []Advisory

	if t, err := observer.ParseTime(s.ObservedAt); err == nil {
		out.ObservedAt = t
	}
	out.Counts.Total = s.Counts.Total
	out.Counts.Live = s.Counts.Live
	out.Counts.Dead = s.Counts.Dead
	out.Counts.Unknown = s.Counts.Unknown
	out.Counts.Attention = s.Counts.Attention
	for k, v := range s.Counts.ByStatus {
		out.Counts.ByStatus[observer.Status(k)] = v
	}
	for k, v := range s.Counts.ByKind {
		out.Counts.ByKind[observer.Kind(k)] = v
	}
	if s.Sources != nil {
		out.Sources = make(map[string]observer.SourceReport, len(s.Sources))
		for id, src := range s.Sources {
			out.Sources[id] = toDomainSource(src)
		}
	}
	if s.Sessions != nil {
		out.Sessions = make([]observer.Session, 0, len(s.Sessions))
		for i := range s.Sessions {
			sess, n := ToDomainSession(s.Sessions[i])
			out.Sessions = append(out.Sessions, sess)
			notes = append(notes, n...)
		}
	}
	return out, notes
}

// ToDomainSession decodes one session. A timestamp that will not parse is
// reported as a note and left nil rather than defaulted to the zero time: a
// zero time would read as January 1st year 1 and compute an age of two
// millennia, which is worse than admitting we do not know.
func ToDomainSession(s Session) (observer.Session, []Advisory) {
	var notes []Advisory
	parse := func(field string, v *string) *time.Time {
		if v == nil {
			return nil
		}
		t, err := observer.ParseTime(*v)
		if err != nil {
			notes = append(notes, Advisory{
				Code:   observer.AdvWireUnknownField,
				Fields: map[string]any{"field": field},
			})
			return nil
		}
		return &t
	}

	out := observer.Session{
		SessionID:   s.SessionID,
		ShortID:     copyStringPtr(s.ShortID),
		PID:         copyIntPtr(s.PID),
		Kind:        observer.Kind(s.Kind),
		RawKind:     copyStringPtr(s.RawKind),
		Name:        s.Name,
		NameSource:  s.NameSource,
		FormerNames: copyStrings(s.FormerNames),
		CWD:         s.CWD,
		StartedAt:   parse("startedAt", s.StartedAt),

		Status:                observer.Status(s.Status),
		RawStatus:             s.RawStatus,
		StatusSource:          s.StatusSource,
		StatusSince:           parse("statusSince", s.StatusSince),
		StatusAgeMs:           copyInt64Ptr(s.StatusAgeMs),
		StatusSinceLowerBound: parse("statusSinceLowerBound", s.StatusSinceLowerBound),
		BlockedForMs:          copyInt64Ptr(s.BlockedForMs),
		BlockedForUnavailable: copyStringPtr(s.BlockedForUnavailable),
		WaitReason:            observer.WaitReason(s.WaitReason),
		RawWaitingFor:         copyStringPtr(s.RawWaitingFor),
		Attention:             s.Attention,
		LastActivityAt:        parse("lastActivityAt", s.LastActivityAt),

		Needs:           copyStringPtr(s.Needs),
		Detail:          copyStringPtr(s.Detail),
		Tempo:           copyStringPtr(s.Tempo),
		Tokens:          copyInt64Ptr(s.Tokens),
		Children:        copyStrings(s.Children),
		ResumeSessionID: copyStringPtr(s.ResumeSessionID),
		RecordUpdatedAt: parse("recordUpdatedAt", s.RecordUpdatedAt),
		RecordAgeMs:     copyInt64Ptr(s.RecordAgeMs),
		ReapedMidWorkAt: parse("reapedMidWorkAt", s.ReapedMidWorkAt),

		PaneAlive:  copyBoolPtr(s.PaneAlive),
		ProcAlive:  copyBoolPtr(s.ProcAlive),
		Live:       copyBoolPtr(s.Live),
		LiveReason: observer.LiveReason(s.LiveReason),
		ListedBy:   copyStrings(s.ListedBy),

		ClaudeVersion:       s.ClaudeVersion,
		MessagingSocketPath: copyStringPtr(s.MessagingSocketPath),
		SpawnedBy:           copyStringPtr(s.SpawnedBy),

		TranscriptCollected: s.TranscriptCollected,

		Sources:    copyStrings(s.Sources),
		Stale:      s.Stale,
		Advisories: toDomainAdvisories(s.Advisories),
	}
	if t, err := observer.ParseTime(s.ObservedAt); err == nil {
		out.ObservedAt = t
	}
	if s.Tmux != nil {
		out.Tmux = &observer.TmuxRef{
			Raw:     s.Tmux.Raw,
			Session: s.Tmux.Session,
			Window:  s.Tmux.Window,
			Pane:    s.Tmux.Pane,
		}
	}
	if s.Transcript != nil {
		t := s.Transcript
		out.Transcript = &observer.TranscriptFacts{
			Path:           t.Path,
			SizeBytes:      t.SizeBytes,
			TailBytes:      t.TailBytes,
			Truncated:      t.Truncated,
			AgentColor:     t.AgentColor,
			ColorFound:     t.ColorFound,
			APIErrors:      t.APIErrors,
			Subagents:      copyIntPtr(t.Subagents),
			SubagentsKnown: t.SubagentsKnown,
			SessionKind:    copyStringPtr(t.SessionKind),
			ContextTokens:  copyInt64Ptr(t.ContextTokens),
			Model:          t.Model,
			Compactions:    t.Compactions,
		}
	}
	return out, notes
}

func toDomainSource(s Source) observer.SourceReport {
	out := observer.SourceReport{
		ID:           s.ID,
		Role:         s.Role,
		Path:         s.Path,
		AgeMs:        copyInt64Ptr(s.AgeMs),
		OK:           s.OK,
		Stale:        s.Stale,
		StaleAfterMs: copyInt64Ptr(s.StaleAfterMs),
		LastError:    copyStringPtr(s.LastError),
		Advisories:   toDomainAdvisories(s.Advisories),
	}
	if s.ObservedAt != nil {
		if t, err := observer.ParseTime(*s.ObservedAt); err == nil {
			out.ObservedAt = &t
		}
	}
	if s.Counters != nil {
		out.Counters = make(map[string]int64, len(s.Counters))
		for k, v := range s.Counters {
			out.Counters[k] = v
		}
	}
	if s.Labels != nil {
		out.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			out.Labels[k] = v
		}
	}
	if s.Watch != nil {
		w := &observer.WatchReport{
			Mode:             s.Watch.Mode,
			ArmedDirs:        s.Watch.ArmedDirs,
			ArmedFiles:       s.Watch.ArmedFiles,
			ArmedTranscripts: s.Watch.ArmedTranscripts,
			ArmFailures:      s.Watch.ArmFailures,
			ArmIterations:    s.Watch.ArmIterations,
			Degraded:         copyStringPtr(s.Watch.Degraded),
			Events:           s.Watch.Events,
		}
		if s.Watch.LastEventAt != nil {
			if t, err := observer.ParseTime(*s.Watch.LastEventAt); err == nil {
				w.LastEventAt = &t
			}
		}
		out.Watch = w
	}
	return out
}

// toDomainAdvisories preserves every advisory verbatim, INCLUDING codes this
// build does not recognise. §3.7 promises codes may be added in a minor
// version, so a client one build behind must still be able to show them; a
// decoder that dropped the unknown ones would make every such addition
// invisible until the client was rebuilt.
func toDomainAdvisories(in []Advisory) []observer.Advisory {
	if in == nil {
		return nil
	}
	out := make([]observer.Advisory, 0, len(in))
	for _, a := range in {
		d := observer.Advisory{Code: a.Code}
		if len(a.Fields) > 0 {
			d.Fields = make(map[string]any, len(a.Fields))
			for k, v := range a.Fields {
				d.Fields[k] = v
			}
		}
		out = append(out, d)
	}
	return out
}

// ApplySessionFrame folds one session-delta frame into a local snapshot,
// in place. This is how a socket client maintains its own *observer.Snapshot
// without decoding anything itself.
//
// session_changed carries the WHOLE session, so applying it is a replace rather
// than a merge — which is the point of the redundancy: the alternative is every
// client implementing a merge, and at about a kilobyte against a measured three
// events per nine minutes the redundancy is free while a wrong merge is not.
func ApplySessionFrame(dst *observer.Snapshot, f Frame) ([]Advisory, error) {
	if dst == nil {
		return nil, errors.New("wire: ApplySessionFrame on a nil snapshot")
	}
	switch f.Type {
	case FrameSessionAdded, FrameSessionChanged:
		if f.Session == nil {
			return nil, fmt.Errorf("wire: %s frame carries no session", f.Type)
		}
		sess, notes := ToDomainSession(*f.Session)
		id := sess.SessionID
		if id == "" {
			return notes, fmt.Errorf("wire: %s frame carries a session with no id", f.Type)
		}
		// Identity is (sessionId, kind), the join's key (A14): a background
		// job's change must not overwrite the interactive session that shares
		// its id.
		for i := range dst.Sessions {
			if dst.Sessions[i].SessionID == id && dst.Sessions[i].Kind == sess.Kind {
				dst.Sessions[i] = sess
				return notes, nil
			}
		}
		dst.Sessions = append(dst.Sessions, sess)
		return notes, nil

	case FrameSessionRemoved:
		id := f.SessionID
		if id == "" {
			return nil, errors.New("wire: session_removed frame carries no sessionId")
		}
		// A removal names its kind (A14). One from a server that predates the
		// field names none, and falls back to the id alone — the old rule.
		for i := range dst.Sessions {
			if dst.Sessions[i].SessionID == id && (f.Kind == "" || string(dst.Sessions[i].Kind) == f.Kind) {
				dst.Sessions = append(dst.Sessions[:i], dst.Sessions[i+1:]...)
				return nil, nil
			}
		}
		// Removing a session we never had is not an error: a filtered
		// subscriber legitimately misses the add that preceded it.
		return nil, nil

	default:
		return nil, fmt.Errorf("%w: %q", ErrFrameUnsupported, f.Type)
	}
}

// DecodeSnapshot decodes raw JSON into a wire.Snapshot AND reports the fields
// it did not recognise.
//
// It exists because ToDomain's specified signature — taking an already-decoded
// Snapshot — structurally cannot report an unknown field: by the time the
// struct exists, encoding/json has already discarded anything that did not
// match a tag. The wire-unknown-field advisory therefore has to be raised at
// the point bytes become a struct, which is here. Callers that hold bytes
// should use this; ToDomain remains the pure struct-to-domain conversion.
//
// An unknown field is DROPPED and named. It is either additive — in which case
// dropping it loses information this build could not render anyway, and the
// advisory says so — or a schema violation, which the connection handshake
// refuses before any frame is read.
func DecodeSnapshot(data []byte) (Snapshot, []Advisory, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, nil, fmt.Errorf("wire: decode snapshot: %w", err)
	}
	notes := unknownFields(data, snapshotKnownFields())
	return s, notes, nil
}

// DecodeFrame is DecodeSnapshot's sibling for one NDJSON frame.
func DecodeFrame(data []byte) (Frame, []Advisory, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, nil, fmt.Errorf("wire: decode frame: %w", err)
	}
	notes := unknownFields(data, frameKnownFields())
	return f, notes, nil
}

// unknownFields compares the top-level keys actually present against the keys
// the struct knows, plus a recursive pass over the session objects, which is
// where a vendor-driven schema addition would realistically land.
func unknownFields(data []byte, known map[string]map[string]bool) []Advisory {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var notes []Advisory
	for k := range raw {
		if !known["root"][k] {
			notes = append(notes, Advisory{
				Code:   observer.AdvWireUnknownField,
				Fields: map[string]any{"field": k},
			})
		}
	}
	// Sessions, wherever they are: an array at root, or a single one under
	// "session".
	if arr, ok := raw["sessions"]; ok {
		var items []map[string]json.RawMessage
		if json.Unmarshal(arr, &items) == nil {
			notes = append(notes, unknownSessionFields(items, known["session"])...)
		}
	}
	if one, ok := raw["session"]; ok {
		var item map[string]json.RawMessage
		if json.Unmarshal(one, &item) == nil {
			notes = append(notes, unknownSessionFields([]map[string]json.RawMessage{item}, known["session"])...)
		}
	}
	// A snapshot FRAME nests the whole envelope under "snapshot" (see Frame's
	// doc comment for why it is nested). Recursing here is what makes an
	// addition inside that envelope as visible over the socket as it is in
	// `tt sessions --json`.
	if nested, ok := raw["snapshot"]; ok && known["snapshot"] != nil {
		var env map[string]json.RawMessage
		if json.Unmarshal(nested, &env) == nil {
			for k := range env {
				if !known["snapshot"][k] {
					notes = append(notes, Advisory{
						Code:   observer.AdvWireUnknownField,
						Fields: map[string]any{"field": "snapshot." + k},
					})
				}
			}
			if arr, ok := env["sessions"]; ok {
				var items []map[string]json.RawMessage
				if json.Unmarshal(arr, &items) == nil {
					notes = append(notes, unknownSessionFields(items, known["session"])...)
				}
			}
		}
	}
	return dedupeAdvisories(notes)
}

func unknownSessionFields(items []map[string]json.RawMessage, known map[string]bool) []Advisory {
	var notes []Advisory
	for _, item := range items {
		for k := range item {
			if !known[k] {
				notes = append(notes, Advisory{
					Code:   observer.AdvWireUnknownField,
					Fields: map[string]any{"field": "sessions[]." + k},
				})
			}
		}
	}
	return notes
}

func dedupeAdvisories(in []Advisory) []Advisory {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]Advisory, 0, len(in))
	for _, a := range in {
		key := a.Code
		if f, ok := a.Fields["field"].(string); ok {
			key += "\x00" + f
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}
