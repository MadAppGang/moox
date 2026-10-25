package observer_test

// The acceptance criteria P1 is answerable for: AC1, AC2 and AC3(a)(b), over
// §9.1's registry fixtures with an injected clock.
//
// Everything here is asserted against fixtures, never against
// ~/.claude/sessions. The live set moves constantly — sessions start, go idle
// and die — so an assertion against it is flaky by construction, and
// `validation-criteria.md` makes the live run a separate bracketed spot-check
// for exactly that reason.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/registryfs"
	"github.com/MadAppGang/moox/internal/observer/fakes"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

const fixtureDir = "testdata/registry"

// tRef is §5.2's single reference instant. Every fixture timestamp is relative
// to it and every age below is exact arithmetic against it, so no assertion
// here needs a tolerance.
var tRef = time.UnixMilli(1789699211204).UTC()

const (
	threeDaysMs    int64 = 259_200_000
	thirtyFiveMins int64 = 35 * 60 * 1000
	thirtyOneMins  int64 = 31 * 60 * 1000
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	t     *testing.T
	obs   *observer.Observer
	fsys  fstest.MapFS
	panes *fakes.Panes
	proc  *fakes.Proc
	clock *fakes.Clock
}

// newHarness wires a real registryfs over the named fixtures to fake proc and
// pane gates, with every pid reported alive and every referenced pane reported
// present. A test then breaks exactly the one gate it is about, which keeps the
// reason a session is excluded unambiguous.
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

	// Seed the gates from what the fixtures actually reference, so adding a
	// fixture to a test does not silently make it dead.
	pre, err := reg.Read(context.Background())
	if err != nil {
		t.Fatalf("seeding from fixtures: %v", err)
	}
	proc := fakes.NewProc()
	var paneIDs []string
	for _, r := range pre.Records {
		proc.SetAlive(r.PID, true)
		if r.Tmux != "" {
			paneIDs = append(paneIDs, r.Tmux)
		}
	}
	panes := fakes.NewPanes(paneIDs...)

	obs, err := observer.New(observer.Deps{
		Registry: reg,
		// P3 made Jobs and Transcripts required ports. An EMPTY jobs source is
		// the right double for a registry-only test: this harness is about the
		// interactive population, and a nil port would instead mean "this
		// observer confidently reports that no background agents exist".
		Jobs:        &fakes.Jobs{},
		Transcripts: fakes.NewTranscripts(),
		Proc:        proc,
		Panes:       panes,
		Clock:       clock,
	}, observer.Config{Paths: observer.Paths{Registry: "/fixtures/sessions"}})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}

	return &harness{t: t, obs: obs, fsys: fsys, panes: panes, proc: proc, clock: clock}
}

func (h *harness) snap(opts observer.SnapshotOptions) *observer.Snapshot {
	h.t.Helper()
	s, err := h.obs.Snapshot(context.Background(), opts)
	if err != nil {
		h.t.Fatalf("Snapshot: %v", err)
	}
	return s
}

func (h *harness) defaultSnap() *observer.Snapshot {
	return h.snap(observer.DefaultSnapshotOptions())
}

func (h *harness) session(snap *observer.Snapshot, id string) observer.Session {
	h.t.Helper()
	s, ok := snap.SessionByID(id)
	if !ok {
		h.t.Fatalf("session %s not in the snapshot; present: %v", id, ids(snap))
	}
	return s
}

func ids(snap *observer.Snapshot) []string {
	out := make([]string, 0, len(snap.Sessions))
	for _, s := range snap.Sessions {
		out = append(out, s.SessionID)
	}
	return out
}

func hasAdvisory(s observer.Session, code string) bool {
	for _, a := range s.Advisories {
		if a.Code == code {
			return true
		}
	}
	return false
}

func advisoryCodes(s observer.Session) []string {
	out := make([]string, 0, len(s.Advisories))
	for _, a := range s.Advisories {
		out = append(out, a.Code)
	}
	return out
}

// The fixture session ids, named so a failure message says which fixture it is
// about rather than quoting a UUID.
const (
	idIdle            = "f350c44d-d22a-4c47-b3ef-00021713db7f" // live-idle
	idBusy            = "0b3a1d41-1c2f-4a56-9d0e-7f2b10a4c8e1" // live-busy
	idShell           = "2f9c6e77-4b18-4d2a-9c31-55ad0e6f7b02" // live-shell
	idWaitingInput    = "7d1e4b92-8a0c-4f37-b6d5-2e91c3a05f48" // waiting + "input needed"
	idWaitingPrompt   = "a4c07f15-3d6b-42e8-8f19-0c5b7e2a91d6" // waiting + "permission prompt"
	idWaitingNoReason = "371ebab5-6d54-4184-863e-103ba71f6ea9" // waiting, no waitingFor
	idDeadPID         = "c8b2e930-5f41-4a7d-9e08-3b6c1d4f7a25" // dead-pid
	idIdle3Days       = "6ae3c8cb-426b-4d4c-a218-ca04fe0b5402" // idle-3-days
	idWaiting3Days    = "9e57a0d3-2b64-4c18-8ad7-16f0b9c4e237" // waiting-3-days
	idUnknownStatus   = "b1d4f082-7e35-496a-a2c8-40f7e5b19c63" // unknown-status
	idNoTmux          = "4a6b8d02-9e17-4c53-8b2f-c1d7e0a35946" // no-tmux-field
	idBusy31m         = "5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36" // busy-31-minutes
	idNoStatusTime    = "ce5f7a13-8204-4b96-a7d3-05e21c8b4f70" // no-status-updated-at
	idDuplicate       = "aaaaaaaa-1111-2222-3333-444444444444" // duplicate-sessionid-a/-b
	idWithFormerNames = "0a180476-fe5f-4e29-b117-e10dbca60839" // with-former-names
)

// ---------------------------------------------------------------------------
// AC1 — enumerates every live session and EXCLUDES dead records
// ---------------------------------------------------------------------------

func TestAC1_ADeadPidIsExcludedFromTheDefaultSetAndRetrievableWithIncludeDead(t *testing.T) {
	h := newHarness(t, "live-idle.json", "dead-pid.json")
	// Dead pids cannot be relied on live — a reboot leaves every record's pid
	// alive — so the death is injected. Measured fact 8: records are NEVER
	// deleted, and 32 of 63 were dead pids before a reboot.
	h.proc.SetAlive(999001, false)

	def := h.defaultSnap()
	if _, ok := def.SessionByID(idDeadPID); ok {
		t.Errorf("the dead-pid record is in the default set: %v", ids(def))
	}
	if _, ok := def.SessionByID(idIdle); !ok {
		t.Errorf("the live record is missing from the default set: %v", ids(def))
	}
	if def.Counts.Total != 1 || def.Counts.Live != 1 || def.Counts.Dead != 0 {
		t.Errorf("counts = %+v; the default set holds only the live session", def.Counts)
	}

	withDead := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})
	dead := h.session(withDead, idDeadPID)
	if dead.Live == nil || *dead.Live {
		t.Errorf("live = %v, want an explicit false", fmtBool(dead.Live))
	}
	if dead.LiveReason != observer.LiveReasonPIDDead {
		t.Errorf("liveReason = %q, want %q", dead.LiveReason, observer.LiveReasonPIDDead)
	}
	if dead.ProcAlive == nil || *dead.ProcAlive {
		t.Errorf("procAlive = %v, want an explicit false", fmtBool(dead.ProcAlive))
	}
	if withDead.Counts.Total != 2 || withDead.Counts.Dead != 1 || withDead.Counts.Live != 1 {
		t.Errorf("counts with includeDead = %+v", withDead.Counts)
	}
}

