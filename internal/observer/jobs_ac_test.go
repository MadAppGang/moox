package observer_test

// The background half of the observer, over §9.1's jobs/ and projects/
// fixtures with an injected clock.
//
// What this file proves, and the order matters because it is the order the
// user's own correction implies:
//
//	AC3(c)   blockedForMs computed for a blocked AGENT, statusSinceLowerBound on
//	         the record-later-than-activity fixture, and timeline.jsonl ignored
//	         even when it is sitting right there carrying a different answer.
//	§3.4     the six background cases of the precedence table.
//	§3.5     each of the seven liveness rules on its own fixture.
//	M7/M14   live: null records are IN the default set; terminal ones are out
//	         and come back with includeDead.
//	M8       the duplicate and cross-kind rules.
//	M13      the depth-6 decoy is never read.
//
// Every assertion is against fixtures. The live set moves constantly — a job
// resumes, a transcript is reaped — so an assertion against ~/.claude/ is flaky
// by construction, and validation-criteria.md makes the live run a separate
// bracketed spot-check for exactly that reason.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jobsfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/registryfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/transcriptfs"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

const (
	jobsFixtureDir     = "testdata/jobs"
	projectsFixtureDir = "testdata/projects"
	// fixtureProjectsRoot is the absolute path every fixture's linkScanPath is
	// written against, so the fixtures are self-contained and no assertion
	// depends on the machine the suite runs on.
	fixtureProjectsRoot = "/fixtures/projects"
)

// The session ids of the jobs fixtures, by the thing each one is for.
const (
	idBlockedWithNeeds = "97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01"
	idTempoIdle        = "ea780079-5a4c-444f-a80c-c6a7b29bf62c"
	idBeyondRetention  = "7a324a6d-27ec-4431-b17b-70ab8ac764c7"
	idReaped           = "d3ed2cc8-420f-4a35-be7b-5d374345faae"
	idWorkingFresh     = "11111111-1111-4111-8111-111111111111"
	idWorkingStale     = "22222222-2222-4222-8222-222222222222"
	idDone             = "33333333-3333-4333-8333-333333333333"
	idFailed           = "44444444-4444-4444-8444-444444444444"
	idRecordLater      = "093c8e29-d6e3-47d9-9634-35686cc52c13"
	idShortIDMismatch  = "55555555-5555-4555-8555-555555555555"
	idOutsideRoot      = "66666666-6666-4666-8666-666666666666"
	idJobDuplicate     = "77777777-7777-4777-8777-777777777777"
	idJobUnknownState  = "88888888-8888-4888-8888-888888888888"
	idJobCrossKind     = "f350c44d-d22a-4c47-b3ef-00021713db7f"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type jobsHarness struct {
	t     *testing.T
	obs   *observer.Observer
	jobs  fstest.MapFS
	proj  fstest.MapFS
	clock *fakes.Clock
}

// loadTree loads a fixture directory into a MapFS. The WHOLE directory is
// loaded every time, decoys included: a harness that loaded only the fixtures a
// test expected to be read could never discover that something else was.
func loadTree(t *testing.T, dir string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = &fstest.MapFile{Data: data, Mode: 0o600}
		return nil
	})
	if err != nil {
		t.Fatalf("loading %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty; the suite would pass vacuously", dir)
	}
	return out
}

type jobsHarnessOpts struct {
	// registryFixtures are loaded into the interactive half. Empty means "no
	// interactive sessions", which is the normal case for this file.
	registryFixtures []string
	timingTailBytes  int
	policy           observer.Policy
}

func newJobsHarness(t *testing.T, opts jobsHarnessOpts) *jobsHarness {
	t.Helper()

	jobsFS := loadTree(t, jobsFixtureDir)
	projFS := loadTree(t, projectsFixtureDir)

	regFS := fstest.MapFS{}
	for _, name := range opts.registryFixtures {
		data, err := os.ReadFile(filepath.Join("testdata/registry", name))
		if err != nil {
			t.Fatalf("reading registry fixture %s: %v", name, err)
		}
		regFS[name] = &fstest.MapFile{Data: data, Mode: 0o600}
	}

	clock := fakes.NewClock(tRef)
	policy := opts.policy
	if policy == (observer.Policy{}) {
		policy = observer.DefaultPolicy()
	}

	reg := registryfs.NewFS(regFS, "/fixtures/sessions", clock)
	pre, err := reg.Read(context.Background())
	if err != nil {
		t.Fatalf("seeding from the registry fixtures: %v", err)
	}
	proc := fakes.NewProc()
	var paneIDs []string
	for _, r := range pre.Records {
		proc.SetAlive(r.PID, true)
		if r.Tmux != "" {
			paneIDs = append(paneIDs, r.Tmux)
		}
	}

	obs, err := observer.New(observer.Deps{
		Registry:    reg,
		Jobs:        jobsfs.NewFS(jobsFS, "/fixtures/jobs", clock),
		Transcripts: transcriptfs.NewFS(projFS, fixtureProjectsRoot, clock),
		Proc:        proc,
		Panes:       fakes.NewPanes(paneIDs...),
		Clock:       clock,
	}, observer.Config{
		Paths: observer.Paths{
			Registry: "/fixtures/sessions",
			Jobs:     "/fixtures/jobs",
			Projects: fixtureProjectsRoot,
		},
		Policy:          policy,
		TimingTailBytes: opts.timingTailBytes,
	})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return &jobsHarness{t: t, obs: obs, jobs: jobsFS, proj: projFS, clock: clock}
}

func newJobs(t *testing.T) *jobsHarness {
	t.Helper()
	return newJobsHarness(t, jobsHarnessOpts{})
}

func (h *jobsHarness) snap(opts observer.SnapshotOptions) *observer.Snapshot {
	h.t.Helper()
	s, err := h.obs.Snapshot(context.Background(), opts)
	if err != nil {
		h.t.Fatalf("Snapshot: %v", err)
	}
	return s
}

func (h *jobsHarness) all() *observer.Snapshot {
	return h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})
}

func (h *jobsHarness) def() *observer.Snapshot {
	return h.snap(observer.DefaultSnapshotOptions())
}

func (h *jobsHarness) job(snap *observer.Snapshot, id string) observer.Session {
	h.t.Helper()
	for _, s := range snap.Sessions {
		if s.SessionID == id && s.Kind == observer.KindBackground {
			return s
		}
	}
	h.t.Fatalf("no background session %s; present: %v", id, ids(snap))
	return observer.Session{}
}

