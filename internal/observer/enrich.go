package observer

import (
	"context"
	"errors"
	"time"
)

// This file is §7.3's ENRICHMENT half: Enrich, its cache, and the two locators
// it chooses between.
//
// # Why enrichment is a separate method and not a snapshot option
//
// The 200 KiB pass is the only expensive thing this observer does. At 37
// sessions a full pass is up to 7.4 MB of reads, against a 43 KB budget for a
// whole watch-driven rebuild (§6.2). So it is NEVER part of a rebuild — not
// once, not "only when something changed". A surface asks for it, for the
// sessions it is actually about to present, and pays for it in its own request.
//
// The consequence worth stating: `tt observe`'s event stream carries
// transcript: null unless a client negotiates enrichment (P7's M9), and that is
// correct rather than a gap. A colour that arrives 200 ms after a status change
// is not a status stream's business.
//
// # The two locators, and why the choice is not "try one, then the other"
//
//	interactive  ->  FindByGlob(sessionId)          measured fact 6
//	background   ->  OpenPath(record.linkScanPath)  §0.4a
//
// A background job's transcript is named by its own record. If that name is
// absent, refused or reaped, the answer is "no transcript" and the pass STOPS.
// It does not fall back to the glob. The dotfiles project slug alone holds 31
// unrelated transcripts, so a fallback would publish a neighbouring
// conversation's colour, error count and subagent count as this job's — a
// well-formed, confident, wrong answer that no consumer could detect. AC7's
// second clause is exactly this, and it is asserted by counting opens.

// EnrichCacheKey is §7.3's enrichment cache key: the session, the path, and the
// SIZE and MTIME of the file the facts were read from. It is agentbar's cache
// shape (§8.2), and each of the four parts earns its place:
//
//	SessionID   two sessions can share a path only through a bug, but the key
//	            is also how the cache is bounded — one entry per session.
//	Path        a resume repoints a job's transcript. Without the path, the
//	            previous conversation's facts answer for the new one.
//	SizeBytes   a transcript only ever grows, so a changed size means new
//	            records, which may include a new colour.
//	ModUnixMs   an edit in place that preserves the size still moves the mtime.
//
// Unlike the TIMING cache (which keys on the session and path only, because
// Invariant B's change predicates decide when to re-read), the enrichment cache
// DOES key on size and mtime: nothing else decides when to re-read it, and a
// request that arrives after an append should see the append.
type EnrichCacheKey struct {
	SessionID string
	Path      string
	SizeBytes int64
	ModUnixMs int64
}

// enrichCacheKeyFor builds the key for a located transcript.
func enrichCacheKeyFor(sessionID string, loc TranscriptLocation) EnrichCacheKey {
	return EnrichCacheKey{
		SessionID: sessionID,
		Path:      loc.Path,
		SizeBytes: loc.SizeBytes,
		ModUnixMs: loc.ModTime.UnixMilli(),
	}
}

// enrichEntry is one cached read: the key it was read under, and the facts.
//
// The cache is map[sessionID]enrichEntry rather than map[EnrichCacheKey]facts,
// and the difference is memory rather than semantics. Both miss on any change
// to path, size or mtime — an unchanged transcript is never re-read either way.
// But a map keyed by the whole tuple NEVER EVICTS: every append to every
// transcript adds a permanent entry, and `tt observe` (P7) is designed to run
// for weeks. One entry per session is bounded by the number of sessions the
// process has ever seen, which is a real-world 40.
type enrichEntry struct {
	key   EnrichCacheKey
	facts TranscriptFacts
	// timing is an interactive session's 64 KiB timing read, taken from the
	// same located file under the same key: a transcript whose size and mtime
	// have not moved has no newer timestamp either. timingOK is false until a
	// read has succeeded, so a failed read is retried rather than cached.
	timing   TimingFacts
	timingOK bool
}

// maxEnrichCacheEntries bounds the per-session cache. The live machine has 37
// sessions; a long-lived observer accumulates one entry per session it has ever
// seen, which includes every session that has since exited. 512 is ~14x the
// measured population, and reaching it drops the whole cache rather than
// evicting cleverly: an eviction policy is a thing to get wrong, and the cost
// of a cold cache is one re-read per session.
const maxEnrichCacheEntries = 512

// EnrichCacheSize is how many sessions the enrichment cache currently holds,
// for the doctor and for the test that asserts the bound is real.
func (o *Observer) EnrichCacheSize() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.enrichCache)
}

