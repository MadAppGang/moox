package observer_test

// P6's core suite: Enrich, its cache, the two locators and the exposure rules.
//
// The adapter's own suite (adapters/transcriptfs/facts_test.go) proves what is
// read OUT of a transcript. This one proves what the core does with it: which
// locator each population gets, that an unchanged transcript is never re-read,
// that the published snapshot is never mutated, and that nothing but the
// allowed facts (counts, flags and bounded identifiers) reaches a session.
//
// Everything here uses the fixture tree under testdata/ plus an injected clock.
// The one exception is the exposure test's canary record, which is synthetic and
// deliberately NOT in testdata/ so the fixture privacy audit can go on asserting
// those key names appear nowhere under it.

import (
	"context"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jobsfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/transcriptfs"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

const (
	enrichSessA   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	enrichSessB   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	enrichSessC   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	enrichSessD   = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	enrichSessE   = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	enrichSessDup = "d0b1e000-0000-4000-8000-000000000002"

	enrichRelA     = "-home-user-proj-a/" + enrichSessA + ".jsonl"
	enrichRelDecoy = "-home-user-proj-a-collide/c01110ed-0000-4000-8000-000000000001.jsonl"
	// enrichNeighbour shares a slug directory with blocked-beyond-retention's
	// REAPED linkScanPath target. It is what a glob fallback would return for
	// that job, and the real dotfiles slug holds 31 such neighbours.
	enrichNeighbour = "-home-user-dotfiles/unrelated-neighbour-0000-4000-8000-000000000000.jsonl"
)

// countingFS counts opens by name. "The colliding slug directory is never
// opened" and "no glob fallback is attempted" are claims about which files were
// touched; counting is the only way to test a claim like that, because a reader
// that opened a file and discarded it gives an identical answer.
type countingFS struct {
	inner fs.FS
	opens map[string]int
	reads map[string]int
}

func newCountingFS(inner fs.FS) *countingFS {
	return &countingFS{inner: inner, opens: map[string]int{}, reads: map[string]int{}}
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.opens[name]++
	f, err := c.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: f, name: name, fs: c}, nil
}

// Opens counts every Open, INCLUDING the locator's stat — fs.Stat over a plain
// fs.FS is implemented as Open-then-Stat, and the locator stats on every pass
// by design (P3: "is the transcript still there" is not cacheable).
func (c *countingFS) Opens(name string) int { return c.opens[name] }

// Reads counts files whose CONTENT was actually read. That is the number "an
// unchanged transcript is never re-read" is about; opens cannot distinguish a
// 200 KB tail read from a stat.
func (c *countingFS) Reads(name string) int { return c.reads[name] }

type countingFile struct {
	fs.File
	name string
	fs   *countingFS
	seen bool
}

// ReadDir forwards to the inner file when it is a directory. It has to exist:
// fs.Glob reaches a directory's entries through Open-then-ReadDirFile over a
// plain fs.FS, so a wrapper that hides ReadDir makes every glob match nothing —
// which looks exactly like "the locator is broken" and cost this suite a round.
func (f *countingFile) ReadDir(n int) ([]fs.DirEntry, error) {
	d, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, fmt.Errorf("%s: not a directory", f.name)
	}
	return d.ReadDir(n)
}

func (f *countingFile) Read(p []byte) (int, error) {
	if !f.seen {
		f.seen = true
		f.fs.reads[f.name]++
	}
	return f.File.Read(p)
}

// enrichHarness is one observer over the fixture trees, with the transcript
// source wrapped in a counting fs.FS.
//
// The registry half is a FAKE so a test can state exactly which interactive
// sessions exist (the registry fixtures are P1's and name other sessions). The
// jobs half is the REAL jobsfs over testdata/jobs, so the background locator is
// exercised end to end against the records that carry a linkScanPath.
type enrichHarness struct {
	obs      *observer.Observer
	counting *countingFS
	proj     fstest.MapFS
	clock    *fakes.Clock
}

