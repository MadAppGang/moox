package observer

import "sort"

// This file is the BACKGROUND half of the join, and it is the phase that
// implements the user's own correction to the design:
//
//	"why the problem with subagents? they write messages to the same log and
//	 have dedicated conversation thread we can monitor the same way as the main
//	 thread."
//
// They were right. A background job is observed exactly the way an interactive
// session is: the RECORD for its state, its own CONVERSATION TRANSCRIPT for its
// timing. `state.json`'s `updatedAt` gives statusSince; the transcript at
// `linkScanPath` gives lastActivityAt, an exact gap-free bound, plus
// statusSinceLowerBound wherever the record post-dates the thread.
//
// That retires the "both time sources are holed" problem the earlier revision
// was built around, and with it the claim that `blockedForMs` was undeliverable
// for background jobs. It is deliverable, and it is computed here.
//
// The two clocks this file keeps separate, because conflating them is the
// defect the whole design was revised to remove:
//
//	statusSince / recordUpdatedAt = when the RECORD was last written. Exact.
//	                                Errs LATE, never early (0 of 7 measured).
//	lastActivityAt                = when the THREAD last produced a message.
//	                                Exact. An INDEPENDENT measurement, never a
//	                                fallback for statusSince.
//
// And the third, which is neither: `timeline.jsonl` is not read at runtime at
// all. It is an incomplete log — one record's current state does not appear in
// its own timeline, and 2 of 7 records were rewritten days after their last
// entry.

// StatusSinceEpsMs is §3.4's cross-check tolerance: how far the record's
// updatedAt may sit after the thread's last message before we publish a lower
// bound.
//
// 2 s, and it is generous by three orders of magnitude on purpose. state.json's
// mtime tracks updatedAt to the millisecond on 6 of 7 records, and the one
// corroborable record's transcript turn boundary sits 101 ms from its
// updatedAt. The cases this threshold is meant to catch are measured at +5 days
// and +20 days, so no plausible value between 1 s and an hour changes any
// answer — which is the point of not making it a knob.
const StatusSinceEpsMs int64 = 2000

