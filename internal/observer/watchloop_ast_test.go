package observer_test

// The ordering assertion §7.2 asks for, made against the SOURCE rather than
// against behaviour:
//
//	"Reconcile is never called once at the end — revision 2's ordering is what
//	 round 3's CRITICAL 1 was about, and an AST test asserts there is no
//	 Reconcile call after the publish other than the releasing one."
//
// A behavioural test can only fail when a specific interleaving happens to be
// exercised. This one fails on the EDIT: move one Reconcile call below the
// publish, or add a second releasing call, and it goes red with no timing
// involved at all. That matters because the defect it guards is exactly "a
// later refactor moves one Reconcile call".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const watchLoopFile = "watchloop.go"

func parseWatchLoop(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, watchLoopFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", watchLoopFile, err)
	}
	return fset, file
}

// calleesOf returns, for every function in the file, the method/function names
// it calls. Only the selector's final identifier is kept, which is all this
// analysis needs and is stable against the receiver being renamed.
func calleesOf(file *ast.File) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		names := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				names[f.Sel.Name] = true
			case *ast.Ident:
				names[f.Name] = true
			}
			return true
		})
		out[fn.Name.Name] = names
	}
	return out
}

// reaches computes the transitive closure of "this function eventually calls
// target", so a wave helper three calls deep is still recognised as an arm.
func reaches(callees map[string]map[string]bool, from, target string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		for callee := range callees[name] {
			if callee == target {
				return true
			}
			if walk(callee) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

func funcDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// statementCallees is the called names of one statement, flattened.
func statementCallees(stmt ast.Stmt) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(stmt, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			names[f.Sel.Name] = true
		case *ast.Ident:
			names[f.Name] = true
		}
		return true
	})
	return names
}

func TestNoReconcileCallFollowsThePublishExceptTheReleasingOne(t *testing.T) {
	_, file := parseWatchLoop(t)
	callees := calleesOf(file)

	rebuild := funcDecl(file, "Rebuild")
	if rebuild == nil {
		t.Fatal("Rebuild is gone from watchloop.go; the protocol has moved and this test " +
			"must move with it")
	}

	publishAt := -1
	var armAt, releaseAt []int
	for i, stmt := range rebuild.Body.List {
		called := statementCallees(stmt)
		for name := range called {
			switch {
			case name == "publish":
				publishAt = i
			case name == "reconcileRelease" || reaches(callees, name, "reconcileRelease"):
				releaseAt = append(releaseAt, i)
			case name == "reconcileArm" || reaches(callees, name, "reconcileArm"):
				armAt = append(armAt, i)
			case name == "Reconcile":
				t.Errorf("statement %d calls the port's Reconcile directly; every call must "+
					"go through the arm or release helper so the schedule has one home", i)
			}
		}
	}

	if publishAt < 0 {
		t.Fatal("Rebuild never publishes")
	}
	if len(armAt) < 3 {
		t.Errorf("only %d arming statements in Rebuild; §6.3 specifies THREE arm-only "+
			"waves, each immediately before the read it protects", len(armAt))
	}
	for _, i := range armAt {
		if i > publishAt {
			t.Errorf("an ARMING statement (%d) follows the publish (%d). kqueue does not "+
				"replay vnode events that predate registration, so an arm after the read "+
				"loses — permanently, with no later event — anything that materialised in "+
				"between", i, publishAt)
		}
	}
	if len(releaseAt) != 1 {
		t.Fatalf("%d releasing statements, want exactly 1", len(releaseAt))
	}
	if releaseAt[0] <= publishAt {
		t.Errorf("the releasing call (%d) runs at or before the publish (%d). Releases go "+
			"LAST because releasing a target we are about to re-read would reopen the "+
			"window we just closed, and because close(fd) discards every event the kernel "+
			"had queued on it", releaseAt[0], publishAt)
	}
}

func TestTheReadIsBetweenTheLastArmAndThePublish(t *testing.T) {
	_, file := parseWatchLoop(t)
	callees := calleesOf(file)
	rebuild := funcDecl(file, "Rebuild")
	if rebuild == nil {
		t.Fatal("Rebuild is gone")
	}

	readAt, publishAt, lastArm := -1, -1, -1
	for i, stmt := range rebuild.Body.List {
		called := statementCallees(stmt)
		for name := range called {
			switch {
			case name == "Snapshot":
				readAt = i
			case name == "publish":
				publishAt = i
			case name == "reconcileArm" || reaches(callees, name, "reconcileArm"):
				if i > lastArm {
					lastArm = i
				}
			}
		}
	}
	if readAt < 0 || publishAt < 0 || lastArm < 0 {
		t.Fatalf("could not locate the three landmarks: read=%d publish=%d lastArm=%d",
			readAt, publishAt, lastArm)
	}
	if !(lastArm < readAt && readAt < publishAt) {
		t.Errorf("the order is arm=%d read=%d publish=%d; Invariant A requires every arm "+
			"to precede the read", lastArm, readAt, publishAt)
	}
}

