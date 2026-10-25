package main

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/MadAppGang/moox/internal/mooxroot"
)

// cmdRpaster runs the copy-image helper with the given arguments: moox's
// bundled rpaster from <root>/../../libexec, else one on PATH. The bundled
// copy is never linked onto PATH (V10: the separate madappgang/tap/rpaster
// formula owns bin/rpaster, and a second one made brew link none of moox's
// binaries), so this verb is how it is reached: `moox rpaster setup`,
// `moox rpaster doctor`, `moox rpaster install --remote HOST`.
func cmdRpaster(m *meta, args []string) int {
	root, _ := m.rootDir()
	p, bundled, err := mooxroot.Rpaster(root, m.lookPath)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox rpaster: %v\n", err)
		return exitError
	}
	if len(args) == 1 && args[0] == "--which" {
		where := "on PATH"
		if bundled {
			where = "bundled"
		}
		fmt.Fprintf(m.stdout, "%s (%s)\n", p, where)
		return exitOK
	}
	cmd := exec.CommandContext(m.ctx, p, args...)
	cmd.Env = m.environ
	cmd.Stdin, cmd.Stdout, cmd.Stderr = m.stdin, m.stdout, m.stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		fmt.Fprintf(m.stderr, "moox rpaster: %v\n", err)
		return exitError
	}
	return exitOK
}
