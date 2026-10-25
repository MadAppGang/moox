package transcriptfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// P6's suite: the glob locator and the 200 KB enrichment pass.
//
// Every fixture here is under testdata/projects/ and is documented per entry in
// that directory's README as REAL-shape or SYNTHETIC. The session ids are
// mnemonic — proj-a's transcript is aaaaaaaa-…, proj-e's is eeeeeeee-… — so a
// failure message names the fixture without a lookup.

const (
	sessA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" // colour in window + subagents
	sessB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" // colour BEYOND a 2 KB tail
	sessC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" // last-wins + the decoys
	sessD = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" // a RECOVERED is_error
	sessE = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee" // isApiErrorMessage: true
	// sessDecoy lives in the colliding slug directory and belongs to an
	// unrelated session. It is what a cwd->slug derivation would have read for
	// proj-a, and the test asserts it is never opened.
	sessDecoy = "c01110ed-0000-4000-8000-000000000001"
	// sessDup exists in BOTH -home-user-proj-a and -home-user-proj-a-collide:
	// the glob matches twice.
	sessDup = "d0b1e000-0000-4000-8000-000000000002"
)

// relA etc. are the fixture paths, spelled once.
const (
	relA      = "-home-user-proj-a/" + sessA + ".jsonl"
	relB      = "-home-user-proj-b/" + sessB + ".jsonl"
	relC      = "-home-user-proj-c/" + sessC + ".jsonl"
	relD      = "-home-user-proj-d/" + sessD + ".jsonl"
	relE      = "-home-user-proj-e/" + sessE + ".jsonl"
	relDecoy  = "-home-user-proj-a-collide/" + sessDecoy + ".jsonl"
	relDupA   = "-home-user-proj-a/" + sessDup + ".jsonl"
	relDupCol = "-home-user-proj-a-collide/" + sessDup + ".jsonl"
)

// locate is find-then-read, which is how the core uses the pair.
func locate(t *testing.T, src *Source, sessionID string, tailBytes int) observer.TranscriptFacts {
	t.Helper()
	loc, err := src.FindByGlob(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("FindByGlob(%s): %v", sessionID, err)
	}
	if !loc.Found {
		t.Fatalf("FindByGlob(%s): not found (ambiguous=%v matches=%v)", sessionID, loc.Ambiguous, loc.Matches)
	}
	facts, err := src.ReadFacts(context.Background(), loc, tailBytes)
	if err != nil {
		t.Fatalf("ReadFacts(%s): %v", loc.Path, err)
	}
	return facts
}

// ---------------------------------------------------------------------------
// AC6 — the colour is agentColor, last-wins, and every `color` key is ignored
// ---------------------------------------------------------------------------

func TestAC6_TheColourIsTheLastAgentColorRecordAndNothingElse(t *testing.T) {
	// proj-c carries FOUR things that a wrong implementation reads instead,
	// and the fixture is built so each wrong answer is a DIFFERENT colour —
	// so the failure message says which mistake was made:
	//
	//	"blue"    an earlier agent-color record  -> first-wins instead of last
	//	"red"     an agent-name record's `color` -> the subagent key (fact 5)
	//	"pink"    an assistant record's `color`  -> ditto, on another type
	//	"orange"  the LAST line, which carries `agentColor` with type
	//	          "assistant" and slug "agent-color" -> a substring grep, which
	//	          measured 72 % overcounting on pid 98693
	//
	// The right answer is "green": the last record whose TYPE is agent-color.
	facts := locate(t, newSource(t, fixtures(t)), sessC, 0)

	if !facts.ColorFound {
		t.Fatal("colorFound is false; proj-c carries two agent-color records")
	}
	switch facts.AgentColor {
	case "green":
	case "blue":
		t.Error(`agentColor = "blue": the FIRST agent-color record won. §8.3 item 3 is last-wins`)
	case "red", "pink":
		t.Errorf(`agentColor = %q: a bare "color" key was read. Every one of those belongs to a `+
			`SUBAGENT (measured fact 5)`, facts.AgentColor)
	case "orange":
		t.Error(`agentColor = "orange": a record that merely CONTAINS agentColor was read as a ` +
			`colour record. The type gate is the whole of AC6`)
	default:
		t.Errorf("agentColor = %q, want %q", facts.AgentColor, "green")
	}
}

func TestAC6_ASubagentColorKeyNeverCrossesTheBoundaryAtAll(t *testing.T) {
	// Not merely "is not chosen" — is not CARRIED. A field that held it and
	// went unused today is a field a later surface can render.
	facts := locate(t, newSource(t, fixtures(t)), sessC, 0)
	rendered := fmt.Sprintf("%+v", facts)
	for _, wrong := range []string{"red", "pink", "orange", "blue"} {
		if strings.Contains(rendered, wrong) {
			t.Errorf("%q appears in the facts: %s", wrong, rendered)
		}
	}
}

func TestTheColourKeyIsAgentColorAndTheTypeGateIsExplicit(t *testing.T) {
	// The constants are the measurement. Three independent greps for "color"
	// missed that the key is agentColor, so the names are pinned rather than
	// left inline where the next grep can miss them again.
	if agentColorKey != "agentColor" {
		t.Errorf("agentColorKey = %q, want agentColor — NOT color (fact 5)", agentColorKey)
	}
	if agentColorType != "agent-color" {
		t.Errorf("agentColorType = %q, want agent-color", agentColorType)
	}
	if apiErrorKey != "isApiErrorMessage" {
		t.Errorf("apiErrorKey = %q, want isApiErrorMessage — NOT is_error (fact 11)", apiErrorKey)
	}
}

// ---------------------------------------------------------------------------
// AC5 — is_error is not the error signal
// ---------------------------------------------------------------------------

