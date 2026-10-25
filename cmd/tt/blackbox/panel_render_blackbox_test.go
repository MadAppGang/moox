// Black-box tests for `tt panel-render` (architecture §7.3), written from the
// spec and contract only. They drive the binary that TestMain in
// observer_blackbox_test.go builds from this checkout (checkoutBinary) and
// define nothing that file defines; every helper here is prefixed "pr".
//
// Hermetic by construction: every run gets HOME, XDG_* , TMUX_TMPDIR and
// TMPDIR inside a fresh /tmp/ttpr-* directory and a PATH that is an empty
// directory, so neither the user's ~/.claude, ~/.config nor any tmux can be
// reached. No tmux command is run. Snapshots are built in code from the wire
// field names the observer black-box tests document plus §4.2's transcript
// fields; no JSON key is named "message" (fixture privacy, §4.5).
package blackbox_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// prTextProfile is the colourless profile used for text assertions (caller's
// instruction). NOTE: §7.3 lists only truecolor|256|16; see TEST-PLAN.md gaps.
const prTextProfile = "ascii"

var prNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

type prObj = map[string]any

// ---------------------------------------------------------------- snapshot --

// prSession is one wire session as `tt sessions --json --all` emits it.
// Attention follows the status (waiting/blocked), as the observer computes it.
func prSession(name, status string, age time.Duration) prObj {
	sum := sha256.Sum256([]byte(name))
	id := fmt.Sprintf("%x", sum)[:8]
	pane := fmt.Sprintf("%%%d", 100+int(sum[0]))
	attention := status == "waiting" || status == "blocked"
	since := prNow.Add(-age)
	s := prObj{
		"sessionId": id + "-0000-4000-8000-000000000000", "shortId": id,
		"kind": "interactive", "status": status, "rawStatus": status,
		"waitReason": "", "needs": "", "attention": attention,
		"statusSince": since.Format(time.RFC3339Nano), "statusAgeMs": float64(age.Milliseconds()),
		"blockedForMs": nil, "observedAt": prNow.Format(time.RFC3339Nano),
		"name": name, "cwd": "/tmp/ttpr-projects/alpha",
		"pid": 4242, "tmux": prObj{"raw": "work:@1." + pane, "session": "work", "window": "@1", "pane": pane},
		"live": true, "procAlive": true, "paneAlive": true, "liveReason": "proc+pane",
		"advisories": []any{}, "transcriptCollected": true, "lastActivityAt": nil,
		"statusSource": "registry",
		"transcript": prObj{
			"path": "/tmp/ttpr-projects/none.jsonl", "agentColor": "", "colorFound": false,
			"apiErrors": 0, "subagents": nil, "subagentsKnown": false,
			"contextTokens": nil, "model": "", "compactions": 0,
		},
	}
	if attention {
		s["blockedForMs"] = s["statusAgeMs"]
	}
	if status == "waiting" {
		s["waitReason"] = "input-needed"
	}
	return s
}

// prJob is a background session (a job): no pid, no tmux.
func prJob(name, status string, age time.Duration) prObj {
	s := prSession(name, status, age)
	s["kind"] = "background"
	s["pid"], s["tmux"], s["procAlive"], s["paneAlive"] = nil, nil, nil, nil
	s["liveReason"] = "job-non-terminal-corroborated"
	s["statusSource"] = "jobs"
	return s
}

func prTranscript(s prObj) prObj { return s["transcript"].(prObj) }

func prCtx(s prObj, used any) prObj {
	prTranscript(s)["contextTokens"] = used
	return s
}

func prColour(s prObj, colour string) prObj {
	prTranscript(s)["agentColor"] = colour
	prTranscript(s)["colorFound"] = colour != ""
	return s
}

