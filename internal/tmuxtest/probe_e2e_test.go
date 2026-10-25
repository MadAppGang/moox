//go:build tmuxe2e

package tmuxtest_test

// Probes PV1–PV14 (architecture §12.2): every tmux behaviour the design rests
// on, measured on isolated servers and kept in the suite as a regression
// test. Each probe writes one line to $MOOX_PROBES_OUT (when set) in the form
//
//	PVn <PASS|FAIL> tmux <version>: <what was measured>
//
// PV3/4/5/7/13/14 were answered on 3.8 during planning (E1–E6, Q1–Q7, Q10);
// here they are regression tests, and CI re-runs them on 3.6a.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func lookPath(name string) (string, error) { return exec.LookPath(name) }

var probeOut struct {
	sync.Mutex
	once sync.Once
	ver  string
}

// record writes the probe's verdict. A FAIL is also a test failure: the
// fallback in §12.2's table is then a design decision to make, not a result
// to file away.
func record(t *testing.T, id string, pass bool, format string, args ...any) {
	t.Helper()
	verdict := "PASS"
	if !pass {
		verdict = "FAIL"
	}
	probeOut.once.Do(func() {
		probeOut.ver = "?"
		if bin, err := exec.LookPath("tmux"); err == nil {
			if v, err := (tmuxtarget.Target{Bin: bin}).Version(context.Background()); err == nil {
				probeOut.ver = strings.TrimSpace(strings.TrimPrefix(v.Raw, "tmux "))
			}
		}
	})
	line := fmt.Sprintf("%s %s tmux %s: %s", id, verdict, probeOut.ver, fmt.Sprintf(format, args...))
	t.Log(line)
	if p := os.Getenv("MOOX_PROBES_OUT"); p != "" {
		probeOut.Lock()
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(f, line)
			_ = f.Close()
		}
		probeOut.Unlock()
	}
	if !pass {
		t.Errorf("%s failed: %s", id, fmt.Sprintf(format, args...))
	}
}

func trim(s string) string { return strings.TrimRight(s, "\n") }

// script writes an executable /bin/sh script into a scratch dir.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func tmuxBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux")
	}
	return p
}

// layoutChecksum is tmux's layout_checksum (layout-custom.c).
func layoutChecksum(s string) string {
	var c uint16
	for i := 0; i < len(s); i++ {
		c = (c >> 1) + ((c & 1) << 15)
		c += uint16(s[i])
	}
	return fmt.Sprintf("%04x", c)
}

func layout(body string) string { return layoutChecksum(body) + "," + body }

func paneNum(id string) string { return strings.TrimPrefix(strings.TrimSpace(id), "%") }

func winSize(t *testing.T, s *tmuxtest.Server, w string) string {
	t.Helper()
	return trim(s.Run(t, "display", "-p", "-t", w, "#{window_width}x#{window_height}"))
}

// ---------------------------------------------------------------------------

func TestProbe_PV1_CurrentFileDir(t *testing.T) {
	s := tmuxtest.Start(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "p.conf")
	body := "set -gF @pv1 '#{d:current_file}'\n" +
		"if -F '#{!=:#{current_file},}' { set -gF @pv1b '#{d:current_file}' }\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Run(t, "source-file", conf)
	a, b := s.Opt(t, "@pv1"), s.Opt(t, "@pv1b")

	share, _ := tmuxtest.ShareRoot()
	cold := tmuxtest.StartCold(t)
	root := cold.Opt(t, "@moox-root")
	record(t, "PV1", a == dir && b == dir && root == share,
		"set -gF '#{d:current_file}' in a sourced file = %q, inside an if -F block = %q (want %q); moox.conf via -f at cold start: @moox-root = %q (want %q)",
		a, b, dir, root, share)
}

func TestProbe_PV2_ConfigFilesAndReload(t *testing.T) {
	s := tmuxtest.Start(t)
	single := trim(s.Run(t, "display", "-p", "#{config_files}"))

	cold := tmuxtest.StartCold(t, tmuxtest.WithHarnessLines("set -gF @pv2-loads '#{e|+:#{@pv2-loads},1}'"))
	list := trim(cold.Run(t, "display", "-p", "#{config_files}"))
	loads0 := cold.Opt(t, "@pv2-loads")
	_, fErr := cold.RunErr("source-file", "-F", "#{config_files}")
	files := strings.Split(list, ",")
	cold.Run(t, append([]string{"source-file"}, files...)...)
	loads1 := cold.Opt(t, "@pv2-loads")

	pass := !strings.Contains(single, ",") && len(files) == 2 && loads0 == "1" && fErr != nil && loads1 == "2"
	record(t, "PV2", pass,
		"one -f: config_files=%q (single path); two -f: %q (comma-joined, %d paths); source-file -F '#{config_files}' with the list: err=%v; source-file '<f1>' '<f2>' reloads both: @pv2-loads %s -> %s",
		single, list, len(files), fErr, loads0, loads1)
}

