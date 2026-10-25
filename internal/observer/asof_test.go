package observer_test

// Code review round 2, A7: answer-time derivation (§7.5c), as a pure function.
// Every case advances ONLY the consumer's clock over one published snapshot —
// there is no server, no event and no timer anywhere in this file.

import (
	"reflect"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

var asofT0 = time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)

func asofPtr[T any](v T) *T { return &v }

// asofWorld is a snapshot as the server publishes it at asofT0.
func asofWorld() *observer.Snapshot {
	since := asofT0.Add(-2 * time.Second)
	activity := asofT0.Add(-time.Minute)
	old := asofT0.Add(-10 * 24 * time.Hour)
	listing := asofT0
	staleAfter := int64(30_000)
	busySince := asofT0.Add(-29 * time.Minute)

	waiter := observer.Session{
		SessionID: "waiter", Kind: observer.KindInteractive, Status: observer.StatusWaiting,
		Attention: true, StatusSince: &since, StatusAgeMs: asofPtr(int64(2000)), BlockedForMs: asofPtr(int64(2000)),
		Live: asofPtr(true), LiveReason: observer.LiveReasonProcOnly,
		Sources: []string{observer.SourceRegistry}, ListedBy: []string{observer.SourceRegistry},
		ObservedAt: asofT0, Advisories: []observer.Advisory{},
	}
	busy := observer.Session{
		SessionID: "busy", Kind: observer.KindInteractive, Status: observer.StatusBusy,
		StatusSince: &busySince, StatusAgeMs: asofPtr(int64(29 * 60 * 1000)),
		Live: asofPtr(true), LiveReason: observer.LiveReasonProcOnly,
		Sources: []string{observer.SourceRegistry}, ListedBy: []string{observer.SourceRegistry},
		ObservedAt: asofT0, Advisories: []observer.Advisory{},
	}
	worker := observer.Session{
		SessionID: "worker", Kind: observer.KindBackground, Status: observer.StatusWorking, RawStatus: "working",
		StatusSince: &activity, StatusAgeMs: asofPtr(int64(60_000)),
		RecordUpdatedAt: &activity, RecordAgeMs: asofPtr(int64(60_000)), LastActivityAt: &activity,
		Tempo: asofPtr("active"), Live: asofPtr(true), LiveReason: observer.LiveReasonJobNonTerminalCorroborated,
		Sources:    []string{observer.SourceJobs, observer.SourceAgentsCLI, observer.SourceTranscript},
		ListedBy:   []string{observer.SourceJobs, observer.SourceAgentsCLI},
		ObservedAt: asofT0, Advisories: []observer.Advisory{},
	}
	blocked := observer.Session{
		SessionID: "blocked", Kind: observer.KindBackground, Status: observer.StatusBlocked, RawStatus: "blocked",
		Attention: true, StatusSince: &old, StatusAgeMs: asofPtr(asofT0.Sub(old).Milliseconds()),
		BlockedForMs:    asofPtr(asofT0.Sub(old).Milliseconds()),
		RecordUpdatedAt: &old, RecordAgeMs: asofPtr(asofT0.Sub(old).Milliseconds()),
		Live: asofPtr(true), LiveReason: observer.LiveReasonJobNonTerminalCorroborated,
		// No transcript: rule 4 applies once the record passes retention.
		Sources:    []string{observer.SourceJobs, observer.SourceAgentsCLI},
		ListedBy:   []string{observer.SourceJobs, observer.SourceAgentsCLI},
		ObservedAt: asofT0, Advisories: []observer.Advisory{},
	}
	finished := observer.Session{
		SessionID: "finished", Kind: observer.KindBackground, Status: observer.StatusDone, RawStatus: "done",
		RecordUpdatedAt: &old, RecordAgeMs: asofPtr(asofT0.Sub(old).Milliseconds()),
		Live: asofPtr(false), LiveReason: observer.LiveReasonJobTerminal,
		Sources: []string{observer.SourceJobs}, ListedBy: []string{observer.SourceJobs},
		ObservedAt: asofT0, Advisories: []observer.Advisory{observer.AdvisoryJobTerminal("done")},
	}
	sessions := []observer.Session{blocked, busy, finished, waiter, worker}
	counts := observer.NewCounts()
	return &observer.Snapshot{
		ObservedAt: asofT0,
		Counts:     counts,
		Sources: map[string]observer.SourceReport{
			observer.SourceAgentsCLI: {
				ID: observer.SourceAgentsCLI, Role: "corroboration", OK: true,
				ObservedAt: &listing, AgeMs: asofPtr(int64(0)), StaleAfterMs: &staleAfter,
				Advisories: []observer.Advisory{},
				Labels:     map[string]string{"crossChecksSuppressed": "false"},
			},
		},
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}
}

