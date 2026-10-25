package observer_test

// diff.go is the reason the watch port can say only "something changed": the
// meaning is in the difference between two snapshots. These tests pin the two
// properties that difference has to have.
//
//  1. The comparison covers EXACTLY the diffed set. A field that is diffed but
//     not compared is invisible to a subscriber; a clock-derived field that IS
//     compared turns the delta stream into a poll loop with extra steps, because
//     statusAgeMs moves on every rebuild whether or not anything happened.
//  2. Membership is client-relative, and a removal carries positive evidence.

import (
	"reflect"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

func strPtr(s string) *string        { return &s }
func intPtr(i int) *int              { return &i }
func i64Ptr(i int64) *int64          { return &i }
func boolPtr(b bool) *bool           { return &b }
func timePtr(t time.Time) *time.Time { return &t }

var diffBase = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

// baseSession is populated everywhere, so a mutation is always a CHANGE rather
// than a first appearance — a comparator that only ever saw a zero value on one
// side could be wrong in a way no test noticed.
func baseSession() observer.Session {
	return observer.Session{
		SessionID:             "11111111-2222-3333-4444-555555555555",
		ShortID:               strPtr("1111abcd"),
		PID:                   intPtr(4242),
		Kind:                  observer.KindInteractive,
		RawKind:               strPtr("interactive"),
		Name:                  "base",
		NameSource:            "user",
		FormerNames:           []string{"older"},
		CWD:                   "/home/user/dotfiles",
		StartedAt:             timePtr(diffBase.Add(-4 * time.Hour)),
		Status:                observer.StatusIdle,
		RawStatus:             "idle",
		StatusSource:          "registry",
		StatusSince:           timePtr(diffBase.Add(-3 * time.Hour)),
		StatusAgeMs:           i64Ptr(3 * 60 * 60 * 1000),
		StatusSinceLowerBound: timePtr(diffBase.Add(-2 * time.Hour)),
		BlockedForMs:          i64Ptr(1000),
		BlockedForUnavailable: strPtr("no-record"),
		WaitReason:            observer.WaitReasonNone,
		RawWaitingFor:         strPtr(""),
		Attention:             false,
		LastActivityAt:        timePtr(diffBase.Add(-time.Minute)),
		Needs:                 strPtr("input"),
		Detail:                strPtr("detail"),
		Tempo:                 strPtr("active"),
		Tokens:                i64Ptr(17),
		Children:              []string{"child-a"},
		ResumeSessionID:       strPtr("resume-a"),
		RecordUpdatedAt:       timePtr(diffBase.Add(-time.Hour)),
		RecordAgeMs:           i64Ptr(3_600_000),
		ReapedMidWorkAt:       timePtr(diffBase.Add(-30 * time.Minute)),
		Tmux:                  &observer.TmuxRef{Raw: "madbench:@53.%109", Session: "madbench", Window: "@53", Pane: "%109"},
		PaneAlive:             boolPtr(true),
		ProcAlive:             boolPtr(true),
		Live:                  boolPtr(true),
		LiveReason:            observer.LiveReasonProcPane,
		ListedBy:              []string{"registry"},
		ClaudeVersion:         "2.1.275",
		MessagingSocketPath:   strPtr("/tmp/cc-socks/4242.sock"),
		TranscriptCollected:   false,
		Transcript:            nil,
		Sources:               []string{"registry"},
		Stale:                 false,
		ObservedAt:            diffBase,
		Advisories:            []observer.Advisory{},
	}
}

// mutators is one change per field name. The key set is asserted to equal
// observer.DiffedFields exactly, which is what stops the two lists drifting:
// adding a field to the diffed set without teaching the comparison about it
// fails here rather than going silently unreported to every subscriber.
var mutators = map[string]func(*observer.Session){
	"status":                func(s *observer.Session) { s.Status = observer.StatusBusy },
	"rawStatus":             func(s *observer.Session) { s.RawStatus = "busy" },
	"statusSource":          func(s *observer.Session) { s.StatusSource = "jobs" },
	"statusSince":           func(s *observer.Session) { s.StatusSince = timePtr(diffBase) },
	"statusSinceLowerBound": func(s *observer.Session) { s.StatusSinceLowerBound = nil },
	"lastActivityAt":        func(s *observer.Session) { s.LastActivityAt = timePtr(diffBase) },
	"waitReason":            func(s *observer.Session) { s.WaitReason = observer.WaitReasonInputNeeded },
	"attention":             func(s *observer.Session) { s.Attention = true },
	"needs":                 func(s *observer.Session) { s.Needs = nil },
	"detail":                func(s *observer.Session) { s.Detail = strPtr("other") },
	"tempo":                 func(s *observer.Session) { s.Tempo = strPtr("idle") },
	"tokens":                func(s *observer.Session) { s.Tokens = i64Ptr(18) },
	"children":              func(s *observer.Session) { s.Children = []string{"child-a", "child-b"} },
	"resumeSessionId":       func(s *observer.Session) { s.ResumeSessionID = nil },
	"reapedMidWorkAt":       func(s *observer.Session) { s.ReapedMidWorkAt = nil },
	"name":                  func(s *observer.Session) { s.Name = "renamed" },
	"nameSource":            func(s *observer.Session) { s.NameSource = "cwd" },
	"cwd":                   func(s *observer.Session) { s.CWD = "/elsewhere" },
	"tmux":                  func(s *observer.Session) { s.Tmux = nil },
	"pid":                   func(s *observer.Session) { s.PID = intPtr(4243) },
	"kind":                  func(s *observer.Session) { s.Kind = observer.KindBackground },
	"rawKind":               func(s *observer.Session) { s.RawKind = strPtr("bg") },
	"live":                  func(s *observer.Session) { s.Live = nil },
	"liveReason":            func(s *observer.Session) { s.LiveReason = observer.LiveReasonProcOnly },
	"listedBy":              func(s *observer.Session) { s.ListedBy = []string{"registry", "jobs"} },
	"procAlive":             func(s *observer.Session) { s.ProcAlive = boolPtr(false) },
	"paneAlive":             func(s *observer.Session) { s.PaneAlive = nil },
	"sources":               func(s *observer.Session) { s.Sources = []string{"registry", "transcript"} },
	"advisories": func(s *observer.Session) {
		s.Advisories = []observer.Advisory{{Code: observer.AdvStatusSuspect, Fields: map[string]any{"ageMs": int64(1)}}}
	},
	"transcriptCollected": func(s *observer.Session) { s.TranscriptCollected = true },
	// A walk that errored last rebuild completing on this one: the session is
	// found to be another's child. (baseSession stays unlaunched, because
	// every membership test below needs it in the default view.)
	"spawnedBy": func(s *observer.Session) { s.SpawnedBy = strPtr("launcher-a") },
	"transcript": func(s *observer.Session) {
		s.Transcript = &observer.TranscriptFacts{Path: "/tmp/x.jsonl", SizeBytes: 10}
	},
}

// clockMutators changes each clock-derived field. Every one of these must
// produce NO changed field at all.
var clockMutators = map[string]func(*observer.Session){
	"statusAgeMs":  func(s *observer.Session) { s.StatusAgeMs = i64Ptr(999) },
	"blockedForMs": func(s *observer.Session) { s.BlockedForMs = i64Ptr(999) },
	"recordAgeMs":  func(s *observer.Session) { s.RecordAgeMs = i64Ptr(999) },
	"observedAt":   func(s *observer.Session) { s.ObservedAt = diffBase.Add(time.Hour) },
	"stale":        func(s *observer.Session) { s.Stale = true },
}

// identityMutators changes each field classified as immutable for the life of a
// session. They are not diffed either: a change in one of them would mean a
// different session, not a changed one.
var identityMutators = map[string]func(*observer.Session){
	"shortId":               func(s *observer.Session) { s.ShortID = nil },
	"startedAt":             func(s *observer.Session) { s.StartedAt = timePtr(diffBase) },
	"formerNames":           func(s *observer.Session) { s.FormerNames = []string{"a", "b"} },
	"claudeVersion":         func(s *observer.Session) { s.ClaudeVersion = "2.1.999" },
	"messagingSocketPath":   func(s *observer.Session) { s.MessagingSocketPath = nil },
	"blockedForUnavailable": func(s *observer.Session) { s.BlockedForUnavailable = nil },
	"rawWaitingFor":         func(s *observer.Session) { s.RawWaitingFor = strPtr("input") },
	"recordUpdatedAt":       func(s *observer.Session) { s.RecordUpdatedAt = timePtr(diffBase) },
}

func TestTheComparisonCoversExactlyTheDiffedSet(t *testing.T) {
	for _, field := range observer.DiffedFields {
		if _, ok := mutators[field]; !ok {
			t.Errorf("DiffedFields names %q and no test mutates it: the comparison may not "+
				"look at it, and a subscriber would never hear about it", field)
		}
	}
	for field := range mutators {
		if !observer.IsDiffed(field) {
			t.Errorf("a mutator exists for %q, which is not in DiffedFields", field)
		}
	}
	if len(mutators) != len(observer.DiffedFields) {
		t.Errorf("%d mutators against %d diffed fields", len(mutators), len(observer.DiffedFields))
	}
}

func TestEveryDiffedFieldIsDetectedAndNamed(t *testing.T) {
	for field, mutate := range mutators {
		t.Run(field, func(t *testing.T) {
			prev := baseSession()
			next := baseSession()
			mutate(&next)

			got := observer.ChangedFields(prev, next)
			if len(got) != 1 || got[0] != field {
				t.Fatalf("ChangedFields = %v, want exactly [%q]", got, field)
			}
			// And the comparison is symmetric: a change back is also a change.
			if back := observer.ChangedFields(next, prev); len(back) != 1 || back[0] != field {
				t.Fatalf("reverse ChangedFields = %v, want exactly [%q]", back, field)
			}
		})
	}
}

func TestNoClockDerivedFieldEverProducesAChange(t *testing.T) {
	for field, mutate := range clockMutators {
		t.Run(field, func(t *testing.T) {
			prev := baseSession()
			next := baseSession()
			mutate(&next)
			if got := observer.ChangedFields(prev, next); len(got) != 0 {
				t.Fatalf("changing the clock-derived %q reported %v; every rebuild moves "+
					"these, so diffing them would emit a session_changed for every session "+
					"on every event", field, got)
			}
		})
	}
}

func TestNoIdentityFieldProducesAChange(t *testing.T) {
	for field, mutate := range identityMutators {
		t.Run(field, func(t *testing.T) {
			prev := baseSession()
			next := baseSession()
			mutate(&next)
			if got := observer.ChangedFields(prev, next); len(got) != 0 {
				t.Fatalf("changing the identity field %q reported %v", field, got)
			}
		})
	}
}

func TestTheClassificationIsTotalOverEverySessionField(t *testing.T) {
	// The three test tables together must account for every field on the wire
	// except sessionId itself, which is the join key and cannot mutate without
	// being a different session.
	covered := map[string]bool{"sessionId": true}
	for f := range mutators {
		covered[f] = true
	}
	for f := range clockMutators {
		covered[f] = true
	}
	for f := range identityMutators {
		covered[f] = true
	}
	for _, f := range observer.AllSessionFields {
		if !covered[f] {
			t.Errorf("%q is on a session and no test says whether it is diffed", f)
		}
	}
}

func TestAnIdenticalSessionProducesNoChangedFields(t *testing.T) {
	if got := observer.ChangedFields(baseSession(), baseSession()); len(got) != 0 {
		t.Fatalf("two identical sessions differ in %v", got)
	}
}

func TestChangedFieldsAreReportedInDiffedFieldOrder(t *testing.T) {
	prev := baseSession()
	next := baseSession()
	next.Name = "renamed"
	next.Status = observer.StatusBusy
	next.Live = nil

	got := observer.ChangedFields(prev, next)
	want := []string{"status", "name", "live"} // DiffedFields order, not alphabetical
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedFields = %v, want %v — a stable order is what makes two frames "+
			"over the same state byte-identical", got, want)
	}

	// And the general property the example stands for: the output is always a
	// SUBSEQUENCE of DiffedFields, so no pair of fields can swap places
	// depending on which of them changed.
	index := map[string]int{}
	for i, f := range observer.DiffedFields {
		index[f] = i
	}
	last := -1
	for _, f := range got {
		if index[f] <= last {
			t.Fatalf("changed[] is not in DiffedFields order: %v", got)
		}
		last = index[f]
	}
}

