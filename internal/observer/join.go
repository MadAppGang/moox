package observer

import (
	"reflect"
	"sort"
	"time"
)

// This file is the JOIN: it turns what the sources reported into the domain's
// sessions. Phase P1 builds the registry half (interactive sessions); P3 adds
// the jobs half and M8's cross-kind rule, and P4 adds agents-cli corroboration.
//
// The two properties that have to survive those additions, stated here because
// they are what the join is FOR:
//
//  1. Determinism. Sources are iterated in a fixed order and, within a source,
//     records in a stable identity order (registry: pid ascending). Never
//     map-enumeration order. "The first in a fixed order wins" is only a rule
//     if the order is fixed.
//  2. A failure is never a removal. A record that would not parse, a pane
//     listing that would not answer, a gate that errored — none of them make a
//     session disappear. Removal requires POSITIVE evidence: a registry file
//     gone AND a dead pid, or a job record in a terminal state. This is the
//     "one malformed record blanked everything" bug this repository has already
//     shipped once, and the join is where it would come back.

// buildInput is everything one rebuild read, plus the errors it met. It is a
// plain value so the join is a pure function of it and the previous state,
// which is what makes every case below testable without a filesystem.
type buildInput struct {
	now      time.Time
	registry RegistryResult
	// panes is nil when the pane gate could not answer; panesErr says why. A
	// nil map and an empty map are NOT the same thing here, and the difference
	// decides whether an interactive session reads live: null or live: false.
	panes    map[string]bool
	panesErr error
	// members is the foreign-server judge (moox §4.5), nil when no
	// MembershipSource is wired or the pane listing failed; membersErr is set
	// when the process table could not be read.
	members    Membership
	membersErr error
	// procGate is the per-pid liveness gate, bound to the ProcLive port. It is
	// a function rather than the port itself so the join stays a pure function
	// of its input, and nil means "no gate available", which the policy reads
	// as no evidence rather than as death.
	procGate func(pid int) (bool, error)

	// ---- the background half ----

	jobs JobsResult
	// jobTiming is what the snapshot path already read about each job's
	// transcript, keyed by sessionId. It is an INPUT to the join rather than
	// something the join fetches, which is what keeps the join a pure function
	// and what lets Invariant B's ordering obligation (arm -> read -> publish)
	// live in one place.
	jobTiming map[string]jobTiming
	// changedPaths is the set of transcript paths that fired a watch event
	// since the last rebuild — Invariant B case 5. Empty in a one-shot CLI run;
	// P5 populates it.
	changedPaths map[string]bool

	// agents is the corroboration listing: the ONLY positive liveness signal a
	// background job has. The zero value has OK false, which is rule 6b(ii) —
	// "the source never ran" — and answers live: null with its evidence rather
	// than true from the absence of disproof. P4 fills it.
	//
	// Its AGE and STALENESS are deliberately NOT carried alongside it: they are
	// derived in the join, from ObservedAt against this rebuild's `now` and the
	// policy's StaleAfterMs. Carrying a precomputed `stale` bool would give two
	// places the chance to disagree about it, and it would hide the fact that
	// rule 6 is CLOCK-SENSITIVE — the same record and the same listing answer
	// live: true at one instant and live: null thirty seconds later, with
	// nothing having changed on disk.
	agents AgentsResult
}