// joinJobs builds one session per job record.
//
// It writes the next rebuild's timing state into next as it goes, which is how
// Invariant B's change predicates have something to compare against, and it
// returns the sessions in directory order (the caller sorts the merged set).
func joinJobs(in buildInput, prev previousState, next *previousState, policy Policy) []Session {
	// The corroboration listing's age and staleness are derived HERE, once,
	// against this rebuild's clock. Rule 6 is clock-sensitive and nothing wakes
	// up to publish the crossing, so the derivation has to happen wherever the
	// answer is produced rather than having been decided earlier by someone
	// else.
	agentsAgeMs, agentsStale := corroborationAge(in, policy)

	sessions := make([]Session, 0, len(in.jobs.Records))
	seen := make(map[string]int, len(in.jobs.Records))
	twins := backgroundTwins(in)
	// winnerDir remembers which DIRECTORY produced each accepted session, for
	// the duplicate advisory below. It is a local map rather than a field on
	// Session because Session is the round-tripped domain type: an unexported
	// field that the wire codec cannot carry would make a decoded session
	// compare unequal to the one it was encoded from, and the codec's identity
	// property is what AC11 rests on.
	winnerDir := make(map[string]string, len(in.jobs.Records))

	for _, rec := range in.jobs.Records {
		if idx, dup := seen[rec.SessionID]; dup {
			// M8, background flavour. Records are read directory-name
			// ascending, so "first wins" is deterministic. The loser is
			// DISCARDED, not merged: merging two records means choosing one
			// record's state and the other's timing, which is a wrong answer
			// where "no opinion about the loser" is an honest one.
			//
			// The identities are DIRECTORY NAMES rather than pids: a job record
			// has no pid, anywhere (measured — no key in state.json contains
			// "pid" or "proc"), so the registry flavour's winner/loser pids
			// have no analogue here.
			sessions[idx].Advisories = appendAdvisory(sessions[idx].Advisories,
				NewAdvisory(AdvSessionIDDuplicate, map[string]any{
					"kind":   string(KindBackground),
					"source": SourceJobs,
					"winner": winnerDir[rec.SessionID],
					"loser":  rec.Dir,
				}))
			continue
		}

		var twin *RegistryRecord
		if reg, ok := twins[rec.SessionID]; ok {
			twin = &reg
		}
		s := sessionFromJob(rec, twin, in, prev, policy, agentsAgeMs, agentsStale)
		seen[rec.SessionID] = len(sessions)
		winnerDir[rec.SessionID] = rec.Dir
		sessions = append(sessions, s)

		next.jobPathToSessionID[rec.Path] = rec.SessionID

		t := in.jobTiming[rec.SessionID]
		next.jobs[rec.SessionID] = TimingState{
			Path:      rec.LinkScanPath,
			UpdatedAt: rec.UpdatedAt,
			Status:    s.Status,
		}
		if t.Located {
			// Keyed on (sessionId, path). Nothing copies the OLD path's entry
			// forward, so Invariant B case 2's invalidation is structural: a
			// resume cannot return the previous conversation's last turn
			// because there is no entry under the new key to return.
			next.timing[TimingCacheKey{SessionID: rec.SessionID, Path: t.Path}] = t.Facts
		}
	}

	// §6.5 ON THE JOBS TREE, and it is the same rule the registry half applies
	// for a different mechanism. The registry is rewritten IN PLACE, so a
	// reader can see a truncated prefix mid-write; a job record is published by
	// rename-or-replace, so the window here is an UNLINK-then-create rather
	// than a partial write. Either way a parse failure means TORN, not gone.
	//
	// Removal requires POSITIVE evidence — a record gone AND a dead pid, or a
	// terminal state — and a half-written file is neither. This is the "one
	// malformed record blanked everything" bug this repository has already
	// shipped once, reached from the background population's direction.
	for _, f := range in.jobs.Failures {
		id, ok := prev.jobPathToSessionID[f.Path]
		if !ok {
			// A file we have never successfully read. There is no previous
			// answer to keep, and inventing an identity for it is worse than a
			// counted parse error: the directory name is an 8-char prefix and a
			// sessionId cannot be reconstructed from it.
			continue
		}
		if _, alreadyRead := seen[id]; alreadyRead {
			continue
		}
		old, ok := prev.sessions[sessionKey{ID: id, Kind: KindBackground}]
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

		if prev.jobUnreadable[id] {
			// Two consecutive events with an unreadable record. We stop
			// asserting a state we can no longer see, and keep the raw value so
			// the last thing the vendor actually said stays legible.
			carried.Status = StatusUnknown
			carried.WaitReason = WaitReasonNone
			carried.Attention = false
			carried.BlockedForUnavailable = nil
			carried.Advisories = appendAdvisory(carried.Advisories,
				NewAdvisory(AdvUnparseable, map[string]any{"path": f.Path}))
		}

		// The ages are RE-DERIVED against this rebuild's clock even though the
		// record is unchanged. They are our arithmetic, not the vendor's value,
		// and freezing them would make a stale session look fresh — the
		// opposite of the point.
		carried.StatusAgeMs = AgeMs(in.now, carried.StatusSince)
		carried.RecordAgeMs = AgeMs(in.now, carried.RecordUpdatedAt)
		carried.BlockedForMs = nil
		if carried.Attention {
			carried.BlockedForMs = carried.StatusAgeMs
		}

		next.jobUnreadable[id] = true
		next.jobPathToSessionID[f.Path] = id
		// The timing state and its cached facts are carried too, so the next
		// rebuild does not read the tail again on an unchanged transcript and
		// does not mistake a recovered record for a NEW session.
		if ts, had := prev.jobs[id]; had {
			next.jobs[id] = ts
			if facts, hadFacts := prev.timing[TimingCacheKey{SessionID: id, Path: ts.Path}]; hadFacts {
				next.timing[TimingCacheKey{SessionID: id, Path: ts.Path}] = facts
			}
		}
		seen[id] = len(sessions)
		sessions = append(sessions, carried)
	}

	return sessions
}

