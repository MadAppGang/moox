// Package sidmap is the pane → Claude session id climb (B6–B10), a Go port
// of the vendored plugin's scripts/lib/cc_sidmap.awk. The awk stays in use by
// the popup and freeze, so TestSidmap_ParityWithAwk runs both over the same
// fixtures (K8).
//
// The rule, in one place:
//
//   - From each REGISTERED Claude pid (the by-pid registry), climb UP the
//     process tree to a pane_pid, at most 24 hops (B6). One ps table.
//   - A path that crosses an MCP helper yields no pane (B8): the session
//     belongs to the helper, not to the pane hosting it. A helper is keyed off
//     HOW it was invoked (`--mcp`, ` mcp `, or an mcp-server/mcp_server file in
//     exec position), never a substring: `claude --mcp-config …/mcp-servers.json`
//     is an ordinary Claude (R32).
//   - The shallowest claim wins a pane (B7), so a nested agent's Claude cannot
//     shadow the pane's own session.
//   - The registry beats the `--resume|-r|--session-id <uuid>` argv scrape; the
//     scrape is used only for panes the registry left empty (B9).
//   - One session, one pane: when two panes hold the same sid, the lowest
//     sorted S:W.P keeps it and the others are reported as duplicates (B10,
//     R8), stable across saves.
//
// Two deliberate differences from the awk, both in cases where the awk's
// answer depends on its hash iteration order (so it has no defined answer):
// equal-depth registry claims on one pane go to the LOWER pid, and among
// several scraped candidates for one empty pane the shallowest wins (lowest
// pid on a tie). The parity fixtures avoid those ties.
package sidmap

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxHops is the climb bound (cc_sidmap.awk: `for (i = 0; i < 24; i++)`).
const MaxHops = 24

// Pane is one live pane: its process, its S:W.P target and its id.
type Pane struct {
	Pid    int
	Target string
	ID     string
}

// Proc is one row of the ps table.
type Proc struct {
	Pid, PPid int
	Command   string
}

// Entry is a pane's elected session.
type Entry struct {
	Target string
	PaneID string
	SID    string
	Pid    int // the winning (Claude) pid
	// ClaudishPid is the first claudish LAUNCHER on the climb path, 0 if none.
	ClaudishPid int
	Depth       int
}

// Dup is a pane whose sid another pane already owns (B10).
type Dup struct {
	Target, PaneID, SID, Owner string
}

// Result is the climb's output.
type Result struct {
	ByPane map[string]Entry // keyed by pane id
	Dups   []Dup
}

// table is the ps snapshot as the climb needs it.
type table struct {
	parent    map[int]int
	mcp       map[int]bool
	claudish  map[int]bool
	scraped   map[int]string
	paneByPid map[int]Pane
}

var uuidish = regexp.MustCompile(`^[0-9a-fA-F-]+$`)

// interpreters are skipped when looking for the exec token.
var interpreters = map[string]bool{"node": true, "bun": true, "deno": true, "npx": true, "env": true,
	"python": true, "python3": true, "ruby": true, "perl": true, "sh": true, "bash": true, "zsh": true}

// ExecBase is the basename of a command line's EXEC TOKEN (cc_sidmap.awk
// execbase, cc_proc.sh _cc_exec_token): the token after a standalone `--`,
// else the first token that is not an interpreter, else "".
func ExecBase(cmd string) string {
	tok := ExecToken(cmd)
	if tok == "" {
		return ""
	}
	return path.Base(tok)
}

// ExecToken is the exec token itself (with its path), or "".
func ExecToken(cmd string) string {
	exe, take := "", false
	for _, tok := range strings.Fields(cmd) {
		if take {
			return tok
		}
		if tok == "--" {
			take = true
			continue
		}
		if exe == "" {
			if interpreters[path.Base(tok)] {
				continue
			}
			exe = tok
		}
	}
	return exe
}

// IsMCPHelper is the awk's helper test for one ps row's command.
func IsMCPHelper(cmd string) bool {
	line := " " + cmd
	if hasWord(line, " --mcp", " =") || hasWord(line, " mcp", " ") {
		return true
	}
	if b := ExecBase(cmd); b == "claude" || b == "claudish" {
		return false
	}
	toks := strings.Fields(cmd)
	for _, tok := range toks {
		b := tok
		if i := strings.LastIndexByte(b, '/'); i >= 0 {
			b = b[i+1:]
		}
		if strings.HasPrefix(b, "mcp-server") || strings.HasPrefix(b, "mcp_server") {
			return true
		}
	}
	return false
}

