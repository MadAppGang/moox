package sidmap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	sidA = "aaaaaaaa-0000-4000-8000-000000000001"
	sidB = "bbbbbbbb-0000-4000-8000-000000000002"
	sidC = "cccccccc-0000-4000-8000-000000000003"
	sidD = "dddddddd-0000-4000-8000-000000000004"
)

// world is a fixture: panes, a ps table and the by-pid registry.
type world struct {
	panes []Pane
	procs []Proc
	reg   map[int]string
}

func (w *world) pane(pid int, target, id string) {
	w.panes = append(w.panes, Pane{Pid: pid, Target: target, ID: id})
	w.proc(pid, 900, "-zsh")
}

func (w *world) proc(pid, ppid int, cmd string) { w.procs = append(w.procs, Proc{pid, ppid, cmd}) }

func (w *world) register(pid int, sid string) {
	if w.reg == nil {
		w.reg = map[int]string{}
	}
	w.reg[pid] = sid
}

// chain adds n wrapper processes under parent and returns the deepest pid.
func (w *world) chain(parent, firstPid, n int) int {
	pid := parent
	for i := 0; i < n; i++ {
		w.proc(firstPid+i, pid, fmt.Sprintf("/usr/bin/wrapper%d run", i))
		pid = firstPid + i
	}
	return pid
}

func (w *world) run() Result { return Map(w.panes, w.procs, w.reg) }

func sidOf(r Result, paneID string) string { return r.ByPane[paneID].SID }

// TestSidmap_ClimbAnyDepth (B6, R2, R3): depth 0 (Claude exec'd as the pane
// process), depth 2 (zsh → op run → claude, the alias that hid every fresh
// session), and the deepest the bound allows: the pane plus 23 processes
// below it (24 processes in the chain).
func TestSidmap_ClimbAnyDepth(t *testing.T) {
	var w world
	w.panes = append(w.panes, Pane{Pid: 100, Target: "s:1.1", ID: "%1"})
	w.proc(100, 900, "claude --dangerously-skip-permissions")
	w.register(100, sidA)

	w.pane(200, "s:1.2", "%2")
	w.proc(201, 200, "op run --env-file x -- claude")
	w.proc(202, 201, "claude")
	w.register(202, sidB)

	w.pane(300, "s:1.3", "%3")
	deep := w.chain(300, 301, 23)
	w.register(deep, sidC)

	r := w.run()
	for id, want := range map[string]string{"%1": sidA, "%2": sidB, "%3": sidC} {
		if got := sidOf(r, id); got != want {
			t.Errorf("%s: sid %q, want %q", id, got, want)
		}
	}
	if r.ByPane["%1"].Depth != 0 || r.ByPane["%2"].Depth != 2 || r.ByPane["%3"].Depth != 23 {
		t.Errorf("depths: %+v", r.ByPane)
	}
}

// TestSidmap_DepthCap25NoPane (B6): one more process than the bound gives no
// pane at all, never a wrong one.
func TestSidmap_DepthCap25NoPane(t *testing.T) {
	var w world
	w.pane(300, "s:1.1", "%3")
	deep := w.chain(300, 301, 24)
	w.register(deep, sidC)
	if r := w.run(); len(r.ByPane) != 0 {
		t.Fatalf("a 25-process chain mapped: %+v", r.ByPane)
	}
}

// TestSidmap_ShallowestWins (B7): a nested agent's Claude (deeper, and with
// the LOWER pid, so the pid tie-break cannot be what decides) never shadows
// the pane's own session.
func TestSidmap_ShallowestWins(t *testing.T) {
	var w world
	w.pane(500, "s:1.1", "%5")
	w.proc(590, 500, "claude")           // the pane's own session, depth 1
	w.proc(510, 590, "node agent.js")    // a tool it runs
	w.proc(505, 510, "claude -p review") // the nested agent's Claude, depth 3
	w.register(505, sidB)
	w.register(590, sidA)
	r := w.run()
	if got := sidOf(r, "%5"); got != sidA {
		t.Fatalf("pane got %q, want the shallow %q", got, sidA)
	}
}

