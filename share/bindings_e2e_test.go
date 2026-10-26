//go:build tmuxe2e

package share_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// These tests read key bindings back from a real tmux server (`list-keys`), so
// they see what tmux actually holds and prints — not what a config file or a
// plugin's source text says (G3-H2, G3-2-H1, G3-2-M1). The parser below is the
// test's own reading of tmux's printed form (args_escape, cmd_list_print), kept
// independent of the awk in scripts/silence-bindings.sh.

// tmuxToken is one word of a printed tmux command line: raw is the text as
// printed, val the argument tmux holds once the quoting and escapes are undone.
type tmuxToken struct {
	raw, val string
	quote    byte // '"', '\'', or 0 (bare)
}

// tmuxTokens splits a list-keys line. ok is false on text tmux would not print
// (an unterminated quote).
func tmuxTokens(line string) (toks []tmuxToken, ok bool) {
	i, n := 0, len(line)
	for i < n {
		if line[i] == ' ' || line[i] == '\t' {
			i++
			continue
		}
		start := i
		var q byte
		switch line[i] {
		case '"':
			q = '"'
			for i++; i < n && line[i] != '"'; i++ {
				if line[i] == '\\' {
					i++
				}
			}
			if i >= n {
				return nil, false
			}
			i++
		case '\'':
			q = '\''
			for i++; i < n && line[i] != '\''; i++ {
			}
			if i >= n {
				return nil, false
			}
			i++
		default:
			for ; i < n && line[i] != ' ' && line[i] != '\t'; i++ {
				if line[i] == '\\' {
					i++
				}
			}
			if i > n {
				i = n
			}
		}
		raw := line[start:i]
		t := tmuxToken{raw: raw, quote: q}
		switch q {
		case '\'':
			t.val = raw[1 : len(raw)-1]
		case '"':
			t.val = unescapeTmux(raw[1 : len(raw)-1])
		default:
			t.val = unescapeTmux(raw)
		}
		toks = append(toks, t)
	}
	return toks, true
}

