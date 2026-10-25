package mcpserve_test

// S2's tests. Three groups, in the order a client meets them: the handshake and
// the catalogue, the four tools, and the transport.
//
// Every test here is hermetic: the source is a fixture snapshot held in memory,
// no file is opened, no process is started and no socket is bound. The
// four-surface identity that needs a real server is in ac11_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/mcpserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// t0 is the one instant the fixtures are built against. Every age in an answer
// derives from a snapshot's own instant, so a fixed t0 makes the whole suite
// deterministic without a clock.
var t0 = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

const testTT = "v0.4.0-test"

// ---------------------------------------------------------------------------
// the fixture source
// ---------------------------------------------------------------------------

// fakeSource answers with ONE snapshot and records what it was asked for. It
// deliberately does not apply the options itself: what is under test is which
// options this surface derives from the arguments, and a fake that filtered
// would hide a surface that passed the wrong ones.
type fakeSource struct {
	mu      sync.Mutex
	snap    *observer.Snapshot
	err     error
	opts    []observer.SnapshotOptions
	enrichN int
	// refresh is the outcome RefreshSources returns, and refreshN counts the
	// calls: "a refresh happened at all" is half of §7.5(d)'s contract.
	refresh  observer.RefreshOutcome
	refreshN int
}

func (f *fakeSource) Snapshot(_ context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts = append(f.opts, opts)
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func (f *fakeSource) Enrich(_ context.Context, snap *observer.Snapshot, _ []string) (*observer.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichN++
	return snap, nil
}

func (f *fakeSource) RefreshSources(_ context.Context, _ []string) observer.RefreshOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshN++
	return f.refresh
}

func (f *fakeSource) lastOptions(t *testing.T) observer.SnapshotOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.opts) == 0 {
		t.Fatal("the source was never asked for a snapshot")
	}
	return f.opts[len(f.opts)-1]
}

func (f *fakeSource) counts() (enrich, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enrichN, f.refreshN
}

// session is one fixture session, complete enough that an envelope built from
// it is well formed rather than half filled.
func session(id string, tune func(*observer.Session)) observer.Session {
	pid := 4242
	since := t0.Add(-3 * time.Hour)
	age := int64(3 * 60 * 60 * 1000)
	s := observer.Session{
		SessionID:           id,
		PID:                 &pid,
		Kind:                observer.KindInteractive,
		Name:                "session " + id,
		NameSource:          observer.SourceRegistry,
		FormerNames:         []string{},
		CWD:                 "/home/user/dotfiles",
		Status:              observer.StatusIdle,
		RawStatus:           "idle",
		StatusSource:        observer.SourceRegistry,
		StatusSince:         &since,
		StatusAgeMs:         &age,
		WaitReason:          observer.WaitReasonNone,
		Children:            []string{},
		Live:                boolPtr(true),
		LiveReason:          observer.LiveReasonProcPane,
		ListedBy:            []string{observer.SourceRegistry},
		Sources:             []string{observer.SourceRegistry},
		ObservedAt:          t0,
		Advisories:          []observer.Advisory{},
		ClaudeVersion:       "2.0.1",
		TranscriptCollected: false,
	}
	if tune != nil {
		tune(&s)
	}
	return s
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// fixture is the two-session snapshot most tests use: one quiet interactive
// session and one blocked background agent, which is the pair every projection
// has to get right.
func fixture() *observer.Snapshot {
	blocked := session("s2", func(s *observer.Session) {
		s.Kind = observer.KindBackground
		s.Name = "Explain project plan"
		s.Status = observer.StatusBlocked
		s.RawStatus = "blocked"
		s.Attention = true
		s.PID = nil
		s.ShortID = strPtr("s2short")
		s.Needs = strPtr("open a terminal and test `ck`")
		s.Live = nil
		s.LiveReason = observer.LiveReasonJobBeyondRetention
		s.BlockedForMs = int64Ptr(2_745_034)
		s.ListedBy = []string{observer.SourceJobs}
		s.Sources = []string{observer.SourceJobs}
	})
	sessions := []observer.Session{session("s1", nil), blocked}

	counts := observer.NewCounts()
	counts.Total = 2
	counts.Live = 1
	counts.Unknown = 1
	counts.Attention = 1
	counts.ByStatus[observer.StatusIdle] = 1
	counts.ByStatus[observer.StatusBlocked] = 1
	counts.ByKind[observer.KindInteractive] = 1
	counts.ByKind[observer.KindBackground] = 1

	return &observer.Snapshot{
		ObservedAt: t0,
		Counts:     counts,
		Sources: map[string]observer.SourceReport{
			observer.SourceRegistry: {
				ID: observer.SourceRegistry, Role: "authoritative", Path: "/fixture/sessions",
				ObservedAt: &t0, OK: true,
				Counters: map[string]int64{"records": 1}, Labels: map[string]string{},
				Advisories: []observer.Advisory{},
			},
			observer.SourceJobs: {
				ID: observer.SourceJobs, Role: "authoritative", Path: "/fixture/jobs",
				ObservedAt: &t0, OK: true,
				Counters: map[string]int64{"records": 1}, Labels: map[string]string{},
				Advisories: []observer.Advisory{},
			},
		},
		Warnings:   []observer.Advisory{},
		Sessions:   sessions,
		NotCapable: observer.DefaultNotCapable(),
	}
}

func int64Ptr(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// the harness
// ---------------------------------------------------------------------------

type harness struct {
	t   *testing.T
	src *fakeSource
	srv *mcpserve.Server
}

func newHarness(t *testing.T, tune func(*mcpserve.Config, *fakeSource)) *harness {
	t.Helper()
	src := &fakeSource{snap: fixture()}
	cfg := mcpserve.Config{
		TT:      testTT,
		Surface: wire.SurfaceWatch{Mode: observer.WatchModeOnDemand, Attached: false, Socket: "/tmp/x/observer.sock"},
		Deps:    mcpserve.Deps{Core: src},
	}
	if tune != nil {
		tune(&cfg, src)
	}
	srv, err := mcpserve.New(cfg)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return &harness{t: t, src: src, srv: srv}
}

// rpc sends one request and returns the decoded response object.
func (h *harness) rpc(method string, params any) map[string]json.RawMessage {
	h.t.Helper()
	line := request(h.t, 1, method, params)
	resp, answer := h.srv.Handle(context.Background(), line)
	if !answer {
		h.t.Fatalf("%s produced no answer", method)
	}
	blob, err := json.Marshal(resp)
	if err != nil {
		h.t.Fatalf("marshalling the response: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(blob, &out); err != nil {
		h.t.Fatalf("decoding the response: %v", err)
	}
	return out
}

// call invokes one tool and returns its result. Errors at the JSON-RPC level
// fail the test: a tool refusal is a RESULT with isError, and a test that
// conflated the two would not notice the surface losing that distinction.
func (h *harness) call(name string, args any) toolResult {
	h.t.Helper()
	resp := h.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if raw, ok := resp["error"]; ok {
		h.t.Fatalf("tools/call %s failed at the protocol level: %s", name, raw)
	}
	var out toolResult
	if err := json.Unmarshal(resp["result"], &out); err != nil {
		h.t.Fatalf("decoding the tool result: %v", err)
	}
	return out
}

// toolResult mirrors wire.CallToolResult for reading. It is a test-local
// DECODER shape with no json tags of its own — §5.1's audit bans tags outside
// wire, in test files as well.
type toolResult struct {
	Content           []map[string]string
	StructuredContent json.RawMessage
	IsError           bool
}

func (r toolResult) text(t *testing.T) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatal("the tool answered with no content")
	}
	return r.Content[0]["text"]
}

// obj decodes one JSON object into its raw members, and text reads one string
// member. Together with toolEntries they are how this file reads an answer.
//
// They deliberately do NOT declare a Go shape for the payload. A test-local
// struct with json tags would be a SECOND definition of the wire shape, which
// §5.1's audit bans in test files for the reason it exists: when the real shape
// changed, the private copy would keep decoding the old one and this test would
// keep passing while no longer testing the contract. Reading the members by
// their literal protocol names is also the stronger assertion for a handshake
// and a catalogue — those key names ARE the MCP contract, and decoding them
// through wire's own types would make the check tautological. Where a payload
// is tt's own and wire already names it (GetSessionResult, ToolResult,
// NameAmbiguous, RPCError), the tests below decode through that type instead.
func obj(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding an object: %v (from %s)", err, raw)
	}
	return out
}