func TestAC1_APaneThatNoLongerExistsExcludesTheSessionEvenWithALivePid(t *testing.T) {
	// Liveness needs BOTH gates (fact 8). A live pid alone is not enough: a
	// Claude process whose pane has gone is unreachable, which is what the
	// user actually cares about.
	h := newHarness(t, "live-idle.json", "live-busy.json")
	h.panes.Set("madbench:@51.%107") // magai:@15.%24 is gone

	def := h.defaultSnap()
	if _, ok := def.SessionByID(idBusy); ok {
		t.Errorf("a session whose pane is gone is in the default set: %v", ids(def))
	}

	withDead := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})
	gone := h.session(withDead, idBusy)
	if gone.LiveReason != observer.LiveReasonPaneGone {
		t.Errorf("liveReason = %q, want %q", gone.LiveReason, observer.LiveReasonPaneGone)
	}
	if gone.ProcAlive == nil || !*gone.ProcAlive {
		t.Error("procAlive should still be true: the process is alive, the pane is not")
	}
	if gone.PaneAlive == nil || *gone.PaneAlive {
		t.Errorf("paneAlive = %v, want an explicit false", fmtBool(gone.PaneAlive))
	}
}

func TestAC1_AnUnknownLivenessSessionIsNeverHiddenByTheDefaultFilter(t *testing.T) {
	// M7. The default filter is `live != false`, NOT `live == true`: a session
	// vanishing from the default view requires POSITIVE evidence of death. A
	// pane gate that could not answer is not evidence of anything.
	h := newHarness(t, "live-idle.json")
	h.panes.Err = errNoServer{}

	def := h.defaultSnap()
	s := h.session(def, idIdle)
	if s.Live != nil {
		t.Errorf("live = %v, want null on the first snapshot with a broken gate", fmtBool(s.Live))
	}
	if s.LiveReason != observer.LiveReasonGateError {
		t.Errorf("liveReason = %q, want %q", s.LiveReason, observer.LiveReasonGateError)
	}
	if !hasAdvisory(s, observer.AdvLivenessUnknown) {
		t.Errorf("advisories = %v, want liveness-unknown", advisoryCodes(s))
	}
	if s.PaneAlive != nil {
		t.Errorf("paneAlive = %v; a pane set we could not read is not an empty pane set", fmtBool(s.PaneAlive))
	}
	if def.Counts.Unknown != 1 || def.Counts.Total != 1 {
		t.Errorf("counts = %+v; an unknown-liveness session needs a home in the accounting", def.Counts)
	}
	if def.Sources[observer.SourceTmux].OK {
		t.Error("the tmux source reports ok despite the failure")
	}
}

func TestAC1_ASessionWithNoPaneReferenceIsLiveOnTheProcGateAlone(t *testing.T) {
	// B6: the `tmux` field is NOT structurally guaranteed. Waiving the pane
	// clause when there is no pane to check is different from waiving it
	// because the check failed.
	h := newHarness(t, "no-tmux-field.json")

	s := h.session(h.defaultSnap(), idNoTmux)
	if s.Live == nil || !*s.Live {
		t.Fatalf("live = %v, want true", fmtBool(s.Live))
	}
	if s.LiveReason != observer.LiveReasonProcOnly {
		t.Errorf("liveReason = %q, want %q", s.LiveReason, observer.LiveReasonProcOnly)
	}
	if s.Tmux != nil {
		t.Errorf("tmux = %+v, want null", s.Tmux)
	}
	if s.PaneAlive != nil {
		t.Errorf("paneAlive = %v, want null — there was nothing to check", fmtBool(s.PaneAlive))
	}
}

func TestAC1_APaneGateFailureDoesNotAffectASessionWithNoPaneReference(t *testing.T) {
	h := newHarness(t, "no-tmux-field.json", "live-idle.json")
	h.panes.Err = errNoServer{}

	def := h.defaultSnap()
	noTmux := h.session(def, idNoTmux)
	if noTmux.Live == nil || !*noTmux.Live {
		t.Errorf("a session with no pane reference lost its liveness to another session's gate failure: live = %v",
			fmtBool(noTmux.Live))
	}
	withTmux := h.session(def, idIdle)
	if withTmux.Live != nil {
		t.Errorf("the session that DOES reference a pane should be null: live = %v", fmtBool(withTmux.Live))
	}
}

func TestAC1_TheCountsInvariantHolds(t *testing.T) {
	h := newHarness(t,
		"live-idle.json", "live-busy.json", "dead-pid.json",
		"live-waiting-input-needed.json", "no-tmux-field.json")
	h.proc.SetAlive(999001, false)
	// And one session with no evidence either way, so all three buckets are
	// populated at once.
	h.proc.SetError(15775, errGateBroken{})

	snap := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})
	c := snap.Counts
	if c.Total != c.Live+c.Dead+c.Unknown {
		t.Errorf("total %d != live %d + dead %d + unknown %d", c.Total, c.Live, c.Dead, c.Unknown)
	}
	if c.Total != len(snap.Sessions) {
		t.Errorf("total %d != %d sessions returned", c.Total, len(snap.Sessions))
	}
	if c.Unknown == 0 || c.Dead == 0 || c.Live == 0 {
		t.Errorf("counts = %+v; this fixture set should populate all three buckets", c)
	}

	// byStatus and byKind always carry every known key, including zeros, so the
	// encoded form never varies by which statuses happened to occur.
	for _, st := range observer.AllStatuses {
		if _, ok := c.ByStatus[st]; !ok {
			t.Errorf("byStatus is missing the key %q", st)
		}
	}
	for _, k := range observer.AllKinds {
		if _, ok := c.ByKind[k]; !ok {
			t.Errorf("byKind is missing the key %q", k)
		}
	}
}

func TestAC1_ADeadRecordsStaleAttentionNeverContributesToTheAttentionCount(t *testing.T) {
	// A dead record's last known status was `waiting`. Counting it would put a
	// permanent "1 session needs you" in front of the user, pointing at a pane
	// that does not exist.
	h := newHarness(t, "live-waiting-input-needed.json")
	h.proc.SetAlive(15775, false)

	snap := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})
	s := h.session(snap, idWaitingInput)
	if !s.Attention {
		t.Error("the record's own attention flag should still reflect its last known status")
	}
	if snap.Counts.Attention != 0 {
		t.Errorf("counts.attention = %d, want 0 for a dead record", snap.Counts.Attention)
	}
}

