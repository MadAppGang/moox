package main

// P9's tests for `tt observe --pane` (§5.5b).
//
// NO TEST HERE STARTS OR TOUCHES A TMUX SERVER. Both tmux seams are stubbed
// and the assertions are about which commands WOULD have been issued, with
// what arguments and in what scope. The rule is absolute for this repo: a
// scratch tmux server has twice destroyed this user's workspace (2026-08-16, a
// run-shell segfault that killed all 16 sessions; 2026-08-17, a scratch server
// that cloned the whole workspace and destroyed 16 sessions / 44 windows /
// 71 panes). The isolated-server gate is §9.6's, behind a build tag, and it is
// run by hand — see tmuxlive_gate_test.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
)

// ---------------------------------------------------------------------------
// the two refusals, which happen before anything is drawn
// ---------------------------------------------------------------------------

// Outside tmux there is no pane to render into, so it refuses with exit 2 and
// issues no tmux command at all.
func TestThePaneRendererRefusesToRunOutsideTmux(t *testing.T) {
	rec := newTmuxRecorder(nil)
	rec.install(t)

	var stdout, stderr strings.Builder
	code := runObservePane(paneOptions{Pane: ""}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "must be run inside tmux") {
		t.Fatalf("stderr does not say why:\n%s", stderr.String())
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("it issued %d tmux calls with no pane to render into: %v", len(calls), calls)
	}
}

// The claim lost: another live pane IN THIS WINDOW already holds @tt_obs, so
// tmux did not set this pane's flag, this process exits 0, names the holder,
// and issues no write of its own — the pane closes itself.
func TestAnExistingObserverPaneInThisWindowMakesTheNewOneExitWithoutWriting(t *testing.T) {
	rec := newTmuxRecorder(nil)
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
		rec.record(args)
		switch args[0] {
		case "if-shell":
			return "\n", nil // display-message printed an empty @tt_obs: the claim lost
		case "list-panes":
			return "%3 1\n%7 \n%9 \n", nil
		}
		return "", nil
	})
	stubSeam(t, &tmuxSet, func(args ...string) error {
		rec.record(args)
		return nil
	})

	var stdout, stderr strings.Builder
	code := runObservePane(paneOptions{Pane: "%9"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code %d, want 0: finding an observer is not a failure", code)
	}
	if !strings.Contains(stdout.String(), "already open at %3") {
		t.Fatalf("stdout does not name the existing pane:\n%s", stdout.String())
	}
	for _, c := range rec.all() {
		if len(c) > 0 && c[0] == "set-option" {
			t.Fatalf("the refused pane still wrote an option: %v", c)
		}
	}
}

// The claim is ONE tmux invocation — test and set together, so no second
// renderer's test can land between them (the L8 race P10 measured) — and it is
// WINDOW-scoped: it names this pane as its target, loops over that window's
// panes with #{P:…}, and never passes -a. A server-wide guard made the pane
// flash and vanish when the toggle was pressed in a second session, and it
// would make scope="all", where one observer per window is the intended state,
// impossible.
func TestTheSingleInstanceGuardIsWindowScopedAndNeverAsksTheWholeServer(t *testing.T) {
	var calls [][]string
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "1\n", nil
	})

	won, _, err := claimObserverPane("%9")
	if err != nil || !won {
		t.Fatalf("claimObserverPane = (%v, %v), want a win when tmux reads back 1", won, err)
	}
	if len(calls) != 1 {
		t.Fatalf("the claim took %d tmux invocations, want exactly 1 (test and set must not be separable): %v",
			len(calls), calls)
	}
	got := calls[0]
	want := []string{"if-shell", "-F", "-t", "%9", paneClaimCondition("%9"),
		"set-option -p -t %9 @tt_obs 1", ";", "display-message", "-p", "-t", "%9", "#{@tt_obs}"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("the claim is\n  %q\nwant\n  %q", got, want)
	}
	for _, a := range got {
		if a == "-a" {
			t.Fatalf("the claim passed -a (%v): that is the server-wide scope H9 removed", got)
		}
	}
	if !strings.HasPrefix(paneClaimCondition("%9"), "#{==:#{P:") {
		t.Fatalf("the condition does not loop over THIS window's panes: %s", paneClaimCondition("%9"))
	}
}

// The claim ignores THIS pane's own flag (a restarted renderer in the same
// pane is not blocked by its own residue), panes without the flag, and DEAD
// panes (a remain-on-exit corpse's flag belongs to nobody); the loser's
// message ignores the same things when it names the holder.
func TestTheGuardIgnoresItsOwnPaneAndPanesWithoutTheFlag(t *testing.T) {
	cond := paneClaimCondition("%9")
	for _, part := range []string{"#{!=:#{pane_id},%9}", "#{!=:#{pane_dead},1}", "#{==:#{@tt_obs},1}"} {
		if !strings.Contains(cond, part) {
			t.Errorf("the claim condition lacks %q:\n%s", part, cond)
		}
	}

	for _, tc := range []struct {
		name  string
		panes string
		want  string
	}{
		{"only me, flagged", "%9 1\n", "another pane in this window"},
		{"nobody flagged", "%1 \n%2 \n", "another pane in this window"},
		{"the holder is named", "%1 \n%2 1\n%9 \n", "%2"},
		{"a malformed line is skipped", "\n%2\n%3 1\n", "%3"},
		{"a zero flag is not a flag", "%2 0\n", "another pane in this window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
				if args[0] != "list-panes" || args[1] != "-t" || args[2] != "%9" {
					t.Fatalf("the holder lookup is %v, want list-panes -t %%9 (this pane's window)", args)
				}
				return tc.panes, nil
			})
			if got := observerHolder("%9"); got != tc.want {
				t.Fatalf("observerHolder = %q, want %q", got, tc.want)
			}
		})
	}
}

