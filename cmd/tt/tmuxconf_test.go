package main

// The tmux wiring tt documents is text in two places — moox's
// share/moox/conf/tt.conf, which moox loads (from active.conf, on a server that
// takes part in continuity), and README.md, which tells a reader what it does.
// The tmuxlive gate sources the tt.conf block into an isolated server and tests
// its behaviour; the tests here are the hermetic half: the README shows exactly
// what ships, and the shipped text carries none of the mistakes that broke
// P10's design. NOTHING HERE TOUCHES TMUX: the files are read as text.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// observerBlock returns the text between the tt markers, inclusive, plus a
// final newline.
func observerBlock(conf string) (string, error) {
	const begin, end = "# >>> tt observer >>>", "# <<< tt observer <<<"
	i := strings.Index(conf, begin)
	j := strings.Index(conf, end)
	if i < 0 || j < 0 || j < i {
		return "", fmt.Errorf("the tt observer markers are missing")
	}
	return conf[i:j+len(end)] + "\n", nil
}

// statusRightLine is the one status-right line of status.conf, or "".
func statusRightLine(conf string) string {
	sc := bufio.NewScanner(strings.NewReader(conf))
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "set -g status-right ") {
			return l
		}
	}
	return ""
}

// ttConf and statusConf are the files moox ships (moox/share/moox/conf), read
// from moox/cmd/tt.
func ttConf(t *testing.T) string {
	return readRepoFile(t, "..", "..", "share", "moox", "conf", "tt.conf")
}

func statusConf(t *testing.T) string {
	return readRepoFile(t, "..", "..", "share", "moox", "conf", "status.conf")
}

func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rel...))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(rel...), err)
	}
	return string(data)
}

// The README's block IS the shipped block. A reader who copies it gets exactly
// what the gate tested; a change to one without the other fails here.
func TestTheREADMEShowsTheTmuxBlockThatShips(t *testing.T) {
	conf := ttConf(t)
	readme := readRepoFile(t, "README.md")

	shipped, err := observerBlock(conf)
	if err != nil {
		t.Fatalf("tt.conf: %v", err)
	}
	shown, err := observerBlock(readme)
	if err != nil {
		t.Fatalf("README.md: %v", err)
	}
	if shipped != shown {
		t.Fatalf("the README's block differs from share/moox/conf/tt.conf's.\n--- tt.conf\n%s\n--- README\n%s", shipped, shown)
	}

	const placeholder = `#{?@tt_status,#{E:@tt_status} ,}`
	if line := statusRightLine(statusConf(t)); !strings.HasPrefix(line, `set -g status-right "`+placeholder) {
		t.Fatalf("status.conf's status-right does not start with the @tt_status placeholder: %q", line)
	}
	if !strings.Contains(readme, placeholder) {
		t.Fatal("the README does not show the status-right placeholder")
	}
}

