// Package tmuxpanes answers "does this pane still exist" with one read-only
// `tmux list-panes` call, which is the second of the two gates interactive
// liveness needs (measured fact 8).
//
// # The hard rules this package lives under
//
//   - Read-only subcommands only. `list-panes` queries; it creates nothing and
//     changes nothing.
//   - NEVER `run-shell`, in any form, from any tt process. On 2026-08-16 a
//     run-shell whose client had gone away segfaulted the tmux server and killed
//     16 sessions. Never `kill-*`. Never `split-window` from tt's own process.
//   - This package never STARTS a server. `list-panes` against a machine with no
//     tmux server exits non-zero; the only code in tt that starts a server is
//     restore.go's bootstrapServer, and it stays the only one.
//   - Tests never shell out. runPanes is a package-level variable, following the
//     seam pattern tmux.go already uses (listSessions, killSession, newSession,
//     renameSession), so no test needs a tmux server at all — and scratch tmux
//     servers on this machine have twice cloned the whole workspace.
//   - It asks ONE server: the tmuxtarget.Target it was built with, always with
//     -u -S (moox §4.5). $TMUX and the default socket never decide it.
//
// # The scope is server-wide, deliberately
//
// `-a` is required, not incidental: a session's pane can be in any window, so a
// window-scoped listing would report panes as gone merely because the observer
// was looking at a different window. (§5.5b's pane-singleton guard is the
// opposite case — window-scoped on purpose. Two different questions.)
package tmuxpanes

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// PaneFormat is the exact form pane ids are requested and returned in, sigils
// INCLUDED. The @ and % are part of the window and pane identifiers (measured
// fact 7), and this string is character-for-character what the registry's
// `tmux` field carries — which is what lets the gate compare the two verbatim
// instead of reassembling either.
const PaneFormat = "#{session_name}:#{window_id}.#{pane_id}"

// ListFormat is what is actually requested: the pane id, a TAB, and the pane's
// pid. The pid is for the foreign-server rule (moox §4.5) and costs nothing —
// it is the same one listing. The line is split at its LAST tab, so a session
// name with colons or dots survives untouched.
const ListFormat = PaneFormat + "\t#{pane_pid}"

// ListPanesArgs is the entire tmux command this package will ever run. It is a
// package-level value rather than an inline argument list so that a test can
// assert its contents — "read-only subcommands only" is a rule about an argv,
// and the only way to test a rule about an argv is to be able to read it.
var ListPanesArgs = []string{"list-panes", "-a", "-F", ListFormat}

// runPanes runs the listing and returns its output. It is a package-level
// variable so tests substitute a deterministic fake: no test in this repository
// may start or touch a tmux server, because a scratch server has twice cloned
// the user's whole workspace and destroyed it.
var runPanes = runTmuxListPanes

func runTmuxListPanes(ctx context.Context, t tmuxtarget.Target) ([]byte, error) {
	cmd := t.Command(ctx, ListPanesArgs...)
	// CombinedOutput rather than Output so tmux's own message ("no server
	// running on /tmp/tmux-501/default") reaches the error. That is the
	// difference between a diagnosable degraded gate and a bare exit status 1.
	return cmd.CombinedOutput()
}

// Source implements observer.PanesLister and observer.PanePIDLister for one
// tmux server.
type Source struct{ target tmuxtarget.Target }

// New returns the production pane gate for the server t names.
func New(t tmuxtarget.Target) Source { return Source{target: t} }

// ListPanes returns every pane id the server currently knows.
//
// An error means "we could not ask", and the liveness policy turns that into
// paneAlive: null with a liveness-unknown advisory. That distinction is the
// whole value of this function's error return: an EMPTY pane set returned as a
// success would mark every session's pane as gone and therefore every
// interactive session as dead — the "one failure blanks the whole set" bug this
// repo has already shipped once. So:
//
//   - tmux exits non-zero (including "no server running") -> error.
//   - tmux exits zero with no pane lines at all -> error. A running server
//     always has at least one pane, so this is a malfunction, not an answer.
//   - anything else -> the set, with malformed lines skipped individually.
func (s Source) ListPanes(ctx context.Context) (map[string]bool, error) {
	panes, _, err := s.ListPanePIDs(ctx)
	return panes, err
}

// ListPanePIDs is ListPanes plus every pane's pane_pid, from the same one
// listing (observer.PanePIDLister). A line whose pid will not parse still
// contributes its id; it just names no pid.
func (s Source) ListPanePIDs(ctx context.Context) (map[string]bool, []int, error) {
	out, err := runPanes(ctx, s.target)
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return nil, nil, fmt.Errorf("tmux list-panes: %s: %w", msg, err)
		}
		return nil, nil, fmt.Errorf("tmux list-panes: %w", err)
	}

	panes := make(map[string]bool)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		id, pidText := line, ""
		if i := strings.LastIndexByte(line, '\t'); i >= 0 {
			id, pidText = line[:i], line[i+1:]
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		// A line that does not carry both sigils is not a pane id in the
		// requested format. Skipping it individually is the right degradation:
		// one unexpected line must not discard the other 57.
		if !strings.Contains(id, ":@") || !strings.Contains(id, ".%") {
			continue
		}
		panes[id] = true
		if pid, perr := strconv.Atoi(strings.TrimSpace(pidText)); perr == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}

	if len(panes) == 0 {
		return nil, nil, errors.New("tmux list-panes: server reported no panes")
	}
	return panes, pids, nil
}

var (
	_ observer.PanesLister   = Source{}
	_ observer.PanePIDLister = Source{}
)