func TestProbe_PV3_ChainStopsAtFirstFailure(t *testing.T) {
	s := tmuxtest.Start(t)
	_, err := s.RunErr(tmuxtarget.Chain(
		[]string{"set", "-g", "@pv3a", "1"},
		[]string{"select-pane", "-t", "%999"},
		[]string{"set", "-g", "@pv3c", "1"},
	)...)
	a, c := s.Opt(t, "@pv3a"), s.Opt(t, "@pv3c")
	record(t, "PV3", err != nil && a == "1" && c == "",
		"chain 'set @a ; select-pane -t %%999 ; set @c': exit error=%v, @a=%q, @c=%q (the chain stops at the failing command)", err != nil, a, c)
}

func TestProbe_PV4_DefaultSizeAndWindowSizes(t *testing.T) {
	s := tmuxtest.Start(t)
	global := s.Opt(t, "default-size")

	// (a) new-session -x -y sets the SESSION default-size; set -u resets it,
	// and only windows created afterwards follow (Q4/Q10).
	ids := strings.Fields(s.Run(t, tmuxtarget.NewSession{Name: tmuxtarget.Literal("pv4"), Width: 220, Height: 60,
		PrintFormat: "#{session_id} #{window_id} #{pane_id}"}.Args()...))
	sid, w0, p0 := ids[0], ids[1], ids[2]
	sessOpt := trim(s.Run(t, "show-options", "-qv", "-t", sid, "default-size"))
	s.Run(t, "set-option", "-u", "-t", sid, "default-size")
	sessAfter := trim(s.Run(t, "show-options", "-qv", "-t", sid, "default-size"))
	w1 := trim(s.Run(t, "new-window", "-d", "-P", "-F", "#{window_id}", "-t", sid))
	a := fmt.Sprintf("session default-size=%q, after set -u=%q (global %q); first window %s, later window %s",
		sessOpt, sessAfter, global, winSize(t, s, w0), winSize(t, s, w1))
	aPass := sessOpt == "220x60" && sessAfter == "" && winSize(t, s, w0) == "220x60" && winSize(t, s, w1) == global

	// (b) SHRINK with select-layout (Q10): the window takes the layout
	// root's size and window-size stays unset (latest), unlike resize-window.
	p1 := trim(s.Run(t, "split-window", "-d", "-h", "-P", "-F", "#{pane_id}", "-t", p0))
	shrink := layout(fmt.Sprintf("160x40,0,0{79x40,0,0,%s,80x40,80,0,%s}", paneNum(p0), paneNum(p1)))
	s.Run(t, "select-layout", "-t", w0, shrink)
	shrunk := winSize(t, s, w0)
	shrinkLocal := trim(s.Run(t, "show-options", "-wqv", "-t", w0, "window-size"))
	panes := trim(s.Run(t, "list-panes", "-t", w0, "-F", "#{pane_width}x#{pane_height}"))
	bPass := shrunk == "160x40" && shrinkLocal == "" && strings.ReplaceAll(panes, "\n", " ") == "79x40 80x40"

	// (c) GROW (plan review, LOW): a window smaller than its saved layout
	// grows to it, with no client attached, two panes and one.
	gids := strings.Fields(s.Run(t, tmuxtarget.NewSession{Name: tmuxtarget.Literal("pv4g"), Width: 80, Height: 24,
		PrintFormat: "#{session_id} #{window_id} #{pane_id}"}.Args()...))
	gs, gw, gp0 := gids[0], gids[1], gids[2]
	gp1 := trim(s.Run(t, "split-window", "-d", "-h", "-P", "-F", "#{pane_id}", "-t", gp0))
	before := winSize(t, s, gw)
	grow := layout(fmt.Sprintf("200x50,0,0{99x50,0,0,%s,100x50,100,0,%s}", paneNum(gp0), paneNum(gp1)))
	_, gErr := s.RunErr("select-layout", "-t", gw, grow)
	grown := winSize(t, s, gw)
	gPanes := strings.ReplaceAll(trim(s.Run(t, "list-panes", "-t", gw, "-F", "#{pane_width}x#{pane_height}")), "\n", " ")
	growLocal := trim(s.Run(t, "show-options", "-wqv", "-t", gw, "window-size"))
	gw1Ids := strings.Fields(s.Run(t, "new-window", "-d", "-P", "-F", "#{window_id} #{pane_id}", "-t", gs))
	before1 := winSize(t, s, gw1Ids[0])
	_, g1Err := s.RunErr("select-layout", "-t", gw1Ids[0], layout("200x50,0,0,"+paneNum(gw1Ids[1])))
	grown1 := winSize(t, s, gw1Ids[0])
	// still that size a moment later, with no client to re-size it
	tmuxtest.Poll(300*time.Millisecond, 300*time.Millisecond, func() bool { return false })
	stable := winSize(t, s, gw) == "200x50" && winSize(t, s, gw1Ids[0]) == "200x50"
	cPass := gErr == nil && g1Err == nil && before == "80x24" && grown == "200x50" && gPanes == "99x50 100x50" &&
		growLocal == "" && before1 == "80x24" && grown1 == "200x50" && stable

	record(t, "PV4", aPass && bPass && cPass,
		"(a) %s | (b) shrink 220x60 -> select-layout 160x40: %s, panes %q, window-size local %q | (c) GROW 80x24 -> select-layout 200x50: 2 panes %s -> %s (panes %q, err %v, window-size local %q), 1 pane %s -> %s (err %v), still 200x50 after 300ms with no client: %v",
		a, shrunk, strings.ReplaceAll(panes, "\n", " "), shrinkLocal, before, grown, gPanes, gErr, growLocal, before1, grown1, g1Err, stable)
}

