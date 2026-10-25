package capture

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/internal/sidmap"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

const nonce = "0123456789abcdef"

func row(rec byte, f ...string) string {
	return nonce + string(rec) + Sep + strings.Join(f, Sep) + "\n"
}

func paneRow(id, sess, win, wid, idx, title, cwd string) string {
	return row(recPane, id, sess, win, wid, idx, title, cwd, "1", "zsh", "100", "0", "10", "5", "0", "", "", "", "", "")
}

// TestCapture_ExactFieldCountSplit (R13, RR1): an empty title keeps its slot;
// a title holding the separator gives a row with the wrong count, which is
// reported by id for a re-read instead of being misparsed; a newline inside a
// value leaves a stray line, never a shifted field.
func TestCapture_ExactFieldCountSplit(t *testing.T) {
	out := row(recServer, "812", "1761262800", "3.8", "main", "", "1", "", "", "", "", "", "3") +
		paneRow("%1", "main", "1", "@1", "1", "", "/home/user/dir") +
		paneRow("%2", "main", "1", "@1", "2", "evil"+Sep+"title", "/home/user/x") +
		paneRow("%3", "main", "1", "@1", "3", "line1\nline2", "/home/user/y") +
		nonce + "G\non\n"
	r := ParseRead([]byte(out), nonce)
	if !r.HaveServer || r.Server.Pid != 812 || r.Server.SaveSeq != 3 || r.Server.Active != "1" {
		t.Fatalf("server: %+v", r.Server)
	}
	if len(r.Panes) != 1 || r.Panes[0].Title != "" || r.Panes[0].Cwd != "/home/user/dir" || r.Panes[0].CurrentCommand != "zsh" {
		t.Fatalf("panes: %+v", r.Panes)
	}
	if !reflect.DeepEqual(r.BadPanes, []string{"%2", "%3"}) {
		t.Fatalf("bad panes: %v", r.BadPanes)
	}
	if len(r.Stray) != 1 || r.GlobalAutoRename != "on" {
		t.Fatalf("stray %q global %q", r.Stray, r.GlobalAutoRename)
	}
	// The re-read drops the title.
	args := RereadPaneArgs(nonce, "%2")
	if args[3] != "%2" || strings.Contains(args[4], "pane_title") {
		t.Fatalf("re-read argv: %q", args)
	}
	if !ParseReread(&r, []byte(paneRow("%2", "main", "1", "@1", "2", "", "/home/user/x")), nonce) || len(r.Panes) != 2 {
		t.Fatal("re-read row not parsed")
	}
}

func TestReadArgs_OneChainNoResurrectOptions(t *testing.T) {
	a := ReadArgs(nonce)
	joined := strings.Join(a, " ")
	if strings.Count(joined, " ; ") != 6 {
		t.Fatalf("not one chain of 7 commands: %q", a)
	}
	for _, want := range []string{"#{@moox-restoring}", "#{@moox-active}", "#{@moox-want-contents}",
		"#{@claude-continuity-by-pid-dir}", "#{start_time}", "list-panes -a", "list-windows -a", "list-clients", "list-sessions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("read chain lacks %s", want)
		}
	}
	if strings.Contains(joined, "@resurrect") {
		t.Error("the read chain reads a resurrect option (I1)")
	}
	for _, x := range a {
		if strings.HasSuffix(x, ";") && x != ";" {
			t.Errorf("argument ends in ';': %q", x)
		}
	}
}

func TestParseRestoring(t *testing.T) {
	cases := []struct {
		in         string
		set, valid bool
		kind       string
	}{
		{"", false, true, ""}, {"boot:12:34", true, true, "boot"}, {"shutdown:9:1", true, true, "shutdown"},
		{"restore:x:1", true, false, ""}, {"garbage", true, false, ""}, {"boot:0:1", true, false, ""}, {"other:1:1", true, false, ""},
	}
	for _, c := range cases {
		r, set, valid := ParseRestoring(c.in)
		if set != c.set || valid != c.valid || r.Kind != c.kind {
			t.Errorf("%q: %+v %v %v", c.in, r, set, valid)
		}
	}
}

