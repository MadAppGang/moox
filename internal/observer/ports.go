package observer

import (
	"context"
	"time"
)

// This file holds the ports: seven interfaces the core OWNS and calls out
// through (driven), and one the core implements and adapters call in through
// (incoming).
//
// The rule that keeps the boundary honest: a port must never return the
// adapter's vocabulary. RegistrySource returns []RegistryRecord — a typed
// struct in the core's words — not []json.RawMessage, not fs.DirEntry, not an
// *os.File. Watcher returns "a watched thing changed", not a kevent, so a
// Linux/inotify implementation can differ completely. Reconcile takes
// core-vocabulary WatchTargets, not paths-plus-flags.
//
// Therefore: this file imports context and time, and nothing else. A reviewer
// finding a syscall, unix, exec, net or os type here has found a real defect.

// ---------------------------------------------------------------------------
// 1. RegistrySource — ~/.claude/sessions/*.json
// ---------------------------------------------------------------------------

// RegistryRecord is one interactive session as the registry records it. Raw
// fields are kept raw: Status and WaitingFor are the vendor's literals, mapped
// by MapRegistryStatus rather than by the adapter, so an unrecognised value
// reaches the one place that knows the pass-through rule.
type RegistryRecord struct {
	// Path is the file this came from, for advisories. Not a handle.
	Path string
	PID  int
	// SessionID is the join key. A record without one is unusable and the
	// adapter reports it as a parse error rather than inventing one.
	SessionID string

	Status     string
	WaitingFor string
	// StatusUpdatedAt means "in this status SINCE", not "last written". It is
	// taken verbatim and never corrected against a transcript mtime.
	StatusUpdatedAt *time.Time

	Name        string
	NameSource  string
	FormerNames []string
	CWD         string
	StartedAt   *time.Time

	// Tmux is the raw "session:@window.%pane" reference, sigils included, or ""
	// when the record carries none. It is NOT structurally guaranteed to be
	// present.
	//
	// It is carried VERBATIM because that is the string the pane gate compares
	// against `tmux list-panes` output. Nothing reassembles it from TmuxParts:
	// the sigils are part of the identifiers (measured fact 7) and a
	// round-tripped reconstruction is exactly how that fails silently.
	Tmux string
	// TmuxParts is Tmux split into its three identifiers, for display. It is
	// nil when the record carries no reference, and non-nil with empty parts
	// when the reference is present but not in the documented shape — a case
	// that must stay distinguishable from absence, because a reference we
	// cannot parse is still a reference we must not vouch for.
	TmuxParts           *TmuxRef
	Version             string
	MessagingSocketPath string

	// Kind is the vendor's literal, "" when the record carries none. The
	// registry is NOT interactive-only: a background session's own process
	// registers itself with kind "bg" and the job it serves in JobID
	// (measured 2026-09-23 on 2 of 45 live records). Such a record is the
	// same session as that job's record, and the join merges the two.
	Kind  string
	JobID string
}

// RegistryKindBackground is the registry's literal for a background
// session's own process. It differs from the domain's KindBackground
// ("background") and from agents-cli's spelling, which is why it is named.
const RegistryKindBackground = "bg"

// RecordFailure names a file that could not be read or parsed, so the core can
// KEEP the previous known state for whatever session that file described rather
// than treating the failure as "session gone" (§6.5). Path is the handle: the
// core matches it against the paths of the records it read last time, which is
// why RegistryRecord carries Path at all.
//
// Bytes is how much was read before the failure, for the debug log. The bytes
// themselves are deliberately not carried — a registry record contains a cwd
// and a session name, and a failure path is not a reason to start copying
// content around.
type RecordFailure struct {
	Path  string
	Bytes int
	Error string
}