func text(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding a string: %v (from %s)", err, raw)
	}
	return out
}

// toolEntries decodes a tools/list result into one raw object per tool.
func toolEntries(t *testing.T, result json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var list []json.RawMessage
	if err := json.Unmarshal(obj(t, result)["tools"], &list); err != nil {
		t.Fatalf("decoding tools[]: %v", err)
	}
	out := make([]map[string]json.RawMessage, 0, len(list))
	for _, raw := range list {
		out = append(out, obj(t, raw))
	}
	return out
}

func request(t *testing.T, id int, method string, params any) []byte {
	t.Helper()
	obj := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		obj["params"] = params
	}
	line, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	return line
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

// §5.4's version rule, both branches. A supported version is echoed EXACTLY; an
// unsupported one gets this build's preference rather than being echoed —
// echoing an unknown protocol version advertises semantics the implementation
// may not have, which is what revision 0 did and why the rule was rewritten.
func TestInitializeNegotiatesTheProtocolVersionAndNeverEchoesAnUnknownOne(t *testing.T) {
	h := newHarness(t, nil)

	for _, asked := range mcpserve.SupportedProtocols {
		resp := h.rpc("initialize", map[string]any{
			"protocolVersion": asked,
			"clientInfo":      map[string]string{"name": "claude", "version": "2.0.1"},
		})
		var got map[string]any
		if err := json.Unmarshal(resp["result"], &got); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if got["protocolVersion"] != asked {
			t.Fatalf("a supported version was not echoed: asked %q, got %v", asked, got["protocolVersion"])
		}
	}

	for _, asked := range []string{"2024-11-05", "", "tomorrow"} {
		resp := h.rpc("initialize", map[string]any{"protocolVersion": asked})
		var got map[string]any
		if err := json.Unmarshal(resp["result"], &got); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if got["protocolVersion"] != mcpserve.SupportedProtocols[0] {
			t.Fatalf("version %q was answered with %v, want %q",
				asked, got["protocolVersion"], mcpserve.SupportedProtocols[0])
		}
	}
}

// The handshake declares tools and nothing else, and reports tt's own version
// from the one source §5.1 gives it.
func TestInitializeDeclaresToolsOnlyAndTTsOwnVersion(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.rpc("initialize", map[string]any{"protocolVersion": mcpserve.SupportedProtocols[0]})

	result := obj(t, resp["result"])
	caps := obj(t, result["capabilities"])
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("no tools capability: %v", caps)
	}
	for _, unwanted := range []string{"resources", "prompts", "sampling", "logging"} {
		if _, ok := caps[unwanted]; ok {
			t.Errorf("the server declared %q, which §5.4 rejects by name", unwanted)
		}
	}
	if version := text(t, obj(t, result["serverInfo"])["version"]); version != testTT {
		t.Errorf("serverInfo.version = %q, want tt's own version %q", version, testTT)
	}
	if instructions := text(t, result["instructions"]); !strings.Contains(instructions, "read-only") {
		t.Errorf("the instructions do not state the boundary:\n%s", instructions)
	}
}