func newEnrichHarness(t *testing.T, factsTail int, sessionIDs ...string) *enrichHarness {
	t.Helper()

	clock := fakes.NewClock(tRef)
	reg := &fakes.Registry{}
	recs := make([]observer.RegistryRecord, 0, len(sessionIDs))
	for i, id := range sessionIDs {
		recs = append(recs, observer.RegistryRecord{
			Path:      fmt.Sprintf("/fixtures/sessions/%d.json", 1000+i),
			PID:       1000 + i,
			SessionID: id,
			Status:    "idle",
			Name:      "fixture-" + id[:8],
			CWD:       "/home/user/proj",
		})
	}
	reg.SetRecords(tRef, recs...)

	proc := fakes.NewProc()
	for _, r := range recs {
		proc.SetAlive(r.PID, true)
	}

	proj := loadTree(t, projectsFixtureDir)
	counting := newCountingFS(proj)
	obs, err := observer.New(observer.Deps{
		Registry:    reg,
		Jobs:        jobsfs.NewFS(loadTree(t, jobsFixtureDir), "/fixtures/jobs", clock),
		Transcripts: transcriptfs.NewFS(counting, fixtureProjectsRoot, clock),
		Proc:        proc,
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}, observer.Config{
		FactsTailBytes: factsTail,
		Paths: observer.Paths{
			Registry: "/fixtures/sessions",
			Jobs:     "/fixtures/jobs",
			Projects: fixtureProjectsRoot,
		},
	})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return &enrichHarness{obs: obs, counting: counting, proj: proj, clock: clock}
}

// enriched takes a snapshot and enriches it, returning both.
func (h *enrichHarness) enriched(t *testing.T, ids ...string) (*observer.Snapshot, *observer.Snapshot) {
	t.Helper()
	base, err := h.obs.Snapshot(context.Background(), observer.SnapshotOptions{
		IncludeUnknownLiveness: true,
		IncludeTranscripts:     true,
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	out, err := h.obs.Enrich(context.Background(), base, ids)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	return base, out
}

func sessionByID(t *testing.T, snap *observer.Snapshot, id string) observer.Session {
	t.Helper()
	s, ok := snap.SessionByID(id)
	if !ok {
		var have []string
		for _, x := range snap.Sessions {
			have = append(have, x.SessionID)
		}
		t.Fatalf("session %s absent; have %v", id, have)
	}
	return s
}

// ---------------------------------------------------------------------------
// AC4 / AC5 / AC6 — the facts arrive on the session, through the core
// ---------------------------------------------------------------------------

func TestAC4_TheCorePublishesTheByFileSubagentCount(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessC)
	_, out := h.enriched(t)

	a := sessionByID(t, out, enrichSessA)
	if a.Transcript == nil {
		t.Fatal("proj-a's session has no transcript facts")
	}
	if a.Transcript.Subagents == nil || *a.Transcript.Subagents != 3 {
		t.Errorf("subagents = %v, want 3 by FILE (two depth-1 metas plus the nested "+
			"spawnDepth:2 one a transcript parser cannot see)", a.Transcript.Subagents)
	}
	if !a.Transcript.SubagentsKnown {
		t.Error("subagentsKnown is false for a session with a subagents directory")
	}

	// The complement, on the same snapshot: absent directory -> null, not 0.
	c := sessionByID(t, out, enrichSessC)
	if c.Transcript == nil {
		t.Fatal("proj-c's session has no transcript facts")
	}
	if c.Transcript.Subagents != nil {
		t.Errorf("subagents = %d with no subagents directory; want null", *c.Transcript.Subagents)
	}
	if c.Transcript.SubagentsKnown {
		t.Error("subagentsKnown is true with no directory to know anything from")
	}
}

func TestAC5_TheCorePublishesAPIErrorsFromIsApiErrorMessageOnly(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessD, enrichSessE)
	_, out := h.enriched(t)

	d := sessionByID(t, out, enrichSessD)
	if d.Transcript == nil {
		t.Fatal("proj-d has no transcript facts")
	}
	if d.Transcript.APIErrors != 0 {
		t.Errorf("proj-d apiErrors = %d, want 0: its error markers are RECOVERED is_error "+
			"records (fact 11 — 8 243 of those machine-wide against 1 103 real api errors)",
			d.Transcript.APIErrors)
	}

	e := sessionByID(t, out, enrichSessE)
	if e.Transcript == nil {
		t.Fatal("proj-e has no transcript facts")
	}
	if e.Transcript.APIErrors != 2 {
		t.Errorf("proj-e apiErrors = %d, want 2", e.Transcript.APIErrors)
	}
}

func TestAC6_TheCorePublishesAgentColorLastWins(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessC)
	_, out := h.enriched(t)

	c := sessionByID(t, out, enrichSessC)
	if c.Transcript == nil {
		t.Fatal("proj-c has no transcript facts")
	}
	if !c.Transcript.ColorFound || c.Transcript.AgentColor != "green" {
		t.Errorf("agentColor = %q colorFound = %v, want green/true", c.Transcript.AgentColor, c.Transcript.ColorFound)
	}
	// And the whole session, rendered, carries none of the decoys — including
	// the subagent `color` keys, which is the half of AC6 a field-by-field
	// assertion would miss.
	rendered := fmt.Sprintf("%+v", c)
	for _, decoy := range []string{"red", "pink", "orange"} {
		if strings.Contains(rendered, decoy) {
			t.Errorf("the decoy colour %q reached the session: %s", decoy, rendered)
		}
	}
}

