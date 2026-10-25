package tmuxtest

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"moox-test-1-2", "moox-val-a", "moox-test-cold-3"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "default", "work", "moox-tes", "moox-test-/x", "a moox-test-", "moox-val- x"} {
		if err := CheckName(bad); err == nil {
			t.Errorf("CheckName(%q) accepted", bad)
		}
	}
}

func TestCheckColdSocket(t *testing.T) {
	root := t.TempDir()
	if err := CheckColdSocket(filepath.Join(root, "moox-val-a"), root); err != nil {
		t.Fatalf("a socket under the root was refused: %v", err)
	}
	label, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, "moox-test-x")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"named default":    {filepath.Join(root, "default"), root},
		"outside the root": {filepath.Join(t.TempDir(), "s"), root},
		"the root itself":  {root, root},
		"tmux's own dir":   {label, filepath.Dir(label)},
		"/tmp/tmux-uid":    {filepath.Join("/tmp", "tmux-"+strconv.Itoa(os.Getuid()), "s"), "/tmp"},
		"over sockaddr_un": {filepath.Join(root, strings.Repeat("x", 110)), root},
		"dot-dot escape":   {filepath.Join(root, "..", "s"), root},
	}
	for name, c := range cases {
		if err := CheckColdSocket(c[0], c[1]); err == nil {
			t.Errorf("%s: %q accepted", name, c[0])
		}
	}
	t.Run("TMUX_TMPDIR", func(t *testing.T) {
		d := t.TempDir()
		t.Setenv("TMUX_TMPDIR", d)
		if err := CheckColdSocket(filepath.Join(d, "s"), d); err == nil {
			t.Fatal("a socket under $TMUX_TMPDIR was accepted")
		}
	})
}

func TestHookShell_RefusesOutsideTempDir(t *testing.T) {
	outside, err := os.MkdirTemp("", "mxhook")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside)
	for _, kind := range []string{"zsh", "bash"} {
		if _, err := hookShell(t, outside, kind); !errors.Is(err, ErrOutsideTempDir) {
			t.Errorf("%s fixture outside t.TempDir(): %v", kind, err)
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Errorf("%s: refused fixture still wrote %v", kind, entries)
		}
	}
}

func TestHookShell_Fixtures(t *testing.T) {
	share, err := ShareRoot()
	if err != nil {
		t.Fatal(err)
	}
	z := HookZsh(t)
	if z.Kind() != "zsh" || !strings.Contains(z.Command(), " zsh -d -i") || !strings.HasPrefix(z.Command(), "ZDOTDIR='") {
		t.Fatalf("zsh fixture: %q", z.Command())
	}
	dir := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(z.Command())[0], "ZDOTDIR='"), "'")
	rc, err := os.ReadFile(filepath.Join(dir, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "source '" + filepath.Join(share, "shell", "moox.zsh") + "'\n"; string(rc) != want {
		t.Fatalf(".zshrc = %q, want %q", rc, want)
	}
	if !within(dir, filepath.Dir(t.TempDir())) {
		t.Fatalf("zsh fixture dir %s is not under the test temp root", dir)
	}

	b := HookBash(t)
	if b.Kind() != "bash" || !strings.Contains(b.Command(), "bash --noprofile --rcfile ") || !strings.HasSuffix(b.Command(), " -i") {
		t.Fatalf("bash fixture: %q", b.Command())
	}
	var zero Shell
	if zero.Command() != "/bin/sh" || zero.Kind() != "sh" {
		t.Fatalf("zero Shell = %q", zero.Command())
	}
}

func TestWithEnv_RefusesProtectedNames(t *testing.T) {
	for _, kv := range []string{"TMUX=x", "HOME=/", "PATH=/", "XDG_STATE_HOME=/x", "TMUX_TMPDIR=/t", "ENV=/e", "noequals"} {
		c := newConfig([]Option{WithEnv(kv)})
		if c.configErr == nil {
			t.Errorf("WithEnv(%q) accepted", kv)
		}
	}
	c := newConfig([]Option{WithEnv("MOOX_STUB_COMMANDS=1", "MOOX_NO_AUTO_RESTORE=1")})
	if c.configErr != nil || len(c.env) != 2 {
		t.Fatalf("allowed env refused: %v %v", c.configErr, c.env)
	}
}

func TestScrubbedEnv(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,1,0")
	t.Setenv("TMUX_TMPDIR", "/x")
	t.Setenv("MOOX_STATE_DIR", "/real")
	t.Setenv("ENV", "/rc/.shrc")
	t.Setenv("XDG_STATE_HOME", "/real/state")
	s := &Server{Home: "/h", XS: "/s", XD: "/d", XC: "/c", Bin: "/b"}
	env := scrubbedEnv(s, []string{"MOOX_STUB_COMMANDS=1"})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Errorf("duplicate %s", k)
		}
		got[k] = v
	}
	for _, k := range []string{"TMUX", "TMUX_TMPDIR", "MOOX_STATE_DIR", "ENV"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s was passed on", k)
		}
	}
	want := map[string]string{"HOME": "/h", "XDG_STATE_HOME": "/s", "XDG_DATA_HOME": "/d", "XDG_CONFIG_HOME": "/c",
		"SHELL": "/bin/sh", "MOOX_BIN": "/b", "MOOX_STUB_COMMANDS": "1"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestWithHarnessLines_RefusesLoudRunShell(t *testing.T) {
	for _, l := range []string{
		`run-shell "x"`,
		`run-shell 'x >/dev/null 2>&1'`,
		`run 'x'`,
	} {
		if c := newConfig([]Option{WithHarnessLines(l)}); c.configErr == nil {
			t.Errorf("accepted %q", l)
		}
	}
	ok := []string{`run-shell "'/p' >/dev/null 2>&1 || true"`, `set -g @x 1`, `run-shell 'sleep 2 >/dev/null 2>&1 || true'`}
	if c := newConfig([]Option{WithHarnessLines(ok...)}); c.configErr != nil || len(c.harness) != 3 {
		t.Fatalf("silent lines refused: %v", c.configErr)
	}
}

func TestConfQuote(t *testing.T) {
	if got := ConfQuote(`a'b c`); got != `'a'\''b c'` {
		t.Fatalf("ConfQuote = %s", got)
	}
}