// sessionFromJob maps one job record onto a domain session.
//
// Every field §3.4 gives the jobs source comes from here. Everything a job
// record cannot know stays nil — and for a background job that permanently
// includes pid, tmux, procAlive and paneAlive, because a background agent has
// no process of its own and no pane. That is exactly why a pid cannot be the
// join key.
//
// twin is the registry record of the process serving this job, when there is
// one (join_twins.go). It lends the session its pid, its liveness and two
// facts the job's state cannot express in time — the turn started (busy) and
// the turn stopped on the user (waiting) — and the session stays background.
func sessionFromJob(
	rec JobRecord, twin *RegistryRecord, in buildInput, prev previousState, policy Policy,
	agentsAgeMs *int64, agentsStale bool,
) Session {
	status, waitReason := MapJobRecord(rec.State, rec.Tempo)
	statusSource, rawStatus, statusSince := SourceJobs, rec.State, rec.UpdatedAt
	var rawWaitingFor string
	var twinAlive *bool
	var twinErr error
	if twin != nil {
		twinAlive, twinErr = gateProc(in, twin.PID)
		v := twinStatus(rec, *twin, twinAlive)
		status, waitReason = v.status, v.reason
		statusSource, rawStatus, statusSince, rawWaitingFor = v.source, v.raw, v.since, v.waiting
	}
	t := in.jobTiming[rec.SessionID]

	s := Session{
		SessionID: rec.SessionID,
		PID:       nil,
		Kind:      KindBackground,

		Name:       rec.Name,
		NameSource: rec.NameSource,
		// Empty rather than nil so the JSON reads [] — a job record has no
		// formerNames key at all, and a consumer should not have to treat null
		// and [] as the same thing.
		FormerNames: []string{},
		CWD:         rec.CWD,
		// createdAt is the job's own start. §3.4's tie-break rule gives Name,
		// CWD, StartedAt and Kind to the registry if present, else jobs — and a
		// job record is never in the registry, so jobs wins them here.
		StartedAt: rec.CreatedAt,

		Status:       status,
		RawStatus:    rawStatus,
		StatusSource: statusSource,
		// VERBATIM from updatedAt, and this is the reversal the measurements
		// forced: revision 1 derived it from the timeline's run start, which
		// over-reported a blocked duration permanently. updatedAt errs LATE,
		// never early (0 of 7), so it UNDER-reports — which merely delays a
		// notification, where over-reporting fires one immediately and forever.
		// A twin's statusUpdatedAt when the process's status decided it.
		StatusSince: statusSince,

		WaitReason: waitReason,
		Attention:  status.IsAttention(),

		Needs:           stringPtrOrNil(rec.Needs),
		Detail:          stringPtrOrNil(rec.Detail),
		Tempo:           stringPtrOrNil(rec.Tempo),
		Tokens:          rec.Tokens,
		Children:        rec.Children,
		ResumeSessionID: stringPtrOrNil(rec.ResumeSessionID),
		RecordUpdatedAt: rec.UpdatedAt,
		ReapedMidWorkAt: rec.ReapedMidWorkAt,

		ClaudeVersion: rec.CLIVersion,

		Sources:    []string{SourceJobs},
		ListedBy:   []string{SourceJobs},
		ObservedAt: in.now,
		Advisories: []Advisory{},
	}
	if rawWaitingFor != "" {
		s.RawWaitingFor = &rawWaitingFor
	}
	if twin != nil {
		overlayTwin(&s, *twin, twinAlive)
		if twinsDisagree(rec, *twin) {
			jobRaw := rec.State
			if rec.Tempo != "" {
				jobRaw += "/" + rec.Tempo
			}
			s.Advisories = appendAdvisory(s.Advisories, NewAdvisory(AdvStatusConflict, map[string]any{
				"sources":   []string{SourceRegistry, SourceJobs},
				"rawValues": []string{twin.Status, jobRaw},
			}))
		}
	}

	// agents-cli MEMBERSHIP (P4), before the transcript's append so the fixed
	// join order — registry, jobs, agents-cli, transcript — holds on the wire.
	// Membership, not liveness: a stale listing still records that the vendor
	// listed this job, while rule 6 below is the only thing that reads its AGE.
	// The rule-6b(iii) case (a fresh listing that does NOT name a non-terminal
	// record) is the liveness policy's, and it already raises
	// background-gate-disagreement.
	if in.agents.OK && in.agents.Lists(rec.SessionID) {
		s.Sources = appendSource(s.Sources, SourceAgentsCLI)
		s.ListedBy = appendSource(s.ListedBy, SourceAgentsCLI)
	}

	// shortId is a background-only field by construction: only a job record
	// carries an id. The record's own daemonShort wins over the directory name
	// — the directory is where we FOUND it, not what it says it is — and a
	// disagreement is reported rather than resolved silently.
	shortID := rec.DaemonShort
	if shortID == "" {
		shortID = rec.Dir
	}
	if shortID != "" {
		s.ShortID = &shortID
	}
	if rec.DaemonShort != "" && rec.Dir != "" && rec.DaemonShort != rec.Dir {
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvShortIDMismatch, map[string]any{
				"dirName":     rec.Dir,
				"daemonShort": rec.DaemonShort,
			}))
	}

	// ---- the four derived time fields, jobs flavour ----

	s.StatusAgeMs = AgeMs(in.now, s.StatusSince)
	s.RecordAgeMs = AgeMs(in.now, s.RecordUpdatedAt)
	if s.StatusAgeMs == nil {
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceUnavailable, map[string]any{"source": SourceJobs}))
	}
	if s.Attention {
		// AC3(c). blockedForMs is a VIEW of statusAgeMs and is deliverable for
		// this population after all — that is decision D1's consequence.
		s.BlockedForMs = s.StatusAgeMs
		if s.BlockedForMs == nil {
			reason := AdvStatusSinceUnavailable
			s.BlockedForUnavailable = &reason
		}
	}

	// ---- the transcript: TIMING, and nothing else ----

	applyJobTiming(&s, t, policy)

	// ---- liveness: the five signals of §3.5 ----

	ev := Evidence{
		Kind:            KindBackground,
		Status:          status,
		RawState:        rec.State,
		HasJobRecord:    true,
		ReapedMidWorkAt: rec.ReapedMidWorkAt,
		Tempo:           rec.Tempo,
		// The liveness rule reads LOCATED, not "we have facts": a reaped
		// transcript is the signal, and an unreadable one is not evidence of
		// life either.
		TranscriptPresent: t.Located,
		TranscriptGone:    t.Named && !t.Unread && !t.Located && t.Err == nil,
		CWD:               rec.CWD,
		CWDGone:           rec.CWDGone,
		RecordAgeMs:       s.RecordAgeMs,
		LastActivityAt:    s.LastActivityAt,
		// The only POSITIVE signal available for a background job, and the
		// reason rule 6 exists at all. In a build with no corroboration source
		// AgentsOK is false, which is rule 6b(ii) — null with its evidence, not
		// true from the absence of disproof.
		AgentsOK:    in.agents.OK,
		AgentsStale: agentsStale,
		AgentsAgeMs: agentsAgeMs,
		AgentsLists: in.agents.Lists(rec.SessionID),
	}
	if old, ok := prev.sessions[sessionKey{ID: rec.SessionID, Kind: KindBackground}]; ok {
		ev.Previous = copyBool(old.Live)
	}

	v := Decide(ev, in.now, policy)
	if twin != nil && twinErr == nil && twinAlive != nil && *twinAlive {
		// The one POSITIVE signal rule 6 never had: the process serving the
		// job is running. It answers for the session outright, the way a live
		// pid does for an interactive one with no pane, and the job rules'
		// hedges are not attached to a verdict they did not reach.
		v = Verdict{Live: boolPtr(true), Reason: LiveReasonProcOnly}
	}
	s.Live = v.Live
	s.LiveReason = v.Reason
	for _, a := range v.Advisories {
		s.Advisories = appendAdvisory(s.Advisories, a)
	}

	return s
}

