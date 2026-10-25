package observer

import "time"

// AsOf re-derives every CLOCK-DEPENDENT value of a published snapshot at a
// consumer's `now` — the ages, blockedForMs, the busy-too-long advisory, the
// corroboration source's staleness, and the clock-sensitive background
// liveness rules — and leaves every OBSERVATION exactly as it was published.
//
// # Why (code review round 2, A7)
//
// §7.5(c): "Ages are computed at answer time and cannot freeze, and the §3.5
// liveness policy is re-run there too." The socket client returned its
// retained snapshot, and MCP encoded it at that snapshot's own instant, so on a
// quiet machine every answer described the moment the server last rebuilt: a
// session waiting for an hour reported blockedForMs ≈ 0, and a working job
// whose thread had stopped moving stayed live long past rule 5's threshold.
// The user's decision rules out the obvious fix — a server timer — so the
// derivation belongs to the consumer, from its own clock, which is what the
// server declares with notCapable ["time-transitions"].
//
// # What it needs, and why it is all already on the wire
//
// Nothing is re-read. Ages come from the timestamps every session carries
// (statusSince, recordUpdatedAt). Rules 4, 5 and 6 need the job's transcript
// presence (the "transcript" entry in sources, added only when the record's
// transcript was located), its lastActivityAt, whether the vendor listing
// names it (listedBy), and the listing's own instant and threshold — the
// agents-cli source report's observedAt and staleAfterMs. The retention and
// working-stale thresholds come from p, which a consumer resolves the same way
// the server does (the vendor's settings for the first, defaults for the
// rest); the listing's threshold is taken from the SERVER's report, so both
// ends judge freshness against one number.
//
// # What it does not do
//
// It never runs time backwards: a `now` at or before the snapshot's instant
// returns the snapshot unchanged, so two processes whose clocks disagree by a
// few milliseconds cannot make an answer younger than its observation. And it
// re-runs only the background rules that CAN change with time (4, 5, 6/6b);
// rules 1–3 and every interactive gate are observations and stand.
//
// session.observedAt moves to `now` with the ages: it is the reference instant
// every derived number in the session is computed against (statusAgeMs ==
// observedAt − statusSince, §9.2c), and an answer whose ages were derived at
// `now` but stamped with the server's instant would break that identity.
//
// The input is never mutated; published snapshots are immutable (§7.3).
func AsOf(snap *Snapshot, now time.Time, p Policy) *Snapshot {
	if snap == nil {
		return nil
	}
	now = TruncateTime(now)
	if !now.After(snap.ObservedAt) {
		return snap
	}
	p = policyOrDefault(p)

	out := *snap
	out.ObservedAt = now

	then := snap.ObservedAt
	agThen := agentsEvidenceAt(snap.Sources, then, p)
	ag := agentsEvidenceAt(snap.Sources, now, p)
	if snap.Sources != nil {
		out.Sources = make(map[string]SourceReport, len(snap.Sources))
		for id, src := range snap.Sources {
			out.Sources[id] = sourceAsOf(id, src, now, ag)
		}
		out.Warnings = promoteSourceAdvisories(out.Sources)
	}

	if snap.Sessions != nil {
		out.Sessions = make([]Session, len(snap.Sessions))
		for i := range snap.Sessions {
			out.Sessions[i] = sessionAsOf(snap.Sessions[i], then, now, p, agThen, ag)
		}
	}
	out.Counts = countSessions(out.Sessions)
	return &out
}

func policyOrDefault(p Policy) Policy {
	def := DefaultPolicy()
	if p.StaleAfterMs <= 0 {
		p.StaleAfterMs = def.StaleAfterMs
	}
	if p.WorkingStaleAfterMs <= 0 {
		p.WorkingStaleAfterMs = def.WorkingStaleAfterMs
	}
	if p.TranscriptRetentionMs <= 0 {
		p.TranscriptRetentionMs = def.TranscriptRetentionMs
	}
	if p.SuspectBusyAfterMs <= 0 {
		p.SuspectBusyAfterMs = def.SuspectBusyAfterMs
	}
	return p
}

// agentsEvidence is the corroboration listing as rule 6 reads it, at `now`.
type agentsEvidence struct {
	ok         bool
	stale      bool
	ageMs      *int64
	staleAfter int64
}

