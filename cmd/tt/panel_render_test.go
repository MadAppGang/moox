package main

// `tt panel-render` (architecture §7.3), driven through runSubcommand the way
// main() drives it.
//
// The snapshot is SYNTHETIC and built in code: observer sessions, encoded by
// wire.NewSnapshot into the exact JSON `tt sessions --json` prints, written to
// a temp file. Nothing is read from ~/.claude and nothing lands under testdata
// (the fixture-privacy walk would refuse a real one).
//
// The render assertions are STRUCTURAL on purpose — lines within --height,
// every line within --width display columns, the expected names, chips and
// badges present — rather than a byte golden: the panel's own goldens
// (panel_golden_test.go) own the exact look, and a restyle there must not
// have to be re-approved here. What this file pins is the harness: input,
// flags, determinism, forced theme and profile, panel.conf, --check, exits.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// renderAt is the synthetic snapshot's observedAt.
var renderAt = time.Date(2026, 9, 25, 15, 2, 0, 0, time.UTC)

// renderFixture is a small fleet covering what the harness must carry through
// the wire: both band statuses, a loud and a warm and a quiet ctx badge and an
// unknown one, a /color, a tmux pane, a background agent's needs text, and an
// ended session.
func renderFixture() *observer.Snapshot {
	live, dead := true, false
	at := func(min int) *time.Time { t := renderAt.Add(-time.Duration(min) * time.Minute); return &t }
	facts := func(colour string, ctx int64) *observer.TranscriptFacts {
		f := &observer.TranscriptFacts{AgentColor: colour, ColorFound: colour != "", Model: "claude-opus-5-5"}
		if ctx > 0 {
			f.ContextTokens = ptr(ctx)
		}
		return f
	}
	sess := func(id, name, cwd string, st observer.Status, ageMin int) observer.Session {
		return observer.Session{
			SessionID: id, Name: name, CWD: cwd, Kind: observer.KindInteractive,
			Status: st, Attention: st.IsAttention(), Live: &live,
			StatusSince: at(ageMin), StartedAt: at(ageMin + 180), ObservedAt: renderAt,
		}
	}

	blocked := sess("r0000001", "migrate-db", "/tt-render-fixture/alpha", observer.StatusBlocked, 41)
	blocked.Kind, blocked.Needs = observer.KindBackground, ptr("confirm the migration plan")
	blocked.Transcript = facts("purple", 612_000)

	waiting := sess("r0000002", "wire-ui", "/tt-render-fixture/alpha", observer.StatusWaiting, 13)
	waiting.WaitReason = observer.WaitReasonInputNeeded
	waiting.Tmux = &observer.TmuxRef{Session: "alpha", Pane: "%107"}
	waiting.Transcript = facts("blue", 248_000)

	busy := sess("r0000003", "chip-fit", "/tt-render-fixture/beta", observer.StatusBusy, 2)
	busy.Tmux = &observer.TmuxRef{Session: "beta", Pane: "%12"}
	busy.Transcript = facts("green", 194_000)

	loud := sess("r0000004", "badge-tone", "/tt-render-fixture/beta", observer.StatusIdle, 14)
	loud.Transcript = facts("", 431_000)

	unknown := sess("r0000005", "no-usage-yet", "/tt-render-fixture/gamma", observer.StatusIdle, 180)

	ended := sess("r0000006", "old-run", "/tt-render-fixture/gamma", observer.StatusDone, 600)
	ended.Live, ended.Attention = &dead, false

	return &observer.Snapshot{
		ObservedAt: renderAt,
		Counts:     observer.NewCounts(),
		Sessions:   []observer.Session{blocked, waiting, busy, loud, unknown, ended},
	}
}

// renderFixtureJSON is the fixture as `tt sessions --json` would print it.
func renderFixtureJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(wire.NewSnapshot(renderFixture(), renderAt, wire.Options{TT: "test"}))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeRenderFixture puts the fixture in a temp file and returns its path.
func writeRenderFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snap.json")
	must(t, os.WriteFile(path, renderFixtureJSON(t), 0o600))
	return path
}

// panelRender runs the command. The theme state it forces is restored
// afterwards (withPanelTheme), so no other test inherits it.
func panelRender(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	withPanelTheme(t, true)
	var out, errOut bytes.Buffer
	code, handled := runSubcommand("panel-render", args, &out, &errOut)
	if !handled {
		t.Fatal("panel-render is not a known command")
	}
	return code, out.String(), errOut.String()
}