// A tmux that cannot be reached is an ERROR, not a win: the claim is also the
// pane's first write, and a renderer that cannot mark its own pane is one the
// toggle could never find to close.
func TestAnUnreadableTmuxDoesNotBlockTheObserver(t *testing.T) {
	stubSeam(t, &tmuxCapture, func(...string) (string, error) { return "", stubErr("no server") })
	won, _, err := claimObserverPane("%1")
	if err == nil || won {
		t.Fatalf("claimObserverPane on an unreachable tmux = (%v, %v), want an error and no claim", won, err)
	}

	var stdout, stderr strings.Builder
	if code := runObservePane(paneOptions{Pane: "%1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code %d, want 1 when the claim cannot be made", code)
	}
}

// $TMUX_PANE is interpolated into the claim's format, so anything that is not
// a pane id is refused before any tmux call.
func TestAMalformedPaneIDIsRefusedBeforeAnyTmuxCall(t *testing.T) {
	rec := newTmuxRecorder(nil)
	rec.install(t)
	var stdout, stderr strings.Builder
	if code := runObservePane(paneOptions{Pane: "%1},#{@x"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code %d, want 2", code)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("a malformed pane id reached tmux: %v", calls)
	}
}

// ---------------------------------------------------------------------------
// the writes tt is allowed to make on its own pane
// ---------------------------------------------------------------------------

// The renderer writes only PANE-scoped options on ITS OWN pane: @tt_obs inside
// the atomic claim, remain-on-exit off, and the unset on the way out. §2.3
// enumerates these; nothing else is written, and every destructive verb stays
// in the user's own config.
func TestThePaneRendererWritesOnlyTheTwoPaneScopedOptionsOnItsOwnPane(t *testing.T) {
	// resolveTheme is on this path (§5.5b step 4) and it probes the terminal.
	// TERM_THEME is an explicit statement, which theme.go honours before any
	// probe — so this test stays hermetic instead of asking tmux or the tty.
	t.Setenv("TERM_THEME", "dark")

	rec := newTmuxRecorder(nil)
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
		rec.record(args)
		if args[0] == "if-shell" {
			return "1\n", nil
		}
		return "", nil
	})
	stubSeam(t, &tmuxSet, func(args ...string) error {
		rec.record(args)
		return nil
	})
	// A manual pane at scope off with no server takes the local-watch branch;
	// failing it makes the renderer print one line and return instead of
	// starting bubbletea — this test is about the option writes.
	stubSeam(t, &localSource, func(context.Context, observer.Clock) (panelSource, string, error) {
		return nil, "", stubErr("not used")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stubSeam(t, &paneContext, func() (context.Context, func()) { return ctx, func() {} })

	var stdout, stderr strings.Builder
	if code := runObservePane(paneOptions{Pane: "%42", Scope: scopeOff, SocketDir: t.TempDir()}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d, want 0 (stderr: %s)", code, stderr.String())
	}

	calls := rec.all()
	if len(calls) == 0 || calls[0][0] != "if-shell" {
		t.Fatalf("the first tmux call is %v, want the atomic claim", calls)
	}
	claim := calls[0]
	if inner := claim[5]; inner != "set-option -p -t %42 @tt_obs 1" {
		t.Fatalf("the claim's write is %q, want a pane-scoped set on %%42", inner)
	}

	var writes [][]string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "set-option" {
			writes = append(writes, c)
		}
	}
	want := [][]string{
		{"set-option", "-p", "-t", "%42", "remain-on-exit", "off"},
		{"set-option", "-pu", "-t", "%42", "@tt_obs"},
	}
	if len(writes) != len(want) {
		t.Fatalf("the renderer issued %d direct option writes, want %d (remain-on-exit, then the unset): %v",
			len(writes), len(want), writes)
	}
	for i, w := range want {
		if strings.Join(writes[i], " ") != strings.Join(w, " ") {
			t.Errorf("write %d is %v, want %v", i, writes[i], w)
		}
	}
	// Every write is -p or -pu: pane-scoped, on this pane. A global option
	// holding a reference is exactly what broke in C1.
	for _, w := range writes {
		if w[1] != "-p" && w[1] != "-pu" {
			t.Errorf("write %v is not pane-scoped", w)
		}
	}
}

