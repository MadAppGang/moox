package jobsfs

import (
	"context"
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

// tRef is §5.2's single reference instant, shared with the core's tests so
// every age in both suites is exact arithmetic against the same clock.
var tRef = time.UnixMilli(1789699211204).UTC()

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return observer.TruncateTime(c.t) }

// countingFS records every Open and every Stat by path. Three of this
// package's assertions are about which files were TOUCHED rather than about the
// answer — "the depth-6 decoy is never read", "timeline.jsonl is never opened",
// "pins.json is never opened" — and none of them can be tested by inspecting a
// result, because a reader that opened and discarded a file would produce the
// same result.
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

func (c *countingFS) OpenedMatching(substr string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, n := range c.opens {
		if n > 0 && strings.Contains(name, substr) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// fixtures loads testdata/jobs into a MapFS. The whole tree is loaded, decoys
// and all: a test that only loaded the files it expected to be read could not
// discover that something else was.
func fixtures(t *testing.T) fstest.MapFS {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "jobs")
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
		t.Fatalf("loading the jobs fixtures: %v", err)
	}
	return out
}

func read(t *testing.T, fsys fs.FS) observer.JobsResult {
	t.Helper()
	res, err := NewFS(fsys, "/fixtures/jobs", fixedClock{tRef}).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return res
}

func recordFor(t *testing.T, res observer.JobsResult, dir string) observer.JobRecord {
	t.Helper()
	for _, r := range res.Records {
		if r.Dir == dir {
			return r
		}
	}
	t.Fatalf("no record from directory %q; got %v", dir, dirsOf(res))
	return observer.JobRecord{}
}

func dirsOf(res observer.JobsResult) []string {
	out := make([]string, 0, len(res.Records))
	for _, r := range res.Records {
		out = append(out, r.Dir)
	}
	return out
}

// ---------------------------------------------------------------------------
// the three counts, and why they are three
// ---------------------------------------------------------------------------

func TestEntriesDirsAndRecordsAreThreeDifferentNumbers(t *testing.T) {
	// M3's correction. The real tree has 15 entries, 14 of which are
	// directories and one of which — pins.json — is a FILE; 7 of the 14 carry a
	// state.json. Conflating "entries" with "directories" is what produced the
	// wrong number in measured fact 13, and reporting only one of the three
	// makes the others unrecoverable from the output.
	res := read(t, fixtures(t))

	if res.Entries <= res.Dirs {
		t.Errorf("entries %d is not greater than dirs %d; the root file is being counted as a directory",
			res.Entries, res.Dirs)
	}
	if len(res.Records) >= res.Dirs {
		t.Errorf("records %d is not fewer than dirs %d; the directories with no state.json are being counted as records",
			len(res.Records), res.Dirs)
	}
	if res.NonTerminal >= len(res.Records) {
		t.Errorf("nonTerminal %d is not fewer than records %d; the done/failed fixtures are missing",
			res.NonTerminal, len(res.Records))
	}
}

func TestPinsJSONIsNeverEnumeratedAsAJobAndNeverOpened(t *testing.T) {
	counting := newCountingFS(fixtures(t))
	res := read(t, counting)

	for _, r := range res.Records {
		if r.Dir == "pins.json" {
			t.Fatal("pins.json was enumerated as a job directory")
		}
	}
	if n := counting.Opens("pins.json"); n != 0 {
		t.Errorf("pins.json was opened %d times; it is not ours", n)
	}
}

func TestADirectoryWithNoStateJSONIsNotAParseError(t *testing.T) {
	// 7 of the 14 real directories look like this, the oldest dating from
	// 4 June. Counting them as failures would make a healthy machine report
	// seven parse errors forever, which is how a real error stops being
	// noticeable.
	res := read(t, fixtures(t))

	for _, f := range res.Failures {
		if strings.Contains(f.Path, "no-state-json") || strings.Contains(f.Path, "missing-state-json-transient") {
			t.Errorf("an absent state.json was reported as a failure: %+v", f)
		}
	}
	if res.Dirs-len(res.Records) < 2 {
		t.Errorf("dirs %d minus records %d should leave the record-less fixtures", res.Dirs, len(res.Records))
	}
}

// ---------------------------------------------------------------------------
// depth 1, and the adversarial decoy
// ---------------------------------------------------------------------------

