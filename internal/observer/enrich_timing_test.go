package observer_test

// lastActivityAt for the INTERACTIVE population, through the real core and the
// real transcriptfs adapter.
//
// §3.4 defines lastActivityAt as "the timestamp of the NEWEST timestamped record
// in this session's transcript" for both populations, and gives the registry
// row its source: "from the transcript found by glob, when enrichment ran". The
// independent black-box suite found it computed for background jobs only —
// every interactive session read null with enrichment on — and found a tail
// holding no timestamp nulled WITHOUT the activity-since-truncated advisory AC12
// requires. These pin both, plus the AC3 half that must NOT move: statusSince
// stays the registry's statusUpdatedAt, verbatim, whatever the transcript says.

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/transcriptfs"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

const timingSess = "71717171-0000-4000-8000-000000000001"

// timingCore is one observer with one registry session whose transcript lives
// in fsys under some unrelated slug directory (the glob finds it anyway).
func timingCore(t *testing.T, fsys fstest.MapFS, timingTail int, status string, since time.Time) (*observer.Observer, *countingFS) {
	t.Helper()
	clock := fakes.NewClock(tRef)
	reg := &fakes.Registry{}
	reg.SetRecords(tRef, observer.RegistryRecord{
		Path: "/fixtures/sessions/7171.json", PID: 7171, SessionID: timingSess,
		Status: status, WaitingFor: "input needed", StatusUpdatedAt: &since,
	})
	counting := newCountingFS(fsys)
	obs, err := observer.New(observer.Deps{
		Registry:    reg,
		Jobs:        &fakes.Jobs{},
		Transcripts: transcriptfs.NewFS(counting, fixtureProjectsRoot, clock),
		Proc:        fakes.NewProc(7171),
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}, observer.Config{TimingTailBytes: timingTail, Paths: observer.Paths{Projects: fixtureProjectsRoot}})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return obs, counting
}

func enrichOnce(t *testing.T, obs *observer.Observer) (*observer.Snapshot, *observer.Snapshot) {
	t.Helper()
	base, err := obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	out, err := obs.Enrich(context.Background(), base, nil)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	return base, out
}

const timingRel = "some-unrelated-slug/" + timingSess + ".jsonl"

func TestAnEnrichedRegistrySessionCarriesItsTranscriptsNewestTimestamp(t *testing.T) {
	for _, status := range []string{"idle", "waiting"} {
		t.Run(status, func(t *testing.T) {
			since := tRef.Add(-72 * time.Hour)
			latest := tRef.Add(-time.Minute)
			fsys := fstest.MapFS{timingRel: &fstest.MapFile{
				// The newest timestamp is NOT on the last line: an untimestamped
				// record follows it, as on 4 of 5 measured job transcripts.
				Data: []byte(`{"type":"assistant","timestamp":"` + observer.FormatTime(tRef.Add(-time.Hour)) + `"}` + "\n" +
					`{"type":"assistant","timestamp":"` + observer.FormatTime(latest) + `"}` + "\n" +
					`{"type":"last-prompt"}` + "\n"),
				ModTime: tRef,
			}}
			obs, _ := timingCore(t, fsys, 0, status, since)
			base, out := enrichOnce(t, obs)

			s := sessionByID(t, out, timingSess)
			if s.LastActivityAt == nil || !s.LastActivityAt.Equal(latest) {
				t.Fatalf("lastActivityAt = %v, want the newest timestamped record %s", s.LastActivityAt, latest)
			}
			// AC3: statusSince is the registry's, verbatim — three days old — and
			// is NOT corrected against the transcript a minute ago.
			if s.StatusSince == nil || !s.StatusSince.Equal(since) {
				t.Fatalf("statusSince = %v, want the registry's %s untouched", s.StatusSince, since)
			}
			if want := int64(72 * time.Hour / time.Millisecond); s.StatusAgeMs == nil || *s.StatusAgeMs != want {
				t.Fatalf("statusAgeMs = %v, want %d from statusSince", s.StatusAgeMs, want)
			}
			if s.StatusSinceLowerBound != nil {
				t.Fatalf("statusSinceLowerBound = %v: the cross-check is the background half's", s.StatusSinceLowerBound)
			}
			// The published input is untouched: the field is enrichment's.
			if b := sessionByID(t, base, timingSess); b.LastActivityAt != nil {
				t.Fatal("Enrich wrote lastActivityAt into the snapshot it was given")
			}
			if rep := out.Sources[observer.SourceTranscripts]; rep.Counters["enrichTimingReads"] != 1 {
				t.Fatalf("enrichTimingReads = %d, want 1", rep.Counters["enrichTimingReads"])
			}
		})
	}
}