// renderedLines splits the command's stdout into the panel's lines.
func renderedLines(t *testing.T, stdout string) []string {
	t.Helper()
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("the output does not end in a newline: %q", stdout)
	}
	return strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
}

func TestPanelRenderUsageIsPrintedExactlyOnce(t *testing.T) {
	assertUsageOnce(t, "panel-render", panelRenderUsage)
}

func TestTheTopLevelUsageListsPanelRender(t *testing.T) {
	if !strings.Contains(usage, "\n  panel-render  ") {
		t.Fatalf("the command list does not name panel-render:\n%s", usage)
	}
}

// The render path must never reach the terminal: resolveTheme's OSC 11 query
// would land in front of the ANSI a caller is capturing (the same rule
// cli_test.go holds cli.go to), and nothing here may start the TUI.
func TestPanelRenderNeverTouchesTheTerminalOrTheTUI(t *testing.T) {
	src := printedSource(t, "panel_render.go")
	for _, banned := range []string{"resolveTheme", "hasDarkBackground", "lipgloss.HasDarkBackground", "termenv.NewOutput", "newModel", "tea.", ".start()", "run-shell"} {
		if strings.Contains(src, banned) {
			t.Fatalf("panel_render.go references %q", banned)
		}
	}
}

func TestPanelRenderDrawsTheSnapshotInsideItsBox(t *testing.T) {
	snap := writeRenderFixture(t)
	for _, theme := range []string{"dark", "light"} {
		for _, mode := range []string{"project", "status", "tmux", "colour"} {
			for _, box := range []struct{ w, h int }{{46, 59}, {46, 12}, {30, 20}, {80, 8}} {
				name := theme + "/" + mode + "/" + strconv.Itoa(box.w) + "x" + strconv.Itoa(box.h)
				t.Run(name, func(t *testing.T) {
					code, stdout, stderr := panelRender(t, "--snapshot="+snap, "--theme="+theme, "--mode="+mode,
						"--width="+strconv.Itoa(box.w), "--height="+strconv.Itoa(box.h))
					if code != 0 || stderr != "" {
						t.Fatalf("exit %d, stderr:\n%s", code, stderr)
					}
					lines := renderedLines(t, stdout)
					if len(lines) > box.h {
						t.Fatalf("%d lines, --height is %d", len(lines), box.h)
					}
					for i, l := range lines {
						if w := ansi.StringWidth(l); w > box.w {
							t.Fatalf("line %d is %d columns, --width is %d: %q", i+1, w, box.w, ansi.Strip(l))
						}
					}
					if !strings.Contains(stdout, "\x1b[") || !strings.Contains(stdout, ";2;") {
						t.Fatal("no truecolor SGR in the output: the profile was not forced")
					}
					if !strings.HasPrefix(strings.TrimSpace(ansi.Strip(lines[0])), "[P]") {
						t.Fatalf("the first line is not the tabs: %q", ansi.Strip(lines[0]))
					}
				})
			}
		}
	}
}

