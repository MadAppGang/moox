package observer

import (
	"reflect"
	"time"
)

// This file computes the deltas a subscriber is told about, from two published
// snapshots. It is the reason the watch can be coarse: the port delivers "some
// watched thing changed", the core rebuilds the whole snapshot, and the
// difference between two snapshots is where the meaning is.
//
// Two rules decide everything here, and both are the difference between a delta
// stream and a poll loop with extra steps:
//
//  1. Only DiffedFields are compared. ClockDerivedFields change on every
//     rebuild by construction — statusAgeMs moves with the clock whether or not
//     anything happened — so diffing them would emit a session_changed for
//     every session on every event.
//  2. Membership is CLIENT-RELATIVE. A subscriber with a filter is told about
//     additions to and removals from ITS OWN view, so a session that changes
//     status out of a subscriber's filter is a session_removed for that
//     subscriber and a session_changed for one watching everything. The
//     alternative — one global delta set that every client re-filters — makes
//     every client implement the filter, and a client that implemented it
//     differently would silently disagree about which sessions exist.

// fieldComparator is one diffed field: its wire name and the equality test for
// it. The list is data rather than a chain of ifs so that a test can assert it
// covers exactly DiffedFields — no more, no less — which is what stops the
// diffed set and the comparison drifting apart.
type fieldComparator struct {
	Name  string
	Equal func(a, b Session) bool
}

// sessionComparators is the comparison, field by field, in DiffedFields order.
//
// Compound fields are compared with reflect.DeepEqual deliberately: it
// distinguishes a nil slice from an empty one, and the wire codec preserves
// that distinction, so "children went from null to []" is a change a consumer
// can see and must therefore be a change we report.
var sessionComparators = []fieldComparator{
	{"status", func(a, b Session) bool { return a.Status == b.Status }},
	{"rawStatus", func(a, b Session) bool { return a.RawStatus == b.RawStatus }},
	{"statusSource", func(a, b Session) bool { return a.StatusSource == b.StatusSource }},
	{"statusSince", func(a, b Session) bool { return eqTimePtr(a.StatusSince, b.StatusSince) }},
	{"statusSinceLowerBound", func(a, b Session) bool {
		return eqTimePtr(a.StatusSinceLowerBound, b.StatusSinceLowerBound)
	}},
	{"lastActivityAt", func(a, b Session) bool { return eqTimePtr(a.LastActivityAt, b.LastActivityAt) }},
	{"waitReason", func(a, b Session) bool { return a.WaitReason == b.WaitReason }},
	{"attention", func(a, b Session) bool { return a.Attention == b.Attention }},
	{"needs", func(a, b Session) bool { return eqStrPtr(a.Needs, b.Needs) }},
	{"detail", func(a, b Session) bool { return eqStrPtr(a.Detail, b.Detail) }},
	{"tempo", func(a, b Session) bool { return eqStrPtr(a.Tempo, b.Tempo) }},
	{"tokens", func(a, b Session) bool { return eqInt64Ptr(a.Tokens, b.Tokens) }},
	{"children", func(a, b Session) bool { return reflect.DeepEqual(a.Children, b.Children) }},
	{"resumeSessionId", func(a, b Session) bool { return eqStrPtr(a.ResumeSessionID, b.ResumeSessionID) }},
	{"reapedMidWorkAt", func(a, b Session) bool { return eqTimePtr(a.ReapedMidWorkAt, b.ReapedMidWorkAt) }},
	{"name", func(a, b Session) bool { return a.Name == b.Name }},
	{"nameSource", func(a, b Session) bool { return a.NameSource == b.NameSource }},
	{"cwd", func(a, b Session) bool { return a.CWD == b.CWD }},
	{"tmux", func(a, b Session) bool { return reflect.DeepEqual(a.Tmux, b.Tmux) }},
	{"pid", func(a, b Session) bool { return eqIntPtr(a.PID, b.PID) }},
	{"kind", func(a, b Session) bool { return a.Kind == b.Kind }},
	{"rawKind", func(a, b Session) bool { return eqStrPtr(a.RawKind, b.RawKind) }},
	{"live", func(a, b Session) bool { return eqBoolPtr(a.Live, b.Live) }},
	{"liveReason", func(a, b Session) bool { return a.LiveReason == b.LiveReason }},
	{"listedBy", func(a, b Session) bool { return reflect.DeepEqual(a.ListedBy, b.ListedBy) }},
	{"procAlive", func(a, b Session) bool { return eqBoolPtr(a.ProcAlive, b.ProcAlive) }},
	{"paneAlive", func(a, b Session) bool { return eqBoolPtr(a.PaneAlive, b.PaneAlive) }},
	{"sources", func(a, b Session) bool { return reflect.DeepEqual(a.Sources, b.Sources) }},
	{"advisories", func(a, b Session) bool { return advisoriesEqual(a.Advisories, b.Advisories) }},
	{"transcriptCollected", func(a, b Session) bool { return a.TranscriptCollected == b.TranscriptCollected }},
	{"transcript", func(a, b Session) bool { return reflect.DeepEqual(a.Transcript, b.Transcript) }},
	{"spawnedBy", func(a, b Session) bool { return eqStrPtr(a.SpawnedBy, b.SpawnedBy) }},
}