func TestATimingTailWithNoTimestampIsNullWithTheTruncationAdvisory(t *testing.T) {
	// One timestamped record, then more untimestamped bytes than the tail holds
	// — the black-box suite's shape, with a 512-byte tail standing in for 64 KiB.
	var b strings.Builder
	b.WriteString(`{"type":"assistant","timestamp":"` + observer.FormatTime(tRef) + `"}` + "\n")
	for b.Len() < 4096 {
		b.WriteString(`{"type":"progress","padding":"` + strings.Repeat("x", 100) + `"}` + "\n")
	}
	fsys := fstest.MapFS{timingRel: &fstest.MapFile{Data: []byte(b.String()), ModTime: tRef}}
	obs, _ := timingCore(t, fsys, 512, "idle", tRef.Add(-time.Hour))
	_, out := enrichOnce(t, obs)

	s := sessionByID(t, out, timingSess)
	if s.LastActivityAt != nil {
		t.Fatalf("lastActivityAt = %v; a bounded tail with no timestamp must not invent one", s.LastActivityAt)
	}
	var adv *observer.Advisory
	for i := range s.Advisories {
		if s.Advisories[i].Code == observer.AdvActivitySinceTruncated {
			adv = &s.Advisories[i]
		}
	}
	if adv == nil {
		t.Fatalf("no activity-since-truncated: AC12 is 'reach a timestamp, or null the field WITH "+
			"an advisory' — %+v", s.Advisories)
	}
	if got := adv.Fields["tailBytes"]; got != int64(512) {
		t.Fatalf("tailBytes = %v, want the 512 actually read", got)
	}
}

func TestAWholeFileWithNoTimestampIsNullWithoutAnAdvisory(t *testing.T) {
	// The tail reached offset 0 and there is genuinely no timestamp: a fact
	// about the file, not a truncation.
	fsys := fstest.MapFS{timingRel: &fstest.MapFile{
		Data: []byte(`{"type":"agent-color","agentColor":"green"}` + "\n"), ModTime: tRef,
	}}
	obs, _ := timingCore(t, fsys, 0, "idle", tRef.Add(-time.Hour))
	_, out := enrichOnce(t, obs)

	s := sessionByID(t, out, timingSess)
	if s.LastActivityAt != nil || hasAdvisory(s, observer.AdvActivitySinceTruncated) {
		t.Fatalf("lastActivityAt %v advisories %+v, want null and no truncation advisory",
			s.LastActivityAt, s.Advisories)
	}
}

func TestAnUnchangedTranscriptsTimingIsNotReadTwice(t *testing.T) {
	fsys := fstest.MapFS{timingRel: &fstest.MapFile{
		Data: []byte(`{"type":"assistant","timestamp":"` + observer.FormatTime(tRef) + `"}` + "\n"), ModTime: tRef,
	}}
	obs, counting := timingCore(t, fsys, 0, "idle", tRef.Add(-time.Hour))
	base, _ := enrichOnce(t, obs)
	reads := counting.Reads(timingRel)

	out, err := obs.Enrich(context.Background(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := counting.Reads(timingRel); n != reads {
		t.Fatalf("the transcript's content was read %d more times on a warm cache", n-reads)
	}
	s := sessionByID(t, out, timingSess)
	if s.LastActivityAt == nil || !s.LastActivityAt.Equal(tRef) {
		t.Fatalf("the cached answer is %v, want %s", s.LastActivityAt, tRef)
	}
	if rep := out.Sources[observer.SourceTranscripts]; rep.Counters["enrichTimingCacheHits"] != 1 ||
		rep.Counters["enrichTimingReads"] != 0 {
		t.Fatalf("enrichTimingReads %d enrichTimingCacheHits %d, want 0 and 1",
			rep.Counters["enrichTimingReads"], rep.Counters["enrichTimingCacheHits"])
	}
}