func TestAC5_ARecoveredIsErrorIsNotCountedAsAnAPIError(t *testing.T) {
	// proj-d carries TWO `is_error` records and one isApiErrorMessage:false.
	// The answer is 0. Machine-wide `is_error` fires 8 243 times across 1 803
	// transcripts, overwhelmingly on tool failures the model then recovered
	// from; counting it would be wrong by ~7.5x and would report healthy
	// sessions as broken.
	facts := locate(t, newSource(t, fixtures(t)), sessD, 0)
	if facts.APIErrors != 0 {
		t.Errorf("apiErrors = %d, want 0: proj-d's only error markers are RECOVERED is_error "+
			"records and an isApiErrorMessage:false", facts.APIErrors)
	}
	// The fixture hoists `is_error` to the TOP LEVEL of the record (the real
	// one nests it at message.content[].is_error, and those keys are redacted
	// out of every fixture). That makes the assertion STRICTER than reality: a
	// substring scan for is_error fails here, and so does a top-level key
	// check.
	if facts.ColorFound && facts.AgentColor != "blue" {
		t.Errorf("proj-d's own colour changed: %q", facts.AgentColor)
	}
}

func TestAC5_APIErrorsAreCountedFromIsApiErrorMessageOnly(t *testing.T) {
	// proj-e carries two isApiErrorMessage:true records AND one recovered
	// is_error. The answer is exactly 2 — not 3.
	facts := locate(t, newSource(t, fixtures(t)), sessE, 0)
	if facts.APIErrors != 2 {
		t.Errorf("apiErrors = %d, want 2 (two isApiErrorMessage:true records; the is_error "+
			"beside them is not one)", facts.APIErrors)
	}
}

func TestOnlyTheJSONLiteralTrueCountsAsAnAPIError(t *testing.T) {
	// A vendor that starts writing this key as a string has changed the
	// format, and guessing at a changed format is what this codebase keeps
	// measuring itself to have got wrong. "true", 1 and {} are not true.
	for _, raw := range []string{`"true"`, `1`, `{}`, `null`, `false`, `"1"`} {
		line := []byte(`{"type":"assistant","isApiErrorMessage":` + raw + `}`)
		var facts observer.TranscriptFacts
		scanFacts(append(line, '\n'), true, &facts)
		if facts.APIErrors != 0 {
			t.Errorf("isApiErrorMessage:%s counted as an error", raw)
		}
	}
	var facts observer.TranscriptFacts
	scanFacts([]byte(`{"type":"assistant","isApiErrorMessage":true}`+"\n"), true, &facts)
	if facts.APIErrors != 1 {
		t.Errorf("the literal true was not counted: %d", facts.APIErrors)
	}
}

// ---------------------------------------------------------------------------
// AC4 — subagents are counted BY FILE
// ---------------------------------------------------------------------------

func TestAC4_SubagentsAreCountedByFileAndBeatWhatTheTranscriptKnows(t *testing.T) {
	// proj-a's subagents directory holds THREE meta files. Two of them are
	// spawnDepth 1 and their toolUseIds appear in the parent transcript; the
	// third is spawnDepth 2, nested under another agent, and its toolUseId
	// appears NOWHERE in the parent — which is measured fact 10 and was
	// re-verified on the live machine while writing this phase (session
	// c874de20: 17 meta files, the one spawnDepth:2 toolUseId occurring 0
	// times in the parent against 3-6 for every depth-1 one).
	//
	// So the test computes BOTH answers and asserts the by-file one is larger.
	// A test that only asserted "3" would pass just as happily against a
	// transcript-derived implementation on a fixture that happened to agree.
	fsys := fixtures(t)
	facts := locate(t, newSource(t, fsys), sessA, 0)

	if !facts.SubagentsKnown || facts.Subagents == nil {
		t.Fatalf("subagentsKnown=%v subagents=%v; proj-a HAS a subagents directory",
			facts.SubagentsKnown, facts.Subagents)
	}
	if *facts.Subagents != 3 {
		t.Errorf("subagents = %d, want 3 (three *.meta.json files; the two agent-*.jsonl "+
			"siblings in the same directory are not agents)", *facts.Subagents)
	}

	derived := transcriptDerivedSubagentCount(t, fsys, relA, metaToolUseIDs(t, fsys, sessA))
	if derived >= *facts.Subagents {
		t.Fatalf("the transcript-derived count is %d and the by-file count is %d; the fixture no "+
			"longer reproduces fact 10's undercount and this test proves nothing",
			derived, *facts.Subagents)
	}
	t.Logf("by file %d, transcript-derived %d — the nested spawnDepth:2 agent is the difference",
		*facts.Subagents, derived)
}

func TestAC4_AnAbsentSubagentsDirectoryIsNullAndNeverZero(t *testing.T) {
	// "We cannot say" and "there were none" are different answers and the
	// consumer needs both. proj-c has no subagents directory.
	facts := locate(t, newSource(t, fixtures(t)), sessC, 0)
	if facts.Subagents != nil {
		t.Errorf("subagents = %d for a session with no subagents directory; want null", *facts.Subagents)
	}
	if facts.SubagentsKnown {
		t.Error("subagentsKnown is true with no directory to know anything from")
	}
}

