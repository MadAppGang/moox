package observer

import "testing"

// P6's finding, kept as a regression test rather than as a log entry.
//
// appendAdvisory de-duplicates by comparing fields, and comparing two interface
// values whose dynamic type is a SLICE is a runtime panic, not a false.
// transcript-ambiguous's `matches` is the only field the advisory registry
// documents as a slice, and P6's glob locator is what raises it — so the crash
// was one session with two ambiguous globs away. Reproduced before the fix:
//
//	panic: runtime error: comparing uncomparable type []string
//	  observer.advisoryEqual(...) join.go:438
//	  observer.appendAdvisory(...) join.go:425
func TestTwoAdvisoriesCarryingSlicesCanBeComparedAtAll(t *testing.T) {
	a := NewAdvisory(AdvTranscriptAmbiguous, map[string]any{"matches": []string{"x"}})
	b := NewAdvisory(AdvTranscriptAmbiguous, map[string]any{"matches": []string{"y"}})
	same := NewAdvisory(AdvTranscriptAmbiguous, map[string]any{"matches": []string{"x"}})

	list := appendAdvisory(nil, a)
	list = appendAdvisory(list, b)
	list = appendAdvisory(list, same)

	if len(list) != 2 {
		t.Fatalf("len = %d, want 2 (a and b kept, the duplicate of a dropped): %+v", len(list), list)
	}
}

func TestScalarAdvisoryFieldsStillCompareTheWayTheyDid(t *testing.T) {
	// reflect.DeepEqual replaced ==, so the scalar cases the design actually
	// uses — int64, string, bool, nil — are re-pinned rather than assumed.
	for _, tc := range []struct {
		name  string
		a, b  map[string]any
		equal bool
	}{
		{"same int64", map[string]any{"deltaMs": int64(5)}, map[string]any{"deltaMs": int64(5)}, true},
		{"different int64", map[string]any{"deltaMs": int64(5)}, map[string]any{"deltaMs": int64(6)}, false},
		{"same string", map[string]any{"path": "/a"}, map[string]any{"path": "/a"}, true},
		{"different string", map[string]any{"path": "/a"}, map[string]any{"path": "/b"}, false},
		{"nil vs value", map[string]any{"agentsAgeMs": nil}, map[string]any{"agentsAgeMs": int64(0)}, false},
		{"both nil", map[string]any{"agentsAgeMs": nil}, map[string]any{"agentsAgeMs": nil}, true},
		{"different key", map[string]any{"a": int64(1)}, map[string]any{"b": int64(1)}, false},
	} {
		got := advisoryEqual(Advisory{Code: "x", Fields: tc.a}, Advisory{Code: "x", Fields: tc.b})
		if got != tc.equal {
			t.Errorf("%s: equal = %v, want %v", tc.name, got, tc.equal)
		}
	}
}
