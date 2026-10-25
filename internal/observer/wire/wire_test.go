package wire

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// refNow is the single reference instant every test in this file derives from.
// One clock, stated once: the architecture's own samples drifted three
// revisions running because each capture introduced a second "now".
var refNow = time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)

func opts() Options { return Options{TT: "v0.4.0"} }

// roundTrip encodes a domain snapshot, serialises it, decodes it, and brings it
// back to the domain. The JSON hop is deliberate: a test that called
// ToDomain(NewSnapshot(s)) directly would never exercise the pointer/null
// distinctions that only exist once the value has been through encoding/json,
// which is exactly where a tri-state collapses into a zero.
func roundTrip(t *testing.T, s *observer.Snapshot) *observer.Snapshot {
	t.Helper()
	w := NewSnapshot(s, s.ObservedAt, opts())
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, notes, err := DecodeSnapshot(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("our own encoder produced fields our decoder calls unknown: %v", notes)
	}
	back, _ := ToDomain(decoded)
	return back
}

// ---------------------------------------------------------------------------
// The P0 contract: domain -> wire -> domain is the identity
// ---------------------------------------------------------------------------

// TestRoundTripIsIdentityOnGeneratedSnapshots is H7's property test, and the
// reason P0 owns the wire inverse at all: P8's four-surface agreement rests on
// this, and without it a decoder drift surfaces as a byte-identity failure that
// looks like a socket bug rather than a schema bug.
func TestRoundTripIsIdentityOnGeneratedSnapshots(t *testing.T) {
	rng := rand.New(rand.NewSource(20260918))
	for i := 0; i < 200; i++ {
		snap := generateSnapshot(rng)
		back := roundTrip(t, snap)

		if len(back.Sessions) != len(snap.Sessions) {
			t.Fatalf("case %d: session count %d -> %d", i, len(snap.Sessions), len(back.Sessions))
		}
		for j := range snap.Sessions {
			want, got := snap.Sessions[j], back.Sessions[j]
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("case %d session %d: round trip is not the identity\n want %#v\n  got %#v", i, j, want, got)
			}
		}
		if !reflect.DeepEqual(snap.Sources, back.Sources) {
			t.Errorf("case %d: sources differ\n want %#v\n  got %#v", i, snap.Sources, back.Sources)
		}
		if !reflect.DeepEqual(snap.Warnings, back.Warnings) {
			t.Errorf("case %d: warnings differ", i)
		}
		if !reflect.DeepEqual(snap.Counts, back.Counts) {
			t.Errorf("case %d: counts differ\n want %#v\n  got %#v", i, snap.Counts, back.Counts)
		}
		if !snap.ObservedAt.Equal(back.ObservedAt) {
			t.Errorf("case %d: observedAt %v -> %v", i, snap.ObservedAt, back.ObservedAt)
		}
	}
}

// TestRoundTripCoversEveryDiffedField asserts the property field by field over
// the SAME shared const slice diff() will use, so the two lists cannot drift
// apart. A field that round-trips but is not diffed is invisible to a
// subscriber; a field that is diffed but does not round-trip makes a
// socket-attached consumer disagree with a direct one.
func TestRoundTripCoversEveryDiffedField(t *testing.T) {
	accessors := domainFieldAccessors()

	// The accessor set must cover the shared slice exactly. This is the drift
	// alarm: add a field to DiffedFields without an accessor and this fails.
	for _, f := range observer.DiffedFields {
		if _, ok := accessors[f]; !ok {
			t.Fatalf("DiffedFields names %q but this test has no accessor for it", f)
		}
	}

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		snap := generateSnapshot(rng)
		if len(snap.Sessions) == 0 {
			continue
		}
		back := roundTrip(t, snap)
		for j := range snap.Sessions {
			for _, f := range observer.DiffedFields {
				get := accessors[f]
				want, got := get(snap.Sessions[j]), get(back.Sessions[j])
				if !reflect.DeepEqual(want, got) {
					t.Errorf("case %d session %d field %q: %#v -> %#v", i, j, f, want, got)
				}
			}
		}
	}
}

// TestFieldClassificationIsTotal asserts every json tag on wire.Session is
// classified exactly once as diffed, clock-derived or identity. Without this,
// a field added to the wire struct joins neither set and is silently never
// compared — which is how a delta stream quietly stops reporting a field.
func TestFieldClassificationIsTotal(t *testing.T) {
	tags := jsonFieldNames(Session{})

	declared := map[string]int{}
	for _, group := range [][]string{
		observer.DiffedFields, observer.ClockDerivedFields, observer.IdentityFields,
	} {
		for _, f := range group {
			declared[f]++
		}
	}

	for f, n := range declared {
		if n != 1 {
			t.Errorf("field %q is classified %d times; it must be exactly once", f, n)
		}
		if !tags[f] {
			t.Errorf("field %q is classified but wire.Session carries no such json tag", f)
		}
	}
	for tag := range tags {
		if declared[tag] == 0 {
			t.Errorf("wire.Session carries json tag %q which is in none of "+
				"DiffedFields, ClockDerivedFields or IdentityFields", tag)
		}
	}

	// AllSessionFields must be the same set, in wire order, so a reader has one
	// authoritative list.
	if len(observer.AllSessionFields) != len(tags) {
		t.Errorf("AllSessionFields has %d entries, wire.Session has %d json tags",
			len(observer.AllSessionFields), len(tags))
	}
	for _, f := range observer.AllSessionFields {
		if !tags[f] {
			t.Errorf("AllSessionFields names %q, which wire.Session does not carry", f)
		}
	}
}

// ---------------------------------------------------------------------------
// The awkward cases, each named because each is a whole acceptance criterion
// ---------------------------------------------------------------------------

// TestNilAndAbsentTimesStayNil: an absent optional time must not become the
// zero time. A zero time.Time serialises to year 1 and computes an age of two
// millennia, so "we do not know when" would render as "blocked for 2025 years".
func TestNilAndAbsentTimesStayNil(t *testing.T) {
	s := minimalSnapshot()
	sess := &s.Sessions[0]
	sess.StartedAt = nil
	sess.StatusSince = nil
	sess.StatusSinceLowerBound = nil
	sess.LastActivityAt = nil
	sess.RecordUpdatedAt = nil
	sess.ReapedMidWorkAt = nil

	w := NewSnapshot(s, refNow, opts())
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		"startedAt", "statusSince", "statusSinceLowerBound",
		"lastActivityAt", "recordUpdatedAt", "reapedMidWorkAt",
	} {
		if !strings.Contains(string(data), `"`+field+`":null`) {
			t.Errorf("expected %q to serialise as explicit null; got %s", field, data)
		}
	}

	back := roundTrip(t, s)
	got := back.Sessions[0]
	for name, v := range map[string]*time.Time{
		"StartedAt":             got.StartedAt,
		"StatusSince":           got.StatusSince,
		"StatusSinceLowerBound": got.StatusSinceLowerBound,
		"LastActivityAt":        got.LastActivityAt,
		"RecordUpdatedAt":       got.RecordUpdatedAt,
		"ReapedMidWorkAt":       got.ReapedMidWorkAt,
	} {
		if v != nil {
			t.Errorf("%s came back as %v, want nil", name, *v)
		}
	}
}