func TestAnEmptySubagentsDirectoryIsAGenuineZero(t *testing.T) {
	// The complement, and the reason Subagents is a pointer at all: a
	// directory that exists and holds no meta file is a MEASUREMENT of zero.
	fsys := fstest.MapFS{
		"p/" + sessA + ".jsonl":                        &fstest.MapFile{Data: []byte("{}\n")},
		"p/" + sessA + "/subagents/agent-x.jsonl":      &fstest.MapFile{Data: []byte("{}\n")},
		"p/" + sessA + "/subagents/notes.md":           &fstest.MapFile{Data: []byte("x")},
		"p/" + sessA + "/subagents/agent-y.meta.json2": &fstest.MapFile{Data: []byte("{}")},
	}
	facts := locate(t, newSource(t, fsys), sessA, 0)
	if !facts.SubagentsKnown {
		t.Fatal("subagentsKnown is false for a directory that exists")
	}
	if facts.Subagents == nil || *facts.Subagents != 0 {
		t.Errorf("subagents = %v, want a pointer to 0: the directory holds a transcript, a "+
			"markdown file and a near-miss suffix, and none of them is an agent", facts.Subagents)
	}
}

func TestNoSubagentMetaFileIsEverOPENED(t *testing.T) {
	// The count is the whole answer, so opening a meta file could only add
	// exposure: they carry a `description` field holding the instruction the
	// agent was given, which is user text (§5.4). This is a claim about which
	// files were touched, so it is counted.
	counting := newCountingFS(fixtures(t))
	locate(t, newSource(t, counting), sessA, 0)

	for _, name := range counting.All() {
		if strings.HasSuffix(name, MetaSuffix) {
			t.Errorf("%s was opened %d times; the count is by FILE NAME and parses nothing",
				name, counting.Opens(name))
		}
	}
}

func TestSubagentsDirForDerivesTheSiblingDirectory(t *testing.T) {
	got := SubagentsDirFor("-home-user-proj-a/" + sessA + ".jsonl")
	want := "-home-user-proj-a/" + sessA + "/" + SubagentsDirName
	if got != want {
		t.Errorf("SubagentsDirFor = %q, want %q", got, want)
	}
}

// metaToolUseIDs reads the toolUseIds out of a session's meta files. It exists
// only to build the comparison count in the AC4 test — production NEVER opens
// a meta file, which TestNoSubagentMetaFileIsEverOPENED asserts.
func metaToolUseIDs(t *testing.T, fsys fs.FS, sessionID string) []string {
	t.Helper()
	dir := SubagentsDirFor("-home-user-proj-a/" + sessionID + ".jsonl")
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), MetaSuffix) {
			continue
		}
		data, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		// Decoded by KEY NAME into a map rather than into a tagged struct:
		// §5.1's AST audit bans json tags outside the wire package and it
		// scans test files too, which is the right scope — a tagged struct in
		// a test is a second definition of a format.
		var meta map[string]any
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("decoding %s: %v", e.Name(), err)
		}
		if id, ok := meta["toolUseId"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no toolUseIds in %s; the comparison count would be vacuous", dir)
	}
	return out
}

