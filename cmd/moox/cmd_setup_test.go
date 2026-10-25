package main

// Setup and uninstall over a FAKE HOME (§13 P7 gate): every test builds its
// own scratch HOME/XDG tree and runs the verb in process with an explicit
// environment, so nothing reaches the user's real rc files, ~/.claude or
// tmux servers (TMUX is never set; TMUX_TMPDIR points at an empty dir).

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/setup/manifest"
)

type fakeHome struct {
	t                 *testing.T
	root              string
	home, xc, xs, bin string
	share             string
	shell             string
	path              string
	extra             []string
}

func newFakeHome(t *testing.T) *fakeHome {
	t.Helper()
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	h := &fakeHome{t: t, root: root, home: filepath.Join(root, "home"), xc: filepath.Join(root, "xc"),
		xs: filepath.Join(root, "xs"), bin: filepath.Join(root, "bin"), shell: "/bin/zsh"}
	h.path = h.bin + ":/usr/bin:/bin"
	for _, d := range []string{h.home, h.xc, h.xs, h.bin, filepath.Join(root, "tmux-tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	share, err := mooxroot.DevShare(".")
	if err != nil {
		t.Fatal(err)
	}
	h.share = share
	return h
}

func (h *fakeHome) environ() []string {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		h.t.Skip("tmux not installed")
	}
	env := []string{
		"HOME=" + h.home, "XDG_CONFIG_HOME=" + h.xc, "XDG_STATE_HOME=" + h.xs,
		"XDG_DATA_HOME=" + filepath.Join(h.root, "xd"), "SHELL=" + h.shell,
		"PATH=" + h.path, "MOOX_ROOT=" + h.share, "MOOX_TMUX_BIN=" + tmux,
		"TMUX_TMPDIR=" + filepath.Join(h.root, "tmux-tmp"), "LANG=en_US.UTF-8",
	}
	return append(env, h.extra...)
}

// run runs one verb in process; stdin/stdout are not terminals.
func (h *fakeHome) run(args ...string) (int, string) {
	h.t.Helper()
	var out bytes.Buffer
	m := newMeta(context.Background(), h.environ())
	m.stdin, m.stdout, m.stderr = strings.NewReader(""), &out, &out
	m.stdinTTY, m.stdoutTTY = false, false
	m.cwd = h.home
	code := run(m, args)
	return code, out.String()
}

func (h *fakeHome) write(rel, content string, mode os.FileMode) string {
	h.t.Helper()
	p := filepath.Join(h.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *fakeHome) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, rel))
	if err != nil {
		h.t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func (h *fakeHome) fakeJq() {
	h.write("bin/jq", "#!/bin/sh\nexit 0\n", 0o755)
}

// tree is every entry under the fake root (but tmux-tmp): type, mode,
// content or link target.
func (h *fakeHome) tree() map[string]string {
	h.t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(h.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(h.root, p)
		if rel == "tmux-tmp" {
			return filepath.SkipDir
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			out[rel] = "link " + tgt
		case fi.IsDir():
			out[rel] = "dir " + fi.Mode().Perm().String()
		default:
			b, _ := os.ReadFile(p)
			out[rel] = "file " + fi.Mode().Perm().String() + " " + string(b)
		}
		return nil
	})
	return out
}

func diffTrees(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			d = append(d, "- "+k)
		} else if v != w {
			d = append(d, "~ "+k+": "+firstN(v)+" → "+firstN(w))
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "+ "+k)
		}
	}
	sort.Strings(d)
	return d
}

func firstN(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func (h *fakeHome) manifest() *manifest.Manifest {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.xc, "moox", "setup.json"))
	if err != nil {
		h.t.Fatalf("no manifest: %v", err)
	}
	m, err := manifest.Decode(b)
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

func (h *fakeHome) zshLine() string {
	return `[[ -r "` + h.share + `/shell/moox.zsh" ]] && source "` + h.share + `/shell/moox.zsh"  # moox`
}

func mustCode(t *testing.T, code int, out string, want int) {
	t.Helper()
	if code != want {
		t.Fatalf("exit %d, want %d; output:\n%s", code, want, out)
	}
}

