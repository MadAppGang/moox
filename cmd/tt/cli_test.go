package main

// The §2.4 dispatch, and the daily-use guarantee it has to protect.
//
// main.go is the file the user runs every day. Three of these tests are §2.4's
// three named properties; the fourth is stronger than any of them and is the
// reason this file exists at all — it asserts that the statements of main()
// AFTER the dispatch guard are the same statements HEAD had, so "bare tt takes
// the old path" is proven against the source rather than argued.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
)

// ---------------------------------------------------------------------------
// §2.4 property 1 — a bare `tt`, and anything flag-led, takes the TUI path
// ---------------------------------------------------------------------------

func TestBareTTAndFlagLedArgvTakeTheTUIPath(t *testing.T) {
	// The daily invocation is the first row and the reason for the rest.
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"bare tt, the daily invocation", nil},
		{"bare tt with an empty argv slice", []string{}},
		// --help, -h and --version used to be here. moox §4.5 made them
		// print-and-exit (`tt --version` is what brew test runs, with no
		// terminal): TestRouteArgs_VersionNeverTUI.
		{"tt -", []string{"-"}},
		{"tt --json (a flag with no command)", []string{"--json"}},
		{"tt --kind=interactive", []string{"--kind=interactive"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := routeArgs(tc.argv)
			if r.kind != routeTUI {
				t.Fatalf("routeArgs(%q).kind = %v, want routeTUI — the daily path must not be diverted",
					tc.argv, r.kind)
			}
			if r.name != "" || r.args != nil {
				t.Fatalf("routeArgs(%q) = %+v, want a bare TUI route with no command captured", tc.argv, r)
			}
		})
	}
}

