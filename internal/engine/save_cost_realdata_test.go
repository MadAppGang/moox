//go:build realdata && tmuxe2e

package engine

// V5 (architecture §12.3): the cost of a save, moox against resurrect, on the
// SAME isolated server holding the restored V4 workspace (a stubbed restore
// of an import of a COPY of the live resurrect snapshot), both with pane
// contents at full history.
//
//	MOOX_V5_REPORT=<file> go test -tags 'realdata tmuxe2e' -run '^$' -bench BenchmarkE2E_V5_SaveCost -benchtime 1x ./internal/engine/
//
// (a) `moox save --direct` ×5 (the binary built from this tree).
// (b) tmux-resurrect's `save.sh quiet` ×5, from a COPY of the plugin under a
//     temp dir, run directly (never through run-shell), with TMUX naming the
//     scratch server and HOME/XDG_* scratch, and on the scratch server
//     @resurrect-dir = a scratch dir, @resurrect-save-command-strategy pgrep,
//     @resurrect-capture-pane-contents on (PR-M6).
// PATH shims count the tmux/pgrep/ps/sed/cut execs of every run. The real
// files are only READ (the copy); before and after, the real resurrect
// `last`'s inode and mtime and the newest real tmux_resurrect_* name must be
// unchanged.

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func copyTB(tb testing.TB, from, to string) {
	tb.Helper()
	in, err := os.Open(from)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		tb.Fatal(err)
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm()|0o600)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		tb.Fatal(err)
	}
	if err := out.Close(); err != nil {
		tb.Fatal(err)
	}
	_ = os.Chtimes(to, st.ModTime(), st.ModTime())
}

func copyTreeTB(tb testing.TB, from, to string) {
	tb.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(dst, 0o700)
		case d.Type().IsRegular():
			copyTB(tb, p, dst)
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
}

type realMark struct {
	ino    uint64
	mtime  time.Time
	newest string
}

func markReal(tb testing.TB, dir string) realMark {
	tb.Helper()
	var m realMark
	if st, err := os.Lstat(filepath.Join(dir, "last")); err == nil {
		m.mtime = st.ModTime()
		if s, ok := st.Sys().(*syscall.Stat_t); ok {
			m.ino = s.Ino
		}
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "tmux_resurrect_") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		m.newest = names[len(names)-1]
	}
	return m
}

// shims writes counting wrappers for the tools both savers fork.
func shims(tb testing.TB, dir, countFile string) string {
	tb.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	for _, tool := range []string{"tmux", "pgrep", "ps", "sed", "cut"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			tb.Fatalf("no %s: %v", tool, err)
		}
		body := "#!/bin/sh\nprintf '%s\\n' " + tool + " >> \"" + countFile + "\"\nexec " + real + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(body), 0o700); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

type run struct {
	took   time.Duration
	execs  map[string]int
	total  int
	result string
}

func countExecs(tb testing.TB, f string) (map[string]int, int) {
	b, _ := os.ReadFile(f)
	m := map[string]int{}
	n := 0
	for _, l := range strings.Fields(string(b)) {
		m[l]++
		n++
	}
	_ = os.Remove(f)
	return m, n
}

