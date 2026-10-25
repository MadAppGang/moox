package observer

// The generation rule and the backoff arithmetic, tested from INSIDE the
// package.
//
// Why inside: §7.2's "obsolete results are dropped" is a property of
// applySpawn, and the only way to reach it from outside would be to export a
// test hook — a method on the production type that exists for a test to call,
// which is how a core grows an API nobody wanted. An internal test file cannot
// import `fakes` or any adapter (both import this package, so either would be
// an import cycle), hence the deliberately minimal stubs below: they exist so
// New() succeeds, and nothing here calls a port at all.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- the smallest ports that satisfy New. Nothing below calls any of them. ---

type stubRegistry struct{}

func (stubRegistry) Read(context.Context) (RegistryResult, error) { return RegistryResult{}, nil }

type stubJobs struct{}

func (stubJobs) Read(context.Context) (JobsResult, error) { return JobsResult{}, nil }

type stubTranscripts struct{}

func (stubTranscripts) FindByGlob(context.Context, string) (TranscriptLocation, error) {
	return TranscriptLocation{}, nil
}
func (stubTranscripts) OpenPath(context.Context, string) (TranscriptLocation, error) {
	return TranscriptLocation{}, nil
}
func (stubTranscripts) ReadTiming(context.Context, TranscriptLocation, int) (TimingFacts, error) {
	return TimingFacts{}, nil
}
func (stubTranscripts) ReadFacts(context.Context, TranscriptLocation, int) (TranscriptFacts, error) {
	return TranscriptFacts{}, nil
}

type stubProc struct{}

func (stubProc) Alive(int) (bool, error) { return false, nil }
func (stubProc) Parent(int) (int, error) { return 0, errors.New("stub: no process tree") }

type stubPanes struct{}

func (stubPanes) ListPanes(context.Context) (map[string]bool, error) { return nil, nil }

type stubClock struct{ now time.Time }

func (c stubClock) Now() time.Time { return c.now }

func newGenObserver(t *testing.T, now time.Time) *Observer {
	t.Helper()
	obs, err := New(Deps{
		Registry:    stubRegistry{},
		Jobs:        stubJobs{},
		Transcripts: stubTranscripts{},
		Proc:        stubProc{},
		Panes:       stubPanes{},
		Clock:       stubClock{now: now},
	}, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return obs
}

func listingOf(at time.Time, ids ...string) AgentsResult {
	entries := make([]AgentsEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, AgentsEntry{SessionID: id, Kind: "background", State: "blocked"})
	}
	return AgentsResult{Entries: entries, ObservedAt: at, OK: true, Cache: AgentsCacheMiss}
}

func heldIDs(o *Observer) []string {
	res := o.corroborationListing()
	out := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		out = append(out, e.SessionID)
	}
	return out
}

// ---------------------------------------------------------------------------
// §7.2 — obsolete results are dropped
// ---------------------------------------------------------------------------

func TestAnObsoleteSpawnResultIsDropped(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 40, 11, 0, time.UTC)
	o := newGenObserver(t, now)

	if !o.applySpawn(listingOf(now, "first"), 1) {
		t.Fatal("generation 1 was not adopted")
	}
	if !o.applySpawn(listingOf(now.Add(time.Second), "second"), 2) {
		t.Fatal("generation 2 was not adopted")
	}

	// A slow spawn from generation 1 finally answers. It must NOT overwrite the
	// newer corroboration — that is the whole of the generation counter.
	if o.applySpawn(listingOf(now.Add(2*time.Second), "stale-answer"), 1) {
		t.Fatal("an older generation was adopted over a newer one")
	}
	if got := heldIDs(o); len(got) != 1 || got[0] != "second" {
		t.Fatalf("held listing = %v, want the generation-2 one", got)
	}

	// And the SAME generation twice is dropped too: "strictly newer" rather
	// than "not older", so a retried or duplicated result cannot resurrect a
	// listing that has since been superseded.
	if o.applySpawn(listingOf(now.Add(3*time.Second), "duplicate"), 2) {
		t.Fatal("the same generation was adopted twice")
	}
	if got := heldIDs(o); got[0] != "second" {
		t.Fatalf("held listing = %v after a duplicate generation", got)
	}
}