// transcriptDerivedSubagentCount is what a transcript-PARSING implementation
// would have answered: how many of the agents' tool-use ids the parent
// transcript mentions at all. It is the method §8.3 item 6 replaced.
func transcriptDerivedSubagentCount(t *testing.T, fsys fs.FS, rel string, ids []string) int {
	t.Helper()
	data, err := fs.ReadFile(fsys, rel)
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	src := string(data)
	var n int
	for _, id := range ids {
		if strings.Contains(src, id) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// AC7 — located by GLOB; the colliding slug directory is never read
// ---------------------------------------------------------------------------

func TestAC7_TheGlobLocatesATranscriptWithoutDerivingASlug(t *testing.T) {
	mod := time.Date(2026, 9, 18, 1, 9, 2, 0, time.UTC)
	fsys := fstest.MapFS{
		relA: &fstest.MapFile{Data: []byte(`{"type":"agent-color","agentColor":"purple"}` + "\n"), ModTime: mod},
	}
	loc, err := newSource(t, fsys).FindByGlob(context.Background(), sessA)
	if err != nil {
		t.Fatalf("FindByGlob: %v", err)
	}
	if !loc.Found {
		t.Fatal("not found")
	}
	if want := abs(relA); loc.Path != want {
		t.Errorf("path = %q, want %q", loc.Path, want)
	}
	if loc.SizeBytes == 0 {
		t.Error("sizeBytes is 0; it is half the enrichment cache key")
	}
	if !loc.ModTime.Equal(observer.TruncateTime(mod)) {
		t.Errorf("modTime = %s, want %s", loc.ModTime, observer.TruncateTime(mod))
	}
	if loc.Ambiguous {
		t.Error("a single match reported Ambiguous")
	}
}

func TestAC7_TheCollidingSlugDirectoryIsNEVEROpened(t *testing.T) {
	// -home-user-proj-a-collide holds an UNRELATED session's transcript. It is
	// exactly what a cwd->slug derivation resolves proj-a's session to — the
	// measured 4.14 % failure, unfixable because `proj_a` and `proj-a` produce
	// the same slug. A slug-deriving implementation reads that file and
	// publishes another conversation's colour, api error count and subagent
	// count as this session's. Nothing about the answer would look wrong.
	//
	// So the assertion is on which files were TOUCHED, not on the answer.
	counting := newCountingFS(fixtures(t))
	facts := locate(t, newSource(t, counting), sessA, 0)

	if n := counting.Opens(relDecoy); n != 0 {
		t.Errorf("the colliding directory's transcript was opened %d times", n)
	}
	if n := counting.Opens(relA); n == 0 {
		t.Error("proj-a's own transcript was never opened, so the read proved nothing")
	}
	if facts.AgentColor == "yellow" {
		t.Error(`agentColor = "yellow" — that is the DECOY's colour, from the colliding slug ` +
			`directory`)
	}
	if facts.AgentColor != "purple" {
		t.Errorf("agentColor = %q, want purple", facts.AgentColor)
	}
}

func TestAC7_TwoMatchesAreAmbiguousAndNeitherFileIsOpened(t *testing.T) {
	// The same session id exists in two slug directories. Picking one would be
	// a coin flip dressed as an answer, and the wrong side of the flip is
	// another session's conversation.
	counting := newCountingFS(fixtures(t))
	loc, err := newSource(t, counting).FindByGlob(context.Background(), sessDup)
	if err != nil {
		t.Fatalf("FindByGlob: %v", err)
	}
	if loc.Found {
		t.Error("Found is true for an ambiguous match; the core would publish one of two files")
	}
	if !loc.Ambiguous {
		t.Fatal("Ambiguous is false; the core has nothing to raise transcript-ambiguous from")
	}
	if len(loc.Matches) != 2 {
		t.Fatalf("matches = %v, want both slug directories", loc.Matches)
	}
	// Sorted and absolute, because these strings go onto the wire inside an
	// advisory and AC11 compares bytes.
	if loc.Matches[0] >= loc.Matches[1] {
		t.Errorf("matches are not sorted: %v", loc.Matches)
	}
	for _, m := range loc.Matches {
		if !strings.HasPrefix(m, projectsRoot+"/") {
			t.Errorf("match %q is not an absolute path under the root", m)
		}
	}
	for _, rel := range []string{relDupA, relDupCol} {
		if n := counting.Opens(rel); n != 0 {
			t.Errorf("%s was opened %d times for an ambiguous match", rel, n)
		}
	}
}

func TestAGlobMissIsNotFoundAndIsNotAnError(t *testing.T) {
	// A session that has not written a transcript yet is ordinary, and a brand
	// new session is exactly the case. An error here would make a normal state
	// look like a failure on every surface.
	loc, err := newSource(t, fixtures(t)).FindByGlob(context.Background(),
		"00000000-0000-4000-8000-00000000dead")
	if err != nil {
		t.Fatalf("a miss returned an error: %v", err)
	}
	if loc.Found || loc.Ambiguous || loc.Path != "" {
		t.Errorf("a miss returned %+v", loc)
	}
}

func TestASessionIDThatWouldWIDENTheGlobIsRefusedAndReadsNothing(t *testing.T) {
	// A session id arrives out of a registry record or an agents listing —
	// untrusted input, by the same argument that makes OpenPath validate
	// linkScanPath. `*` turns "find this session" into "match a neighbour",
	// and the answer looks perfectly well-formed.
	counting := newCountingFS(fixtures(t))
	src := newSource(t, counting)
	for _, id := range []string{"*", "?", sessA + "*", "[a-z]", "../" + sessA, "a/b", "", ".."} {
		loc, err := src.FindByGlob(context.Background(), id)
		if !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("FindByGlob(%q) err = %v, want ErrInvalidSessionID (loc %+v)", id, err, loc)
		}
	}
	if opened := counting.All(); len(opened) != 0 {
		t.Errorf("a refused session id still touched %v", opened)
	}
}

func TestTheGlobIsNonRecursiveAndCannotReachASubagentTranscript(t *testing.T) {
	// The pattern is two segments. A subagent transcript sits at depth 3
	// (<slug>/<sessionId>/subagents/agent-….jsonl), so it is STRUCTURALLY
	// unreachable rather than excluded by a filter someone could relax — the
	// same property the retention floor's glob depends on.
	if got := SessionGlobFor(sessA); got != "*/"+sessA+".jsonl" {
		t.Fatalf("SessionGlobFor = %q", got)
	}
	if strings.Contains(SessionGlobFor(sessA), "**") {
		t.Error("the pattern grew a recursive wildcard")
	}
	loc, err := newSource(t, fixtures(t)).FindByGlob(context.Background(), "agent-0001aaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("FindByGlob: %v", err)
	}
	if loc.Found {
		t.Errorf("a subagent transcript was located as a session transcript: %s", loc.Path)
	}
}

// ---------------------------------------------------------------------------
// AC12 — the 200 KB tail finds the colour, and falls back cleanly when it does not
// ---------------------------------------------------------------------------

func TestAC12_AColourBeyondTheTailIsNoOpinionAndNotAnError(t *testing.T) {
	// proj-b's agent-color record is the SECOND line of an 11 KB file, so a
	// 2 KB tail cannot see it. The required answer is the triple
	// (colorFound:false, agentColor:"", truncated:true) with NO error — "we
	// did not read far enough" has to stay distinguishable from "this session
	// has no colour", because the first is a reason to widen the tail and the
	// second is a fact about the session.
	src := newSource(t, fixtures(t))

	narrow := locate(t, src, sessB, 2<<10)
	if narrow.ColorFound {
		t.Error("colorFound is true from a tail that cannot reach the record")
	}
	if narrow.AgentColor != "" {
		t.Errorf("agentColor = %q from a truncated tail", narrow.AgentColor)
	}
	if !narrow.Truncated {
		t.Error("truncated is false for a 2 KB tail of an 11 KB file")
	}
	if narrow.TailBytes > 2<<10 {
		t.Errorf("tailBytes = %d, over the injected bound", narrow.TailBytes)
	}

	// The same fixture at the production bound reads the whole file and finds
	// it. One fixture, both sides of the bound.
	wide := locate(t, src, sessB, 0)
	if !wide.ColorFound || wide.AgentColor != "cyan" {
		t.Errorf("at the production bound: colorFound=%v agentColor=%q, want true/cyan",
			wide.ColorFound, wide.AgentColor)
	}
	if wide.Truncated {
		t.Error("truncated is true for a tail that reached offset 0")
	}
}

func TestAC12_TheProductionFactsBoundIsTheMeasuredTwoHundredKB(t *testing.T) {
	if FactsTailBytes != 200<<10 {
		t.Errorf("FactsTailBytes = %d, want %d", FactsTailBytes, 200<<10)
	}
	if FactsTailBytes != observer.DefaultFactsTailBytes {
		t.Errorf("transcriptfs.FactsTailBytes = %d but observer.DefaultFactsTailBytes = %d; the "+
			"published bound would not be the bound that was used",
			FactsTailBytes, observer.DefaultFactsTailBytes)
	}
	if FactsTailBytes == TimingTailBytes {
		t.Error("the two bounds are equal; §7.3's split has collapsed")
	}
}

func TestAPartialFirstLineIsDiscardedByTheEnrichmentPassToo(t *testing.T) {
	// A tail taken mid-file begins inside a record. The fragment here is the
	// TAIL HALF of an agent-color record, and it is valid-looking enough that
	// a lenient reader could take "silver" from it.
	head := strings.Repeat(`{"type":"user","uuid":"pad","timestamp":"2026-09-18T01:00:00.000Z"}`+"\n", 40)
	body := `{"type":"agent-color","agentColor":"silver"}` + "\n" +
		`{"type":"user","uuid":"z","timestamp":"2026-09-18T01:00:01.000Z"}` + "\n"
	fsys := fstest.MapFS{relA: &fstest.MapFile{Data: []byte(head + body)}}

	src := newSource(t, fsys)

	// (a) the tail starts PART WAY THROUGH the colour record's line.
	if facts := locate(t, src, sessA, len(body)-20); facts.ColorFound {
		t.Errorf("a partial first line produced a colour: %q", facts.AgentColor)
	}

	// (b) the tail starts EXACTLY at the colour record's first byte, so the
	// first line is a complete, perfectly parseable colour record — and it is
	// still discarded. That is deliberate and it is the conservative
	// direction: at a byte offset we cannot tell a whole record from the tail
	// half of one, and colorFound:false tells the consumer we did not see a
	// colour, whereas a colour taken from a fragment would be asserted as
	// fact. The cost is bounded to one record of one tail.
	if facts := locate(t, src, sessA, len(body)); facts.ColorFound {
		t.Errorf("the discarded first line was read anyway: %q", facts.AgentColor)
	}
}

// ---------------------------------------------------------------------------
// exposure and degradation
// ---------------------------------------------------------------------------

func TestOnlyTheAllowedFactsEverLeaveATranscript(t *testing.T) {
	// §5.4: only agentColor, apiErrors, subagents, sessionKind, sizeBytes, the
	// context size, the model id, the compaction count and the newest
	// timestamp may leave transcriptfs. The canaries are synthetic and
	// deliberately NOT in testdata/, so the fixture privacy audit can go on
	// asserting those key names appear nowhere under it.
	//
	// The message object is the one the context read OPENS, so it is where a
	// leak would come from: it carries content as a string AND as blocks, and
	// a canary in every sibling key the read does not ask for.
	record := map[string]any{
		"type":              "assistant",
		"timestamp":         "2026-09-18T02:39:11.204Z",
		"isApiErrorMessage": true,
		"sessionKind":       "bg",
		"message": map[string]any{
			"content":     "CANARY-CONTENT",
			"model":       "claude-opus-5-5",
			"role":        "CANARY-ROLE",
			"stop_reason": "CANARY-STOP",
			"usage": map[string]any{
				"input_tokens": 2, "cache_creation_input_tokens": 20844,
				"cache_read_input_tokens": 559849, "output_tokens": 968,
				"service_tier": "CANARY-TIER",
			},
		},
		"lastPrompt":    "CANARY-PROMPT",
		"text":          "CANARY-TEXT",
		"toolUseResult": "CANARY-RESULT",
	}
	blocks := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model":   "<synthetic>",
			"content": []any{map[string]any{"type": "text", "text": "CANARY-BLOCK"}},
			"usage":   map[string]any{"input_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		},
	}
	bdata, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	colour := map[string]any{"type": "agent-color", "agentColor": "purple"}
	cdata, err := json.Marshal(colour)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{relA: &fstest.MapFile{
		Data: jsonl(string(data), string(cdata), string(bdata)),
	}}

	facts := locate(t, newSource(t, fsys), sessA, 0)
	rendered := fmt.Sprintf("%+v", facts)
	for _, canary := range []string{
		"CANARY-CONTENT", "CANARY-PROMPT", "CANARY-TEXT", "CANARY-RESULT",
		"CANARY-ROLE", "CANARY-STOP", "CANARY-TIER", "CANARY-BLOCK",
	} {
		if strings.Contains(rendered, canary) {
			t.Errorf("%s crossed the boundary: %s", canary, rendered)
		}
	}
	// The context facts arrived from the same record the canaries ride in,
	// so "nothing leaked" is not "nothing was read".
	if facts.ContextTokens == nil || *facts.ContextTokens != 2+20844+559849 {
		t.Errorf("contextTokens = %v, want %d", facts.ContextTokens, 2+20844+559849)
	}
	if facts.Model != "claude-opus-5-5" {
		t.Errorf("model = %q, want claude-opus-5-5", facts.Model)
	}
	// ...and the facts that ARE allowed did arrive, so the test is not passing
	// by reading nothing at all.
	if facts.APIErrors != 1 || facts.AgentColor != "purple" {
		t.Errorf("the allowed facts did not arrive: %+v", facts)
	}
	if facts.SessionKind == nil || *facts.SessionKind != "bg" {
		t.Errorf("sessionKind = %v, want bg", facts.SessionKind)
	}
}

func TestAnOverlongOrWhitespacedColourIsDroppedRatherThanCarried(t *testing.T) {
	// AgentColor is the one string field on TranscriptFacts that comes out of
	// a transcript's own bytes, so the exposure guarantee cannot rest on the
	// type. Measured values are colour NAMES.
	long := strings.Repeat("x", MaxAgentColorBytes+1)
	for _, bad := range []string{long, "a sentence with spaces", "green\x00", "\t"} {
		line, err := json.Marshal(map[string]any{"type": "agent-color", "agentColor": bad})
		if err != nil {
			t.Fatal(err)
		}
		var facts observer.TranscriptFacts
		scanFacts(append(line, '\n'), true, &facts)
		if facts.ColorFound || facts.AgentColor != "" {
			t.Errorf("agentColor %q was carried: %+v", bad, facts)
		}
	}
	// A hex triple is a shape we have not measured but would be legible, and
	// it is well inside the bound — the check must not be so tight that a
	// plausible future format is refused.
	var ok observer.TranscriptFacts
	scanFacts([]byte(`{"type":"agent-color","agentColor":"#a0ffc3"}`+"\n"), true, &ok)
	if !ok.ColorFound || ok.AgentColor != "#a0ffc3" {
		t.Errorf("a hex colour was refused: %+v", ok)
	}
}

func TestAnUnusableColourRecordDoesNotClearAGoodOne(t *testing.T) {
	// Last-wins applies to colours we can carry. A later record whose value we
	// refuse is not evidence that the earlier colour stopped applying.
	lines := `{"type":"agent-color","agentColor":"green"}` + "\n" +
		`{"type":"agent-color","agentColor":"` + strings.Repeat("y", 200) + `"}` + "\n"
	var facts observer.TranscriptFacts
	scanFacts([]byte(lines), true, &facts)
	if !facts.ColorFound || facts.AgentColor != "green" {
		t.Errorf("facts = %+v, want green", facts)
	}
}

func TestOneMalformedLineDoesNotBlankTheEnrichment(t *testing.T) {
	// "One malformed record must never blank the set" is the bug this
	// repository has already shipped once.
	data := `{"type":"agent-color","agentColor":"green"}` + "\n" +
		`{"type":"assistant",` + "\n" + // torn mid-object
		`not json at all` + "\n" +
		`{"type":"assistant","isApiErrorMessage":true}` + "\n"
	fsys := fstest.MapFS{relA: &fstest.MapFile{Data: []byte(data)}}
	facts := locate(t, newSource(t, fsys), sessA, 0)
	if facts.AgentColor != "green" || !facts.ColorFound {
		t.Errorf("a torn line cost the colour: %+v", facts)
	}
	if facts.APIErrors != 1 {
		t.Errorf("apiErrors = %d, want 1", facts.APIErrors)
	}
}

func TestReadFactsOnAnAbsentTargetIsEmptyAndNotAnError(t *testing.T) {
	facts, err := newSource(t, fixtures(t)).ReadFacts(context.Background(), observer.TranscriptLocation{}, 0)
	if err != nil {
		t.Fatalf("ReadFacts on a not-found location: %v", err)
	}
	if facts != (observer.TranscriptFacts{}) {
		t.Errorf("facts = %+v, want the zero value", facts)
	}
}

func TestReadFactsRefusesALocationOutsideTheRoot(t *testing.T) {
	// Symmetry with the timing pass: the containment check is on the READ as
	// well as on the locator, so a hand-built location cannot reach out of the
	// tree.
	_, err := newSource(t, fixtures(t)).ReadFacts(context.Background(),
		observer.TranscriptLocation{Found: true, Path: "/etc/passwd"}, 0)
	if !errors.Is(err, ErrOutsideProjectsRoot) {
		t.Errorf("err = %v, want ErrOutsideProjectsRoot", err)
	}
}

func TestACancelledContextStopsTheEnrichmentPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := newSource(t, fixtures(t))
	if _, err := src.FindByGlob(ctx, sessA); err == nil {
		t.Error("FindByGlob ignored a cancelled context")
	}
	if _, err := src.ReadFacts(ctx, observer.TranscriptLocation{Found: true, Path: abs(relA)}, 0); err == nil {
		t.Error("ReadFacts ignored a cancelled context")
	}
}