func median(rs []run) time.Duration {
	d := make([]time.Duration, len(rs))
	for i, r := range rs {
		d[i] = r.took
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

func BenchmarkE2E_V5_SaveCost(b *testing.B) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		b.Fatal(err)
	}
	realRes := filepath.Join(realHome, ".local", "share", "tmux", "resurrect")
	plugin := os.Getenv("MOOX_V5_RESURRECT_PLUGIN")
	if plugin == "" {
		plugin = filepath.Join(realHome, "dotfiles", "tmux", ".tmux", "plugins", "tmux-resurrect")
	}
	ln, err := os.Readlink(filepath.Join(realRes, "last"))
	if err != nil {
		b.Skipf("no resurrect last to copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plugin, "scripts", "save.sh")); err != nil {
		b.Skipf("no tmux-resurrect plugin to copy: %v", err)
	}
	markBefore := markReal(b, realRes)

	// ---- copies (the real files are only read) -----------------------------
	work := b.TempDir()
	cp := filepath.Join(work, "resurrect")
	if err := os.Mkdir(cp, 0o700); err != nil {
		b.Fatal(err)
	}
	target := ln
	if !filepath.IsAbs(target) {
		target = filepath.Join(realRes, target)
	}
	copyTB(b, target, filepath.Join(cp, filepath.Base(target)))
	if err := os.Symlink(filepath.Base(target), filepath.Join(cp, "last")); err != nil {
		b.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(realRes, "pane_contents.tar.gz")); err == nil {
		copyTB(b, filepath.Join(realRes, "pane_contents.tar.gz"), filepath.Join(cp, "pane_contents.tar.gz"))
	}
	pluginCopy := filepath.Join(work, "tmux-resurrect")
	copyTreeTB(b, plugin, pluginCopy)

	ctx := context.Background()
	im, err := Import(ctx, ImportSource{From: cp, Home: realHome, Producer: "moox v5"}, nil)
	if err != nil {
		b.Fatalf("import of the copy: %v", err)
	}
	// A stubbed restore brings no real Claude back, so every later save would
	// be refused by the clobber guard (B20: ≥3 sids → 0) after doing all of
	// its work but the write. V5 measures a PUBLISHING save, so the Claude
	// rows are imported as shell rows: same panes, cwds, contents.
	sids := 0
	for i := range im.Snapshot.Panes {
		if p := &im.Snapshot.Panes[i]; p.Claude != nil {
			p.Claude, p.Role = nil, "shell"
			sids++
		}
	}

	// ---- the restored workspace on an isolated server ----------------------
	s := tmuxtest.StartCold(b, tmuxtest.WithName("moox-val-v5"), tmuxtest.WithBinaries(),
		tmuxtest.WithShell(tmuxtest.HookZsh(b)), tmuxtest.WithSize(200, 50), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"),
		tmuxtest.WithPrepare(func(s *tmuxtest.Server) {
			e, err := New(Options{Target: s.T, Env: tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS,
				"XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC}, Producer: "moox v5"})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := e.PublishImport(ctx, im, PublishImportOptions{}); err != nil {
				b.Fatalf("publish on the isolated key: %v", err)
			}
		}))
	if !tmuxtest.Poll(60*time.Second, 250*time.Millisecond, func() bool { return s.Opt(b, "@moox-boot-result") != "" }) {
		b.Fatal("no boot result")
	}
	if !tmuxtest.Poll(240*time.Second, 500*time.Millisecond, func() bool { return s.Opt(b, "@moox-restoring") == "" }) {
		b.Fatal("no LAUNCH verdict within 240 s")
	}
	// let the daemon's post-restore save happen first
	logf := filepath.Join(s.Paths(b).State, "daemon.log")
	lastSize, since := int64(-1), time.Now()
	for time.Since(since) < 5*time.Second {
		if st, err := os.Stat(logf); err == nil && st.Size() != lastSize {
			lastSize, since = st.Size(), time.Now()
		}
		time.Sleep(200 * time.Millisecond)
	}
	panes := len(strings.Fields(s.Run(b, "list-panes", "-a", "-F", "#{pane_id}")))
	windows := len(strings.Fields(s.Run(b, "list-windows", "-a", "-F", "#{window_id}")))
	lines := 0
	for _, h := range strings.Fields(s.Run(b, "list-panes", "-a", "-F", "#{history_size}")) {
		var n int
		_, _ = fmt.Sscanf(h, "%d", &n)
		lines += n
	}
	serverPid := strings.TrimSpace(s.Run(b, "display-message", "-p", "#{pid}"))

	countFile := filepath.Join(work, "execs")
	shimDir := shims(b, filepath.Join(work, "shims"), countFile)
	path := shimDir + ":" + os.Getenv("PATH")
	base := []string{"HOME=" + s.Home, "XDG_STATE_HOME=" + s.XS, "XDG_DATA_HOME=" + s.XD, "XDG_CONFIG_HOME=" + s.XC,
		"PATH=" + path, "LANG=" + os.Getenv("LANG"), "LC_ALL=C"}

	// The daemon's saver is stopped for the measurement (it would answer the
	// renames below with saves of its own and contend for save.lock), and
	// every moox run is preceded by a window rename, so each of the five
	// PUBLISHES — resurrect writes a file on every run too.
	stop := exec.Command(filepath.Join(s.Bin, "tt"), "serve", "--stop", "--tmux-socket", s.T.Socket)
	stop.Env = base
	if out, err := stop.CombinedOutput(); err != nil {
		b.Fatalf("tt serve --stop: %v %s", err, out)
	}
	pidFile := filepath.Join(s.Paths(b).TTState, "observer.pid")
	if !tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool { _, err := os.Stat(pidFile); return err != nil }) {
		b.Fatal("the daemon did not stop")
	}
	firstWindow := strings.Fields(s.Run(b, "list-windows", "-a", "-F", "#{window_id}"))[0]

	// (a) moox save --direct, contents at full history
	var moox []run
	for i := 0; i < 5; i++ {
		s.Run(b, "rename-window", "-t", firstWindow, fmt.Sprintf("v5-run-%d", i))
		cmd := exec.Command(filepath.Join(s.Bin, "moox"), "save", "--direct", "-S", s.T.Socket, "--reason", "v5")
		cmd.Env = base
		start := time.Now()
		out, err := cmd.Output()
		took := time.Since(start)
		if err != nil {
			b.Fatalf("moox save --direct: %v %s", err, out)
		}
		m, n := countExecs(b, countFile)
		moox = append(moox, run{took: took, execs: m, total: n, result: strings.TrimSpace(string(out))})
	}

	// (b) resurrect save.sh quiet, pgrep strategy, contents on
	resDir := filepath.Join(work, "rs")
	if err := os.MkdirAll(resDir, 0o700); err != nil {
		b.Fatal(err)
	}
	s.Run(b, tmuxtarget.Chain(
		[]string{"set-option", "-g", "@resurrect-dir", resDir},
		[]string{"set-option", "-g", "@resurrect-save-command-strategy", "pgrep"},
		[]string{"set-option", "-g", "@resurrect-capture-pane-contents", "on"},
	)...)
	for _, h := range []string{"post-save-layout", "post-save-all", "pre-save-all"} {
		if v := s.Opt(b, "@resurrect-hook-"+h); v != "" {
			b.Fatalf("a resurrect hook is set on the scratch server: %s=%q", h, v)
		}
	}
	var res []run
	for i := 0; i < 5; i++ {
		before, _ := os.ReadDir(resDir)
		cmd := exec.Command("bash", filepath.Join(pluginCopy, "scripts", "save.sh"), "quiet")
		cmd.Env = append([]string{"TMUX=" + s.T.Socket + "," + serverPid + ",0"}, base...)
		cmd.Dir = work
		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start)
		if err != nil {
			b.Fatalf("resurrect save.sh: %v %s", err, out)
		}
		after, _ := os.ReadDir(resDir)
		m, n := countExecs(b, countFile)
		res = append(res, run{took: took, execs: m, total: n, result: fmt.Sprintf("%d → %d files", len(before), len(after))})
		time.Sleep(1100 * time.Millisecond) // resurrect names files by the second
	}
	markAfter := markReal(b, realRes)
	if markAfter != markBefore {
		b.Fatalf("the REAL resurrect dir changed: %+v → %+v", markBefore, markAfter)
	}

	mm, rm := median(moox), median(res)
	b.ReportMetric(float64(mm.Milliseconds()), "moox-ms")
	b.ReportMetric(float64(rm.Milliseconds()), "resurrect-ms")
	b.ReportMetric(float64(moox[0].total), "moox-execs")
	b.ReportMetric(float64(res[0].total), "resurrect-execs")

	var rep strings.Builder
	fmt.Fprintf(&rep, "## BenchmarkE2E_V5_SaveCost — %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&rep, "Workspace: the V4 import of a COPY of the live resurrect `last` (%s), restored stubbed on the isolated "+
		"cold server `moox-val-v5` (StartCold, HookZsh panes, scratch HOME/XDG): **%d panes, %d windows, %d history lines** "+
		"held at measurement time (its %d Claude rows imported as shell rows, so the clobber guard lets moox publish). Both savers capture pane contents at full history.\n\n", filepath.Base(target), panes, windows, lines, sids)
	fmt.Fprintf(&rep, "| run | moox save --direct | execs (tmux/ps) | result | resurrect save.sh (pgrep) | execs (tmux/pgrep/ps/sed/cut) | result |\n|---|---|---|---|---|---|---|\n")
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&rep, "| %d | %s | %d (%d/%d) | %s | %s | %d (%d/%d/%d/%d/%d) | %s |\n", i+1,
			moox[i].took.Round(time.Millisecond), moox[i].total, moox[i].execs["tmux"], moox[i].execs["ps"], moox[i].result,
			res[i].took.Round(time.Millisecond), res[i].total, res[i].execs["tmux"], res[i].execs["pgrep"], res[i].execs["ps"],
			res[i].execs["sed"], res[i].execs["cut"], res[i].result)
	}
	fmt.Fprintf(&rep, "\n**Median wall time:** moox %s, resurrect %s (%.1f× faster). **Execs per save:** moox %d, resurrect %d.\n\n",
		mm.Round(time.Millisecond), rm.Round(time.Millisecond), float64(rm)/float64(mm), moox[0].total, res[0].total)
	fmt.Fprintf(&rep, "Real resurrect dir, before → after: `last` inode %d → %d, mtime %s → %s, newest `%s` → `%s` (unchanged).\n\n",
		markBefore.ino, markAfter.ino, markBefore.mtime.Format(time.RFC3339), markAfter.mtime.Format(time.RFC3339),
		markBefore.newest, markAfter.newest)
	b.Log("\n" + rep.String())
	if f := os.Getenv("MOOX_V5_REPORT"); f != "" {
		if err := os.WriteFile(f, []byte(rep.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	if mm >= rm || moox[0].total >= res[0].total {
		b.Fatalf("moox is not cheaper: %s/%d execs vs resurrect %s/%d execs", mm, moox[0].total, rm, res[0].total)
	}
}