// TestMainsTUIPathIsTheSameStatementsHEADHad is the daily-use guarantee as a
// test rather than as a promise.
//
// It parses the main.go on disk, takes main()'s body, asserts the FIRST
// statement is the dispatch guard, and asserts every statement after it is
// syntactically identical to the statements HEAD's main() had — in order. So a
// later phase cannot reorder, wrap or "tidy" the TUI path without this failing,
// and it cannot insert anything before resolveTheme() either.
//
// headMainGo below is tools/tt/main.go at e57489f, verbatim.
func TestMainsTUIPathIsTheSameStatementsHEADHad(t *testing.T) {
	fset := token.NewFileSet()

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	current := mainBody(t, fset, "main.go", string(src))
	head := mainBody(t, fset, "head-main.go", headMainGo)

	// The one deliberate addition since HEAD (the live theme listener,
	// theme_live.go): theme reports are switched on right after
	// resolveTheme() and off right after p.Run() returns. Exactly those two
	// statements, in exactly those places, are taken out before the
	// comparison; anything else added still fails it.
	allowed := map[string]string{
		"startThemeReports(os.Stdout)": "resolveTheme()",
		"stopThemeReports(os.Stdout)":  "final, err := p.Run()",
	}
	kept := current[:0:0]
	for i, st := range current {
		after, ok := allowed[render(t, fset, st)]
		if !ok {
			kept = append(kept, st)
			continue
		}
		if i == 0 || render(t, fset, current[i-1]) != after {
			t.Fatalf("main() statement %d, %s, must come right after %s", i, render(t, fset, st), after)
		}
		delete(allowed, render(t, fset, st))
	}
	if len(allowed) != 0 {
		t.Fatalf("main() lacks the theme listener's statements %v", allowed)
	}
	current = kept

	if len(current) != len(head)+1 {
		t.Fatalf("main() has %d statements, want %d (HEAD's %d plus exactly one dispatch guard)",
			len(current), len(head)+1, len(head))
	}

	guard, ok := current[0].(*ast.IfStmt)
	if !ok {
		t.Fatalf("main()'s first statement is %T, want the *ast.IfStmt dispatch guard", current[0])
	}
	// The decision must come from routeArgs — in the guard's init or in its
	// condition, since `if r := routeArgs(...); r.kind == ...` splits it.
	decision := render(t, fset, guard.Cond)
	if guard.Init != nil {
		decision = render(t, fset, guard.Init) + "; " + decision
	}
	if !strings.Contains(decision, "routeArgs") {
		t.Fatalf("main()'s first statement guards on %q; the dispatch decision must come from routeArgs", decision)
	}
	if !strings.Contains(decision, "routeSubcommand") {
		t.Fatalf("main()'s guard %q does not test for routeSubcommand; the TUI must be the fall-through, not the branch", decision)
	}
	if body := render(t, fset, guard.Body); !strings.Contains(body, "os.Exit") {
		t.Fatalf("the dispatch guard's body does not exit:\n%s\n\nA subcommand that fell through would open the TUI.", body)
	}

	for i := range head {
		got, want := render(t, fset, current[i+1]), render(t, fset, head[i])
		if got != want {
			t.Fatalf("main() statement %d changed from HEAD:\n got: %s\nwant: %s", i+1, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// §2.4 property 2 — an unknown subcommand is a usage error, never the TUI
// ---------------------------------------------------------------------------

func TestAnUnknownSubcommandIsRefusedAndNeverOpensTheTUI(t *testing.T) {
	// `tt session` is the realistic typo: one character from `tt sessions`.
	//
	// P7 EDITED THIS LIST, and the edit is the point rather than a tidy-up:
	// `observe` was here as a name that did not exist yet, and P7 is the phase
	// that makes it exist. Leaving it would not have failed a name assertion —
	// it would have STARTED A SERVER inside the test binary, against the real
	// ~/.claude and the real ~/.local/state/tt, and blocked until SIGTERM.
	// (It did exactly that once, for nine minutes, before this line was
	// written.) `observer` replaces it: still not a command, one character from
	// one that is, and the same shape of typo.
	for _, argv := range [][]string{
		{"session"},
		{"sesions", "--json"},
		{"observer"},
		// P8 EDITED THIS ENTRY, for the reason P7's comment above gives about
		// `observe`: `mcp` was here as a name that did not exist yet, and P8
		// is the phase that makes it exist. Left in place it would not have
		// failed a name assertion — it would have started an MCP server inside
		// the test binary, reading the test runner's stdin until the suite was
		// killed. `mcpserver` replaces it: still not a command, one word from
		// one that is.
		{"mcpserver"},
		{""}, // an empty first argument is a name, and it matches nothing
		{"sessions.json"},
	} {
		t.Run(fmt.Sprintf("%q", argv), func(t *testing.T) {
			r := routeArgs(argv)
			if r.kind != routeSubcommand {
				t.Fatalf("routeArgs(%q) routed to the TUI; an unrecognised word must be refused, not rendered", argv)
			}

			var stdout, stderr bytes.Buffer
			code, handled := runSubcommand(r.name, r.args, &stdout, &stderr)
			if handled {
				t.Fatalf("runSubcommand(%q) reported handled=true, code=%d", argv, code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("an unknown command wrote %q to stdout; stdout is the machine surface", stdout.String())
			}
		})
	}
}

// TestTheUnknownCommandMessageAndExitCodeAreTheOnesSpecified pins the pair a
// human and a script each read. Exit 2, the name quoted, and the usage text —
// asserted here rather than in main() because main() calls os.Exit.
func TestTheUnknownCommandMessageAndExitCodeAreTheOnesSpecified(t *testing.T) {
	var stderr bytes.Buffer
	name := "session"
	code, handled := runSubcommand(name, nil, &bytes.Buffer{}, &stderr)
	if handled {
		t.Fatalf("%q should not be a known command", name)
	}

	// This is the message main() prints for !handled; asserting it here keeps
	// the format in one place and lets the exit code be a value, not an exit.
	fmt.Fprintf(&stderr, "tt: unknown command %q\n\n%s", name, usage)

	if got := stderr.String(); !strings.Contains(got, `unknown command "session"`) {
		t.Fatalf("stderr does not name the offending command:\n%s", got)
	}
	if !strings.Contains(stderr.String(), "sessions") {
		t.Fatalf("the usage text does not list the commands that do exist:\n%s", stderr.String())
	}
	if code != 0 {
		t.Fatalf("an unhandled command returned code %d; main() decides the code for !handled (2)", code)
	}

	// And the code main() actually uses, asserted against the source so the
	// number and the specification cannot drift apart.
	if !strings.Contains(readFile(t, "main.go"), "os.Exit(2)") {
		t.Fatal("main.go does not exit 2 on an unknown command (§2.4 property 2)")
	}
}

// ---------------------------------------------------------------------------
// §2.4 property 3 — resolveTheme() is never called on the subcommand path
// ---------------------------------------------------------------------------

func TestTheSubcommandPathNeverTouchesTheTerminalOrTheTUI(t *testing.T) {
	// resolveTheme writes an OSC 11 query to the tty and reads the reply
	// (theme.go:55). On a path whose stdout is JSON that is corruption, not a
	// slow start. The assertion is structural: none of these identifiers may
	// appear anywhere in the CLI file at all.
	banned := []string{"resolveTheme", "newModel", "tea.NewProgram", "attach("}
	src := printedSource(t, "cli.go")
	for _, b := range banned {
		if strings.Contains(src, b) {
			t.Fatalf("cli.go references %q; the subcommand path must not reach the TUI or the terminal", b)
		}
	}

	// And in main.go, resolveTheme must come AFTER the dispatch guard: being
	// called once is not enough if it is called first.
	main := printedSource(t, "main.go")
	guard, theme := strings.Index(main, "routeArgs"), strings.Index(main, "resolveTheme()")
	if guard < 0 || theme < 0 {
		t.Fatalf("main.go is missing the dispatch guard (%d) or resolveTheme (%d)", guard, theme)
	}
	if theme < guard {
		t.Fatal("main.go calls resolveTheme() before the dispatch guard; JSON on stdout would be preceded by an OSC 11 query")
	}
	if n := strings.Count(main, "resolveTheme()"); n != 1 {
		t.Fatalf("main.go calls resolveTheme() %d times, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// the flag surface
// ---------------------------------------------------------------------------

func TestSessionsFlagErrorsAreUsageErrorsWithNothingOnStdout(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"an unknown flag", []string{"--jsonn"}},
		{"an unknown kind", []string{"--kind=bogus"}},
		{"a zero timeout", []string{"--timeout=0"}},
		{"a negative timeout", []string{"--timeout=-1s"}},
		{"a positional argument", []string{"extra"}},
		// --doctor is implemented as of P10, and it REFUSES every narrowing
		// flag rather than ignoring one: the doctor lists no sessions, so a
		// filter beside it describes something that will not happen, and a
		// silently dropped --waiting is how a user comes to believe they
		// diagnosed a subset of their machine.
		{"the doctor with a narrowing flag", []string{"--doctor", "--waiting"}},
		{"the doctor with an output flag it does not honour", []string{"--doctor", "--include-dead"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runSessions(tc.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code %d, want 2 (a usage error)", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("a usage error wrote to stdout: %q", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("a usage error said nothing on stderr")
			}
		})
	}
}

func TestSessionsHelpIsASuccessAndGoesToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runSessions([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, want 0 — asking for help is not an error", code)
	}
	if !strings.Contains(stdout.String(), "--json") {
		t.Fatalf("the help text does not document --json:\n%s", stdout.String())
	}
}

// TestTheFlagsBuildOnTheSafeDefaults is P1 finding 6's trap, as a test. A
// SnapshotOptions{} literal has IncludeUnknownLiveness false, which hides every
// live: null session — the one thing M7 forbids. Every flag combination must
// keep it true unless something explicitly turns it off, and nothing does.
func TestTheFlagsBuildOnTheSafeDefaults(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--json"},
		{"--waiting"},
		{"--kind=interactive"},
		{"--include-dead"},
		{"--all"},
		{"--no-transcript"},
		{"--cwd-prefix=/home/user"},
		{"--json", "--waiting", "--kind=background", "--timeout=2s"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			opts, timeout, _, code, ok := parseSessionsFlags(args, &bytes.Buffer{}, &bytes.Buffer{})
			if !ok {
				t.Fatalf("parse failed with code %d", code)
			}
			if !opts.Snapshot.IncludeUnknownLiveness {
				t.Fatal("IncludeUnknownLiveness is false: every live: null session would be silently dropped (M7)")
			}
			if timeout <= 0 {
				t.Fatalf("timeout %s", timeout)
			}
			if opts.TT == "" {
				t.Fatal("the tt version string is empty; §5.1 gives it one source and this is it")
			}
		})
	}
}

func TestAllWidensTheSetAndClearsEveryNarrowing(t *testing.T) {
	// --all is "everything you know", so it must beat a narrowing given
	// earlier on the same line rather than coexisting with it.
	opts, _, _, code, ok := parseSessionsFlags(
		[]string{"--waiting", "--kind=background", "--cwd-prefix=/tmp", "--all"},
		&bytes.Buffer{}, &bytes.Buffer{})
	if !ok {
		t.Fatalf("parse failed with code %d", code)
	}
	if !opts.Snapshot.IncludeDead || !opts.Snapshot.IncludeUnknownLiveness {
		t.Fatalf("--all did not widen the liveness set: %+v", opts.Snapshot)
	}
	// The cleared filter is NOT the zero Filter: the zero Filter hides the
	// sessions other sessions launched, and "everything you know" lists them.
	if !reflect.DeepEqual(opts.Snapshot.Filter, observer.Filter{IncludeSpawned: true}) {
		t.Fatalf("--all left a narrowing in place, or hid launched sessions: %+v", opts.Snapshot.Filter)
	}
}

// Launched sessions (spawnedBy set — an MCP helper's headless child) are out of
// every default view and back in with --all, and with nothing else: in
// particular --include-dead must not bring them back, because the panel asks
// for the dead and must still not draw the children.
func TestOnlyAllListsTheSessionsOtherSessionsLaunched(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--json"}, false},
		{[]string{"--include-dead"}, false},
		{[]string{"--json", "--all"}, true},
	} {
		opts, _, _, code, ok := parseSessionsFlags(tc.args, &bytes.Buffer{}, &bytes.Buffer{})
		if !ok {
			t.Fatalf("%v: parse failed with code %d", tc.args, code)
		}
		if got := opts.Snapshot.Filter.IncludeSpawned; got != tc.want {
			t.Errorf("%v: IncludeSpawned = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestNoTranscriptTurnsOffTheEnrichmentPassAndNothingElse(t *testing.T) {
	on, _, _, _, ok := parseSessionsFlags(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok {
		t.Fatal("parse failed")
	}
	if !on.Snapshot.IncludeTranscripts {
		t.Fatal("S1 collects transcripts by default (§9.2a); it is the documented per-surface default")
	}

	off, _, _, _, ok := parseSessionsFlags([]string{"--no-transcript"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok {
		t.Fatal("parse failed")
	}
	if off.Snapshot.IncludeTranscripts {
		t.Fatal("--no-transcript did not turn the enrichment pass off")
	}
	if off.Snapshot.IncludeDead != on.Snapshot.IncludeDead ||
		off.Snapshot.IncludeUnknownLiveness != on.Snapshot.IncludeUnknownLiveness {
		t.Fatal("--no-transcript changed the session SET as well as the enrichment; it must do only one thing")
	}
}

// ---------------------------------------------------------------------------
// the composition root, end to end, hermetically
// ---------------------------------------------------------------------------

// hermeticTree points the whole composition root at t.TempDir(): the Claude
// state tree it READS, the tt state directory it may WRITE, and a PATH with no
// `claude` on it.
//
// ADDED BY P4, because the live run found what the fixtures could not. Wiring
// the corroboration port into the composition root made these tests spawn the
// REAL vendor CLI — which reads CLAUDE_CONFIG_DIR, so it dutifully described
// the fixture tree — and publish that one-entry listing into the author's own
// ~/.local/state/tt/agents-cache.json. The next live `tt sessions --refresh`
// read it back as a cache HIT and reported one session on a machine running
// thirty-nine.
//
// Three variables, each closing one hole:
//
//	CLAUDE_CONFIG_DIR  what is read (already set before P4).
//	XDG_STATE_HOME     where the cache may be written. Without it a test writes
//	                   to the user's live cache.
//	PATH               emptied to a temp dir, so `claude` cannot be found and no
//	                   child process is started at all. A test suite that
//	                   spawns the vendor CLI is slow, environment-dependent, and
//	                   in this repository's history the start of every
//	                   process-accumulation problem it has had.
//
// It returns the state directory so a test can assert what landed there.
func hermeticTree(t *testing.T, claudeRoot string) string {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeRoot)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("PATH", t.TempDir())
	return filepath.Join(stateHome, "tt")
}

// TestSessionsJSONAnswersFromAHermeticRegistry runs the real CLI path — flag
// parsing, the real registryfs and proclive adapters, the real core, the real
// encoder — against a registry tree in t.TempDir() via CLAUDE_CONFIG_DIR.
//
// The fixture record carries NO tmux reference, so no tmux command is issued at
// all (§P1: the pane gate is not asked when nothing references a pane). The pid
// is this test process, which is the only pid a test can honestly call alive.
func TestSessionsJSONAnswersFromAHermeticRegistry(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatalf("seeding the fixture tree: %v", err)
	}
	pid := os.Getpid()
	record := fmt.Sprintf(`{
  "pid": %d,
  "sessionId": "11111111-2222-3333-4444-555555555555",
  "cwd": "/home/user/dotfiles",
  "startedAt": 1789684811204,
  "version": "2.1.275",
  "kind": "interactive",
  "name": "hermetic-cli-fixture",
  "nameSource": "user",
  "updatedAt": 1789699206204,
  "status": "idle",
  "statusUpdatedAt": 1789699206204
}`, pid)
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), []byte(record), 0o600); err != nil {
		t.Fatalf("seeding the fixture record: %v", err)
	}
	stateDir := hermeticTree(t, root)

	var stdout, stderr bytes.Buffer
	if code := runSessions([]string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, want 0; stderr:\n%s", code, stderr.String())
	}

	// Decoded with encoding/json into an untyped map on purpose: §5.1 bans a
	// json-tagged struct outside `wire`, and a second description of the wire
	// shape in a test is exactly what that ban exists to prevent.
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("the --json output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if envelope["schema"] != float64(1) {
		t.Fatalf("schema = %v, want 1", envelope["schema"])
	}
	list, ok := envelope["sessions"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("sessions = %v, want exactly the one seeded record", envelope["sessions"])
	}
	got := list[0].(map[string]any)
	if got["name"] != "hermetic-cli-fixture" {
		t.Fatalf("name = %v", got["name"])
	}
	if got["live"] != true {
		t.Fatalf("live = %v, want true: this process's own pid, and no pane reference to gate on", got["live"])
	}
	if _, present := got["statusAgeMs"]; !present {
		t.Fatal("statusAgeMs is absent from the payload; omitempty is banned and AC3 rests on the key existing")
	}

	// The tree is READ-ONLY to us. The seeded record is the only file, and it
	// is byte-identical after the run.
	after, err := os.ReadFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)))
	if err != nil {
		t.Fatalf("re-reading the fixture: %v", err)
	}
	if string(after) != record {
		t.Fatal("the CLI modified the registry record it read")
	}
	entries, err := os.ReadDir(sessions)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the registry directory has %d entries after the run, want 1 (err %v)", len(entries), err)
	}

	// And nothing was written outside tt's own state directory. With `claude`
	// absent from PATH there is no listing to publish, so the directory need not
	// exist at all — what must be true is that whatever the run wrote is in
	// there and nowhere else.
	if written, err := os.ReadDir(stateDir); err == nil {
		for _, e := range written {
			switch e.Name() {
			case "agents-cache.json", "agents-cache.lock":
			default:
				t.Fatalf("the run left %s in the state directory", e.Name())
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("reading the state directory: %v", err)
	}
	claudeEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the fixture tree: %v", err)
	}
	if len(claudeEntries) != 1 || claudeEntries[0].Name() != "sessions" {
		t.Fatalf("the Claude tree gained entries: %v", names(claudeEntries))
	}
}

// names is for a failure message, and exists because a []os.DirEntry prints as
// a list of pointers.
func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestATimedOutRefreshLeavesNoLockBehind is the live run's finding, as a test.
//
// `tt sessions --refresh --timeout=80ms` against a ~150 ms spawn expires the
// caller's context while the spawn goroutine still holds the cross-process
// lock. Before the drain in runSessions, the process exited first and left the
// lock file on disk — where §4.3(b)'s abandoned-lock rule makes every other tt
// process on the machine answer without corroboration until its mtime ages past
// spawnTimeout + 1 s.
//
// The fake `claude` here is a real child process that really outlives the
// deadline, because the thing being tested is what happens at process exit.
func TestATimedOutRefreshLeavesNoLockBehind(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	stateDir := hermeticTree(t, root)

	// A `claude` that takes far longer than the deadline. It goes on the PATH
	// hermeticTree just emptied.
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// /bin/sleep by absolute path: hermeticTree emptied the PATH, and the child
	// inherits it — the first version of this script reported `sleep: command
	// not found` (exit 127) instead of outliving the deadline.
	script := "#!/bin/sh\n/bin/sleep 5\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := runSessions([]string{"--json", "--refresh", "--timeout=80ms"}, &stdout, &stderr)
	elapsed := time.Since(start)

	// Either exit code is correct and which one appears is a property of the
	// MACHINE, not of this phase: --timeout bounds the whole read, so a tree
	// small enough to finish inside the remaining budget answers (0) and a real
	// ~/.claude with thirty-six records does not (1, as the live run showed).
	// Both are asserted rather than one, because pinning the fixture's answer
	// here would make this test a statement about how fast a temp directory is.
	if code != jsonout.ExitOK && code != jsonout.ExitNoRead {
		t.Fatalf("exit code %d, want 0 or 1; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "timeout") {
		t.Fatalf("stderr does not report the rejected refresh:\n%s", stderr.String())
	}
	if elapsed > 4*time.Second {
		t.Fatalf("the command took %s: the drain waited for the child instead of killing it", elapsed)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("reading the state directory: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "agents-cache.lock" {
			t.Fatal("a lock file survived a timed-out refresh: every other tt process " +
				"on the machine now answers without corroboration until it ages out")
		}
	}
}

// TestTheHumanSurfaceNamesTheSessions is the whole of the contract on the table
// (§5.2: the human output is explicitly not a contract beyond this).
func TestTheHumanSurfaceNamesTheSessions(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	pid := os.Getpid()
	record := fmt.Sprintf(`{"pid":%d,"sessionId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",`+
		`"cwd":"/home/user/dotfiles","startedAt":1789684811204,"kind":"interactive",`+
		`"name":"table-fixture","nameSource":"user","status":"idle","statusUpdatedAt":1789699206204}`, pid)
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), []byte(record), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	hermeticTree(t, root)

	var stdout, stderr bytes.Buffer
	if code := runSessions(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "table-fixture") {
		t.Fatalf("the table does not name the session:\n%s", stdout.String())
	}
}

func TestAnAbsentClaudeTreeIsAnAnswerNotAFailure(t *testing.T) {
	// "Claude has never run here" is a fact (§6.6). Exit 0, zero sessions.
	hermeticTree(t, filepath.Join(t.TempDir(), "nothing-here"))

	var stdout, stderr bytes.Buffer
	if code := runSessions([]string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, want 0; stderr:\n%s", code, stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, stdout.String())
	}
	counts := envelope["counts"].(map[string]any)
	if counts["total"] != float64(0) {
		t.Fatalf("counts.total = %v, want 0", counts["total"])
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mainBody(t *testing.T, fset *token.FileSet, name, src string) []ast.Stmt {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "main" {
			return fn.Body.List
		}
	}
	t.Fatalf("%s has no func main", name)
	return nil
}

func render(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, n); err != nil {
		t.Fatalf("printing: %v", err)
	}
	return b.String()
}

// printedSource is a file's source with every comment removed, so a structural
// assertion about what the code REFERENCES is not satisfied or broken by prose.
// The first draft of this test failed on cli.go's own comment explaining why it
// must not call resolveTheme.
func printedSource(t *testing.T, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return render(t, fset, f)
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// headMainGo is tools/tt/main.go as of e57489f — the commit this phase started
// from. It is embedded rather than read from git so the assertion works in a
// worktree, an archive, or a CI checkout with no history.
const headMainGo = `// Command tt is a modal terminal UI (k9s / lazygit style) for managing tmux
// sessions: list, filter, attach, create, rename, and kill sessions.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// Decide light vs dark before the TUI starts — the detection needs the
	// terminal to itself for one round trip. See resolveTheme.
	resolveTheme()

	p := tea.NewProgram(newModel(), tea.WithAltScreen())

	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tt:", err)
		os.Exit(1)
	}

	// The TUI has released the terminal. If a session was selected with
	// "enter", attach to it now — after Run() returns — so tmux can fully take
	// over the terminal.
	m, ok := final.(model)
	if !ok || m.selected == "" {
		return
	}

	if err := attach(m.selected); err != nil {
		fmt.Fprintln(os.Stderr, "tt:", err)
		os.Exit(1)
	}
}
`

// ---------------------------------------------------------------------------
// §2.4 — the `observe` dispatch (P7)
//
// EVERY test here passes a flag that makes runObserve return. A bare
// `runSubcommand("observe", nil, …)` binds the real ~/.local/state/tt socket,
// arms kqueue watches over the real ~/.claude and blocks until SIGTERM — which
// is correct behaviour for the command and catastrophic inside a test binary.
// That is not hypothetical: P2's unknown-command list carried "observe" as a
// name that did not exist yet, and the first run of the gate after this phase
// wired it hung for nine minutes doing precisely that.
// ---------------------------------------------------------------------------

func TestObserveIsAKnownCommandAndItsHelpIsASuccessOnStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := runSubcommand("observe", []string{"--help"}, &stdout, &stderr)
	if !handled {
		t.Fatal("observe is not a known command")
	}
	if code != 0 {
		t.Fatalf("--help exited %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "tt observe") {
		t.Fatalf("--help did not print the usage on stdout:\n%s", stdout.String())
	}
	// What matters is that a user who ASKED for help finds it on stdout. That it
	// lands there ONLY, and exactly once, is cli_usage_test.go's job.
	for _, want := range []string{"--socket-dir", "--stop", "no timer of any kind"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("the usage text does not mention %q:\n%s", want, stdout.String())
		}
	}
}

// The top-level usage lists observe, so a user who mistypes a command is told
// it exists. A usage message that named only `sessions` after P7 shipped would
// be the same lie P2's comment warned about, in the other direction.
func TestTheTopLevelUsageListsObserve(t *testing.T) {
	if !strings.Contains(usage, "observe") {
		t.Fatalf("the top-level usage does not list observe:\n%s", usage)
	}
}

// A usage error stays a usage error: exit 2, nothing on stdout, and NO server.
func TestObserveFlagErrorsAreUsageErrorsWithNothingOnStdout(t *testing.T) {
	for _, args := range [][]string{
		{"--nonsense"},
		{"--timeout=0"},
		{"--timeout=-1s"},
		{"leftover"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runObserve(args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code %d, want 2; stderr:\n%s", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("a usage error wrote to stdout: %q", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("a usage error said nothing on stderr")
			}
		})
	}
}

// --stop against a directory with no pid file is a successful no-op, and it
// touches nothing. It is also the one form of `tt observe` a test can run end
// to end without binding anything.
func TestObserveStopWithNoServerIsASuccessfulNoOp(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runObserve([]string{"--stop", "--socket-dir=" + dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no server is running") {
		t.Fatalf("stderr does not say there was nothing to stop:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("--stop wrote to stdout: %q", stdout.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the state dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("--stop created %d entries in the state directory", len(entries))
	}
}

// The socket directory defaults to tt's own state directory and is honoured
// from the flag. Asserted through the SAME resolution the command uses, so the
// default cannot drift into ~/.claude (hard constraint 1) or into /tmp.
func TestTheObserveSocketDirectoryDefaultsToTTsOwnStateDirectory(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	// P9 turned parseObserveFlags' five returns into one struct (it gained
	// --pane and --status-line); the assertions below are the ones this test
	// always made, read off the struct's fields.
	f, code, ok := parseObserveFlags(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok {
		t.Fatalf("parsing no flags failed with code %d", code)
	}
	if want := filepath.Join(stateHome, "tt"); f.Dir != want {
		t.Fatalf("the socket dir is %q, want %q", f.Dir, want)
	}
	if f.Stop {
		t.Fatal("--stop defaulted to true")
	}
	if f.Timeout != 5*time.Second {
		t.Fatalf("the spawn bound is %s, want 5s", f.Timeout)
	}

	explicit := filepath.Join(t.TempDir(), "elsewhere")
	f, _, ok = parseObserveFlags([]string{"--socket-dir=" + explicit}, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok || f.Dir != explicit {
		t.Fatalf("--socket-dir gave %q, want %q", f.Dir, explicit)
	}
}

// The unavailable-watcher stand-in reports on-demand rather than exiting:
// §6.6's rule is that a degraded observer is a correct observer that cannot
// push, and a machine that will not give us a kqueue must still answer.
func TestAnUnavailableWatcherDegradesToOnDemandRatherThanFailing(t *testing.T) {
	w := unavailableWatcher{err: errors.New("kqueue: too many open files")}

	if _, err := w.Watch(context.Background()); err == nil {
		t.Fatal("the stand-in claimed it could watch")
	}
	rep, err := w.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.Degraded != observer.DegradedKqueueUnavailable {
		t.Fatalf("degraded = %q, want %q", rep.Degraded, observer.DegradedKqueueUnavailable)
	}
	if rep.Armed != 0 || rep.ByKind == nil {
		t.Fatalf("the stand-in reported %+v, want nothing armed and a usable ByKind", rep)
	}
}

// TestTheCorroborationSpawnCannotEatTheWholeReadBudget is the regression test
// for the defect the AC14 spot-check found: with `claude agents --json` wedged,
// `tt sessions --json --refresh` spent its entire --timeout inside the spawn and
// then had no context left to answer with — measured at --timeout=20s as
// "context deadline exceeded" and no output after 20.008s, against 0.53s and 37
// sessions for the same read without --refresh.
//
// The rule is arithmetic, so the test is arithmetic: half the budget, capped at
// the §4.3 production bound of 5s, and never zero.
func TestTheCorroborationSpawnCannotEatTheWholeReadBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		read time.Duration
		want time.Duration
	}{
		{"the default 5s read halves to 2.5s", 5 * time.Second, 2500 * time.Millisecond},
		{"a 20s read is capped at the 5s production bound", 20 * time.Second, 5 * time.Second},
		{"a 10s read halves to exactly the bound", 10 * time.Second, 5 * time.Second},
		{"a 200ms read halves to 100ms", 200 * time.Millisecond, 100 * time.Millisecond},
		{"a 1ns read keeps its whole self rather than becoming zero", time.Nanosecond, time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := corroborationBudget(tc.read); got != tc.want {
				t.Errorf("corroborationBudget(%s) = %s, want %s", tc.read, got, tc.want)
			}
		})
	}

	// The property that matters, over every budget a user could type: the spawn
	// never gets all of it, so an answer always has room after a failed spawn.
	for _, read := range []time.Duration{
		time.Millisecond, 100 * time.Millisecond, time.Second,
		5 * time.Second, 30 * time.Second, 5 * time.Minute,
	} {
		if got := corroborationBudget(read); got >= read && read > time.Nanosecond {
			t.Errorf("a %s read gives the spawn %s, which leaves nothing to answer with", read, got)
		}
	}
}

// --auto marks a hook-created PANE. On its own it would silently start a
// server nobody asked for, so it is a usage error rather than ignored; with
// --pane it reaches the pane options.
func TestAutoIsOnlyAcceptedWithPane(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stderr bytes.Buffer
	if _, code, ok := parseObserveFlags([]string{"--auto"}, &bytes.Buffer{}, &stderr); ok || code != 2 {
		t.Fatalf("--auto alone: ok=%v code=%d, want a usage error (2)", ok, code)
	}
	if !strings.Contains(stderr.String(), "--auto is only meaningful with --pane") {
		t.Fatalf("the refusal does not say why:\n%s", stderr.String())
	}

	f, code, ok := parseObserveFlags([]string{"--pane", "--auto"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok {
		t.Fatalf("--pane --auto failed with code %d", code)
	}
	if !f.Pane || !f.Auto {
		t.Fatalf("--pane --auto parsed as pane=%v auto=%v", f.Pane, f.Auto)
	}

	f, _, ok = parseObserveFlags([]string{"--pane"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok || f.Auto {
		t.Fatalf("--pane alone parsed auto=%v, want false", f.Auto)
	}
}