func TestProbe_PV5_PaneEnvAndSessionEnvClear(t *testing.T) {
	s := tmuxtest.Start(t)
	dir := t.TempDir()
	f := func(n string) string { return filepath.Join(dir, n) }
	read := func(n string) (string, bool) {
		var got string
		ok := tmuxtest.Poll(5*time.Second, 20*time.Millisecond, func() bool {
			b, err := os.ReadFile(f(n))
			got = string(b)
			return err == nil && len(b) > 0
		})
		return got, ok
	}
	cmd := func(n string) []string {
		return []string{`printf '[%s]' "${PV5K-unset}" > ` + tmuxtest.ConfQuote(f(n)) + "; exec sleep 120"}
	}
	sid := trim(s.Run(t, tmuxtarget.NewSession{Name: tmuxtarget.Literal("pv5"), PrintFormat: "#{session_id}",
		Env: []tmuxtarget.EnvVar{{Name: "PV5K", Value: "first; with #"}}, Command: cmd("first")}.Args()...))
	first, _ := read("first")
	sessEnvBefore := trim(s.Run(t, "show-environment", "-t", sid, "PV5K"))
	s.Run(t, tmuxtarget.UnsetSessionEnv(sid, "PV5K")...)
	sessEnv := trim(s.Run(t, "show-environment", "-t", sid, "PV5K"))
	s.Run(t, tmuxtarget.NewWindow{Target: sid, Command: cmd("sibling")}.Args()...)
	sibling, _ := read("sibling")
	s.Run(t, tmuxtarget.SplitWindow{Target: sid, Env: []tmuxtarget.EnvVar{{Name: "PV5K", Value: "own"}}, Command: cmd("split")}.Args()...)
	split, _ := read("split")
	globalEnv, _ := s.RunErr("show-environment", "-g", "PV5K")

	pass := first == "[first; with #]" && sessEnvBefore == "PV5K=first; with #" && sessEnv == "-PV5K" &&
		sibling == "[unset]" && split == "[own]" && !strings.Contains(globalEnv, "PV5K=")
	record(t, "PV5", pass,
		"new-session -e: first pane %s, session env %q; after set-environment -t $sid -r: session env %q, sibling window %s, split with its own -e %s, global env %q",
		first, sessEnvBefore, sessEnv, sibling, split, trim(globalEnv))
}

