package observer

// Kind is what sort of session this is. A record found in the registry, or in
// `claude agents --json` with a pid, is interactive. A record found at depth 1
// under ~/.claude/jobs/ is background. state.json carries no kind key at all —
// "background" is our value and the agents-cli value, not the record's.
type Kind string

const (
	KindInteractive Kind = "interactive"
	KindBackground  Kind = "background"
	// KindUnknown is in the vocabulary deliberately. With a closed MCP enum and
	// additionalProperties:false, omitting it would make a legal record
	// unrepresentable in a filter.
	KindUnknown Kind = "unknown"
)

// AllKinds is every known Kind, in the order Counts.ByKind emits them.
var AllKinds = []Kind{KindInteractive, KindBackground, KindUnknown}

// Status is the one domain status vocabulary. It deliberately unions two source
// vocabularies WITHOUT conflating them: idle/busy/shell/waiting come from the
// registry's status field, working/blocked/done/failed from a job record's
// state field. blocked is NOT folded into waiting and working is NOT folded
// into busy — a consumer rendering "4 sessions need your input" from such a
// union would send the user hunting for panes that do not exist, because
// background agents have no tmux reference, no pid and no pane.
//
// Status is a string rather than a compiler-closed enum because this is an
// observed set, not a closed one: an unrecognised value passes through as
// StatusUnknown with the literal kept in Session.RawStatus. The MCP schema's
// enum is the closed FILTER vocabulary, not a claim about what can be emitted.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusBusy    Status = "busy"
	StatusShell   Status = "shell"
	StatusWaiting Status = "waiting"
	StatusBlocked Status = "blocked"
	StatusWorking Status = "working"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusUnknown Status = "unknown"
)

// AllStatuses is every known Status, in the order Counts.ByStatus emits them.
var AllStatuses = []Status{
	StatusIdle, StatusBusy, StatusShell, StatusWaiting,
	StatusBlocked, StatusWorking, StatusDone, StatusFailed, StatusUnknown,
}

// WaitReason refines a waiting status. It is a non-pointer string and is never
// null on the wire: "" is the value for "not applicable", from every source,
// including blocked. It is WaitReasonUnknown only when the status is waiting
// and the reason cannot be determined.
//
// blocked gets "" rather than "unknown" because Needs already carries the
// blocked reason in plain words, and WaitReason is the waitingFor enum's
// domain — blocked does not come from waitingFor. Giving it a waiting-vocabulary
// value would re-introduce the conflation Status refuses.
type WaitReason string

const (
	WaitReasonNone             WaitReason = ""
	WaitReasonInputNeeded      WaitReason = "input-needed"
	WaitReasonPermissionPrompt WaitReason = "permission-prompt"
	WaitReasonUnknown          WaitReason = "unknown"
)

// Source ids, used by Session.StatusSource, Session.Sources, Session.ListedBy
// and the keys of Snapshot.Sources.
const (
	SourceRegistry    = "registry"
	SourceJobs        = "jobs"
	SourceAgentsCLI   = "agents-cli"
	SourceTranscript  = "transcript"
	SourceTranscripts = "transcripts"
	SourceTmux        = "tmux"
)

// The two raw waitingFor values the vendor is known to produce. "input needed"
// is the only one observed in 1920 samples; "permission prompt" is named in the
// vendor's own strings but has never been seen live, and its fixture is marked
// SYNTHETIC.
const (
	rawWaitingInputNeeded      = "input needed"
	rawWaitingPermissionPrompt = "permission prompt"
)

// The raw job-record state values.
const (
	rawStateBlocked = "blocked"
	rawStateWorking = "working"
	rawStateDone    = "done"
	rawStateFailed  = "failed"
)

// MapRegistryStatus maps a registry record's status and waitingFor fields onto
// the domain vocabulary. An unrecognised status yields StatusUnknown with
// WaitReasonNone; the caller keeps the literal in RawStatus.
func MapRegistryStatus(rawStatus, rawWaitingFor string) (Status, WaitReason) {
	switch rawStatus {
	case string(StatusIdle):
		return StatusIdle, WaitReasonNone
	case string(StatusBusy):
		return StatusBusy, WaitReasonNone
	case string(StatusShell):
		return StatusShell, WaitReasonNone
	case string(StatusWaiting):
		return StatusWaiting, mapWaitingFor(rawWaitingFor)
	default:
		return StatusUnknown, WaitReasonNone
	}
}