func agentsEvidenceAt(sources map[string]SourceReport, now time.Time, p Policy) agentsEvidence {
	ev := agentsEvidence{staleAfter: p.StaleAfterMs}
	src, present := sources[SourceAgentsCLI]
	if !present {
		return ev
	}
	if src.StaleAfterMs != nil && *src.StaleAfterMs > 0 {
		ev.staleAfter = *src.StaleAfterMs
	}
	ev.ok = src.OK
	if !src.OK || src.ObservedAt == nil || src.ObservedAt.IsZero() {
		// corroborationAge's rule: a listing never taken has no age and is
		// ABSENT rather than stale; rule 6b tells the two apart by ok.
		return ev
	}
	ev.ageMs = AgeMs(now, src.ObservedAt)
	ev.stale = ev.ageMs != nil && *ev.ageMs > ev.staleAfter
	return ev
}

// sourceAsOf re-derives a source report's age and, for the corroboration
// source, its staleness and the advisory that states it.
func sourceAsOf(id string, src SourceReport, now time.Time, ag agentsEvidence) SourceReport {
	if src.ObservedAt != nil {
		src.AgeMs = sourceAgeMs(now, src.ObservedAt)
	}
	if id != SourceAgentsCLI {
		return src
	}
	suppressed := !src.OK || src.ObservedAt == nil || src.ObservedAt.IsZero() ||
		now.Sub(*src.ObservedAt).Milliseconds() > ag.staleAfter
	src.Stale = src.OK && suppressed
	if src.Labels != nil {
		labels := make(map[string]string, len(src.Labels))
		for k, v := range src.Labels {
			labels[k] = v
		}
		labels["crossChecksSuppressed"] = boolLabel(suppressed)
		src.Labels = labels
	}
	var stale []Advisory
	if src.Stale {
		stale = []Advisory{NewAdvisory(AdvSourceStale, map[string]any{
			"source":       SourceAgentsCLI,
			"ageMs":        derefInt64(src.AgeMs),
			"staleAfterMs": ag.staleAfter,
			"detail":       "corroboration only; no reported status depends on this source",
		})}
	}
	// refresh.go appends source-stale FIRST, before the three that describe
	// the last attempt.
	src.Advisories = splice(src.Advisories, []string{AdvSourceStale}, stale,
		AdvAgentsCacheContended, AdvSpawnLoopGuard, AdvCorroborationWaitExpired)
	return src
}

// clockSensitiveBackground are the §3.5 verdicts time alone can change: a
// record ageing past retention (4), a thread going quiet (5), a listing going
// stale (6 -> 6b). Every other verdict is an observation.
var clockSensitiveBackground = map[LiveReason]bool{
	LiveReasonJobBeyondRetention:           true,
	LiveReasonJobWorkingStale:              true,
	LiveReasonJobNonTerminalCorroborated:   true,
	LiveReasonJobNonTerminalUncorroborated: true,
	LiveReasonJobTranscriptGone:            true,
}

// livenessVerdictCodes are the advisories those verdicts attach. They are
// dropped and re-derived together with the verdict.
var livenessVerdictCodes = []string{
	AdvBeyondTranscriptRetention, AdvBackgroundWorkingStale,
	AdvJobCorroborationStale, AdvBackgroundGateDisagreement, AdvJobTranscriptGone,
}

func sessionAsOf(s Session, then, now time.Time, p Policy, agThen, agNow agentsEvidence) Session {
	published := s
	s.ObservedAt = now
	if s.StatusSince != nil {
		s.StatusAgeMs = AgeMs(now, s.StatusSince)
	}
	if s.RecordUpdatedAt != nil {
		s.RecordAgeMs = AgeMs(now, s.RecordUpdatedAt)
	}
	if s.Attention && s.StatusSince != nil {
		// AC3(b): a VIEW of statusAgeMs, never a second measurement.
		s.BlockedForMs = s.StatusAgeMs
	}

	advisories := s.Advisories
	if hasAdvisory(advisories, AdvStatusSuspect) || s.Status == StatusBusy {
		// §3.6's busy-too-long advisory is a function of statusAgeMs. The
		// registry join adds it after the age fields and before the liveness
		// gates' advisories; everything the merge and the enrichment add comes
		// later still.
		var suspect []Advisory
		if s.Status == StatusBusy && s.StatusAgeMs != nil && *s.StatusAgeMs > p.SuspectBusyAfterMs &&
			containsString(s.Sources, SourceRegistry) {
			suspect = []Advisory{NewAdvisory(AdvStatusSuspect, map[string]any{"statusAgeMs": *s.StatusAgeMs})}
		}
		advisories = splice(advisories, []string{AdvStatusSuspect}, suspect, append([]string{AdvLivenessUnknown}, postJoinCodes...)...)
	}

	if s.Kind == KindBackground && containsString(s.Sources, SourceJobs) && clockSensitiveBackground[s.LiveReason] {
		// The re-derivation is trusted only if the wire evidence REPRODUCES
		// the published verdict at the server's own instant. When it does not
		// — a field the policy read that the wire does not carry, a snapshot
		// assembled by hand — guessing would be worse than a stale verdict, so
		// the published one stands.
		check := Decide(backgroundEvidence(published, then, agThen), then, policyFor(p, agThen))
		if eqBoolPtr(check.Live, published.Live) && check.Reason == published.LiveReason {
			v := Decide(backgroundEvidence(published, now, agNow), now, policyFor(p, agNow))
			// The verdict's advisories are the LAST the job join adds; only
			// the merge and the enrichment add anything after them.
			advisories = splice(advisories, livenessVerdictCodes, v.Advisories, postJoinCodes...)
			s.Live = v.Live
			s.LiveReason = v.Reason
		}
	}
	s.Advisories = advisories
	return s
}