// ---------------------------------------------------------------------------
// tools/list
// ---------------------------------------------------------------------------

// The catalogue is §5.4's, every entry is annotated read-only, and every schema
// is a well-formed JSON Schema that closes itself to unknown properties.
func TestTheToolCatalogueIsTheSpecifiedOneAndEveryToolIsAnnotatedReadOnly(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.rpc("tools/list", nil)

	tools := toolEntries(t, resp["result"])

	want := []string{"list_sessions", "get_session", "session_counts", "describe_sources"}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d: %v", len(tools), len(want), tools)
	}
	for i, tool := range tools {
		name := text(t, tool["name"])
		if name != want[i] {
			t.Errorf("tool %d is %q, want %q", i, name, want[i])
		}
		if strings.TrimSpace(text(t, tool["description"])) == "" {
			t.Errorf("%s has no description", name)
		}
		var annotations map[string]bool
		if err := json.Unmarshal(tool["annotations"], &annotations); err != nil {
			t.Fatalf("%s: decoding the annotations: %v", name, err)
		}
		if !annotations["readOnlyHint"] || !annotations["idempotentHint"] {
			t.Errorf("%s is not annotated read-only and idempotent: %+v", name, annotations)
		}
		if annotations["destructiveHint"] || annotations["openWorldHint"] {
			t.Errorf("%s is annotated destructive or open-world: %+v", name, annotations)
		}
		schema := obj(t, tool["inputSchema"])
		if string(schema["type"]) != `"object"` {
			t.Errorf("%s's schema is not an object: %s", name, schema["type"])
		}
		if string(schema["additionalProperties"]) != "false" {
			t.Errorf("%s's schema does not close additionalProperties: %s",
				name, schema["additionalProperties"])
		}
	}
}

// The two defaults §5.4 makes visible in the schema, asserted against the
// schema rather than against the code — the point of publishing them is that a
// caller can read them.
func TestListSessionsSchemaPublishesTheTwoDefaultsThatAreNotZero(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.rpc("tools/list", nil)

	var props map[string]map[string]any
	for _, tool := range toolEntries(t, resp["result"]) {
		if text(t, tool["name"]) != "list_sessions" {
			continue
		}
		if err := json.Unmarshal(obj(t, tool["inputSchema"])["properties"], &props); err != nil {
			t.Fatalf("decoding list_sessions' properties: %v", err)
		}
	}
	if props == nil {
		t.Fatal("list_sessions is not in the catalogue")
	}
	if props["includeUnknownLiveness"]["default"] != true {
		t.Errorf("includeUnknownLiveness does not default to true (M7): %+v", props["includeUnknownLiveness"])
	}
	if props["includeTranscript"]["default"] != false {
		t.Errorf("includeTranscript does not default to false (§9.2a): %+v", props["includeTranscript"])
	}
	if props["limit"]["default"] != float64(50) ||
		props["limit"]["minimum"] != float64(1) || props["limit"]["maximum"] != float64(200) {
		t.Errorf("limit's bounds are not 1..200 default 50: %+v", props["limit"])
	}
	if props["includeSpawned"]["default"] != false {
		t.Errorf("includeSpawned does not default to false: a model asking what is running "+
			"would be shown every session's headless children: %+v", props["includeSpawned"])
	}
	for _, key := range []string{"status", "kind", "waitingOnly", "cwdPrefix", "includeDead", "includeSpawned", "refresh"} {
		if _, ok := props[key]; !ok {
			t.Errorf("list_sessions' schema has no %q", key)
		}
	}
}

// The catalogue offers NOTHING that writes, answers, attaches, kills or reads a
// transcript body (§5.4's "deliberately NOT offered"). Asserted over the whole
// catalogue as a property, because the way this list grows a forbidden verb is
// one tool at a time.
func TestNoToolOffersAnythingThatWritesOrReadsATranscriptBody(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.rpc("tools/list", nil)
	tools := toolEntries(t, resp["result"])

	// Checked over the NAMES, which is where a verb arrives: a description may
	// legitimately contain the word "answer" (three of them say the surface
	// never answers a prompt), and a substring match over the whole catalogue
	// would fail on exactly the sentence that states the rule.
	banned := []string{
		"read_transcript", "transcript", "send", "input", "answer", "prompt",
		"rename", "attach", "kill", "write", "create", "update", "delete",
		"resume", "set_", "stop",
	}
	for _, tool := range tools {
		name := text(t, tool["name"])
		for _, verb := range banned {
			if strings.Contains(name, verb) {
				t.Errorf("the catalogue offers %q, which contains the forbidden verb %q", name, verb)
			}
		}
	}
	// And the catalogue is CLOSED: four tools on a build with no diagnostic
	// function wired, nothing else, so a fifth added without a decision fails
	// here rather than shipping.
	if len(tools) != 4 {
		t.Fatalf("the catalogue has %d tools; §5.4 offers four, plus doctor only when one is wired", len(tools))
	}
}

