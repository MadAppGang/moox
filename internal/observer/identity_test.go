package observer_test

// Code review round 2, A14: the join deliberately keeps an interactive session
// and a background job that share a sessionId as TWO records (the resume case,
// with a session-id-cross-kind advisory on each). The stream collapsed them:
// Diff indexed by sessionId alone, so a change to one was compared against the
// other, and removing one was hidden by the other still holding the id.
// Streaming identity is now (kind, sessionId), the same key the join uses.

import (
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
)

func twoPopulations() (inter, bg observer.Session) {
	live := true
	inter = observer.Session{SessionID: "X", Kind: observer.KindInteractive, Status: observer.StatusIdle,
		Live: &live, LiveReason: observer.LiveReasonProcOnly, ObservedAt: loopT0}
	bg = observer.Session{SessionID: "X", Kind: observer.KindBackground, Status: observer.StatusBlocked,
		Attention: true, Live: &live, LiveReason: observer.LiveReasonJobNonTerminalCorroborated, ObservedAt: loopT0}
	return inter, bg
}

func TestTwoPopulationsSharingAnIDAreDiffedIndependently(t *testing.T) {
	inter, bg := twoPopulations()
	prev := &observer.Snapshot{Sessions: []observer.Session{bg, inter}}
	changed := inter
	changed.Status = observer.StatusWaiting
	next := &observer.Snapshot{Sessions: []observer.Session{bg, changed}}

	events := observer.Diff(prev, next, observer.Filter{})
	if len(events) != 1 || events[0].Type != observer.EventSessionChanged ||
		events[0].Session.Kind != observer.KindInteractive ||
		len(events[0].Changed) != 1 || events[0].Changed[0] != "status" {
		t.Fatalf("one status change to the interactive X produced %+v; want exactly one "+
			"session_changed(status) for the interactive record", summarize(events))
	}
	if events[0].Kind != observer.KindInteractive {
		t.Fatalf("the event names kind %q", events[0].Kind)
	}
}

func TestRemovingOnePopulationLeavesTheOtherAndSaysWhich(t *testing.T) {
	inter, bg := twoPopulations()
	prev := &observer.Snapshot{Sessions: []observer.Session{bg, inter}}
	next := &observer.Snapshot{Sessions: []observer.Session{bg}}

	events := observer.Diff(prev, next, observer.Filter{})
	if len(events) != 1 || events[0].Type != observer.EventSessionRemoved || events[0].SessionID != "X" {
		t.Fatalf("the interactive X's record went away and Diff said %+v; want exactly one "+
			"session_removed", summarize(events))
	}
	if events[0].Kind != observer.KindInteractive || events[0].Reason != observer.RemovedRecordRemoved {
		t.Fatalf("the removal names kind %q reason %q, want interactive / record-removed",
			events[0].Kind, events[0].Reason)
	}
}

func summarize(events []observer.Event) []string {
	var out []string
	for _, e := range events {
		kind := ""
		if e.Session != nil {
			kind = string(e.Session.Kind)
		}
		out = append(out, string(e.Type)+":"+e.SessionID+":"+kind)
	}
	return out
}
