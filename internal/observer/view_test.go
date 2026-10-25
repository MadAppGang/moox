package observer_test

// view.go is the baseline half of client-relative membership. These tests pin
// the three properties the socket server depends on, and the one AC11 does.

import (
	"reflect"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
)

// viewSnapshot is two sessions, one interactive and idle, one background and
// blocked, in one published snapshot with counts over both.
func viewSnapshot() *observer.Snapshot {
	interactive := baseSession()
	interactive.SessionID = "aaaa"
	interactive.Kind = observer.KindInteractive
	interactive.Status = observer.StatusIdle
	interactive.Attention = false

	background := baseSession()
	background.SessionID = "bbbb"
	background.Kind = observer.KindBackground
	background.Status = observer.StatusBlocked
	background.Attention = true

	sessions := []observer.Session{interactive, background}
	counts := observer.NewCounts()
	for _, s := range sessions {
		counts.Total++
		counts.Live++
		if s.Attention {
			counts.Attention++
		}
		counts.ByStatus[s.Status]++
		counts.ByKind[s.Kind]++
	}
	return &observer.Snapshot{
		Seq:        7,
		ObservedAt: diffBase,
		Counts:     counts,
		Sources:    map[string]observer.SourceReport{"registry": {ID: "registry", OK: true}},
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}
}

// The zero Filter is an IDENTITY on an already-published snapshot, field for
// field. AC11 rests on this: the socket serves View(published, zeroFilter) and
// the CLI serves the published snapshot itself, so anything View changed here
// would be a difference between two surfaces that must not differ.
func TestAnEmptyFilterViewsASnapshotUnchanged(t *testing.T) {
	snap := viewSnapshot()
	got := observer.View(snap, observer.Filter{})

	if !reflect.DeepEqual(got.Sessions, snap.Sessions) {
		t.Fatalf("the sessions changed under an empty filter")
	}
	if !reflect.DeepEqual(got.Counts, snap.Counts) {
		t.Fatalf("counts = %+v, want %+v", got.Counts, snap.Counts)
	}
	if got.ObservedAt != snap.ObservedAt || got.Seq != snap.Seq {
		t.Fatalf("the view moved the instant or the sequence: %v/%d", got.ObservedAt, got.Seq)
	}
	if !reflect.DeepEqual(got.Sources, snap.Sources) ||
		!reflect.DeepEqual(got.NotCapable, snap.NotCapable) ||
		!reflect.DeepEqual(got.Warnings, snap.Warnings) {
		t.Fatal("the view altered the envelope")
	}
}

// A filter narrows the sessions AND the counts. An envelope whose counts
// described 40 sessions above an array of three would be a lie in the field a
// consumer reads first.
func TestAFilterNarrowsTheSessionsAndRecomputesTheCounts(t *testing.T) {
	snap := viewSnapshot()
	got := observer.View(snap, observer.Filter{Kinds: []observer.Kind{observer.KindBackground}})

	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "bbbb" {
		t.Fatalf("the view carries %d sessions, want only bbbb", len(got.Sessions))
	}
	if got.Counts.Total != 1 || got.Counts.Attention != 1 {
		t.Fatalf("counts = %+v, want total 1 attention 1", got.Counts)
	}
	if got.Counts.ByKind[observer.KindInteractive] != 0 {
		t.Fatalf("counts still count the interactive session: %+v", got.Counts.ByKind)
	}
	// Every known key survives, zeros included — the map rule §5.2 states.
	if len(got.Counts.ByStatus) != len(observer.AllStatuses) {
		t.Fatalf("byStatus has %d keys, want %d", len(got.Counts.ByStatus), len(observer.AllStatuses))
	}
}

// The input is never mutated. A published snapshot is read without a lock
// through an atomic pointer, so a reader may be holding the one being viewed —
// and 32 connections each viewing the same snapshot must not be able to see
// each other's filter.
func TestViewNeverMutatesThePublishedSnapshot(t *testing.T) {
	snap := viewSnapshot()
	before := append([]observer.Session(nil), snap.Sessions...)
	beforeCounts := snap.Counts

	narrow := observer.View(snap, observer.Filter{Kinds: []observer.Kind{observer.KindInteractive}})
	narrow.Sessions[0].Name = "mutated by a consumer"

	if !reflect.DeepEqual(snap.Sessions, before) {
		t.Fatal("the published snapshot's sessions were mutated through a view")
	}
	if !reflect.DeepEqual(snap.Counts, beforeCounts) {
		t.Fatal("the published snapshot's counts were mutated through a view")
	}
}

// The view and the diff use the SAME predicate, which is the property that
// stops a filtered client's baseline disagreeing with its deltas.
func TestTheViewAndTheDiffAgreeOnMembership(t *testing.T) {
	snap := viewSnapshot()
	filter := observer.Filter{Statuses: []observer.Status{observer.StatusBlocked}}

	view := observer.View(snap, filter)
	ids := observer.VisibleIDs(snap, filter)
	if len(view.Sessions) != len(ids) {
		t.Fatalf("the view has %d sessions and VisibleIDs has %d", len(view.Sessions), len(ids))
	}
	for i := range ids {
		if view.Sessions[i].SessionID != ids[i] {
			t.Fatalf("VisibleIDs[%d] = %q, the view has %q", i, ids[i], view.Sessions[i].SessionID)
		}
	}

	// Diff from nothing to this snapshot must add exactly the visible set.
	events := observer.Diff(nil, snap, filter)
	if len(events) != len(ids) {
		t.Fatalf("the first diff produced %d events for a view of %d sessions", len(events), len(ids))
	}
	for i, ev := range events {
		if ev.Type != observer.EventSessionAdded || ev.SessionID != ids[i] {
			t.Fatalf("event %d = (%s, %s), want an addition for %s", i, ev.Type, ev.SessionID, ids[i])
		}
	}
}

// A nil snapshot views as nil rather than as an empty answer: "nothing has
// been published yet" is a state the caller must handle, and synthesising an
// empty snapshot would let a client be told the machine is empty when it is
// merely unread.
func TestViewingNothingIsNothing(t *testing.T) {
	if got := observer.View(nil, observer.Filter{}); got != nil {
		t.Fatalf("View(nil) = %+v, want nil", got)
	}
	if got := observer.VisibleIDs(nil, observer.Filter{}); got != nil {
		t.Fatalf("VisibleIDs(nil) = %v, want nil", got)
	}
}
