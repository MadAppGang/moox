package engine

import (
	"context"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/statedir"
)

// The LAUNCH verdict's bounded probe chain (§6.5, tt §9.3a stated explicitly,
// PR-L3): a self-re-arming one-shot inside a bounded, event-started probe.
// Each timer is one time.NewTimer; the chain is armed by the restore's
// completion; it disarms itself at the verdict or the deadline; and it is
// never armed while the server is idle, because a restore is not idle.
type launchTiming struct {
	first, every, settle, deadline time.Duration
}

var defaultLaunchTiming = launchTiming{first: 3 * time.Second, every: 3 * time.Second, settle: 15 * time.Second,
	deadline: 180 * time.Second}

type launchRow struct {
	verdict.LaunchRow
	ack string // the ack file's name in ack/
}

// timerFunc is time.NewTimer as a seam: the channel, and a stop.
type timerFunc func(d time.Duration) (<-chan time.Time, func() bool)

func realTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// wait blocks for one one-shot timer or ctx; false means ctx ended first.
func (e *Engine) wait(ctx context.Context, d time.Duration) bool {
	nt := e.newTimer
	if nt == nil {
		nt = realTimer
	}
	c, stop := nt(d)
	select {
	case <-ctx.Done():
		stop()
		return false
	case <-c:
		return true
	}
}

func (e *Engine) launchTimes() launchTiming {
	if e.timing != nil {
		return *e.timing
	}
	return defaultLaunchTiming
}

// probeTimeout bounds one LAUNCH probe (its list-panes and its ps).
const probeTimeout = 10 * time.Second

// launchVerdict judges rows by probes until every row is decided (then one
// settle and a last look) or the deadline passes.
func (e *Engine) launchVerdict(ctx context.Context, rows []launchRow, ackDir string) verdict.Launch {
	lrs := make([]verdict.LaunchRow, len(rows))
	for i, r := range rows {
		lrs[i] = r.LaunchRow
	}
	j := verdict.NewJudge(lrs)
	if len(rows) == 0 {
		return j.Verdict() // zero judged rows is PASS; no timer is armed
	}
	tm := e.launchTimes()
	dctx, cancel := context.WithTimeout(ctx, tm.deadline)
	defer cancel()
	d := tm.first
	for {
		if !e.wait(dctx, d) {
			p, _ := e.probe(ctx, rows, ackDir)
			j.Deadline(p)
			break
		}
		p, alive := e.probe(ctx, rows, ackDir)
		if !alive {
			e.logf("LAUNCH probe: the server is gone")
			j.Deadline(p)
			break
		}
		if j.Observe(p) {
			if e.wait(dctx, tm.settle) {
				if p2, ok := e.probe(ctx, rows, ackDir); ok {
					j.Settle(p2)
				}
			}
			break
		}
		d = tm.every
	}
	lv := j.Verdict()
	for _, key := range lv.Exited {
		for _, r := range rows {
			if r.Key == key {
				cctx, ccancel := context.WithTimeout(ctx, probeTimeout)
				out, _ := e.tmux.Run(cctx, "capture-pane", "-p", "-t", r.Pane, "-S", "-12")
				ccancel()
				e.logf("LAUNCH EXITED %s (%s, claude=%v), last lines:\n%s", r.Key, r.Pane, r.Claude,
					strings.TrimRight(string(out), "\n"))
			}
		}
	}
	return lv
}

// probe is one ps, one list-panes and one ReadDirNames(ack/). alive is false
// when the server no longer answers.
func (e *Engine) probe(ctx context.Context, rows []launchRow, ackDir string) (verdict.Probe, bool) {
	// Each probe is bounded on its own: the verdict's ctx is uncancelled
	// (WithoutCancel), and one hung tmux or ps must not hold save.lock and
	// the restore suppression forever (G1-M6).
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p := verdict.Probe{Panes: map[string]verdict.PaneState{}, Acks: map[string]bool{}}
	out, err := e.tmux.Run(ctx, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_dead} #{pane_current_command}")
	if err != nil {
		if isNoSessions(err) {
			return p, false
		}
		e.logf("LAUNCH probe list-panes: %v", err)
	}
	p.Panes = verdict.ParsePanes(string(out))
	if ps, err := e.procs.Table(ctx); err == nil {
		for _, pr := range capture.ParsePS(ps).Procs {
			p.Procs = append(p.Procs, verdict.Proc{Pid: pr.Pid, PPid: pr.PPid, Command: pr.Command})
		}
	}
	// The job-control read, only when a judged shell pane has a child: it
	// tells a shell back at its prompt with background helpers (gitstatusd)
	// from one running a foreground job (verdict.ForegroundBusy).
	judged := make([]verdict.PaneState, 0, len(rows))
	for _, r := range rows {
		if ps, ok := p.Panes[r.Pane]; ok {
			judged = append(judged, ps)
		}
	}
	if verdict.NeedsJobs(judged, p.Procs) {
		if jobs, err := e.procs.Jobs(ctx); err == nil {
			p.Procs = jobProcs(p.Procs, capture.ParseJobs(jobs))
		}
	}
	if d, err := statedir.OpenExisting(ackDir, e.claudeRoot); err == nil {
		names, _ := d.ReadDirNames()
		_ = d.Close()
		have := map[string]bool{}
		for _, n := range names {
			have[n] = true
		}
		for _, r := range rows {
			p.Acks[r.Key] = have[r.ack]
		}
	}
	return p, true
}
