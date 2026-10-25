package observer_test

// FIX: wedged agents-cli — the regression test for the defect P7's live run
// found and declared out of scope, reproduced by the orchestrator, and fixed by
// reordering the rebuild's reads.
//
// The defect, in one sentence: `claude agents --json` wedged on the user's
// machine (three of three probes exit 124 at a 12 s timeout, having returned in
// ~130 ms the same morning), and `tt sessions --json` answered 38 sessions with
// `live: null` on EVERY one of them — 35 of them with liveReason `gate-error` —
// in 5 077 ms, while reporting `sources.agents-cli` as `ok: false` with
// `lastError: null` and an empty advisories array.
//
// Two faults, and this file asserts both are gone:
//
//	(a) a SUPPLEMENTARY source's failure destroyed the AUTHORITATIVE answer.
//	    §3.5's liveness is two gates, kill -0 and pane-exists; neither needs the
//	    CLI, and both were working. Corroboration is a cross-check that §4.4
//	    says "nothing a consumer reads as a status depends on", yet it ran FIRST
//	    and spent the whole read budget, so `tmux list-panes` was asked with an
//	    expired context.
//	(b) it failed SILENTLY. Something that turns every session unknown must say
//	    why.
//
// The fake here HANGS — it is the wedge, in-process — and the caller is given a
// deliberately generous budget, so every assertion below is about OUR bound
// rather than about the caller's.

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

// tWedge is this file's reference instant.
var tWedge = time.Date(2026, 9, 18, 7, 18, 43, 637_000_000, time.UTC)

const (
	// The two interactive sessions and their panes, in the shape measured fact
	// 7 requires: the sigils are part of the identifiers.
	wedgeIntA   = "f350c44d-d22a-4c47-b3ef-00021713db7f"
	wedgeIntB   = "e9c21455-4c33-4e63-9d3a-3f3f1b0c5a21"
	wedgePaneA  = "madbench:@53.%109"
	wedgePaneB  = "madbench:@51.%107"
	wedgePIDA   = 28984
	wedgePIDB   = 59717
	wedgeReadMs = 1000 // the tight bound: see the elapsed assertion
)

// The three background jobs, one per verdict class the live reproduction saw:
// a non-terminal record (uncorroborated without a listing), a terminal one
// (independent of the listing), and one whose record is old enough to be beyond
// transcript retention.
const (
	wedgeJobBlocked = "97030e53-528c-4452-bbae-535400039def"
	wedgeJobDone    = "7a324a6d-1b58-4d1a-9a3e-2f9a4c0b6e51"
	wedgeJobWorking = "d1f05570-4e47-4517-b4c5-9bda85d3bd6d"
)

// wedgeHarness is a whole machine in fixtures: two live interactive sessions
// whose pids and panes both check out, three background jobs, and a
// corroboration port that can be made to hang.
type wedgeHarness struct {
	t   *testing.T
	obs *observer.Observer
	set *fakes.Set
}