func TestSetup_NoConfig(t *testing.T) {
	h := newFakeHome(t)
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, 0)
	conf := h.read("home/.tmux.conf")
	if !strings.Contains(conf, "moox-managed") || !strings.Contains(conf, `source-file "`+h.share+`/moox.conf"`) {
		t.Fatalf("~/.tmux.conf:\n%s", conf)
	}
	if local := h.read("xc/moox/local.conf"); !strings.Contains(local, "local.conf") || strings.Contains(local, `@moox-sockets "moox"  # moox`) {
		t.Fatalf("local.conf:\n%s", local)
	}
	if rc := h.read("home/.zshrc"); rc != h.zshLine()+"\n" {
		t.Fatalf("~/.zshrc = %q", rc)
	}
	m := h.manifest()
	n := 0
	for _, a := range m.Actions {
		if a.Kind != manifest.KindMkdir {
			n++
		}
	}
	if m.Mode != "replace" || n != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, want := range []string{"prefix r", "prefix C-s", "prefix g", "prefix Space", "prefix T", "doctor:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSetup_ExistingSeparate(t *testing.T) {
	h := newFakeHome(t)
	h.write("home/.tmux.conf", "set -g prefix C-b\n", 0o644)
	before := h.tree()["home/.tmux.conf"]
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, 0)
	if got := h.tree()["home/.tmux.conf"]; got != before {
		t.Fatalf("separate mode changed ~/.tmux.conf: %q → %q", before, got)
	}
	if local := h.read("xc/moox/local.conf"); !strings.Contains(local, `set -g @moox-sockets "moox"`) {
		t.Fatalf("local.conf has no sockets line:\n%s", local)
	}
	if m := h.manifest(); m.Mode != "separate" {
		t.Fatalf("mode = %q", m.Mode)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.xc, "moox", "backups")); len(entries) != 0 {
		t.Fatalf("separate mode made backups: %v", entries)
	}
}

func TestSetup_ExistingReplace(t *testing.T) {
	h := newFakeHome(t)
	h.write("home/.tmux.conf", "set -g prefix C-b\n", 0o644)
	h.write("xc/tmux/tmux.conf", "set -g mouse on\n", 0o600)
	code, out := h.run("setup", "--yes", "--mode", "replace")
	mustCode(t, code, out, 0)
	if conf := h.read("home/.tmux.conf"); !strings.Contains(conf, "moox-managed") {
		t.Fatalf("~/.tmux.conf not replaced:\n%s", conf)
	}
	if _, err := os.Lstat(filepath.Join(h.xc, "tmux", "tmux.conf")); err == nil {
		t.Fatal("XC/tmux/tmux.conf still in place")
	}
	m := h.manifest()
	var backups []manifest.Action
	for _, a := range m.Actions {
		if a.Kind == manifest.KindBackup {
			backups = append(backups, a)
		}
	}
	if len(backups) != 2 {
		t.Fatalf("backups = %+v", backups)
	}
	for _, b := range backups {
		got, err := os.ReadFile(b.Backup)
		if err != nil {
			t.Fatal(err)
		}
		want := "set -g prefix C-b\n"
		if strings.HasSuffix(b.Path, "tmux/tmux.conf") {
			want = "set -g mouse on\n"
		}
		if string(got) != want || !strings.HasPrefix(b.Backup, filepath.Join(h.xc, "moox", "backups")) {
			t.Fatalf("backup %s = %q", b.Backup, got)
		}
	}
}

