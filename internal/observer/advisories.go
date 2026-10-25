package observer

import "time"

// Advisory codes. These are strings consumers match on, so they are a
// contract:
//
//	a code is never repurposed;
//	a code may be added in a minor version;
//	a code is removed only with a wire schema bump.
//
// Every code the design defines appears here, and every code here appears in
// the architecture's §3.7 registry. A code that exists in one and not the other
// is a defect.
const (
	// ---- session-scoped ----

	// AdvLivenessUnknown: a liveness gate errored, or a background state is
	// unrecognised. Fields: field, error.
	AdvLivenessUnknown = "liveness-unknown"
	// AdvJobReaped: reapedMidWorkAt is present. Fields: reapedMidWorkAt.
	AdvJobReaped = "job-reaped"
	// AdvJobTranscriptGone: the job's transcript is confirmed missing and the
	// record is past the grace period, so nothing can resume it. Fields:
	// recordAgeMs.
	AdvJobTranscriptGone = "job-transcript-gone"
	// AdvJobFolderGone: the job's working directory is confirmed missing (a
	// removed worktree), so it cannot be attached where it lives. Fields: cwd.
	AdvJobFolderGone = "job-folder-gone"
	// AdvTempoStateDisagreement: tempo contradicts state. Fields: state, tempo.
	AdvTempoStateDisagreement = "tempo-state-disagreement"
	// AdvBeyondTranscriptRetention: no transcript and the record is older than
	// the retention window, so its claim is unfalsifiable. Fields: recordAgeMs,
	// retentionMs.
	AdvBeyondTranscriptRetention = "beyond-transcript-retention"
	// AdvJobCorroborationStale: a non-terminal job record passed every
	// counter-signal but the agents-cli listing is stale, absent or errored, so
	// live is null rather than true. Fields: agentsAgeMs, staleAfterMs.
	AdvJobCorroborationStale = "job-corroboration-stale"
	// AdvBackgroundWorkingStale: working, and the TRANSCRIPT is older than the
	// working-stale threshold. Fields: lastActivityAt, thresholdMs.
	AdvBackgroundWorkingStale = "background-working-stale"
	// AdvJobTerminal: state is done or failed. Carried on the record when
	// includeDead. Fields: state.
	AdvJobTerminal = "job-terminal"
	// AdvAgentsAbsent: a registry or jobs record is missing from a FRESH agents
	// snapshot. Independent evidence of death that never overrides a gate.
	// Fields: observedAt.
	AdvAgentsAbsent = "agents-absent"
	// AdvBackgroundGateDisagreement: the listing and the record's terminal
	// state disagree. This is the standing falsification test for the
	// terminal-state rule. Fields: recordState, listed.
	AdvBackgroundGateDisagreement = "background-gate-disagreement"
	// AdvStatusConflict: two sources disagree on status. The §3.4 winner is
	// still reported. Fields: sources, rawValues.
	AdvStatusConflict = "status-conflict"
	// AdvStatusSuspect: busy for more than 30 minutes. An advisory is a
	// separate field; it never changes status. Fields: statusAgeMs.
	AdvStatusSuspect = "status-suspect"
	// AdvStatusSinceUnavailable: no status timestamp available for this source.
	// Fields: source.
	AdvStatusSinceUnavailable = "status-since-unavailable"
	// AdvStatusSinceRecordLaterThanActivity: the record was written after the
	// thread went quiet, so statusSince under-reports the duration. Fields:
	// recordUpdatedAt, lastActivityAt, deltaMs.
	AdvStatusSinceRecordLaterThanActivity = "status-since-record-later-than-activity"
	// AdvStatusSinceUnverifiable: a job's statusSince has no transcript to
	// bound against. Fields: reason.
	AdvStatusSinceUnverifiable = "status-since-unverifiable"
	// AdvActivitySinceTruncated: no timestamped record in the transcript tail.
	// Fields: tailBytes.
	AdvActivitySinceTruncated = "activity-since-truncated"
	// AdvTranscriptAbsent: the transcript path is named but the file is not
	// there. Fields: path.
	AdvTranscriptAbsent = "transcript-absent"
	// AdvBlockedForUnavailable: attention is true but statusAgeMs is null.
	// Fields: reason.
	AdvBlockedForUnavailable = "blockedForUnavailable"
	// AdvShortIDMismatch: daemonShort differs from the job directory name. The
	// record is still used; both values are carried. Fields: dirName,
	// daemonShort.
	AdvShortIDMismatch = "shortid-mismatch"
	// AdvSessionIDDuplicate: two same-kind records share a sessionId. The first
	// in the fixed join order wins; the loser is discarded, not merged. Fields:
	// kind, source, winner, loser.
	AdvSessionIDDuplicate = "sessionid-duplicate"
	// AdvSessionIDCrossKind: one sessionId appears as both kinds. They are NOT
	// merged — two sessions are emitted. Fields: other.
	AdvSessionIDCrossKind = "sessionid-cross-kind"
	// AdvTranscriptAmbiguous: the transcript glob returned more than one match.
	// Fields: matches.
	AdvTranscriptAmbiguous = "transcript-ambiguous"
	// AdvParseRetryFailed: a record failed to parse twice in one rebuild.
	// Fields: path, bytes.
	AdvParseRetryFailed = "parse-retry-failed"
	// AdvUnparseable: a record failed to parse on two consecutive events.
	// Fields: path.
	AdvUnparseable = "unparseable"
	// AdvWireUnknownField: the wire decoder met a field it does not know. The
	// field is dropped and named here. Fields: field.
	AdvWireUnknownField = "wire-unknown-field"
	// AdvTmuxRefStale: the session's process descends from a pane of this
	// server, but its registry tmux ref names no current pane (the pane moved
	// or was renumbered). paneAlive is true; the ref is not to be trusted for
	// navigation. Fields: tmux. (moox §4.5)
	AdvTmuxRefStale = "tmux-ref-stale"

	// ---- source-scoped ----
	//
	// Source-scoped advisories appear in sources.<id>.advisories AND are
	// promoted once into the top-level warnings array. Session-scoped
	// advisories appear only on their session and are never promoted.

	// AdvSourceStale: a source's age exceeds its staleness threshold. Fields:
	// source, ageMs, staleAfterMs.
	AdvSourceStale = "source-stale"
	// AdvSpawnLoopGuard: the corroboration circuit breaker tripped. Fields:
	// spawns, windowMs.
	AdvSpawnLoopGuard = "spawn-loop-guard"
	// AdvRefreshRejected: a refresh was rate-limited, timed out or hit backoff.
	// REQUEST-SCOPED — never merged into the shared snapshot. Fields: reason.
	AdvRefreshRejected = "refresh-rejected"
	// AdvAgentsCacheContended: the cross-process corroboration lock was held
	// and the wait expired. Fields: waitedMs.
	AdvAgentsCacheContended = "agents-cache-contended"
	// AdvCorroborationWaitExpired: a rebuild answered WITHOUT corroboration
	// because the spawn it issued had not returned inside the rebuild's bounded
	// wait — the wedged-CLI case. It is the visible half of that bound: without
	// it the source reads ok:false with no lastError and no advisory, which is
	// how a wedged vendor process turned every session unknown in silence.
	// Fields: source, waitMs, detail.
	AdvCorroborationWaitExpired = "corroboration-wait-expired"
	// AdvFileWatchCap: a descriptor cap stopped per-file arming. Fields: armed,
	// cap.
	AdvFileWatchCap = "file-watch-cap"
	// AdvWatchTargetMissing: a desired watch target could not be armed. Fields:
	// path, error.
	AdvWatchTargetMissing = "watch-target-missing"
	// AdvSourceNotOK: a source could not be read at all — its report carries
	// ok:false — and no more specific source-scoped advisory already says so.
	// It is the standing explanation of a degraded answer, so a consumer that
	// watches `warnings` learns about a broken source without having to walk
	// `sources` itself. Fields: source, error, role.
	//
	// ADDED BY P10, and deliberately DIFF-SAFE: both fields are stable for as
	// long as the failure is. It carries no age, no duration and no count, so
	// it is emitted once when a source breaks, never again while it stays
	// broken, and withdrawn when it recovers. P7's live run found the opposite
	// shape — advisories carrying recordAgeMs/statusAgeMs emitted a change
	// event for three sessions on EVERY rebuild — and that is the defect this
	// advisory is written not to repeat.
	AdvSourceNotOK = "source-not-ok"
	// AdvWatchArmUnconverged: the arm/scan fixed point hit its iteration cap
	// without the armed set and the enumeration agreeing. Latency only — the
	// already-armed jobs root queued a hint for every directory created during
	// the loop, so the next rebuild converges. Fields: iterations, pendingDirs.
	AdvWatchArmUnconverged = "watch-arm-unconverged"
)