func TestParsePS_LstartAndCommandSpacing(t *testing.T) {
	out := "  812     1 Sat Oct 25 03:00:00 2026 tmux -L x\n" +
		"  900   812 Sat Oct 25 03:00:01 2026 -zsh\n" +
		"  901   900 Sat Oct  4 13:00:01 2026 claude  --x 'a  b'\n" +
		"garbage\n"
	tb := ParsePS([]byte(out))
	if len(tb.Procs) != 3 {
		t.Fatalf("%+v", tb.Procs)
	}
	p, _ := tb.Get(901)
	if p.PPid != 900 || p.Start != "Sat_Oct_4_13:00:01_2026" || p.Command != "claude  --x 'a  b'" {
		t.Fatalf("%+v", p)
	}
	if StartToken("  Sat Oct  4 13:00:01 2026\n") != p.Start {
		t.Fatal("StartToken differs from the table's token")
	}
}

// TestSave_FullCommandRule lives here as a unit (R3, PR-M12): the depth-1
// child, the FIRST in ps order; else the pane process itself.
func TestFullCommand_DepthOneChildElsePane(t *testing.T) {
	out := " 100 1 Sat Oct 25 03:00:00 2026 -zsh\n" +
		" 101 100 Sat Oct 25 03:00:00 2026 op run -- claude\n" +
		" 102 101 Sat Oct 25 03:00:00 2026 claude\n" +
		" 103 100 Sat Oct 25 03:00:00 2026 second child\n" +
		" 200 1 Sat Oct 25 03:00:00 2026 claude --dangerously-skip-permissions\n"
	tb := ParsePS([]byte(out))
	if got := tb.FullCommand(100); got != "op run -- claude" {
		t.Errorf("depth-1 child: %q", got)
	}
	if got := tb.FullCommand(200); got != "claude --dangerously-skip-permissions" {
		t.Errorf("pane process itself: %q", got)
	}
	if got := tb.FullCommand(999); got != "" {
		t.Errorf("unknown pid: %q", got)
	}
}

func TestClaudishReplay(t *testing.T) {
	cases := []struct{ argv, model, want string }{
		{"node /home/user/.bun/bin/claudish -d", "cx@gpt-5.6-sol", "-d --model cx@gpt-5.6-sol"},
		{"node /home/user/.bun/bin/claudish --model x -d", "ignored", "--model x -d"},
		{"node /x/claudish --resume --resume aaaaaaaa-0000-4000-8000-000000000001 -d", "", "-d"},
		{"node /x/claudish --profile p", "m", "--profile p"},
		{"node /x/claudish --model-opus o", "m", "--model-opus o"},
		{"/x/claudish", "", ""},
	}
	for _, c := range cases {
		if got := ClaudishReplay(c.argv, c.model); got != c.want {
			t.Errorf("ClaudishReplay(%q, %q) = %q, want %q", c.argv, c.model, got, c.want)
		}
	}
	env := []byte("claude --x PATH=/bin CLAUDISH_ACTIVE_MODEL_NAME=or@qwen-3 HOME=/h")
	if ModelFromEnv(env) != "or@qwen-3" || ModelFromEnv([]byte("claude")) != "" {
		t.Fatal("ModelFromEnv")
	}
	if a := PSEnvArgs(42); strings.Join(a, " ") != "eww -o command= -p 42" {
		t.Fatalf("PSEnvArgs: %q", a)
	}
}

func TestRole_Order(t *testing.T) {
	base := PaneRow{ID: "%1", CurrentCommand: "zsh"}
	obs := base
	obs.StartCommand = `/bin/sh -c "tt observe --pane --auto; exit"`
	obs.MCPOwner, obs.MCPPane = "agent", "%1" // observer outranks helper
	helper := base
	helper.MCPOwner, helper.MCPPane = "agent", "%1"
	leaked := base
	leaked.MCPOwner, leaked.MCPPane = "agent", "%9" // leaked option: not a helper (B14)
	acquired := base
	acquired.MCPOwner, acquired.MCPPane = "acquired", "%1"
	frozen := base
	frozen.Title = "❄ FROZEN k7 3p/2s 2026-10-20"
	prog := base
	prog.CurrentCommand = "htop"

	cases := []struct {
		p        PaneRow
		sid      string
		role     string
		frozenKy string
	}{
		{obs, "", snapshot.RoleTTObserver, ""}, {helper, "s", snapshot.RoleMCPHelper, ""},
		{leaked, "", snapshot.RoleShell, ""}, {acquired, "s", snapshot.RoleClaude, ""},
		{frozen, "", snapshot.RoleFrozen, "k7"}, {prog, "", snapshot.RoleProgram, ""}, {base, "", snapshot.RoleShell, ""},
	}
	for i, c := range cases {
		role, key := Role(c.p, "", c.sid)
		if role != c.role || key != c.frozenKy {
			t.Errorf("case %d: %s %q, want %s %q", i, role, key, c.role, c.frozenKy)
		}
	}
}