func newWedge(t *testing.T, wire bool) *wedgeHarness {
	t.Helper()
	deps, set := fakes.Deps(tWedge)
	if !wire {
		// The control: a build with NO corroboration port. A wedged CLI must
		// degrade to exactly this and no further, which is what makes the
		// background verdicts comparable between the two runs.
		deps.Agents = nil
	}

	updated := tWedge
	set.Registry.SetRecords(tWedge,
		observer.RegistryRecord{
			Path: "/fixtures/sessions/28984.json", PID: wedgePIDA,
			SessionID: wedgeIntA, Status: "idle", StatusUpdatedAt: &updated,
			CWD: "/home/user/dotfiles", Tmux: wedgePaneA,
		},
		observer.RegistryRecord{
			Path: "/fixtures/sessions/59717.json", PID: wedgePIDB,
			SessionID: wedgeIntB, Status: "waiting", WaitingFor: "input_needed",
			StatusUpdatedAt: &updated, CWD: "/home/user/dotfiles", Tmux: wedgePaneB,
		},
	)
	set.Proc.SetAlive(wedgePIDA, true)
	set.Proc.SetAlive(wedgePIDB, true)
	set.Panes.Set(wedgePaneA, wedgePaneB)

	set.Jobs.SetRecords(tWedge,
		jobRec(wedgeJobBlocked, "97030e53", "blocked", updated),
		jobRec(wedgeJobDone, "7a324a6d", "done", updated),
		jobRec(wedgeJobWorking, "d1f05570", "working", updated),
	)

	obs, err := observer.New(deps, observer.Config{
		Paths: observer.Paths{
			ClaudeRoot: "/fixtures",
			Registry:   "/fixtures/sessions",
			Jobs:       "/fixtures/jobs",
			Projects:   "/fixtures/projects",
		},
		Policy: observer.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return &wedgeHarness{t: t, obs: obs, set: set}
}

func jobRec(id, short, state string, at time.Time) observer.JobRecord {
	stamp := at
	return observer.JobRecord{
		Dir:         short,
		Path:        "/fixtures/jobs/" + short + "/state.json",
		DaemonShort: short,
		SessionID:   id,
		State:       state,
		CWD:         "/home/user/dotfiles",
		Name:        "job " + short,
		CreatedAt:   &stamp,
		UpdatedAt:   &stamp,
	}
}

// wedge makes every spawn hang until the returned function is called. The
// channel is set before any spawn exists and is only ever CLOSED, which is why
// this is safe under -race: the fake reads the field under its own lock.
func (h *wedgeHarness) wedge() func() {
	ch := make(chan struct{})
	h.set.Agents.Block = ch
	closed := false
	return func() {
		if !closed {
			closed = true
			close(ch)
		}
	}
}

// verdicts is the (live, liveReason) pair of every session, keyed by id — the
// only thing the background comparison is allowed to look at, because the
// advisory FIELDS legitimately differ between "no port wired" and "a port that
// hung".
func verdicts(snap *observer.Snapshot) map[string]string {
	out := map[string]string{}
	for _, s := range snap.Sessions {
		live := "null"
		if s.Live != nil {
			live = "false"
			if *s.Live {
				live = "true"
			}
		}
		out[s.SessionID] = live + "/" + string(s.LiveReason)
	}
	return out
}

func wedgeSession(t *testing.T, snap *observer.Snapshot, id string) observer.Session {
	t.Helper()
	for _, s := range snap.Sessions {
		if s.SessionID == id {
			return s
		}
	}
	t.Fatalf("session %s is not in the snapshot", id)
	return observer.Session{}
}

func carries(advs []observer.Advisory, code string) bool {
	for _, a := range advs {
		if a.Code == code {
			return true
		}
	}
	return false
}

// TestAWedgedCorroborationCostsOnlyTheCorroboration is the whole fix, asserted
// at once because the faults are one behaviour: the authoritative gates
// complete, the answer arrives inside our own bound rather than the caller's,
// only the corroboration block is marked failed, and it says why.
func TestAWedgedCorroborationCostsOnlyTheCorroboration(t *testing.T) {
	h := newWedge(t, true)
	release := h.wedge()
	defer release()

	// A deliberately generous caller budget — 20x CorroborationWait and 4x the
	// 5 s the live reproduction took. If the answer still arrives quickly, the
	// bound that produced it is ours.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := time.Now()
	snap, err := h.obs.Snapshot(ctx, observer.DefaultSnapshotOptions())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// ---- the bound. This is the "5 077 ms" number in the defect report. ----
	if elapsed > wedgeReadMs*time.Millisecond {
		t.Errorf("Snapshot took %v with a wedged CLI, want < %dms: the spawn bound "+
			"is still the read bound", elapsed, wedgeReadMs)
	}

	// ---- (a) the authoritative gates completed and decided. ----
	if h.set.Panes.Calls() != 1 {
		t.Errorf("ListPanes called %d times, want exactly 1: the gate must be asked, "+
			"once, per rebuild", h.set.Panes.Calls())
	}
	if rep := snap.Sources[observer.SourceTmux]; !rep.OK || rep.LastError != nil {
		t.Errorf("sources.tmux = ok:%v lastError:%v, want a working gate: the pane "+
			"gate does not need the CLI", rep.OK, rep.LastError)
	}
	for _, id := range []string{wedgeIntA, wedgeIntB} {
		s := wedgeSession(t, snap, id)
		if s.Live == nil || !*s.Live {
			t.Errorf("%s live = %v, want true: kill -0 and pane-exists both passed",
				id[:8], s.Live)
		}
		if s.LiveReason != observer.LiveReasonProcPane {
			t.Errorf("%s liveReason = %q, want %q", id[:8], s.LiveReason,
				observer.LiveReasonProcPane)
		}
		// listedBy stays honest: a session the CLI could not confirm is still
		// registry-listed, and it is NOT accused of being absent from a listing
		// that was never taken.
		if got := s.ListedBy; len(got) != 1 || got[0] != observer.SourceRegistry {
			t.Errorf("%s listedBy = %v, want [%s] only", id[:8], got, observer.SourceRegistry)
		}
		if carries(s.Advisories, observer.AdvAgentsAbsent) {
			t.Errorf("%s carries %s from a listing that never arrived", id[:8],
				observer.AdvAgentsAbsent)
		}
	}
	if snap.Counts.Live != 2 {
		t.Errorf("counts.live = %d, want 2 (the interactive pair)", snap.Counts.Live)
	}

	// ---- (b) the failure is not silent. ----
	rep, ok := snap.Sources[observer.SourceAgentsCLI]
	if !ok {
		t.Fatal("sources.agents-cli is absent although the port is wired")
	}
	if rep.OK {
		t.Error("sources.agents-cli.ok = true although the spawn never returned")
	}
	if rep.LastError == nil || *rep.LastError == "" {
		t.Error("sources.agents-cli.lastError is null — this is fault (b), verbatim")
	}
	if !carries(rep.Advisories, observer.AdvCorroborationWaitExpired) {
		t.Errorf("sources.agents-cli.advisories = %v, want %s", rep.Advisories,
			observer.AdvCorroborationWaitExpired)
	}
	if !carries(snap.Warnings, observer.AdvCorroborationWaitExpired) {
		t.Errorf("warnings = %v, want %s promoted into it", snap.Warnings,
			observer.AdvCorroborationWaitExpired)
	}
	if rep.Counters["spawns"] != 1 {
		t.Errorf("spawns = %d, want 1: the wedge must be visible as an attempt",
			rep.Counters["spawns"])
	}

	// ---- the background verdicts are unchanged. ----
	//
	// "Unchanged" is asserted against the build with NO corroboration port,
	// which is the most a missing listing may cost: a wedged CLI must degrade to
	// "we have no listing" and not one step further.
	control := newWedge(t, false)
	base, err := control.obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("control Snapshot: %v", err)
	}
	got, want := verdicts(snap), verdicts(base)
	for _, id := range []string{wedgeJobBlocked, wedgeJobDone, wedgeJobWorking} {
		if got[id] != want[id] {
			t.Errorf("job %s verdict = %q with a wedged CLI, want %q (the no-port "+
				"build's answer)", id[:8], got[id], want[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d sessions with a wedged CLI, %d without the port at all",
			len(got), len(want))
	}

	// The session that actually depends on the listing says so, on itself —
	// §3.5 rule 6b's advisory, which is the per-session half of (b).
	blocked := wedgeSession(t, snap, wedgeJobBlocked)
	if !carries(blocked.Advisories, observer.AdvJobCorroborationStale) {
		t.Errorf("the non-terminal job's advisories = %v, want %s: a session whose "+
			"verdict depended on the listing must carry the reason",
			blocked.Advisories, observer.AdvJobCorroborationStale)
	}
}

// TestTheWedgeStateClearsWhenTheListingFinallyLands is the other half of the
// bound: on a long-lived surface the spawn is NOT cancelled, so the rebuild
// after it lands must be fully corroborated and must stop reporting a failure.
// Without this, one slow spawn would leave lastError and the advisory latched
// for the life of the process.
func TestTheWedgeStateClearsWhenTheListingFinallyLands(t *testing.T) {
	h := newWedge(t, true)
	release := h.wedge()

	h.set.Agents.SetEntries(tWedge, observer.AgentsEntry{
		SessionID: wedgeJobBlocked, ID: "97030e53", Kind: "background", State: "blocked",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := h.obs.Snapshot(ctx, observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	if !carries(first.Sources[observer.SourceAgentsCLI].Advisories,
		observer.AdvCorroborationWaitExpired) {
		t.Fatal("the first rebuild did not report the expired wait")
	}

	// Let the wedge go and wait for the result to be adopted, exactly as the
	// next event-driven rebuild would find it.
	release()
	drain, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	h.obs.AwaitCorroboration(drain)

	second, err := h.obs.Snapshot(ctx, observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	rep := second.Sources[observer.SourceAgentsCLI]
	if !rep.OK {
		t.Error("sources.agents-cli.ok = false after the listing landed")
	}
	if rep.LastError != nil {
		t.Errorf("lastError = %q after a successful listing, want null", *rep.LastError)
	}
	if carries(rep.Advisories, observer.AdvCorroborationWaitExpired) {
		t.Error("the expired-wait advisory latched past the listing that landed")
	}
	if carries(second.Warnings, observer.AdvCorroborationWaitExpired) {
		t.Error("the expired-wait advisory latched in warnings")
	}
	// And the verdict the listing exists to produce is now available.
	blocked := wedgeSession(t, second, wedgeJobBlocked)
	if blocked.Live == nil || !*blocked.Live {
		t.Errorf("the corroborated job's live = %v, want true", blocked.Live)
	}
	if blocked.LiveReason != observer.LiveReasonJobNonTerminalCorroborated {
		t.Errorf("liveReason = %q, want %q", blocked.LiveReason,
			observer.LiveReasonJobNonTerminalCorroborated)
	}
}

// TestAHealthyCorroborationStillOverlapsTheAuthoritativeReads pins the reason
// the spawn is ISSUED before the gates and COLLECTED after them, rather than
// simply moved behind them: a listing that returns promptly must still be in
// the answer. If a later edit moved the request down next to the collection,
// this test would still pass — so it asserts the observable consequence, which
// is that no rebuild silently loses corroboration it could have had.
func TestAHealthyCorroborationStillOverlapsTheAuthoritativeReads(t *testing.T) {
	h := newWedge(t, true)
	h.set.Agents.SetEntries(tWedge, observer.AgentsEntry{
		SessionID: wedgeJobBlocked, ID: "97030e53", Kind: "background", State: "blocked",
	})

	snap, err := h.obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	rep := snap.Sources[observer.SourceAgentsCLI]
	if !rep.OK || rep.LastError != nil {
		t.Fatalf("sources.agents-cli = ok:%v lastError:%v, want a clean listing",
			rep.OK, rep.LastError)
	}
	if carries(rep.Advisories, observer.AdvCorroborationWaitExpired) {
		t.Error("a healthy 0 ms listing reported an expired wait")
	}
	blocked := wedgeSession(t, snap, wedgeJobBlocked)
	if blocked.LiveReason != observer.LiveReasonJobNonTerminalCorroborated {
		t.Errorf("liveReason = %q, want %q: the startup listing was lost",
			blocked.LiveReason, observer.LiveReasonJobNonTerminalCorroborated)
	}
	// And the interactive gates are untouched by a healthy listing too.
	if s := wedgeSession(t, snap, wedgeIntA); s.LiveReason != observer.LiveReasonProcPane {
		t.Errorf("interactive liveReason = %q, want %q", s.LiveReason,
			observer.LiveReasonProcPane)
	}
}