func TestProbe_PV6_HooksWaitForNoClient(t *testing.T) {
	s := tmuxtest.Start(t)
	s.Run(t, "set-hook", "-g", "after-new-window[50]", "set -g @pv6-after 1")
	s.Run(t, "set-hook", "-g", "session-created[50]", "set -g @pv6-created 1")
	s.Run(t, "new-window", "-d")
	s.Run(t, "new-session", "-d", "-s", "pv6")
	hooksFired := tmuxtest.Poll(2*time.Second, 20*time.Millisecond, func() bool {
		return s.Opt(t, "@pv6-after") == "1" && s.Opt(t, "@pv6-created") == "1"
	})
	clients := trim(s.Run(t, "list-clients", "-F", "#{client_name}"))

	// wait-for -S with no waiter is KEPT for a later waiter (Q6 c).
	s.Run(t, "wait-for", "-S", "pv6-kept")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, keptErr := s.T.Run(ctx, "wait-for", "pv6-kept")
	cancel()
	keptTook := time.Since(start)

	// A blocked waiter is not a client.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	waiter := s.T.Command(ctx2, "wait-for", "pv6-block")
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	tmuxtest.Poll(300*time.Millisecond, 300*time.Millisecond, func() bool { return false })
	whileWaiting := trim(s.Run(t, "list-clients", "-F", "#{client_pid}"))
	s.Run(t, "wait-for", "-S", "pv6-block")
	werr := waiter.Wait()

	pass := hooksFired && clients == "" && keptErr == nil && keptTook < time.Second && whileWaiting == "" && werr == nil
	record(t, "PV6", pass,
		"with no client: after-new-window and session-created hooks fired=%v (list-clients %q); wait-for -S before any waiter, then a waiter: returned err=%v in %v; a blocked waiter (pid %d) in list-clients: %q; released by -S: err=%v",
		hooksFired, clients, keptErr, keptTook.Round(time.Millisecond), waiter.Process.Pid, whileWaiting, werr)
}

func TestProbe_PV7_ConfigRunShellReachesServer(t *testing.T) {
	bin := tmuxBin(t)
	job := script(t, `"$1" -S "$2" set -g @pv7-x 1`+"\n")
	s := tmuxtest.StartCold(t, tmuxtest.WithHarnessLines(
		`run-shell "`+tmuxtest.ConfQuote(job)+` `+tmuxtest.ConfQuote(bin)+` '#{socket_path}' >/dev/null 2>&1 || true"`,
		`if -F '#{==:#{@pv7-x},1}' { set -g @pv7-y seen } { set -g @pv7-y missed }`,
	))
	x, y := s.Opt(t, "@pv7-x"), s.Opt(t, "@pv7-y")
	record(t, "PV7", x == "1" && y == "seen",
		"a synchronous config run-shell job set @pv7-x=%q through the server; the if -F on the next config line saw it: @pv7-y=%q", x, y)
}

func TestProbe_PV8_WindowRenamedStorm(t *testing.T) {
	s := tmuxtest.Start(t)
	s.Run(t, "set-hook", "-g", "window-renamed[50]", "set -gF @pv8 '#{e|+:#{@pv8},1}'")
	w := trim(s.Run(t, "new-window", "-d", "-P", "-F", "#{window_id}", "-n", "storm"))
	s.Run(t, "set-option", "-w", "-t", w, "automatic-rename", "on")
	loop := `while :; do printf x; sleep 0.1; done`
	s.Run(t, "respawn-pane", "-k", "-t", w, loop)
	for i := 1; i < 10; i++ {
		s.Run(t, "split-window", "-d", "-t", w, loop)
		s.Run(t, "select-layout", "-t", w, "tiled")
	}
	panes := len(strings.Fields(s.Run(t, "list-panes", "-t", w, "-F", "#{pane_id}")))
	s.Run(t, "set", "-g", "@pv8", "0")
	const window = 3 * time.Second
	tmuxtest.Poll(window, window, func() bool { return false })
	n, _ := strconv.Atoi(s.Opt(t, "@pv8"))
	// Bounded: tmux re-checks names on a 500 ms timer (names.c NAME_INTERVAL),
	// so 3 s allows ~6 renames per window; 20 means no storm. The 3 s debounce
	// would collapse even that into one save.
	record(t, "PV8", panes == 10 && n <= 20,
		"%d panes printing every 100 ms under automatic-rename: %d window-renamed hooks in %v (bound 20)", panes, n, window)
}

