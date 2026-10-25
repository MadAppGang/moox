package observer

import "time"

// Advisory is a machine-readable note attached to a session, to a source, or to
// the snapshot as a whole. Code is one of the constants in advisories.go and is
// a contract: a code is never repurposed, may be added in a minor version, and
// is removed only with a wire schema bump.
//
// Fields carries the accompanying values named in the advisory registry
// (§3.7). Integer values MUST be stored as int64 and integer slices as []string
// / []int64, because Fields round-trips through JSON and the wire decoder
// normalises numbers back to int64. Storing an int or a float64 here will
// survive encoding but will not compare equal after a round trip.
type Advisory struct {
	Code   string
	Fields map[string]any
}

// TmuxRef is a parsed tmux pane reference as the registry records it, e.g.
// "madbench:@51.%107". The sigils are part of the identifiers and are kept
// (fact 7) — "@51" and "%107", not "51" and "107".
type TmuxRef struct {
	Raw     string
	Session string
	Window  string
	Pane    string
}

// TranscriptFacts is everything that is allowed to leave a transcript. It is
// deliberately a closed, narrow set: only AgentColor, APIErrors, Subagents,
// SessionKind, SizeBytes, the context size (ContextTokens), the model id,
// the compaction count and (separately) the newest timestamp ever cross this
// boundary. Counts, flags and bounded identifiers — no message content, no
// prompt text, no tool output. The test
// TestTranscriptFactsHasNoStringFieldThatCouldCarryAMessage pins every field's
// type and the exact set of string fields, each of which is bounded.
type TranscriptFacts struct {
	Path      string
	SizeBytes int64
	// TailBytes is how much of the file the enrichment pass actually read, and
	// Truncated says whether that tail stopped short of offset 0.
	TailBytes int
	Truncated bool
	// AgentColor is the last "agent-color" record's colour, or "" when none was
	// found in the tail. ColorFound distinguishes "no colour in this file" from
	// "we did not get far enough back to see one".
	AgentColor string
	ColorFound bool
	APIErrors  int
	// Subagents is nil when the subagents directory does not exist ("we cannot
	// say"), and a pointer to 0 when it exists and is empty ("none"). That
	// distinction is the whole of AC4 and is why this is not a plain int.
	Subagents      *int
	SubagentsKnown bool
	// SessionKind is the transcript's own "sessionKind" value (e.g. "bg") when
	// present. It corroborates Kind and never overrides it. Bounded by
	// transcriptfs.MaxSessionKindBytes.
	SessionKind *string
	// ContextTokens is the context the model last saw: input + cache_creation
	// + cache_read from the newest qualifying assistant usage in the tail, or
	// compactMetadata.postTokens after a newer compaction. nil = not
	// measurable: absent from the tail, or reset by a compaction that recorded
	// no postTokens.
	ContextTokens *int64
	// Model is that record's message.model, bounded by
	// transcriptfs.MaxModelBytes and charset-checked; "" when absent or
	// refused. An identifier, never content.
	Model string
	// Compactions counts compact_boundary records WITHIN THE TAIL: "compacted
	// recently", a lower bound on the session's total when Truncated is true.
	Compactions int
}

