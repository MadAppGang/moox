package main

// panel_render.go is `tt panel-render` (architecture §7.3): the session panel
// drawn once, to stdout, as ANSI, from a saved `tt sessions --json` snapshot or
// from the running `tt observe` — with no terminal, no tmux and no TUI.
//
// It exists for three readers:
//
//  1. validation, which renders the user's REAL sessions without making
//     `go test` read ~/.claude;
//  2. a panel.conf author, who wants to see an edit before a sidebar picks it
//     up (`--config`), and to lint it (`--check`);
//  3. the screenshot pipeline, which needs the same bytes every time.
//
// Determinism is the whole design. Nothing here asks the terminal anything:
// resolveTheme is never called (its OSC 11 query would write to the tty that
// stdout may be), the light/dark decision and the colour profile are FORCED
// from flags, and the clock is the snapshot's own instant unless --now says
// otherwise. The same snapshot, flags and panel.conf give the same bytes.
//
// It is read-only. A snapshot file is read; the live path dials the observer's
// socket and asks for one enriched snapshot, exactly as a sidebar does, and
// starts nothing when no server answers.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockclient"
	"github.com/MadAppGang/moox/internal/observer/wire"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

const panelRenderUsage = `usage: tt panel-render [flags]

Draws the Claude session panel once, to stdout, as ANSI — the rows a sidebar
would show, from a saved snapshot or from the running observer. No terminal is
queried and no tmux is involved: the theme and the colour profile are forced,
so the same input gives the same bytes. Read-only.

Input (one of):
  --snapshot=FILE     the JSON of "tt sessions --json --all"; "-" reads stdin
  --live              ask the running "tt observe" for one enriched snapshot,
                      the way a sidebar does. Nothing is started when none runs
  --socket-dir=DIR    with --live: where the observer socket lives
                      (default $XDG_STATE_HOME/tt, else ~/.local/state/tt)

Drawing:
  --width=N           columns (default 46, the sidebar's)
  --height=N          lines; the panel gives way to fit, as it does on screen
                      (default 59; 0 draws every line)
  --mode=MODE         project (default), status, tmux or colour
  --surface=SURFACE   pane (default: the sidebar, with its footer line) or tui
  --theme=THEME       dark or light (default $TERM_THEME, else dark)
  --profile=PROFILE   truecolor (default), 256, 16 or ascii (no colour)
  --now=TIME          the instant ages are drawn against, RFC 3339
                      (default the snapshot's observedAt; now with --live)
  --ended             accepted and ignored: ended sessions are not drawn
  --config=PATH       draw with this panel.conf instead of
                      $XDG_CONFIG_HOME/tt/panel.conf (else ~/.config/tt/panel.conf);
                      a PATH that cannot be read is an error, not the default

Checking panel.conf:
  --check             lint panel.conf (or --config's file) and draw nothing:
                      every problem as path:line:col, then a verdict line.
                      The input and drawing flags are accepted and ignored

Exit codes: 0 drawn (with --check: the file is clean, absent, or has only soft
problems — it is used, and each one draws in place as {token?}), 1 nothing
could be read — or --check found a fatal problem, so the panel is drawing the
built-in format — 2 a usage error.
`

// Defaults and bounds of the drawing flags. The width and height are the
// sidebar's: 46 columns (panelWidth) and the 59 lines of a typical window.
const (
	panelRenderDefaultHeight = 59
	panelRenderMaxCols       = 1000
	panelRenderMaxLines      = 10000
	// panelRenderSnapshotLimit bounds a snapshot read. The machine's real
	// snapshot is a few hundred KB; anything near this is not one.
	panelRenderSnapshotLimit = 64 << 20
	// panelRenderLiveTimeout bounds the whole live read — dial, handshake and
	// the enriched snapshot — through a context deadline, not a timer.
	panelRenderLiveTimeout = 5 * time.Second
)

// panelRenderStdin is where --snapshot=- reads. A seam, like mcpStdin: a test
// substitutes a reader instead of driving the process's real stdin.
var panelRenderStdin io.Reader = os.Stdin

// panelRenderFlags is what one invocation asked for.
type panelRenderFlags struct {
	snapshot  string
	live      bool
	socketDir string

	width, height int
	mode          panelMode
	surface       panelSurface
	dark          bool
	profile       termenv.Profile
	now           time.Time // zero: the input's own instant
	ended         bool

	config string // "" is the usual panel.conf location
	check  bool
}

