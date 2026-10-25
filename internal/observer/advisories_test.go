package observer

import (
	"strings"
	"testing"
	"time"
)

// TestAdvisoryRegistryIsComplete asserts the declared codes, the scope map and
// AllAdvisoryCodes are the same set. Advisory codes are a contract consumers
// match on, so a code that exists in one of the three and not the others is a
// silent hole: it would encode fine and never be promoted, or be promoted and
// never be documented.
func TestAdvisoryRegistryIsComplete(t *testing.T) {
	if len(AllAdvisoryCodes) != len(advisoryScopes) {
		t.Errorf("AllAdvisoryCodes has %d entries, advisoryScopes has %d",
			len(AllAdvisoryCodes), len(advisoryScopes))
	}
	seen := map[string]bool{}
	for _, code := range AllAdvisoryCodes {
		if seen[code] {
			t.Errorf("AllAdvisoryCodes lists %q twice", code)
		}
		seen[code] = true
		if _, ok := ScopeOf(code); !ok {
			t.Errorf("%q has no declared scope", code)
		}
	}
	for code := range advisoryScopes {
		if !seen[code] {
			t.Errorf("advisoryScopes declares %q, which AllAdvisoryCodes omits", code)
		}
	}
}

// TestAdvisoryScopes pins which codes are session-scoped and which are
// source-scoped, because the promotion rule depends on it: a source-scoped
// advisory appears in sources.<id>.advisories AND once in the top-level
// warnings array, while a session-scoped one is NEVER promoted. Getting this
// backwards would either bury an infrastructure failure or spam the warnings
// array with per-session noise.
func TestAdvisoryScopes(t *testing.T) {
	wantSource := []string{
		AdvSourceStale, AdvSpawnLoopGuard, AdvRefreshRejected,
		AdvAgentsCacheContended, AdvCorroborationWaitExpired,
		AdvFileWatchCap, AdvWatchTargetMissing,
		AdvWatchArmUnconverged,
		// ADDED BY P10, deliberately as a contract change: a source that is
		// ok:false with nothing in `warnings` is the P7 live gap, and the fix
		// is a promoted advisory. It carries no clock-derived field, so it is
		// emitted once per outage rather than per rebuild.
		AdvSourceNotOK,
	}
	for _, code := range wantSource {
		scope, ok := ScopeOf(code)
		if !ok {
			t.Fatalf("%q is not registered", code)
		}
		if scope != ScopeSource {
			t.Errorf("%q is session-scoped; it must be source-scoped", code)
		}
	}

	sourceSet := map[string]bool{}
	for _, c := range wantSource {
		sourceSet[c] = true
	}
	for _, code := range AllAdvisoryCodes {
		scope, _ := ScopeOf(code)
		if scope == ScopeSource && !sourceSet[code] {
			t.Errorf("%q is source-scoped but is not in this test's expected list; "+
				"widening the promoted set is a contract change", code)
		}
	}
}

// TestUnrecognisedCodeHasNoScope. ScopeOf must report "not ours" rather than
// defaulting, because the default would be ScopeSession (the zero value) and an
// unknown code would then be quietly treated as a session advisory.
func TestUnrecognisedCodeHasNoScope(t *testing.T) {
	if _, ok := ScopeOf("a-code-from-a-future-build"); ok {
		t.Error("an unregistered code reported a scope")
	}
	if _, ok := ScopeOf(""); ok {
		t.Error("the empty code reported a scope")
	}
}

// TestAdvisoryCodesAreKebabCase is a small consistency guard. Codes are
// matched on by consumers, so a stray camelCase one is a permanent wart.
//
// blockedForUnavailable is the one exception and it is deliberate: it is named
// after the SESSION FIELD it explains, not after an event, and the architecture
// registers it under that spelling.
func TestAdvisoryCodesAreKebabCase(t *testing.T) {
	const knownException = AdvBlockedForUnavailable
	for _, code := range AllAdvisoryCodes {
		if code == knownException {
			continue
		}
		if code != strings.ToLower(code) {
			t.Errorf("%q is not lower-case", code)
		}
		if strings.Contains(code, "_") || strings.Contains(code, " ") {
			t.Errorf("%q should use hyphens", code)
		}
	}
}

// TestNewAdvisoryNormalisesEmptyFields so that an advisory with no fields
// compares equal however it was built. Without this, a constructor passing an
// empty map and a literal passing nil would be two different values with the
// same meaning, and the round-trip identity would fail on one of them.
func TestNewAdvisoryNormalisesEmptyFields(t *testing.T) {
	if a := NewAdvisory(AdvUnparseable, nil); a.Fields != nil {
		t.Errorf("nil fields became %#v", a.Fields)
	}
	if a := NewAdvisory(AdvUnparseable, map[string]any{}); a.Fields != nil {
		t.Errorf("empty fields became %#v", a.Fields)
	}
	a := NewAdvisory(AdvUnparseable, map[string]any{"path": "/x"})
	if a.Fields["path"] != "/x" {
		t.Errorf("fields did not survive: %#v", a.Fields)
	}
}