// TestTheDoctorToolAppearsOnlyWhenADiagnosticIsWired is P10's half of the
// catalogue rule. `tt mcp` always wires one; a server built without one must
// not advertise a tool it cannot run, because a model told a tool exists will
// call it and report the refusal as a finding about the machine.
func TestTheDoctorToolAppearsOnlyWhenADiagnosticIsWired(t *testing.T) {
	bare := newHarness(t, nil)
	for _, tool := range toolList(t, bare) {
		if text(t, tool["name"]) == "doctor" {
			t.Fatal("a build with no diagnostic function advertises the doctor")
		}
	}
	if res := bare.rpc("tools/call", map[string]any{"name": "doctor"}); res["error"] == nil {
		t.Error("calling the unwired doctor was not refused")
	}

	wired := newHarness(t, func(cfg *mcpserve.Config, _ *fakeSource) {
		cfg.Doctor = func(context.Context) observer.DoctorReport {
			return observer.DoctorReport{
				TakenAt:    time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
				DurationMs: 12,
				Paths:      observer.Paths{ClaudeRoot: "/fixtures"},
				Checks: []observer.DoctorCheck{
					{ID: "registry-keys", Title: "keys", Cite: "§0.1", Status: observer.DoctorOK,
						Detail: "all 19 present", Evidence: []string{"34 records"}},
					{ID: "agents-cli-shape", Title: "listing", Cite: "D2", Status: observer.DoctorWarn,
						Detail: "the listing could not be taken: timed out after 5s"},
				},
			}
		}
	})

	var found bool
	for _, tool := range toolList(t, wired) {
		if text(t, tool["name"]) == "doctor" {
			found = true
			if !strings.Contains(text(t, tool["description"]), "read-only") &&
				!strings.Contains(text(t, tool["description"]), "Read-only") {
				t.Error("the doctor's description does not say it is read-only")
			}
		}
	}
	if !found {
		t.Fatal("a wired build does not advertise the doctor")
	}

	res := wired.call("doctor", map[string]any{})
	if res.IsError {
		t.Fatalf("the doctor refused: %s", res.text(t))
	}
	summary := res.text(t)
	// The text leads with the verdict and names the check that is NOT ok; a
	// model handed twenty-two passing lines summarises them, a model handed the
	// one failing line acts on it.
	if !strings.HasPrefix(summary, "warn:") {
		t.Errorf("the summary does not lead with the verdict: %q", summary)
	}
	if !strings.Contains(summary, "agents-cli-shape") {
		t.Errorf("the summary does not name the degraded check: %q", summary)
	}
	if strings.Contains(summary, "registry-keys") {
		t.Errorf("the summary repeats a passing check: %q", summary)
	}

	var report wire.DoctorReport
	if err := json.Unmarshal(res.StructuredContent, &report); err != nil {
		t.Fatalf("decoding structuredContent: %v", err)
	}
	if report.Verdict != "warn" || report.Tally.OK != 1 || report.Tally.Warn != 1 {
		t.Errorf("structured tally is %+v with verdict %q", report.Tally, report.Verdict)
	}
	if report.Checks[0].Evidence == nil {
		t.Error("evidence decoded as null; the encoder must emit an empty array")
	}
	if report.TT != testTT {
		t.Errorf("the report carries tt %q, want %q", report.TT, testTT)
	}
}

// toolList returns the tools/list array, read by its literal protocol key —
// never through a test-local struct, because §5.1's audit bans json tags
// outside wire in test files too, and for the reason it exists: a private copy
// of a shape keeps decoding the old one after the real one changes.
func toolList(t *testing.T, h *harness) []map[string]json.RawMessage {
	t.Helper()
	result := obj(t, h.rpc("tools/list", map[string]any{})["result"])
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		t.Fatalf("decoding tools/list: %v", err)
	}
	return tools
}

// ---------------------------------------------------------------------------
// list_sessions
// ---------------------------------------------------------------------------

// The structured payload IS the §5.2 envelope — the same value `tt sessions
// --json` prints — and the text beside it is a summary rather than a second
// copy of the record set.
func TestListSessionsAnswersWithTheEnvelopeAndACompactSummary(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("list_sessions", map[string]any{})
	if res.IsError {
		t.Fatalf("list_sessions refused: %s", res.text(t))
	}

	var env wire.Snapshot
	if err := json.Unmarshal(res.StructuredContent, &env); err != nil {
		t.Fatalf("the structured content is not the envelope: %v", err)
	}
	if env.Schema != wire.Schema || env.TT != testTT {
		t.Fatalf("the envelope does not carry the schema and version: %+v", env.Schema)
	}
	if len(env.Sessions) != 2 || env.Counts.Total != 2 {
		t.Fatalf("the envelope does not carry the fixture: %d sessions, counts %+v",
			len(env.Sessions), env.Counts)
	}
	if env.ObservedAt != observer.FormatTime(t0) {
		t.Fatalf("observedAt is %q, not the snapshot's own instant %q — a second clock read",
			env.ObservedAt, observer.FormatTime(t0))
	}

	text := res.text(t)
	if !strings.Contains(text, "2 sessions") || !strings.Contains(text, "attention 1") {
		t.Fatalf("the summary does not carry the counts:\n%s", text)
	}
	if strings.Contains(text, "/home/user/dotfiles") && strings.Count(text, "\n") > 4 {
		t.Fatalf("the summary looks like the full record set:\n%s", text)
	}
	// The blocked agent's `needs` IS the answer to "is anyone blocked, and on
	// what?", so the summary carries it — as a PLACEHOLDER by default, because
	// this surface's audience is other agents rather than the user. Presence
	// and length survive; the sentence does not.
	if !strings.Contains(text, wire.JobTextWithheldPrefix) {
		t.Fatalf("the summary does not say that the blocked agent needs something:\n%s", text)
	}
	if strings.Contains(text, "test `ck`") {
		t.Fatalf("the summary shipped the user's own sentence to an MCP client:\n%s", text)
	}
}

// The reversal, in one step: --publish-job-text puts the sentence back on this
// surface. It is a judgement call, so it has to be visible and undoable, and
// this test is what stops the switch silently rotting.
func TestPublishJobTextPutsTheSentenceBack(t *testing.T) {
	h := newHarness(t, func(cfg *mcpserve.Config, _ *fakeSource) { cfg.PublishJobText = true })
	res := h.call("list_sessions", map[string]any{})
	if res.IsError {
		t.Fatalf("list_sessions refused: %s", res.text(t))
	}
	if text := res.text(t); !strings.Contains(text, "test `ck`") {
		t.Fatalf("--publish-job-text did not publish the job text:\n%s", text)
	}

	var env wire.Snapshot
	if err := json.Unmarshal(res.StructuredContent, &env); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if env.Sessions[1].Needs == nil || *env.Sessions[1].Needs != "open a terminal and test `ck`" {
		t.Fatalf("needs on the wire is %v, want the record's own value", env.Sessions[1].Needs)
	}
}

