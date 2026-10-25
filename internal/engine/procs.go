package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// osProcs is the real procLister: `ps`, run with LC_ALL=C so lstart has one
// spelling (the shell writes the by-pid start tokens the same way, §7).
type osProcs struct{}

func psCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd
}

// Table is the one ps of a save (B25).
func (osProcs) Table(ctx context.Context) ([]byte, error) {
	return psCommand(ctx, capture.PSArgs()).Output()
}

// Jobs is the pid/ppid/pgid/tpgid table of the idle-shell check (B47).
func (osProcs) Jobs(ctx context.Context) ([]byte, error) {
	return psCommand(ctx, capture.JobsArgs()).Output()
}

// Env is one single-pid `ps eww` (B12). Never a multi-pid ps -p.
func (osProcs) Env(ctx context.Context, pid int) ([]byte, error) {
	return psCommand(ctx, capture.PSEnvArgs(pid)).Output()
}

// Alive is kill(pid, 0): EPERM still means the process exists.
func (osProcs) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// scriptRunner runs one vendored continuity script (cc_freeze.sh reclaim and
// ledger-seed, cc_tab_reconcile.sh --all) with an explicit environment.
type scriptRunner interface {
	Run(ctx context.Context, env []string, path string, args ...string) ([]byte, error)
}

type osScripts struct{}

func (osScripts) Run(ctx context.Context, env []string, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	return cmd.Output()
}

// scriptEnv is the environment a script the engine starts runs with: the
// process's own minus everything that could steer it at another server or
// at real state (TMUX, MOOX_*, HOME, XDG_*), plus this engine's roots and the
// MOOX_* contract of §4.3 for THIS server, so cc_common.sh's TMUX_CMD and
// _cc_socket_ns resolve to it (§4.2) and a test engine's scratch HOME is the
// HOME the script sees.
func (e *Engine) scriptEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "TMUX", k == "TMUX_PANE", k == "HOME", k == "CLAUDE_CONFIG_DIR",
			strings.HasPrefix(k, "XDG_"), strings.HasPrefix(k, "MOOX_"), strings.HasPrefix(k, "CC_"):
			continue
		}
		out = append(out, kv)
	}
	env := e.opts.Env
	if env == nil {
		env = tmuxtarget.OSEnv{}
	}
	if r, err := sockets.RootsFrom(env); err == nil {
		out = append(out, "HOME="+r.Home, "XDG_STATE_HOME="+r.State, "XDG_DATA_HOME="+r.Data, "XDG_CONFIG_HOME="+r.Config)
	}
	return append(out,
		"MOOX_SOCKET="+e.paths.Socket, "MOOX_ACTIVE=1", "MOOX_STATE_KEY="+e.paths.Key,
		"MOOX_STATE_DIR="+e.paths.State, "MOOX_PANES_DIR="+e.paths.Panes,
		"MOOX_LAUNCH_DIR="+e.paths.Launch, "MOOX_PENDING_DIR="+e.paths.Pending)
}

// Heartbeat runs `cc_freeze.sh heartbeat` (§5.7, B23: the ledger tick, the
// thaw confirmation and the sweep kick that pre_save.sh ran on every
// resurrect save) under ctx, with this server's script environment, its
// output discarded. The script path comes from @moox-root; with no root it
// runs nothing and returns an error (the saver logs it).
func (e *Engine) Heartbeat(ctx context.Context) error {
	out, err := e.tmux.Run(ctx, "show-options", "-gqv", "@moox-root")
	if err != nil {
		return err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return errors.New("engine: heartbeat: @moox-root is unset")
	}
	_, err = e.scripts.Run(ctx, e.scriptEnv(), root+"/plugins/tmux-claude-continuity/scripts/cc_freeze.sh", "heartbeat")
	return err
}
