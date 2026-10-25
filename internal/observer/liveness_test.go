package observer

import (
	"testing"
	"time"
)

// refNow is the one reference instant. Every age below is computed from it
// rather than captured, which is the discipline the architecture's own samples
// failed three revisions running.
var refNow = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

func tPtr(t time.Time) *time.Time { return &t }

func describeLive(v *bool) string {
	switch {
	case v == nil:
		return "null"
	case *v:
		return "true"
	default:
		return "false"
	}
}

func assertVerdict(t *testing.T, got Verdict, wantLive *bool, wantReason LiveReason, wantAdvisories ...string) {
	t.Helper()
	if describeLive(got.Live) != describeLive(wantLive) {
		t.Errorf("live = %s, want %s (reason %q)", describeLive(got.Live), describeLive(wantLive), got.Reason)
	}
	if got.Reason != wantReason {
		t.Errorf("reason = %q, want %q", got.Reason, wantReason)
	}
	if len(got.Advisories) != len(wantAdvisories) {
		t.Fatalf("got %d advisories %v, want %d %v",
			len(got.Advisories), codesOf(got.Advisories), len(wantAdvisories), wantAdvisories)
	}
	for i, want := range wantAdvisories {
		if got.Advisories[i].Code != want {
			t.Errorf("advisory %d = %q, want %q", i, got.Advisories[i].Code, want)
		}
	}
}

func codesOf(as []Advisory) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Code)
	}
	return out
}

// ---------------------------------------------------------------------------
// Interactive
// ---------------------------------------------------------------------------

// TestInteractiveLiveness covers the five interactive outcomes. The ordering
// matters and is asserted: a dead pid is decided before a missing pane, because
// "the process is gone" is the more specific and more actionable answer.
func TestInteractiveLiveness(t *testing.T) {
	yes, no := true, false
	p := DefaultPolicy()

	t.Run("proc and pane both known alive", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, Status: StatusIdle, HasPID: true,
			HasTmuxRef: true, ProcAlive: &yes, PaneAlive: &yes,
		}, refNow, p)
		assertVerdict(t, v, &yes, LiveReasonProcPane)
	})

	t.Run("proc alive with no tmux reference at all", func(t *testing.T) {
		// tmux is not structurally guaranteed to be in a registry record, so
		// "no pane reference" must not read as "pane gone".
		v := Decide(Evidence{
			Kind: KindInteractive, Status: StatusBusy, HasPID: true,
			HasTmuxRef: false, ProcAlive: &yes,
		}, refNow, p)
		assertVerdict(t, v, &yes, LiveReasonProcOnly)
	})

	t.Run("dead pid", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, Status: StatusIdle, HasPID: true,
			HasTmuxRef: true, ProcAlive: &no, PaneAlive: &yes,
		}, refNow, p)
		assertVerdict(t, v, &no, LiveReasonPIDDead)
	})

	t.Run("dead pid wins over a gone pane", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, Status: StatusIdle, HasPID: true,
			HasTmuxRef: true, ProcAlive: &no, PaneAlive: &no,
		}, refNow, p)
		assertVerdict(t, v, &no, LiveReasonPIDDead)
	})

	t.Run("pane gone", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, Status: StatusIdle, HasPID: true,
			HasTmuxRef: true, ProcAlive: &yes, PaneAlive: &no,
		}, refNow, p)
		assertVerdict(t, v, &no, LiveReasonPaneGone)
	})

	t.Run("an unchecked pane is not a vouched-for pane", func(t *testing.T) {
		// list-panes failed, so PaneAlive is nil. A cached pane set is exactly
		// the stale evidence that would report a dead session as live, so the
		// answer is null rather than proc-only.
		v := Decide(Evidence{
			Kind: KindInteractive, Status: StatusIdle, HasPID: true,
			HasTmuxRef: true, ProcAlive: &yes, PaneAlive: nil,
		}, refNow, p)
		assertVerdict(t, v, nil, LiveReasonUnknown, AdvLivenessUnknown)
	})
}

