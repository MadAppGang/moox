//go:build tmuxe2e

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// TestE2E_LaunchVerdict_ClaudeQuitsShellKeepsHelper (iter-2 real-zsh bug,
// B42/R27): a real zsh with powerlevel10k keeps gitstatusd as a child of the
// idle shell in a process group of its own. The restored Claude runs (OK on
// the first probes), then quits during the settle; the shell is back at its
// prompt with the helper still its child. The old "no children" rule never
// read that pane as bare, so the settle kept OK: LAUNCH PASS, the source
// unpinned. The foreground rule reads EXITED: LAUNCH FAIL, the source
// pinned, the warning set.
func TestE2E_LaunchVerdict_ClaudeQuitsShellKeepsHelper(t *testing.T) {
	s, e := hookServer(t)
	e.timing.settle = 4 * time.Second
	bin := t.TempDir()
	writeFile(t, bin, "claude", "#!/bin/sh\nsleep 1\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	// the helper is the shell's own background job (MONITOR on: its own
	// process group), started on the same line before Claude, as a prompt
	// plugin starts gitstatusd; its pid goes to a file for the cleanup
	helperPid := filepath.Join(bin, "helper.pid")
	t.Cleanup(func() { killPidFile(t, helperPid) })
	snap := snapOf("q:0:1", "q:1:1")
	claudeRow(&snap.Panes[0], "sid-q", "{ sleep 120 & echo $! > "+helperPid+"; } ; "+bin+"/claude")
	publish(t, e, snap)
	resetTo(t, s, "bootstrap")
	res, lv := restoreNow(t, e, false)
	k := res.Plan.Rows[0].Key
	pane := res.Created[k]

	// the setup is the bug's: the helper is the shell's child, in its own group
	shell, _ := strconv.Atoi(strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", pane, "#{pane_pid}")))
	b, err := os.ReadFile(helperPid)
	if err != nil {
		t.Fatalf("the helper never started: %v\n%s", err, s.Run(t, "capture-pane", "-p", "-t", pane))
	}
	hp, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	out, _ := osProcs{}.Jobs(context.Background())
	var helper, sh *capture.Job
	jobs := capture.ParseJobs(out)
	for i := range jobs {
		switch jobs[i].Pid {
		case hp:
			helper = &jobs[i]
		case shell:
			sh = &jobs[i]
		}
	}
	if helper == nil || sh == nil || helper.PPid != shell || helper.Pgid == sh.Pgid {
		t.Fatalf("setup: helper %+v shell %+v (want a child of %d in another group)", helper, sh, shell)
	}

	if lv.States[k] != verdict.StateExited || lv.Result != verdict.Fail {
		t.Fatalf("LAUNCH %+v, want %s=EXITED and FAIL", lv, k)
	}
	if !strings.Contains(s.Opt(t, "@claude-continuity-boot-warning"), "LAUNCH FAIL") {
		t.Fatalf("FAIL did not set the warning: %q", s.Opt(t, "@claude-continuity-boot-warning"))
	}
	last, err := e.Last()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(e.Paths(), filepath.Join(s.Home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if pins, _ := st.Pins(); len(pins) != 1 || pins[0] != last.Name {
		t.Fatalf("pins %v, want [%s] (a LAUNCH FAIL pins its source)", pins, last.Name)
	}
}

// typeLine types one command line into pane and presses Enter.
func typeLine(t *testing.T, s *tmuxtest.Server, pane, line string) {
	t.Helper()
	s.Run(t, "send-keys", "-t", pane, "-l", line)
	s.Run(t, "send-keys", "-t", pane, "Enter")
}

// waitJobs polls the real job-control table until ok holds for it.
func waitJobs(t *testing.T, what string, ok func([]capture.Job) bool) []capture.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := osProcs{}.Jobs(context.Background())
		if err != nil {
			t.Fatalf("ps jobs: %v", err)
		}
		jobs := capture.ParseJobs(out)
		if ok(jobs) {
			return jobs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killPidFile kills the one pid this test started and wrote to path.
func killPidFile(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// TestE2E_ManualPreconditions_BackgroundChildIsIdle (iter-2 real-zsh bug,
// B47): a real zsh with powerlevel10k keeps gitstatusd as a child of an idle
// shell, in a process group of its own. A manual restore into that bootstrap
// is allowed — by ManualPreconditions and by the recheck under save.lock —
// while a foreground program in the same pane still refuses.
func TestE2E_ManualPreconditions_BackgroundChildIsIdle(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSession("main"))
	e := e2eEngine(t, s)
	s.Run(t, "set-option", "-g", "@moox-active", "1")
	pane := strings.TrimSpace(s.Run(t, "list-panes", "-t", "main", "-F", "#{pane_id}"))
	shell, err := strconv.Atoi(strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", pane, "#{pane_pid}")))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// (a) a background child in its own process group (gitstatusd's shape)
	bgPid := filepath.Join(s.Home, "bg.pid")
	t.Cleanup(func() { killPidFile(t, bgPid) })
	typeLine(t, s, pane, "set -m; sleep 120 & echo $! > "+bgPid)
	waitJobs(t, "a background child in its own group", func(jobs []capture.Job) bool {
		var sh *capture.Job
		child := false
		for i, j := range jobs {
			if j.Pid == shell {
				sh = &jobs[i]
			}
		}
		if sh == nil {
			return false
		}
		for _, j := range jobs {
			if j.PPid == shell && j.Pgid != sh.Pgid && !j.Stopped {
				child = true
			}
		}
		// the shell is back at its prompt: it owns the foreground again
		return child && sh.TPgid == sh.Pgid
	})
	if err := e.ManualPreconditions(ctx); err != nil {
		t.Fatalf("(a) an idle shell with a background child: %v", err)
	}
	if err := e.idleShell(ctx, shell, strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", pane, "#{pane_current_command}"))); err != nil {
		t.Fatalf("(a) the recheck under save.lock: %v", err)
	}

	// (b) a foreground sleep in the same pane: refused
	typeLine(t, s, pane, "sleep 120")
	waitJobs(t, "the foreground sleep", func(jobs []capture.Job) bool {
		for _, j := range jobs {
			if j.Pid == shell {
				return j.TPgid > 0 && j.TPgid != j.Pgid
			}
		}
		return false
	})
	if err := e.ManualPreconditions(ctx); !errors.Is(err, ErrPopulated) {
		t.Fatalf("(b) a foreground sleep: %v, want ErrPopulated", err)
	}
	// (b') the same foreground job seen only by the recheck: even with the
	// pane reporting a shell name, the foreground group decides
	if err := e.idleShell(ctx, shell, "sh"); !errors.Is(err, ErrPopulated) {
		t.Fatalf("(b) the recheck over a foreground sleep: %v, want ErrPopulated", err)
	}
	s.Run(t, "send-keys", "-t", pane, "C-c")
}