// TestLiveNullIsDistinctFromLiveFalse is the tri-state contract. null means "no
// evidence either way" and must never be produced by, or collapse into, a zero
// value — the default filter is live != false, so confusing the two would hide
// every unknown-liveness session.
func TestLiveNullIsDistinctFromLiveFalse(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		val  *bool
		json string
	}{
		{"null", nil, `"live":null`},
		{"false", &no, `"live":false`},
		{"true", &yes, `"live":true`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := minimalSnapshot()
			s.Sessions[0].Live = c.val
			s.Sessions[0].ProcAlive = c.val
			s.Sessions[0].PaneAlive = c.val

			data, err := json.Marshal(NewSnapshot(s, refNow, opts()))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(data), c.json) {
				t.Errorf("expected %s in %s", c.json, data)
			}

			back := roundTrip(t, s)
			for name, got := range map[string]*bool{
				"Live":      back.Sessions[0].Live,
				"ProcAlive": back.Sessions[0].ProcAlive,
				"PaneAlive": back.Sessions[0].PaneAlive,
			} {
				switch {
				case c.val == nil && got != nil:
					t.Errorf("%s: null became %v", name, *got)
				case c.val != nil && got == nil:
					t.Errorf("%s: %v became null", name, *c.val)
				case c.val != nil && *got != *c.val:
					t.Errorf("%s: %v became %v", name, *c.val, *got)
				}
			}
		})
	}
}

// TestBlockedForMsNullIsDistinctFromZero: nil means "not applicable, or not
// measurable"; 0 means "it just happened". A consumer that cannot tell them
// apart either renders "blocked for 0ms" on every idle session or drops a
// genuinely fresh block.
func TestBlockedForMsNullIsDistinctFromZero(t *testing.T) {
	zero := int64(0)
	cases := []struct {
		name string
		val  *int64
		json string
	}{
		{"null", nil, `"blockedForMs":null`},
		{"zero", &zero, `"blockedForMs":0`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := minimalSnapshot()
			s.Sessions[0].BlockedForMs = c.val
			s.Sessions[0].StatusAgeMs = c.val
			s.Sessions[0].RecordAgeMs = c.val
			s.Sessions[0].Tokens = c.val

			data, err := json.Marshal(NewSnapshot(s, refNow, opts()))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(data), c.json) {
				t.Errorf("expected %s in %s", c.json, data)
			}

			back := roundTrip(t, s)
			for name, got := range map[string]*int64{
				"BlockedForMs": back.Sessions[0].BlockedForMs,
				"StatusAgeMs":  back.Sessions[0].StatusAgeMs,
				"RecordAgeMs":  back.Sessions[0].RecordAgeMs,
				"Tokens":       back.Sessions[0].Tokens,
			} {
				switch {
				case c.val == nil && got != nil:
					t.Errorf("%s: null became %d", name, *got)
				case c.val != nil && got == nil:
					t.Errorf("%s: %d became null", name, *c.val)
				case c.val != nil && *got != *c.val:
					t.Errorf("%s: %d became %d", name, *c.val, *got)
				}
			}
		})
	}
}

// TestSubagentsNullIsDistinctFromZero: null is "the subagents directory does
// not exist, so we cannot say"; 0 is "it exists and is empty".
func TestSubagentsNullIsDistinctFromZero(t *testing.T) {
	zero := 0
	for name, val := range map[string]*int{"null": nil, "zero": &zero} {
		t.Run(name, func(t *testing.T) {
			s := minimalSnapshot()
			s.Sessions[0].TranscriptCollected = true
			s.Sessions[0].Transcript = &observer.TranscriptFacts{
				Path: "/tmp/x.jsonl", Subagents: val, SubagentsKnown: val != nil,
			}
			back := roundTrip(t, s)
			got := back.Sessions[0].Transcript
			if got == nil {
				t.Fatal("transcript facts lost")
			}
			switch {
			case val == nil && got.Subagents != nil:
				t.Errorf("null became %d", *got.Subagents)
			case val != nil && got.Subagents == nil:
				t.Errorf("%d became null", *val)
			}
		})
	}
}

// TestTranscriptCollectedDistinguishesDidNotLookFromFoundNothing. With
// omitempty banned the field has to serialise something in both cases, so the
// difference needs its own boolean — and that boolean has to survive the round
// trip or the distinction is decorative.
func TestTranscriptCollectedDistinguishesDidNotLookFromFoundNothing(t *testing.T) {
	didNotLook := minimalSnapshot()
	didNotLook.Sessions[0].TranscriptCollected = false
	didNotLook.Sessions[0].Transcript = nil

	lookedFoundNothing := minimalSnapshot()
	lookedFoundNothing.Sessions[0].TranscriptCollected = true
	lookedFoundNothing.Sessions[0].Transcript = nil

	a := roundTrip(t, didNotLook).Sessions[0]
	b := roundTrip(t, lookedFoundNothing).Sessions[0]

	if a.TranscriptCollected {
		t.Error("did-not-look came back as collected")
	}
	if !b.TranscriptCollected {
		t.Error("looked-and-found-nothing came back as not collected")
	}
	if a.Transcript != nil || b.Transcript != nil {
		t.Error("a nil transcript came back non-nil")
	}
}

// transcriptWithContext is one session whose transcript carries all three
// context facts at non-zero values, so a dropped copy cannot hide as a zero.
func transcriptWithContext() *observer.Snapshot {
	s := minimalSnapshot()
	used := int64(132_000)
	kind := "bg"
	n := 2
	s.Sessions[0].TranscriptCollected = true
	s.Sessions[0].Transcript = &observer.TranscriptFacts{
		Path: "/tmp/x.jsonl", SizeBytes: 4096, TailBytes: 4096,
		AgentColor: "green", ColorFound: true, APIErrors: 1,
		Subagents: &n, SubagentsKnown: true, SessionKind: &kind,
		ContextTokens: &used, Model: "claude-opus-5-5", Compactions: 3,
	}
	return s
}