// previousState is what the join is allowed to remember between rebuilds. It is
// deliberately tiny: the previously published sessions (for the gate-error rule
// and for §6.5's keep-previous), and the paths the previous registry read came
// from (so an unreadable file can be matched to the session it described).
type previousState struct {
	// sessions is keyed by (sessionId, kind), not by sessionId alone. M8's
	// cross-kind case is real — one id can name both an interactive session and
	// a background job — and a map keyed on the id alone would let one
	// population's previous liveness answer be handed to the other's gate-error
	// rule, silently.
	sessions map[sessionKey]Session
	// pathToSessionID maps a registry file path to the session it held last
	// time. A torn file cannot tell us its own sessionId — that is what torn
	// means — so the previous read is the only handle on it.
	pathToSessionID map[string]string
	// unreadable is the set of session ids whose record failed to parse on the
	// PREVIOUS rebuild. A second consecutive failure is what promotes
	// parse-retry-failed to unparseable (§6.5).
	unreadable map[string]bool

	// timing is the 64 KiB timing tail's cache, keyed on (sessionId,
	// linkScanPath) per §7.3. A rebuild builds a FRESH map and copies forward
	// only the entries whose key still matches the record, which is Invariant
	// B case 2's "the OLD path's facts are invalidated, not carried forward"
	// expressed as an operation nobody can forget to perform.
	timing map[TimingCacheKey]TimingFacts
	// jobs is the per-job state Invariant B's change predicates compare
	// against: the path, the updatedAt and the derived status as of the last
	// rebuild. An absent entry IS case 1 (a new job).
	jobs map[string]TimingState
	// jobPathToSessionID maps a state.json path to the session it held last
	// time, and jobUnreadable is the set of background session ids whose record
	// failed to parse on the PREVIOUS rebuild. They are the jobs-tree twins of
	// pathToSessionID and unreadable, kept SEPARATE rather than shared: the two
	// populations can collide on a session id (M8's cross-kind case is real),
	// and one population's torn record must not promote the other's status to
	// unknown.
	jobPathToSessionID map[string]string
	jobUnreadable      map[string]bool
}

// sessionKey identifies a session across rebuilds.
type sessionKey struct {
	ID   string
	Kind Kind
}

func newPreviousState() previousState {
	return previousState{
		sessions:           map[sessionKey]Session{},
		pathToSessionID:    map[string]string{},
		unreadable:         map[string]bool{},
		timing:             map[TimingCacheKey]TimingFacts{},
		jobs:               map[string]TimingState{},
		jobPathToSessionID: map[string]string{},
		jobUnreadable:      map[string]bool{},
	}
}

// joinRegistry builds one session per registry record, applies §6.5's
// keep-previous rule to the records that would not parse, and returns the
// sessions in a stable order together with the next previousState.
func joinRegistry(in buildInput, prev previousState, policy Policy) ([]Session, previousState) {
	next := newPreviousState()
	sessions := make([]Session, 0, len(in.registry.Records))
	seen := make(map[string]int, len(in.registry.Records))
	twins := backgroundTwins(in)

	for _, rec := range in.registry.Records {
		next.pathToSessionID[rec.Path] = rec.SessionID

		if twin, ok := twins[rec.SessionID]; ok && twin.Path == rec.Path {
			// The process serving a background job. It is not a second
			// session: the job half merges it (join_twins.go).
			continue
		}

		if idx, dup := seen[rec.SessionID]; dup {
			// §3.2's duplicate rule: the FIRST record in the fixed order wins
			// and the loser is DISCARDED, not merged. Merging would mean
			// choosing one pid's liveness for two records, which is a wrong
			// answer where "no opinion about the loser" is an honest one. The
			// advisory travels on the winner, because the loser is not emitted.
			sessions[idx].Advisories = appendAdvisory(sessions[idx].Advisories,
				NewAdvisory(AdvSessionIDDuplicate, map[string]any{
					"kind":   string(KindInteractive),
					"source": SourceRegistry,
					"winner": int64(sessions[idx].pidOrZero()),
					"loser":  int64(rec.PID),
				}))
			continue
		}

		s := sessionFromRegistry(rec, in, prev, policy)
		seen[rec.SessionID] = len(sessions)
		sessions = append(sessions, s)
	}

	// §6.5 — a file that would not parse means TORN, not gone. Carry the
	// previous answer forward, marked stale, rather than dropping the session.
	for _, f := range in.registry.Failures {
		id, ok := prev.pathToSessionID[f.Path]
		if !ok {
			// We have never successfully read this file, so there is no
			// previous answer to keep. Reporting a session we have never
			// decoded would mean inventing its identity; the file is simply
			// counted in parseErrors and re-read on the next event.
			continue
		}
		if _, alreadyRead := seen[id]; alreadyRead {
			continue
		}
		old, ok := prev.sessions[sessionKey{ID: id, Kind: KindInteractive}]
		if !ok {
			continue
		}

		carried := old
		carried.Stale = true
		carried.ObservedAt = in.now
		carried.Advisories = appendAdvisory(carried.Advisories,
			NewAdvisory(AdvParseRetryFailed, map[string]any{
				"path":  f.Path,
				"bytes": int64(f.Bytes),
			}))

		if prev.unreadable[id] {
			// Two consecutive events with an unreadable record. We stop
			// asserting a status we can no longer see, and keep the raw value
			// so the last thing the vendor actually said is still legible.
			carried.Status = StatusUnknown
			carried.WaitReason = WaitReasonNone
			carried.Attention = false
			carried.BlockedForMs = nil
			carried.BlockedForUnavailable = nil
			carried.Advisories = appendAdvisory(carried.Advisories,
				NewAdvisory(AdvUnparseable, map[string]any{"path": f.Path}))
		}

		// Ages are re-derived against this rebuild's clock even though the
		// record is unchanged: statusAgeMs is our arithmetic, not the vendor's
		// value, and freezing it would make a stale session look fresh.
		carried.StatusAgeMs = AgeMs(in.now, carried.StatusSince)
		carried.BlockedForMs = nil
		if carried.Attention {
			carried.BlockedForMs = carried.StatusAgeMs
		}

		next.unreadable[id] = true
		next.pathToSessionID[f.Path] = id
		seen[id] = len(sessions)
		sessions = append(sessions, carried)
	}

	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	// next.sessions is filled by the CALLER, once, from the merged set: the
	// background half has not run yet and a half-filled previous state would
	// make the gate-error rule's "previous value" depend on which half of the
	// join went first.
	return sessions, next
}