// unescapeTmux undoes the escapes tmux's parser reads in bare and "…" words
// (cmd-parse.y yylex_token_escape): \a \b \e \f \n \r \s \t \v, \NNN octal, and
// a backslash before any other character is that character.
func unescapeTmux(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		c := s[i]
		switch {
		case c >= '0' && c <= '3' && i+2 < len(s):
			b.WriteByte((c-'0')<<6 | (s[i+1]-'0')<<3 | (s[i+2] - '0'))
			i += 2
		case strings.IndexByte("abefnrstv", c) >= 0:
			b.WriteByte(map[byte]byte{'a': '\a', 'b': '\b', 'e': 0x1b, 'f': '\f', 'n': '\n', 'r': '\r', 's': ' ', 't': '\t', 'v': '\v'}[c])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isSep(t tmuxToken) bool {
	return t.quote == 0 && (t.raw == `\;` || t.raw == ";" || t.raw == "{" || t.raw == "}")
}

// boundRunShell is one run-shell at a command position of a binding.
type boundRunShell struct {
	flags  []string
	dashC  bool
	cmd    *tmuxToken // nil: no command (`run-shell -d 0.3`, a delay)
	cmdIdx int
}

// binding is one list-keys line: its table and key, and every run-shell it
// runs — first, last, between `\;`, or inside a `{ … }` block.
type binding struct {
	line, table, key string
	toks             []tmuxToken
	runShells        []boundRunShell
}

func parseBinding(t *testing.T, line string) (binding, bool) {
	t.Helper()
	toks, ok := tmuxTokens(line)
	if !ok || len(toks) < 3 || toks[0].raw != "bind-key" {
		return binding{}, false
	}
	b := binding{line: line, toks: toks}
	i := 1
	for i < len(toks) && (toks[i].raw == "-r" || toks[i].raw == "-T" || toks[i].raw == "-N") {
		if toks[i].raw == "-T" && i+1 < len(toks) {
			b.table = toks[i+1].val
		}
		if toks[i].raw == "-r" {
			i++
		} else {
			i += 2
		}
	}
	if i >= len(toks) {
		return binding{}, false
	}
	b.key = toks[i].raw
	cmdPos := true
	for k := i + 1; k < len(toks); k++ {
		if isSep(toks[k]) {
			cmdPos = true
			continue
		}
		if !cmdPos {
			continue
		}
		cmdPos = false
		if toks[k].raw != "run-shell" {
			continue
		}
		rs := boundRunShell{cmdIdx: -1}
		j := k + 1
		for j < len(toks) && toks[j].quote == 0 && len(toks[j].raw) > 1 && toks[j].raw[0] == '-' {
			f := toks[j].raw
			rs.flags = append(rs.flags, f)
			if strings.ContainsAny(f[1:], "cdst") && len(f) == 2 {
				j += 2
				continue
			}
			if strings.Contains(f, "C") {
				rs.dashC = true
			}
			j++
		}
		if j < len(toks) && !isSep(toks[j]) {
			rs.cmd, rs.cmdIdx = &toks[j], j
			if j+1 < len(toks) && !isSep(toks[j+1]) {
				t.Errorf("run-shell with more than one argument, the parser does not know this shape:\n  %s", line)
			}
		}
		b.runShells = append(b.runShells, rs)
	}
	return b, true
}

func listBindings(t *testing.T, s *tmuxtest.Server) []binding {
	t.Helper()
	var out []binding
	for _, l := range strings.Split(s.Run(t, "list-keys"), "\n") {
		if b, ok := parseBinding(t, l); ok {
			out = append(out, b)
		} else if strings.HasPrefix(l, "bind-key") {
			t.Errorf("list-keys line the parser cannot read:\n  %s", l)
		}
	}
	if len(out) == 0 {
		t.Fatal("list-keys returned no bindings")
	}
	return out
}

const silentSuffix = ") >/dev/null 2>&1 || true"

// exactlySilent is the one shape the silencer writes: `( CMD ) >/dev/null 2>&1
// || true`, with a blank or a newline before the `)`. A command that only ENDS
// that way (`a; b >/dev/null 2>&1 || true`) is not silent: `a` still prints.
func exactlySilent(v string) bool {
	if !strings.HasPrefix(v, "( ") || !strings.HasSuffix(v, silentSuffix) || len(v) < len(silentSuffix)+3 {
		return false
	}
	c := v[len(v)-len(silentSuffix)-1]
	return c == ' ' || c == '\n'
}

var tailSilent = regexp.MustCompile(`>\s*/dev/null\s+2>&1\s*\|\|\s*true\s*$`)

// vendored is a job the silencer owns: it names <root>/plugins/, tmux-fingers,
// or is tmux-sensible's `Sourced …!` reload.
func vendored(root, v string) bool {
	return strings.Contains(v, root+"/plugins/") || strings.Contains(v, "tmux-fingers") ||
		strings.Contains(v, "display-message 'Sourced ")
}

// formatRef approximates tmux's format expansion before the shell sees the
// command: `#{…}` becomes a word, `##` a `#`.
var formatRef = regexp.MustCompile(`#\{[^{}]*\}`)

// shellParses runs `sh -n` over a job as tmux would hand it to the shell, so a
// rewrap whose `)` a comment swallowed is caught.
func shellParses(t *testing.T, v string) bool {
	t.Helper()
	for formatRef.MatchString(v) {
		v = formatRef.ReplaceAllString(v, "X")
	}
	v = strings.ReplaceAll(v, "##", "#")
	return exec.Command("/bin/sh", "-n", "-c", v).Run() == nil
}

// deferredRunShellText: tmux-open's copy-mode `o`/`S` bindings pipe the
// selection to `xargs tmux run-shell -b '…'` INSIDE a copy-pipe job — not a
// run-shell binding, so not rewrapped (G3-M4, deferred: needs a plugin edit).
// They are the only bindings allowed to mention run-shell without one at a
// command position.
func deferredRunShellText(b binding) bool {
	return strings.Contains(b.line, "copy-pipe") && strings.Contains(b.line, "| xargs -I {} tmux run-shell -b 'cd #{pane_current_path}; ")
}

// checkAllSilent: EVERY binding whose text contains run-shell either runs it as
// a job that is silent and `|| true` — in the exact rewrap shape when the job
// is a vendored one — or is a run-shell with no job (-C, or no command), or is
// the deferred tmux-open copy-pipe text.
func checkAllSilent(t *testing.T, root string, bs []binding) (jobs int) {
	t.Helper()
	for _, b := range bs {
		if !strings.Contains(b.line, "run-shell") {
			continue
		}
		if len(b.runShells) == 0 {
			if !deferredRunShellText(b) {
				t.Errorf("binding mentions run-shell but runs none the test can see (unknown shape):\n  %s", b.line)
			}
			continue
		}
		for _, rs := range b.runShells {
			if rs.dashC {
				continue
			}
			if rs.cmd == nil {
				if !strings.Contains(strings.Join(rs.flags, " "), "-d") {
					t.Errorf("run-shell with neither a command nor a delay:\n  %s", b.line)
				}
				continue
			}
			jobs++
			v := rs.cmd.val
			switch {
			case vendored(root, v) && !exactlySilent(v):
				t.Errorf("vendored binding %s %s runs a job not in the exact `( … ) >/dev/null 2>&1 || true` shape:\n  %s", b.table, b.key, b.line)
			case !tailSilent.MatchString(v):
				t.Errorf("binding %s %s runs a shell job that is not silent and || true:\n  %s", b.table, b.key, b.line)
			}
			if strings.Contains(v, "( ( ") {
				t.Errorf("binding %s %s rewrapped twice:\n  %s", b.table, b.key, b.line)
			}
			if !shellParses(t, v) {
				t.Errorf("binding %s %s runs a job /bin/sh cannot parse:\n  %q", b.table, b.key, v)
			}
		}
	}
	return jobs
}

func bindingByKey(bs []binding, table, key string) (binding, bool) {
	for _, b := range bs {
		if b.table == table && b.key == key {
			return b, true
		}
	}
	return binding{}, false
}

// requireClipboardTool fails a test that checks tmux-yank's prefix y/Y when
// the machine has no clipboard command: tmux-yank binds y/Y only after
// clipboard_copy_command finds one (yank.tmux: error_handling_if_command_not_present
// exits first), so on a headless Linux runner the bindings under test do not
// exist. A clear precondition failure, not a skip: CI installs xclip.
func requireClipboardTool(t *testing.T) {
	t.Helper()
	for _, c := range []string{"pbcopy", "clip.exe", "wl-copy", "xsel", "xclip", "putclip"} {
		if _, err := exec.LookPath(c); err == nil {
			return
		}
	}
	t.Fatal("no clipboard command on PATH (pbcopy, wl-copy, xsel, xclip, …): tmux-yank binds prefix y/Y only " +
		"when it finds one — install xclip (ci.yml does)")
}

// TestE2E_Conf_LoadedBindingsSilent (G3-H2, G3-2-H1) loads the SHIPPED config
// into an isolated server and reads back every key binding it holds, in every
// table — including those the vendored plugins and tmux-fingers bind at load
// time, which no config file shows the conf-lint. tmux prints a one-word job
// bare (`run-shell -b /…/copy_line.sh`), so the bare form is the one that
// matters most: tmux-yank's `y`/`Y`, tmux-fzf's `f` and tmux-jump's `j` print
// that way on a standard path. A job that prints or fails after its client has
// gone is the 3.7b cmd_run_shell_print crash. A reload rewraps nothing twice.
func TestE2E_Conf_LoadedBindingsSilent(t *testing.T) {
	requireClipboardTool(t)
	s := tmuxtest.Start(t)
	stubGate(t, s, "1")
	s.SourceMoox(t)
	root, err := tmuxtest.ShareRoot()
	if err != nil {
		t.Fatal(err)
	}
	bs := listBindings(t, s)
	n := checkAllSilent(t, root, bs)
	if n == 0 {
		t.Fatal("no run-shell jobs on a loaded server: the scan saw nothing")
	}
	// The plugin bindings known to exist, each checked by name so a parser or
	// silencer that silently skips one cannot pass.
	want := map[string]string{
		"y": "tmux-yank/scripts/copy_line.sh",
		"Y": "tmux-yank/scripts/copy_pane_pwd.sh",
		"f": "tmux-fzf/main.sh",
		"j": "tmux-jump/scripts/tmux-jump.sh",
		"u": "tmux-fzf-url/fzf-url.sh",
		"V": "tmux-copy-image/plugin/scripts/paste.sh",
		"R": "Sourced ",
	}
	if _, err := exec.LookPath("tmux-fingers"); err == nil {
		want["F"], want["J"] = "tmux-fingers", "tmux-fingers"
		if !strings.Contains(s.Run(t, "list-keys"), "-T fingers") {
			t.Error("tmux-fingers is installed but its key table is missing: the fingers case was not exercised")
		}
	}
	for key, needle := range want {
		b, ok := bindingByKey(bs, "prefix", key)
		if !ok {
			t.Errorf("prefix %s is not bound", key)
			continue
		}
		found := false
		for _, rs := range b.runShells {
			if rs.cmd != nil && strings.Contains(rs.cmd.val, needle) && exactlySilent(rs.cmd.val) {
				found = true
			}
		}
		if !found {
			t.Errorf("prefix %s does not run %s rewrapped silent:\n  %s", key, needle, b.line)
		}
	}
	s.SourceMoox(t)
	bs2 := listBindings(t, s)
	if n2 := checkAllSilent(t, root, bs2); n2 != n {
		t.Errorf("reload changed the number of run-shell jobs %d -> %d", n, n2)
	}
	if len(bs2) != len(bs) {
		t.Errorf("reload changed the number of bindings %d -> %d", len(bs), len(bs2))
	}
}

// edgeBindings are shapes no vendored plugin binds today, bound next to the
// real ones so the silencer meets each quoting and position form tmux prints:
// P is the moox root of the copy the server loaded.
const edgeBindings = `bind-key -T edge a run-shell -b 'P/plugins/x/a"x".sh'
bind-key -T edge b if-shell -F 1 { run-shell -b "P/plugins/x/b.sh arg" ; display-message hi }
bind-key -T edge c display-message one \; run-shell "P/plugins/x/c.sh one" \; display-message two
bind-key -T edge d run-shell "P/plugins/x/d.sh ; echo x # note"
bind-key -T edge e run-shell "echo loud; P/plugins/x/e.sh >/dev/null 2>&1 || true"
bind-key -T edge f run-shell "echo user binding"
bind-key -T edge g run-shell -C "display-message P/plugins/x/g"
bind-key -T edge h run-shell -d 0.3
bind-key -T edge i run-shell -b -d 1 P/plugins/x/i.sh
bind-key -T edge j run-shell -b "cd #{pane_current_path} && P/plugins/x/j.sh"
bind-key -T edge k run-shell "( P/plugins/x/k.sh ) >/dev/null 2>&1 || true"
bind-key -T edge l run-shell -b P/plugins/x/l1.sh \; run-shell -b P/plugins/x/l2.sh
`

// TestE2E_SilenceBindings_RewrapsExactly (G3-2-M1, G3-2-H1) captures list-keys
// BEFORE the silencer runs — a copy of the shipped tree whose silencer is a
// no-op, plus the edge bindings above — then runs the real
// scripts/silence-bindings.sh against the server and captures AFTER. Each
// binding must come back identical, word for word, except that a vendored job
// not already in the exact shape is replaced by `( <original> ) >/dev/null 2>&1
// || true` (a newline before the `)` when the job has a shell comment), with
// <original> read from the BEFORE capture, not from the silencer.
func TestE2E_SilenceBindings_RewrapsExactly(t *testing.T) {
	requireClipboardTool(t)
	share, err := tmuxtest.ShareRoot()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "moox")
	if err := os.CopyFS(root, os.DirFS(share)); err != nil {
		t.Fatal(err)
	}
	silencer := filepath.Join(share, "scripts", "silence-bindings.sh")
	if err := os.WriteFile(filepath.Join(root, "scripts", "silence-bindings.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	edge := filepath.Join(t.TempDir(), "edge.conf")
	if err := os.WriteFile(edge, []byte(strings.ReplaceAll(edgeBindings, "P/", root+"/")), 0o600); err != nil {
		t.Fatal(err)
	}

	s := tmuxtest.Start(t)
	stubGate(t, s, "1")
	s.Run(t, "source-file", filepath.Join(root, "moox.conf"))
	if got := s.Opt(t, "@moox-root"); got != root {
		t.Fatalf("@moox-root = %q, want the copy %q", got, root)
	}
	s.Run(t, "source-file", edge)
	before := listBindings(t, s)

	pid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}"))
	runSilencer := func() {
		cmd := exec.Command("/bin/sh", silencer, root)
		cmd.Env = s.Env("TMUX=" + s.T.Socket + "," + pid + ",0")
		cmd.Dir = s.Home
		out, err := cmd.CombinedOutput()
		if err != nil || len(out) != 0 {
			t.Fatalf("silencer: err=%v output=%q (it must exit 0 and print nothing)", err, out)
		}
	}
	runSilencer()
	after := listBindings(t, s)

	type tk struct{ table, key string }
	index := func(bs []binding) map[tk]binding {
		m := map[tk]binding{}
		for _, b := range bs {
			m[tk{b.table, b.key}] = b
		}
		return m
	}
	bm, am := index(before), index(after)
	if len(bm) != len(am) {
		t.Errorf("the silencer changed the number of bindings %d -> %d", len(bm), len(am))
	}
	rewrapped := map[tk]int{}
	for k, b := range bm {
		a, ok := am[k]
		if !ok {
			t.Errorf("binding %s %s disappeared", k.table, k.key)
			continue
		}
		if len(a.toks) != len(b.toks) {
			t.Errorf("binding %s %s changed shape:\n  before %s\n  after  %s", k.table, k.key, b.line, a.line)
			continue
		}
		jobAt := map[int]bool{}
		for _, rs := range b.runShells {
			if rs.cmd != nil && !rs.dashC {
				jobAt[rs.cmdIdx] = true
			}
		}
		for i := range b.toks {
			bv, av := b.toks[i].val, a.toks[i].val
			if !jobAt[i] {
				if bv != av {
					t.Errorf("binding %s %s: word %d changed %q -> %q", k.table, k.key, i, bv, av)
				}
				continue
			}
			mustWrap := vendored(root, bv) && !exactlySilent(bv)
			switch {
			case !mustWrap && av != bv:
				t.Errorf("binding %s %s: a job that is not the silencer's changed %q -> %q", k.table, k.key, bv, av)
			case mustWrap && av != "( "+bv+" "+silentSuffix && av != "( "+bv+"\n"+silentSuffix:
				t.Errorf("binding %s %s: job not rewrapped as ( <original> ) >/dev/null 2>&1 || true:\n  original %q\n  got      %q", k.table, k.key, bv, av)
			case mustWrap:
				rewrapped[k]++
			}
			if mustWrap && !shellParses(t, av) {
				t.Errorf("binding %s %s: the rewrapped job does not parse in /bin/sh: %q", k.table, k.key, av)
			}
		}
	}
	// Everything but the user's own edge f (left loud on purpose) is silent.
	var ours []binding
	for _, b := range after {
		if b.table != "edge" || b.key != "f" {
			ours = append(ours, b)
		}
	}
	checkAllSilent(t, root, ours)

	// The edge cases, by name.
	jobOf := func(key string, nth int) string {
		b, ok := am[tk{"edge", key}]
		if !ok {
			t.Fatalf("edge %s not bound", key)
		}
		var jobs []string
		for _, rs := range b.runShells {
			if rs.cmd != nil {
				jobs = append(jobs, rs.cmd.val)
			}
		}
		if nth >= len(jobs) {
			t.Fatalf("edge %s has %d jobs, want job %d: %s", key, len(jobs), nth, b.line)
		}
		return jobs[nth]
	}
	P := root + "/plugins/x/"
	wrap := func(c string) string { return "( " + c + " ) >/dev/null 2>&1 || true" }
	for _, c := range []struct {
		key  string
		nth  int
		want string
	}{
		{"a", 0, wrap(P + `a"x".sh`)},                                          // printed single-quoted
		{"b", 0, wrap(P + "b.sh arg")},                                         // inside a { … } block
		{"c", 0, wrap(P + "c.sh one")},                                         // between two \; commands
		{"d", 0, "( " + P + "d.sh ; echo x # note\n) >/dev/null 2>&1 || true"}, // a comment
		{"e", 0, wrap("echo loud; " + P + "e.sh >/dev/null 2>&1 || true")},     // only the tail was silent
		{"f", 0, "echo user binding"},                                          // a user's job is theirs
		{"i", 0, wrap(P + "i.sh")},                                             // bare, after -d 1
		{"j", 0, wrap("cd #{pane_current_path} && " + P + "j.sh")},             // a format is not a comment
		{"k", 0, wrap(P + "k.sh")},                                             // already exact: unchanged
		{"l", 0, wrap(P + "l1.sh")},                                            // two jobs, both bare
		{"l", 1, wrap(P + "l2.sh")},
	} {
		if got := jobOf(c.key, c.nth); got != c.want {
			t.Errorf("edge %s job %d = %q, want %q", c.key, c.nth, got, c.want)
		}
	}
	if g := am[tk{"edge", "g"}].line; !strings.Contains(g, `run-shell -C "display-message `) || strings.Contains(g, "/dev/null") {
		t.Errorf("edge g (-C, a tmux command) was touched: %s", g)
	}
	if h := am[tk{"edge", "h"}].line; strings.Contains(h, "/dev/null") {
		t.Errorf("edge h (a delay, no job) was touched: %s", h)
	}
	for _, key := range []string{"y", "Y", "f", "j"} {
		if b := bm[tk{"prefix", key}]; !strings.Contains(b.line, "run-shell -b "+root+"/plugins/") {
			t.Errorf("prefix %s before the silencer is not the bare form the review saw (fixture drift):\n  %s", key, b.line)
		}
		if rewrapped[tk{"prefix", key}] != 1 {
			t.Errorf("prefix %s (printed bare by tmux) was not rewrapped once", key)
		}
	}

	// Idempotent: a second run on an already-silenced server changes nothing.
	runSilencer()
	if again := listBindings(t, s); joinLines(again) != joinLines(after) {
		t.Error("a second silencer run changed the bindings")
	}
}

func joinLines(bs []binding) string {
	var b strings.Builder
	for _, x := range bs {
		b.WriteString(x.line)
		b.WriteByte('\n')
	}
	return b.String()
}
