package main

// pane.go is the other half of S4: `tt observe --pane` (§5.5b), plus the one
// factory both panels use to obtain a data source (§5.5a's decision table).
//
// It is a separate file from cli.go deliberately. cli_test.go asserts
// structurally that cli.go mentions neither resolveTheme, newModel,
// tea.NewProgram nor attach( — §2.4 property 3, because the subcommand path
// writes machine-readable bytes to stdout and resolveTheme writes an OSC 11
// query to the tty. `--pane` is the one subcommand that IS a TUI, so the
// identifiers that property bans live here and cli.go only calls in.
//
// # What `--pane` is, and what it refuses to be
//
//	tt observe --pane implies --no-serve. It NEVER binds the socket. It is a
//	renderer. --pane with any server-only flag (--stop) is a usage error,
//	exit 2.
//
// # The claim is ATOMIC and WINDOW-scoped
//
// A pane claims "I am this window's observer" with ONE tmux invocation:
//
//	tmux if-shell -F -t %N '<no other live pane in this window has @tt_obs=1>' \
//	     'set-option -p -t %N @tt_obs 1' \; display-message -p -t %N '#{@tt_obs}'
//
// tmux runs a client's command list to completion before it looks at another
// client's, so the test and the set cannot be separated by a second renderer's
// test: exactly one of two racing panes reads back "1", and the other exits.
// That replaces §5.5(b)'s read-then-write guard, whose race (L8) was the
// measured cause of P10's case 7 and case 10 failures — two renderers both read
// "no observer" before either wrote its flag, and neither exited. `if-shell -F`
// forks no shell and creates no job (-F: "shell-command is not executed"), so
// this is not the run-shell family that segfaulted the server on 2026-08-16.
//
// The condition's `#{P:…}` loop is the panes of THIS window and nothing wider.
// Revision 1's server-wide guard (`list-panes -a`) meant that opening the
// observer in one session and pressing the toggle in another split a pane
// which immediately saw the first window's flag and exited, and it contradicted
// §7.1/§5.5(a). Under @tt_obs_scope = "all" one observer pane per window is the
// INTENDED state and a server-wide guard would make it impossible. The liveness
// gate elsewhere in tt keeps -a, because "does this pane still exist anywhere
// on the server" genuinely is a server-wide question (§2.3). Two questions, two
// scopes, stated so neither is tidied into the other.
//
// # The tmux writes, and why they are permitted
//
// `set-option -p @tt_obs 1` (inside the claim) and `set-option -p
// remain-on-exit off`, both on THIS pane, and `set-option -pu @tt_obs` on the
// way out. A pane option dies with its pane, so it cannot go stale — which is
// the single property whose absence produced C1: revision 0 stored a pane id in
// a GLOBAL option, it outlived the pane it described, and the next toggle acted
// on a corpse. Nothing here creates or destroys a pane; split-window and
// kill-pane stay in the user's own config, executed by tmux on the user's
// behalf.
//
// There is one GLOBAL write, and one key press is the only thing that makes it:
// `q` in an --auto sidebar sets @tt_obs_scope off for the server, because the
// panel is one thing and closing it closes it everywhere
// (closePanelEverywhere). It is the same pair of writes prefix C-o performs,
// and it is deliberately NOT on the signal path — kill-pane delivers SIGHUP,
// including the kill-pane that closes a window whose own panes have ended.
//
// NOTHING IN THIS FILE STARTS A TMUX SERVER. Every call is issued into an
// existing server through $TMUX_PANE, and `tmux run-shell` is not used in any
// form (2026-08-16: a run-shell from a client that had gone away segfaulted the
// server and killed all 16 sessions).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/proclive"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// The tmux user options S4 reads. tt READS all of them and WRITES four: the
// pane-scoped @tt_obs and, while it is re-docking itself, @tt_obs_moving —
// both on its own pane — the global @tt_status the status-line
// writer owns (§2.3, widened in §5.5c), and — only when `q` is pressed in an
// automatic sidebar — the global @tt_obs_scope pair the toggle writes. Every
// one of these is set by the user in their own ~/.tmux.conf; tt never edits
// that file.
const (
	optObsFlag      = "@tt_obs"
	optObsScope     = "@tt_obs_scope"
	optObsScopePrev = "@tt_obs_scope_prev"

	scopeOff = "off"
)

// The pane program runs without bubbletea's renderer (pane_paint.go). That
// renderer runs a ticker and, on EVERY wake, copies the frame buffer to
// compare it with the last frame — whether or not anything changed. One TUI
// can afford that; one per window cannot. MEASURED at scope="all", 8 idle
// renderers on this machine: ~3.5 ms of CPU per second each at 60 fps. A
// lower rate (it ran at 4 fps) trades that for key latency, since bubbletea
// v1 paints only on its ticker: 240–260 ms per key, measured on the live
// sidebar (the user, 2026-10-06: "why do we have almost 1 second delay when
// we move the cursor?"). MEASURED on this machine's real snapshot, one idle
// renderer, 20 s: 1.1 ms of CPU per second at 4 fps, 3.3 at 30. The painter
// pays neither: it paints right after each Update, and nothing when idle.

// paneSpawnTimeout is the corroboration spawn bound for a pane renderer. It is
// the same 5 s default the CLI and the server use: §4.3(a) makes the spawn
// bound "one timeout, every surface".
const paneSpawnTimeout = 5 * time.Second

// tmuxCapture runs a tmux command and returns its stdout. It is a variable so
// every test in this package can substitute a recorder: NOTHING in tt's test
// suite may reach a real tmux server (2026-08-17: a scratch server cloned the
// whole workspace and destroyed 16 sessions / 44 windows / 71 panes).
var tmuxCapture = func(args ...string) (string, error) {
	out, err := tmuxCommand(context.Background(), args...).Output()
	return string(out), err
}