// AdvisoryScope says where an advisory may appear.
type AdvisoryScope int

const (
	// ScopeSession: appears only on its session, never promoted.
	ScopeSession AdvisoryScope = iota
	// ScopeSource: appears in sources.<id>.advisories and is promoted once into
	// the top-level warnings array.
	ScopeSource
)

// advisoryScopes is the registry. Its key set is the complete set of codes, so
// a code added to the consts above and not here fails the registry test.
var advisoryScopes = map[string]AdvisoryScope{
	AdvLivenessUnknown:                    ScopeSession,
	AdvJobReaped:                          ScopeSession,
	AdvJobTranscriptGone:                  ScopeSession,
	AdvJobFolderGone:                      ScopeSession,
	AdvTempoStateDisagreement:             ScopeSession,
	AdvBeyondTranscriptRetention:          ScopeSession,
	AdvJobCorroborationStale:              ScopeSession,
	AdvBackgroundWorkingStale:             ScopeSession,
	AdvJobTerminal:                        ScopeSession,
	AdvAgentsAbsent:                       ScopeSession,
	AdvBackgroundGateDisagreement:         ScopeSession,
	AdvStatusConflict:                     ScopeSession,
	AdvStatusSuspect:                      ScopeSession,
	AdvStatusSinceUnavailable:             ScopeSession,
	AdvStatusSinceRecordLaterThanActivity: ScopeSession,
	AdvStatusSinceUnverifiable:            ScopeSession,
	AdvActivitySinceTruncated:             ScopeSession,
	AdvTranscriptAbsent:                   ScopeSession,
	AdvBlockedForUnavailable:              ScopeSession,
	AdvShortIDMismatch:                    ScopeSession,
	AdvSessionIDDuplicate:                 ScopeSession,
	AdvSessionIDCrossKind:                 ScopeSession,
	AdvTranscriptAmbiguous:                ScopeSession,
	AdvParseRetryFailed:                   ScopeSession,
	AdvUnparseable:                        ScopeSession,
	AdvWireUnknownField:                   ScopeSession,
	AdvTmuxRefStale:                       ScopeSession,

	AdvSourceStale:              ScopeSource,
	AdvSpawnLoopGuard:           ScopeSource,
	AdvRefreshRejected:          ScopeSource,
	AdvAgentsCacheContended:     ScopeSource,
	AdvCorroborationWaitExpired: ScopeSource,
	AdvFileWatchCap:             ScopeSource,
	AdvWatchTargetMissing:       ScopeSource,
	AdvWatchArmUnconverged:      ScopeSource,
	AdvSourceNotOK:              ScopeSource,
}