// The shipped block carries none of the mistakes that broke the first design,
// each of which was measured on a real server before it was a rule here.
func TestTheShippedBlockCarriesNoneOfTheMeasuredMistakes(t *testing.T) {
	conf := ttConf(t)
	block, err := observerBlock(conf)
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, l := range strings.Split(block, "\n") {
		if trimmed := strings.TrimSpace(l); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			code = append(code, l)
		}
	}
	body := strings.Join(code, "\n")

	// Shipped ON, everywhere. This was `off` until the user stated the intent
	// plainly — "the idea was that panel is visible across all sessions" — and
	// had been running the live server at `all` for a day. It is his own
	// dotfiles repository: the conservative default was protecting nobody.
	if !strings.Contains(body, "set -g @tt_obs_scope all") {
		t.Error("the block does not ship @tt_obs_scope all")
	}
	// run-shell segfaulted the server on 2026-08-16 and is banned by name.
	if strings.Contains(body, "run-shell") {
		t.Error("the block uses run-shell")
	}
	// An unquoted `#` in a command line starts a COMMENT: `-l #{…}` became
	// "-l expects an argument" (P10, measured). The width must be quoted.
	if regexp.MustCompile(`-l\s+#`).MatchString(body) {
		t.Error("the block passes an unquoted format to -l, which tmux parses as a comment")
	}
	// Every split takes its width from the option, quoted. Both live inside
	// single-quoted command aliases now, so both spell it with double quotes.
	if n := strings.Count(body, `-l "#{@tt_obs_width}"`); n < 2 {
		t.Errorf("%d splits take a quoted @tt_obs_width, want the reconcile's and the toggle's", n)
	}
	// Hooks sit in tt's fixed slot [104] (moox's slot table, architecture
	// §5.1): an unindexed `set-hook -g` clears everybody else's hook of the
	// same name, and an indexed one replaces itself on a re-source instead of
	// appending a second copy.
	for _, hook := range []string{"session-window-changed", "client-session-changed", "client-attached", "window-resized"} {
		if !regexp.MustCompile(`(?m)^set-hook -g '` + hook + `\[104\]'\s+tt-obs-reconcile$`).MatchString(body) {
			t.Errorf("the %s hook is not in slot [104]", hook)
		}
	}
	// A sidebar must never hold a window open once the user's own panes have
	// gone. window-pane-changed is the hook that resolves `:.` to the window
	// whose pane went (measured); pane-exited covers the case where the pane
	// that ends was not the active one.
	for _, hook := range []string{"pane-exited", "window-pane-changed"} {
		if !regexp.MustCompile(`(?m)^set-hook -g '` + hook + `\[104\]'\s+tt-obs-lastpane$`).MatchString(body) {
			t.Errorf("the %s hook is not in slot [104]", hook)
		}
	}
	// A layout is the one thing the sidebar cannot infer from its own size, so
	// tmux writes it down and the renderer reads it.
	if !regexp.MustCompile(`(?m)^set-hook -g 'after-select-layout\[104\]'\s+tt-obs-relayout$`).MatchString(body) {
		t.Error("the after-select-layout hook is not in slot [104]")
	}
	// `next-layout` is the one layout command tmux gives no after-hook
	// (measured: "invalid option: after-next-layout"), so prefix Space runs
	// the equivalent that does fire one.
	// In a tt popup it detaches instead (panel_cursor.go): the popup's session
	// shares the real windows, so cycling there re-lays the one behind it.
	if !regexp.MustCompile(`(?m)^bind-key Space tt-popup-space$`).MatchString(body) ||
		!regexp.MustCompile(`(?m)^set -s command-alias\[\d+\] 'tt-popup-space=if-shell -F "#\{m:tt-popup-\*,#\{session_name\}\}" \{ detach-client \} \{ select-layout -n \}'$`).MatchString(body) {
		t.Error("prefix Space does not run select-layout -n outside a tt popup, so cycling layouts is not marked")
	}
	if strings.Contains(body, "after-next-layout") {
		t.Error("the block hooks after-next-layout, which tmux 3.7c rejects")
	}
	if !strings.Contains(body, `'tt-obs-relayout=if-shell -F "#{==:#{@tt_obs_relayout},}" { set-option -w @tt_obs_relayout 1 }'`) {
		t.Error("the relayout marker does not defer to a value already held, so a re-dock would trigger itself")
	}
	if regexp.MustCompile(`(?m)^set-hook -g [a-z-]+ `).MatchString(body) {
		t.Error("an unindexed set-hook -g would clear other plugins' hooks")
	}
	if strings.Contains(body, "set-hook -ga") || strings.Contains(body, "%if") {
		t.Error("the hooks are appended behind a %if sentinel again; slot [104] replaces that")
	}
	// No creation hook: window-linked / after-new-window / session-created fire
	// while a restore is still creating the saved panes.
	for _, hook := range []string{"window-linked", "after-new-window", "session-created"} {
		if strings.Contains(body, "set-hook -g '"+hook) {
			t.Errorf("the block hooks %s, which fires mid-restore", hook)
		}
	}
	// The sidebar holds off for the whole of a moox restore (RR9).
	if !strings.Contains(body, "#{!=:#{@moox-restoring},}") {
		t.Error("@tt_obs_hold does not include #{@moox-restoring}")
	}
	// The renderer runs under a persistent /bin/sh (…; exit) so a save sees
	// `tt observe --pane` as the pane's command and never restores it, and it
	// starts the tt moox ships ($MOOX_BIN, kept literal for the pane's shell,
	// because split-window does not format-expand its command).
	// Both launch sites — the reconcile alias and the binding — start an
	// --auto pane now that the toggle is global: a pane the scope created is a
	// pane the scope may retire, and the binding IS the scope's switch. A
	// manual `tt observe --pane` is still supported for a hand-made split; it
	// is simply not what the key press makes.
	if n := strings.Count(body, `/bin/sh -c "\${MOOX_BIN:+\$MOOX_BIN/}tt observe --pane --auto; exit"`); n < 2 {
		t.Errorf("%d launch sites run the shipped renderer under /bin/sh with a trailing exit, want 2", n)
	}
	if strings.Contains(conf, "resurrect") && strings.Contains(body, "resurrect") {
		t.Error("tt.conf still carries tmux-resurrect wiring; the moox engine replaces it")
	}
}

