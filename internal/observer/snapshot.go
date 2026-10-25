package observer

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Snapshot is one immutable answer about every session the observer can see.
//
// P1 answers for the interactive population only; P3 adds the background half.
// The shape of the answer does not change when it does, which is the point of
// building the envelope here rather than in each adapter.
//
// Behaviour worth stating because it is load-bearing rather than incidental:
//
//   - The registry is read once and the pane set is listed ONCE per call, not
//     once per session. One `tmux list-panes -a` covers the whole server.
//   - Neither a read error nor a gate error fails the call. A source that could
//     not answer is reported as a source with ok: false and a lastError, and the
//     sessions it could not describe keep their previous answer. The call fails
//     only when NOTHING could be read.
//   - The clock is read exactly once, and every derived age in the returned
//     snapshot is against that one instant. A second clock read would let
//     statusAgeMs disagree with observedAt, which is the drift §9.2(c)'s
//     identity test exists to catch.
func (o *Observer) Snapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	now := o.deps.Clock.Now()

	reg, regErr := o.deps.Registry.Read(ctx)
	if reg.ObservedAt.IsZero() {
		reg.ObservedAt = now
	}

	jobs, jobsErr := o.deps.Jobs.Read(ctx)
	if jobs.ObservedAt.IsZero() {
		jobs.ObservedAt = now
	}

	// P4 — corroboration, from THIS rebuild's instant (§4.2 T5/T6). It is
	// ISSUED here and COLLECTED below, after every authoritative read, and the
	// decision to issue it at all is made in refresh.go so that this file holds
	// no spawn policy.
	//
	// THE ORDER IS THE FIX, and it is the whole of it. Corroboration is a
	// CROSS-CHECK (§4.3, §4.4: "nothing a consumer reads as a status depends on
	// this source"), so it may never be in front of the two gates §3.5 says
	// liveness is: kill -0 and pane-exists. P4 waited for the startup spawn
	// right here, under the caller's context — so a wedged `claude agents
	// --json` spent the entire read budget and `tmux list-panes` was asked with
	// an expired one, turning 35 live interactive sessions into
	// liveReason: gate-error / live: null from a source that is forbidden to
	// override a gate.
	//
	// Issued FIRST and collected LAST rather than simply moved after the gate,
	// because a spawn is asynchronous work: a healthy 130 ms listing runs
	// concurrently with the ~198 ms of authoritative reads below and costs the
	// answer nothing at all, while a wedged one costs CorroborationWait and
	// degrades only the corroboration-derived fields.
	corrCall := o.requestCorroboration(ctx, now)

	in := buildInput{now: now, registry: reg, jobs: jobs, procGate: o.deps.Proc.Alive}
	if len(opts.ChangedTranscripts) > 0 {
		in.changedPaths = make(map[string]bool, len(opts.ChangedTranscripts))
		for _, p := range opts.ChangedTranscripts {
			in.changedPaths[p] = true
		}
	}

	// The pane gate is asked only when there is something to ask about. A
	// machine with no interactive sessions should not be shelling out to tmux,
	// and a `tmux list-panes` failure on such a machine is not a degraded
	// answer — there was no question.
	panesAsked := false
	if anyTmuxRef(reg.Records) {
		panesAsked = true
		o.readPanes(ctx, &in)
	}

	// The timing tails, BEFORE the join and therefore before the publish
	// (§3.4's Invariant B). There is no watcher in a one-shot call, so
	// changedPaths is empty and the four change predicates carry the whole
	// burden — which is exactly why the rule is not "read on notification".
	// Under the watch loop it carries the transcripts that fired (A3).
	//
	// --no-transcript skips them, and says so on every job it affects (see
	// SnapshotOptions.NoTranscriptReads): no read means no transcripts source
	// report either, since a report for a read nobody performed describes
	// nothing.
	var (
		timing             map[string]jobTiming
		timingCache        map[TimingCacheKey]TimingFacts
		transcriptCounters transcriptCounters
	)
	if opts.NoTranscriptReads {
		timing, timingCache = unreadJobTiming(jobs)
	} else {
		changed := in.changedPaths
		if opts.RereadTranscripts {
			changed = make(map[string]bool, len(jobs.Records)+len(in.changedPaths))
			for p := range in.changedPaths {
				changed[p] = true
			}
			for _, rec := range jobs.Records {
				if rec.LinkScanPath != "" {
					changed[rec.LinkScanPath] = true
				}
			}
		}
		timing, timingCache, transcriptCounters = o.readJobTiming(ctx, jobs, o.prev, changed)
	}
	in.jobTiming = timing

	// Every authoritative read is done. Only now does the rebuild look at
	// corroboration, and only for CorroborationWait — see requestCorroboration
	// and CorroborationWait for why the wait is bounded separately from the
	// spawn's own timeout, and how an expired wait is published.
	o.collectCorroboration(ctx, corrCall)
	agents := o.corroborationListing()
	in.agents = agents

	interactive, next := joinRegistry(in, o.prev, o.policy)
	// §3.4's fifth case: an interactive session the vendor lists and no
	// registry record describes. After the registry half, so the registry's
	// sessions (and the pids they hold) are what it defers to.
	interactive = append(interactive, joinAgentsOnly(in, o.prev, interactive, o.policy)...)
	next.timing = timingCache
	background := joinJobs(in, o.prev, &next, o.policy)
	all := mergeSessions(interactive, background)
	// Ancestry over the MERGED set, after both halves of the join, because a
	// launcher is "another live session" whichever source described it, and
	// the liveness each session carries is the one just decided. Before the
	// previous-state write, so a subscriber's diff sees spawnedBy change the
	// rebuild the launching session dies.
	markSpawned(all, o.deps.Proc.Parent)

	// The previous state is written ONCE, from the merged set, so that neither
	// half of the join can see a partially updated view of the other.
	for _, s := range all {
		next.sessions[sessionKey{ID: s.SessionID, Kind: s.Kind}] = s
	}
	o.prev = next

	// Exit 1 only when nothing at all could be read (§5.2). An empty answer
	// PLUS a failure from an authoritative source is that case; an absent
	// directory is not — "Claude has never run here" is a fact, and it keeps
	// its exit 0 and its empty array.
	//
	// The condition is deliberately "either source errored" rather than "both".
	// Returning exit 0 with zero sessions while the registry was unreadable
	// would report "no sessions" for what is actually "we could not look", and
	// the human table has no field in which to show a source's lastError. A
	// source that errored while the OTHER one produced sessions is a different
	// case: that is a partial answer, and it is described by sources.<id>.ok
	// rather than by an exit code.
	if len(all) == 0 {
		switch {
		case regErr != nil:
			return nil, regErr
		case jobsErr != nil:
			return nil, jobsErr
		}
	}

	sessions := make([]Session, 0, len(all))
	for _, s := range all {
		if !IncludedByDefault(s.Live, opts) {
			continue
		}
		if !matchesFilter(s, opts.Filter) {
			continue
		}
		sessions = append(sessions, s)
	}

	snap := &Snapshot{
		ObservedAt: now,
		Counts:     countSessions(sessions),
		Sources:    map[string]SourceReport{},
		Warnings:   []Advisory{},
		Sessions:   sessions,
		NotCapable: o.notCapable,
	}

	snap.Sources[SourceRegistry] = o.registryReport(reg, regErr, now)
	snap.Sources[SourceJobs] = o.jobsReport(jobs, jobsErr, now)
	if transcriptCounters.lookups > 0 {
		// Reported only when a transcript was actually looked for, for the same
		// reason the tmux gate is: a source report for a read nobody performed
		// invites a consumer to compare counters that describe nothing.
		snap.Sources[SourceTranscripts] = o.transcriptsReport(transcriptCounters, now)
	}
	if panesAsked {
		snap.Sources[SourceTmux] = o.tmuxReport(in, now)
	}
	if o.deps.Agents != nil {
		// Reported only when a corroboration port is wired, for the same reason
		// the tmux and transcript reports are conditional: a source block for a
		// read nobody could perform invites a consumer to compare counters that
		// describe nothing. A build without the port says so by the block's
		// absence, and every background job's live: null carries
		// job-corroboration-stale to explain it.
		snap.Sources[SourceAgentsCLI] = o.agentsReport(agents, now)
	}
	attachSourceNotOK(snap.Sources)
	snap.Warnings = promoteSourceAdvisories(snap.Sources)

	return snap, nil
}

