package observer

import (
	"reflect"
	"testing"
	"time"
)

// §3.4's INVARIANT B, unit-tested against ALL FIVE of its predicates plus the
// old-path invalidation. Five predicates that all produce "a read" are five
// untested rules, which is why TimingTriggerFor names the one that fired.

func at(ms int64) *time.Time {
	t := time.UnixMilli(ms).UTC()
	return &t
}

const pathA = "/p/-slug/aaaa.jsonl"
const pathB = "/p/-slug/bbbb.jsonl"

func steady() (TimingInputs, TimingState) {
	cur := TimingInputs{
		SessionID: "s1",
		Path:      pathA,
		UpdatedAt: at(1789699200000),
		Status:    StatusBlocked,
	}
	prev := TimingState{Path: cur.Path, UpdatedAt: cur.UpdatedAt, Status: cur.Status}
	return cur, prev
}

func TestInvariantBCase1ANewJobIsAlwaysRead(t *testing.T) {
	// The case revision 2's read-on-notification rule could not serve: a FIRST
	// state.json whose transcript was already complete before the record
	// existed. Nothing will ever append, so no event will ever fire, and if the
	// job is `blocked` lastActivityAt stays null FOREVER — non-convergence, not
	// a staleness residual.
	cur, _ := steady()
	if got := TimingTriggerFor(cur, nil, nil); got != TimingTriggerNewSession {
		t.Errorf("trigger = %q, want %q", got, TimingTriggerNewSession)
	}
}

func TestInvariantBCase2ARepointedLinkScanPathIsRead(t *testing.T) {
	// A resume repoints linkScanPath at a different conversation. The new
	// transcript was written while the OLD path was armed, so the new one is
	// armed only afterwards and its append is already in the past.
	cur, prev := steady()
	cur.Path = pathB
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerPathChanged {
		t.Errorf("trigger = %q, want %q", got, TimingTriggerPathChanged)
	}
}

func TestInvariantBCase3AChangedUpdatedAtIsRead(t *testing.T) {
	cur, prev := steady()
	cur.UpdatedAt = at(1789699205000)
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerUpdatedAt {
		t.Errorf("trigger = %q, want %q", got, TimingTriggerUpdatedAt)
	}
}

func TestInvariantBCase3TreatsAppearingAndDisappearingTimestampsAsChanges(t *testing.T) {
	// A nil-vs-value comparison by == compares POINTERS and reports every
	// rebuild as a change; a nil-vs-nil one by .Equal panics. Both directions
	// have to be right or the predicate is either always or never true.
	cur, prev := steady()

	cur.UpdatedAt = nil
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerUpdatedAt {
		t.Errorf("value -> nil: trigger = %q, want %q", got, TimingTriggerUpdatedAt)
	}

	cur, prev = steady()
	prev.UpdatedAt = nil
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerUpdatedAt {
		t.Errorf("nil -> value: trigger = %q, want %q", got, TimingTriggerUpdatedAt)
	}

	cur, prev = steady()
	cur.UpdatedAt, prev.UpdatedAt = nil, nil
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerNone {
		t.Errorf("nil -> nil: trigger = %q, want none; a pointer comparison fires here forever", got)
	}

	cur, prev = steady()
	same := *cur.UpdatedAt
	prev.UpdatedAt = &same
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerNone {
		t.Errorf("equal instants in two pointers: trigger = %q, want none", got)
	}
}

func TestInvariantBCase4AChangedStatusIsRead(t *testing.T) {
	cur, prev := steady()
	cur.Status = StatusWorking
	if got := TimingTriggerFor(cur, &prev, nil); got != TimingTriggerStatus {
		t.Errorf("trigger = %q, want %q", got, TimingTriggerStatus)
	}
}

func TestInvariantBCase5ATranscriptEventIsRead(t *testing.T) {
	// Revision 2's WHOLE rule, retained as one of five.
	cur, prev := steady()
	changed := map[string]bool{pathA: true}
	if got := TimingTriggerFor(cur, &prev, changed); got != TimingTriggerEvent {
		t.Errorf("trigger = %q, want %q", got, TimingTriggerEvent)
	}
}

func TestInvariantBCase5DoesNotFireForANeighboursEvent(t *testing.T) {
	cur, prev := steady()
	changed := map[string]bool{pathB: true, "": true}
	if got := TimingTriggerFor(cur, &prev, changed); got != TimingTriggerNone {
		t.Errorf("trigger = %q, want none — another session's transcript changed", got)
	}
}

func TestInvariantBFiresForNobodyInTheSteadyState(t *testing.T) {
	// This is what makes the widening affordable rather than a poll in
	// disguise. Four of the five predicates are CHANGE predicates over the join
	// key set, so a rebuild triggered by an unrelated registry .key write
	// re-reads no transcript at all.
	cur, prev := steady()
	if got := TimingTriggerFor(cur, &prev, map[string]bool{}); got != TimingTriggerNone {
		t.Errorf("trigger = %q, want none", got)
	}
}