// TestSidmap_MCPHelperPathYieldsNoPane (B8): a Claude spawned UNDER an MCP
// helper belongs to the helper, not to the pane hosting it. Every helper
// shape: --mcp, a bare `mcp` verb, and an mcp-server file in exec position.
func TestSidmap_MCPHelperPathYieldsNoPane(t *testing.T) {
	for _, helper := range []string{
		"node /home/user/.bun/bin/claudish --mcp",
		"railway mcp",
		"python3 /home/user/tools/mcp-server.py",
		"/usr/local/bin/mcp_server_git --repo x",
	} {
		var w world
		w.pane(700, "s:1.1", "%7")
		w.proc(701, 700, "claude")
		w.proc(702, 701, helper)
		w.proc(703, 702, "claude --print hi")
		w.register(703, sidB)
		r := w.run()
		if len(r.ByPane) != 0 {
			t.Errorf("helper %q: the helper's session mapped to the pane: %+v", helper, r.ByPane)
		}
		w.register(701, sidA)
		if got := sidOf(w.run(), "%7"); got != sidA {
			t.Errorf("helper %q: the pane's own session %q lost (got %q)", helper, sidA, got)
		}
	}
}

// TestSidmap_MCPConfigIsNotAHelper (R32): a substring is not an invocation.
func TestSidmap_MCPConfigIsNotAHelper(t *testing.T) {
	for _, cmd := range []string{
		"claude --mcp-config /home/user/.config/mcp-servers.json",
		"claude --add-dir /home/user/src/mcp-server",
		"node /home/user/.bun/bin/claudish -d --mcp-config x",
	} {
		if IsMCPHelper(cmd) {
			t.Errorf("IsMCPHelper(%q) = true", cmd)
		}
		var w world
		w.pane(800, "s:1.1", "%8")
		w.proc(801, 800, cmd)
		w.register(801, sidA)
		if got := sidOf(w.run(), "%8"); got != sidA {
			t.Errorf("%q: sid %q", cmd, got)
		}
	}
}

// TestSidmap_RegistryBeatsScrape (B9): the registry wins a pane even when a
// scraped --resume sits SHALLOWER on it; the scrape fills only empty panes.
func TestSidmap_RegistryBeatsScrape(t *testing.T) {
	var w world
	w.pane(100, "s:1.1", "%1")
	w.proc(101, 100, "claude --resume "+sidB) // scraped, depth 1
	w.proc(102, 101, "node x")
	w.proc(103, 102, "claude") // registered, depth 3
	w.register(103, sidA)

	w.pane(200, "s:1.2", "%2")
	w.proc(201, 200, "claude -r "+sidC) // scrape only
	w.pane(300, "s:1.3", "%3")
	w.proc(301, 300, "claude --session-id "+sidD)

	r := w.run()
	if got := sidOf(r, "%1"); got != sidA {
		t.Errorf("%%1 = %q, want the registry's %q", got, sidA)
	}
	if got := sidOf(r, "%2"); got != sidC {
		t.Errorf("%%2 = %q, want the scraped %q", got, sidC)
	}
	if got := sidOf(r, "%3"); got != sidD {
		t.Errorf("%%3 = %q, want the scraped %q", got, sidD)
	}
}

