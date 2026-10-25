package observer

import "time"

// LiveReason names the rule that decided Session.Live, or "" when nothing did.
// It exists so a consumer can tell "we checked and it is gone" from "we have no
// evidence", which one tri-state boolean cannot express on its own.
type LiveReason string

const (
	LiveReasonNone LiveReason = ""

	// Interactive.
	LiveReasonProcPane  LiveReason = "proc+pane"
	LiveReasonProcOnly  LiveReason = "proc-only"
	LiveReasonPIDDead   LiveReason = "pid-dead"
	LiveReasonPaneGone  LiveReason = "pane-gone"
	LiveReasonGateError LiveReason = "gate-error"

	// Background.
	LiveReasonJobTerminal                  LiveReason = "job-terminal"
	LiveReasonJobReaped                    LiveReason = "job-reaped"
	LiveReasonJobTranscriptGone            LiveReason = "job-transcript-gone"
	LiveReasonJobFolderGone                LiveReason = "job-folder-gone"
	LiveReasonJobTempoDisagrees            LiveReason = "job-tempo-disagrees"
	LiveReasonJobBeyondRetention           LiveReason = "job-beyond-transcript-retention"
	LiveReasonJobWorkingStale              LiveReason = "job-working-stale"
	LiveReasonJobNonTerminalCorroborated   LiveReason = "job-non-terminal-corroborated"
	LiveReasonJobNonTerminalUncorroborated LiveReason = "job-non-terminal-uncorroborated"

	// Either population, when no rule can speak.
	LiveReasonUnknown LiveReason = "liveness-unknown"
)

// AllLiveReasons is every reason this policy can produce.
var AllLiveReasons = []LiveReason{
	LiveReasonProcPane, LiveReasonProcOnly, LiveReasonPIDDead, LiveReasonPaneGone,
	LiveReasonGateError, LiveReasonJobTerminal, LiveReasonJobReaped,
	LiveReasonJobTranscriptGone, LiveReasonJobFolderGone,
	LiveReasonJobTempoDisagrees, LiveReasonJobBeyondRetention,
	LiveReasonJobWorkingStale, LiveReasonJobNonTerminalCorroborated,
	LiveReasonJobNonTerminalUncorroborated, LiveReasonUnknown, LiveReasonNone,
}

// Policy holds the four liveness thresholds. Three are policy guesses and are
// documented as such; one is the vendor's own retention setting.
type Policy struct {
	// StaleAfterMs is how old a corroboration listing may be and still speak
	// for the present. A policy knob: raising it trades cross-check freshness
	// for fewer spawns.
	StaleAfterMs int64
	// WorkingStaleAfterMs is how long a working job's TRANSCRIPT may sit still
	// before we stop vouching for it. Unmeasured — no job on the reference
	// machine was working — but a wrong value costs an advisory and a dim
	// marker, never a disappearing session, because null excludes nothing from
	// the default view.
	WorkingStaleAfterMs int64
	// TranscriptRetentionMs is the vendor's transcript retention window, read
	// from ~/.claude/settings.json's cleanupPeriodDays when present. It is the
	// only age-sensitive threshold in the background rules that is not a guess.
	//
	// It MUST be derived from the non-recursive projects/*/*.jsonl population.
	// Subagent transcripts are not reaped at all, so a recursive re-derivation
	// returns months and silently disables the rule that depends on this.
	TranscriptRetentionMs int64
	// SuspectBusyAfterMs is when a busy status earns a status-suspect advisory.
	// The advisory never changes the status.
	SuspectBusyAfterMs int64
	// TranscriptGraceMs is how old a job record must be before a transcript
	// confirmed missing makes it dead (rule 2b). A guess, and a generous one:
	// it only has to outlast the gap between a new job's record and its first
	// transcript write, so a job being born never reads as ended.
	TranscriptGraceMs int64
}