// Session is one Claude Code session — interactive or background — as the core
// understands it after the three-source join.
//
// Nullable fields are pointers on purpose. Absence is meaningful almost
// everywhere in this domain: a nil Live means "no evidence either way" and is
// not the same as a pointer to false; a nil BlockedForMs means "not applicable
// or not measurable" and is not the same as a pointer to 0. The wire codec
// bans omitempty precisely so those distinctions survive serialisation.
//
// WaitReason is the one deliberate non-pointer exception (§3.3): "" means "no
// reason applies" and WaitReasonUnknown means "waiting, reason undetermined",
// so a third null state would be meaningless. WaitReason is never null on the
// wire.
type Session struct {
	// ---- identity ----

	// SessionID is the join key. A UUID, present in all three sources on every
	// entry in every sample taken. Never derived from anything else.
	SessionID string
	// ShortID is background-only, by construction: only job records carry an
	// id. Nil for every interactive session.
	ShortID *string
	// PID is nil for background agents — they have no process of their own,
	// which is exactly why a pid cannot be the join key.
	PID     *int
	Kind    Kind
	RawKind *string

	Name        string
	NameSource  string
	FormerNames []string
	CWD         string
	StartedAt   *time.Time

	// ---- status ----

	Status Status
	// RawStatus is always populated, verbatim, from whichever field of
	// whichever source produced Status. Normalisation never destroys the input.
	RawStatus string
	// StatusSource names which source won: "registry", "jobs" or "agents-cli".
	StatusSource string

	// StatusSince is the instant the session entered its CURRENT status. From
	// the registry's statusUpdatedAt, or a job record's updatedAt, verbatim.
	// Never "corrected" against a transcript mtime.
	StatusSince *time.Time
	// StatusAgeMs is now - StatusSince. Nil exactly when StatusSince is nil.
	// Clock-derived, and therefore excluded from the diffed set.
	StatusAgeMs *int64
	// StatusSinceLowerBound is published when a job record was demonstrably
	// written after its thread went quiet, so StatusSince is known to
	// under-report the duration. It is a bound, not a correction.
	StatusSinceLowerBound *time.Time
	// BlockedForMs is a VIEW of StatusAgeMs: it equals StatusAgeMs when
	// Attention is true and is nil otherwise. It is never a second measurement.
	BlockedForMs *int64
	// BlockedForUnavailable carries the reason whenever Attention is true but
	// BlockedForMs is nil.
	BlockedForUnavailable *string

	WaitReason    WaitReason
	RawWaitingFor *string
	// Attention is the one derived boolean the core computes so the cheap
	// consumer question stays one field: Status is waiting or blocked. The
	// union is made once, here, rather than accidentally in each of four
	// adapters. A failed status is deliberately NOT attention.
	Attention bool
	// LastActivityAt is the timestamp of the newest timestamped record in this
	// session's transcript. An INDEPENDENT measurement of when the thread last
	// moved — never a fallback for StatusSince.
	LastActivityAt *time.Time

	// ---- background-only ----
	//
	// All nine serialise as null for an interactive session.

	Needs           *string
	Detail          *string
	Tempo           *string
	Tokens          *int64
	Children        []string
	ResumeSessionID *string
	// RecordUpdatedAt is when the job record was last written, reported
	// alongside RecordAgeMs so a consumer can see how wide the staleness window
	// could be without the core asserting something false.
	RecordUpdatedAt *time.Time
	RecordAgeMs     *int64
	ReapedMidWorkAt *time.Time

	// ---- liveness ----
	//
	// Live, ProcAlive and PaneAlive are each tri-state. nil always means "no
	// evidence", never "false", and is never produced by a zero value.

	Tmux      *TmuxRef
	PaneAlive *bool
	ProcAlive *bool
	Live      *bool
	// LiveReason names the deciding rule, or "".
	LiveReason LiveReason
	// Foreign marks a live session whose process does not descend from any
	// pane of THIS tmux server (moox §4.5): it belongs to another server.
	// PaneAlive is nil for it, it is never reported dead, and it is hidden
	// from every view (matchesFilter). Not on the wire: a published set never
	// contains one.
	Foreign bool
	// ListedBy is MEMBERSHIP — which sources currently list this session. It
	// says nothing about liveness; that is what Live is for.
	ListedBy []string

	// ---- provenance ----

	ClaudeVersion       string
	MessagingSocketPath *string
	// SpawnedBy is the sessionId of the live session whose process is an
	// ancestor of this one's, or nil when no such session was found — a
	// session launched BY another session (an MCP helper's headless child, a
	// terminal-mux isolated slot) rather than by a person. Nothing in a
	// registry record or an agents listing says so; the process tree is the
	// only evidence, which is why spawned.go walks it. A set value hides the
	// session from every view whose Filter does not ask for IncludeSpawned.
	//
	// Diffed, not an identity field: it changes when the launching session
	// dies (the child is orphaned and becomes visible) or when a walk that
	// errored last rebuild completes on this one.
	SpawnedBy *string

	// TranscriptCollected distinguishes "we did not look" (false) from "we
	// looked and found nothing" (true with a nil Transcript).
	TranscriptCollected bool
	Transcript          *TranscriptFacts

	// Sources lists the sources that contributed to this record, in the fixed
	// join order: registry, jobs, agents-cli, transcript.
	Sources []string
	// Stale is clock-derived and excluded from the diffed set.
	Stale      bool
	ObservedAt time.Time
	Advisories []Advisory
}