// TestTheContextFactsRoundTrip: contextTokens, model and compactions survive
// domain -> JSON -> domain, and a null contextTokens stays null rather than
// becoming a measured 0 (which the panel would paint as a quiet green badge).
func TestTheContextFactsRoundTrip(t *testing.T) {
	s := transcriptWithContext()
	got := roundTrip(t, s).Sessions[0].Transcript
	if got == nil {
		t.Fatal("transcript facts lost")
	}
	if !reflect.DeepEqual(got, s.Sessions[0].Transcript) {
		t.Errorf("round trip is not the identity\n want %+v\n  got %+v", s.Sessions[0].Transcript, got)
	}
	if got.ContextTokens == s.Sessions[0].Transcript.ContextTokens {
		t.Error("contextTokens shares the domain pointer; the codec must copy it")
	}

	s.Sessions[0].Transcript.ContextTokens = nil
	data, err := json.Marshal(NewSnapshot(s, s.ObservedAt, opts()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"contextTokens":null`) {
		t.Errorf("a nil contextTokens did not encode as null: %s", data)
	}
	if back := roundTrip(t, s).Sessions[0].Transcript; back.ContextTokens != nil {
		t.Errorf("null became %d", *back.ContextTokens)
	}
}

// legacyTranscript is wire.Transcript as it was BEFORE the context facts: the
// shape a one-build-older sidebar decodes into.
type legacyTranscript struct {
	Path           string  `json:"path"`
	SizeBytes      int64   `json:"sizeBytes"`
	TailBytes      int     `json:"tailBytes"`
	Truncated      bool    `json:"truncated"`
	AgentColor     string  `json:"agentColor"`
	ColorFound     bool    `json:"colorFound"`
	APIErrors      int     `json:"apiErrors"`
	Subagents      *int    `json:"subagents"`
	SubagentsKnown bool    `json:"subagentsKnown"`
	SessionKind    *string `json:"sessionKind"`
}

// TestOldAndNewBuildsReadEachOthersFrames is #31's two-version test. The three
// fields are ADDITIVE, so Schema stays 1; this pins what that has to mean when
// a new `tt observe` feeds an old sidebar and the other way round.
func TestOldAndNewBuildsReadEachOthersFrames(t *testing.T) {
	s := transcriptWithContext()
	newFrame, err := json.Marshal(NewSnapshot(s, s.ObservedAt, opts()))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("an old decoder reading a new frame", func(t *testing.T) {
		// The old struct simply has no home for the three keys: encoding/json
		// ignores them and every field it does know arrives intact.
		var old struct {
			Schema   int `json:"schema"`
			Sessions []struct {
				Transcript *legacyTranscript `json:"transcript"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(newFrame, &old); err != nil {
			t.Fatalf("an old decoder refused a new frame: %v", err)
		}
		if old.Schema != Schema || Schema != 1 {
			t.Errorf("schema = %d (const %d); an additive change must not bump it", old.Schema, Schema)
		}
		tr := old.Sessions[0].Transcript
		if tr == nil || tr.AgentColor != "green" || tr.APIErrors != 1 ||
			tr.Subagents == nil || *tr.Subagents != 2 || tr.SessionKind == nil {
			t.Errorf("the fields an old build knows did not survive: %+v", tr)
		}
		// And the old build's unknown-field detector raises nothing: it stops
		// at sessions[], so keys inside transcript are never named. Proven with
		// THIS build's detector and a key it does not know, in the same place.
		var env map[string]any
		if err := json.Unmarshal(newFrame, &env); err != nil {
			t.Fatal(err)
		}
		sess := env["sessions"].([]any)[0].(map[string]any)
		sess["transcript"].(map[string]any)["aFutureTranscriptKey"] = 7
		future, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		_, notes, err := DecodeSnapshot(future)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(notes) != 0 {
			t.Errorf("a key added inside transcript raised advisories %v; an old sidebar "+
				"would show a wire-unknown-field warning for this upgrade", notes)
		}
	})

	t.Run("a new decoder reading an old frame", func(t *testing.T) {
		var env map[string]any
		if err := json.Unmarshal(newFrame, &env); err != nil {
			t.Fatal(err)
		}
		tr := env["sessions"].([]any)[0].(map[string]any)["transcript"].(map[string]any)
		for _, k := range []string{"contextTokens", "model", "compactions"} {
			if _, ok := tr[k]; !ok {
				t.Fatalf("the new frame has no %q key; this subtest would prove nothing", k)
			}
			delete(tr, k)
		}
		oldFrame, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		decoded, notes, err := DecodeSnapshot(oldFrame)
		if err != nil {
			t.Fatalf("a new decoder refused an old frame: %v", err)
		}
		if len(notes) != 0 {
			t.Errorf("an old frame raised advisories: %v", notes)
		}
		back, _ := ToDomain(decoded)
		got := back.Sessions[0].Transcript
		if got == nil {
			t.Fatal("transcript facts lost")
		}
		if got.ContextTokens != nil || got.Model != "" || got.Compactions != 0 {
			t.Errorf("an old frame decoded to contextTokens=%v model=%q compactions=%d; "+
				"want nil, \"\" and 0 so the panel renders —", got.ContextTokens, got.Model, got.Compactions)
		}
		if got.AgentColor != "green" || got.Subagents == nil || *got.Subagents != 2 {
			t.Errorf("the old fields did not survive: %+v", got)
		}
	})
}

// TestUnknownStatusPreservedVerbatim: an unrecognised status maps to "unknown"
// with the literal kept in rawStatus, and BOTH survive the round trip. This is
// "degrade silently on unknown data, keep the raw value" made testable — the
// vendor's formats are undocumented and will change, and the first sign of a
// change is a status string we have never seen.
func TestUnknownStatusPreservedVerbatim(t *testing.T) {
	for _, raw := range []string{
		"compacting", "rate-limited", "", "BUSY", "waiting-for-mcp", "状態",
	} {
		t.Run("raw="+raw, func(t *testing.T) {
			mapped, reason := observer.MapRegistryStatus(raw, "")
			if raw != "" && mapped != observer.StatusUnknown {
				t.Fatalf("expected %q to map to unknown, got %q", raw, mapped)
			}
			if reason != observer.WaitReasonNone {
				t.Errorf("an unknown status must carry no wait reason, got %q", reason)
			}

			s := minimalSnapshot()
			s.Sessions[0].Status = mapped
			s.Sessions[0].RawStatus = raw
			s.Sessions[0].StatusSource = observer.SourceRegistry

			back := roundTrip(t, s).Sessions[0]
			if back.Status != mapped {
				t.Errorf("status %q -> %q", mapped, back.Status)
			}
			if back.RawStatus != raw {
				t.Errorf("rawStatus %q -> %q: the raw value must survive verbatim", raw, back.RawStatus)
			}
		})
	}
}

// TestUnknownStatusStringOnTheWireIsNotRewritten: a status value this build
// does not know, arriving FROM the wire, passes through unchanged. Status is a
// pass-through string, not a closed enum, so a newer server's vocabulary must
// not be silently flattened by an older client.
func TestUnknownStatusStringOnTheWireIsNotRewritten(t *testing.T) {
	raw := `{"schema":1,"tt":"v0.4.0","observedAt":"2026-09-18T02:40:11.204Z",
	  "counts":{"total":1,"live":1,"dead":0,"unknown":0,"attention":0,
	            "byStatus":{},"byKind":{}},
	  "sources":{},"warnings":[],"notCapable":[],
	  "sessions":[{"sessionId":"s1","status":"compacting","rawStatus":"compacting",
	               "waitReason":"","observedAt":"2026-09-18T02:40:11.204Z"}]}`

	decoded, notes, err := DecodeSnapshot([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected unknown-field notes: %v", notes)
	}
	dom, _ := ToDomain(decoded)
	if got := dom.Sessions[0].Status; got != observer.Status("compacting") {
		t.Errorf("status came back as %q; an unrecognised value must pass through verbatim", got)
	}
	if got := dom.Sessions[0].RawStatus; got != "compacting" {
		t.Errorf("rawStatus came back as %q", got)
	}
}

// TestWaitReasonIsNeverNull pins §3.3's one rule: waitReason is a non-pointer
// string, "" for every status other than waiting, "unknown" only when waiting
// with an undetermined reason. It is the single allowlisted exception to the
// nullable-pointer convention, and the document asserted it three contradictory
// ways — so it gets its own test rather than relying on encoder symmetry, which
// would have made a wrong-but-consistent value pass.
func TestWaitReasonIsNeverNull(t *testing.T) {
	cases := []struct {
		status observer.Status
		want   observer.WaitReason
	}{
		{observer.StatusIdle, observer.WaitReasonNone},
		{observer.StatusBusy, observer.WaitReasonNone},
		{observer.StatusShell, observer.WaitReasonNone},
		{observer.StatusBlocked, observer.WaitReasonNone},
		{observer.StatusWorking, observer.WaitReasonNone},
		{observer.StatusDone, observer.WaitReasonNone},
		{observer.StatusFailed, observer.WaitReasonNone},
		{observer.StatusUnknown, observer.WaitReasonNone},
		{observer.StatusWaiting, observer.WaitReasonUnknown},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			s := minimalSnapshot()
			s.Sessions[0].Status = c.status
			s.Sessions[0].WaitReason = c.want

			data, err := json.Marshal(NewSnapshot(s, refNow, opts()))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(data), `"waitReason":null`) {
				t.Errorf("waitReason serialised as null for status %q", c.status)
			}
			if !strings.Contains(string(data), `"waitReason":"`+string(c.want)+`"`) {
				t.Errorf("expected waitReason %q in %s", c.want, data)
			}
			if got := roundTrip(t, s).Sessions[0].WaitReason; got != c.want {
				t.Errorf("waitReason %q -> %q", c.want, got)
			}
		})
	}
}

