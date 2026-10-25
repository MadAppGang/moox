package audit

// The rules moox adds to tt's audit (architecture §2.3, §2.4). Same contract
// as the rest of this package: each scanner is a pure function over parsed
// files (or file contents), each has a test asserting zero findings over the
// real module AND at least one finding over a violating snippet, and every
// allowlist entry carries a citation.

import (
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// §2.3 — package dependency rules (Imports)
// ---------------------------------------------------------------------------

// ImportRule says which module-internal packages one package may import.
// Patterns ending in "/**" match the directory and everything below it.
type ImportRule struct {
	Pkg      string
	Allow    []string
	Citation string
}

// ImportRules is §2.3's table, plus the rows the table did not need to state
// because those packages predate it (each with its citation).
var ImportRules = []ImportRule{
	{Pkg: "cmd/moox", Allow: []string{"internal/engine", "internal/engine/snapshot", "internal/saver", "internal/setup",
		"internal/setup/manifest", "internal/doctor", "internal/tmuxtarget", "internal/sockets", "internal/mooxroot",
		"internal/statedir"}, Citation: "§2.3 cmd/moox"},
	{Pkg: "cmd/tt", Allow: []string{"internal/observer/**", "internal/engine", "internal/engine/snapshot", "internal/saver",
		"internal/tmuxtarget", "internal/sockets", "internal/mooxroot", "internal/statedir", "internal/panelfmt",
		"internal/projectkey"}, Citation: "§2.3 cmd/tt"},
	{Pkg: "cmd/tt/blackbox", Citation: "tt's black-box suite drives the built binary only; it imports nothing of the module"},
	{Pkg: "internal/engine", Allow: []string{"internal/engine/snapshot", "internal/engine/internal/**",
		"internal/tmuxtarget", "internal/sockets", "internal/statedir"}, Citation: "§2.3 engine facade"},
	{Pkg: "internal/engine/snapshot", Citation: "§6.1: the on-disk format is a leaf, so nothing it depends on can change it"},
	{Pkg: "internal/engine/internal/**", Allow: []string{"internal/engine/internal/**", "internal/engine/snapshot",
		"internal/tmuxtarget", "internal/sockets", "internal/statedir"}, Citation: "§2.3 engine internals"},
	{Pkg: "internal/saver", Allow: []string{"internal/engine", "internal/engine/snapshot", "internal/tmuxtarget",
		"internal/sockets"}, Citation: "§2.3 saver"},
	{Pkg: "internal/observer/**", Allow: []string{"internal/observer/**", "internal/tmuxtarget", "internal/sockets",
		"internal/statedir", "internal/panelfmt", "internal/projectkey", "internal/mooxroot"},
		Citation: "§2.3 observer (mooxroot for the linked Version only, R2-H18); never engine, saver or setup"},
	{Pkg: "internal/setup", Allow: []string{"internal/setup/manifest", "internal/sockets", "internal/statedir",
		"internal/mooxroot", "internal/tmuxtarget", "internal/doctor"}, Citation: "§2.3 setup"},
	{Pkg: "internal/setup/manifest", Citation: "§2.3 setup/manifest: a wire format, a leaf like snapshot"},
	{Pkg: "internal/doctor", Allow: []string{"internal/engine", "internal/sockets", "internal/statedir",
		"internal/tmuxtarget", "internal/mooxroot"}, Citation: "§2.3 doctor"},
	{Pkg: "internal/tmuxtarget", Citation: "§2.3 leaf"},
	{Pkg: "internal/sockets", Citation: "§2.3 leaf"},
	{Pkg: "internal/mooxroot", Citation: "§2.3 leaf"},
	{Pkg: "internal/statedir", Citation: "§2.3 leaf"},
	{Pkg: "internal/panelfmt", Citation: "moved unchanged from tt (§2.1); a leaf of the panel"},
	{Pkg: "internal/projectkey", Citation: "moved unchanged from tt (§2.1); a leaf"},
	{Pkg: "internal/audit", Citation: "the audit reads source, it links nothing of the module"},
	{Pkg: "internal/tmuxtest", Allow: []string{"internal/tmuxtarget", "internal/sockets", "internal/statedir", "internal/mooxroot"},
		Citation: "§2.3 tmuxtest (tmuxtarget, sockets) + statedir, because every file it writes must go through the one " +
			"writer, + mooxroot to find the share root it runs the real config from (P1 decision)"},
}

// TestOnlyPackages may be imported only from _test.go files.
var TestOnlyPackages = []string{"internal/tmuxtest"}

func matchPkg(pattern, dir string) bool {
	if base, ok := strings.CutSuffix(pattern, "/**"); ok {
		return dir == base || strings.HasPrefix(dir, base+"/")
	}
	return pattern == dir
}

// ruleFor picks the most specific rule for a package directory: an exact
// match beats a "/**" pattern, and a longer pattern beats a shorter one.
func ruleFor(rules []ImportRule, dir string) (ImportRule, bool) {
	best, found, bestLen := ImportRule{}, false, -1
	for _, r := range rules {
		if !matchPkg(r.Pkg, dir) {
			continue
		}
		l := len(r.Pkg)
		if !strings.HasSuffix(r.Pkg, "/**") {
			l += 1 << 16 // exact wins
		}
		if l > bestLen {
			best, found, bestLen = r, true, l
		}
	}
	return best, found
}

// Imports reports every module-internal import a non-test file makes that its
// package's rule does not allow, every non-test package with no rule at all,
// and every non-test import of a test-only package.
func Imports(files []File, module string, rules []ImportRule) []Finding {
	var findings []Finding
	for _, f := range files {
		if f.IsTest {
			continue
		}
		rule, ok := ruleFor(rules, f.Dir)
		for _, imp := range f.AST.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || (p != module && !strings.HasPrefix(p, module+"/")) {
				continue
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(p, module), "/")
			at := line(f, imp.Pos())
			for _, t := range TestOnlyPackages {
				if rel == t {
					findings = append(findings, Finding{File: f.Rel, Line: at, Rule: "imports",
						Detail: fmt.Sprintf("%s is imported only from _test.go files (§2.3)", rel)})
				}
			}
			if !ok {
				findings = append(findings, Finding{File: f.Rel, Line: at, Rule: "imports",
					Detail: fmt.Sprintf("package %s has no row in the §2.3 import table but imports %s", f.Dir, rel)})
				continue
			}
			if rel == f.Dir {
				continue
			}
			allowed := false
			for _, a := range rule.Allow {
				if matchPkg(a, rel) {
					allowed = true
					break
				}
			}
			if !allowed {
				findings = append(findings, Finding{File: f.Rel, Line: at, Rule: "imports",
					Detail: fmt.Sprintf("%s may not import %s (§2.3: %s)", f.Dir, rel, rule.Citation)})
			}
		}
	}
	return findings
}

