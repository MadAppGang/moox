// Package share_test checks the tmux configuration moox ships (share/moox):
// the §3.1 file order, the §5.1 hook slot table, and the run-shell rules that
// keep a config load from crashing tmux (RR12). These are static checks over
// the files; conf_e2e_test.go (tag tmuxe2e) loads them into an isolated server.
package share_test

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/audit"
)

// shareDir is share/moox of this tree (the test runs in moox/share).
const shareDir = "moox"

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(shareDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// codeLines are the non-blank, non-comment lines, trimmed, with their 1-based
// line numbers.
type codeLine struct {
	n    int
	text string
}

func codeLines(content string) []codeLine {
	var out []codeLine
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, codeLine{n, t})
	}
	return out
}

// confFiles are moox.conf, conf/*.conf and the vendored plugin entry scripts
// (plugins/**/*.tmux): every file whose text tmux runs at load.
func confFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(shareDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(shareDir, p)
		rel = filepath.ToSlash(rel)
		ok := rel == "moox.conf" || (strings.HasPrefix(rel, "conf/") && strings.HasSuffix(rel, ".conf")) ||
			(strings.HasPrefix(rel, "plugins/") && strings.HasSuffix(rel, ".tmux"))
		if !ok {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 8 {
		t.Fatalf("only %d config files found under %s", len(files), shareDir)
	}
	return files
}

func TestConf_EveryRunShellSilentAndTrue(t *testing.T) {
	for rel, content := range confFiles(t) {
		if f := audit.RunShellConf("share/moox/"+rel, content); len(f) != 0 {
			t.Errorf("%s:\n%s", rel, audit.Format(f))
		}
	}
}

// The entry file sources the generic files in exactly this order, then the
// user's local.conf, then runs the gate, then sources active.conf behind the
// runtime `if -F` (PR-H9, §3.1).
func TestConf_OrderPrePluginPluginsKeysTheme(t *testing.T) {
	var got []string
	for _, l := range codeLines(read(t, "moox.conf")) {
		switch {
		case strings.HasPrefix(l.text, "source-file"):
			got = append(got, strings.TrimSuffix(lastField(l.text), "'"))
		case strings.Contains(l.text, "serve --ensure"):
			got = append(got, "GATE")
		case strings.HasPrefix(l.text, "if -F '#{==:#{@moox-active},1}'"):
			got = append(got, "IF-ACTIVE "+strings.TrimSuffix(strings.TrimSuffix(lastField(l.text), "}"), "'"))
		}
	}
	want := []string{
		"'#{@moox-root}/conf/options.conf", "'#{@moox-root}/conf/plugins.conf", "'#{@moox-root}/conf/keys.conf",
		"'#{@moox-root}/conf/theme.conf", "'#{@moox-root}/conf/status.conf",
		"'#{?XDG_CONFIG_HOME,#{XDG_CONFIG_HOME},#{HOME}/.config}/moox/local.conf",
		"GATE",
		"IF-ACTIVE ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		// The active.conf source is inside the braces; check it separately.
		t.Fatalf("moox.conf order:\n got  %q\n want %q", got, want)
	}
	if !strings.Contains(read(t, "moox.conf"), `if -F '#{==:#{@moox-active},1}' { source-file -F '#{@moox-root}/conf/active.conf' }`) {
		t.Fatal("moox.conf does not source active.conf behind the runtime @moox-active check")
	}
	// tt.conf is NOT sourced by moox.conf, only by active.conf (D7).
	if strings.Contains(read(t, "moox.conf"), "tt.conf") || !strings.Contains(read(t, "conf/active.conf"), "conf/tt.conf'") {
		t.Fatal("tt.conf must be sourced from active.conf only")
	}
	// Prefix and mode-keys are set before plugins (tmux-sensible reads the prefix at load).
	opts := read(t, "conf/options.conf")
	for _, want := range []string{"set -g prefix C-a", "setw -g mode-keys vi", "# >>> theme: pre-plugin >>>", "@fingers-key F", `TMUX_FZF_LAUNCH_KEY="f"`} {
		if !strings.Contains(opts, want) {
			t.Errorf("options.conf (sourced before plugins) lacks %q", want)
		}
	}
	if strings.Contains(read(t, "conf/keys.conf"), "set -g prefix") {
		t.Error("the prefix belongs in options.conf, before the plugins")
	}
}

func lastField(s string) string {
	f := strings.Fields(s)
	return f[len(f)-1]
}

// The plugin order of §3.1: pain-control before tmux-jump (jump's binding wins),
// catppuccin after the rest of the visual plugins, copy-image last.
func TestConf_PluginOrder(t *testing.T) {
	re := regexp.MustCompile(`plugins/([^/]+)/`)
	var got []string
	for _, l := range codeLines(read(t, "conf/plugins.conf")) {
		if m := re.FindStringSubmatch(l.text); m != nil {
			got = append(got, m[1])
		}
	}
	want := []string{"tmux-sensible", "tmux-yank", "tmux-open", "tmux-pain-control", "tmux-jump", "tmux-fzf",
		"tmux-fzf-url", "vim-tmux-navigator", "catppuccin-tmux", "tmux-copy-image"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("plugins.conf order\n got  %v\n want %v", got, want)
	}
	for _, p := range want {
		if _, err := os.Stat(filepath.Join(shareDir, "plugins", p)); err != nil {
			t.Errorf("plugins.conf loads %s, which is not vendored: %v", p, err)
		}
	}
	if !strings.Contains(read(t, "conf/plugins.conf"), "tmux-fingers load-config") {
		t.Error("tmux-fingers is loaded with `tmux-fingers load-config` when present")
	}
}

// No conf line may name a home directory: everything is relative to the share
// root or comes from the environment (X13).
func TestConf_NoHardcodedHomePaths(t *testing.T) {
	bad := regexp.MustCompile(`~/|\$HOME/\.tmux|\.tmux/plugins|\.tmux/scripts|/Users/|/home/[a-z]`)
	for rel, content := range confFiles(t) {
		if strings.HasPrefix(rel, "plugins/") {
			continue // upstream entry scripts compute their own directory
		}
		for _, l := range codeLines(content) {
			if bad.MatchString(l.text) {
				t.Errorf("%s:%d names a home path: %s", rel, l.n, l.text)
			}
		}
	}
}

// RR17: the bootstrap pane of a cold start gets the history-limit that is set
// when it is created, which is after the gate; so the limit is set explicitly,
// in options.conf, which loads before the gate.
func TestConf_HistoryLimitBeforeGate(t *testing.T) {
	if !regexp.MustCompile(`(?m)^set -g history-limit [0-9]+$`).MatchString(read(t, "conf/options.conf")) {
		t.Fatal("options.conf does not set history-limit explicitly")
	}
	opt, gate := -1, -1
	for _, l := range codeLines(read(t, "moox.conf")) {
		if opt < 0 && strings.Contains(l.text, "conf/options.conf") {
			opt = l.n
		}
		if gate < 0 && strings.Contains(l.text, "serve --ensure") {
			gate = l.n
		}
	}
	if opt < 0 || gate < 0 || opt > gate {
		t.Fatal("options.conf is not sourced before the gate")
	}
}

// One config-time gate, in moox.conf, and nothing else starts tt or a restore
// from config (I3, R2-L7): no resurrect/continuum hooks or scripts survive.
func TestConf_SingleGateRunShell(t *testing.T) {
	gates := 0
	for rel, content := range confFiles(t) {
		for _, l := range codeLines(content) {
			if strings.Contains(l.text, "serve --ensure") {
				gates++
				if rel != "moox.conf" || !strings.HasPrefix(l.text, "run-shell '") {
					t.Errorf("%s:%d: the gate belongs in moox.conf as a synchronous run-shell", rel, l.n)
				}
			}
			for _, gone := range []string{"resurrect", "continuum", "pre_restore", "post_restore", "pre_save", "tpm"} {
				if strings.Contains(l.text, gone) && !strings.HasPrefix(rel, "plugins/") {
					t.Errorf("%s:%d still references %s: %s", rel, l.n, gone, l.text)
				}
			}
		}
	}
	if gates != 1 {
		t.Fatalf("want exactly one gate run-shell, found %d", gates)
	}
	if l := codeLines(read(t, "moox.conf")); countRunShell(l) != 1 {
		t.Fatalf("moox.conf has %d run-shells; the gate is the only one", countRunShell(l))
	}
}

func countRunShell(ls []codeLine) int {
	n := 0
	for _, l := range ls {
		if strings.Contains(l.text, "run-shell") {
			n++
		}
	}
	return n
}

// D10/I6/V2: no `#()` job anywhere in moox's own config — not a save trigger
// and not the host chip either (I-3: the chip is a plain `#H` format now).
func TestConf_NoStatusJobSave(t *testing.T) {
	jobs := regexp.MustCompile(`#\(([^)]*)\)`)
	for rel, content := range confFiles(t) {
		if strings.HasPrefix(rel, "plugins/") {
			continue
		}
		for _, l := range codeLines(content) {
			for _, m := range jobs.FindAllStringSubmatch(l.text, -1) {
				t.Errorf("%s:%d runs a status job: #(%s)", rel, l.n, m[1])
			}
		}
	}
	if !strings.Contains(read(t, "conf/status.conf"), "#H") {
		t.Error("status-right lost the host chip's #H")
	}
	if !strings.Contains(read(t, "conf/status.conf"), "#{?@moox-warning,") {
		t.Error("status-right does not show @moox-warning (A16)")
	}
}

// slotOwner is §5.1's table: the slot each subsystem owns, and the one file
// allowed to register it.
var slotOwner = map[int]string{
	100: "conf/theme.conf",
	101: "conf/active.conf",
	102: "conf/active.conf",
	103: "conf/active.conf",
	104: "conf/tt.conf",
	105: "plugins/tmux-copy-image/plugin/tmux-clip-image.tmux",
}

var setHook = regexp.MustCompile(`set-hook\s+((?:-[a-zA-Z]+\s+)*)(?:'|")?([a-z-]+)(?:\[([0-9]+)\])?(?:'|")?`)

// TestConf_HookIndexTableUnique: every set-hook moox loads is indexed, in a slot
// of the table, registered by the slot's owner, and no (hook, slot) pair is
// claimed twice (an unindexed `set-hook -g` clears the whole array,
// cmd-set-option.c:368).
func TestConf_HookIndexTableUnique(t *testing.T) {
	seen := map[string]string{}
	count := 0
	for rel, content := range confFiles(t) {
		for _, l := range codeLines(content) {
			for _, m := range setHook.FindAllStringSubmatch(l.text, -1) {
				flags, name, idx := m[1], m[2], m[3]
				if strings.Contains(flags, "u") {
					continue // an unset, e.g. in a cleanup path
				}
				count++
				if strings.Contains(flags, "a") || idx == "" {
					t.Errorf("%s:%d: unindexed or appending set-hook %q", rel, l.n, l.text)
					continue
				}
				slot, _ := strconv.Atoi(idx)
				owner, ok := slotOwner[slot]
				if !ok {
					t.Errorf("%s:%d: slot [%d] is not in the §5.1 table", rel, l.n, slot)
					continue
				}
				if owner != rel {
					t.Errorf("%s:%d: slot [%d] belongs to %s", rel, l.n, slot, owner)
				}
				key := name + "[" + idx + "]"
				if prev, dup := seen[key]; dup {
					t.Errorf("%s:%d: %s already claimed at %s", rel, l.n, key, prev)
				}
				seen[key] = rel + ":" + strconv.Itoa(l.n)
			}
		}
	}
	if count < 40 {
		t.Fatalf("only %d set-hook lines found; the parser missed the config", count)
	}
	var keys []string
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d hook slots: %s", len(keys), strings.Join(keys, " "))
}

// RR9: the sidebar holds off while a restore runs, and starts the shipped tt.
func TestConf_HoldIncludesRestoring(t *testing.T) {
	tt := read(t, "conf/tt.conf")
	hold := regexp.MustCompile(`(?m)^set -g @tt_obs_hold +'(.*)'$`).FindStringSubmatch(tt)
	if hold == nil || !strings.Contains(hold[1], "#{!=:#{@moox-restoring},}") {
		t.Fatalf("@tt_obs_hold does not hold while @moox-restoring is set: %v", hold)
	}
	if n := strings.Count(tt, `\${MOOX_BIN:+\$MOOX_BIN/}tt observe --pane --auto; exit`); n != 2 {
		t.Fatalf("the observer start command does not use the shipped tt in both places (%d)", n)
	}
}

// The bindings moox defines itself, in the files the order test pins.
func TestConf_KeyBindingsDeclared(t *testing.T) {
	keys, active := read(t, "conf/keys.conf"), read(t, "conf/active.conf")
	for _, want := range []string{
		`bind h split-window -v -c "#{pane_current_path}"`,
		`bind r source-file -F '#{config_files}'`,
	} {
		if !strings.Contains(keys, want) {
			t.Errorf("keys.conf lacks %q", want)
		}
	}
	for _, want := range []string{
		`bind C-s { wait-for -S moox-save`,
		`bind Z display-popup -E -w 90% -h 85% "\"\$MOOX_ROOT/plugins/tmux-claude-continuity/scripts/cc_popup.sh\""`,
	} {
		if !strings.Contains(active, want) {
			t.Errorf("active.conf lacks %q", want)
		}
	}
	if strings.Contains(keys, "bind F") || strings.Contains(keys, "bind Z") || strings.Contains(keys, "bind C-s") {
		t.Error("keys.conf must not take F (fingers) or the active-only keys Z / C-s")
	}
}