// Code review MEDIUM 5, the case it named: at scope != "off" the pane NEVER
// arms a watch of its own — not with a server running (it attaches) and not
// without one (it waits and says so). 36 panes each arming ~57 descriptors is
// the cost §5.5(b2) exists to refuse.
func TestAtScopeAllWithNoServerThePaneRefusesToArmItsOwnWatch(t *testing.T) {
	stubSeam(t, &localSource, func(context.Context, observer.Clock) (panelSource, string, error) {
		t.Fatal("the pane armed a watch at scope != off")
		return nil, "", nil
	})
	stubSeam(t, &dialObserverSocket, func(context.Context, string) (socketClient, error) {
		return nil, stubErr("connect: no such file or directory")
	})
	stubSeam(t, &armSocketDirWatch, func(ctx context.Context, _ string) (dirWait, error) {
		return blockingDirWait(ctx), nil
	})

	for _, running := range []bool{false, true} {
		dir := t.TempDir()
		if running {
			writeRunningPidFile(t, dir)
		}
		ctx, cancel := context.WithCancel(context.Background())
		src, _, err := panePanelSource(ctx, paneOptions{Pane: "%1", Scope: "all", SocketDir: dir})
		if err != nil {
			cancel()
			t.Fatalf("server running=%v: panePanelSource: %v", running, err)
		}
		if _, ok := src.(*socketSource); !ok {
			cancel()
			t.Fatalf("server running=%v: the source is %T, want the socket source", running, src)
		}
		if !running {
			ch, err := src.Subscribe(ctx, observer.Filter{})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			ev := <-ch
			if ev.Type != observer.EventResync || ev.Snapshot != nil || ev.Reason != noteObserverNotRunning {
				cancel()
				t.Fatalf("the first event is %+v; it must say there is no observer", ev)
			}
			// EXACTLY the constant, not a sentence containing it: the panel
			// matches it exactly to decide whether to draw the start button.
			if !panelOffline(ev.Reason) {
				cancel()
				t.Fatalf("the note %q does not put the panel in its offline state, so no button is drawn", ev.Reason)
			}
		}
		cancel()
	}
}