// TestGateErrorKeepsThePreviousAnswer resolves a contradiction in the spec, and
// the resolution is asserted rather than left to a reader: §3.5's table labels
// the gate-error row "live = null" while its own text says "the previous value
// is KEPT". Keeping it is the substantive instruction — discarding a good
// answer on a transient gate failure would make every `tmux list-panes` hiccup
// blank the liveness of every session — so the previous value wins when there
// is one, and the answer is null only on the first snapshot. Either way the
// reason is gate-error and the advisory is attached.
func TestGateErrorKeepsThePreviousAnswer(t *testing.T) {
	yes, no := true, false
	p := DefaultPolicy()

	t.Run("previous true is kept", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, HasPID: true, GateError: true,
			GateField: "paneAlive", GateErr: "list-panes: exit 1", Previous: &yes,
		}, refNow, p)
		assertVerdict(t, v, &yes, LiveReasonGateError, AdvLivenessUnknown)
	})

	t.Run("previous false is kept", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, HasPID: true, GateError: true,
			GateField: "procAlive", GateErr: "kill: EPERM", Previous: &no,
		}, refNow, p)
		assertVerdict(t, v, &no, LiveReasonGateError, AdvLivenessUnknown)
	})

	t.Run("no previous value on the first snapshot gives null", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, HasPID: true, GateError: true,
			GateField: "procAlive", GateErr: "kill: EPERM",
		}, refNow, p)
		assertVerdict(t, v, nil, LiveReasonGateError, AdvLivenessUnknown)
	})

	t.Run("the advisory names the field and carries the error", func(t *testing.T) {
		v := Decide(Evidence{
			Kind: KindInteractive, HasPID: true, GateError: true,
			GateField: "paneAlive", GateErr: "list-panes: exit 1",
		}, refNow, p)
		a := v.Advisories[0]
		if a.Fields["field"] != "paneAlive" {
			t.Errorf("advisory field = %v", a.Fields["field"])
		}
		if a.Fields["error"] != "list-panes: exit 1" {
			t.Errorf("advisory error = %v", a.Fields["error"])
		}
	})

	t.Run("a gate error beats every other interactive signal", func(t *testing.T) {
		// Even with a positive proc+pane pair in hand, an errored gate means we
		// did not actually establish the thing we would be asserting.
		v := Decide(Evidence{
			Kind: KindInteractive, HasPID: true, HasTmuxRef: true,
			ProcAlive: &yes, PaneAlive: &yes, GateError: true, GateField: "paneAlive",
		}, refNow, p)
		if v.Reason != LiveReasonGateError {
			t.Errorf("reason = %q, want gate-error", v.Reason)
		}
	})
}

// ---------------------------------------------------------------------------
// Background — the seven rules, each on its own case, in order
// ---------------------------------------------------------------------------

func bgEvidence() Evidence {
	return Evidence{
		Kind:              KindBackground,
		Status:            StatusBlocked,
		RawState:          "blocked",
		HasJobRecord:      true,
		TranscriptPresent: true,
		RecordAgeMs:       int64Ptr(60_000),
		AgentsOK:          true,
		AgentsStale:       false,
		AgentsLists:       true,
		AgentsAgeMs:       int64Ptr(4_120),
	}
}

func int64Ptr(v int64) *int64 { return &v }

// TestBackgroundRule1Terminal: done and failed are the only two rules that can
// answer false from a record alone.
func TestBackgroundRule1Terminal(t *testing.T) {
	no := false
	for _, raw := range []string{"done", "failed"} {
		e := bgEvidence()
		e.Status, _ = MapJobState(raw)
		e.RawState = raw
		v := Decide(e, refNow, DefaultPolicy())
		assertVerdict(t, v, &no, LiveReasonJobTerminal, AdvJobTerminal)
		if v.Advisories[0].Fields["state"] != raw {
			t.Errorf("advisory state = %v, want %q", v.Advisories[0].Fields["state"], raw)
		}
	}
}