// jobsfs.NeverRead excludes `intent`, `output` and `suggestedReply` because
// they carry the user's own prompt and the agent's replies. `needs` and
// `detail` come out of the SAME record — measured live as
// "open new terminal and test `ck` command; report if it works or breaks",
// a verbatim restatement of an instruction the user typed — and every tool on
// this surface published them to every Claude session on the machine.
//
// The line is now drawn by audience: local surfaces show the user their own
// machine, this one hands data to other agents. This test walks ALL FOUR tools,
// because the defect was one projection at a time.
func TestNoToolPublishesTheFreeTextJobFields(t *testing.T) {
	const secret = "open a terminal and test `ck`"

	h := newHarness(t, func(_ *mcpserve.Config, src *fakeSource) {
		snap := fixture()
		snap.Sessions[1].Detail = strPtr("blocked on the toolbar mount location")
		src.snap = snap
	})

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"list_sessions", map[string]any{}},
		{"get_session", map[string]any{"sessionId": "s2"}},
		{"session_counts", map[string]any{}},
		{"describe_sources", map[string]any{}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res := h.call(tc.tool, tc.args)
			if res.IsError {
				t.Fatalf("%s refused: %s", tc.tool, res.text(t))
			}
			// BOTH halves of the payload: the structured answer and the human
			// text a model reads. The text is the copy that cannot be filtered.
			blob := string(res.StructuredContent) + "\n" + res.text(t)
			if strings.Contains(blob, secret) {
				t.Errorf("%s published the free-text needs:\n%s", tc.tool, blob)
			}
			if strings.Contains(blob, "toolbar mount location") {
				t.Errorf("%s published the free-text detail:\n%s", tc.tool, blob)
			}
		})
	}

	// And the attention projection — the one the spec's own comment justified
	// carrying `needs` because "for a blocked background agent that string IS
	// the answer" — still says a need EXISTS and how long it is.
	res := h.call("session_counts", map[string]any{})
	var out wire.ToolResult
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decoding session_counts: %v", err)
	}
	if len(out.Attention) != 1 {
		t.Fatalf("the attention list holds %d sessions, want the blocked one", len(out.Attention))
	}
	needs := out.Attention[0].Needs
	if needs == nil || !strings.HasPrefix(*needs, wire.JobTextWithheldPrefix) {
		t.Fatalf("attention[0].needs is %v, want a presence-and-length placeholder: an agent "+
			"must still be able to tell that this session needs something", needs)
	}
	if !strings.Contains(*needs, strconv.Itoa(len(secret))) {
		t.Errorf("the placeholder %q does not carry the length", *needs)
	}
}

// §9.2(c)'s identity, asserted on THIS surface: statusAgeMs is what the
// snapshot's own instant minus statusSince gives, because the encoder was
// handed that instant and never read a clock.
func TestTheAgeIdentityHoldsOnTheMCPSurface(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("list_sessions", map[string]any{})

	var env wire.Snapshot
	if err := json.Unmarshal(res.StructuredContent, &env); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	observed, err := observer.ParseTime(env.ObservedAt)
	if err != nil {
		t.Fatalf("parsing observedAt: %v", err)
	}
	for _, s := range env.Sessions {
		if s.StatusSince == nil || s.StatusAgeMs == nil {
			continue
		}
		since, err := observer.ParseTime(*s.StatusSince)
		if err != nil {
			t.Fatalf("parsing statusSince: %v", err)
		}
		if want := observed.Sub(since).Milliseconds(); want != *s.StatusAgeMs {
			t.Fatalf("%s: statusAgeMs = %d, observedAt - statusSince = %d",
				s.SessionID, *s.StatusAgeMs, want)
		}
	}
}

// The arguments become SnapshotOptions, including the two defaults that are not
// zero values. This is where M7's rule is either kept or silently broken.
func TestTheArgumentsBecomeSnapshotOptionsWithTheDocumentedDefaults(t *testing.T) {
	h := newHarness(t, nil)

	h.call("list_sessions", map[string]any{})
	opts := h.src.lastOptions(t)
	if !opts.IncludeUnknownLiveness {
		t.Error("includeUnknownLiveness did not default to true; every live:null session would vanish (M7)")
	}
	if opts.IncludeTranscripts {
		t.Error("includeTranscript did not default to false (§9.2a)")
	}
	if opts.IncludeDead {
		t.Error("includeDead did not default to false")
	}

	h.call("list_sessions", map[string]any{
		"includeUnknownLiveness": false,
		"includeDead":            true,
		"waitingOnly":            true,
		"cwdPrefix":              "/home/user",
		"kind":                   "background",
		"status":                 []string{"blocked", "waiting"},
	})
	opts = h.src.lastOptions(t)
	if opts.IncludeUnknownLiveness || !opts.IncludeDead || !opts.Filter.WaitingOnly {
		t.Errorf("the booleans did not reach the core: %+v", opts)
	}
	if opts.Filter.CWDPrefix != "/home/user" {
		t.Errorf("cwdPrefix did not reach the core: %+v", opts.Filter)
	}
	if len(opts.Filter.Kinds) != 1 || opts.Filter.Kinds[0] != observer.KindBackground {
		t.Errorf("kind did not reach the core: %+v", opts.Filter.Kinds)
	}
	if len(opts.Filter.Statuses) != 2 {
		t.Errorf("status did not reach the core: %+v", opts.Filter.Statuses)
	}
}

