package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Boot results the gate writes to @moox-boot-result itself (the restore
// writes PASS / INCOMPLETE / NOTHING).
const (
	BootHandover        = "HANDOVER"
	BootSkippedPrefix   = "SKIPPED:"
	envNoAutoRestore    = "MOOX_NO_AUTO_RESTORE"
	envNoAutoRestoreOld = "TT_NO_AUTO_RESTORE"
)

// GateDecision is BootGate's answer (§6.4 checks 2–6; check 1, the socket
// match, is the caller's: `tt serve --ensure` only calls BootGate on a match).
type GateDecision struct {
	// Cold: the server had 0 sessions when the gate ran (E4, Q1).
	Cold bool
	// Restore: every check passed; the daemon is forked with a boot restore.
	Restore bool
	// Result is what the gate wrote to @moox-boot-result ("" = nothing).
	Result string
	// Reason is one word for the log.
	Reason    string
	StartTime int64
	Purge     PurgeResult
	// Snapshot is the snapshot the boot restore would read (Last()), or the
	// resurrect file it would import (Import).
	Snapshot string
	// Import: no valid moox snapshot; the daemon publishes the import of
	// resurrect's `last` before restoring it (D12).
	Import bool
	// Recovered is a leftover restoring-from turned into a pin.
	Recovered string
}

// Line is the gate's log line.
func (g GateDecision) Line() string {
	return fmt.Sprintf("BOOT-GATE cold=%v restore=%v result=%q reason=%s start_time=%d purged pending=%d launch=%d snapshot=%s",
		g.Cold, g.Restore, g.Result, g.Reason, g.StartTime, g.Purge.Pending, g.Purge.Launch, g.Snapshot)
}

// BootGate runs in `tt serve --ensure`, synchronously, during config load
// (B27, G1–G4, RR5, R19). sessions is the server's session count as the
// caller read it: cold is exactly 0 (E4). On a cold server BOTH purges run
// whatever the later checks decide (PR-C11, R2-M12).
func (e *Engine) BootGate(ctx context.Context, sessions int) (GateDecision, error) {
	var g GateDecision
	if sessions != 0 {
		g.Reason = "not-cold"
		return g, nil
	}
	g.Cold = true
	out, err := e.tmux.Run(ctx, tmuxtarget.Chain(
		[]string{"display-message", "-p", "#{start_time}" + optSep + "#{@moox-boot-result}"},
		[]string{"show-environment", "-g"},
	)...)
	lines := strings.Split(string(out), "\n")
	var bootResult string
	if len(lines) > 0 {
		f := strings.SplitN(lines[0], optSep, 2)
		if n, perr := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64); perr == nil && n > 0 {
			g.StartTime = n
		}
		if len(f) == 2 {
			bootResult = strings.TrimSpace(f[1])
		}
	}
	handover := false
	for _, l := range lines[1:] {
		k, _, ok := strings.Cut(l, "=")
		if ok && (k == envNoAutoRestore || k == envNoAutoRestoreOld) {
			handover = true
		}
	}

	// 3. Both purges, before any pane exists, whatever 4–6 decide.
	g.Purge, _ = e.Purge(ctx, PurgeRequest{Pending: true, Launch: true, StartTime: g.StartTime})
	if err != nil {
		e.logf("BOOT-GATE read: %v (start_time unknown: launch/ kept)", err)
	}
	// Crash safety: a leftover restoring-from becomes a pin (§6.2).
	if st, serr := store.Open(e.paths, e.claudeRoot); serr == nil {
		if l, lerr := st.TryLock(e.record()); lerr == nil {
			if src, ok, rerr := st.RecoverRestoringFrom(l); ok {
				g.Recovered = src
				e.logf("PIN %s: its restore died before the verdict (err=%v)", src, rerr)
			}
			l.Release()
		}
		_ = st.Close()
	}

	set := func(result, reason string, extra ...[]string) (GateDecision, error) {
		g.Result, g.Reason = result, reason
		cmds := append(extra, []string{"set-option", "-g", "@moox-boot-result", result})
		if _, err := e.tmux.Run(ctx, tmuxtarget.Chain(cmds...)...); err != nil {
			return g, fmt.Errorf("engine: gate result: %w", err)
		}
		return g, nil
	}
	// 4. Handover (G3, U3): HANDOVER lives in @moox-boot-result only, never
	// in @moox-restore-result (PR-M3).
	if handover {
		return set(BootHandover, "handover",
			[]string{"set-environment", "-gu", envNoAutoRestore},
			[]string{"set-environment", "-gu", envNoAutoRestoreOld})
	}
	// 6. A boot result already set on this server life.
	if bootResult != "" {
		g.Reason = "boot-result-set"
		return g, nil
	}
	// 5. Last() valid with more than one window (G4, RR11).
	last, err := e.Last()
	switch {
	case errors.Is(err, ErrNewerFormat):
		// Never import over a newer moox's workspace (G1-H4).
		e.logf("BOOT-GATE %v", err)
		return set(BootSkippedPrefix+"newer-format", "newer-format")
	case err != nil:
		// …or an importable resurrect `last` whose in-memory import
		// validates with more than one window (PR-M4, D12). The daemon
		// publishes it before restoring (Restore, bootimport.go).
		im, ok := e.importAvailable(ctx)
		switch {
		case !ok:
			return set(BootSkippedPrefix+"no-snapshot", "no-snapshot")
		case len(im.Snapshot.Windows) <= 1:
			g.Snapshot = im.Report.SourceFile
			return set(BootSkippedPrefix+"one-window", "one-window")
		}
		g.Snapshot, g.Import = im.Report.SourceFile, true
		g.Restore, g.Reason = true, "restore-import"
		return g, nil
	case len(last.Snapshot.Windows) <= 1:
		g.Snapshot = last.Path
		return set(BootSkippedPrefix+"one-window", "one-window")
	}
	g.Snapshot = last.Path
	g.Restore, g.Reason = true, "restore"
	return g, nil
}