// RegistryResult is one read of the registry directory, with its accounting.
// A read that partly failed is still a result: Err carries what went wrong and
// Records carries what was readable.
type RegistryResult struct {
	Records    []RegistryRecord
	ObservedAt time.Time
	// ParseErrors counts records that could not be parsed after a retry.
	ParseErrors int
	// IgnoredKeyFiles counts the sibling .key files the registry keeps beside
	// each session record. They are reported so the file count reconciles.
	IgnoredKeyFiles int
	// Failures names the files that were still unreadable after their retry.
	// One malformed record must never blank the whole set, so these are
	// reported alongside the records that DID read rather than replacing them.
	Failures []RecordFailure
	// RetriesDeferred counts files that failed and were not retried because the
	// per-rebuild retry bound was already spent. They keep their previous state
	// and are re-read on the next event.
	RetriesDeferred int
	// Absent is true when the registry directory does not exist at all, which
	// is the state of a machine where Claude has never run. It is a fact, not
	// an error: Err stays nil and Records is empty.
	Absent bool
	Err    error
}

// RegistrySource reads the interactive session registry.
type RegistrySource interface {
	// Read returns every readable registry record. Records are returned sorted
	// by pid ascending — a stable identity order, never map-enumeration order,
	// because the join's duplicate rule depends on "first wins" being
	// deterministic.
	Read(ctx context.Context) (RegistryResult, error)
}

// ---------------------------------------------------------------------------
// 2. JobsSource — ~/.claude/jobs/<shortId>/state.json, depth 1 only
// ---------------------------------------------------------------------------

// JobRecord is one background agent as its state.json records it.
//
// Note what is absent and deliberately so: no Intent, no Output, no timeline
// text. Those carry full user prompts and assistant messages, and this is a
// status observer. The record also carries no kind key at all — "background" is
// our word for where it was found, not the record's claim.
type JobRecord struct {
	// Dir is the job directory name, used only to FIND the record.
	Dir string
	// Path is the state.json this came from, for advisories and for §6.5's
	// keep-previous lookup. NOT a handle: nothing reopens it, and the session
	// id is never derived from it.
	Path string
	// DaemonShort is the record's own short id. It is observed to equal both
	// Dir and SessionID[:8], and neither is relied on: a mismatch raises
	// shortid-mismatch and the record is still used.
	DaemonShort string
	// SessionID, from INSIDE the record, is what joins. Never derived from Dir.
	SessionID string

	// State is the vendor's literal, mapped by MapJobState.
	State  string
	Detail string
	Needs  string
	// Tempo is a second vendor label. When it contradicts State, liveness
	// answers null rather than picking one.
	Tempo string

	Tokens *int64
	CWD    string
	// CWDGone is the adapter's CONFIRMED statement that CWD does not exist: a
	// stat answered "no such file". A refused stat, a relative path or an
	// adapter that cannot look (a test fs.FS) leaves it false, which is "no
	// opinion", never "present".
	CWDGone    bool
	Name       string
	NameSource string
	CLIVersion string
	// Children is the record's `children` entries, reduced to one string each,
	// and nil when the key is absent.
	//
	// MEASURED, and it is not what the architecture assumed: the entries are
	// ARTIFACTS the job produced, not child sessions. The one real record
	// carrying any holds {"id":"158","href":"https://…/pull/158","kind":"pr"}.
	// The adapter therefore publishes the href (or "kind:id" when there is
	// none), never a bare id, because a bare id in a field documented as
	// "child session ids" is a plausible-looking wrong answer.
	Children []string

	CreatedAt *time.Time
	// UpdatedAt is the last recorded event and is what StatusSince comes from.
	// It errs LATE, never early, which under-reports a blocked duration rather
	// than over-reporting it.
	UpdatedAt       *time.Time
	ReapedMidWorkAt *time.Time

	// LinkScanPath is the absolute path of this job's transcript, from the
	// record. A record is untrusted input: the adapter validates this against
	// the projects root and refuses anything outside it.
	LinkScanPath    string
	ResumeSessionID string
}

