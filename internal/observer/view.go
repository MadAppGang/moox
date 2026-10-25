package observer

// View is the baseline half of H13's client-relative membership.
//
// Diff already answers "what changed for THIS subscriber" (diff.go), and this
// answers the question that has to agree with it: "what does THIS subscriber
// see right now". A published snapshot is the whole machine, so a filtered
// subscriber that were handed it as its baseline would be told about 39
// sessions and then receive deltas for three — and every consumer would have to
// re-implement the filter to reconcile them, which is the divergence H13 exists
// to prevent.
//
// So both halves go through ONE predicate, Visible:
//
//	baseline : View(snap, f).Sessions  == { s : Visible(s, f) }
//	deltas   : Diff(prev, next, f)     uses Visible for both endpoints
//
// A session leaving one client's filter is therefore a session_removed for that
// client and nothing at all for a client watching everything, with no second
// filter anywhere to disagree.
//
// # What it does not do
//
// It never rebuilds, never reads the clock and never touches a source. It is a
// pure projection of an already-published immutable snapshot, which is what
// lets a socket server call it once per connection per event without adding a
// single read to the machine.

// View returns what one subscriber sees of a published snapshot.
//
// Counts are RECOMPUTED over the visible set by the same function the core uses
// (countSessions), not carried over: an envelope whose counts described 39
// sessions above an array of three would be a lie in the field a consumer reads
// first. For the zero Filter over an already-default-filtered snapshot every
// session is visible and the recomputation is an identity, which is what makes
// AC11's byte-identity hold on the unfiltered socket path — the CLI and the
// socket then run the same counting code over the same sessions.
//
// The returned snapshot SHARES the input's Sources, Warnings and NotCapable.
// Published snapshots are immutable (§7.3), so sharing is safe, and copying
// them would produce a value that differs from the CLI's by nothing except
// identity — which AC11 would then have to excuse.
//
// A nil snapshot views as nil: "nothing has been published yet" is a state a
// caller must handle rather than a reason to synthesise an empty answer.
func View(snap *Snapshot, f Filter) *Snapshot {
	if snap == nil {
		return nil
	}

	out := *snap
	out.Sessions = make([]Session, 0, len(snap.Sessions))
	for i := range snap.Sessions {
		if Visible(snap.Sessions[i], f) {
			out.Sessions = append(out.Sessions, snap.Sessions[i])
		}
	}
	out.Counts = countSessions(out.Sessions)
	return &out
}

// VisibleIDs are the session ids one subscriber can see, in snapshot order.
//
// It exists for the enrichment path: §7.5(f) says an enrich:true subscriber is
// enriched "for the sessions in that connection's filtered view", and a caller
// that derived that list itself would be a second implementation of the filter
// — the thing this file exists to keep singular.
func VisibleIDs(snap *Snapshot, f Filter) []string {
	if snap == nil {
		return nil
	}
	out := make([]string, 0, len(snap.Sessions))
	for i := range snap.Sessions {
		if Visible(snap.Sessions[i], f) {
			out = append(out, snap.Sessions[i].SessionID)
		}
	}
	return out
}