// runPanelRender is `tt panel-render`.
func runPanelRender(args []string, stdout, stderr io.Writer) int {
	f, code, ok := parsePanelRenderFlags(args, stdout, stderr)
	if !ok {
		return code
	}

	// --config replaces where panel.conf is looked for, for this process. The
	// seam is put back so that a test calling this function leaves the
	// package as it found it.
	if f.config != "" {
		saved := panelConfPath
		defer func() { panelConfPath = saved }()
		path := f.config
		panelConfPath = func() string { return path }
	}

	if f.check {
		return runPanelConfCheck(panelConfPath(), f.config != "", stdout)
	}
	// A file named on the command line is one the reader wants to SEE: when
	// it cannot be read, drawing the built-in format instead would pass for
	// "my edit did nothing". Only the implicit location may be absent.
	if f.config != "" {
		if _, err := readPanelConf(f.config); err != nil {
			fmt.Fprintf(stderr, "tt panel-render: --config %s: %v\n", f.config, panelConfReadError(err))
			return jsonout.ExitNoRead
		}
	}

	snap, note, now, err := panelRenderInput(f)
	if err != nil {
		fmt.Fprintf(stderr, "tt panel-render: %v\n", err)
		return jsonout.ExitNoRead
	}
	if !f.now.IsZero() {
		now = f.now
	}
	if now.IsZero() {
		fmt.Fprintln(stderr, "tt panel-render: the snapshot has no observedAt, so its ages cannot be drawn; pass --now")
		return jsonout.ExitNoRead
	}

	forcePanelTheme(f.dark, f.profile)
	out := strings.Join(renderPanelLines(snap, f, now, note), "\n") + "\n"
	if _, err := io.WriteString(stdout, out); err != nil {
		fmt.Fprintf(stderr, "tt panel-render: %v\n", err)
		return jsonout.ExitNoRead
	}
	return jsonout.ExitOK
}

// forcePanelTheme pins lipgloss's two render-time decisions. It only ever
// SETS them: reading lipgloss.HasDarkBackground first, to put it back later,
// would itself send the OSC 11 query this command exists to avoid.
func forcePanelTheme(dark bool, profile termenv.Profile) {
	lipgloss.SetColorProfile(profile)
	lipgloss.SetHasDarkBackground(dark)
}

// renderPanelLines builds the panel a sidebar would build over snap and
// returns its lines: construction, then followConfig (R6: the first and only
// frame uses panel.conf), then lines(). It never starts the panel — no
// subscription, no tick, no goroutine.
func renderPanelLines(snap *observer.Snapshot, f panelRenderFlags, now time.Time, note string) []string {
	p := newPanel(nil, nil)
	p.followConfig()
	p.open = true
	p.width, p.height = f.width, f.height
	p.mode, p.surface = f.mode, f.surface
	p.snap, p.now = snap, now
	if f.surface == surfacePane {
		// The sidebar always carries one footer line, and a layout at a given
		// height depends on it, so the pane surface gets one too.
		p.note = note
	}
	return p.lines()
}

// panelSnapshotNote is the note a saved snapshot is drawn with ("snapshot of
// 2026-09-25 15:02 UTC"). Like "via tt observe (pid N)" it names the source,
// and the default view does not draw it (panelSourceNote).
const panelSnapshotNote = "snapshot"

// panelRenderInput reads the snapshot the flags name. It returns the
// snapshot, the footer a sidebar would show for that source, and the instant
// to draw at when --now is not given.
func panelRenderInput(f panelRenderFlags) (*observer.Snapshot, string, time.Time, error) {
	if f.live {
		ctx, cancel := context.WithTimeout(context.Background(), panelRenderLiveTimeout)
		defer cancel()
		snap, note, err := readLiveSnapshot(ctx, f.socketDir)
		if err != nil {
			return nil, "", time.Time{}, err
		}
		return snap, note, observer.SystemClock{}.Now(), nil
	}
	snap, err := readSnapshotFile(f.snapshot)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	note := panelSnapshotNote
	if !snap.ObservedAt.IsZero() {
		note += " of " + snap.ObservedAt.UTC().Format("2006-01-02 15:04 MST")
	}
	return snap, note, snap.ObservedAt, nil
}