// JobsResult is one read of the jobs tree, with the four counts that let any
// two be reconciled. Entries is every directory entry directly under jobs/
// (including the pins.json file); Dirs is the subset that are directories;
// Records is the subset of Dirs carrying a depth-1 state.json; NonTerminal is
// the subset of Records whose state is neither done nor failed.
type JobsResult struct {
	Records     []JobRecord
	ObservedAt  time.Time
	Entries     int
	Dirs        int
	NonTerminal int
	ParseErrors int
	// Failures names the records that would not parse, so that one malformed
	// state.json is a counted failure beside the records that DID read rather
	// than something that blanks the background population. Symmetrical with
	// RegistryResult.Failures, and what P5 needs for §6.5's rule on this tree.
	Failures []RecordFailure
	// Absent is true when the jobs directory does not exist at all, which is
	// the state of a machine that has never run a background agent. It is a
	// fact, not an error: Err stays nil and Records is empty.
	Absent bool
	Err    error
}

// JobsSource reads the background job tree.
type JobsSource interface {
	// Read returns every readable job record, sorted by directory name
	// ascending — the stable identity order the duplicate rule needs.
	Read(ctx context.Context) (JobsResult, error)
}

// ---------------------------------------------------------------------------
// 3. TranscriptSource — ~/.claude/projects/<slug>/*.jsonl
// ---------------------------------------------------------------------------

// TranscriptLocation is a located transcript. It is a description, not a
// handle: no adapter hands the core an open file.
type TranscriptLocation struct {
	// Found is false when no transcript exists for this session. The zero
	// TranscriptLocation is therefore "not found", which is the safe default.
	Found bool
	Path  string
	// SizeBytes and ModTime are the enrichment cache key, together with the
	// session id and the path.
	SizeBytes int64
	ModTime   time.Time
	// Ambiguous is true when the locator matched more than one file. The core
	// then reports no transcript and raises transcript-ambiguous: guessing
	// which of two files belongs to a session is exactly the wrong answer.
	Ambiguous bool
	Matches   []string
}

// TimingFacts is the result of the bounded timing tail read — the correctness
// input to StatusSince's cross-check, and a much smaller read than enrichment.
type TimingFacts struct {
	// LastActivityAt is the newest timestamped record in the tail. Nil when the
	// tail held no timestamped record at all.
	LastActivityAt *time.Time
	// TailBytes is how much was read, and ReachedStart says whether the tail
	// reached offset 0. A nil LastActivityAt with ReachedStart false is
	// activity-since-truncated; with ReachedStart true it means the file
	// genuinely has no timestamps.
	TailBytes    int
	ReachedStart bool
}

// TranscriptSource reads bounded tails of session transcripts. It has two
// locators because the two populations are reached differently: an interactive
// session by glob over the cwd-slug directory, a background job by the absolute
// LinkScanPath its record carries. Same adapter, same tail reader, same cache,
// so a background job costs exactly what an interactive one costs.
//
// No adapter other than this one ever reads a transcript.
type TranscriptSource interface {
	// FindByGlob locates an interactive session's transcript by session id.
	// More than one match returns Ambiguous with Matches populated and Found
	// false.
	FindByGlob(ctx context.Context, sessionID string) (TranscriptLocation, error)

	// OpenPath locates a background job's transcript from its record's
	// LinkScanPath. The path is validated against the projects root; anything
	// outside it is refused with a not-found result, and NO glob fallback is
	// attempted — a record is untrusted input and a fallback would turn a
	// refused path into a neighbouring session's transcript.
	OpenPath(ctx context.Context, path string) (TranscriptLocation, error)

	// ReadTiming reads the bounded timing tail (64 KiB in production) and
	// scans backwards for the newest record carrying a timestamp. Eight record
	// types carry none, and on most job transcripts the final line is one of
	// them, so the last line is not the answer.
	ReadTiming(ctx context.Context, loc TranscriptLocation, tailBytes int) (TimingFacts, error)

	// ReadFacts is the enrichment pass (200 KiB in production): agent colour,
	// api error count, subagent count. It is never part of a watch-driven
	// rebuild.
	ReadFacts(ctx context.Context, loc TranscriptLocation, tailBytes int) (TranscriptFacts, error)
}