// At the sidebar's size every session of the fixture is drawn, with its chip
// and its badge: the facts crossed the wire and the default templates drew
// them.
func TestPanelRenderCarriesEveryFactThroughTheWire(t *testing.T) {
	code, stdout, stderr := panelRender(t, "--snapshot="+writeRenderFixture(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	plain := ansi.Strip(stdout)
	for _, want := range []string{
		"[P]roject", "NEEDS YOU", // the header's tabs, the band's title
		"migrate-db", "wire-ui", "chip-fit", "badge-tone", "no-usage-yet",
		" BLK ", " ASK ", // the blocked and waiting status codes
		"612k", "248k", "194k", "431k", // every ctx tone
		"—",                         // the unknown badge
		"waiting 41m · confirm the", // the band's why, wrapped under it
		" f  only ",                 // the key strip
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("the drawing lacks %q:\n%s", want, plain)
		}
	}
	// The snapshot's date names the source, and the default view no longer
	// draws the source (FR9).
	if strings.Contains(plain, "snapshot of") {
		t.Errorf("the source note is drawn:\n%s", plain)
	}
	if strings.Contains(plain, "old-run") {
		t.Errorf("an ended session is drawn:\n%s", plain)
	}
}

func TestPanelRenderIsDeterministicAndReadsStdin(t *testing.T) {
	path := writeRenderFixture(t)
	_, first, _ := panelRender(t, "--snapshot="+path)
	_, second, _ := panelRender(t, "--snapshot="+path)
	if first != second {
		t.Fatal("two renders of one snapshot differ")
	}

	stubSeam(t, &panelRenderStdin, io.Reader(bytes.NewReader(renderFixtureJSON(t))))
	code, fromStdin, stderr := panelRender(t, "--snapshot=-")
	if code != 0 {
		t.Fatalf("--snapshot=- exited %d: %s", code, stderr)
	}
	if fromStdin != first {
		t.Fatal("the stdin render differs from the file render of the same snapshot")
	}
}

// --theme forces lipgloss's light/dark decision, and --profile its colour
// profile: the palette's own painted values appear, and only the chosen ones.
func TestPanelRenderForcesTheThemeAndTheProfile(t *testing.T) {
	path := writeRenderFixture(t)
	_, dark, _ := panelRender(t, "--snapshot="+path, "--theme=dark")
	_, light, _ := panelRender(t, "--snapshot="+path, "--theme=light")
	darkCloud, lightCloud := sgrRGB(t, 38, panelPal.cloud.Dark), sgrRGB(t, 38, panelPal.cloud.Light)
	if !strings.Contains(dark, darkCloud) || strings.Contains(dark, lightCloud) {
		t.Fatalf("--theme=dark does not paint the dark cloud (%s) alone", darkCloud)
	}
	if !strings.Contains(light, lightCloud) || strings.Contains(light, darkCloud) {
		t.Fatalf("--theme=light does not paint the light cloud (%s) alone", lightCloud)
	}

	t.Setenv("TERM_THEME", "light")
	if _, byEnv, _ := panelRender(t, "--snapshot="+path); byEnv != light {
		t.Fatal("TERM_THEME=light is not the default theme")
	}
	t.Setenv("TERM_THEME", "")

	_, c256, _ := panelRender(t, "--snapshot="+path, "--profile=256")
	if strings.Contains(c256, "38;2;") || !strings.Contains(c256, "38;5;") {
		t.Fatal("--profile=256 did not draw in the 256-colour palette")
	}
	_, plain, _ := panelRender(t, "--snapshot="+path, "--profile=ascii")
	if strings.Contains(plain, "\x1b") {
		t.Fatal("--profile=ascii drew an escape sequence")
	}
	// Colour changes the paint, never the geometry.
	if plain != ansi.Strip(dark) {
		t.Fatalf("the ascii drawing is not the truecolor drawing without its colour:\n%s\n---\n%s", plain, ansi.Strip(dark))
	}
}

func TestPanelRenderFlagsShapeTheDrawing(t *testing.T) {
	path := writeRenderFixture(t)
	rowOf := func(t *testing.T, stdout, name string) string {
		t.Helper()
		for _, l := range strings.Split(ansi.Strip(stdout), "\n") {
			if strings.Contains(l, name) {
				return l
			}
		}
		t.Fatalf("no line names %q", name)
		return ""
	}

	t.Run("--now moves every age", func(t *testing.T) {
		_, at, _ := panelRender(t, "--snapshot="+path)
		_, later, _ := panelRender(t, "--snapshot="+path, "--now=2026-09-25T17:02:00Z")
		// The badge's runtime and the why line's wait both move.
		if row := rowOf(t, at, "wire-ui"); !strings.Contains(row, " 3h13m ") {
			t.Fatalf("at observedAt the runtime is not 3h13m: %q", row)
		}
		if row := rowOf(t, later, "wire-ui"); !strings.Contains(row, " 5h13m ") {
			t.Fatalf("two hours later the runtime is not 5h13m: %q", row)
		}
		if why := rowOf(t, at, "waiting 13m · input-needed"); why == "" {
			t.Fatal("at observedAt the wait is not 13m")
		}
		if why := rowOf(t, later, "waiting 2h · input-needed"); why == "" {
			t.Fatal("two hours later the wait is not 2h")
		}
	})

	t.Run("--ended is accepted and draws no ended session", func(t *testing.T) {
		code, stdout, stderr := panelRender(t, "--snapshot="+path, "--ended", "--height=0")
		if code != 0 || strings.Contains(ansi.Strip(stdout), "old-run") {
			t.Fatalf("--ended: exit %d, stderr %q, or the ended old-run is drawn:\n%s", code, stderr, ansi.Strip(stdout))
		}
	})

	t.Run("--surface=tui names its own keys", func(t *testing.T) {
		_, pane, _ := panelRender(t, "--snapshot="+path, "--height=0")
		_, tui, _ := panelRender(t, "--snapshot="+path, "--height=0", "--surface=tui")
		// The sidebar's tab hotkeys and plain-letter keys; the TUI's chords,
		// and no tab hotkey (its letters are search characters).
		if p := ansi.Strip(pane); !strings.Contains(p, "[P]roject") || !strings.Contains(p, " f  only ") {
			t.Fatalf("the pane surface does not name its keys:\n%s", p)
		}
		if u := ansi.Strip(tui); strings.Contains(u, "[P]") || !strings.Contains(u, " ^g  group ") || strings.Contains(u, " r ") {
			t.Fatalf("the TUI surface names a key it does not bind:\n%s", u)
		}
	})

	t.Run("--height=0 draws every line", func(t *testing.T) {
		_, all, _ := panelRender(t, "--snapshot="+path, "--height=0", "--ended")
		_, cut, _ := panelRender(t, "--snapshot="+path, "--height=6", "--ended")
		if n, m := len(renderedLines(t, all)), len(renderedLines(t, cut)); n <= m || m > 6 {
			t.Fatalf("unbounded %d lines, bounded %d (want > and ≤ 6)", n, m)
		}
	})
}

// --config draws with the named file from the first (and only) frame (R6), a
// broken file falls back with the panel's own note, and the seam is put back.
func TestPanelRenderDrawsWithTheConfigItIsGiven(t *testing.T) {
	path := writeRenderFixture(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "panel.conf")
	must(t, os.WriteFile(conf, []byte(
		"row      = \"{name} ~~{>}{ctx:plain}\"\n"+
			"attn_row = \"{name} !!{>}{status:word}\"\n"+
			"meta     = \"\"\n"), 0o600))

	code, stdout, stderr := panelRender(t, "--snapshot="+path, "--config="+conf)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	plain := ansi.Strip(stdout)
	for _, want := range []string{"wire-ui !!", "chip-fit ~~"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("the drawing did not use --config (%q missing):\n%s", want, plain)
		}
	}
	if got := panelConfPath(); got != "" {
		t.Fatalf("panel-render left the panel.conf seam pointing at %q", got)
	}

	// The note is the panel's own footer line, cut at the panel's width like
	// every other line; the rows are the built-in ones.
	must(t, os.WriteFile(conf, []byte("row = \"{name\"\n"), 0o600))
	_, broken, _ := panelRender(t, "--snapshot="+path, "--config="+conf)
	if b := ansi.Strip(broken); !strings.Contains(b, "panel.conf:1:8: unclosed") || !strings.Contains(b, " ASK   3h13m    wire-ui ") {
		t.Fatalf("a broken file did not fall back to the built-in rows with its note:\n%s", b)
	}
}

