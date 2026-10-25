package registryfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// fixtureDir is §9.1's registry fixture set. Assertions are made against these
// files and never against ~/.claude/sessions: the live set changes constantly
// (sessions start, go idle, die), so an assertion against it is flaky by
// construction. The fixtures were shaped from live records and the README in
// that directory marks which are REAL captures and which are SYNTHETIC.
const fixtureDir = "../../testdata/registry"

// tRef is the single reference instant every fixture's timestamps are relative
// to (§5.2). Deriving ages against one stated instant rather than against
// time.Now() is what makes the arithmetic assertions exact instead of
// approximate.
var tRef = time.UnixMilli(1789699211204).UTC()

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return observer.TruncateTime(c.now) }

// fixtureFS builds a registry directory containing exactly the named fixture
// files, read from disk under their own names. Composing per-case directories
// this way keeps each case's record set explicit — reading the whole fixture
// directory as one registry would mix unrelated cases into every assertion.
func fixtureFS(t *testing.T, names ...string) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatalf("reading fixture %s: %v", name, err)
		}
		m[name] = &fstest.MapFile{Data: data, Mode: 0o600}
	}
	return m
}

func readFixtures(t *testing.T, names ...string) observer.RegistryResult {
	t.Helper()
	src := NewFS(fixtureFS(t, names...), "/fixtures/sessions", fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return res
}

func byID(t *testing.T, res observer.RegistryResult, id string) observer.RegistryRecord {
	t.Helper()
	for _, r := range res.Records {
		if r.SessionID == id {
			return r
		}
	}
	t.Fatalf("no record with sessionId %s in %d records", id, len(res.Records))
	return observer.RegistryRecord{}
}

// ---------------------------------------------------------------------------
// Parse by key NAME
// ---------------------------------------------------------------------------

func TestABackgroundProcessRecordNamesItsKindAndItsJob(t *testing.T) {
	r := byID(t, readFixtures(t, "background-process.json"), "857ef435-aa77-46f9-bb28-ca9c5374be9d")
	if r.Kind != observer.RegistryKindBackground || r.JobID != "857ef435" {
		t.Errorf("kind/jobId = %q/%q, want bg/857ef435", r.Kind, r.JobID)
	}
	if r.Tmux != "" || r.TmuxParts != nil {
		t.Errorf("a background process has no pane; got tmux %q", r.Tmux)
	}
	if other := byID(t, readFixtures(t, "live-idle.json"), "f350c44d-d22a-4c47-b3ef-00021713db7f"); other.Kind != "interactive" || other.JobID != "" {
		t.Errorf("an interactive record read as kind/jobId %q/%q", other.Kind, other.JobID)
	}
}

func TestEveryFieldIsReadFromARealShapedRecord(t *testing.T) {
	res := readFixtures(t, "live-idle.json")
	if len(res.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(res.Records))
	}
	r := res.Records[0]

	if r.PID != 78761 {
		t.Errorf("pid = %d", r.PID)
	}
	if r.SessionID != "f350c44d-d22a-4c47-b3ef-00021713db7f" {
		t.Errorf("sessionId = %q", r.SessionID)
	}
	if r.Status != "idle" {
		t.Errorf("status = %q", r.Status)
	}
	if r.WaitingFor != "" {
		t.Errorf("waitingFor = %q, want empty for a non-waiting record", r.WaitingFor)
	}
	if r.Name != "attachments-slangs" || r.NameSource != "user" {
		t.Errorf("name/nameSource = %q/%q", r.Name, r.NameSource)
	}
	if r.CWD != "/home/user/mag/madbench" {
		t.Errorf("cwd = %q", r.CWD)
	}
	if r.Version != "2.1.275" {
		t.Errorf("version = %q", r.Version)
	}
	if r.MessagingSocketPath != "/tmp/cc-socks/78761.sock" {
		t.Errorf("messagingSocketPath = %q", r.MessagingSocketPath)
	}
	if r.Tmux != "madbench:@51.%107" {
		t.Errorf("tmux = %q", r.Tmux)
	}
	if r.Path != "/fixtures/sessions/live-idle.json" {
		t.Errorf("path = %q", r.Path)
	}

	// Epoch millis converted ONCE, here, to the domain's UTC millisecond
	// precision. Nothing downstream should have to know that the registry
	// speaks epochs while job records speak RFC3339.
	if r.StatusUpdatedAt == nil {
		t.Fatal("statusUpdatedAt is nil")
	}
	want := time.UnixMilli(1789699211204 - 279119).UTC()
	if !r.StatusUpdatedAt.Equal(want) {
		t.Errorf("statusUpdatedAt = %s, want %s", r.StatusUpdatedAt.Format(observer.TimeFormat), want.Format(observer.TimeFormat))
	}
	if r.StatusUpdatedAt.Location() != time.UTC {
		t.Errorf("statusUpdatedAt is not UTC: %v", r.StatusUpdatedAt.Location())
	}
	if r.StatusUpdatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("statusUpdatedAt carries sub-millisecond precision: %v", r.StatusUpdatedAt)
	}
}

