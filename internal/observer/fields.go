package observer

// This file holds the ONE list of session fields that deltas are computed over.
// Two separate things read it and must never drift apart:
//
//  1. diff(prev, next), which decides whether to emit a session_changed event
//     and what to put in its changed[] array;
//  2. the wire round-trip property test, which asserts that
//     domain -> wire -> domain is the identity field by field.
//
// They are the same list because a field that round-trips but is not diffed is
// invisible to a subscriber, and a field that is diffed but does not round-trip
// makes a socket-attached consumer disagree with a direct one. One slice makes
// both impossible to forget.

// DiffedFields is the set of session fields compared to produce deltas, using
// the wire (JSON) names so that a changed[] array is meaningful to a consumer
// that only ever sees JSON.
var DiffedFields = []string{
	"status",
	"rawStatus",
	"statusSource",
	"statusSince",
	"statusSinceLowerBound",
	"lastActivityAt",
	"waitReason",
	"attention",
	"needs",
	"detail",
	"tempo",
	"tokens",
	"children",
	"resumeSessionId",
	"reapedMidWorkAt",
	"name",
	"nameSource",
	"cwd",
	"tmux",
	"pid",
	"kind",
	"rawKind",
	"live",
	"liveReason",
	"listedBy",
	"procAlive",
	"paneAlive",
	"sources",
	"advisories",
	"transcriptCollected",
	"transcript",
	// spawnedBy changes when the launching session dies or a walk that errored
	// completes, and a subscriber watching everything must be told — it is the
	// difference between a child row and a top-level one.
	"spawnedBy",
}

// ClockDerivedFields are excluded from diffing, exhaustively. They change on
// every rebuild by construction, so diffing them would emit a session_changed
// event for every session on every event — which is the difference between a
// delta stream and a poll loop with extra steps.
//
// Note that lastActivityAt is DIFFED while statusAgeMs is not: it is a measured
// timestamp read from a file, not a derivation from our clock, so it changes
// only when the transcript actually grows.
var ClockDerivedFields = []string{
	"statusAgeMs",
	"blockedForMs",
	"recordAgeMs",
	"observedAt",
	"stale",
}

// IdentityFields are neither diffed nor clock-derived: they are fixed for the
// life of a session, so a change in one would mean a different session.
//
// UNDER-SPECIFIED IN THE ARCHITECTURE, and resolved here rather than left
// implicit: §5.3's M19 rule says "the diffed set is everything else" and then
// enumerates it "exhaustively", but the enumeration omits these nine fields,
// which are in neither list. Treating them as diffed would mean claiming a
// session's id or start time can change; treating them as silently absent
// would leave them unclassified and untested. They are therefore classified
// here as immutable, and the wire round-trip test covers them anyway by
// asserting equality over the WHOLE session, not just DiffedFields. If a later
// phase finds one of these genuinely mutating, it moves to DiffedFields and
// the test follows automatically.
var IdentityFields = []string{
	"sessionId",
	"shortId",
	"startedAt",
	"formerNames",
	"claudeVersion",
	"messagingSocketPath",
	"blockedForUnavailable",
	"rawWaitingFor",
	"recordUpdatedAt",
}

// AllSessionFields is every wire field name on a session, in wire order. A test
// asserts this equals the union of the three classifications above, and that
// the union equals the json tags wire.Session actually carries — so adding a
// field to the wire struct without classifying it fails the build's tests
// rather than silently joining neither set.
var AllSessionFields = []string{
	"sessionId",
	"shortId",
	"pid",
	"kind",
	"rawKind",
	"name",
	"nameSource",
	"formerNames",
	"cwd",
	"startedAt",
	"status",
	"rawStatus",
	"statusSource",
	"statusSince",
	"statusAgeMs",
	"statusSinceLowerBound",
	"blockedForMs",
	"blockedForUnavailable",
	"waitReason",
	"rawWaitingFor",
	"attention",
	"lastActivityAt",
	"needs",
	"detail",
	"tempo",
	"tokens",
	"children",
	"resumeSessionId",
	"recordUpdatedAt",
	"recordAgeMs",
	"reapedMidWorkAt",
	"tmux",
	"paneAlive",
	"procAlive",
	"live",
	"liveReason",
	"listedBy",
	"claudeVersion",
	"messagingSocketPath",
	"spawnedBy",
	"transcriptCollected",
	"transcript",
	"sources",
	"stale",
	"observedAt",
	"advisories",
}

// IsDiffed reports whether a wire field name is in the diffed set.
func IsDiffed(field string) bool {
	for _, f := range DiffedFields {
		if f == field {
			return true
		}
	}
	return false
}