// --config names a file the reader wants to see drawn: a missing or unreadable
// one is an error (exit 1, the path and the reason on stderr, nothing on
// stdout), never a silent drawing in the built-in format (review round 1,
// Issue 4). The implicit location may still be absent: that is "no
// customisation", and draws.
func TestPanelRenderRefusesAConfigItCannotRead(t *testing.T) {
	path := writeRenderFixture(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "panle.conf")
	for _, c := range []struct {
		name, conf, reason string
	}{
		{"a mistyped path", missing, "no such file"},
		{"a directory", dir, "is a directory"},
	} {
		code, stdout, stderr := panelRender(t, "--snapshot="+path, "--config="+c.conf)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", c.name, code)
		}
		if stdout != "" {
			t.Errorf("%s: drew a panel anyway:\n%s", c.name, ansi.Strip(stdout))
		}
		if want := "tt panel-render: --config " + c.conf + ": " + c.reason; !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr %q lacks %q", c.name, stderr, want)
		}
	}

	// The implicit location, absent: the built-in format, exit 0.
	saved := panelConfPath
	t.Cleanup(func() { panelConfPath = saved })
	panelConfPath = func() string { return missing }
	if code, stdout, stderr := panelRender(t, "--snapshot="+path); code != 0 || !strings.Contains(ansi.Strip(stdout), "wire-ui") {
		t.Errorf("an absent implicit panel.conf: exit %d, stderr %q", code, stderr)
	}
}