func TestAC1_AnUnknownLivenessSessionDoesContributeToTheAttentionCount(t *testing.T) {
	h := newHarness(t, "live-waiting-input-needed.json")
	h.panes.Err = errNoServer{}

	snap := h.defaultSnap()
	if snap.Counts.Attention != 1 {
		t.Errorf("counts.attention = %d, want 1: an unknown-liveness waiting session still needs the user", snap.Counts.Attention)
	}
}

// ---------------------------------------------------------------------------
// AC2 — reports `waiting` and distinguishes the two reasons
// ---------------------------------------------------------------------------

func TestAC2_TheTwoWaitingReasonsAreDistinguished(t *testing.T) {
	h := newHarness(t, "live-waiting-input-needed.json", "live-waiting-permission-prompt.json")
	snap := h.defaultSnap()

	input := h.session(snap, idWaitingInput)
	if input.Status != observer.StatusWaiting {
		t.Errorf("status = %q, want waiting", input.Status)
	}
	if input.WaitReason != observer.WaitReasonInputNeeded {
		t.Errorf("waitReason = %q, want input-needed", input.WaitReason)
	}
	if input.RawWaitingFor == nil || *input.RawWaitingFor != "input needed" {
		t.Errorf("rawWaitingFor = %v, want the literal %q", input.RawWaitingFor, "input needed")
	}

	prompt := h.session(snap, idWaitingPrompt)
	if prompt.Status != observer.StatusWaiting {
		t.Errorf("status = %q, want waiting", prompt.Status)
	}
	if prompt.WaitReason != observer.WaitReasonPermissionPrompt {
		t.Errorf("waitReason = %q, want permission-prompt", prompt.WaitReason)
	}
	if prompt.RawWaitingFor == nil || *prompt.RawWaitingFor != "permission prompt" {
		t.Errorf("rawWaitingFor = %v, want the literal %q", prompt.RawWaitingFor, "permission prompt")
	}

	if input.WaitReason == prompt.WaitReason {
		t.Fatal("the two reasons are not distinguished, which is the whole of AC2")
	}
	if !input.Attention || !prompt.Attention {
		t.Error("a waiting session must set attention")
	}
}

func TestAC2_WaitingWithNoReasonIsUnknownNotEmpty(t *testing.T) {
	// The session IS waiting; we just do not know for what. That is different
	// from "no reason applies", and collapsing the two would lose the ability
	// to tell a consumer "someone is blocked, go look".
	h := newHarness(t, "waiting-unknown-reason.json")
	s := h.session(h.defaultSnap(), idWaitingNoReason)

	if s.Status != observer.StatusWaiting {
		t.Errorf("status = %q, want waiting", s.Status)
	}
	if s.WaitReason != observer.WaitReasonUnknown {
		t.Errorf("waitReason = %q, want unknown", s.WaitReason)
	}
	if s.RawWaitingFor != nil {
		t.Errorf("rawWaitingFor = %v, want null when the key is absent", s.RawWaitingFor)
	}
	if !s.Attention {
		t.Error("a waiting session with an unknown reason still needs the user")
	}
}

func TestAC2_AnUnrecognisedWaitingForValueIsAlsoUnknownAndKeptVerbatim(t *testing.T) {
	// A future Claude adding a third waitingFor value must not blank the
	// session or invent a reason. B7: this is an observed set, not a closed
	// enum.
	h := newHarness(t)
	h.fsys["future.json"] = &fstest.MapFile{Data: []byte(`{
		"pid": 31337, "sessionId": "ffffffff-0000-0000-0000-000000000001",
		"status": "waiting", "waitingFor": "quantum entanglement",
		"statusUpdatedAt": 1789699206204, "tmux": "future:@1.%1"
	}`)}
	h.proc.SetAlive(31337, true)
	h.panes.Set("future:@1.%1")

	s := h.session(h.defaultSnap(), "ffffffff-0000-0000-0000-000000000001")
	if s.Status != observer.StatusWaiting {
		t.Errorf("status = %q, want waiting", s.Status)
	}
	if s.WaitReason != observer.WaitReasonUnknown {
		t.Errorf("waitReason = %q, want unknown", s.WaitReason)
	}
	if s.RawWaitingFor == nil || *s.RawWaitingFor != "quantum entanglement" {
		t.Errorf("rawWaitingFor = %v; normalisation must never destroy the input", s.RawWaitingFor)
	}
}

func TestAC2_WaitReasonIsEmptyForEveryNonWaitingStatus(t *testing.T) {
	// H3's rule, stated once and asserted here: "" is the value for "not
	// applicable", from every source, and waitReason is NEVER null.
	h := newHarness(t,
		"live-idle.json", "live-busy.json", "live-shell.json",
		"unknown-status.json", "idle-3-days.json")
	snap := h.defaultSnap()

	for _, s := range snap.Sessions {
		if s.Status == observer.StatusWaiting {
			t.Fatalf("fixture set unexpectedly contains a waiting session: %s", s.SessionID)
		}
		if s.WaitReason != observer.WaitReasonNone {
			t.Errorf("%s (status %q): waitReason = %q, want \"\"", s.SessionID, s.Status, s.WaitReason)
		}
		if s.Attention {
			t.Errorf("%s (status %q): attention = true", s.SessionID, s.Status)
		}
	}

	// The statuses that reach the same rule from the OTHER source vocabulary.
	// The jobs adapter arrives in P3, so the mapping table is asserted
	// directly here — the rule is "from every source", and it is the mapping
	// that has to hold it, not the adapter.
	for _, raw := range []string{"blocked", "working", "done", "failed", "reticulating"} {
		status, reason := observer.MapJobState(raw)
		if reason != observer.WaitReasonNone {
			t.Errorf("MapJobState(%q) = (%q, %q); a job state never yields a waiting-vocabulary reason",
				raw, status, reason)
		}
	}
	// blocked in particular: `needs` carries the blocked reason in plain
	// words, and waitReason is the waitingFor enum's domain.
	if st, reason := observer.MapJobState("blocked"); st != observer.StatusBlocked || reason != "" {
		t.Errorf("MapJobState(\"blocked\") = (%q, %q), want (blocked, \"\")", st, reason)
	}
}