func TestANilSliceAndAnEmptySliceAreDifferent(t *testing.T) {
	prev := baseSession()
	prev.Children = nil
	next := baseSession()
	next.Children = []string{}
	if got := observer.ChangedFields(prev, next); len(got) != 1 || got[0] != "children" {
		t.Fatalf("ChangedFields = %v, want [children]: the wire preserves nil-ness, so "+
			"null -> [] is a change a consumer can see and must therefore be reported", got)
	}
}

// ---------------------------------------------------------------------------
// Diff over snapshots
// ---------------------------------------------------------------------------

func snapshotOf(at time.Time, sessions ...observer.Session) *observer.Snapshot {
	return &observer.Snapshot{
		ObservedAt: at,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}
}

func TestTheFirstPublicationIsAllAdditions(t *testing.T) {
	a := baseSession()
	b := baseSession()
	b.SessionID = "22222222-2222-3333-4444-555555555555"

	events := observer.Diff(nil, snapshotOf(diffBase, a, b), observer.Filter{})
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 additions: %+v", len(events), events)
	}
	for _, e := range events {
		if e.Type != observer.EventSessionAdded {
			t.Errorf("event type %q, want session_added", e.Type)
		}
		if e.Session == nil {
			t.Error("a session_added carries no session")
		}
		if !e.At.Equal(diffBase) {
			t.Errorf("event At = %v, want the snapshot's own instant %v", e.At, diffBase)
		}
	}
}