// An AUTO pane (hook-created) is a thin renderer even at scope "off": the
// hooks only run at other scopes, but a pane that outlives a scope change must
// not start watching for itself.
func TestAnAutoPaneNeverArmsItsOwnWatchEvenAtScopeOff(t *testing.T) {
	stubSeam(t, &localSource, func(context.Context, observer.Clock) (panelSource, string, error) {
		t.Fatal("an auto pane armed a watch")
		return nil, "", nil
	})
	src, _, err := panePanelSource(context.Background(), paneOptions{
		Pane: "%1", Scope: scopeOff, Auto: true, SocketDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*socketSource); !ok {
		t.Fatalf("the source is %T, want the socket source", src)
	}
}

// A manual pane at scope "off" shares a RUNNING server's watch rather than
// arming its own — the half of MEDIUM 5 that applies even to the toggle.
func TestAtScopeOffWithAServerRunningThePaneAttachesInsteadOfWatching(t *testing.T) {
	stubSeam(t, &localSource, func(context.Context, observer.Clock) (panelSource, string, error) {
		t.Fatal("the pane armed its own watch with a server running")
		return nil, "", nil
	})
	dir := t.TempDir()
	writeRunningPidFile(t, dir)
	src, _, err := panePanelSource(context.Background(), paneOptions{Pane: "%1", Scope: scopeOff, SocketDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*socketSource); !ok {
		t.Fatalf("the source is %T, want the socket source", src)
	}
}

// At scope "off" with no server, the one manual pane MAY self-observe, which
// is the other half of the same rule.
func TestAtScopeOffThePaneArmsItsOwnWatch(t *testing.T) {
	armed := false
	fake := newFakePanelSource()
	stubSeam(t, &localSource, func(context.Context, observer.Clock) (panelSource, string, error) {
		armed = true
		return fake, "kqueue", nil
	})

	src, note, err := panePanelSource(context.Background(), paneOptions{
		Pane: "%1", Scope: scopeOff, SocketDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("panePanelSource at scope=off: %v", err)
	}
	if !armed || src == nil {
		t.Fatal("scope=off did not arm a local watch")
	}
	if note != "kqueue" {
		t.Fatalf("note = %q, want the watch mode the source reported", note)
	}
}

// writeRunningPidFile makes observerRunning report a live server: a pid file
// naming THIS process.
func writeRunningPidFile(t *testing.T, dir string) {
	t.Helper()
	rec := []byte(`{"pid":` + itoa(os.Getpid()) + `,"startedAt":"2026-09-18T12:00:00.000Z","socket":"` +
		filepath.Join(dir, sockserve.SocketName) + `","tt":"devel"}`)
	if err := os.WriteFile(filepath.Join(dir, sockserve.PidName), rec, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// scope and server detection
// ---------------------------------------------------------------------------

// @tt_obs_scope defaults to "off" — an unset option, an empty value and an
// unreadable tmux all mean the safe value, which is the one whose behaviour is
// exactly the toggle and nothing else.
func TestTheScopeDefaultsToOffOnAnAbsentOptionOrAnUnreadableTmux(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		err   error
		want  string
	}{
		{"unset", "", nil, scopeOff},
		{"whitespace", "  \n", nil, scopeOff},
		{"all", "all\n", nil, "all"},
		{"sessions", "sessions", nil, "sessions"},
		{"unreadable", "", stubErr("no server"), scopeOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
				if len(args) != 3 || args[0] != "show" || args[1] != "-gv" || args[2] != optObsScope {
					t.Fatalf("the scope read is %v, want show -gv %s", args, optObsScope)
				}
				return tc.value, tc.err
			})
			if got := tmuxScope(); got != tc.want {
				t.Fatalf("tmuxScope() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The server probe reads the pid file and the process, and never dials, never
// signals and never cleans up: a stale pair is "not running", and removing it
// is --stop's job in the one package permitted to write.
func TestTheServerProbeReadsThePidFileAndLeavesItAlone(t *testing.T) {
	dir := t.TempDir()

	if running, _ := observerRunning(dir); running {
		t.Fatal("an empty directory reported a running server")
	}
	if running, _ := observerRunning(""); running {
		t.Fatal("an empty directory path reported a running server")
	}

	path := filepath.Join(dir, sockserve.PidName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if running, _ := observerRunning(dir); running {
		t.Fatal("an unparseable pid file reported a running server")
	}

	// A pid file naming THIS process is a running server, and the file is
	// still there afterwards.
	rec := []byte(`{"pid":` + itoa(os.Getpid()) + `,"startedAt":"2026-09-18T12:00:00.000Z","socket":"` +
		filepath.Join(dir, sockserve.SocketName) + `","tt":"devel"}`)
	if err := os.WriteFile(path, rec, 0o600); err != nil {
		t.Fatal(err)
	}
	running, pid := observerRunning(dir)
	if !running || pid != os.Getpid() {
		t.Fatalf("observerRunning = (%v, %d), want (true, %d)", running, pid, os.Getpid())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the probe removed the pid file: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// ---------------------------------------------------------------------------
// the pane program's keys
// ---------------------------------------------------------------------------

// In the pane program the plain letters ARE available, because there is no
// session list to search: `r` refreshes, q/esc/ctrl+c quit. `d` no longer
// does anything: ended sessions are not drawn, so there is nothing to show.
func TestThePaneProgramBindsThePlainLettersTheTUICannot(t *testing.T) {
	src := newFakePanelSource()
	m := newPaneModel(src, newPanelClock(time.Now()), "", paneOptions{})
	t.Cleanup(m.panel.close)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	pm := next.(paneModel)
	if cmd != nil || !pm.panel.open {
		t.Fatal("`d` did something")
	}

	next, cmd = pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("`q` did not quit")
	}
	if next.(paneModel).panel.open {
		t.Fatal("`q` left the panel open, so its subscription was never cancelled")
	}
}

// The reported bug, in Go: closing an AUTOMATIC sidebar closes the panel
// everywhere. `q` used to end one renderer and leave the other 35 sidebars
// sitting in their windows, because nothing it did was global. It now writes
// the same two options prefix C-o writes — the scope it is turning off,
// remembered, then `off` — and the other windows retire their sidebar through
// the reconcile the next time they are shown.
func TestQuittingAnAutomaticSidebarTurnsThePanelOffForTheWholeServer(t *testing.T) {
	var calls [][]string
	stubSeam(t, &tmuxSet, func(args ...string) error {
		calls = append(calls, args)
		return nil
	})

	src := newFakePanelSource()
	m := newPaneModel(src, newPanelClock(time.Now()), "", paneOptions{Auto: true})
	t.Cleanup(m.panel.close)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("`q` did not quit")
	}
	if next.(paneModel).panel.open {
		t.Fatal("`q` left the panel open, so its subscription was never cancelled")
	}
	if len(calls) != 1 {
		t.Fatalf("tmux calls: %v, want exactly one command list", calls)
	}
	got := strings.Join(calls[0], " ")
	const want = "set-option -gF @tt_obs_scope_prev #{?#{==:#{@tt_obs_scope},off},#{@tt_obs_scope_prev},#{@tt_obs_scope}} ; set-option -g @tt_obs_scope off"
	if got != want {
		t.Fatalf("the write was\n  %s\nwant\n  %s", got, want)
	}
}

// A MANUAL pane is the user's own split, and quitting it closes that pane and
// nothing else. It must not touch a global option: the scope is not its to
// change, and a user who splits one in by hand at scope=all would otherwise
// take every other window's sidebar down with it.
func TestQuittingAManualPaneWritesNoGlobalOption(t *testing.T) {
	var calls [][]string
	stubSeam(t, &tmuxSet, func(args ...string) error {
		calls = append(calls, args)
		return nil
	})

	src := newFakePanelSource()
	m := newPaneModel(src, newPanelClock(time.Now()), "", paneOptions{})
	t.Cleanup(m.panel.close)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
	} {
		if _, cmd := m.Update(key); cmd == nil {
			t.Fatalf("%v did not quit", key)
		}
	}
	// esc backs out of things and never closes the sidebar (the user,
	// 2026-10-20).
	if next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil || !next.(paneModel).panel.open {
		t.Fatal("esc closed the sidebar")
	}
	if len(calls) != 0 {
		t.Fatalf("a manual pane wrote %v", calls)
	}
}

// Every quit key means the same thing in an automatic sidebar. `q` is the one
// a user presses; ctrl+c is the one a user presses by accident, and a panel
// that came back on the next window switch after it would read as broken
// rather than as forgiving. esc is not a quit key.
func TestEveryQuitKeyClosesTheAutomaticPanelTheSameWay(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"q", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			stubSeam(t, &tmuxSet, func(args ...string) error { calls++; return nil })
			m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), "", paneOptions{Auto: true})
			t.Cleanup(m.panel.close)
			if _, cmd := m.Update(tc.key); cmd == nil {
				t.Fatal("did not quit")
			}
			if calls != 1 {
				t.Fatalf("%d tmux calls, want 1", calls)
			}
		})
	}
}

// The pane program renders the panel and nothing else, and it carries the
// note its source came with — the watch mode, or why there is no data.
func TestThePaneProgramRendersThePanelAndCarriesItsSourcesNote(t *testing.T) {
	src := newFakePanelSource()
	m := newPaneModel(src, newPanelClock(time.Now()), "observer gone — local watch", paneOptions{})
	t.Cleanup(m.panel.close)

	next, _ := m.Update(tea.WindowSizeMsg{Width: panelWidth, Height: 20})
	view := next.(paneModel).View()
	if !strings.Contains(view, "[P]roject") {
		t.Fatalf("the pane does not render the panel:\n%s", view)
	}
	if !strings.Contains(view, "observer gone") {
		t.Fatalf("the pane does not carry its source's note:\n%s", view)
	}
}

// `r` on a source that cannot refresh is inert rather than a lie.
func TestRefreshIsInertOnASourceThatCannotRefresh(t *testing.T) {
	src := newFakePanelSource()
	m := newPaneModel(src, newPanelClock(time.Now()), "", paneOptions{})
	t.Cleanup(m.panel.close)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Fatal("`r` produced a command on a source with no Rebuild")
	}
}

// ---------------------------------------------------------------------------
// the start button
// ---------------------------------------------------------------------------

// A pane with no observer used to be a dead end: it named a command and left
// the user to go and type it somewhere else. It now offers the thing itself,
// on a key and on a click.
func TestTheStartButtonAppearsExactlyWhenThereIsNoObserverToAttachTo(t *testing.T) {
	for _, tc := range []struct {
		note string
		want bool
	}{
		{noteObserverNotRunning, true},
		{noteObserverStopped, true},
		{"", false},
		{"via tt observe (pid 9)", false},
		{"watch degraded: kqueue", false},
		// The degraded variants embed the note but describe a pane that can
		// never see a server appear, so starting one would be a lie.
		{noteObserverNotRunning + " - reopen this pane once it runs (cannot watch /x: no)", false},
		{noteObserverStopped + " - reopen this pane once it runs (no)", false},
	} {
		p := newPanel(newFakePanelSource(), newPanelClock(time.Now()))
		t.Cleanup(p.close)
		p.open, p.width, p.height = true, panelWidth, 20
		p.note = tc.note
		if got := p.offline(); got != tc.want {
			t.Errorf("note %q: offline=%v, want %v", tc.note, got, tc.want)
		}
		view := p.view()
		if got := strings.Contains(view, panelStartLabel); got != tc.want {
			t.Errorf("note %q: button shown=%v, want %v\n%s", tc.note, got, tc.want, view)
		}
		if got := p.startButtonRow() >= 0; got != tc.want {
			t.Errorf("note %q: hit-test row present=%v, want %v", tc.note, got, tc.want)
		}
	}
}

// The button's row is where the click lands. It is the row the panel renders
// it on, whichever layout drew it — the waiting panel, a full one, and one
// truncated to a handful of lines.
func TestTheStartButtonsRowIsTheRowItIsDrawnOn(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		sessions int
		height   int
	}{
		{"waiting, no snapshot", -1, 20},
		{"a full panel", 3, 20},
		{"a panel truncated to its reserve", 40, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPanel(newFakePanelSource(), newPanelClock(now))
			t.Cleanup(p.close)
			p.open, p.width, p.height = true, panelWidth, tc.height
			p.note = noteObserverStopped
			if tc.sessions >= 0 {
				snap := &observer.Snapshot{}
				for i := 0; i < tc.sessions; i++ {
					snap.Sessions = append(snap.Sessions, observer.Session{
						SessionID: fmt.Sprintf("s%02d", i),
						Name:      fmt.Sprintf("session %02d", i),
						Status:    observer.StatusBusy,
					})
				}
				p.snap = snap
			}
			lines := p.lines()
			row := p.startButtonRow()
			if row < 0 {
				t.Fatalf("no button row:\n%s", strings.Join(lines, "\n"))
			}
			if row >= len(lines) || !strings.Contains(lines[row], panelStartLabel) {
				t.Fatalf("row %d is not the button in:\n%s", row, strings.Join(lines, "\n"))
			}
			if p.height > 0 && len(lines) > p.height {
				t.Fatalf("the panel drew %d lines into a height of %d", len(lines), p.height)
			}
		})
	}
}

