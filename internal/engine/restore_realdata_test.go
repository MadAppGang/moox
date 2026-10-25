//go:build realdata && tmuxe2e

package engine

// V4's restore half (architecture §12.3): the imported COPY of the live
// resurrect snapshot is restored, stubbed, into an isolated cold server named
// moox-val-import, through the boot path itself.
//
//	MOOX_V4_REPORT_APPEND=<file> go test -tags 'realdata tmuxe2e' -run TestRealData_V4_RestoreStubbed -v ./internal/engine/
//
// The real files are only READ, once, to copy them (as in
// TestRealData_V4_ImportLiveSnapshot). The server is tmuxtest.StartCold: its
// socket, HOME and XDG tree live under a temp root, its panes are hook shells
// sourcing only the vendored hook, and MOOX_STUB_COMMANDS=1 replaces every
// relaunch with a stub that prints the original (base64) and sleeps. The
// report holds counts only.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func TestRealData_V4_RestoreStubbed(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	srcDir := os.Getenv("MOOX_V4_SOURCE")
	if srcDir == "" {
		srcDir = filepath.Join(realHome, ".local", "share", "tmux", "resurrect")
	}
	ln, err := os.Readlink(filepath.Join(srcDir, "last"))
	if err != nil {
		t.Skipf("no resurrect last to copy: %v", err)
	}
	target := ln
	if !filepath.IsAbs(target) {
		target = filepath.Join(srcDir, target)
	}
	work := t.TempDir()
	cp := filepath.Join(work, "resurrect")
	if err := os.Mkdir(cp, 0o700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(target)
	copyFile(t, target, filepath.Join(cp, base))
	if err := os.Symlink(base, filepath.Join(cp, "last")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "pane_contents.tar.gz")); err == nil {
		copyFile(t, filepath.Join(srcDir, "pane_contents.tar.gz"), filepath.Join(cp, "pane_contents.tar.gz"))
	}
	ctx := context.Background()
	im, err := Import(ctx, ImportSource{From: cp, Home: realHome, Producer: "moox v4"}, nil)
	if err != nil {
		t.Fatalf("import of the copy: %v", err)
	}
	snap := im.Snapshot

	// ---- the isolated cold server restores it at boot -----------------------
	start := time.Now()
	s := tmuxtest.StartCold(t, tmuxtest.WithName("moox-val-import"), tmuxtest.WithBinaries(),
		tmuxtest.WithShell(tmuxtest.HookZsh(t)), tmuxtest.WithSize(200, 50), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"),
		tmuxtest.WithPrepare(func(s *tmuxtest.Server) {
			e, err := New(Options{Target: s.T, Env: tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS,
				"XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC}, Producer: "moox v4"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.PublishImport(ctx, im, PublishImportOptions{}); err != nil {
				t.Fatalf("publish on the isolated key: %v", err)
			}
		}))
	if !tmuxtest.Poll(60*time.Second, 250*time.Millisecond, func() bool { return s.Opt(t, "@moox-boot-result") != "" }) {
		t.Fatal("no boot result")
	}
	created := time.Since(start)
	if !tmuxtest.Poll(240*time.Second, 500*time.Millisecond, func() bool { return s.Opt(t, "@moox-restoring") == "" }) {
		t.Fatal("no LAUNCH verdict within 240 s")
	}
	boot := s.Opt(t, "@moox-boot-result")
	log := daemonLog(t, s)
	launchLine, bootLine := "", ""
	for _, l := range strings.Split(log, "\n") {
		if i := strings.Index(l, "LAUNCH VERDICT: "); i >= 0 {
			launchLine = l[i:]
		}
		if i := strings.Index(l, "BOOT VERDICT: "); i >= 0 {
			bootLine = l[i:]
		}
	}
	// BOOT warnings by kind (the lines themselves name sessions)
	warnKinds := map[string]int{}
	for _, l := range strings.Split(log, "\n") {
		for _, k := range []string{"LAYOUT-FALLBACK", "LAYOUT-REMOVE-FAILED", "FROZEN-CLAIM-FAILED", "FROZEN-FOREIGN",
			"FROZEN-UNREADABLE", "WINDOW-INDEX", "GROUPED-SKIPPED", "GROUPED-FAILED", "CONTENTS-DROPPED"} {
			if strings.Contains(l, " "+k+" ") {
				warnKinds[k]++
				if os.Getenv("MOOX_V4_DEBUG") == "1" { // local diagnosis only: the line names a session
					t.Log(l)
				}
			}
		}
	}
	// strip the per-row list (it names sessions)
	if i := strings.Index(launchLine, " ["); i >= 0 {
		launchLine = launchLine[:i]
	}
	if i := strings.Index(bootLine, " [missed:"); i >= 0 {
		bootLine = bootLine[:i] + " [missed: <redacted>]"
	}

	// ---- compare with the import -------------------------------------------
	var fails []string
	observers := 0
	want := map[string]snapshot.Pane{}
	for _, p := range snap.Panes {
		if p.Role == snapshot.RoleTTObserver {
			observers++
			continue
		}
		want[p.Key] = p
	}
	// tt's own sidebar panes (tt.conf opens one in a wide window once
	// @moox-restoring stops holding it) are not restored rows
	var live []livePane
	sidebars := 0
	obs := map[string]bool{}
	for _, line := range strings.Split(s.Run(t, "list-panes", "-a", "-F", "#{pane_id}\x1f#{@tt_obs}\x1f#{pane_start_command}"), "\n") {
		if f := strings.SplitN(line, "\x1f", 3); len(f) == 3 && (f[1] == "1" || strings.Contains(f[2], "tt observe --pane")) {
			obs[f[0]] = true
		}
	}
	for _, lp := range listPanes(t, s) {
		if obs[lp.id] {
			sidebars++
			continue
		}
		live = append(live, lp)
	}
	sessions := map[string]bool{}
	windows := map[string]bool{}
	cwdEqual, cwdMissingDir, stubbed, resumes := 0, 0, 0, 0
	byKeyCount := map[string][]livePane{}
	for _, lp := range live {
		w := lp.key[:strings.LastIndexByte(lp.key, '.')]
		windows[w] = true
		sessions[w[:strings.LastIndexByte(w, ':')]] = true
		byKeyCount[w] = append(byKeyCount[w], lp)
	}
	plan, err := buildPlan(snap, planInputs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range plan.Windows {
		got := byKeyCount[fmt.Sprintf("%s:%d", w.Session, w.Index)]
		if len(got) != len(w.Rows) {
			fails = append(fails, fmt.Sprintf("a window has %d panes, want %d", len(got), len(w.Rows)))
			continue
		}
		for i, ri := range w.Rows {
			row := plan.Rows[ri]
			if got[i].cwd == row.Pane.Cwd {
				cwdEqual++
			} else if _, err := os.Stat(row.Pane.Cwd); err != nil {
				cwdMissingDir++ // tmux falls back when the saved dir no longer exists
			} else {
				fails = append(fails, "a restored pane's cwd differs from its row")
			}
			if row.Resumes() {
				resumes++
				if strings.Contains(s.Run(t, "capture-pane", "-p", "-J", "-S", "-", "-t", got[i].id), "MOOX-STUB") {
					stubbed++
				}
			}
		}
	}
	if len(live) != len(want) {
		fails = append(fails, fmt.Sprintf("panes: %d restored, %d imported (+%d observers dropped)", len(live), len(want), observers))
	}
	if len(windows) != len(plan.Windows) {
		fails = append(fails, fmt.Sprintf("windows: %d restored, %d planned", len(windows), len(plan.Windows)))
	}
	if len(sessions) != len(snap.Sessions) {
		fails = append(fails, fmt.Sprintf("sessions: %d restored, %d imported", len(sessions), len(snap.Sessions)))
	}
	if stubbed != resumes {
		fails = append(fails, fmt.Sprintf("stub output in %d of %d resumed panes", stubbed, resumes))
	}
	for _, sid := range strings.Fields(s.Run(t, "list-sessions", "-F", "#{session_id}")) {
		for _, line := range strings.Split(s.Run(t, "show-environment", "-t", sid), "\n") {
			if strings.HasPrefix(line, "MOOX_") {
				fails = append(fails, "a session env holds a MOOX_ value")
			}
		}
	}
	if boot == "" || launchLine == "" {
		fails = append(fails, "no verdicts in daemon.log")
	}
	verdictWord := "PASS"
	if len(fails) > 0 {
		verdictWord = "FAIL"
	}

	var r strings.Builder
	fmt.Fprintf(&r, "\n## V4 restore half — stubbed boot restore on an isolated server (P4b)\n\n")
	fmt.Fprintf(&r, "Run: %s · `go test -tags 'realdata tmuxe2e' -run TestRealData_V4_RestoreStubbed ./internal/engine/`\n\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&r, "**Verdict: %s**\n\n", verdictWord)
	fmt.Fprintf(&r, "Method: the same read-once COPY of resurrect's `last` target and archive was imported, published on the key of a "+
		"`tmuxtest.StartCold` server named `moox-val-import` (socket, HOME and XDG under a temp root; hook-shell panes sourcing only "+
		"the vendored hook; `MOOX_STUB_COMMANDS=1`), and restored by that server's own boot path: gate, purges, bootstrap poll, "+
		"rename, drop, LAUNCH verdict. No real command ran (every relaunch is a stub that prints its original as base64 and sleeps; "+
		"Claude kinds exec `sleep` named `claude`). The live server was never contacted.\n\n")
	fmt.Fprintf(&r, "| Count | Imported | Restored |\n|---|---|---|\n")
	fmt.Fprintf(&r, "| panes (observers dropped: %d) | %d | %d |\n", observers, len(want), len(live))
	fmt.Fprintf(&r, "| windows | %d | %d |\n", len(plan.Windows), len(windows))
	fmt.Fprintf(&r, "| sessions | %d | %d |\n", len(snap.Sessions), len(sessions))
	fmt.Fprintf(&r, "| cwd equal to the row | %d | %d (+%d whose saved dir no longer exists) |\n", len(want), cwdEqual, cwdMissingDir)
	fmt.Fprintf(&r, "| resumed rows with stub output | %d | %d |\n\n", resumes, stubbed)
	fmt.Fprintf(&r, "| Verdict | Value |\n|---|---|\n")
	fmt.Fprintf(&r, "| @moox-boot-result | %s |\n| BOOT line | %s |\n| LAUNCH line | %s |\n", boot, bootLine, launchLine)
	fmt.Fprintf(&r, "| BOOT warnings by kind | %s |\n", histogram(warnKinds))
	fmt.Fprintf(&r, "| panes created by | %s after the cold start |\n", created.Round(100*time.Millisecond))
	fmt.Fprintf(&r, "| session env with MOOX_* values | none |\n")
	fmt.Fprintf(&r, "| tt sidebar panes tt.conf opened after RESTORE-END (not counted above) | %d |\n", sidebars)
	if len(fails) > 0 {
		fmt.Fprintf(&r, "\nFailures:\n")
		for _, f := range fails {
			fmt.Fprintf(&r, "- %s\n", f)
		}
	}
	text := r.String()
	t.Log(text)
	if p := os.Getenv("MOOX_V4_REPORT_APPEND"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	for _, f := range fails {
		t.Error(f)
	}
}
