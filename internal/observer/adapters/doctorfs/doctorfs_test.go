package doctorfs_test

// The doctor's tests are HERMETIC, which for this package is a stronger claim
// than usual: it is the one adapter whose whole job is to read the live machine,
// so a test that pointed it at ~/.claude would pass or fail depending on what
// the user's sessions happened to be doing. Every tree here is a t.TempDir, and
// the only assertions about live data live in the implementation log, where a
// measurement belongs.
//
// What each test is FOR, since "the doctor reports ok" is not a property worth
// asserting on its own: every check below is driven into BOTH verdicts it can
// produce. A checker that always says ok and a checker that looks at nothing are
// indistinguishable, which is the same reason §9.4's audits each carry a
// deliberately-violating snippet.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/doctorfs"
)

// tDoc is this file's reference instant.
var tDoc = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

// tree builds a synthetic ~/.claude. Every path the doctor reads is under one
// t.TempDir, so nothing here can reach the real one even by an absolute path in
// a fixture — linkScanPath included, which is why that field is written
// relative to this root.
type tree struct {
	root     string
	registry string
	jobs     string
	projects string
}

func newTree(t *testing.T) tree {
	t.Helper()
	root := t.TempDir()
	tr := tree{
		root:     root,
		registry: filepath.Join(root, "sessions"),
		jobs:     filepath.Join(root, "jobs"),
		projects: filepath.Join(root, "projects"),
	}
	for _, d := range []string{tr.registry, tr.jobs, tr.projects} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func (tr tree) paths() observer.Paths {
	return observer.Paths{
		ClaudeRoot: tr.root,
		Registry:   tr.registry,
		Jobs:       tr.jobs,
		Projects:   tr.projects,
	}
}

// writeJSON writes one object and sets its mtime, because two of the checks are
// about mtimes and a fixture that could not control them would be untestable.
func writeJSON(t *testing.T, path string, obj map[string]any, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

// registryRecord is a complete, healthy interactive record: all 19 universal
// keys, in the measured shape.
func registryRecord(sessionID, status string) map[string]any {
	return map[string]any{
		"cwd":                 "/home/user/dotfiles",
		"entrypoint":          "cli",
		"kind":                "interactive",
		"messagingSocketPath": "/tmp/claude-messaging.sock",
		"name":                "dotfiles",
		"nameSince":           "2026-09-18T09:00:00.000Z",
		"nameSource":          "cwd",
		"peerFeatures":        []string{},
		"peerProtocol":        1,
		"pid":                 4242,
		"pidDomain":           "local",
		"procStart":           1789684811204,
		"sessionId":           sessionID,
		"startedAt":           1789684811204,
		"status":              status,
		"statusUpdatedAt":     "2026-09-18T09:59:30.000Z",
		"tmux":                "madbench:@53.%109",
		"updatedAt":           "2026-09-18T09:59:30.000Z",
		"version":             "2.1.275",
	}
}

// jobRecord is a complete, healthy background record.
func jobRecord(tr tree, sessionID, state, tempo string) map[string]any {
	return map[string]any{
		"backend":         "claude",
		"children":        []any{},
		"createdAt":       "2026-09-18T08:00:00.000Z",
		"cwd":             "/home/user/dotfiles",
		"daemonShort":     sessionID[:8],
		"detail":          "",
		"firstTerminalAt": "2026-09-18T08:30:00.000Z",
		"intent":          "",
		"linkScanOffset":  100,
		"linkScanPath":    filepath.Join(tr.projects, "-home-user-dotfiles", sessionID+".jsonl"),
		"output":          "",
		"providerEnv":     map[string]any{},
		"respawnFlags":    []any{},
		"resumeSessionId": sessionID,
		"sessionId":       sessionID,
		"state":           state,
		"template":        "",
		"tempo":           tempo,
		"updatedAt":       "2026-09-18T09:00:00.000Z",
	}
}

// healthy builds a tree in which every §9.5 assumption holds.
func healthy(t *testing.T) tree {
	t.Helper()
	tr := newTree(t)

	writeJSON(t, filepath.Join(tr.registry, "4242.json"),
		registryRecord("5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", "idle"),
		tDoc.Add(-30*time.Second))
	// The .key sibling the registry keeps beside each record. Writing one here
	// is the assertion that the doctor never opens it: it is not valid JSON, so
	// a reader that picked it up would report a parse error.
	if err := os.WriteFile(filepath.Join(tr.registry, "4242.key"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	job := "97030e53-528c-4452-bbae-535400039def"
	writeJSON(t, filepath.Join(tr.jobs, "97030e53", "state.json"),
		jobRecord(tr, job, "blocked", "blocked"), tDoc.Add(-time.Hour))
	timeline := filepath.Join(tr.jobs, "97030e53", "timeline.jsonl")
	writeLines(t, timeline,
		`{"at":"2026-09-18T08:30:00.000Z","state":"working","detail":"","text":"start"}`,
		`{"at":"2026-09-18T09:00:00.000Z","state":"blocked","detail":"","text":"waiting"}`,
	)
	// pins.json is a FILE directly under jobs/ and must never be read as a job.
	writeJSON(t, filepath.Join(tr.jobs, "pins.json"), map[string]any{}, time.Time{})

	// Two transcripts: the job's, and an interactive one with a colour record,
	// an api error and a subagent directory beside it.
	writeLines(t, filepath.Join(tr.projects, "-home-user-dotfiles", job+".jsonl"),
		`{"type":"user","timestamp":"2026-09-18T09:00:00.000Z"}`,
		`{"type":"ai-title","title":"a record with no timestamp"}`,
	)
	writeLines(t, filepath.Join(tr.projects, "-home-user-dotfiles", "5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36.jsonl"),
		`{"type":"assistant","timestamp":"2026-09-18T09:58:00.000Z","isApiErrorMessage":false}`,
		`{"type":"agent-color","agentColor":"cyan"}`,
		`{"type":"mode","mode":"default"}`,
	)
	subagents := filepath.Join(tr.projects, "-home-user-dotfiles",
		"5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", "subagents")
	if err := os.MkdirAll(subagents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subagents, "agent-1.meta.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The retention floor: one transcript exactly at the 30-day window.
	old := filepath.Join(tr.projects, "-home-user-old", "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	writeLines(t, old, `{"type":"user","timestamp":"2026-08-19T10:00:00.000Z"}`)
	floor := tDoc.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, floor, floor); err != nil {
		t.Fatal(err)
	}
	// And one deeper file the recursive walk sees and the glob must not, older
	// than the floor — the divergence r3 M3 says must stay visible.
	deep := filepath.Join(tr.projects, "-home-user-old",
		"aaaaaaaa-0000-0000-0000-000000000000", "subagents", "agent-9.meta.jsonl")
	writeLines(t, deep, `{"type":"user","timestamp":"2026-06-01T10:00:00.000Z"}`)
	deepMod := tDoc.Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(deep, deepMod, deepMod); err != nil {
		t.Fatal(err)
	}

	return tr
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeAgents is a corroboration source under the test's control. It counts its
// calls, because the doctor's exemption from T5's spawn precondition
// (audit.AgentsSpawnAllowlist) is granted on the claim that it takes exactly one
// listing per run — and a claim an audit rests on should be a test.
type fakeAgents struct {
	res   observer.AgentsResult
	err   error
	calls *int
}

func (f fakeAgents) List(context.Context) (observer.AgentsResult, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.res, f.err
}

func run(t *testing.T, tr tree, agents observer.AgentsSource) observer.DoctorReport {
	t.Helper()
	return doctorfs.Run(context.Background(), doctorfs.Config{
		Paths:       tr.paths(),
		StateDir:    filepath.Join(tr.root, "state"),
		Agents:      agents,
		RetentionMs: 30 * 24 * 60 * 60 * 1000,
		Now:         tDoc,
		ProcStats: func(int) (string, int64, error) {
			return "0:00.42", 9_000, nil
		},
	})
}

// find returns one check by id.
func find(t *testing.T, r observer.DoctorReport, id string) observer.DoctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in the report; ids are %v", id, ids(r))
	return observer.DoctorCheck{}
}

func ids(r observer.DoctorReport) []string {
	out := make([]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		out = append(out, c.ID)
	}
	return out
}

func wantStatus(t *testing.T, r observer.DoctorReport, id string, want observer.DoctorStatus) observer.DoctorCheck {
	t.Helper()
	c := find(t, r, id)
	if c.Status != want {
		t.Errorf("%s is %s, want %s: %s\n  %s", id, c.Status, want, c.Detail,
			strings.Join(c.Evidence, "\n  "))
	}
	return c
}

// listing is a corroboration result that agrees with the healthy tree's records.
func listing() fakeAgents {
	return fakeAgents{res: observer.AgentsResult{
		OK:         true,
		ObservedAt: tDoc,
		Command:    "claude agents --json",
		Entries: []observer.AgentsEntry{
			{SessionID: "5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", PID: intPtr(4242), Kind: "interactive", Status: "idle"},
			{SessionID: "97030e53-528c-4452-bbae-535400039def", ID: "97030e53", Kind: "background", State: "blocked"},
		},
	}}
}

func intPtr(v int) *int { return &v }

// ---------------------------------------------------------------------------
// the healthy tree
// ---------------------------------------------------------------------------

// TestAHealthyTreePassesEveryCheck is the baseline every negative test below is
// measured against. It also pins the check set: a check added without a fixture
// to exercise it shows up here as a skip.
func TestAHealthyTreePassesEveryCheck(t *testing.T) {
	r := run(t, healthy(t), listing())

	if r.Worst() == observer.DoctorFail {
		for _, c := range r.Checks {
			if c.Status == observer.DoctorFail {
				t.Errorf("unexpected fail: %s — %s\n  %s", c.ID, c.Detail, strings.Join(c.Evidence, "\n  "))
			}
		}
	}

	for _, id := range []string{
		"registry-keys", "registry-status-values", "registry-tmux-shape",
		"registry-mtime-tracks-status", "registry-version-spread",
		"jobs-keys", "jobs-state-values", "jobs-updatedat-not-earlier-than-timeline",
		"jobs-timeline-shape", "jobs-linkscanpath", "jobs-signal-keys",
		"transcript-retention-floor", "transcript-deep-population",
		"transcript-untimestamped-types", "transcript-agent-color-shape",
		"subagent-meta-files", "agents-cli-shape", "terminal-state-gate",
		"observe-server",
	} {
		c := find(t, r, id)
		if c.Status == observer.DoctorFail || c.Status == observer.DoctorWarn {
			t.Errorf("%s: %s — %s", id, c.Status, c.Detail)
		}
		if len(c.Evidence) == 0 && c.Status == observer.DoctorOK {
			t.Errorf("%s passed with no evidence; an unfalsifiable pass is not a pass", id)
		}
		if c.Cite == "" {
			t.Errorf("%s carries no citation, so a failure would lead nowhere", id)
		}
	}
}

// TestEveryCheckCarriesADistinctID guards the one thing a grep-able report
// needs: two checks under one id make a script's filter silently ambiguous.
func TestEveryCheckCarriesADistinctID(t *testing.T) {
	r := run(t, healthy(t), listing())
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if seen[c.ID] {
			t.Errorf("duplicate check id %q", c.ID)
		}
		seen[c.ID] = true
		if c.Title == "" {
			t.Errorf("%s has no title", c.ID)
		}
	}
	if len(r.Checks) < 20 {
		t.Errorf("the report has %d checks; §9.5 enumerates more than that", len(r.Checks))
	}
}

// ---------------------------------------------------------------------------
// the negative cases — each check driven into its OTHER verdict
// ---------------------------------------------------------------------------

// TestAMissingUniversalRegistryKeyFails. A key this build READS going away
// breaks a feature, so it is a fail and not a warning — that distinction is the
// doctor's whole reason for existing.
func TestAMissingUniversalRegistryKeyFails(t *testing.T) {
	tr := healthy(t)
	rec := registryRecord("5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", "idle")
	delete(rec, "statusUpdatedAt")
	writeJSON(t, filepath.Join(tr.registry, "4242.json"), rec, tDoc.Add(-30*time.Second))

	r := run(t, tr, listing())
	c := wantStatus(t, r, "registry-keys", observer.DoctorFail)
	if !strings.Contains(c.Detail, "statusUpdatedAt") {
		t.Errorf("the failure does not name the missing key: %s", c.Detail)
	}
	if r.Worst() != observer.DoctorFail {
		t.Errorf("the report's verdict is %s, want fail", r.Worst())
	}
}

// TestANewRegistryKeyOnlyWarns. The opposite direction: the format grew, we are
// behind, nothing is broken. This is the live case at P10 — `parkedJobId`
// appeared on one of 34 records.
func TestANewRegistryKeyOnlyWarns(t *testing.T) {
	tr := healthy(t)
	rec := registryRecord("5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", "idle")
	rec["parkedJobId"] = "9d951ada"
	writeJSON(t, filepath.Join(tr.registry, "4242.json"), rec, tDoc.Add(-30*time.Second))

	c := wantStatus(t, run(t, tr, listing()), "registry-keys", observer.DoctorWarn)
	if !strings.Contains(c.Detail, "parkedJobId") {
		t.Errorf("the warning does not name the new key: %s", c.Detail)
	}
}

// TestAnUnmappedStatusWarnsAndAnUnmappedJobStateFails is the same asymmetry one
// level down: an unmapped registry status degrades to `unknown` and tt still
// works, while an unmapped job state breaks the terminal gate that liveness
// rule 1 is built on.
func TestAnUnmappedStatusWarnsAndAnUnmappedJobStateFails(t *testing.T) {
	tr := healthy(t)
	writeJSON(t, filepath.Join(tr.registry, "4242.json"),
		registryRecord("5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", "hibernating"),
		tDoc.Add(-30*time.Second))
	job := "97030e53-528c-4452-bbae-535400039def"
	writeJSON(t, filepath.Join(tr.jobs, "97030e53", "state.json"),
		jobRecord(tr, job, "quiesced", "idle"), tDoc.Add(-time.Hour))

	r := run(t, tr, listing())
	if c := wantStatus(t, r, "registry-status-values", observer.DoctorWarn); !strings.Contains(c.Detail, "hibernating") {
		t.Errorf("the warning does not name the value: %s", c.Detail)
	}
	if c := wantStatus(t, r, "jobs-state-values", observer.DoctorFail); !strings.Contains(c.Detail, "quiesced") {
		t.Errorf("the failure does not name the value: %s", c.Detail)
	}
}

// TestAStrippedTmuxReferenceFails is fact 7 as a check: the sigils are part of
// the identifiers and a stripped reference fails SILENTLY at the tmux level,
// which is exactly the kind of failure this tool exists to make loud.
func TestAStrippedTmuxReferenceFails(t *testing.T) {
	tr := healthy(t)
	rec := registryRecord("5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36", "idle")
	rec["tmux"] = "madbench:53.109" // the sigils stripped
	writeJSON(t, filepath.Join(tr.registry, "4242.json"), rec, tDoc.Add(-30*time.Second))

	c := wantStatus(t, run(t, tr, listing()), "registry-tmux-shape", observer.DoctorFail)
	if !strings.Contains(strings.Join(c.Evidence, " "), "0 match") {
		t.Errorf("the evidence does not show the match count: %v", c.Evidence)
	}
}

// TestAnEarlyUpdatedAtFails. §0.2(c) measured the error to have ONE direction —
// late, never early — and §3.4 publishes a bound that only works in that
// direction. An early updatedAt over-reports a blocked duration, which fires an
// attention notification that never clears.
func TestAnEarlyUpdatedAtFails(t *testing.T) {
	tr := healthy(t)
	job := "97030e53-528c-4452-bbae-535400039def"
	rec := jobRecord(tr, job, "blocked", "blocked")
	rec["updatedAt"] = "2026-09-18T08:45:00.000Z" // BEFORE the last timeline entry
	writeJSON(t, filepath.Join(tr.jobs, "97030e53", "state.json"), rec, tDoc.Add(-time.Hour))

	c := wantStatus(t, run(t, tr, listing()), "jobs-updatedat-not-earlier-than-timeline", observer.DoctorFail)
	if !strings.Contains(strings.Join(c.Evidence, " "), "1 EARLIER") {
		t.Errorf("the evidence does not count the early record: %v", c.Evidence)
	}
}

// TestTheTerminalStateGateReportsADisagreement is §3.5 rule 1's standing
// falsification test. It is a WARN rather than a fail because the join already
// handles the disagreement — it answers null and says why — but the rule has
// stopped being a fact and that must be visible.
func TestTheTerminalStateGateReportsADisagreement(t *testing.T) {
	tr := healthy(t)
	agents := listing()
	agents.res.Entries = agents.res.Entries[:1] // the blocked job vanishes from the listing

	c := wantStatus(t, run(t, tr, agents), "terminal-state-gate", observer.DoctorWarn)
	if !strings.Contains(strings.Join(c.Evidence, " "), "non-terminal but unlisted") {
		t.Errorf("the evidence does not name the disagreeing record: %v", c.Evidence)
	}
}

// TestAWedgedCorroborationDegradesRatherThanFails is D2 as a test: we trust the
// vendor CLI, so its failure is recorded as degradation. It is also the state
// this machine was measured in at P10 — three of three probes exiting 124 at
// 12 s — which is why the gate that needs a listing must SKIP rather than fail.
func TestAWedgedCorroborationDegradesRatherThanFails(t *testing.T) {
	r := run(t, healthy(t), fakeAgents{res: observer.AgentsResult{
		OK:      false,
		Command: "claude agents --json",
		Err:     fmt.Errorf("claude agents --json: timed out after 5s: context deadline exceeded"),
	}})

	c := wantStatus(t, r, "agents-cli-shape", observer.DoctorWarn)
	if !strings.Contains(c.Detail, "timed out") {
		t.Errorf("the warning does not carry the vendor's own error: %s", c.Detail)
	}
	gate := wantStatus(t, r, "terminal-state-gate", observer.DoctorSkip)
	if !strings.Contains(gate.Detail, "timed out") {
		t.Errorf("the skipped gate does not say why: %s", gate.Detail)
	}
	if r.Worst() == observer.DoctorFail {
		t.Error("a wedged vendor CLI produced a FAIL verdict; D2 makes it a degradation")
	}
}

// TestNoCorroborationSourceSkipsRatherThanPasses. A build with no port wired
// must not report the gate as ok: nothing was checked.
func TestNoCorroborationSourceSkipsRatherThanPasses(t *testing.T) {
	r := run(t, healthy(t), nil)
	wantStatus(t, r, "terminal-state-gate", observer.DoctorSkip)
	wantStatus(t, r, "agents-cli-shape", observer.DoctorWarn)
}

// TestAnAbsentTreeSkipsEveryCheckThatNeedsIt. "Claude has never run here" is a
// fact about the machine, not a defect in it, and it must not read as a
// diagnosis of anything.
func TestAnAbsentTreeSkipsEveryCheckThatNeedsIt(t *testing.T) {
	tr := newTree(t)
	if err := os.RemoveAll(tr.registry); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tr.jobs); err != nil {
		t.Fatal(err)
	}

	r := run(t, tr, nil)
	for _, id := range []string{
		"registry-keys", "registry-status-values", "registry-tmux-shape",
		"jobs-keys", "jobs-state-values", "jobs-linkscanpath", "terminal-state-gate",
	} {
		wantStatus(t, r, id, observer.DoctorSkip)
	}
	if r.Worst() == observer.DoctorFail {
		t.Errorf("an empty machine reports %s", r.Worst())
	}
	if n := r.Tally()[observer.DoctorFail]; n != 0 {
		t.Errorf("%d checks failed on an empty machine", n)
	}
}

// TestTheDeepAndShallowPopulationsConvergingIsTheFinding is the check whose
// alarm reads backwards, so it gets its own test: while the two floors differ,
// a future implementer's recursive glob is VISIBLY wrong; once they converge,
// that mistake becomes invisible, and THAT is what must be reported.
func TestTheDeepAndShallowPopulationsConvergingIsTheFinding(t *testing.T) {
	tr := healthy(t)
	// Remove the unreaped deeper file, leaving the two populations identical.
	deep := filepath.Join(tr.projects, "-home-user-old",
		"aaaaaaaa-0000-0000-0000-000000000000", "subagents", "agent-9.meta.jsonl")
	if err := os.Remove(deep); err != nil {
		t.Fatal(err)
	}

	c := wantStatus(t, run(t, tr, listing()), "transcript-deep-population", observer.DoctorWarn)
	if !strings.Contains(c.Detail, "converged") {
		t.Errorf("the warning does not say what converged: %s", c.Detail)
	}
}

// TestEarlyReapingWarns: transcripts disappearing before the configured window
// makes §3.5 rule 4 call live records unfalsifiable too early.
func TestEarlyReapingWarns(t *testing.T) {
	tr := healthy(t)
	old := filepath.Join(tr.projects, "-home-user-old", "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	early := tDoc.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, early, early); err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(tr.projects, "-home-user-dotfiles", "97030e53-528c-4452-bbae-535400039def.jsonl")
	if err := os.Chtimes(job, early, early); err != nil {
		t.Fatal(err)
	}
	interactive := filepath.Join(tr.projects, "-home-user-dotfiles",
		"5b9e2c84-3a17-4f60-8d25-e1c7b0a94f36.jsonl")
	if err := os.Chtimes(interactive, early, early); err != nil {
		t.Fatal(err)
	}

	c := wantStatus(t, run(t, tr, listing()), "transcript-retention-floor", observer.DoctorWarn)
	if !strings.Contains(c.Detail, "earlier") {
		t.Errorf("the warning does not describe early reaping: %s", c.Detail)
	}
}

// TestAStalePidFileWarns. A pid file whose process is gone means `--stop` looks
// for a server that is not there, and the socket beside it is stale.
func TestAStalePidFileWarns(t *testing.T) {
	tr := healthy(t)
	state := filepath.Join(tr.root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(state, "observer.pid")
	body := `{"pid":424242,"startedAt":"2026-09-18T09:00:00.000Z","socket":"` +
		filepath.Join(state, "observer.sock") + `","tt":"test"}`
	if err := os.WriteFile(pidfile, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := doctorfs.Run(context.Background(), doctorfs.Config{
		Paths:       tr.paths(),
		StateDir:    state,
		RetentionMs: 30 * 24 * 60 * 60 * 1000,
		Now:         tDoc,
		ProcStats: func(int) (string, int64, error) {
			return "", 0, fmt.Errorf("ps -p 424242: exit status 1")
		},
	})
	c := wantStatus(t, r, "observe-server", observer.DoctorWarn)
	if !strings.Contains(c.Detail, "stale") {
		t.Errorf("the warning does not mention the stale socket: %s", c.Detail)
	}
}

// TestARunningServerReportsItsCPUAndRSS is §9.3(d)'s "the doctor surfaces the
// same two figures on demand", which is what makes a future regression visible
// without re-running the whole spot-check.
func TestARunningServerReportsItsCPUAndRSS(t *testing.T) {
	tr := healthy(t)
	state := filepath.Join(tr.root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":1234,"startedAt":"2026-09-18T09:00:00.000Z","socket":"` +
		filepath.Join(state, "observer.sock") + `","tt":"test"}`
	if err := os.WriteFile(filepath.Join(state, "observer.pid"), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := doctorfs.Run(context.Background(), doctorfs.Config{
		Paths:       tr.paths(),
		StateDir:    state,
		RetentionMs: 30 * 24 * 60 * 60 * 1000,
		Now:         tDoc,
		ProcStats:   func(int) (string, int64, error) { return "0:01.20", 11_264, nil },
	})
	c := wantStatus(t, r, "observe-server", observer.DoctorOK)
	joined := strings.Join(c.Evidence, " ")
	for _, want := range []string{"0:01.20", "11264 KB", "age 1h0m0s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the evidence lacks %q: %v", want, c.Evidence)
		}
	}
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// TestRenderTextShowsEveryVerdictAndItsEvidence. The text form is what a user
// reads and what gets pasted into an issue, so every check must appear in it —
// including the ones that passed, which is what makes a pass disbelievable.
func TestRenderTextShowsEveryVerdictAndItsEvidence(t *testing.T) {
	r := run(t, healthy(t), listing())
	var b strings.Builder
	if err := doctorfs.RenderText(&b, r); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	for _, c := range r.Checks {
		if !strings.Contains(out, c.ID) {
			t.Errorf("check %s is missing from the rendered report", c.ID)
		}
		for _, e := range c.Evidence {
			if !strings.Contains(out, e) {
				t.Errorf("evidence %q is missing from the rendered report", e)
			}
		}
	}
	if !strings.Contains(out, "verdict: ") {
		t.Error("the report has no overall verdict line")
	}
}

// TestExitCodeSeparatesDegradedFromBroken. 3 and 4 rather than 1 and 2, because
// `tt sessions` already spends 1 on "nothing could be read" and 2 on a usage
// error, and one number meaning two things is how a script treats a broken
// format as a typo.
func TestExitCodeSeparatesDegradedFromBroken(t *testing.T) {
	cases := []struct {
		name   string
		status observer.DoctorStatus
		want   int
	}{
		{"ok", observer.DoctorOK, 0},
		{"skip", observer.DoctorSkip, 0},
		{"warn", observer.DoctorWarn, 3},
		{"fail", observer.DoctorFail, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := observer.DoctorReport{Checks: []observer.DoctorCheck{
				{ID: "a", Status: observer.DoctorOK},
				{ID: "b", Status: tc.status},
			}}
			if got := doctorfs.ExitCode(r); got != tc.want {
				t.Errorf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTheDoctorTakesExactlyOneListingPerRun is the evidence behind the T5
// allowlist entry. Two checks need a corroboration listing — the shape check
// and the terminal-state gate — and a doctor that spawned `claude agents --json`
// once per check would double the only cost this tool has, on a binary the user
// has already seen wedge.
func TestTheDoctorTakesExactlyOneListingPerRun(t *testing.T) {
	calls := 0
	agents := listing()
	agents.calls = &calls

	r := run(t, healthy(t), agents)
	if calls != 1 {
		t.Fatalf("the doctor took %d listings, want exactly 1", calls)
	}
	// And both dependent checks still got their answer from that one listing.
	wantStatus(t, r, "agents-cli-shape", observer.DoctorOK)
	wantStatus(t, r, "terminal-state-gate", observer.DoctorOK)
}