func TestSetup_ZshrcSymlink(t *testing.T) {
	h := newFakeHome(t)
	target := h.write("dotfiles/zshrc", "export EDITOR=vi\n", 0o644)
	if err := os.Symlink(target, filepath.Join(h.home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	before := h.tree()
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, 0)
	after := h.tree()
	if after["home/.zshrc"] != before["home/.zshrc"] || after["dotfiles/zshrc"] != before["dotfiles/zshrc"] {
		t.Fatalf("a symlinked ~/.zshrc (or its target) was written:\n%v", diffTrees(before, after))
	}
	if !strings.Contains(out, "symlink") || !strings.Contains(out, h.zshLine()) {
		t.Fatalf("setup did not print the line to add by hand:\n%s", out)
	}
	for _, a := range h.manifest().Actions {
		if strings.HasSuffix(a.Path, ".zshrc") {
			t.Fatalf("manifest records a write to the symlinked rc: %+v", a)
		}
	}
}

func TestSetup_Bash(t *testing.T) {
	h := newFakeHome(t)
	h.shell = "/bin/bash"
	h.write("home/.bashrc", "alias ll='ls -l'\n", 0o644)
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, 0)
	want := "alias ll='ls -l'\n" + `[ -r "` + h.share + `/shell/moox.bash" ] && . "` + h.share + `/shell/moox.bash"  # moox` + "\n"
	if rc := h.read("home/.bashrc"); rc != want {
		t.Fatalf("~/.bashrc = %q, want %q", rc, want)
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".zshrc")); err == nil {
		t.Fatal("bash setup created ~/.zshrc")
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".bash_profile")); err == nil {
		t.Fatal("bash setup created ~/.bash_profile")
	}
}

func TestSetup_BashProfile(t *testing.T) {
	t.Run("profile-without-bashrc", func(t *testing.T) {
		h := newFakeHome(t)
		h.shell = "/bin/bash"
		h.write("home/.bashrc", "# rc\n", 0o644)
		h.write("home/.bash_profile", "export PATH=$HOME/bin:$PATH\n", 0o644)
		code, out := h.run("setup", "--yes")
		mustCode(t, code, out, 0)
		for _, f := range []string{"home/.bashrc", "home/.bash_profile"} {
			if !strings.Contains(h.read(f), "/shell/moox.bash") {
				t.Errorf("%s has no hook line:\n%s", f, h.read(f))
			}
		}
	})
	t.Run("profile-sources-bashrc", func(t *testing.T) {
		h := newFakeHome(t)
		h.shell = "/bin/bash"
		h.write("home/.bashrc", "# rc\n", 0o644)
		h.write("home/.bash_profile", "[ -f ~/.bashrc ] && . ~/.bashrc\n", 0o644)
		code, out := h.run("setup", "--yes")
		mustCode(t, code, out, 0)
		if !strings.Contains(h.read("home/.bashrc"), "/shell/moox.bash") {
			t.Error("~/.bashrc has no hook line")
		}
		if strings.Contains(h.read("home/.bash_profile"), "moox") {
			t.Error("~/.bash_profile sources ~/.bashrc and still got the line")
		}
	})
}

func TestSetup_NotZshOrBash(t *testing.T) {
	h := newFakeHome(t)
	h.shell = "/usr/local/bin/fish"
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, 0)
	if !strings.Contains(out, "continuity resume needs zsh or bash; layouts and cwd still restore") {
		t.Fatalf("no shell message:\n%s", out)
	}
	for _, f := range []string{".zshrc", ".bashrc", ".bash_profile"} {
		if _, err := os.Lstat(filepath.Join(h.home, f)); err == nil {
			t.Errorf("fish setup wrote ~/%s", f)
		}
	}
}

func TestSetup_AlreadySourcedIndirectly(t *testing.T) {
	h := newFakeHome(t)
	// This Mac's shape: ~/.zshrc sources the dotfiles' zshrc.package, which
	// sources moox.zsh (PR-L5).
	h.write("home/dotfiles/zsh/zshrc.package", "[[ -f /opt/homebrew/opt/moox/share/moox/shell/moox.zsh ]] && source /opt/homebrew/opt/moox/share/moox/shell/moox.zsh\n", 0o644)
	h.write("home/.zshrc", "export ZSH=x\nsource \"$HOME/dotfiles/zsh/zshrc.package\"\n", 0o644)
	before := h.read("home/.zshrc")
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, 0)
	if got := h.read("home/.zshrc"); got != before {
		t.Fatalf("~/.zshrc changed although zshrc.package already loads the hook:\n%s", got)
	}
	if !strings.Contains(out, "already: append to "+filepath.Join(h.home, ".zshrc")) {
		t.Fatalf("not reported as already:\n%s", out)
	}
}