func TestAC2_WaitReasonIsNeverNullOnTheWire(t *testing.T) {
	// The rule is only worth anything if it survives serialisation: all four
	// surfaces encode through this one constructor, so a wrong-but-consistent
	// value would pass AC11's byte-identity assertion.
	h := newHarness(t, "live-idle.json", "live-waiting-input-needed.json", "waiting-unknown-reason.json")
	encoded, err := json.Marshal(wire.NewSnapshot(h.defaultSnap(), tRef, wire.Options{TT: "test"}))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	// Decoded into maps rather than into a local struct on purpose: §5.1 bans
	// json struct tags anywhere outside wire, with no exceptions to reason
	// about, and TestJSONTagsExistOnlyInWire enforces that over test files
	// too. A tagged struct here would be a second, private description of the
	// wire shape — which is the exact thing the ban exists to prevent.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	var sessions []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["sessions"], &sessions); err != nil {
		t.Fatalf("decoding sessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions on the wire, got %d", len(sessions))
	}

	want := map[string]string{
		idIdle:            `""`,
		idWaitingInput:    `"input-needed"`,
		idWaitingNoReason: `"unknown"`,
	}
	for _, s := range sessions {
		var id string
		if err := json.Unmarshal(s["sessionId"], &id); err != nil {
			t.Fatal(err)
		}
		got := string(s["waitReason"])
		if got == "null" {
			t.Errorf("%s: waitReason is null on the wire, which the rule forbids outright", id)
		}
		if w, ok := want[id]; ok && got != w {
			t.Errorf("%s: waitReason = %s, want %s", id, got, w)
		}
	}
}

// ---------------------------------------------------------------------------
// AC3(a) — statusAgeMs for EVERY session, or explicitly null with an advisory
// ---------------------------------------------------------------------------

func TestAC3a_AThreeDayIdleSessionReportsThreeDays(t *testing.T) {
	// The criterion's exact case. Measured fact 3: statusUpdatedAt means "in
	// this status SINCE", not "last refreshed" — 23 of 32 sessions had it set
	// within 20 s of boot and stayed idle for hours, and that is CORRECT, not
	// stale.
	h := newHarness(t, "idle-3-days.json")
	s := h.session(h.defaultSnap(), idIdle3Days)

	if s.StatusAgeMs == nil {
		t.Fatalf("statusAgeMs is null; advisories: %v", advisoryCodes(s))
	}
	if *s.StatusAgeMs != threeDaysMs {
		t.Errorf("statusAgeMs = %d, want exactly %d (3 days)", *s.StatusAgeMs, threeDaysMs)
	}
	// AC3(b): not attention, so blockedForMs is null — and blockedForUnavailable
	// is ALSO null, because the value is not unavailable, it does not apply.
	if s.BlockedForMs != nil {
		t.Errorf("blockedForMs = %d, want null for an idle session", *s.BlockedForMs)
	}
	if s.BlockedForUnavailable != nil {
		t.Errorf("blockedForUnavailable = %q, want null: the value does not apply, it is not unavailable", *s.BlockedForUnavailable)
	}
	if len(s.Advisories) != 0 {
		t.Errorf("advisories = %v, want none for a well-formed idle record", advisoryCodes(s))
	}
}

func TestAC3a_StatusSinceIsTheRecordsOwnValueAndIsNeverCorrected(t *testing.T) {
	// AC3 forbids two specific corrections by name: against transcript mtime,
	// and from startedAt. The 3-day fixture's startedAt is 4 hours before
	// T_ref, so a startedAt derivation would report 4 hours instead of 3 days
	// and the difference is unmissable.
	h := newHarness(t, "idle-3-days.json")
	s := h.session(h.defaultSnap(), idIdle3Days)

	if s.StatusSince == nil {
		t.Fatal("statusSince is nil")
	}
	wantSince := time.UnixMilli(1789699211204 - threeDaysMs).UTC()
	if !s.StatusSince.Equal(wantSince) {
		t.Errorf("statusSince = %s, want the record's statusUpdatedAt verbatim (%s)",
			observer.FormatTime(*s.StatusSince), observer.FormatTime(wantSince))
	}
	if s.StartedAt == nil {
		t.Fatal("startedAt is nil")
	}
	if s.StatusSince.Equal(*s.StartedAt) {
		t.Error("statusSince was derived from startedAt, which AC3 forbids: startedAt is a PROCESS timestamp")
	}
	if s.StatusSource != observer.SourceRegistry {
		t.Errorf("statusSource = %q, want registry", s.StatusSource)
	}
	// No transcript was read at all in P1, so there is nothing for a mtime
	// correction to have come from — asserted rather than assumed.
	if s.TranscriptCollected || s.Transcript != nil {
		t.Error("the registry half of the join must not touch a transcript")
	}
	if s.LastActivityAt != nil {
		t.Error("lastActivityAt is set without a transcript read")
	}
}

func TestAC3a_StatusAgeMsIsExplicitlyNullWithANamedAdvisoryWhenUnknown(t *testing.T) {
	// "never absent, never 0-for-unknown". A zero would read as "changed
	// status this instant", which is the most misleading available answer.
	h := newHarness(t, "no-status-updated-at.json")
	s := h.session(h.defaultSnap(), idNoStatusTime)

	if s.StatusSince != nil {
		t.Errorf("statusSince = %v, want nil", s.StatusSince)
	}
	if s.StatusAgeMs != nil {
		t.Errorf("statusAgeMs = %d, want null", *s.StatusAgeMs)
	}
	if !hasAdvisory(s, observer.AdvStatusSinceUnavailable) {
		t.Errorf("advisories = %v, want %s", advisoryCodes(s), observer.AdvStatusSinceUnavailable)
	}

	// And it serialises as an explicit null rather than vanishing, because
	// omitempty is banned.
	encoded, err := json.Marshal(wire.NewSession(s))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"statusSince", "statusAgeMs"} {
		raw, present := fields[key]
		if !present {
			t.Errorf("%s is absent from the encoded form; omitempty is banned", key)
			continue
		}
		if string(raw) != "null" {
			t.Errorf("%s = %s, want null", key, raw)
		}
	}
}

func TestAC3a_StatusAgeMsIsPresentForEverySessionInEveryFixture(t *testing.T) {
	// The universal half of AC3(a), over the whole fixture set: every session
	// either has a number or has a null WITH the advisory. There is no third
	// state and no session without an answer.
	h := newHarness(t, allJSONFixtures(t)...)
	snap := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})
	if len(snap.Sessions) < 15 {
		t.Fatalf("only %d sessions built from the fixture set", len(snap.Sessions))
	}

	for _, s := range snap.Sessions {
		if s.StatusAgeMs != nil {
			continue
		}
		if !hasAdvisory(s, observer.AdvStatusSinceUnavailable) {
			t.Errorf("%s: statusAgeMs is null with no naming advisory (advisories: %v)",
				s.SessionID, advisoryCodes(s))
		}
	}
}

func TestAC3_StatusAgeMsEqualsObservedAtMinusStatusSinceForEveryFixture(t *testing.T) {
	// §9.2(c)'s identity test. It exists because the architecture's own
	// samples drifted from their derivations three revisions running: the
	// numbers were re-captured without being recomputed. In code the identity
	// is checkable, so it is checked.
	h := newHarness(t, allJSONFixtures(t)...)
	snap := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})

	for _, s := range snap.Sessions {
		if s.StatusSince == nil {
			if s.StatusAgeMs != nil {
				t.Errorf("%s: statusAgeMs is set with a nil statusSince", s.SessionID)
			}
			continue
		}
		if s.StatusAgeMs == nil {
			t.Errorf("%s: statusAgeMs is nil with a non-nil statusSince", s.SessionID)
			continue
		}
		want := snap.ObservedAt.Sub(*s.StatusSince).Milliseconds()
		if *s.StatusAgeMs != want {
			t.Errorf("%s: statusAgeMs = %d, but observedAt - statusSince = %d",
				s.SessionID, *s.StatusAgeMs, want)
		}
		if !s.ObservedAt.Equal(snap.ObservedAt) {
			t.Errorf("%s: session observedAt %s differs from the envelope's %s; the clock is read once per rebuild",
				s.SessionID, observer.FormatTime(s.ObservedAt), observer.FormatTime(snap.ObservedAt))
		}
	}
}

