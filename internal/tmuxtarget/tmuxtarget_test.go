package tmuxtarget_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// unescapeArg is tmux's own rule for one argv element (cmd-parse.y:1094-1110):
// a trailing ';' ends the command unless the byte before it is '\', in which
// case the backslash is replaced by ';' and the argument continues. It returns
// the argument as tmux sees it and whether it ended the command.
func unescapeArg(a string) (string, bool) {
	if !strings.HasSuffix(a, ";") {
		return a, false
	}
	a = a[:len(a)-1]
	if strings.HasSuffix(a, `\`) {
		return a[:len(a)-1] + ";", false
	}
	return a, true
}

// parseChain splits an argv the way tmux does into commands.
func parseChain(argv []string) [][]string {
	var cmds [][]string
	var cur []string
	for _, a := range argv {
		v, ends := unescapeArg(a)
		if ends {
			if v != "" {
				cur = append(cur, v)
			}
			cmds = append(cmds, cur)
			cur = nil
			continue
		}
		cur = append(cur, v)
	}
	if len(cur) > 0 {
		cmds = append(cmds, cur)
	}
	return cmds
}

func TestChain_TrailingSemicolonArg(t *testing.T) {
	cmds := [][]string{
		{"set", "-g", "@t", "x;"},
		{"set", "-g", "@u", `x\;`},
		{"set", "-g", "@v", ";"},
		{"set", "-g", "@w", "a;b"},
		{"new-window", "-n", "fix; test", "-e", "MOOX_RESUME=cd /tmp && ls; git status"},
		{"display", "-p", "ok"},
	}
	argv := tmuxtarget.Chain(cmds...)
	got := parseChain(argv)
	if !reflect.DeepEqual(got, cmds) {
		t.Fatalf("chain does not round-trip through tmux's argv rule:\n argv %q\n got  %q\n want %q", argv, got, cmds)
	}
	// Exactly len-1 separators, each a bare ";" element.
	seps := 0
	for _, a := range argv {
		if a == ";" {
			seps++
		}
	}
	if seps != len(cmds)-1 {
		t.Fatalf("got %d separators, want %d: %q", seps, len(cmds)-1, argv)
	}
	// An inner ';' is data: never escaped (Q5).
	if !containsElem(argv, "a;b") || !containsElem(argv, "fix; test") {
		t.Fatalf("an inner ';' was rewritten: %q", argv)
	}
}

func TestChain_DropsEmptyCommands(t *testing.T) {
	got := tmuxtarget.Chain(nil, []string{"a"}, []string{}, []string{"b"})
	if want := []string{"a", ";", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func FuzzChainRoundTrip(f *testing.F) {
	for _, s := range []string{"", ";", "x;", `x\;`, `\`, `;;`, `a\;;`, "a;b"} {
		f.Add(s, "tail")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		cmds := [][]string{{"set", a}, {"set", b}}
		got := parseChain(tmuxtarget.Chain(cmds...))
		if !reflect.DeepEqual(got, cmds) {
			t.Fatalf("%q, %q: got %q", a, b, got)
		}
	})
}

// expandHashes models tmux's format expander for runs of '#', as measured on
// 3.8 (TestE2E_LiteralRoundTrip): a run immediately before '[' is copied
// verbatim (a style marker); any other run is halved, and an ODD run ends in a
// '#' that starts an expansion (#{, #(, #S …), which a Literal must never
// contain — reported as ok=false.
func expandHashes(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '#' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '#' {
			j++
		}
		n := j - i
		switch {
		case j < len(s) && s[j] == '[':
			b.WriteString(s[i:j])
		case n%2 == 1:
			return "", false
		default:
			b.WriteString(strings.Repeat("#", n/2))
		}
		i = j
	}
	return b.String(), true
}

func FuzzLiteralRoundTrip(f *testing.F) {
	for _, s := range []string{
		"", "#", "##", "###", "#{pane_id}", "#(touch /tmp/x)", "raw #(touch X) #P ##",
		"win #S {x}", "dir#Sx", "#[fg=red]", "a#", "x;", "π #{?a,b,c}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		lit := tmuxtarget.Literal(s)
		got, ok := expandHashes(lit.String())
		if !ok {
			t.Fatalf("Literal(%q) = %q still holds an expansion", s, lit.String())
		}
		if got != s {
			t.Fatalf("Literal(%q) expands to %q", s, got)
		}
	})
}

func TestBuilders_LitPositionsAreEscaped(t *testing.T) {
	ns := tmuxtarget.NewSession{
		Name:        tmuxtarget.Literal("s#S"),
		WindowName:  tmuxtarget.Literal("w #(x)"),
		Cwd:         tmuxtarget.Literal("/d#x"),
		Width:       220,
		Height:      60,
		Env:         []tmuxtarget.EnvVar{{Name: "MOOX_RESUME", Value: "a;b #x"}},
		PrintFormat: "#{session_id}",
		Command:     []string{"/bin/sh"},
	}
	want := []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", "s##S", "-n", "w ##(x)",
		"-c", "/d##x", "-x", "220", "-y", "60", "-e", "MOOX_RESUME=a;b #x", "/bin/sh"}
	if got := ns.Args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("NewSession.Args\n got  %q\n want %q", got, want)
	}
	nw := tmuxtarget.NewWindow{Target: "$1:2", Name: tmuxtarget.Literal("fix; test"), Cwd: tmuxtarget.Literal("/a")}
	if got, want := nw.Args(), []string{"new-window", "-d", "-t", "$1:2", "-n", "fix; test", "-c", "/a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NewWindow.Args = %q, want %q", got, want)
	}
	sw := tmuxtarget.SplitWindow{Target: "%3", Horizontal: true, PrintFormat: "#{pane_id}"}
	if got, want := sw.Args(), []string{"split-window", "-d", "-h", "-P", "-F", "#{pane_id}", "-t", "%3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitWindow.Args = %q, want %q", got, want)
	}
	if got, want := tmuxtarget.SelectPaneTitle("%1", tmuxtarget.Literal("t #P")), []string{"select-pane", "-t", "%1", "-T", "t ##P"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectPaneTitle = %q", got)
	}
	if got, want := tmuxtarget.RenameWindow("@1", tmuxtarget.Literal("#")), []string{"rename-window", "-t", "@1", "##"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RenameWindow = %q", got)
	}
	if got, want := tmuxtarget.RenameSession("$0", tmuxtarget.Literal("a.b")), []string{"rename-session", "-t", "$0", "a.b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RenameSession = %q", got)
	}
	got := tmuxtarget.UnsetSessionEnv("$2", "MOOX_RESUME", "MOOX_RESTORE_CONTENT")
	want = []string{"set-environment", "-t", "$2", "-r", "MOOX_RESUME", ";", "set-environment", "-t", "$2", "-r", "MOOX_RESTORE_CONTENT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnsetSessionEnv = %q", got)
	}
}

func TestTarget_ArgsAlwaysUAndS(t *testing.T) {
	tg := tmuxtarget.Target{Bin: "/usr/bin/tmux", Socket: "/x/s"}
	if got, want := tg.Args("list-sessions"), []string{"-u", "-S", "/x/s", "list-sessions"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Args = %q, want %q", got, want)
	}
	cmd := tg.Command(t.Context(), "ls")
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TMUX=") {
			t.Fatalf("Command kept %s in the environment", kv)
		}
	}
	if cmd.Args[0] != "/usr/bin/tmux" || cmd.Args[3] != "/x/s" {
		t.Fatalf("Command argv = %q", cmd.Args)
	}
}

func TestResolve_Precedence(t *testing.T) {
	tmp := t.TempDir()
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Getuid())
	bin := filepath.Join(tmp, "tmux-bin")

	cases := []struct {
		name   string
		env    tmuxtarget.MapEnv
		prefix string
		flag   string
		want   string
	}{
		{"flag path wins", tmuxtarget.MapEnv{"TT_TMUX_SOCKET": "/env/s", "TMUX": "/tm/x,1,2"}, "TT", "/flag/s", "/flag/s"},
		{"flag name is -L", tmuxtarget.MapEnv{"TMUX_TMPDIR": tmp}, "TT", "work", filepath.Join(real, "tmux-"+uid, "work")},
		{"prefix env", tmuxtarget.MapEnv{"MOOX_TMUX_SOCKET": "/env/s", "TT_TMUX_SOCKET": "/tt/s", "TMUX": "/tm/x,1,2"}, "MOOX", "", "/env/s"},
		{"other prefix ignored", tmuxtarget.MapEnv{"TT_TMUX_SOCKET": "/tt/s", "TMUX": "/tm/x,1,2"}, "MOOX", "", "/tm/x"},
		{"TMUX first field", tmuxtarget.MapEnv{"TMUX": "/private/tmp/tmux-501/default,123,0"}, "TT", "", "/private/tmp/tmux-501/default"},
		{"default label", tmuxtarget.MapEnv{"TMUX_TMPDIR": tmp}, "TT", "", filepath.Join(real, "tmux-"+uid, "default")},
		{"socket kept verbatim", tmuxtarget.MapEnv{}, "TT", "/tmp/../tmp/x", "/tmp/../tmp/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := tmuxtarget.Resolve(c.env, c.prefix, c.flag, bin)
			if err != nil {
				t.Fatal(err)
			}
			if got.Socket != c.want {
				t.Fatalf("Socket = %q, want %q", got.Socket, c.want)
			}
			if got.Bin != bin {
				t.Fatalf("Bin = %q, want %q", got.Bin, bin)
			}
		})
	}

	got, err := tmuxtarget.Resolve(tmuxtarget.MapEnv{"TT_TMUX_BIN": "/env/tmux", "TMUX": "/a,1,1"}, "TT", "", "")
	if err != nil || got.Bin != "/env/tmux" {
		t.Fatalf("env bin: %+v %v", got, err)
	}
}

func TestLabelPath_RefusesPaths(t *testing.T) {
	if _, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, "a/b"); err == nil {
		t.Fatal("a name with '/' was accepted")
	}
	p, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, "default")
	if err != nil {
		t.Fatal(err)
	}
	realTmp, _ := filepath.EvalSymlinks("/tmp")
	if want := filepath.Join(realTmp, "tmux-"+strconv.Itoa(os.Getuid()), "default"); p != want {
		t.Fatalf("LabelPath(default) = %q, want %q", p, want)
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
		patch        string
		master, next bool
		atLeast36    bool
	}{
		{"tmux 3.6a\n", 3, 6, "a", false, false, true},
		{"tmux 3.8", 3, 8, "", false, false, true},
		{"3.8", 3, 8, "", false, false, true},
		{"tmux next-3.9", 3, 9, "", false, true, true},
		{"tmux master", 0, 0, "", true, false, true},
		{"tmux 3.5a", 3, 5, "a", false, false, false},
		{"tmux 2.9", 2, 9, "", false, false, false},
		{"tmux 4.0", 4, 0, "", false, false, true},
	}
	for _, c := range cases {
		v, err := tmuxtarget.ParseVersion(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if v.Major != c.major || v.Minor != c.minor || v.Patch != c.patch || v.Master != c.master || v.Next != c.next {
			t.Errorf("%q parsed as %+v", c.in, v)
		}
		if v.AtLeast(3, 6) != c.atLeast36 {
			t.Errorf("%q AtLeast(3,6) = %v", c.in, !c.atLeast36)
		}
	}
	for _, bad := range []string{"", "tmux", "tmux x.y", "tmux 3"} {
		if _, err := tmuxtarget.ParseVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestLiteral_HashRunsBeforeBracketStayVerbatim(t *testing.T) {
	cases := map[string]string{
		"#[fg=red]":      "#[fg=red]",
		"##[x":           "##[x",
		"#[#S]":          "#[##S]",
		"#[a#]b":         "#[a##]b",
		"#{pane_id}":     "##{pane_id}",
		"x#":             "x##",
		"###{a}#[b]":     "######{a}#[b]",
		"no hashes here": "no hashes here",
	}
	for in, want := range cases {
		if got := tmuxtarget.Literal(in).String(); got != want {
			t.Errorf("Literal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLiteral_ValidUTF8Preserved(t *testing.T) {
	s := "π #{x} ✓"
	if l := tmuxtarget.Literal(s).String(); !utf8.ValidString(l) || l != "π ##{x} ✓" {
		t.Fatalf("Literal = %q", l)
	}
}

// TestStoredTitle_UndoesTmuxBackslashDoubling (I-4): tmux keeps a pane title
// with every `\` doubled (select-pane -T 'a\b' reads back as `a\\b`, 4 bytes;
// measured on 3.8), so the title a snapshot holds must be halved back before
// it is set again, or each save/restore cycle doubles it once more.
func TestStoredTitle_UndoesTmuxBackslashDoubling(t *testing.T) {
	cases := map[string]string{
		`literal\\backslash`: `literal\backslash`,
		`\\\\`:               `\\`,
		`a\\b #{x}`:          `a\b ##{x}`,
		`lone \ kept`:        `lone \ kept`, // not tmux's form: left alone
		`trail\`:             `trail\`,
		"no escapes ✳":       "no escapes ✳",
	}
	for in, want := range cases {
		if got := tmuxtarget.StoredTitle(in).String(); got != want {
			t.Errorf("StoredTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func containsElem(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