// TestBackgroundRule2Reaped must precede everything below it. A record reaped
// while blocked would otherwise be reported live and needing attention with the
// evidence sitting two keys away in the same file.
func TestBackgroundRule2Reaped(t *testing.T) {
	no := false
	e := bgEvidence()
	e.ReapedMidWorkAt = tPtr(refNow.Add(-3 * time.Hour))
	// Everything else says "corroborated live"; the reap must still win.
	v := Decide(e, refNow, DefaultPolicy())
	assertVerdict(t, v, &no, LiveReasonJobReaped, AdvJobReaped)
}

// TestBackgroundRule2bTranscriptGone: a confirmed-missing conversation ends
// the job once the record is past the grace period, even with a fresh listing
// still naming it — that listing is exactly what kept a dead job in NEEDS YOU.
func TestBackgroundRule2bTranscriptGone(t *testing.T) {
	no := false
	p := DefaultPolicy()
	e := bgEvidence()
	e.TranscriptPresent = false
	e.TranscriptGone = true
	e.RecordAgeMs = int64Ptr(p.TranscriptGraceMs + 1)
	v := Decide(e, refNow, p)
	assertVerdict(t, v, &no, LiveReasonJobTranscriptGone, AdvJobTranscriptGone)

	// Inside the grace period a job may simply not have written yet.
	e.RecordAgeMs = int64Ptr(p.TranscriptGraceMs)
	if v := Decide(e, refNow, p); v.Reason == LiveReasonJobTranscriptGone {
		t.Error("rule 2b matched a record inside the grace period")
	}
	// Absent without confirmation (refused, unreadable) is not gone.
	e.RecordAgeMs = int64Ptr(p.TranscriptGraceMs + 1)
	e.TranscriptGone = false
	if v := Decide(e, refNow, p); v.Reason == LiveReasonJobTranscriptGone {
		t.Error("rule 2b matched without a confirmed absence")
	}
}

// TestBackgroundRule3TempoDisagrees: two of the vendor's own labels contradict
// each other, so we do not know which is right — and that is the definition of
// no evidence. null, not false.
func TestBackgroundRule3TempoDisagrees(t *testing.T) {
	for _, status := range []Status{StatusBlocked, StatusWorking} {
		e := bgEvidence()
		e.Status = status
		e.RawState = string(status)
		e.Tempo = "idle"
		v := Decide(e, refNow, DefaultPolicy())
		assertVerdict(t, v, nil, LiveReasonJobTempoDisagrees, AdvTempoStateDisagreement)
		a := v.Advisories[0]
		if a.Fields["state"] != string(status) || a.Fields["tempo"] != "idle" {
			t.Errorf("advisory carried %v, want both labels", a.Fields)
		}
	}
}

// TestBackgroundRule3DoesNotFireWhenTempoAgrees: tempo "blocked" alongside
// state "blocked" is what a GENUINE wait looks like, so it must not be treated
// as a disagreement.
func TestBackgroundRule3DoesNotFireWhenTempoAgrees(t *testing.T) {
	yes := true
	e := bgEvidence()
	e.Tempo = "blocked"
	v := Decide(e, refNow, DefaultPolicy())
	assertVerdict(t, v, &yes, LiveReasonJobNonTerminalCorroborated)
}

// TestBackgroundRule4BeyondRetention: no transcript AND older than the vendor's
// own retention window means no transcript COULD exist to corroborate the
// record, so its claim is unfalsifiable.
func TestBackgroundRule4BeyondRetention(t *testing.T) {
	p := DefaultPolicy()
	e := bgEvidence()
	e.TranscriptPresent = false
	e.RecordAgeMs = int64Ptr(2_745_034_507) // 31.8 days

	v := Decide(e, refNow, p)
	assertVerdict(t, v, nil, LiveReasonJobBeyondRetention, AdvBeyondTranscriptRetention)
	a := v.Advisories[0]
	if a.Fields["recordAgeMs"] != int64(2_745_034_507) {
		t.Errorf("recordAgeMs = %v", a.Fields["recordAgeMs"])
	}
	if a.Fields["retentionMs"] != p.TranscriptRetentionMs {
		t.Errorf("retentionMs = %v, want %d", a.Fields["retentionMs"], p.TranscriptRetentionMs)
	}
}