func TestTheDepthSixDecoyIsNeverRead(t *testing.T) {
	// M13. The real tree carries two state.json decoys at depths 5 and 8 under
	// be9663df/tmp/, one of which has both a `state` and a `sessionId` key and
	// would therefore parse into a plausible-looking background agent. A
	// WalkDir reader reports 9 agents on this machine, two of them fabricated.
	//
	// The assertion is on OPENS, not on the result: a reader that opened the
	// decoy and then rejected it by depth would produce an identical result and
	// would still be one refactor away from the bug.
	counting := newCountingFS(fixtures(t))
	res := read(t, counting)

	const decoy = "decoy-nested/tmp/a/b/c/.claude/state.json"
	if n := counting.Opens(decoy); n != 0 {
		t.Errorf("the depth-6 decoy was opened %d times", n)
	}
	if opened := counting.OpenedMatching("/tmp/"); len(opened) != 0 {
		t.Errorf("files under a job's tmp/ were opened: %v", opened)
	}
	for _, r := range res.Records {
		if strings.HasPrefix(r.SessionID, "decoy") {
			t.Fatalf("the decoy was reported as a session: %+v", r)
		}
	}
}

func TestOnlyStateJSONIsEverOpened(t *testing.T) {
	// The structural form of the previous test: the ONLY file name this adapter
	// passes to Open is "state.json", at depth 1. That covers the decoy, the
	// timeline, pins.json and anything a future vendor drops into the tree,
	// without the adapter having to know about any of them.
	counting := newCountingFS(fixtures(t))
	read(t, counting)

	for _, name := range counting.OpenedMatching("") {
		if name == "." {
			continue // the ReadDir of the root
		}
		if filepath.Base(name) != StateFile {
			t.Errorf("opened %q; the only permitted name is %q", name, StateFile)
		}
		if strings.Count(name, "/") != 1 {
			t.Errorf("opened %q at depth %d; depth 1 is the rule", name, strings.Count(name, "/"))
		}
	}
}