// readSnapshotFile decodes a `tt sessions --json` document through the wire
// package, the one decoder of the domain.
func readSnapshotFile(name string) (*observer.Snapshot, error) {
	var (
		data []byte
		err  error
	)
	if name == "-" {
		data, err = io.ReadAll(io.LimitReader(panelRenderStdin, panelRenderSnapshotLimit+1))
	} else {
		var fh *os.File
		fh, err = os.Open(name)
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(fh, panelRenderSnapshotLimit+1))
			_ = fh.Close()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reading the snapshot: %w", err)
	}
	if len(data) > panelRenderSnapshotLimit {
		return nil, fmt.Errorf("the snapshot is larger than %d MiB; it is not a tt sessions --json document", panelRenderSnapshotLimit>>20)
	}
	ws, _, err := wire.DecodeSnapshot(data)
	if err != nil {
		return nil, err
	}
	// Schema 0 is what any JSON that is not a tt snapshot decodes to; a
	// different number is a build this one cannot speak for.
	if ws.Schema != wire.Schema {
		return nil, fmt.Errorf("the snapshot has schema %d, and this build reads schema %d (make it with: tt sessions --json --all)", ws.Schema, wire.Schema)
	}
	snap, _ := wire.ToDomain(ws)
	return snap, nil
}

// readLiveSnapshot asks the running observer for one enriched snapshot, dead
// sessions included — what a sidebar subscribes to. It dials and reads; it
// never starts a server.
func readLiveSnapshot(ctx context.Context, dir string) (*observer.Snapshot, string, error) {
	c, err := sockclient.Dial(ctx, sockclient.Config{Dir: dir, Policy: panelPolicy()})
	if err != nil {
		if errors.Is(err, sockclient.ErrNoServer) {
			return nil, "", fmt.Errorf("no tt observe is running at %s; start one, or render a saved snapshot: tt sessions --json --all > snap.json",
				sockclient.SocketPath(sockclient.Config{Dir: dir}))
		}
		return nil, "", err
	}
	defer func() { _ = c.Close() }()

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeTranscripts = true
	opts.IncludeDead = true
	snap, err := c.Snapshot(ctx, opts)
	if err != nil {
		return nil, "", err
	}
	return snap, fmt.Sprintf("%s (pid %d)", noteViaObserver, c.ServerPID()), nil
}

// runPanelConfCheck is --check: every problem in the file at path, one per
// line as path:line:col, then one verdict line. explicit says the path was
// named on the command line, where a missing file is a mistake rather than
// "no customisation".
func runPanelConfCheck(path string, explicit bool, stdout io.Writer) int {
	if path == "" {
		fmt.Fprintln(stdout, "panel.conf: no location (no home directory and no $XDG_CONFIG_HOME); the built-in format is in use")
		return jsonout.ExitOK
	}
	src, err := readPanelConf(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !explicit:
		fmt.Fprintf(stdout, "%s: no such file; the built-in format is in use\n", path)
		return jsonout.ExitOK
	case err != nil:
		fmt.Fprintf(stdout, "%s: %v\n", path, err)
		return jsonout.ExitNoRead
	}

	set := panelfmt.ParseConfig(src, panelRegistry, panelFormatOptions(), panelDefaultSet)
	fatal := 0
	for _, pr := range set.Problems {
		line := pr.Format(path)
		if pr.Fatal {
			fatal++
			line += " (fatal)"
		}
		fmt.Fprintln(stdout, line)
	}
	switch n := len(set.Problems); {
	case n == 0:
		// Not "path: ok": a path followed by a colon is how a problem line
		// starts, and an editor's error list would offer to jump to it.
		fmt.Fprintf(stdout, "%s is clean\n", path)
		return jsonout.ExitOK
	case fatal > 0:
		fmt.Fprintf(stdout, "%s: %s, %d fatal; the panel is drawing the built-in format\n", path, problemCount(n), fatal)
		return jsonout.ExitNoRead
	default:
		// Soft problems are printed but do not fail the lint: the file is
		// in use, and each one draws in place as {token?}.
		fmt.Fprintf(stdout, "%s: %s; the file is in use, and each draws in place as {token?}\n", path, problemCount(n))
		return jsonout.ExitOK
	}
}

// panelConfReadError is a read failure in words: the path is already said, so
// a *PathError's own copy of it is dropped.
func panelConfReadError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func problemCount(n int) string {
	if n == 1 {
		return "1 problem"
	}
	return fmt.Sprintf("%d problems", n)
}