// Pressing it. `s` is the documented key, `enter` is the one a person tries on
// a drawn button, and a left click on its row is the button in the sense the
// user asked for.
func TestTheStartButtonIsPressedByKeyAndByClick(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
		want bool
	}{
		{"s", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(panelStartKey)}, true},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, true},
		{"a click on the button", tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: -1}, true},
		{"a click above the button", tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 0}, false},
		{"a release, not a press", tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, Y: -1}, false},
		{"another letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dirs []string
			stubSeam(t, &spawnObserver, func(dir string) error {
				dirs = append(dirs, dir)
				return nil
			})
			m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), noteObserverNotRunning,
				paneOptions{Auto: true, SocketDir: "/tmp/sock"})
			t.Cleanup(m.panel.close)
			m.panel.width, m.panel.height = panelWidth, 20

			msg := tc.msg
			if mouse, ok := msg.(tea.MouseMsg); ok && mouse.Y < 0 {
				// -1 means "the button's own row", resolved here so the case
				// table does not have to know the layout.
				mouse.Y = m.panel.startButtonRow()
				if mouse.Y < 0 {
					t.Fatal("the fixture has no button to click")
				}
				msg = mouse
			}
			next, cmd := m.Update(msg)
			if cmd != nil {
				if _, ok := cmd().(observerStartMsg); !ok && tc.want {
					t.Fatalf("the command did not report a start")
				}
			}
			if got := len(dirs) == 1; got != tc.want {
				t.Fatalf("spawned %d times, want %v (%v)", len(dirs), tc.want, dirs)
			}
			if tc.want {
				if dirs[0] != "/tmp/sock" {
					t.Fatalf("the server was started in %q, not the pane's own socket dir", dirs[0])
				}
				if !strings.Contains(next.(paneModel).panel.view(), "starting") {
					t.Fatalf("the panel does not say it is starting:\n%s", next.(paneModel).panel.view())
				}
			}
		})
	}
}