// attachSourceNotOK gives every source that could not be read an advisory
// saying so, unless a more specific one already does.
//
// WHY THIS EXISTS. P7's live run measured the gap: with `claude agents --json`
// wedged, `sources.agents-cli` read {ok:false, lastError:"… timed out after
// 5s"} while `warnings` was EMPTY, because the advisory that had explained the
// wait (corroboration-wait-expired) is cleared the moment the real failure
// lands — a result arrived, so the wait is over. A consumer subscribed to the
// socket and watching `warnings` therefore saw a healthy-looking stream while a
// whole source was down. The `lastError` was correct the entire time; nothing
// promoted it.
//
// THE SHAPE IS THE CAREFUL PART. The fields are the source id, its role and its
// error text — all of which are constant for as long as the failure lasts.
// Nothing here is derived from the clock, so this advisory is identical on
// every rebuild and the sources block stops changing once it has appeared.
// That is the P7 session-churn defect stated as a design rule: an advisory
// carrying a clock-derived number republishes forever, so a per-rebuild
// advisory must carry none.
//
// It is suppressed when a more specific source-scoped advisory is already
// present, because two advisories for one fact is how a consumer ends up
// showing the same outage twice.
func attachSourceNotOK(sources map[string]SourceReport) {
	for id, rep := range sources {
		if rep.OK {
			continue
		}
		if hasSourceScopedAdvisory(rep.Advisories) {
			continue
		}
		errText := "the source reported no error"
		if rep.LastError != nil {
			errText = *rep.LastError
		}
		rep.Advisories = appendAdvisory(copyAdvisories(rep.Advisories),
			NewAdvisory(AdvSourceNotOK, map[string]any{
				"source": id,
				"role":   rep.Role,
				"error":  errText,
			}))
		sources[id] = rep
	}
}