// parsePanelRenderFlags turns argv into panelRenderFlags, printing the usage
// once, on the stream that fits (see parseSessionsFlags).
func parsePanelRenderFlags(args []string, stdout, stderr io.Writer) (panelRenderFlags, int, bool) {
	fs := flag.NewFlagSet("tt panel-render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}

	defaultTheme := "dark"
	if dark, ok := themeFromEnv(os.Getenv("TERM_THEME")); ok && !dark {
		defaultTheme = "light"
	}
	var (
		snapshot  = fs.String("snapshot", "", "a tt sessions --json file, or - for stdin")
		live      = fs.Bool("live", false, "read one snapshot from the running tt observe")
		socketDir = fs.String("socket-dir", "", "with --live: where the observer socket lives")
		width     = fs.Int("width", panelWidth, "columns")
		height    = fs.Int("height", panelRenderDefaultHeight, "lines (0 = every line)")
		mode      = fs.String("mode", defaultPanelMode.String(), "project|status|tmux|colour")
		surface   = fs.String("surface", "pane", "pane|tui")
		theme     = fs.String("theme", defaultTheme, "dark|light")
		profile   = fs.String("profile", "truecolor", "truecolor|256|16|ascii")
		now       = fs.String("now", "", "RFC 3339 instant to draw at")
		ended     = fs.Bool("ended", false, "ignored (ended sessions are not drawn)")
		config    = fs.String("config", "", "a panel.conf to draw with")
		check     = fs.Bool("check", false, "lint panel.conf and draw nothing")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, panelRenderUsage)
			return panelRenderFlags{}, jsonout.ExitOK, false
		}
		fmt.Fprint(stderr, "\n"+panelRenderUsage)
		return panelRenderFlags{}, jsonout.ExitUsage, false
	}
	usageErr := func(format string, a ...any) (panelRenderFlags, int, bool) {
		fmt.Fprintf(stderr, "tt panel-render: "+format+"\n\n%s", append(a, panelRenderUsage)...)
		return panelRenderFlags{}, jsonout.ExitUsage, false
	}
	if n := fs.NArg(); n > 0 {
		return usageErr("unexpected argument %q", fs.Arg(0))
	}

	f := panelRenderFlags{
		snapshot: *snapshot, live: *live, socketDir: strings.TrimSpace(*socketDir),
		width: *width, height: *height, ended: *ended,
		config: strings.TrimSpace(*config), check: *check,
	}

	if f.check {
		// --check draws nothing, so the input and drawing flags describe
		// something that will not happen. They are accepted and ignored — not
		// refused — so that --check can be added to any panel-render command
		// line (arch §7.3). Their values are not validated either: nothing
		// reads them. An unknown flag is still a usage error, from fs.Parse.
		return f, jsonout.ExitOK, true
	}

	switch {
	case f.live && f.snapshot != "":
		return usageErr("--snapshot and --live are two sources; name one")
	case !f.live && f.snapshot == "":
		return usageErr("nothing to draw: give --snapshot=FILE (or - for stdin) or --live")
	case !f.live && f.socketDir != "":
		return usageErr("--socket-dir is only meaningful with --live")
	}
	if f.live && f.socketDir == "" {
		f.socketDir = ttStateDir()
	}
	if f.width < 1 || f.width > panelRenderMaxCols {
		return usageErr("--width must be 1..%d, got %d", panelRenderMaxCols, f.width)
	}
	if f.height < 0 || f.height > panelRenderMaxLines {
		return usageErr("--height must be 0..%d, got %d", panelRenderMaxLines, f.height)
	}

	var ok bool
	if f.mode, ok = parsePanelMode(*mode); !ok {
		return usageErr("--mode must be project, status, tmux or colour, got %q", *mode)
	}
	switch strings.ToLower(strings.TrimSpace(*surface)) {
	case "pane":
		f.surface = surfacePane
	case "tui":
		f.surface = surfaceTUI
	default:
		return usageErr("--surface must be pane or tui, got %q", *surface)
	}
	if f.dark, ok = themeFromEnv(*theme); !ok {
		return usageErr("--theme must be dark or light, got %q", *theme)
	}
	if f.profile, ok = parseRenderProfile(*profile); !ok {
		return usageErr("--profile must be truecolor, 256, 16 or ascii, got %q", *profile)
	}
	if *now != "" {
		t, err := time.Parse(time.RFC3339Nano, *now)
		if err != nil {
			return usageErr("--now must be an RFC 3339 instant (2026-09-25T17:02:00Z), got %q", *now)
		}
		f.now = t
	}
	return f, jsonout.ExitOK, true
}

// parseRenderProfile maps --profile to termenv's four profiles.
func parseRenderProfile(s string) (termenv.Profile, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "truecolor", "24bit":
		return termenv.TrueColor, true
	case "256":
		return termenv.ANSI256, true
	case "16":
		return termenv.ANSI, true
	case "ascii", "none":
		return termenv.Ascii, true
	}
	return termenv.Ascii, false
}