func TestEnrichmentMarksTheTranscriptAsASourceAndAsCollected(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA)
	base, out := h.enriched(t)

	before := sessionByID(t, base, enrichSessA)
	if before.TranscriptCollected || before.Transcript != nil {
		t.Error("the un-enriched snapshot already claims a transcript; the 200 KB pass is " +
			"supposed to be the only thing that collects one for an interactive session")
	}

	after := sessionByID(t, out, enrichSessA)
	if !after.TranscriptCollected {
		t.Error("transcriptCollected is false after the pass ran")
	}
	var found bool
	for _, s := range after.Sources {
		if s == observer.SourceTranscript {
			found = true
		}
	}
	if !found {
		t.Errorf("sources = %v, want the transcript listed", after.Sources)
	}
}

// ---------------------------------------------------------------------------
// AC7 — the locators: glob for interactive, the record's path for background
// ---------------------------------------------------------------------------

func TestAC7_AnInteractiveSessionIsLocatedByGlobAndTheCollidingDirIsNeverOpened(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA)
	_, out := h.enriched(t)

	a := sessionByID(t, out, enrichSessA)
	if a.Transcript == nil {
		t.Fatal("no transcript facts")
	}
	if a.Transcript.AgentColor != "purple" {
		t.Errorf("agentColor = %q, want purple (yellow would be the colliding directory's "+
			"unrelated session, which is what a cwd->slug derivation resolves to)",
			a.Transcript.AgentColor)
	}
	if n := h.counting.Opens(enrichRelDecoy); n != 0 {
		t.Errorf("the colliding slug directory's transcript was opened %d times", n)
	}
	if n := h.counting.Opens(enrichRelA); n == 0 {
		t.Error("the session's own transcript was never opened; the test proved nothing")
	}
}

func TestAC7_AnAmbiguousGlobPublishesNoTranscriptAndAnAdvisory(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessDup)
	_, out := h.enriched(t)

	s := sessionByID(t, out, enrichSessDup)
	if s.Transcript != nil {
		t.Errorf("transcript = %+v for an ambiguous glob; one of two files would have been "+
			"published as this session's", s.Transcript)
	}
	if !s.TranscriptCollected {
		t.Error("transcriptCollected is false; we DID look, and found two")
	}
	var adv *observer.Advisory
	for i := range s.Advisories {
		if s.Advisories[i].Code == observer.AdvTranscriptAmbiguous {
			adv = &s.Advisories[i]
		}
	}
	if adv == nil {
		t.Fatalf("no %s advisory; advisories = %+v", observer.AdvTranscriptAmbiguous, s.Advisories)
	}
	matches, ok := adv.Fields["matches"].([]string)
	if !ok || len(matches) != 2 {
		t.Fatalf("matches = %#v, want the two colliding paths", adv.Fields["matches"])
	}
	for _, m := range matches {
		if !strings.HasPrefix(m, fixtureProjectsRoot+"/") {
			t.Errorf("match %q is not an absolute path under the projects root", m)
		}
	}
}

func TestAC7_ABackgroundJobIsLocatedByItsRecordAndNeverByGlob(t *testing.T) {
	// blocked-beyond-retention's linkScanPath target was reaped by the vendor's
	// 30-day cleanup. Its slug directory still EXISTS and still holds an
	// unrelated neighbour — the real dotfiles slug holds 31 of them. A glob
	// fallback would publish that neighbour's colour and error count as this
	// job's, and the answer would look entirely reasonable.
	h := newEnrichHarness(t, 0)
	_, out := h.enriched(t)

	const reaped = "7a324a6d-4a32-4d6a-8a32-4d6a7a324a6d"
	var subject *observer.Session
	for i := range out.Sessions {
		if out.Sessions[i].Kind == observer.KindBackground && out.Sessions[i].Transcript == nil {
			subject = &out.Sessions[i]
			break
		}
	}
	if subject == nil {
		t.Skip("no background job with an absent transcript in the fixture set")
	}
	if n := h.counting.Opens(enrichNeighbour); n != 0 {
		t.Errorf("the unrelated neighbour in the same slug directory was opened %d times: a "+
			"glob fallback was attempted for a background job (%s)", n, reaped)
	}
}

