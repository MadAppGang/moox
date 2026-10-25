package main

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/MadAppGang/moox/internal/doctor"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/setup"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const doctorUsage = `usage: moox doctor [-L NAME | -S PATH | --all] [--json] [--fix stale|legacy-hooks|restoring|all] [--dry-run]`

// doctorEnv builds the checks' environment; servers nil = machine checks only.
func (m *meta) doctorEnv(root string, servers []tmuxtarget.Target) doctor.Env {
	e := doctor.Env{Getenv: m.env.Getenv, Environ: m.environ, LookPath: m.lookPath, Root: root,
		MooxVersion: mooxroot.VersionString(), Servers: servers, LiveSockets: doctor.LiveSockets(m.env)}
	if root == "" {
		_, e.RootErr = m.rootDir()
	}
	if bin, err := m.lookPath("tmux"); err == nil {
		e.Tmux = tmuxtarget.Target{Bin: bin}
	} else if bin, err := exec.LookPath("tmux"); err == nil {
		e.Tmux = tmuxtarget.Target{Bin: bin}
	}
	if v := m.env.Getenv("MOOX_TMUX_BIN"); v != "" {
		e.Tmux.Bin = v
	}
	if mf := m.manifest(); mf != nil {
		e.Mode = mf.Mode
	}
	if root != "" {
		e.Hook = func() (bool, string) {
			senv, err := m.setupEnv(root, true)
			if err != nil {
				return false, err.Error()
			}
			senv.ThemeDetect = nil
			f, err := setup.Detect(senv)
			if err != nil {
				return false, err.Error()
			}
			if len(f.Rcs) == 0 {
				return true, "shell " + f.ShellName + ": no resume hook (zsh or bash only); layouts and cwd still restore"
			}
			for _, rc := range f.Rcs {
				if !rc.Already {
					return false, rc.Path + " does not load moox's shell hook"
				}
			}
			return true, f.Rcs[0].Path + " loads the hook (via " + f.Rcs[0].AlreadyVia + ")"
		}
	}
	return e
}

func cmdDoctor(m *meta, args []string) int {
	fs := flagSet(m, "doctor", doctorUsage)
	sf := addSockFlags(fs)
	all := fs.Bool("all", false, "check every live server in tmux's socket dir")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fix := fs.String("fix", "", "stale | legacy-hooks | restoring | all")
	dry := fs.Bool("dry-run", false, "with --fix: print what would change")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := sf.check(); err != nil || fs.NArg() != 0 || (*all && sf.given()) {
		fmt.Fprintln(m.stderr, doctorUsage)
		return exitUsage
	}
	switch *fix {
	case "", "stale", "legacy-hooks", "legacy", "restoring", "all":
	default:
		fmt.Fprintf(m.stderr, "moox doctor: unknown --fix %q\n%s\n", *fix, doctorUsage)
		return exitUsage
	}
	root, _ := m.rootDir()
	var servers []tmuxtarget.Target
	if *all {
		bin := ""
		if t, err := tmuxtarget.Resolve(m.env, "MOOX", "", ""); err == nil {
			bin = t.Bin
		}
		for _, s := range doctor.LiveSockets(m.env) {
			servers = append(servers, tmuxtarget.Target{Bin: bin, Socket: s})
		}
	} else if t, err := m.target(sf); err == nil {
		if alive(m.ctx, t) || sf.given() {
			servers = append(servers, t)
		}
	}
	e := m.doctorEnv(root, servers)

	if *fix != "" {
		code := exitOK
		do := func(name string, f func() ([]string, error)) {
			lines, err := f()
			for _, l := range lines {
				fmt.Fprintf(m.stdout, "%s: %s\n", name, l)
			}
			if err != nil {
				fmt.Fprintf(m.stderr, "moox doctor --fix %s: %v\n", name, err)
				code = exitError
			}
		}
		if *fix == "stale" || *fix == "all" {
			do("stale", func() ([]string, error) { return doctor.FixStale(e, *dry) })
		}
		for _, t := range servers {
			if !alive(m.ctx, t) {
				continue
			}
			if *fix == "legacy-hooks" || *fix == "legacy" || *fix == "all" {
				do("legacy-hooks", func() ([]string, error) { return doctor.FixLegacy(m.ctx, e, t, *dry) })
			}
			if *fix == "restoring" || *fix == "all" {
				do("restoring", func() ([]string, error) { return doctor.FixRestoring(m.ctx, t, *dry) })
			}
		}
		return code
	}

	results := doctor.Run(m.ctx, e)
	worst := doctor.OK
	for _, r := range results {
		if r.Level > worst {
			worst = r.Level
		}
	}
	if *asJSON {
		rows := make([]map[string]string, 0, len(results))
		for _, r := range results {
			rows = append(rows, map[string]string{"check": r.Check, "level": r.Level.String(), "message": r.Message,
				"fix": r.Fix, "server": r.Server})
		}
		b, _ := json.MarshalIndent(map[string]any{"version": mooxroot.VersionString(), "results": rows}, "", "  ")
		fmt.Fprintln(m.stdout, string(b))
	} else {
		for _, r := range results {
			fmt.Fprintln(m.stdout, r.Line())
		}
	}
	if worst >= doctor.Fail {
		return exitError
	}
	return exitOK
}