func TestKeyOrderIsIrrelevant(t *testing.T) {
	// Measured fact 12: registry key order is NOT stable. A positional or
	// order-dependent reader would work for weeks and then quietly return the
	// wrong field.
	shuffled := byID(t, readFixtures(t, "key-order-shuffled.json"), "d5e83c17-4a29-4b60-9f13-7c82e04a1b95")

	if shuffled.PID != 1926 {
		t.Errorf("pid = %d, want 1926", shuffled.PID)
	}
	if shuffled.Status != "busy" {
		t.Errorf("status = %q, want busy", shuffled.Status)
	}
	if shuffled.Tmux != "authflow:@25.%43" {
		t.Errorf("tmux = %q", shuffled.Tmux)
	}
	if shuffled.Name != "authflow-shuffled" {
		t.Errorf("name = %q", shuffled.Name)
	}
	if shuffled.StatusUpdatedAt == nil {
		t.Error("statusUpdatedAt is nil in a shuffled record")
	}
}

func TestTheFilenameIsNotAFieldSource(t *testing.T) {
	// Every fixture in this suite has a descriptive filename rather than the
	// production "<pid>.json", and every field assertion above still passes.
	// That is the proof that nothing is derived from the name — a derived-id
	// assumption is the bug class measured fact 6 records (a cwd-derived slug
	// that was right 95.86% of the time and could not be fixed).
	res := readFixtures(t, "version-2.1.263.json")
	r := res.Records[0]
	if r.PID != 56526 {
		t.Errorf("pid = %d, want the record's own 56526", r.PID)
	}
	if r.Version != "2.1.263" {
		t.Errorf("version = %q; the 2.1.263 fixture is kept so an upstream format change surfaces as a diff, not a break", r.Version)
	}
}

// ---------------------------------------------------------------------------
// The optional keys, and the two formerNames shapes
// ---------------------------------------------------------------------------

func TestOptionalKeysAbsentIsNotAnError(t *testing.T) {
	r := readFixtures(t, "missing-optional.json").Records[0]
	if r.WaitingFor != "" {
		t.Errorf("waitingFor = %q", r.WaitingFor)
	}
	if r.FormerNames != nil {
		t.Errorf("formerNames = %v, want nil for an absent key", r.FormerNames)
	}
	if r.SessionID == "" {
		t.Error("the record was dropped")
	}
}