// AllAdvisoryCodes is every declared code, in registry order (session-scoped
// first, then source-scoped), so tests and the doctor can iterate them.
var AllAdvisoryCodes = []string{
	AdvLivenessUnknown,
	AdvJobReaped,
	AdvJobTranscriptGone,
	AdvJobFolderGone,
	AdvTempoStateDisagreement,
	AdvBeyondTranscriptRetention,
	AdvJobCorroborationStale,
	AdvBackgroundWorkingStale,
	AdvJobTerminal,
	AdvAgentsAbsent,
	AdvBackgroundGateDisagreement,
	AdvStatusConflict,
	AdvStatusSuspect,
	AdvStatusSinceUnavailable,
	AdvStatusSinceRecordLaterThanActivity,
	AdvStatusSinceUnverifiable,
	AdvActivitySinceTruncated,
	AdvTranscriptAbsent,
	AdvBlockedForUnavailable,
	AdvShortIDMismatch,
	AdvSessionIDDuplicate,
	AdvSessionIDCrossKind,
	AdvTranscriptAmbiguous,
	AdvParseRetryFailed,
	AdvUnparseable,
	AdvWireUnknownField,
	AdvTmuxRefStale,
	AdvSourceStale,
	AdvSpawnLoopGuard,
	AdvRefreshRejected,
	AdvAgentsCacheContended,
	AdvCorroborationWaitExpired,
	AdvFileWatchCap,
	AdvWatchTargetMissing,
	AdvWatchArmUnconverged,
	AdvSourceNotOK,
}

// ScopeOf returns the declared scope of a code, and false when the code is not
// one of ours. A decoded advisory whose code we do not recognise is PRESERVED
// rather than dropped — §3.7 promises codes may be added in a minor version, so
// a client one build behind must still be able to show them.
func ScopeOf(code string) (AdvisoryScope, bool) {
	s, ok := advisoryScopes[code]
	return s, ok
}

