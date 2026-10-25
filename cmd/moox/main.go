// Command moox is the agentic tmux distribution's front door (architecture
// §8). main.go is wiring only: a shared meta (streams, the environment, a
// signal-cancelled context) and one func(*meta, []string) int per verb, on
// stdlib flag (cli-architecture.md's meta-command pattern).
//
// Every verb reads the environment through meta, never os.Getenv, so a test
// runs a verb in process over a scratch HOME and nothing reaches the user's
// real files or servers.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/term"

	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Exit codes (§12.4).
const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitNotApplic  = 3
	exitInvalid    = 4
	exitBusy       = 75
	exitAborted    = 130
	exitTmuxAbsent = 69
)

// meta is what every verb shares.
type meta struct {
	ctx context.Context
	// environ is the whole environment the verb runs in; env reads it.
	environ []string
	env     tmuxtarget.Env
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	// stdinTTY, stdoutTTY: a form or confirmation can be shown; attach may exec.
	stdinTTY, stdoutTTY bool
	cwd                 string
	// exe is this binary (os.Executable), for the detached restart re-exec
	// and for finding the share root.
	exe string
}

type verb func(m *meta, args []string) int

var verbs map[string]verb

func init() {
	verbs = map[string]verb{
		"import":    cmdImport,
		"save":      cmdSave,
		"setup":     cmdSetup,
		"uninstall": cmdUninstall,
		"doctor":    cmdDoctor,
		"restart":   cmdRestart,
		"restore":   cmdRestore,
		"snapshot":  cmdSnapshot,
		"root":      cmdRoot,
		"rpaster":   cmdRpaster,
		"version":   cmdVersion,
		"start":     cmdStart,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	m := newMeta(ctx, os.Environ())
	code := run(m, os.Args[1:])
	stop()
	os.Exit(code)
}

func newMeta(ctx context.Context, environ []string) *meta {
	m := &meta{ctx: ctx, environ: environ, env: envMap(environ), stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		stdinTTY: isTTY(os.Stdin), stdoutTTY: isTTY(os.Stdout)}
	m.cwd, _ = os.Getwd()
	if exe, err := os.Executable(); err == nil {
		m.exe = exe
	}
	return m
}

func envMap(environ []string) tmuxtarget.MapEnv {
	out := tmuxtarget.MapEnv{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// isTTY asks the terminal driver (isatty), not the file mode: /dev/null and
// other character devices are not terminals (G2-M2), and automation with
// redirected stdio must not get interactive setup or an attach.
func isTTY(f *os.File) bool { return term.IsTerminal(f.Fd()) }

func run(m *meta, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		switch {
		case len(args) > 0 && (args[0] == "--version" || args[0] == "-V"):
			return cmdVersion(m, nil)
		case len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help"):
			usage(m.stdout)
			return exitOK
		}
		return cmdStart(m, args)
	}
	v, ok := verbs[args[0]]
	if !ok {
		fmt.Fprintf(m.stderr, "moox: unknown verb %q\n", args[0])
		usage(m.stderr)
		return exitUsage
	}
	return v(m, args[1:])
}

const usageText = `usage:
  moox [-L NAME | -S PATH] [-s SESSION] [--nested]   start or attach (default verb)
  moox setup [--yes] [--dry-run] [--mode replace|separate] [--claude-hooks|--no-claude-hooks] [--theme auto|light|dark] [--accessible]
  moox doctor [-L NAME | -S PATH | --all] [--json] [--fix stale|legacy-hooks|restoring|all] [--dry-run]
  moox uninstall [--yes] [--dry-run] [--purge]
  moox restart [-L NAME | -S PATH] [--yes] [--force] [--snapshot FILE]
  moox save [-L NAME | -S PATH] [--reason TEXT] [--wait DURATION] [--direct] [--force]
  moox restore [-L NAME | -S PATH] [--snapshot FILE] [--dry-run] [--stub-commands] [--wait-launch]
  moox import --from DIR|FILE [--contents FILE] [--attach-live] [--out FILE | --publish (-L NAME|-S PATH) [--replace-live] [--yes]] [--report FILE]
  moox snapshot show [-L NAME | -S PATH] [--json]
  moox snapshot unpin [-L NAME | -S PATH] FILE|--all
  moox root
  moox rpaster [--which | ARGS...]   run the bundled copy-image helper (libexec; else rpaster on PATH)
  moox version | --version`

func usage(w io.Writer) { fmt.Fprintln(w, usageText) }

// rootDir is the share root: $MOOX_ROOT when it holds moox.conf (the gate
// exports it into every pane of a moox server, and tests and dev trees set
// it), else the stable root of this executable (§3.2).
func (m *meta) rootDir() (string, error) {
	if r := m.env.Getenv("MOOX_ROOT"); r != "" && filepath.IsAbs(r) {
		if _, err := os.Stat(filepath.Join(r, "moox.conf")); err == nil {
			return filepath.Clean(r), nil
		}
	}
	return mooxroot.Stable(m.exe, m.cwd)
}

func cmdRoot(m *meta, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(m.stderr, "usage: moox root")
		return exitUsage
	}
	r, err := m.rootDir()
	if err != nil {
		fmt.Fprintf(m.stderr, "moox root: %v\n", err)
		return exitError
	}
	fmt.Fprintln(m.stdout, r)
	return exitOK
}

func cmdVersion(m *meta, _ []string) int {
	fmt.Fprintln(m.stdout, "moox "+mooxroot.VersionString())
	return exitOK
}
