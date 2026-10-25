package transcriptfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// tRef is §5.2's single reference instant, shared across every suite.
var tRef = time.UnixMilli(1789699211204).UTC()

// projectsRoot is the absolute path the fixtures' linkScanPath values name. It
// is synthetic so the fixtures are self-contained and no test depends on the
// machine it runs on.
const projectsRoot = "/fixtures/projects"

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return observer.TruncateTime(c.t) }

// countingFS records opens by path. The "no glob fallback is attempted" and
// "the deeper subagent population is never touched" assertions are claims about
// which files were READ, and the only way to test a claim like that is to
// count: a reader that opened a file and discarded it produces an identical
// answer.
type countingFS struct {
	inner fs.FS
	mu    sync.Mutex
	opens map[string]int
}

func newCountingFS(inner fs.FS) *countingFS {
	return &countingFS{inner: inner, opens: map[string]int{}}
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.mu.Lock()
	c.opens[name]++
	c.mu.Unlock()
	return c.inner.Open(name)
}

func (c *countingFS) Opens(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens[name]
}

func (c *countingFS) All() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.opens))
	for name := range c.opens {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// fixtures loads testdata/projects into a MapFS.
func fixtures(t *testing.T) fstest.MapFS {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "projects")
	out := fstest.MapFS{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
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
		t.Fatalf("loading the projects fixtures: %v", err)
	}
	return out
}

func newSource(t *testing.T, fsys fs.FS) *Source {
	t.Helper()
	return NewFS(fsys, projectsRoot, fixedClock{tRef})
}

func abs(rel string) string { return projectsRoot + "/" + rel }

// ---------------------------------------------------------------------------
// §8.3 item 9 — the two production constants
// ---------------------------------------------------------------------------

func TestTheProductionTailConstantsAreTheMeasuredOnes(t *testing.T) {
	// These are measurements, not preferences, and §8.3 item 9 requires a test
	// that says so:
	//
	//   64 KiB timing: the newest TIMESTAMPED record sits at most 1 912 B from
	//                  EOF over the ten most-recently-written transcripts. 33x.
	//  200 KiB facts:  the agent-color record was found in all 35 transcripts
	//                  that carry one, 0 misses, largest file 30.7 MB.
	if TimingTailBytes != 64<<10 {
		t.Errorf("TimingTailBytes = %d, want %d", TimingTailBytes, 64<<10)
	}
	if FactsTailBytes != 200<<10 {
		t.Errorf("FactsTailBytes = %d, want %d", FactsTailBytes, 200<<10)
	}
}

// ---------------------------------------------------------------------------
// §8.3 item 10 — OpenPath, the second locator
// ---------------------------------------------------------------------------

func TestOpenPathFindsAJobsOwnTranscript(t *testing.T) {
	src := newSource(t, fixtures(t))
	loc, err := src.OpenPath(context.Background(),
		abs("-home-user-dotfiles/97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01.jsonl"))
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	if !loc.Found {
		t.Fatal("found = false for a fixture that exists")
	}
	if loc.SizeBytes == 0 {
		t.Error("sizeBytes = 0")
	}
	if !filepath.IsAbs(loc.Path) {
		t.Errorf("path = %q, want the absolute path so the advisory can name it", loc.Path)
	}
}

func TestAReapedTranscriptIsNotFoundAndIsNotAnError(t *testing.T) {
	// The measured case for 7a324a6d: the project directory still holds 31
	// other transcripts and only this session's file is gone, reaped by the
	// vendor's 30-day cleanup. That is §3.5 rule 4's input, so it must arrive
	// as a fact rather than as a failure.
	src := newSource(t, fixtures(t))
	target := abs("-home-user-dotfiles/7a324a6d-27ec-4431-b17b-70ab8ac764c7.jsonl")

	loc, err := src.OpenPath(context.Background(), target)
	if err != nil {
		t.Fatalf("a reaped transcript produced an error: %v", err)
	}
	if loc.Found {
		t.Error("found = true for a reaped transcript")
	}
	if loc.Path != target {
		t.Errorf("path = %q, want the target so transcript-absent can name it", loc.Path)
	}
}