// TestSidmap_GroupedDoesNotStealSid (B10, S5): list-panes -a reports a
// grouped pane once per member; the filter keeps only the primary's rows, so
// the sid lands on the primary's target and no secondary row exists.
func TestSidmap_GroupedDoesNotStealSid(t *testing.T) {
	r := Read{
		Sessions: []SessionRow{{ID: "$3", Name: "api", Grouped: true, Group: "g"},
			{ID: "$7", Name: "aaa-peek", Grouped: true, Group: "g"}},
		Windows: []WindowRow{{ID: "@1", Session: "api", Index: 1, Layout: `{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":0,"I":"%5"}}`},
			{ID: "@1", Session: "aaa-peek", Index: 1, Active: true}},
		Panes: []PaneRow{{ID: "%5", Session: "api", Window: 1, WindowID: "@1", Index: 1, Pid: 100},
			{ID: "%5", Session: "aaa-peek", Window: 1, WindowID: "@1", Index: 1, Pid: 100}},
	}
	l := Filter(r)
	if len(l.Panes) != 1 || l.Panes[0].Session != "api" || l.GroupedWith["aaa-peek"] != "api" {
		t.Fatalf("filter: %+v", l)
	}
	res := sidmap.Map(l.SidmapPanes(), []sidmap.Proc{{Pid: 100, PPid: 1, Command: "-zsh"}, {Pid: 101, PPid: 100, Command: "claude"}},
		map[int]string{101: "aaaaaaaa-0000-4000-8000-000000000001"})
	if e := res.ByPane["%5"]; e.Target != "api:1.1" {
		t.Fatalf("sid on %q (the secondary 'aaa-peek' sorts first and would have stolen it)", e.Target)
	}
}

func TestFilter_IgnoresPopupsAndRenamedBootstrap(t *testing.T) {
	r := Read{
		Sessions: []SessionRow{{ID: "$1", Name: "tt-popup-3"}, {ID: "$2", Name: "moox-boot-812"}, {ID: "$3", Name: "work"}},
		Windows:  []WindowRow{{ID: "@1", Session: "tt-popup-3"}, {ID: "@2", Session: "moox-boot-812"}, {ID: "@3", Session: "work"}},
		Panes:    []PaneRow{{ID: "%1", Session: "tt-popup-3"}, {ID: "%2", Session: "moox-boot-812"}, {ID: "%3", Session: "work"}},
	}
	l := Filter(r)
	if len(l.Sessions) != 1 || len(l.Windows) != 1 || len(l.Panes) != 1 || len(l.Ignored) != 2 {
		t.Fatalf("%+v", l)
	}
}

func TestContentsChains_ChunkedUnderArgvBound(t *testing.T) {
	var ids []string
	for i := 0; i < 400; i++ {
		ids = append(ids, fmt.Sprintf("%%%d", i))
	}
	chains := ContentsChains(nonce, ids, CaptureLines(""))
	if len(chains) < 2 {
		t.Fatalf("400 panes in %d chain(s)", len(chains))
	}
	seen := 0
	for _, c := range chains {
		n := 0
		for _, a := range c.Args {
			n += len(a) + 1
		}
		if n > 16<<10 {
			t.Fatalf("chain of %d bytes exceeds tmux's argv bound", n)
		}
		seen += strings.Count(strings.Join(c.Args, " "), "capture-pane -epJ -t")
		if len(c.IDs) != strings.Count(strings.Join(c.Args, " "), "capture-pane") {
			t.Fatal("chain ids disagree with its argv")
		}
	}
	if seen != 400 {
		t.Fatalf("%d captures", seen)
	}
	if one := ContentsChains(nonce, ids[:66], "-"); len(one) != 1 {
		t.Fatalf("66 panes need %d execs, want 1 (B25)", len(one))
	}
	if CaptureLines("500") != "-500" || CaptureLines("full") != "-" || CaptureLines("-3") != "-" {
		t.Fatal("CaptureLines")
	}
}