// Code review round 2, G2: scope="sessions" matched with the session name on
// the PATTERN side of a glob — `#{m:* #{session_name} *, …}` — so `al*` was in
// scope when "alpha" was listed and `w[1]` was not in scope when it was. The
// live gate's case 20 measures the behaviour; this pins the text, hermetically,
// so the unescaped form cannot come back without failing here first.
func TestTheSessionsScopeMatchesTheNameExactlyNotAsAGlob(t *testing.T) {
	conf := ttConf(t)
	block, err := observerBlock(conf)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^set -g @tt_obs_in_scope '(.*)'$`).FindStringSubmatch(block)
	if m == nil {
		t.Fatal("the block does not define @tt_obs_in_scope")
	}
	predicate := m[1]
	// The name is escaped with q: before it becomes part of a pattern (tmux
	// 3.7c, measured: q: backslash-escapes * ? [ and space, and fnmatch reads
	// a backslash as "the next character, literally").
	if !strings.Contains(predicate, "#{m:* #{q:session_name} *, #{@tt_obs_sessions} }") {
		t.Errorf("the sessions match does not escape the name with q: — %s", predicate)
	}
	if strings.Contains(predicate, "#{session_name} *") {
		t.Errorf("an unescaped session name is still used as a pattern — %s", predicate)
	}
	// A name with a space would match two adjacent listed words, so it is
	// never in scope.
	if !strings.Contains(predicate, "#{!:#{m:* *,#{session_name}}}") {
		t.Errorf("a session name containing a space is not excluded — %s", predicate)
	}
	// Balanced, or tmux silently expands it to something else.
	if strings.Count(predicate, "#{") != strings.Count(predicate, "}") {
		t.Errorf("the predicate's braces do not balance: %d '#{' vs %d '}'", strings.Count(predicate, "#{"), strings.Count(predicate, "}"))
	}
}

// The reported bug, as text: the toggle is GLOBAL. Closing the sidebar in one
// window used to set a WINDOW option (@tt_obs_dismissed) and leave the other
// 35 sidebars exactly where they were. Both branches of the binding now write
// the server-wide @tt_obs_scope, and nothing in the block carries a per-window
// dismissal any more.
func TestTheToggleTurnsThePanelOnAndOffForTheWholeServer(t *testing.T) {
	conf := ttConf(t)
	block, err := observerBlock(conf)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^set -s command-alias\[\d+\] 'tt-obs-toggle=(.*)'$`).FindStringSubmatch(block)
	if m == nil {
		t.Fatal("the block does not define tt-obs-toggle")
	}
	binding := m[1]
	// Two keys, one definition: bind-key expands an alias at BIND time
	// (measured), so neither key can drift from the other. `g` is the short
	// one the user asked for — every other lowercase letter on this config is
	// already bound — and C-o stays for the fingers that learned it.
	for _, key := range []string{"g", "C-o"} {
		if !regexp.MustCompile(`(?m)^bind-key ` + regexp.QuoteMeta(key) + `\s+tt-obs-toggle$`).MatchString(block) {
			t.Errorf("prefix %s does not run tt-obs-toggle", key)
		}
	}
	if regexp.MustCompile(`(?m)^bind-key \S+ if-shell`).MatchString(block) {
		t.Error("a key is bound to its own copy of the toggle instead of the alias")
	}

	// Off -> on restores the previous scope (so a `sessions` setting survives a
	// toggle pair) and defaults to `all`.
	if !strings.Contains(binding, `set-option -gF @tt_obs_scope "#{?@tt_obs_scope_prev,#{@tt_obs_scope_prev},all}"`) {
		t.Errorf("the binding does not turn the scope back on globally:\n%s", binding)
	}
	// On -> off remembers what it turned off, then turns it off globally.
	if !strings.Contains(binding, `set-option -gF @tt_obs_scope_prev "#{@tt_obs_scope}"`) ||
		!strings.Contains(binding, `set-option -g @tt_obs_scope off`) {
		t.Errorf("the binding does not remember the scope it turned off:\n%s", binding)
	}
	// -gF, never a plain -g: tmux stores a command argument literally unless
	// the option write is told to expand it (measured, 3.7c).
	if regexp.MustCompile(`set-option -g @tt_obs_scope(_prev)? ["']#\{`).MatchString(binding) {
		t.Errorf("a format is written to an option without -F, so it would be stored literally:\n%s", binding)
	}
	// Per-window dismissal is gone. A window option that outlives the toggle
	// that set it is the C1 defect class: stale state describing a pane, or a
	// window, that has moved on.
	if strings.Contains(block, "@tt_obs_dismissed") {
		t.Error("the block still carries a per-window dismissal, which the global toggle replaced")
	}
	// The toggle must not create a sidebar a hook would refuse: the width rule
	// is the only thing between a narrow window and a 1-column pane.
	if !strings.Contains(binding, "#{E:@tt_obs_want}") {
		t.Errorf("turning the panel on ignores @tt_obs_want (scope and width):\n%s", binding)
	}
}