// ---------------------------------------------------------------------------
// FR6 — the context size, the model id and the compaction count
// ---------------------------------------------------------------------------
//
// Every record below is built in code. The fixture privacy audit denylists the
// key "message" under testdata/, and an assistant usage record cannot be
// written without one.

// jsonl joins records into a transcript body, one per line.
func jsonl(records ...string) []byte {
	return []byte(strings.Join(records, "\n") + "\n")
}

// rec marshals one record, failing the test on a shape encoding/json refuses.
func rec(t *testing.T, v map[string]any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// usage is a usage object with the three input counts plus an output count
// big enough that including it would be visible in every sum.
func usage(in, cacheCreation, cacheRead any) map[string]any {
	return map[string]any{
		"input_tokens":                in,
		"cache_creation_input_tokens": cacheCreation,
		"cache_read_input_tokens":     cacheRead,
		"output_tokens":               7_000_000,
	}
}

// assistant is a main-thread assistant record with the given model and usage.
func assistant(t *testing.T, model any, u any) string {
	t.Helper()
	return rec(t, map[string]any{
		"type":    "assistant",
		"message": map[string]any{"model": model, "usage": u, "content": "x"},
	})
}

// compaction is a compact_boundary record; post < 0 means "no postTokens key".
func compaction(t *testing.T, post int64) string {
	t.Helper()
	meta := map[string]any{"trigger": "auto", "preTokens": 680872}
	if post >= 0 {
		meta["postTokens"] = post
	}
	return rec(t, map[string]any{"type": "system", "subtype": "compact_boundary", "compactMetadata": meta})
}

// scan runs the enrichment pass over a whole-file tail.
func scan(body []byte) observer.TranscriptFacts {
	var facts observer.TranscriptFacts
	scanFacts(body, true, &facts)
	return facts
}

func wantContext(t *testing.T, facts observer.TranscriptFacts, want int64) {
	t.Helper()
	if facts.ContextTokens == nil {
		t.Fatalf("contextTokens = nil, want %d", want)
	}
	if *facts.ContextTokens != want {
		t.Fatalf("contextTokens = %d, want %d", *facts.ContextTokens, want)
	}
}

func wantNoContext(t *testing.T, facts observer.TranscriptFacts, why string) {
	t.Helper()
	if facts.ContextTokens != nil {
		t.Fatalf("contextTokens = %d, want nil: %s", *facts.ContextTokens, why)
	}
}

func TestTheContextKeysAreTheMeasuredOnes(t *testing.T) {
	// Pinned for the same reason as the colour key: a key name is a
	// measurement, and a literal buried in a loop is where the next rename
	// hides.
	for got, want := range map[string]string{
		inputTokensKey: "input_tokens", cacheCreationKey: "cache_creation_input_tokens",
		cacheReadKey: "cache_read_input_tokens", messageKey: "message", modelKey: "model",
		usageKey: "usage", sidechainKey: "isSidechain", syntheticModel: "<synthetic>",
		compactSubtype: "compact_boundary", compactMetaKey: "compactMetadata",
		postTokensKey: "postTokens",
	} {
		if got != want {
			t.Errorf("key %q, want %q", got, want)
		}
	}
}

func TestContextIsInputPlusBothCacheCountsAndNeverOutput(t *testing.T) {
	// The measured shape: 2 + 20 844 + 559 849. Output tokens (7 000 000 here,
	// so their inclusion cannot hide) are what the model WROTE.
	facts := scan(jsonl(assistant(t, "claude-opus-5", usage(2, 20844, 559849))))
	wantContext(t, facts, 580695)
	if facts.Model != "claude-opus-5" {
		t.Errorf("model = %q", facts.Model)
	}
}

func TestTheNewestQualifyingAssistantRecordWins(t *testing.T) {
	facts := scan(jsonl(
		assistant(t, "claude-opus-5", usage(1, 1, 98)),
		`{"type":"user","uuid":"u1"}`,
		assistant(t, "claude-fable-5-1", usage(10, 20, 170)),
		`{"type":"user","uuid":"u2"}`,
	))
	wantContext(t, facts, 200)
	if facts.Model != "claude-fable-5-1" {
		t.Errorf("model = %q: the model must come from the SAME record as the count", facts.Model)
	}
}

func TestSyntheticZeroUsageAndSidechainRecordsAreSkipped(t *testing.T) {
	sidechain := rec(t, map[string]any{
		"type": "assistant", "isSidechain": true,
		"message": map[string]any{"model": "claude-opus-5-5", "usage": usage(9, 9, 900_000)},
	})
	facts := scan(jsonl(
		assistant(t, "claude-opus-5-5", usage(1, 1000, 4000)),
		// Synthetic with a NON-zero usage: skipped by the model, so the
		// test proves the model check rather than leaning on the zero check.
		assistant(t, "<synthetic>", usage(1, 1, 1)),
		// A real model with an all-zero usage: skipped by the zero check.
		assistant(t, "claude-opus-5-5", usage(0, 0, 0)),
		sidechain,
	))
	wantContext(t, facts, 5001)
}

func TestTwentySyntheticRecordsAfterAValidOneStillReportIt(t *testing.T) {
	// R3: there is no fixed window of candidates. An 8-slot ring would have
	// lost the valid record to the ninth placeholder and reported nil.
	records := []string{assistant(t, "claude-opus-5-5", usage(3, 300, 131_697))}
	for i := 0; i < 20; i++ {
		records = append(records, assistant(t, "<synthetic>", usage(0, 0, 0)))
	}
	facts := scan(jsonl(records...))
	wantContext(t, facts, 132_000)
	if facts.Model != "claude-opus-5-5" {
		t.Errorf("model = %q", facts.Model)
	}
}

func TestAnUnusableUsageIsSkippedAndNeverReadAsZero(t *testing.T) {
	good := assistant(t, "claude-opus-5-5", usage(5, 5, 90))
	bad := map[string]string{
		"missing cache key": assistant(t, "m", map[string]any{"input_tokens": 5, "cache_read_input_tokens": 5}),
		"string count":      assistant(t, "m", usage("5", 5, 5)),
		"negative count":    assistant(t, "m", usage(-5, 50, 50)),
		"fractional count":  assistant(t, "m", usage(1.5, 5, 5)),
		"null count":        assistant(t, "m", usage(nil, 5, 5)),
		"implausible count": assistant(t, "m", usage(maxPlausibleTokens+1, 0, 0)),
		"implausible sum":   assistant(t, "m", usage(maxPlausibleTokens, maxPlausibleTokens, 0)),
		"usage not object":  assistant(t, "m", "lots"),
		"usage null":        assistant(t, "m", nil),
		"message not object": rec(t, map[string]any{
			"type": "assistant", "message": "a string",
		}),
		"no message": `{"type":"assistant"}`,
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			wantNoContext(t, scan(jsonl(b)), name+" alone is not a measurement")
			after := scan(jsonl(good, b))
			wantContext(t, after, 100)
			if after.Model != "claude-opus-5-5" {
				t.Errorf("model = %q: it came from the skipped record", after.Model)
			}
		})
	}
}