// mapWaitingFor maps the registry's waitingFor string. An absent or
// unrecognised value on a waiting session is WaitReasonUnknown — the session IS
// waiting, we just do not know for what, which is different from "no reason
// applies".
func mapWaitingFor(raw string) WaitReason {
	switch raw {
	case rawWaitingInputNeeded:
		return WaitReasonInputNeeded
	case rawWaitingPermissionPrompt:
		return WaitReasonPermissionPrompt
	default:
		return WaitReasonUnknown
	}
}

// MapJobState maps a job record's state field onto the domain vocabulary.
// Every job state yields WaitReasonNone, including blocked.
func MapJobState(rawState string) (Status, WaitReason) {
	switch rawState {
	case rawStateBlocked:
		return StatusBlocked, WaitReasonNone
	case rawStateWorking:
		return StatusWorking, WaitReasonNone
	case rawStateDone:
		return StatusDone, WaitReasonNone
	case rawStateFailed:
		return StatusFailed, WaitReasonNone
	default:
		return StatusUnknown, WaitReasonNone
	}
}

// rawTempoBlocked is the job record's tempo while a turn is stopped mid-way on
// a permission prompt or a question for the user.
const rawTempoBlocked = "blocked"

// MapJobRecord maps a job record's state AND tempo. The two labels answer
// different questions: state is the outcome the end-of-turn classifier
// wrote, and tempo is what the permission bridge writes MID-turn — leaving
// state alone. A job stopped at AskUserQuestion therefore reads
// state:"working", tempo:"blocked" for as long as it waits (measured: 33 h on
// 857ef435), and a state-only mapping reports it as working. The vendor's
// own listing reads the pair the same way this does.
func MapJobRecord(rawState, rawTempo string) (Status, WaitReason) {
	status, reason := MapJobState(rawState)
	if status == StatusWorking && rawTempo == rawTempoBlocked {
		return StatusBlocked, WaitReasonNone
	}
	return status, reason
}

// MapAgentsStatus maps a `claude agents --json` entry. The CLI reports both
// populations: an entry with a pid carries a registry-vocabulary status (and
// does carry waitingFor when waiting), one without carries a job-vocabulary
// state. Whichever field is populated decides which table applies.
//
// agents-cli is corroboration and never wins a precedence contest; this mapping
// exists so its values can be COMPARED with the winner, and so that an
// interactive session with no registry file is still reportable.
func MapAgentsStatus(rawStatus, rawState, rawWaitingFor string) (Status, WaitReason) {
	if rawStatus != "" {
		return MapRegistryStatus(rawStatus, rawWaitingFor)
	}
	if rawState != "" {
		return MapJobState(rawState)
	}
	return StatusUnknown, WaitReasonNone
}

// MapKind maps an agents-cli kind string. The second return is the raw literal
// to keep in Session.RawKind when the value is not one we know, and nil when it
// is.
func MapKind(raw string) (Kind, *string) {
	switch raw {
	case string(KindInteractive):
		return KindInteractive, nil
	case string(KindBackground):
		return KindBackground, nil
	default:
		return KindUnknown, &raw
	}
}

// IsTerminal reports whether a status is one a session cannot leave. Only the
// two job-record terminal states qualify: an idle interactive session is very
// much still alive.
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusFailed
}

// IsAttention reports whether a status means someone is waiting on the user.
// This is the single definition behind Session.Attention. failed is
// deliberately excluded: a failed agent needs reading, not answering.
func (s Status) IsAttention() bool {
	return s == StatusWaiting || s == StatusBlocked
}

// IsNonTerminalJob reports whether a job-record status is one of the two the
// background liveness rules treat as "could still act".
func (s Status) IsNonTerminalJob() bool {
	return s == StatusBlocked || s == StatusWorking
}