// Default policy values. Every one of these is cited in the architecture's
// assumptions list as a knob rather than a measurement, except
// TranscriptRetentionMs, whose default is the vendor's documented one and whose
// value was confirmed in force by a measured 30.03-day floor.
const (
	// DefaultTimingTailBytes is §8.3 item 9's production bound for the timing
	// pass, declared HERE as well as in the transcript adapter for one reason:
	// the core has to publish the bound that was actually in force
	// (sources.transcripts.tailBytesTiming) and it cannot import an adapter to
	// ask. The adapter's test asserts the two constants are equal, so they
	// cannot drift apart silently.
	//
	// 64 KiB, sized from a measured 1 912 B worst case over the ten
	// most-recently-written transcripts on the machine — 33x headroom.
	DefaultTimingTailBytes int = 64 << 10

	// DefaultFactsTailBytes is §8.3 item 9's production bound for the
	// ENRICHMENT pass, declared here for the same reason the timing bound is:
	// the core publishes the bound that was in force
	// (sources.transcripts.tailBytesColor) and cannot import an adapter to ask.
	// The adapter's test asserts the two declarations are equal and that they
	// are not equal to EACH OTHER — §7.3's split is two bounds, and one bound
	// used twice would be the split quietly collapsing.
	//
	// 200 KiB, sized from the measured population rather than chosen: the
	// agent-color record was found for all 35 transcripts on the machine that
	// carry one, 0 misses, against a largest file of 30.7 MB and a 630 MB tree.
	DefaultFactsTailBytes int = 200 << 10

	DefaultStaleAfterMs          int64 = 30_000
	DefaultWorkingStaleAfterMs   int64 = 15 * 60 * 1000
	DefaultTranscriptRetentionMs int64 = 30 * 24 * 60 * 60 * 1000
	DefaultSuspectBusyAfterMs    int64 = 30 * 60 * 1000
	DefaultTranscriptGraceMs     int64 = 10 * 60 * 1000
)

// DefaultPolicy returns the shipping thresholds.
func DefaultPolicy() Policy {
	return Policy{
		StaleAfterMs:          DefaultStaleAfterMs,
		WorkingStaleAfterMs:   DefaultWorkingStaleAfterMs,
		TranscriptRetentionMs: DefaultTranscriptRetentionMs,
		SuspectBusyAfterMs:    DefaultSuspectBusyAfterMs,
		TranscriptGraceMs:     DefaultTranscriptGraceMs,
	}
}

// Evidence is everything the liveness policy is allowed to look at. It is a
// plain value with no methods that reach outside it, which is what makes the
// policy re-runnable at serialisation time as well as at rebuild time: two of
// the background rules are clock-sensitive, so every ANSWER reflects the
// current clock even when no filesystem event has occurred.
//
// What the policy deliberately cannot see: a previous pane set, a previous
// agents listing, or anything it could mistake for current evidence.
type Evidence struct {
	Kind Kind
	// Status is the mapped domain status; RawState is the job record's literal,
	// carried so advisories can quote what the vendor actually said.
	Status   Status
	RawState string

	// ---- interactive ----

	HasPID bool
	// HasTmuxRef distinguishes "the pane is gone" from "there was never a pane
	// reference to check", which is a real case: tmux is not structurally
	// guaranteed to be present in a registry record.
	HasTmuxRef bool
	ProcAlive  *bool
	PaneAlive  *bool
	// GateError is set when a liveness gate ERRORED, as opposed to answering
	// false. An errored gate is not evidence of death.
	GateError bool
	GateField string
	GateErr   string
	// Previous is the previously published Live value for this session, used
	// only on a gate error: an errored gate keeps the previous answer rather
	// than manufacturing a new one. Nil on the first snapshot.
	Previous *bool

	// ---- background ----

	ReapedMidWorkAt *time.Time
	// Tempo is the vendor's second label. Empty when the record carries none.
	Tempo string
	// TranscriptPresent is whether the record's LinkScanPath target exists.
	TranscriptPresent bool
	// TranscriptGone is the stronger statement: the record names a transcript,
	// we looked, the read worked, and the file is not there. A refused path or
	// a read error is neither present nor gone.
	TranscriptGone bool
	// CWD is the job's working directory, and CWDGone the adapter's confirmed
	// statement that it does not exist (JobRecord.CWDGone).
	CWD            string
	CWDGone        bool
	RecordAgeMs    *int64
	LastActivityAt *time.Time
	// AgentsOK and AgentsStale describe the corroboration listing: the only
	// POSITIVE liveness signal available for a background job. AgentsAgeMs is
	// nil when no listing was taken at all.
	AgentsOK    bool
	AgentsStale bool
	AgentsAgeMs *int64
	// AgentsLists is whether a listing that was taken names this session.
	AgentsLists bool
	// HasJobRecord is false for a background session known only to the vendor's
	// listing, with no job record on disk at all.
	HasJobRecord bool
}

// Verdict is the policy's answer: a tri-state, the rule that decided it, and
// the advisories that carry the evidence.
type Verdict struct {
	// Live is tri-state. nil means "no evidence either way" and is never
	// produced by a zero value.
	Live       *bool
	Reason     LiveReason
	Advisories []Advisory
}

func boolPtr(b bool) *bool { return &b }