func TestATailWithNoAssistantUsageIsNotMeasurable(t *testing.T) {
	facts := scan(jsonl(`{"type":"user","uuid":"a"}`, `{"type":"agent-color","agentColor":"green"}`))
	wantNoContext(t, facts, "no assistant record at all")
	if facts.Model != "" || facts.Compactions != 0 {
		t.Errorf("model=%q compactions=%d from a tail with neither", facts.Model, facts.Compactions)
	}
}

func TestACompactionAfterTheUsageResetsTheContext(t *testing.T) {
	big := assistant(t, "claude-opus-5", usage(2, 20844, 659_000))
	small := assistant(t, "claude-opus-5", usage(2, 1000, 30_000))

	t.Run("with postTokens", func(t *testing.T) {
		facts := scan(jsonl(big, compaction(t, 21149)))
		wantContext(t, facts, 21149)
		if facts.Model != "claude-opus-5" {
			t.Errorf("model = %q: a reset context is still the same model's", facts.Model)
		}
	})
	t.Run("without postTokens", func(t *testing.T) {
		wantNoContext(t, scan(jsonl(big, compaction(t, -1))),
			"a stale pre-compaction figure is not the context after a reset")
	})
	t.Run("zero postTokens", func(t *testing.T) {
		wantNoContext(t, scan(jsonl(big, compaction(t, 0))), "0 is not a context size")
	})
	t.Run("newest compaction without postTokens does not inherit the older one's", func(t *testing.T) {
		wantNoContext(t, scan(jsonl(big, compaction(t, 21149), compaction(t, -1))),
			"the newest boundary is the one that describes the context")
	})
	t.Run("usage after the compaction wins", func(t *testing.T) {
		wantContext(t, scan(jsonl(big, compaction(t, 21149), small)), 31002)
	})
	t.Run("compaction with no usage in the tail", func(t *testing.T) {
		wantContext(t, scan(jsonl(compaction(t, 21149))), 21149)
	})
	t.Run("a synthetic record after the compaction does not hide it", func(t *testing.T) {
		wantContext(t, scan(jsonl(big, compaction(t, 21149), assistant(t, "<synthetic>", usage(0, 0, 0)))), 21149)
	})
}

