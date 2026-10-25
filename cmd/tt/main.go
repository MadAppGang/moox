// Command tt is a modal terminal UI (k9s / lazygit style) for managing tmux
// sessions: list, filter, attach, create, rename, and kill sessions.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/MadAppGang/moox/internal/sockets"
)

func main() {
	// Subcommand dispatch (§2.4), and the whole of the change to this file.
	// A BARE `tt` — the daily invocation — falls through to the existing path
	// below, unchanged, including resolveTheme() before bubbletea. So does
	// anything starting with "-", e.g. `tt --help`. See routeArgs in cli.go for
	// the precedence rule and cli_test.go for the three regression tests.
	if r := useGlobals(routeArgs(os.Args[1:])); r.kind == routeSubcommand {
		code, handled := runSubcommand(r.name, r.args, os.Stdout, os.Stderr)
		if !handled {
			fmt.Fprintf(os.Stderr, "tt: unknown command %q\n\n%s", r.name, usage)
			os.Exit(2)
		}
		os.Exit(code)
	}

	// Decide light vs dark before the TUI starts — the detection needs the
	// terminal to itself for one round trip. See resolveTheme.
	resolveTheme()
	// Then follow the terminal's light/dark switch live (theme_live.go).
	startThemeReports(os.Stdout)

	p := tea.NewProgram(newModel(), tea.WithAltScreen())

	final, err := p.Run()
	stopThemeReports(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tt:", err)
		os.Exit(1)
	}

	// The TUI has released the terminal. If a session was selected with
	// "enter", attach to it now — after Run() returns — so tmux can fully take
	// over the terminal.
	m, ok := final.(model)
	if !ok || m.selected == "" {
		return
	}

	if err := attach(m.selected); err != nil {
		fmt.Fprintln(os.Stderr, "tt:", err)
		os.Exit(1)
	}
}

// insideTarget decides how attach reaches the target server: true when $TMUX
// names that same server (switch this client), false outside tmux (exec an
// attach). Inside ANOTHER server it refuses (G2-H8): switch-client sent to a
// server this client is not on switches whichever of ITS clients tmux picks,
// and an attach from here would nest — the same rule as `moox` without
// --nested.
func insideTarget(tmuxEnv, socket string) (bool, error) {
	if tmuxEnv == "" {
		return false, nil
	}
	cur, _, _ := strings.Cut(tmuxEnv, ",")
	a, err1 := sockets.Canonical(cur)
	b, err2 := sockets.Canonical(socket)
	if err1 == nil && err2 == nil && a == b {
		return true, nil
	}
	return false, fmt.Errorf("the session is on tmux server %s, and this terminal is inside %s; run tt there or outside tmux", socket, cur)
}

// attach connects to the named session. Inside tmux it switches the client;
// outside tmux it replaces this process with `tmux attach-session` so tmux owns
// the terminal directly.
//
// Both go to the server tt was pointed at (moox §4.5): -u -S <socket>, never
// whatever the environment would pick.
func attach(name string) error {
	inside, err := insideTarget(os.Getenv("TMUX"), ttTarget().Socket)
	if err != nil {
		return err
	}
	if inside {
		// Already inside tmux: switch-client returns, so run it normally.
		cmd := tmuxCommand(context.Background(), "switch-client", "-t", name)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Outside tmux: replace this process entirely with the attach.
	t := ttTarget()
	bin, err := exec.LookPath(t.Bin)
	if err != nil {
		return err
	}
	args := append([]string{"tmux"}, t.Args("attach-session", "-t", name)...)
	return syscall.Exec(bin, args, os.Environ())
}