// advisoriesEqual compares two advisory lists IGNORING their clock-derived
// fields.
//
// FOUND BY P7's LIVE RUN, and by no fixture: over 40 s against the real
// machine, three of 37 sessions emitted a session_changed with
// changed=["advisories"] on EVERY rebuild and no actual change. The carriers,
// measured:
//
//	beyond-transcript-retention  {recordAgeMs: 2761831770 -> 2761852711}
//	status-suspect               {statusAgeMs: …}
//
// Both numbers are in ClockDerivedFields, which M19 excludes from diffing "so
// that diffing them would not emit a session_changed for all 40 sessions on
// every event" — and an advisory smuggled them back in under a different key
// path. The rule is the rule wherever the number appears, so the SAME list
// governs both, which is also what stops the two drifting apart.
//
// The consequence is exactly what notCapable declares: nothing publishes a
// frame because an age crossed a threshold. A consumer that wants the number
// reads it from the snapshot it already has, against its own render clock.
func advisoriesEqual(a, b []Advisory) bool {
	if len(a) != len(b) {
		return false
	}
	// Nil and empty are distinguishable everywhere else in this codec, and a
	// session going from "no advisories" to "an empty list" is not a change a
	// consumer can act on — but it IS a change in the value, so it is reported
	// like any other.
	if (a == nil) != (b == nil) {
		return false
	}
	for i := range a {
		if a[i].Code != b[i].Code {
			return false
		}
		if !observedFieldsEqual(a[i].Fields, b[i].Fields) {
			return false
		}
	}
	return true
}

// observedFieldsEqual compares two advisory field maps over their OBSERVED
// fields only.
func observedFieldsEqual(a, b map[string]any) bool {
	for k, v := range a {
		if clockDerivedAdvisoryField(k) {
			continue
		}
		other, ok := b[k]
		if !ok || !reflect.DeepEqual(v, other) {
			return false
		}
	}
	for k := range b {
		if clockDerivedAdvisoryField(k) {
			continue
		}
		if _, ok := a[k]; !ok {
			return false
		}
	}
	return true
}

// clockDerivedAdvisoryField reports whether an advisory field is a derivation
// from our clock rather than something observed.
//
// It reads ClockDerivedFields — the one list M19 names — plus "ageMs", which is
// the spelling the same quantity takes inside a source report (§5.3 excludes
// "every sources.*.ageMs" in the same breath).
func clockDerivedAdvisoryField(name string) bool {
	if name == "ageMs" {
		return true
	}
	for _, f := range ClockDerivedFields {
		if f == name {
			return true
		}
	}
	return false
}

// ChangedFields returns the wire names of the diffed fields that differ between
// two observations of the same session, in DiffedFields order so that a
// changed[] array is stable across rebuilds.
//
// It does NOT check that the two are the same session: a caller comparing two
// different sessions has a bug the diff cannot fix, and the identity fields are
// classified as immutable precisely so that this function does not have to
// have an opinion about them.
func ChangedFields(prev, next Session) []string {
	var out []string
	for _, c := range sessionComparators {
		if !c.Equal(prev, next) {
			out = append(out, c.Name)
		}
	}
	return out
}

// Visible reports whether a session is in a subscriber's view: the default
// liveness rule (live != false, unknown liveness INCLUDED — M7) narrowed by the
// subscriber's own filter.
//
// A subscriber asks for the dead set with Filter.IncludeDead. Without it, a
// death reads as a removal — the default view is "sessions that can still
// act". With it, a death is a change to a session the subscriber keeps, which
// is what a consumer with an ENDED view needs: it used to be unreachable over
// the socket entirely, and a panel lost every session that ended while it was
// open (code review round 2, A10).
func Visible(s Session, f Filter) bool {
	if !IncludedByDefault(s.Live, SnapshotOptions{IncludeUnknownLiveness: true, IncludeDead: f.IncludeDead}) {
		return false
	}
	return matchesFilter(s, f)
}

