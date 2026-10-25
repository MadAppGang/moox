package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/doctor"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/setup"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const setupUsage = `usage: moox setup [--yes] [--dry-run] [--mode replace|separate] [--claude-hooks|--no-claude-hooks] [--theme auto|light|dark] [--accessible]`

// setupEnv is setup.Env over this process's environment. offline (--dry-run)
// keeps theme detection away from every tmux server.
func (m *meta) setupEnv(root string, offline bool) (setup.Env, error) {
	r, err := m.roots()
	if err != nil {
		return setup.Env{}, err
	}
	t, terr := tmuxtarget.Resolve(m.env, "MOOX", "", "")
	server := &t
	if terr != nil || offline {
		server = nil
	}
	return setup.Env{
		Home: r.Home, ConfigHome: r.Config, Shell: m.env.Getenv("SHELL"), ZDotDir: m.env.Getenv("ZDOTDIR"),
		Root: root, Getenv: m.env.Getenv, LookPath: m.lookPath, MooxVersion: mooxroot.VersionString(),
		TmuxVersion: func() (tmuxtarget.Version, error) {
			if terr != nil {
				return tmuxtarget.Version{}, terr
			}
			ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
			defer cancel()
			return t.Version(ctx)
		},
		ThemeDetect: func() string { return m.themeDetect(root, server) },
	}, nil
}

// themeDetect runs `<root>/scripts/theme.sh detect` (§8.3 Theme): read-only
// (the pin, the last mode, then the attached clients' reported theme).
//
// The script asks a server only through $TMUX or $MOOX_TMUX_SOCKET, so both
// are replaced here (I-1): with server nil (a dry run, or no tmux) neither is
// set and detection reads the files alone; otherwise the socket and binary
// are exactly the ones moox resolved, never whatever $TMUX or PATH name.
func (m *meta) themeDetect(root string, server *tmuxtarget.Target) string {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", filepath.Join(root, "scripts", "theme.sh"), "detect")
	env := make([]string, 0, len(m.environ)+2)
	for _, kv := range m.environ {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "TMUX", "MOOX_TMUX_SOCKET", "MOOX_TMUX_BIN":
			continue
		}
		env = append(env, kv)
	}
	if server != nil {
		env = append(env, "MOOX_TMUX_SOCKET="+server.Socket, "MOOX_TMUX_BIN="+server.Bin)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	switch v := strings.TrimSpace(string(out)); v {
	case "light", "dark":
		return v
	}
	return ""
}

func cmdSetup(m *meta, args []string) int {
	fs := flagSet(m, "setup", setupUsage)
	yes := fs.Bool("yes", false, "no questions: take the defaults (and the flags)")
	dry := fs.Bool("dry-run", false, "print the plan and write nothing")
	mode := fs.String("mode", "", "replace | separate")
	hooks := fs.Bool("claude-hooks", false, "add the Claude Code SessionStart/Stop hooks")
	noHooks := fs.Bool("no-claude-hooks", false, "do not add the Claude Code hooks")
	theme := fs.String("theme", "", "auto | light | dark")
	accessible := fs.Bool("accessible", false, "accessible (plain prompt) form")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case fs.NArg() != 0:
		fmt.Fprintf(m.stderr, "moox setup: unexpected argument %q\n%s\n", fs.Arg(0), setupUsage)
		return exitUsage
	case *mode != "" && *mode != string(setup.Replace) && *mode != string(setup.Separate):
		fmt.Fprintf(m.stderr, "moox setup: --mode is replace or separate\n")
		return exitUsage
	case *hooks && *noHooks:
		fmt.Fprintf(m.stderr, "moox setup: give one of --claude-hooks and --no-claude-hooks\n")
		return exitUsage
	case *theme != "" && *theme != "auto" && *theme != "light" && *theme != "dark":
		fmt.Fprintf(m.stderr, "moox setup: --theme is auto, light or dark\n")
		return exitUsage
	}
	root, err := m.rootDir()
	if err != nil {
		fmt.Fprintf(m.stderr, "moox setup: %v\n", err)
		return exitError
	}
	env, err := m.setupEnv(root, *dry)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox setup: %v\n", err)
		return exitError
	}
	f, err := setup.Detect(env)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox setup: %v\n", err)
		return exitError
	}
	if f.TmuxErr != nil || !f.Tmux.AtLeast(3, 6) {
		if f.TmuxErr != nil {
			fmt.Fprintf(m.stderr, "moox setup: tmux not found (%v); install tmux 3.6 or later\n", f.TmuxErr)
		} else {
			fmt.Fprintf(m.stderr, "moox setup: %s is too old; moox needs tmux 3.6 or later\n", f.Tmux)
		}
		return exitTmuxAbsent
	}
	p := setup.Defaults(env, f)
	c := p.Choices
	decided := false
	if *mode != "" {
		c.Mode, decided = setup.Mode(*mode), true
	}
	if *hooks || *noHooks {
		c.ClaudeHooks, decided = *hooks, true
	}
	if *theme != "" {
		c.Theme = *theme
	}
	p = setup.Build(env, f, c)
	if !*yes && !decided && m.stdinTTY && m.stdoutTTY {
		p, err = setup.Ask(env, p, f, *accessible || m.env.Getenv("ACCESSIBLE") != "")
		if errors.Is(err, setup.ErrUserAborted) {
			fmt.Fprintln(m.stderr, "moox setup: aborted; nothing was written")
			return exitAborted
		}
		if err != nil {
			fmt.Fprintf(m.stderr, "moox setup: %v\n", err)
			return exitError
		}
	}

	out := m.stdout
	if *dry {
		fmt.Fprintf(out, "moox setup --dry-run (mode %s, Claude hooks %s, theme %s):\n", p.Mode, yesNo(p.ClaudeHooks), p.Theme)
		for _, a := range p.Actions {
			fmt.Fprintln(out, "  "+a.Describe())
		}
		printNotes(m, p)
		return exitOK
	}
	insideTmux := m.env.Getenv("TMUX") != ""
	if insideTmux && p.Pending() {
		// Said BEFORE anything is written (G2-H12).
		fmt.Fprintln(out, "note: you are inside tmux; the running server keeps its config until it restarts, "+
			"but `prefix r` re-sources the file setup writes. Start moox from a terminal outside tmux (or `moox --nested`).")
	}
	report, err := setup.Apply(env, f, p)
	fmt.Fprintf(out, "moox setup (mode %s):\n", p.Mode)
	for _, l := range report {
		fmt.Fprintln(out, "  "+l)
	}
	printNotes(m, p)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox setup: %v\n", err)
		return exitError
	}
	if !p.Pending() {
		fmt.Fprintln(out, "moox is already set up here; nothing to do.")
	}

	// 7. The doctor's checks (the offline ones: no server is touched here).
	results := doctor.Run(m.ctx, m.doctorEnv(root, nil))
	warn := 0
	for _, r := range results {
		if r.Level >= doctor.Warn {
			warn++
			fmt.Fprintf(out, "  doctor: %s\n", r.Line())
		}
	}
	if warn == 0 {
		fmt.Fprintln(out, "  doctor: all checks pass")
	}
	// 8. The welcome: the five keys that matter.
	fmt.Fprint(out, welcome(p.Mode))
	return exitOK
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func printNotes(m *meta, p setup.Plan) {
	for _, n := range p.Notes {
		fmt.Fprintln(m.stdout, "note: "+n)
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(m.stdout, "warning: "+w)
	}
}