func TestTimelineJSONLIsNeverOpenedEvenWhenPresent(t *testing.T) {
	// It is an append-only log of SOME events, not a transition history: one
	// record's current state does not appear in its own timeline at all, and 2
	// of 7 records were rewritten days after their last entry. The fixture
	// deliberately includes one so that "we do not read it" is a tested
	// property rather than an absent file.
	fsys := fixtures(t)
	if _, ok := fsys["blocked-with-needs/"+TimelineFile]; !ok {
		t.Fatalf("the fixture is missing its %s, so this test would pass vacuously", TimelineFile)
	}

	counting := newCountingFS(fsys)
	read(t, counting)

	if opened := counting.OpenedMatching(TimelineFile); len(opened) != 0 {
		t.Errorf("%s was opened: %v", TimelineFile, opened)
	}
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

func TestKeyOrderIsIrrelevant(t *testing.T) {
	// Measured fact 12: key order is not stable. The record is decoded by key
	// NAME over a map, so a reordering cannot change the answer — and §5.1's
	// json-tag ban means a typed decode was never available anyway.
	const a = `{"sessionId":"aaaa","state":"blocked","updatedAt":"2026-08-17T08:09:36.697Z","daemonShort":"x"}`
	const b = `{"daemonShort":"x","updatedAt":"2026-08-17T08:09:36.697Z","state":"blocked","sessionId":"aaaa"}`

	first, err := parseRecord("d", "/p", []byte(a))
	if err != nil {
		t.Fatalf("parsing a: %v", err)
	}
	second, err := parseRecord("d", "/p", []byte(b))
	if err != nil {
		t.Fatalf("parsing b: %v", err)
	}
	if first.SessionID != second.SessionID || first.State != second.State ||
		!first.UpdatedAt.Equal(*second.UpdatedAt) {
		t.Errorf("key order changed the answer:\n a = %+v\n b = %+v", first, second)
	}
}

func TestARecordWithNoSessionIdIsAParseErrorNotAnInventedId(t *testing.T) {
	// The join key lives INSIDE the record and is never derived from the
	// directory name — the directory is an 8-char prefix, so a UUID cannot be
	// reconstructed from it and inventing one is the bug class measured fact 6
	// records. The other records in the same read are unaffected.
	res := read(t, fixtures(t))

	if res.ParseErrors == 0 {
		t.Fatal("no parse errors; the no-session-id and torn fixtures should each produce one")
	}
	var named bool
	for _, f := range res.Failures {
		if strings.Contains(f.Path, "no-session-id") {
			named = true
			if !strings.Contains(f.Error, "sessionId") {
				t.Errorf("the failure does not name the missing key: %q", f.Error)
			}
		}
	}
	if !named {
		t.Errorf("the no-session-id fixture is not among the failures: %+v", res.Failures)
	}
	if len(res.Records) < 5 {
		t.Errorf("only %d records survived a parse error; one malformed record must never blank the set", len(res.Records))
	}
}

func TestATornRecordIsCountedAndDoesNotBlankTheSet(t *testing.T) {
	res := read(t, fixtures(t))

	var found bool
	for _, f := range res.Failures {
		if strings.Contains(f.Path, "torn") {
			found = true
			if f.Bytes == 0 {
				t.Error("the failure reports 0 bytes read; the byte count is the debug trace")
			}
		}
	}
	if !found {
		t.Errorf("the torn fixture is not among the failures: %+v", res.Failures)
	}
	// And the neighbours are all still there — the assertion the "one malformed
	// record blanked everything" bug this repository has already shipped once
	// would have failed.
	recordFor(t, res, "done")
	recordFor(t, res, "blocked-with-needs")
}

func TestAnUnrecognisedStateIsKeptVerbatimRatherThanRefused(t *testing.T) {
	// A vendor adding a fifth state must not delete three background agents
	// from the output. The literal is carried and MapJobState answers unknown,
	// which §3.5 rule 7 reads as live: null.
	res := read(t, fixtures(t))
	rec := recordFor(t, res, "unknown-state")

	if rec.State != "reticulating" {
		t.Errorf("state = %q, want the literal preserved", rec.State)
	}
	if st, _ := observer.MapJobState(rec.State); st != observer.StatusUnknown {
		t.Errorf("mapped status = %q, want unknown", st)
	}
}

func TestTimestampsAreParsedFromRFC3339AndTruncatedToMillis(t *testing.T) {
	// Job records carry RFC3339-with-millis strings, where the registry carries
	// epoch millis. Both are converted ONCE, in their adapter, so nothing
	// downstream has to know which source a timestamp came from — and both
	// truncate, because millisecond precision is a domain rule and a value with
	// more precision would not survive the wire round trip.
	res := read(t, fixtures(t))
	rec := recordFor(t, res, "blocked-with-needs")

	if rec.UpdatedAt == nil {
		t.Fatal("updatedAt is nil")
	}
	want := "2026-08-25T03:15:47.319Z"
	if got := observer.FormatTime(*rec.UpdatedAt); got != want {
		t.Errorf("updatedAt = %s, want %s", got, want)
	}
	if rec.UpdatedAt.Location() != time.UTC {
		t.Errorf("updatedAt is not UTC: %v", rec.UpdatedAt.Location())
	}
	if rec.UpdatedAt.Truncate(time.Millisecond) != *rec.UpdatedAt {
		t.Error("updatedAt carries sub-millisecond precision the wire cannot round-trip")
	}
}

func TestAnUnparseableTimestampIsNullNotTheZeroTime(t *testing.T) {
	rec, err := parseRecord("d", "/p", []byte(`{"sessionId":"a","updatedAt":"not a time","createdAt":0}`))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if rec.UpdatedAt != nil {
		t.Errorf("updatedAt = %v, want nil; year 1 is a wrong answer where null is an honest one", rec.UpdatedAt)
	}
	if rec.CreatedAt != nil {
		t.Errorf("createdAt = %v, want nil", rec.CreatedAt)
	}
}

func TestChildrenAcceptsTheObjectShapeTheVendorActuallyWrites(t *testing.T) {
	// MEASURED, and the second time this class of assumption has been wrong
	// (formerNames was the first). §3.4 and the domain both describe children
	// as child SESSION IDS. The one real record carrying any holds artifacts:
	//
	//   [{"id":"158","href":"https://…/pull/158","kind":"pr"}]
	//
	// A []string decode errors on that; an adapter treating the error as
	// "record unreadable" would drop the record entirely. Reducing them to
	// their `id` would publish "158" in a field documented as carrying session
	// ids, which is worse than dropping them — a plausible wrong answer. So the
	// href is published, and it cannot be mistaken for a session id.
	res := read(t, fixtures(t))
	rec := recordFor(t, res, "blocked-no-needs-tempo-idle")

	if len(rec.Children) != 1 {
		t.Fatalf("children = %v, want one entry reduced from the object shape", rec.Children)
	}
	if !strings.HasPrefix(rec.Children[0], "https://") {
		t.Errorf("children[0] = %q, want the href — a bare id would read as a session id", rec.Children[0])
	}
}

func TestChildrenAlsoAcceptsThePlainStringShape(t *testing.T) {
	// The shape the architecture assumed. Both decoders stay exercised, so
	// neither rots.
	rec, err := parseRecord("d", "/p", []byte(`{"sessionId":"a","children":["one","two"]}`))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if len(rec.Children) != 2 || rec.Children[0] != "one" {
		t.Errorf("children = %v", rec.Children)
	}
}

func TestChildrenNullAndAbsentAreBothNil(t *testing.T) {
	for _, src := range []string{`{"sessionId":"a"}`, `{"sessionId":"a","children":null}`, `{"sessionId":"a","children":[]}`} {
		rec, err := parseRecord("d", "/p", []byte(src))
		if err != nil {
			t.Fatalf("parseRecord(%s): %v", src, err)
		}
		if rec.Children != nil {
			t.Errorf("%s gave children = %v, want nil", src, rec.Children)
		}
	}
}

func TestTokensIsNilRatherThanAnInventedSumWhenItIsNotANumber(t *testing.T) {
	rec, err := parseRecord("d", "/p", []byte(`{"sessionId":"a","tokens":{"input":10,"output":20}}`))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if rec.Tokens != nil {
		t.Errorf("tokens = %v, want nil; 30 would be a number we made up", *rec.Tokens)
	}
}

// ---------------------------------------------------------------------------
// what never leaves the record
// ---------------------------------------------------------------------------

func TestIntentOutputAndSuggestedReplyAreNotCarriedOffTheRecord(t *testing.T) {
	// §11 assumption 8: `output` holds assistant messages, `intent` holds the
	// user's own prompt, and `suggestedReply` holds a draft reply written for
	// the user. This is a STATUS observer. The guarantee is structural —
	// observer.JobRecord has no field for any of them — and this test proves it
	// over a record that actually carries all three, because a fixture with the
	// keys stripped would prove nothing.
	//
	// The content here is synthetic. It is not in testdata/ precisely so that
	// the fixture privacy audit stays able to assert those key names appear
	// nowhere under testdata/.
	const src = `{
	  "sessionId": "abc", "state": "blocked", "daemonShort": "abc",
	  "intent": "CANARY-INTENT", "output": "CANARY-OUTPUT",
	  "suggestedReply": "CANARY-REPLY",
	  "respawnFlags": ["--allowed-tools", "CANARY-FLAG"],
	  "needs": "a legitimate, published question"
	}`
	rec, err := parseRecord("abc", "/p", []byte(src))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}

	rendered := fmtRecord(rec)
	for _, canary := range []string{"CANARY-INTENT", "CANARY-OUTPUT", "CANARY-REPLY", "CANARY-FLAG"} {
		if strings.Contains(rendered, canary) {
			t.Errorf("%s survived into the record: %s", canary, rendered)
		}
	}
	if rec.Needs != "a legitimate, published question" {
		t.Errorf("needs = %q; it IS published (§5.2's sample carries it)", rec.Needs)
	}
	for _, key := range NeverRead {
		if strings.Contains(rendered, key) {
			t.Errorf("the record mentions the never-read key %q", key)
		}
	}
}