// TestSidmap_OneSessionOnePane_StableAcrossSaves (B10, R8): two panes with
// one sid — the lower sorted target keeps it, every time, whatever order the
// inputs arrive in; the other is reported as a duplicate.
func TestSidmap_OneSessionOnePane_StableAcrossSaves(t *testing.T) {
	var first []string
	for i := 0; i < 20; i++ {
		var w world
		targets := []struct {
			pid    int
			target string
			id     string
		}{{400, "apollo:2.1", "%40"}, {410, "apollo:1.1", "%41"}, {420, "chronos:1.2", "%42"}}
		if i%2 == 1 {
			targets[0], targets[2] = targets[2], targets[0]
		}
		for _, p := range targets {
			w.pane(p.pid, p.target, p.id)
			w.proc(p.pid+1, p.pid, "claude --resume "+sidA)
			w.register(p.pid+1, sidA)
		}
		r := w.run()
		if got := sidOf(r, "%41"); got != sidA || len(r.ByPane) != 1 || len(r.Dups) != 2 {
			t.Fatalf("run %d: %+v dups %+v", i, r.ByPane, r.Dups)
		}
		for _, d := range r.Dups {
			if d.Owner != "apollo:1.1" || d.SID != sidA {
				t.Fatalf("dup %+v", d)
			}
		}
		if first == nil {
			first = r.Lines()
		} else if !reflect.DeepEqual(first, r.Lines()) {
			t.Fatalf("run %d differs:\n%v\n%v", i, first, r.Lines())
		}
	}
}

func TestSidmap_ClaudishLauncherOnPath(t *testing.T) {
	var w world
	w.pane(100, "s:1.1", "%1")
	w.proc(101, 100, "node /home/user/.bun/bin/claudish -d")
	w.proc(102, 101, "node /home/user/.bun/lib/claudish/dist/index.js -d")
	w.proc(103, 102, "claude --dangerously-skip-permissions")
	w.register(103, sidA)
	e := w.run().ByPane["%1"]
	if e.SID != sidA || e.ClaudishPid != 101 || e.Pid != 103 {
		t.Fatalf("%+v", e)
	}
}