// TestBackgroundRule4NeedsBothClauses. Either clause alone is not enough:
// a missing transcript inside the window may simply not have been written yet,
// and an old record WITH a transcript can still be corroborated.
func TestBackgroundRule4NeedsBothClauses(t *testing.T) {
	yes := true
	p := DefaultPolicy()

	t.Run("no transcript but inside the window", func(t *testing.T) {
		e := bgEvidence()
		e.TranscriptPresent = false
		e.RecordAgeMs = int64Ptr(60_000)
		v := Decide(e, refNow, p)
		assertVerdict(t, v, &yes, LiveReasonJobNonTerminalCorroborated)
	})

	t.Run("old record but a transcript exists", func(t *testing.T) {
		e := bgEvidence()
		e.TranscriptPresent = true
		e.RecordAgeMs = int64Ptr(2_745_034_507)
		v := Decide(e, refNow, p)
		assertVerdict(t, v, &yes, LiveReasonJobNonTerminalCorroborated)
	})

	t.Run("unknown record age cannot trigger an age rule", func(t *testing.T) {
		e := bgEvidence()
		e.TranscriptPresent = false
		e.RecordAgeMs = nil
		v := Decide(e, refNow, p)
		assertVerdict(t, v, &yes, LiveReasonJobNonTerminalCorroborated)
	})
}

// TestBackgroundRule5WorkingStale is measured against the TRANSCRIPT, not the
// record. A job that writes its record only at milestones is not stale merely
// because it has not written one; the transcript moves every turn, so it is the
// right freshness probe.
func TestBackgroundRule5WorkingStale(t *testing.T) {
	p := DefaultPolicy()
	e := bgEvidence()
	e.Status = StatusWorking
	e.RawState = "working"
	e.LastActivityAt = tPtr(refNow.Add(-2 * time.Hour))

	v := Decide(e, refNow, p)
	assertVerdict(t, v, nil, LiveReasonJobWorkingStale, AdvBackgroundWorkingStale)
	if v.Advisories[0].Fields["thresholdMs"] != p.WorkingStaleAfterMs {
		t.Errorf("thresholdMs = %v", v.Advisories[0].Fields["thresholdMs"])
	}
}

// TestBackgroundRule5IsClockSensitive. This is the property that forces the
// policy to be re-runnable at ANSWER time: the same evidence gives a different
// answer as the clock advances, with no filesystem event in between.
func TestBackgroundRule5IsClockSensitive(t *testing.T) {
	yes := true
	p := DefaultPolicy()
	e := bgEvidence()
	e.Status = StatusWorking
	e.RawState = "working"
	e.LastActivityAt = tPtr(refNow.Add(-10 * time.Minute))

	// Ten minutes idle, under the fifteen-minute threshold.
	assertVerdict(t, Decide(e, refNow, p), &yes, LiveReasonJobNonTerminalCorroborated)

	// Ten more minutes pass. Nothing changed on disk.
	later := refNow.Add(10 * time.Minute)
	assertVerdict(t, Decide(e, later, p), nil, LiveReasonJobWorkingStale, AdvBackgroundWorkingStale)
}

// TestBackgroundRule5NeedsAKnownLastActivity: without a transcript timestamp
// there is nothing to measure, so the rule cannot fire. It must not fall back
// to the record age, which is what an earlier revision did and what a
// milestone-writing job fails.
func TestBackgroundRule5NeedsAKnownLastActivity(t *testing.T) {
	yes := true
	e := bgEvidence()
	e.Status = StatusWorking
	e.RawState = "working"
	e.LastActivityAt = nil
	assertVerdict(t, Decide(e, refNow, DefaultPolicy()), &yes, LiveReasonJobNonTerminalCorroborated)
}