// sessionFromRegistry maps one registry record onto a domain session. Every
// field the registry wins under §3.4 comes from here; everything a registry
// record cannot know stays nil, which for the nine background-only fields is
// the correct and permanent answer.
func sessionFromRegistry(rec RegistryRecord, in buildInput, prev previousState, policy Policy) Session {
	pid := rec.PID
	status, waitReason := MapRegistryStatus(rec.Status, rec.WaitingFor)

	s := Session{
		SessionID:  rec.SessionID,
		PID:        &pid,
		Kind:       KindInteractive,
		Name:       rec.Name,
		NameSource: rec.NameSource,
		// Empty rather than nil so the JSON reads [] and a consumer never has
		// to treat null and [] as the same thing. The codec preserves nil-ness
		// exactly, which is precisely why the choice has to be made here.
		FormerNames: emptyIfNil(rec.FormerNames),
		CWD:         rec.CWD,
		StartedAt:   rec.StartedAt,

		Status:       status,
		RawStatus:    rec.Status,
		StatusSource: SourceRegistry,
		// Verbatim, and never "corrected" against a transcript mtime: this
		// field means "in this status SINCE" (measured fact 3), and 23 of 32
		// sessions legitimately carried a value set within 20 s of boot after
		// hours of idling.
		StatusSince: rec.StatusUpdatedAt,

		WaitReason: waitReason,
		Attention:  status.IsAttention(),

		ClaudeVersion:       rec.Version,
		MessagingSocketPath: stringPtrOrNil(rec.MessagingSocketPath),

		// The registry half of the join does not read transcripts at all, so
		// "we did not look" is the honest value. P6 sets it true.
		TranscriptCollected: false,

		Sources:    []string{SourceRegistry},
		ListedBy:   []string{SourceRegistry},
		ObservedAt: in.now,
		Advisories: []Advisory{},
	}

	if rec.WaitingFor != "" {
		raw := rec.WaitingFor
		s.RawWaitingFor = &raw
	}

	// ---- agents-cli MEMBERSHIP (P4) ----
	//
	// listedBy is membership and live is liveness (H4), so this asks only
	// whether a listing that was TAKEN names this session — never whether that
	// listing is fresh. A stale listing still records that the vendor listed it,
	// while an absent or errored one records nothing at all.
	//
	// The fixed join order is registry, jobs, agents-cli, transcript, so this
	// append happens before the transcript's (P6's Enrich).
	if in.agents.OK && in.agents.Lists(rec.SessionID) {
		s.Sources = appendSource(s.Sources, SourceAgentsCLI)
		s.ListedBy = appendSource(s.ListedBy, SourceAgentsCLI)
	} else if _, agentsStale := corroborationAge(in, policy); in.agents.OK && !agentsStale {
		// §3.5's agents-absent: a registry record missing from a FRESH listing.
		// It is INDEPENDENT evidence and never overrides a gate — a live pid
		// with a live pane still reads live: true — so it is an advisory and
		// nothing more.
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvAgentsAbsent, map[string]any{
				"observedAt": FormatTime(in.agents.ObservedAt),
			}))
	}

	// ---- the four derived time fields, registry flavour ----

	s.StatusAgeMs = AgeMs(in.now, s.StatusSince)
	if s.StatusAgeMs == nil {
		// AC3(a): never absent, never 0-for-unknown. Explicitly null, with a
		// named advisory saying why.
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceUnavailable, map[string]any{"source": SourceRegistry}))
	}
	if s.Attention {
		// AC3(b): blockedForMs is a VIEW of statusAgeMs, never a second
		// measurement, and it is null for every non-attention session.
		s.BlockedForMs = s.StatusAgeMs
		if s.BlockedForMs == nil {
			reason := AdvStatusSinceUnavailable
			s.BlockedForUnavailable = &reason
		}
	}

	// §3.6 — expose the age, never correct the status. An advisory is a
	// separate field and does not change `status`; a null statusAgeMs is exempt
	// by definition.
	if status == StatusBusy && s.StatusAgeMs != nil && *s.StatusAgeMs > policy.SuspectBusyAfterMs {
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSuspect, map[string]any{"statusAgeMs": *s.StatusAgeMs}))
	}

	// ---- the two liveness gates ----

	ev := Evidence{
		Kind:   KindInteractive,
		Status: status,
		HasPID: true,
	}
	if old, ok := prev.sessions[sessionKey{ID: rec.SessionID, Kind: KindInteractive}]; ok {
		ev.Previous = copyBool(old.Live)
	}

	if rec.Tmux != "" {
		ev.HasTmuxRef = true
		ref := TmuxRef{Raw: rec.Tmux}
		if rec.TmuxParts != nil {
			ref = *rec.TmuxParts
		}
		s.Tmux = &ref
		if ref.Session == "" || ref.Window == "" || ref.Pane == "" {
			// The value is present but not in the documented shape. It is kept
			// verbatim in Raw — the NFR is "log the raw value, hold no opinion"
			// — and the pane gate below still checks it against the pane set
			// character for character, because that comparison never needs the
			// parts.
			s.Advisories = appendAdvisory(s.Advisories,
				AdvisoryLivenessUnknown("tmux", "tmux reference not in session:@window.%pane form"))
		}
	}

	procAlive, procErr := gateProc(in, pid)
	member := judgeMembership(in, pid, ev.HasTmuxRef, procAlive, procErr)
	if member.recycled {
		// PR-M16: the pid is alive but it is not the process that registered —
		// its start instant differs from the token recorded beside it. The
		// registered session is gone.
		procAlive = boolPtr(false)
	}
	s.ProcAlive = procAlive
	ev.ProcAlive = procAlive

	if ev.HasTmuxRef {
		switch {
		case in.panesErr != nil:
			// A pane set we could not read is NOT an empty pane set. Reusing a
			// cached one would be exactly the stale evidence that reports a
			// dead session as live, so paneAlive stays nil.
			s.PaneAlive = nil
		case member.err != nil:
			// The process table could not be read: no claim either way about
			// which server owns the pane (moox §4.5, row 5).
			s.PaneAlive = nil
			s.Advisories = appendAdvisory(s.Advisories,
				AdvisoryLivenessUnknown("membership", member.err.Error()))
		case member.foreign:
			// Another server's session (moox §4.5, row 4): never reported
			// dead, hidden from this server's list (matchesFilter).
			s.Foreign = true
			s.PaneAlive = nil
		case member.owned && in.panes != nil && !in.panes[rec.Tmux]:
			// Ours by ancestry, but the ref no longer names a pane (moved or
			// renumbered). The process is in one of our panes, which is the
			// fact paneAlive exists to report.
			alive := true
			s.PaneAlive = &alive
			ev.PaneAlive = &alive
			s.Advisories = appendAdvisory(s.Advisories,
				NewAdvisory(AdvTmuxRefStale, map[string]any{"tmux": rec.Tmux}))
		case in.panes != nil:
			alive := in.panes[rec.Tmux]
			s.PaneAlive = &alive
			ev.PaneAlive = &alive
		}
	}

	// One Evidence carries one gate error, so the order is fixed: the proc gate
	// is the primary one (a dead pid is decisive on its own), the pane gate
	// second. When BOTH errored, the second one gets its own advisory so
	// neither failure is hidden.
	switch {
	case procErr != nil:
		ev.GateError, ev.GateField, ev.GateErr = true, "procAlive", procErr.Error()
		if in.panesErr != nil {
			s.Advisories = appendAdvisory(s.Advisories,
				AdvisoryLivenessUnknown("paneAlive", in.panesErr.Error()))
		}
	case in.panesErr != nil && ev.HasTmuxRef:
		ev.GateError, ev.GateField, ev.GateErr = true, "paneAlive", in.panesErr.Error()
	}

	v := Decide(ev, in.now, policy)
	s.Live = v.Live
	s.LiveReason = v.Reason
	for _, a := range v.Advisories {
		s.Advisories = appendAdvisory(s.Advisories, a)
	}

	return s
}

