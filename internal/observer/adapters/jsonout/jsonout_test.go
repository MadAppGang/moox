package jsonout_test

// S1's tests. Everything here is against §9.1's registry fixtures with an
// injected clock, or against a fake source — never against ~/.claude/sessions,
// which moves constantly and would make every assertion flaky by construction.
//
// The load-bearing one is §9.2(c)'s identity,
// `statusAgeMs == observedAt - statusSince`, asserted on the BYTES this surface
// emits for every session in every fixture. P1 asserts it on the domain value;
// this asserts it survives serialisation, which is the half a consumer
// actually reads. The architecture's own numbers drifted three revisions
// running for want of exactly this test.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
	"github.com/MadAppGang/moox/internal/observer/adapters/registryfs"
	"github.com/MadAppGang/moox/internal/observer/fakes"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// fixtureDir is §9.1's registry set, reached from this adapter's directory.
// The fixtures are shared rather than copied: two copies of a fixture set is
// two things to keep in step, and a fixture that disagrees with the one the
// core is tested against would make this surface's tests meaningless.
var fixtureDir = filepath.Join("..", "..", "testdata", "registry")

// tRef is §5.2's single reference instant, the same one P1's tests use.
var tRef = time.UnixMilli(1789699211204).UTC()

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	obs   *observer.Observer
	clock *fakes.Clock
}

func newHarness(t *testing.T, fixtures ...string) *harness {
	t.Helper()

	fsys := fstest.MapFS{}
	for _, name := range fixtures {
		data, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatalf("reading fixture %s: %v", name, err)
		}
		fsys[name] = &fstest.MapFile{Data: data, Mode: 0o600}
	}

	clock := fakes.NewClock(tRef)
	reg := registryfs.NewFS(fsys, "/fixtures/sessions", clock)

	pre, err := reg.Read(context.Background())
	if err != nil {
		t.Fatalf("seeding the gates from the fixtures: %v", err)
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
		Registry: reg,
		// Required ports as of P3. An empty jobs source, not a nil one: this
		// surface's tests are about the interactive population, and a nil port
		// would mean "confidently, there are no background agents".
		Jobs:        &fakes.Jobs{},
		Transcripts: fakes.NewTranscripts(),
		Proc:        proc,
		Panes:       fakes.NewPanes(paneIDs...),
		Clock:       clock,
	}, observer.Config{Paths: observer.Paths{Registry: "/fixtures/sessions"}})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}
	return &harness{obs: obs, clock: clock}
}

// allFixtures is every .json file in §9.1's registry set. Using the directory
// rather than a list means a fixture added by a later phase is covered by the
// universal assertions here automatically.
func allFixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureDir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) < 15 {
		t.Fatalf("only %d fixtures found; the fixture set is not being seen", len(names))
	}
	return names
}