func asofSession(t *testing.T, snap *observer.Snapshot, id string) observer.Session {
	t.Helper()
	for _, s := range snap.Sessions {
		if s.SessionID == id {
			return s
		}
	}
	t.Fatalf("no session %s", id)
	return observer.Session{}
}

func asofCodes(s observer.Session) []string {
	var out []string
	for _, a := range s.Advisories {
		out = append(out, a.Code)
	}
	return out
}

func TestAsOfAtTheSameInstantIsTheIdentity(t *testing.T) {
	snap := asofWorld()
	if got := observer.AsOf(snap, asofT0, observer.DefaultPolicy()); got != snap {
		t.Fatal("AsOf at the snapshot's own instant built a new value")
	}
	if got := observer.AsOf(snap, asofT0.Add(-time.Minute), observer.DefaultPolicy()); got != snap {
		t.Fatal("AsOf ran time backwards")
	}
}

func TestAsOfAdvancesAgesAndBlockedDurationAndLeavesTheInputAlone(t *testing.T) {
	snap := asofWorld()
	before := asofWorld()
	now := asofT0.Add(time.Hour)
	got := observer.AsOf(snap, now, observer.DefaultPolicy())

	w := asofSession(t, got, "waiter")
	want := int64(time.Hour/time.Millisecond) + 2000
	if w.StatusAgeMs == nil || *w.StatusAgeMs != want || w.BlockedForMs == nil || *w.BlockedForMs != want {
		t.Fatalf("waiter ages = %v / %v, want %d", w.StatusAgeMs, w.BlockedForMs, want)
	}
	if !w.ObservedAt.Equal(now) || !got.ObservedAt.Equal(now) {
		t.Fatalf("observedAt not moved to the answer instant: %v / %v", w.ObservedAt, got.ObservedAt)
	}
	if !reflect.DeepEqual(snap, before) {
		t.Fatal("AsOf mutated the published snapshot")
	}
	if src := got.Sources[observer.SourceAgentsCLI]; src.AgeMs == nil || *src.AgeMs != int64(time.Hour/time.Millisecond) {
		t.Fatalf("the listing's age was not re-derived: %v", src.AgeMs)
	}
}

func TestAsOfCrossesTheClockSensitiveLivenessRules(t *testing.T) {
	now := asofT0.Add(21 * 24 * time.Hour) // the listing is long stale, the record past 30 days
	got := observer.AsOf(asofWorld(), now, observer.DefaultPolicy())

	// Rule 5: a working job whose thread has not moved for 21 days.
	w := asofSession(t, got, "worker")
	if w.Live != nil || w.LiveReason != observer.LiveReasonJobWorkingStale {
		t.Errorf("worker: live=%v reason=%s, want null job-working-stale", w.Live, w.LiveReason)
	}
	if !contains(asofCodes(w), observer.AdvBackgroundWorkingStale) {
		t.Errorf("worker advisories %v lack %s", asofCodes(w), observer.AdvBackgroundWorkingStale)
	}
	// Rule 4: no transcript, and the record is now older than retention.
	b := asofSession(t, got, "blocked")
	if b.Live != nil || b.LiveReason != observer.LiveReasonJobBeyondRetention {
		t.Errorf("blocked: live=%v reason=%s, want null job-beyond-transcript-retention", b.Live, b.LiveReason)
	}
	// The listing itself is stale now, and says so where a consumer reads it.
	src := got.Sources[observer.SourceAgentsCLI]
	if !src.Stale || src.Labels["crossChecksSuppressed"] != "true" {
		t.Errorf("agents-cli source %+v, want stale with cross-checks suppressed", src)
	}
	if len(got.Warnings) == 0 || got.Warnings[0].Code != observer.AdvSourceStale {
		t.Errorf("warnings %v lack the promoted source-stale", got.Warnings)
	}
	// Observations stand: a terminal job stays dead, an interactive gate stays.
	if f := asofSession(t, got, "finished"); f.Live == nil || *f.Live || f.LiveReason != observer.LiveReasonJobTerminal {
		t.Errorf("finished: %v %s — rule 1 is not clock-sensitive", f.Live, f.LiveReason)
	}
	if w := asofSession(t, got, "waiter"); w.Live == nil || !*w.Live {
		t.Errorf("an interactive gate verdict changed with the clock")
	}
	// Counts are recomputed from the re-derived verdicts.
	if got.Counts.Unknown != 2 || got.Counts.Live != 2 || got.Counts.Dead != 1 {
		t.Errorf("counts %+v, want live 2 / unknown 2 / dead 1", got.Counts)
	}
}