func TestThePortsReconcileHasExactlyOneCallSite(t *testing.T) {
	// One call site is what keeps the arm path and the release path from
	// diverging in their bookkeeping — and it is what makes the test above a
	// complete statement rather than a sample.
	_, file := parseWatchLoop(t)
	sites := map[string]int{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Reconcile" {
				return true
			}
			sites[fn.Name.Name]++
			return true
		})
	}
	if len(sites) != 1 {
		t.Fatalf("the port's Reconcile is called from %v; want exactly one function", sites)
	}
	for name, n := range sites {
		if name != "applyReconcile" {
			t.Errorf("the single call site is %q, want applyReconcile", name)
		}
		if n != 1 {
			t.Errorf("%s calls Reconcile %d times", name, n)
		}
	}
}

func TestNoOtherFileInTheCoreCallsTheWatchPort(t *testing.T) {
	// The schedule is only a schedule if nothing else arms. A second caller
	// anywhere in the core would be a second, unsynchronised opinion about what
	// is armed — and the loop's model of the armed set would then be wrong,
	// which is how the fixed point would start lying.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == watchLoopFile {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Reconcile" {
				t.Errorf("%s calls Reconcile; the only caller may be %s", name, watchLoopFile)
			}
			return true
		})
	}
}

func TestTheArmOnlyWavesAreIdentifiableByName(t *testing.T) {
	// The three waves §6.3 names must exist as named functions, because the
	// test above locates them by name and a future refactor that inlined them
	// would make that test silently vacuous.
	_, file := parseWatchLoop(t)
	for _, want := range []string{
		"armSpine",               // wave 0 — the stable spine
		"armJobDirsToFixedPoint", // wave 1 — the fixed point
		"armRegistryFiles",       // wave 1's registry equivalent
		"armRecordNamedTargets",  // wave 3 — the targets the records name
		"reconcileArm",           // the arm-only call
		"reconcileRelease",       // the single releasing call
		"stillDesired",           // whose result makes the release arm nothing
	} {
		if funcDecl(file, want) == nil {
			t.Errorf("%s is gone from watchloop.go", want)
		}
	}
}

// Code review round 2, A4. The releasing call used to reach the port's
// Reconcile — the ARMING operation — which on the kqueue adapter re-arms any
// target whose vnode was replaced. So "release-only" was a property of the
// argument (a subset of what was armed), not of the operation, and a
// replacement vnode was armed after the read that should have been protected
// by it. Nothing reachable from the post-publication phase may call Reconcile.
func TestThePostPublicationPhaseCannotReachTheArmingCall(t *testing.T) {
	_, file := parseWatchLoop(t)
	callees := calleesOf(file)
	if reaches(callees, "reconcileRelease", "Reconcile") {
		t.Fatal("reconcileRelease reaches the port's Reconcile, which ARMS: a replaced vnode " +
			"would be armed after the read. The post-publication phase must use an operation " +
			"that cannot arm anything")
	}
	if !reaches(callees, "reconcileRelease", "Release") {
		t.Fatal("reconcileRelease no longer reaches the port's Release; nothing releases")
	}
}

// ...and the identity check that the release no longer does happens where it
// must: before the read.
func TestVnodeIdentityIsCheckedBeforeTheRead(t *testing.T) {
	_, file := parseWatchLoop(t)
	callees := calleesOf(file)
	rebuild := funcDecl(file, "Rebuild")
	if rebuild == nil {
		t.Fatal("Rebuild is gone")
	}
	verifyAt, readAt := -1, -1
	for i, stmt := range rebuild.Body.List {
		for name := range statementCallees(stmt) {
			switch {
			case name == "Snapshot":
				readAt = i
			case name == "verifyArmedIdentities" || reaches(callees, name, "verifyArmedIdentities"):
				verifyAt = i
			}
		}
	}
	if verifyAt < 0 {
		t.Fatal("Rebuild never checks the armed vnodes' identity")
	}
	if !(verifyAt < readAt) {
		t.Fatalf("the identity check (%d) does not precede the read (%d)", verifyAt, readAt)
	}
	if !reaches(callees, "verifyArmedIdentities", "Reconcile") {
		t.Fatal("verifyArmedIdentities does not reach the port's Reconcile, which is the " +
			"operation that re-arms a replaced vnode")
	}
}