func TestProbe_PV9_PopupInheritsTMUX(t *testing.T) {
	inner := tmuxtest.Start(t)
	outer := tmuxtest.Start(t)
	bin := tmuxBin(t)
	// A real client of the inner server, attached from a pane of the outer
	// one (memory: capture what a client paints). env -u TMUX: no nesting refusal.
	outer.Run(t, "new-window", "-d", "-n", "client",
		"env -u TMUX "+tmuxtest.ConfQuote(bin)+" -u -S "+tmuxtest.ConfQuote(inner.T.Socket)+" attach -t "+inner.Session)
	var client string
	if !tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		client = trim(inner.Run(t, "list-clients", "-F", "#{client_name}"))
		return client != ""
	}) {
		t.Fatal("the nested client never attached")
	}
	out := filepath.Join(t.TempDir(), "popup")
	inner.Run(t, "display-popup", "-c", strings.Fields(client)[0], "-E",
		`printf '%s' "$TMUX" > `+tmuxtest.ConfQuote(out))
	var got string
	ok := tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		b, err := os.ReadFile(out)
		got = string(b)
		return err == nil && len(b) > 0
	})
	sock, _, _ := strings.Cut(got, ",")
	inner.Close() // detaches the nested client before the outer server goes
	record(t, "PV9", ok && sock == inner.T.Socket,
		"display-popup's shell saw TMUX=%q (server socket %q); the popup fix passes -S explicitly regardless", got, inner.T.Socket)
}

func TestProbe_PV10_IndexedHookReplacesOnReload(t *testing.T) {
	s := tmuxtest.Start(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "hooks.conf")
	for i, v := range []string{"1", "2"} {
		body := "set-hook -g after-new-window[100] 'set -g @pv10 " + v + "'\n"
		if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		s.Run(t, "source-file", conf)
		if i == 0 {
			s.Run(t, "source-file", conf) // a plain reload of the same text
		}
	}
	hooks := trim(s.Run(t, "show-hooks", "-g", "after-new-window"))
	lines := strings.Split(hooks, "\n")
	s.Run(t, "new-window", "-d")
	fired := tmuxtest.Poll(2*time.Second, 20*time.Millisecond, func() bool { return s.Opt(t, "@pv10") == "2" })
	record(t, "PV10", len(lines) == 1 && strings.Contains(lines[0], "[100]") && strings.Contains(lines[0], "@pv10 2") && fired,
		"three loads of 'set-hook -g after-new-window[100] …' (text changed on the third): show-hooks = %q; the hook ran the latest text: %v", hooks, fired)
}

func TestProbe_PV11_UnitSeparatorSurvivesFormats(t *testing.T) {
	s := tmuxtest.Start(t)
	pane := trim(s.Run(t, "display", "-p", "#{pane_id}"))
	title := "tïtle; #x ✓"
	s.Run(t, tmuxtarget.SelectPaneTitle(pane, tmuxtarget.Literal(title))...)
	out := trim(s.Run(t, "list-panes", "-t", pane, "-F", "#{pane_id}\x1f#{pane_title}\x1f#{session_name}"))
	fields := strings.Split(out, "\x1f")
	disp := trim(s.Run(t, "display", "-p", "-t", pane, "a\x1fb\x1f#{pane_title}"))
	pass := len(fields) == 3 && fields[0] == pane && fields[1] == title && fields[2] == s.Session &&
		disp == "a\x1fb\x1f"+title
	record(t, "PV11", pass, "-u list-panes -F with \\x1f: %d fields %q; display -p: %q", len(fields), fields, disp)
}

func TestProbe_PV12_AutomaticRenameFormat(t *testing.T) {
	s := tmuxtest.Start(t)
	w0 := trim(s.Run(t, "display", "-p", "#{window_id}"))
	w1 := trim(s.Run(t, "new-window", "-d", "-P", "-F", "#{window_id}"))
	w2 := trim(s.Run(t, "new-window", "-d", "-P", "-F", "#{window_id}"))
	s.Run(t, tmuxtarget.RenameWindow(w1, tmuxtarget.Literal("named"))...) // rename sets automatic-rename off locally
	s.Run(t, "set-option", "-w", "-t", w2, "automatic-rename", "off")     // explicit local off
	listed := trim(s.Run(t, "list-windows", "-F", "#{window_id}=#{automatic-rename}"))
	loop := trim(s.Run(t, "display", "-p", "#{W:#{window_id}=#{automatic-rename} }"))
	local1 := trim(s.Run(t, "show-options", "-wqv", "-t", w1, "automatic-rename"))
	want := w0 + "=1\n" + w1 + "=0\n" + w2 + "=0"
	record(t, "PV12", listed == want && strings.TrimSpace(loop) == strings.ReplaceAll(want, "\n", " ") && local1 == "off",
		"list-windows -F '#{automatic-rename}' per window: %q; #{W:…} loop: %q; rename-window left the window option %q — one chained read gives every window's value, no show-options -w chain needed",
		strings.ReplaceAll(listed, "\n", " "), strings.TrimSpace(loop), local1)
}