func welcome(mode setup.Mode) string {
	start := "`tmux` (or `moox`)"
	if mode == setup.Separate {
		start = "`moox`"
	}
	return "\nWelcome to moox. Start it with " + start + ". The keys that matter (prefix is C-a):\n" +
		"  prefix r        reload the config\n" +
		"  prefix C-s      save the workspace now\n" +
		"  prefix g        the tt sidebar (agents across sessions)\n" +
		"  prefix Space    popups\n" +
		"  prefix T        light/dark theme\n" +
		"Your own settings go in ~/.config/moox/local.conf. `moox doctor` checks everything.\n"
}

const uninstallUsage = `usage: moox uninstall [--yes] [--dry-run] [--purge]`

func cmdUninstall(m *meta, args []string) int {
	fs := flagSet(m, "uninstall", uninstallUsage)
	yes := fs.Bool("yes", false, "no confirmation")
	dry := fs.Bool("dry-run", false, "print what would be undone")
	purge := fs.Bool("purge", false, "also remove moox's state and config dirs (XS/moox, XC/moox)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(m.stderr, uninstallUsage)
		return exitUsage
	}
	root, err := m.rootDir()
	if err != nil {
		// Uninstall must work when the share root is gone (brew uninstall
		// ran first): the manifest has every path it needs.
		root = ""
	}
	env, err := m.setupEnv(root, true)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox uninstall: %v\n", err)
		return exitError
	}
	r, _ := m.roots()
	o := setup.UninstallOptions{DryRun: true, Purge: *purge, StateHome: r.State}
	preview, err := setup.Uninstall(env, o)
	if errors.Is(err, setup.ErrNotSetUp) {
		fmt.Fprintln(m.stdout, "moox uninstall: moox setup has not run here; nothing to undo")
		return exitOK
	}
	for _, l := range preview {
		fmt.Fprintln(m.stdout, "  "+l)
	}
	if *dry {
		return exitOK
	}
	if !*yes {
		if !m.stdinTTY {
			fmt.Fprintln(m.stderr, "moox uninstall: without a terminal, --yes is required")
			return exitUsage
		}
		if !confirm(m, "Undo these steps? [y/N] ", "y") {
			fmt.Fprintln(m.stderr, "moox uninstall: aborted; nothing was changed")
			return exitAborted
		}
	}
	o.DryRun = false
	report, err := setup.Uninstall(env, o)
	for _, l := range report {
		fmt.Fprintln(m.stdout, "  "+l)
	}
	if err != nil {
		fmt.Fprintf(m.stderr, "moox uninstall: %v\n", err)
		return exitError
	}
	return exitOK
}