func run(t *testing.T, src jsonout.Source, o jsonout.Options) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := jsonout.Run(context.Background(), src, o, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func jsonOpts() jsonout.Options {
	o := observer.DefaultSnapshotOptions()
	o.IncludeDead = true
	o.IncludeTranscripts = true
	return jsonout.Options{JSON: true, TT: "v0.0.0-test", Snapshot: o}
}

// decode brings the emitted bytes back through the ONE decoder. A local
// json-tagged struct here would be a second description of the wire shape,
// which §5.1's AST audit bans and which is the drift the ban exists to stop.
func decode(t *testing.T, out string) wire.Snapshot {
	t.Helper()
	snap, advisories, err := wire.DecodeSnapshot([]byte(out))
	if err != nil {
		t.Fatalf("decoding our own output: %v\n%s", err, out)
	}
	for _, a := range advisories {
		t.Fatalf("our own output produced a decode advisory %q; the encoder and decoder disagree", a.Code)
	}
	return snap
}

// ---------------------------------------------------------------------------
// §9.2(c) — the identity, on the wire, for every session in every fixture
// ---------------------------------------------------------------------------

func TestStatusAgeMsEqualsObservedAtMinusStatusSinceForEverySessionOnTheWire(t *testing.T) {
	h := newHarness(t, allFixtures(t)...)
	snapshot, err := h.obs.Snapshot(context.Background(), jsonOpts().Snapshot)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Sessions) == 0 {
		t.Fatal("no sessions in the fixture snapshot; the assertion below would be vacuous")
	}

	code, out, stderr := run(t, h.obs, jsonOpts())
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr)
	}
	got := decode(t, out)

	observedAt, err := observer.ParseTime(got.ObservedAt)
	if err != nil {
		t.Fatalf("the envelope's observedAt %q does not parse: %v", got.ObservedAt, err)
	}
	if !observedAt.Equal(tRef) {
		t.Fatalf("observedAt = %s, want the injected clock's %s", observedAt, tRef)
	}

	checked, null := 0, 0
	for _, s := range got.Sessions {
		switch {
		case s.StatusSince == nil:
			// The null branch is permitted, but only with a named advisory —
			// AC3(a)'s explicit-null half. An unexplained null is a failure.
			if s.StatusAgeMs != nil {
				t.Fatalf("%s: statusSince is null but statusAgeMs is %d", s.SessionID, *s.StatusAgeMs)
			}
			if !hasAdvisory(s, observer.AdvStatusSinceUnavailable) {
				t.Fatalf("%s: statusAgeMs is null with no %s advisory to say why",
					s.SessionID, observer.AdvStatusSinceUnavailable)
			}
			null++
		default:
			if s.StatusAgeMs == nil {
				t.Fatalf("%s: statusSince is %q but statusAgeMs is null", s.SessionID, *s.StatusSince)
			}
			since, err := observer.ParseTime(*s.StatusSince)
			if err != nil {
				t.Fatalf("%s: statusSince %q does not parse: %v", s.SessionID, *s.StatusSince, err)
			}
			want := observedAt.Sub(since).Milliseconds()
			if *s.StatusAgeMs != want {
				t.Fatalf("%s: statusAgeMs = %d, want observedAt - statusSince = %d (%s - %s)",
					s.SessionID, *s.StatusAgeMs, want, observedAt, since)
			}
			checked++
		}
	}

	if checked == 0 {
		t.Fatal("no session exercised the arithmetic; the identity was asserted over nothing")
	}
	if null == 0 {
		t.Fatal("no session exercised the null branch; no-status-updated-at.json should be in the set")
	}
	t.Logf("identity asserted on %d sessions; %d explicit nulls, each with its advisory", checked, null)
}

// TestBlockedForMsIsTheSameViewAsStatusAgeMsOnTheWire is AC3(b)'s half of the
// same property: when it is present it must EQUAL statusAgeMs, because it is a
// view of one measurement and not a second one.
func TestBlockedForMsIsTheSameViewAsStatusAgeMsOnTheWire(t *testing.T) {
	h := newHarness(t, allFixtures(t)...)
	code, out, stderr := run(t, h.obs, jsonOpts())
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr)
	}

	present := 0
	for _, s := range decode(t, out).Sessions {
		if s.BlockedForMs == nil {
			continue
		}
		present++
		if s.StatusAgeMs == nil || *s.BlockedForMs != *s.StatusAgeMs {
			t.Fatalf("%s: blockedForMs %v is not the same number as statusAgeMs %v",
				s.SessionID, *s.BlockedForMs, s.StatusAgeMs)
		}
		if !s.Attention {
			t.Fatalf("%s: blockedForMs is present on a non-attention session", s.SessionID)
		}
	}
	if present == 0 {
		t.Fatal("no attention session in the fixture set; the assertion was vacuous")
	}
}

// ---------------------------------------------------------------------------
// this surface reads no clock of its own
// ---------------------------------------------------------------------------

// TestTheEnvelopeCarriesTheSnapshotsInstantNotASecondClockRead is the reason
// this adapter takes no Clock. If it read time.Now() for the envelope, every
// age in the payload would be against a slightly earlier instant than
// observedAt claims, and §9.2(c)'s identity would be false by however long the
// read took. The fake source's snapshot is timestamped in 2026; the wall clock
// is not.
func TestTheEnvelopeCarriesTheSnapshotsInstantNotASecondClockRead(t *testing.T) {
	since := tRef.Add(-90 * time.Minute)
	ageMs := int64(90 * 60 * 1000)
	src := &fakeSource{snap: &observer.Snapshot{
		ObservedAt: tRef,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		Sessions: []observer.Session{{
			SessionID:   "fake-1",
			Kind:        observer.KindInteractive,
			Status:      observer.StatusIdle,
			StatusSince: &since,
			StatusAgeMs: &ageMs,
			ObservedAt:  tRef,
			ListedBy:    []string{},
			Sources:     []string{},
			Advisories:  []observer.Advisory{},
		}},
	}}

	code, out, stderr := run(t, src, jsonOpts())
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr)
	}
	got := decode(t, out)
	if got.ObservedAt != observer.FormatTime(tRef) {
		t.Fatalf("observedAt = %q, want the snapshot's own %q", got.ObservedAt, observer.FormatTime(tRef))
	}
	observedAt, _ := observer.ParseTime(got.ObservedAt)
	s := got.Sessions[0]
	if want := observedAt.Sub(since).Milliseconds(); *s.StatusAgeMs != want {
		t.Fatalf("statusAgeMs = %d, want %d", *s.StatusAgeMs, want)
	}
}