// fmtRecord renders every field of a JobRecord as text, so a canary hiding in
// any of them is found. Using %+v on the struct is what makes this total: a
// field added later is covered without editing this test.
func fmtRecord(rec observer.JobRecord) string {
	return strings.Join([]string{
		rec.Dir, rec.DaemonShort, rec.SessionID, rec.State, rec.Detail, rec.Needs,
		rec.Tempo, rec.CWD, rec.Name, rec.NameSource, rec.CLIVersion,
		rec.LinkScanPath, rec.ResumeSessionID,
		strings.Join(rec.Children, " "),
	}, "\x00")
}

// ---------------------------------------------------------------------------
// ordering, absence, cancellation
// ---------------------------------------------------------------------------

func TestRecordsAreSortedByDirectoryName(t *testing.T) {
	// M8's duplicate rule is "the FIRST in a fixed order wins", and "first" is
	// only deterministic if the order is. Directory enumeration order is not a
	// fixed order.
	res := read(t, fixtures(t))

	dirs := dirsOf(res)
	sorted := append([]string(nil), dirs...)
	sort.Strings(sorted)
	for i := range dirs {
		if dirs[i] != sorted[i] {
			t.Fatalf("records are not in directory order:\n got  %v\n want %v", dirs, sorted)
		}
	}
}