// Diff computes one subscriber's deltas between two published snapshots.
//
// prev may be nil, which is the first publication: every visible session is an
// addition. next must not be nil — there is no such thing as a delta to
// nowhere, and a caller with a nil next has lost a snapshot rather than
// observed an empty machine.
//
// Seq is NOT assigned here. Sequence numbers are transport metadata owned by
// the one goroutine that publishes, and a diff that stamped them would be a
// second place they came from.
func Diff(prev, next *Snapshot, f Filter) []Event {
	if next == nil {
		return nil
	}

	prevVisible := visibleByID(prev, f)
	nextVisible := visibleByID(next, f)

	events := make([]Event, 0, 4)

	// Additions and changes, in next's session order — which is sorted by
	// sessionId, so two runs over the same state produce byte-identical frames.
	for i := range next.Sessions {
		s := next.Sessions[i]
		if !Visible(s, f) {
			continue
		}
		old, had := prevVisible[keyOf(s)]
		if !had {
			session := s
			events = append(events, Event{
				Type:      EventSessionAdded,
				SessionID: s.SessionID,
				Kind:      s.Kind,
				Session:   &session,
				At:        next.ObservedAt,
			})
			continue
		}
		if changed := ChangedFields(old, s); len(changed) > 0 {
			session := s
			events = append(events, Event{
				Type:      EventSessionChanged,
				SessionID: s.SessionID,
				Kind:      s.Kind,
				Session:   &session,
				Changed:   changed,
				At:        next.ObservedAt,
			})
		}
	}

	// Removals, in prev's order.
	if prev != nil {
		for i := range prev.Sessions {
			s := prev.Sessions[i]
			if !Visible(s, f) {
				continue
			}
			if _, still := nextVisible[keyOf(s)]; still {
				continue
			}
			events = append(events, Event{
				Type:      EventSessionRemoved,
				SessionID: s.SessionID,
				Kind:      s.Kind,
				Reason:    RemovalReason(s, next),
				At:        next.ObservedAt,
			})
		}
	}

	return events
}

// SourceHealthChanged reports whether what the sources say about THEMSELVES
// changed between two publications: a source appearing or disappearing, ok,
// stale, lastError, the observed fields of its advisories, and its watch's
// mode, degradation, failures and armed counts.
//
// Counters, labels and every clock-derived value are excluded on purpose.
// Counters move on every rebuild (timing reads, spawn counts), the watch's
// event counter and lastEventAt move on every hint, and ageMs moves with the
// clock — a rule that fired on those would make "sources" a frame sent on
// every publication, which is the poll-with-extra-steps this stream exists not
// to be. Code review round 2, A12: the HEALTH changes, which used to reach no
// existing subscriber at all.
func SourceHealthChanged(prev, next *Snapshot) bool {
	if prev == nil || next == nil {
		return prev != next
	}
	if len(prev.Sources) != len(next.Sources) {
		return true
	}
	for id, a := range prev.Sources {
		b, ok := next.Sources[id]
		if !ok {
			return true
		}
		if a.OK != b.OK || a.Stale != b.Stale || !eqStrPtr(a.LastError, b.LastError) {
			return true
		}
		if !advisoriesEqual(a.Advisories, b.Advisories) {
			return true
		}
		if !watchHealthEqual(a.Watch, b.Watch) {
			return true
		}
	}
	return false
}

func watchHealthEqual(a, b *WatchReport) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Mode == b.Mode && eqStrPtr(a.Degraded, b.Degraded) && a.ArmFailures == b.ArmFailures &&
		a.ArmedDirs == b.ArmedDirs && a.ArmedFiles == b.ArmedFiles &&
		a.ArmedTranscripts == b.ArmedTranscripts
}

// RemovalReason says why a session left a subscriber's view. It requires
// POSITIVE evidence and takes it from the NEXT snapshot wherever there is any:
// a session still present but no longer live carries the rule that decided it,
// and only a session that is gone from the full set entirely is reported as
// record-removed.
//
// agents-absent is deliberately not reachable from here. Absence from a
// corroboration listing never removes a session — that is the "one malformed
// record blanks everything" class of bug, reached from a different direction.
func RemovalReason(prev Session, next *Snapshot) string {
	cur, present := next.SessionByKey(prev.SessionID, prev.Kind)
	if !present {
		return RemovedRecordRemoved
	}
	if cur.Live != nil && !*cur.Live {
		switch cur.LiveReason {
		case LiveReasonPIDDead:
			return RemovedPIDDead
		case LiveReasonPaneGone:
			return RemovedPaneGone
		case LiveReasonJobTerminal, LiveReasonJobReaped, LiveReasonJobTranscriptGone:
			return RemovedJobTerminal
		}
	}
	// Still in the full set and not provably dead: it stopped matching this
	// subscriber's filter.
	return RemovedFilteredOut
}

// visibleByID indexes a snapshot's visible sessions by (sessionId, kind). A nil
// snapshot indexes to an empty map rather than to nil, so the caller needs no
// nil check and the first publication is not a special case.
//
// The key is the JOIN's key (code review round 2, A14). Indexing by sessionId
// alone collapsed the interactive and background records that legitimately
// share an id: a change to one was compared against the other, and removing
// one was hidden by the other still holding the id.
func visibleByID(snap *Snapshot, f Filter) map[sessionKey]Session {
	if snap == nil {
		return map[sessionKey]Session{}
	}
	out := make(map[sessionKey]Session, len(snap.Sessions))
	for i := range snap.Sessions {
		s := snap.Sessions[i]
		if Visible(s, f) {
			out[keyOf(s)] = s
		}
	}
	return out
}

func keyOf(s Session) sessionKey { return sessionKey{ID: s.SessionID, Kind: s.Kind} }

func eqTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