// tmuxSet issues one tmux option write and folds stderr into the error.
var tmuxSet = func(args ...string) error { return runTmux(args...) }

// paneOptions is everything `tt observe --pane` needs, resolved from argv and
// the environment before anything is drawn.
type paneOptions struct {
	// Pane is $TMUX_PANE: the pane this process was started in.
	Pane string
	// SocketDir is where a running `tt observe` would have bound.
	SocketDir string
	// Scope is @tt_obs_scope, defaulting to "off" — the value that gets a user
	// who installed tt and changed nothing exactly the toggle and nothing else.
	Scope string
	// Auto marks a pane the scope hooks created (`tt observe --pane --auto`).
	// An auto pane is a thin renderer and nothing more: it attaches to the one
	// `tt observe` server or waits for it, and never arms a watch of its own,
	// whatever the scope option says. The flag is also the marker the hooks
	// use to tell their own panes from the user's toggle when a window leaves
	// the scope — it is visible to tmux as part of #{pane_start_command}.
	Auto bool
}

// paneIDPattern is what $TMUX_PANE looks like. The id is interpolated into the
// claim's format string, so anything else is refused rather than quoted.
var paneIDPattern = regexp.MustCompile(`^%[0-9]+$`)

// runObservePane is `tt observe --pane`.
//
// The order of the steps is §5.5(b)'s lifecycle and each one can refuse:
//
//  1. $TMUX_PANE unset or malformed -> "must be run inside tmux", exit 2
//  2. the ATOMIC claim loses        -> print who holds it, exit 0 (the pane closes)
//  3. remain-on-exit off on this pane
//  4. resolveTheme() explicitly, since §2.4 skips it on subcommand paths
//  5. pick a data source: the server's socket, or (one manual pane at scope
//     "off" with no server) a watch of its own
//  6. run the panel until q/Esc/ctrl+c/SIGHUP/SIGTERM, then unset @tt_obs
func runObservePane(opts paneOptions, stdout, stderr io.Writer) int {
	if strings.TrimSpace(opts.Pane) == "" {
		fmt.Fprintln(stderr, "tt observe --pane: must be run inside tmux ($TMUX_PANE is unset)")
		return 2
	}
	if !paneIDPattern.MatchString(opts.Pane) {
		fmt.Fprintf(stderr, "tt observe --pane: $TMUX_PANE is %q, which is not a tmux pane id\n", opts.Pane)
		return 2
	}

	won, holder, err := claimObserverPane(opts.Pane)
	if err != nil {
		fmt.Fprintf(stderr, "tt observe --pane: %v\n", err)
		return 1
	}
	if !won {
		// Exit 0, not an error: the user asked for an observer in this window
		// and there is one. The pane closes itself (remain-on-exit is off, and
		// the documented launch command ends its shell with `; exit`) and the
		// existing observer is untouched.
		fmt.Fprintf(stdout, "tt: observer pane already open at %s\n", holder)
		return 0
	}
	defer func() {
		// Best-effort: if this process dies without unsetting it, nothing
		// leaks — tmux destroys a pane option with its pane.
		_ = tmuxSet("set-option", "-pu", "-t", opts.Pane, optObsFlag)
	}()
	// M5 is retired by measurement (remain-on-exit is off globally and on 0 of
	// 60 panes), and this line is kept anyway: it costs one call, and under
	// scope="all" there are 36 of these panes rather than one, so an
	// unconditional fail-safe is worth more than it was.
	if err := tmuxSet("set-option", "-p", "-t", opts.Pane, "remain-on-exit", "off"); err != nil {
		fmt.Fprintf(stderr, "tt observe --pane: %v\n", err)
	}

	resolveTheme()
	// Then follow the window's light/dark switch live (theme_live.go).
	startThemeReports(stdout)
	defer stopThemeReports(stdout)

	ctx, stopSignals := paneContext()
	defer stopSignals()

	src, note, err := panePanelSource(ctx, opts)
	if err != nil {
		// Only the local-watch branch can fail here (the socket source waits
		// rather than failing). One line, then idle until the pane is closed.
		fmt.Fprintln(stdout, err.Error())
		<-ctx.Done()
		return 0
	}

	// The painter draws the frames (pane_paint.go): on every change and only
	// the lines that changed, with no renderer ticking underneath. It also
	// owns the terminal modes, focus reporting included (tmux forwards focus
	// in/out with focus-events on, and answers the mode with the pane's
	// current state; they light the chrome).
	painter := &framePainter{out: stdout}
	painter.enter()
	defer painter.leave()
	p := tea.NewProgram(paintedPane{pane: newPaneModel(src, observer.SystemClock{}, note, opts), painter: painter},
		tea.WithContext(ctx), tea.WithOutput(stdout), tea.WithoutRenderer())
	restore := paintTerminal(ctx, p, os.Stdin, stdout)
	defer restore()
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) &&
		!errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "tt observe --pane: %v\n", err)
		return 1
	}
	return 0
}

// paneContext is the shutdown context, behind a seam so a test can drive the
// refusal paths without sending itself a signal.
var paneContext = paneSignalContext

// paneSignalContext is the shutdown path: q/Esc/ctrl+c end the program itself,
// and SIGHUP (which is what kill-pane delivers) and SIGTERM cancel this.
func paneSignalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signalNotify(ch, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signalStop(ch)
			cancel()
		})
	}
	go func() {
		select {
		case <-ch:
			stop()
		case <-ctx.Done():
		}
	}()
	return ctx, stop
}

// The signal plumbing is behind variables so a test can drive the shutdown
// path without sending itself a signal.
var (
	signalNotify = func(ch chan<- os.Signal, sigs ...os.Signal) { signal.Notify(ch, sigs...) }
	signalStop   = func(ch chan<- os.Signal) { signal.Stop(ch) }
)

