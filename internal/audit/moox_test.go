package audit_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/audit"
)

// ---------------------------------------------------------------- Imports (§2.3)

func TestImportsFollowTheDependencyTable(t *testing.T) {
	_, files := loadModule(t)
	if f := audit.Imports(files, audit.ModulePath, audit.ImportRules); len(f) != 0 {
		t.Fatalf("a package imports outside its §2.3 row:\n%s", audit.Format(f))
	}
	for _, r := range audit.ImportRules {
		if strings.TrimSpace(r.Citation) == "" {
			t.Errorf("import rule %s has no citation", r.Pkg)
		}
	}
	// Every non-test package in the tree has a row (a new package must be
	// placed in the table deliberately).
	dirs := map[string]bool{}
	for _, f := range audit.NonTest(files) {
		dirs[f.Dir] = true
	}
	if len(dirs) < 20 {
		t.Fatalf("only %d packages seen; the walker is not seeing the module", len(dirs))
	}
}

func TestImportsAuditActuallyDetects(t *testing.T) {
	const m = audit.ModulePath
	cases := []struct {
		rel, src, want string
	}{
		{"internal/observer/x.go", `package observer
import _ "` + m + `/internal/engine"`, "may not import internal/engine"},
		{"internal/tmuxtarget/x.go", `package tmuxtarget
import _ "` + m + `/internal/statedir"`, "may not import internal/statedir"},
		{"cmd/tt/x.go", `package main
import _ "` + m + `/internal/tmuxtest"`, "imported only from _test.go"},
		{"internal/newpkg/x.go", `package newpkg
import _ "` + m + `/internal/sockets"`, "has no row"},
		{"internal/saver/x.go", `package saver
import _ "` + m + `/internal/engine/internal/store"`, "may not import internal/engine/internal/store"},
	}
	for _, c := range cases {
		f := loadSnippet(t, c.rel, c.src)
		got := audit.Format(audit.Imports([]audit.File{f}, m, audit.ImportRules))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: want a finding containing %q, got:\n%s", c.rel, c.want, got)
		}
	}
	// allowed shapes produce nothing
	ok := []audit.File{
		loadSnippet(t, "internal/observer/adapters/x/x.go", `package x
import (
	_ "`+m+`/internal/observer"
	_ "`+m+`/internal/statedir"
	_ "`+m+`/internal/mooxroot"
)`),
		loadSnippet(t, "internal/engine/internal/store/x.go", `package store
import (
	_ "`+m+`/internal/engine/internal/layout"
	_ "`+m+`/internal/engine/snapshot"
)`),
		loadSnippet(t, "cmd/tt/x_test.go", `package main
import _ "`+m+`/internal/tmuxtest"`),
	}
	if f := audit.Imports(ok, m, audit.ImportRules); len(f) != 0 {
		t.Fatalf("allowed imports flagged:\n%s", audit.Format(f))
	}
}

// ------------------------------------------------------- RestrictedCalls (§2.4)