func TestAChangedSessionCarriesTheWholeSessionAndTheChangedNames(t *testing.T) {
	prev := baseSession()
	next := baseSession()
	next.Status = observer.StatusWaiting
	next.WaitReason = observer.WaitReasonPermissionPrompt
	next.Attention = true

	events := observer.Diff(snapshotOf(diffBase, prev), snapshotOf(diffBase, next), observer.Filter{})
	if len(events) != 1 || events[0].Type != observer.EventSessionChanged {
		t.Fatalf("got %+v, want one session_changed", events)
	}
	e := events[0]
	if e.Session == nil || e.Session.Status != observer.StatusWaiting {
		t.Fatal("a session_changed must carry the WHOLE session, not just the names: the " +
			"alternative is every client implementing a merge")
	}
	want := []string{"status", "waitReason", "attention"}
	if !reflect.DeepEqual(e.Changed, want) {
		t.Errorf("Changed = %v, want %v", e.Changed, want)
	}
}

func TestARemovalCarriesPositiveEvidenceForWhy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*observer.Session)
		gone   bool
		want   string
		// kind is the session's kind at BOTH publications. A record does not
		// change kind between rebuilds, and since code review round 2 (A14) a
		// session's streaming identity is (sessionId, kind): a fixture that
		// changed the kind would be describing a removal plus an addition.
		kind observer.Kind
	}{
		{
			name:   "a dead pid",
			mutate: func(s *observer.Session) { s.Live = boolPtr(false); s.LiveReason = observer.LiveReasonPIDDead },
			want:   observer.RemovedPIDDead,
		},
		{
			name:   "a pane that no longer exists",
			mutate: func(s *observer.Session) { s.Live = boolPtr(false); s.LiveReason = observer.LiveReasonPaneGone },
			want:   observer.RemovedPaneGone,
		},
		{
			name: "a job that reached a terminal state",
			mutate: func(s *observer.Session) {
				s.Live = boolPtr(false)
				s.LiveReason = observer.LiveReasonJobTerminal
			},
			want: observer.RemovedJobTerminal,
			kind: observer.KindBackground,
		},
		{
			name: "a record that is gone from the full set entirely",
			gone: true,
			want: observer.RemovedRecordRemoved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := baseSession()
			if tc.kind != "" {
				prev.Kind = tc.kind
			}
			var next *observer.Snapshot
			if tc.gone {
				next = snapshotOf(diffBase)
			} else {
				s := prev
				tc.mutate(&s)
				next = snapshotOf(diffBase, s)
			}

			events := observer.Diff(snapshotOf(diffBase, prev), next, observer.Filter{})
			if len(events) != 1 || events[0].Type != observer.EventSessionRemoved {
				t.Fatalf("got %+v, want one session_removed", events)
			}
			if events[0].Reason != tc.want {
				t.Errorf("Reason = %q, want %q", events[0].Reason, tc.want)
			}
			if events[0].SessionID != prev.SessionID {
				t.Errorf("SessionID = %q, want %q", events[0].SessionID, prev.SessionID)
			}
		})
	}
}