// paneClaimCondition is the claim's test, evaluated by tmux in the context of
// pane `self`: "no OTHER live pane in this window carries @tt_obs=1".
//
// `#{P:…}` expands its argument once per pane of the target's window and
// concatenates the results, so the loop prints "x" for every other flagged
// pane and the whole condition is true exactly when it printed nothing. A dead
// pane (remain-on-exit) is excluded: its flag belongs to a process that no
// longer exists, and letting it block the claim would mean "never an observer
// again" in that window.
func paneClaimCondition(self string) string {
	return "#{==:#{P:#{?#{&&:#{!=:#{pane_id}," + self + "},#{&&:#{!=:#{pane_dead},1},#{==:#{" +
		optObsFlag + "},1}}},x,}},}"
}

// claimObserverPane is the ATOMIC single-instance guard (see the file
// comment). It reports whether this pane now holds the window's observer flag,
// and, when it lost, which pane holds it.
//
// A tmux that cannot be reached is an error rather than a win: the claim is
// also the first write, and a renderer that cannot mark its own pane cannot be
// found by the toggle that would close it.
func claimObserverPane(self string) (bool, string, error) {
	out, err := tmuxCapture("if-shell", "-F", "-t", self, paneClaimCondition(self),
		"set-option -p -t "+self+" "+optObsFlag+" 1",
		";", "display-message", "-p", "-t", self, "#{"+optObsFlag+"}")
	if err != nil {
		return false, "", fmt.Errorf("claiming %s as this window's observer: %w", self, err)
	}
	if strings.TrimSpace(out) == "1" {
		return true, "", nil
	}
	return false, observerHolder(self), nil
}

// observerHolder names the pane that won, for the one line a losing pane
// prints. `list-panes -t <pane>` is that pane's window and nothing wider.
func observerHolder(self string) string {
	out, err := tmuxCapture("list-panes", "-t", self, "-F", "#{pane_id} #{"+optObsFlag+"}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) == 2 && fields[0] != self && fields[1] == "1" {
				return fields[0]
			}
		}
	}
	return "another pane in this window"
}

// closePanelEverywhere is what `q` means in an AUTOMATIC sidebar: the panel is
// one thing, so closing it closes it everywhere, exactly as prefix C-o does.
// It writes the same two global options the binding writes — the scope it is
// turning off, remembered, and then `off` — and the windows you are not
// looking at lose their sidebar through the same reconcile, the next time they
// are shown.
//
// This is the THIRD tmux write tt performs, and the second global one (the
// status line owns @tt_status). It is here rather than in the renderer's
// shutdown path on purpose: only an explicit key press means it. SIGHUP does
// not, because kill-pane delivers SIGHUP — including the kill-pane the
// last-pane guard issues when a window closes, which must never turn the
// user's panel off across the server.
//
// A manual pane (`tt observe --pane` with no --auto) writes nothing: that pane
// is the user's own split, and quitting it closes that pane and no more.
func closePanelEverywhere() error {
	// The scope to come back to is remembered only while there is one: a
	// second close (two sidebars quitting, or one after the toggle) would
	// otherwise remember "off", and the toggle would then turn the panel
	// "back on" to off for ever (seen live on 2026-10-20).
	return tmuxSet("set-option", "-gF", optObsScopePrev,
		"#{?#{==:#{"+optObsScope+"},"+scopeOff+"},#{"+optObsScopePrev+"},#{"+optObsScope+"}}",
		";", "set-option", "-g", optObsScope, scopeOff)
}

// ---------------------------------------------------------------------------
// the start button's other half: starting `tt observe`
// ---------------------------------------------------------------------------

// observerStartProbe is how long a start attempt watches the child before it
// reports success. It exists for ONE outcome: a server that refuses at the
// bind and exits at once (another one already listening, a socket directory it
// will not write) must be reported in the pane that asked for it, rather than
// leaving a button that looks pressed and a panel that waits for ever.
//
// It is generous on purpose — the refusals it is looking for happen in
// milliseconds, and the cost of being wrong in this direction is a button that
// stays highlighted a moment longer.
// It is a var only so a test can shorten it; nothing in the program writes it.
var observerStartProbe = 1500 * time.Millisecond

// spawnObserver starts a `tt observe` server and reports why it could not.
// It is a variable because no test in this suite may start a real server: a
// double is substituted, and the one test that drives the real function drives
// it against a binary that exits.
var spawnObserver = startObserverProcess

// observerExecutable names the binary a start runs. It is os.Executable in
// production — THIS build, never a PATH lookup — and a seam in tests, which
// must never start a real server.
var observerExecutable = os.Executable

// ensureServe runs `<exe> serve --ensure --tmux-socket <socket>` and waits
// for it, bounded: --ensure always exits 0 and prints nothing, so there is
// nothing to report — the caller looks at the observer's pid file next. A
// variable so the sidebar tests never run a gate.
var ensureServe = func(exe, socket string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "serve", "--ensure", "--tmux-socket", socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Run()
}