func advisory(s observer.Session, code string) (observer.Advisory, bool) {
	for _, a := range s.Advisories {
		if a.Code == code {
			return a, true
		}
	}
	return observer.Advisory{}, false
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := observer.ParseTime(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// AC3(c) — the criterion this phase exists for
// ---------------------------------------------------------------------------

func TestAC3c_BlockedForMsIsComputedForABlockedAgent(t *testing.T) {
	// Decision D1, in one assertion. The earlier design declared blockedForMs
	// undeliverable for background jobs because "both time sources are holed".
	// The user pointed out that a background job has its own conversation
	// thread, measurement confirmed it, and the field is deliverable.
	//
	// statusSince comes from updatedAt VERBATIM, and blockedForMs is a VIEW of
	// statusAgeMs — never a second measurement.
	h := newJobs(t)
	s := h.job(h.def(), idBlockedWithNeeds)

	if s.Status != observer.StatusBlocked {
		t.Fatalf("status = %q, want blocked", s.Status)
	}
	if !s.Attention {
		t.Error("attention = false for a blocked agent")
	}
	if s.StatusSince == nil {
		t.Fatal("statusSince is nil")
	}
	if got := observer.FormatTime(*s.StatusSince); got != "2026-08-25T03:15:47.319Z" {
		t.Errorf("statusSince = %s, want the record's updatedAt verbatim", got)
	}
	if s.BlockedForMs == nil {
		t.Fatal("blockedForMs is nil for a blocked agent; this is the field AC3(c) names")
	}
	if s.StatusAgeMs == nil || *s.BlockedForMs != *s.StatusAgeMs {
		t.Errorf("blockedForMs %v is not the same value as statusAgeMs %v; it is a VIEW, not a measurement",
			s.BlockedForMs, s.StatusAgeMs)
	}
	want := tRef.Sub(*s.StatusSince).Milliseconds()
	if *s.BlockedForMs != want {
		t.Errorf("blockedForMs = %d, want %d against the injected clock", *s.BlockedForMs, want)
	}
	if s.BlockedForUnavailable != nil {
		t.Errorf("blockedForUnavailable = %q, want nil when the value IS available", *s.BlockedForUnavailable)
	}
}

func TestAC3c_TheRecordAndTheThreadAgreeSoNoBoundIsPublished(t *testing.T) {
	// The 3-of-5 case. The transcript's newest timestamp (03:15:47.420Z) sits
	// 101 ms AFTER the record's updatedAt (03:15:47.319Z) — the real measured
	// figure for 97030e53 — which is well inside the 2 s eps, so statusSince is
	// simply right and nothing is added.
	h := newJobs(t)
	s := h.job(h.def(), idBlockedWithNeeds)

	if s.LastActivityAt == nil {
		t.Fatal("lastActivityAt is nil; the transcript exists and carries timestamps")
	}
	if got := observer.FormatTime(*s.LastActivityAt); got != "2026-08-25T03:15:47.420Z" {
		t.Errorf("lastActivityAt = %s", got)
	}
	if s.StatusSinceLowerBound != nil {
		t.Errorf("statusSinceLowerBound = %s, want nil when the two sources agree",
			observer.FormatTime(*s.StatusSinceLowerBound))
	}
	for _, code := range []string{
		observer.AdvStatusSinceRecordLaterThanActivity,
		observer.AdvStatusSinceUnverifiable,
		observer.AdvActivitySinceTruncated,
		observer.AdvTranscriptAbsent,
	} {
		if _, has := advisory(s, code); has {
			t.Errorf("advisory %s fired on a record that agrees with its thread; all advisories: %v",
				code, advisoryCodes(s))
		}
	}
}

func TestAC3c_StatusSinceIgnoresTimelineJSONLEvenWhenItIsPresent(t *testing.T) {
	// THE REGRESSION TEST FOR REVISION 1'S ERROR, and the fixture is built to
	// make it fail if the rule is ever reintroduced: blocked-with-needs carries
	// a timeline.jsonl whose last entry's `at` is 2026-08-25T00:10:42.247Z —
	// the state RUN START, three hours before the record's updatedAt. Revision 1
	// derived statusSince from exactly that and over-reported every blocked
	// duration permanently.
	//
	// timeline.jsonl is an incomplete log: one real record's current state does
	// not appear in its own timeline at all, and 2 of 7 were rewritten days
	// after their last entry. Nothing in the runtime path reads it.
	h := newJobs(t)
	if _, ok := h.jobs["blocked-with-needs/timeline.jsonl"]; !ok {
		t.Fatal("the fixture has no timeline.jsonl, so this test would pass vacuously")
	}

	s := h.job(h.def(), idBlockedWithNeeds)
	runStart := mustTime(t, "2026-08-25T00:10:42.247Z")

	if s.StatusSince.Equal(runStart) {
		t.Fatal("statusSince came from the timeline's run start")
	}
	if s.StatusSince.Before(runStart) {
		t.Errorf("statusSince = %s is before the timeline's run start; something is reading the log",
			observer.FormatTime(*s.StatusSince))
	}
	// The difference is three hours and five minutes of real conversation, which
	// is exactly the magnitude of the wrong answer.
	if delta := s.StatusSince.Sub(runStart); delta < 3*time.Hour {
		t.Errorf("statusSince is only %s after the run start; the fixture no longer distinguishes the two rules", delta)
	}
	if got := *s.BlockedForMs; got > (4*time.Hour).Milliseconds()+tRef.Sub(*s.StatusSince).Milliseconds() {
		t.Errorf("blockedForMs = %d looks like it was measured from the run start", got)
	}
}

func TestAC3c_ARecordWrittenAfterTheThreadWentQuietPublishesALowerBound(t *testing.T) {
	// The 2-of-5 case, measured at +20 days on 093c8e29 and +5 days on
	// d3ed2cc8. statusSince STAYS at updatedAt — it is the only state-labelled
	// timestamp there is — and the transcript publishes what it can prove.
	h := newJobs(t)
	s := h.job(h.def(), idRecordLater)

	if s.StatusSince == nil || observer.FormatTime(*s.StatusSince) != "2026-09-08T08:33:28.956Z" {
		t.Fatalf("statusSince = %v, want the record's updatedAt", s.StatusSince)
	}
	if s.StatusSinceLowerBound == nil {
		t.Fatal("statusSinceLowerBound is nil; the record post-dates its thread by 20 days")
	}
	if got := observer.FormatTime(*s.StatusSinceLowerBound); got != "2026-08-19T08:33:28.956Z" {
		t.Errorf("statusSinceLowerBound = %s, want the transcript's newest timestamp", got)
	}
	if s.LastActivityAt == nil || !s.LastActivityAt.Equal(*s.StatusSinceLowerBound) {
		t.Error("the lower bound is not lastActivityAt")
	}

	a, has := advisory(s, observer.AdvStatusSinceRecordLaterThanActivity)
	if !has {
		t.Fatalf("no %s advisory; got %v", observer.AdvStatusSinceRecordLaterThanActivity, advisoryCodes(s))
	}
	wantDelta := (20 * 24 * time.Hour).Milliseconds()
	if got := a.Fields["deltaMs"]; got != wantDelta {
		t.Errorf("deltaMs = %v, want %d (20 days)", got, wantDelta)
	}
	if a.Fields["recordUpdatedAt"] != "2026-09-08T08:33:28.956Z" {
		t.Errorf("recordUpdatedAt = %v", a.Fields["recordUpdatedAt"])
	}
	if a.Fields["lastActivityAt"] != "2026-08-19T08:33:28.956Z" {
		t.Errorf("lastActivityAt = %v", a.Fields["lastActivityAt"])
	}
}

func TestAC3c_TheBoundOnlyEverUnderReportsAndTheTwoMeasurementsStaySeparate(t *testing.T) {
	// §3.4's error-direction argument, in the form that is actually testable
	// against fixtures.
	//
	// The claim "statusSince is never EARLIER than the truth" is about the
	// instant the state was entered, which nothing on disk records — so it
	// cannot be asserted here, and asserting it against lastActivityAt instead
	// would be wrong: a transcript legitimately receives a record AFTER the
	// status write. That is measured in both directions (97030e53's transcript
	// is 101 ms later than its updatedAt; d3ed2cc8's outlives its daemon by
	// 3 m 08 s) and §5.2's own interactive sample notes the same thing. It is
	// exactly why lastActivityAt is called an INDEPENDENT measurement.
	//
	// What IS assertable, and what the argument rests on:
	//
	//  1. statusSinceLowerBound is always EARLIER than statusSince. It is a
	//     bound that says "at least this long", never a correction that would
	//     move the duration forward and fire a notification immediately.
	//  2. lastActivityAt is never copied into statusSince. statusSince comes
	//     from updatedAt or it is null; the transcript never substitutes for it.
	h := newJobs(t)
	var bounded, distinct int
	for _, s := range h.all().Sessions {
		if s.Kind != observer.KindBackground {
			continue
		}
		if s.StatusSinceLowerBound != nil {
			bounded++
			if s.StatusSince == nil {
				t.Errorf("%s: a lower bound with no statusSince to bound", s.SessionID)
				continue
			}
			if !s.StatusSinceLowerBound.Before(*s.StatusSince) {
				t.Errorf("%s: statusSinceLowerBound %s is not before statusSince %s — that is a correction, not a bound",
					s.SessionID,
					observer.FormatTime(*s.StatusSinceLowerBound),
					observer.FormatTime(*s.StatusSince))
			}
		}
		if s.StatusSince != nil && s.LastActivityAt != nil && !s.StatusSince.Equal(*s.LastActivityAt) {
			distinct++
		}
		if s.StatusSince == nil && s.LastActivityAt != nil {
			t.Errorf("%s: statusSince is null while lastActivityAt is known; the transcript must never be a fallback",
				s.SessionID)
		}
	}
	if bounded == 0 {
		t.Error("no fixture published a lower bound; the record-later-than-activity case is missing")
	}
	if distinct == 0 {
		t.Error("no fixture has two DIFFERENT timestamps; the suite cannot tell the measurements apart")
	}
}

func TestAC3c_AnAbsentTranscriptMakesStatusSinceUnverifiableRatherThanWrong(t *testing.T) {
	h := newJobs(t)
	// all(): rule 2b ends this job, and the default view hides ended ones.
	s := h.job(h.all(), idBeyondRetention)

	if s.LastActivityAt != nil {
		t.Errorf("lastActivityAt = %s for a reaped transcript", observer.FormatTime(*s.LastActivityAt))
	}
	if s.StatusSince == nil {
		t.Fatal("statusSince is nil; updatedAt is still the only state-labelled timestamp we have")
	}
	if s.StatusSinceLowerBound != nil {
		t.Error("a lower bound was published with no transcript to derive it from")
	}
	a, has := advisory(s, observer.AdvStatusSinceUnverifiable)
	if !has {
		t.Fatalf("no %s advisory; got %v", observer.AdvStatusSinceUnverifiable, advisoryCodes(s))
	}
	if a.Fields["reason"] != "no transcript to bound against" {
		t.Errorf("reason = %v", a.Fields["reason"])
	}
	if _, has := advisory(s, observer.AdvTranscriptAbsent); !has {
		t.Errorf("no %s advisory naming the missing file", observer.AdvTranscriptAbsent)
	}
	// And it DID look, which is a different statement from "we did not look".
	if !s.TranscriptCollected {
		t.Error("transcriptCollected = false; we looked and it was not there")
	}
	if s.Transcript != nil {
		t.Error("transcript facts were published for an absent file")
	}
}

func TestAC3c_ATailWithNoTimestampedRecordSaysSoRatherThanGuessing(t *testing.T) {
	// AC12's timing half, reached by injecting a 2 KB tail. At the production
	// 64 KiB against a measured 1 912 B worst case this path is a guard rather
	// than a route, which is exactly why it needs an injectable bound to be
	// testable at all.
	//
	// The fixture is working-fresh with a repointed transcript: proj-g is
	// 78 KB of untimestamped trailing records.
	h := newJobsHarness(t, jobsHarnessOpts{timingTailBytes: 2048})
	jobs := h.jobs
	rewriteLinkScanPath(t, jobs, "working-fresh/state.json",
		fixtureProjectsRoot+"/-home-user-proj-g/99999999-9999-4999-8999-999999999999.jsonl")

	s := h.job(h.def(), idWorkingFresh)
	if s.LastActivityAt != nil {
		t.Errorf("lastActivityAt = %s, want nil", observer.FormatTime(*s.LastActivityAt))
	}
	a, has := advisory(s, observer.AdvActivitySinceTruncated)
	if !has {
		t.Fatalf("no %s advisory; got %v", observer.AdvActivitySinceTruncated, advisoryCodes(s))
	}
	if got, ok := a.Fields["tailBytes"].(int64); !ok || got == 0 || got > 2048 {
		t.Errorf("tailBytes = %v, want the injected bound", a.Fields["tailBytes"])
	}
	if _, has := advisory(s, observer.AdvTranscriptAbsent); has {
		t.Error("transcript-absent fired for a file that is present but whose tail was short")
	}
}

// rewriteLinkScanPath edits a loaded fixture in place, in memory. It is how a
// test expresses "the same record, pointing somewhere else" without a second
// fixture that would then have to be kept in step with the first.
func rewriteLinkScanPath(t *testing.T, fsys fstest.MapFS, name, newPath string) {
	t.Helper()
	f, ok := fsys[name]
	if !ok {
		t.Fatalf("no fixture %s", name)
	}
	var out []byte
	src := string(f.Data)
	i := strings.Index(src, `"linkScanPath"`)
	if i < 0 {
		t.Fatalf("%s has no linkScanPath", name)
	}
	colon := strings.Index(src[i:], ":") + i
	start := strings.Index(src[colon:], `"`) + colon
	end := strings.Index(src[start+1:], `"`) + start + 1
	out = append(out, src[:start+1]...)
	out = append(out, newPath...)
	out = append(out, src[end:]...)
	fsys[name] = &fstest.MapFile{Data: out, Mode: 0o600}
}

// ---------------------------------------------------------------------------
// §3.4 — the six background cases of the precedence table
// ---------------------------------------------------------------------------

func TestBackgroundCase_JobsNonTerminalCarriesTheWholeShape(t *testing.T) {
	// §3.4 row 2. pid, tmux, procAlive and paneAlive are PERMANENTLY null for
	// this population — a background agent has no process of its own and no
	// pane, which is exactly why a pid cannot be the join key.
	h := newJobs(t)
	s := h.job(h.def(), idBlockedWithNeeds)

	if s.PID != nil {
		t.Errorf("pid = %d; a background agent has no process of its own", *s.PID)
	}
	if s.Tmux != nil || s.ProcAlive != nil || s.PaneAlive != nil {
		t.Errorf("tmux/procAlive/paneAlive = %v/%v/%v, want all null", s.Tmux, s.ProcAlive, s.PaneAlive)
	}
	if s.Kind != observer.KindBackground {
		t.Errorf("kind = %q", s.Kind)
	}
	if s.RawKind != nil {
		t.Errorf("rawKind = %q; state.json carries NO kind key — background is our word for where we found it",
			*s.RawKind)
	}
	if s.ShortID == nil || *s.ShortID != "blocked-with-needs" {
		t.Errorf("shortId = %v, want the record's own daemonShort", s.ShortID)
	}
	if s.StatusSource != observer.SourceJobs {
		t.Errorf("statusSource = %q, want jobs", s.StatusSource)
	}
	if s.Needs == nil || !strings.Contains(*s.Needs, "terminal") {
		t.Errorf("needs = %v; jobs is the only source of it", s.Needs)
	}
	if s.Detail == nil || s.Tempo == nil || *s.Tempo != "blocked" {
		t.Errorf("detail = %v, tempo = %v", s.Detail, s.Tempo)
	}
	if s.Tokens == nil || *s.Tokens != 47104 {
		t.Errorf("tokens = %v", s.Tokens)
	}
	if s.ResumeSessionID == nil {
		t.Error("resumeSessionId is nil")
	}
	if s.RecordUpdatedAt == nil || !s.RecordUpdatedAt.Equal(*s.StatusSince) {
		t.Error("recordUpdatedAt should be reported beside statusSince, clearly labelled")
	}
	if s.RecordAgeMs == nil {
		t.Error("recordAgeMs is nil; a consumer needs to see how wide the staleness window could be")
	}
	if len(s.FormerNames) != 0 || s.FormerNames == nil {
		t.Errorf("formerNames = %v, want an empty non-nil slice so the JSON reads []", s.FormerNames)
	}
	if s.WaitReason != observer.WaitReasonNone {
		t.Errorf("waitReason = %q; every job state gives \"\", including blocked", s.WaitReason)
	}
}

func TestBackgroundCase_ATranscriptMakesItASourceOfTheSession(t *testing.T) {
	// §3.4 row 2: sources is ["jobs"] plus "transcript" when one exists. The
	// transcript contributes exactly one field — lastActivityAt — and the
	// enrichment facts are a different read that P3 does not perform.
	h := newJobs(t)
	snap := h.all()

	located := h.job(snap, idBlockedWithNeeds)
	if got := strings.Join(located.Sources, ","); got != "jobs,transcript" {
		t.Errorf("sources = %q, want \"jobs,transcript\"", got)
	}
	if got := strings.Join(located.ListedBy, ","); got != "jobs" {
		t.Errorf("listedBy = %q; membership is not the same question as which sources contributed", got)
	}

	absent := h.job(snap, idBeyondRetention)
	if got := strings.Join(absent.Sources, ","); got != "jobs" {
		t.Errorf("sources = %q for an absent transcript, want \"jobs\"", got)
	}
}

func TestBackgroundCase_TerminalRecordsAreExcludedByDefaultAndReturnWithIncludeDead(t *testing.T) {
	// §3.4 row 3 and AC1's background half. And the directory stays watched
	// regardless, because a terminal job RESUMES: 17 terminal -> non-terminal
	// transitions measured across 6 of 7 timelines.
	h := newJobs(t)

	def := h.def()
	for _, id := range []string{idDone, idFailed} {
		for _, s := range def.Sessions {
			if s.SessionID == id {
				t.Errorf("%s (%s) is in the default set", id, s.Status)
			}
		}
	}

	all := h.all()
	for _, id := range []string{idDone, idFailed} {
		s := h.job(all, id)
		if s.Live == nil || *s.Live {
			t.Errorf("%s: live = %v, want false", id, s.Live)
		}
		if s.LiveReason != observer.LiveReasonJobTerminal {
			t.Errorf("%s: liveReason = %q, want job-terminal", id, s.LiveReason)
		}
		a, has := advisory(s, observer.AdvJobTerminal)
		if !has {
			t.Errorf("%s: no job-terminal advisory", id)
		} else if a.Fields["state"] != s.RawStatus {
			t.Errorf("%s: the advisory does not carry the raw state: %v", id, a.Fields)
		}
	}
}

func TestBackgroundCase_ARecordWithNoTranscriptNamedIsStillAFullSession(t *testing.T) {
	// The `failed` fixture names no transcript at all. "The record names no
	// transcript" is a legible state, not a failure, and transcriptCollected is
	// FALSE for it — we did not look, which is different from looking and
	// finding nothing.
	h := newJobs(t)
	s := h.job(h.all(), idFailed)

	if s.TranscriptCollected {
		t.Error("transcriptCollected = true for a record that names no transcript")
	}
	if _, has := advisory(s, observer.AdvTranscriptAbsent); has {
		t.Error("transcript-absent fired for a path that was never named")
	}
	a, has := advisory(s, observer.AdvStatusSinceUnverifiable)
	if !has || a.Fields["reason"] != "the record names no transcript" {
		t.Errorf("advisories = %v, want status-since-unverifiable naming the reason", advisoryCodes(s))
	}
}

func TestBackgroundCase_AShortIDMismatchIsReportedAndTheRecordIsStillUsed(t *testing.T) {
	// The record's own daemonShort wins — the directory is where we FOUND it,
	// not what it says it is — and the disagreement is published rather than
	// resolved silently.
	h := newJobs(t)
	s := h.job(h.def(), idShortIDMismatch)

	if s.ShortID == nil || *s.ShortID != "deadbeef" {
		t.Errorf("shortId = %v, want the record's daemonShort", s.ShortID)
	}
	a, has := advisory(s, observer.AdvShortIDMismatch)
	if !has {
		t.Fatalf("no shortid-mismatch advisory; got %v", advisoryCodes(s))
	}
	if a.Fields["dirName"] != "shortid-mismatch" || a.Fields["daemonShort"] != "deadbeef" {
		t.Errorf("the advisory does not carry both values: %v", a.Fields)
	}
}

func TestBackgroundCase_ALinkScanPathOutsideTheProjectsRootIsRefused(t *testing.T) {
	// A record is UNTRUSTED INPUT. The refusal degrades to transcript-absent
	// rather than failing the snapshot: one bad record must never blank the
	// background population.
	h := newJobs(t)
	s := h.job(h.def(), idOutsideRoot)

	if s.LastActivityAt != nil {
		t.Error("lastActivityAt was read from a path outside the projects root")
	}
	if _, has := advisory(s, observer.AdvTranscriptAbsent); !has {
		t.Errorf("no transcript-absent advisory; got %v", advisoryCodes(s))
	}
	// And every other session is still there.
	if len(h.def().Sessions) < 5 {
		t.Errorf("only %d sessions survived a refused path", len(h.def().Sessions))
	}
}

// ---------------------------------------------------------------------------
// §3.5 — each rule on its own fixture, through the real join
// ---------------------------------------------------------------------------

func TestRule1TerminalAnswersFalse(t *testing.T) {
	h := newJobs(t)
	s := h.job(h.all(), idDone)
	if s.Live == nil || *s.Live || s.LiveReason != observer.LiveReasonJobTerminal {
		t.Errorf("live = %v, reason = %q", s.Live, s.LiveReason)
	}
}

func TestRule2ReapedAnswersFalseBeforeAnythingElseIsConsulted(t *testing.T) {
	// Measured on d3ed2cc8, which the real data covers only BY LUCK because
	// that record is also `failed`. A record reaped while non-terminal would
	// otherwise be reported live: true, attention: true, with the evidence two
	// keys away. The fixture is therefore `working`, so rule 2 is the rule
	// under test rather than rule 1.
	h := newJobs(t)
	s := h.job(h.all(), idReaped)

	if s.RawStatus != "working" {
		t.Fatalf("the fixture's state is %q; it must be non-terminal or rule 1 answers first", s.RawStatus)
	}
	if s.Live == nil || *s.Live {
		t.Errorf("live = %v, want false", s.Live)
	}
	if s.LiveReason != observer.LiveReasonJobReaped {
		t.Errorf("liveReason = %q, want job-reaped", s.LiveReason)
	}
	a, has := advisory(s, observer.AdvJobReaped)
	if !has {
		t.Fatalf("no job-reaped advisory; got %v", advisoryCodes(s))
	}
	if a.Fields["reapedMidWorkAt"] != "2026-08-19T13:24:24.580Z" {
		t.Errorf("reapedMidWorkAt = %v", a.Fields["reapedMidWorkAt"])
	}
	if s.ReapedMidWorkAt == nil {
		t.Error("reapedMidWorkAt is not published on the session")
	}
}

// noTranscriptGrace is the default policy with rule 2b pushed out of reach.
// The fixtures for rules 3 and 4 are real records whose transcripts are gone,
// so 2b would decide them first; these tests are about the rules below it.
func noTranscriptGrace() observer.Policy {
	p := observer.DefaultPolicy()
	p.TranscriptGraceMs = 100 * 365 * 24 * 60 * 60 * 1000
	return p
}

// TestRule2bAGoneTranscriptEndsTheJobThroughTheRealJoin: the fixture's
// transcript is confirmed missing and its record is a month old, so nothing
// can resume it. It is ended, and the default view hides it.
func TestRule2bAGoneTranscriptEndsTheJobThroughTheRealJoin(t *testing.T) {
	h := newJobs(t)
	s := h.job(h.all(), idBeyondRetention)
	if s.Live == nil || *s.Live {
		t.Fatalf("live = %v, want false", s.Live)
	}
	if s.LiveReason != observer.LiveReasonJobTranscriptGone {
		t.Errorf("liveReason = %q, want job-transcript-gone", s.LiveReason)
	}
	if _, has := advisory(s, observer.AdvJobTranscriptGone); !has {
		t.Errorf("no job-transcript-gone advisory; got %v", advisoryCodes(s))
	}
	if _, found := h.def().SessionByID(idBeyondRetention); found {
		t.Error("an ended job is still in the default view")
	}
}

func TestRule3TempoDisagreesAnswersNullNotFalse(t *testing.T) {
	// Measured on ea780079: state blocked, tempo idle. Two of the vendor's own
	// labels disagree and we do not know which is right — that IS the
	// definition of no evidence, so the answer is null rather than false.
	h := newJobsHarness(t, jobsHarnessOpts{policy: noTranscriptGrace()})
	s := h.job(h.def(), idTempoIdle)

	if s.Live != nil {
		t.Errorf("live = %v, want null", *s.Live)
	}
	if s.LiveReason != observer.LiveReasonJobTempoDisagrees {
		t.Errorf("liveReason = %q, want job-tempo-disagrees", s.LiveReason)
	}
	a, has := advisory(s, observer.AdvTempoStateDisagreement)
	if !has {
		t.Fatalf("no tempo-state-disagreement advisory; got %v", advisoryCodes(s))
	}
	if a.Fields["state"] != "blocked" || a.Fields["tempo"] != "idle" {
		t.Errorf("the advisory does not carry both labels: %v", a.Fields)
	}
}

func TestRule4BeyondRetentionAnswersNullWithItsEvidence(t *testing.T) {
	// THE BUG THIS PHASE EXISTS TO PREVENT, in one test. The record claims
	// `blocked`, it is 31.8 days old, and its transcript is gone — so no
	// transcript COULD corroborate it, because nothing in the depth-2 session
	// population survives past the retention floor. Revision 1 reported this
	// record live: true, attention: true, which made counts.attention
	// permanently >= 3 and dominated the projection that exists so any Claude
	// session can ask what the others are doing.
	h := newJobsHarness(t, jobsHarnessOpts{policy: noTranscriptGrace()})
	s := h.job(h.def(), idBeyondRetention)

	if s.Live != nil {
		t.Fatalf("live = %v, want null — a 31-day-old unfalsifiable `blocked` claim", *s.Live)
	}
	if s.LiveReason != observer.LiveReasonJobBeyondRetention {
		t.Errorf("liveReason = %q, want job-beyond-transcript-retention", s.LiveReason)
	}
	a, has := advisory(s, observer.AdvBeyondTranscriptRetention)
	if !has {
		t.Fatalf("no beyond-transcript-retention advisory; got %v", advisoryCodes(s))
	}
	if a.Fields["retentionMs"] != observer.DefaultTranscriptRetentionMs {
		t.Errorf("retentionMs = %v, want the policy's value", a.Fields["retentionMs"])
	}
	ageMs, ok := a.Fields["recordAgeMs"].(int64)
	if !ok || ageMs <= observer.DefaultTranscriptRetentionMs {
		t.Errorf("recordAgeMs = %v, want more than the retention window", a.Fields["recordAgeMs"])
	}
	// It is STILL in the default set, with its evidence attached. M7: a session
	// vanishing from the default view requires POSITIVE evidence of death.
	if _, found := h.def().SessionByID(idBeyondRetention); !found {
		t.Error("a live: null session was hidden by the default filter")
	}
}

func TestRule4NeedsBothClausesThroughTheRealJoin(t *testing.T) {
	// A missing transcript on a RECENT record is not evidence of anything — the
	// retention argument is the whole basis of the rule, so without the age
	// clause it would be an invented expiry heuristic. Same fixture, a policy
	// with a 100-year window: rule 4 must stop matching.
	policy := noTranscriptGrace()
	policy.TranscriptRetentionMs = 100 * 365 * 24 * 60 * 60 * 1000
	h := newJobsHarness(t, jobsHarnessOpts{policy: policy})

	s := h.job(h.def(), idBeyondRetention)
	if s.LiveReason == observer.LiveReasonJobBeyondRetention {
		t.Error("rule 4 matched with the record inside the retention window")
	}
	if _, has := advisory(s, observer.AdvBeyondTranscriptRetention); has {
		t.Error("the beyond-retention advisory fired inside the window")
	}
	// It falls through to 6b, because this build has no corroboration source.
	if s.LiveReason != observer.LiveReasonJobNonTerminalUncorroborated {
		t.Errorf("liveReason = %q, want the rule 6b fallthrough", s.LiveReason)
	}
}

func TestRule5WorkingStaleIsMeasuredAgainstTheTranscriptNotTheRecord(t *testing.T) {
	// Revision 1 measured this against recordAgeMs, which a job that writes its
	// record only at milestones fails while genuinely working. The fixture's
	// record is FRESH (updatedAt 02:39, one minute before the reference
	// instant) and its transcript is 40 minutes stale against a 15-minute
	// threshold — so a record-based rule answers "fine" and a transcript-based
	// one answers "stale". They cannot both be right, and the transcript moves
	// every turn.
	h := newJobs(t)
	s := h.job(h.def(), idWorkingStale)

	if s.RecordAgeMs == nil || *s.RecordAgeMs > (5*time.Minute).Milliseconds() {
		t.Fatalf("recordAgeMs = %v; the fixture's record must be FRESH for this test to mean anything", s.RecordAgeMs)
	}
	if s.Live != nil {
		t.Errorf("live = %v, want null", *s.Live)
	}
	if s.LiveReason != observer.LiveReasonJobWorkingStale {
		t.Errorf("liveReason = %q, want job-working-stale", s.LiveReason)
	}
	a, has := advisory(s, observer.AdvBackgroundWorkingStale)
	if !has {
		t.Fatalf("no background-working-stale advisory; got %v", advisoryCodes(s))
	}
	if a.Fields["thresholdMs"] != observer.DefaultWorkingStaleAfterMs {
		t.Errorf("thresholdMs = %v", a.Fields["thresholdMs"])
	}

	// M14: it is STILL in the default set. A slow `working` job vanishing from
	// a live == true filter on an admittedly unmeasured threshold is the exact
	// failure M14 named.
	if _, found := h.def().SessionByID(idWorkingStale); !found {
		t.Error("a working-stale session was hidden by the default filter")
	}
}

func TestRule5IsClockSensitiveThroughTheRealJoin(t *testing.T) {
	// working-fresh's transcript is one minute old at the reference instant and
	// 41 minutes old sixteen minutes later. Nothing changes on disk; the answer
	// changes. Nothing wakes up to publish that crossing, which is what
	// notCapable: ["time-transitions"] advertises.
	h := newJobs(t)

	first := h.job(h.def(), idWorkingFresh)
	if first.LiveReason == observer.LiveReasonJobWorkingStale {
		t.Fatalf("working-fresh is already stale at the reference instant: %v", first.LiveReason)
	}

	h.clock.Advance(20 * time.Minute)
	second := h.job(h.def(), idWorkingFresh)
	if second.LiveReason != observer.LiveReasonJobWorkingStale {
		t.Errorf("liveReason = %q after 20 minutes, want job-working-stale", second.LiveReason)
	}
	if second.Live != nil {
		t.Errorf("live = %v, want null", *second.Live)
	}
}

func TestRule6bIsWhatABuildWithNoCorroborationSourceReports(t *testing.T) {
	// The honest answer for THIS build: there is no AgentsSource wired, so the
	// only POSITIVE liveness signal a background job has was never taken. That
	// is rule 6b(ii) — "the source never ran" — and it reads null with
	// job-corroboration-stale, never true from the absence of disproof.
	//
	// This is the assertion that distinguishes P3 from the bug it replaces:
	// under revision 2's rule 6 this same record read live: true.
	h := newJobs(t)
	s := h.job(h.def(), idBlockedWithNeeds)

	if s.Live != nil {
		t.Fatalf("live = %v, want null with no corroboration source wired", *s.Live)
	}
	if s.LiveReason != observer.LiveReasonJobNonTerminalUncorroborated {
		t.Errorf("liveReason = %q, want job-non-terminal-uncorroborated", s.LiveReason)
	}
	a, has := advisory(s, observer.AdvJobCorroborationStale)
	if !has {
		t.Fatalf("no job-corroboration-stale advisory; got %v", advisoryCodes(s))
	}
	if got := a.Fields["agentsAgeMs"]; got != nil {
		t.Errorf("agentsAgeMs = %v, want nil when no listing was taken at all", got)
	}
	if a.Fields["staleAfterMs"] != observer.DefaultStaleAfterMs {
		t.Errorf("staleAfterMs = %v", a.Fields["staleAfterMs"])
	}
}

func TestRule7AnUnrecognisedStateAnswersNull(t *testing.T) {
	h := newJobs(t)
	s := h.job(h.def(), idJobUnknownState)

	if s.Live != nil {
		t.Errorf("live = %v, want null", *s.Live)
	}
	if s.LiveReason != observer.LiveReasonUnknown {
		t.Errorf("liveReason = %q, want liveness-unknown", s.LiveReason)
	}
	if s.Status != observer.StatusUnknown {
		t.Errorf("status = %q, want unknown", s.Status)
	}
	if s.RawStatus != "reticulating" {
		t.Errorf("rawStatus = %q, want the literal preserved", s.RawStatus)
	}
	if _, found := h.def().SessionByID(idJobUnknownState); !found {
		t.Error("an unrecognised state was dropped rather than reported with no opinion")
	}
}

func TestNoBackgroundSessionReadsLiveTrueWithoutAFreshListing(t *testing.T) {
	// The invariant behind the rule 6/6b split, asserted over the whole fixture
	// set rather than sampled: in a build whose corroboration source never ran,
	// NOTHING may read live: true. If any fixture does, a negative premise has
	// been dressed up in a positive field's name again.
	h := newJobs(t)
	for _, s := range h.all().Sessions {
		if s.Kind != observer.KindBackground {
			continue
		}
		if s.Live != nil && *s.Live {
			t.Errorf("%s reads live: true with no corroboration source; reason %q",
				s.SessionID, s.LiveReason)
		}
	}
}

// ---------------------------------------------------------------------------
// M7 / M14 — membership and counting
// ---------------------------------------------------------------------------

func TestEveryLiveNullBackgroundRecordIsInTheDefaultSet(t *testing.T) {
	// Default filtering is `live != false`, NOT `live == true`. A session
	// vanishing from the default view requires POSITIVE evidence of death.
	h := newJobs(t)
	all := h.all()
	def := h.def()

	var nulls int
	for _, s := range all.Sessions {
		if s.Kind != observer.KindBackground || s.Live != nil {
			continue
		}
		nulls++
		if _, found := def.SessionByID(s.SessionID); !found {
			t.Errorf("%s (live: null, %s) is not in the default set", s.SessionID, s.LiveReason)
		}
	}
	if nulls < 4 {
		t.Errorf("only %d live: null fixtures; this assertion needs the rule 3/4/5/6b cases", nulls)
	}
}

func TestTheCountsInvariantHoldsWithBothPopulations(t *testing.T) {
	h := newJobsHarness(t, jobsHarnessOpts{
		registryFixtures: []string{"live-idle.json", "live-busy.json", "dead-pid.json"},
	})
	for name, snap := range map[string]*observer.Snapshot{"default": h.def(), "all": h.all()} {
		c := snap.Counts
		if c.Total != c.Live+c.Dead+c.Unknown {
			t.Errorf("%s: total %d != live %d + dead %d + unknown %d",
				name, c.Total, c.Live, c.Dead, c.Unknown)
		}
		if c.Total != len(snap.Sessions) {
			t.Errorf("%s: total %d != %d sessions", name, c.Total, len(snap.Sessions))
		}
		if c.ByKind[observer.KindBackground] == 0 {
			t.Errorf("%s: no background sessions were counted", name)
		}
	}
}

func TestADeadRecordsStaleBlockedNeverContributesToAttention(t *testing.T) {
	// counts.attention counts `attention && live != false`, so a terminal
	// record's stale `blocked` cannot inflate it while an unknown-liveness one
	// does, with its advisories attached.
	h := newJobs(t)
	all := h.all()

	var attentionAndDead int
	for _, s := range all.Sessions {
		if s.Attention && s.Live != nil && !*s.Live {
			attentionAndDead++
		}
	}
	expected := 0
	for _, s := range all.Sessions {
		if s.Attention && !(s.Live != nil && !*s.Live) {
			expected++
		}
	}
	if all.Counts.Attention != expected {
		t.Errorf("counts.attention = %d, want %d", all.Counts.Attention, expected)
	}
	if attentionAndDead > 0 && all.Counts.Attention >= expected+attentionAndDead {
		t.Error("a dead record's stale blocked contributed to the attention count")
	}
}

func TestStatusAgeMsEqualsObservedAtMinusStatusSinceForEveryBackgroundSession(t *testing.T) {
	// The identity §9.2(c) names, applied to the population P3 adds. Its
	// companion for recordAgeMs is here too, because round 3 found that one
	// inheriting the background sample's error.
	h := newJobs(t)
	snap := h.all()
	for _, s := range snap.Sessions {
		if s.Kind != observer.KindBackground {
			continue
		}
		if s.StatusSince == nil {
			if s.StatusAgeMs != nil {
				t.Errorf("%s: statusAgeMs is set with a nil statusSince", s.SessionID)
			}
			continue
		}
		if want := snap.ObservedAt.Sub(*s.StatusSince).Milliseconds(); s.StatusAgeMs == nil || *s.StatusAgeMs != want {
			t.Errorf("%s: statusAgeMs = %v, want %d", s.SessionID, s.StatusAgeMs, want)
		}
		if s.RecordUpdatedAt != nil {
			if want := snap.ObservedAt.Sub(*s.RecordUpdatedAt).Milliseconds(); s.RecordAgeMs == nil || *s.RecordAgeMs != want {
				t.Errorf("%s: recordAgeMs = %v, want %d", s.SessionID, s.RecordAgeMs, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// M8 — the duplicate and cross-kind rules
// ---------------------------------------------------------------------------

func TestTwoJobDirectoriesSharingASessionIdProduceOneSession(t *testing.T) {
	// Records are read directory-name ascending, so "the first wins" is
	// deterministic. The loser is DISCARDED, not merged: merging means taking
	// one record's state and the other's timing, which is a wrong answer where
	// "no opinion about the loser" is an honest one.
	h := newJobs(t)
	all := h.all()

	var matches []observer.Session
	for _, s := range all.Sessions {
		if s.SessionID == idJobDuplicate {
			matches = append(matches, s)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("%d sessions carry the duplicated id, want 1", len(matches))
	}

	a, has := advisory(matches[0], observer.AdvSessionIDDuplicate)
	if !has {
		t.Fatalf("no sessionid-duplicate advisory; got %v", advisoryCodes(matches[0]))
	}
	if a.Fields["kind"] != string(observer.KindBackground) || a.Fields["source"] != observer.SourceJobs {
		t.Errorf("the advisory does not name the population: %v", a.Fields)
	}
	if a.Fields["winner"] != "dup-a" || a.Fields["loser"] != "dup-b" {
		t.Errorf("winner/loser = %v/%v, want dup-a/dup-b — a job record has no pid to identify it by",
			a.Fields["winner"], a.Fields["loser"])
	}
}

func TestOneSessionIdAsBothKindsEmitsTwoSessionsAndNeverMergesThem(t *testing.T) {
	// M8's cross-kind case. Folding them into one record would publish a
	// session with a pid AND a job state — a thing that does not exist. Both
	// are emitted, each naming the other kind.
	h := newJobsHarness(t, jobsHarnessOpts{registryFixtures: []string{"live-idle.json"}})
	all := h.all()

	var kinds []observer.Kind
	for _, s := range all.Sessions {
		if s.SessionID != idJobCrossKind {
			continue
		}
		kinds = append(kinds, s.Kind)
		a, has := advisory(s, observer.AdvSessionIDCrossKind)
		if !has {
			t.Errorf("the %s session carries no sessionid-cross-kind advisory; got %v",
				s.Kind, advisoryCodes(s))
			continue
		}
		if a.Fields["other"] == string(s.Kind) {
			t.Errorf("the advisory names its own kind: %v", a.Fields)
		}
	}
	if len(kinds) != 2 {
		t.Fatalf("%d sessions carry the cross-kind id (%v), want 2", len(kinds), kinds)
	}
	// And the two are ordered deterministically, or AC11's byte-identity
	// assertion is decided by which half of the join ran first.
	if kinds[0] != observer.KindBackground || kinds[1] != observer.KindInteractive {
		t.Errorf("kinds = %v; the order must be stable", kinds)
	}

	first := idsOfKind(h.all())
	second := idsOfKind(h.all())
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Errorf("two snapshots of the same state ordered differently:\n %v\n %v", first, second)
	}
}

func idsOfKind(snap *observer.Snapshot) []string {
	out := make([]string, 0, len(snap.Sessions))
	for _, s := range snap.Sessions {
		out = append(out, s.SessionID+":"+string(s.Kind))
	}
	return out
}

// ---------------------------------------------------------------------------
// the timing cache, and Invariant B end to end
// ---------------------------------------------------------------------------

func TestAnUnchangedJobIsNotReReadOnTheSecondSnapshot(t *testing.T) {
	// The steady state. Four of Invariant B's five predicates are CHANGE
	// predicates, so a second rebuild with nothing changed reads no transcript
	// at all — which is what makes reading on four predicates affordable rather
	// than a poll in disguise.
	h := newJobs(t)
	opts := observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true}

	first := h.snap(opts).Sources[observer.SourceTranscripts]
	if first.Counters["timingReads"] == 0 {
		t.Fatal("the first rebuild read no transcript; Invariant B case 1 did not fire")
	}
	if first.Counters["timingCacheHits"] != 0 {
		t.Errorf("the first rebuild had %d cache hits", first.Counters["timingCacheHits"])
	}

	second := h.snap(opts)
	rep := second.Sources[observer.SourceTranscripts]
	if rep.Counters["timingReads"] != 0 {
		t.Errorf("timingReads = %d on an unchanged second rebuild, want 0",
			rep.Counters["timingReads"])
	}
	if rep.Counters["timingCacheHits"] != first.Counters["timingReads"] {
		t.Errorf("cacheHits %d != the %d reads the first rebuild performed",
			rep.Counters["timingCacheHits"], first.Counters["timingReads"])
	}

	// Note what DOES still happen every rebuild: the STAT. "Is the transcript
	// still there" is §3.5 rule 4's input and it is not cacheable — a reaped
	// file has to become visible on the next rebuild, not on the next resume.
	if rep.Counters["byLinkScanPath"] != first.Counters["byLinkScanPath"] {
		t.Errorf("the lookups stopped happening: %d -> %d",
			first.Counters["byLinkScanPath"], rep.Counters["byLinkScanPath"])
	}

	// ... and the answer is still there, served from the cache.
	for _, s := range second.Sessions {
		if s.SessionID == idBlockedWithNeeds && s.LastActivityAt == nil {
			t.Error("a cache hit lost the answer it was caching")
		}
	}
}

func TestARepointedLinkScanPathReadsTheNewThreadAndDropsTheOldFacts(t *testing.T) {
	// INVARIANT B CASE 2, end to end, and the failure it prevents: without the
	// path in the cache key, a resume publishes the PREVIOUS conversation's
	// last turn as the new thread's last activity. Both answers are well-formed
	// timestamps, so nothing looks wrong.
	//
	// The two transcripts are given the SAME size and the SAME mtime, which is
	// what makes this a test of the KEY rather than of a size/mtime cache: a
	// (sessionId, size, mtime) key hits, and a (sessionId, path) key misses.
	jobsFS := loadTree(t, jobsFixtureDir)
	projFS := loadTree(t, projectsFixtureDir)

	const (
		relA = "-home-user-resume/aaaaaaaa-0000-4000-8000-000000000000.jsonl"
		relB = "-home-user-resume/bbbbbbbb-0000-4000-8000-000000000000.jsonl"
	)
	// Identical length, identical mtime, DIFFERENT newest timestamp. B's
	// timestamp is chosen to EQUAL the record's own updatedAt, so a resume that
	// correctly re-reads publishes no lower bound at all — which is how "A's
	// derived facts were dropped rather than carried forward" becomes an
	// assertion about absence rather than about a different number.
	projFS[relA] = &fstest.MapFile{
		Data:    []byte(`{"type":"user","timestamp":"2026-01-01T00:00:00.000Z"}` + "\n"),
		ModTime: tRef,
	}
	projFS[relB] = &fstest.MapFile{
		Data:    []byte(`{"type":"user","timestamp":"2026-09-18T02:39:00.000Z"}` + "\n"),
		ModTime: tRef,
	}
	if len(projFS[relA].Data) != len(projFS[relB].Data) {
		t.Fatal("the two transcripts differ in size; the test would pass for the wrong reason")
	}

	counting := &countingProjects{inner: projFS, opens: map[string]int{}}
	clock := fakes.NewClock(tRef)
	obs, err := observer.New(observer.Deps{
		Registry:    registryfs.NewFS(fstest.MapFS{}, "/fixtures/sessions", clock),
		Jobs:        jobsfs.NewFS(jobsFS, "/fixtures/jobs", clock),
		Transcripts: transcriptfs.NewFS(counting, fixtureProjectsRoot, clock),
		Proc:        fakes.NewProc(),
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}, observer.Config{Paths: observer.Paths{Projects: fixtureProjectsRoot}})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	opts := observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true}

	// Round 1: the record names transcript A.
	rewriteLinkScanPath(t, jobsFS, "working-fresh/state.json", fixtureProjectsRoot+"/"+relA)
	snap, err := obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	first := backgroundByID(t, snap, idWorkingFresh)
	if first.LastActivityAt == nil || observer.FormatTime(*first.LastActivityAt) != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("round 1 lastActivityAt = %v, want A's", first.LastActivityAt)
	}
	if first.StatusSinceLowerBound == nil {
		t.Fatal("round 1 published no lower bound; A is old enough that the record post-dates it")
	}

	// Round 2: the resume repoints it at transcript B.
	rewriteLinkScanPath(t, jobsFS, "working-fresh/state.json", fixtureProjectsRoot+"/"+relB)
	snap, err = obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	second := backgroundByID(t, snap, idWorkingFresh)

	if second.LastActivityAt == nil {
		t.Fatal("round 2 lastActivityAt is nil")
	}
	if got := observer.FormatTime(*second.LastActivityAt); got != "2026-09-18T02:39:00.000Z" {
		t.Errorf("round 2 lastActivityAt = %s, want B's — A's answer was carried across a resume", got)
	}
	if counting.opens[relB] == 0 {
		t.Error("the new transcript was never read; the cache key does not include the path")
	}
	// And every fact derived from A is GONE rather than carried forward.
	if second.StatusSinceLowerBound != nil {
		t.Errorf("statusSinceLowerBound = %s survived the resume; it measured a different conversation",
			observer.FormatTime(*second.StatusSinceLowerBound))
	}
	if _, has := advisory(second, observer.AdvStatusSinceRecordLaterThanActivity); has {
		t.Error("an advisory derived from the OLD thread survived the resume")
	}
}

func TestAChangedUpdatedAtReReadsTheTranscript(t *testing.T) {
	// Invariant B case 3, end to end.
	jobsFS := loadTree(t, jobsFixtureDir)
	projFS := loadTree(t, projectsFixtureDir)
	counting := &countingProjects{inner: projFS, opens: map[string]int{}}

	clock := fakes.NewClock(tRef)
	obs, err := observer.New(observer.Deps{
		Registry:    registryfs.NewFS(fstest.MapFS{}, "/fixtures/sessions", clock),
		Jobs:        jobsfs.NewFS(jobsFS, "/fixtures/jobs", clock),
		Transcripts: transcriptfs.NewFS(counting, fixtureProjectsRoot, clock),
		Proc:        fakes.NewProc(),
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}, observer.Config{Paths: observer.Paths{Projects: fixtureProjectsRoot}})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	opts := observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true}
	const target = "-home-user-proj-h/11111111-1111-4111-8111-111111111111.jsonl"

	if _, err := obs.Snapshot(context.Background(), opts); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := counting.opens[target]

	// The daemon writes its record again. Nothing about the transcript changed,
	// and no watch event fired — the record's own timestamp is the trigger.
	replaceInFixture(t, jobsFS, "working-fresh/state.json",
		"2026-09-18T02:39:00.000Z", "2026-09-18T02:39:30.000Z")

	if _, err := obs.Snapshot(context.Background(), opts); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := counting.opens[target]; got <= before {
		t.Errorf("the transcript was not re-read after updatedAt changed: %d -> %d", before, got)
	}
}

func replaceInFixture(t *testing.T, fsys fstest.MapFS, name, old, new string) {
	t.Helper()
	f, ok := fsys[name]
	if !ok {
		t.Fatalf("no fixture %s", name)
	}
	src := string(f.Data)
	if !strings.Contains(src, old) {
		t.Fatalf("%s does not contain %q", name, old)
	}
	fsys[name] = &fstest.MapFile{
		Data: []byte(strings.Replace(src, old, new, 1)), Mode: 0o600,
	}
}

func backgroundByID(t *testing.T, snap *observer.Snapshot, id string) observer.Session {
	t.Helper()
	for _, s := range snap.Sessions {
		if s.SessionID == id && s.Kind == observer.KindBackground {
			return s
		}
	}
	t.Fatalf("no background session %s; present: %v", id, ids(snap))
	return observer.Session{}
}

// ---------------------------------------------------------------------------
// the jobs source report
// ---------------------------------------------------------------------------

func TestTheJobsSourceReportsAllFourCountsSoTheyReconcile(t *testing.T) {
	h := newJobs(t)
	rep, ok := h.all().Sources[observer.SourceJobs]
	if !ok {
		t.Fatal("no jobs source report")
	}
	if rep.Role != "authoritative" {
		t.Errorf("role = %q", rep.Role)
	}
	if rep.Path != "/fixtures/jobs" {
		t.Errorf("path = %q", rep.Path)
	}
	for _, key := range []string{"entries", "dirs", "records", "nonTerminal", "parseErrors"} {
		if _, has := rep.Counters[key]; !has {
			t.Errorf("the report has no %q counter; the counts cannot be reconciled without all of them", key)
		}
	}
	if rep.Counters["entries"] <= rep.Counters["dirs"] {
		t.Errorf("entries %d <= dirs %d; pins.json is being counted as a directory",
			rep.Counters["entries"], rep.Counters["dirs"])
	}
	if rep.Counters["records"] >= rep.Counters["dirs"] {
		t.Errorf("records %d >= dirs %d", rep.Counters["records"], rep.Counters["dirs"])
	}
	if rep.Counters["parseErrors"] == 0 {
		t.Error("parseErrors = 0; the torn and no-session-id fixtures should each produce one")
	}
	if !rep.OK {
		t.Error("ok = false; a parse error is a counted value, not a source failure")
	}
}

func TestTheTranscriptsSourceReportsTheTimingPassAndItsBound(t *testing.T) {
	h := newJobs(t)
	rep, ok := h.all().Sources[observer.SourceTranscripts]
	if !ok {
		t.Fatal("no transcripts source report")
	}
	if rep.Labels["pass"] != "timing" {
		t.Errorf("the report does not say which pass ran: %v", rep.Labels)
	}
	if rep.Counters["byLinkScanPath"] == 0 {
		t.Error("byLinkScanPath = 0")
	}
	if rep.Counters["located"] == 0 || rep.Counters["absent"] == 0 {
		t.Errorf("located = %d, absent = %d; the fixture set has both",
			rep.Counters["located"], rep.Counters["absent"])
	}
	if rep.Counters["timingReads"] == 0 {
		t.Error("timingReads = 0")
	}
	if rep.Counters["lookups"] != rep.Counters["byLinkScanPath"] {
		t.Error("lookups and byLinkScanPath disagree; P3 performs no glob lookups at all")
	}
}

func TestNoTranscriptSourceIsReportedWhenNoRecordNamesOne(t *testing.T) {
	// The same discipline the tmux gate follows: a source report for a read
	// nobody performed invites a consumer to compare counters that describe
	// nothing.
	clock := fakes.NewClock(tRef)
	obs, err := observer.New(observer.Deps{
		Registry:    registryfs.NewFS(fstest.MapFS{}, "/x", clock),
		Jobs:        &fakes.Jobs{},
		Transcripts: fakes.NewTranscripts(),
		Proc:        fakes.NewProc(),
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}, observer.Config{})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	snap, err := obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, has := snap.Sources[observer.SourceTranscripts]; has {
		t.Error("a transcripts source is reported although no transcript was looked for")
	}
	if _, has := snap.Sources[observer.SourceJobs]; !has {
		t.Error("no jobs source report; the jobs tree IS read on every rebuild")
	}
}

func TestAnAbsentJobsTreeIsAnEmptyBackgroundPopulationNotAnError(t *testing.T) {
	clock := fakes.NewClock(tRef)
	obs, err := observer.New(observer.Deps{
		Registry:    registryfs.NewFS(fstest.MapFS{}, "/x", clock),
		Jobs:        jobsfs.New(filepath.Join(t.TempDir(), "nope"), clock),
		Transcripts: fakes.NewTranscripts(),
		Proc:        fakes.NewProc(),
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}, observer.Config{})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	snap, err := obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("an absent jobs tree produced an error: %v", err)
	}
	rep := snap.Sources[observer.SourceJobs]
	if !rep.OK {
		t.Error("ok = false; \"no background agent has ever run here\" is a fact")
	}
	if rep.Labels["state"] != "absent" {
		t.Errorf("labels = %v, want state=absent", rep.Labels)
	}
}

// ---------------------------------------------------------------------------
// M13, through the whole snapshot path
// ---------------------------------------------------------------------------

func TestTheDecoyIsNeverReportedAsASessionByTheObserver(t *testing.T) {
	h := newJobs(t)
	for _, s := range h.all().Sessions {
		if strings.HasPrefix(s.SessionID, "decoy") {
			t.Fatalf("the depth-6 decoy reached the snapshot: %+v", s)
		}
	}
}

// countingProjects counts opens by relative path, for the cache assertions.
// The answer is identical whether or not a read happened, so counting is the
// only way to make a claim about it.
type countingProjects struct {
	inner fstest.MapFS
	opens map[string]int
}

func (c *countingProjects) Open(name string) (fs.File, error) {
	c.opens[name]++
	return c.inner.Open(name)
}

// ---------------------------------------------------------------------------
// two defects the LIVE run surfaced, pinned so they cannot come back
// ---------------------------------------------------------------------------

func TestNoSourceIsEverReportedAsObservedInTheFuture(t *testing.T) {
	// FOUND ON THE LIVE MACHINE: sources.jobs.ageMs was -1.
	//
	// Each adapter stamps its result with its OWN clock read, which happens
	// after the rebuild's single `now`; the jobs tree (15 entries, 7 small
	// files) is fast enough for the truncated-to-millisecond difference to come
	// out negative. A source observed in the future is not a fact about
	// anything, and it makes a consumer's `ageMs > staleAfterMs` comparison read
	// as absurd.
	h := newJobs(t)
	for id, rep := range h.all().Sources {
		if rep.AgeMs == nil {
			continue
		}
		if *rep.AgeMs < 0 {
			t.Errorf("sources.%s.ageMs = %d", id, *rep.AgeMs)
		}
	}
}

func TestThePublishedTimingBoundIsTheBoundThatWasUsed(t *testing.T) {
	// FOUND ON THE LIVE MACHINE: tailBytesTiming was 0, because the core passed
	// 0 to mean "the adapter's default" and then published the 0. A consumer
	// diagnosing a null lastActivityAt would have been told no bytes were read.
	h := newJobs(t)
	rep := h.all().Sources[observer.SourceTranscripts]
	if got := rep.Counters["tailBytesTiming"]; got != int64(observer.DefaultTimingTailBytes) {
		t.Errorf("tailBytesTiming = %d, want %d", got, observer.DefaultTimingTailBytes)
	}

	injected := newJobsHarness(t, jobsHarnessOpts{timingTailBytes: 2048})
	rep = injected.all().Sources[observer.SourceTranscripts]
	if got := rep.Counters["tailBytesTiming"]; got != 2048 {
		t.Errorf("tailBytesTiming = %d with an injected 2048", got)
	}
}