// hasWord reports whether line contains needle followed by one of the bytes
// in follow, or by the end of the line (the awk's /[ ]--mcp([ =]|$)/).
func hasWord(line, needle, follow string) bool {
	for i := 0; ; {
		j := strings.Index(line[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end == len(line) || strings.IndexByte(follow, line[end]) >= 0 {
			return true
		}
		i = i + j + 1
	}
}

// isClaudishLauncher is `$0 ~ /\/claudish([ ]|$)/`.
func isClaudishLauncher(cmd string) bool { return hasWord(cmd, "/claudish", " ") }

// scrape is the awk's SOURCE 2: `--resume|-r|--session-id <36-char uuid>`,
// with the awk's exact token handling (a `--resume=<x>` token's value is
// taken from the NEXT token, as the awk does).
func scrape(cmd string) string {
	f := strings.Fields(cmd)
	for i := 0; i+1 < len(f); i++ {
		w := f[i]
		if k := strings.IndexByte(w, '='); k >= 0 {
			w = w[:k]
		}
		if w == "--resume" || w == "-r" || w == "--session-id" {
			u := f[i+1]
			if k := strings.LastIndexByte(u, '='); k >= 0 {
				u = u[k+1:]
			}
			if len(u) == 36 && uuidish.MatchString(u) {
				return u
			}
		}
	}
	return ""
}

func newTable(panes []Pane, procs []Proc) *table {
	t := &table{parent: map[int]int{}, mcp: map[int]bool{}, claudish: map[int]bool{},
		scraped: map[int]string{}, paneByPid: map[int]Pane{}}
	for _, p := range panes {
		if p.Pid > 0 {
			t.paneByPid[p.Pid] = p // a grouped pane listed twice: the last wins, as in the awk
		}
	}
	for _, p := range procs {
		if p.Pid <= 0 {
			continue
		}
		t.parent[p.Pid] = p.PPid
		if IsMCPHelper(p.Command) {
			t.mcp[p.Pid] = true
		} else if isClaudishLauncher(p.Command) {
			t.claudish[p.Pid] = true
		}
		if u := scrape(p.Command); u != "" {
			t.scraped[p.Pid] = u
		}
	}
	return t
}

// climb walks up from pid: the pane pid it lands on (0 if none, or if the
// path crosses an MCP helper), the depth, and the first claudish launcher.
func (t *table) climb(pid int) (pane, depth, claudish int) {
	cur := pid
	for i := 0; i < MaxHops; i++ {
		if t.mcp[cur] {
			return 0, -1, 0
		}
		if _, ok := t.paneByPid[cur]; ok {
			return cur, i, claudish
		}
		if t.claudish[cur] && claudish == 0 {
			claudish = cur
		}
		par, ok := t.parent[cur]
		if !ok || par == cur || par == 0 || par == 1 {
			return 0, -1, 0
		}
		cur = par
	}
	return 0, -1, 0
}

type claim struct {
	panePid, depth, pid, claudish int
	sid                           string
}

// better: shallower wins; on equal depth the lower pid (deterministic).
func better(a, b claim) bool {
	if a.depth != b.depth {
		return a.depth < b.depth
	}
	return a.pid < b.pid
}

// Map runs the climb. registered maps a Claude pid to its session id (line 1
// of <pid>.session-id); entries the caller already judged dead are absent.
func Map(panes []Pane, procs []Proc, registered map[int]string) Result {
	t := newTable(panes, procs)
	best := map[int]claim{} // by pane pid

	pids := make([]int, 0, len(registered))
	for pid := range registered {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		sid := registered[pid]
		if sid == "" {
			continue
		}
		pp, d, cl := t.climb(pid)
		if pp == 0 {
			continue
		}
		c := claim{panePid: pp, depth: d, pid: pid, claudish: cl, sid: sid}
		if old, ok := best[pp]; !ok || better(c, old) {
			best[pp] = c
		}
	}
	// SOURCE 2, only for panes the registry left empty.
	fromRegistry := map[int]bool{}
	for pp := range best {
		fromRegistry[pp] = true
	}
	spids := make([]int, 0, len(t.scraped))
	for pid := range t.scraped {
		spids = append(spids, pid)
	}
	sort.Ints(spids)
	for _, pid := range spids {
		pp, d, cl := t.climb(pid)
		if pp == 0 || fromRegistry[pp] {
			continue
		}
		c := claim{panePid: pp, depth: d, pid: pid, claudish: cl, sid: t.scraped[pid]}
		if old, ok := best[pp]; !ok || better(c, old) {
			best[pp] = c
		}
	}

	// One session, one pane: by sorted target.
	type row struct {
		pane Pane
		c    claim
	}
	rows := make([]row, 0, len(best))
	for pp, c := range best {
		rows = append(rows, row{t.paneByPid[pp], c})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pane.Target < rows[j].pane.Target })
	res := Result{ByPane: map[string]Entry{}}
	owner := map[string]string{}
	for _, r := range rows {
		if o, ok := owner[r.c.sid]; ok {
			res.Dups = append(res.Dups, Dup{Target: r.pane.Target, PaneID: r.pane.ID, SID: r.c.sid, Owner: o})
			continue
		}
		owner[r.c.sid] = r.pane.Target
		res.ByPane[r.pane.ID] = Entry{Target: r.pane.Target, PaneID: r.pane.ID, SID: r.c.sid, Pid: r.c.pid,
			ClaudishPid: r.c.claudish, Depth: r.c.depth}
	}
	return res
}

// Lines renders a Result in cc_sidmap.awk's output format, sorted by target:
//
//	<S:W.P>\t<pane id>\t<sid>\t<winning pid>\t<claudish pid or "">
//	<S:W.P>\t<pane id>\t;DUP=<sid>@<owner>
//
// so the parity test can compare the two implementations line for line.
func (r Result) Lines() []string {
	var out []string
	for _, e := range r.ByPane {
		cl := ""
		if e.ClaudishPid != 0 {
			cl = strconv.Itoa(e.ClaudishPid)
		}
		out = append(out, e.Target+"\t"+e.PaneID+"\t"+e.SID+"\t"+strconv.Itoa(e.Pid)+"\t"+cl)
	}
	for _, d := range r.Dups {
		out = append(out, d.Target+"\t"+d.PaneID+"\t;DUP="+d.SID+"@"+d.Owner)
	}
	sort.Strings(out)
	return out
}