// ---------------------------------------------------------------------------
// §2.4 RestrictedCalls
// ---------------------------------------------------------------------------

// RestrictedCall is one exported function only certain packages may name.
type RestrictedCall struct {
	Pkg      string // the package qualifier as imported, e.g. "statedir"
	Name     string
	Allowed  []string // package dirs allowed to reference it (the defining package always is)
	Defining string
	Citation string
}

// RestrictedCalls is §2.4's list.
var RestrictedCallList = []RestrictedCall{{
	Pkg: "statedir", Name: "OpenUserConfig", Allowed: []string{"internal/setup"}, Defining: "internal/statedir",
	Citation: "§2.4: the one statedir entry that skips the ~/.claude refusal; setup writes the Claude hooks into " +
		"~/.claude/settings.json with explicit consent (F5)",
}}

// RestrictedCalls reports every reference, in any file (tests included), to a
// restricted function from a package not allowed to name it.
func RestrictedCalls(files []File, list []RestrictedCall) []Finding {
	var findings []Finding
	for _, f := range files {
		walkWithFuncs(f, func(fnName string, n ast.Node) {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return
			}
			for _, rc := range list {
				if pkg.Name != rc.Pkg || sel.Sel.Name != rc.Name || f.Dir == rc.Defining {
					continue
				}
				allowed := false
				for _, a := range rc.Allowed {
					if matchPkg(a, f.Dir) {
						allowed = true
					}
				}
				if !allowed {
					findings = append(findings, Finding{File: f.Rel, Line: line(f, sel.Pos()), Func: fnName,
						Rule: "restricted-call", Detail: fmt.Sprintf("%s.%s may be referenced only from %s (%s)",
							rc.Pkg, rc.Name, strings.Join(rc.Allowed, ", "), rc.Citation)})
				}
			}
		})
	}
	return findings
}

