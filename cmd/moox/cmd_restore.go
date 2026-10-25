package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const restoreUsage = `usage: moox restore [-L NAME | -S PATH] [--snapshot FILE] [--dry-run] [--stub-commands] [--wait-launch]`

// cmdRestore is `moox restore` (§6.4a): a REQUEST to the server's daemon,
// never a restore in this process. --dry-run is local: it reads and
// validates the snapshot and prints the plan without contacting or starting
// a server. With no server running it bootstraps one with handover first.
func cmdRestore(m *meta, args []string) int {
	fs := flagSet(m, "restore", restoreUsage)
	sf := addSockFlags(fs)
	snap := fs.String("snapshot", "", "restore FILE instead of moox-last")
	dry := fs.Bool("dry-run", false, "print the plan; contact no server")
	stub := fs.Bool("stub-commands", m.env.Getenv("MOOX_STUB_COMMANDS") == "1", "replace every relaunch with a harmless stub")
	waitLaunch := fs.Bool("wait-launch", false, "also wait for the LAUNCH verdict")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := sf.check(); err != nil || fs.NArg() != 0 {
		fmt.Fprintf(m.stderr, "moox restore: %v\n%s\n", firstErr(err, fs.Args()), restoreUsage)
		return exitUsage
	}
	t, err := m.target(sf)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return exitTmuxAbsent
	}
	e, err := m.engine(t)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return exitError
	}
	if *dry {
		return restoreDryRun(m, e, *snap)
	}
	if _, code := m.tmuxVersion(t, "restore"); code != exitOK {
		return code
	}
	path, err := saver.ResolveSnapshot(e, *snap, m.cwd)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return exitInvalid
	}
	return requestRestore(m, t, e, path, *stub, *waitLaunch, "main")
}

// requestRestore is §6.4a steps 0–4 for a resolved snapshot path.
func requestRestore(m *meta, t tmuxtarget.Target, e *engine.Engine, path string, stub, waitLaunch bool, session string) int {
	root, err := m.rootDir()
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return exitError
	}
	c := saver.Client{Target: t, Paths: e.Paths(), Root: root, Environ: m.environ, Session: session}
	boot, err := c.Bootstrap(m.ctx)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return exitError
	}
	if boot {
		fmt.Fprintf(m.stderr, "moox restore: started %s with handover\n", t.Socket)
	}
	if err := c.EnsureDaemon(m.ctx); err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return restoreExit(err)
	}
	// The local precondition check is only a fast error; the daemon checks
	// again before it starts.
	if err := e.ManualPreconditions(m.ctx); err != nil {
		fmt.Fprintf(m.stderr, "moox restore: refused: %v\n", err)
		if errors.Is(err, engine.ErrRestoring) {
			return exitBusy
		}
		return exitNotApplic
	}
	o, err := c.Request(m.ctx, path, stub)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return restoreExit(err)
	}
	fmt.Fprintln(m.stdout, o.Result)
	if waitLaunch {
		if err := c.WaitLaunch(m.ctx, 5*time.Minute); err != nil {
			fmt.Fprintf(m.stderr, "moox restore: waiting for the LAUNCH verdict: %v\n", err)
			return restoreExit(err)
		}
	}
	return exitOK
}

func restoreExit(err error) int {
	switch {
	case errors.Is(err, saver.ErrBusyRestoring):
		return exitBusy
	case errors.Is(err, saver.ErrRefused):
		return exitNotApplic
	case errors.Is(err, saver.ErrNoAnswer):
		return exitDeadline
	}
	return exitError
}

// restoreDryRun is §6.4a step 0 (R2-H5): local only.
func restoreDryRun(m *meta, e *engine.Engine, file string) int {
	var s *snapshot.Snapshot
	var path string
	if file != "" {
		path = file
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.cwd, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
			return exitInvalid
		}
		if s, err = snapshot.Decode(b); err == nil {
			err = snapshot.Validate(s)
		}
		if err != nil {
			fmt.Fprintf(m.stderr, "moox restore: %s: %v\n", path, err)
			return exitInvalid
		}
	} else {
		info, err := e.Inspect()
		if err == nil && info.LastErr != nil {
			err = info.LastErr
		}
		if err != nil {
			fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
			return exitInvalid
		}
		s, path = info.LastSnapshot, info.LastPath
	}
	p, err := e.Plan(m.ctx, s)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox restore: %v\n", err)
		return exitInvalid
	}
	fmt.Fprintf(m.stdout, "moox restore --dry-run %s\n", path)
	fmt.Fprintf(m.stdout, "  %d sessions, %d windows, %d panes\n", len(p.Sessions), len(p.Windows), len(p.Rows))
	for _, ps := range p.Sessions {
		g := ""
		if ps.GroupedWith != "" {
			g = " (grouped with " + ps.GroupedWith + ")"
		}
		fmt.Fprintf(m.stdout, "  session %q%s: %d windows\n", ps.Name, g, len(ps.Windows))
	}
	for _, r := range p.Rows {
		what := "shell"
		if r.Cmd != "" {
			what = r.Kind + ": " + r.Cmd
		}
		fmt.Fprintf(m.stdout, "    %s  %s  [%s]\n", r.Key, r.Pane.Cwd, what)
	}
	for _, d := range p.Dropped {
		fmt.Fprintf(m.stdout, "  dropped (tt observer): %s\n", d)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(m.stdout, "  warning: %s\n", w)
	}
	return exitOK
}

