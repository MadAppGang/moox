package observer_test

// §4.2's T5 invariant, as an audit that is shown to work on THIS file's call
// site rather than on a synthetic snippet.
//
// audit_test.go (P1) already asserts two things: zero findings over the real
// module, and at least one finding over a deliberately violating snippet. Both
// were true before refresh.go existed, and both would stay true if the audit
// silently stopped recognising the shape the corroboration spawn is actually
// written in — `List` called through `o.deps.Agents`, inside a disjunction, on a
// method with a receiver.
//
// So this test takes the real source of refresh.go, removes the staleAfterMs
// comparison from the guard, and asserts the audit REPORTS it. That is the only
// way to know the rule covers the code it was written for.

import (
	"os"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/audit"
)

const (
	refreshFile = "refresh.go"
	// guard is the T5 precondition as written. If this literal stops matching,
	// the test fails loudly rather than passing vacuously — which is the
	// failure mode of every "assert zero findings" audit.
	guard = `if trigger == TriggerExplicit || trigger == TriggerStartup ||
		staleListing(held, now, o.policy.StaleAfterMs) {`
	guardWithoutThePrecondition = `if trigger == TriggerExplicit || trigger == TriggerStartup || true {`
)

func TestTheT5AuditActuallyCoversTheCorroborationSpawn(t *testing.T) {
	src, err := os.ReadFile(refreshFile)
	if err != nil {
		t.Fatalf("reading %s: %v", refreshFile, err)
	}
	if !strings.Contains(string(src), guard) {
		t.Fatalf("%s no longer contains the T5 guard as written; this test can no "+
			"longer prove the audit covers it. Update `guard` deliberately, having "+
			"checked the new shape is still dominated by a staleAfterMs comparison.", refreshFile)
	}

	asWritten, err := audit.LoadSource("internal/observer/"+refreshFile, string(src))
	if err != nil {
		t.Fatalf("parsing %s: %v", refreshFile, err)
	}
	if f := audit.AgentsSpawnGuard([]audit.File{asWritten}); len(f) != 0 {
		t.Fatalf("the spawn as written is reported as unguarded:\n%s", audit.Format(f))
	}

	broken := strings.Replace(string(src), guard, guardWithoutThePrecondition, 1)
	brokenFile, err := audit.LoadSource("internal/observer/"+refreshFile, broken)
	if err != nil {
		t.Fatalf("parsing the modified source: %v", err)
	}
	found := audit.AgentsSpawnGuard([]audit.File{brokenFile})
	if len(found) != 1 {
		t.Fatalf("removing the staleAfterMs precondition produced %d findings, want 1: "+
			"the audit is not looking at this call site", len(found))
	}
	if !strings.Contains(found[0].Detail, "staleAfterMs") {
		t.Fatalf("finding = %s, want it to name the missing precondition", found[0])
	}
}

func TestOnlyOneCallSiteTakesACorroborationListing(t *testing.T) {
	// Single-flight, the timeout, the backoff and the breaker are all enforced
	// at one place. A second List call anywhere in the core would bypass every
	// one of them, and it would pass the T5 audit as long as it happened to sit
	// under a staleAfterMs comparison — so the count is its own assertion.
	root, err := audit.ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	files, err := audit.LoadModule(root)
	if err != nil {
		t.Fatal(err)
	}

	var sites, allowlisted int
	for _, f := range audit.NonTest(files) {
		if strings.Contains(f.Dir, "adapters/agentscli") {
			// The adapter IS the thing being called.
			continue
		}
		// P10's doctor is the one deliberate second caller, and it is counted
		// separately rather than ignored: the exemption is §9.5's and it is
		// declared in audit.AgentsSpawnAllowlist with its citation, so this
		// test asserts the exemption EXISTS and is used exactly once rather
		// than quietly tolerating any number of extra call sites.
		exempt := false
		for _, e := range audit.AgentsSpawnAllowlist {
			if f.Dir == e.Dir {
				exempt = true
			}
		}
		for _, line := range strings.Split(sourceOf(t, root, f.Rel), "\n") {
			if !strings.Contains(line, ".Agents.List(") {
				continue
			}
			if exempt {
				allowlisted++
				continue
			}
			sites++
		}
	}
	if sites != 1 {
		t.Fatalf("%d unexempted call sites take a corroboration listing, want exactly 1 (refresh.go's)", sites)
	}
	if allowlisted != 1 {
		t.Fatalf("%d allowlisted call sites, want exactly 1 (the doctor's, §9.5)", allowlisted)
	}
}

func sourceOf(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(root + "/" + rel)
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}