// ---------------------------------------------------------------------------
// 4. AgentsSource — `claude agents --json`
// ---------------------------------------------------------------------------

// AgentsEntry is one entry from the vendor's own listing. It covers both
// populations: an interactive entry carries PID and Status, a background entry
// carries ID and State.
type AgentsEntry struct {
	SessionID string
	// ID is the short id, present on background entries only.
	ID  string
	PID *int

	Kind       string
	Status     string
	State      string
	WaitingFor string

	Name      string
	CWD       string
	StartedAt *time.Time
}

// AgentsResult is one corroboration listing. It is the only positive liveness
// signal available for a background job, so its age matters as much as its
// contents: a listing older than StaleAfterMs cannot speak for the present.
type AgentsResult struct {
	Entries    []AgentsEntry
	ObservedAt time.Time
	// Command is the exact command run, reported so the choice is visible.
	Command string
	// SpawnMs is how long the spawn took, or 0 on a cache hit.
	SpawnMs int64
	// Cached is true when this result came from the cross-process cache rather
	// than from a spawn.
	Cached bool
	// Cache names which of §4.3(b)'s five outcomes produced this result:
	// AgentsCacheHit, AgentsCacheMiss, AgentsCacheHitAfterWait,
	// AgentsCacheContended or AgentsCacheUnwritable. It is "" for a source that
	// keeps no cache (a fake). ADDED BY P4, which owns §4.3(b): the five
	// outcomes are reported as sources.agents-cli.labels.agentsCache, and
	// Cached alone cannot distinguish the three that took no spawn.
	Cache string
	// WaitedMs is how long a contended caller waited on the cross-process lock
	// before answering, and 0 on every other path. ADDED BY P4: it is the field
	// of the agents-cache-contended advisory (§3.7).
	WaitedMs int64
	// OK is false when the listing could not be taken at all. A false OK makes
	// every cross-check in the liveness policy answer null rather than guess.
	OK  bool
	Err error
}

// Lists reports whether the listing names this session id.
func (r AgentsResult) Lists(sessionID string) bool {
	for _, e := range r.Entries {
		if e.SessionID == sessionID {
			return true
		}
	}
	return false
}

// AgentsSource takes a corroboration listing. It is a bootstrap and
// cross-check source; no reported status depends on it, and it never wins a
// precedence contest. Its one exclusive contribution is EXISTENCE for an
// interactive session that has no registry file.
type AgentsSource interface {
	// List returns the current listing, spawning only if the cache, the
	// cross-process lock and the backoff all allow it. Entries are sorted by
	// session id ascending.
	List(ctx context.Context) (AgentsResult, error)
}

// ---------------------------------------------------------------------------
// 5. PanesLister — `tmux list-panes -a -F …`
// ---------------------------------------------------------------------------

// PanesLister answers "does this pane still exist", which is half of what
// interactive liveness needs — the other half being a live pid.
type PanesLister interface {
	// ListPanes returns every pane id currently known, in the exact
	// "#{session_name}:#{window_id}.#{pane_id}" form, sigils INCLUDED. One call
	// covers the whole server, and the scope is server-wide on purpose: a
	// session's pane can be in any window.
	//
	// On failure the core must NOT reuse a previous pane set — a cached pane
	// set is exactly the stale evidence that would report a dead session as
	// live. PaneAlive becomes nil instead.
	ListPanes(ctx context.Context) (map[string]bool, error)
}

// PanePIDLister is the optional half of the pane gate the foreign-server rule
// needs (moox §4.5): the same one listing, plus every pane's pane_pid. An
// adapter that implements it is asked INSTEAD of ListPanes when a
// MembershipSource is wired, so a rebuild still costs one tmux exec.
type PanePIDLister interface {
	ListPanePIDs(ctx context.Context) (panes map[string]bool, panePIDs []int, err error)
}