func TestAWholeMissingProjectDirectoryIsAlsoJustNotFound(t *testing.T) {
	// The measured case for ea780079: its worktree was deleted, so the project
	// directory is gone too. A different shape from a reaped file, the same
	// answer.
	src := newSource(t, fixtures(t))
	loc, err := src.OpenPath(context.Background(),
		abs("-home-user-mag-claudish--claude-worktrees-resume-message/4d09bc19-27e6-4f6e-9512-0f7711a6b970.jsonl"))
	if err != nil {
		t.Fatalf("a missing project directory produced an error: %v", err)
	}
	if loc.Found {
		t.Error("found = true")
	}
}

func TestAPathOutsideTheProjectsRootIsRefused(t *testing.T) {
	// A record is UNTRUSTED INPUT. linkScanPath is an absolute path we did not
	// construct, in a file we do not own, and following it wherever it points
	// turns "read a bounded transcript tail" into "read a bounded tail of any
	// file on this machine".
	src := newSource(t, fixtures(t))

	for _, p := range []string{
		"/etc/passwd",
		projectsRoot + "/../../../etc/passwd",
		projectsRoot + "/-home-user-dotfiles/../../../../etc/passwd",
		"/fixtures/projects-evil/x.jsonl", // shares a string prefix, different tree
		"relative/path.jsonl",
		"../escape.jsonl",
	} {
		loc, err := src.OpenPath(context.Background(), p)
		if !errors.Is(err, ErrOutsideProjectsRoot) {
			t.Errorf("OpenPath(%q) err = %v, want ErrOutsideProjectsRoot", p, err)
		}
		if loc.Found {
			t.Errorf("OpenPath(%q) reported found", p)
		}
	}
}

func TestARefusedPathOpensNothingAtAllAndFallsBackToNoGlob(t *testing.T) {
	// §7.3: "a path outside it is refused with transcript-absent", and NO glob
	// fallback is attempted. A fallback would turn a refused path into a
	// NEIGHBOURING session's transcript — the dotfiles slug alone holds 31
	// unrelated ones — and publish another conversation's last turn as this
	// job's last activity. The assertion is on opens, because the wrong
	// behaviour returns a perfectly well-formed answer.
	counting := newCountingFS(fixtures(t))
	src := newSource(t, counting)

	if _, err := src.OpenPath(context.Background(), "/etc/passwd"); err == nil {
		t.Fatal("a path outside the root was accepted")
	}
	if opened := counting.All(); len(opened) != 0 {
		t.Errorf("a refused path opened %v", opened)
	}

	// And the same for a target that is inside the root but absent: the
	// neighbour must not be read.
	if _, err := src.OpenPath(context.Background(),
		abs("-home-user-dotfiles/7a324a6d-27ec-4431-b17b-70ab8ac764c7.jsonl")); err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	const neighbour = "-home-user-dotfiles/unrelated-neighbour-0000-4000-8000-000000000000.jsonl"
	if n := counting.Opens(neighbour); n != 0 {
		t.Errorf("the neighbouring transcript was opened %d times after an absent target", n)
	}
}

func TestAnEmptyPathIsNeitherFoundNorAnError(t *testing.T) {
	// "The record names no transcript" is a legible state, not a failure.
	loc, err := newSource(t, fixtures(t)).OpenPath(context.Background(), "   ")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if loc.Found {
		t.Error("found = true for an empty path")
	}
}