// ---------------------------------------------------------------------------
// exit codes (§5.2)
// ---------------------------------------------------------------------------

func TestZeroSessionsIsExitZeroWithAnEmptyArray(t *testing.T) {
	// "No Claude is running" is a fact, not an error. A non-zero exit here
	// would make every `if tt sessions --json` in a script wrong.
	src := &fakeSource{snap: &observer.Snapshot{
		ObservedAt: tRef,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		Sessions:   []observer.Session{},
	}}
	code, out, _ := run(t, src, jsonOpts())
	if code != jsonout.ExitOK {
		t.Fatalf("exit %d, want %d", code, jsonout.ExitOK)
	}
	if got := decode(t, out); len(got.Sessions) != 0 || got.Counts.Total != 0 {
		t.Fatalf("sessions = %v, counts.total = %d", got.Sessions, got.Counts.Total)
	}
	// An empty session set serialises as [], not null: a consumer iterating
	// the array should not have to nil-check it.
	if !strings.Contains(out, `"sessions":[]`) {
		t.Fatalf("an empty session set did not serialise as []:\n%s", out)
	}
}

func TestNothingReadableIsExitOneAndSaysSoOnStderr(t *testing.T) {
	src := &fakeSource{err: errors.New("registry: nothing could be read")}
	code, out, stderr := run(t, src, jsonout.Options{JSON: true})
	if code != jsonout.ExitNoRead {
		t.Fatalf("exit %d, want %d", code, jsonout.ExitNoRead)
	}
	if out != "" {
		t.Fatalf("a failed read wrote %q to stdout; a partial payload is worse than none", out)
	}
	if !strings.Contains(stderr, "nothing could be read") {
		t.Fatalf("stderr does not carry the reason: %q", stderr)
	}
}

// ---------------------------------------------------------------------------
// the machine surface's shape
// ---------------------------------------------------------------------------

// TestTheJSONIsOneNewlineTerminatedLine pins the framing §5.1 writes: one
// compact object per invocation, newline-terminated, so the output is valid
// JSON Lines and `jq` needs no flags. Indentation would be a private choice on
// a surface whose bytes three later surfaces must match (AC11).
func TestTheJSONIsOneNewlineTerminatedLine(t *testing.T) {
	h := newHarness(t, allFixtures(t)...)
	_, out, _ := run(t, h.obs, jsonOpts())

	if !strings.HasSuffix(out, "\n") {
		t.Fatal("the output is not newline-terminated")
	}
	if n := strings.Count(strings.TrimSuffix(out, "\n"), "\n"); n != 0 {
		t.Fatalf("the output spans %d extra lines; it must be one compact object", n+1)
	}
}