func TestPanelRenderCheckLintsPanelConf(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	clean := write("clean.conf", "meta = \"    {pane}\"\nctx_warn = 150k\n")
	soft := write("soft.conf", "meta = \"    {pane} · {foo}\"\n")
	fatal := write("fatal.conf", "# a comment\nrow = \"{name\"\nmeta = \"{bar}\"\n")

	for _, tc := range []struct {
		name  string
		args  []string
		code  int
		wants []*regexp.Regexp
	}{
		{"a clean file", []string{"--config=" + clean}, 0,
			[]*regexp.Regexp{regexp.MustCompile("^" + regexp.QuoteMeta(clean) + " is clean\n$")}},
		// Soft problems are printed, but the file is in use: not a failure.
		{"a soft problem", []string{"--config=" + soft}, 0, []*regexp.Regexp{
			regexp.MustCompile("(?m)^" + regexp.QuoteMeta(soft) + `:1:\d+: unknown token \{foo\}$`),
			regexp.MustCompile(`(?m)1 problem; the file is in use`),
		}},
		{"a fatal problem", []string{"--config=" + fatal}, 1, []*regexp.Regexp{
			regexp.MustCompile("(?m)^" + regexp.QuoteMeta(fatal) + `:2:\d+: .* \(fatal\)$`),
			regexp.MustCompile(`(?m)fatal; the panel is drawing the built-in format$`),
		}},
		{"a named file that is missing", []string{"--config=" + filepath.Join(dir, "nope.conf")}, 1,
			[]*regexp.Regexp{regexp.MustCompile("no such file or directory")}},
		{"a directory", []string{"--config=" + dir}, 1,
			[]*regexp.Regexp{regexp.MustCompile("is a directory")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := panelRender(t, append([]string{"--check"}, tc.args...)...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			for _, re := range tc.wants {
				if !re.MatchString(stdout) {
					t.Fatalf("stdout does not match %s:\n%s", re, stdout)
				}
			}
		})
	}

	t.Run("the usual location, absent", func(t *testing.T) {
		missing := filepath.Join(dir, "tt", "panel.conf")
		stubSeam(t, &panelConfPath, func() string { return missing })
		code, stdout, _ := panelRender(t, "--check")
		if code != 0 || !strings.Contains(stdout, "no such file; the built-in format is in use") {
			t.Fatalf("exit %d:\n%s", code, stdout)
		}
	})

	t.Run("the shipped default", func(t *testing.T) {
		code, stdout, _ := panelRender(t, "--check", "--config="+write("default.conf", defaultPanelConf))
		if code != 0 {
			t.Fatalf("the built-in panel.conf, copied out of the README, does not lint clean:\n%s", stdout)
		}
	})
}

// --check accepts the input and drawing flags and ignores them (arch §7.3):
// the verdict is the file's alone, whatever else is on the command line —
// even values a drawing would refuse, since nothing reads them.
func TestPanelRenderCheckIgnoresDrawingFlags(t *testing.T) {
	path := writeRenderFixture(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	clean := write("clean.conf", "meta = \"    {pane}\"\n")
	soft := write("soft.conf", "meta = \"    {pane} · {foo}\"\n")
	fatal := write("fatal.conf", "row = \"{name\"\n")
	drawing := []string{
		"--snapshot=" + path, "--live", "--socket-dir=/nonexistent", "--width=0", "--height=-1",
		"--mode=folder", "--surface=window", "--theme=auto", "--profile=8", "--now=yesterday", "--ended",
	}
	for _, tc := range []struct {
		name, conf string
		code       int
	}{
		{"clean", clean, 0},
		{"soft only", soft, 0},
		{"fatal", fatal, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--check", "--config=" + tc.conf}, drawing...)
			code, stdout, stderr := panelRender(t, args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stdout:\n%s\nstderr:\n%s", code, tc.code, stdout, stderr)
			}
			if stderr != "" {
				t.Fatalf("--check wrote to stderr: %q", stderr)
			}
			if !strings.HasPrefix(stdout, tc.conf) {
				t.Fatalf("--check drew something other than its lint:\n%s", stdout)
			}
		})
	}
}

// --check ignores the drawing flags' values, but the command line is still
// parsed: a flag that does not exist, or one the flag package cannot parse,
// is a usage error (exit 2), not a lint verdict.
func TestPanelRenderCheckStillRefusesBadFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"an unknown flag", []string{"--check", "--frobnicate"}},
		{"a value the flag package cannot parse", []string{"--check", "--width=wide"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := panelRender(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (a usage error); stderr:\n%s", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("a usage error printed on stdout: %q", stdout)
			}
			if strings.Count(stderr, "usage: tt panel-render") != 1 {
				t.Fatalf("stderr does not carry the usage once:\n%s", stderr)
			}
		})
	}
}