func TestEveryLiveFalseReasonMapsToANamedRemovalReason(t *testing.T) {
	// The fallback in RemovalReason must be unreachable for a provably dead
	// session: a removal that said "filtered-out" about a dead one would tell a
	// consumer the wrong thing about the only transition that matters.
	falseProducing := []observer.LiveReason{
		observer.LiveReasonPIDDead,
		observer.LiveReasonPaneGone,
		observer.LiveReasonJobTerminal,
		observer.LiveReasonJobReaped,
	}
	named := map[string]bool{
		observer.RemovedPIDDead:     true,
		observer.RemovedPaneGone:    true,
		observer.RemovedJobTerminal: true,
	}
	for _, reason := range falseProducing {
		s := baseSession()
		s.Live = boolPtr(false)
		s.LiveReason = reason
		got := observer.RemovalReason(baseSession(), snapshotOf(diffBase, s))
		if !named[got] {
			t.Errorf("live:false with reason %q produced removal reason %q, which is not one "+
				"of the three that describe a death", reason, got)
		}
	}
}

func TestMembershipIsClientRelative(t *testing.T) {
	// A subscriber filtered to waiting sessions is told a session was REMOVED
	// when it stops waiting, while a subscriber watching everything is told it
	// CHANGED. One global delta set that every client re-filtered would make
	// each client implement the filter, and a client that implemented it
	// differently would disagree about which sessions exist.
	waiting := baseSession()
	waiting.Status = observer.StatusWaiting
	waiting.WaitReason = observer.WaitReasonInputNeeded
	waiting.Attention = true

	idle := baseSession()

	prev := snapshotOf(diffBase, waiting)
	next := snapshotOf(diffBase, idle)

	all := observer.Diff(prev, next, observer.Filter{})
	if len(all) != 1 || all[0].Type != observer.EventSessionChanged {
		t.Fatalf("unfiltered subscriber got %+v, want one session_changed", all)
	}

	attentive := observer.Diff(prev, next, observer.Filter{WaitingOnly: true})
	if len(attentive) != 1 || attentive[0].Type != observer.EventSessionRemoved {
		t.Fatalf("filtered subscriber got %+v, want one session_removed", attentive)
	}
	if attentive[0].Reason != observer.RemovedFilteredOut {
		t.Errorf("Reason = %q, want %q — it is still alive, it just left this "+
			"subscriber's view", attentive[0].Reason, observer.RemovedFilteredOut)
	}
}