func prSnapshot(t *testing.T, ss ...prObj) []byte {
	t.Helper()
	attention, live, dead, unknown := 0, 0, 0, 0
	for _, s := range ss {
		if s["attention"] == true {
			attention++
		}
		switch s["live"] {
		case true:
			live++
		case false:
			dead++
		case nil:
			unknown++
		}
	}
	env := prObj{
		"schema":     1,
		"observedAt": prNow.Format(time.RFC3339Nano),
		"counts":     prObj{"attention": attention, "live": live, "dead": dead, "unknown": unknown},
		"warnings":   []any{},
		"sessions":   ss,
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"message"`)) {
		t.Fatal("fixture privacy: a snapshot key is named \"message\"")
	}
	return b
}

// ------------------------------------------------------------- environment --

type prEnv struct {
	t                     *testing.T
	root, home, cfg, snap string
	env                   []string
}

func newPREnv(t *testing.T) *prEnv {
	t.Helper()
	if _, err := os.Stat(checkoutBinary); err != nil {
		t.Fatalf("TestMain checkout binary unavailable: %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "ttpr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	e := &prEnv{t: t, root: root, home: filepath.Join(root, "home"), cfg: filepath.Join(root, "cfg"), snap: filepath.Join(root, "snap.json")}
	for _, d := range []string{e.home, e.cfg, filepath.Join(root, "state"), filepath.Join(root, "cache"), filepath.Join(root, "data"), filepath.Join(root, "run"), filepath.Join(root, "tmux"), filepath.Join(root, "tmp"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.env = []string{
		"HOME=" + e.home, "XDG_CONFIG_HOME=" + e.cfg,
		"XDG_STATE_HOME=" + filepath.Join(root, "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"TMUX_TMPDIR=" + filepath.Join(root, "tmux"), "TMPDIR=" + filepath.Join(root, "tmp"),
		"PATH=" + filepath.Join(root, "bin"), "TERM=dumb", "LANG=C", "LC_ALL=C",
	}
	return e
}

func (e *prEnv) write(path, content string) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// conf writes a panel.conf outside XDG_CONFIG_HOME, for --config.
func (e *prEnv) conf(content string) string {
	return e.write(filepath.Join(e.root, "c", "panel.conf"), content)
}

func (e *prEnv) run(stdin []byte, args ...string) (string, string, int) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, checkoutBinary, append([]string{"panel-render"}, args...)...)
	c.Env = e.env
	c.Dir = e.home
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var out, errout bytes.Buffer
	c.Stdout, c.Stderr = &out, &errout
	err := c.Run()
	if c.Process != nil {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		e.t.Fatalf("tt panel-render %v exceeded 20s: %s", args, errout.String())
	}
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errout.String(), code
}

// render writes the snapshot, runs panel-render with the fixed clock, project
// mode and dark theme plus extra flags, requires exit 0 and returns the lines.
func (e *prEnv) render(snap []byte, extra ...string) []string {
	e.t.Helper()
	e.write(e.snap, string(snap))
	args := append([]string{"--snapshot=" + e.snap, "--now=" + prNow.Format(time.RFC3339), "--mode=project", "--theme=dark"}, extra...)
	out, stderr, code := e.run(nil, args...)
	if code != 0 {
		e.t.Fatalf("tt panel-render %v exited %d\nstderr: %s\nstdout: %s", args, code, stderr, out)
	}
	return prLines(out)
}

func prLines(out string) []string {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func prPlain(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Strip(l)
	}
	return out
}

func prDump(lines []string) string { return strings.Join(prPlain(lines), "\n") }

// prHead is the index of the first line naming the session, or -1.
func prHead(plain []string, name string) int {
	for i, l := range plain {
		if strings.Contains(l, name) {
			return i
		}
	}
	return -1
}

func prMustHead(t *testing.T, plain []string, name string) int {
	t.Helper()
	i := prHead(plain, name)
	if i < 0 {
		t.Fatalf("session %q is not drawn:\n%s", name, strings.Join(plain, "\n"))
	}
	return i
}

// prRowLines is the head plus its continuation lines. Every line of a row
// starts with the 1-column gutter (the session colour's ▌, or a space); after
// it, a head has one space before the status code, a continuation (latest
// text, WHY, meta) at least four. Headings and blanks end the row.
func prRowLines(plain []string, head int) []string {
	rows := []string{plain[head]}
	for j := head + 1; j < len(plain); j++ {
		_, size := utf8.DecodeRuneInString(plain[j])
		if rest := plain[j][size:]; !strings.HasPrefix(rest, "    ") || strings.TrimSpace(rest) == "" {
			break
		}
		rows = append(rows, plain[j])
	}
	return rows
}

// prUngutter is the text of row lines without the gutter and the indent.
func prUngutter(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		_, size := utf8.DecodeRuneInString(l)
		out[i] = strings.TrimSpace(l[size:])
	}
	return out
}

// prChrome reports whether line i of a frame is chrome rather than a row: the
// view tabs, their baseline and the count strip above the first section, and
// the key strip at the bottom. The count strip's chips (NEED in the attention
// hue) and the active tab are filled by design, so the row rules skip them.
// A frame with no section heading has no header to exempt: only its last line
// is chrome then, so a broken frame fails the row rules instead of passing them
// with nothing checked.
func prChrome(plain []string, i int) bool {
	if i == len(plain)-1 {
		return true
	}
	for j := range plain {
		if strings.Contains(plain[j], "───  ") {
			return i < j
		}
	}
	return false
}

// ---------------------------------------------------------- colour reading --

type prRGB struct{ r, g, b int }

type prCell struct {
	ch     rune
	fg, bg *prRGB
	bold   bool
}

func prCells(s string) []prCell {
	var fg, bg *prRGB
	var bold bool
	var out []prCell
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j >= len(s) {
					break
				}
				if s[j] == 'm' {
					fg, bg, bold = prSGR(s[i+2:j], fg, bg, bold)
				}
				i = j + 1
				continue
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		out = append(out, prCell{ch: r, fg: fg, bg: bg, bold: bold})
		i += n
	}
	return out
}

func prSGR(params string, fg, bg *prRGB, bold bool) (*prRGB, *prRGB, bool) {
	if params == "" {
		return nil, nil, false
	}
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		n, _ := strconv.Atoi(ps[k])
		switch {
		case n == 0:
			fg, bg, bold = nil, nil, false
		case n == 1:
			bold = true
		case n == 22:
			bold = false
		case n == 39:
			fg = nil
		case n == 49:
			bg = nil
		case n == 38 || n == 48:
			if k+4 < len(ps) && ps[k+1] == "2" {
				r, _ := strconv.Atoi(ps[k+2])
				g, _ := strconv.Atoi(ps[k+3])
				b, _ := strconv.Atoi(ps[k+4])
				if n == 38 {
					fg = &prRGB{r, g, b}
				} else {
					bg = &prRGB{r, g, b}
				}
				k += 4
			} else if k+2 < len(ps) && ps[k+1] == "5" {
				k += 2
			}
		}
	}
	return fg, bg, bold
}

// Colour families (dark theme, §6.1): coral #FF4D5E, honey #FFB454 (the warm
// ctx tier), violet #A78BFA (ASK, chosen 2026-10-21), moss-ish
// greens. Solid fills are "saturated": bright and far from grey.
func prCoral(c *prRGB) bool  { return c != nil && c.r >= 200 && c.g < 120 && c.b < 140 }
func prHoney(c *prRGB) bool  { return c != nil && c.r >= 200 && c.g >= 120 && c.b < c.g }
func prViolet(c *prRGB) bool { return c != nil && c.b >= 200 && c.b > c.r && c.r > c.g }
func prGreen(c *prRGB) bool  { return c != nil && c.g > c.r && c.g > c.b }
func prSaturated(c *prRGB) bool {
	if c == nil {
		return false
	}
	hi, lo := max(c.r, c.g, c.b), min(c.r, c.g, c.b)
	return hi >= 180 && hi-lo >= 100
}

func prHasBg(line string, pred func(*prRGB) bool) bool {
	for _, c := range prCells(line) {
		if pred(c.bg) {
			return true
		}
	}
	return false
}

// prBadge is the ctx badge: the last run of non-space cells on a head line,
// with the colours of its first cell.
func prBadge(line string) (string, prCell) {
	cs := prCells(line)
	end := len(cs) - 1
	for end >= 0 && cs[end].ch == ' ' {
		end--
	}
	if end < 0 {
		return "", prCell{}
	}
	start := end
	for start > 0 && cs[start-1].ch != ' ' {
		start--
	}
	var b strings.Builder
	for _, c := range cs[start : end+1] {
		b.WriteRune(c.ch)
	}
	return b.String(), cs[start]
}

func prTone(c prCell) string {
	switch {
	case prCoral(c.bg):
		return "solid-red"
	case prCoral(c.fg) && c.bg != nil && !prSaturated(c.bg):
		return "soft-red"
	case prHoney(c.fg) && c.bg != nil && !prSaturated(c.bg):
		return "warm"
	case prGreen(c.fg) && c.bg != nil && !prSaturated(c.bg):
		return "quiet"
	case c.bg == nil:
		return "no-fill"
	}
	return fmt.Sprintf("unclassified(fg=%+v bg=%+v)", c.fg, c.bg)
}

// prBadgeSpan is the cell range of the ctx badge including its filled pads:
// the last non-space run widened over neighbours that share its background.
func prBadgeSpan(cs []prCell) (int, int) {
	end := len(cs) - 1
	for end >= 0 && cs[end].ch == ' ' && cs[end].bg == nil {
		end--
	}
	if end < 0 {
		return -1, -1
	}
	for end > 0 && cs[end].ch == ' ' {
		end-- // a filled trailing pad: step onto the text, widened again below
	}
	start := end
	for start > 0 && cs[start-1].ch != ' ' {
		start--
	}
	same := func(a, b *prRGB) bool { return a != nil && b != nil && *a == *b }
	for start > 0 && same(cs[start-1].bg, cs[start].bg) {
		start--
	}
	for end+1 < len(cs) && same(cs[end+1].bg, cs[end].bg) {
		end++
	}
	return start, end
}

// prFind is the cell index where the rune sequence s first starts, or -1.
func prFind(cs []prCell, s string) int {
	rs := []rune(s)
	for i := 0; i+len(rs) <= len(cs); i++ {
		ok := true
		for k, r := range rs {
			if cs[i+k].ch != r {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// prSlate is the dim grey family (madbench slate #7A7F9A dark): a present,
// unsaturated, mid-brightness foreground.
func prSlate(c *prRGB) bool {
	if c == nil || prSaturated(c) {
		return false
	}
	hi := max(c.r, c.g, c.b)
	return hi >= 90 && hi <= 190
}

// prNameCol is the display column where name starts on a plain line, or -1.
func prNameCol(line, name string) int {
	i := strings.Index(line, name)
	if i < 0 {
		return -1
	}
	return ansi.StringWidth(line[:i])
}

// prIcon is the status icon of an opt-in `{status:icon}` head: the runes after
// the 1-column gutter up to the first space.
func prIcon(head string) string {
	rs := []rune(head)
	j := 1
	for j < len(rs) && rs[j] != ' ' {
		j++
	}
	if j <= 1 {
		return ""
	}
	return string(rs[1:j])
}

// prCode is the status code of a default head (FR2 FINAL): the first word
// after the 1-column gutter.
func prCode(head string) string {
	rs := []rune(head)
	if len(rs) < 2 {
		return ""
	}
	f := strings.Fields(string(rs[1:]))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// Status codes of the default row (FR2 FINAL, status family A).
var prCodes = map[string]string{
	"busy": "RUN", "working": "WRK", "waiting": "ASK", "blocked": "BLK",
	"idle": "IDL", "shell": "SH", "unknown": "???", "done": "END", "failed": "ERR",
}

// Opt-in `{status:icon}` glyphs (FR2 of 2026-09-26, kept as the opt-in by FR2
// FINAL; busy/work = 🔄 by the orchestrator's amendment).
var prIcons = map[string]string{
	"busy": "🔄", "working": "🔄", "waiting": "⏳", "blocked": "⛔",
	"idle": "💤", "shell": "🐚", "unknown": "❓", "done": "✅",
}

// prIconConf opts into the emoji status slot for every head.
const prIconConf = "row = \"{gutter}{status:icon} {name}{>}{ctx}\"\n" +
	"attn_row = \"{gutter}{status:icon} {name}{>}{ctx}\"\n"

// prEmoji: a pictographic rune (the opt-in status glyphs and their blocks).
func prEmoji(r rune) bool {
	return (r >= 0x1F000 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF) ||
		(r >= 0x23E9 && r <= 0x23FA) || r == 0xFE0F || r == 0x200D
}

func prFirstEmoji(lines []string) (int, rune) {
	for i, l := range prPlain(lines) {
		for _, r := range l {
			if prEmoji(r) {
				return i, r
			}
		}
	}
	return -1, 0
}

// prRun is the cell range around cs[i] that shares its (non-nil) background:
// one badge, pads included.
func prRun(cs []prCell, i int) (int, int) {
	same := func(a, b *prRGB) bool { return a != nil && b != nil && *a == *b }
	s, e := i, i
	for s > 0 && same(cs[s-1].bg, cs[i].bg) {
		s--
	}
	for e+1 < len(cs) && same(cs[e+1].bg, cs[i].bg) {
		e++
	}
	return s, e
}

// prStarted sets the wire startedAt: the session start the RUNTIME badge
// counts from (FR2 FINAL). The key is not in the contract; see TEST-PLAN gaps.
func prStarted(s prObj, ago time.Duration) prObj {
	s["startedAt"] = prNow.Add(-ago).Format(time.RFC3339Nano)
	return s
}

// ------------------------------------------------------------------ fleets --

const prMockWhy = "confirm the migration plan before it touches the prod replica (step 4 of 6)"

// prBandFleet: two band rows (a blocked job, a waiting interactive) and quiet
// rows. Every runtime (since start) differs from the time in status.
func prBandFleet() []prObj {
	blk := prStarted(prColour(prCtx(prJob("db-migrate", "blocked", 41*time.Minute), 612_000), "purple"), 5*time.Hour+20*time.Minute)
	blk["needs"] = prMockWhy
	prTranscript(blk)["model"] = "claude-opus-5"
	wt := prStarted(prColour(prCtx(prSession("madbench-ws", "waiting", 13*time.Minute), 248_000), "blue"), 2*time.Hour+10*time.Minute)
	wt["waitReason"] = "permission-prompt"
	prTranscript(wt)["model"] = "claude-opus-5-5"
	idle := prStarted(prColour(prCtx(prSession("kcenv-keychain", "idle", 14*time.Minute), 88_000), "cyan"), 50*time.Hour)
	busy := prStarted(prCtx(prSession("tt-ui-rebuild", "busy", 2*time.Minute), 194_000), 35*time.Minute)
	return []prObj{blk, wt, idle, busy}
}

// prBandRuntime is a substring of each band-fleet row's runtime badge.
var prBandRuntime = map[string]string{"db-migrate": "5h", "madbench-ws": "2h", "kcenv-keychain": "2d", "tt-ui-rebuild": "35m"}

// ------------------------------------------------------------------- tests --

// REQ-18 (FR9): a synthetic snapshot renders, names every session,
// deterministically. The frame opens with the view tabs (no title line) and
// closes with the key strip.
func TestPanelRenderRendersASyntheticSnapshot(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t, prBandFleet()...)
	first := e.render(snap, "--width=46", "--height=59", "--profile="+prTextProfile)
	plain := prPlain(first)
	if len(plain) == 0 || !strings.Contains(plain[0], "roject") || !strings.Contains(plain[0], "atus") {
		t.Fatalf("the first line is not the view tabs:\n%s", prDump(first))
	}
	if foot := plain[len(plain)-1]; !strings.HasPrefix(foot, "═") || !strings.Contains(foot, "move") {
		t.Errorf("the last line %q is not the key strip", foot)
	}
	for _, name := range []string{"db-migrate", "madbench-ws", "kcenv-keychain", "tt-ui-rebuild"} {
		prMustHead(t, plain, name)
	}
	again := e.render(snap, "--width=46", "--height=59", "--profile="+prTextProfile)
	if strings.Join(again, "\n") != strings.Join(first, "\n") {
		t.Errorf("two renders of the same snapshot differ:\n%s\n---\n%s", prDump(first), prDump(again))
	}
}

// REQ-18: `--snapshot -` reads stdin.
func TestPanelRenderReadsTheSnapshotFromStdin(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t, prBandFleet()...)
	fromFile := e.render(snap, "--width=46", "--height=59", "--profile="+prTextProfile)
	out, stderr, code := e.run(snap, "--snapshot=-", "--now="+prNow.Format(time.RFC3339), "--mode=project", "--theme=dark", "--width=46", "--height=59", "--profile="+prTextProfile)
	if code != 0 {
		t.Fatalf("stdin render exited %d: %s", code, stderr)
	}
	if strings.Join(prLines(out), "\n") != strings.Join(fromFile, "\n") {
		t.Errorf("stdin render differs from the file render:\n%s\n---\n%s", out, prDump(fromFile))
	}
}

// REQ-19: every line fits --width in display columns and the frame fits
// --height, with wide, emoji and control-laden session text.
func TestPanelRenderFitsWidthAndHeight(t *testing.T) {
	e := newPREnv(t)
	fleet := prBandFleet()
	cjk := prCtx(prSession("名前のセッション長い名前のテスト", "idle", 3*time.Hour), 57_000)
	emoji := prCtx(prSession("🚀rocket-launch-🚀-👍🏽-🇺🇦-session", "busy", 5*time.Minute), 431_000)
	hostile := prSession("tab\tcr\rline\nesc\x1b[31mred\x1b]0;pwned\x07bel\u009bc1", "waiting", time.Minute)
	hostile["waitReason"] = "permission-prompt"
	long := prCtx(prSession(strings.Repeat("very-long-session-name-", 4), "idle", 26*time.Hour), 1_200_000)
	job := prJob("needs-ctl", "blocked", 9*time.Minute)
	job["needs"] = "line one\nline two\ttabbed \x1b[2Jclear " + strings.Repeat("word ", 30)
	fleet = append(fleet, cjk, emoji, hostile, long, job)
	snap := prSnapshot(t, fleet...)
	for _, profile := range []string{prTextProfile, "truecolor"} {
		for _, w := range []int{20, 30, 46, 80} {
			for _, h := range []int{8, 15, 59} {
				t.Run(fmt.Sprintf("%s/%dx%d", profile, w, h), func(t *testing.T) {
					lines := e.render(snap, fmt.Sprintf("--width=%d", w), fmt.Sprintf("--height=%d", h), "--profile="+profile)
					if len(lines) > h {
						t.Errorf("%d lines > --height %d:\n%s", len(lines), h, prDump(lines))
					}
					for i, l := range lines {
						if got := ansi.StringWidth(l); got > w {
							t.Errorf("line %d is %d columns > --width %d: %q", i, got, w, ansi.Strip(l))
						}
					}
					out := strings.Join(lines, "\n")
					for _, bad := range []string{"\x1b]", "\a", "\r", "\t", "\u009b"} {
						if strings.Contains(out, bad) {
							t.Errorf("control sequence %q from session text reached the output", bad)
						}
					}
					if profile == prTextProfile && strings.Contains(out, "\x1b") {
						t.Errorf("the %s profile emitted an escape sequence", prTextProfile)
					}
				})
			}
		}
	}
}

// REQ-20 (FR2 FINAL, FR3): attention rows lead in NEEDS YOU with their status
// code and are tall; the WHY line starts `waiting <time-in-status> · …`; the
// head carries the runtime (since start), not the time in status; no emoji.
func TestPanelRenderAttentionRowsAreLoudAndTall(t *testing.T) {
	e := newPREnv(t)
	lines := e.render(prSnapshot(t, prBandFleet()...), "--width=46", "--height=59", "--profile="+prTextProfile)
	plain := prPlain(lines)
	band := prHead(plain, "NEEDS YOU")
	if band < 0 {
		t.Fatalf("no NEEDS YOU heading:\n%s", prDump(lines))
	}
	blk, wt := prMustHead(t, plain, "db-migrate"), prMustHead(t, plain, "madbench-ws")
	idle, busy := prMustHead(t, plain, "kcenv-keychain"), prMustHead(t, plain, "tt-ui-rebuild")
	if !(band < blk && band < wt && blk < idle && blk < busy && wt < idle && wt < busy) {
		t.Errorf("band rows are not drawn first under NEEDS YOU (band %d, blk %d, wt %d, idle %d, busy %d):\n%s",
			band, blk, wt, idle, busy, prDump(lines))
	}

	rows := prRowLines(plain, blk)
	if h := rows[0]; prCode(h) != "BLK" || !strings.Contains(h, "5h") || !strings.HasSuffix(strings.TrimRight(h, " "), "612k") {
		t.Errorf("blocked head %q; want BLK, the runtime 5h… and the 612k badge last", h)
	}
	if strings.Contains(rows[0], "41m") {
		t.Errorf("blocked head %q shows the time in status 41m; the head carries the runtime, the WHY line the wait", rows[0])
	}
	// FR9: a band row is at most 4 lines: the head, up to 3 lines of WHY, and
	// the meta line naming its home group (the band lifts it out of that
	// group) only on a line the WHY leaves over. The long blocked WHY takes
	// all three.
	if len(rows) != 4 {
		t.Errorf("blocked band row has %d lines; want 4 (head, 3 lines of why):\n%s", len(rows), strings.Join(rows, "\n"))
	} else {
		why := strings.Join(prUngutter(rows[1:]), " ")
		if !strings.HasPrefix(why, "waiting 41m · confirm the migration plan") || !strings.HasSuffix(why, "(step 4 of 6)") {
			t.Errorf("blocked WHY %q; want all of `waiting 41m · confirm the migration plan … (step 4 of 6)`", why)
		}
	}

	// The short waiting WHY leaves a line for the group.
	rows = prRowLines(plain, wt)
	if h := rows[0]; prCode(h) != "ASK" || !strings.Contains(h, "2h") || strings.Contains(h, "13m") {
		t.Errorf("waiting head %q; want ASK and the runtime 2h…, not the wait 13m", h)
	}
	if len(rows) != 3 {
		t.Errorf("waiting band row has %d lines; want 3 (head, why, group):\n%s", len(rows), strings.Join(rows, "\n"))
	} else if m := prUngutter(rows[2:])[0]; m != "alpha" {
		t.Errorf("waiting band meta %q; want the home group alpha", m)
	}
	if len(rows) > 1 && !strings.HasPrefix(prUngutter(rows[1:2])[0], "waiting 13m · permission-prompt") {
		t.Errorf("the line under the waiting head is %q; want `waiting 13m · permission-prompt`", rows[1])
	}

	if h := plain[idle]; prCode(h) != "IDL" {
		t.Errorf("idle head %q; want IDL", h)
	}
	if h := plain[busy]; prCode(h) != "RUN" {
		t.Errorf("busy head %q; want RUN", h)
	}
	if i, r := prFirstEmoji(lines); i >= 0 {
		t.Errorf("line %d carries the emoji %q; the default row has none: %q", i, r, plain[i])
	}
}

// REQ-20, REQ-21, REQ-23, REQ-35 (FR2 FINAL, corrected 2026-09-28): the status
// and runtime badges of an Attention row are SOLID in its hue (BLK coral, ASK
// violet); every other row's are soft (filled, unsaturated). The ctx badge is
// its soft tier tint everywhere, solid red only on Attention + > 400k. Status
// and runtime are separate badges.
func TestPanelRenderBadgeToneFollowsAttention(t *testing.T) {
	e := newPREnv(t)
	lines := e.render(prSnapshot(t, prBandFleet()...), "--width=46", "--height=59", "--profile=truecolor")
	plain := prPlain(lines)
	for _, tc := range []struct {
		name, code string
		hue        func(*prRGB) bool // nil: a soft row
		ctxTone    string
	}{
		{"db-migrate", "BLK", prCoral, "solid-red"}, // Attention + 612k
		{"madbench-ws", "ASK", prViolet, "warm"},    // Attention + 248k: ctx on the soft honey tier
		{"kcenv-keychain", "IDL", nil, "quiet"},
		{"tt-ui-rebuild", "RUN", nil, "quiet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := prMustHead(t, plain, tc.name)
			cs := prCells(lines[h])
			at := prFind(cs, tc.code)
			if at < 0 {
				t.Fatalf("no %s on the head %q", tc.code, plain[h])
			}
			rt := prFind(cs[at+len(tc.code):], prBandRuntime[tc.name])
			if rt < 0 {
				t.Fatalf("no runtime %s… after %s on the head %q", prBandRuntime[tc.name], tc.code, plain[h])
			}
			rt += at + len(tc.code)
			cs0, cs1 := prRun(cs, at)
			rs0, _ := prRun(cs, rt)
			if cs[at].bg == nil || cs[rt].bg == nil {
				t.Fatalf("status %s bg %+v, runtime bg %+v: both are badges (filled)", tc.code, cs[at].bg, cs[rt].bg)
			}
			gap := false
			for _, c := range cs[cs1+1 : max(rs0, cs1+1)] {
				gap = gap || c.bg == nil
			}
			if !gap || cs0 > at {
				t.Errorf("status badge %d..%d and runtime badge from %d are not separate badges: %q", cs0, cs1, rs0, plain[h])
			}
			if txt, c := prBadge(lines[h]); prTone(c) != tc.ctxTone {
				t.Errorf("ctx badge %q: tone %s; want %s", txt, prTone(c), tc.ctxTone)
			}
			for part, c := range map[string]prCell{"status": cs[at], "runtime": cs[rt]} {
				if tc.hue != nil && !(prSaturated(c.bg) && tc.hue(c.bg)) {
					t.Errorf("%s badge of the Attention row: bg %+v; want a solid %s fill", part, c.bg, tc.code)
				}
				if tc.hue == nil && (c.bg == nil || prSaturated(c.bg)) {
					t.Errorf("%s badge of a calm row: bg %+v; want a soft (filled, unsaturated) tint", part, c.bg)
				}
			}
		})
	}
}

// REQ-23 (FR2 FINAL, corrected 2026-09-28): on an Attention row the ctx badge
// keeps its soft tier tint up to 400k and turns solid red only above it; a
// calm row above 400k stays soft red.
func TestPanelRenderAttentionCtxBadgeIsSolidOnlyAbove400k(t *testing.T) {
	e := newPREnv(t)
	cases := []struct {
		name, status string
		used         int
		tone         string
	}{
		{"a-ask-50k", "waiting", 50_000, "quiet"},
		{"a-blk-300k", "blocked", 300_000, "warm"},
		{"a-ask-400k", "waiting", 400_000, "warm"},
		{"a-ask-400k1", "waiting", 400_001, "solid-red"},
		{"a-blk-612k", "blocked", 612_000, "solid-red"},
		{"c-idle-612k", "idle", 612_000, "soft-red"},
	}
	var fleet []prObj
	for _, c := range cases {
		fleet = append(fleet, prCtx(prSession(c.name, c.status, 10*time.Minute), c.used))
	}
	lines := e.render(prSnapshot(t, fleet...), "--width=60", "--height=59", "--profile=truecolor")
	plain := prPlain(lines)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			txt, cell := prBadge(lines[prMustHead(t, plain, c.name)])
			if got := prTone(cell); got != c.tone {
				t.Errorf("%s %d: ctx badge %q tone %s; want %s", c.status, c.used, txt, got, c.tone)
			}
		})
	}
}

// REQ-35 (FR2 FINAL): the runtime badge counts from the session start, not the
// status change, and has one fixed width whatever the value.
func TestPanelRenderRuntimeBadgeIsTimeSinceStart(t *testing.T) {
	e := newPREnv(t)
	cases := []struct {
		name     string
		status   string
		inStatus time.Duration
		started  time.Duration
		want     string // substring of the runtime
		notWant  string // the time in status
	}{
		{"rt-secs", "busy", 20 * time.Second, 45 * time.Second, "45s", "20s"},
		{"rt-mins", "idle", 3 * time.Minute, 7 * time.Minute, "7m", "3m"},
		{"rt-hours", "idle", 14 * time.Minute, 5*time.Hour + 20*time.Minute, "5h", "14m"},
		{"rt-days", "waiting", 13 * time.Minute, 3*24*time.Hour + time.Hour, "3d", "13m"},
	}
	var fleet []prObj
	for _, c := range cases {
		fleet = append(fleet, prStarted(prCtx(prSession(c.name, c.status, c.inStatus), 10_000), c.started))
	}
	snap := prSnapshot(t, fleet...)
	plain := prPlain(e.render(snap, "--width=60", "--height=40", "--profile="+prTextProfile))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := plain[prMustHead(t, plain, c.name)]
			head := h[:strings.Index(h, c.name)]
			if !strings.Contains(head, c.want) || strings.Contains(head, c.notWant) {
				t.Errorf("head %q; want the runtime %s… before the name, not the time in status %s", h, c.want, c.notWant)
			}
		})
	}
	if rows := prRowLines(plain, prMustHead(t, plain, "rt-days")); len(rows) < 2 || !strings.HasPrefix(strings.TrimSpace(rows[1]), "waiting 13m") {
		t.Errorf("rt-days rows %q; want the WHY line to carry the time in status `waiting 13m`", rows)
	}
	lines := e.render(snap, "--width=60", "--height=40", "--profile=truecolor")
	tp := prPlain(lines)
	widths := map[string]int{}
	for _, c := range cases {
		cs := prCells(lines[prMustHead(t, tp, c.name)])
		at := prFind(cs, c.want)
		if at < 0 || cs[at].bg == nil {
			t.Fatalf("%s: runtime %s… is not on a badge: %q", c.name, c.want, tp[prMustHead(t, tp, c.name)])
		}
		s, e := prRun(cs, at)
		widths[c.name] = e - s + 1
	}
	for _, w := range widths {
		if w != widths["rt-secs"] {
			t.Errorf("runtime badge widths differ: %v; want one fixed width", widths)
			break
		}
	}
}

// REQ-36 (FR2 FINAL, FR4): the name sits on a soft badge in the session's
// /color when it has one, and is plain (no fill) otherwise.
func TestPanelRenderNameBadgeOnlyWithAColour(t *testing.T) {
	e := newPREnv(t)
	lines := e.render(prSnapshot(t,
		prColour(prCtx(prSession("n-green", "idle", time.Minute), 10_000), "green"),
		prColour(prCtx(prSession("n-cyan", "busy", time.Minute), 10_000), "cyan"),
		prColour(prCtx(prSession("n-waitblue", "waiting", time.Minute), 10_000), "blue"),
		prCtx(prSession("n-plain", "idle", time.Minute), 10_000),
		prCtx(prSession("n-plainwait", "waiting", time.Minute), 10_000),
	), "--width=60", "--height=40", "--profile=truecolor")
	plain := prPlain(lines)
	for _, tc := range []struct {
		name string
		hue  func(*prRGB) bool // nil: no fill
	}{
		{"n-green", func(c *prRGB) bool { return c.g > c.r && c.g > c.b }},
		{"n-cyan", func(c *prRGB) bool { return c.g > c.r && c.b > c.r }},
		{"n-waitblue", func(c *prRGB) bool { return c.b > c.r && c.b > c.g }},
		{"n-plain", nil},
		{"n-plainwait", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := prCells(lines[prMustHead(t, plain, tc.name)])
			at := prFind(cs, tc.name)
			for _, c := range cs[at : at+len([]rune(tc.name))] {
				switch {
				case tc.hue == nil && c.bg != nil:
					t.Fatalf("name cell %q of a session without a colour has a fill %+v", string(c.ch), *c.bg)
				case tc.hue != nil && (c.bg == nil || prSaturated(c.bg) || !tc.hue(c.bg)):
					t.Fatalf("name cell %q bg %+v; want a soft tint of the session colour", string(c.ch), c.bg)
				}
			}
		})
	}
}

// REQ-21 (FR2 FINAL, FR3, R5): saturated fills appear only on the head of an
// Attention row; attention tone follows s.Attention on every row, Live=nil
// included, and that row keeps its home group; >400k off attention is soft.
func TestPanelRenderOnlyAttentionRowsAreSaturated(t *testing.T) {
	e := newPREnv(t)
	fleet := prBandFleet()
	hazy := prCtx(prSession("hazy-worker", "waiting", 7*time.Minute), 140_000)
	hazy["live"], hazy["paneAlive"], hazy["liveReason"] = nil, nil, "gate-error"
	hot := prCtx(prSession("streaming-ui", "busy", 5*time.Minute), 431_000)
	fleet = append(fleet, hazy, hot)
	lines := e.render(prSnapshot(t, fleet...), "--width=60", "--height=59", "--profile=truecolor")
	plain := prPlain(lines)
	blk, wt, hz := prMustHead(t, plain, "db-migrate"), prMustHead(t, plain, "madbench-ws"), prMustHead(t, plain, "hazy-worker")

	// Control: saturation is detectable — 612k + Attention is solid red.
	if txt, c := prBadge(lines[blk]); prTone(c) != "solid-red" {
		t.Errorf("control: the 612k attention badge %q is %s; want solid-red", txt, prTone(c))
	}
	cs := prCells(lines[hz])
	if at := prFind(cs, "ASK"); at < 0 || !prSaturated(cs[at].bg) || !prViolet(cs[at].bg) {
		t.Errorf("Attention + Live=nil head %q; want ASK on a solid violet badge", plain[hz])
	}
	band := prHead(plain, "NEEDS YOU")
	next := -1
	for j := band + 1; band >= 0 && j < len(plain); j++ {
		if plain[j] != "" && !strings.HasPrefix(plain[j], " ") && !strings.HasPrefix(plain[j], "▌") {
			next = j
			break
		}
	}
	if band < 0 || next < 0 || hz < next {
		t.Errorf("Live=nil attention row should sit in its home group, after the band (band %d, next heading %d, row %d):\n%s", band, next, hz, prDump(lines))
	}
	attn := map[int]bool{blk: true, wt: true, hz: true}
	for i, l := range lines {
		if attn[i] || prChrome(plain, i) {
			continue
		}
		for j, c := range prCells(l) {
			if prSaturated(c.bg) {
				t.Errorf("line %d cell %d %q carries a saturated fill off an Attention head: %q", i, j, string(c.ch), plain[i])
				break
			}
		}
	}
	hotLine := lines[prMustHead(t, plain, "streaming-ui")]
	if prHasBg(hotLine, prSaturated) {
		t.Errorf(">400k without attention must be a soft badge, found a solid fill: %q", hotLine)
	}
	if _, c := prBadge(hotLine); prTone(c) != "soft-red" {
		t.Errorf(">400k without attention: ctx tone %s; want soft-red", prTone(c))
	}
}

// prHasBgBefore: a cell before the session name has a background matching pred.
func prHasBgBefore(line, name string, pred func(*prRGB) bool) bool {
	cs := prCells(line)
	end := prFind(cs, name)
	if end < 0 {
		end = len(cs)
	}
	for _, c := range cs[:end] {
		if pred(c.bg) {
			return true
		}
	}
	return false
}

// REQ-32 (FR2 revised): `{status:chip}` is opt-in and still renders the loud
// WAIT/BLCK chips (Attention, Live=nil included) and the dim chip.
func TestPanelRenderStatusChipIsOptIn(t *testing.T) {
	e := newPREnv(t)
	fleet := prBandFleet()
	hazy := prCtx(prSession("hazy-worker", "waiting", 7*time.Minute), 140_000)
	hazy["live"], hazy["paneAlive"], hazy["liveReason"] = nil, nil, "gate-error"
	fleet = append(fleet, hazy)
	cfg := e.conf("row = \"{gutter}{status:chip} {name:colour} · {group}{>}{ctx}\"\n" +
		"attn_row = \"{gutter}{status:chip} {name:colour}{>}{ctx}\"\n")
	lines := e.render(prSnapshot(t, fleet...), "--width=60", "--height=59", "--profile=truecolor", "--config="+cfg)
	plain := prPlain(lines)
	for _, tc := range []struct {
		name, label string
		fill        func(*prRGB) bool
	}{
		{"madbench-ws", "WAIT", prViolet},
		{"db-migrate", "BLCK", prCoral},
		{"hazy-worker", "WAIT", prViolet},
	} {
		i := prMustHead(t, plain, tc.name)
		if !strings.Contains(plain[i], tc.label) || !prHasBgBefore(lines[i], tc.name, tc.fill) {
			t.Errorf("%s with {status:chip}: want the loud %s chip with a solid fill before the name: %q", tc.name, tc.label, lines[i])
		}
	}
	i := prMustHead(t, plain, "kcenv-keychain")
	if !strings.Contains(plain[i], "idle") || strings.Contains(plain[i], "IDLE") {
		t.Errorf("idle head %q with {status:chip}; want the lower-case idle chip", plain[i])
	}
	if !prHasBgBefore(lines[i], "kcenv-keychain", func(c *prRGB) bool { return c != nil }) ||
		prHasBgBefore(lines[i], "kcenv-keychain", prSaturated) {
		t.Errorf("idle chip with {status:chip}: want a dim, unsaturated fill: %q", lines[i])
	}
}

// prStatusFleet: one row per live status, varied ages, runtimes and gutters
// (st-shell has no start time).
func prStatusFleet() []prObj {
	unclear := prStarted(prSession("st-unclear", "unknown", 41*time.Minute), time.Hour)
	unclear["live"], unclear["paneAlive"], unclear["liveReason"] = nil, nil, "gate-error"
	return []prObj{
		prStarted(prColour(prCtx(prSession("st-busy", "busy", time.Minute), 30_000), "green"), 45*time.Second),
		prStarted(prCtx(prJob("st-work", "working", 26*time.Hour), 40_000), 3*24*time.Hour),
		prStarted(prColour(prCtx(prSession("st-wait", "waiting", 13*time.Minute), 50_000), "blue"), 7*time.Minute),
		prStarted(prCtx(prJob("st-block", "blocked", 3*time.Hour), 60_000), 5*time.Hour+20*time.Minute),
		prStarted(prColour(prCtx(prSession("st-idle", "idle", 14*time.Minute), 70_000), "cyan"), 14*time.Minute),
		prCtx(prSession("st-shell", "shell", 5*time.Minute), 80_000),
		unclear,
	}
}

// prEndedFleet: a live idle row plus the two ended statuses (with --ended).
func prEndedFleet() []prObj {
	done := prJob("st-done", "done", 2*time.Hour)
	done["live"], done["liveReason"] = false, "job-terminal"
	failed := prJob("st-failed", "failed", 2*time.Hour)
	failed["live"], failed["liveReason"] = false, "job-terminal"
	return []prObj{prCtx(prSession("st-live", "idle", time.Minute), 1000), done, failed}
}

// REQ-30 (FR2 FINAL, status family A): the default row draws the 3-letter code
// per status, and no emoji anywhere.
func TestPanelRenderDefaultStatusCodePerStatus(t *testing.T) {
	e := newPREnv(t)
	check := func(t *testing.T, lines []string, name, status string) {
		t.Helper()
		plain := prPlain(lines)
		h := plain[prMustHead(t, plain, name)]
		if got, want := prCode(h), prCodes[status]; got != want {
			t.Errorf("%s (%s): code %q; want %q in head %q", name, status, got, want, h)
		}
	}
	noEmoji := func(t *testing.T, lines []string) {
		t.Helper()
		if i, r := prFirstEmoji(lines); i >= 0 {
			t.Errorf("line %d carries %q; the default frame has no emoji:\n%s", i, r, prDump(lines))
		}
	}
	lines := e.render(prSnapshot(t, prStatusFleet()...), "--width=46", "--height=59", "--profile="+prTextProfile)
	noEmoji(t, lines)
	for _, s := range prStatusFleet() {
		name, status := s["name"].(string), s["status"].(string)
		t.Run(status, func(t *testing.T) { check(t, lines, name, status) })
	}
	// FR9: ended sessions are not drawn; --ended is accepted and ignored.
	ended := e.render(prSnapshot(t, prEndedFleet()...), "--width=46", "--height=59", "--profile="+prTextProfile, "--ended")
	noEmoji(t, ended)
	plain := prPlain(ended)
	prMustHead(t, plain, "st-live")
	for _, name := range []string{"st-done", "st-failed"} {
		if i := prHead(plain, name); i >= 0 {
			t.Errorf("the ended session %s is drawn at line %d:\n%s", name, i, prDump(ended))
		}
	}
}

// REQ-38 (FR2 FINAL): no tmux pane id and no background job id anywhere in
// the default frame, band included; ended jobs in the snapshot (not drawn,
// FR9) leak none either.
func TestPanelRenderDefaultShowsNoTmuxOrJobIDs(t *testing.T) {
	e := newPREnv(t)
	live := append(prBandFleet(), prStatusFleet()...)
	fleet := append(live, prEndedFleet()[1:]...)
	snap := prSnapshot(t, fleet...)
	paneID := regexp.MustCompile(`%[0-9]+`)
	jobID := regexp.MustCompile(`\bbg\b`)
	for _, w := range []int{46, 120} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			plain := prPlain(e.render(snap, fmt.Sprintf("--width=%d", w), "--height=80", "--profile="+prTextProfile, "--ended"))
			for _, s := range live {
				prMustHead(t, plain, s["name"].(string))
			}
			for i, l := range plain {
				if m := paneID.FindString(l); m != "" {
					t.Errorf("line %d shows the tmux pane id %s: %q", i, m, l)
				}
				if jobID.MatchString(l) {
					t.Errorf("line %d shows a background-job marker: %q", i, l)
				}
				for _, s := range fleet {
					if id := s["shortId"].(string); strings.Contains(l, id) {
						t.Errorf("line %d shows the id %s of %s: %q", i, id, s["name"], l)
					}
				}
			}
		})
	}
}

// REQ-39 (FR9): the built-in meta line only names a row's group when the row
// sits outside it, so a calm row in its home group has none. The opt-in meta
// the built-in file suggests, `{group} · {model:short} · {sub} · {apierr}`,
// draws `model · N sub`, api errors only when non-zero; a row with none of
// them has no meta line.
func TestPanelRenderMetaLineIsModelAndSubagents(t *testing.T) {
	e := newPREnv(t)
	sub := prCtx(prSession("m-sub", "idle", 5*time.Minute), 1000)
	prTranscript(sub)["model"] = "claude-opus-5-5"
	prTranscript(sub)["subagents"], prTranscript(sub)["subagentsKnown"] = 2, true
	errs := prCtx(prSession("m-err", "busy", 5*time.Minute), 1000)
	prTranscript(errs)["model"] = "claude-sonnet-5"
	prTranscript(errs)["apiErrors"] = 3
	bare := prCtx(prSession("m-bare", "idle", 5*time.Minute), 1000)
	snap := prSnapshot(t, sub, errs, bare)

	def := prPlain(e.render(snap, "--width=46", "--height=30", "--profile="+prTextProfile))
	for _, name := range []string{"m-sub", "m-err", "m-bare"} {
		if rows := prRowLines(def, prMustHead(t, def, name)); len(rows) != 1 {
			t.Errorf("built-in meta: %s rows %q; a row in its home group has no meta line", name, rows)
		}
	}

	cfg := e.conf("meta = \"     {group} · {model:short} · {sub} · {apierr}\"\n")
	plain := prPlain(e.render(snap, "--width=46", "--height=30", "--profile="+prTextProfile, "--config="+cfg))
	rows := prRowLines(plain, prMustHead(t, plain, "m-sub"))
	if len(rows) != 2 {
		t.Fatalf("m-sub rows %q; want head + meta", rows)
	}
	m := rows[1]
	if i, j := strings.Index(m, "opus-5-5"), strings.Index(m, "2 sub"); i < 0 || j < i || !strings.Contains(m[i:j], "·") {
		t.Errorf("meta %q; want `opus-5-5 · 2 sub`", m)
	}
	if strings.Contains(m, "api") || strings.Contains(m, "%") {
		t.Errorf("meta %q; api errors appear only when non-zero, and no pane id", m)
	}
	rows = prRowLines(plain, prMustHead(t, plain, "m-err"))
	if len(rows) != 2 || !strings.Contains(rows[1], "sonnet-5") || !strings.Contains(rows[1], "3 api") {
		t.Errorf("m-err rows %q; want a meta line with sonnet-5 and 3 api err", rows)
	}
	if rows = prRowLines(plain, prMustHead(t, plain, "m-bare")); len(rows) != 1 {
		t.Errorf("m-bare rows %q; with no model, sub or api err the meta line vanishes", rows)
	}
}

// REQ-31 (FR2 FINAL): names start in one column whatever the status code, the
// runtime value (45s … 3d, unknown) or the gutter.
func TestPanelRenderNamesStartInOneColumn(t *testing.T) {
	e := newPREnv(t)
	for _, profile := range []string{prTextProfile, "truecolor"} {
		t.Run(profile, func(t *testing.T) {
			lines := e.render(prSnapshot(t, prStatusFleet()...), "--width=46", "--height=59", "--profile="+profile)
			plain := prPlain(lines)
			cols := map[string]int{}
			for _, s := range prStatusFleet() {
				name := s["name"].(string)
				cols[name] = prNameCol(plain[prMustHead(t, plain, name)], name)
			}
			want := cols["st-idle"]
			for name, c := range cols {
				if c != want {
					t.Errorf("%s starts at column %d; st-idle at %d:\n%s", name, c, want, prDump(lines))
				}
			}
		})
	}
}

// REQ-30 (FR2 FINAL: `{status:icon}` remains opt-in): the opt-in slot draws
// the emoji per status, single codepoints, 2 columns wide.
func TestPanelRenderStatusIconIsOptIn(t *testing.T) {
	e := newPREnv(t)
	cfg := e.conf(prIconConf)
	check := func(t *testing.T, lines []string, name, status string) {
		t.Helper()
		plain := prPlain(lines)
		h := plain[prMustHead(t, plain, name)]
		got, want := prIcon(h), prIcons[status]
		if got != want {
			t.Errorf("%s (%s): icon %q; want %q in head %q", name, status, got, want, h)
		}
		if w := ansi.StringWidth(got); got != "" && w != 2 {
			t.Errorf("%s: icon %q is %d columns; the slot is 2", name, got, w)
		}
		if out := strings.Join(lines, "\n"); strings.ContainsAny(out, "\uFE0F\u200D") {
			t.Errorf("the icon render carries U+FE0F or U+200D:\n%s", prDump(lines))
		}
	}
	lines := e.render(prSnapshot(t, prStatusFleet()...), "--width=46", "--height=59", "--profile="+prTextProfile, "--config="+cfg)
	for _, s := range prStatusFleet() {
		name, status := s["name"].(string), s["status"].(string)
		t.Run(status, func(t *testing.T) { check(t, lines, name, status) })
	}
}

// REQ-33: with `{status:icon}` opted in, icon_<status> overrides the glyph; a
// 1-column glyph is padded to the 2-column slot so the name does not move.
func TestPanelRenderIconOverridePadsToTheSlot(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t,
		prCtx(prSession("o-idle", "idle", 14*time.Minute), 20_000),
		prCtx(prSession("o-busy", "busy", 2*time.Minute), 30_000),
	)
	cfg := e.conf(prIconConf + "icon_idle = \"z\"\n")
	plain := prPlain(e.render(snap, "--width=46", "--height=30", "--profile="+prTextProfile, "--config="+cfg))
	idle, busy := plain[prMustHead(t, plain, "o-idle")], plain[prMustHead(t, plain, "o-busy")]
	if rs := []rune(idle); prIcon(idle) != "z" || len(rs) < 3 || rs[2] != ' ' {
		t.Errorf("idle head %q; want z then a pad space in the 2-column slot", idle)
	}
	if prIcon(busy) != "🔄" {
		t.Errorf("busy head %q; an icon_idle override changed another status", busy)
	}
	if a, b := prNameCol(idle, "o-idle"), prNameCol(busy, "o-busy"); a != b || a < 0 {
		t.Errorf("name columns: overridden idle %d, busy %d; want equal", a, b)
	}
	for _, l := range plain {
		if strings.Contains(l, "panel.conf") {
			t.Errorf("a valid icon override raised a note: %q", l)
		}
	}
	e.write(e.snap, string(snap))
	if out, stderr, code := e.run(nil, "--check", "--config="+cfg, "--snapshot="+e.snap); code != 0 || strings.Contains(out+stderr, "panel.conf:") {
		t.Errorf("--check on {status:icon} + icon_idle = \"z\": exit %d, output %q; want 0 and no problem", code, out+stderr)
	}
}

// REQ-40 (FR2 FINAL): no context bar by default; `{ctxbar}` draws one when
// opted in, without a config problem.
func TestPanelRenderCtxBarIsOptIn(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t, prBandFleet()...)
	const bar = "▰▱█░"
	for _, profile := range []string{prTextProfile, "truecolor"} {
		plain := prPlain(e.render(snap, "--width=60", "--height=59", "--profile="+profile))
		for i, l := range plain {
			// The count strip's ░▒▓ shading is chrome, not a context bar.
			if !prChrome(plain, i) && strings.ContainsAny(l, bar) {
				t.Errorf("%s: default line %d draws a context bar: %q", profile, i, l)
			}
		}
	}
	cfg := e.conf("attn_row = \"{gutter}{status} {name}{>}{ctxbar}\"\n")
	plain := prPlain(e.render(snap, "--width=60", "--height=59", "--profile=truecolor", "--config="+cfg))
	if h := plain[prMustHead(t, plain, "db-migrate")]; !strings.ContainsAny(h, bar) {
		t.Errorf("{ctxbar} opted in: the 612k head %q draws no bar", h)
	}
	for _, l := range plain {
		if strings.Contains(l, "panel.conf") {
			t.Errorf("{ctxbar} raised a config note: %q", l)
		}
	}
}

// REQ-34 (FR2 revised): an icon with U+FE0F or a ZWJ is a soft problem —
// --check reports it and exits 0; the rejected glyph never reaches the frame.
func TestPanelRenderIconWithVariationSelectorIsSoft(t *testing.T) {
	for _, tc := range []struct{ name, icon string }{
		{"FE0F", "\u26A0\uFE0F"},
		{"ZWJ", "\U0001F469\u200D\U0001F4BB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPREnv(t)
			snap := prSnapshot(t,
				prCtx(prSession("v-idle", "idle", 14*time.Minute), 20_000),
				prCtx(prSession("v-busy", "busy", 2*time.Minute), 30_000),
			)
			e.write(e.snap, string(snap))
			cfg := e.conf("icon_idle = \"" + tc.icon + "\"\n")
			out, stderr, code := e.run(nil, "--check", "--config="+cfg, "--snapshot="+e.snap)
			if code != 0 {
				t.Errorf("--check exit %d; want 0 (soft problem)\n%s%s", code, out, stderr)
			}
			if !strings.Contains(out+stderr, "panel.conf:1") {
				t.Errorf("--check did not report panel.conf:1:\n%s%s", out, stderr)
			}
			lines := e.render(snap, "--width=46", "--height=30", "--profile="+prTextProfile, "--config="+cfg)
			plain := prPlain(lines)
			if o := strings.Join(lines, "\n"); strings.ContainsAny(o, "\uFE0F\u200D") {
				t.Errorf("the rejected glyph reached the frame:\n%s", prDump(lines))
			}
			idle, busy := plain[prMustHead(t, plain, "v-idle")], plain[prMustHead(t, plain, "v-busy")]
			if a, b := prNameCol(idle, "v-idle"), prNameCol(busy, "v-busy"); a != b {
				t.Errorf("name columns: idle %d, busy %d; want equal", a, b)
			}
		})
	}
}

// REQ-22: under height pressure the band keeps head + WHY and is never cut.
func TestPanelRenderBandKeepsItsWhyUnderPressure(t *testing.T) {
	e := newPREnv(t)
	fleet := prBandFleet()[:2]
	for i := 0; i < 40; i++ {
		status := "idle"
		if i%3 == 0 {
			status = "busy"
		}
		s := prCtx(prSession(fmt.Sprintf("quiet-%02d", i), status, time.Duration(i+1)*time.Minute), 50_000+i*1000)
		s["cwd"] = fmt.Sprintf("/tmp/ttpr-projects/p%d", i%8)
		fleet = append(fleet, s)
	}
	lines := e.render(prSnapshot(t, fleet...), "--width=46", "--height=14", "--profile="+prTextProfile)
	plain := prPlain(lines)
	if len(lines) > 14 {
		t.Errorf("%d lines > --height 14", len(lines))
	}
	for name, why := range map[string]string{"db-migrate": "confirm", "madbench-ws": "permission-prompt"} {
		rows := prRowLines(plain, prMustHead(t, plain, name))
		if len(rows) < 2 || !strings.Contains(rows[1], why) {
			t.Errorf("band row %s kept %d line(s) %q; want head + WHY (%q) while non-band rows can be cut:\n%s",
				name, len(rows), rows, why, prDump(lines))
		}
	}
}

// REQ-23: the badge shows the used count; its tone follows absolute thresholds;
// solid red only with attention; unknown usage is an unfilled "—".
func TestPanelRenderCtxBadgeTones(t *testing.T) {
	e := newPREnv(t)
	cases := []struct {
		name string
		used any
		tone string
		text string
	}{
		{"q-850", 850, "quiet", "850"},
		{"q-150k", 150_000, "quiet", "150k"},
		{"q-edge", 199_999, "quiet", ""},
		{"w-edge", 200_000, "warm", ""},
		{"w-300k", 300_000, "warm", "300k"},
		{"w-top", 400_000, "warm", ""},
		{"r-edge", 400_001, "soft-red", ""},
		{"r-450k", 450_000, "soft-red", "450k"},
		{"r-1m2", 1_200_000, "soft-red", "1.2M"},
		{"hot-attn", 450_000, "solid-red", "450k"},
		{"no-usage", nil, "no-fill", "—"},
	}
	var fleet []prObj
	for _, c := range cases {
		status := "idle"
		if c.name == "hot-attn" {
			status = "waiting"
		}
		fleet = append(fleet, prCtx(prSession(c.name, status, 10*time.Minute), c.used))
	}
	lines := e.render(prSnapshot(t, fleet...), "--width=60", "--height=59", "--profile=truecolor")
	plain := prPlain(lines)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			txt, cell := prBadge(lines[prMustHead(t, plain, c.name)])
			if got := prTone(cell); got != c.tone {
				t.Errorf("used=%v: badge %q tone %s; want %s", c.used, txt, got, c.tone)
			}
			if c.text != "" && txt != c.text {
				t.Errorf("used=%v: badge text %q; want %q", c.used, txt, c.text)
			}
		})
	}
}

// REQ-23: unknown usage reads "—", never a fake 0.
func TestPanelRenderUnknownUsageIsADash(t *testing.T) {
	e := newPREnv(t)
	s := prSession("no-usage", "idle", time.Minute)
	lines := e.render(prSnapshot(t, s), "--width=46", "--height=20", "--profile="+prTextProfile)
	plain := prPlain(lines)
	head := plain[prMustHead(t, plain, "no-usage")]
	if !strings.HasSuffix(strings.TrimRight(head, " "), "—") {
		t.Errorf("head %q; want the ctx badge to read —", head)
	}
	if f := strings.Fields(head); f[len(f)-1] == "0" {
		t.Errorf("head %q shows a fake 0", head)
	}
}

// REQ-24: ctx_warn / ctx_alert in panel.conf move the badge edges.
func TestPanelRenderConfigMovesCtxThresholds(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t,
		prCtx(prSession("t-90k", "idle", time.Minute), 90_000),
		prCtx(prSession("t-110k", "idle", time.Minute), 110_000),
		prCtx(prSession("t-150k", "idle", time.Minute), 150_000),
	)
	tone := func(lines []string, name string) string {
		t.Helper()
		_, c := prBadge(lines[prMustHead(t, prPlain(lines), name)])
		return prTone(c)
	}
	def := e.render(snap, "--width=60", "--height=30", "--profile=truecolor")
	if got := tone(def, "t-150k"); got != "quiet" {
		t.Fatalf("control: 150k with default thresholds is %s; want quiet", got)
	}
	cfg := e.conf("ctx_warn = 100k\nctx_alert = 120k\n")
	moved := e.render(snap, "--width=60", "--height=30", "--profile=truecolor", "--config="+cfg)
	for name, want := range map[string]string{"t-90k": "quiet", "t-110k": "warm", "t-150k": "soft-red"} {
		if got := tone(moved, name); got != want {
			t.Errorf("%s with ctx_warn=100k ctx_alert=120k: %s; want %s", name, got, want)
		}
	}
}

func prQuietFleet() []prObj {
	return []prObj{
		prCtx(prSession("alpha-one", "idle", 3*time.Minute), 20_000),
		prCtx(prSession("beta-two", "busy", time.Minute), 30_000),
		prCtx(prSession("gamma-three", "idle", time.Hour), 40_000),
	}
}

// REQ-25, REQ-9: --config replaces the row templates.
func TestPanelRenderCustomConfigChangesTheLayout(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t, prQuietFleet()...)
	def := prPlain(e.render(snap, "--width=46", "--height=40", "--profile="+prTextProfile))
	if strings.Contains(strings.Join(def, "\n"), "<<") {
		t.Fatal("control: the default render already contains <<")
	}
	for _, tc := range []struct{ conf, open, shut string }{
		{"row = '<<{name}>>'\nmeta = ''\n", "<<", ">>"},
		{"row = \"{{{name}}}\"\nmeta = \"\"\n", "{", "}"},
	} {
		t.Run(tc.open, func(t *testing.T) {
			lines := e.render(snap, "--width=46", "--height=40", "--profile="+prTextProfile, "--config="+e.conf(tc.conf))
			plain := prPlain(lines)
			for _, name := range []string{"alpha-one", "beta-two", "gamma-three"} {
				want := tc.open + name + tc.shut
				if got := strings.TrimRight(plain[prMustHead(t, plain, name)], " "); got != want {
					t.Errorf("row %q; want %q", got, want)
				}
			}
			for i, l := range plain {
				if strings.HasPrefix(l, "    ") {
					t.Errorf("line %d %q is a meta line, but meta = '' removes it", i, l)
				}
			}
		})
	}
}

// REQ-25: $XDG_CONFIG_HOME/tt/panel.conf is read without --config.
func TestPanelRenderReadsXDGConfigHome(t *testing.T) {
	e := newPREnv(t)
	e.write(filepath.Join(e.cfg, "tt", "panel.conf"), "row = '<<{name}>>'\nmeta = ''\n")
	plain := prPlain(e.render(prSnapshot(t, prQuietFleet()...), "--width=46", "--height=40", "--profile="+prTextProfile))
	if got := strings.TrimRight(plain[prMustHead(t, plain, "alpha-one")], " "); got != "<<alpha-one>>" {
		t.Errorf("row %q; want <<alpha-one>> from $XDG_CONFIG_HOME/tt/panel.conf", got)
	}
}

// REQ-25: no file → built-in format and no note.
func TestPanelRenderWithoutConfigShowsNoNote(t *testing.T) {
	e := newPREnv(t)
	plain := prPlain(e.render(prSnapshot(t, prQuietFleet()...), "--width=120", "--height=40", "--profile="+prTextProfile))
	for _, l := range plain {
		if strings.Contains(l, "panel.conf") {
			t.Errorf("a config note without a config file: %q", l)
		}
	}
}

// REQ-26: a fatal config falls back to the defaults with one note naming file:line.
func TestPanelRenderBrokenConfigFallsBackWithANote(t *testing.T) {
	e := newPREnv(t)
	snap := prSnapshot(t, prQuietFleet()...)
	def := prPlain(e.render(snap, "--width=120", "--height=40", "--profile="+prTextProfile))
	cfg := e.conf("# my panel\n# broken below\nrow = '{name'\n")
	got := prPlain(e.render(snap, "--width=120", "--height=40", "--profile="+prTextProfile, "--config="+cfg))
	// The note takes the place of a blank filler line, so blanks are left out
	// of the comparison.
	var notes, rest, want []string
	for _, l := range got {
		if strings.Contains(l, "panel.conf") {
			notes = append(notes, l)
		} else if strings.TrimSpace(l) != "" {
			rest = append(rest, l)
		}
	}
	for _, l := range def {
		if strings.TrimSpace(l) != "" {
			want = append(want, l)
		}
	}
	if len(got) != len(def) {
		t.Errorf("the frame with the note is %d lines, the built-in one %d", len(got), len(def))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "panel.conf:3") {
		t.Errorf("config notes %q; want exactly one naming panel.conf:3", notes)
	}
	if strings.Join(rest, "\n") != strings.Join(want, "\n") {
		t.Errorf("apart from the note, the frame differs from the built-in render:\n%s\n--- default ---\n%s",
			strings.Join(got, "\n"), strings.Join(def, "\n"))
	}
}

// REQ-26, REQ-11: a soft problem keeps the template, draws {foo?}, shows a note.
func TestPanelRenderSoftProblemIsVisibleInPlace(t *testing.T) {
	e := newPREnv(t)
	cfg := e.conf("row = '{gutter}{status:chip} {name:colour} {foo}{>}{ctx}'\n")
	plain := prPlain(e.render(prSnapshot(t, prQuietFleet()...), "--width=120", "--height=40", "--profile="+prTextProfile, "--config="+cfg))
	if h := plain[prMustHead(t, plain, "alpha-one")]; !strings.Contains(h, "{foo?}") {
		t.Errorf("row %q; want the unknown token drawn in place as {foo?}", h)
	}
	found := false
	for _, l := range plain {
		if strings.Contains(l, "panel.conf:1") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note naming panel.conf:1:\n%s", strings.Join(plain, "\n"))
	}
}

// REQ-27: --check lints: every problem printed, exit 1 only on a fatal one.
func TestPanelRenderCheckExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name, conf string
		code       int
		want       []string
	}{
		{"clean", "row = '{name}'\n", 0, nil},
		{"soft", "row = '{name} {foo}'\n", 0, []string{"panel.conf:1", "foo"}},
		{"fatal", "meta = '{name}'\nrow = '{name'\n", 1, []string{"panel.conf:2"}},
		{"all soft listed", "row = '{name} {foo}'\nmeta = '{bar}'\nattn_meta = '{pane:zzz}'\n", 0, []string{"panel.conf:1", "panel.conf:2", "panel.conf:3"}},
		{"soft and fatal listed", "row = '{name} {foo}'\nmeta = '{name'\n", 1, []string{"panel.conf:1", "panel.conf:2"}},
		{"bad threshold", "ctx_warn = 500k\n", 1, []string{"panel.conf:1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPREnv(t)
			e.write(e.snap, string(prSnapshot(t, prQuietFleet()...)))
			out, stderr, code := e.run(nil, "--check", "--config="+e.conf(tc.conf), "--snapshot="+e.snap)
			all := out + stderr
			if code != tc.code {
				t.Errorf("exit %d; want %d\n%s", code, tc.code, all)
			}
			for _, w := range tc.want {
				if !strings.Contains(all, w) {
					t.Errorf("output lacks %q:\n%s", w, all)
				}
			}
			if tc.want == nil && strings.Contains(all, "panel.conf:") {
				t.Errorf("a clean file reported a problem:\n%s", all)
			}
		})
	}
	t.Run("built-in", func(t *testing.T) {
		e := newPREnv(t)
		e.write(e.snap, string(prSnapshot(t, prQuietFleet()...)))
		if out, stderr, code := e.run(nil, "--check", "--snapshot="+e.snap); code != 0 {
			t.Errorf("--check with no config file exited %d:\n%s%s", code, out, stderr)
		}
	})
}

type prFile struct {
	mode fs.FileMode
	size int64
	mtim int64
	sum  [32]byte
}

func prTree(t *testing.T, root string) map[string]prFile {
	t.Helper()
	out := map[string]prFile{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p == filepath.Join(root, "tmp") {
			return filepath.SkipDir // TMPDIR: process scratch, not a user file
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f := prFile{mode: info.Mode(), size: info.Size(), mtim: info.ModTime().UnixNano()}
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f.sum = sha256.Sum256(b)
		}
		out[p] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// REQ-28: panel-render and --check write nothing (panel.conf is read-only).
func TestPanelRenderWritesNothing(t *testing.T) {
	e := newPREnv(t)
	e.write(filepath.Join(e.cfg, "tt", "panel.conf"), "row = '{name} {foo}'\n")
	other := e.conf("row = '{name'\n")
	e.write(e.snap, string(prSnapshot(t, prBandFleet()...)))
	before := prTree(t, e.root)
	e.run(nil, "--snapshot="+e.snap, "--width=46", "--height=59", "--profile=truecolor")
	e.run(nil, "--snapshot="+e.snap, "--config="+other, "--profile="+prTextProfile)
	e.run(nil, "--check", "--snapshot="+e.snap)
	e.run(nil, "--check", "--config="+other, "--snapshot="+e.snap)
	after := prTree(t, e.root)
	for p, b := range before {
		if a, ok := after[p]; !ok || a != b {
			t.Errorf("%s changed or vanished", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}
}

// REQ-29: the gutter is ▌ in the session colour; blank without a known colour.
func TestPanelRenderGutterUsesTheSessionColour(t *testing.T) {
	e := newPREnv(t)
	lines := e.render(prSnapshot(t,
		prColour(prSession("g-green", "idle", time.Minute), "green"),
		prSession("g-none", "idle", time.Minute),
		prColour(prSession("g-hex", "idle", time.Minute), "#123456"),
	), "--width=46", "--height=30", "--profile=truecolor")
	plain := prPlain(lines)
	green := prCells(lines[prMustHead(t, plain, "g-green")])
	if len(green) == 0 || green[0].ch != '▌' || green[0].fg == nil || *green[0].fg != (prRGB{0x5F, 0xD7, 0x5F}) {
		t.Errorf("green gutter %+v; want ▌ in #5FD75F", green[:min(1, len(green))])
	}
	for _, name := range []string{"g-none", "g-hex"} {
		cs := prCells(lines[prMustHead(t, plain, name)])
		if len(cs) == 0 || cs[0].ch != ' ' {
			t.Errorf("%s: gutter %q; want a blank reserved cell", name, plain[prMustHead(t, plain, name)])
		}
	}
}