func TestPanelRenderRefusesWhatItCannotDraw(t *testing.T) {
	path := writeRenderFixture(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no input", nil},
		{"two inputs", []string{"--snapshot=" + path, "--live"}},
		{"a socket dir without --live", []string{"--snapshot=" + path, "--socket-dir=/tmp"}},
		{"a zero width", []string{"--snapshot=" + path, "--width=0"}},
		{"a negative height", []string{"--snapshot=" + path, "--height=-1"}},
		{"an unknown mode", []string{"--snapshot=" + path, "--mode=folder"}},
		{"an unknown surface", []string{"--snapshot=" + path, "--surface=window"}},
		{"an unknown theme", []string{"--snapshot=" + path, "--theme=auto"}},
		{"an unknown profile", []string{"--snapshot=" + path, "--profile=8"}},
		{"a bad instant", []string{"--snapshot=" + path, "--now=yesterday"}},
		{"a positional argument", []string{"--snapshot=" + path, "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := panelRender(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (a usage error); stderr:\n%s", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("a usage error drew on stdout: %q", stdout)
			}
			if !strings.HasPrefix(stderr, "tt panel-render: ") || strings.Count(stderr, "usage: tt panel-render") != 1 {
				t.Fatalf("stderr does not say what was wrong, then the usage once:\n%s", stderr)
			}
		})
	}
}

func TestPanelRenderSaysWhyItHasNothingToDraw(t *testing.T) {
	dir := t.TempDir()
	notSnapshot := filepath.Join(dir, "other.json")
	must(t, os.WriteFile(notSnapshot, []byte(`{"sessions":[]}`), 0o600))
	garbage := filepath.Join(dir, "garbage.json")
	must(t, os.WriteFile(garbage, []byte("not json"), 0o600))

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a missing file", []string{"--snapshot=" + filepath.Join(dir, "nope.json")}, "no such file"},
		{"JSON that is not a snapshot", []string{"--snapshot=" + notSnapshot}, "schema 0"},
		{"not JSON", []string{"--snapshot=" + garbage}, "decode snapshot"},
		// An empty directory: nothing listens there, and nothing is started.
		{"--live with no observer", []string{"--live", "--socket-dir=" + dir}, "no tt observe is running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := panelRender(t, tc.args...)
			if code != 1 {
				t.Fatalf("exit %d, want 1; stderr:\n%s", code, stderr)
			}
			if stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("stdout %q, stderr %q (want it to say %q)", stdout, stderr, tc.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "observer.sock")); err == nil {
		t.Fatal("--live created a socket: it must only ever dial one")
	}
}

// readmeINIBlocks is every ```ini block of the README: the panel.conf texts it
// tells a reader to copy.
func readmeINIBlocks(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile("(?s)```ini\n(.*?)```")
	var out []string
	for _, m := range re.FindAllStringSubmatch(readFile(t, "README.md"), -1) {
		out = append(out, m[1])
	}
	return out
}

// The README says its default panel.conf is the built-in one byte for byte,
// and that is only true while a test holds them together.
func TestTheREADMEShowsTheBuiltInPanelConf(t *testing.T) {
	blocks := readmeINIBlocks(t)
	for _, b := range blocks {
		if b == defaultPanelConf {
			return
		}
	}
	t.Fatalf("no ```ini block of README.md is defaultPanelConf byte for byte (%d blocks)", len(blocks))
}

// Every panel.conf the README offers for copying lints clean: an example that
// draws {foo?} would teach the reader the wrong grammar.
func TestEveryPanelConfInTheREADMELintsClean(t *testing.T) {
	blocks := readmeINIBlocks(t)
	if len(blocks) < 2 {
		t.Fatalf("README.md has %d ```ini blocks; the default and an example were expected", len(blocks))
	}
	for i, b := range blocks {
		path := filepath.Join(t.TempDir(), "panel.conf")
		must(t, os.WriteFile(path, []byte(b), 0o600))
		if code, stdout, _ := panelRender(t, "--check", "--config="+path); code != 0 {
			t.Errorf("README ini block %d does not lint clean (exit %d):\n%s", i+1, code, stdout)
		}
	}
}