func TestAsOfRule6GoesStaleOnTheServersThreshold(t *testing.T) {
	// 31 s later: the listing (staleAfterMs 30 000, from the SERVER's report)
	// can no longer vouch; the thread is still fresh, so rule 6b(i) applies.
	got := observer.AsOf(asofWorld(), asofT0.Add(31*time.Second), observer.DefaultPolicy())
	w := asofSession(t, got, "worker")
	if w.Live != nil || w.LiveReason != observer.LiveReasonJobNonTerminalUncorroborated {
		t.Fatalf("worker: live=%v reason=%s, want null job-non-terminal-uncorroborated", w.Live, w.LiveReason)
	}
	if !contains(asofCodes(w), observer.AdvJobCorroborationStale) {
		t.Fatalf("advisories %v lack %s", asofCodes(w), observer.AdvJobCorroborationStale)
	}
	// And 29 s later nothing about liveness changes.
	early := observer.AsOf(asofWorld(), asofT0.Add(29*time.Second), observer.DefaultPolicy())
	if w := asofSession(t, early, "worker"); w.Live == nil || !*w.Live {
		t.Fatalf("worker went uncorroborated before the threshold: %v %s", w.Live, w.LiveReason)
	}
}

func TestAsOfRaisesAndUpdatesTheBusyTooLongAdvisory(t *testing.T) {
	got := observer.AsOf(asofWorld(), asofT0.Add(2*time.Minute), observer.DefaultPolicy())
	b := asofSession(t, got, "busy")
	if !contains(asofCodes(b), observer.AdvStatusSuspect) {
		t.Fatalf("busy for 31 minutes and no %s: %v", observer.AdvStatusSuspect, asofCodes(b))
	}
	later := observer.AsOf(got, asofT0.Add(10*time.Minute), observer.DefaultPolicy())
	b2 := asofSession(t, later, "busy")
	n := 0
	for _, a := range b2.Advisories {
		if a.Code == observer.AdvStatusSuspect {
			n++
			if a.Fields["statusAgeMs"] != *b2.StatusAgeMs {
				t.Fatalf("the advisory's statusAgeMs %v is not the session's %d", a.Fields["statusAgeMs"], *b2.StatusAgeMs)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d status-suspect advisories, want 1", n)
	}
}

// When the evidence on the wire cannot reproduce the published verdict at the
// server's own instant, the verdict is not re-derived: a guess would be worse
// than a stale answer.
func TestAsOfLeavesAVerdictItCannotReproduceAlone(t *testing.T) {
	snap := asofWorld()
	for i := range snap.Sessions {
		if snap.Sessions[i].SessionID == "blocked" {
			// "beyond retention" with no record time to have measured it by.
			snap.Sessions[i].LiveReason = observer.LiveReasonJobBeyondRetention
			snap.Sessions[i].Live = nil
			snap.Sessions[i].RecordUpdatedAt = nil
			snap.Sessions[i].RecordAgeMs = nil
		}
	}
	got := observer.AsOf(snap, asofT0.Add(time.Hour), observer.DefaultPolicy())
	if b := asofSession(t, got, "blocked"); b.LiveReason != observer.LiveReasonJobBeyondRetention {
		t.Fatalf("an unreproducible verdict was replaced by %s", b.LiveReason)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