// TestBackgroundRule6RequiresAPositiveSignal is the heart of the split that
// replaced "no counter-signal matched" with "a current signal says yes".
// live: true means POSITIVE, CURRENT evidence the session can still act.
func TestBackgroundRule6RequiresAPositiveSignal(t *testing.T) {
	yes := true
	for _, status := range []Status{StatusBlocked, StatusWorking} {
		e := bgEvidence()
		e.Status = status
		e.RawState = string(status)
		assertVerdict(t, Decide(e, refNow, DefaultPolicy()), &yes, LiveReasonJobNonTerminalCorroborated)
	}
}

// TestBackgroundRule6bIsTheExactComplementOfRule6. Three sub-cases, one
// outcome, distinguished only by the advisory — so there is no third branch and
// no gap.
func TestBackgroundRule6bIsTheExactComplementOfRule6(t *testing.T) {
	p := DefaultPolicy()

	t.Run("(i) the listing is stale", func(t *testing.T) {
		e := bgEvidence()
		e.AgentsStale = true
		e.AgentsAgeMs = int64Ptr(45_000)
		v := Decide(e, refNow, p)
		assertVerdict(t, v, nil, LiveReasonJobNonTerminalUncorroborated, AdvJobCorroborationStale)
		if v.Advisories[0].Fields["agentsAgeMs"] != int64(45_000) {
			t.Errorf("agentsAgeMs = %v", v.Advisories[0].Fields["agentsAgeMs"])
		}
		if v.Advisories[0].Fields["staleAfterMs"] != p.StaleAfterMs {
			t.Errorf("staleAfterMs = %v", v.Advisories[0].Fields["staleAfterMs"])
		}
	})

	t.Run("(ii) the listing is absent or errored", func(t *testing.T) {
		e := bgEvidence()
		e.AgentsOK = false
		e.AgentsAgeMs = nil
		e.AgentsLists = false
		v := Decide(e, refNow, p)
		assertVerdict(t, v, nil, LiveReasonJobNonTerminalUncorroborated, AdvJobCorroborationStale)
		if got := v.Advisories[0].Fields["agentsAgeMs"]; got != nil {
			t.Errorf("agentsAgeMs = %v, want nil when no listing was taken", got)
		}
	})

	t.Run("(iii) the listing is fresh but does not name this session", func(t *testing.T) {
		// This is the case that falsifies the terminal-state rule. Under the
		// earlier "no counter-signal matched" rule, a record the vendor had
		// stopped listing still read live: true, because absence from a fresh
		// listing did not count as a counter-signal. It must never read true.
		e := bgEvidence()
		e.AgentsLists = false
		v := Decide(e, refNow, p)
		assertVerdict(t, v, nil, LiveReasonJobNonTerminalUncorroborated, AdvBackgroundGateDisagreement)
		a := v.Advisories[0]
		if a.Fields["recordState"] != "blocked" || a.Fields["listed"] != false {
			t.Errorf("advisory carried %v, want both values", a.Fields)
		}
	})

	t.Run("rule 6 and 6b partition the non-terminal space", func(t *testing.T) {
		// Every combination of the three corroboration inputs must land in
		// exactly one of the two rules, and never anywhere else.
		for _, ok := range []bool{true, false} {
			for _, stale := range []bool{true, false} {
				for _, lists := range []bool{true, false} {
					e := bgEvidence()
					e.AgentsOK, e.AgentsStale, e.AgentsLists = ok, stale, lists
					v := Decide(e, refNow, p)
					corroborated := ok && !stale && lists
					want := LiveReasonJobNonTerminalUncorroborated
					if corroborated {
						want = LiveReasonJobNonTerminalCorroborated
					}
					if v.Reason != want {
						t.Errorf("ok=%v stale=%v lists=%v: reason %q, want %q",
							ok, stale, lists, v.Reason, want)
					}
					if !corroborated && v.Live != nil {
						t.Errorf("ok=%v stale=%v lists=%v: live = %s, want null",
							ok, stale, lists, describeLive(v.Live))
					}
				}
			}
		}
	})
}