func TestFormerNamesAcceptsTheShapeTheVendorActuallyWrites(t *testing.T) {
	// MEASURED DIVERGENCE. The architecture describes formerNames as a list of
	// names and the wire field is []string. On this machine it is a list of
	// OBJECTS: {"name":…,"until":…,"sessionId":…}, on 2 of 33 live records. A
	// []string decode returns an error there, and an adapter that treated the
	// error as "record unreadable" would drop exactly the sessions that have
	// been renamed.
	r := byID(t, readFixtures(t, "with-former-names.json"), "0a180476-fe5f-4e29-b117-e10dbca60839")
	want := []string{"remediate-catalog-v3-review", "remediate-catalog-v2"}
	if len(r.FormerNames) != len(want) {
		t.Fatalf("formerNames = %v, want %v", r.FormerNames, want)
	}
	for i := range want {
		if r.FormerNames[i] != want[i] {
			t.Errorf("formerNames[%d] = %q, want %q (record order must be preserved)", i, r.FormerNames[i], want[i])
		}
	}
}

func TestFormerNamesAlsoAcceptsThePlainStringShape(t *testing.T) {
	// The shape the architecture assumed. Both are accepted because we do not
	// know which one a future version writes, and refusing one of them would
	// mean losing whole records over a cosmetic field.
	r := byID(t, readFixtures(t, "formernames-strings.json"), "f1a27c90-6b48-4e35-9d01-8a5c3e7b2604")
	if len(r.FormerNames) != 2 || r.FormerNames[0] != "older-name" {
		t.Errorf("formerNames = %v", r.FormerNames)
	}
}

func TestAnUnknownFormerNamesShapeIsIgnoredNotFatal(t *testing.T) {
	m := fstest.MapFS{
		"weird.json": &fstest.MapFile{Data: []byte(`{
			"pid": 4242, "sessionId": "11111111-2222-3333-4444-555555555555",
			"status": "idle", "formerNames": {"not": "a list"}
		}`)},
	}
	src := NewFS(m, "/fixtures/sessions", fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("an unknown formerNames shape cost the whole record: %d records", len(res.Records))
	}
	if res.Records[0].FormerNames != nil {
		t.Errorf("formerNames = %v, want nil", res.Records[0].FormerNames)
	}
}

// ---------------------------------------------------------------------------
// The tmux reference
// ---------------------------------------------------------------------------