// The dotfiles' zshrc.local.template sources its repo through a default:
// `source "${DOTFILES:-$HOME/dotfiles}/zsh/zshrc.package"`. Both branches of
// that default must be followed, or setup appends a second hook line.
func TestSetup_AlreadySourcedThroughDefaultVar(t *testing.T) {
	hook := "[[ -f /opt/homebrew/opt/moox/share/moox/shell/moox.zsh ]] && source /opt/homebrew/opt/moox/share/moox/shell/moox.zsh\n"
	rc := "source \"${DOTFILES:-$HOME/dotfiles}/zsh/zshrc.package\"\n"
	cases := []struct {
		name, pkg string
		extra     func(h *fakeHome) []string
	}{
		{"unset takes the word", "home/dotfiles/zsh/zshrc.package", func(*fakeHome) []string { return nil }},
		{"set takes the value", "elsewhere/zsh/zshrc.package", func(h *fakeHome) []string {
			return []string{"DOTFILES=" + filepath.Join(h.root, "elsewhere")}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeHome(t)
			h.extra = c.extra(h)
			h.write(c.pkg, hook, 0o644)
			h.write("home/.zshrc", rc, 0o644)
			code, out := h.run("setup", "--yes")
			mustCode(t, code, out, 0)
			if got := h.read("home/.zshrc"); got != rc {
				t.Fatalf("~/.zshrc changed although zshrc.package already loads the hook:\n%s", got)
			}
			if !strings.Contains(out, "already: append to "+filepath.Join(h.home, ".zshrc")) {
				t.Fatalf("not reported as already:\n%s", out)
			}
		})
	}
}

func TestSetup_DryRunWritesNothing(t *testing.T) {
	h := newFakeHome(t)
	h.write("home/.tmux.conf", "set -g prefix C-b\n", 0o644)
	h.write("home/.zshrc", "# z\n", 0o644)
	h.write("home/.claude/settings.json", `{"theme": "dark"}`+"\n", 0o644)
	h.fakeJq()
	before := h.tree()
	code, out := h.run("setup", "--dry-run", "--yes", "--mode", "replace", "--claude-hooks")
	mustCode(t, code, out, 0)
	if d := diffTrees(before, h.tree()); len(d) != 0 {
		t.Fatalf("--dry-run wrote:\n%s", strings.Join(d, "\n"))
	}
	for _, want := range []string{"back up " + filepath.Join(h.home, ".tmux.conf"), "write " + filepath.Join(h.home, ".tmux.conf"),
		"append to " + filepath.Join(h.home, ".zshrc"), "add the Claude Code hooks"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not list %q:\n%s", want, out)
		}
	}
}

func TestSetup_Idempotent(t *testing.T) {
	h := newFakeHome(t)
	h.write("home/.zshrc", "# z\n", 0o644)
	h.write("home/.claude/settings.json", `{"theme": "dark"}`+"\n", 0o644)
	h.fakeJq()
	code, out := h.run("setup", "--yes", "--claude-hooks")
	mustCode(t, code, out, 0)
	first := h.tree()
	code, out = h.run("setup", "--yes", "--claude-hooks")
	mustCode(t, code, out, 0)
	if d := diffTrees(first, h.tree()); len(d) != 0 {
		t.Fatalf("a second setup changed files:\n%s", strings.Join(d, "\n"))
	}
	if !strings.Contains(out, "already set up") || strings.Contains(out, "done:") {
		t.Fatalf("second run is not a no-op:\n%s", out)
	}
}

