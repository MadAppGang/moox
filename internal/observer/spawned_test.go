package observer_test

// A session another session launched is hidden from the default view.
//
// THE CASE, MEASURED (2026-10-21): claude 27603 (session f6bfe249, in tmux pane
// %24) ran the claudish MCP helper, which ran `magmux --headless`, which ran a
// second claude — 45393, session 6bdf1e00, on its own pty and in no tmux pane:
//
//	claude 27603 └ node claudish --mcp └ bun claudish --mcp
//	  └ magmux --headless └ node claudish └ bun claudish └ claude 45393
//
// The child writes an ordinary registry record (kind interactive, entrypoint
// cli) and `claude agents --json` lists it, so nothing in any record tells it
// apart. The panel drew it as a RUN row in its "no tmux" group. The only thing
// that DOES tell it apart is the process tree, so the rule is the process tree:
// a session whose process has another live session's pid among its ancestors
// is that session's child, and is hidden unless the caller asks for everything.
//
// Every test here drives the real core through fakes. No real pid is ever
// walked: the parent chain is whatever the fake says it is.

import (
	"context"
	"errors"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

const (
	spawnParentID = "f6bfe249-0000-4000-8000-000000000001"
	spawnChildID  = "6bdf1e00-0000-4000-8000-000000000002"

	spawnParentPID = 27603
	spawnChildPID  = 45393
)

// spawnCore is a core over two registry records — the launching session and
// the one it launched — both with live pids and neither in a tmux pane, so that
// liveness is kill -0 alone and nothing but the ancestry can separate them.
func spawnCore(t *testing.T) (*observer.Observer, *fakes.Set) {
	t.Helper()
	deps, set := fakes.Deps(tRef)
	set.Registry.SetRecords(tRef,
		observer.RegistryRecord{
			Path: "/sessions/27603.json", PID: spawnParentPID, SessionID: spawnParentID,
			Status: "busy", Name: "parent", CWD: "/fixture",
		},
		observer.RegistryRecord{
			Path: "/sessions/45393.json", PID: spawnChildPID, SessionID: spawnChildID,
			Status: "busy", Name: "investgiage-gpt-file-f0", CWD: "/fixture",
		},
	)
	set.Proc.SetAlive(spawnParentPID, true)
	set.Proc.SetAlive(spawnChildPID, true)
	obs, err := observer.New(deps, observer.Config{})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return obs, set
}

// measuredChain installs the process tree the live machine had: the child's
// pid reaches the parent's through five helper processes, and the parent's
// own chain ends at launchd without meeting another session.
func measuredChain(p *fakes.Proc) {
	p.SetParent(spawnChildPID, 45280) // bun claudish
	p.SetParent(45280, 45236)         // node claudish
	p.SetParent(45236, 45233)         // magmux --headless
	p.SetParent(45233, 38257)         // bun claudish --mcp
	p.SetParent(38257, 37954)         // node claudish --mcp
	p.SetParent(37954, spawnParentPID)
	p.SetParent(spawnParentPID, 27000) // the pane's shell
	p.SetParent(27000, 1)
}

func snapWith(t *testing.T, obs *observer.Observer, f observer.Filter) *observer.Snapshot {
	t.Helper()
	opts := observer.DefaultSnapshotOptions()
	opts.Filter = f
	snap, err := obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

func hasSession(snap *observer.Snapshot, id string) bool {
	_, ok := snap.SessionByID(id)
	return ok
}

func TestASessionLaunchedByAnotherSessionIsHiddenByDefaultAndListedWhenAskedFor(t *testing.T) {
	obs, set := spawnCore(t)
	measuredChain(set.Proc)

	def := snapWith(t, obs, observer.Filter{})
	if hasSession(def, spawnChildID) {
		t.Fatalf("the launched session is in the default view: %v", ids(def))
	}
	if !hasSession(def, spawnParentID) {
		t.Fatalf("the LAUNCHING session vanished from the default view: %v", ids(def))
	}
	if def.Counts.Total != 1 {
		t.Errorf("counts.total = %d, want 1 — the counts must describe the array they sit above", def.Counts.Total)
	}

	all := snapWith(t, obs, observer.Filter{IncludeSpawned: true})
	child := sessionByID(t, all, spawnChildID)
	if child.SpawnedBy == nil || *child.SpawnedBy != spawnParentID {
		t.Fatalf("spawnedBy = %v, want %s", child.SpawnedBy, spawnParentID)
	}
	if parent := sessionByID(t, all, spawnParentID); parent.SpawnedBy != nil {
		t.Errorf("the launching session claims a parent: spawnedBy = %s", *parent.SpawnedBy)
	}

	// The subscriber half goes through the same predicate (Visible), so a
	// socket client and a direct one cannot disagree about the child.
	if got := observer.View(all, observer.Filter{}); hasSession(got, spawnChildID) {
		t.Errorf("View with the zero filter kept the launched session")
	}
	if got := observer.View(all, observer.Filter{IncludeSpawned: true}); !hasSession(got, spawnChildID) {
		t.Errorf("View with includeSpawned dropped the launched session")
	}
}

func TestAnAncestryLookupErrorNeverHidesASession(t *testing.T) {
	obs, set := spawnCore(t)
	measuredChain(set.Proc)
	// The walk breaks halfway up: magmux's parent cannot be read. That is no
	// evidence of anything, and hiding is only ever done on positive evidence.
	set.Proc.SetParentError(45233, errors.New("sysctl: no such process"))

	s := sessionByID(t, snapWith(t, obs, observer.Filter{}), spawnChildID)
	if s.SpawnedBy != nil {
		t.Errorf("spawnedBy = %s from a walk that errored", *s.SpawnedBy)
	}
}

func TestAnAncestorThatIsNotASessionHidesNothing(t *testing.T) {
	obs, set := spawnCore(t)
	// Two sibling sessions under one shell: the shared ancestor is a process,
	// not a session, and siblings do not hide each other.
	set.Proc.SetParent(spawnChildPID, 40000)
	set.Proc.SetParent(spawnParentPID, 40000)
	set.Proc.SetParent(40000, 1)

	def := snapWith(t, obs, observer.Filter{})
	for _, id := range []string{spawnParentID, spawnChildID} {
		s := sessionByID(t, def, id)
		if s.SpawnedBy != nil {
			t.Errorf("%s: spawnedBy = %s with no session among its ancestors", id, *s.SpawnedBy)
		}
	}
}

func TestADeadSessionsPidHidesNothing(t *testing.T) {
	obs, set := spawnCore(t)
	measuredChain(set.Proc)
	// The launcher's record is still on disk but its pid is gone. A dead
	// record's pid is a number the kernel may have handed to anything since,
	// so it is not evidence of parentage.
	set.Proc.SetAlive(spawnParentPID, false)

	s := sessionByID(t, snapWith(t, obs, observer.Filter{}), spawnChildID)
	if s.SpawnedBy != nil {
		t.Errorf("spawnedBy = %s, but the launching session is dead", *s.SpawnedBy)
	}
}

func TestASessionIsNeverHiddenByItself(t *testing.T) {
	obs, set := spawnCore(t)
	// A tree that loops back to the session's own pid — impossible in a real
	// kernel, possible in a torn read — must neither hide the session nor walk
	// forever.
	set.Proc.SetParent(spawnChildPID, 45280)
	set.Proc.SetParent(45280, spawnChildPID)

	s := sessionByID(t, snapWith(t, obs, observer.Filter{}), spawnChildID)
	if s.SpawnedBy != nil {
		t.Errorf("spawnedBy = %s: a session was hidden by its own pid", *s.SpawnedBy)
	}
}