// NewAdvisory builds an advisory. A nil or empty fields map is normalised to
// nil so that an advisory with no fields compares equal however it was built.
//
// Integer values MUST be int64 and timestamps MUST be pre-formatted strings or
// int64 epoch millis: Fields round-trips through JSON, and the wire decoder
// normalises numbers to int64. See Advisory's doc comment.
func NewAdvisory(code string, fields map[string]any) Advisory {
	if len(fields) == 0 {
		return Advisory{Code: code}
	}
	return Advisory{Code: code, Fields: fields}
}

// The constructors below cover the advisories P0's liveness policy emits. The
// rest get theirs in the phase that first raises them — a constructor with no
// caller is a field list nobody has checked against the registry.

// AdvisoryLivenessUnknown reports a liveness gate that errored, or a background
// state we do not recognise.
func AdvisoryLivenessUnknown(field, errText string) Advisory {
	return NewAdvisory(AdvLivenessUnknown, map[string]any{
		"field": field,
		"error": errText,
	})
}

// AdvisoryJobReaped reports a record that states the daemon was reaped
// mid-work. A record reaped while blocked would otherwise be reported live and
// needing attention with the evidence two keys away.
func AdvisoryJobReaped(at time.Time) Advisory {
	return NewAdvisory(AdvJobReaped, map[string]any{
		"reapedMidWorkAt": FormatTime(at),
	})
}

// AdvisoryJobTranscriptGone reports a job whose conversation no longer exists.
func AdvisoryJobTranscriptGone(recordAgeMs int64) Advisory {
	return NewAdvisory(AdvJobTranscriptGone, map[string]any{"recordAgeMs": recordAgeMs})
}

// AdvisoryJobFolderGone reports a job whose working directory no longer exists.
func AdvisoryJobFolderGone(cwd string) Advisory {
	return NewAdvisory(AdvJobFolderGone, map[string]any{"cwd": cwd})
}

// AdvisoryTempoStateDisagreement reports two of the vendor's own labels
// disagreeing. That is the definition of no evidence, so liveness answers null.
func AdvisoryTempoStateDisagreement(state, tempo string) Advisory {
	return NewAdvisory(AdvTempoStateDisagreement, map[string]any{
		"state": state,
		"tempo": tempo,
	})
}

// AdvisoryBeyondTranscriptRetention reports a record older than the vendor's
// own transcript retention window with no transcript to corroborate it.
func AdvisoryBeyondTranscriptRetention(recordAgeMs, retentionMs int64) Advisory {
	return NewAdvisory(AdvBeyondTranscriptRetention, map[string]any{
		"recordAgeMs": recordAgeMs,
		"retentionMs": retentionMs,
	})
}

// AdvisoryJobCorroborationStale reports that the only positive liveness signal
// available for a background job — a fresh vendor listing — was too old,
// absent or errored. agentsAgeMs is nil when no listing was taken at all.
func AdvisoryJobCorroborationStale(agentsAgeMs *int64, staleAfterMs int64) Advisory {
	fields := map[string]any{"staleAfterMs": staleAfterMs}
	if agentsAgeMs != nil {
		fields["agentsAgeMs"] = *agentsAgeMs
	} else {
		fields["agentsAgeMs"] = nil
	}
	return NewAdvisory(AdvJobCorroborationStale, fields)
}

// AdvisoryBackgroundWorkingStale reports a working job whose TRANSCRIPT has not
// moved for longer than the threshold. Measured against the transcript, not the
// record: a job that writes its record only at milestones is not stale merely
// because it has not written one.
func AdvisoryBackgroundWorkingStale(lastActivityAt time.Time, thresholdMs int64) Advisory {
	return NewAdvisory(AdvBackgroundWorkingStale, map[string]any{
		"lastActivityAt": FormatTime(lastActivityAt),
		"thresholdMs":    thresholdMs,
	})
}

// AdvisoryJobTerminal reports a done or failed record. It travels with the
// record when a consumer asks for dead sessions.
func AdvisoryJobTerminal(state string) Advisory {
	return NewAdvisory(AdvJobTerminal, map[string]any{"state": state})
}

// AdvisoryBackgroundGateDisagreement reports a fresh listing and a record
// disagreeing about whether the job still exists. This case must NEVER read
// live: true — it is what falsifies the terminal-state rule.
func AdvisoryBackgroundGateDisagreement(recordState string, listed bool) Advisory {
	return NewAdvisory(AdvBackgroundGateDisagreement, map[string]any{
		"recordState": recordState,
		"listed":      listed,
	})
}