// TestTheEmittedBytesDecodeBackToTheSameSessions is AC11's first leg: S1's
// bytes are not merely valid, they are the same value. It is the wire
// round-trip property applied to the ACTUAL output of a real surface rather
// than to a generated snapshot, which is what makes it catch an encoder that
// is correct in isolation and wired wrongly here.
func TestTheEmittedBytesDecodeBackToTheSameSessions(t *testing.T) {
	h := newHarness(t, allFixtures(t)...)
	want, err := h.obs.Snapshot(context.Background(), jsonOpts().Snapshot)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// The same snapshot value, encoded by the surface and decoded back.
	code, out, _ := run(t, &fakeSource{snap: want}, jsonOpts())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	back, advisories, err := wire.DecodeSnapshot([]byte(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(advisories) != 0 {
		t.Fatalf("decode advisories: %+v", advisories)
	}
	got, _ := wire.ToDomain(back)

	if !reflect.DeepEqual(got.Sessions, want.Sessions) {
		t.Fatalf("the sessions did not survive S1's encode/decode.\ngot  %d sessions\nwant %d sessions\nfirst difference:\n%s",
			len(got.Sessions), len(want.Sessions), firstDiff(got.Sessions, want.Sessions))
	}
	if !reflect.DeepEqual(got.Counts, want.Counts) {
		t.Fatalf("counts differ:\ngot  %+v\nwant %+v", got.Counts, want.Counts)
	}
	if !got.ObservedAt.Equal(want.ObservedAt) {
		t.Fatalf("observedAt %s != %s", got.ObservedAt, want.ObservedAt)
	}
}

// TestEveryStatusAndKindKeyIsEmittedIncludingZeros pins §5.2's map rule. The
// omitempty ban covers struct tags; a map needs its own rule, and without one
// AC11's byte-identity test would inherit whatever the first implementation
// happened to do.
func TestEveryStatusAndKindKeyIsEmittedIncludingZeros(t *testing.T) {
	h := newHarness(t, "live-idle.json")
	_, out, _ := run(t, h.obs, jsonOpts())
	got := decode(t, out)

	for _, st := range observer.AllStatuses {
		if _, present := got.Counts.ByStatus[string(st)]; !present {
			t.Fatalf("counts.byStatus is missing the key %q", st)
		}
	}
	for _, k := range observer.AllKinds {
		if _, present := got.Counts.ByKind[string(k)]; !present {
			t.Fatalf("counts.byKind is missing the key %q", k)
		}
	}
}

func TestTheVersionStringComesFromTheCallerNotTheEncoder(t *testing.T) {
	h := newHarness(t, "live-idle.json")
	o := jsonOpts()
	o.TT = "v9.9.9-from-the-composition-root"
	_, out, _ := run(t, h.obs, o)
	if got := decode(t, out).TT; got != o.TT {
		t.Fatalf("tt = %q, want %q", got, o.TT)
	}
}

// ---------------------------------------------------------------------------
// the human surface — §5.2: not a contract beyond the names
// ---------------------------------------------------------------------------

func TestTheTableNamesEverySessionAndNothingIsAssertedBeyondThat(t *testing.T) {
	h := newHarness(t, allFixtures(t)...)
	snap, err := h.obs.Snapshot(context.Background(), jsonOpts().Snapshot)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	o := jsonOpts()
	o.JSON = false
	code, out, _ := run(t, h.obs, o)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, s := range snap.Sessions {
		if s.Name == "" {
			continue
		}
		if !strings.Contains(out, s.Name) {
			t.Fatalf("the table omits the session named %q:\n%s", s.Name, out)
		}
	}
	// It must not be JSON: the two surfaces are chosen by --json and a human
	// running `tt sessions` in a pipe should get neither by accident.
	if json.Valid([]byte(strings.SplitN(out, "\n", 2)[0])) && strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatal("the human surface emitted JSON")
	}
}

// TestTheTableDistinguishesUnknownFromFalse is the one substantive thing the
// compact view must not lose. "?" is not "no": a session whose liveness could
// not be determined is not a dead session, and a table that rendered both as
// the same word would undo M7 visually.
func TestTheTableDistinguishesUnknownFromFalse(t *testing.T) {
	yes, no := true, false
	base := observer.Session{
		Kind: observer.KindInteractive, Status: observer.StatusIdle,
		ObservedAt: tRef, ListedBy: []string{}, Sources: []string{}, Advisories: []observer.Advisory{},
	}
	mk := func(name string, live *bool) observer.Session {
		s := base
		s.SessionID, s.Name, s.Live = name, name, live
		return s
	}
	src := &fakeSource{snap: &observer.Snapshot{
		ObservedAt: tRef,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		Sessions:   []observer.Session{mk("alive", &yes), mk("dead", &no), mk("unknown", nil)},
	}}

	o := jsonOpts()
	o.JSON = false
	_, out, _ := run(t, src, o)
	for _, want := range []string{"yes", "no", "?"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the table does not render %q; all three liveness values must look different:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// --refresh, and its degraded paths
// ---------------------------------------------------------------------------

// TestRefreshOnASourceThatCannotRefreshStillAnswers is P2's honest position:
// the corroboration source arrives in P4, so --refresh has nothing to re-read.
// A user asking for it gets a note on stderr and a correct answer on stdout,
// not an error — the authoritative sources were read either way.
func TestRefreshOnASourceThatCannotRefreshStillAnswers(t *testing.T) {
	h := newHarness(t, "live-idle.json")
	o := jsonOpts()
	o.Refresh = true

	code, out, stderr := run(t, h.obs, o)
	if code != jsonout.ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if len(decode(t, out).Sessions) != 1 {
		t.Fatalf("the answer is missing: %s", out)
	}
	if !strings.Contains(stderr, "--refresh") {
		t.Fatalf("stderr does not say the refresh did not happen: %q", stderr)
	}
	// And the note must not be in the payload: a request-scoped fact about one
	// caller's refresh is not a fact about the observer (§7.2).
	if strings.Contains(out, "refresh") {
		t.Fatalf("the request-scoped refresh note leaked into the payload:\n%s", out)
	}
}

func TestARejectedRefreshIsReportedOnStderrAndTheSnapshotIsStillEmitted(t *testing.T) {
	src := &fakeRefresher{fakeSource: fakeSource{snap: &observer.Snapshot{
		ObservedAt: tRef,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		Sessions:   []observer.Session{},
	}}, outcome: observer.RefreshOutcome{
		Rejected: map[string]string{"agents-cli": "rate-limited"},
	}}

	o := jsonOpts()
	o.Refresh = true
	code, out, stderr := run(t, src, o)
	if code != jsonout.ExitOK {
		t.Fatalf("exit %d, want 0 — a rate limit is a successful degraded answer", code)
	}
	if !src.refreshed {
		t.Fatal("Refresh was never called on a source that implements it")
	}
	if !strings.Contains(stderr, "rate-limited") {
		t.Fatalf("the rejection is not on stderr: %q", stderr)
	}
	if strings.Contains(out, "rate-limited") {
		t.Fatalf("the rejection leaked into the payload:\n%s", out)
	}
}

func TestTheSnapshotOptionsReachTheCoreUnaltered(t *testing.T) {
	// The surface must not second-guess what it was asked for. This is the
	// other half of "filtering is the core's job": if jsonout adjusted the
	// options, two surfaces given identical options would disagree about which
	// sessions exist, which is the AC11 hazard §5.1 is shaped to prevent.
	src := &fakeSource{snap: &observer.Snapshot{
		ObservedAt: tRef, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{}, Warnings: []observer.Advisory{},
		Sessions: []observer.Session{},
	}}
	want := observer.DefaultSnapshotOptions()
	want.IncludeDead = true
	want.IncludeTranscripts = true
	want.Filter = observer.Filter{Kinds: []observer.Kind{observer.KindBackground}, CWDPrefix: "/x", WaitingOnly: true}

	run(t, src, jsonout.Options{JSON: true, Snapshot: want})

	if !reflect.DeepEqual(src.gotOpts, want) {
		t.Fatalf("the core was asked for\n%+v\nwant\n%+v", src.gotOpts, want)
	}
}

// ---------------------------------------------------------------------------
// fakes and helpers
// ---------------------------------------------------------------------------

type fakeSource struct {
	snap    *observer.Snapshot
	err     error
	gotOpts observer.SnapshotOptions
	calls   int
}

func (f *fakeSource) Snapshot(_ context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	f.calls++
	f.gotOpts = opts
	return f.snap, f.err
}

type fakeRefresher struct {
	fakeSource
	outcome   observer.RefreshOutcome
	refreshed bool
}

func (f *fakeRefresher) Refresh(_ context.Context, _ []string) (*observer.Snapshot, observer.RefreshOutcome, error) {
	f.refreshed = true
	return f.snap, f.outcome, nil
}

func hasAdvisory(s wire.Session, code string) bool {
	for _, a := range s.Advisories {
		if a.Code == code {
			return true
		}
	}
	return false
}

func firstDiff(got, want []observer.Session) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if !reflect.DeepEqual(got[i], want[i]) {
			return "index " + got[i].SessionID + ":\ngot  " + describe(got[i]) + "\nwant " + describe(want[i])
		}
	}
	return "the lengths differ"
}

func describe(s observer.Session) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	// Deliberately the raw domain value, for a failure message only. It is
	// never a surface, so it carries no json tags and needs none.
	_ = enc.Encode(map[string]any{
		"sessionId": s.SessionID, "status": s.Status, "live": s.Live,
		"statusAgeMs": s.StatusAgeMs, "advisories": len(s.Advisories),
	})
	return strings.TrimSpace(b.String())
}
