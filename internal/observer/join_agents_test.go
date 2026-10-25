package observer_test

// §3.4's fifth case — an interactive session listed by `claude agents --json`
// with no registry record — through the real core.
//
// The black-box suite found the case DROPPED: `tt sessions --json --refresh`
// answered an empty set for a listing that named a live pid. These tests pin the
// row's four promises (reported, kill -0 alone, statusSince null + advisory,
// never dropped) and the two ways the case must NOT be reached (a sessionId or
// a pid the registry already owns).

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

const (
	cliOnlyID    = "c1100000-0000-4000-8000-000000000001"
	registeredID = "c1100000-0000-4000-8000-000000000002"
)

func agentsOnlyCore(t *testing.T) (*observer.Observer, *fakes.Set) {
	t.Helper()
	deps, set := fakes.Deps(tRef)
	obs, err := observer.New(deps, observer.Config{})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return obs, set
}

func agentsEntry(id string, pid int, status, waitingFor string) observer.AgentsEntry {
	started := tRef.Add(-40 * 24 * time.Hour)
	return observer.AgentsEntry{
		SessionID: id, PID: &pid, Kind: "interactive", Status: status, WaitingFor: waitingFor,
		Name: "listed-" + id[:8], CWD: "/fixture", StartedAt: &started,
	}
}

func everything(t *testing.T, obs *observer.Observer) *observer.Snapshot {
	t.Helper()
	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = true
	snap, err := obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

func TestAnInteractiveSessionOnlyTheListingKnowsIsReportedAndNeverDropped(t *testing.T) {
	obs, set := agentsOnlyCore(t)
	set.Proc.SetAlive(4242, true)
	set.Agents.SetEntries(tRef, agentsEntry(cliOnlyID, 4242, "waiting", "permission prompt"))

	s := sessionByID(t, everything(t, obs), cliOnlyID)

	if s.Kind != observer.KindInteractive || s.PID == nil || *s.PID != 4242 {
		t.Fatalf("kind %q pid %v, want an interactive session on pid 4242", s.Kind, s.PID)
	}
	// Liveness reduces to kill -0 alone: no pane reference, so no pane gate.
	if s.Live == nil || !*s.Live || s.LiveReason != observer.LiveReasonProcOnly {
		t.Fatalf("live %v (%s), want true by kill -0 alone (proc-only)", s.Live, s.LiveReason)
	}
	if s.Tmux != nil || s.PaneAlive != nil {
		t.Fatalf("tmux %v paneAlive %v, want both null — the listing carries no pane", s.Tmux, s.PaneAlive)
	}
	if s.Status != observer.StatusWaiting || s.WaitReason != observer.WaitReasonPermissionPrompt {
		t.Fatalf("status %q waitReason %q, want waiting / permission-prompt", s.Status, s.WaitReason)
	}
	if s.StatusSource != observer.SourceAgentsCLI {
		t.Fatalf("statusSource %q, want agents-cli: it is the only source", s.StatusSource)
	}
	// statusSince is NULL with a named advisory — never startedAt, which is 40
	// days old here and would read "blocked for 40 days".
	if s.StatusSince != nil || s.StatusAgeMs != nil || s.BlockedForMs != nil {
		t.Fatalf("statusSince %v statusAgeMs %v blockedForMs %v, want all null",
			s.StatusSince, s.StatusAgeMs, s.BlockedForMs)
	}
	if !hasAdvisory(s, observer.AdvStatusSinceUnavailable) {
		t.Fatalf("no status-since-unavailable advisory: %+v", s.Advisories)
	}
	if s.BlockedForUnavailable == nil || *s.BlockedForUnavailable != observer.AdvStatusSinceUnavailable {
		t.Fatalf("blockedForUnavailable = %v, want the reason", s.BlockedForUnavailable)
	}
	if len(s.ListedBy) != 1 || s.ListedBy[0] != observer.SourceAgentsCLI ||
		len(s.Sources) != 1 || s.Sources[0] != observer.SourceAgentsCLI {
		t.Fatalf("listedBy %v sources %v, want [agents-cli] for both", s.ListedBy, s.Sources)
	}

	// And it is in the DEFAULT set, which is what "never dropped" is about.
	def, err := obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatal(err)
	}
	sessionByID(t, def, cliOnlyID)
	if def.Counts.Attention != 1 {
		t.Fatalf("counts.attention = %d, want the listed waiting session counted", def.Counts.Attention)
	}
}

func TestAListedPidThatHasExitedReadsDeadAndLeavesTheDefaultView(t *testing.T) {
	obs, set := agentsOnlyCore(t)
	set.Proc.SetAlive(4343, false)
	set.Agents.SetEntries(tRef, agentsEntry(cliOnlyID, 4343, "idle", ""))

	s := sessionByID(t, everything(t, obs), cliOnlyID)
	if s.Live == nil || *s.Live || s.LiveReason != observer.LiveReasonPIDDead {
		t.Fatalf("live %v (%s), want false / pid-dead", s.Live, s.LiveReason)
	}
	def, err := obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := def.SessionByID(cliOnlyID); ok {
		t.Fatal("a dead listed pid is in the default set; removal from the default view is " +
			"what positive evidence of death is for")
	}
}

func TestTheRegistryOwnsItsSessionIDsAndItsPids(t *testing.T) {
	obs, set := agentsOnlyCore(t)
	set.Proc.SetAlive(5000, true)
	set.Registry.SetRecords(tRef, observer.RegistryRecord{
		Path: "/fixture/sessions/5000.json", PID: 5000, SessionID: registeredID,
		Status: "idle", StatusUpdatedAt: &tRef,
	})
	set.Agents.SetEntries(tRef,
		// The registry's own session, listed: membership only, no second row.
		agentsEntry(registeredID, 5000, "idle", ""),
		// A DIFFERENT id on the registry's pid — a cached listing from before a
		// /clear. Admitting it would put a ghost session on a live pid.
		agentsEntry(cliOnlyID, 5000, "busy", ""),
	)

	snap := everything(t, obs)
	if n := len(snap.Sessions); n != 1 {
		ids := make([]string, 0, n)
		for _, s := range snap.Sessions {
			ids = append(ids, s.SessionID)
		}
		t.Fatalf("%d sessions %v, want exactly the registry's one", n, ids)
	}
	s := sessionByID(t, snap, registeredID)
	if s.StatusSource != observer.SourceRegistry {
		t.Fatalf("statusSource %q: the listing must never win a precedence contest", s.StatusSource)
	}
}

func TestNoListingMeansNoListedSessions(t *testing.T) {
	obs, set := agentsOnlyCore(t)
	set.Proc.SetAlive(4242, true)
	set.Agents.SetEntries(tRef, agentsEntry(cliOnlyID, 4242, "idle", ""))
	set.Agents.Result.OK = false // the listing could not be taken

	if snap := everything(t, obs); len(snap.Sessions) != 0 {
		t.Fatalf("%d sessions from a listing that failed", len(snap.Sessions))
	}
}