func TestAC3_TheClockIsReadOncePerSnapshotAndAgesMoveWithIt(t *testing.T) {
	h := newHarness(t, "idle-3-days.json")

	first := h.session(h.defaultSnap(), idIdle3Days)
	h.clock.Advance(90 * time.Minute)
	second := h.session(h.defaultSnap(), idIdle3Days)

	if *second.StatusAgeMs-*first.StatusAgeMs != int64(90*time.Minute/time.Millisecond) {
		t.Errorf("statusAgeMs moved by %d ms over 90 minutes of clock",
			*second.StatusAgeMs-*first.StatusAgeMs)
	}
	if !second.StatusSince.Equal(*first.StatusSince) {
		t.Error("statusSince moved with the clock; it is a measurement, not a derivation")
	}
}

// ---------------------------------------------------------------------------
// AC3(b) — blockedForMs equals statusAgeMs when attention, null otherwise
// ---------------------------------------------------------------------------

func TestAC3b_AThreeDayWaitingSessionReportsThreeDaysOnBothFields(t *testing.T) {
	h := newHarness(t, "waiting-3-days.json")
	s := h.session(h.defaultSnap(), idWaiting3Days)

	if !s.Attention {
		t.Fatal("attention = false for a waiting session")
	}
	if s.StatusAgeMs == nil || *s.StatusAgeMs != threeDaysMs {
		t.Errorf("statusAgeMs = %v, want %d", s.StatusAgeMs, threeDaysMs)
	}
	if s.BlockedForMs == nil {
		t.Fatalf("blockedForMs is null for an attention session; advisories: %v", advisoryCodes(s))
	}
	if *s.BlockedForMs != threeDaysMs {
		t.Errorf("blockedForMs = %d, want %d", *s.BlockedForMs, threeDaysMs)
	}
	if *s.BlockedForMs != *s.StatusAgeMs {
		t.Error("blockedForMs is a VIEW of statusAgeMs and must equal it exactly, not be measured again")
	}
	if s.BlockedForUnavailable != nil {
		t.Errorf("blockedForUnavailable = %q, want null when the value is available", *s.BlockedForUnavailable)
	}
}

func TestAC3b_BlockedForMsIsNullForEveryNonAttentionSession(t *testing.T) {
	h := newHarness(t, allJSONFixtures(t)...)
	snap := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})

	sawAttention := false
	for _, s := range snap.Sessions {
		if s.Attention {
			sawAttention = true
			if s.BlockedForMs == nil && s.BlockedForUnavailable == nil {
				t.Errorf("%s: attention with blockedForMs null and no blockedForUnavailable reason", s.SessionID)
			}
			if s.BlockedForMs != nil && (s.StatusAgeMs == nil || *s.BlockedForMs != *s.StatusAgeMs) {
				t.Errorf("%s: blockedForMs %v != statusAgeMs %v", s.SessionID, s.BlockedForMs, s.StatusAgeMs)
			}
			continue
		}
		if s.BlockedForMs != nil {
			t.Errorf("%s (status %q, attention false): blockedForMs = %d, want null",
				s.SessionID, s.Status, *s.BlockedForMs)
		}
		if s.BlockedForUnavailable != nil {
			t.Errorf("%s: blockedForUnavailable = %q on a non-attention session", s.SessionID, *s.BlockedForUnavailable)
		}
	}
	if !sawAttention {
		t.Fatal("no attention session in the fixture set, so the positive half was not exercised")
	}
}

func TestAC3b_BlockedForUnavailableCarriesTheReasonWhenAttentionAndUnmeasurable(t *testing.T) {
	// The no-statusUpdatedAt fixture is `waiting`, so attention is true and
	// blockedForMs cannot be computed. The criterion requires the REASON to be
	// published rather than the field simply being null.
	h := newHarness(t, "no-status-updated-at.json")
	s := h.session(h.defaultSnap(), idNoStatusTime)

	if !s.Attention {
		t.Fatal("the fixture should be waiting")
	}
	if s.BlockedForMs != nil {
		t.Errorf("blockedForMs = %d, want null", *s.BlockedForMs)
	}
	if s.BlockedForUnavailable == nil {
		t.Fatal("blockedForUnavailable is null while attention is true and the value is unmeasurable")
	}
	if *s.BlockedForUnavailable != observer.AdvStatusSinceUnavailable {
		t.Errorf("blockedForUnavailable = %q, want %q", *s.BlockedForUnavailable, observer.AdvStatusSinceUnavailable)
	}
}

func TestAC3b_BlockedForMsTracksTheClockNotTheRecord(t *testing.T) {
	h := newHarness(t, "live-waiting-input-needed.json")

	s := h.session(h.defaultSnap(), idWaitingInput)
	if *s.BlockedForMs != thirtyFiveMins {
		t.Errorf("blockedForMs = %d, want %d", *s.BlockedForMs, thirtyFiveMins)
	}

	h.clock.Advance(25 * time.Minute)
	later := h.session(h.defaultSnap(), idWaitingInput)
	if *later.BlockedForMs != thirtyFiveMins+25*60*1000 {
		t.Errorf("blockedForMs after 25 more minutes = %d, want %d",
			*later.BlockedForMs, thirtyFiveMins+25*60*1000)
	}
	// This is exactly the number a notification delay needs: "only tell me
	// after N minutes blocked" is a comparison against this field.
}

// ---------------------------------------------------------------------------
// §3.6 — expose the age, never correct the status
// ---------------------------------------------------------------------------

func TestStatusSuspectIsAnAdvisoryAndNeverChangesTheStatus(t *testing.T) {
	h := newHarness(t, "busy-31-minutes.json", "live-busy.json")
	snap := h.defaultSnap()

	long := h.session(snap, idBusy31m)
	if long.Status != observer.StatusBusy {
		t.Errorf("status = %q; an advisory must never change the status — the registry's value is Claude's own belief about itself",
			long.Status)
	}
	if *long.StatusAgeMs != thirtyOneMins {
		t.Errorf("statusAgeMs = %d, want %d", *long.StatusAgeMs, thirtyOneMins)
	}
	if !hasAdvisory(long, observer.AdvStatusSuspect) {
		t.Errorf("advisories = %v, want status-suspect past 30 minutes", advisoryCodes(long))
	}

	short := h.session(snap, idBusy)
	if hasAdvisory(short, observer.AdvStatusSuspect) {
		t.Errorf("a 5-second-busy session was flagged suspect: %v", advisoryCodes(short))
	}
}