// A second press while the first is in flight starts nothing: one button, one
// server. Nor does a press when there is an observer already — `s` is inert
// then, like every other letter the panel does not bind.
func TestTheStartButtonStartsOneServerAndOnlyWhenThereIsNone(t *testing.T) {
	var calls int
	stubSeam(t, &spawnObserver, func(string) error { calls++; return nil })

	m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), noteObserverNotRunning, paneOptions{})
	t.Cleanup(m.panel.close)
	m.panel.width, m.panel.height = panelWidth, 20

	press := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(panelStartKey)}
	next, cmd := m.Update(press)
	if cmd == nil {
		t.Fatal("the first press did nothing")
	}
	cmd()
	pm := next.(paneModel)
	if _, again := pm.Update(press); again != nil {
		t.Fatal("a second press while starting produced a second start")
	}
	if calls != 1 {
		t.Fatalf("%d spawns, want 1", calls)
	}

	// An attached panel: the button is gone and so is the key.
	pm.panel.note = "via tt observe (pid 9)"
	if _, c := pm.Update(press); c != nil {
		t.Fatal("`s` started a server while one was attached")
	}
	if calls != 1 {
		t.Fatalf("%d spawns, want 1", calls)
	}
}

// A refusal is reported in the pane that asked for it. The button is pressable
// again afterwards, because the reason may be one the user can fix.
func TestAStartThatFailsSaysWhyAndLeavesTheButtonPressable(t *testing.T) {
	stubSeam(t, &spawnObserver, func(string) error {
		return errors.New("one is already listening (pid 7)")
	})
	m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), noteObserverNotRunning, paneOptions{})
	t.Cleanup(m.panel.close)
	m.panel.width, m.panel.height = panelWidth, 20

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(panelStartKey)})
	if cmd == nil {
		t.Fatal("the press did nothing")
	}
	msg := cmd()
	next, _ := m.Update(msg)
	pm := next.(paneModel)
	if pm.starting {
		t.Fatal("the model is still starting after a failure")
	}
	if !strings.Contains(pm.panel.note, "already listening") {
		t.Fatalf("the panel note is %q; it does not say why", pm.panel.note)
	}
}

// The sidebar holds the mouse for its whole life (the user, 2026-10-20): the
// wheel and the clicks reach it instead of putting the pane in copy mode.
func TestTheSidebarHoldsTheMouse(t *testing.T) {
	m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), "", paneOptions{})
	t.Cleanup(m.panel.close)
	if !m.mouse {
		t.Fatal("a new sidebar does not want the mouse")
	}
	m.panel.note = "via tt observe (pid 9)"
	next, _ := m.Update(panelErrMsg{})
	if !next.(paneModel).mouse {
		t.Fatal("an attached sidebar let the mouse go")
	}
}