func TestOpenUserConfigIsReferencedOnlyFromSetup(t *testing.T) {
	_, files := loadModule(t)
	if f := audit.RestrictedCalls(files, audit.RestrictedCallList); len(f) != 0 {
		t.Fatalf("a restricted call is referenced outside its allowlist:\n%s", audit.Format(f))
	}
	// statedir's own test references it, so the walker demonstrably sees a site.
	seen := false
	for _, f := range files {
		if f.Dir == "internal/statedir" && f.IsTest {
			src, _ := os.ReadFile(filepath.Join(mustRoot(t), f.Rel))
			if strings.Contains(string(src), "statedir.OpenUserConfig") {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatal("no reference to OpenUserConfig found anywhere; the rule polices nothing")
	}
}

func TestRestrictedCallAuditActuallyDetects(t *testing.T) {
	bad := loadSnippet(t, "cmd/tt/x.go", `package main
import "github.com/MadAppGang/moox/internal/statedir"
var f = statedir.OpenUserConfig
func g() { statedir.OpenUserConfig("/x") }`)
	if f := audit.RestrictedCalls([]audit.File{bad}, audit.RestrictedCallList); len(f) != 2 {
		t.Fatalf("want 2 findings (a value and a call), got:\n%s", audit.Format(f))
	}
	good := loadSnippet(t, "internal/setup/x.go", `package setup
import "github.com/MadAppGang/moox/internal/statedir"
func g() { statedir.OpenUserConfig("/x") }`)
	if f := audit.RestrictedCalls([]audit.File{good}, audit.RestrictedCallList); len(f) != 0 {
		t.Fatalf("setup flagged:\n%s", audit.Format(f))
	}
}

// ----------------------------------------------------------- LiteralArgs (§2.4)

func TestEngineArgvUsesLitBuilders(t *testing.T) {
	_, files := loadModule(t)
	if f := audit.LiteralArgs(files, audit.LiteralArgsDirs); len(f) != 0 {
		t.Fatalf("engine spells a format-expanded flag by hand:\n%s", audit.Format(f))
	}
}

func TestLiteralArgsAuditActuallyDetects(t *testing.T) {
	bad := loadSnippet(t, "internal/engine/internal/restore/x.go", `package restore
func a(title string) []string { return []string{"select-pane", "-T", title, "new-window", "-n", title, "-c", title} }`)
	if f := audit.LiteralArgs([]audit.File{bad}, audit.LiteralArgsDirs); len(f) != 3 {
		t.Fatalf("want 3 findings, got:\n%s", audit.Format(f))
	}
	outside := loadSnippet(t, "cmd/tt/x.go", `package main
var a = []string{"-T", "-n", "-c"}`)
	if f := audit.LiteralArgs([]audit.File{outside}, audit.LiteralArgsDirs); len(f) != 0 {
		t.Fatalf("non-engine code flagged:\n%s", audit.Format(f))
	}
}

// -------------------------------------------------------------- tmuxtest forms

func TestTmuxtestHasExactlyTheTwoGuardedExecForms(t *testing.T) {
	_, files := loadModule(t)
	n := 0
	for _, f := range files {
		if f.Dir == audit.TmuxtestDir && !f.IsTest {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no tmuxtest source scanned")
	}
	if f := audit.TmuxtestExecForms(files); len(f) != 0 {
		t.Fatalf("tmuxtest's exec forms:\n%s", audit.Format(f))
	}
}

func TestTmuxtestFormsAuditActuallyDetects(t *testing.T) {
	good := `package tmuxtest
import "os/exec"
type Server struct{}
func Start() { _ = exec.Command("tmux", "-f", "/dev/null", "-L", name, "new-session") }
func StartCold() { cmd := exec.Command(bin, "-f", h, "-f", c, "-S", s, "new-session"); cmd.Env = nil }
func build() { _ = exec.Command("go", "build") }
func (s *Server) SourceMoox() { s.Run("source-file", p) }`
	if f := audit.TmuxtestExecForms([]audit.File{loadSnippet(t, "internal/tmuxtest/x.go", good)}); len(f) != 0 {
		t.Fatalf("the two good forms flagged:\n%s", audit.Format(f))
	}
	cases := map[string]string{
		"third exec": good + `
func extra() { _ = exec.Command("tmux", "-L", "x") }`,
		"cold without Env": `package tmuxtest
import "os/exec"
func Start() { _ = exec.Command("tmux", "-f", "/dev/null", "-L", name) }
func StartCold() { _ = exec.Command(bin, "-f", h, "-f", c, "-S", s, "new-session") }`,
		"source-file off a Server": good + `
func src(t T) { t.Run("source-file", "~/.tmux.conf") }`,
		"kill-server": good + `
func (s *Server) Kill() { s.Run("kill-server") }`,
		"variable program": good + `
func other() { _ = exec.Command(prog, "-S", "/x") }`,
	}
	for name, src := range cases {
		if f := audit.TmuxtestExecForms([]audit.File{loadSnippet(t, "internal/tmuxtest/x.go", src)}); len(f) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
}

// -------------------------------------------------------------- RunShell (§2.4)

func TestNoGoCodeExecsRunShell(t *testing.T) {
	_, files := loadModule(t)
	if f := audit.RunShellGo(files); len(f) != 0 {
		t.Fatalf("run-shell in Go:\n%s", audit.Format(f))
	}
}

func TestRunShellGoAuditActuallyDetects(t *testing.T) {
	bad := loadSnippet(t, "cmd/tt/x.go", `package main
var a = []string{"run-shell", "x"}
func f() { tg.Run(ctx, "run", "-b", "x"); args = append(args, "run-s", "y"); g("run-shell") }
var conf = "run-shell 'echo hi'"`)
	if f := audit.RunShellGo([]audit.File{bad}); len(f) != 4 {
		t.Fatalf("want 4 findings, got:\n%s", audit.Format(f))
	}
	// A test may name run-shell as data, but never send it.
	tst := loadSnippet(t, "internal/x/x_test.go", `package x
func TestA(t *testing.T) {
	if strings.Contains(conf, "run-shell") { t.Fatal() }
	_ = filepath.Join(root, "run")
	s.Run(t, "run-shell", "x")
	s.RunErr(tmuxtarget.Chain([]string{"run", "-b", "y"})...)
	_ = exec.Command("tmux", "-f", "/dev/null", "-L", "x", "run-shell", "z")
}`)
	f := audit.RunShellGo([]audit.File{tst})
	if len(f) != 3 {
		t.Fatalf("test file: want 3 findings (Run, Chain element, exec), got:\n%s", audit.Format(f))
	}
	ok := loadSnippet(t, "cmd/tt/x.go", `package main
func label() string { return "run" }
var conf = "run-shell \"x >/dev/null 2>&1 || true\""`)
	if f := audit.RunShellGo([]audit.File{ok}); len(f) != 0 {
		t.Fatalf("allowed shapes flagged:\n%s", audit.Format(f))
	}
}

func TestShareConfRunShellsAreSilent(t *testing.T) {
	root := mustRoot(t)
	share := filepath.Join(root, "share", "moox")
	scanned := 0
	err := filepath.WalkDir(share, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !audit.IsConfPath(rel) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		scanned++
		if f := audit.RunShellConf(rel, string(b)); len(f) != 0 {
			t.Errorf("%s:\n%s", rel, audit.Format(f))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no config file under share/moox was scanned")
	}
}

func TestRunShellConfAuditActuallyDetects(t *testing.T) {
	conf := strings.Join([]string{
		`# run-shell 'commented out is fine'`,
		`run-shell '"#{@moox-root}/plugins/p/p.tmux" >/dev/null 2>&1 || true'`,                              // ok
		`run-shell '"#{@moox-root}/plugins/p/p.tmux"'`,                                                      // not silent
		`run 'x >/dev/null 2>&1'`,                                                                           // no || true
		`set-hook -g session-created[105] 'run-shell "x >/dev/null 2>&1 || true"'`,                          // hook without -b
		`set-hook -g session-created[105] 'run-shell -b "x >/dev/null 2>&1 || true"'`,                       // ok
		`if-shell 'command -v tmux-fingers' 'run-shell "tmux-fingers load-config >/dev/null 2>&1 || true"'`, // ok
	}, "\n")
	f := audit.RunShellConf("share/moox/conf/x.conf", conf)
	if len(f) != 3 {
		t.Fatalf("want 3 findings (lines 3, 4, 5), got:\n%s", audit.Format(f))
	}
	for i, want := range []int{3, 4, 5} {
		if f[i].Line != want {
			t.Errorf("finding %d on line %d, want %d", i, f[i].Line, want)
		}
	}
	if !audit.IsConfPath("share/moox/plugins/tmux-yank/yank.tmux") || audit.IsConfPath("share/moox/scripts/theme.sh") {
		t.Fatal("IsConfPath does not select .conf files and plugin entry scripts")
	}
}

// TestRunShellConfReadsLogicalLines: run-shell split over several physical
// lines is judged whole — backslash continuations (tmux-sensible's real
// `prefix R` form, in a plugin entry script and in a .conf) and brace blocks
// (a set-hook's `{` … `}` body is hook text, so its run-shell needs -b).
func TestRunShellConfReadsLogicalLines(t *testing.T) {
	// tmux-sensible's sensible.tmux, verbatim (tabs and all): pinned by its
	// joined text, so the vendored copy passes...
	sensible := "main() {\n" +
		"\tif key_binding_not_set \"R\"; then\n" +
		"\t\tlocal tmux_config=$(get_tmux_config)\n" +
		"\n" +
		"\t\ttmux bind-key R run-shell \" \\\n" +
		"\t\t\ttmux source-file ${tmux_config} > /dev/null; \\\n" +
		"\t\t\ttmux display-message 'Sourced ${tmux_config}!'\"\n" +
		"\tfi\n" +
		"}\n" +
		"main\n"
	if f := audit.RunShellConf("share/moox/plugins/tmux-sensible/sensible.tmux", sensible); len(f) != 0 {
		t.Fatalf("the pinned sensible binding was flagged:\n%s", audit.Format(f))
	}
	// ...while the same text in any other file is one loud run-shell, found
	// on its first line. Reading line by line saw only `run-shell " \` there.
	f := audit.RunShellConf("share/moox/plugins/other/other.tmux", sensible)
	if len(f) != 1 || f[0].Line != 5 || !strings.Contains(f[0].Detail, "display-message 'Sourced") {
		t.Fatalf("want one finding on line 5 carrying the joined command, got:\n%s", audit.Format(f))
	}
	// A continued run-shell whose silent tail sits on the last line is clean
	// (line by line its first line looked loud).
	contOK := "run-shell -b '\"#{@moox-root}/scripts/x.sh\" \\\n    >/dev/null 2>&1 || true'\n"
	if f := audit.RunShellConf("share/moox/conf/x.conf", contOK); len(f) != 0 {
		t.Fatalf("a continued silent run-shell was flagged:\n%s", audit.Format(f))
	}

	conf := strings.Join([]string{
		`set-hook -g client-attached[107] {`,         // 1
		`  run-shell "x >/dev/null 2>&1 || true"`,    // 2: hook text without -b
		`  run-shell -b "y >/dev/null 2>&1 || true"`, // 3: ok
		`}`,            // 4
		`bind-key Q {`, // 5
		`  run-shell "z >/dev/null 2>&1 || true"`, // 6: a binding, -b not needed
		`}`,                                  // 7
		`set-hook -g session-created[108] \`, // 8
		`  'run-shell "w >/dev/null 2>&1 || true"'`,             // 9: continued hook without -b
		`# a comment ending in a backslash continues nothing \`, // 10
		`run-shell 'loud'`, // 11: not silent
	}, "\n")
	f = audit.RunShellConf("share/moox/conf/x.conf", conf)
	var lines []int
	for _, x := range f {
		lines = append(lines, x.Line)
	}
	if len(lines) != 3 || lines[0] != 2 || lines[1] != 8 || lines[2] != 11 {
		t.Fatalf("want findings on lines [2 8 11], got %v:\n%s", lines, audit.Format(f))
	}
	if !strings.Contains(f[0].Detail, "-b") || !strings.Contains(f[1].Detail, "-b") {
		t.Fatalf("hook findings should be the -b rule:\n%s", audit.Format(f))
	}
}

// ------------------------------------------------------ JSONTags wire list

func TestWireListIsTheThreeFormats(t *testing.T) {
	want := []string{"internal/observer/wire", "internal/engine/snapshot", "internal/setup/manifest"}
	if strings.Join(audit.WireDirs, ",") != strings.Join(want, ",") {
		t.Fatalf("WireDirs = %v, want %v", audit.WireDirs, want)
	}
	_, files := loadModule(t)
	if f := audit.JSONTags(audit.NonTest(files), audit.WireDirs...); len(f) != 0 {
		t.Fatalf("json tags outside the wire packages:\n%s", audit.Format(f))
	}
	snap := loadSnippet(t, "internal/engine/snapshot/x.go", "package snapshot\ntype S struct{ A int `json:\"a\"` }")
	other := loadSnippet(t, "internal/engine/internal/store/x.go", "package store\ntype S struct{ A int `json:\"a\"` }")
	if f := audit.JSONTags([]audit.File{snap, other}, audit.WireDirs...); len(f) != 1 || f[0].File != other.Rel {
		t.Fatalf("want exactly the store finding, got:\n%s", audit.Format(f))
	}
}

// ---------------------------------------------------- LoadModule (R2-H11)

func TestAudit_SkipsNestedModules(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module "+audit.ModulePath+"\n")
	write("internal/a/a.go", "package a\n")
	write("third_party/tmux-copy-image/go.mod", "module github.com/MadAppGang/tmux-copy-image\n")
	write("third_party/tmux-copy-image/main.go", "package main\nimport \"os\"\nfunc main(){ os.Remove(\"x\") }\n")
	write("tools/nested/go.mod", "module example.com/nested\n")
	write("tools/nested/n.go", "package nested\n")
	files, err := audit.LoadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Rel != "internal/a/a.go" {
		var got []string
		for _, f := range files {
			got = append(got, f.Rel)
		}
		t.Fatalf("LoadModule entered a nested module: %v", got)
	}
}

func mustRoot(t *testing.T) string {
	t.Helper()
	root, err := audit.ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