// backgroundEvidence rebuilds §3.5's Evidence for one job from what its
// published session carries, with the clock-derived inputs taken at `at`.
func backgroundEvidence(s Session, at time.Time, ag agentsEvidence) Evidence {
	tempo := ""
	if s.Tempo != nil {
		tempo = *s.Tempo
	}
	recordAge := s.RecordAgeMs
	if s.RecordUpdatedAt != nil {
		recordAge = AgeMs(at, s.RecordUpdatedAt)
	}
	return Evidence{
		Kind:              KindBackground,
		Status:            s.Status,
		RawState:          s.RawStatus,
		HasJobRecord:      true,
		ReapedMidWorkAt:   s.ReapedMidWorkAt,
		Tempo:             tempo,
		TranscriptPresent: containsString(s.Sources, SourceTranscript),
		// A refused or unreadable path raises transcript-absent too, so this
		// can over-read gone; the reproduce-the-published-verdict check in
		// sessionAsOf is what keeps that from changing an answer.
		TranscriptGone: s.TranscriptCollected && !containsString(s.Sources, SourceTranscript) &&
			hasAdvisory(s.Advisories, AdvTranscriptAbsent),
		RecordAgeMs:    recordAge,
		LastActivityAt: s.LastActivityAt,
		AgentsOK:       ag.ok,
		AgentsStale:    ag.stale,
		AgentsAgeMs:    ag.ageMs,
		AgentsLists:    containsString(s.ListedBy, SourceAgentsCLI),
	}
}

func policyFor(p Policy, ag agentsEvidence) Policy {
	p.StaleAfterMs = ag.staleAfter
	return p
}

// postJoinCodes are the session advisories added AFTER a session is built:
// the cross-kind merge's, and the enrichment pass's.
var postJoinCodes = []string{AdvSessionIDCrossKind, AdvTranscriptAmbiguous, AdvActivitySinceTruncated}

// splice replaces every advisory whose code is in remove with insert, AT THE
// POSITION the first removed one held — or, when none was there, before the
// first advisory whose code is in before (the ones the server adds later), or
// at the end. Position matters: every surface's answer is compared field for
// field and in order (AC11), and a re-derivation that changed nothing must
// produce the list the server would have.
//
// It never mutates its input (a published snapshot's slice), and it keeps a
// nil list nil when there is nothing to insert.
func splice(in []Advisory, remove []string, insert []Advisory, before ...string) []Advisory {
	if in == nil && len(insert) == 0 {
		return nil
	}
	drop := make(map[string]bool, len(remove))
	for _, c := range remove {
		drop[c] = true
	}
	kept := make([]Advisory, 0, len(in)+len(insert))
	at := -1
	for _, a := range in {
		if drop[a.Code] {
			if at < 0 {
				at = len(kept)
			}
			continue
		}
		kept = append(kept, a)
	}
	if at < 0 {
		at = len(kept)
		later := make(map[string]bool, len(before))
		for _, c := range before {
			later[c] = true
		}
		for i, a := range kept {
			if later[a.Code] {
				at = i
				break
			}
		}
	}
	out := make([]Advisory, 0, len(kept)+len(insert))
	out = append(out, kept[:at]...)
	out = append(out, insert...)
	return append(out, kept[at:]...)
}

func hasAdvisory(in []Advisory, code string) bool {
	for _, a := range in {
		if a.Code == code {
			return true
		}
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
