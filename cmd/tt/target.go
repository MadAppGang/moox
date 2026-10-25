package main

// target.go: which tmux server tt talks to (moox §4.5, D8).
//
// Every tmux exec in tt goes through ttTarget(), a tmuxtarget.Target, so every
// one of them passes `-u -S <socket>`. The server is chosen ONCE per process,
// by the same rule everywhere:
//
//	--tmux-socket NAME|PATH  >  $TT_TMUX_SOCKET  >  the first field of $TMUX  >  -L default
//	--tmux-bin PATH          >  $TT_TMUX_BIN     >  tmux on PATH
//
// Inside tmux that is the server the pane lives on — today's behaviour — and
// outside it is never "whatever tmux decides": `TT_TMUX_SOCKET=work tt
// sessions` asks the work server and only it. The global flags are accepted
// before or after a subcommand and by the TUI (routeArgs peels them), and they
// are applied by useGlobals, which main calls on the routing decision.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// globalFlags are the flags every tt invocation accepts.
type globalFlags struct {
	// socket and bin are --tmux-socket and --tmux-bin, "" when absent.
	socket, bin string
}

// The process's globals, set once by useGlobals before anything runs.
var (
	globalsMu sync.Mutex
	globals   globalFlags
)

// useGlobals applies a routing decision's global flags and returns the route
// unchanged, so main's dispatch guard stays the one statement it always was.
func useGlobals(r route) route {
	globalsMu.Lock()
	globals = r.globals
	globalsMu.Unlock()
	return r
}

func currentGlobals() globalFlags {
	globalsMu.Lock()
	defer globalsMu.Unlock()
	return globals
}

// resolveTarget is a seam: tests substitute a fixed Target so nothing in the
// suite depends on (or reaches) the server the test runner happens to sit in.
var resolveTarget = func() (tmuxtarget.Target, error) {
	g := currentGlobals()
	return tmuxtarget.Resolve(tmuxtarget.OSEnv{}, "TT", g.socket, g.bin)
}

// ttTarget is the server this process talks to. A resolution failure (no tmux
// on PATH, an unresolvable $TMUX_TMPDIR) still yields a Target whose exec fails
// with a message, rather than a nil that every caller would have to check.
func ttTarget() tmuxtarget.Target {
	t, err := resolveTarget()
	if err != nil {
		g := currentGlobals()
		bin := g.bin
		if bin == "" {
			bin = "tmux"
		}
		return tmuxtarget.Target{Bin: bin, Socket: g.socket}
	}
	return t
}

// tmuxCommand builds one tmux exec on ttTarget(). tmuxtarget strips $TMUX so
// nothing but -S picks the server; it is put back when it names the SAME server,
// because a command that needs "the current client" (switch-client,
// display-popup) finds it through the client environment, exactly as before.
func tmuxCommand(ctx context.Context, args ...string) *exec.Cmd {
	t := ttTarget()
	cmd := t.Command(ctx, args...)
	if tm := os.Getenv("TMUX"); tm != "" {
		if sock, _, _ := strings.Cut(tm, ","); sock == t.Socket {
			cmd.Env = append(cmd.Env, "TMUX="+tm)
		}
	}
	return cmd
}

// ttPaths is the per-socket layout for ttTarget() (moox §4.2), or false when
// no layout can be built (no absolute $HOME).
func ttPaths() (sockets.Paths, bool) {
	p, err := sockets.For(ttTarget().Socket, sockets.Env(tmuxtarget.OSEnv{}))
	if err != nil {
		return sockets.Paths{}, false
	}
	return p, true
}