// ErrNoSnapshot is returned by Enrich when handed a nil snapshot. It is a
// programming error rather than a degraded mode — every data failure inside
// Enrich degrades to "no transcript for this session" and returns the snapshot.
var ErrNoSnapshot = errors.New("observer: enrich: nil snapshot")

// Enrich returns a NEW snapshot with transcript facts filled in for the named
// sessions, or for every session when ids is empty.
//
// It never mutates the snapshot it is given. Snapshots are published through an
// atomic pointer and read without a lock (§7.3), so a reader may be holding the
// input while this runs; the sessions slice, the sources map and the per-session
// Sources/Advisories slices are all copied before anything is appended to them.
// An in-place append onto a shared backing array is the kind of defect that
// shows up as a subscriber seeing a field it was never sent.
//
// It never reads the clock. The returned snapshot keeps the input's ObservedAt
// and every age derived from it, because a second clock read is how
// `statusAgeMs == observedAt - statusSince` stops holding — twice now in this
// build, once in a source report and once in a published bound.
//
// Errors: only ErrNoSnapshot. A refused path, an unreadable file, an ambiguous
// glob and a cancelled read all degrade to "this session has no transcript
// facts", counted in the source report and — where the design names one —
// carried as a session advisory. One malformed transcript must never blank the
// enrichment for the other 36 sessions.
func (o *Observer) Enrich(ctx context.Context, snap *Snapshot, ids []string) (*Snapshot, error) {
	if snap == nil {
		return nil, ErrNoSnapshot
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.enrichCache == nil {
		o.enrichCache = map[string]enrichEntry{}
	}
	if len(o.enrichCache) > maxEnrichCacheEntries {
		o.enrichCache = map[string]enrichEntry{}
	}

	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}

	out := *snap
	out.Sessions = make([]Session, len(snap.Sessions))
	copy(out.Sessions, snap.Sessions)
	out.Sources = make(map[string]SourceReport, len(snap.Sources)+1)
	for id, rep := range snap.Sources {
		out.Sources[id] = rep
	}

	var c enrichCounters
	for i := range out.Sessions {
		if len(wanted) > 0 && !wanted[out.Sessions[i].SessionID] {
			continue
		}
		if err := ctx.Err(); err != nil {
			// A cancelled request stops the pass and keeps what it has. The
			// sessions already enriched are not un-enriched: a deadline is a
			// bound on the work, not a reason to discard it.
			c.cancelled = true
			break
		}
		o.enrichOne(ctx, &out.Sessions[i], &c)
	}

	if c.lookups > 0 {
		out.Sources[SourceTranscripts] = o.enrichedTranscriptsReport(snap.Sources[SourceTranscripts], c, snap.ObservedAt)
		// Recomputed rather than copied: warnings are the promotion of the
		// source-scoped advisories, and the transcripts report is one of the
		// sources. Enrichment raises none today, so this is an identity —
		// deriving it anyway is what keeps it an identity if that changes.
		out.Warnings = promoteSourceAdvisories(out.Sources)
	}

	return &out, nil
}

// enrichCounters is what one enrichment pass did, for the source report. The
// counts reconcile: lookups == byGlob + byRecordPath, and each of those splits
// into located + missing + refused.
type enrichCounters struct {
	lookups      int
	byGlob       int
	byRecordPath int
	located      int
	globMisses   int
	pathMisses   int
	ambiguous    int
	refused      int
	reads        int
	cacheHits    int
	colorMisses  int
	// timingReads and timingCacheHits are the interactive half's 64 KiB
	// timing reads — published as enrichTimingReads/enrichTimingCacheHits,
	// PREFIXED like every other enrichment counter because the flat
	// timingReads already belongs to the background half's rebuild pass.
	timingReads     int
	timingCacheHits int
	cancelled       bool
	lastError       error
}