func TestASessionWithANullStatusAgeIsExemptFromStatusSuspect(t *testing.T) {
	h := newHarness(t)
	h.fsys["busy-no-time.json"] = &fstest.MapFile{Data: []byte(`{
		"pid": 31338, "sessionId": "ffffffff-0000-0000-0000-000000000002",
		"status": "busy", "tmux": "future:@2.%2"
	}`)}
	h.proc.SetAlive(31338, true)
	h.panes.Set("future:@2.%2")

	s := h.session(h.defaultSnap(), "ffffffff-0000-0000-0000-000000000002")
	if hasAdvisory(s, observer.AdvStatusSuspect) {
		t.Errorf("status-suspect requires a non-null statusAgeMs by definition: %v", advisoryCodes(s))
	}
}

// ---------------------------------------------------------------------------
// The unknown-status pass-through
// ---------------------------------------------------------------------------

func TestAnUnrecognisedStatusPassesThroughWithItsLiteralKept(t *testing.T) {
	h := newHarness(t, "unknown-status.json")
	s := h.session(h.defaultSnap(), idUnknownStatus)

	if s.Status != observer.StatusUnknown {
		t.Errorf("status = %q, want unknown", s.Status)
	}
	if s.RawStatus != "reticulating" {
		t.Errorf("rawStatus = %q; normalisation must never destroy the input", s.RawStatus)
	}
	if s.StatusSource != observer.SourceRegistry {
		t.Errorf("statusSource = %q", s.StatusSource)
	}
	// And it is still a live session. An unknown status says nothing about
	// whether the process is running.
	if s.Live == nil || !*s.Live {
		t.Errorf("live = %v; an unrecognised status is not evidence of death", fmtBool(s.Live))
	}
}

// ---------------------------------------------------------------------------
// §3.2's duplicate rule
// ---------------------------------------------------------------------------

