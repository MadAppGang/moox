package observer

import "testing"

// TestMapRegistryStatus covers the registry half of §3.3's mapping table,
// including the row that matters most: anything unrecognised becomes unknown
// and carries no wait reason, so a vendor vocabulary change degrades into "no
// opinion" rather than into a wrong label.
func TestMapRegistryStatus(t *testing.T) {
	cases := []struct {
		raw, waitingFor string
		wantStatus      Status
		wantReason      WaitReason
	}{
		{"idle", "", StatusIdle, WaitReasonNone},
		{"busy", "", StatusBusy, WaitReasonNone},
		{"shell", "", StatusShell, WaitReasonNone},
		// The only waitingFor value observed live, in 1920 samples.
		{"waiting", "input needed", StatusWaiting, WaitReasonInputNeeded},
		// Named in the vendor's strings, never observed; its fixture is
		// SYNTHETIC, which is the difference between testing the path and
		// testing our guess at it.
		{"waiting", "permission prompt", StatusWaiting, WaitReasonPermissionPrompt},
		// Waiting with nothing to say why is "unknown", not "" — the session IS
		// waiting, we just cannot say for what.
		{"waiting", "", StatusWaiting, WaitReasonUnknown},
		{"waiting", "something new", StatusWaiting, WaitReasonUnknown},
		// A waitingFor on a non-waiting status is ignored, not promoted.
		{"idle", "input needed", StatusIdle, WaitReasonNone},
		{"compacting", "", StatusUnknown, WaitReasonNone},
		{"", "", StatusUnknown, WaitReasonNone},
		{"IDLE", "", StatusUnknown, WaitReasonNone},
	}
	for _, c := range cases {
		gotStatus, gotReason := MapRegistryStatus(c.raw, c.waitingFor)
		if gotStatus != c.wantStatus || gotReason != c.wantReason {
			t.Errorf("MapRegistryStatus(%q, %q) = (%q, %q), want (%q, %q)",
				c.raw, c.waitingFor, gotStatus, gotReason, c.wantStatus, c.wantReason)
		}
	}
}

// TestMapJobStateNeverProducesAWaitReason is §3.3's H3 rule at its source:
// every job state gives "", including blocked. blocked's reason lives in
// `needs`, in plain words, and waitReason is the waitingFor enum's domain —
// blocked does not come from waitingFor, so giving it a waiting-vocabulary
// value would re-introduce by the back door the conflation Status refuses.
func TestMapJobStateNeverProducesAWaitReason(t *testing.T) {
	cases := map[string]Status{
		"blocked":   StatusBlocked,
		"working":   StatusWorking,
		"done":      StatusDone,
		"failed":    StatusFailed,
		"reaping":   StatusUnknown,
		"":          StatusUnknown,
		"BLOCKED":   StatusUnknown,
		"completed": StatusUnknown,
	}
	for raw, want := range cases {
		got, reason := MapJobState(raw)
		if got != want {
			t.Errorf("MapJobState(%q) = %q, want %q", raw, got, want)
		}
		if reason != WaitReasonNone {
			t.Errorf("MapJobState(%q) gave waitReason %q; every job state must give \"\"", raw, reason)
		}
	}
}

// TestBlockedIsNotWaitingAndWorkingIsNotBusy pins the two non-foldings. A
// consumer rendering "4 sessions need your input" from a union would send the
// user hunting for panes that do not exist: background agents have no tmux
// reference, no pid and no pane.
func TestBlockedIsNotWaitingAndWorkingIsNotBusy(t *testing.T) {
	if blocked, _ := MapJobState("blocked"); blocked == StatusWaiting {
		t.Error("blocked was folded into waiting")
	}
	if working, _ := MapJobState("working"); working == StatusBusy {
		t.Error("working was folded into busy")
	}
}

// TestMapAgentsStatusPicksTheTableByPopulatedField. The CLI reports both
// populations through one shape: an interactive entry carries status, a
// background one carries state.
func TestMapAgentsStatusPicksTheTableByPopulatedField(t *testing.T) {
	cases := []struct {
		status, state, waitingFor string
		wantStatus                Status
		wantReason                WaitReason
	}{
		{"waiting", "", "input needed", StatusWaiting, WaitReasonInputNeeded},
		{"idle", "", "", StatusIdle, WaitReasonNone},
		{"", "blocked", "", StatusBlocked, WaitReasonNone},
		{"", "done", "", StatusDone, WaitReasonNone},
		{"", "", "", StatusUnknown, WaitReasonNone},
		// Both populated: the registry vocabulary wins, because an entry with a
		// status is an interactive one and that is the table it belongs to.
		{"busy", "blocked", "", StatusBusy, WaitReasonNone},
	}
	for _, c := range cases {
		gotStatus, gotReason := MapAgentsStatus(c.status, c.state, c.waitingFor)
		if gotStatus != c.wantStatus || gotReason != c.wantReason {
			t.Errorf("MapAgentsStatus(%q,%q,%q) = (%q,%q), want (%q,%q)",
				c.status, c.state, c.waitingFor, gotStatus, gotReason, c.wantStatus, c.wantReason)
		}
	}
}