// membershipVerdict is what the foreign-server rule concluded for one pid.
type membershipVerdict struct {
	owned, foreign, recycled bool
	err                      error
}

// judgeMembership applies moox §4.5's table. It only speaks about a session
// that has a tmux ref and a pid the proc gate reported ALIVE: a dead pid is dead
// whoever owned it, and a session outside tmux has no server to belong to.
func judgeMembership(in buildInput, pid int, hasRef bool, procAlive *bool, procErr error) membershipVerdict {
	if !hasRef || in.panesErr != nil || procErr != nil || procAlive == nil || !*procAlive {
		return membershipVerdict{}
	}
	if in.membersErr != nil {
		return membershipVerdict{err: in.membersErr}
	}
	if in.members == nil {
		return membershipVerdict{}
	}
	if in.members.Recycled(pid) {
		return membershipVerdict{recycled: true}
	}
	owns, err := in.members.Owns(pid)
	switch {
	case err != nil:
		return membershipVerdict{err: err}
	case owns:
		return membershipVerdict{owned: true}
	default:
		return membershipVerdict{foreign: true}
	}
}

// gateProc asks the proc gate about one pid. A nil return with a nil error
// means the gate was not available at all, which the liveness policy reads as
// "no evidence" rather than as death.
func gateProc(in buildInput, pid int) (*bool, error) {
	if in.procGate == nil {
		return nil, nil
	}
	alive, err := in.procGate(pid)
	if err != nil {
		return nil, err
	}
	return &alive, nil
}