// SkipBoot records why a decided boot restore did not run (R2-M3: no
// bootstrap within the deadline; R3-L2: the lock wait timed out) and
// releases @moox-restoring if it still holds value.
func (e *Engine) SkipBoot(ctx context.Context, reason, restoring string) {
	cmds := [][]string{{"set-option", "-g", "@moox-boot-result", BootSkippedPrefix + reason}}
	if restoring != "" {
		cmds = append(cmds, capture.ClearRestoringArgs(restoring))
	}
	if _, err := e.tmux.Run(ctx, tmuxtarget.Chain(cmds...)...); err != nil {
		e.logf("BOOT-SKIP %s: %v", reason, err)
	}
	e.logf("BOOT-SKIPPED %s", reason)
}

// WaitBootstrap is the daemon's bounded poll for the bootstrap session
// (§6.4): `list-sessions` every interval until one exists or within passes.
// Each wait is one one-shot timer armed by the poll before it.
func (e *Engine) WaitBootstrap(ctx context.Context, interval, within time.Duration) bool {
	dctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		if out, err := e.tmux.Run(dctx, "list-sessions", "-F", "#{session_id}"); err == nil && strings.TrimSpace(string(out)) != "" {
			return true
		}
		if !e.wait(dctx, interval) {
			return false
		}
	}
}

// ---------------------------------------------------------------------------
// Purge (B28, R15, R16, R18, X2, X3): per socket only
// ---------------------------------------------------------------------------

// PurgeRequest selects the parts.
type PurgeRequest struct {
	Pending bool
	Launch  bool
	// StartTime is the server's #{start_time}; 0 = unknown, and then no
	// launch recording is deleted (the opposite of the old blanket delete).
	StartTime int64
}