func TestProbe_PV13_ColdStartOrdering(t *testing.T) {
	bin := tmuxBin(t)
	gate := script(t, `n=$("$1" -S "$2" list-sessions 2>/dev/null | wc -l | tr -d ' ')`+"\n"+
		`"$1" -S "$2" set -g @pv13-gate "$n"`+"\n")
	gateLine := `run-shell "` + tmuxtest.ConfQuote(gate) + ` ` + tmuxtest.ConfQuote(bin) + ` '#{socket_path}' >/dev/null 2>&1 || true"`

	fast := tmuxtest.StartCold(t, tmuxtest.WithHarnessLines(gateLine))
	fGate, fSess, fAlive := fast.Opt(t, "@pv13-gate"), trim(fast.Run(t, "list-sessions", "-F", "#S")), fast.Alive()

	slow := tmuxtest.StartCold(t, tmuxtest.WithHarnessLines(gateLine, `run-shell "sleep 2 >/dev/null 2>&1 || true"`))
	sGate, sSess, sAlive := slow.Opt(t, "@pv13-gate"), trim(slow.Run(t, "list-sessions", "-F", "#S")), slow.Alive()

	pass := fGate == "0" && fSess == "boot" && fAlive && sGate == "0" && sSess == "boot" && sAlive
	record(t, "PV13", pass,
		"config gate saw sessions=%q, then the client's new-session made %q, server alive after the gate returned: %v; with a 2 s config job after the gate: sessions=%q, %q, alive %v",
		fGate, fSess, fAlive, sGate, sSess, sAlive)
}

func TestProbe_PV14_NamesBySidAndSemicolons(t *testing.T) {
	s := tmuxtest.Start(t)
	var notes []string
	pass := true
	for _, name := range []string{"a.b", "a:b", `a\b`, "api.v2"} {
		sid, err := s.RunErr(tmuxtarget.NewSession{Name: tmuxtarget.Literal(name), PrintFormat: "#{session_id}"}.Args()...)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%q: new-session failed: %v", name, err))
			pass = false
			continue
		}
		sid = trim(sid)
		stored := trim(s.Run(t, "display", "-p", "-t", sid, "#{session_name}"))
		_, eqErr := s.RunErr("has-session", "-t", "="+name)
		_, clrErr := s.RunErr(tmuxtarget.UnsetSessionEnv(sid, "MOOX_RESUME")...)
		env := trim(s.Run(t, "show-environment", "-t", sid, "MOOX_RESUME"))
		if clrErr != nil || env != "-MOOX_RESUME" {
			pass = false
		}
		notes = append(notes, fmt.Sprintf("%q stored %q, -t =name ok=%v, -t %s clear ok=%v (%s)", name, stored, eqErr == nil, sid, clrErr == nil, env))
	}
	values := []string{"cd /tmp && ls; git status", "fix; test", ";", `x\;`, "{ a; }", `it's "q"; x`, "ends;"}
	var cmds [][]string
	for i, v := range values {
		cmds = append(cmds, []string{"set", "-g", "@pv14v" + strconv.Itoa(i), v})
	}
	cmds = append(cmds, []string{"set", "-g", "@pv14tail", "ok"})
	_, chainErr := s.RunErr(tmuxtarget.Chain(cmds...)...)
	same := 0
	for i, v := range values {
		if s.Opt(t, "@pv14v"+strconv.Itoa(i)) == v {
			same++
		}
	}
	tail := s.Opt(t, "@pv14tail")
	pass = pass && chainErr == nil && same == len(values) && tail == "ok"
	record(t, "PV14", pass, "%s; one Chain of %d values with ';' (inner and trailing): %d/%d same, tail %q, err %v",
		strings.Join(notes, "; "), len(values), same, len(values), tail, chainErr)
}