// Counts is the snapshot envelope's tally. The invariant Total == Live + Dead +
// Unknown is asserted by a test: an unknown-liveness session has a home here,
// which is what stops it being silently dropped.
//
// ByStatus and ByKind always carry every known key, including zeros. A map
// needs its own rule because the omitempty ban only covers struct tags.
type Counts struct {
	Total     int
	Live      int
	Dead      int
	Unknown   int
	Attention int
	ByStatus  map[Status]int
	ByKind    map[Kind]int
}

// NewCounts returns a Counts whose ByStatus and ByKind carry every known key at
// zero, so a caller can increment without checking for presence and the encoded
// form never varies by which statuses happened to occur.
func NewCounts() Counts {
	byStatus := make(map[Status]int, len(AllStatuses))
	for _, s := range AllStatuses {
		byStatus[s] = 0
	}
	byKind := make(map[Kind]int, len(AllKinds))
	for _, k := range AllKinds {
		byKind[k] = 0
	}
	return Counts{ByStatus: byStatus, ByKind: byKind}
}

// WatchReport is the per-source watch accounting surfaced as
// sources.<id>.watch. Mode is "kqueue", "on-demand", "via-socket" or
// "nowatch", so the choice is visible rather than implicit.
type WatchReport struct {
	Mode             string
	ArmedDirs        int
	ArmedFiles       int
	ArmedTranscripts int
	ArmFailures      int
	// ArmIterations is §6.3's arm/scan fixed point: 1 in the steady state, 2 on
	// the first rebuild, higher only while the tree is changing.
	ArmIterations int
	Degraded      *string
	LastEventAt   *time.Time
	Events        int64
}

// SourceReport is what one source has to say about itself. A source failure is
// a value here, not a returned error that kills the response.
//
// Counters and Labels carry the per-source metrics whose key sets differ by
// source (registry: records, parseErrors, ignoredKeyFiles; jobs: entries, dirs,
// records, nonTerminal; agents-cli: spawns, lastSpawnMs, entries, and the
// string labels command / agentsCache / trigger; tmux: panes). They are split
// by value type rather than held in a map[string]any so the wire codec's
// inverse is exact — see the note in wire.go.
type SourceReport struct {
	ID         string
	Role       string
	Path       string
	ObservedAt *time.Time
	// AgeMs is clock-derived and excluded from the diffed set.
	AgeMs        *int64
	OK           bool
	Stale        bool
	StaleAfterMs *int64
	LastError    *string
	Advisories   []Advisory
	Watch        *WatchReport
	Counters     map[string]int64
	Labels       map[string]string
}

// Snapshot is one immutable answer about every session on the machine.
// Snapshots are never mutated after publication; the observer publishes a new
// *Snapshot through an atomic pointer and readers load it without a lock.
type Snapshot struct {
	// Seq is monotonic per publishing observer. It is transport metadata rather
	// than an observation, so it is not carried by wire.Snapshot and does not
	// round-trip through wire.ToDomain.
	Seq        int64
	ObservedAt time.Time
	Counts     Counts
	Sources    map[string]SourceReport
	// Warnings holds the source-scoped advisories, promoted once from
	// Sources[id].Advisories. Session-scoped advisories are NEVER promoted.
	Warnings []Advisory
	Sessions []Session
	// NotCapable is the declared list of things this server will not do, so a
	// consumer can see it will never get a frame at the instant a time
	// threshold is crossed rather than waiting for one.
	NotCapable []string
}

// SessionByKey returns the session with the given (id, kind) — the identity the
// join keeps and the stream now uses (A14) — and whether it was found.
func (s *Snapshot) SessionByKey(id string, kind Kind) (Session, bool) {
	if s == nil {
		return Session{}, false
	}
	for _, sess := range s.Sessions {
		if sess.SessionID == id && sess.Kind == kind {
			return sess, true
		}
	}
	return Session{}, false
}

// SessionByID returns the session with the given id and whether it was found.
func (s *Snapshot) SessionByID(id string) (Session, bool) {
	if s == nil {
		return Session{}, false
	}
	for _, sess := range s.Sessions {
		if sess.SessionID == id {
			return sess, true
		}
	}
	return Session{}, false
}