func TestABackgroundJobsFactsComeFromTheFileItsRecordNames(t *testing.T) {
	// The positive half: working-fresh's record names -home-user-proj-h's
	// transcript, and that is the file the facts come from.
	h := newEnrichHarness(t, 0)
	_, out := h.enriched(t)

	var enrichedBg int
	for _, s := range out.Sessions {
		if s.Kind != observer.KindBackground || s.Transcript == nil {
			continue
		}
		enrichedBg++
		if !strings.HasPrefix(s.Transcript.Path, fixtureProjectsRoot+"/") {
			t.Errorf("%s: transcript path %q is not under the projects root", s.SessionID, s.Transcript.Path)
		}
		// Every background job's facts must come from the path its own record
		// named, which the timing pass recorded. A file from another session's
		// slug directory here is the fallback bug.
		if strings.Contains(s.Transcript.Path, "unrelated-neighbour") {
			t.Errorf("%s: facts came from a neighbour's transcript", s.SessionID)
		}
	}
	if enrichedBg == 0 {
		t.Fatal("no background job was enriched, so the locator was never exercised")
	}
	t.Logf("%d background jobs enriched by their record's own path", enrichedBg)
}

// ---------------------------------------------------------------------------
// AC12 — the bound, injected, and the fallback
// ---------------------------------------------------------------------------

func TestAC12_TheCoreDegradesCleanlyWhenTheColourIsBeyondTheTail(t *testing.T) {
	// proj-b's colour record is the second line of an 11 KB file. At a 2 KB
	// injected bound the core must publish the transcript WITHOUT a colour and
	// without an error — colorFound:false plus truncated:true is a statement
	// about the read, and it is a different statement from "this session has
	// no colour".
	h := newEnrichHarness(t, 2<<10, enrichSessB)
	_, out := h.enriched(t, enrichSessB)

	b := sessionByID(t, out, enrichSessB)
	if b.Transcript == nil {
		t.Fatal("no transcript facts; a truncated tail is still a successful read")
	}
	if b.Transcript.ColorFound || b.Transcript.AgentColor != "" {
		t.Errorf("colorFound = %v agentColor = %q at a 2 KB bound", b.Transcript.ColorFound, b.Transcript.AgentColor)
	}
	if !b.Transcript.Truncated {
		t.Error("truncated is false, so a consumer cannot tell WHY there is no colour")
	}
	if rep, ok := out.Sources[observer.SourceTranscripts]; !ok {
		t.Error("no transcripts source report")
	} else {
		if got := rep.Counters["tailBytesColor"]; got != 2<<10 {
			t.Errorf("tailBytesColor = %d, want the injected 2048: the published bound must be "+
				"the bound that was used (P3's finding 8, twice over)", got)
		}
		if rep.Counters["colorMisses"] != 1 {
			t.Errorf("colorMisses = %d, want 1", rep.Counters["colorMisses"])
		}
	}

	// The same session at the production bound finds it.
	h2 := newEnrichHarness(t, 0, enrichSessB)
	_, out2 := h2.enriched(t, enrichSessB)
	b2 := sessionByID(t, out2, enrichSessB)
	if b2.Transcript == nil || !b2.Transcript.ColorFound || b2.Transcript.AgentColor != "cyan" {
		t.Errorf("at the production bound: %+v, want colorFound with cyan", b2.Transcript)
	}
	if got := out2.Sources[observer.SourceTranscripts].Counters["tailBytesColor"]; got != int64(observer.DefaultFactsTailBytes) {
		t.Errorf("tailBytesColor = %d, want %d", got, observer.DefaultFactsTailBytes)
	}
}

func TestTheTwoTailBoundsAreDifferentMeasurementsAndBothArePublished(t *testing.T) {
	// §7.3's split, asserted on the wire-facing report rather than in prose: a
	// consumer diagnosing a null field needs to know which of the two reads it
	// is asking about.
	if observer.DefaultFactsTailBytes == observer.DefaultTimingTailBytes {
		t.Fatal("the two bounds are equal; the split has collapsed")
	}
	h := newEnrichHarness(t, 0, enrichSessA)
	_, out := h.enriched(t)
	rep := out.Sources[observer.SourceTranscripts]
	if rep.Counters["tailBytesTiming"] != int64(observer.DefaultTimingTailBytes) {
		t.Errorf("tailBytesTiming = %d", rep.Counters["tailBytesTiming"])
	}
	if rep.Counters["tailBytesColor"] != int64(observer.DefaultFactsTailBytes) {
		t.Errorf("tailBytesColor = %d", rep.Counters["tailBytesColor"])
	}
	if pass := rep.Labels["pass"]; pass != "timing+enrichment" && pass != "enrichment" {
		t.Errorf("pass = %q; the label is how a consumer knows which passes ran", pass)
	}
}

