package observer

import "time"

// This file is §3.4's INVARIANT B and §7.3's timing cache key: when the 64 KiB
// timing tail is read, and what its cached result is keyed to.
//
// Both are pure arithmetic over values, with no I/O, so the rule can be
// unit-tested against all five of its predicates without a filesystem. The
// reads themselves happen in snapshot.go, which is also where the ordering
// obligation lives (arm -> read -> publish).
//
// # Why there is a rule at all, rather than "read it when the file changes"
//
// Revision 2 read the tail only for a job whose transcript fired a watch
// event. That is a read-on-NOTIFICATION rule, and it cannot serve a target
// that was not armed while it was being written — which is the case for the two
// transitions the design cares most about:
//
//   - a FIRST state.json: the transcript may already be complete before the
//     record exists, so there is nothing left to append and no event will ever
//     fire;
//   - a RESUME that repoints linkScanPath: the new transcript was written while
//     the OLD path was armed, and the new one is armed only afterwards.
//
// If the job is then `blocked`, nothing appends again — ever. lastActivityAt
// stays null permanently, the record-later-than-activity comparison never runs,
// and statusSinceLowerBound is permanently absent on a field AC3(c) names. That
// is not a staleness residual, it is non-convergence. The window is not
// theoretical either: one measured record's corroborating transcript write sits
// 101 ms after its updatedAt, well inside the 250 ms coalescing interval, so a
// newly discovered target can finish appending before the rebuild that
// discovers it has even armed it.
//
// # Why widening it is affordable
//
// Four of the five predicates are CHANGE predicates over the join key set, so
// in the steady state they fire for nobody: 7 job records, at most one changing
// per event, against a measured 3 events per 9 minutes across 32 sessions. The
// worst case is the first rebuild of a process — every existing job transcript
// read once, 5 targets x 64 KiB = 320 KiB, one time. A resume adds one 64 KiB
// read. A registry .key write triggers a rebuild and changes nothing about any
// job, so nothing is re-read.

// TimingCacheKey is §7.3's key for a cached timing read: the session AND the
// transcript path, never the session alone.
//
// The path is in the key because a resume repoints linkScanPath at a different
// conversation. With a sessionId-only key, the resume returns the PREVIOUS
// thread's last turn as the new thread's last activity — and it does so
// invisibly, because both answers are well-formed timestamps. Putting the path
// in the key makes that a cache MISS instead of a wrong answer, which is the
// cheapest possible place for the rule to be impossible to forget.
//
// It deliberately does NOT include size or mtime, unlike the enrichment cache's
// (sessionId, path, size, mtime). Invariant B's change predicates already
// decide when to re-read, and a size/mtime key would ALSO re-read on every
// append — turning a bounded correctness read into one that fires on every
// transcript write.
type TimingCacheKey struct {
	SessionID string
	Path      string
}

// TimingTrigger names which of Invariant B's predicates fired, or "" when none
// did and the cached facts stand. It is carried so a test can assert WHICH rule
// fired rather than only that a read happened — five predicates that all
// produce "a read" are five untested rules.
type TimingTrigger string

const (
	// TimingTriggerNone is a cache hit: nothing about this job changed.
	TimingTriggerNone TimingTrigger = ""
	// TimingTriggerNewSession is Invariant B case 1 — no entry for this
	// sessionId in the previously published snapshot. This is the case
	// revision 2's rule could not serve.
	TimingTriggerNewSession TimingTrigger = "new-session"
	// TimingTriggerPathChanged is case 2 — linkScanPath differs from the path
	// the cached facts are keyed to. It ALSO invalidates the old path's facts.
	TimingTriggerPathChanged TimingTrigger = "path-changed"
	// TimingTriggerUpdatedAt is case 3 — the record's updatedAt changed.
	TimingTriggerUpdatedAt TimingTrigger = "updated-at-changed"
	// TimingTriggerStatus is case 4 — the derived status changed.
	TimingTriggerStatus TimingTrigger = "status-changed"
	// TimingTriggerEvent is case 5 — the transcript target fired a watch
	// event. Revision 2's whole rule, retained as one of five.
	TimingTriggerEvent TimingTrigger = "transcript-event"
)

// AllTimingTriggers is every predicate, in evaluation order, so a test can
// enumerate them rather than sample them.
var AllTimingTriggers = []TimingTrigger{
	TimingTriggerNewSession,
	TimingTriggerPathChanged,
	TimingTriggerUpdatedAt,
	TimingTriggerStatus,
	TimingTriggerEvent,
}

// TimingState is what one rebuild remembers about one job so the next rebuild
// can evaluate the change predicates. Three fields, because three of the five
// predicates are comparisons against the previous value.
type TimingState struct {
	// Path is the linkScanPath the cached facts are keyed to.
	Path string
	// UpdatedAt is the record's updatedAt as of the last rebuild.
	UpdatedAt *time.Time
	// Status is the DERIVED status as of the last rebuild, not the raw state:
	// a vendor renaming a state literal should not look like a transition.
	Status Status
}

// TimingInputs is one job's current values, as read this rebuild.
type TimingInputs struct {
	SessionID string
	Path      string
	UpdatedAt *time.Time
	Status    Status
}

// TimingTriggerFor evaluates Invariant B's five predicates IN ORDER and returns
// the first that fires, or TimingTriggerNone.
//
// prev is nil when this sessionId was not in the previously published snapshot,
// which is case 1 by definition and is checked first — the other four
// comparisons have nothing to compare against.
//
// changedPaths is the set of transcript paths that fired a watch event since
// the last rebuild. It is empty in a one-shot `tt sessions` run (there is no
// watcher) and populated by P5, which is exactly why the other four predicates
// carry the correctness burden: a rule that only works when a watcher is
// running would not work for the CLI at all.
func TimingTriggerFor(cur TimingInputs, prev *TimingState, changedPaths map[string]bool) TimingTrigger {
	if prev == nil {
		return TimingTriggerNewSession
	}
	if prev.Path != cur.Path {
		return TimingTriggerPathChanged
	}
	if !sameInstant(prev.UpdatedAt, cur.UpdatedAt) {
		return TimingTriggerUpdatedAt
	}
	if prev.Status != cur.Status {
		return TimingTriggerStatus
	}
	if cur.Path != "" && changedPaths[cur.Path] {
		return TimingTriggerEvent
	}
	return TimingTriggerNone
}

// sameInstant compares two nullable timestamps, treating "both absent" as
// unchanged and "one absent" as changed. A nil-vs-value comparison by == would
// compare pointers and report every rebuild as a change.
func sameInstant(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

// jobTiming is what the snapshot path gathered about one job's transcript,
// handed to the join as a value so the join stays a pure function.
type jobTiming struct {
	// Named is whether the record carries a linkScanPath at all.
	Named bool
	// Path is that path, verbatim, for the advisory that names it.
	Path string
	// Located is whether the target exists and is readable. FALSE is a
	// liveness signal (§3.5 rule 4), not merely a missing enrichment.
	Located bool
	// Facts are the timing facts in force — read this rebuild, or carried from
	// the cache under the same (sessionId, path) key.
	Facts TimingFacts
	// Trigger says why the tail was read, or "" for a cache hit.
	Trigger TimingTrigger
	// Err is a read or containment failure. It degrades to transcript-absent:
	// a record is untrusted input and a refused path is not a reason to fail
	// the snapshot.
	Err error
	// Unread is set when the caller turned transcript reads off
	// (SnapshotOptions.NoTranscriptReads): nothing was opened or even stat'd,
	// so Located is false because we did not LOOK, which is a different
	// statement from "we looked and it was not there".
	Unread bool
}