func TestParseContents_SplitsOnMarkersAndStripsTrailingBlanks(t *testing.T) {
	out := nonce + "P %1\nhello\n\n   \n" + nonce + "P %2\n" + nonce + "P %3\nx " + nonce + "P %9 is text\n\n"
	e, last := ParseContents([]byte(out), nonce)
	if string(e["%1"]) != "hello\n" || string(e["%2"]) != "" || string(e["%3"]) != "x "+nonce+"P %9 is text\n" || last != "%3" {
		t.Fatalf("%q last %q", e, last)
	}
}

// TestClearRestoringArgs_OnlyGrammarValuesReachTheFormat (G1-M1): the value
// is spliced into an if-shell -F format, so a raw value holding `}` or `#(`
// must never get there; it yields a never-true condition instead.
func TestClearRestoringArgs_OnlyGrammarValuesReachTheFormat(t *testing.T) {
	if got := ClearRestoringArgs("boot:42:1700000000"); got[2] != "#{==:#{@moox-restoring},boot:42:1700000000}" {
		t.Fatalf("valid value: %q", got)
	}
	for _, bad := range []string{"", "x}#(touch /tmp/x)", "boot:1:2}", "shutdown:#{pid}:1", " boot:1:2", "restore:1:2:3"} {
		got := ClearRestoringArgs(bad)
		if got[2] != "0" || strings.Contains(strings.Join(got, " "), bad+"}") && bad != "" {
			t.Errorf("%q reached the format: %q", bad, got)
		}
	}
}