// MembershipSource answers "is this registered Claude process running inside
// THIS tmux server" (moox §4.5, the foreign-server rule). The registry's tmux
// ref (`sess:@win.%pane`) names no socket, so a session of another server whose
// ref happens to read like one of ours — or whose pane is simply not in our
// listing — would otherwise be reported dead or, worse, matched to the wrong
// pane. Process ancestry decides instead: one process-table read per rebuild,
// climbing UP from the registered pid to one of this server's pane pids.
//
// Optional: a nil source is today's behaviour (pane set only).
type MembershipSource interface {
	// Membership takes ONE process-table read and returns a judge over it,
	// bound to this server's pane pids. An error means "could not tell".
	Membership(ctx context.Context, panePIDs []int) (Membership, error)
}

// Membership is one rebuild's view of the process table.
type Membership interface {
	// Owns reports whether pid climbs (≤24 hops) to one of this server's pane
	// pids. An error means "could not tell" and is never read as an answer.
	Owns(pid int) (bool, error)
	// Recycled reports whether pid's start token disagrees with the one its
	// by-pid registry entry recorded (PR-M16): the pid is now somebody else's
	// process, so the registered session is DEAD. No token recorded means
	// "trusted, as before" and answers false.
	Recycled(pid int) bool
}

// ---------------------------------------------------------------------------
// 6. ProcLive — kill(pid, 0)
// ---------------------------------------------------------------------------

// ProcLive answers "is this process still there". The error return is not
// decoration: an errored gate answers nil liveness with an advisory, which is a
// different outcome from a gate that answered false.
//
// It also answers "who started this process", which is how a session launched
// BY another session is told apart (spawned.go). The two questions share a
// port because they share a subject and a rule: both are one read-only kernel
// query about a pid, neither may fork a child to answer it, and neither error
// is evidence of anything.
type ProcLive interface {
	Alive(pid int) (bool, error)
	// Parent returns pid's parent pid. An error means "could not tell" — the
	// process is gone, or the platform has no way to ask — and the core must
	// never read it as an answer: a session is hidden as another's child only
	// on a walk that positively reached the other session's pid.
	Parent(pid int) (int, error)
}

// ---------------------------------------------------------------------------
// 7. Watcher — kqueue on darwin, a no-op elsewhere
// ---------------------------------------------------------------------------

// TargetKind is the family a watch target belongs to, so per-family arm counts
// can be reported and so the core's desired set is expressible in its own
// words.
type TargetKind int

const (
	// TargetClaudeRoot is ~/.claude, armed ONLY while jobs/ is absent — it is
	// how the first job directory is noticed, and arming it permanently would
	// wake us for everything else in that tree.
	TargetClaudeRoot TargetKind = iota
	// TargetRegistryDir is ~/.claude/sessions.
	TargetRegistryDir
	// TargetRegistryFile is ~/.claude/sessions/<pid>.json. Per-FILE arming is
	// required, not an optimisation: the registry is rewritten IN PLACE, and a
	// directory vnode receives nothing on an in-place write.
	TargetRegistryFile
	// TargetJobsRoot is ~/.claude/jobs.
	TargetJobsRoot
	// TargetJobDir is ~/.claude/jobs/<shortId> — EVERY one, unconditionally. A
	// terminal job's directory stays watched, because a job can resume.
	TargetJobDir
	// TargetJobFile is ~/.claude/jobs/<shortId>/state.json.
	TargetJobFile
	// TargetJobTranscript is the LinkScanPath target of a job record.
	TargetJobTranscript
)

// String names the target kind for reporting.
func (k TargetKind) String() string {
	switch k {
	case TargetClaudeRoot:
		return "claude-root"
	case TargetRegistryDir:
		return "registry-dir"
	case TargetRegistryFile:
		return "registry-file"
	case TargetJobsRoot:
		return "jobs-root"
	case TargetJobDir:
		return "job-dir"
	case TargetJobFile:
		return "job-file"
	case TargetJobTranscript:
		return "job-transcript"
	default:
		return "unknown"
	}
}