func TestADirectoryAtThePathIsNotFound(t *testing.T) {
	src := newSource(t, fixtures(t))
	loc, err := src.OpenPath(context.Background(), abs("-home-user-dotfiles"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if loc.Found {
		t.Error("a directory was reported as a located transcript")
	}
}

// ---------------------------------------------------------------------------
// the 64 KB timing tail
// ---------------------------------------------------------------------------

func TestTheNewestTIMESTAMPEDRecordWinsAndTheLastLineDoesNot(t *testing.T) {
	// §0.4d, and this is the test the naive implementation fails. Eight record
	// types carry no `timestamp` key, and on 4 of the 5 real job transcripts the
	// FINAL LINE IS ONE OF THEM. proj-f is that shape: its last three records
	// are cost-state, file-history-snapshot and last-prompt, and the answer is
	// three records back.
	src := newSource(t, fixtures(t))
	loc, err := src.OpenPath(context.Background(),
		abs("-home-user-proj-f/ffffffff-ffff-4fff-8fff-ffffffffffff.jsonl"))
	if err != nil || !loc.Found {
		t.Fatalf("locating proj-f: %v / found %v", err, loc.Found)
	}

	facts, err := src.ReadTiming(context.Background(), loc, 0)
	if err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	if facts.LastActivityAt == nil {
		t.Fatal("lastActivityAt is nil; a last-line reader returns exactly this")
	}
	const want = "2026-09-18T02:35:12.345Z"
	if got := observer.FormatTime(*facts.LastActivityAt); got != want {
		t.Errorf("lastActivityAt = %s, want %s", got, want)
	}
}

func TestATailThatReachesNoTimestampedRecordSaysSoRatherThanGuessing(t *testing.T) {
	// proj-g is SYNTHETIC: 78 KB of untimestamped trailing records. With an
	// injected 2 KB tail the read reaches none of them, and the honest answer
	// is null plus ReachedStart false — which the core turns into
	// activity-since-truncated. At the production 64 KiB against a measured
	// 1 912 B worst case this path is a guard, not a route.
	src := newSource(t, fixtures(t))
	loc, err := src.OpenPath(context.Background(),
		abs("-home-user-proj-g/99999999-9999-4999-8999-999999999999.jsonl"))
	if err != nil || !loc.Found {
		t.Fatalf("locating proj-g: %v / found %v", err, loc.Found)
	}

	facts, err := src.ReadTiming(context.Background(), loc, 2048)
	if err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	if facts.LastActivityAt != nil {
		t.Errorf("lastActivityAt = %s, want nil", observer.FormatTime(*facts.LastActivityAt))
	}
	if facts.ReachedStart {
		t.Error("reachedStart = true; the tail did not reach offset 0")
	}
	if facts.TailBytes > 2048 {
		t.Errorf("tailBytes = %d, want no more than the injected 2048", facts.TailBytes)
	}

	// This fixture is the ONE case where the guard is reachable at the
	// production size too — it is 78 KB of untimestamped records, deliberately
	// larger than the 64 KiB tail — so the same answer comes back there. That
	// is the assertion, not an accident: the guard has to exist precisely
	// because a file CAN be shaped this way, even though no real one measured
	// worse than 1 912 B.
	full, err := src.ReadTiming(context.Background(), loc, 0)
	if err != nil {
		t.Fatalf("ReadTiming at the production size: %v", err)
	}
	if full.LastActivityAt != nil {
		t.Errorf("lastActivityAt = %s at the production size; the fixture is no longer larger than the tail",
			observer.FormatTime(*full.LastActivityAt))
	}
	if full.ReachedStart {
		t.Error("reachedStart = true at the production size; the fixture has shrunk below 64 KiB")
	}
}

func TestAWiderTailFindsWhatANarrowOneMissed(t *testing.T) {
	// The complement of the previous test, and what makes it about the BOUND
	// rather than about the file: one file, two tail sizes, two answers.
	data := []byte(`{"type":"user","timestamp":"2026-01-01T00:00:00.000Z"}` + "\n" +
		strings.Repeat(`{"type":"atis-latch","uuid":"`+strings.Repeat("p", 60)+`"}`+"\n", 40))
	fsys := fstest.MapFS{"p/t.jsonl": &fstest.MapFile{Data: data}}
	src := newSource(t, fsys)
	loc, _ := src.OpenPath(context.Background(), abs("p/t.jsonl"))

	narrow, err := src.ReadTiming(context.Background(), loc, 256)
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if narrow.LastActivityAt != nil || narrow.ReachedStart {
		t.Errorf("a 256 B tail should have truncated: %+v", narrow)
	}

	wide, err := src.ReadTiming(context.Background(), loc, 0)
	if err != nil {
		t.Fatalf("wide: %v", err)
	}
	if wide.LastActivityAt == nil {
		t.Fatal("the production tail missed a timestamp inside it")
	}
	if got := observer.FormatTime(*wide.LastActivityAt); got != "2026-01-01T00:00:00.000Z" {
		t.Errorf("lastActivityAt = %s", got)
	}
}

func TestAShortFileReachesTheStartAndReportsIt(t *testing.T) {
	// ReachedStart is what distinguishes "this file genuinely has no
	// timestamps" from "we did not read far enough back", and only the second
	// earns an advisory.
	fsys := fstest.MapFS{
		"p/none.jsonl": &fstest.MapFile{Data: []byte(`{"type":"last-prompt"}` + "\n")},
	}
	src := newSource(t, fsys)
	loc, err := src.OpenPath(context.Background(), abs("p/none.jsonl"))
	if err != nil || !loc.Found {
		t.Fatalf("OpenPath: %v / %v", err, loc.Found)
	}

	facts, err := src.ReadTiming(context.Background(), loc, 0)
	if err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	if facts.LastActivityAt != nil {
		t.Error("lastActivityAt is set")
	}
	if !facts.ReachedStart {
		t.Error("reachedStart = false for a file smaller than the tail")
	}
}

func TestAPartialFirstLineIsDiscardedUnlessTheStartWasReached(t *testing.T) {
	// A tail taken mid-file almost always begins in the middle of a record.
	// The first line is dropped so a fragment can never be read as a record —
	// here the fragment would otherwise decode to a NEWER timestamp than the
	// genuine last record, which is the shape of the bug.
	good := `{"type":"user","timestamp":"2026-01-01T00:00:00.000Z"}`
	fragment := `RBAGE","timestamp":"2099-01-01T00:00:00.000Z"}`
	data := []byte(strings.Repeat("x", 64) + "\n" + fragment + "\n" + good + "\n")

	fsys := fstest.MapFS{"p/t.jsonl": &fstest.MapFile{Data: data}}
	src := newSource(t, fsys)
	loc, err := src.OpenPath(context.Background(), abs("p/t.jsonl"))
	if err != nil || !loc.Found {
		t.Fatalf("OpenPath: %v", err)
	}

	facts, err := src.ReadTiming(context.Background(), loc, 100)
	if err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	if facts.LastActivityAt == nil {
		t.Fatal("lastActivityAt is nil")
	}
	if got := observer.FormatTime(*facts.LastActivityAt); got != "2026-01-01T00:00:00.000Z" {
		t.Errorf("lastActivityAt = %s; a partial line was read as a record", got)
	}
}

func TestAMalformedLineIsSkippedRatherThanFatal(t *testing.T) {
	data := []byte(strings.Join([]string{
		`{"type":"user","timestamp":"2026-01-01T00:00:00.000Z"}`,
		`{not json at all`,
		`{"type":"cost-state"}`,
	}, "\n") + "\n")
	fsys := fstest.MapFS{"p/t.jsonl": &fstest.MapFile{Data: data}}
	src := newSource(t, fsys)
	loc, _ := src.OpenPath(context.Background(), abs("p/t.jsonl"))

	facts, err := src.ReadTiming(context.Background(), loc, 0)
	if err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	if facts.LastActivityAt == nil {
		t.Error("one malformed line blanked the field")
	}
}

func TestReadTimingNeverConsultsMTime(t *testing.T) {
	// Measured: mtime and the newest record's timestamp differ by up to
	// 1 h 37 m on real job transcripts, because the file is appended to after
	// the last message is timestamped. §3.4/AC3 forbid correcting a timestamp
	// against a file's mtime, and this asserts the timing read does not sneak
	// one in: the same bytes with a wildly different mtime give the same answer.
	data := []byte(`{"type":"user","timestamp":"2026-01-01T00:00:00.000Z"}` + "\n")
	early := fstest.MapFS{"p/t.jsonl": &fstest.MapFile{
		Data: data, ModTime: time.Unix(1, 0).UTC()}}
	late := fstest.MapFS{"p/t.jsonl": &fstest.MapFile{
		Data: data, ModTime: tRef}}

	var answers []string
	for _, fsys := range []fstest.MapFS{early, late} {
		src := newSource(t, fsys)
		loc, _ := src.OpenPath(context.Background(), abs("p/t.jsonl"))
		facts, err := src.ReadTiming(context.Background(), loc, 0)
		if err != nil {
			t.Fatalf("ReadTiming: %v", err)
		}
		if facts.LastActivityAt == nil {
			t.Fatal("lastActivityAt is nil")
		}
		answers = append(answers, observer.FormatTime(*facts.LastActivityAt))
	}
	if answers[0] != answers[1] {
		t.Errorf("mtime changed the answer: %v", answers)
	}
}

func TestReadTimingOnAnAbsentTargetIsEmptyAndNotAnError(t *testing.T) {
	facts, err := newSource(t, fixtures(t)).ReadTiming(
		context.Background(), observer.TranscriptLocation{}, 0)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if facts != (observer.TimingFacts{}) {
		t.Errorf("facts = %+v, want the zero value", facts)
	}
}

func TestReadTimingRefusesALocationOutsideTheRoot(t *testing.T) {
	// A Found location whose path is outside the root can only come from a
	// caller that built it by hand; refusing it keeps the containment rule a
	// property of the adapter rather than of the call order.
	_, err := newSource(t, fixtures(t)).ReadTiming(context.Background(),
		observer.TranscriptLocation{Found: true, Path: "/etc/passwd"}, 0)
	if !errors.Is(err, ErrOutsideProjectsRoot) {
		t.Errorf("err = %v, want ErrOutsideProjectsRoot", err)
	}
}

// The enrichment pair — FindByGlob and ReadFacts — is P6's and its suite lives
// in facts_test.go. §7.3 keeps the two reads apart; the tests are separate for
// the same reason.

// ---------------------------------------------------------------------------
// the retention floor, and the glob that must stay non-recursive
// ---------------------------------------------------------------------------

func TestTheRetentionFloorIsDerivedFromTheNonRecursiveGlobAndNotFromAWalk(t *testing.T) {
	// THE r3 M3 REGRESSION TEST, and the numbers are live measurements taken
	// from this machine on 2026-09-18:
	//
	//   projects/*/*.jsonl      3 181 files   oldest  30.11 d   past 30 d:   2
	//   recursive               4 981 files   oldest 105.68 d   past 30 d: 335
	//
	// The vendor does not reap subagents/ at all. So a WalkDir or `**`
	// re-derivation returns 105 days and SILENTLY DISABLES §3.5 rule 4 for
	// three months — from a change that looks like a tidy-up. This fixture has
	// the same shape at 1/1000 the scale.
	const day = 24 * time.Hour
	fsys := fstest.MapFS{
		"-slug-a/aaaa.jsonl": &fstest.MapFile{
			Data: []byte("{}\n"), ModTime: tRef.Add(-10 * day)},
		"-slug-a/bbbb.jsonl": &fstest.MapFile{
			Data: []byte("{}\n"), ModTime: tRef.Add(-2 * day)},
		// Depth 4. Older than any session transcript, and NOT reaped. A
		// recursive derivation returns THIS file's age and the rule dies.
		"-slug-a/aaaa/subagents/agent-1.jsonl": &fstest.MapFile{
			Data: []byte("{}\n"), ModTime: tRef.Add(-105 * day)},
		// Depth 1. Not a session transcript either; the glob requires two
		// segments, so a stray file at the root is out of scope too.
		"stray.jsonl": &fstest.MapFile{
			Data: []byte("{}\n"), ModTime: tRef.Add(-200 * day)},
	}

	counting := newCountingFS(fsys)
	src := newSource(t, counting)
	floor, err := src.DeriveFloor(context.Background(), tRef, observer.DefaultTranscriptRetentionMs)
	if err != nil {
		t.Fatalf("DeriveFloor: %v", err)
	}

	if floor.Files != 2 {
		t.Errorf("files = %d, want 2 — only the depth-2 population is in scope", floor.Files)
	}
	wantOldest := (10 * day).Milliseconds()
	if floor.OldestAgeMs != wantOldest {
		t.Errorf("oldestAgeMs = %d (%s), want %d — a recursive glob returns 105 days here",
			floor.OldestAgeMs, floor.OldestPath, wantOldest)
	}
	if floor.PastRetention != 0 {
		t.Errorf("pastRetention = %d, want 0", floor.PastRetention)
	}
	if n := counting.Opens("-slug-a/aaaa/subagents/agent-1.jsonl"); n != 0 {
		t.Errorf("the subagent transcript was touched %d times", n)
	}
	if floor.OldestPath == "stray.jsonl" {
		t.Error("a depth-1 file was treated as a session transcript")
	}
	// Note on what is NOT asserted here: fs.Glob expands a two-segment pattern
	// by listing the root and then attempting a ReadDir of each entry, so
	// "stray.jsonl" receives one failed directory-listing attempt. That is a
	// listing, not a read of its contents, and it is fs.Glob's business rather
	// than this adapter's. The load-bearing assertion is the one above: the
	// subagent transcript is never touched at all, because the pattern's base
	// segment cannot match a directory name and so nothing ever descends.
}

func TestTheGlobIsTwoSegmentsAndCarriesNoRecursiveWildcard(t *testing.T) {
	// The constant is normative. Asserting its SHAPE rather than its value is
	// what catches the plausible-looking edit ("projects/**/*.jsonl") that a
	// value comparison would also catch but that nobody would think to write a
	// value comparison for.
	if strings.Contains(SessionTranscriptGlob, "**") {
		t.Errorf("the glob %q is recursive", SessionTranscriptGlob)
	}
	if got := strings.Count(SessionTranscriptGlob, "/"); got != 1 {
		t.Errorf("the glob %q has %d separators, want exactly 1 (a two-segment pattern)",
			SessionTranscriptGlob, got)
	}
}

func TestTheFloorFlagsATreeWhoseCleanupHasStopped(t *testing.T) {
	const day = 24 * time.Hour
	fsys := fstest.MapFS{
		"-slug/a.jsonl": &fstest.MapFile{Data: []byte("{}\n"), ModTime: tRef.Add(-40 * day)},
		"-slug/b.jsonl": &fstest.MapFile{Data: []byte("{}\n"), ModTime: tRef.Add(-90 * day)},
		"-slug/c.jsonl": &fstest.MapFile{Data: []byte("{}\n"), ModTime: tRef.Add(-1 * day)},
	}
	floor, err := newSource(t, fsys).DeriveFloor(
		context.Background(), tRef, observer.DefaultTranscriptRetentionMs)
	if err != nil {
		t.Fatalf("DeriveFloor: %v", err)
	}
	if floor.PastRetention != 2 {
		t.Errorf("pastRetention = %d, want 2 — rule 4's premise has changed and nobody would know",
			floor.PastRetention)
	}
}

// ---------------------------------------------------------------------------
// the vendor's own retention setting
// ---------------------------------------------------------------------------

func TestRetentionMsReadsCleanupPeriodDaysWhenItIsSet(t *testing.T) {
	fsys := fstest.MapFS{
		SettingsFile: &fstest.MapFile{Data: []byte(`{"cleanupPeriodDays": 7, "other": true}`)},
	}
	ms, ok := RetentionMs(fsys)
	if !ok {
		t.Fatal("ok = false for a settings file that sets the key")
	}
	if want := int64(7 * 24 * 60 * 60 * 1000); ms != want {
		t.Errorf("ms = %d, want %d", ms, want)
	}
}

func TestRetentionMsFallsBackWhenTheKeyIsAbsentUnreadableOrNonsense(t *testing.T) {
	// MEASURED: the key is ABSENT on this machine, so the vendor's documented
	// 30-day default is in force — which the 30.11-day floor confirms
	// independently. Every one of these cases must leave the caller on the
	// default rather than on a number derived from something we do not
	// understand.
	//
	// Zero and negative are rejected on purpose: `cleanupPeriodDays: 0` read
	// literally makes EVERY background record "beyond retention" and answers
	// live: null for the whole population.
	cases := map[string]fs.FS{
		"absent file":  fstest.MapFS{},
		"absent key":   fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`{"theme":"dark"}`)}},
		"not json":     fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`not json`)}},
		"null":         fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`{"cleanupPeriodDays":null}`)}},
		"a string":     fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`{"cleanupPeriodDays":"30"}`)}},
		"zero":         fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`{"cleanupPeriodDays":0}`)}},
		"negative":     fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`{"cleanupPeriodDays":-1}`)}},
		"a fractional": fstest.MapFS{SettingsFile: &fstest.MapFile{Data: []byte(`{"cleanupPeriodDays":1.5}`)}},
		"nil fs":       nil,
	}
	for name, fsys := range cases {
		if _, ok := RetentionMs(fsys); ok {
			t.Errorf("%s: ok = true, want false so the caller keeps the default", name)
		}
	}
}