// TestMapKindKeepsTheRawLiteral: an unrecognised kind becomes unknown AND
// keeps what the vendor said, which is the only way a format change is
// diagnosable after the fact.
func TestMapKindKeepsTheRawLiteral(t *testing.T) {
	if k, raw := MapKind("interactive"); k != KindInteractive || raw != nil {
		t.Errorf(`MapKind("interactive") = (%q, %v)`, k, raw)
	}
	if k, raw := MapKind("background"); k != KindBackground || raw != nil {
		t.Errorf(`MapKind("background") = (%q, %v)`, k, raw)
	}
	k, raw := MapKind("swarm")
	if k != KindUnknown {
		t.Errorf(`MapKind("swarm") kind = %q, want unknown`, k)
	}
	if raw == nil || *raw != "swarm" {
		t.Errorf(`MapKind("swarm") did not keep the raw literal: %v`, raw)
	}
}

// TestStatusPredicates pins the three derived questions, and in particular that
// `failed` is NOT attention: a failed agent needs reading, not answering.
func TestStatusPredicates(t *testing.T) {
	for _, s := range AllStatuses {
		wantTerminal := s == StatusDone || s == StatusFailed
		if s.IsTerminal() != wantTerminal {
			t.Errorf("%q.IsTerminal() = %v, want %v", s, s.IsTerminal(), wantTerminal)
		}
		wantAttention := s == StatusWaiting || s == StatusBlocked
		if s.IsAttention() != wantAttention {
			t.Errorf("%q.IsAttention() = %v, want %v", s, s.IsAttention(), wantAttention)
		}
		wantNonTerminalJob := s == StatusBlocked || s == StatusWorking
		if s.IsNonTerminalJob() != wantNonTerminalJob {
			t.Errorf("%q.IsNonTerminalJob() = %v, want %v", s, s.IsNonTerminalJob(), wantNonTerminalJob)
		}
	}
	if StatusFailed.IsAttention() {
		t.Error("failed must not be attention")
	}
}

// TestVocabularySlicesAreComplete guards the enum lists the MCP schema and
// Counts both iterate. A status added to the consts and not to AllStatuses
// would silently vanish from byStatus and from the filter enum.
func TestVocabularySlicesAreComplete(t *testing.T) {
	if len(AllStatuses) != 9 {
		t.Errorf("AllStatuses has %d entries, want 9", len(AllStatuses))
	}
	if len(AllKinds) != 3 {
		t.Errorf("AllKinds has %d entries, want 3", len(AllKinds))
	}
	seen := map[Status]bool{}
	for _, s := range AllStatuses {
		if seen[s] {
			t.Errorf("AllStatuses lists %q twice", s)
		}
		seen[s] = true
	}
	// unknown must be in the vocabulary: with a closed MCP enum and
	// additionalProperties:false, omitting it would make a legal record
	// unrepresentable in a filter.
	if !seen[StatusUnknown] {
		t.Error("AllStatuses omits unknown")
	}
	kinds := map[Kind]bool{}
	for _, k := range AllKinds {
		kinds[k] = true
	}
	if !kinds[KindUnknown] {
		t.Error("AllKinds omits unknown")
	}
}

// TestNewCountsCarriesEveryKeyAtZero: the encoder emits every key including
// zeros, so the domain has to be able to produce them without the caller
// checking for presence.
func TestNewCountsCarriesEveryKeyAtZero(t *testing.T) {
	c := NewCounts()
	for _, s := range AllStatuses {
		if n, ok := c.ByStatus[s]; !ok || n != 0 {
			t.Errorf("ByStatus[%q] = (%d, %v), want (0, true)", s, n, ok)
		}
	}
	for _, k := range AllKinds {
		if n, ok := c.ByKind[k]; !ok || n != 0 {
			t.Errorf("ByKind[%q] = (%d, %v), want (0, true)", k, n, ok)
		}
	}
}