// Decide applies the liveness policy. It is pure arithmetic over (evidence,
// now) with no I/O, so it can be re-run at answer time as well as at rebuild
// time — which is the whole point, because two of the background rules depend
// on the clock and nothing wakes up to publish a crossing.
//
// The meaning of a true answer, which is the one thing every rule here serves:
// POSITIVE, CURRENT evidence that this session can still act. Not "the vendor
// lists it". Not "its record has not reached a terminal state". Where the
// evidence is only historical membership, or a listing too old to speak for the
// present, the answer is nil.
func Decide(e Evidence, now time.Time, p Policy) Verdict {
	if e.Kind == KindBackground || (!e.HasPID && e.Status.IsNonTerminalJob()) {
		return decideBackground(e, now, p)
	}
	if e.HasPID || e.Kind == KindInteractive {
		return decideInteractive(e)
	}
	return Verdict{
		Reason:     LiveReasonUnknown,
		Advisories: []Advisory{AdvisoryLivenessUnknown("kind", "no pid and no recognised kind")},
	}
}

// decideInteractive: a live pid AND an existing pane, with the pane clause
// waived when there is no pane reference to check.
func decideInteractive(e Evidence) Verdict {
	// A gate error comes first: it is not evidence of death, and a false answer
	// from a working gate must not be manufactured from a broken one.
	//
	// NOTE on a contradiction in the spec, resolved here: §3.5's table labels
	// this row "live = null" while its own text says "the previous value is
	// KEPT". Keeping the previous value is the substantive instruction — a
	// null would discard a good answer on a transient gate failure — so the
	// previous value wins when there is one, and it is null only on the first
	// snapshot. Either way the reason is gate-error and the advisory is
	// attached, so a consumer can always tell.
	if e.GateError {
		field := e.GateField
		if field == "" {
			field = "liveness"
		}
		v := Verdict{
			Reason:     LiveReasonGateError,
			Advisories: []Advisory{AdvisoryLivenessUnknown(field, e.GateErr)},
		}
		if e.Previous != nil {
			kept := *e.Previous
			v.Live = &kept
		}
		return v
	}

	if e.ProcAlive != nil && !*e.ProcAlive {
		return Verdict{Live: boolPtr(false), Reason: LiveReasonPIDDead}
	}
	if e.HasTmuxRef && e.PaneAlive != nil && !*e.PaneAlive {
		return Verdict{Live: boolPtr(false), Reason: LiveReasonPaneGone}
	}
	if e.ProcAlive != nil && *e.ProcAlive {
		if e.HasTmuxRef {
			if e.PaneAlive != nil && *e.PaneAlive {
				return Verdict{Live: boolPtr(true), Reason: LiveReasonProcPane}
			}
			// A pane reference we could not check is not a pane we can vouch
			// for.
			return Verdict{
				Reason:     LiveReasonUnknown,
				Advisories: []Advisory{AdvisoryLivenessUnknown("paneAlive", "pane set unavailable")},
			}
		}
		return Verdict{Live: boolPtr(true), Reason: LiveReasonProcOnly}
	}

	return Verdict{
		Reason:     LiveReasonUnknown,
		Advisories: []Advisory{AdvisoryLivenessUnknown("procAlive", "no pid evidence")},
	}
}