// TestConstructorsUseInt64ForNumbers. Advisory fields round-trip through JSON
// and the decoder normalises integral numbers to int64, so a constructor
// storing an int or a float64 would survive encoding but not compare equal
// afterwards. This is the kind of bug that shows up as one failing property
// test with an unhelpful diff, so it gets its own assertion at the source.
func TestConstructorsUseInt64ForNumbers(t *testing.T) {
	ageMs := int64(4_120)
	built := []Advisory{
		AdvisoryBeyondTranscriptRetention(2_745_034_507, 2_592_000_000),
		AdvisoryJobCorroborationStale(&ageMs, 30_000),
		AdvisoryBackgroundWorkingStale(refNow, 900_000),
	}
	for _, a := range built {
		for name, v := range a.Fields {
			switch v.(type) {
			case int64, string, bool, []string, nil:
				// fine
			default:
				t.Errorf("%s.%s is %T; advisory numbers must be int64", a.Code, name, v)
			}
		}
	}
}

// TestConstructorsFormatTimesAsStrings: a time.Time in an advisory field would
// marshal to RFC3339Nano and come back as a string, so the constructors format
// it once, in our canonical millisecond-UTC form.
func TestConstructorsFormatTimesAsStrings(t *testing.T) {
	at := time.Date(2026, 8, 17, 8, 9, 36, 697_000_000, time.UTC)
	a := AdvisoryJobReaped(at)
	got, ok := a.Fields["reapedMidWorkAt"].(string)
	if !ok {
		t.Fatalf("reapedMidWorkAt is %T, want string", a.Fields["reapedMidWorkAt"])
	}
	if got != "2026-08-17T08:09:36.697Z" {
		t.Errorf("reapedMidWorkAt = %q", got)
	}
}

// TestJobCorroborationStaleCarriesAnExplicitNullAge. The "no listing was taken
// at all" case must be distinguishable from "a listing was taken 0 ms ago", so
// the field is present and null rather than absent.
func TestJobCorroborationStaleCarriesAnExplicitNullAge(t *testing.T) {
	a := AdvisoryJobCorroborationStale(nil, 30_000)
	v, present := a.Fields["agentsAgeMs"]
	if !present {
		t.Fatal("agentsAgeMs is absent; it must be present and null")
	}
	if v != nil {
		t.Errorf("agentsAgeMs = %v, want nil", v)
	}
}

// TestEveryLivenessAdvisoryCodeIsRegistered ties the policy to the registry:
// the liveness rules are the biggest producer of advisories, and a code emitted
// but unregistered would be a code no consumer could have been told about.
func TestEveryLivenessAdvisoryCodeIsRegistered(t *testing.T) {
	yes, no := true, false
	boolStates := []*bool{nil, &yes, &no}
	p := DefaultPolicy()

	emitted := map[string]bool{}
	for _, kind := range AllKinds {
		for _, status := range AllStatuses {
			for _, tempo := range []string{"", "idle", "blocked", "working"} {
				for _, proc := range boolStates {
					for _, hasRecord := range []bool{true, false} {
						for _, gate := range []bool{true, false} {
							e := Evidence{
								Kind: kind, Status: status, RawState: string(status),
								Tempo: tempo, HasPID: kind == KindInteractive,
								ProcAlive: proc, GateError: gate,
								HasJobRecord: hasRecord,
								RecordAgeMs:  int64Ptr(3_000_000_000),
								ReapedMidWorkAt: func() *time.Time {
									if status == StatusWorking && tempo == "working" {
										return tPtr(refNow.Add(-time.Hour))
									}
									return nil
								}(),
								LastActivityAt: tPtr(refNow.Add(-2 * time.Hour)),
							}
							for _, a := range Decide(e, refNow, p).Advisories {
								emitted[a.Code] = true
							}
						}
					}
				}
			}
		}
	}

	if len(emitted) == 0 {
		t.Fatal("the liveness policy emitted no advisories at all")
	}
	for code := range emitted {
		if _, ok := ScopeOf(code); !ok {
			t.Errorf("the liveness policy emits %q, which is not in the advisory registry", code)
		}
		if scope, _ := ScopeOf(code); scope != ScopeSession {
			t.Errorf("the liveness policy emits %q, which is source-scoped; "+
				"a per-session verdict must not produce a promoted advisory", code)
		}
	}
}