func TestExecToken(t *testing.T) {
	for cmd, want := range map[string]string{
		"op run --env x -- claude --x":            "claude",
		"node /home/user/.bun/bin/claudish -d":    "claudish",
		"/opt/homebrew/bin/nvim foo":              "nvim",
		"env FOO=1 python3 x.py":                  "FOO=1",
		"bash -c 'pnpm build'":                    "-c",
		"":                                        "",
		"/usr/bin/env node /x/claudish --model m": "claudish",
	} {
		if got := ExecBase(cmd); got != want {
			t.Errorf("ExecBase(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// parity with the shipped awk (K8)
// ---------------------------------------------------------------------------

// awkPath is the vendored plugin's copy — the one file the popup and freeze
// run (K8). There is no fallback copy: a second copy is a second file that can
// drift from the shipped one (P3's testdata copy did, and kept an un-redacted
// comment, §9.6), and parity against it would prove nothing about what ships.
func awkPath(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Join(filepath.Dir(here), "..", "..", "..", "..")
	vendored := filepath.Join(moduleRoot, "share", "moox", "plugins", "tmux-claude-continuity", "scripts", "lib", "cc_sidmap.awk")
	if _, err := os.Stat(vendored); err != nil {
		t.Fatalf("the vendored cc_sidmap.awk is the parity source and must exist: %v", err)
	}
	return vendored
}

func runAwk(t *testing.T, awkFile string, w world) []string {
	t.Helper()
	dir := t.TempDir()
	var pt, ps strings.Builder
	for _, p := range w.panes {
		fmt.Fprintf(&pt, "%d\t%s\t%s\n", p.Pid, p.Target, p.ID)
	}
	for _, p := range w.procs {
		fmt.Fprintf(&ps, "%5d %5d %s\n", p.Pid, p.PPid, p.Command)
	}
	panes, pstab := filepath.Join(dir, "panes"), filepath.Join(dir, "ps")
	byPid := filepath.Join(dir, "by-pid")
	if err := os.Mkdir(byPid, 0o700); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(panes, []byte(pt.String()), 0o600))
	must(os.WriteFile(pstab, []byte(ps.String()), 0o600))
	args := []string{"-v", "panes=" + panes, "-v", "pstab=" + pstab, "-f", awkFile, panes, pstab}
	for pid, sid := range w.reg {
		f := filepath.Join(byPid, fmt.Sprintf("%d.session-id", pid))
		must(os.WriteFile(f, []byte(sid+"\ntitle line\n"), 0o600))
		args = append(args, f)
	}
	out, err := exec.Command("awk", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("awk: %v\n%s", err, out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return sortStrings(lines)
}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// parityWorlds are fixtures with no equal-depth ties on one pane (where the
// awk's answer depends on its hash order).
func parityWorlds() map[string]world {
	ws := map[string]world{}

	var a world // depth 0 / 2 / 23, claudish on path, a 25-chain
	a.panes = append(a.panes, Pane{Pid: 100, Target: "s:1.1", ID: "%1"})
	a.proc(100, 900, "claude")
	a.register(100, sidA)
	a.pane(200, "s:1.2", "%2")
	a.proc(201, 200, "op run -- claude")
	a.proc(202, 201, "claude --dangerously-skip-permissions")
	a.register(202, sidB)
	a.pane(300, "s:2.1", "%3")
	a.register(a.chain(300, 301, 23), sidC)
	a.pane(400, "s:2.2", "%4")
	a.register(a.chain(400, 401, 24), sidD)
	a.pane(500, "w:1.1", "%5")
	a.proc(501, 500, "node /home/user/.bun/bin/claudish -d --model x")
	a.proc(502, 501, "claude")
	a.register(502, "eeeeeeee-0000-4000-8000-000000000005")
	ws["depths"] = a

	var b world // helpers, mcp-config, shallow vs deep, registry vs scrape
	b.pane(100, "a:1.1", "%1")
	b.proc(101, 100, "claude --mcp-config /home/user/.config/mcp-servers.json")
	b.proc(102, 101, "node /home/user/.bun/bin/claudish --mcp")
	b.proc(103, 102, "claude -p x")
	b.register(101, sidA)
	b.register(103, sidB)
	b.pane(200, "a:1.2", "%2")
	b.proc(201, 200, "claude --resume "+sidD)
	b.proc(202, 201, "bun x")
	b.proc(203, 202, "claude")
	b.register(203, sidC)
	b.pane(300, "a:2.1", "%3")
	b.proc(301, 300, "python3 /x/mcp-server.py")
	b.proc(302, 301, "claude")
	b.register(302, "ffffffff-0000-4000-8000-000000000006")
	b.pane(400, "a:2.2", "%4")
	b.proc(401, 400, "claude --session-id 11111111-0000-4000-8000-000000000007")
	b.pane(450, "a:2.3", "%9")
	b.proc(451, 450, "claude")
	b.proc(452, 451, "node t")
	b.proc(453, 452, "claude")
	b.register(453, "22222222-0000-4000-8000-000000000008") // deep
	b.register(451, "33333333-0000-4000-8000-000000000009") // shallow
	ws["helpers"] = b

	var c world // duplicates
	for i, tgt := range []string{"zeta:1.1", "alpha:3.2", "alpha:10.1", "mid:1.1"} {
		pid := 100 * (i + 1)
		c.pane(pid, tgt, fmt.Sprintf("%%%d", 10+i))
		c.proc(pid+1, pid, "claude")
		c.register(pid+1, sidA)
	}
	c.pane(600, "solo:1.1", "%20")
	c.proc(601, 600, "claude")
	c.register(601, sidB)
	ws["duplicates"] = c
	return ws
}

// TestSidmap_ParityWithAwk (B6–B10, K8): the Go climb and the shipped awk give
// the same lines over the same ps / panes / by-pid fixtures.
func TestSidmap_ParityWithAwk(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	awkFile := awkPath(t)
	for name, w := range parityWorlds() {
		t.Run(name, func(t *testing.T) {
			want := runAwk(t, awkFile, w)
			got := w.run().Lines()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go and awk differ\n  go: %q\n awk: %q", got, want)
			}
			if len(got) == 0 {
				t.Fatal("empty fixture output proves nothing")
			}
		})
	}
}