func TestInvariantBEvaluatesItsPredicatesInTheSpecifiedOrder(t *testing.T) {
	// The order matters for what a test and a log can say, not for whether a
	// read happens. A record that is new AND repointed AND updated AND
	// restatused AND has fired an event must report case 1: "new" is the reason
	// there was nothing to compare against in the first place.
	cur, _ := steady()
	if got := TimingTriggerFor(cur, nil, map[string]bool{pathA: true}); got != TimingTriggerNewSession {
		t.Errorf("trigger = %q, want %q", got, TimingTriggerNewSession)
	}

	cur, prev := steady()
	cur.Path = pathB
	cur.UpdatedAt = at(1789699205000)
	cur.Status = StatusWorking
	if got := TimingTriggerFor(cur, &prev, map[string]bool{pathB: true}); got != TimingTriggerPathChanged {
		t.Errorf("trigger = %q, want %q — the path change is what invalidates", got, TimingTriggerPathChanged)
	}
}

func TestEveryDeclaredTriggerIsReachable(t *testing.T) {
	// A predicate nobody can produce is a rule nobody has tested. This
	// enumerates AllTimingTriggers and constructs an input for each, so a
	// sixth predicate added later without a case here fails.
	reached := map[TimingTrigger]bool{}

	cur, prev := steady()
	reached[TimingTriggerFor(cur, nil, nil)] = true

	cur, prev = steady()
	cur.Path = pathB
	reached[TimingTriggerFor(cur, &prev, nil)] = true

	cur, prev = steady()
	cur.UpdatedAt = at(1)
	reached[TimingTriggerFor(cur, &prev, nil)] = true

	cur, prev = steady()
	cur.Status = StatusDone
	reached[TimingTriggerFor(cur, &prev, nil)] = true

	cur, prev = steady()
	reached[TimingTriggerFor(cur, &prev, map[string]bool{pathA: true})] = true

	for _, trig := range AllTimingTriggers {
		if !reached[trig] {
			t.Errorf("trigger %q is declared and was not reached by any case here", trig)
		}
	}
}

// ---------------------------------------------------------------------------
// the (sessionId, linkScanPath) cache key
// ---------------------------------------------------------------------------

func TestTheTimingCacheKeyIncludesThePathAndNotOnlyTheSession(t *testing.T) {
	// §7.3, and this is the cheapest possible place for Invariant B case 2 to
	// be impossible to forget. With a sessionId-only key a resume returns the
	// PREVIOUS conversation's last turn as the new thread's last activity — and
	// invisibly, because both answers are well-formed timestamps.
	a := TimingCacheKey{SessionID: "s1", Path: pathA}
	b := TimingCacheKey{SessionID: "s1", Path: pathB}
	if a == b {
		t.Fatal("two paths under one session produce the same key")
	}

	cache := map[TimingCacheKey]TimingFacts{a: {LastActivityAt: at(1000)}}
	if _, hit := cache[b]; hit {
		t.Error("the new path hit the old path's entry")
	}
}

func TestTheCacheKeyDeliberatelyExcludesSizeAndMtime(t *testing.T) {
	// Unlike the enrichment cache's (sessionId, path, size, mtime), the timing
	// key is two fields. Invariant B's change predicates already decide when to
	// re-read; adding size or mtime would ALSO re-read on every append, turning
	// a bounded correctness read into one that fires on every transcript write.
	//
	// Asserting the struct's field count is how that stays a decision: adding a
	// third field fails here and the person adding it reads this comment.
	if got := reflectFieldCount(TimingCacheKey{}); got != 2 {
		t.Errorf("TimingCacheKey has %d fields, want 2 (sessionId, path)", got)
	}
}

func TestTheTriggerIsEmptyStringForACacheHitSoTheZeroValueIsSafe(t *testing.T) {
	// A zero TimingTrigger must mean "nothing fired". If the zero value meant
	// "new", every uninitialised struct would force a read.
	if TimingTriggerNone != "" {
		t.Errorf("TimingTriggerNone = %q, want the zero value", TimingTriggerNone)
	}
	var zero TimingTrigger
	if zero != TimingTriggerNone {
		t.Error("the zero TimingTrigger is not TimingTriggerNone")
	}
}

func TestTheEpsToleranceIsTheSpecifiedTwoSeconds(t *testing.T) {
	// §3.4's cross-check tolerance. Generous by three orders of magnitude
	// against a measured 101 ms agreement, and the cases it catches are at
	// +5 days and +20 days, so the value is not a tuning knob.
	if StatusSinceEpsMs != 2000 {
		t.Errorf("StatusSinceEpsMs = %d, want 2000", StatusSinceEpsMs)
	}
}

// reflectFieldCount is a one-line helper kept here rather than in a shared
// place: it exists for exactly one assertion.
func reflectFieldCount(v any) int { return reflect.TypeOf(v).NumField() }