// startObserverProcess is `tt observe`, detached from this pane.
//
// Detached is the point. The server is the machine's, not this sidebar's: it
// serves every other window's renderer and the status line, and it outlives the
// pane that happened to start it. kill-pane delivers SIGHUP to the pane's
// process group, so without Setsid the first window closed would take the
// observer down with it.
//
// The binary is THIS one (os.Executable), never a PATH lookup: a sidebar
// started from a build under test must not start the installed server, and the
// gate's fixture PATH would otherwise decide which one ran.
//
// # Why the child's stderr goes to /dev/null, and the refusal is INFERRED
//
// The obvious design is a pipe: capture the child's first line and show it. It
// is a trap, and the reason is worth keeping. The read end of that pipe belongs
// to THIS renderer, so it closes when the sidebar closes — and `tt observe`
// writes to stderr long after it starts ("the watch loop stopped", "the status
// line stopped", "stopped"). A Go program whose write to fd 2 fails with EPIPE
// takes SIGPIPE and dies. The server would then be killed, silently, by a pane
// that had merely been closed — the exact failure a detached server exists to
// avoid, reintroduced in order to print one line.
//
// So the child gets /dev/null, and a child that exits inside the probe window
// is explained from state this process can read for itself: a pid file naming a
// live server is the ordinary refusal ("one is already listening"), and
// anything else is reported as the exit status it was.
//
// # tt serve first (moox §4.5)
//
// The button first runs `tt serve --ensure --tmux-socket <this server>`, the
// same gate moox.conf runs: on a server @moox-sockets matches it starts the
// per-server daemon, which hosts the observer, and that is the answer. Only
// when no observer is listening afterwards — a server moox does not take part
// in, where the user still wants the panel — does it start a plain `tt
// observe`, on this server.
func startObserverProcess(dir string) error {
	exe, err := observerExecutable()
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	socket := ttTarget().Socket
	if before, _ := observerRunning(dir); socket != "" && !before {
		ensureServe(exe, socket)
		if after, _ := observerRunning(dir); after {
			return nil
		}
	}
	args := []string{"observe"}
	if strings.TrimSpace(dir) != "" {
		args = append(args, "--socket-dir", dir)
	}
	if socket != "" {
		args = append(args, "--tmux-socket", socket)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped in the background: an unwaited child that exits would sit as a
	// zombie for as long as this renderer lives.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// A ONE-SHOT timer, armed by a key press and stopped on either outcome.
	// §9.3a's rule permits exactly this shape and bans the others, which is why
	// this is time.NewTimer and not time.After.
	probe := time.NewTimer(observerStartProbe)
	defer probe.Stop()
	select {
	case err := <-done:
		if running, pid := observerRunning(dir); running {
			return fmt.Errorf("one is already listening (pid %d)", pid)
		}
		if err != nil {
			return fmt.Errorf("it exited at once: %w", err)
		}
		return errors.New("it exited at once")
	case <-probe.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// keeping the sidebar at its own width
// ---------------------------------------------------------------------------

// The sidebar's width defaults, used when the option is unset — a user running
// tt without the config block still gets a sidebar of a sane size. They are the
// same numbers the shipped block sets.
const (
	defaultObsWidth    = 46
	defaultObsMinWidth = 80
)

// panePlace is what one tmux round-trip tells a renderer about where it is:
// what its width should be, whether the window can spare the columns, whether
// it is still DOCKED at the right edge, and who else is in the window.
type panePlace struct {
	want   int
	room   bool
	docked bool
	// relayout is @tt_obs_relayout: tmux telling this pane that a layout was
	// just applied to its window.
	relayout bool
	// spread is @tt_obs_spread: the layout was prefix E's even spread
	// (select-layout -E), which is not a preset, so the re-dock must spread
	// again rather than re-apply the window's last preset.
	spread bool
	window string
	others []string
}

// panePlaceOf reads everything the dock check needs in ONE tmux call. A layout
// change is an event with a fork in it already; it does not need four.
//
// `#{pane_at_right}#{pane_at_top}#{pane_at_bottom}` is "111" exactly when this
// pane is the full-height column at the right edge — which is what a sidebar
// is, and what `split-window -h -f` made it.
func panePlaceOf(pane string) panePlace {
	out, err := tmuxCapture("display-message", "-p", "-t", pane,
		"#{window_width}\t#{@tt_obs_width}\t#{@tt_obs_min_width}\t"+
			"#{pane_at_right}#{pane_at_top}#{pane_at_bottom}\t#{P:#{pane_id} }\t#{window_id}\t"+
			"#{"+optObsRelayout+"}\t#{"+optObsSpread+"}")
	if err != nil {
		return panePlace{}
	}
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	if len(f) < 7 {
		return panePlace{}
	}
	atoi := func(i, fallback int) int {
		n, err := strconv.Atoi(strings.TrimSpace(f[i]))
		if err != nil || n <= 0 {
			return fallback
		}
		return n
	}
	place := panePlace{
		want:   atoi(1, defaultObsWidth),
		docked: strings.TrimSpace(f[3]) == "111",
	}
	// The same rule the hooks apply before they create one: below this, the
	// sidebar is not entitled to the columns and the reconcile will retire it
	// when the window is next shown. Fighting for them in the meantime would
	// crush the user's own pane, which is what the rule exists to prevent.
	place.room = atoi(0, 0) >= place.want+1+atoi(2, defaultObsMinWidth)
	for _, id := range strings.Fields(f[4]) {
		if id != pane {
			place.others = append(place.others, id)
		}
	}
	place.window = strings.TrimSpace(f[5])
	place.relayout = strings.TrimSpace(f[6]) == relayoutWanted
	place.spread = len(f) > 7 && strings.TrimSpace(f[7]) == "1"
	return place
}

// optObsMoving marks this pane as one that is mid-redock, parked for a moment
// in a window of its own. The last-pane guard in the user's config refuses to
// close a window while a pane there carries it — without that, a guard whose
// job is "a window with nothing but a sidebar in it should close" would close
// the one the sidebar is standing in.
const optObsMoving = "@tt_obs_moving"

// optObsSpread is the window option the user's prefix E binding sets before
// `select-layout -E` (tmux/.tmux.conf): the layout hook cannot tell an even
// spread from a preset, and the re-dock must redo the one the user asked for.
// select-layout with no argument re-applies the last PRESET, which -E is not:
// the sidebar's columns would all go to its neighbour (the user, 2026-10-20:
// prefix E gave 47 / 93 / 46).
const optObsSpread = "@tt_obs_spread"

// optObsRelayout is the window option the user's config sets from
// after-select-layout and after-next-layout. It is the only way this pane can
// know a LAYOUT happened rather than a window resize, and the two need
// different repairs: a resize just took columns, while a layout arranged the
// user's panes around a sidebar that should not have been in the arrangement.
// The renderer clears it as the last step of the re-dock, and CLAIMS it with a
// value of its own before it starts: the re-dock applies a layout, which fires
// the hook that sets this, and the hook only writes when nobody holds it.
// Without that claim the re-dock triggers itself (measured: the live gate saw
// the sidebar leave its window a second time after every layout).
const (
	optObsRelayout = "@tt_obs_relayout"
	// relayoutWanted is the hook's value: a layout the USER asked for.
	relayoutWanted = "1"
	// relayoutMine is the re-dock's own claim, which it ignores.
	relayoutMine = "2"
)

// redock is the sequence that puts the sidebar back at the right edge AND
// leaves the user's own panes in the layout they asked for.
//
// `join-pane` alone is not enough, and the user's screenshot is the proof: a
// three-pane window laid out even-horizontal comes back 57 / 46 / 115, because
// the columns the sidebar gives up go to whichever pane it was joined beside.
// The layout has to be applied to the user's panes with the sidebar NOT in the
// window — there is no tmux layout that excludes a pane, so it leaves:
//
//	set -p @tt_obs_moving 1   the guard must not close the window it is parked in
//	break-pane -d             out of the window, into one of its own, detached
//	select-layout             re-applies the window's LAST PRESET over the panes
//	                          that are left, which is the layout the user asked
//	                          for, for their panes alone (measured: a window
//	                          that never had a preset takes this as a no-op,
//	                          exit 0, not an error)
//	join-pane -h -f -l N      back as the full-height column at the right edge
//	set -pu @tt_obs_moving
//
// MEASURED, and the reason these are four separate tmux invocations rather than
// one command list: a failing command ABORTS the rest of a `;` list. If
// select-layout ever failed inside one, the join would never run and the
// sidebar would be stranded in a window of its own. Split like this, the join
// always runs.
func redockCmds(pane, window, want string, spread bool) [][]string {
	layout := []string{"select-layout", "-t", window}
	if spread {
		// prefix E's even spread, over the panes that are left.
		layout = []string{"select-layout", "-E", "-t", window, ";",
			"set-option", "-wu", "-t", window, optObsSpread}
	}
	return [][]string{
		{"set-option", "-w", "-t", window, optObsRelayout, relayoutMine, ";",
			"set-option", "-p", "-t", pane, optObsMoving, "1", ";",
			"break-pane", "-d", "-s", pane},
		layout,
		{"join-pane", "-h", "-f", "-l", want, "-s", pane, "-t", window},
		// Last, and in this order: the select-layout above fires the very hook
		// that sets this option, so clearing it earlier would leave it set and
		// buy one pointless extra re-dock.
		{"set-option", "-pu", "-t", pane, optObsMoving, ";",
			"set-option", "-wu", "-t", window, optObsRelayout},
	}
}

// keepPlace puts the sidebar back where a sidebar belongs: the full-height
// column at the right edge, @tt_obs_width wide.
//
// Two things move it, both measured on tmux 3.7c and both reported:
//
//	a window that GROWS   tmux scales every pane proportionally, so 46 columns
//	                      in a 120-column window becomes 92 at 260
//	a LAYOUT              select-layout lays out every pane in the window,
//	                      including this one — "prefix + M-1,2,3,4" puts the
//	                      sidebar in the grid with everything else
//
// tmux has no concept of a pinned or docked pane: there is no layout that
// excludes one, and no option that says "leave this pane alone". What it does
// have is `join-pane`, and MEASURED, a pane can be joined back into the window
// it is already in — `join-pane -h -f -l 46 -s <self> -t <a pane in the same
// window>` moves it out of the layout's grid and back to a full-height column
// at the right edge, in one command, without killing the renderer.
//
// The user's own panes are then in the layout they would have had without the
// sidebar, because the sidebar is always the LAST pane: tmux's presets fill in
// pane order, so removing the last cell leaves exactly the arrangement of the
// remaining N-1 (measured for tiled and even-horizontal).
//
// This lives here rather than in a tmux hook because a hook cannot name the
// pane it is about — `resize-pane -x` does not expand formats, and `:.{right}`
// is not this pane once a layout has moved it. The renderer knows its own id.
//
// The loop guard is the second observed width: a correction this process asked
// for comes back as another WindowSizeMsg, and if that carries the SAME wrong
// width, tmux would not or could not do it, and asking again is how a renderer
// spins for ever against a window that cannot spare the columns.
func (m paneModel) keepPlace(observed int) (paneModel, tea.Cmd) {
	if !m.opts.Auto || m.opts.Pane == "" || observed <= 0 {
		// A pane the user split in by hand is the user's, place included.
		return m, nil
	}
	place := panePlaceOf(m.opts.Pane)
	if place.want <= 0 || !place.room {
		return m, nil
	}
	pane, want := m.opts.Pane, strconv.Itoa(place.want)

	// Out of place: join it back, with NO guard. A join cannot silently
	// half-succeed the way a resize can — it either moves the pane or errors,
	// and a pane that is already docked is not joined at all, so there is no
	// loop to guard against. The guard was on this path once, and the live
	// gate caught what that costs: a layout whose cell happened to be the same
	// width as the previous one was refused, and the sidebar sat in the grid.
	// A layout was applied, or something moved this pane out of its column:
	// either way the repair is the same, and it is not a resize.
	if (place.relayout || !place.docked) && len(place.others) > 0 && place.window != "" {
		m.fixedFrom = 0
		cmds := redockCmds(pane, place.window, want, place.spread)
		return m, func() tea.Msg {
			for _, args := range cmds {
				_ = tmuxSet(args...)
			}
			return nil
		}
	}
	if observed == place.want {
		m.fixedFrom = 0 // settled: a later stretch may be corrected again
		return m, nil
	}
	// Docked but stretched. This one IS guarded: tmux may simply not have the
	// columns, and asking again for what it just refused is how a renderer
	// spins for ever.
	if observed == m.fixedFrom {
		return m, nil
	}
	m.fixedFrom = observed
	return m, func() tea.Msg {
		_ = tmuxSet("resize-pane", "-t", pane, "-x", want)
		return nil
	}
}

// tmuxScope reads @tt_obs_scope, defaulting to "off". A tmux that cannot be
// read, or an option that is unset, both mean "off" — the safe value, and the
// one whose behaviour is exactly revision 2's toggle-only panel.
func tmuxScope() string {
	out, err := tmuxCapture("show", "-gv", optObsScope)
	if err != nil {
		return scopeOff
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return scopeOff
	}
	return v
}

// ---------------------------------------------------------------------------
// §5.5(a)'s decision table — where a panel's data comes from
// ---------------------------------------------------------------------------

// openPanelSource is the seam the in-TUI panel opens its source through. It is
// a variable so a test can hand the panel a double without a watch, a socket
// or a Claude tree.
var openPanelSource = func(ctx context.Context, clock observer.Clock) (panelSource, string, error) {
	if clock == nil {
		clock = observer.SystemClock{}
	}
	return localSource(ctx, clock)
}

// localSource is the local-watch half of the decision table, behind a seam so
// a test can assert WHETHER a watch was armed — which is the whole of
// §5.5(b2)'s rule — without arming one.
var localSource = localPanelSource

// panePanelSource decides where a PANE renderer's data comes from. It is the
// fix for code review MEDIUM 5, whose finding was that the refusal to
// self-observe keyed on the server being ABSENT, so in the normal scope="all"
// case — a server running — every pane armed a watch of its own anyway.
//
//	an auto pane, or scope != "off" -> the socket source, ALWAYS: attached to
//	                                   the one server, or waiting for it on a
//	                                   directory watch. Never a watch of its
//	                                   own, server or no server.
//	a manual pane at scope "off"    -> the socket source when a server is
//	                                   running (share its one watch), else
//	                                   this pane's own watch — the one pane
//	                                   that may self-observe.
//
// A manual pane that attached keeps the socket source for its lifetime: if the
// server later stops it waits for it rather than arming a watch mid-flight,
// and the footer says so.
func panePanelSource(ctx context.Context, opts paneOptions) (panelSource, string, error) {
	scope := opts.Scope
	if scope == "" {
		scope = scopeOff
	}
	if opts.Auto || scope != scopeOff {
		return newSocketSource(opts.SocketDir), "", nil
	}
	if running, _ := observerRunning(opts.SocketDir); running {
		return newSocketSource(opts.SocketDir), "", nil
	}
	return localSource(ctx, observer.SystemClock{})
}

// observerRunning reports whether a `tt observe` server is alive, read from
// the pid file it writes and the process it names. Read-only: the socket is
// not dialled, nothing is signalled, and a stale pair is reported as "not
// running" rather than cleaned up (that is `--stop`'s job, in the one package
// permitted to remove files).
func observerRunning(dir string) (bool, int) {
	if strings.TrimSpace(dir) == "" {
		return false, 0
	}
	data, err := os.ReadFile(filepath.Join(dir, sockserve.PidName))
	if err != nil {
		return false, 0
	}
	rec, err := wire.DecodePidFile(data)
	if err != nil {
		return false, 0
	}
	alive, err := proclive.New().Alive(rec.PID)
	if err != nil || !alive {
		return false, rec.PID
	}
	return true, rec.PID
}

// localPanelSource arms this process's own watch and returns the loop as a
// panelSource. The loop runs under ctx, so closing the panel (or ending the
// pane program) stops the watch and releases every descriptor.
func localPanelSource(ctx context.Context, clock observer.Clock) (panelSource, string, error) {
	st, err := newObserverStack(clock, paneSpawnTimeout)
	if err != nil {
		return nil, "", err
	}

	watcher, note := newWatcher()
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:             st.obs,
		Refresh:          st.obs,
		Watch:            watcher,
		Tree:             watchkq.OSTree{},
		Jobs:             st.jobs,
		Clock:            clock,
		Paths:            st.paths,
		SubscriberBuffer: observeSubscriberBuffer,
	})
	if err != nil {
		return nil, "", err
	}

	go func() {
		// The loop's error is not fatal to the panel: §6.6 says a degraded
		// observer is a correct on-demand observer, and the panel's footer is
		// where that shows up.
		_ = loop.Run(ctx)
	}()

	return localStream{WatchLoop: loop, obs: st.obs}, note, nil
}

// localStream is the local-watch branch as the panel sees it: the loop's
// stream (Subscribe) and its re-read (Rebuild, the `r` key), plus the core's
// Enrich. The loop never reads a transcript — IncludeTranscripts is false for
// every watch-driven rebuild (§7.5f) — so without Enrich this branch could not
// show the colour, subagent and API-error facts the panel renders (code review
// round 2, A11). The panel's pump calls it (panel.go: panelEnricher).
type localStream struct {
	*observer.WatchLoop
	obs *observer.Observer
}

// Enrich is the core's own enrichment pass, cached per (session, path, size,
// mtime), so a session whose transcript has not grown is not read again.
func (s localStream) Enrich(ctx context.Context, snap *observer.Snapshot, ids []string) (*observer.Snapshot, error) {
	return s.obs.Enrich(ctx, snap, ids)
}

// ---------------------------------------------------------------------------
// the pane program's model
// ---------------------------------------------------------------------------

// paneModel is the standalone bubbletea model for `tt observe --pane`: the
// same panel as the TUI's, with nothing else on screen. Because there is no
// session list here, §5.5(a)'s plain-letter keys are available and are used —
// `d` shows the ENDED group, `r` refreshes, q/Esc/ctrl+c quit.
type paneModel struct {
	panel panel
	// init is the panel's opening command, captured by the CONSTRUCTOR rather
	// than produced in Init(). Init has a value receiver, so a panel opened
	// there would open a copy and the program would render an empty panel
	// forever — a subtle enough trap to be worth the field.
	init tea.Cmd
	// focusSeen is a focus change having arrived from tmux, which outranks
	// the answer to askFocus.
	focusSeen bool
	// opts is what this renderer was started with: --auto (so quitting means
	// "close the panel", not "close this pane") and the socket directory a
	// server it starts must bind in.
	opts paneOptions
	// starting is true between pressing the start button and hearing back, so
	// a second press does not start a second server.
	starting bool
	// fixedFrom is the last wrong width this pane asked tmux to correct, so a
	// correction tmux will not make is attempted once rather than for ever.
	fixedFrom int
	// mouse is whether the sidebar wants mouse reporting: always, for its
	// whole life (the user, 2026-10-20). Tabs, counts and rows are clickable
	// and the wheel scrolls (panel_mouse.go), where a pane that left the mouse
	// to tmux went into copy mode on the wheel; tmux still focuses the pane on
	// a click. The painter turns the mode on (pane_paint.go).
	mouse bool
}

func newPaneModel(src panelSource, clock observer.Clock, note string, opts paneOptions) paneModel {
	p := newPanel(src, clock)
	p.surface, p.navigable = surfacePane, true
	cmd := p.start()
	if note != "" {
		p.note = note
	}
	return paneModel{panel: p, init: cmd, opts: opts, mouse: true}
}

func (m paneModel) Init() tea.Cmd { return tea.Batch(m.init, m.askFocus()) }

// paneFocusFormat is true when keys typed in the attached client reach this
// pane: the active pane of the active window of an attached session.
const paneFocusFormat = "#{&&:#{pane_active},#{&&:#{window_active},#{session_attached}}}"

// paneFocusAnswer is tmux's answer to askFocus.
type paneFocusAnswer struct{ focused bool }

// askFocus asks tmux, once at start, whether this pane holds the focus: tmux
// forwards focus changes to a pane that asked for them, but never says which
// it was to begin with.
func (m paneModel) askFocus() tea.Cmd {
	pane := m.opts.Pane
	if pane == "" {
		return nil
	}
	return func() tea.Msg {
		out, err := tmuxCapture("display-message", "-p", "-t", pane, paneFocusFormat)
		if err != nil {
			return nil
		}
		return paneFocusAnswer{focused: strings.TrimSpace(out) == "1"}
	}
}

// paneFocusCheck is tmux's answer to checkFocus: unlike the start-up answer
// it is current, and is applied as it comes.
type paneFocusCheck struct{ focused bool }

// checkFocus asks tmux whether this pane still holds the focus.
func (m paneModel) checkFocus() tea.Cmd {
	pane := m.opts.Pane
	if pane == "" {
		return nil
	}
	return func() tea.Msg {
		out, err := tmuxCapture("display-message", "-p", "-t", pane, paneFocusFormat)
		if err != nil {
			return nil
		}
		return paneFocusCheck{focused: strings.TrimSpace(out) == "1"}
	}
}

// observerStartMsg is a start attempt's outcome, as a line for the panel.
type observerStartMsg struct{ note string }

// startObserver is the button's action, on the key and on the click alike.
func (m paneModel) startObserver() (tea.Model, tea.Cmd) {
	if m.starting || !m.panel.offline() {
		return m, nil
	}
	m.starting = true
	m.panel.note = "starting the observer…"
	dir := m.opts.SocketDir
	return m, func() tea.Msg {
		if err := spawnObserver(dir); err != nil {
			return observerStartMsg{note: "could not start it: " + err.Error()}
		}
		// Nothing more to say: the source is watching the socket directory and
		// attaches on its own, which replaces this note with the server's pid.
		return observerStartMsg{}
	}
}

// isCursorKey reports whether key moves the sidebar's cursor.
func isCursorKey(key string) bool {
	switch key {
	case keyScrollDown, keyScrollUp, "down", "up":
		return true
	}
	return false
}

func (m paneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The window's terminal switched light/dark (theme_live.go): the next
	// frame is drawn in the new theme.
	if followTheme(msg) {
		return m, nil
	}
	// bubbletea hands over every rune that came in one read as ONE key: a
	// held j arrives as "jjj", which matches no key and did nothing. Outside
	// the filter's prompt (which types them as text) each rune is its own key.
	if km, ok := msg.(tea.KeyMsg); ok && km.Type == tea.KeyRunes && len(km.Runes) > 1 && !km.Paste && !m.panel.filtering {
		var cmds []tea.Cmd
		var next tea.Model = m
		for _, r := range km.Runes {
			var cmd tea.Cmd
			next, cmd = next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: km.Alt})
			cmds = append(cmds, cmd)
		}
		return next, tea.Batch(cmds...)
	}
	switch msg := msg.(type) {
	case tea.FocusMsg:
		// Entering the sidebar puts the cursor on the pane you came from.
		m.panel.blurred, m.focusSeen = false, true
		return m, m.cameFrom()
	case paneCameFrom:
		m.panel.cursorToPane(msg.pane)
		return m, nil
	case tea.BlurMsg:
		m.panel.blurred, m.focusSeen = true, true
		return m, nil
	case paneFocusAnswer:
		// A focus change that came in first is newer than the answer.
		if !m.focusSeen {
			m.panel.blurred = !msg.focused
			if msg.focused {
				return m, m.cameFrom()
			}
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.panel.width = msg.Width
		m.panel.height = msg.Height
		next, place := m.keepPlace(msg.Width)
		return next, place

	case tea.KeyMsg:
		// A key reaches only the pane holding the focus, so a key IS the
		// focus, whether or not tmux's focus-in came first (the user,
		// 2026-10-20: a focused sidebar stayed at rest, keys and cursor
		// hidden).
		m.panel.blurred, m.focusSeen = false, true
		// The filter's prompt takes every key but ctrl+c while it is open:
		// letters type (q, g and the tab keys included), backspace erases,
		// enter keeps the filter and esc clears it.
		if m.panel.filtering && msg.String() != "ctrl+c" {
			switch msg.Type {
			case tea.KeyEsc:
				m.panel.clearFilter()
			case tea.KeyEnter:
				m.panel.keepFilter()
			case tea.KeyBackspace:
				m.panel.backspaceFilter()
			case tea.KeySpace:
				m.panel.typeFilter(" ")
			case tea.KeyRunes:
				m.panel.typeFilter(string(msg.Runes))
			}
			return m, nil
		}
		if m.narrow() && msg.String() == paneWidenKey {
			return m.widen()
		}
		// The "show only" picker takes the next key (panel_pick.go): a
		// section's key or a state's letter, or f/esc to close it. A cursor
		// key closes it and moves the cursor, which the picker hides, rather
		// than doing nothing (the user, 2026-10-06: "when I filter with f and
		// 1, the cursor navigation stopped working").
		if m.panel.picking && msg.String() != "ctrl+c" {
			if !isCursorKey(msg.String()) {
				m.panel.pickByKey(msg.String())
				return m, nil
			}
			m.panel.picking = false
		}
		switch msg.String() {
		case filterKey:
			m.panel.startFilter()
			return m, nil
		case pickKey:
			m.panel.pressPick()
			return m, nil
		case keyScrollDown, "down":
			m.panel.moveCursor(1)
			return m, nil
		case keyScrollUp, "up":
			m.panel.moveCursor(-1)
			return m, nil
		case "esc":
			// esc only ever backs out of something — the prompt, the picker,
			// the kept filters — and never closes the sidebar (the user,
			// 2026-10-20: "we should not close it on esc"); q does.
			m.panel.clearFilters()
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			m.panel.close()
			if m.opts.Auto {
				// The panel is global, so closing it is global. An unreachable
				// tmux is not worth a message in a pane that is going away.
				_ = closePanelEverywhere()
			}
			return m, tea.Quit
		case keyPanelMode:
			m.panel.cycleMode()
			return m, nil
		case keyPanelRefresh:
			return m, m.panel.refresh()
		case "enter":
			// With an observer, enter goes to the highlighted session
			// (panel_cursor.go); without one it presses the start button,
			// the only thing drawn to press.
			if !m.panel.offline() {
				return m.jump()
			}
			return m.startObserver()
		case panelStartKey:
			return m.startObserver()
		case popupKey:
			if !m.panel.offline() {
				return m.popup()
			}
			return m, nil
		}
		// The tabs' hotkeys, the letter each tab underlines (p, t, m, c, in
		// either case): straight to that grouping, saved like g's cycle. g
		// still cycles, and s stays the start button's key.
		if mode, ok := panelModeForKey(msg.String()); ok {
			m.panel.chooseMode(mode)
		}
		return m, nil

	case tea.MouseMsg:
		// The sidebar holds the mouse (paneModel.mouse, panel_mouse.go): the wheel
		// scrolls, and a click acts on what it lands on.
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelDown:
			m.panel.wheel(1)
		case tea.MouseButtonWheelUp:
			m.panel.wheel(-1)
		case tea.MouseButtonLeft:
			// tmux selects the pane it passes a click to (MouseDown1Pane), so a
			// click is the focus too; the wheel is not, tmux scrolls a pane
			// without selecting it.
			m.panel.blurred, m.focusSeen = false, true
			// Too narrow, the whole pane is the widen button: at one or two
			// columns there is nothing else to aim at.
			if m.narrow() {
				return m.widen()
			}
			if row := m.panel.startButtonRow(); row >= 0 && msg.Y == row {
				return m.startObserver()
			}
			// The prompt and the picker give way to a click, as to a cursor key.
			// A click on the row the cursor is already on goes there, like
			// enter: the first click selects, the second opens.
			if !m.panel.filtering {
				before := m.panel.cursor
				if m.panel.clickAt(msg.X, msg.Y) && m.panel.clickedRow && before != "" && m.panel.cursor == before {
					return m.jump()
				}
			}
		}
		return m, nil

	case paneNoteMsg:
		m.panel.note = msg.note
		return m, nil

	case observerStartMsg:
		m.starting = false
		if msg.note != "" {
			m.panel.note = msg.note
		}
		return m, nil

	case panelEventMsg, panelErrMsg, panelTickMsg, panelRefreshedMsg:
		cmd := m.panel.update(msg)
		// A lit sidebar checks it still holds the focus whenever it draws
		// anyway: tmux's focus-out does not always arrive, and a sidebar
		// left lit after you moved on would offer keys that go elsewhere.
		if !m.panel.blurred {
			cmd = tea.Batch(cmd, m.checkFocus())
		}
		return m, cmd
	case paneFocusCheck:
		m.panel.blurred = !msg.focused
		return m, nil
	}
	return m, nil
}

func (m paneModel) View() string {
	if m.narrow() {
		return m.narrowView()
	}
	return m.panel.view()
}
