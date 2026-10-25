package main

// Code review round 2, A7, the panel's half: the panel advanced its displayed
// AGES from its own clock but grouped rows on the published `live`, which the
// server — with no timer, by the user's decision — never republishes when only
// time passes. A working job whose thread went quiet stayed under ACTIVE for as
// long as the machine stayed quiet. The grouping now reads liveness derived at
// the panel's render clock (observer.AsOf), the same function the MCP answer
// path uses.

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

func TestThePanelGroupsOnLivenessDerivedAtItsOwnClock(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	activity := t0.Add(-time.Minute)
	listing := t0
	staleAfter := int64(30_000)
	live := true
	tempo := "active"
	snap := &observer.Snapshot{
		ObservedAt: t0,
		Counts:     observer.NewCounts(),
		Sources: map[string]observer.SourceReport{observer.SourceAgentsCLI: {
			ID: observer.SourceAgentsCLI, OK: true, ObservedAt: &listing, StaleAfterMs: &staleAfter,
			Advisories: []observer.Advisory{}, Labels: map[string]string{},
		}},
		Sessions: []observer.Session{{
			SessionID: "worker", Name: "a working job", Kind: observer.KindBackground,
			Status: observer.StatusWorking, RawStatus: "working",
			StatusSince: &activity, RecordUpdatedAt: &activity, LastActivityAt: &activity, Tempo: &tempo,
			Live: &live, LiveReason: observer.LiveReasonJobNonTerminalCorroborated,
			Sources:    []string{observer.SourceJobs, observer.SourceAgentsCLI, observer.SourceTranscript},
			ListedBy:   []string{observer.SourceJobs, observer.SourceAgentsCLI},
			ObservedAt: t0, Advisories: []observer.Advisory{},
		}},
	}

	if rows := panelRows(snap, t0.Add(10*time.Second), observer.DefaultPolicy()); len(rows[groupActive]) != 1 {
		t.Fatalf("at first the worker is ACTIVE: %v", rows)
	}
	// Twenty quiet minutes later, with no publication in between.
	rows := panelRows(snap, t0.Add(20*time.Minute), observer.DefaultPolicy())
	if len(rows[groupActive]) != 0 || len(rows[groupUnclear]) != 1 {
		t.Fatalf("a job whose thread has been quiet for 21 minutes is still grouped as %v; "+
			"its liveness at the panel's clock is null (job-working-stale)", rows)
	}
}

// Code review round 2, A10, the panel's half: the panel subscribed with the
// default filter, so a session that ENDED while the panel was open arrived as
// a removal and vanished — while one that was already dead at startup sat in
// the ENDED group (the baseline is the whole published set). The panel now
// asks for the dead population, and a death is a change it keeps.
func TestAPanelKeepsASessionThatEndsWhileItIsOpen(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	live, dead := true, false
	core := &panelCoreStub{sessions: []observer.Session{{SessionID: "job", Name: "a job",
		Kind: observer.KindBackground, Status: observer.StatusWorking, Live: &live,
		LiveReason: observer.LiveReasonJobNonTerminalCorroborated, ObservedAt: t0}}}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core: core, Watch: fakes.NewWatcher(4), Tree: panelEmptyTree{}, Clock: fakes.NewClock(t0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatal(err)
	}
	out := make(chan tea.Msg, 16)
	go panelPump(ctx, loop, 1, out)

	var p panel
	take := func() {
		t.Helper()
		select {
		case m := <-out:
			if ev, ok := m.(panelEventMsg); ok {
				p.apply(ev.ev)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the pump delivered nothing")
		}
	}
	take() // the baseline

	core.set([]observer.Session{{SessionID: "job", Name: "a job", Kind: observer.KindBackground,
		Status: observer.StatusDone, Live: &dead, LiveReason: observer.LiveReasonJobTerminal, ObservedAt: t0}})
	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatal(err)
	}
	take()

	if p.snap == nil || len(p.snap.Sessions) != 1 || p.snap.Sessions[0].Status != observer.StatusDone {
		t.Fatalf("the job that finished while the panel was open is gone: %+v", p.snap)
	}
	if g := panelGroupOf(p.snap.Sessions[0]); g != groupEnded {
		t.Fatalf("the finished job is grouped %v, want ENDED", g)
	}
}

// A14, the panel's half: deltas are applied to the record of the right KIND.
func TestAPanelAppliesAChangeToTheRecordOfTheRightKind(t *testing.T) {
	live := true
	inter := observer.Session{SessionID: "X", Kind: observer.KindInteractive, Status: observer.StatusIdle, Live: &live}
	bg := observer.Session{SessionID: "X", Kind: observer.KindBackground, Status: observer.StatusBlocked, Live: &live}
	p := panel{snap: &observer.Snapshot{Sessions: []observer.Session{bg, inter}}}

	changed := inter
	changed.Status = observer.StatusWaiting
	p.apply(observer.Event{Type: observer.EventSessionChanged, SessionID: "X", Session: &changed})
	got := map[observer.Kind]observer.Status{}
	for _, s := range p.snap.Sessions {
		got[s.Kind] = s.Status
	}
	if len(p.snap.Sessions) != 2 || got[observer.KindBackground] != observer.StatusBlocked ||
		got[observer.KindInteractive] != observer.StatusWaiting {
		t.Fatalf("after the interactive X changed the panel holds %v (%d records); want the "+
			"background X still blocked and the interactive X waiting", got, len(p.snap.Sessions))
	}
}

type panelCoreStub struct {
	mu       sync.Mutex
	sessions []observer.Session
}

func (c *panelCoreStub) set(s []observer.Session) {
	c.mu.Lock()
	c.sessions = s
	c.mu.Unlock()
}

func (c *panelCoreStub) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	return &observer.Snapshot{ObservedAt: at, Counts: observer.NewCounts(),
		Sessions: append([]observer.Session(nil), c.sessions...)}, nil
}

type panelEmptyTree struct{}

func (panelEmptyTree) ListDirs(context.Context, string) ([]string, error)          { return nil, nil }
func (panelEmptyTree) ListFiles(context.Context, string, string) ([]string, error) { return nil, nil }
func (panelEmptyTree) Exists(context.Context, string) (bool, error)                { return false, nil }