// TestNilSliceIsDistinctFromEmptySlice. children is null for an interactive
// session and [] for a background one with no children; conflating them would
// report "this agent has no children" where the truth is "children do not
// apply".
func TestNilSliceIsDistinctFromEmptySlice(t *testing.T) {
	nilCase := minimalSnapshot()
	nilCase.Sessions[0].Children = nil

	emptyCase := minimalSnapshot()
	emptyCase.Sessions[0].Children = []string{}

	if got := roundTrip(t, nilCase).Sessions[0].Children; got != nil {
		t.Errorf("nil children came back as %#v", got)
	}
	got := roundTrip(t, emptyCase).Sessions[0].Children
	if got == nil {
		t.Error("empty children came back as nil")
	} else if len(got) != 0 {
		t.Errorf("empty children came back as %#v", got)
	}
}

// TestAdvisoriesRoundTripVerbatimIncludingUnrecognisedCodes. §3.7 promises
// codes may be added in a minor version, so a client one build behind must
// still be able to show them: a decoder that dropped unrecognised codes would
// make every such addition invisible until the client was rebuilt.
func TestAdvisoriesRoundTripVerbatimIncludingUnrecognisedCodes(t *testing.T) {
	ageMs := int64(4120)
	advisories := []observer.Advisory{
		observer.AdvisoryLivenessUnknown("paneAlive", "list-panes failed"),
		observer.AdvisoryJobReaped(refNow.Add(-time.Hour)),
		observer.AdvisoryTempoStateDisagreement("blocked", "idle"),
		observer.AdvisoryBeyondTranscriptRetention(2_745_034_507, 2_592_000_000),
		observer.AdvisoryJobCorroborationStale(&ageMs, 30_000),
		observer.AdvisoryJobCorroborationStale(nil, 30_000),
		observer.AdvisoryBackgroundWorkingStale(refNow.Add(-2*time.Hour), 900_000),
		observer.AdvisoryJobTerminal("failed"),
		observer.AdvisoryBackgroundGateDisagreement("blocked", false),
		// A code from a build newer than ours, with fields we do not know.
		{Code: "some-future-code", Fields: map[string]any{
			"n": int64(7), "who": "vendor", "list": []string{"a", "b"}, "flag": true,
		}},
		// A code with no fields at all.
		{Code: "bare-code"},
	}

	s := minimalSnapshot()
	s.Sessions[0].Advisories = advisories
	s.Warnings = []observer.Advisory{observer.NewAdvisory(observer.AdvSourceStale, map[string]any{
		"source": "agents-cli", "ageMs": int64(31_000), "staleAfterMs": int64(30_000),
	})}

	back := roundTrip(t, s)
	if !reflect.DeepEqual(advisories, back.Sessions[0].Advisories) {
		t.Errorf("session advisories are not verbatim\n want %#v\n  got %#v",
			advisories, back.Sessions[0].Advisories)
	}
	if !reflect.DeepEqual(s.Warnings, back.Warnings) {
		t.Errorf("warnings are not verbatim\n want %#v\n  got %#v", s.Warnings, back.Warnings)
	}
}

// TestAdvisoryEncodesFlat asserts the shape consumers actually read: the code
// and its fields as siblings, not nested under a "fields" key.
func TestAdvisoryEncodesFlat(t *testing.T) {
	a := Advisory{Code: "transcript-absent", Fields: map[string]any{"path": "/tmp/x.jsonl"}}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["code"] != "transcript-absent" || got["path"] != "/tmp/x.jsonl" {
		t.Errorf("expected a flat {code, path} object, got %s", data)
	}
	if _, nested := got["fields"]; nested {
		t.Errorf("advisory fields must be siblings of code, not nested: %s", data)
	}
}

// ---------------------------------------------------------------------------
// omitempty ban, unknown fields, frames
// ---------------------------------------------------------------------------

// TestNoOmitemptyAnywhereInWire. The ban is what makes null distinguishable
// from zero, and it is enforced structurally rather than by review because a
// single omitempty added to a nullable field would silently delete an
// acceptance criterion.
func TestNoOmitemptyAnywhereInWire(t *testing.T) {
	types := []any{
		Session{}, Snapshot{}, Counts{}, Source{}, Watch{},
		Tmux{}, Transcript{}, Frame{}, AttentionSession{}, ToolResult{},
	}
	for _, v := range types {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("json")
			if strings.Contains(tag, "omitempty") {
				t.Errorf("%s.%s carries omitempty (tag %q); omitempty is banned in wire",
					rt.Name(), f.Name, tag)
			}
		}
	}
}

