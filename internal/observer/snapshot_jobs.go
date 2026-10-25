package observer

import (
	"context"
	"time"
)

// This file is the I/O half of the background population: the jobs read, the
// timing tails Invariant B selects, and the two source reports they produce.
//
// It is separate from join_jobs.go on purpose. Everything here touches ports;
// everything there is a pure function of what this produced. That is what lets
// all five of Invariant B's predicates and all seven of §3.5's rules be tested
// without a filesystem, while the ORDERING obligation — arm, then read, then
// publish — has exactly one home.

// transcriptCounters is what the transcripts source reports about one rebuild.
// The four counts reconcile: lookups == byLinkScanPath, and byLinkScanPath ==
// located + absent + refused.
type transcriptCounters struct {
	lookups         int
	byLinkScanPath  int
	located         int
	absent          int
	refused         int
	timingReads     int
	timingCacheHits int
	lastError       error
}

// readJobTiming performs the timing reads for one rebuild and returns what the
// join needs, the cache to carry forward, and the accounting.
//
// THE ORDER HERE IS THE POINT, and it is round 3's CRITICAL 2:
//
//	arm the target  ->  read its tail  ->  publish
//
// never read-then-arm. §6.3's Invariant A owns the arm (P5); this function owns
// the middle step and runs entirely BEFORE the snapshot is built, so a rebuild
// publishes lastActivityAt on the FIRST frame a new job appears in rather than
// on some later one. Under a read-on-notification rule that first frame carries
// null forever for a job whose transcript was complete before its record
// existed — and if the job is `blocked`, nothing ever appends again, so
// "forever" is literal.
//
// A failure anywhere in here degrades to "no transcript for this job". It never
// fails the snapshot: a record is untrusted input, and a refused path or an
// unreadable file must not be able to blank the background population.
func (o *Observer) readJobTiming(
	ctx context.Context,
	jobs JobsResult,
	prev previousState,
	changedPaths map[string]bool,
) (map[string]jobTiming, map[TimingCacheKey]TimingFacts, transcriptCounters) {
	out := make(map[string]jobTiming, len(jobs.Records))
	cache := make(map[TimingCacheKey]TimingFacts, len(jobs.Records))
	var counters transcriptCounters

	for _, rec := range jobs.Records {
		if rec.LinkScanPath == "" {
			// 0 of 7 measured records look like this — linkScanPath is
			// universal — but a record that does not name a transcript is a
			// legible state, not a failure, and it must not become one.
			out[rec.SessionID] = jobTiming{}
			continue
		}

		t := jobTiming{Named: true, Path: rec.LinkScanPath}
		counters.lookups++
		counters.byLinkScanPath++

		loc, err := o.deps.Transcripts.OpenPath(ctx, rec.LinkScanPath)
		switch {
		case err != nil:
			// Either a refused path (outside the projects root) or a read
			// error. Both are "we cannot see a transcript here", which is what
			// §3.5 rule 4 reads, and neither is a reason to fail.
			t.Err = err
			counters.refused++
			counters.lastError = err
		case !loc.Found:
			counters.absent++
		default:
			t.Located = true
			t.Path = loc.Path
			counters.located++

			cur := TimingInputs{
				SessionID: rec.SessionID,
				Path:      rec.LinkScanPath,
				UpdatedAt: rec.UpdatedAt,
			}
			cur.Status, _ = MapJobState(rec.State)

			var prevState *TimingState
			if ps, ok := prev.jobs[rec.SessionID]; ok {
				prevState = &ps
			}
			t.Trigger = TimingTriggerFor(cur, prevState, changedPaths)

			key := TimingCacheKey{SessionID: rec.SessionID, Path: loc.Path}
			if t.Trigger == TimingTriggerNone {
				if cached, ok := prev.timing[key]; ok {
					t.Facts = cached
					counters.timingCacheHits++
					out[rec.SessionID] = t
					cache[key] = cached
					continue
				}
				// No cached entry despite no change predicate firing. That can
				// only happen if a previous read failed, so read rather than
				// publish a null we already know how to fill.
				t.Trigger = TimingTriggerNewSession
			}

			facts, err := o.deps.Transcripts.ReadTiming(ctx, loc, o.timingTailBytes)
			if err != nil {
				t.Err = err
				counters.lastError = err
			} else {
				t.Facts = facts
				cache[key] = facts
			}
			counters.timingReads++
		}

		out[rec.SessionID] = t
	}

	return out, cache, counters
}