// hasSourceScopedAdvisory reports whether this source already explains itself.
// An UNKNOWN code counts as an explanation: a code this build does not know is
// still an advisory somebody put there on purpose, and §3.7's rule is that
// codes may be added.
func hasSourceScopedAdvisory(in []Advisory) bool {
	for _, a := range in {
		if scope, known := ScopeOf(a.Code); !known || scope == ScopeSource {
			return true
		}
	}
	return false
}

// anyTmuxRef reports whether any record carries a pane reference at all.
func anyTmuxRef(recs []RegistryRecord) bool {
	for _, r := range recs {
		if r.Tmux != "" {
			return true
		}
	}
	return false
}

// registryReport describes the registry source. The three counters are the ones
// §5.2 names, and they are reported together so any two can be reconciled: a
// file in the directory is a record, an ignored .key sibling, or a parse error.
func (o *Observer) registryReport(reg RegistryResult, err error, now time.Time) SourceReport {
	observedAt := reg.ObservedAt
	rep := SourceReport{
		ID:         SourceRegistry,
		Role:       "authoritative",
		Path:       o.paths.Registry,
		ObservedAt: &observedAt,
		AgeMs:      sourceAgeMs(now, &observedAt),
		OK:         err == nil,
		Stale:      false,
		Advisories: []Advisory{},
		Counters: map[string]int64{
			"records":         int64(len(reg.Records)),
			"parseErrors":     int64(reg.ParseErrors),
			"ignoredKeyFiles": int64(reg.IgnoredKeyFiles),
			"retriesDeferred": int64(reg.RetriesDeferred),
			"failures":        int64(len(reg.Failures)),
		},
		Labels: map[string]string{},
	}
	if reg.Absent {
		// A missing directory is reported as a fact, not a failure: ok stays
		// true and the record count is zero. §6.6's watch mode will say
		// "absent" for the same state once P5 owns the watch.
		rep.Labels["state"] = "absent"
	}
	if err != nil {
		msg := err.Error()
		rep.LastError = &msg
	}
	return rep
}