// The enrichment pass runs only when it is asked for, because it is the only
// expensive thing tt does.
func TestEnrichmentRunsOnlyWhenTheCallerAsksForIt(t *testing.T) {
	h := newHarness(t, func(cfg *mcpserve.Config, src *fakeSource) {
		cfg.Deps.Enrich = src
	})

	h.call("list_sessions", map[string]any{})
	if n, _ := h.src.counts(); n != 0 {
		t.Fatalf("the default call enriched %d times, want 0", n)
	}

	h.call("list_sessions", map[string]any{"includeTranscript": true})
	if n, _ := h.src.counts(); n != 1 {
		t.Fatalf("includeTranscript:true enriched %d times, want 1", n)
	}
	if !h.src.lastOptions(t).IncludeTranscripts {
		t.Error("includeTranscript did not reach the core's options")
	}

	// session_counts never enriches: none of its ten fields comes from a
	// transcript.
	h.call("session_counts", map[string]any{})
	if n, _ := h.src.counts(); n != 1 {
		t.Fatalf("session_counts enriched; the projection needs no transcript (%d)", n)
	}
}

// limit truncates the array and SAYS SO, and the counts keep describing the
// whole filtered view — an envelope whose counts silently matched a truncated
// array would be a different lie.
func TestLimitTruncatesTheArrayAndSaysSoWhileTheCountsStayWhole(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("list_sessions", map[string]any{"limit": 1})

	var env wire.Snapshot
	if err := json.Unmarshal(res.StructuredContent, &env); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(env.Sessions) != 1 {
		t.Fatalf("limit 1 returned %d sessions", len(env.Sessions))
	}
	if env.Counts.Total != 2 {
		t.Fatalf("counts.total = %d; the counts must describe the whole filtered view", env.Counts.Total)
	}
	if !strings.Contains(res.text(t), "truncated") {
		t.Fatalf("the summary does not say the list was truncated:\n%s", res.text(t))
	}
}

