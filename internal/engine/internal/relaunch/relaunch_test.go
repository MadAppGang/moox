package relaunch

import (
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

const sid = "0b9a1c2d-0000-4000-8000-000000000001"

func pane(role string) snapshot.Pane {
	return snapshot.Pane{Key: "s:1.1", Session: "s", Window: 1, Index: 1, SavedID: "%1", Cwd: "/home/user", Role: role,
		CurrentCommand: "zsh", FullCommand: "-zsh"}
}

func withSID(p snapshot.Pane, launcher, replay string) snapshot.Pane {
	p.Claude = &snapshot.Claude{SessionID: sid, Launcher: launcher, Replay: replay}
	p.Role = snapshot.RoleClaude
	return p
}

var defOpts = Options{NvimSession: true}

// TestRelaunch_QualificationTable (B30): sid / recording / processes match /
// legacy claude / none.
func TestRelaunch_QualificationTable(t *testing.T) {
	rec := pane(snapshot.RoleShell)
	rec.Launch = "make serve"
	proc := pane(snapshot.RoleProgram)
	proc.FullCommand, proc.CurrentCommand = "htop -d 5", "htop"
	legacy := pane(snapshot.RoleProgram)
	legacy.FullCommand = "claude --dangerously-skip-permissions"
	none := pane(snapshot.RoleProgram)
	none.FullCommand = "python3 server.py"

	cases := []struct {
		name string
		p    snapshot.Pane
		o    Options
		kind string
	}{
		{"sid", withSID(pane(""), "claude", ""), defOpts, KindSidOnly},
		{"recording", rec, defOpts, KindRecording},
		{"processes match", proc, defOpts, KindProgram},
		{"legacy unenriched import", legacy, Options{UnenrichedImport: true}, KindLauncher},
		{"legacy claude NOT from an import", legacy, defOpts, KindSkip},
		{"none", none, defOpts, KindSkip},
		{"plain shell", pane(snapshot.RoleShell), defOpts, KindSkip},
	}
	for _, c := range cases {
		d := Compose(c.p, c.o)
		if d.Kind != c.kind {
			t.Errorf("%s: kind %q (%q), want %q", c.name, d.Kind, d.Cmd, c.kind)
		}
		if (d.Kind == KindSkip || d.Kind == KindLayout) && d.Cmd != "" {
			t.Errorf("%s: a %s row emitted a command %q", c.name, d.Kind, d.Cmd)
		}
	}
}

func TestRelaunch_RoleExclusions(t *testing.T) {
	for role, kind := range map[string]string{snapshot.RoleTTObserver: KindSkip, snapshot.RoleMCPHelper: KindLayout,
		snapshot.RoleFrozen: KindFrozen} {
		p := withSID(pane(role), "claude", "")
		p.Role = role
		p.Launch = "claude"
		if d := Compose(p, defOpts); d.Kind != kind || d.Cmd != "" {
			t.Errorf("%s: %+v", role, d)
		}
	}
}

// TestRelaunch_UnenrichedClaudeLauncher (B30 legacy arm): an imported row with
// only a Claude full_command comes back as that command, made safe.
func TestRelaunch_UnenrichedClaudeLauncher(t *testing.T) {
	p := pane(snapshot.RoleProgram)
	p.FullCommand = "claude --dangerously-skip-permissions --dangerously-skip-permissions --continue"
	d := Compose(p, Options{UnenrichedImport: true})
	if d.Kind != KindLauncher || d.Cmd != "claude --dangerously-skip-permissions" || !d.Claude {
		t.Fatalf("%+v", d)
	}
}

// TestRelaunch_SidIsAuthoritative (B31): a sid qualifies whatever the
// full_command says, even one that would never qualify on its own.
func TestRelaunch_SidIsAuthoritative(t *testing.T) {
	p := withSID(pane(""), "claude", "")
	p.FullCommand = "node /home/user/.bun/bin/claudish --mcp"
	d := Compose(p, defOpts)
	if d.Cmd != "claude --resume "+sid || d.Kind != KindSidOnly || !d.Claude {
		t.Fatalf("%+v", d)
	}
}

// TestRelaunch_MCPChildFullCmdStillRelaunches (R4): resurrect recorded the MCP
// child in place of claude; the sid still brings the session back.
func TestRelaunch_MCPChildFullCmdStillRelaunches(t *testing.T) {
	p := withSID(pane(""), "claude", "")
	p.FullCommand = "/opt/homebrew/bin/railway mcp"
	if d := Compose(p, defOpts); !strings.HasSuffix(d.Cmd, "--resume "+sid) || !d.Claude {
		t.Fatalf("%+v", d)
	}
}

// TestRelaunch_Priority (B32 a–f).
func TestRelaunch_Priority(t *testing.T) {
	typed := withSID(pane(""), "claude", "")
	typed.Launch = "wt foo red"
	typed.FullCommand = "claude --x"

	claudish := withSID(pane(""), "claudish", "--model cx@gpt-5.6-sol -d")
	claudish.Launch = "c --worktree foo" // (a) beats (b)

	launcher := withSID(pane(""), "claude", "")
	launcher.FullCommand = "op run --env-file x -- claude --dangerously-skip-permissions --resume " + sid

	unsafe := withSID(pane(""), "claude", "")
	unsafe.FullCommand = "claude --append-system-prompt be nice"

	proc := pane(snapshot.RoleProgram)
	proc.FullCommand = "tail -f /var/log/x"

	cases := []struct {
		name string
		p    snapshot.Pane
		want string
		kind string
	}{
		{"a claudish", claudish, "claudish --model cx@gpt-5.6-sol -d --resume " + sid, KindClaudish},
		{"b typed+sid", typed, "wt foo red --resume " + sid, KindTyped},
		{"c launcher", launcher, "op run --env-file x -- claude --dangerously-skip-permissions --resume " + sid, KindLauncher},
		{"d sid only (excluded flag in c)", unsafe, "claude --resume " + sid, KindSidOnly},
		{"e processes", proc, "tail -f /var/log/x", KindProgram},
	}
	for _, c := range cases {
		d := Compose(c.p, defOpts)
		if d.Cmd != c.want || d.Kind != c.kind {
			t.Errorf("%s: got %q (%s), want %q (%s)", c.name, d.Cmd, d.Kind, c.want, c.kind)
		}
	}
	// f: nothing usable
	if d := Compose(pane(snapshot.RoleShell), defOpts); d.Kind != KindSkip || d.Cmd != "" {
		t.Errorf("f: %+v", d)
	}
}

// TestRelaunch_WtCase (R1): the typed launcher grammar `wt <name> <colour>`
// gets exactly one --resume, after its own flags are stripped.
func TestRelaunch_WtCase(t *testing.T) {
	p := withSID(pane(""), "claude", "")
	p.Launch = "wt foo red --continue"
	if d := Compose(p, defOpts); d.Cmd != "wt foo red --resume "+sid {
		t.Fatalf("%q", d.Cmd)
	}
	p.Launch = "wt foo red --resume " + sid + " --fork-session"
	if d := Compose(p, defOpts); d.Cmd != "wt foo red --resume "+sid || strings.Count(d.Cmd, "--resume") != 1 {
		t.Fatalf("%q", d.Cmd)
	}
}

// TestRelaunch_ReadsLegacyClaudishOption (PR-H10): the claudish and claude
// commands come from continuity's existing option names.
func TestRelaunch_ReadsLegacyClaudishOption(t *testing.T) {
	asked := map[string]bool{}
	o := FromOptions(func(name string) string {
		asked[name] = true
		switch name {
		case "@claude-continuity-claudish-cmd":
			return "CLAUDISH_X=1 claudish"
		case "@claude-continuity-claude-cmd":
			return "my-claude"
		}
		return ""
	})
	if asked["@moox-claudish-cmd"] || !asked["@claude-continuity-claudish-cmd"] {
		t.Fatalf("options read: %v", asked)
	}
	if d := Compose(withSID(pane(""), "claudish", "-d"), o); d.Cmd != "CLAUDISH_X=1 claudish -d --resume "+sid {
		t.Fatalf("%q", d.Cmd)
	}
	p := withSID(pane(""), "claude", "")
	if d := Compose(p, o); d.Cmd != "my-claude --resume "+sid {
		t.Fatalf("%q", d.Cmd)
	}
	if !o.NvimSession {
		t.Fatal("nvim session strategy must default on")
	}
}

// TestRelaunch_ClaudishNeverPlainClaude (R9, R10): a claudish pane never comes
// back as a bare `claude --resume`, which would replay the transcript against
// the real API on the wrong account and model.
func TestRelaunch_ClaudishNeverPlainClaude(t *testing.T) {
	for _, replay := range []string{"", "-d", "--model m"} {
		p := withSID(pane(""), "claudish", replay)
		p.FullCommand = "claude --dangerously-skip-permissions"
		d := Compose(p, defOpts)
		if !strings.HasPrefix(d.Cmd, "claudish") || d.Kind != KindClaudish {
			t.Errorf("replay %q: %q", replay, d.Cmd)
		}
	}
}

// TestRelaunch_ClaudishReplayUnsafeFallsBack (R2-M5, R3-M4).
func TestRelaunch_ClaudishReplayUnsafeFallsBack(t *testing.T) {
	cases := map[string]string{
		`--model cx@gpt-5.6 -d "fix it; $(touch x)"`: "claudish --model cx@gpt-5.6 --resume " + sid,
		"--model $(id) -d;":                          "claudish --resume " + sid,
		"-m good/model:1 --flag `x`":                 "claudish --model good/model:1 --resume " + sid,
		"--model=we'ird -d":                          "claudish --resume " + sid,
		"-d --model sonnet --append-system-prompt x": "claudish --model sonnet --resume " + sid, // excluded flag
	}
	for replay, want := range cases {
		d := Compose(withSID(pane(""), "claudish", replay), defOpts)
		if d.Cmd != want || !strings.Contains(d.Reason, "REPLAY-UNSAFE") {
			t.Errorf("replay %q: %q (%s), want %q", replay, d.Cmd, d.Reason, want)
		}
	}
}

// TestRelaunch_ClaudishReplayExcludedFlag: a replay with a one-shot flag is
// not eval'd at the next boot even though its charset is safe.
func TestRelaunch_ClaudishReplayExcludedFlag(t *testing.T) {
	for _, f := range []string{"-p", "--print", "--prompt", "--name", "--system-prompt", "--append-system-prompt", "--output-format"} {
		d := Compose(withSID(pane(""), "claudish", "--model m "+f+" x"), defOpts)
		if strings.Contains(d.Cmd, f+" ") || d.Cmd != "claudish --model m --resume "+sid {
			t.Errorf("%s: %q", f, d.Cmd)
		}
	}
}

// TestRelaunch_NonClaudeRecordingKeepsFlags (R3-H5): without a sid the
// recording is replayed verbatim, never through StripSessionFlags.
func TestRelaunch_NonClaudeRecordingKeepsFlags(t *testing.T) {
	for _, rec := range []string{`python -c 'print(1)'`, `bash -c "sleep 9; echo done"`, "git -c k=v log",
		"tar -cf x.tar dir", "psql --continue x", "sleep 1000; true"} {
		p := pane(snapshot.RoleShell)
		p.Launch = rec
		if d := Compose(p, defOpts); d.Cmd != rec || d.Kind != KindRecording || d.Claude {
			t.Errorf("%q → %q (%s)", rec, d.Cmd, d.Kind)
		}
	}
}

// TestRelaunch_ShellRoleWithRecordingReplays (B13): presence of the recording
// is the only test, whatever the role.
func TestRelaunch_ShellRoleWithRecordingReplays(t *testing.T) {
	p := pane(snapshot.RoleShell)
	p.Launch = "npm run dev"
	if d := Compose(p, defOpts); d.Cmd != "npm run dev" {
		t.Fatalf("%+v", d)
	}
}

// TestRelaunch_FirstLineOnly (R35).
func TestRelaunch_FirstLineOnly(t *testing.T) {
	p := pane(snapshot.RoleShell)
	p.Launch = "echo one\nrm -rf /tmp/x\n"
	if d := Compose(p, defOpts); d.Cmd != "echo one" {
		t.Fatalf("%q", d.Cmd)
	}
	q := withSID(pane(""), "claude", "")
	q.Launch = "c --worktree x\r\nsecond"
	if d := Compose(q, defOpts); d.Cmd != "c --worktree x --resume "+sid {
		t.Fatalf("%q", d.Cmd)
	}
}

// TestRelaunch_FullCommandContinuationsNeverMatch (I-2, PR-H14): an imported
// full_command carries resurrect's continuation lines (the pane's other
// children) after "\n"; only the first line is the pane's command, so the
// rest never reaches a @moox-processes match, the legacy-claude test or the
// launcher replay.
func TestRelaunch_FullCommandContinuationsNeverMatch(t *testing.T) {
	p := pane(snapshot.RoleShell)
	p.FullCommand = "-zsh\nvim notes.md\nclaude --dangerously-skip-permissions"
	if d := Compose(p, Options{NvimSession: true, UnenrichedImport: true}); d.Kind != KindSkip || d.Cmd != "" {
		t.Fatalf("a continuation line qualified the pane: %+v", d)
	}
	q := pane(snapshot.RoleProgram)
	q.FullCommand = "htop\nrm -rf /tmp/x"
	if d := Compose(q, defOpts); d.Cmd != "htop" {
		t.Fatalf("process rule replayed %q, want the first line only", d.Cmd)
	}
	c := withSID(pane(""), "claude", "")
	c.FullCommand = "claude --model opus\nhelper --flag"
	if d := Compose(c, defOpts); d.Cmd != "claude --model opus --resume "+sid {
		t.Fatalf("launcher replay %q", d.Cmd)
	}
}

// TestStripSessionFlags_FixedPoint (B33, R12): adjacent and doubled flags need
// more than one pass; the result is a fixed point.
func TestStripSessionFlags_FixedPoint(t *testing.T) {
	cases := map[string]string{
		"claudish --resume --resume " + sid + " -d":            "claudish -d",
		"claude -c -c --continue":                              "claude",
		"claude --resume=" + sid + " --x":                      "claude --x",
		"claude -r " + sid:                                     "claude",
		"claude --session-id=" + sid + " --fork-session --y":   "claude --y",
		"claude --from-pr 12 --resume abc --model m":           "claude --model m",
		"claude --resume --continue --fork-session -c --model": "claude --model",
		"c --worktree foo":                                     "c --worktree foo",
	}
	for in, want := range cases {
		got := StripSessionFlags(in)
		if got != want {
			t.Errorf("StripSessionFlags(%q) = %q, want %q", in, got, want)
		}
		if StripSessionFlags(got) != got {
			t.Errorf("%q is not a fixed point", got)
		}
	}
}

// TestStripSessionFlags_DedupeSkipPermissions (R11): four copies of a
// valueless flag become one; valued flags stay repeatable.
func TestStripSessionFlags_DedupeSkipPermissions(t *testing.T) {
	in := "claude --dangerously-skip-permissions --add-dir a --dangerously-skip-permissions --add-dir b " +
		"--dangerously-skip-permissions --model sonnet --dangerously-skip-permissions"
	want := "claude --dangerously-skip-permissions --add-dir a --add-dir b --model sonnet"
	if got := DedupeValuelessFlags(StripSessionFlags(in)); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// TestSafeSID_RejectsInjection (B34, R33).
func TestSafeSID_RejectsInjection(t *testing.T) {
	for _, bad := range []string{"0000; /usr/bin/touch /tmp/pwn", "$(id)", "a b", "`x`", "", "a\nb", "x|y"} {
		if SafeSID(bad) {
			t.Errorf("SafeSID(%q) = true", bad)
		}
		p := pane("")
		p.Claude = &snapshot.Claude{SessionID: bad}
		if d := Compose(p, defOpts); strings.Contains(d.Cmd, "--resume") {
			t.Errorf("unsafe sid %q reached a command: %q", bad, d.Cmd)
		}
	}
	if !SafeSID(sid) {
		t.Fatal("a real uuid refused")
	}
}

// TestRelaunch_NvimSessionStrategy (B46).
func TestRelaunch_NvimSessionStrategy(t *testing.T) {
	o := Options{NvimSession: true, SessionVim: func(cwd string) bool { return cwd == "/proj" }}
	p := pane(snapshot.RoleProgram)
	p.FullCommand, p.Cwd = "/opt/homebrew/bin/nvim main.go", "/proj"
	if d := Compose(p, o); d.Cmd != "/opt/homebrew/bin/nvim -S" || d.Kind != KindNvim {
		t.Errorf("Session.vim: %+v", d)
	}
	p.Cwd = "/other"
	p.FullCommand = "nvim -S my.vim"
	if d := Compose(p, o); d.Cmd != "nvim" {
		t.Errorf("-S in original: %+v", d)
	}
	p.FullCommand = "vim notes.md"
	if d := Compose(p, o); d.Cmd != "vim notes.md" || d.Kind != KindNvim {
		t.Errorf("plain: %+v", d)
	}
	rec := pane(snapshot.RoleShell)
	rec.Launch, rec.Cwd = "nvim .", "/proj"
	if d := Compose(rec, o); d.Cmd != "nvim -S" {
		t.Errorf("recording: %+v", d)
	}
	o.NvimSession = false
	if d := Compose(rec, o); d.Cmd != "nvim ." || d.Kind != KindRecording {
		t.Errorf("strategy off: %+v", d)
	}
}

// TestRelaunch_ProcessesRules (T8): plain, ~, -> and *.
func TestRelaunch_ProcessesRules(t *testing.T) {
	cases := []struct {
		setting, full, want string
		ok                  bool
	}{
		{"", "htop", "htop", true},
		{"", "less +F x.log", "less +F x.log", true},
		{"", "lesser x", "", false},
		{"ssh psql", "psql -h db", "psql -h db", true},
		{"ssh", "/usr/bin/ssh host", "/usr/bin/ssh host", true},
		{`"~rails server"`, "ruby bin/rails server -p 3000", "ruby bin/rails server -p 3000", true},
		{`"~rails server->rails server"`, "ruby bin/rails server -p 3000", "rails server", true},
		{`"git log->git log *"`, "git log --oneline -5", "git log --oneline -5", true},
		{`"~irb->irb *"`, "/x/irb --simple-prompt", "irb --simple-prompt", true},
		{"vim", "nvim x", "", false},
	}
	for _, c := range cases {
		got, ok := matchProcesses(c.full, or(c.setting, DefaultProcesses))
		if ok != c.ok || got != c.want {
			t.Errorf("setting %q, full %q: got %q %v, want %q %v", c.setting, c.full, got, ok, c.want, c.ok)
		}
	}
}

// TestRelaunch_StubPayloadRoundTrip (V3/V4): the stub prints the original,
// carries it intact whatever bytes it holds, and execs the stub claude only
// for Claude kinds.
func TestRelaunch_StubPayloadRoundTrip(t *testing.T) {
	for _, orig := range []string{"sleep 1000; true", "wt 'a b' \"c\" $(x) `y` #{z}", "+//+==", "claude --resume " + sid,
		"", "tab\there\nnewline", strings.Repeat("ü", 50)} {
		for _, cl := range []bool{false, true} {
			s := Stub(orig, cl, "/x/claude")
			got, ok := StubPayload(s)
			if !ok || got != orig {
				t.Fatalf("payload of %q: %q %v", orig, got, ok)
			}
			if strings.Count(s, "'") != 4 {
				t.Fatalf("payload not single-quoted once: %s", s)
			}
			if cl != strings.HasSuffix(s, "exec /x/claude 600") || !cl != strings.HasSuffix(s, "exec sleep 600") {
				t.Fatalf("tail: %s", s)
			}
		}
	}
	d := Compose(withSID(pane(""), "claude", ""), Options{Stub: true, StubClaude: "/x/claude"})
	if p, _ := StubPayload(d.Cmd); p != "claude --resume "+sid || !strings.HasSuffix(d.Cmd, "/x/claude 600") {
		t.Fatalf("Compose stub: %q", d.Cmd)
	}
	if d := Compose(pane(snapshot.RoleShell), Options{Stub: true}); d.Cmd != "" {
		t.Fatalf("a skip row got a stub: %q", d.Cmd)
	}
}

func FuzzRelaunch_StubPayloadRoundTrip(f *testing.F) {
	f.Add("sleep 1; true")
	f.Fuzz(func(t *testing.T, orig string) {
		got, ok := StubPayload(Stub(orig, false, ""))
		if !ok || got != orig {
			t.Fatalf("%q → %q", orig, got)
		}
	})
}

// TestRelaunch_ProcessCommandNeverEvalsUnsafe (G1-C1, CWE-78): a ps-derived
// full_command lost its quoting, so a file name holding shell syntax comes
// back as live code. The @moox-processes path (plain rule, `->… *`
// substitution, the nvim fallback) must refuse such a command — it restores
// as a plain shell, logged PROCESS-UNSAFE — and still replay safe ones.
func TestRelaunch_ProcessCommandNeverEvalsUnsafe(t *testing.T) {
	unsafe := []struct{ setting, full string }{
		{"", "vim $(curl x|sh).md"},
		{"", "less /tmp/$(id)"},
		{"", "tail -f a;b"},
		{"", "vim notes (draft).md"},
		{"", "nvim `id`.txt"},
		{"", "man x|sh"},
		{`"git log->git log *"`, "git log --format=%H;id"},
		{`"~irb->irb *"`, "/x/irb $(id)"},
	}
	for _, c := range unsafe {
		p := pane(snapshot.RoleShell)
		p.FullCommand = c.full
		p.Cwd = "/nonexistent"
		d := Compose(p, Options{Processes: c.setting, NvimSession: true})
		if d.Cmd != "" {
			t.Errorf("%q (setting %q) replays %q (kind %s): ps-derived shell syntax reaches eval", c.full, c.setting, d.Cmd, d.Kind)
		}
		if !strings.Contains(d.Reason, "PROCESS-UNSAFE") || !d.Qualified {
			t.Errorf("%q: decision %+v, want a qualified PROCESS-UNSAFE (a visible loss, not a silent skip)", c.full, d)
		}
	}
	// Safe commands still replay, the user-authored half of a -> rule is
	// trusted, and the stub mode still never sees the unsafe text.
	safe := []struct{ setting, full, want string }{
		{"", "vim notes.md", "vim notes.md"},
		{"", "less +F /var/log/x.log", "less +F /var/log/x.log"},
		{`"~rails server->rails server"`, "ruby bin/rails server $(id)", "rails server"},
		{`"git log->git log *"`, "git log --oneline -5", "git log --oneline -5"},
	}
	for _, c := range safe {
		p := pane(snapshot.RoleShell)
		p.FullCommand = c.full
		if d := Compose(p, Options{Processes: c.setting}); d.Cmd != c.want {
			t.Errorf("%q (setting %q): %q, want %q", c.full, c.setting, d.Cmd, c.want)
		}
	}
}