func TestCompactionsAreCountedWithinTheTailOnly(t *testing.T) {
	notACompaction := rec(t, map[string]any{"type": "system", "subtype": "api_error"})
	wrongType := rec(t, map[string]any{"type": "user", "subtype": "compact_boundary"})
	body := jsonl(compaction(t, 100), notACompaction, wrongType, compaction(t, -1), compaction(t, 300))
	if got := scan(body).Compactions; got != 3 {
		t.Errorf("compactions = %d, want 3 (a non-compaction subtype and a non-system type do not count)", got)
	}

	// Through the real read with a tail that cannot reach the first
	// boundary: the count is a LOWER BOUND, and Truncated says so.
	pad := strings.Repeat(`{"type":"user","uuid":"pad"}`+"\n", 100)
	tailPart := jsonl(`{"type":"user","uuid":"cut"}`, compaction(t, 200), compaction(t, 300))
	data := append(append(jsonl(compaction(t, 100)), pad...), tailPart...)
	fsys := fstest.MapFS{relA: &fstest.MapFile{Data: data}}
	facts := locate(t, newSource(t, fsys), sessA, len(tailPart))
	if !facts.Truncated {
		t.Fatal("the tail reached offset 0; the test proves nothing")
	}
	if facts.Compactions != 2 {
		t.Errorf("compactions = %d, want 2: the first boundary is outside the tail", facts.Compactions)
	}
	wantContext(t, facts, 300)
}