// TestBuild_ExcludeActivePaneKeepsFocus (G1-H9): `moox restart` excludes
// the pane it runs in, which is usually the active (and maybe zoomed) one.
// A multi-pane window still has one active pane afterwards and no zoom on
// a pane that is gone; a window dropped with its only pane is never the
// session's active or last window.
func TestBuild_ExcludeActivePaneKeepsFocus(t *testing.T) {
	lay := `{"V":2,"L":{"t":"h","w":160,"h":40,"x":0,"y":0,"c":[{"t":"p","w":80,"h":40,"x":0,"y":0,"i":0,"I":"%1"},{"t":"p","w":79,"h":40,"x":81,"y":0,"a":true,"i":1,"I":"%2"}]}}`
	one := `{"V":2,"L":{"t":"p","w":160,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%3"}}`
	r := Read{
		Sessions: []SessionRow{{ID: "$1", Name: "w"}},
		Windows: []WindowRow{{ID: "@1", Session: "w", Index: 1, Active: true, Zoomed: true, Layout: lay, Width: 160, Height: 40},
			{ID: "@2", Session: "w", Index: 2, Last: true, Layout: one, Width: 160, Height: 40}},
		Panes: []PaneRow{{ID: "%1", Session: "w", Window: 1, WindowID: "@1", Index: 0, CurrentCommand: "zsh"},
			{ID: "%2", Session: "w", Window: 1, WindowID: "@1", Index: 1, CurrentCommand: "zsh", Active: true},
			{ID: "%3", Session: "w", Window: 2, WindowID: "@2", Index: 0, CurrentCommand: "zsh", Active: true}},
	}
	s, _ := Build(Input{Live: Filter(r), ExcludePane: "%2"})
	if len(s.Panes) != 2 || !s.Panes[0].Active || s.Windows[0].Zoomed {
		t.Fatalf("multi-pane window: panes %+v, zoomed %v", s.Panes, s.Windows[0].Zoomed)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
	// the active window's only pane excluded: window 2 becomes active, and
	// the dropped window 1 is not named anywhere
	r.Panes = []PaneRow{{ID: "%1", Session: "w", Window: 1, WindowID: "@1", Index: 0, CurrentCommand: "zsh", Active: true},
		{ID: "%3", Session: "w", Window: 2, WindowID: "@2", Index: 0, CurrentCommand: "zsh", Active: true}}
	r.Windows[0].Layout = `{"V":2,"L":{"t":"p","w":160,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%1"}}`
	r.Windows[0].Zoomed = false
	s, _ = Build(Input{Live: Filter(r), ExcludePane: "%1"})
	if len(s.Windows) != 1 || s.Sessions[0].ActiveWindow != 2 || s.Sessions[0].LastWindow == 1 {
		t.Fatalf("windows %d, session %+v", len(s.Windows), s.Sessions[0])
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestBuild_ExcludePaneRemovesItsLeaf(t *testing.T) {
	lay := `{"V":2,"L":{"t":"h","w":160,"h":40,"x":0,"y":0,"c":[{"t":"p","w":80,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%1"},{"t":"p","w":79,"h":40,"x":81,"y":0,"i":1,"I":"%2"}]}}`
	r := Read{
		Sessions: []SessionRow{{ID: "$1", Name: "w"}},
		Windows:  []WindowRow{{ID: "@1", Session: "w", Index: 1, Active: true, Layout: lay, Width: 160, Height: 40}},
		Panes: []PaneRow{{ID: "%1", Session: "w", Window: 1, WindowID: "@1", Index: 0, CurrentCommand: "zsh"},
			{ID: "%2", Session: "w", Window: 1, WindowID: "@1", Index: 1, CurrentCommand: "zsh"}},
	}
	s, rep := Build(Input{Live: Filter(r), ExcludePane: "%2"})
	if len(s.Panes) != 1 || s.Windows[0].Layout != `{"V":2,"L":{"t":"p","w":160,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%1"}}` {
		t.Fatalf("%+v %q %v", s.Panes, s.Windows[0].Layout, rep.Warnings)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
}

// TestLocalAutoRename_ExplicitLocalEqualToGlobalIsKept (I-5, T6): tmux reports
// the EFFECTIVE automatic-rename, so a window-local value equal to the global
// one looked unset. Windows whose effective value equals the global are asked
// for their own value in one chain; a local `off` under a global `off` saves
// "off", a window with no local value saves "unset", and one whose effective
// value differs from the global is local without asking.
func TestLocalAutoRename_ExplicitLocalEqualToGlobalIsKept(t *testing.T) {
	r := Read{GlobalAutoRename: "off", Windows: []WindowRow{
		{ID: "@1", Session: "s", Index: 0},                   // effective off, no local value
		{ID: "@2", Session: "s", Index: 1},                   // effective off, local off
		{ID: "@3", Session: "s", Index: 2, AutoRename: true}, // differs: local on
	}}
	ids := AmbiguousAutoRename(r)
	if !reflect.DeepEqual(ids, []string{"@1", "@2"}) {
		t.Fatalf("ambiguous windows %v", ids)
	}
	args := strings.Join(LocalAutoRenameArgs(nonce, ids), " ")
	for _, want := range []string{"show-options -wqv -t @1 automatic-rename", "show-options -wqv -t @2 automatic-rename"} {
		if !strings.Contains(args, want) {
			t.Fatalf("argv %q lacks %q", args, want)
		}
	}
	out := nonce + "L" + Sep + "@1\n" + nonce + "L" + Sep + "@2\noff\n"
	if n := ParseLocalAutoRename(&r, []byte(out), nonce); n != 2 {
		t.Fatalf("parsed %d windows, want 2", n)
	}
	got := map[string]string{}
	for _, w := range r.Windows {
		got[w.ID] = window(w, r.GlobalAutoRename).AutomaticRename
	}
	want := map[string]string{"@1": snapshot.AutoRenameUnset, "@2": snapshot.AutoRenameOff, "@3": snapshot.AutoRenameOn}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("automatic_rename %v, want %v", got, want)
	}
	// A window the chain never reached (it stopped at a vanished window)
	// keeps the old inference rather than inventing a value.
	r2 := Read{GlobalAutoRename: "on", Windows: []WindowRow{{ID: "@9", AutoRename: true}}}
	ParseLocalAutoRename(&r2, nil, nonce)
	if a := window(r2.Windows[0], "on").AutomaticRename; a != snapshot.AutoRenameUnset {
		t.Fatalf("unread window %q", a)
	}
}