func TestAFailedResultNeverReplacesTheHeldListing(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 40, 11, 0, time.UTC)
	o := newGenObserver(t, now)

	o.applySpawn(listingOf(now, "held"), 1)
	if o.applySpawn(AgentsResult{ObservedAt: now.Add(time.Second), OK: false,
		Err: errors.New("claude: command not found"), Cache: AgentsCacheMiss}, 2) {
		t.Fatal("a failed result was adopted")
	}

	if got := heldIDs(o); len(got) != 1 || got[0] != "held" {
		t.Fatalf("held listing = %v, want the surviving one", got)
	}
	// The failure is a value: it is reported, and it arms the backoff.
	rep := o.agentsReport(o.corroborationListing(), now.Add(time.Second))
	if rep.LastError == nil {
		t.Fatal("no lastError after a failed spawn")
	}
	if rep.Counters["entries"] != 1 {
		t.Fatalf("entries = %d, want the surviving listing's 1", rep.Counters["entries"])
	}
}

func TestAContendedResultIsNotAFailureAndDoesNotArmBackoff(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 40, 11, 0, time.UTC)
	o := newGenObserver(t, now)

	o.applySpawn(AgentsResult{
		ObservedAt: now, OK: false, Cache: AgentsCacheContended, WaitedMs: 250,
	}, 1)

	// Nothing went wrong: another process was holding the spawn right. Backing
	// off for that would punish this process for another's success.
	if reason, _ := o.takeCorroborationReasonForTest(now); reason == RejectBackoff {
		t.Fatal("contention armed the backoff")
	}
}

// takeCorroborationReasonForTest asks what the gate would answer right now,
// without a port to call. It exists because the backoff is the one piece of
// state whose absence is the assertion.
func (o *Observer) takeCorroborationReasonForTest(now time.Time) (string, []Advisory) {
	o.corr.mu.Lock()
	defer o.corr.mu.Unlock()
	if o.corr.degraded != "" {
		return RejectRateLimited, nil
	}
	if now.Before(o.corr.backoffUntil) {
		return RejectBackoff, nil
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// §4.3(a) — the backoff is arithmetic, not a timer
// ---------------------------------------------------------------------------

func TestBackoffDelayDoublesAndCapsAtFiveMinutes(t *testing.T) {
	want := []time.Duration{
		0,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		64 * time.Second,
		128 * time.Second,
		256 * time.Second,
		5 * time.Minute,
		5 * time.Minute,
		5 * time.Minute,
	}
	for failures, expect := range want {
		if got := backoffDelay(failures); got != expect {
			t.Errorf("backoffDelay(%d) = %s, want %s", failures, got, expect)
		}
	}
}

func TestStaleListingIsStrictAndTreatsAnAbsentListingAsStale(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 40, 11, 0, time.UTC)

	if !staleListing(AgentsResult{}, now, 30_000) {
		t.Fatal("a listing that was never taken must read as stale: there is nothing to speak with")
	}
	if !staleListing(AgentsResult{OK: false, ObservedAt: now}, now, 30_000) {
		t.Fatal("a failed listing must read as stale")
	}
	fresh := AgentsResult{OK: true, ObservedAt: now.Add(-30 * time.Second)}
	if staleListing(fresh, now, 30_000) {
		t.Fatal("exactly staleAfterMs must NOT be stale: the comparison is strictly greater")
	}
	if !staleListing(AgentsResult{OK: true, ObservedAt: now.Add(-30_001 * time.Millisecond)}, now, 30_000) {
		t.Fatal("one millisecond past staleAfterMs must be stale")
	}
}

func TestTheFingerprintChangesWithAnythingASessionWouldSee(t *testing.T) {
	pid := 4242
	base := AgentsResult{Entries: []AgentsEntry{
		{SessionID: "a", Kind: "interactive", Status: "idle", PID: &pid},
	}}
	same := AgentsResult{Entries: []AgentsEntry{
		// A field nothing downstream reads — the name — must not count as a
		// change, or a job renaming itself would reset the breaker.
		{SessionID: "a", Kind: "interactive", Status: "idle", PID: &pid, Name: "renamed"},
	}}
	if listingFingerprint(base) != listingFingerprint(same) {
		t.Fatal("the fingerprint changed on a field no session field is derived from")
	}

	for name, mutate := range map[string]func(*AgentsEntry){
		"status":     func(e *AgentsEntry) { e.Status = "busy" },
		"state":      func(e *AgentsEntry) { e.State = "working" },
		"waitingFor": func(e *AgentsEntry) { e.WaitingFor = "input needed" },
		"kind":       func(e *AgentsEntry) { e.Kind = "background" },
		"pid":        func(e *AgentsEntry) { other := 9; e.PID = &other },
		"sessionId":  func(e *AgentsEntry) { e.SessionID = "b" },
	} {
		changed := AgentsResult{Entries: []AgentsEntry{base.Entries[0]}}
		mutate(&changed.Entries[0])
		if listingFingerprint(base) == listingFingerprint(changed) {
			t.Errorf("the fingerprint ignored a change to %s", name)
		}
	}
}