func TestAModelIsKeptOnlyWhenBoundedAndPlainButTheCountsAlwaysArrive(t *testing.T) {
	for _, id := range []string{
		"claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "gpt-6-astra",
		"qwen/qwen3.8-max-0902", "google/gemma-4-26b-a4b-qat", "GLM-5.3",
		"claude-opus-5-5[1m]", "vendor:model@2026_09", strings.Repeat("m", MaxModelBytes),
	} {
		facts := scan(jsonl(assistant(t, id, usage(1, 1, 1))))
		if facts.Model != id {
			t.Errorf("a valid id %q was refused (got %q)", id, facts.Model)
		}
	}
	for name, model := range map[string]any{
		"whitespace":     "claude opus",
		"tab":            "claude-opus\t5",
		"newline":        "claude\nopus",
		"over the bound": strings.Repeat("m", MaxModelBytes+1),
		"a sentence":     "Here is the summary you asked for.",
		"non-ascii":      "modèle",
		"quote":          `claude"opus`,
		"empty":          "",
		"number":         42,
		"object":         map[string]any{"id": "claude"},
		"array":          []any{"claude"},
		"bool":           true,
		"null":           nil,
	} {
		facts := scan(jsonl(assistant(t, model, usage(1, 2, 3))))
		if facts.Model != "" {
			t.Errorf("%s: model %q was carried", name, facts.Model)
		}
		// Dropped, NOT truncated, and the numbers beside it are kept.
		wantContext(t, facts, 6)
	}
}

func TestAnOverlongOrWhitespacedSessionKindIsDropped(t *testing.T) {
	// SessionKind was the one string out of a transcript's bytes with no
	// bound. Measured values are all "bg".
	for _, bad := range []string{strings.Repeat("k", MaxSessionKindBytes+1), "a b", "bg\n"} {
		body := jsonl(rec(t, map[string]any{"type": "user", "sessionKind": "bg"}),
			rec(t, map[string]any{"type": "user", "sessionKind": bad}))
		facts := scan(body)
		if facts.SessionKind == nil || *facts.SessionKind != "bg" {
			t.Errorf("sessionKind %q: got %v, want the earlier bg kept", bad, facts.SessionKind)
		}
	}
}