// emptyIfNil normalises an absent list to an empty one. The wire codec
// preserves nil-ness exactly — a nil slice encodes null, an empty slice encodes
// [] — so this is the core making a deliberate choice rather than the codec
// making it silently.
func emptyIfNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func copyBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// appendAdvisory appends unless an identical advisory is already present.
// Duplicates are possible because two gates can produce the same
// liveness-unknown, and a repeated advisory is noise a consumer would have to
// de-duplicate itself.
func appendAdvisory(list []Advisory, a Advisory) []Advisory {
	for _, existing := range list {
		if advisoryEqual(existing, a) {
			return list
		}
	}
	return append(list, a)
}

func advisoryEqual(a, b Advisory) bool {
	if a.Code != b.Code || len(a.Fields) != len(b.Fields) {
		return false
	}
	for k, v := range a.Fields {
		other, ok := b.Fields[k]
		// reflect.DeepEqual and NOT `v != other` (P6's finding, with a
		// reproducer in TestTwoAdvisoriesCarryingSlicesCanBeComparedAtAll).
		// Comparing two interface values whose dynamic type is a slice is a
		// RUNTIME PANIC, not a false — and the advisory registry documents
		// exactly one field as a slice, transcript-ambiguous's `matches`,
		// which is the field P6's glob locator raises. Two ambiguous globs in
		// one session's advisory list would have crashed the observer. For
		// every scalar the design does use, DeepEqual answers what == would.
		if !ok || !reflect.DeepEqual(v, other) {
			return false
		}
	}
	return true
}

// pidOrZero is for advisory identities, where a nil pid cannot happen (only
// registry records take part in the duplicate rule) but must not panic if a
// later phase widens the rule.
func (s Session) pidOrZero() int {
	if s.PID == nil {
		return 0
	}
	return *s.PID
}