// AllTargetKinds is every target family, for per-kind reporting.
var AllTargetKinds = []TargetKind{
	TargetClaudeRoot, TargetRegistryDir, TargetRegistryFile,
	TargetJobsRoot, TargetJobDir, TargetJobFile, TargetJobTranscript,
}

// WatchTarget is one thing the core wants watched.
type WatchTarget struct {
	Kind TargetKind
	Path string
}

// ArmFailure names a target that could not be armed. Counting failures without
// saying which made a systematically unarmable target invisible.
type ArmFailure struct {
	Path  string
	Error string
}

// ArmReport is what Reconcile achieved. The adapter owns descriptors and
// decides what it can afford; the shortfall is reported here rather than
// silently dropped.
type ArmReport struct {
	Armed    int
	Released int
	Failed   int
	// ByKind is the ARMED SET by family as of the report. The loop derives it
	// from what it holds wherever it stores a report, so the one published with
	// a snapshot describes that publication (code review round 2, G3).
	ByKind   map[TargetKind]int
	Failures []ArmFailure
	// Iterations is how many enumerate/arm rounds this rebuild took: 1 in the
	// steady state, 2 on the first rebuild, higher only while the tree is
	// changing.
	Iterations int
	// Degraded is "", "file-watch-cap", "job-dir-cap", "kqueue-unavailable" or
	// "arm-scan-unconverged".
	Degraded string
}

// Change is a coalesced hint that something changed. It says only THAT
// something changed — never what, and never a kernel event type. Deltas are
// computed by diffing two snapshots, which is what lets this port stay coarse
// and lets a Linux implementation differ completely.
type Change struct {
	Kind TargetKind
	// Ref is the path that hinted. It is never parsed. The one decision it
	// feeds is Invariant B's fifth predicate: a TargetJobTranscript hint names
	// the transcript whose cached timing is stale (code review round 2, A3).
	// Nothing else is re-read on its word — every rebuild reads the whole tree.
	Ref string
	At  time.Time
}

// Watcher arms and releases watches and delivers change hints.
type Watcher interface {
	// Watch delivers coalesced change hints until ctx is done, then closes the
	// channel.
	Watch(ctx context.Context) (<-chan Change, error)

	// Reconcile makes the armed set equal to targets: it arms what is new,
	// releases what is gone, and LEAVES THE REST UNTOUCHED. That last clause is
	// a correctness requirement rather than an optimisation — closing a
	// descriptor discards every event already queued on it, so a gratuitous
	// close-then-open of an unchanged target is a data-loss bug.
	//
	// Idempotent, and cheap to call repeatedly with a GROWING set. It is called
	// INTERLEAVED WITH the rebuild, never once at the end: arm-only waves each
	// precede the read they protect, and one final call carrying the true
	// desired set is the only call that RELEASES anything. An arm landing after
	// the read it was meant to protect loses, permanently and with no later
	// event, anything that materialised in between.
	//
	// Always the full desired set — never a delta — so the core never has to
	// remember what the adapter holds.
	//
	// A held target whose VNODE was replaced underneath its path (a
	// rename-publish) is re-armed by Reconcile: the old descriptor is deaf from
	// the replacement on. That is why Reconcile is an ARMING operation even when
	// its argument is a subset of what is armed, and why the post-publication
	// phase must not call it (code review round 2, A4) — see Release.
	Reconcile(ctx context.Context, targets []WatchTarget) (ArmReport, error)

	// Release closes every held target NOT in keep, and ARMS NOTHING: not a new
	// target, and not a replacement vnode for a kept one. It is the only thing
	// the loop calls after a publication, so "no arm follows the read" is a
	// property of the operation rather than of its argument. A kept target
	// whose vnode was replaced stays on its old descriptor until the next
	// rebuild's identity pass — which runs BEFORE that rebuild's read — and the
	// replacement itself fired the directory watch that causes that rebuild.
	Release(ctx context.Context, keep []WatchTarget) (ArmReport, error)
}