// Arguments that the schema forbids are refused as a TOOL RESULT the model can
// correct, not as a JSON-RPC error the transport swallows.
func TestBadArgumentsAreARefusalTheCallerCanCorrect(t *testing.T) {
	h := newHarness(t, nil)
	for _, args := range []map[string]any{
		{"waiting_only": true},
		{"kind": "bogus"},
		{"status": []string{"nonsense"}},
		{"limit": 0},
		{"limit": 500},
	} {
		res := h.call("list_sessions", args)
		if !res.IsError {
			t.Errorf("%v was accepted", args)
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(res.StructuredContent, &payload); err != nil {
			t.Fatalf("decoding the refusal: %v", err)
		}
		if payload["error"] != "bad_request" {
			t.Errorf("%v was refused with %q, want bad_request", args, payload["error"])
		}
	}
}

// ---------------------------------------------------------------------------
// refresh — §7.5(d)'s three shapes on a pull surface
// ---------------------------------------------------------------------------

// A rate-limited refresh DEGRADES the answer: the sessions are still there, and
// the envelope carries the request-scoped refresh-rejected advisory that says
// corroboration was not re-read. §5.4 and §7.4 both turn on this: a refresh
// must never fail a caller's request.
func TestARateLimitedRefreshDegradesTheAnswerRatherThanFailingIt(t *testing.T) {
	h := newHarness(t, func(cfg *mcpserve.Config, src *fakeSource) {
		src.refresh = observer.RefreshOutcome{
			Rejected: map[string]string{observer.SourceAgentsCLI: observer.RejectRateLimited},
			Advisories: []observer.Advisory{observer.NewAdvisory(observer.AdvRefreshRejected, map[string]any{
				"source": observer.SourceAgentsCLI,
				"reason": observer.RejectRateLimited,
			})},
		}
		cfg.Deps.Refresh = src
	})

	res := h.call("list_sessions", map[string]any{"refresh": true})
	if res.IsError {
		t.Fatalf("a rate-limited refresh failed the call: %s", res.text(t))
	}
	if _, n := h.src.counts(); n != 1 {
		t.Fatalf("the refresh was not attempted (%d calls)", n)
	}

	var env wire.Snapshot
	if err := json.Unmarshal(res.StructuredContent, &env); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(env.Sessions) != 2 {
		t.Fatalf("the degraded answer lost the sessions: %d", len(env.Sessions))
	}
	found := false
	for _, w := range env.Warnings {
		if w.Code == observer.AdvRefreshRejected && w.Fields["reason"] == observer.RejectRateLimited {
			found = true
		}
	}
	if !found {
		t.Fatalf("the degraded answer does not say corroboration was refused: %+v", env.Warnings)
	}

	// And the rejection is REQUEST-SCOPED: the next call, without --refresh,
	// carries no such warning. A caller's refusal must not leak into an
	// unrelated answer.
	plain := h.call("list_sessions", map[string]any{})
	var env2 wire.Snapshot
	if err := json.Unmarshal(plain.StructuredContent, &env2); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, w := range env2.Warnings {
		if w.Code == observer.AdvRefreshRejected {
			t.Fatalf("a request-scoped advisory leaked into a later answer: %+v", env2.Warnings)
		}
	}
}

// A source that cannot refresh at all says so in the same place, rather than
// pretending corroboration was re-read.
func TestARefreshAgainstASourceThatCannotSaysUnsupported(t *testing.T) {
	h := newHarness(t, nil) // no Refresh dependency at all
	res := h.call("session_counts", map[string]any{"refresh": true})
	if res.IsError {
		t.Fatalf("the call failed: %s", res.text(t))
	}
	var out wire.ToolResult
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	found := false
	for _, w := range out.Warnings {
		if w.Code == observer.AdvRefreshRejected && w.Fields["reason"] == observer.RejectUnsupported {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unrefreshable source did not say so: %+v", out.Warnings)
	}
}

// ---------------------------------------------------------------------------
// get_session
// ---------------------------------------------------------------------------

func TestGetSessionLooksUpByEachSelector(t *testing.T) {
	h := newHarness(t, nil)

	for _, args := range []map[string]any{
		{"sessionId": "s1"},
		{"pid": 4242},
		{"name": "session s1"},
	} {
		res := h.call("get_session", args)
		if res.IsError {
			t.Fatalf("%v was refused: %s", args, res.text(t))
		}
		var out wire.GetSessionResult
		if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if out.Session == nil || out.Session.SessionID != "s1" {
			t.Fatalf("%v found %+v", args, out.Session)
		}
		if len(out.Sources) == 0 {
			t.Fatalf("%v answered without the sources block", args)
		}
	}
}

// A lookup that matches nothing is an ANSWER — session: null — not an error.
// "No session by that name" and "we could not read anything" must not look
// alike.
func TestGetSessionAnswersNullForNoMatchRatherThanFailing(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("get_session", map[string]any{"sessionId": "nobody"})
	if res.IsError {
		t.Fatalf("a miss was reported as an error: %s", res.text(t))
	}
	if !strings.Contains(string(res.StructuredContent), `"session":null`) {
		t.Fatalf("a miss did not answer session:null: %s", res.StructuredContent)
	}
}

// Zero and two selectors are both bad_request. Either answer would be a guess
// at which session was meant.
func TestGetSessionRequiresExactlyOneSelector(t *testing.T) {
	h := newHarness(t, nil)
	for _, args := range []map[string]any{
		{},
		{"sessionId": "s1", "name": "session s1"},
		{"sessionId": "s1", "pid": 4242},
	} {
		res := h.call("get_session", args)
		if !res.IsError {
			t.Errorf("%v was accepted", args)
		}
		if !strings.Contains(res.text(t), "bad_request") {
			t.Errorf("%v was refused with %q", args, res.text(t))
		}
	}
}

// A name matching more than one session is refused WITH the candidates, never
// resolved by picking one — names are reused, which is why formerNames exists.
func TestAnAmbiguousNameIsRefusedWithItsCandidates(t *testing.T) {
	h := newHarness(t, func(_ *mcpserve.Config, src *fakeSource) {
		snap := fixture()
		snap.Sessions[1].Name = snap.Sessions[0].Name
		src.snap = snap
	})

	res := h.call("get_session", map[string]any{"name": "session s1"})
	if !res.IsError {
		t.Fatal("an ambiguous name was resolved rather than refused")
	}
	var out wire.NameAmbiguous
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Error != "name-ambiguous" || len(out.Candidates) != 2 {
		t.Fatalf("the refusal is not name-ambiguous with both candidates: %+v", out)
	}
}

// formerNames is NEVER matched. It exists precisely because names are reused,
// so matching it would return a session that is no longer the one asked for.
func TestAFormerNameIsNeverMatched(t *testing.T) {
	h := newHarness(t, func(_ *mcpserve.Config, src *fakeSource) {
		snap := fixture()
		snap.Sessions[0].FormerNames = []string{"the old name"}
		src.snap = snap
	})
	res := h.call("get_session", map[string]any{"name": "the old name"})
	if res.IsError {
		t.Fatalf("the lookup errored: %s", res.text(t))
	}
	if !strings.Contains(string(res.StructuredContent), `"session":null`) {
		t.Fatalf("a former name was matched: %s", res.StructuredContent)
	}
}

// A lookup is not a listing: get_session finds a session with positive evidence
// of death, because "it is gone" and "it never existed" are different answers.
func TestGetSessionLooksThroughTheDefaultLivenessFilter(t *testing.T) {
	h := newHarness(t, nil)
	h.call("get_session", map[string]any{"sessionId": "s1"})
	opts := h.src.lastOptions(t)
	if !opts.IncludeDead || !opts.IncludeUnknownLiveness {
		t.Fatalf("get_session narrowed by liveness: %+v", opts)
	}
}

// ---------------------------------------------------------------------------
// session_counts and describe_sources
// ---------------------------------------------------------------------------

func TestSessionCountsAnswersTheProjectionAndNotTheRecordSet(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("session_counts", nil)

	var out wire.ToolResult
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Counts.Total != 2 || out.Counts.Attention != 1 {
		t.Fatalf("the counts are not the envelope's: %+v", out.Counts)
	}
	if len(out.Attention) != 1 || out.Attention[0].SessionID != "s2" {
		t.Fatalf("the attention list is wrong: %+v", out.Attention)
	}
	if out.Attention[0].Needs == nil {
		t.Fatal("the blocked agent's needs is missing; for a blocked agent that string IS the answer")
	}
	if out.Attention[0].Live != nil {
		t.Fatal("live is not null for a session we cannot vouch for (H4)")
	}
	if strings.Contains(string(res.StructuredContent), `"sessionId":"s1"`) {
		t.Fatalf("session_counts returned the full record set: %s", res.StructuredContent)
	}
}

func TestDescribeSourcesReportsTheFiveKeysIncludingHowThisProcessGetsItsData(t *testing.T) {
	pid := wire.PidFile{PID: 999, StartedAt: observer.FormatTime(t0), Socket: "/tmp/x/observer.sock", TT: testTT}
	h := newHarness(t, func(cfg *mcpserve.Config, _ *fakeSource) {
		cfg.Surface = wire.SurfaceWatch{
			Mode: observer.WatchModeViaSocket, Attached: true, Socket: "/tmp/x/observer.sock",
		}
		cfg.Server = func() *wire.PidFile { return &pid }
	})

	res := h.call("describe_sources", map[string]any{})
	var out wire.DescribeSourcesResult
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Sources) != 2 {
		t.Fatalf("the sources block did not travel: %+v", out.Sources)
	}
	if !out.Watch.Attached || out.Watch.Mode != observer.WatchModeViaSocket {
		t.Fatalf("the watch block does not say how this process gets its data: %+v", out.Watch)
	}
	if out.Server == nil || out.Server.PID != 999 {
		t.Fatalf("the running server was not reported: %+v", out.Server)
	}
	if len(out.NotCapable) != 1 || out.NotCapable[0] != "time-transitions" {
		t.Fatalf("notCapable did not travel: %+v", out.NotCapable)
	}
	if !strings.Contains(res.text(t), "via-socket") {
		t.Fatalf("the summary does not say where the data comes from:\n%s", res.text(t))
	}
}

// With no server running the same block says so, and `server` is null rather
// than a stale record.
func TestDescribeSourcesReportsNoServerAsNull(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("describe_sources", nil)
	var out wire.DescribeSourcesResult
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Server != nil {
		t.Fatalf("a server was reported with none running: %+v", out.Server)
	}
	if out.Watch.Attached {
		t.Fatal("the watch block claims an attachment that does not exist")
	}
}

// ---------------------------------------------------------------------------
// privacy — §5.4's exposure rules, asserted over the bytes
// ---------------------------------------------------------------------------

// No answer this surface produces may carry a prompt, a message, a tool output
// or a job's intent. The assertion is over the BYTES of every tool's answer,
// because the way this rule breaks is a field added upstream that a surface
// forwards without noticing.
func TestNoToolAnswerCarriesAPrivacyDenylistedField(t *testing.T) {
	h := newHarness(t, func(cfg *mcpserve.Config, src *fakeSource) {
		cfg.Deps.Enrich = src
		cfg.Deps.Refresh = src
	})

	denylist := []string{
		"intent", "output", "text", "content", "message", "messages",
		"lastPrompt", "prompt", "summary", "toolUse", "toolResult", "timeline",
	}
	calls := []struct {
		name string
		args any
	}{
		{"list_sessions", map[string]any{"includeTranscript": true}},
		{"get_session", map[string]any{"sessionId": "s2"}},
		{"session_counts", map[string]any{}},
		{"describe_sources", map[string]any{}},
	}
	for _, c := range calls {
		res := h.call(c.name, c.args)
		blob := string(res.StructuredContent)
		for _, banned := range denylist {
			if strings.Contains(blob, `"`+banned+`":`) {
				t.Errorf("%s's structured answer carries %q:\n%s", c.name, banned, blob)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the transport
// ---------------------------------------------------------------------------

// A whole session over a pipe: one line in, one line out, in order, and a
// notification produces nothing at all.
func TestServeAnswersLineByLineAndIsSilentForNotifications(t *testing.T) {
	h := newHarness(t, nil)

	var in bytes.Buffer
	in.Write(request(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}))
	in.WriteByte('\n')
	in.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	in.Write(request(t, 2, "tools/list", nil))
	in.WriteByte('\n')
	in.Write(request(t, 3, "tools/call", map[string]any{"name": "session_counts", "arguments": map[string]any{}}))
	in.WriteByte('\n')

	var out bytes.Buffer
	if err := h.srv.Serve(context.Background(), &in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d response lines, want 3 (the notification must be silent):\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("line %d is not one JSON object: %q", i, line)
		}
		if resp["jsonrpc"] != "2.0" {
			t.Fatalf("line %d is not JSON-RPC 2.0: %q", i, line)
		}
		if want := float64(i + 1); resp["id"] != want {
			t.Fatalf("line %d answers id %v, want %v (answers must be in order)", i, resp["id"], want)
		}
	}
}

// The protocol-level failures, each with the code JSON-RPC specifies.
func TestTheProtocolErrorsAreTheSpecifiedCodes(t *testing.T) {
	h := newHarness(t, nil)

	cases := []struct {
		name string
		line string
		code int
	}{
		{"not json", `{"jsonrpc":`, -32700},
		{"not a request", `{"id":1,"method":"tools/list"}`, -32600},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, -32601},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"doctor"}}`, -32602},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, answer := h.srv.Handle(context.Background(), []byte(c.line))
			if !answer {
				t.Fatal("no answer")
			}
			blob, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			raw, ok := obj(t, blob)["error"]
			if !ok {
				t.Fatalf("got %s, want an error member carrying code %d", blob, c.code)
			}
			var rpcErr wire.RPCError
			if err := json.Unmarshal(raw, &rpcErr); err != nil {
				t.Fatalf("decoding the error member: %v", err)
			}
			if rpcErr.Code != c.code || strings.TrimSpace(rpcErr.Message) == "" {
				t.Fatalf("got %s, want code %d with a message", blob, c.code)
			}
		})
	}
}

// A source that cannot read at all is a tool refusal rather than a protocol
// error: the caller asked a legitimate question and the machine could not
// answer it, which is a fact about the machine.
func TestASourceThatCannotReadIsARefusalNotAProtocolError(t *testing.T) {
	h := newHarness(t, func(_ *mcpserve.Config, src *fakeSource) {
		src.err = fmt.Errorf("the registry is unreadable")
	})
	res := h.call("list_sessions", map[string]any{})
	if !res.IsError {
		t.Fatal("an unreadable source answered successfully")
	}
	if !strings.Contains(res.text(t), "unreadable") {
		t.Fatalf("the refusal does not say why: %s", res.text(t))
	}
}

// New refuses a wiring with no source rather than accepting one that would
// panic on the first call.
func TestNewRefusesAServerWithNoSource(t *testing.T) {
	if _, err := mcpserve.New(mcpserve.Config{TT: testTT}); err == nil {
		t.Fatal("a server with no session source was accepted")
	}
}