func TestAnAbsentJobsDirectoryIsAFactNotAnError(t *testing.T) {
	src := New(filepath.Join(t.TempDir(), "does-not-exist"), fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("an absent jobs directory produced an error: %v", err)
	}
	if !res.Absent {
		t.Error("absent is false")
	}
	if len(res.Records) != 0 || res.Entries != 0 || res.Dirs != 0 {
		t.Errorf("counts are not zero: %+v", res)
	}
}

func TestAnEmptyJobsDirectoryIsNotAbsent(t *testing.T) {
	// A machine that HAS run background agents and has had them all reaped is
	// a different state from one that never has, and the watch mode P5 chooses
	// depends on the difference.
	res, err := New(t.TempDir(), fixedClock{tRef}).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Absent {
		t.Error("an existing but empty directory reported absent")
	}
}

func TestACancelledContextStopsTheRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewFS(fixtures(t), "/fixtures/jobs", fixedClock{tRef}).Read(ctx)
	if err == nil {
		t.Error("a cancelled context did not stop the read")
	}
}

func TestTheObservedAtComesFromTheInjectedClock(t *testing.T) {
	res := read(t, fixtures(t))
	if !res.ObservedAt.Equal(observer.TruncateTime(tRef)) {
		t.Errorf("observedAt = %s, want the injected clock", observer.FormatTime(res.ObservedAt))
	}
}

func TestNoWriteMethodIsReachableFromTheSource(t *testing.T) {
	// The read-only guarantee is a property of the TYPE: Source holds an fs.FS,
	// which has exactly one method. This asserts the field's interface has not
	// been widened to something with a Create or an OpenFile.
	var src any = NewFS(fstest.MapFS{}, "/x", fixedClock{tRef})
	if _, ok := src.(interface {
		Create(string) (fs.File, error)
	}); ok {
		t.Error("the source exposes a Create method")
	}
	if _, ok := src.(interface {
		WriteFile(string, []byte, fs.FileMode) error
	}); ok {
		t.Error("the source exposes a WriteFile method")
	}
}

// The live case of 2026-10-22: a worktree is removed once its branch merges,
// and the blocked job left in it keeps its record. New stats each
// non-terminal record's cwd; only a confirmed absence sets CWDGone.
func TestANonTerminalJobWhoseFolderIsGoneSaysSo(t *testing.T) {
	jobs := t.TempDir()
	here := t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed-worktree")
	write := func(dir, state, cwd string) {
		if err := os.MkdirAll(filepath.Join(jobs, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"sessionId":"` + dir + `-0000-0000-0000-000000000000","state":"` + state + `","cwd":"` + cwd + `"}`
		if err := os.WriteFile(filepath.Join(jobs, dir, StateFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("aaaaaaaa", "blocked", gone)
	write("bbbbbbbb", "blocked", here)
	write("cccccccc", "done", gone)     // ended: not looked at
	write("dddddddd", "blocked", "rel") // relative: no opinion

	res, err := New(jobs, fixedClock{tRef}).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := map[string]bool{"aaaaaaaa": true, "bbbbbbbb": false, "cccccccc": false, "dddddddd": false}
	for dir, gone := range want {
		if got := recordFor(t, res, dir).CWDGone; got != gone {
			t.Errorf("%s: CWDGone = %v, want %v", dir, got, gone)
		}
	}

	// NewFS takes no look outside the tree at all.
	fsRes, err := NewFS(os.DirFS(jobs), jobs, fixedClock{tRef}).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if recordFor(t, fsRes, "aaaaaaaa").CWDGone {
		t.Error("NewFS stat'ed a cwd outside its fs.FS")
	}
}