func TestTwoRecordsWithOneSessionIdResolveDeterministically(t *testing.T) {
	// Presence of sessionId was measured on every record in every sample;
	// uniqueness was NOT. So the rule is deterministic by construction rather
	// than by hope: the first record in the fixed order (pid ascending) wins
	// and the loser is discarded, not merged.
	//
	// Asserted twice with the input order reversed, because the defect this
	// guards against is a join that depends on enumeration order.
	for _, order := range [][]string{
		{"duplicate-sessionid-a.json", "duplicate-sessionid-b.json"},
		{"duplicate-sessionid-b.json", "duplicate-sessionid-a.json"},
	} {
		h := newHarness(t, order...)
		snap := h.defaultSnap()

		if len(snap.Sessions) != 1 {
			t.Fatalf("order %v: expected 1 session, got %d", order, len(snap.Sessions))
		}
		s := h.session(snap, idDuplicate)
		if s.PID == nil || *s.PID != 1000 {
			t.Errorf("order %v: winner pid = %v, want 1000 (lowest pid, fixed order)", order, s.PID)
		}
		if s.Name != "dupe-first" {
			t.Errorf("order %v: winner name = %q; the loser must be discarded, not merged", order, s.Name)
		}
		if !hasAdvisory(s, observer.AdvSessionIDDuplicate) {
			t.Errorf("order %v: advisories = %v, want sessionid-duplicate", order, advisoryCodes(s))
		}
		for _, a := range s.Advisories {
			if a.Code != observer.AdvSessionIDDuplicate {
				continue
			}
			if a.Fields["winner"] != int64(1000) || a.Fields["loser"] != int64(2000) {
				t.Errorf("order %v: advisory fields = %v, want winner 1000 / loser 2000", order, a.Fields)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// §3.5's gate-error rule
// ---------------------------------------------------------------------------

func TestAGateErrorKeepsThePreviousAnswerRatherThanBlankingIt(t *testing.T) {
	// The table in §3.5 says `live = null` and the same row's text says "the
	// previous value is KEPT". Keeping it is the substantive instruction:
	// discarding a good answer on a transient failure would blank every
	// session's liveness at once on one tmux hiccup.
	h := newHarness(t, "live-idle.json")

	first := h.session(h.defaultSnap(), idIdle)
	if first.Live == nil || !*first.Live {
		t.Fatalf("first snapshot: live = %v, want true", fmtBool(first.Live))
	}

	h.panes.Err = errNoServer{}
	second := h.session(h.defaultSnap(), idIdle)

	if second.Live == nil || !*second.Live {
		t.Errorf("second snapshot: live = %v, want the previous true to be kept", fmtBool(second.Live))
	}
	if second.LiveReason != observer.LiveReasonGateError {
		t.Errorf("liveReason = %q, want gate-error so a consumer can always tell", second.LiveReason)
	}
	if !hasAdvisory(second, observer.AdvLivenessUnknown) {
		t.Errorf("advisories = %v, want liveness-unknown", advisoryCodes(second))
	}
}

func TestBothGateFailuresAreReportedNotJustTheFirst(t *testing.T) {
	h := newHarness(t, "live-idle.json")
	h.proc.SetError(78761, errGateBroken{})
	h.panes.Err = errNoServer{}

	s := h.session(h.defaultSnap(), idIdle)
	fields := map[string]bool{}
	for _, a := range s.Advisories {
		if a.Code != observer.AdvLivenessUnknown {
			continue
		}
		if f, ok := a.Fields["field"].(string); ok {
			fields[f] = true
		}
	}
	if !fields["procAlive"] || !fields["paneAlive"] {
		t.Errorf("both gate failures should be visible; got advisory fields %v", fields)
	}
}

// ---------------------------------------------------------------------------
// §6.5 — a torn record means TORN, not gone
// ---------------------------------------------------------------------------

func TestATornRecordKeepsItsPreviousSessionRatherThanRemovingIt(t *testing.T) {
	// "One malformed record blanked everything" is named in
	// validation-criteria.md as a real prior failure of this repository.
	// Removal requires POSITIVE evidence: a registry file gone AND a dead pid.
	// A file we could not parse is neither.
	h := newHarness(t, "live-idle.json", "live-busy.json")

	before := h.session(h.defaultSnap(), idIdle)
	if before.Stale {
		t.Fatal("the first snapshot is already stale")
	}

	torn, err := os.ReadFile(filepath.Join(fixtureDir, "torn.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.fsys["live-idle.json"] = &fstest.MapFile{Data: torn, Mode: 0o600}

	snap := h.defaultSnap()
	carried := h.session(snap, idIdle)

	if carried.Status != before.Status || carried.Name != before.Name {
		t.Errorf("the carried-forward session changed: %q/%q -> %q/%q",
			before.Status, before.Name, carried.Status, carried.Name)
	}
	if !carried.Stale {
		t.Error("stale = false; a carried-forward answer must say so")
	}
	if !hasAdvisory(carried, observer.AdvParseRetryFailed) {
		t.Errorf("advisories = %v, want parse-retry-failed", advisoryCodes(carried))
	}
	// The other session is completely unaffected. This is the assertion the
	// prior bug would have failed.
	if _, ok := snap.SessionByID(idBusy); !ok {
		t.Errorf("the unrelated session vanished: %v", ids(snap))
	}
	// And the age still moves, because statusAgeMs is our arithmetic and
	// freezing it would make a stale session look fresh.
	if carried.StatusAgeMs == nil {
		t.Error("statusAgeMs was dropped on the carried-forward session")
	}
}

func TestASecondConsecutiveUnreadableEventStopsAssertingTheStatus(t *testing.T) {
	h := newHarness(t, "live-idle.json")
	_ = h.defaultSnap()

	torn, err := os.ReadFile(filepath.Join(fixtureDir, "torn.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.fsys["live-idle.json"] = &fstest.MapFile{Data: torn, Mode: 0o600}

	firstFailure := h.session(h.defaultSnap(), idIdle)
	if firstFailure.Status != observer.StatusIdle {
		t.Errorf("one failed event should not change the status, got %q", firstFailure.Status)
	}

	secondFailure := h.session(h.defaultSnap(), idIdle)
	if secondFailure.Status != observer.StatusUnknown {
		t.Errorf("status = %q after two consecutive unreadable events, want unknown", secondFailure.Status)
	}
	if secondFailure.RawStatus != "idle" {
		t.Errorf("rawStatus = %q; the last thing the vendor actually said must stay legible", secondFailure.RawStatus)
	}
	if !hasAdvisory(secondFailure, observer.AdvUnparseable) {
		t.Errorf("advisories = %v, want unparseable", advisoryCodes(secondFailure))
	}
}

// ---------------------------------------------------------------------------
// The envelope
// ---------------------------------------------------------------------------

func TestTheSourceReportsDescribeWhatWasRead(t *testing.T) {
	h := newHarness(t, "live-idle.json", "live-busy.json", "no-session-id.json")
	snap := h.defaultSnap()

	reg, ok := snap.Sources[observer.SourceRegistry]
	if !ok {
		t.Fatalf("no registry source report; got %v", sourceIDs(snap))
	}
	if reg.Role != "authoritative" {
		t.Errorf("registry role = %q, want authoritative", reg.Role)
	}
	if reg.Path != "/fixtures/sessions" {
		t.Errorf("registry path = %q", reg.Path)
	}
	if !reg.OK {
		t.Errorf("registry ok = false, lastError = %v; one unusable record is not a source failure", reg.LastError)
	}
	if reg.Counters["records"] != 2 {
		t.Errorf("records = %d, want 2", reg.Counters["records"])
	}
	if reg.Counters["parseErrors"] != 1 {
		t.Errorf("parseErrors = %d, want 1", reg.Counters["parseErrors"])
	}

	tm, ok := snap.Sources[observer.SourceTmux]
	if !ok {
		t.Fatalf("no tmux source report; got %v", sourceIDs(snap))
	}
	if tm.Role != "gate" {
		t.Errorf("tmux role = %q, want gate", tm.Role)
	}
	if tm.Counters["panes"] != 2 {
		t.Errorf("panes = %d, want 2", tm.Counters["panes"])
	}
}

func TestTheGatesAreAskedOncePerSnapshotNotOncePerSession(t *testing.T) {
	h := newHarness(t, "live-idle.json", "live-busy.json", "live-shell.json",
		"idle-3-days.json", "waiting-3-days.json")
	before := h.panes.Calls()
	_ = h.defaultSnap()

	if got := h.panes.Calls() - before; got != 1 {
		t.Errorf("the pane gate was asked %d times for 5 sessions; one `tmux list-panes -a` covers the whole server", got)
	}
}

func TestNoTmuxCallIsMadeWhenNoSessionReferencesAPane(t *testing.T) {
	// A machine with no interactive panes should not be shelling out to tmux
	// at all, and a tmux failure there is not a degraded answer — there was no
	// question.
	h := newHarness(t, "no-tmux-field.json")
	h.panes.Err = errNoServer{}

	snap := h.defaultSnap()
	if h.panes.Calls() != 0 {
		t.Errorf("the pane gate was asked %d times with nothing to ask about", h.panes.Calls())
	}
	if _, ok := snap.Sources[observer.SourceTmux]; ok {
		t.Error("a tmux source is reported although tmux was never asked")
	}
	if s := h.session(snap, idNoTmux); s.Live == nil || !*s.Live {
		t.Errorf("live = %v, want true", fmtBool(s.Live))
	}
}

func TestAnAbsentRegistryIsAnEmptySnapshotNotAnError(t *testing.T) {
	clock := fakes.NewClock(tRef)
	reg := registryfs.New(filepath.Join(t.TempDir(), "nope"), clock)
	obs, err := observer.New(observer.Deps{
		Registry: reg, Jobs: &fakes.Jobs{}, Transcripts: fakes.NewTranscripts(),
		Proc: fakes.NewProc(), Panes: fakes.NewPanes(), Clock: clock,
	}, observer.Config{})
	if err != nil {
		t.Fatalf("observer.New: %v", err)
	}

	snap, err := obs.Snapshot(context.Background(), observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("an absent registry produced an error: %v", err)
	}
	if len(snap.Sessions) != 0 || snap.Counts.Total != 0 {
		t.Errorf("sessions = %d, counts = %+v", len(snap.Sessions), snap.Counts)
	}
	if !snap.Sources[observer.SourceRegistry].OK {
		t.Error("the registry source is not ok; \"Claude has never run here\" is a fact, not a failure")
	}
	if !snap.ObservedAt.Equal(observer.TruncateTime(tRef)) {
		t.Errorf("observedAt = %s, want the injected clock", observer.FormatTime(snap.ObservedAt))
	}
}

func TestTheSessionsAreReturnedInAStableOrder(t *testing.T) {
	// Two snapshots of the same state must list sessions identically, or
	// AC11's byte-identity assertion is decided by map iteration order.
	h := newHarness(t, allJSONFixtures(t)...)
	opts := observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true}

	first := ids(h.snap(opts))
	second := ids(h.snap(opts))
	if len(first) != len(second) {
		t.Fatalf("session count changed between identical snapshots: %d -> %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("order differs at %d: %q vs %q", i, first[i], second[i])
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Errorf("sessions are not sorted by id: %q >= %q", first[i-1], first[i])
		}
	}
}

func TestAbsentListsSerialiseAsEmptyArraysNotNull(t *testing.T) {
	// §5.2's samples show "formerNames": []. The codec preserves nil-ness
	// exactly, which is why the core has to make this choice explicitly rather
	// than inherit it.
	h := newHarness(t, "missing-optional.json", "with-former-names.json")
	snap := h.defaultSnap()

	for _, s := range snap.Sessions {
		if s.FormerNames == nil {
			t.Errorf("%s: formerNames is nil, which encodes as null", s.SessionID)
		}
		if s.Advisories == nil {
			t.Errorf("%s: advisories is nil, which encodes as null", s.SessionID)
		}
		if len(s.Sources) == 0 || s.Sources[0] != observer.SourceRegistry {
			t.Errorf("%s: sources = %v, want [registry]", s.SessionID, s.Sources)
		}
		if len(s.ListedBy) == 0 || s.ListedBy[0] != observer.SourceRegistry {
			t.Errorf("%s: listedBy = %v, want [registry]", s.SessionID, s.ListedBy)
		}
	}
	if snap.Warnings == nil {
		t.Error("warnings is nil, which encodes as null")
	}

	former := h.session(snap, idWithFormerNames)
	if len(former.FormerNames) != 2 {
		t.Errorf("formerNames = %v, want the two names from the object form", former.FormerNames)
	}
}

func TestBackgroundOnlyFieldsAreNullForEveryInteractiveSession(t *testing.T) {
	// §5.2: needs, detail, tempo, tokens, children, resumeSessionId,
	// recordUpdatedAt, recordAgeMs and reapedMidWorkAt are background-only and
	// serialise as null for an interactive session. shortId too, by
	// construction (L4).
	h := newHarness(t, allJSONFixtures(t)...)
	snap := h.snap(observer.SnapshotOptions{IncludeDead: true, IncludeUnknownLiveness: true})

	for _, s := range snap.Sessions {
		if s.Kind != observer.KindInteractive {
			t.Errorf("%s: kind = %q; every registry record is interactive", s.SessionID, s.Kind)
		}
		switch {
		case s.ShortID != nil:
			t.Errorf("%s: shortId = %q, want null (L4: background-only by construction)", s.SessionID, *s.ShortID)
		case s.Needs != nil, s.Detail != nil, s.Tempo != nil, s.Tokens != nil,
			s.Children != nil, s.ResumeSessionID != nil, s.RecordUpdatedAt != nil,
			s.RecordAgeMs != nil, s.ReapedMidWorkAt != nil:
			t.Errorf("%s: a background-only field is populated on an interactive session", s.SessionID)
		case s.StatusSinceLowerBound != nil:
			t.Errorf("%s: statusSinceLowerBound is a jobs cross-check field", s.SessionID)
		}
		if s.PID == nil {
			t.Errorf("%s: pid is null on an interactive session", s.SessionID)
		}
	}
}

func TestTheFilterNarrowsWithoutChangingAnyField(t *testing.T) {
	h := newHarness(t, "live-idle.json", "live-busy.json", "live-waiting-input-needed.json")

	waiting := h.snap(observer.SnapshotOptions{
		IncludeUnknownLiveness: true,
		Filter:                 observer.Filter{WaitingOnly: true},
	})
	if len(waiting.Sessions) != 1 || waiting.Sessions[0].SessionID != idWaitingInput {
		t.Errorf("WaitingOnly returned %v", ids(waiting))
	}
	if waiting.Counts.Total != 1 {
		t.Errorf("counts.total = %d; counts describe the set that was returned", waiting.Counts.Total)
	}

	byCWD := h.snap(observer.SnapshotOptions{
		IncludeUnknownLiveness: true,
		Filter:                 observer.Filter{CWDPrefix: "/home/user/mag/magai"},
	})
	if len(byCWD.Sessions) != 2 {
		t.Errorf("CWDPrefix returned %v, want the two magai sessions", ids(byCWD))
	}

	byStatus := h.snap(observer.SnapshotOptions{
		IncludeUnknownLiveness: true,
		Filter:                 observer.Filter{Statuses: []observer.Status{observer.StatusBusy}},
	})
	if len(byStatus.Sessions) != 1 || byStatus.Sessions[0].SessionID != idBusy {
		t.Errorf("status filter returned %v", ids(byStatus))
	}

	// The zero Filter matches everything.
	all := h.snap(observer.SnapshotOptions{IncludeUnknownLiveness: true})
	if len(all.Sessions) != 3 {
		t.Errorf("the zero filter returned %v, want all three", ids(all))
	}
}

func TestNewRefusesAMissingPort(t *testing.T) {
	// A nil port is a programming error, not a degraded mode: the degraded
	// modes live inside the adapters, so "this machine has no kqueue" is a
	// value in the snapshot rather than a nil check in the core.
	clock := fakes.NewClock(tRef)
	full := observer.Deps{
		Registry:    registryfs.NewFS(fstest.MapFS{}, "/x", clock),
		Jobs:        &fakes.Jobs{},
		Transcripts: fakes.NewTranscripts(),
		Proc:        fakes.NewProc(),
		Panes:       fakes.NewPanes(),
		Clock:       clock,
	}
	if _, err := observer.New(full, observer.Config{}); err != nil {
		t.Fatalf("a complete Deps was refused: %v", err)
	}

	// Jobs and Transcripts joined this list in P3, alongside the code that
	// dereferences them. A nil Jobs port would make the observer answer "there
	// are no background agents on this machine" — well-formed, confident, and
	// indistinguishable from the truth.
	for name, mutate := range map[string]func(*observer.Deps){
		"Registry":   func(d *observer.Deps) { d.Registry = nil },
		"JobsSource": func(d *observer.Deps) { d.Jobs = nil },
		"Transcript": func(d *observer.Deps) { d.Transcripts = nil },
		"Proc":       func(d *observer.Deps) { d.Proc = nil },
		"Panes":      func(d *observer.Deps) { d.Panes = nil },
		"Clock":      func(d *observer.Deps) { d.Clock = nil },
	} {
		d := full
		mutate(&d)
		_, err := observer.New(d, observer.Config{})
		if err == nil {
			t.Errorf("a nil %s was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error for a nil %s does not name it: %v", name, err)
		}
	}
}

func TestTheDefaultPolicyAndNotCapableAreApplied(t *testing.T) {
	h := newHarness(t, "live-idle.json")
	if p := h.obs.Policy(); p != observer.DefaultPolicy() {
		t.Errorf("a zero Config.Policy was not replaced by the defaults: %+v", p)
	}

	snap := h.defaultSnap()
	if len(snap.NotCapable) == 0 || snap.NotCapable[0] != "time-transitions" {
		t.Errorf("notCapable = %v; a consumer needs to be told it will never get a frame at the instant a threshold is crossed",
			snap.NotCapable)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// allJSONFixtures is every .json fixture in the registry directory. Reading
// them as one registry is the widest realistic input, and several assertions
// above are properties that must hold for EVERY session rather than for a
// chosen one.
func allJSONFixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, e.Name())
		}
	}
	if len(out) < 20 {
		t.Fatalf("only %d json fixtures found; §9.1's registry set is larger", len(out))
	}
	return out
}

func sourceIDs(snap *observer.Snapshot) []string {
	out := make([]string, 0, len(snap.Sources))
	for id := range snap.Sources {
		out = append(out, id)
	}
	return out
}

func fmtBool(b *bool) string {
	if b == nil {
		return "null"
	}
	if *b {
		return "true"
	}
	return "false"
}

type errNoServer struct{}

func (errNoServer) Error() string { return "tmux list-panes: no server running" }

type errGateBroken struct{}

func (errGateBroken) Error() string { return "kill(0): resource temporarily unavailable" }