// The spawn itself, against real processes rather than a double: a child that
// refuses is reported in its own words, and one that keeps running is a
// success. The binary is a seam precisely so this test cannot start a server.
func TestStartingTheObserverReportsAChildThatRefusesAndAcceptsOneThatRuns(t *testing.T) {
	stubSeam(t, &observerStartProbe, time.Second)
	// The gate runs first (moox §4.5); here it starts nothing, so the plain
	// observer start below is what is measured. It is told THIS server.
	var ensured []string
	stubSeam(t, &ensureServe, func(exe, socket string) { ensured = append(ensured, socket) })
	t.Cleanup(func() {
		if len(ensured) == 0 || ensured[0] != ttTarget().Socket {
			t.Errorf("tt serve --ensure was run for %q, want this server %q first", ensured, ttTarget().Socket)
		}
	})
	dir := t.TempDir()

	// Each script is exec'd once before it is measured. MEASURED on this
	// machine: the FIRST exec of a freshly written executable costs >150 ms
	// (macOS inspects it), which is not the latency this probe is about and
	// would make the case a timing lottery.
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		_ = exec.Command(path, "warm").Run()
		return path
	}
	refuses := write("refuses", `[ "$1" = warm ] || echo "no" >&2; exit 1`)
	runs := write("runs", `[ "$1" = warm ] && exit 0; exec sleep 5`)

	stubSeam(t, &observerExecutable, func() (string, error) { return refuses, nil })
	err := startObserverProcess("")
	if err == nil {
		t.Fatal("a child that exited 1 was reported as a start")
	}
	if !strings.Contains(err.Error(), "exited at once") {
		t.Fatalf("the error is %q; it does not say the child went straight away", err)
	}

	// The ordinary refusal is named from state this process can read, not from
	// the child's output: a pid file naming a LIVE server means one is already
	// listening. (The pid is this test, which is certainly alive.)
	withPid := t.TempDir()
	if err := os.MkdirAll(withPid, 0o700); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"pid":%d,"socketPath":%q,"startedAt":"2026-01-01T00:00:00Z","schema":1}`,
		os.Getpid(), filepath.Join(withPid, "observer.sock"))
	if err := os.WriteFile(filepath.Join(withPid, sockserve.PidName), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := startObserverProcess(withPid); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("a refusal against a live pid file reported %v, want \"already listening\"", err)
	}

	stubSeam(t, &observerExecutable, func() (string, error) { return runs, nil })
	if err := startObserverProcess(""); err != nil {
		t.Fatalf("a child that kept running was reported as a failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the sidebar's width
// ---------------------------------------------------------------------------

// tmux scales panes PROPORTIONALLY when a window grows, and a LAYOUT lays out
// every pane including this one — measured, and both reported: "they all thre
// have equal widht", and "when i do prfix + E … that make th panel be part of
// difretn layotu". tmux has no pinned pane, so the renderer puts itself back:
// a resize when it is still docked, and a join-pane back to the right edge when
// a layout has moved it out of place.
func TestTheSidebarPutsItselfBackWhereASidebarBelongs(t *testing.T) {
	// reply fields: window_width, @tt_obs_width, @tt_obs_min_width,
	// at_right/top/bottom, the window's pane ids.
	// The whole re-dock, as the four calls it is: park, break out, re-apply the
	// layout to the panes left behind, join back, unpark.
	redock := strings.Join([]string{
		"set-option -w -t @3 @tt_obs_relayout 2 ; set-option -p -t %7 @tt_obs_moving 1 ; break-pane -d -s %7",
		"select-layout -t @3",
		"join-pane -h -f -l 46 -s %7 -t @3",
		"set-option -pu -t %7 @tt_obs_moving ; set-option -wu -t @3 @tt_obs_relayout",
	}, " | ")
	// prefix E's even spread is redone as a spread, not as the last preset,
	// and its flag cleared with it.
	spread := strings.Join([]string{
		"set-option -w -t @3 @tt_obs_relayout 2 ; set-option -p -t %7 @tt_obs_moving 1 ; break-pane -d -s %7",
		"select-layout -E -t @3 ; set-option -wu -t @3 @tt_obs_spread",
		"join-pane -h -f -l 46 -s %7 -t @3",
		"set-option -pu -t %7 @tt_obs_moving ; set-option -wu -t @3 @tt_obs_relayout",
	}, " | ")
	for _, tc := range []struct {
		name     string
		observed int
		reply    string
		auto     bool
		want     string
	}{
		{"prefix E spread the sidebar with the rest", 64, "260\t46\t80\t111\t%7 %8 %9 \t@3\t1\t1", true, spread},
		{"stretched by a window that grew", 92, "260\t46\t80\t111\t%7 %8 \t@3\t", true,
			"resize-pane -t %7 -x 46"},
		{"moved into a layout's grid", 100, "260\t46\t80\t100\t%8 %7 %9 \t@3\t", true, redock},
		{"a grid cell that happens to be 46 wide", 46, "260\t46\t80\t011\t%8 %7 \t@3\t", true, redock},
		{"docked and the right width", 46, "260\t46\t80\t111\t%7 %8 \t@3\t", true, ""},
		{"a window too narrow to spare the columns", 36, "90\t46\t80\t111\t%7 %8 \t@3\t", true, ""},
		{"a width the user configured", 70, "300\t60\t80\t111\t%7 %8 \t@3\t", true,
			"resize-pane -t %7 -x 60"},
		{"options unset, so the defaults", 92, "260\t\t\t111\t%7 %8 \t@3\t", true,
			"resize-pane -t %7 -x 46"},
		{"undocked with nobody to join to", 92, "260\t46\t80\t000\t%7 \t@3\t", true,
			"resize-pane -t %7 -x 46"},
		{"a pane the user split in by hand", 92, "260\t46\t80\t000\t%7 %8 \t@3\t", false, ""},
		// The case the live gate caught: after even-horizontal the sidebar is
		// already at the right edge, merely too wide, so "am I out of place"
		// does not see it. tmux says a layout happened, and that is what
		// separates a re-dock from a resize.
		{"docked but a layout just ran", 72, "260\t46\t80\t111\t%7 %8 \t@3\t1", true, redock},
		{"docked, right width, and a layout just ran", 46, "260\t46\t80\t111\t%7 %8 \t@3\t1", true, redock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
				if args[0] != "display-message" {
					t.Fatalf("unexpected tmux read: %v", args)
				}
				return tc.reply + "\n", nil
			})
			stubSeam(t, &tmuxSet, func(args ...string) error {
				got = append(got, strings.Join(args, " "))
				return nil
			})
			m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), "",
				paneOptions{Auto: tc.auto, Pane: "%7"})
			t.Cleanup(m.panel.close)

			_, cmd := m.Update(tea.WindowSizeMsg{Width: tc.observed, Height: 20})
			if cmd != nil {
				cmd()
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("issued %v, want nothing", got)
				}
				return
			}
			if joined := strings.Join(got, " | "); joined != tc.want {
				t.Fatalf("issued\n  %s\nwant\n  %s", joined, tc.want)
			}
		})
	}
}

// The loop guard: a resize tmux will not make is asked for ONCE. Without it a
// renderer in a window that cannot spare the columns would ask, be refused,
// see the same width come back, and ask again for ever.
func TestTheWidthIsNotFoughtForTwiceFromTheSameWidth(t *testing.T) {
	var calls int
	stubSeam(t, &tmuxCapture, func(...string) (string, error) { return "260\t46\t80\t111\t%7 %8 \t@3\t\n", nil })
	stubSeam(t, &tmuxSet, func(...string) error { calls++; return nil })

	m := newPaneModel(newFakePanelSource(), newPanelClock(time.Now()), "",
		paneOptions{Auto: true, Pane: "%7"})
	t.Cleanup(m.panel.close)

	next, cmd := m.Update(tea.WindowSizeMsg{Width: 92, Height: 20})
	cmd()
	pm := next.(paneModel)
	// tmux refused: the same wrong width arrives again.
	next, cmd = pm.Update(tea.WindowSizeMsg{Width: 92, Height: 20})
	if cmd != nil {
		cmd()
	}
	if calls != 1 {
		t.Fatalf("%d resizes for one refused correction, want 1", calls)
	}

	// A DIFFERENT wrong width is a new stretch, and is corrected.
	// (The guard is on THIS path only. Being out of place is never guarded —
	// a join either moves the pane or errors, and the live gate caught what
	// guarding it costs: a layout whose cell was the same width as the last
	// one went unfixed.)
	pm = next.(paneModel)
	next, cmd = pm.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if cmd != nil {
		cmd()
	}
	if calls != 2 {
		t.Fatalf("%d resizes after a new stretch, want 2", calls)
	}

	// And once it settles at the configured width, a later stretch is fair
	// game again — the guard is not a one-shot for the life of the pane.
	pm = next.(paneModel)
	next, _ = pm.Update(tea.WindowSizeMsg{Width: 46, Height: 20})
	pm = next.(paneModel)
	_, cmd = pm.Update(tea.WindowSizeMsg{Width: 92, Height: 20})
	if cmd != nil {
		cmd()
	}
	if calls != 3 {
		t.Fatalf("%d resizes after settling and stretching again, want 3", calls)
	}

	// Out of place at a width already refused: joined anyway, every time.
	stubSeam(t, &tmuxCapture, func(...string) (string, error) { return "260\t46\t80\t010\t%7 %8 \t@3\t\n", nil })
	for i := 0; i < 3; i++ {
		next, cmd = pm.Update(tea.WindowSizeMsg{Width: 92, Height: 20})
		if cmd == nil {
			t.Fatalf("attempt %d: a pane out of place was left there", i+1)
		}
		cmd()
		pm = next.(paneModel)
	}
	// Three re-docks, four calls each, on top of the three resizes above.
	if calls != 3+3*4 {
		t.Fatalf("%d commands after three layouts moved it, want %d", calls, 3+3*4)
	}
}