// enrichOne fills one session's transcript facts in place.
func (o *Observer) enrichOne(ctx context.Context, s *Session, c *enrichCounters) {
	loc, ok := o.locateForEnrichment(ctx, s, c)
	if !ok {
		return
	}

	// We looked. That is true whether or not we found anything, and it is the
	// distinction transcriptCollected exists to carry: "we did not look" and
	// "we looked and there was nothing" are different statements and a
	// consumer acts differently on each.
	s.TranscriptCollected = true

	if loc.Ambiguous {
		c.ambiguous++
		// The one case the design gives an advisory of its own, because it is
		// the one where a wrong answer was AVAILABLE and declined. matches[]
		// names the candidates so a human can see which two slug directories
		// collided.
		s.Advisories = appendAdvisory(copyAdvisories(s.Advisories),
			NewAdvisory(AdvTranscriptAmbiguous, map[string]any{"matches": append([]string(nil), loc.Matches...)}))
		return
	}
	if !loc.Found {
		// No advisory for an interactive miss: a session that has not written
		// a transcript yet is ordinary, and the background named-but-absent
		// case already carries transcript-absent from the timing pass (§3.4),
		// which is a liveness input rather than a missing enrichment.
		return
	}

	key := enrichCacheKeyFor(s.SessionID, loc)
	if entry, hit := o.enrichCache[s.SessionID]; hit && entry.key == key {
		c.cacheHits++
		c.located++
		o.applyFacts(s, entry.facts, c)
		o.applyInteractiveTiming(ctx, s, loc, &entry, c)
		o.enrichCache[s.SessionID] = entry
		return
	}

	facts, err := o.deps.Transcripts.ReadFacts(ctx, loc, o.factsTailBytes)
	if err != nil {
		// Degrade: this session has no transcript facts. The error is reported
		// on the source, once, rather than on 37 sessions.
		c.lastError = err
		return
	}
	c.reads++
	c.located++
	entry := enrichEntry{key: key, facts: facts}
	o.applyFacts(s, facts, c)
	o.applyInteractiveTiming(ctx, s, loc, &entry, c)
	o.enrichCache[s.SessionID] = entry
}

// applyInteractiveTiming fills lastActivityAt for a session whose transcript
// was found by GLOB — §3.4's registry row: "from the transcript found by glob,
// when enrichment ran" — using the same 64 KiB timing read, the same backwards
// scan and the same advisory the background half uses.
//
// FOUND BY THE INDEPENDENT BLACK-BOX SUITE. The field was defined for both
// populations and computed for one: every interactive session read
// lastActivityAt: null even with enrichment on and a current transcript
// (TestRegistryAgesIgnoreTranscriptMtime), and a tail with no timestamp in it
// nulled the field WITHOUT activity-since-truncated, which AC12 requires
// (TestTimestampBehindUntimestampedTailIsAdvised).
//
// It is an INDEPENDENT measurement and nothing else: statusSince is the
// registry's statusUpdatedAt verbatim and is never compared with, bounded by or
// "corrected" against this value (AC3). The cross-check and the lower bound are
// the background half's, where a record's own timestamp errs late.
//
// A background session is skipped: its lastActivityAt is Invariant B's, read by
// the rebuild from the record's own linkScanPath before publication.
func (o *Observer) applyInteractiveTiming(ctx context.Context, s *Session, loc TranscriptLocation, entry *enrichEntry, c *enrichCounters) {
	if s.Kind == KindBackground {
		return
	}
	if entry.timingOK {
		c.timingCacheHits++
	} else {
		t, err := o.deps.Transcripts.ReadTiming(ctx, loc, o.timingTailBytes)
		if err != nil {
			c.lastError = err
			return
		}
		c.timingReads++
		entry.timing, entry.timingOK = t, true
	}

	if t := entry.timing.LastActivityAt; t != nil {
		at := *t
		s.LastActivityAt = &at
		return
	}
	s.LastActivityAt = nil
	if !entry.timing.ReachedStart {
		// §3.4: the tail held no timestamped record and did not reach offset 0.
		// Null WITH the advisory (AC12), never an invented time.
		s.Advisories = appendAdvisory(copyAdvisories(s.Advisories),
			NewAdvisory(AdvActivitySinceTruncated, map[string]any{
				"tailBytes": int64(entry.timing.TailBytes),
			}))
	}
}

// locateForEnrichment picks the locator by KIND and reports whether the pass
// should continue.
//
// A background job's path comes from what the last rebuild read out of its
// record (the timing state's path), not from the session, because §5.4 keeps
// linkScanPath off the wire — it is a filesystem path into a tree the consumer
// has no business being handed. The second return is false when there was
// nothing to look up at all, which leaves transcriptCollected alone.
func (o *Observer) locateForEnrichment(ctx context.Context, s *Session, c *enrichCounters) (TranscriptLocation, bool) {
	if s.Kind == KindBackground {
		path := o.prev.jobs[s.SessionID].Path
		if path == "" {
			// The record names no transcript. NOT a glob fallback — see the
			// file comment.
			return TranscriptLocation{}, false
		}
		c.lookups++
		c.byRecordPath++
		loc, err := o.deps.Transcripts.OpenPath(ctx, path)
		if err != nil {
			c.refused++
			c.lastError = err
			return TranscriptLocation{}, true
		}
		if !loc.Found {
			c.pathMisses++
		}
		return loc, true
	}

	c.lookups++
	c.byGlob++
	loc, err := o.deps.Transcripts.FindByGlob(ctx, s.SessionID)
	if err != nil {
		c.refused++
		c.lastError = err
		return TranscriptLocation{}, true
	}
	if !loc.Found && !loc.Ambiguous {
		c.globMisses++
	}
	return loc, true
}