// TestBackgroundRule7UnrecognisedState: a state we do not know answers null
// with an advisory, never a guess.
func TestBackgroundRule7UnrecognisedState(t *testing.T) {
	e := bgEvidence()
	e.Status = StatusUnknown
	e.RawState = "reaping"
	assertVerdict(t, Decide(e, refNow, DefaultPolicy()), nil, LiveReasonUnknown, AdvLivenessUnknown)
}

// TestBackgroundWithNoJobRecordAtAll: a session the vendor lists with no record
// on disk gives us membership and nothing else.
func TestBackgroundWithNoJobRecordAtAll(t *testing.T) {
	e := bgEvidence()
	e.HasJobRecord = false
	assertVerdict(t, Decide(e, refNow, DefaultPolicy()), nil, LiveReasonUnknown, AdvLivenessUnknown)
}

// TestTodaysThreeMeasuredJobs reproduces the three real background records the
// architecture measured, which is the test of whether the rule means anything:
// one of three is asserted live, and it is the one whose 10.7 MB transcript
// corroborates its record to 101 ms. The other two are reported with their
// evidence and no assertion.
func TestTodaysThreeMeasuredJobs(t *testing.T) {
	yes := true
	p := DefaultPolicy()

	t.Run("ea780079 39.1d tempo idle vs state blocked -> rule 3", func(t *testing.T) {
		e := bgEvidence()
		e.Tempo = "idle"
		e.RecordAgeMs = int64Ptr(3_378_240_000)
		e.TranscriptPresent = false
		v := Decide(e, refNow, p)
		assertVerdict(t, v, nil, LiveReasonJobTempoDisagrees, AdvTempoStateDisagreement)
		// Reported with its evidence, and NOT excluded from the default view.
		if !IncludedByDefault(v.Live, DefaultSnapshotOptions()) {
			t.Error("a null-liveness record must stay in the default view")
		}
	})

	t.Run("7a324a6d 31.8d no transcript past the floor -> rule 4", func(t *testing.T) {
		e := bgEvidence()
		e.Tempo = "blocked"
		e.TranscriptPresent = false
		e.RecordAgeMs = int64Ptr(2_745_034_507)
		v := Decide(e, refNow, p)
		assertVerdict(t, v, nil, LiveReasonJobBeyondRetention, AdvBeyondTranscriptRetention)
		if !IncludedByDefault(v.Live, DefaultSnapshotOptions()) {
			t.Error("a null-liveness record must stay in the default view")
		}
	})

	t.Run("97030e53 24.0d fresh listing names it -> rule 6", func(t *testing.T) {
		e := bgEvidence()
		e.Tempo = "blocked"
		e.TranscriptPresent = true
		e.RecordAgeMs = int64Ptr(2_073_600_000)
		e.AgentsAgeMs = int64Ptr(4_120)
		assertVerdict(t, Decide(e, refNow, p), &yes, LiveReasonJobNonTerminalCorroborated)
	})

	t.Run("97030e53 with corroboration older than 30s -> rule 6b", func(t *testing.T) {
		e := bgEvidence()
		e.Tempo = "blocked"
		e.TranscriptPresent = true
		e.RecordAgeMs = int64Ptr(2_073_600_000)
		e.AgentsStale = true
		e.AgentsAgeMs = int64Ptr(31_000)
		assertVerdict(t, Decide(e, refNow, p), nil,
			LiveReasonJobNonTerminalUncorroborated, AdvJobCorroborationStale)
	})
}