// applyJobTiming applies §3.4's cross-check: the whole point of having two
// sources.
//
// Four outcomes, and the difference between them is what makes statusSince
// defensible rather than merely reported:
//
//	no linkScanPath              -> nothing to bound against. status-since-unverifiable.
//	                                transcriptCollected stays FALSE — we did not look.
//	named, target absent         -> transcript-absent + status-since-unverifiable.
//	                                transcriptCollected is TRUE — we looked and it
//	                                was not there, which is a different statement
//	                                and is also §3.5 rule 4's input.
//	located, record <= activity  -> the record and the thread agree. No advisory.
//	                                3 of 5 measured.
//	located, record >  activity  -> the record was written AFTER the thread went
//	                                quiet. statusSince STAYS at updatedAt (it is
//	                                the only state-labelled timestamp we have) and
//	                                statusSinceLowerBound publishes what the
//	                                thread can prove. 2 of 5 measured, at +20 days
//	                                and +5 days.
func applyJobTiming(s *Session, t jobTiming, policy Policy) {
	if t.Unread {
		// --no-transcript (§5.2): we did not look, so transcriptCollected is
		// FALSE whatever the record names, lastActivityAt stays null, and the
		// cross-check cannot run. That narrowing is REPORTED on the one field it
		// weakens rather than left for a consumer to infer. Liveness is not
		// patched up here: Located stays false, so §3.5 rule 4 decides on
		// recordAgeMs alone, which is the fallback §5.2 names.
		s.TranscriptCollected = false
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceUnverifiable, map[string]any{
				"reason": "transcript reads were turned off (--no-transcript)",
			}))
		return
	}
	if !t.Named {
		s.TranscriptCollected = false
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceUnverifiable, map[string]any{
				"reason": "the record names no transcript",
			}))
		return
	}

	// We looked. That is true whether or not we found anything, and it is the
	// distinction transcriptCollected exists to carry.
	s.TranscriptCollected = true

	if !t.Located {
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvTranscriptAbsent, map[string]any{"path": t.Path}))
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceUnverifiable, map[string]any{
				"reason": "no transcript to bound against",
			}))
		return
	}

	// The transcript is one of this session's sources now — and only for
	// lastActivityAt. The enrichment facts (colour, api errors, subagents) are
	// a different read with a different cache key and are not collected here.
	s.Sources = appendSource(s.Sources, SourceTranscript)
	s.LastActivityAt = t.Facts.LastActivityAt

	if s.LastActivityAt == nil {
		if !t.Facts.ReachedStart {
			// The tail held no timestamped record and did not reach offset 0.
			// Measured worst case is 1 912 B against a 64 KiB tail, so this is
			// a guard rather than a path.
			s.Advisories = appendAdvisory(s.Advisories,
				NewAdvisory(AdvActivitySinceTruncated, map[string]any{
					"tailBytes": int64(t.Facts.TailBytes),
				}))
		}
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceUnverifiable, map[string]any{
				"reason": "no timestamped record in the transcript tail",
			}))
		return
	}

	if s.StatusSince == nil {
		// lastActivityAt is NEVER a fallback for statusSince. A record with no
		// updatedAt has no state-labelled timestamp, and substituting the
		// thread's last message would label a duration with a state it does not
		// belong to. The advisory already fired above.
		return
	}

	deltaMs := s.StatusSince.Sub(*s.LastActivityAt).Milliseconds()
	if deltaMs > StatusSinceEpsMs {
		lower := *s.LastActivityAt
		s.StatusSinceLowerBound = &lower
		s.Advisories = appendAdvisory(s.Advisories,
			NewAdvisory(AdvStatusSinceRecordLaterThanActivity, map[string]any{
				"recordUpdatedAt": FormatTime(*s.StatusSince),
				"lastActivityAt":  FormatTime(*s.LastActivityAt),
				"deltaMs":         deltaMs,
			}))
	}
}