// TestProbe_ChainArgvLimit measures the largest single exec tmux accepts
// (found in P1: a 400-command chain failed with "command too long"). The
// restore executor (P4b) must split chains below it. The bound is the
// client's imsg payload (MAX_IMSGSIZE 16384 less the header), counted over
// the argv bytes plus one NUL per argument.
func TestProbe_ChainArgvLimit(t *testing.T) {
	s := tmuxtest.Start(t)
	fits := func(n int) bool {
		_, err := s.RunErr("set", "-g", "@argv", strings.Repeat("x", n))
		return err == nil
	}
	lo, hi := 0, 64<<10
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	// bytes of the full argv as sent: -u -S <sock> set -g @argv <value>, NUL-terminated
	total := 0
	for _, a := range append([]string{"-u", "-S", s.T.Socket, "set", "-g", "@argv"}, strings.Repeat("x", lo)) {
		total += len(a) + 1
	}
	_, tooLong := s.RunErr("set", "-g", "@argv", strings.Repeat("x", lo+1))
	stderr := ""
	var te *tmuxtarget.Error
	if errors.As(tooLong, &te) {
		stderr = strings.TrimSpace(te.Stderr)
	}
	record(t, "PV-ARGV", lo > 8<<10 && te != nil,
		"largest accepted single argument %d bytes (whole argv incl. -u -S <socket> and NULs: %d bytes); one more byte fails with %q — a restore chain must be split to stay under it",
		lo, total, stderr)
}

// TestE2E_LiteralRoundTrip is FuzzLiteralRoundTrip's seed corpus against real
// tmux: a Literal title, window name and option value come back byte-identical
// and nothing in them runs (E2).
func TestE2E_LiteralRoundTrip(t *testing.T) {
	s := tmuxtest.Start(t)
	marker := filepath.Join(t.TempDir(), "ran")
	pane := trim(s.Run(t, "display", "-p", "#{pane_id}"))
	win := trim(s.Run(t, "display", "-p", "#{window_id}"))
	for _, v := range []string{"#", "##", "#{pane_id}", "raw #(touch " + marker + ") #P ##", "win #S {x}", "#[fg=red]", "a#", "π #{?a,b,c}"} {
		s.Run(t, tmuxtarget.SelectPaneTitle(pane, tmuxtarget.Literal(v))...)
		s.Run(t, tmuxtarget.RenameWindow(win, tmuxtarget.Literal(v))...)
		s.Run(t, "set", "-gF", "@lit", tmuxtarget.Literal(v).String())
		title := trim(s.Run(t, "display", "-p", "-t", pane, "#{pane_title}"))
		name := trim(s.Run(t, "display", "-p", "-t", win, "#{window_name}"))
		opt := s.Opt(t, "@lit")
		if title != v || name != v || opt != v {
			t.Errorf("%q came back title=%q name=%q option=%q", v, title, name, opt)
		}
	}

	// A deterministic random corpus over the characters the format language
	// cares about, through one Chain of set -gF and one read-back. This is
	// what found that a '#' run before '[' is copied verbatim.
	rng := rand.New(rand.NewPCG(1, 2))
	const alphabet = "##[](){},xS?;"
	corpus := make([]string, 400)
	var cmds [][]string
	for i := range corpus {
		b := make([]byte, 1+rng.IntN(9))
		for j := range b {
			b[j] = alphabet[rng.IntN(len(alphabet))]
		}
		corpus[i] = string(b)
		cmds = append(cmds, []string{"set", "-gF", "@lit" + strconv.Itoa(i), tmuxtarget.Literal(corpus[i]).String()})
	}
	for i := 0; i < len(cmds); i += 50 { // one exec is bounded (TestProbe_ChainArgvLimit)
		s.Run(t, tmuxtarget.Chain(cmds[i:min(i+50, len(cmds))]...)...)
	}
	bad := 0
	for i, v := range corpus {
		if got := s.Opt(t, "@lit"+strconv.Itoa(i)); got != v {
			bad++
			if bad <= 10 {
				t.Errorf("corpus %q -> Literal %q -> tmux %q", v, tmuxtarget.Literal(v).String(), got)
			}
		}
	}
	t.Logf("%d/%d random strings round-tripped", len(corpus)-bad, len(corpus))

	tmuxtest.Poll(500*time.Millisecond, 500*time.Millisecond, func() bool { return false })
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a #() job in a Literal ran")
	}
}
