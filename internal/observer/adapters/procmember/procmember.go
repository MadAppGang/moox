// Package procmember is the foreign-server rule's adapter (moox §4.5): it
// answers "does this registered Claude process run inside THIS tmux server"
// from ONE `ps -axo pid=,ppid=,lstart=` per rebuild, and "is this pid still the
// process that registered" from the by-pid registry's start tokens (PR-M16).
//
// Read-only: it runs ps and reads `<by-pid dir>/<pid>.start`. It writes
// nothing, signals nothing and never talks to tmux — the pane pids it climbs
// to are handed in by the pane gate's own listing.
package procmember

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
)

// MaxHops bounds the climb from a registered pid towards a pane pid: the same
// bound and direction as the save's sid climb (B6) and the shell reconciler
// (moox §7), so the three never disagree about who owns a process.
const MaxHops = 24

// PSArgs is the one process-table read. LC_ALL=C is set on the exec so lstart
// is spelled the way the shell's `_cc_start_token` spells it (moox §7).
var PSArgs = []string{"-axo", "pid=,ppid=,lstart="}

// runPS is a seam: tests substitute a fixed table, so no test depends on the
// machine's real process list.
var runPS = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ps", PSArgs...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps %s: %w: %s", strings.Join(PSArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// StartToken turns the lstart text of one process into the token the shell
// writes beside a by-pid entry, `_cc_start_token` in the vendored
// cc_common.sh (moox §7): `LC_ALL=C ps -o lstart= -p <pid> | tr -s
// '[:space:]' '_'` with the leading and trailing '_' removed — so every run of
// blanks inside the instant is one '_' and the padding and newline vanish. An
// empty text is "-", the shell's floor.
func StartToken(lstart string) string {
	f := strings.Fields(lstart)
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, "_")
}

// sameToken compares a recorded token with a computed one. Only the outer '_'
// and whitespace are forgiven (a file written with or without its newline);
// the instant itself must match exactly.
func sameToken(recorded, computed string) bool {
	trim := func(s string) string { return strings.Trim(strings.TrimSpace(s), "_") }
	return trim(recorded) == trim(computed)
}

// Config wires the adapter.
type Config struct {
	// ByPidDir returns the shared by-pid registry directory (moox §4.2, the
	// one rule R3-C2: @claude-continuity-by-pid-dir if set, else
	// $HOME/.config/tmux-claude/panes/by-pid). A func so the composition root
	// can resolve the option lazily, once, only when a membership is asked.
	ByPidDir func() string
}

// Source implements observer.MembershipSource.
type Source struct{ cfg Config }

// New returns the production membership source.
func New(cfg Config) Source { return Source{cfg: cfg} }

type proc struct {
	ppid  int
	token string
}

// Membership takes one process-table read.
func (s Source) Membership(ctx context.Context, panePIDs []int) (observer.Membership, error) {
	out, err := runPS(ctx)
	if err != nil {
		return nil, err
	}
	table := parse(out)
	if len(table) == 0 {
		return nil, errors.New("procmember: ps listed no processes")
	}
	panes := make(map[int]bool, len(panePIDs))
	for _, p := range panePIDs {
		panes[p] = true
	}
	dir := ""
	if s.cfg.ByPidDir != nil {
		dir = s.cfg.ByPidDir()
	}
	return &judge{table: table, panes: panes, byPid: dir}, nil
}

// parse reads `ps -axo pid=,ppid=,lstart=` output. A line that does not start
// with two integers is skipped individually.
func parse(out []byte) map[int]proc {
	table := map[int]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || pid <= 0 {
			continue
		}
		table[pid] = proc{ppid: ppid, token: StartToken(strings.Join(f[2:], " "))}
	}
	return table
}

type judge struct {
	table map[int]proc
	panes map[int]bool
	byPid string
}

// Owns climbs UP from pid, at most MaxHops parents, to one of this server's
// pane pids.
func (j *judge) Owns(pid int) (bool, error) {
	if _, ok := j.table[pid]; !ok {
		// The proc gate called it alive a moment ago; the table does not have
		// it. A race (it just exited) or a ps that hid it: no answer.
		return false, fmt.Errorf("procmember: pid %d is not in the process table", pid)
	}
	cur := pid
	for hop := 0; hop <= MaxHops; hop++ {
		if j.panes[cur] {
			return true, nil
		}
		p, ok := j.table[cur]
		if !ok || p.ppid <= 1 || p.ppid == cur {
			return false, nil
		}
		cur = p.ppid
	}
	return false, nil
}

// Recycled compares the pid's lstart with its by-pid `<pid>.start` token. A
// missing token (an entry written before tokens existed) is trusted.
func (j *judge) Recycled(pid int) bool {
	if j.byPid == "" || pid <= 0 {
		return false
	}
	p, ok := j.table[pid]
	if !ok {
		return false
	}
	data, err := os.ReadFile(filepath.Join(j.byPid, strconv.Itoa(pid)+".start"))
	if err != nil {
		return false
	}
	recorded := string(data)
	if strings.TrimSpace(recorded) == "" {
		return false
	}
	return !sameToken(recorded, p.token)
}

var _ observer.MembershipSource = Source{}