// TestEverySessionFieldIsPresentInTheEncodedForm is the omitempty ban's
// observable consequence: a fully zero session still emits every key, so a
// consumer never has to distinguish "absent" from "null".
func TestEverySessionFieldIsPresentInTheEncodedForm(t *testing.T) {
	data, err := json.Marshal(NewSession(observer.Session{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range observer.AllSessionFields {
		if _, ok := got[f]; !ok {
			t.Errorf("a zero session omitted %q", f)
		}
	}
	if len(got) != len(observer.AllSessionFields) {
		t.Errorf("encoded session has %d keys, AllSessionFields lists %d",
			len(got), len(observer.AllSessionFields))
	}
}

// TestUnknownFieldIsDroppedAndNamed. An added field arrives silently because
// schema is bumped only on breaking changes, so the decoder has to say what it
// threw away — otherwise "the client shows less than the server sent" is
// undiagnosable.
func TestUnknownFieldIsDroppedAndNamed(t *testing.T) {
	raw := `{"schema":1,"tt":"v0.4.0","observedAt":"2026-09-18T02:40:11.204Z",
	  "counts":{"total":0,"live":0,"dead":0,"unknown":0,"attention":0,"byStatus":{},"byKind":{}},
	  "sources":{},"warnings":[],"notCapable":[],"aNewEnvelopeKey":42,
	  "sessions":[{"sessionId":"s1","waitReason":"","observedAt":"2026-09-18T02:40:11.204Z",
	               "aNewSessionKey":"surprise"}]}`

	_, notes, err := DecodeSnapshot([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var named []string
	for _, n := range notes {
		if n.Code != observer.AdvWireUnknownField {
			t.Errorf("unexpected advisory %q", n.Code)
			continue
		}
		f, _ := n.Fields["field"].(string)
		named = append(named, f)
	}
	sort.Strings(named)
	want := []string{"aNewEnvelopeKey", "sessions[].aNewSessionKey"}
	if !reflect.DeepEqual(named, want) {
		t.Errorf("unknown fields named %v, want %v", named, want)
	}
}

// TestApplySessionFrame covers the three session deltas a socket client folds
// into its local snapshot, plus the two refusals.
func TestApplySessionFrame(t *testing.T) {
	base := func() *observer.Snapshot {
		s := minimalSnapshot()
		s.Sessions[0].SessionID = "s1"
		return s
	}

	t.Run("added appends", func(t *testing.T) {
		dst := base()
		w := NewSession(observer.Session{SessionID: "s2", Name: "new one", ObservedAt: refNow})
		if _, err := ApplySessionFrame(dst, Frame{Type: FrameSessionAdded, Session: &w}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(dst.Sessions) != 2 || dst.Sessions[1].SessionID != "s2" {
			t.Fatalf("expected s2 appended, got %d sessions", len(dst.Sessions))
		}
	})

	t.Run("changed replaces wholesale", func(t *testing.T) {
		dst := base()
		updated := dst.Sessions[0]
		updated.Status = observer.StatusBlocked
		updated.Attention = true
		updated.Needs = strPtr("test the ck command")
		w := NewSession(updated)

		// The changed[] array is advisory; the whole session is authoritative,
		// so a client must never need to merge.
		if _, err := ApplySessionFrame(dst, Frame{
			Type: FrameSessionChanged, SessionID: "s1", Session: &w,
			Changed: []string{"status", "attention", "needs"},
		}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(dst.Sessions) != 1 {
			t.Fatalf("changed must replace, not append: %d sessions", len(dst.Sessions))
		}
		got := dst.Sessions[0]
		if got.Status != observer.StatusBlocked || !got.Attention {
			t.Errorf("replacement did not take: %+v", got)
		}
		if got.Needs == nil || *got.Needs != "test the ck command" {
			t.Errorf("needs did not survive the frame")
		}
	})

	t.Run("removed deletes", func(t *testing.T) {
		dst := base()
		if _, err := ApplySessionFrame(dst, Frame{
			Type: FrameSessionRemoved, SessionID: "s1", Reason: observer.RemovedPIDDead,
		}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(dst.Sessions) != 0 {
			t.Fatalf("expected s1 removed, got %d sessions", len(dst.Sessions))
		}
	})

	t.Run("removing an unknown session is not an error", func(t *testing.T) {
		// A filtered subscriber legitimately misses the add that preceded a
		// removal, so this must not be treated as corruption.
		dst := base()
		if _, err := ApplySessionFrame(dst, Frame{
			Type: FrameSessionRemoved, SessionID: "never-seen",
		}); err != nil {
			t.Errorf("apply: %v", err)
		}
		if len(dst.Sessions) != 1 {
			t.Errorf("an unknown removal changed the session set")
		}
	})

	t.Run("a non-delta frame is refused distinguishably", func(t *testing.T) {
		dst := base()
		_, err := ApplySessionFrame(dst, Frame{Type: FrameHello})
		if err == nil {
			t.Fatal("expected an error for a hello frame")
		}
		if !strings.Contains(err.Error(), "not a session delta") {
			t.Errorf("error should identify the frame as not a delta: %v", err)
		}
	})

	t.Run("a delta frame with no session is refused", func(t *testing.T) {
		dst := base()
		if _, err := ApplySessionFrame(dst, Frame{Type: FrameSessionChanged, SessionID: "s1"}); err == nil {
			t.Error("expected an error for a changed frame carrying no session")
		}
	})
}

// TestDecodeFrameRoundTripsASessionDelta ties the frame path to the same codec
// the envelope uses, which is the whole point of frames living in wire.
func TestDecodeFrameRoundTripsASessionDelta(t *testing.T) {
	src := minimalSnapshot().Sessions[0]
	src.Live = boolPtrT(true)
	src.Tmux = &observer.TmuxRef{Raw: "madbench:@51.%107", Session: "madbench", Window: "@51", Pane: "%107"}

	w := NewSession(src)
	data, err := json.Marshal(Frame{
		Type: FrameSessionChanged, Seq: 3, SessionID: src.SessionID,
		Session: &w, Changed: []string{"live"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f, notes, err := DecodeFrame(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("our own frame produced unknown-field notes: %v", notes)
	}
	// The local record carries its kind: identity is (sessionId, kind) since
	// code review round 2 (A14), and every record a real client holds has one.
	dst := &observer.Snapshot{Sessions: []observer.Session{{SessionID: src.SessionID, Kind: src.Kind}}}
	if _, err := ApplySessionFrame(dst, f); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !reflect.DeepEqual(src, dst.Sessions[0]) {
		t.Errorf("a session did not survive the frame path\n want %#v\n  got %#v", src, dst.Sessions[0])
	}
}

// TestNewAttentionProjectsTenFields pins the projection the spec describes as
// eight fields and then illustrates with ten. The example wins: it is what a
// consumer would have been written against.
func TestNewAttentionProjectsTenFields(t *testing.T) {
	blockedFor := int64(2_745_034_507)
	s := NewSession(observer.Session{
		SessionID:    "7a324a6d-27ec-4431-b17b-70ab8ac764c7",
		Name:         "Explain project plan and build decisions",
		Kind:         observer.KindBackground,
		Status:       observer.StatusBlocked,
		WaitReason:   observer.WaitReasonNone,
		BlockedForMs: &blockedFor,
		Needs:        strPtr("open new terminal and test `ck` command"),
		Live:         nil,
		CWD:          "/home/user/dotfiles",
		ObservedAt:   refNow,
	})
	a := NewAttention(s)

	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{
		"sessionId", "name", "kind", "status", "waitReason",
		"blockedForMs", "needs", "live", "tmux", "cwd",
	}
	if len(got) != len(want) {
		t.Errorf("projection has %d fields, want %d: %s", len(got), len(want), data)
	}
	for _, f := range want {
		if _, ok := got[f]; !ok {
			t.Errorf("projection omits %q", f)
		}
	}
	// live must be an explicit null, not absent: an agent this list names has
	// to say whether we can vouch for it.
	if !strings.Contains(string(data), `"live":null`) {
		t.Errorf("expected an explicit null live, got %s", data)
	}
	if a.WaitReason != "" {
		t.Errorf("a blocked agent's waitReason must be \"\", got %q", a.WaitReason)
	}
}

// TestSchemaAndProtocolAreIndependent guards against the two version spaces
// being conflated, which is the mistake that would make a cosmetic framing
// change look like a breaking payload change.
func TestSchemaAndProtocolAreIndependent(t *testing.T) {
	if Schema != 1 || Protocol != 1 {
		t.Fatalf("unexpected versions: schema=%d protocol=%d", Schema, Protocol)
	}
	s := NewSnapshot(minimalSnapshot(), refNow, opts())
	if s.Schema != Schema {
		t.Errorf("envelope schema %d, want %d", s.Schema, Schema)
	}
	if s.TT != "v0.4.0" {
		t.Errorf("envelope tt %q; the version must come from Options, never a const", s.TT)
	}
}

// TestNewSnapshotDoesNotOverwriteSessionObservedAt. A session's observedAt is
// when THAT session was observed. Replacing it with encode time would make the
// field a lie in exactly the case it matters — a snapshot re-encoded by an
// attached client — which is also how AC11's byte-identity assertion could pass
// on a wrong value.
func TestNewSnapshotDoesNotOverwriteSessionObservedAt(t *testing.T) {
	s := minimalSnapshot()
	observedEarlier := refNow.Add(-90 * time.Second)
	s.Sessions[0].ObservedAt = observedEarlier

	w := NewSnapshot(s, refNow, opts())
	if w.ObservedAt != observer.FormatTime(refNow) {
		t.Errorf("envelope observedAt %q, want the encoder's now", w.ObservedAt)
	}
	if w.Sessions[0].ObservedAt != observer.FormatTime(observedEarlier) {
		t.Errorf("session observedAt was rewritten to %q", w.Sessions[0].ObservedAt)
	}
}

// TestCountsAlwaysCarryEveryKnownKey: the omitempty ban covers struct tags, and
// a map needs its own rule or the first implementation's incidental behaviour
// becomes the contract.
func TestCountsAlwaysCarryEveryKnownKey(t *testing.T) {
	s := &observer.Snapshot{ObservedAt: refNow, Counts: observer.Counts{}}
	w := NewSnapshot(s, refNow, opts())
	if len(w.Counts.ByStatus) != len(observer.AllStatuses) {
		t.Errorf("byStatus has %d keys, want %d", len(w.Counts.ByStatus), len(observer.AllStatuses))
	}
	for _, st := range observer.AllStatuses {
		if _, ok := w.Counts.ByStatus[string(st)]; !ok {
			t.Errorf("byStatus omits %q", st)
		}
	}
	for _, k := range observer.AllKinds {
		if _, ok := w.Counts.ByKind[string(k)]; !ok {
			t.Errorf("byKind omits %q", k)
		}
	}
}

// TestNilSnapshotEncodesAnEmptyEnvelope. "No Claude running" is a fact, not an
// error, and an adapter handed nothing must still emit a well-formed envelope
// rather than panic.
func TestNilSnapshotEncodesAnEmptyEnvelope(t *testing.T) {
	w := NewSnapshot(nil, refNow, opts())
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"schema":1`) {
		t.Errorf("expected a well-formed envelope, got %s", data)
	}
	if w.Counts.Total != 0 || len(w.Counts.ByStatus) != len(observer.AllStatuses) {
		t.Errorf("expected zeroed counts with every key present")
	}
}

// TestTimestampsAreRFC3339MillisUTC pins the one timestamp representation, in
// the one place that produces it.
func TestTimestampsAreRFC3339MillisUTC(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	local := time.Date(2026, 9, 18, 12, 40, 11, 204_000_000, loc)

	s := minimalSnapshot()
	s.Sessions[0].StatusSince = &local

	w := NewSnapshot(s, refNow, opts())
	got := *w.Sessions[0].StatusSince
	if got != "2026-09-18T02:40:11.204Z" {
		t.Errorf("statusSince rendered as %q, want the UTC millisecond form", got)
	}
}

// ---------------------------------------------------------------------------
// generators and helpers
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }
func boolPtrT(b bool) *bool   { return &b }

// minimalSnapshot is one session with every list field initialised to empty —
// the convention the core follows so the JSON reads "[]" rather than "null" for
// fields where empty is the natural zero.
func minimalSnapshot() *observer.Snapshot {
	return &observer.Snapshot{
		ObservedAt: refNow,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Warnings:   []observer.Advisory{},
		NotCapable: []string{},
		Sessions: []observer.Session{{
			SessionID:   "f350c44d-d22a-4c47-b3ef-00021713db7f",
			Kind:        observer.KindInteractive,
			Name:        "attacemtns-slangs",
			NameSource:  "user",
			FormerNames: []string{},
			CWD:         "/home/user/mag/madbench",
			Status:      observer.StatusIdle,
			RawStatus:   "idle",
			WaitReason:  observer.WaitReasonNone,
			ListedBy:    []string{"registry"},
			Sources:     []string{"registry"},
			ObservedAt:  refNow,
			Advisories:  []observer.Advisory{},
		}},
	}
}

// generateSnapshot builds a randomised but structurally valid snapshot. It
// exercises every tri-state at each of its three values and an
// empty/zero/null variant of every nullable field, which is what makes the
// round-trip property a property rather than a handful of examples.
func generateSnapshot(rng *rand.Rand) *observer.Snapshot {
	now := refNow.Add(time.Duration(rng.Intn(1_000_000)) * time.Millisecond)
	n := rng.Intn(4)

	snap := &observer.Snapshot{
		ObservedAt: observer.TruncateTime(now),
		Counts:     observer.NewCounts(),
	}
	if rng.Intn(4) > 0 {
		snap.Sources = map[string]observer.SourceReport{
			"registry": generateSource(rng, now, "registry", "authoritative"),
			"jobs":     generateSource(rng, now, "jobs", "authoritative"),
		}
	}
	if rng.Intn(3) > 0 {
		snap.Warnings = generateAdvisories(rng, now)
	}
	if rng.Intn(3) > 0 {
		snap.NotCapable = []string{"time-transitions"}
	}
	if n > 0 {
		snap.Sessions = make([]observer.Session, 0, n)
		for i := 0; i < n; i++ {
			snap.Sessions = append(snap.Sessions, generateSession(rng, now, i))
		}
	}
	// Counts must be internally consistent, and the invariant
	// total == live + dead + unknown is asserted here as well as in the core.
	for _, s := range snap.Sessions {
		snap.Counts.Total++
		switch {
		case s.Live == nil:
			snap.Counts.Unknown++
		case *s.Live:
			snap.Counts.Live++
		default:
			snap.Counts.Dead++
		}
		if s.Attention && (s.Live == nil || *s.Live) {
			snap.Counts.Attention++
		}
		snap.Counts.ByStatus[s.Status]++
		snap.Counts.ByKind[s.Kind]++
	}
	return snap
}

func generateSession(rng *rand.Rand, now time.Time, i int) observer.Session {
	background := rng.Intn(2) == 0
	statuses := []observer.Status{
		observer.StatusIdle, observer.StatusBusy, observer.StatusShell,
		observer.StatusWaiting, observer.StatusUnknown,
	}
	if background {
		statuses = []observer.Status{
			observer.StatusBlocked, observer.StatusWorking,
			observer.StatusDone, observer.StatusFailed, observer.StatusUnknown,
		}
	}
	status := statuses[rng.Intn(len(statuses))]

	waitReason := observer.WaitReasonNone
	if status == observer.StatusWaiting {
		waitReason = []observer.WaitReason{
			observer.WaitReasonInputNeeded,
			observer.WaitReasonPermissionPrompt,
			observer.WaitReasonUnknown,
		}[rng.Intn(3)]
	}

	s := observer.Session{
		SessionID:    "session-" + string(rune('a'+i)),
		Kind:         observer.KindInteractive,
		Name:         "gen",
		NameSource:   []string{"user", "auto", ""}[rng.Intn(3)],
		FormerNames:  maybeStrings(rng, []string{"old-name"}),
		CWD:          "/home/user/dotfiles",
		StartedAt:    maybeTime(rng, now.Add(-time.Duration(rng.Intn(1_000_000))*time.Second)),
		Status:       status,
		RawStatus:    string(status),
		StatusSource: observer.SourceRegistry,
		StatusSince:  maybeTime(rng, now.Add(-time.Duration(rng.Intn(100_000))*time.Millisecond)),
		WaitReason:   waitReason,
		ListedBy:     maybeStrings(rng, []string{"registry", "agents-cli"}),
		Sources:      maybeStrings(rng, []string{"registry"}),
		ObservedAt:   observer.TruncateTime(now),
		LiveReason:   observer.AllLiveReasons[rng.Intn(len(observer.AllLiveReasons))],
		Attention:    status.IsAttention(),
	}
	if background {
		s.Kind = observer.KindBackground
		s.StatusSource = observer.SourceJobs
		s.ShortID = maybeString(rng, "7a324a6d")
		s.Needs = maybeString(rng, "test the ck command")
		s.Detail = maybeString(rng, "awaiting terminal test")
		s.Tempo = maybeString(rng, []string{"idle", "blocked", "working"}[rng.Intn(3)])
		s.Tokens = maybeInt64(rng, int64(rng.Intn(100_000)))
		s.Children = maybeStrings(rng, []string{"child-1"})
		s.ResumeSessionID = maybeString(rng, "7a324a6d-27ec")
		s.RecordUpdatedAt = maybeTime(rng, now.Add(-time.Duration(rng.Intn(100_000))*time.Second))
		s.RecordAgeMs = maybeInt64(rng, int64(rng.Intn(3_000_000_000)))
		s.ReapedMidWorkAt = maybeTime(rng, now.Add(-time.Hour))
	} else {
		s.PID = maybeInt(rng, 70000+rng.Intn(20000))
		s.ClaudeVersion = "2.1.275"
		s.MessagingSocketPath = maybeString(rng, "/tmp/cc-socks/78761.sock")
		s.SpawnedBy = maybeString(rng, "f6bfe249-30b3-4c1e-9a43-0d1b3c5e7f21")
		if rng.Intn(3) > 0 {
			s.Tmux = &observer.TmuxRef{
				Raw: "madbench:@51.%107", Session: "madbench", Window: "@51", Pane: "%107",
			}
		}
	}
	if rng.Intn(3) == 0 {
		s.RawKind = maybeString(rng, "something-new")
		s.Kind = observer.KindUnknown
	}
	s.StatusAgeMs = observer.AgeMs(now, s.StatusSince)
	if s.Attention {
		s.BlockedForMs = s.StatusAgeMs
		if s.StatusAgeMs == nil {
			s.BlockedForUnavailable = maybeString(rng, "no status timestamp")
		}
	}
	s.StatusSinceLowerBound = maybeTime(rng, now.Add(-time.Duration(rng.Intn(500_000))*time.Second))
	s.LastActivityAt = maybeTime(rng, now.Add(-time.Duration(rng.Intn(500_000))*time.Second))
	s.RawWaitingFor = maybeString(rng, "input needed")
	s.ProcAlive = maybeBool(rng)
	s.PaneAlive = maybeBool(rng)
	s.Live = maybeBool(rng)
	s.Stale = rng.Intn(2) == 0

	switch rng.Intn(3) {
	case 0:
		s.TranscriptCollected = false
	case 1:
		s.TranscriptCollected = true
	case 2:
		s.TranscriptCollected = true
		s.Transcript = &observer.TranscriptFacts{
			Path:           "/home/user/.claude/projects/-home-user-dotfiles/x.jsonl",
			SizeBytes:      int64(rng.Intn(10_000_000)),
			TailBytes:      204800,
			Truncated:      rng.Intn(2) == 0,
			AgentColor:     []string{"", "green", "blue"}[rng.Intn(3)],
			ColorFound:     rng.Intn(2) == 0,
			APIErrors:      rng.Intn(3),
			Subagents:      maybeInt(rng, rng.Intn(8)),
			SubagentsKnown: rng.Intn(2) == 0,
			SessionKind:    maybeString(rng, "bg"),
			ContextTokens:  maybeInt64(rng, int64(1+rng.Intn(900_000))),
			Model:          []string{"", "claude-opus-5-5", "gpt-6-astra"}[rng.Intn(3)],
			Compactions:    rng.Intn(3),
		}
	}
	if rng.Intn(2) == 0 {
		s.Advisories = generateAdvisories(rng, now)
	}
	return s
}

func generateSource(rng *rand.Rand, now time.Time, id, role string) observer.SourceReport {
	src := observer.SourceReport{
		ID:           id,
		Role:         role,
		Path:         "/home/user/.claude/" + id,
		ObservedAt:   maybeTime(rng, now),
		AgeMs:        maybeInt64(rng, int64(rng.Intn(60_000))),
		OK:           rng.Intn(2) == 0,
		Stale:        rng.Intn(2) == 0,
		StaleAfterMs: maybeInt64(rng, 30_000),
		LastError:    maybeString(rng, "read: permission denied"),
	}
	if rng.Intn(3) > 0 {
		src.Counters = map[string]int64{"records": int64(rng.Intn(50)), "parseErrors": 0}
	}
	if rng.Intn(3) > 0 {
		src.Labels = map[string]string{"command": "claude agents --json"}
	}
	if rng.Intn(2) == 0 {
		src.Advisories = generateAdvisories(rng, now)
	}
	if rng.Intn(2) == 0 {
		src.Watch = &observer.WatchReport{
			Mode:          []string{"kqueue", "on-demand", "nowatch", "via-socket"}[rng.Intn(4)],
			ArmedDirs:     rng.Intn(20),
			ArmedFiles:    rng.Intn(40),
			ArmIterations: 1 + rng.Intn(3),
			Degraded:      maybeString(rng, "file-watch-cap"),
			LastEventAt:   maybeTime(rng, now.Add(-time.Minute)),
			Events:        int64(rng.Intn(100)),
		}
	}
	return src
}

func generateAdvisories(rng *rand.Rand, now time.Time) []observer.Advisory {
	n := rng.Intn(3)
	out := make([]observer.Advisory, 0, n)
	ageMs := int64(rng.Intn(60_000))
	pool := []observer.Advisory{
		observer.AdvisoryLivenessUnknown("procAlive", "kill: no such process"),
		observer.AdvisoryJobReaped(now.Add(-time.Hour)),
		observer.AdvisoryTempoStateDisagreement("blocked", "idle"),
		observer.AdvisoryBeyondTranscriptRetention(2_745_034_507, 2_592_000_000),
		observer.AdvisoryJobCorroborationStale(&ageMs, 30_000),
		observer.AdvisoryJobCorroborationStale(nil, 30_000),
		observer.AdvisoryBackgroundWorkingStale(now.Add(-2*time.Hour), 900_000),
		observer.AdvisoryJobTerminal("done"),
		observer.AdvisoryBackgroundGateDisagreement("working", false),
		observer.NewAdvisory(observer.AdvTranscriptAmbiguous, map[string]any{
			"matches": []string{"/a.jsonl", "/b.jsonl"},
		}),
		observer.NewAdvisory(observer.AdvStatusConflict, map[string]any{
			"sources": []string{"registry", "agents-cli"}, "rawValues": []string{"busy", "idle"},
		}),
		{Code: "a-code-from-the-future", Fields: map[string]any{"n": int64(3)}},
		{Code: observer.AdvUnparseable},
	}
	for i := 0; i < n; i++ {
		out = append(out, pool[rng.Intn(len(pool))])
	}
	return out
}

func maybeString(rng *rand.Rand, v string) *string {
	switch rng.Intn(3) {
	case 0:
		return nil
	case 1:
		empty := ""
		return &empty
	default:
		return &v
	}
}

func maybeInt(rng *rand.Rand, v int) *int {
	switch rng.Intn(3) {
	case 0:
		return nil
	case 1:
		zero := 0
		return &zero
	default:
		return &v
	}
}

func maybeInt64(rng *rand.Rand, v int64) *int64 {
	switch rng.Intn(3) {
	case 0:
		return nil
	case 1:
		zero := int64(0)
		return &zero
	default:
		return &v
	}
}

func maybeBool(rng *rand.Rand) *bool {
	switch rng.Intn(3) {
	case 0:
		return nil
	case 1:
		f := false
		return &f
	default:
		t := true
		return &t
	}
}

func maybeTime(rng *rand.Rand, v time.Time) *time.Time {
	if rng.Intn(3) == 0 {
		return nil
	}
	t := observer.TruncateTime(v)
	return &t
}

// maybeStrings covers all three states a list field can be in: absent (nil),
// present but empty, and populated.
func maybeStrings(rng *rand.Rand, v []string) []string {
	switch rng.Intn(3) {
	case 0:
		return nil
	case 1:
		return []string{}
	default:
		out := make([]string, len(v))
		copy(out, v)
		return out
	}
}

// domainFieldAccessors maps each wire field name to a reader of the DOMAIN
// value behind it. Written out by hand on purpose: a reflection-driven version
// would read the same struct the encoder reads and could therefore agree with
// a bug in it, whereas this names the domain field the wire name is supposed to
// mean.
func domainFieldAccessors() map[string]func(observer.Session) any {
	return map[string]func(observer.Session) any{
		"status":                func(s observer.Session) any { return s.Status },
		"rawStatus":             func(s observer.Session) any { return s.RawStatus },
		"statusSource":          func(s observer.Session) any { return s.StatusSource },
		"statusSince":           func(s observer.Session) any { return derefTime(s.StatusSince) },
		"statusSinceLowerBound": func(s observer.Session) any { return derefTime(s.StatusSinceLowerBound) },
		"lastActivityAt":        func(s observer.Session) any { return derefTime(s.LastActivityAt) },
		"waitReason":            func(s observer.Session) any { return s.WaitReason },
		"attention":             func(s observer.Session) any { return s.Attention },
		"needs":                 func(s observer.Session) any { return derefString(s.Needs) },
		"detail":                func(s observer.Session) any { return derefString(s.Detail) },
		"tempo":                 func(s observer.Session) any { return derefString(s.Tempo) },
		"tokens":                func(s observer.Session) any { return derefInt64(s.Tokens) },
		"children":              func(s observer.Session) any { return s.Children },
		"resumeSessionId":       func(s observer.Session) any { return derefString(s.ResumeSessionID) },
		"reapedMidWorkAt":       func(s observer.Session) any { return derefTime(s.ReapedMidWorkAt) },
		"name":                  func(s observer.Session) any { return s.Name },
		"nameSource":            func(s observer.Session) any { return s.NameSource },
		"cwd":                   func(s observer.Session) any { return s.CWD },
		"tmux":                  func(s observer.Session) any { return s.Tmux },
		"pid":                   func(s observer.Session) any { return derefInt(s.PID) },
		"kind":                  func(s observer.Session) any { return s.Kind },
		"rawKind":               func(s observer.Session) any { return derefString(s.RawKind) },
		"live":                  func(s observer.Session) any { return derefBool(s.Live) },
		"liveReason":            func(s observer.Session) any { return s.LiveReason },
		"listedBy":              func(s observer.Session) any { return s.ListedBy },
		"procAlive":             func(s observer.Session) any { return derefBool(s.ProcAlive) },
		"paneAlive":             func(s observer.Session) any { return derefBool(s.PaneAlive) },
		"sources":               func(s observer.Session) any { return s.Sources },
		"advisories":            func(s observer.Session) any { return s.Advisories },
		"transcriptCollected":   func(s observer.Session) any { return s.TranscriptCollected },
		"transcript":            func(s observer.Session) any { return s.Transcript },
		"spawnedBy":             func(s observer.Session) any { return derefString(s.SpawnedBy) },
	}
}

// The deref helpers return the pointer's VALUE, or a typed nil marker, so that
// DeepEqual compares the thing rather than two addresses.
type absent struct{}

func derefString(v *string) any {
	if v == nil {
		return absent{}
	}
	return *v
}

func derefInt(v *int) any {
	if v == nil {
		return absent{}
	}
	return *v
}

func derefInt64(v *int64) any {
	if v == nil {
		return absent{}
	}
	return *v
}

func derefBool(v *bool) any {
	if v == nil {
		return absent{}
	}
	return *v
}

func derefTime(v *time.Time) any {
	if v == nil {
		return absent{}
	}
	return v.UTC()
}

// The three cases of a withheld free-text job field, which a consumer acts on
// differently: absent, present-and-blank, present-and-withheld.
func TestWithheldJobTextKeepsPresenceAndLengthAndNothingElse(t *testing.T) {
	if got := WithheldJobText(nil); got != nil {
		t.Errorf("a nil field became %q; absent must stay absent, or every session looks "+
			"like it needs something", *got)
	}
	empty := ""
	if got := WithheldJobText(&empty); got == nil || *got != "" {
		t.Errorf("an empty field became %v, want it left empty", got)
	}
	secret := "open new terminal and test `ck` command"
	got := WithheldJobText(&secret)
	if got == nil {
		t.Fatal("a present field was dropped rather than withheld")
	}
	if strings.Contains(*got, "ck") || strings.Contains(*got, "terminal") {
		t.Errorf("the placeholder %q carries the content", *got)
	}
	if !strings.HasPrefix(*got, JobTextWithheldPrefix) {
		t.Errorf("the placeholder %q is not recognisable as one", *got)
	}
	if !strings.Contains(*got, strconv.Itoa(len(secret))) {
		t.Errorf("the placeholder %q does not carry the length", *got)
	}
	// The input is not mutated: the caller's own copy is still the real value.
	if secret != "open new terminal and test `ck` command" {
		t.Errorf("the source string was mutated to %q", secret)
	}

	// Multi-byte content counts RUNES, so the length is what a reader would
	// count rather than what a byte counter would.
	runes := "日本語のテキスト"
	if got := WithheldJobText(&runes); got == nil || !strings.Contains(*got, "8 chars") {
		t.Errorf("a multi-byte field became %v, want a rune count of 8", got)
	}
}

// RedactJobText walks the whole envelope, because the defect it closes was one
// projection at a time.
func TestRedactJobTextCoversEverySessionInTheEnvelope(t *testing.T) {
	a, b := "first secret", "second secret"
	env := Snapshot{Sessions: []Session{
		{SessionID: "s1", Needs: &a},
		{SessionID: "s2", Detail: &b},
	}}
	env.RedactJobText()
	if *env.Sessions[0].Needs == a {
		t.Error("session 0's needs survived redaction")
	}
	if *env.Sessions[1].Detail == b {
		t.Error("session 1's detail survived redaction")
	}
	// The attention projection reads a wire.Session, so it inherits this.
	if at := NewAttention(env.Sessions[0]); at.Needs == nil || *at.Needs == a {
		t.Errorf("the attention projection re-published the content: %v", at.Needs)
	}

	var nilEnv *Snapshot
	nilEnv.RedactJobText() // must not panic
}