// ---------------------------------------------------------------------------
// The one incoming port
// ---------------------------------------------------------------------------

// SnapshotOptions is what a consumer asks for.
type SnapshotOptions struct {
	// IncludeDead adds the live == false set. Default filtering is
	// live != false, so a session vanishing from the default view requires
	// POSITIVE evidence of death.
	IncludeDead bool
	// IncludeUnknownLiveness defaults to TRUE: an unknown-liveness session is
	// never hidden by the default filter.
	IncludeUnknownLiveness bool
	// IncludeTranscripts runs the 200 KiB enrichment pass. It is false for
	// every watch-driven rebuild, always. It does NOT govern the 64 KiB timing
	// tail, which is a correctness input to StatusSince rather than enrichment.
	IncludeTranscripts bool
	// NoTranscriptReads is `tt sessions --no-transcript`: read NO transcript at
	// all — not the enrichment pass, and not the 64 KiB timing tail either.
	//
	// It is a second option rather than IncludeTranscripts widened, because the
	// two answer different questions. IncludeTranscripts:false is every
	// surface's cheap default (MCP's, the watch loop's) and must keep a job's
	// timing, which is a correctness input. This one is §5.2's public flag
	// contract, which the black-box suite held the build to: transcriptCollected
	// false on EVERY session, lastActivityAt null, a job's statusSince carrying
	// status-since-unverifiable, and §3.5 rule 4 deciding on recordAgeMs alone
	// because presence was never checked. The narrowing is reported, not silent.
	NoTranscriptReads bool
	// ChangedTranscripts are the job transcripts whose watch fired since the
	// previous rebuild: Invariant B's fifth predicate (TimingTriggerEvent). The
	// watch loop fills it, from the coalescing window it just drained; every
	// other caller leaves it empty, which is correct for a one-shot read (the
	// other four predicates carry the burden when nothing is watching).
	//
	// Code review round 2, A3: nothing filled it, so that predicate was
	// unreachable in production and a transcript append on a job whose
	// state.json had not moved was served from the timing cache — a working
	// job drifted into job-working-stale while it was still writing.
	ChangedTranscripts []string
	// RereadTranscripts treats EVERY job transcript as changed. It is for a
	// long-lived caller that can no longer tell which ones changed — the watch
	// loop after its watch has degraded (A8) — where a cached timing read would
	// otherwise stand indefinitely.
	RereadTranscripts bool
	// Filter narrows the session set. The zero Filter matches everything.
	Filter Filter
}

// DefaultSnapshotOptions is the shape a consumer gets when it asks for nothing:
// no dead sessions, unknown liveness included, no enrichment.
func DefaultSnapshotOptions() SnapshotOptions {
	return SnapshotOptions{IncludeUnknownLiveness: true}
}

// Filter narrows a session set. Empty slices and an empty CWDPrefix match
// everything. The zero Filter is the DEFAULT view rather than "no filter":
// it drops the dead (see IncludeDead) and the launched (see IncludeSpawned),
// and a caller that wants the whole population asks for both by name.
type Filter struct {
	Kinds     []Kind
	Statuses  []Status
	CWDPrefix string
	// WaitingOnly keeps only sessions where Attention is true.
	WaitingOnly bool
	// IncludeDead keeps sessions with live == false in view. A subscriber that
	// sets it is told of a death as a CHANGE (it keeps the session, and can
	// show it as ended) and of a removal only when the record itself is gone
	// (code review round 2, A10). The zero value is the default view: live !=
	// false, unknown liveness included.
	IncludeDead bool
	// IncludeSpawned keeps sessions with SpawnedBy set — sessions another live
	// session launched — in view. The default drops them: a headless child
	// that an MCP helper started on its own pty is the parent's working part,
	// not a session a person can attach to, and the panel drew every one of
	// them as a RUN row under "no tmux".
	//
	// It is a Filter field, not a SnapshotOptions one, so that it is applied
	// by matchesFilter — the predicate the one-shot snapshot, View, VisibleIDs
	// and Diff all share — and a socket subscriber and a direct caller cannot
	// disagree about which children exist. A separate flag rather than
	// IncludeDead widened, because the panel asks for the dead (to show ENDED
	// rows) and must still not be shown the children.
	IncludeSpawned bool
}

