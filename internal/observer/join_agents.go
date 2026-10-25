package observer

// This file is §3.4's fifth case: an INTERACTIVE session that `claude agents
// --json` lists and that no registry record describes.
//
//	agents-cli only, kind: interactive (pid, no registry file)
//	  -> reported, tmux: null, paneAlive: null, liveness reduces to kill -0
//	     alone, statusSince: null + advisory. NEVER DROPPED — dropping it
//	     would be the registry-blindness bug in reverse.
//
// It is the corroboration source's one EXCLUSIVE contribution (ports.go says so
// on AgentsSource), and nothing produced it: P4 added membership to both halves
// of the join and saw the case live — pid 49145, in the listing and not in the
// registry — but no half of the join ever built a session from a listing entry
// alone, so `tt sessions --json --refresh` returned an EMPTY set for a machine
// whose vendor listing named a live session. FOUND BY THE INDEPENDENT
// BLACK-BOX SUITE (TestCLIOnlyInteractiveHasUnknownTime).
//
// Scope, deliberately: interactive entries only. §3.5's last line also gives a
// BACKGROUND entry with no job record a home (live: null, liveness-unknown), and
// the policy already answers it (Evidence.HasJobRecord), but no measurement has
// ever shown that case and it is not what was found missing; it is recorded as
// a residual rather than built speculatively.

// joinAgentsOnly returns one session per interactive listing entry that the
// registry half of the join did not already produce.
//
// An entry is left out when EITHER its sessionId or its pid belongs to a
// registry session. The sessionId half is the join key. The pid half is the
// registry's authority over its own process: `<pid>.json` says which session a
// process holds NOW, and the listing may be a cached one taken before a /clear
// or a resume gave that pid a new sessionId. Admitting the listing's older id
// there would report a second, ghost session on a live pid — live: true, from a
// source that is forbidden to win anything.
//
// Staleness does NOT gate membership, for the reason listedBy gives: a listing
// that has aged still records that the vendor listed the session, and a
// staleness gate here would make the session appear and vanish with the age of
// a cache rather than with anything that happened. Liveness is the pid gate's,
// exactly as for a registry session, so a listed pid that has exited reads
// live: false and leaves the default view on positive evidence.
func joinAgentsOnly(in buildInput, prev previousState, registered []Session, policy Policy) []Session {
	if !in.agents.OK || len(in.agents.Entries) == 0 {
		return nil
	}

	claimedIDs := make(map[string]bool, len(registered)+len(in.registry.Records))
	claimedPIDs := make(map[int]bool, len(registered)+len(in.registry.Records))
	for _, rec := range in.registry.Records {
		claimedIDs[rec.SessionID] = true
		claimedPIDs[rec.PID] = true
	}
	for _, s := range registered {
		// The registry half also carries sessions forward from a torn file
		// (§6.5), which are not in Records this time.
		claimedIDs[s.SessionID] = true
		if s.PID != nil {
			claimedPIDs[*s.PID] = true
		}
	}

	var out []Session
	for _, e := range in.agents.Entries {
		if !isInteractiveEntry(e) || claimedIDs[e.SessionID] {
			continue
		}
		if e.PID != nil && claimedPIDs[*e.PID] {
			continue
		}
		// Entries are sorted by sessionId, so a duplicate in the listing is
		// resolved by that fixed order: first wins, the rest are discarded.
		claimedIDs[e.SessionID] = true
		if e.PID != nil {
			claimedPIDs[*e.PID] = true
		}
		out = append(out, sessionFromAgentsEntry(e, in, prev, policy))
	}
	return out
}

// isInteractiveEntry is §3.4's "kind: interactive (pid, …)". An entry the
// vendor did not label is taken as interactive only when it carries a pid,
// which is what every measured interactive entry has and no background one
// does.
func isInteractiveEntry(e AgentsEntry) bool {
	switch e.Kind {
	case string(KindInteractive):
		return true
	case "":
		return e.PID != nil
	default:
		return false
	}
}

// sessionFromAgentsEntry maps one listing entry onto a domain session.
//
// Everything the listing cannot know stays nil: no tmux reference, so no pane
// gate and paneAlive null; no status timestamp, so statusSince null with
// status-since-unavailable — startedAt is a PROCESS timestamp and substituting
// it would report a 40-day-old agent as blocked for 40 days (§3.4's table).
func sessionFromAgentsEntry(e AgentsEntry, in buildInput, prev previousState, policy Policy) Session {
	status, waitReason := MapAgentsStatus(e.Status, e.State, e.WaitingFor)

	s := Session{
		SessionID:   e.SessionID,
		Kind:        KindInteractive,
		Name:        e.Name,
		FormerNames: []string{},
		CWD:         e.CWD,
		StartedAt:   e.StartedAt,

		Status:       status,
		RawStatus:    e.Status,
		StatusSource: SourceAgentsCLI,
		StatusSince:  nil,

		WaitReason: waitReason,
		Attention:  status.IsAttention(),

		// Not read here; the enrichment pass may locate one by glob exactly as
		// it does for a registry session.
		TranscriptCollected: false,

		Sources:    []string{SourceAgentsCLI},
		ListedBy:   []string{SourceAgentsCLI},
		ObservedAt: in.now,
		Advisories: []Advisory{},
	}
	if e.PID != nil {
		pid := *e.PID
		s.PID = &pid
	}
	if e.WaitingFor != "" {
		raw := e.WaitingFor
		s.RawWaitingFor = &raw
	}

	// AC3(a): never absent, never 0-for-unknown — an explicit null with the
	// named advisory. statusAgeMs is null, so §3.6's status-suspect cannot
	// apply by definition.
	s.Advisories = appendAdvisory(s.Advisories,
		NewAdvisory(AdvStatusSinceUnavailable, map[string]any{"source": SourceAgentsCLI}))
	if s.Attention {
		reason := AdvStatusSinceUnavailable
		s.BlockedForUnavailable = &reason
	}

	// Liveness reduces to kill -0 alone: there is no pane reference to gate
	// on, and HasTmuxRef false is decideInteractive's "proc-only" branch.
	ev := Evidence{Kind: KindInteractive, Status: status, HasPID: s.PID != nil}
	if old, ok := prev.sessions[sessionKey{ID: e.SessionID, Kind: KindInteractive}]; ok {
		ev.Previous = copyBool(old.Live)
	}
	if s.PID != nil {
		procAlive, procErr := gateProc(in, *s.PID)
		s.ProcAlive = procAlive
		ev.ProcAlive = procAlive
		if procErr != nil {
			ev.GateError, ev.GateField, ev.GateErr = true, "procAlive", procErr.Error()
		}
	}

	v := Decide(ev, in.now, policy)
	s.Live = v.Live
	s.LiveReason = v.Reason
	for _, a := range v.Advisories {
		s.Advisories = appendAdvisory(s.Advisories, a)
	}
	return s
}