// ---------------------------------------------------------------------------
// §2.4 LiteralArgs
// ---------------------------------------------------------------------------

// LiteralArgsDirs are where tmux argv is built from user-derived strings.
var LiteralArgsDirs = []string{"internal/engine/**"}

// FormatExpandedFlags are the tmux flags whose argument tmux format-expands.
// The builders in tmuxtarget take a Lit there; spelling the flag by hand in
// engine code would bypass that (E2).
var FormatExpandedFlags = []string{"-T", "-n", "-c"}

// LiteralArgs reports a string literal of a format-expanded flag in engine
// (non-test) code.
func LiteralArgs(files []File, dirs []string) []Finding {
	var findings []Finding
	for _, f := range files {
		if f.IsTest || !inAny(f.Dir, dirs) {
			continue
		}
		walkWithFuncs(f, func(fnName string, n ast.Node) {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !containsString(FormatExpandedFlags, s) {
				return
			}
			findings = append(findings, Finding{File: f.Rel, Line: line(f, lit.Pos()), Func: fnName, Rule: "literal-args",
				Detail: fmt.Sprintf("%q spelled by hand in engine argv: use the tmuxtarget builder, whose position takes a Lit (§2.4, E2)", s)})
		})
	}
	return findings
}

func inAny(dir string, patterns []string) bool {
	for _, p := range patterns {
		if matchPkg(p, dir) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// §2.4 TmuxExecInTests, the tmuxtest half
// ---------------------------------------------------------------------------

// TmuxtestDir is the harness package.
const TmuxtestDir = "internal/tmuxtest"

// TmuxtestExecForms holds the harness to its two exec forms (§12.2, R2-M4):
//
//	A  exec.Command("tmux", "-f", "/dev/null", "-L", …)      (Start)
//	B  exec.Command(<bin>, "-f", <x>, "-f", <y>, "-S", …)    (StartCold), in
//	   a function that assigns the command's Env explicitly
//
// Exactly one of each. Any other exec whose program is not a string literal
// naming something other than tmux counts as a tmux exec and is a finding. A
// "source-file" literal outside a *Server method (which always targets the
// server's own -S socket) is a finding, and so is "kill-server" anywhere.
func TmuxtestExecForms(files []File) []Finding {
	var findings []Finding
	formA, formB := 0, 0
	for _, f := range files {
		if f.IsTest || f.Dir != TmuxtestDir {
			continue
		}
		for _, decl := range f.AST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			name := ""
			onServer := false
			if ok {
				name = fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) > 0 && receiverTypeName(fn.Recv.List[0].Type) == "Server" {
					onServer = true
				}
			}
			assignsEnv := false
			ast.Inspect(decl, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok {
					for _, l := range as.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
							assignsEnv = true
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.BasicLit:
					if v.Kind != token.STRING {
						return true
					}
					s, _ := strconv.Unquote(v.Value)
					if s == "kill-server" {
						findings = append(findings, Finding{File: f.Rel, Line: line(f, v.Pos()), Func: name,
							Rule: "tmuxtest-forms", Detail: "kill-server is never used (the terminal hook blocks it; Close kills sessions)"})
					}
					if s == "source-file" && !onServer {
						findings = append(findings, Finding{File: f.Rel, Line: line(f, v.Pos()), Func: name,
							Rule: "tmuxtest-forms", Detail: "source-file outside a *Server method could reach another socket"})
					}
				case *ast.CallExpr:
					sel, ok := v.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != "exec" || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
						return true
					}
					args := v.Args
					if sel.Sel.Name == "CommandContext" && len(args) > 0 {
						args = args[1:]
					}
					if len(args) == 0 {
						return true
					}
					if prog, isLit := strLit(args[0]); isLit && prog != "tmux" {
						return true // go build, …: not a tmux exec
					}
					switch {
					case isFormA(args):
						formA++
					case isFormB(args) && assignsEnv:
						formB++
					default:
						findings = append(findings, Finding{File: f.Rel, Line: line(f, v.Pos()), Func: name,
							Rule: "tmuxtest-forms", Detail: "a tmux exec that is neither Start's `-f /dev/null -L` form nor StartCold's `-f -f -S` form with an explicit Env"})
					}
				}
				return true
			})
		}
	}
	if formA != 1 || formB != 1 {
		findings = append(findings, Finding{File: TmuxtestDir, Rule: "tmuxtest-forms",
			Detail: fmt.Sprintf("want exactly one Start form and one StartCold form, found %d and %d", formA, formB)})
	}
	return findings
}

func strLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func isFormA(args []ast.Expr) bool {
	if len(args) < 5 {
		return false
	}
	want := []string{"tmux", "-f", "/dev/null", "-L"}
	for i, w := range want {
		if s, ok := strLit(args[i]); !ok || s != w {
			return false
		}
	}
	return true
}

func isFormB(args []ast.Expr) bool {
	if len(args) < 7 {
		return false
	}
	if _, isLit := strLit(args[0]); isLit {
		return false
	}
	for i, w := range map[int]string{1: "-f", 3: "-f", 5: "-S"} {
		if s, ok := strLit(args[i]); !ok || s != w {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// §2.4 RunShell
// ---------------------------------------------------------------------------

// RunShellExempt are the package dirs that may spell "run-shell" as data.
var RunShellExempt = []string{"internal/audit"}

var runShellArg = regexp.MustCompile(`^run(-shell|-s[a-z]*)?$`)

// execCallNames are the calls whose string arguments are tmux argv: the
// tmuxtarget/tmuxtest runners and builders, and exec itself.
var execCallNames = map[string]bool{
	"Run": true, "RunErr": true, "Command": true, "CommandContext": true, "Chain": true, "Args": true,
	"append": true,
}

// RunShellGo reports Go code that could exec run-shell (tt rule pane.go:63-66;
// memory never-bare-tmux-run-shell: a tool client's run-shell segfaults tmux
// 3.7b and kills every session). It flags "run-shell", a unique prefix such
// as "run-s", or the alias "run" in an ARGV POSITION:
//
//   - an argument of Run/RunErr/Command/CommandContext/Chain/Args/append, or
//     an element of a composite literal passed to one (shipped and test code);
//   - any composite-literal element in shipped code, where argv slices are
//     built ([]string{…, "run-shell", …}).
//
// Config text in shipped Go (a literal starting "run-shell ") is held to
// RunShellConf. Naming run-shell as data — `strings.Contains(conf,
// "run-shell")`, a test asserting an argv does NOT hold it — is not a
// finding; sending it is.
func RunShellGo(files []File) []Finding {
	var findings []Finding
	for _, f := range files {
		if inAny(f.Dir, RunShellExempt) {
			continue
		}
		argLits := map[*ast.BasicLit]bool{}
		mark := func(e ast.Expr) {
			switch v := e.(type) {
			case *ast.BasicLit:
				argLits[v] = true
			case *ast.CompositeLit:
				for _, el := range v.Elts {
					if lit, ok := el.(*ast.BasicLit); ok {
						argLits[lit] = true
					}
				}
			}
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if cl, ok := n.(*ast.CompositeLit); ok && !f.IsTest {
				mark(cl)
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if execCallNames[name] {
				for _, a := range call.Args {
					mark(a)
				}
			}
			return true
		})
		walkWithFuncs(f, func(fnName string, n ast.Node) {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return
			}
			switch {
			case runShellArg.MatchString(s) && argLits[lit]:
				findings = append(findings, Finding{File: f.Rel, Line: line(f, lit.Pos()), Func: fnName, Rule: "run-shell",
					Detail: fmt.Sprintf("%q as a tmux argument: Go code never execs run-shell (§2.4)", s)})
			case !f.IsTest && (strings.HasPrefix(strings.TrimSpace(s), "run-shell ") || strings.HasPrefix(strings.TrimSpace(s), "run ")):
				for _, p := range RunShellConf(f.Rel, s) {
					p.Line = line(f, lit.Pos())
					p.Func = fnName
					findings = append(findings, p)
				}
			}
		})
	}
	return findings
}

// runShellCmd finds a run-shell (or run) COMMAND in a config line: at the
// start, after a ';' or '{', or as the quoted body of a hook or binding.
var runShellCmd = regexp.MustCompile(`(^|[;{'"]\s*|\s)(run-shell|run)(\s+-[a-zA-Z]+)*\s+['"]`)

// runShellFlags captures a run-shell's flags, to look for -b.
var runShellFlags = regexp.MustCompile(`(run-shell|run)((\s+-[a-zA-Z]+)*)`)

// silentTail is the required ending of a config run-shell's command.
var silentTail = regexp.MustCompile(`>\s*/dev/null\s+2>&1\s*\|\|\s*true\s*\\?['"]`)

// UpstreamBindingRunShells are the KEY-BINDING run-shells inside unmodified
// vendored plugin entry scripts (PINS mode `plain`) that RunShellConf would
// otherwise flag, keyed "<file>: <logical line>" (continuations joined, blank
// runs squeezed — the same text tests/vendored-run-shell-lint.test.sh pins).
//
// Cited exemption (Phase 4 P6, implementation-log "Decisions changed"): a
// binding's job is issued by the attached client that pressed the key, not by
// a config-time or hook client, which is the shape of the 3.7b print-path crash
// (RR12, memory never-bare-tmux-run-shell); and silencing them would mean
// editing upstream plugins that moox ships unmodified (tmux-jump is GPL-3.0,
// shipped as-is). Hooks get NO exemption: a set-hook line is never matched
// here, and TestE2E_Conf_LoadedHooksSilentAndIndexed checks the hooks a loaded
// server really holds. Pinned by exact text, so a pin bump that changes or
// adds one fails the audit until someone reviews it.
var UpstreamBindingRunShells = map[string]bool{
	`share/moox/plugins/tmux-fzf/main.tmux: tmux bind-key "$TMUX_FZF_LAUNCH_KEY" run-shell -b "$CURRENT_DIR/main.sh"`:                                                                           true,
	`share/moox/plugins/tmux-fzf-url/fzf-url.tmux: tmux bind-key "${note_flag[@]}" "$key" run -b "$cmd"`:                                                                                        true,
	`share/moox/plugins/tmux-jump/tmux-jump.tmux: tmux bind-key -N "Jump to pane location in copy mode" $(get_tmux_option "@jump-key" "j") run-shell -b "$CURRENT_DIR/scripts/tmux-jump.sh"`:    true,
	`share/moox/plugins/tmux-open/open.tmux: echo "$(preserve_url_hash) | xargs -I {} tmux run-shell -b 'cd #{pane_current_path}; $command_string \"{}\" > /dev/null'"`:                         true,
	`share/moox/plugins/tmux-open/open.tmux: echo "$(preserve_url_hash) | sed 's/\ /+/g' | xargs -I {} tmux run-shell -b 'cd #{pane_current_path}; $command_string $engine\"{}\" > /dev/null'"`: true,
	`share/moox/plugins/tmux-sensible/sensible.tmux: tmux bind-key R run-shell " tmux source-file ${tmux_config} > /dev/null; tmux display-message 'Sourced ${tmux_config}!'"`:                  true,
	`share/moox/plugins/tmux-yank/yank.tmux: tmux bind-key "$(yank_line_key)" run-shell -b "$SCRIPTS_DIR/copy_line.sh"`:                                                                         true,
	`share/moox/plugins/tmux-yank/yank.tmux: tmux bind-key "$(yank_pane_pwd_key)" run-shell -b "$SCRIPTS_DIR/copy_pane_pwd.sh"`:                                                                 true,
}

// RunShellConf checks one config file's text (a .conf, or a plugin's .tmux
// entry script): every run-shell must be silent and end
// `>/dev/null 2>&1 || true'`, and one inside hook text (a set-hook line) must
// also carry -b (RR12, R2-C1). Comment lines are skipped, and so are the
// listed UpstreamBindingRunShells (never hook text).
//
// The text is read as LOGICAL lines (logicalConfLines): a backslash-newline
// continuation is joined first, so a run-shell split over several lines
// (tmux-sensible's `prefix R`) is judged whole, and a run-shell inside a
// brace block opened by a set-hook line (`set-hook -g x {` … `}`) is hook
// text even though "set-hook" is on another line. A finding names the
// logical line's first physical line.
func RunShellConf(name, content string) []Finding {
	var findings []Finding
	for _, l := range logicalConfLines(content) {
		t, n := l.text, l.line
		locs := runShellCmd.FindAllStringIndex(t, -1)
		if len(locs) == 0 {
			continue
		}
		if !l.hook && UpstreamBindingRunShells[name+": "+t] {
			continue
		}
		rest := t[locs[0][0]:]
		if !silentTail.MatchString(rest) {
			findings = append(findings, Finding{File: name, Line: n, Rule: "run-shell",
				Detail: "run-shell must be silent and end `>/dev/null 2>&1 || true'` (RR12): " + t})
		}
		if l.hook {
			hasB := false
			for _, m := range runShellFlags.FindAllStringSubmatch(t, -1) {
				if strings.Contains(m[2], "b") {
					hasB = true
				}
			}
			if !hasB {
				findings = append(findings, Finding{File: name, Line: n, Rule: "run-shell",
					Detail: "run-shell in hook text must carry -b (R2-C1): " + t})
			}
		}
	}
	return findings
}

// confLine is one logical line of a config file or plugin entry script.
type confLine struct {
	line int    // first physical line, 1-based
	text string // continuations joined, blank runs squeezed to one space
	hook bool   // hook text: a set-hook line, or inside a set-hook's { } block
}

var blankRun = regexp.MustCompile(`[ \t]+`)

// logicalConfLines splits tmux config (or a shell entry script) into logical
// lines: a physical line ending in a backslash continues on the next one
// (both tmux's parser and sh join them), comment and blank logical lines are
// dropped. Brace blocks are tracked by their shape — a logical line ending in
// `{` opens one, a line starting with `}` closes the innermost — which covers
// tmux's `set-hook -g x {` … `}` / `bind X {` … `}` and sh function bodies
// alike; a block opened by a set-hook line marks everything inside it as hook
// text, nested blocks included.
func logicalConfLines(content string) []confLine {
	var out []confLine
	var blocks []bool // one per open brace block: was it opened as hook text
	inHook := func() bool {
		for _, h := range blocks {
			if h {
				return true
			}
		}
		return false
	}
	emit := func(first int, text string) {
		t := strings.TrimSpace(blankRun.ReplaceAllString(text, " "))
		if t == "" || strings.HasPrefix(t, "#") {
			return
		}
		if strings.HasPrefix(t, "}") && len(blocks) > 0 {
			blocks = blocks[:len(blocks)-1]
		}
		hook := strings.Contains(t, "set-hook") || inHook()
		out = append(out, confLine{line: first, text: t, hook: hook})
		if strings.HasSuffix(t, "{") {
			blocks = append(blocks, hook)
		}
	}
	var acc strings.Builder
	first := 0
	for i, raw := range strings.Split(content, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if acc.Len() == 0 {
			first = i + 1
		} else {
			acc.WriteByte(' ')
		}
		trimmed := strings.TrimRight(raw, " \t")
		// A comment ends at its newline: a trailing backslash on a comment
		// line continues nothing (sh; tmux reads it the same way).
		isComment := acc.Len() == 0 && strings.HasPrefix(strings.TrimSpace(raw), "#")
		if !isComment && strings.HasSuffix(trimmed, `\`) {
			acc.WriteString(strings.TrimSuffix(trimmed, `\`))
			continue
		}
		acc.WriteString(raw)
		emit(first, acc.String())
		acc.Reset()
	}
	if acc.Len() > 0 {
		emit(first, acc.String())
	}
	return out
}

// ConfFile is one config file's text for RunShellConf, relative to the root.
type ConfFile struct{ Rel, Content string }

// IsConfPath reports whether a share/moox path is held to RunShellConf:
// every *.conf, and every plugin entry script plugins/**/*.tmux.
func IsConfPath(rel string) bool {
	if !strings.HasPrefix(rel, "share/moox/") {
		return false
	}
	if path.Ext(rel) == ".conf" {
		return true
	}
	return strings.HasPrefix(rel, "share/moox/plugins/") && path.Ext(rel) == ".tmux"
}

// WireDirs are the packages allowed to carry json tags: one per format
// (§2.4 JSONTags): tt's wire protocol, the snapshot on-disk format, and the
// setup manifest.
var WireDirs = []string{WirePackageDir, "internal/engine/snapshot", "internal/setup/manifest"}