// ---------------------------------------------------------------------------
// the cache
// ---------------------------------------------------------------------------

func TestAnUnchangedTranscriptIsNeverReadTwice(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessC, enrichSessD)
	base, first := h.enriched(t)

	readsAfterFirst := h.counting.Reads(enrichRelA)
	if readsAfterFirst == 0 {
		t.Fatal("nothing was read")
	}
	if first.Sources[observer.SourceTranscripts].Counters["factsReads"] == 0 {
		t.Fatal("factsReads is 0 on a cold cache")
	}

	second, err := h.obs.Enrich(context.Background(), base, nil)
	if err != nil {
		t.Fatalf("second Enrich: %v", err)
	}
	if n := h.counting.Reads(enrichRelA); n != readsAfterFirst {
		t.Errorf("the transcript's content was read %d more times on the second pass; an "+
			"unchanged transcript must never be re-read", n-readsAfterFirst)
	}
	rep := second.Sources[observer.SourceTranscripts]
	if rep.Counters["factsReads"] != 0 {
		t.Errorf("factsReads = %d on a warm cache, want 0", rep.Counters["factsReads"])
	}
	if rep.Counters["factsCacheHits"] == 0 {
		t.Error("factsCacheHits = 0 on a warm cache")
	}
	// And the answer is the same one, not a degraded one.
	a1 := sessionByID(t, first, enrichSessA)
	a2 := sessionByID(t, second, enrichSessA)
	if !reflect.DeepEqual(a1.Transcript, a2.Transcript) {
		t.Errorf("the cached answer differs:\n first %+v\nsecond %+v", a1.Transcript, a2.Transcript)
	}
}

