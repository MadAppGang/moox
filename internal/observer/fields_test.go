package observer

import "testing"

// TestFieldClassificationsAreDisjointAndTotal. The three slices partition the
// session's wire fields, and the partition is asserted here in the core so the
// property holds even before the wire package's own test runs. A field in two
// sets would be diffed and excluded at once; a field in none would never be
// compared.
func TestFieldClassificationsAreDisjointAndTotal(t *testing.T) {
	count := map[string]int{}
	for _, group := range [][]string{DiffedFields, ClockDerivedFields, IdentityFields} {
		for _, f := range group {
			count[f]++
		}
	}
	for f, n := range count {
		if n != 1 {
			t.Errorf("%q is classified %d times; exactly one is required", f, n)
		}
	}
	if len(count) != len(AllSessionFields) {
		t.Errorf("the three classifications cover %d fields, AllSessionFields lists %d",
			len(count), len(AllSessionFields))
	}
	for _, f := range AllSessionFields {
		if count[f] == 0 {
			t.Errorf("AllSessionFields names %q, which is in no classification", f)
		}
	}
}

// TestClockDerivedFieldsAreExactlyTheFive. These change on every rebuild by
// construction, so diffing them would emit session_changed for every session on
// every event — a delta stream that is a poll loop with extra steps. Adding a
// sixth is a deliberate act, so it fails here first.
func TestClockDerivedFieldsAreExactlyTheFive(t *testing.T) {
	want := map[string]bool{
		"statusAgeMs": true, "blockedForMs": true, "recordAgeMs": true,
		"observedAt": true, "stale": true,
	}
	if len(ClockDerivedFields) != len(want) {
		t.Fatalf("ClockDerivedFields has %d entries, want %d: %v",
			len(ClockDerivedFields), len(want), ClockDerivedFields)
	}
	for _, f := range ClockDerivedFields {
		if !want[f] {
			t.Errorf("%q is not one of the five clock-derived fields", f)
		}
	}
}

// TestLastActivityAtIsDiffedButStatusAgeIsNot is the distinction that is easy
// to get wrong and expensive when wrong: lastActivityAt is a measured timestamp
// read from a file, so it changes only when the transcript actually grows,
// whereas statusAgeMs is arithmetic against our own clock.
func TestLastActivityAtIsDiffedButStatusAgeIsNot(t *testing.T) {
	if !IsDiffed("lastActivityAt") {
		t.Error("lastActivityAt must be diffed: it is a measured timestamp, not a derivation")
	}
	if IsDiffed("statusAgeMs") {
		t.Error("statusAgeMs must not be diffed: it changes on every rebuild")
	}
	if IsDiffed("blockedForMs") {
		t.Error("blockedForMs must not be diffed: it is a view of statusAgeMs")
	}
}

// TestDiffedFieldsHasNoDuplicates guards the one slice two separate mechanisms
// iterate.
func TestDiffedFieldsHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range DiffedFields {
		if seen[f] {
			t.Errorf("DiffedFields lists %q twice", f)
		}
		seen[f] = true
	}
}

// TestSessionByID covers the lookup the frame applier and the get_session tool
// both need, including the two answers that are answers rather than errors.
func TestSessionByID(t *testing.T) {
	snap := &Snapshot{Sessions: []Session{{SessionID: "a"}, {SessionID: "b"}}}
	if s, ok := snap.SessionByID("b"); !ok || s.SessionID != "b" {
		t.Errorf("SessionByID(b) = (%v, %v)", s.SessionID, ok)
	}
	if _, ok := snap.SessionByID("zzz"); ok {
		t.Error("SessionByID found a session that does not exist")
	}
	// A nil snapshot is "no sessions", not a panic: "no Claude running" is a
	// fact, not an error.
	var nilSnap *Snapshot
	if _, ok := nilSnap.SessionByID("a"); ok {
		t.Error("a nil snapshot reported a session")
	}
}