// decideBackground evaluates the seven background rules IN ORDER; the first
// match wins. Rules 1, 2 and 2b are the only ones that can answer false; rule 6 is
// the only one that can answer true, and it is the only one resting on a
// positive signal.
func decideBackground(e Evidence, now time.Time, p Policy) Verdict {
	// A background session the vendor lists with no job record at all: we have
	// membership and nothing else.
	if !e.HasJobRecord {
		return Verdict{
			Reason:     LiveReasonUnknown,
			Advisories: []Advisory{AdvisoryLivenessUnknown("state", "no job record on disk")},
		}
	}

	// Rule 1 — terminal.
	if e.Status.IsTerminal() {
		return Verdict{
			Live:       boolPtr(false),
			Reason:     LiveReasonJobTerminal,
			Advisories: []Advisory{AdvisoryJobTerminal(e.RawState)},
		}
	}

	// Rule 2 — a direct in-record statement that the daemon was reaped. This
	// must precede everything below: a record reaped while blocked would
	// otherwise be reported live and needing attention with the evidence two
	// keys away.
	if e.ReapedMidWorkAt != nil {
		return Verdict{
			Live:       boolPtr(false),
			Reason:     LiveReasonJobReaped,
			Advisories: []Advisory{AdvisoryJobReaped(*e.ReapedMidWorkAt)},
		}
	}

	// Rule 2b — the conversation is gone. A job cannot be resumed without its
	// transcript, so a record that still says blocked or working is a claim
	// nothing can act on: the vendor keeps listing it as needing input (the
	// user, 2026-10-21: a 25-day-old job with its worktree and transcript both
	// deleted sat pinned in NEEDS YOU, and opening it failed). This precedes
	// rule 4, which reads the same absence as no evidence; a CONFIRMED absence
	// past the grace period is evidence.
	if e.TranscriptGone && e.RecordAgeMs != nil && *e.RecordAgeMs > p.TranscriptGraceMs {
		return Verdict{
			Live:       boolPtr(false),
			Reason:     LiveReasonJobTranscriptGone,
			Advisories: []Advisory{AdvisoryJobTranscriptGone(*e.RecordAgeMs)},
		}
	}

	// Rule 2c — the job's folder is gone (the user, 2026-10-22: a worktree is
	// removed once its branch merges, and the job left in it still reads
	// blocked). Nothing can attach to it where it lives — Claude refuses with
	// "Workspace not trusted" — so the listing naming it is no evidence it can
	// act. null, not false: the conversation may survive, and `claude
	// --resume` from another folder can still reach it, so the row stays
	// visible, just out of NEEDS YOU.
	if e.CWDGone {
		return Verdict{
			Reason:     LiveReasonJobFolderGone,
			Advisories: []Advisory{AdvisoryJobFolderGone(e.CWD)},
		}
	}

	// Rule 3 — two of the vendor's own labels disagree. null, not false: we do
	// not know which is right, and that is the definition of no evidence.
	if e.Tempo == "idle" && e.Status.IsNonTerminalJob() {
		return Verdict{
			Reason:     LiveReasonJobTempoDisagrees,
			Advisories: []Advisory{AdvisoryTempoStateDisagreement(e.RawState, e.Tempo)},
		}
	}

	// Rule 4 — no transcript, and the record is older than the vendor's own
	// retention window, so no transcript COULD exist to corroborate it. Its
	// claim is unfalsifiable.
	if !e.TranscriptPresent && e.RecordAgeMs != nil && *e.RecordAgeMs > p.TranscriptRetentionMs {
		return Verdict{
			Reason: LiveReasonJobBeyondRetention,
			Advisories: []Advisory{
				AdvisoryBeyondTranscriptRetention(*e.RecordAgeMs, p.TranscriptRetentionMs),
			},
		}
	}

	// Rule 5 — a working job whose transcript has stopped moving. Measured
	// against the TRANSCRIPT, which moves every turn, and not against the
	// record, which a milestone-writing job may not touch for an hour.
	if e.Status == StatusWorking && e.LastActivityAt != nil {
		idleMs := now.Sub(*e.LastActivityAt).Milliseconds()
		if idleMs > p.WorkingStaleAfterMs {
			return Verdict{
				Reason: LiveReasonJobWorkingStale,
				Advisories: []Advisory{
					AdvisoryBackgroundWorkingStale(*e.LastActivityAt, p.WorkingStaleAfterMs),
				},
			}
		}
	}

	if e.Status.IsNonTerminalJob() {
		// Rule 6 — the only positive signal we have: a FRESH vendor listing
		// that still names this session.
		if e.AgentsOK && !e.AgentsStale && e.AgentsLists {
			return Verdict{Live: boolPtr(true), Reason: LiveReasonJobNonTerminalCorroborated}
		}
		// Rule 6b — the exact complement of rule 6, so there is no third branch
		// and no gap. Three sub-cases, one outcome, distinguished only by the
		// advisory.
		v := Verdict{Reason: LiveReasonJobNonTerminalUncorroborated}
		switch {
		case !e.AgentsOK || e.AgentsStale:
			// (i) stale, or (ii) absent/errored.
			v.Advisories = []Advisory{AdvisoryJobCorroborationStale(e.AgentsAgeMs, p.StaleAfterMs)}
		default:
			// (iii) the listing is fresh and does NOT name this session. This is
			// the case that falsifies the terminal-state rule, and it must
			// never read live: true.
			v.Advisories = []Advisory{AdvisoryBackgroundGateDisagreement(e.RawState, false)}
		}
		return v
	}

	// Rule 7 — any unrecognised state.
	return Verdict{
		Reason:     LiveReasonUnknown,
		Advisories: []Advisory{AdvisoryLivenessUnknown("state", "unrecognised background state")},
	}
}

// IncludedByDefault reports whether a session with this Live value survives the
// default filter. The rule is live != false, NOT live == true: a session
// vanishing from the default view requires POSITIVE evidence of death, so an
// unknown-liveness session is never silently hidden.
func IncludedByDefault(live *bool, opts SnapshotOptions) bool {
	switch {
	case live == nil:
		return opts.IncludeUnknownLiveness
	case *live:
		return true
	default:
		return opts.IncludeDead
	}
}