func TestADeadSessionIsNeverAddedToASubscribersView(t *testing.T) {
	dead := baseSession()
	dead.Live = boolPtr(false)
	dead.LiveReason = observer.LiveReasonPIDDead

	if events := observer.Diff(nil, snapshotOf(diffBase, dead), observer.Filter{}); len(events) != 0 {
		t.Fatalf("got %+v; a stream of events about sessions that have provably ended is a "+
			"stream of events about the past", events)
	}
}

func TestAnUnknownLivenessSessionIsVisibleToASubscriber(t *testing.T) {
	// M7 again, at the delta layer: the default rule is live != false, NOT
	// live == true, so an unknown-liveness session is never silently hidden.
	unknown := baseSession()
	unknown.Live = nil
	unknown.LiveReason = observer.LiveReasonUnknown

	events := observer.Diff(nil, snapshotOf(diffBase, unknown), observer.Filter{})
	if len(events) != 1 || events[0].Type != observer.EventSessionAdded {
		t.Fatalf("got %+v, want one session_added for a live:null session", events)
	}
	if !observer.Visible(unknown, observer.Filter{}) {
		t.Error("Visible said no to a live:null session")
	}
}

func TestADiffAgainstANilNextIsNoEventsRatherThanAPanic(t *testing.T) {
	if events := observer.Diff(snapshotOf(diffBase, baseSession()), nil, observer.Filter{}); events != nil {
		t.Fatalf("got %+v, want nil", events)
	}
}