// tmuxReport describes the pane gate. Its role is "gate": it contributes no
// field to any session, only the second half of one liveness answer.
func (o *Observer) tmuxReport(in buildInput, now time.Time) SourceReport {
	observedAt := now
	rep := SourceReport{
		ID:         SourceTmux,
		Role:       "gate",
		ObservedAt: &observedAt,
		AgeMs:      sourceAgeMs(now, &observedAt),
		OK:         in.panesErr == nil,
		Advisories: []Advisory{},
		Counters:   map[string]int64{"panes": int64(len(in.panes))},
		Labels:     map[string]string{},
	}
	if in.panesErr != nil {
		msg := in.panesErr.Error()
		rep.LastError = &msg
	}
	return rep
}

// promoteSourceAdvisories copies every source-scoped advisory into the
// top-level warnings array, ONCE, in a deterministic order. Session-scoped
// advisories are never promoted — a consumer reading warnings is reading about
// the observer's own health, not about individual sessions.
func promoteSourceAdvisories(sources map[string]SourceReport) []Advisory {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := []Advisory{}
	for _, id := range ids {
		for _, a := range sources[id].Advisories {
			if scope, known := ScopeOf(a.Code); known && scope != ScopeSource {
				continue
			}
			out = appendAdvisory(out, a)
		}
	}
	return out
}

// countSessions tallies the set that is actually being returned.
//
// The invariant total == live + dead + unknown is what gives an
// unknown-liveness session a home in the accounting; without it such a session
// is silently dropped from every total, which is how it goes unnoticed.
// counts.attention deliberately counts `attention && live != false`, so a dead
// record's stale `blocked` never contributes while an unknown-liveness one
// does.
func countSessions(sessions []Session) Counts {
	c := NewCounts()
	c.Total = len(sessions)
	for _, s := range sessions {
		switch {
		case s.Live == nil:
			c.Unknown++
		case *s.Live:
			c.Live++
		default:
			c.Dead++
		}
		if s.Attention && !(s.Live != nil && !*s.Live) {
			c.Attention++
		}
		c.ByStatus[s.Status]++
		c.ByKind[s.Kind]++
	}
	return c
}

// readPanes is the pane gate, plus the foreign-server judge when one is wired
// (moox §4.5). The listing is ONE tmux exec either way: an adapter that can
// also return pane pids is asked for both at once. The membership read happens
// only after a listing that answered, because "not under any of our panes" is
// meaningless against a pane set we do not have.
func (o *Observer) readPanes(ctx context.Context, in *buildInput) {
	pl, ok := o.deps.Panes.(PanePIDLister)
	if !ok || o.deps.Members == nil {
		in.panes, in.panesErr = o.deps.Panes.ListPanes(ctx)
		return
	}
	var pids []int
	in.panes, pids, in.panesErr = pl.ListPanePIDs(ctx)
	if in.panesErr != nil {
		in.panes = nil
		return
	}
	if len(pids) == 0 {
		// A listing with panes but no pane pids cannot vouch for ANY
		// process; judging against an empty set would call every live
		// session foreign and hide the lot. No pids, no foreign rule.
		return
	}
	in.members, in.membersErr = o.deps.Members.Membership(ctx, pids)
}

// matchesFilter applies the consumer's narrowing. Empty slices and an empty
// prefix match everything; the one rule the zero Filter does apply is that a
// launched session (SpawnedBy set) is out of view until IncludeSpawned asks for
// it — see Filter.IncludeSpawned for why that rule lives here.
func matchesFilter(s Session, f Filter) bool {
	if s.SpawnedBy != nil && !f.IncludeSpawned {
		return false
	}
	// Another tmux server's session (moox §4.5) is in no view of this one:
	// tt is per server, and that server's own observer lists it.
	if s.Foreign {
		return false
	}
	if len(f.Kinds) > 0 && !containsKind(f.Kinds, s.Kind) {
		return false
	}
	if len(f.Statuses) > 0 && !containsStatus(f.Statuses, s.Status) {
		return false
	}
	if f.CWDPrefix != "" && !strings.HasPrefix(s.CWD, f.CWDPrefix) {
		return false
	}
	if f.WaitingOnly && !s.Attention {
		return false
	}
	return true
}

func containsKind(list []Kind, k Kind) bool {
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func containsStatus(list []Status, st Status) bool {
	for _, v := range list {
		if v == st {
			return true
		}
	}
	return false
}
