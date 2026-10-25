package observer_test

// SnapshotOptions.NoTranscriptReads — `tt sessions --no-transcript` — through
// the real core.
//
// §5.2's contract for the flag: "sets transcriptCollected: false on EVERY
// session … lastActivityAt is null and §3.4's cross-check cannot run, so a job's
// statusSince carries status-since-unverifiable and §3.5 rules 4 and 5 fall back
// to recordAgeMs." The black-box suite found jobs still reporting
// transcriptCollected: true and a lastActivityAt under the flag, because P3's
// 64 KiB timing read (a correctness input) ignored it. These assert the
// contract, and — by counting calls on the transcript port — that "no
// transcript is read" means not one open and not one stat.

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

const (
	noTxJob        = "0770b000-0000-4000-8000-000000000001"
	noTxTranscript = "/fixtures/projects/own/job-thread.jsonl"
)

func noTranscriptCore(t *testing.T, updated time.Time) (*observer.Observer, *fakes.Set) {
	t.Helper()
	deps, set := fakes.Deps(tRef)
	set.Jobs.SetRecords(tRef, observer.JobRecord{
		Dir: "0770b000", Path: "/fixtures/jobs/0770b000/state.json", DaemonShort: "0770b000",
		SessionID: noTxJob, State: "blocked", Tempo: "blocked", UpdatedAt: &updated,
		LinkScanPath: noTxTranscript,
	})
	activity := updated.Add(-time.Minute)
	set.Transcripts.ByPath[noTxTranscript] = observer.TranscriptLocation{
		Found: true, Path: noTxTranscript, SizeBytes: 100, ModTime: updated,
	}
	set.Transcripts.Timing[noTxTranscript] = observer.TimingFacts{LastActivityAt: &activity, ReachedStart: true}
	obs, err := observer.New(deps, observer.Config{})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return obs, set
}

func TestNoTranscriptReadsReadsNoTranscriptAndSaysSoOnEveryJob(t *testing.T) {
	obs, set := noTranscriptCore(t, tRef.Add(-time.Hour))

	// The control: with reads on, the job is timed from its own transcript.
	on := sessionByID(t, everything(t, obs), noTxJob)
	if !on.TranscriptCollected || on.LastActivityAt == nil {
		t.Fatalf("control: transcriptCollected %v lastActivityAt %v, want the timing read to have run",
			on.TranscriptCollected, on.LastActivityAt)
	}
	opensBefore, readsBefore := set.Transcripts.PathCalls(noTxTranscript), set.Transcripts.ReadCalls(noTxTranscript)

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = true
	opts.NoTranscriptReads = true
	snap, err := obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	s := sessionByID(t, snap, noTxJob)

	if s.TranscriptCollected {
		t.Error("transcriptCollected is true under --no-transcript; §5.2 makes it false on every session")
	}
	if s.LastActivityAt != nil || s.StatusSinceLowerBound != nil {
		t.Errorf("lastActivityAt %v statusSinceLowerBound %v, want both null: no transcript was read",
			s.LastActivityAt, s.StatusSinceLowerBound)
	}
	if s.StatusSince == nil || !s.StatusSince.Equal(tRef.Add(-time.Hour)) {
		t.Errorf("statusSince = %v; the record's updatedAt does not depend on a transcript", s.StatusSince)
	}
	var reason any
	for _, a := range s.Advisories {
		if a.Code == observer.AdvStatusSinceUnverifiable {
			reason = a.Fields["reason"]
		}
	}
	if reason != "transcript reads were turned off (--no-transcript)" {
		t.Errorf("status-since-unverifiable reason = %v, want the flag named: the narrowing is "+
			"reported, not silent", reason)
	}
	if hasAdvisory(s, observer.AdvTranscriptAbsent) {
		t.Error("transcript-absent under --no-transcript: we did not look, so we cannot say it is absent")
	}
	for _, src := range s.Sources {
		if src == observer.SourceTranscript {
			t.Error("the transcript is listed as a source of a session none was read for")
		}
	}

	// Not one open, not one stat, not one read.
	if n := set.Transcripts.PathCalls(noTxTranscript) - opensBefore; n != 0 {
		t.Errorf("OpenPath was called %d times under --no-transcript", n)
	}
	if n := set.Transcripts.ReadCalls(noTxTranscript) - readsBefore; n != 0 {
		t.Errorf("the transcript was read %d times under --no-transcript", n)
	}
	if _, ok := snap.Sources[observer.SourceTranscripts]; ok {
		t.Error("a transcripts source report was published for a pass that did not run")
	}
}

// Rule 4 falls back to recordAgeMs: a record past the retention window whose
// transcript EXISTS is not rule 4 when the transcript was looked at, and is
// rule 4 when it was not — presence was never checked, which is §5.2's named
// narrowing. It moves liveness towards null, never towards true or false.
func TestNoTranscriptReadsLetsRule4DecideOnRecordAgeAlone(t *testing.T) {
	obs, _ := noTranscriptCore(t, tRef.Add(-40*24*time.Hour))

	on := sessionByID(t, everything(t, obs), noTxJob)
	if on.LiveReason == observer.LiveReasonJobBeyondRetention {
		t.Fatalf("control: rule 4 fired with the transcript present (%s)", on.LiveReason)
	}

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = true
	opts.NoTranscriptReads = true
	snap, err := obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	off := sessionByID(t, snap, noTxJob)
	if off.LiveReason != observer.LiveReasonJobBeyondRetention || off.Live != nil {
		t.Fatalf("live %v (%s), want null by rule 4 on recordAgeMs alone", off.Live, off.LiveReason)
	}
}