func TestSetup_ClaudeHooksNeedJq(t *testing.T) {
	h := newFakeHome(t)
	h.path = h.bin + ":/bin" // macOS ships /usr/bin/jq; keep it off this PATH
	h.write("home/.claude/settings.json", `{
  "theme": "dark",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "bash ~/.tmux/plugins/tmux-claude-continuity/scripts/on_session_start.sh"}]},
      {"matcher": "startup", "hooks": [{"type": "command", "command": "bash other.sh"}]}
    ]
  },
  "statusLine": {"type": "command", "command": "x"}
}
`, 0o644)
	code, out := h.run("setup", "--yes", "--claude-hooks")
	mustCode(t, code, out, 0)
	if !strings.Contains(out, "jq is not on PATH") {
		t.Fatalf("no jq warning without jq:\n%s", out)
	}
	got := h.read("home/.claude/settings.json")
	start := "bash '" + h.share + "/plugins/tmux-claude-continuity/scripts/on_session_start.sh'"
	stop := "bash '" + h.share + "/plugins/tmux-claude-continuity/scripts/on_stop.sh'"
	if strings.Count(got, "on_session_start.sh") != 1 || !strings.Contains(got, start) || !strings.Contains(got, stop) {
		t.Fatalf("hooks not merged exactly once:\n%s", got)
	}
	if !strings.Contains(got, `"bash other.sh"`) || !strings.Contains(got, `"theme": "dark"`) ||
		strings.Index(got, `"theme"`) > strings.Index(got, `"hooks"`) || !strings.Contains(got, `"statusLine"`) {
		t.Fatalf("other settings were lost or reordered:\n%s", got)
	}
	if fi, _ := os.Stat(filepath.Join(h.home, ".claude", "settings.json")); fi.Mode().Perm() != 0o644 {
		t.Fatalf("settings.json mode = %v", fi.Mode().Perm())
	}

	h2 := newFakeHome(t)
	h2.write("home/.claude/settings.json", "{}\n", 0o600)
	h2.fakeJq()
	code, out = h2.run("setup", "--yes", "--claude-hooks")
	mustCode(t, code, out, 0)
	if strings.Contains(out, "jq is not on PATH") {
		t.Fatalf("jq warning although jq is on PATH:\n%s", out)
	}
}

func TestUninstall_RestoresByteIdentical(t *testing.T) {
	h := newFakeHome(t)
	h.write("home/.tmux.conf", "# mine\nset -g prefix C-b", 0o644) // no trailing newline
	tgt := h.write("dotfiles/tmux.conf", "set -g mouse on\n", 0o644)
	if err := os.MkdirAll(filepath.Join(h.xc, "tmux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tgt, filepath.Join(h.xc, "tmux", "tmux.conf")); err != nil {
		t.Fatal(err)
	}
	h.write("home/.zshrc", "export A=1", 0o644) // no trailing newline
	h.write("home/.claude/settings.json", `{"theme":"dark","hooks":{"Stop":[{"hooks":[{"type":"command","command":"bash ~/.tmux/plugins/tmux-claude-continuity/scripts/on_stop.sh"}]}]}}`, 0o644)
	h.fakeJq()
	before := h.tree()

	code, out := h.run("setup", "--yes", "--mode", "replace", "--claude-hooks", "--theme", "dark")
	mustCode(t, code, out, 0)
	if d := diffTrees(before, h.tree()); len(d) < 5 {
		t.Fatalf("setup changed too little to make the test meaningful:\n%s", strings.Join(d, "\n"))
	}
	code, out = h.run("uninstall", "--yes")
	mustCode(t, code, out, 0)
	if d := diffTrees(before, h.tree()); len(d) != 0 {
		t.Fatalf("uninstall is not byte-identical:\n%s\noutput:\n%s", strings.Join(d, "\n"), out)
	}

	// A file the user changed after setup is kept, and said so.
	code, out = h.run("setup", "--yes", "--mode", "replace")
	mustCode(t, code, out, 0)
	h.write("xc/moox/local.conf", "set -g mouse off\n", 0o600)
	code, out = h.run("uninstall", "--yes")
	mustCode(t, code, out, 0)
	if !strings.Contains(out, "kept "+filepath.Join(h.xc, "moox", "local.conf")) || h.read("xc/moox/local.conf") != "set -g mouse off\n" {
		t.Fatalf("an edited local.conf was not kept:\n%s", out)
	}
}

func TestSetup_TmuxTooOldExits69(t *testing.T) {
	h := newFakeHome(t)
	h.write("bin/oldtmux", "#!/bin/sh\necho 'tmux 3.4'\n", 0o755)
	h.extra = []string{"MOOX_TMUX_BIN=" + filepath.Join(h.bin, "oldtmux")}
	code, out := h.run("setup", "--yes")
	mustCode(t, code, out, exitTmuxAbsent)
	if _, err := os.Lstat(filepath.Join(h.home, ".tmux.conf")); err == nil {
		t.Fatal("setup wrote with tmux 3.4")
	}
}