// EventType is the kind of change a subscriber is told about.
type EventType string

const (
	EventSnapshot       EventType = "snapshot"
	EventSessionAdded   EventType = "session_added"
	EventSessionChanged EventType = "session_changed"
	EventSessionRemoved EventType = "session_removed"
	EventSources        EventType = "sources"
	EventWatchDegraded  EventType = "watch_degraded"
	EventResync         EventType = "resync"
)

// Removal reasons. agents-absent is deliberately NOT among them: absence from a
// corroboration listing never overrides a liveness gate, and admitting it here
// would invite removing registry-backed sessions on a stale listing — the "one
// malformed record blanks everything" class of bug. job-terminal IS
// authoritative.
const (
	RemovedPIDDead       = "pid-dead"
	RemovedPaneGone      = "pane-gone"
	RemovedRecordRemoved = "record-removed"
	RemovedJobTerminal   = "job-terminal"
	RemovedFilteredOut   = "filtered-out"
)

// Event is one delta. A changed event carries the WHOLE session as well as the
// changed field names — deliberately redundant, because the alternative is
// every client implementing a merge, and at ~1 KB against a measured three
// events per nine minutes redundancy is free while correctness is not.
type Event struct {
	Type      EventType
	Seq       int64
	SessionID string
	// Kind is the other half of a session's identity. An interactive session
	// and a background job can share a sessionId (the resume case) and are two
	// records, so every per-session event names both (code review round 2,
	// A14); a removal has no Session to carry it.
	Kind     Kind
	Session  *Session
	Changed  []string
	Reason   string
	Snapshot *Snapshot
	At       time.Time
}

// RefreshOutcome is what a refresh actually achieved. Refresh returns a result
// rather than only an error because a rate limit, a timeout and a backoff are
// all SUCCESSFUL degraded answers: the caller gets the current snapshot plus a
// request-scoped note. Mutating the shared snapshot to carry that note would
// leak one caller's rejected refresh to unrelated subscribers, and returning an
// error would lose the answer.
type RefreshOutcome struct {
	// Refreshed names the source ids actually re-read.
	Refreshed []string
	// Rejected maps a source id to why: RejectRateLimited, RejectTimeout,
	// RejectBackoff, RejectUnsupported or RejectError.
	//
	// RejectError was ADDED BY P4 and is a divergence from the four reasons this
	// comment carried: a spawn that RAN AND FAILED (the measured cases are
	// `claude` absent from PATH — exit 127 — and output that will not parse) is
	// none of the other four, and reporting it as a timeout would make a
	// diagnosis wrong in the one place a consumer looks.
	Rejected map[string]string
	// Advisories are REQUEST-SCOPED and never merged into the shared snapshot.
	Advisories []Advisory
}

// SessionSource is the one incoming port. All four driving adapters — the CLI,
// the MCP server, the socket server and the TUI panel — depend on this and
// nothing else, which is what makes them interchangeable over a local observer
// and over a client attached to a running one.
type SessionSource interface {
	Snapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error)
	Subscribe(ctx context.Context, f Filter) (<-chan Event, error)
	Refresh(ctx context.Context, sources []string) (*Snapshot, RefreshOutcome, error)
	Enrich(ctx context.Context, snap *Snapshot, ids []string) (*Snapshot, error)
}