func TestParseTmuxKeepsTheSigilsAndIsColonSafe(t *testing.T) {
	cases := []struct {
		raw                   string
		ok                    bool
		session, window, pane string
	}{
		{"madbench:@51.%107", true, "madbench", "@51", "%107"},
		{"orbit-demo:@3.%5", true, "orbit-demo", "@3", "%5"},
		// A session name may contain colons. A naive SplitN(raw, ":", 2)
		// splits at the FIRST colon and returns "mag" as the session name,
		// which is a plausible-looking wrong answer.
		{"mag:api:worker:@7.%13", true, "mag:api:worker", "@7", "%13"},
		// The sigils are part of the ids. Without them this is not a
		// reference, and pretending otherwise fails silently later.
		{"madbench:51.107", false, "", "", ""},
		{"madbench:@51.107", false, "", "", ""},
		{"madbench:51.%107", false, "", "", ""},
		{"", false, "", "", ""},
		{"nonsense", false, "", "", ""},
	}

	for _, tc := range cases {
		ref, ok := ParseTmux(tc.raw)
		if ok != tc.ok {
			t.Errorf("ParseTmux(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			continue
		}
		if ref.Raw != tc.raw {
			t.Errorf("ParseTmux(%q).Raw = %q — the raw value must survive even an unparseable reference", tc.raw, ref.Raw)
		}
		if ref.Session != tc.session || ref.Window != tc.window || ref.Pane != tc.pane {
			t.Errorf("ParseTmux(%q) = %q/%q/%q, want %q/%q/%q",
				tc.raw, ref.Session, ref.Window, ref.Pane, tc.session, tc.window, tc.pane)
		}
	}
}

func TestARecordWithNoTmuxFieldHasNoParts(t *testing.T) {
	// B6: the tmux field is NOT structurally guaranteed. A nil TmuxParts is
	// "there is no reference", which is different from "there is a reference
	// we could not parse" — and the liveness policy treats them differently.
	r := byID(t, readFixtures(t, "no-tmux-field.json"), "4a6b8d02-9e17-4c53-8b2f-c1d7e0a35946")
	if r.Tmux != "" {
		t.Errorf("tmux = %q, want empty", r.Tmux)
	}
	if r.TmuxParts != nil {
		t.Errorf("TmuxParts = %+v, want nil", r.TmuxParts)
	}
}

func TestTheRawReferenceIsCarriedVerbatimForTheGate(t *testing.T) {
	r := readFixtures(t, "live-idle.json").Records[0]
	if r.TmuxParts == nil {
		t.Fatal("TmuxParts is nil")
	}
	if r.TmuxParts.Raw != r.Tmux {
		t.Errorf("TmuxParts.Raw = %q but Tmux = %q; the gate compares the raw string against tmux output and must not reassemble it",
			r.TmuxParts.Raw, r.Tmux)
	}
}

// ---------------------------------------------------------------------------
// The .key siblings
// ---------------------------------------------------------------------------

func TestKeyFilesAreCountedAndNeverOpened(t *testing.T) {
	const keyName = "56526.7641b44ee0dbaa6648875c14fe0ee0a02aab5f2d52256204ae125a75d2d52407.key"

	// A counting FS: the assertion is about which files were OPENED, and the
	// only way to test that is to count opens. The .key fixture's content is
	// not JSON, so a reader that opened it would also raise a parse error —
	// but "it did not error" is weaker than "it was never touched".
	base := fixtureFS(t, "live-idle.json", keyName)
	counting := &countingFS{inner: base, opens: map[string]int{}}

	src := NewFS(counting, "/fixtures/sessions", fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if res.IgnoredKeyFiles != 1 {
		t.Errorf("ignoredKeyFiles = %d, want 1 — the count is what makes the file total reconcile", res.IgnoredKeyFiles)
	}
	if len(res.Records) != 1 {
		t.Errorf("expected 1 record, got %d", len(res.Records))
	}
	if res.ParseErrors != 0 {
		t.Errorf("parseErrors = %d; the .key file must not be parsed at all", res.ParseErrors)
	}
	if n := counting.opens[keyName]; n != 0 {
		t.Errorf("the .key file was opened %d times; it is key material and there is no reason to read it", n)
	}
	if n := counting.opens["live-idle.json"]; n != 1 {
		t.Errorf("the record was opened %d times, want exactly 1", n)
	}
}

func TestAnUnrecognisedFileTypeIsLeftAlone(t *testing.T) {
	m := fixtureFS(t, "live-idle.json")
	m["someone-elses.lock"] = &fstest.MapFile{Data: []byte("not ours")}
	m["notes.txt"] = &fstest.MapFile{Data: []byte("also not ours")}

	src := NewFS(m, "/fixtures/sessions", fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Records) != 1 || res.ParseErrors != 0 {
		t.Errorf("records = %d, parseErrors = %d; files that are not ours must not be counted as anything",
			len(res.Records), res.ParseErrors)
	}
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

func TestRecordsAreSortedByPidAscending(t *testing.T) {
	// §3.2's duplicate rule is "the FIRST in a fixed order wins". "First" is
	// only deterministic if the order is, and directory enumeration order is
	// not a fixed order.
	res := readFixtures(t,
		"live-idle.json",             // 78761
		"duplicate-sessionid-a.json", // 1000
		"live-busy.json",             // 98693
		"duplicate-sessionid-b.json", // 2000
		"idle-3-days.json",           // 1061
	)
	want := []int{1000, 1061, 2000, 78761, 98693}
	if len(res.Records) != len(want) {
		t.Fatalf("expected %d records, got %d", len(want), len(res.Records))
	}
	for i, w := range want {
		if res.Records[i].PID != w {
			t.Fatalf("records[%d].PID = %d, want %d (full order: %v)", i, res.Records[i].PID, w, pids(res.Records))
		}
	}
}

// ---------------------------------------------------------------------------
// Unusable records
// ---------------------------------------------------------------------------

func TestARecordWithNoSessionIdIsAParseErrorNotAnInventedId(t *testing.T) {
	res := readFixtures(t, "live-idle.json", "no-session-id.json")

	if len(res.Records) != 1 {
		t.Fatalf("expected the one usable record, got %d: %v", len(res.Records), pids(res.Records))
	}
	if res.ParseErrors == 0 {
		t.Error("the unusable record was not counted")
	}
	// And the usable one survived. One malformed record must never blank the
	// set — that exact bug shipped in this repository's previous feature.
	if res.Records[0].SessionID != "f350c44d-d22a-4c47-b3ef-00021713db7f" {
		t.Errorf("the readable record did not survive: %+v", res.Records[0])
	}
}

func TestAnUnknownStatusIsCarriedVerbatim(t *testing.T) {
	// The adapter does NOT map. It hands the vendor's literal to the one place
	// that knows the pass-through rule, so an unrecognised value reaches
	// exactly one decision point instead of being normalised in four.
	r := byID(t, readFixtures(t, "unknown-status.json"), "b1d4f082-7e35-496a-a2c8-40f7e5b19c63")
	if r.Status != "reticulating" {
		t.Errorf("status = %q, want the literal", r.Status)
	}
}

func TestAZeroOrAbsentTimestampIsNilNot1970(t *testing.T) {
	r := byID(t, readFixtures(t, "no-status-updated-at.json"), "ce5f7a13-8204-4b96-a7d3-05e21c8b4f70")
	if r.StatusUpdatedAt != nil {
		t.Errorf("statusUpdatedAt = %v, want nil for an absent key", r.StatusUpdatedAt)
	}

	m := fstest.MapFS{"zero.json": &fstest.MapFile{Data: []byte(
		`{"pid":1,"sessionId":"aaaaaaaa-0000-0000-0000-000000000000","status":"idle","statusUpdatedAt":0}`)}}
	res, err := NewFS(m, "/fixtures/sessions", fixedClock{tRef}).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Records[0].StatusUpdatedAt != nil {
		t.Errorf("a zero epoch became %v; reporting a session as idle since 1970 is a wrong answer where nil is an honest one",
			res.Records[0].StatusUpdatedAt)
	}
}

// ---------------------------------------------------------------------------
// §6.5 — a torn read means TORN, not dead
// ---------------------------------------------------------------------------

func TestATornRecordIsRetriedOnceAndSucceedsIfTheWriterFinished(t *testing.T) {
	torn, err := os.ReadFile(filepath.Join(fixtureDir, "torn.json"))
	if err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(filepath.Join(fixtureDir, "live-idle.json"))
	if err != nil {
		t.Fatal(err)
	}

	// A mutating FS: the first read of the file returns the truncated bytes an
	// in-place rewrite leaves behind; the second returns the finished record.
	// This is the real mechanism — the registry is rewritten in place, so a
	// read landing mid-write sees a prefix.
	m := &mutatingFS{inner: fstest.MapFS{
		"live-idle.json": &fstest.MapFile{Data: torn},
	}, after: map[string][]byte{"live-idle.json": whole}}

	src := NewFS(m, "/fixtures/sessions", fixedClock{tRef})
	slept := 0
	src.sleep = func(time.Duration) { slept++ }

	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(res.Records) != 1 {
		t.Fatalf("the retry did not recover the record: %d records, %d failures", len(res.Records), len(res.Failures))
	}
	if slept != 1 {
		t.Errorf("slept %d times, want exactly 1 — the retry is bounded, not a loop", slept)
	}
	if res.ParseErrors != 1 {
		t.Errorf("parseErrors = %d, want 1: the counter is the whole trace of a recovered torn read", res.ParseErrors)
	}
	if len(res.Failures) != 0 {
		t.Errorf("a recovered read reported failures: %+v", res.Failures)
	}
}

func TestAPersistentlyTornRecordIsReportedNotDropped(t *testing.T) {
	torn, err := os.ReadFile(filepath.Join(fixtureDir, "torn.json"))
	if err != nil {
		t.Fatal(err)
	}

	m := fixtureFS(t, "live-idle.json")
	m["torn.json"] = &fstest.MapFile{Data: torn}

	src := NewFS(m, "/fixtures/sessions", fixedClock{tRef})
	src.sleep = func(time.Duration) {}

	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// The failure is REPORTED, with its path, so the core can keep the
	// previous answer for whatever session that file described. Silence here
	// would mean "session gone", and removal requires positive evidence.
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %+v, want exactly one", res.Failures)
	}
	if res.Failures[0].Path != "/fixtures/sessions/torn.json" {
		t.Errorf("failure path = %q", res.Failures[0].Path)
	}
	if res.Failures[0].Bytes != len(torn) {
		t.Errorf("failure bytes = %d, want %d", res.Failures[0].Bytes, len(torn))
	}
	// And the other record is untouched.
	if len(res.Records) != 1 {
		t.Errorf("records = %d; one torn file must never blank the set", len(res.Records))
	}
}

func TestTheRetryBudgetIsBounded(t *testing.T) {
	torn, err := os.ReadFile(filepath.Join(fixtureDir, "torn.json"))
	if err != nil {
		t.Fatal(err)
	}

	m := fstest.MapFS{}
	for i := 0; i < 10; i++ {
		m[string(rune('a'+i))+".json"] = &fstest.MapFile{Data: torn}
	}

	src := NewFS(m, "/fixtures/sessions", fixedClock{tRef})
	slept := 0
	src.sleep = func(time.Duration) { slept++ }

	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if slept != maxRetriesPerRebuild {
		t.Errorf("slept %d times, want the bound of %d — a whole directory being rewritten must cost latency, not a stall",
			slept, maxRetriesPerRebuild)
	}
	if res.RetriesDeferred != 10-maxRetriesPerRebuild {
		t.Errorf("retriesDeferred = %d, want %d; the bound has to be observable",
			res.RetriesDeferred, 10-maxRetriesPerRebuild)
	}
	if len(res.Failures) != 10 {
		t.Errorf("failures = %d, want all 10 reported so none is silently forgotten", len(res.Failures))
	}
	if tornRetryDelay != 10*time.Millisecond {
		t.Errorf("tornRetryDelay = %v, want §6.5's 10ms", tornRetryDelay)
	}
}

// ---------------------------------------------------------------------------
// §6.6 — an absent registry is a fact, not an error
// ---------------------------------------------------------------------------

func TestAnAbsentRegistryDirectoryIsNotAnError(t *testing.T) {
	// This is what a machine where Claude has never run looks like. Exit 0,
	// zero sessions. Treating it as an error would make `tt sessions` fail on
	// a clean machine.
	src := New(filepath.Join(t.TempDir(), "does-not-exist"), fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("an absent registry directory returned an error: %v", err)
	}
	if !res.Absent {
		t.Error("Absent is false for a missing directory")
	}
	if len(res.Records) != 0 || res.Err != nil {
		t.Errorf("records = %d, err = %v", len(res.Records), res.Err)
	}
	if res.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero: even an empty read happened at a time")
	}
}

func TestAnEmptyRegistryDirectoryIsNotAbsent(t *testing.T) {
	src := New(t.TempDir(), fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Absent {
		t.Error("an existing but empty directory was reported as absent; the two states are different")
	}
	if len(res.Records) != 0 {
		t.Errorf("records = %d", len(res.Records))
	}
}

// ---------------------------------------------------------------------------
// The adapter never writes
// ---------------------------------------------------------------------------

func TestReadingAReadOnlyTreeChangesNothing(t *testing.T) {
	// The runtime half of AC13, scoped to this adapter: a real directory,
	// chmod 0500, walked before and after. The static audit proves there is no
	// write CALL; this proves the reads do not move anything either.
	dir := t.TempDir()
	for _, name := range []string{"live-idle.json", "live-busy.json", "with-former-names.json"} {
		data, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	before := statTree(t, dir)

	src := New(dir, fixedClock{tRef})
	res, err := src.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(res.Records))
	}

	after := statTree(t, dir)
	if len(before) != len(after) {
		t.Fatalf("the tree changed size: %d -> %d entries", len(before), len(after))
	}
	for name, b := range before {
		a, ok := after[name]
		if !ok {
			t.Errorf("%s disappeared", name)
			continue
		}
		if b != a {
			t.Errorf("%s changed: %+v -> %+v", name, b, a)
		}
	}
}

type fileStat struct {
	size  int64
	mode  os.FileMode
	mtime int64
}

func statTree(t *testing.T, dir string) map[string]fileStat {
	t.Helper()
	out := map[string]fileStat{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		// mtime + size + mode is the right invariant: O_RDONLY does not move
		// mtime, and atime is unreliable on APFS.
		out[e.Name()] = fileStat{size: info.Size(), mode: info.Mode(), mtime: info.ModTime().UnixNano()}
	}
	return out
}

// ---------------------------------------------------------------------------
// The fixture set itself
// ---------------------------------------------------------------------------

func TestEveryFixtureIsEitherReadableOrDeliberatelyNot(t *testing.T) {
	// A fixture directory that has silently rotted is worse than no fixtures:
	// every assertion above would still pass while testing less than it
	// claims. So the whole directory is read as one registry, and the numbers
	// are pinned.
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}

	names := []string{}
	keyFiles, jsonFiles := 0, 0
	for _, e := range entries {
		if e.IsDir() || e.Name() == "README.md" {
			continue
		}
		names = append(names, e.Name())
		switch filepath.Ext(e.Name()) {
		case ".key":
			keyFiles++
		case ".json":
			jsonFiles++
		}
	}

	res := readFixtures(t, names...)

	if keyFiles != res.IgnoredKeyFiles {
		t.Errorf("the directory has %d .key files but the reader counted %d", keyFiles, res.IgnoredKeyFiles)
	}
	// Two fixtures are deliberately unusable: torn.json (truncated mid-object)
	// and no-session-id.json (no join key).
	const deliberatelyUnusable = 2
	if got := len(res.Records); got != jsonFiles-deliberatelyUnusable {
		t.Errorf("read %d records from %d .json fixtures; expected %d unusable ones (torn.json, no-session-id.json). Failures: %+v",
			got, jsonFiles, deliberatelyUnusable, res.Failures)
	}
	if len(names) < 20 {
		t.Errorf("only %d fixtures found; §9.1's registry set is larger than that", len(names))
	}
}

func TestNoFixtureCarriesUserContent(t *testing.T) {
	// §9.1: every fixture derived from a real record has prompt-bearing fields
	// redacted, and a test asserts no fixture contains a field name on the
	// privacy denylist. Registry records do not carry prompts, so this is a
	// guard against a future fixture being pasted in from somewhere that does.
	denylist := []string{
		`"intent"`, `"output"`, `"lastPrompt"`, `"content"`, `"text"`,
		`"suggestedReply"`, `"message"`,
	}
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(fixtureDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		for _, banned := range denylist {
			if idx := indexOf(body, banned); idx >= 0 {
				t.Errorf("%s contains the denylisted field %s at offset %d", e.Name(), banned, idx)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func pids(recs []observer.RegistryRecord) []int {
	out := make([]int, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.PID)
	}
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