// unreadJobTiming is readJobTiming's --no-transcript twin: one entry per job
// saying the transcript was NOT looked at, and not one byte read — not even the
// stat OpenPath performs. §5.2's flag contract, which the black-box suite held
// the build to, is that the flag makes transcriptCollected false on EVERY
// session; P3's timing read, a correctness input, had kept it true on every job
// that named a transcript.
//
// The cache it returns is empty on purpose. Carrying facts forward that this
// rebuild did not read would publish them as if it had; the next rebuild that
// DOES read finds a timing state with no cached facts and reads, which is
// readJobTiming's existing "no cached entry" rule.
func unreadJobTiming(jobs JobsResult) (map[string]jobTiming, map[TimingCacheKey]TimingFacts) {
	out := make(map[string]jobTiming, len(jobs.Records))
	for _, rec := range jobs.Records {
		out[rec.SessionID] = jobTiming{
			Named:  rec.LinkScanPath != "",
			Path:   rec.LinkScanPath,
			Unread: true,
		}
	}
	return out, map[TimingCacheKey]TimingFacts{}
}

// jobsReport describes the jobs source. The four counts are the ones §5.2 names
// and they are reported TOGETHER because that is what makes them checkable:
// entries is every direct entry under jobs/ (15 on this machine, pins.json
// included), dirs is the subset that are directories (14), records is the
// subset carrying a depth-1 state.json (7), and nonTerminal is the subset whose
// state is neither done nor failed (3). Conflating the first two is what put
// the wrong number in measured fact 13.
func (o *Observer) jobsReport(res JobsResult, err error, now time.Time) SourceReport {
	observedAt := res.ObservedAt
	rep := SourceReport{
		ID:         SourceJobs,
		Role:       "authoritative",
		Path:       o.paths.Jobs,
		ObservedAt: &observedAt,
		AgeMs:      sourceAgeMs(now, &observedAt),
		OK:         err == nil,
		Stale:      false,
		Advisories: []Advisory{},
		Counters: map[string]int64{
			"entries":     int64(res.Entries),
			"dirs":        int64(res.Dirs),
			"records":     int64(len(res.Records)),
			"nonTerminal": int64(res.NonTerminal),
			"parseErrors": int64(res.ParseErrors),
			"failures":    int64(len(res.Failures)),
		},
		Labels: map[string]string{},
	}
	if res.Absent {
		// A machine that has never run a background agent. A fact, not a
		// failure: ok stays true and every count is zero.
		rep.Labels["state"] = "absent"
	}
	if err != nil {
		msg := err.Error()
		rep.LastError = &msg
	}
	return rep
}

// transcriptsReport describes the transcript source for the reads P3 actually
// performs — the 64 KiB timing pass.
//
// Both tail sizes are published even though only one has run, because a
// consumer diagnosing "why is lastActivityAt null" needs to know which bound
// was in force, and because §8.3 item 9 requires the two constants to be
// visible rather than buried. The colour-pass counters (colorMisses, globMisses,
// ambiguous) are P6's and are absent here rather than reported as zero: a zero
// would claim a pass ran and found nothing.
func (o *Observer) transcriptsReport(c transcriptCounters, now time.Time) SourceReport {
	observedAt := now
	rep := SourceReport{
		ID:         SourceTranscripts,
		Role:       "enrichment",
		Path:       o.paths.Projects,
		ObservedAt: &observedAt,
		AgeMs:      sourceAgeMs(now, &observedAt),
		OK:         c.lastError == nil,
		Advisories: []Advisory{},
		Counters: map[string]int64{
			"lookups":         int64(c.lookups),
			"byLinkScanPath":  int64(c.byLinkScanPath),
			"located":         int64(c.located),
			"absent":          int64(c.absent),
			"refused":         int64(c.refused),
			"timingReads":     int64(c.timingReads),
			"timingCacheHits": int64(c.timingCacheHits),
			"tailBytesTiming": int64(o.timingTailBytes),
		},
		Labels: map[string]string{"pass": "timing"},
	}
	if c.lastError != nil {
		msg := c.lastError.Error()
		rep.LastError = &msg
	}
	return rep
}

// sourceAgeMs is AgeMs floored at zero, for source reports only.
//
// An adapter stamps its result with its OWN clock read, which happens after the
// rebuild's single `now` — so on a fast read the difference is negative by the
// read latency, and the jobs tree (14 directory entries, 7 small files) is fast
// enough to produce it: the live machine reported ageMs -1.
//
// A source observed in the future is not a fact about anything, and it makes a
// consumer's `ageMs > staleAfterMs` comparison read as absurd. The floor is
// applied HERE, to source ages only, and never to AgeMs itself: a SESSION
// timestamp in the future is a real signal about the vendor's data and must
// survive to be seen.
func sourceAgeMs(now time.Time, at *time.Time) *int64 {
	ms := AgeMs(now, at)
	if ms != nil && *ms < 0 {
		zero := int64(0)
		return &zero
	}
	return ms
}