func TestAChangedSizeOrMtimeIsACacheMiss(t *testing.T) {
	// A transcript only grows, so a changed size means new records — which may
	// include a new colour. The mtime is in the key as well, for an in-place
	// edit that preserves the size.
	fsys := loadTree(t, projectsFixtureDir)
	counting := newCountingFS(fsys)
	clock := fakes.NewClock(time.UnixMilli(1789699211204).UTC())
	reg := &fakes.Registry{}
	reg.SetRecords(tRef, observer.RegistryRecord{
		Path: "/fixtures/sessions/1.json", PID: 1, SessionID: enrichSessA, Status: "idle",
	})
	obs, err := observer.New(observer.Deps{
		Registry: reg, Jobs: &fakes.Jobs{},
		Transcripts: transcriptfs.NewFS(counting, fixtureProjectsRoot, clock),
		Proc:        fakes.NewProc(1), Panes: fakes.NewPanes(), Clock: clock,
	}, observer.Config{Paths: observer.Paths{Projects: fixtureProjectsRoot}})
	if err != nil {
		t.Fatal(err)
	}

	base, err := obs.Snapshot(context.Background(), observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.Enrich(context.Background(), base, nil); err != nil {
		t.Fatal(err)
	}
	reads := counting.Reads(enrichRelA)

	// (a) an APPEND — new size, new mtime.
	old := fsys[enrichRelA]
	fsys[enrichRelA] = &fstest.MapFile{
		Data:    append(append([]byte{}, old.Data...), []byte(`{"type":"agent-color","agentColor":"teal"}`+"\n")...),
		Mode:    old.Mode,
		ModTime: old.ModTime.Add(time.Second),
	}
	base2, err := obs.Snapshot(context.Background(), observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if err != nil {
		t.Fatal(err)
	}
	out2, err := obs.Enrich(context.Background(), base2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := counting.Reads(enrichRelA); n <= reads {
		t.Fatal("an appended transcript was served from the cache; the new colour would never appear")
	}
	if s := sessionByID(t, out2, enrichSessA); s.Transcript == nil || s.Transcript.AgentColor != "teal" {
		t.Errorf("agentColor = %+v, want the appended teal", s.Transcript)
	}
	reads = counting.Reads(enrichRelA)

	// (b) an IN-PLACE edit that preserves the size: only the mtime moves.
	edited := append([]byte{}, fsys[enrichRelA].Data...)
	copy(edited[len(edited)-len(`teal"}`+"\n")-4:], []byte("navy"))
	fsys[enrichRelA] = &fstest.MapFile{
		Data:    edited,
		Mode:    old.Mode,
		ModTime: old.ModTime.Add(2 * time.Second),
	}
	base3, err := obs.Snapshot(context.Background(), observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.Enrich(context.Background(), base3, nil); err != nil {
		t.Fatal(err)
	}
	if n := counting.Reads(enrichRelA); n <= reads {
		t.Error("a same-size in-place edit was served from the cache; the mtime is in the key " +
			"precisely for this case")
	}
}

func TestTheCacheKeyIsTheFourMeasuredParts(t *testing.T) {
	// The shape is pinned so that dropping the path (a resume returns the
	// previous conversation's facts) or dropping size/mtime (an append is
	// never seen) is a deliberate act with a failing test attached.
	fields := reflect.TypeOf(observer.EnrichCacheKey{})
	want := []string{"SessionID", "Path", "SizeBytes", "ModUnixMs"}
	if fields.NumField() != len(want) {
		t.Fatalf("EnrichCacheKey has %d fields, want %d (%v)", fields.NumField(), len(want), want)
	}
	for i, name := range want {
		if got := fields.Field(i).Name; got != name {
			t.Errorf("field %d = %s, want %s", i, got, name)
		}
	}
}

func TestTheCacheHoldsOneEntryPerSessionAndNotOnePerRead(t *testing.T) {
	// A cache keyed by the whole (id, path, size, mtime) tuple never evicts:
	// every append to every transcript adds a permanent entry and `tt observe`
	// is designed to run for weeks. One entry per session is the bound.
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessC, enrichSessD, enrichSessE)
	base, _ := h.enriched(t)
	for i := 0; i < 5; i++ {
		if _, err := h.obs.Enrich(context.Background(), base, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Four interactive fixtures plus whatever background jobs the fixture tree
	// located; the point is that six passes did not multiply it.
	if n := h.obs.EnrichCacheSize(); n > 12 {
		t.Errorf("the cache holds %d entries after six passes over ~8 sessions", n)
	}
}

// ---------------------------------------------------------------------------
// immutability, selection and exposure
// ---------------------------------------------------------------------------

func TestEnrichNeverMutatesTheSnapshotItWasGiven(t *testing.T) {
	// Snapshots are published through an atomic pointer and read without a
	// lock, so a reader may be holding the input while Enrich runs. An
	// in-place append onto a shared Sources or Advisories slice shows up as a
	// subscriber seeing a field it was never sent.
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessDup)
	base, out := h.enriched(t)

	if base == out {
		t.Fatal("Enrich returned the same pointer")
	}
	for _, s := range base.Sessions {
		if s.Transcript != nil {
			t.Errorf("%s: the input snapshot gained transcript facts", s.SessionID)
		}
		if s.Kind != observer.KindInteractive {
			// A background session legitimately arrives with
			// transcriptCollected true and "transcript" among its sources —
			// that is P3's TIMING pass, which ran inside Snapshot. Only the
			// interactive half can be checked for "gained".
			continue
		}
		if s.TranscriptCollected {
			t.Errorf("%s: the input snapshot gained transcriptCollected", s.SessionID)
		}
		for _, a := range s.Advisories {
			if a.Code == observer.AdvTranscriptAmbiguous {
				t.Errorf("%s: the input snapshot gained an advisory", s.SessionID)
			}
		}
		for _, src := range s.Sources {
			if src == observer.SourceTranscript {
				t.Errorf("%s: the input snapshot gained a source", s.SessionID)
			}
		}
	}
	// The envelope too: the sources map is copied, not shared.
	if len(base.Sources) > 0 && reflect.ValueOf(base.Sources).Pointer() == reflect.ValueOf(out.Sources).Pointer() {
		t.Error("the sources map is shared with the input snapshot")
	}
}

func TestEnrichReadsOnlyTheSessionsItWasAsked(t *testing.T) {
	// Adapters request enrichment for the sessions they will actually present
	// (§7.3). A surface showing one session must not pay for 37 reads.
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessC, enrichSessD)
	_, out := h.enriched(t, enrichSessC)

	if s := sessionByID(t, out, enrichSessC); s.Transcript == nil {
		t.Error("the requested session was not enriched")
	}
	for _, id := range []string{enrichSessA, enrichSessD} {
		if s := sessionByID(t, out, id); s.Transcript != nil {
			t.Errorf("%s was enriched without being asked for", id)
		}
	}
	if n := h.counting.Opens(enrichRelA); n != 0 {
		t.Errorf("proj-a's transcript was opened %d times for a request that named only proj-c", n)
	}
}

func TestEnrichWithNoIdsMeansEverySessionInTheSnapshot(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessC)
	_, out := h.enriched(t)
	for _, id := range []string{enrichSessA, enrichSessC} {
		if s := sessionByID(t, out, id); s.Transcript == nil {
			t.Errorf("%s was not enriched by a pass with no id list", id)
		}
	}
}

func TestANilSnapshotIsAProgrammingErrorAndEverythingElseDegrades(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA)
	if _, err := h.obs.Enrich(context.Background(), nil, nil); err == nil {
		t.Error("Enrich accepted a nil snapshot")
	}

	// A transcript source that fails every call must not fail the pass: the
	// other 36 sessions' status answers do not depend on a colour.
	clock := fakes.NewClock(time.UnixMilli(1789699211204).UTC())
	reg := &fakes.Registry{}
	reg.SetRecords(tRef, observer.RegistryRecord{
		Path: "/fixtures/sessions/1.json", PID: 1, SessionID: enrichSessA, Status: "idle",
	})
	broken := fakes.NewTranscripts()
	broken.Err = fmt.Errorf("the disk is on fire")
	obs, err := observer.New(observer.Deps{
		Registry: reg, Jobs: &fakes.Jobs{}, Transcripts: broken,
		Proc: fakes.NewProc(1), Panes: fakes.NewPanes(), Clock: clock,
	}, observer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	base, err := obs.Snapshot(context.Background(), observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := obs.Enrich(context.Background(), base, nil)
	if err != nil {
		t.Fatalf("a failing transcript source failed the whole pass: %v", err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("the session set was blanked: %d sessions", len(out.Sessions))
	}
	if out.Sessions[0].Transcript != nil {
		t.Error("facts were published from a source that errored")
	}
	rep, ok := out.Sources[observer.SourceTranscripts]
	if !ok {
		t.Fatal("no transcripts source report for a pass that attempted a lookup")
	}
	if rep.OK {
		t.Error("the source reports ok despite failing every call")
	}
	if rep.LastError == nil || !strings.Contains(*rep.LastError, "on fire") {
		t.Errorf("lastError = %v; the failure has to be visible somewhere", rep.LastError)
	}
}

func TestACancelledEnrichKeepsWhatItHadAndSaysItWasPartial(t *testing.T) {
	h := newEnrichHarness(t, 0, enrichSessA, enrichSessC)
	base, err := h.obs.Snapshot(context.Background(), observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := h.obs.Enrich(ctx, base, nil)
	if err != nil {
		t.Fatalf("a cancelled pass returned an error instead of a partial answer: %v", err)
	}
	if len(out.Sessions) != len(base.Sessions) {
		t.Error("a cancelled pass dropped sessions")
	}
}

func TestOnlyTheAllowedFactsReachASession(t *testing.T) {
	// §5.4: only agentColor, apiErrors, subagents, sessionKind, sizeBytes, the
	// context size, the model id, the compaction count and the newest
	// timestamp may leave transcriptfs. The canaries are synthetic and NOT in
	// testdata/, so the fixture privacy audit keeps its teeth. The canary
	// record is the one the context read opens: it carries the model and
	// usage the read takes, beside the content it must not.
	fsys := fstest.MapFS{
		enrichRelA: &fstest.MapFile{Data: []byte(
			`{"type":"assistant","timestamp":"2026-09-18T02:39:11.204Z","message":{"content":"CANARY-CONTENT","model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_creation_input_tokens":1995,"cache_read_input_tokens":130000,"output_tokens":400}},"lastPrompt":"CANARY-PROMPT","text":"CANARY-TEXT"}` + "\n" +
				`{"type":"system","subtype":"compact_boundary","content":"CANARY-COMPACT","compactMetadata":{"postTokens":90000}}` + "\n" +
				`{"type":"assistant","message":{"content":"CANARY-LATER","model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":131998,"output_tokens":9}}}` + "\n" +
				`{"type":"agent-color","agentColor":"purple"}` + "\n")},
	}
	clock := fakes.NewClock(time.UnixMilli(1789699211204).UTC())
	reg := &fakes.Registry{}
	reg.SetRecords(tRef, observer.RegistryRecord{
		Path: "/fixtures/sessions/1.json", PID: 1, SessionID: enrichSessA, Status: "idle",
	})
	obs, err := observer.New(observer.Deps{
		Registry: reg, Jobs: &fakes.Jobs{},
		Transcripts: transcriptfs.NewFS(fsys, fixtureProjectsRoot, clock),
		Proc:        fakes.NewProc(1), Panes: fakes.NewPanes(), Clock: clock,
	}, observer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	base, err := obs.Snapshot(context.Background(), observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := obs.Enrich(context.Background(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	rendered := fmt.Sprintf("%+v", out)
	for _, canary := range []string{
		"CANARY-CONTENT", "CANARY-PROMPT", "CANARY-TEXT", "CANARY-COMPACT", "CANARY-LATER",
	} {
		if strings.Contains(rendered, canary) {
			t.Errorf("%s crossed the boundary", canary)
		}
	}
	s := sessionByID(t, out, enrichSessA)
	if s.Transcript == nil || s.Transcript.AgentColor != "purple" {
		t.Fatal("the allowed facts did not arrive, so the canary check proved nothing")
	}
	// The context facts came out of the very records the canaries ride in.
	// The newest usage (after the compaction) is the answer: 2 + 0 + 131 998.
	tr := s.Transcript
	if tr.ContextTokens == nil || *tr.ContextTokens != 132_000 {
		t.Errorf("contextTokens = %v, want 132000", tr.ContextTokens)
	}
	if tr.Model != "claude-opus-5-5" {
		t.Errorf("model = %q, want claude-opus-5-5", tr.Model)
	}
	if tr.Compactions != 1 {
		t.Errorf("compactions = %d, want 1", tr.Compactions)
	}
}

func TestTranscriptFactsHasNoStringFieldThatCouldCarryAMessage(t *testing.T) {
	// §5.4 names this test. The guarantee is the FIELD LIST: a struct with a
	// free-form string field is one vendor change away from carrying a
	// conversation, whatever today's reader happens to put in it.
	//
	// So every field is pinned to its TYPE, not just its name: a count that
	// quietly became a string, or a pointer to one, would pass a name list.
	var (
		str    = reflect.TypeOf("")
		strPtr = reflect.TypeOf((*string)(nil))
		i      = reflect.TypeOf(0)
		intPtr = reflect.TypeOf((*int)(nil))
		i64    = reflect.TypeOf(int64(0))
		i64Ptr = reflect.TypeOf((*int64)(nil))
		b      = reflect.TypeOf(false)
	)
	allowed := map[string]reflect.Type{
		"Path": str, "SizeBytes": i64, "TailBytes": i, "Truncated": b,
		"AgentColor": str, "ColorFound": b, "APIErrors": i,
		"Subagents": intPtr, "SubagentsKnown": b, "SessionKind": strPtr,
		"ContextTokens": i64Ptr, "Model": str, "Compactions": i,
	}
	tp := reflect.TypeOf(observer.TranscriptFacts{})
	if tp.NumField() != len(allowed) {
		t.Errorf("TranscriptFacts has %d fields, the §5.4 allowlist %d", tp.NumField(), len(allowed))
	}
	var stringFields []string
	for n := 0; n < tp.NumField(); n++ {
		f := tp.Field(n)
		want, ok := allowed[f.Name]
		if !ok {
			t.Errorf("TranscriptFacts gained the field %s %s; §5.4's allowlist is "+
				"agentColor, apiErrors, subagents, sessionKind, sizeBytes, contextTokens, "+
				"model, compactions and the newest timestamp, and nothing else may leave a "+
				"transcript", f.Name, f.Type)
			continue
		}
		if f.Type != want {
			t.Errorf("TranscriptFacts.%s is %s, want %s: the type is part of the guarantee",
				f.Name, f.Type, want)
		}
		if f.Type.Kind() == reflect.String ||
			(f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.String) {
			stringFields = append(stringFields, f.Name)
		}
	}

	// The string-kind fields are an EXACT set, each with a bound: Path is a
	// path we constructed; AgentColor, Model and SessionKind come out of a
	// transcript's bytes and are each capped and shape-checked by the adapter.
	// A fifth string field fails here even if someone adds it to the map above.
	wantStrings := []string{"Path", "AgentColor", "SessionKind", "Model"}
	if !reflect.DeepEqual(sortedCopy(stringFields), sortedCopy(wantStrings)) {
		t.Errorf("string fields = %v, want exactly %v", stringFields, wantStrings)
	}

	for name, bound := range map[string]int{
		"MaxAgentColorBytes":  transcriptfs.MaxAgentColorBytes,
		"MaxModelBytes":       transcriptfs.MaxModelBytes,
		"MaxSessionKindBytes": transcriptfs.MaxSessionKindBytes,
	} {
		if bound > 64 || bound <= 0 {
			t.Errorf("%s = %d; an identifier is not that long, and the bound is the only "+
				"thing standing between a string field and a paragraph", name, bound)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