// TestDecideIsTotal asserts the policy always produces a reason. A verdict with
// no reason would be an unreachable branch the tests had silently stopped
// covering.
func TestDecideIsTotal(t *testing.T) {
	yes, no := true, false
	boolStates := []*bool{nil, &yes, &no}
	p := DefaultPolicy()

	for _, kind := range AllKinds {
		for _, status := range AllStatuses {
			for _, proc := range boolStates {
				for _, pane := range boolStates {
					for _, hasPID := range []bool{true, false} {
						e := Evidence{
							Kind: kind, Status: status, RawState: string(status),
							HasPID: hasPID, HasTmuxRef: pane != nil,
							ProcAlive: proc, PaneAlive: pane,
							HasJobRecord: true,
						}
						v := Decide(e, refNow, p)
						if v.Reason == LiveReasonNone {
							t.Fatalf("no reason for kind=%q status=%q hasPID=%v proc=%s pane=%s",
								kind, status, hasPID, describeLive(proc), describeLive(pane))
						}
						found := false
						for _, r := range AllLiveReasons {
							if r == v.Reason {
								found = true
								break
							}
						}
						if !found {
							t.Fatalf("reason %q is not in AllLiveReasons", v.Reason)
						}
					}
				}
			}
		}
	}
}

// TestIncludedByDefault pins M7 and M14 in one place: default filtering is
// live != false, so a session vanishing from the default view requires POSITIVE
// evidence of death, and an unknown-liveness session is never silently hidden.
func TestIncludedByDefault(t *testing.T) {
	yes, no := true, false
	def := DefaultSnapshotOptions()

	if !IncludedByDefault(&yes, def) {
		t.Error("a live session must be in the default view")
	}
	if IncludedByDefault(&no, def) {
		t.Error("a dead session must be out of the default view")
	}
	if !IncludedByDefault(nil, def) {
		t.Error("an unknown-liveness session must be in the default view")
	}

	withDead := def
	withDead.IncludeDead = true
	if !IncludedByDefault(&no, withDead) {
		t.Error("includeDead must add the dead set")
	}

	withoutUnknown := def
	withoutUnknown.IncludeUnknownLiveness = false
	if IncludedByDefault(nil, withoutUnknown) {
		t.Error("an explicit opt-out must be able to exclude unknown liveness")
	}
}

// TestDefaultPolicyValues pins the four thresholds so a change is deliberate.
// Three are policy guesses; the retention default is the vendor's documented
// one, confirmed in force by a measured 30.03-day floor.
func TestDefaultPolicyValues(t *testing.T) {
	p := DefaultPolicy()
	if p.StaleAfterMs != 30_000 {
		t.Errorf("staleAfterMs = %d, want 30000", p.StaleAfterMs)
	}
	if p.WorkingStaleAfterMs != 900_000 {
		t.Errorf("workingStaleAfterMs = %d, want 900000 (15 min)", p.WorkingStaleAfterMs)
	}
	if p.TranscriptRetentionMs != 2_592_000_000 {
		t.Errorf("transcriptRetentionMs = %d, want 2592000000 (30 days)", p.TranscriptRetentionMs)
	}
	if p.SuspectBusyAfterMs != 1_800_000 {
		t.Errorf("suspectBusyAfterMs = %d, want 1800000 (30 min)", p.SuspectBusyAfterMs)
	}
}

// TestBackgroundRule2cFolderGone: a job whose folder was removed cannot be
// attached where it lives, so even a fresh listing naming it is not "live" —
// but its conversation may survive, so the answer is null, not false.
func TestBackgroundRule2cFolderGone(t *testing.T) {
	e := bgEvidence()
	e.CWD = "/home/user/dotfiles/.claude/worktrees/tt-ui-rebuil"
	e.CWDGone = true
	v := Decide(e, refNow, DefaultPolicy())
	assertVerdict(t, v, nil, LiveReasonJobFolderGone, AdvJobFolderGone)
	if v.Advisories[0].Fields["cwd"] != e.CWD {
		t.Errorf("advisory fields = %v, want the cwd", v.Advisories[0].Fields)
	}

	// A confirmed-missing transcript is the stronger statement and wins.
	e.TranscriptPresent = false
	e.TranscriptGone = true
	e.RecordAgeMs = int64Ptr(DefaultPolicy().TranscriptGraceMs + 1)
	if v := Decide(e, refNow, DefaultPolicy()); v.Reason != LiveReasonJobTranscriptGone {
		t.Errorf("reason = %s, want rule 2b ahead of 2c", v.Reason)
	}
}