func TestTheDefaultRetentionIsTheVendorsDocumentedThirtyDays(t *testing.T) {
	if want := int64(30 * 24 * 60 * 60 * 1000); observer.DefaultTranscriptRetentionMs != want {
		t.Errorf("DefaultTranscriptRetentionMs = %d, want %d",
			observer.DefaultTranscriptRetentionMs, want)
	}
}

// ---------------------------------------------------------------------------
// what never leaves a transcript
// ---------------------------------------------------------------------------

func TestOnlyTheTimestampIsReadOutOfATranscriptRecord(t *testing.T) {
	// The timing pass reads ONE key. A record carrying a full conversation must
	// produce a timestamp and nothing else — the canaries here are synthetic
	// and deliberately not in testdata/, so the fixture privacy audit stays
	// able to assert those key names appear nowhere under it.
	record := map[string]any{
		"type":       "assistant",
		"timestamp":  "2026-09-18T02:39:11.204Z",
		"message":    map[string]any{"content": "CANARY-CONTENT"},
		"lastPrompt": "CANARY-PROMPT",
		"text":       "CANARY-TEXT",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"p/t.jsonl": &fstest.MapFile{Data: append(data, '\n')}}
	src := newSource(t, fsys)
	loc, _ := src.OpenPath(context.Background(), abs("p/t.jsonl"))

	facts, err := src.ReadTiming(context.Background(), loc, 0)
	if err != nil {
		t.Fatalf("ReadTiming: %v", err)
	}
	rendered := fmt.Sprintf("%+v", facts)
	for _, canary := range []string{"CANARY-CONTENT", "CANARY-PROMPT", "CANARY-TEXT"} {
		if strings.Contains(rendered, canary) {
			t.Errorf("%s crossed the boundary: %s", canary, rendered)
		}
	}
	if facts.LastActivityAt == nil {
		t.Error("the timestamp was not read")
	}
}

func TestACancelledContextStopsBothReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := newSource(t, fixtures(t))
	if _, err := src.OpenPath(ctx, abs("x/y.jsonl")); err == nil {
		t.Error("OpenPath ignored a cancelled context")
	}
	if _, err := src.ReadTiming(ctx, observer.TranscriptLocation{Found: true, Path: abs("x/y.jsonl")}, 0); err == nil {
		t.Error("ReadTiming ignored a cancelled context")
	}
}

func TestTheAdaptersAndTheCoresTimingConstantsCannotDrift(t *testing.T) {
	// The bound is declared twice — here, where the read happens, and in the
	// core, which has to PUBLISH the bound that was in force
	// (sources.transcripts.tailBytesTiming) and cannot import an adapter to ask.
	// Two declarations of one measurement is a drift hazard, so it is pinned
	// rather than commented.
	if TimingTailBytes != observer.DefaultTimingTailBytes {
		t.Errorf("transcriptfs.TimingTailBytes = %d but observer.DefaultTimingTailBytes = %d; "+
			"the published bound would not be the bound that was used",
			TimingTailBytes, observer.DefaultTimingTailBytes)
	}
}