// PurgeResult counts what was removed.
type PurgeResult struct{ Pending, Launch int }

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Purge deletes this socket's stale pending files (every all-digit name: on
// a server with no panes every one is stale) and launch recordings older than
// the server's start. Other servers' directories are never opened: the paths
// differ by key.
func (e *Engine) Purge(_ context.Context, r PurgeRequest) (PurgeResult, error) {
	var res PurgeResult
	var errs []error
	sweep := func(dir string, keep func(statedir.Info) bool) int {
		d, err := statedir.OpenExisting(dir, e.claudeRoot)
		if errors.Is(err, fs.ErrNotExist) {
			return 0
		}
		if err != nil {
			errs = append(errs, err)
			return 0
		}
		defer func() { _ = d.Close() }()
		names, err := d.ReadDirNames()
		if err != nil {
			errs = append(errs, err)
			return 0
		}
		n := 0
		for _, name := range names {
			if !allDigits(name) {
				continue
			}
			info, err := d.Lstat(name)
			if err != nil || info.Type != statedir.TypeRegular || keep(info) {
				continue
			}
			if err := d.Remove(name, statedir.TypeRegular, &info); err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
		return n
	}
	if r.Pending {
		res.Pending = sweep(e.paths.Pending, func(statedir.Info) bool { return false })
	}
	if r.Launch && r.StartTime > 0 {
		start := time.Unix(r.StartTime, 0)
		res.Launch = sweep(e.paths.Launch, func(i statedir.Info) bool { return !i.ModTime.Before(start) })
	}
	if res.Pending > 0 || res.Launch > 0 {
		e.logf("PURGE pending=%d launch=%d (start_time=%d)", res.Pending, res.Launch, r.StartTime)
	}
	return res, errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// ManualPreconditions (moox restore, the tt TUI offer; B47, R28)
// ---------------------------------------------------------------------------

// ErrPreconditions is a manual restore refused (exit 3 with the reason).
var ErrPreconditions = errors.New("engine: restore refused")

// ManualPreconditions: the socket matches (@moox-active 1); no restore is
// running; the server holds at most one session with one window and one
// pane, idle at a shell prompt: the shell owns its terminal's foreground
// (U2/U4; background helpers in their own process group do not count). A
// populated server is never restored into (B47). @moox-boot-result is not
// consulted.
func (e *Engine) ManualPreconditions(ctx context.Context) error {
	out, err := e.tmux.Run(ctx, "display-message", "-p", "#{@moox-active}"+optSep+"#{@moox-restoring}")
	if err != nil {
		return fmt.Errorf("engine: preconditions read: %w", err)
	}
	f := strings.SplitN(strings.TrimRight(string(out), "\n"), optSep, 2)
	if len(f) != 2 || f[0] != "1" {
		return ErrNotActive
	}
	if rs, set, valid := capture.ParseRestoring(f[1]); set && (!valid || e.procs.Alive(rs.Pid)) {
		return fmt.Errorf("%w: busy-restoring (%s)", ErrRestoring, f[1])
	}
	panes, err := e.tmux.Run(ctx, "list-panes", "-a", "-F", "#{session_id} #{window_id} #{pane_id} #{pane_pid} #{pane_current_command}")
	if err != nil && !isNoSessions(err) {
		return fmt.Errorf("engine: preconditions list-panes: %w", err)
	}
	sessions, windows := map[string]bool{}, map[string]bool{}
	type pane struct {
		pid int
		cmd string
	}
	var ps []pane
	for _, line := range nonEmptyLines(string(panes)) {
		w := strings.SplitN(line, " ", 5)
		if len(w) < 4 {
			continue
		}
		sessions[w[0]], windows[w[1]] = true, true
		pid, _ := strconv.Atoi(w[3])
		cmd := ""
		if len(w) == 5 {
			cmd = w[4]
		}
		ps = append(ps, pane{pid, cmd})
	}
	if len(sessions) > 1 || len(windows) > 1 || len(ps) > 1 {
		return fmt.Errorf("%w (%d sessions, %d windows, %d panes): %w", ErrPreconditions, len(sessions), len(windows), len(ps), ErrPopulated)
	}
	if len(ps) == 1 {
		return e.idleShell(ctx, ps[0].pid, ps[0].cmd)
	}
	return nil
}

// idleShell: the one pane of a would-be bootstrap sits at a shell prompt —
// its #{pane_current_command} is a shell, and the shell owns its terminal's
// foreground. ManualPreconditions runs it before the lock wait, and a manual
// restore again under save.lock (G1-2-H1), since a job started during the
// wait keeps the bootstrap's shape.
//
// The foreground, not "the shell has no child": an idle zsh keeps helpers
// as children — powerlevel10k's gitstatusd, async prompt workers — in
// process groups of their own, and those must not make a fresh bootstrap
// look populated. Anything the user runs at the prompt (claude, vim, a
// command) is the terminal's foreground process group, so it still refuses.
func (e *Engine) idleShell(ctx context.Context, pid int, cmd string) error {
	if !verdict.IsShell(cmd) {
		return fmt.Errorf("%w: the one pane runs %q, not a shell: %w", ErrPreconditions, cmd, ErrPopulated)
	}
	out, err := e.procs.Jobs(ctx)
	if err != nil {
		return fmt.Errorf("engine: preconditions ps: %w", err)
	}
	if busy := shellForegroundBusy(capture.ParseJobs(out), pid); busy != "" {
		return fmt.Errorf("%w: the one pane's shell is not at its prompt (%s): %w", ErrPreconditions, busy, ErrPopulated)
	}
	return nil
}

// shellForegroundBusy says why the shell with pid shell does not own its
// terminal's foreground, or "" when it does:
//   - the terminal's foreground process group is another group (a job
//     control shell runs every foreground job in a group of its own);
//   - a descendant shares the shell's own group (a shell without job control
//     runs its foreground commands in its group);
//   - a descendant is stopped (a job the user suspended with ^Z: the shell is
//     back at its prompt, but the job would die with the bootstrap).
//
// Other descendants in other groups are background helpers and do not count.
//
// When the shell's row or its terminal is missing, it falls back to the
// stricter old rule: any child refuses.
//
// It is verdict.ForegroundBusy, the one idle-at-prompt rule the LAUNCH
// verdict shares, over the job-control table.
func shellForegroundBusy(jobs []capture.Job, shell int) string {
	return verdict.ForegroundBusy(jobProcs(nil, jobs), shell)
}

// jobProcs merges the job-control rows into procs (matched by pid; a row only
// in jobs is added) — the shape verdict.ForegroundBusy reads.
func jobProcs(procs []verdict.Proc, jobs []capture.Job) []verdict.Proc {
	at := make(map[int]int, len(procs))
	for i, p := range procs {
		at[p.Pid] = i
	}
	for _, j := range jobs {
		i, ok := at[j.Pid]
		if !ok {
			procs = append(procs, verdict.Proc{Pid: j.Pid, PPid: j.PPid})
			i = len(procs) - 1
			at[j.Pid] = i
		}
		procs[i].Job, procs[i].Pgid, procs[i].TPgid, procs[i].Stopped = true, j.Pgid, j.TPgid, j.Stopped
	}
	return procs
}

// ClearRestoringArgs is the conditional unset of @moox-restoring: it clears
// the option only while it still holds value, so a value set meanwhile by a
// new restore survives (§5.4). One tmux exec.
func ClearRestoringArgs(value string) []string { return capture.ClearRestoringArgs(value) }

// RestoringOwner parses an @moox-restoring value ("<kind>:<pid>:<epoch>");
// ok is false for an empty or unparsable one.
func RestoringOwner(value string) (kind string, pid int, ok bool) {
	rs, set, valid := capture.ParseRestoring(value)
	if !set || !valid {
		return "", 0, false
	}
	return rs.Kind, rs.Pid, true
}