// The last-pane guard, as text. Its whole safety argument is the predicate: a
// hook whose `:.` resolves to the wrong window must be able to do NOTHING, so
// the pane being killed has to be an observer itself and the only kind of pane
// in its own window.
func TestTheLastPaneGuardCanOnlyEverKillAnObserverInASidebarOnlyWindow(t *testing.T) {
	conf := ttConf(t)
	block, err := observerBlock(conf)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^set -s command-alias\[\d+\] 'tt-obs-lastpane=(.*)'$`).FindStringSubmatch(block)
	if m == nil {
		t.Fatal("the block does not define tt-obs-lastpane")
	}
	alias := m[1]
	if !strings.HasPrefix(alias, `if-shell -F -t ":.{right}" `) {
		t.Errorf("the guard does not evaluate its predicate on the pane it would kill: %s", alias)
	}
	// `auto` on the pane being killed, `is` on the window's panes: a sidebar
	// the hooks created only ever exists beside the user's work, while a manual
	// `tt observe --pane` may be the whole point of its window.
	if !strings.Contains(alias, `#{&&:#{E:@tt_obs_auto},#{==:#{P:#{?#{E:@tt_obs_is},,x}},}}`) {
		t.Errorf("the predicate is not `this pane is an automatic observer AND every pane here is an observer`: %s", alias)
	}
	// A sidebar re-docking itself is parked, for a moment, in a window of its
	// own. Without this the guard would close that window under it.
	if !strings.Contains(alias, `#{!=:#{@tt_obs_moving},1}`) {
		t.Errorf("the guard does not exempt a pane that is mid-redock: %s", alias)
	}
	if !strings.Contains(alias, `kill-pane -t ":.{right}"`) {
		t.Errorf("the guard kills something other than the pane it tested: %s", alias)
	}
	// One command, one target. A second kill-pane, or a kill-window, would put
	// a window the predicate never described inside the blast radius.
	if n := strings.Count(alias, "kill-"); n != 1 {
		t.Errorf("the guard issues %d kill commands, want exactly 1: %s", n, alias)
	}
	if strings.Contains(alias, "kill-window") || strings.Contains(alias, "kill-session") {
		t.Errorf("the guard kills more than a pane: %s", alias)
	}
	// The aliases must not collide: nested command-alias expansion does NOT
	// work (measured: "unknown command: <alias>" inside an if-shell block), so
	// each one is self-contained and each index is used once.
	idx := regexp.MustCompile(`(?m)^set -s command-alias\[(\d+)\]`).FindAllStringSubmatch(block, -1)
	seen := map[string]bool{}
	for _, m := range idx {
		if seen[m[1]] {
			t.Errorf("command-alias[%s] is defined twice", m[1])
		}
		seen[m[1]] = true
	}
	if len(idx) < 2 {
		t.Errorf("the block defines %d command aliases, want the reconcile and the last-pane guard", len(idx))
	}
}