// applyFacts attaches the facts to the session.
func (o *Observer) applyFacts(s *Session, facts TranscriptFacts, c *enrichCounters) {
	f := facts
	s.Transcript = &f
	s.Sources = appendSource(copyStrings(s.Sources), SourceTranscript)
	if !facts.ColorFound {
		// Counted, not advised. "This session has no colour" is the ordinary
		// case — only 30 transcripts machine-wide carry an agent-color record
		// — and an advisory per colourless session would bury the real ones.
		// The count plus truncated says whether the tail was the reason.
		c.colorMisses++
	}
}

// copyStrings and copyAdvisories exist because Enrich must not append onto a
// slice the published snapshot still shares. append() would either allocate
// (fine) or write into the shared backing array (a subscriber seeing a field it
// was never sent), and which one happens depends on capacity — i.e. on
// history. Copying makes it not depend on anything.
func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyAdvisories(in []Advisory) []Advisory {
	if in == nil {
		return nil
	}
	out := make([]Advisory, len(in))
	copy(out, in)
	return out
}

// enrichedTranscriptsReport merges the enrichment counters into whatever the
// timing pass already reported for this source.
//
// The enrichment counters are PREFIXED (enrichLookups, enrichLocated, …) rather
// than sharing §5.2's flat names. §5.2's sample has one `lookups` and one
// `absent` for a source that now performs two passes over two different
// populations — 37 interactive sessions by glob, 7 background jobs by record
// path — and a reader of `absent: 2` could not tell which pass it described.
// The timing names are P3's and keep their meaning; nothing is renamed.
func (o *Observer) enrichedTranscriptsReport(prev SourceReport, c enrichCounters, observedAt time.Time) SourceReport {
	rep := prev
	if rep.ID == "" {
		// No timing pass ran (a machine with no background jobs), so there is
		// no report to merge into.
		rep = SourceReport{
			ID:         SourceTranscripts,
			Role:       "enrichment",
			Path:       o.paths.Projects,
			ObservedAt: &observedAt,
			AgeMs:      sourceAgeMs(observedAt, &observedAt),
			OK:         true,
			Advisories: []Advisory{},
			Counters:   map[string]int64{"tailBytesTiming": int64(o.timingTailBytes)},
			Labels:     map[string]string{},
		}
	}

	counters := make(map[string]int64, len(rep.Counters)+13)
	for k, v := range rep.Counters {
		counters[k] = v
	}
	counters["tailBytesColor"] = int64(o.factsTailBytes)
	counters["enrichLookups"] = int64(c.lookups)
	counters["enrichByGlob"] = int64(c.byGlob)
	counters["enrichByRecordPath"] = int64(c.byRecordPath)
	counters["enrichLocated"] = int64(c.located)
	counters["globMisses"] = int64(c.globMisses)
	counters["enrichPathMisses"] = int64(c.pathMisses)
	counters["ambiguous"] = int64(c.ambiguous)
	counters["enrichRefused"] = int64(c.refused)
	counters["factsReads"] = int64(c.reads)
	counters["factsCacheHits"] = int64(c.cacheHits)
	counters["colorMisses"] = int64(c.colorMisses)
	counters["enrichTimingReads"] = int64(c.timingReads)
	counters["enrichTimingCacheHits"] = int64(c.timingCacheHits)
	rep.Counters = counters

	labels := make(map[string]string, len(rep.Labels)+1)
	for k, v := range rep.Labels {
		labels[k] = v
	}
	// The label says which passes ran, because the counter set alone does not:
	// a consumer seeing timingReads 0 cannot tell "no job needed one" from
	// "the timing pass did not run".
	switch labels["pass"] {
	case "timing":
		labels["pass"] = "timing+enrichment"
	default:
		labels["pass"] = "enrichment"
	}
	if c.cancelled {
		// A partial pass is an answer, and the consumer is told it was partial
		// rather than left to infer it from arithmetic.
		labels["enrich"] = "cancelled"
	}
	rep.Labels = labels

	if c.lastError != nil {
		msg := c.lastError.Error()
		rep.LastError = &msg
		rep.OK = false
	}
	return rep
}