const snapshotUsage = `usage: moox snapshot show [-L NAME | -S PATH] [--json]
       moox snapshot unpin [-L NAME | -S PATH] FILE|--all`

func cmdSnapshot(m *meta, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(m.stderr, snapshotUsage)
		return exitUsage
	}
	sub, args := args[0], args[1:]
	fs := flagSet(m, "snapshot "+sub, snapshotUsage)
	sf := addSockFlags(fs)
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "unpin every snapshot")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := sf.check(); err != nil {
		fmt.Fprintln(m.stderr, snapshotUsage)
		return exitUsage
	}
	t, err := m.target(sf)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox snapshot: %v\n", err)
		return exitTmuxAbsent
	}
	e, err := m.engine(t)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox snapshot: %v\n", err)
		return exitError
	}
	switch sub {
	case "show":
		if fs.NArg() != 0 || *all {
			fmt.Fprintln(m.stderr, snapshotUsage)
			return exitUsage
		}
		return snapshotShow(m, e, *asJSON)
	case "unpin":
		if (fs.NArg() == 1) == *all || fs.NArg() > 1 {
			fmt.Fprintln(m.stderr, snapshotUsage)
			return exitUsage
		}
		name := ""
		if !*all {
			name = fs.Arg(0)
			if strings.Contains(name, "/") && !filepath.IsAbs(name) {
				name = filepath.Join(m.cwd, name)
			}
		}
		left, err := e.Unpin(m.ctx, name)
		switch {
		case errors.Is(err, engine.ErrNotPinned):
			fmt.Fprintf(m.stderr, "moox snapshot unpin: %v\n", err)
			return exitNotApplic
		case errors.Is(err, engine.ErrBusy):
			fmt.Fprintf(m.stderr, "moox snapshot unpin: %v (a save or restore holds the lock; try again)\n", err)
			return exitBusy
		case err != nil:
			fmt.Fprintf(m.stderr, "moox snapshot unpin: %v\n", err)
			return exitError
		}
		fmt.Fprintf(m.stdout, "%d pinned\n", len(left))
		return exitOK
	}
	fmt.Fprintf(m.stderr, "moox snapshot: unknown subcommand %q\n%s\n", sub, snapshotUsage)
	return exitUsage
}

// shArg is s as one sh word: unchanged when it holds only safe characters,
// else single-quoted.
func shArg(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%=,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func snapshotShow(m *meta, e *engine.Engine, asJSON bool) int {
	info, err := e.Inspect()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(m.stderr, "moox snapshot show: %v\n", err)
		return exitError
	}
	last := ""
	if info.LastErr == nil {
		last = info.LastPath
	}
	if asJSON {
		lastErr := ""
		if info.LastErr != nil {
			lastErr = info.LastErr.Error()
		}
		pins := info.PinPaths
		if pins == nil {
			pins = []string{}
		}
		b, _ := json.MarshalIndent(map[string]any{"dir": info.Dir, "moox_last": info.Pointer, "last": last,
			"last_error": lastErr, "healed": info.Healed, "pins": pins, "restoring_from": info.RestoringFrom}, "", "  ")
		fmt.Fprintln(m.stdout, string(b))
		return exitOK
	}
	fmt.Fprintf(m.stdout, "snapshots: %s\n", info.Dir)
	switch {
	case info.LastErr != nil:
		fmt.Fprintf(m.stdout, "moox-last: none valid (%v)\n", info.LastErr)
	default:
		age := ""
		if d, ok := engine.SnapshotAge(info.LastSnapshot, time.Now()); ok {
			age = ", " + d.Round(time.Second).String() + " old"
		}
		healed := ""
		if info.Healed {
			healed = fmt.Sprintf(" (moox-last says %q, which does not validate)", info.Pointer)
		}
		fmt.Fprintf(m.stdout, "moox-last: %s (%d sessions, %d windows, %d panes%s)%s\n", info.Last,
			len(info.LastSnapshot.Sessions), len(info.LastSnapshot.Windows), len(info.LastSnapshot.Panes), age, healed)
	}
	if len(info.Pins) == 0 {
		fmt.Fprintln(m.stdout, "pins: none")
	}
	// Every printed command names the inspected server: run from another
	// server's pane, a bare one would restart or unpin THAT server (G2-H4).
	sock := shArg(e.Paths().Socket)
	for _, p := range info.PinPaths {
		fmt.Fprintf(m.stdout, "pinned: %s\n  bring it back: moox restart -S %s --snapshot %s\n  release it:    moox snapshot unpin -S %s %s\n",
			p, sock, shArg(p), sock, shArg(filepath.Base(p)))
	}
	if info.RestoringFrom != "" {
		fmt.Fprintf(m.stdout, "restoring-from: %s\n", info.RestoringFrom)
	}
	return exitOK
}