func TestNoSessionIsBothAddedAndRemoved(t *testing.T) {
	prev := baseSession()
	next := baseSession()
	next.Status = observer.StatusBusy

	events := observer.Diff(snapshotOf(diffBase, prev), snapshotOf(diffBase, next), observer.Filter{})
	seen := map[string]int{}
	for _, e := range events {
		seen[e.SessionID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appears in %d events for one rebuild", id, n)
		}
	}
}

// ---------------------------------------------------------------------------
// P7 — advisories carrying clock-derived numbers
// ---------------------------------------------------------------------------

// An advisory whose only difference is a clock-derived field is NOT a change.
//
// FOUND BY P7's LIVE RUN: three of 37 real sessions emitted
// changed=["advisories"] on every single rebuild, because
// beyond-transcript-retention carries recordAgeMs and status-suspect carries
// statusAgeMs — both in ClockDerivedFields, both excluded from diffing by M19,
// and both smuggled back in under an advisory's key path. One event on the
// machine therefore produced four frames instead of one, and on a machine with
// more stale background records it would produce more.
func TestAnAdvisoryDifferingOnlyInAClockDerivedFieldIsNotAChange(t *testing.T) {
	for _, field := range append(append([]string{}, observer.ClockDerivedFields...), "ageMs") {
		if field == "observedAt" || field == "stale" {
			continue // not numbers an advisory carries
		}
		t.Run(field, func(t *testing.T) {
			prev := baseSession()
			prev.Advisories = []observer.Advisory{{
				Code:   observer.AdvBeyondTranscriptRetention,
				Fields: map[string]any{field: int64(2_761_831_770), "retentionMs": int64(2_592_000_000)},
			}}
			next := prev
			next.Advisories = []observer.Advisory{{
				Code:   observer.AdvBeyondTranscriptRetention,
				Fields: map[string]any{field: int64(2_761_852_711), "retentionMs": int64(2_592_000_000)},
			}}

			if changed := observer.ChangedFields(prev, next); len(changed) != 0 {
				t.Fatalf("changed = %v; a moving %s is not a change", changed, field)
			}
		})
	}
}

// Everything else about an advisory IS a change: the code, an observed field,
// the count, and a field appearing or disappearing. The exclusion is narrow by
// construction, not a blanket "advisories do not count".
func TestEveryOtherAdvisoryDifferenceIsStillAChange(t *testing.T) {
	base := func() observer.Session {
		s := baseSession()
		s.Advisories = []observer.Advisory{{
			Code:   observer.AdvBeyondTranscriptRetention,
			Fields: map[string]any{"recordAgeMs": int64(1), "retentionMs": int64(2_592_000_000)},
		}}
		return s
	}

	cases := map[string]func(*observer.Session){
		"a different code": func(s *observer.Session) {
			s.Advisories[0].Code = observer.AdvStatusSinceUnverifiable
		},
		"a different observed field": func(s *observer.Session) {
			s.Advisories[0].Fields = map[string]any{"recordAgeMs": int64(1), "retentionMs": int64(99)}
		},
		"an observed field appearing": func(s *observer.Session) {
			s.Advisories[0].Fields = map[string]any{
				"recordAgeMs": int64(1), "retentionMs": int64(2_592_000_000), "reason": "new",
			}
		},
		"an observed field disappearing": func(s *observer.Session) {
			s.Advisories[0].Fields = map[string]any{"recordAgeMs": int64(1)}
		},
		"one more advisory": func(s *observer.Session) {
			s.Advisories = append(s.Advisories, observer.Advisory{Code: observer.AdvLivenessUnknown})
		},
		"none at all": func(s *observer.Session) {
			s.Advisories = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			prev, next := base(), base()
			// The mutation must not share the map with prev.
			next.Advisories = append([]observer.Advisory(nil), next.Advisories...)
			mutate(&next)
			changed := observer.ChangedFields(prev, next)
			if !reflect.DeepEqual(changed, []string{"advisories"}) {
				t.Fatalf("changed = %v, want [advisories]", changed)
			}
		})
	}
}
