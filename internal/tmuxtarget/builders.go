package tmuxtarget

import "strconv"

// The builders below are the only way engine code produces argv for the tmux
// commands whose arguments tmux format-expands (§2.4 LiteralArgs, §6.4, E2):
//
//	new-session -s <name> -n <window> -c <dir>
//	new-window  -n <name> -c <dir>
//	split-window -c <dir>
//	select-pane -T <title>
//	rename-window / rename-session <name>
//
// Those positions take a Lit, so user-derived text cannot reach them without
// going through Literal. Targets (-t) are NOT format-expanded (E7) and are
// plain strings; restore code addresses everything by id ($N, @N, %N).
//
// Env values passed with -e are not format-expanded either, and a ';' inside
// one arrives intact (Q5); a trailing ';' is handled by Chain like any other
// argument, so the builders never escape anything themselves — they are used
// inside Chain or passed to Run directly, and Run's single command has no
// separator to confuse.

// EnvVar is one -e NAME=VALUE.
type EnvVar struct{ Name, Value string }

func envArgs(env []EnvVar) []string {
	out := make([]string, 0, 2*len(env))
	for _, e := range env {
		out = append(out, "-e", e.Name+"="+e.Value)
	}
	return out
}

// NewSession is `new-session -d`.
type NewSession struct {
	// Group, when set, is -t <session id>: the new session joins that
	// session's group (S5, T11).
	Group      string
	Name       Lit
	WindowName Lit
	Cwd        Lit
	Width      int // -x, 0 = omitted
	Height     int // -y, 0 = omitted
	Env        []EnvVar
	// PrintFormat, when set, adds -P -F <format> so the exec prints the new
	// ids (the restore chain resumes after the last printed id, E5).
	PrintFormat string
	// Command is the pane's shell-command, if any (not format-expanded).
	Command []string
}

// Args builds the argv (without the -u -S prefix Target adds).
func (n NewSession) Args() []string {
	a := []string{"new-session", "-d"}
	if n.PrintFormat != "" {
		a = append(a, "-P", "-F", n.PrintFormat)
	}
	if !n.Name.IsZero() {
		a = append(a, "-s", n.Name.String())
	}
	if n.Group != "" {
		a = append(a, "-t", n.Group)
	}
	if !n.WindowName.IsZero() {
		a = append(a, "-n", n.WindowName.String())
	}
	if !n.Cwd.IsZero() {
		a = append(a, "-c", n.Cwd.String())
	}
	if n.Width > 0 {
		a = append(a, "-x", strconv.Itoa(n.Width))
	}
	if n.Height > 0 {
		a = append(a, "-y", strconv.Itoa(n.Height))
	}
	a = append(a, envArgs(n.Env)...)
	return append(a, n.Command...)
}

// NewWindow is `new-window -d`.
type NewWindow struct {
	Target      string // -t, e.g. "$3:2" (an id-based target, E7)
	Name        Lit
	Cwd         Lit
	Env         []EnvVar
	PrintFormat string
	Command     []string
}

// Args builds the argv.
func (n NewWindow) Args() []string {
	a := []string{"new-window", "-d"}
	if n.PrintFormat != "" {
		a = append(a, "-P", "-F", n.PrintFormat)
	}
	if n.Target != "" {
		a = append(a, "-t", n.Target)
	}
	if !n.Name.IsZero() {
		a = append(a, "-n", n.Name.String())
	}
	if !n.Cwd.IsZero() {
		a = append(a, "-c", n.Cwd.String())
	}
	a = append(a, envArgs(n.Env)...)
	return append(a, n.Command...)
}

// SplitWindow is `split-window -d`, or without -d when Follow is set.
type SplitWindow struct {
	Target     string // -t, a pane or window id
	Horizontal bool   // -h (side by side); default -v
	// Follow omits -d: the new pane becomes the window's active pane, so the
	// next split of `-t <@wid>` in the same chain splits it (§6.4 exec C: the
	// window's pane order then equals row order).
	Follow      bool
	Cwd         Lit
	Env         []EnvVar
	PrintFormat string
	Command     []string
}

// Args builds the argv.
func (s SplitWindow) Args() []string {
	a := []string{"split-window"}
	if !s.Follow {
		a = append(a, "-d")
	}
	if s.Horizontal {
		a = append(a, "-h")
	} else {
		a = append(a, "-v")
	}
	if s.PrintFormat != "" {
		a = append(a, "-P", "-F", s.PrintFormat)
	}
	if s.Target != "" {
		a = append(a, "-t", s.Target)
	}
	if !s.Cwd.IsZero() {
		a = append(a, "-c", s.Cwd.String())
	}
	a = append(a, envArgs(s.Env)...)
	return append(a, s.Command...)
}

// SelectPaneTitle is `select-pane -t <pane> -T <title>`.
func SelectPaneTitle(pane string, title Lit) []string {
	return []string{"select-pane", "-t", pane, "-T", title.String()}
}

// RenameWindow is `rename-window -t <window> <name>`.
func RenameWindow(window string, name Lit) []string {
	return []string{"rename-window", "-t", window, name.String()}
}

// RenameSession is `rename-session -t <session> <name>`.
func RenameSession(session string, name Lit) []string {
	return []string{"rename-session", "-t", session, name.String()}
}

// SwitchClient is `switch-client -c <client> -t <session id>`. Its -c names a
// client (a tty path), which tmux does not format-expand; the builder exists
// so engine argv never spells -c by hand (§2.4 LiteralArgs).
func SwitchClient(client, target string) []string {
	return []string{"switch-client", "-c", client, "-t", target}
}

// UnsetSessionEnv is `set-environment -t <$sid> -r NAME` for each name, as one
// chain (§4.4: -e on new-session writes the SESSION env, E1, so it is cleared
// by id right after; never by =name, Q3).
func UnsetSessionEnv(session string, names ...string) []string {
	cmds := make([][]string, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, []string{"set-environment", "-t", session, "-r", n})
	}
	return Chain(cmds...)
}