// corroborationAge derives how old the vendor listing is and whether that is
// too old to speak for the present.
//
// A listing that was never taken has a nil age and is not "stale" — it is
// ABSENT, which rule 6b distinguishes by looking at AgentsOK. Reporting a nil
// listing as stale would conflate "we asked and the answer has aged" with "we
// never asked", and 6b(ii) exists precisely because those are different.
func corroborationAge(in buildInput, policy Policy) (*int64, bool) {
	if !in.agents.OK || in.agents.ObservedAt.IsZero() {
		return nil, false
	}
	observedAt := in.agents.ObservedAt
	ageMs := AgeMs(in.now, &observedAt)
	if ageMs == nil {
		return nil, false
	}
	return ageMs, *ageMs > policy.StaleAfterMs
}

// mergeSessions concatenates the two halves of the join, applies M8's
// cross-kind rule, and returns one stably ordered set.
//
// The cross-kind rule is NOT a merge. One sessionId appearing as both an
// interactive session and a background job means two different things exist
// that happen to share an id — the resume case measured on two records creates
// exactly that shape — and folding them into one record would publish a
// session with a pid AND a job state, i.e. a thing that does not exist. Both
// are emitted, each carrying an advisory naming the other kind.
//
// The sort is by (sessionId, kind) rather than sessionId alone, because two
// sessions can now share an id and AC11's byte-identity assertion must not be
// decided by which half of the join ran first.
func mergeSessions(interactive, background []Session) []Session {
	byID := make(map[string][]Kind, len(interactive)+len(background))
	for _, s := range interactive {
		byID[s.SessionID] = append(byID[s.SessionID], s.Kind)
	}
	for _, s := range background {
		byID[s.SessionID] = append(byID[s.SessionID], s.Kind)
	}

	all := make([]Session, 0, len(interactive)+len(background))
	all = append(all, interactive...)
	all = append(all, background...)

	for i := range all {
		kinds := byID[all[i].SessionID]
		if len(kinds) < 2 {
			continue
		}
		for _, k := range kinds {
			if k == all[i].Kind {
				continue
			}
			all[i].Advisories = appendAdvisory(all[i].Advisories,
				NewAdvisory(AdvSessionIDCrossKind, map[string]any{"other": string(k)}))
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].SessionID != all[j].SessionID {
			return all[i].SessionID < all[j].SessionID
		}
		return all[i].Kind < all[j].Kind
	})
	return all
}

// appendSource adds a source id once, preserving the fixed join order the
// domain documents (registry, jobs, agents-cli, transcript).
func appendSource(list []string, id string) []string {
	for _, existing := range list {
		if existing == id {
			return list
		}
	}
	return append(list, id)
}
