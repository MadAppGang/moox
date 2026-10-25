package main

// The command-line surface, and the composition root for the observer.
//
// Two jobs, kept in one file because they are one concern — turning an argv
// into one answer:
//
//  1. routeArgs + runSubcommand: the §2.4 dispatch. Everything here is a pure
//     function of argv plus injected writers, so all three of §2.4's properties
//     are testable without launching a TUI or reading a terminal.
//  2. newObserver: the wiring. This is the ONLY place in the binary that knows
//     both an adapter and the core, which is what "package main is the
//     composition root" means in practice (§2.2).
//
// What must never appear here: a call into resolveTheme, newModel or tea. The
// subcommand path writes machine-readable bytes to stdout, and resolveTheme
// writes an OSC 11 query to the tty and reads the reply (theme.go:55) — doing
// that before emitting JSON would corrupt the output (§2.4 property 3).
// cli_test.go asserts the absence structurally rather than trusting this
// comment.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/agentscli"
	"github.com/MadAppGang/moox/internal/observer/adapters/doctorfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/jobsfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
	"github.com/MadAppGang/moox/internal/observer/adapters/mcpserve"
	"github.com/MadAppGang/moox/internal/observer/adapters/proclive"
	"github.com/MadAppGang/moox/internal/observer/adapters/procmember"
	"github.com/MadAppGang/moox/internal/observer/adapters/registryfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockclient"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/adapters/tmuxpanes"
	"github.com/MadAppGang/moox/internal/observer/adapters/transcriptfs"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// usage is what an unknown command prints. It lists only what exists: a
// usage message naming subcommands that are not wired yet would be a lie the
// next phase has to remember to correct.
const usage = `usage: tt [<command>]

With no arguments, tt opens the interactive tmux session manager (the TUI).

Commands:
  sessions      report the Claude Code sessions on this machine (read-only)
  observe       serve the session event stream on a unix socket (read-only)
  mcp           serve the session observer to an MCP client on stdio (read-only)
  panel-render  draw the session panel once, from a snapshot (read-only)
  serve         the per-server moox daemon: --ensure, --foreground, --stop, --status

Global flags, accepted before or after the command and by the TUI:
  --tmux-socket=NAME|PATH  the tmux server to talk to: a name (as tmux -L) or a
                           socket path (as tmux -S). Default $TT_TMUX_SOCKET,
                           else the server of $TMUX, else the default server
  --tmux-bin=PATH          the tmux binary. Default $TT_TMUX_BIN, else tmux on PATH
  --version                print "tt <version> (moox)" and exit
  --help                   print this text and exit

Every tmux command tt runs passes -u -S <socket>, so one tt process only ever
talks to one server. State (the observer socket, its pid file, the agents
cache, the panel mode) is per server: ~/.local/state/tt for the default
server, ~/.local/state/tt/sockets/<name>@<hash> for any other.

Run "tt <command> --help" for the flags of that command.
`

const mcpUsage = `usage: tt mcp [flags]

Serves the session observer to an MCP client over stdio: JSON-RPC 2.0 on stdin
and stdout, four tools, every one of them READ-ONLY. It answers questions about
sessions; it never answers a prompt, sends input, renames, attaches, kills, or
reads a transcript body.

It first dials the observer socket, so a running "tt observe" is shared rather
than duplicated. With no server, or one whose schema this build does not speak,
it answers from its own on-demand reads instead — no watch is armed either way.

Tools: list_sessions, get_session, session_counts, describe_sources.

Flags:
  --socket-dir=DIR    where the observer socket lives
                      (default $XDG_STATE_HOME/tt, else ~/.local/state/tt)
  --local             do not dial the socket; answer from this process's own
                      reads even when a server is running
  --timeout=DURATION  bound on one corroboration spawn (default 5s)
  --publish-job-text  publish the free-text job fields (needs, detail) to MCP
                      clients in full. Off by default: those strings come from
                      the same job record whose intent/output/suggestedReply are
                      never read at all, and this is the one surface whose
                      audience is other agents rather than you. Withheld, they
                      read "[withheld: 63 chars]" — presence and length, no
                      sentence — while "tt sessions" and the TUI panel go on
                      showing them in full either way

Nothing auto-starts "tt observe". Register this command with an MCP client, e.g.

  claude mcp add tt -- tt mcp

Exit codes: 0 the client disconnected, 1 it could not start, 2 a usage error.
`

const observeUsage = `usage: tt observe [flags]

Watches the Claude Code state tree and serves every change as newline-delimited
JSON frames on a unix socket. Read-only: it opens no file for writing except its
own socket and pid file in ~/.local/state/tt, and it starts nothing.

It runs until it is killed or the machine reboots. There is no idle timeout and
no timer of any kind: between events the process executes no code at all.

Flags:
  --socket-dir=DIR    where the socket and pid file live
                      (default $XDG_STATE_HOME/tt, else ~/.local/state/tt)
  --stop              SIGTERM the server named by the pid file, then exit —
                      but only after corroborating that the pid IS this server
                      (the socket answers and declares it, or the process start
                      instant matches the record). An uncorroborated pid is
                      refused, not signalled: the pid file survives crashes and
                      reboots, and pids get recycled
  --timeout=DURATION  bound on one corroboration spawn (default 5s)
  --pane              render the session panel in this tmux pane and serve
                      nothing (a renderer; it never binds the socket)
  --auto              with --pane: this pane was created by the @tt_obs_scope
                      hooks. It attaches to a running tt observe or waits for
                      one, and never watches for itself
  --status-line=MODE  write the aggregate counts to the @tt_status tmux option:
                      auto (default, only when @tt_status appears in
                      status-right, status-left or status-format), always,
                      never

Nothing ever starts this server automatically. Connect with any NDJSON reader:

  nc -U ~/.local/state/tt/observer.sock

Exit codes: 0 served and shut down cleanly (and 0 when another server already
holds the socket), 1 it could not start — or --stop refused an uncorroborated
pid — 2 a usage error.
`

const sessionsUsage = `usage: tt sessions [flags]

Reports every Claude Code session this machine can see. Read-only: it opens no
file for writing, starts nothing, and arms no watch. One shot, then it exits.

Flags:
  --json              emit the machine-readable snapshot (schema 1) on stdout
  --all               include dead sessions and sessions another session launched
                      (spawnedBy set), and drop every other narrowing
  --waiting           only sessions needing attention
  --kind=KIND         only interactive or only background sessions
  --cwd-prefix=PATH   only sessions whose cwd is under PATH
  --include-dead      include sessions with positive evidence of death
  --enrich            run the transcript enrichment pass (the default)
  --no-transcript     read no transcript at all: no enrichment pass and no job
                      timing tail, so transcriptCollected is false and
                      lastActivityAt null on every session
  --refresh           re-read the corroboration source before answering
  --timeout=DURATION  give up reading after this long (default 5s)
  --doctor            diagnose the FORMATS instead of listing sessions: read one
                      live example of each and report, per assumption, whether it
                      still holds. Combines with --json.

Exit codes: 0 an answer (including "no sessions"), 1 nothing could be read,
2 a usage error. --doctor adds 3 (something degrades) and 4 (something is
broken), which are distinct from 1 and 2 on purpose.
`

// routeKind is what an argv means. Two values, because `tt` has exactly two
// modes and a third would be a silent fall-through.
type routeKind int

const (
	// routeTUI is the daily invocation: the existing path, untouched.
	routeTUI routeKind = iota
	// routeSubcommand is "argv[0] names a command", whether or not that
	// command exists. An unrecognised name is routed here on purpose so it can
	// be REFUSED (exit 2) rather than opening a full-screen TUI over what the
	// user thought was a query.
	routeSubcommand
)

// route is the decision, separated from acting on it so it can be tested
// exhaustively without side effects.
type route struct {
	kind routeKind
	name string
	args []string
	// globals are the flags every invocation accepts (--tmux-socket,
	// --tmux-bin, moox §4.5), peeled from wherever they appeared.
	globals globalFlags
}

// The two names routeArgs dispatches that are not subcommands a user could
// mistake for a session name: they print and exit, and never open the TUI —
// `tt --version` is what `brew test` runs, with no terminal.
const (
	cmdVersion = "--version"
	cmdHelp    = "--help"
)

// globalFlagError names a global flag given without its value; runSubcommand
// refuses it as a usage error rather than letting it fall into the TUI.
const globalFlagError = "--tmux-flag-without-value"

// peelGlobals removes --tmux-socket and --tmux-bin (either "--x v" or
// "--x=v", one or two dashes) from anywhere before a "--" terminator, so they
// are accepted before or after the subcommand and by the TUI. A flag missing
// its value reports ok=false.
func peelGlobals(argv []string) (globalFlags, []string, bool) {
	var g globalFlags
	var rest []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || (name != "tmux-socket" && name != "tmux-bin") {
			rest = append(rest, a)
			continue
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return g, rest, false
			}
			i++
			value = argv[i]
		}
		if value == "" {
			return g, rest, false
		}
		if name == "tmux-socket" {
			g.socket = value
		} else {
			g.bin = value
		}
	}
	return g, rest, true
}

// routeArgs decides what a command line means, given os.Args[1:].
//
// Precedence, explicitly, because tt took no subcommands before this change and
// an implicit rule here is a daily-use hazard:
//
//  0. the global flags (--tmux-socket, --tmux-bin) are peeled first, from
//     wherever they appear (moox §4.5); they decide WHICH server, never WHAT
//     runs, so they ride along on every route.
//  1. NOTHING left             -> the TUI. This is the guarantee; it is first.
//  2. --version, --help or -h  -> printed and exited (cmdVersion/cmdHelp);
//     never the TUI, so `tt --version` works without a terminal.
//  3. first word "-"-led       -> the TUI, which owns the flag namespace it
//     always owned.
//  4. anything else            -> a subcommand name, recognised or not.
//
// Rule 4 is total: every argv that is neither empty nor flag-led is a
// subcommand, so nothing can be swallowed into the TUI by accident. That
// includes the empty string, which is a name that matches no command and is
// therefore refused rather than ignored. A global flag missing its value is
// refused too (globalFlagError), rather than read as a TUI flag.
func routeArgs(argv []string) route {
	g, rest, ok := peelGlobals(argv)
	if !ok {
		return route{kind: routeSubcommand, name: globalFlagError, globals: g}
	}
	if len(rest) == 0 {
		return route{kind: routeTUI, globals: g}
	}
	switch rest[0] {
	case "--version", "-version":
		return route{kind: routeSubcommand, name: cmdVersion, args: rest[1:], globals: g}
	case "--help", "-help", "-h":
		return route{kind: routeSubcommand, name: cmdHelp, args: rest[1:], globals: g}
	}
	if strings.HasPrefix(rest[0], "-") {
		return route{kind: routeTUI, globals: g}
	}
	return route{kind: routeSubcommand, name: rest[0], args: rest[1:], globals: g}
}

// runSubcommand runs a named command. The bool is "this name is known": false
// means the caller prints the usage error, so that an unknown command has ONE
// message no matter how many commands are added.
func runSubcommand(name string, args []string, stdout, stderr io.Writer) (int, bool) {
	switch name {
	case cmdVersion:
		// moox §12.4: `tt <semver> (moox)`, on stdout, exit 0.
		fmt.Fprintf(stdout, "tt %s (moox)\n", observer.ResolveVersion())
		return jsonout.ExitOK, true
	case cmdHelp:
		fmt.Fprint(stdout, usage)
		return jsonout.ExitOK, true
	case globalFlagError:
		fmt.Fprintf(stderr, "tt: --tmux-socket and --tmux-bin need a value\n\n%s", usage)
		return jsonout.ExitUsage, true
	case "serve":
		return runServe(args, stdout, stderr), true
	case "sessions":
		return runSessions(args, stdout, stderr), true
	case "observe":
		return runObserve(args, stdout, stderr), true
	case "mcp":
		return runMCP(args, stdout, stderr), true
	case "panel-render":
		// panel_render.go: it draws with the panel's own renderer, and its
		// theme is forced from flags — resolveTheme is never called on this
		// path either.
		return runPanelRender(args, stdout, stderr), true
	default:
		return 0, false
	}
}

// runSessions is S1: parse, wire, ask once, print, return an exit code.
func runSessions(args []string, stdout, stderr io.Writer) int {
	opts, timeout, doctor, code, ok := parseSessionsFlags(args, stdout, stderr)
	if !ok {
		return code
	}
	if doctor {
		return runDoctor(opts, timeout, stdout, stderr)
	}

	clock := observer.SystemClock{}
	obs, err := newObserver(clock, corroborationBudget(timeout))
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}

	// A bound on the whole read, not per source. The context is the only
	// deadline mechanism used: §9.3a bans time.After/Sleep/Tick/NewTicker
	// outside one allowlisted site, and a one-shot command that armed a timer
	// would be the first step back towards a poll loop.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	code = jsonout.Run(ctx, obs, opts, stdout, stderr)

	// Do not exit while a corroboration spawn is still unwinding.
	//
	// FOUND BY THE LIVE RUN, not by a fixture. `--timeout=80ms --refresh`
	// against a ~150 ms spawn expired the caller's context, so the spawn
	// goroutine was still holding the cross-process lock when main returned —
	// and the lock file was left on disk, where §4.3(b)'s abandoned-lock rule
	// would make every other tt process on the machine answer without
	// corroboration until its mtime aged past spawnTimeout + 1 s.
	//
	// The spawn's context is a child of the read context, so cancelling the read
	// IS the kill, and AwaitCorroboration is the Wait: immediate in practice,
	// and bounded here rather than trusted. On the ordinary path there is
	// nothing in flight and this returns at once.
	//
	// The explicit cancel is load-bearing after the wedged-CLI fix, not
	// tidiness. The rebuild now stops waiting on corroboration after
	// observer.CorroborationWait instead of after the whole read budget, so a
	// wedged `claude agents --json` is STILL RUNNING when the answer is printed
	// — and without this line the drain below would sit out the remainder of
	// the spawn's timeout, moving the 5 s the fix removed from the read to the
	// exit. Nothing after jsonout.Run uses ctx, and cancel is idempotent, so
	// cancelling here and again in the defer is safe.
	cancel()

	drain, cancelDrain := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDrain()
	obs.AwaitCorroboration(drain)

	return code
}

// runDoctor is §9.5: the one surface whose job is to SHOUT.
//
// It shares `tt sessions`'s wiring and none of its output. The reason it is a
// flag on `sessions` rather than a command of its own is the daily-use rule in
// §2.4 — every subcommand added to tt is a word the user can no longer use as a
// tmux session name in the TUI — and the reason it shares the wiring is that a
// diagnosis of a DIFFERENTLY configured observer than the one that answers
// every other invocation would diagnose the wrong thing.
//
// The corroboration source is wired, so `claude agents --json` is spawned ONCE
// per run, for the terminal-state gate. That is the check §9.5 calls "the
// single check most likely to catch a Claude behaviour change", and it cannot
// be made without a listing. A wedged CLI costs the doctor the spawn timeout
// and two checks, both of which then report `skip` — which is the honest answer
// and also, measured during this build, a state this machine actually reaches.
func runDoctor(opts jsonout.Options, timeout time.Duration, stdout, stderr io.Writer) int {
	clock := observer.SystemClock{}
	paths, err := claudePaths()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}
	policy := resolvePolicy(paths)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	report := doctorfs.Run(ctx, doctorConfig(paths, policy, clock, timeout))

	if opts.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(wire.NewDoctorReport(report, wire.Options{TT: opts.TT})); err != nil {
			fmt.Fprintf(stderr, "tt: %v\n", err)
			return jsonout.ExitNoRead
		}
		return doctorfs.ExitCode(report)
	}

	if err := doctorfs.RenderText(stdout, report); err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}
	return doctorfs.ExitCode(report)
}

// corroborationBudget bounds ONE corroboration spawn inside a one-shot read's
// deadline, and it exists because of a defect the AC14 spot-check found on a
// machine whose `claude agents --json` was wedged.
//
// THE DEFECT. `--timeout` set both the whole-read deadline AND the spawn's, so
// they expired together: with the vendor CLI hung, `tt sessions --json
// --refresh` spent the entire budget inside the spawn, reported the refresh as
// rejected — correctly — and then had no context left to answer with. Measured:
// `--timeout=20s` produced "context deadline exceeded" and NO OUTPUT after
// 20.008 s, while the same read without --refresh answered 37 sessions in
// 0.53 s. An explicitly requested corroboration that cannot be taken must cost
// the corroboration, never the answer — which is the same rule the wedged-CLI
// fix applied to the watch-driven rebuild, applied here to the --refresh path.
//
// THE RULE. The spawn gets at most HALF the read budget, and never more than
// the §4.3 production bound of 5 s. Half rather than "budget minus epsilon"
// because the authoritative reads that follow are what the remaining half is
// for: on this machine they take ~0.5 s against 37 sessions and 60 panes.
func corroborationBudget(read time.Duration) time.Duration {
	const productionSpawnBound = 5 * time.Second
	half := read / 2
	if half > productionSpawnBound {
		return productionSpawnBound
	}
	if half <= 0 {
		// A read budget too small to halve keeps the whole of it: the caller
		// asked for something very short and gets one attempt at it rather than
		// a zero-length timeout, which would fail every spawn instantly.
		return read
	}
	return half
}

// doctorConfig is the doctor's wiring, in one place, because two surfaces
// construct it — `tt sessions --doctor` and the MCP `doctor` tool — and a
// diagnosis that differed between them would be a diagnosis of the wiring.
func doctorConfig(paths observer.Paths, policy observer.Policy, clock observer.Clock, timeout time.Duration) doctorfs.Config {
	return doctorfs.Config{
		Paths:    paths,
		StateDir: ttStateDir(),
		Agents: agentscli.New(agentscli.Config{
			StateDir:     ttStateDir(),
			ClaudeRoot:   paths.ClaudeRoot,
			SpawnTimeout: timeout,
			StaleAfterMs: policy.StaleAfterMs,
			TT:           observer.ResolveVersion(),
			Clock:        clock,
		}),
		RetentionMs: policy.TranscriptRetentionMs,
		Now:         clock.Now(),
	}
}

// runDoctorReport is the MCP tool's entry point: the same reads, no rendering.
// A path that cannot even be resolved is reported as a report with one failing
// check rather than as an error, because the MCP tool's contract is an answer
// and "I could not find your home directory" is one.
func runDoctorReport(ctx context.Context, timeout time.Duration) observer.DoctorReport {
	clock := observer.SystemClock{}
	paths, err := claudePaths()
	if err != nil {
		return observer.DoctorReport{
			TakenAt: clock.Now(),
			Checks: []observer.DoctorCheck{{
				ID: "paths", Title: "the Claude Code directories can be located",
				Cite: "§5.2", Status: observer.DoctorFail, Detail: err.Error(),
			}},
		}
	}
	return doctorfs.Run(ctx, doctorConfig(paths, resolvePolicy(paths), clock, timeout))
}

// observeSubscriberBuffer is §5.3's per-connection queue depth, and it is set
// HERE rather than in sockserve because the queue belongs to the watch loop
// (§7.2 owns the one bounded queue per subscriber, and the server deliberately
// adds no second one). 256 frames at ~1 KB a frame is a quarter of a megabyte
// per stuck client before it is resynced, against a measured event rate of
// three per nine minutes.
const observeSubscriberBuffer = 256

// runObserve is S3: bind, arm the watch, serve frames until killed.
//
// It is the only long-lived surface tt has. Everything about its lifecycle is
// the user's override: no idle timeout, no timer, and nothing that auto-starts
// it. The three mechanisms that replace the idle timer are all here — the pid
// file (written by sockserve.Listen), --stop, and a message naming the socket
// and the pid so the user knows what they started.
func runObserve(args []string, stdout, stderr io.Writer) int {
	f, code, ok := parseObserveFlags(args, stdout, stderr)
	if !ok {
		return code
	}
	if f.Stop {
		return runObserveStop(f.Dir, stderr)
	}
	if f.Pane {
		// S4's renderer (§5.5b). It is in pane.go rather than here because it
		// is the one subcommand that IS a TUI, and §2.4 property 3 keeps
		// resolveTheme and tea out of this file — cli_test.go asserts it.
		return runObservePane(paneOptions{
			Pane:      os.Getenv("TMUX_PANE"),
			SocketDir: f.Dir,
			Scope:     tmuxScope(),
			Auto:      f.Auto,
		}, stdout, stderr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if observeTied() {
		// D8: a per-server observer lives exactly as long as its server
		// (F1: one started by the sidebar's button outlived it).
		tieToServer(ctx, cancel, ttTarget(), stderr)
	}
	f.Role = wire.RoleObserve
	return serveObserve(ctx, f, stderr)
}

// serveObserve is runObserve's server half: the whole production wiring — the
// core, the kqueue watcher, the directory lister, the watch loop, the socket
// server and the status-line writer — from flags to shutdown.
//
// It is split out for ONE reason, and the reason is a defect. The independent
// black-box suite found that `tt observe` on a machine with no visible session
// sent `hello` and then nothing, ever, while every in-repo test passed: they
// all built the loop, the watcher and the server by hand, and none of them
// connected a client to the composition this function performs. A test now
// drives THIS function against a hermetic tree (observe_wiring_test.go), so the
// next defect of that shape cannot hide behind a seam again.
//
// parent is context.Background() in production, where SIGTERM and SIGINT are
// the only way the server ends. A test passes a context it can cancel.
func serveObserve(parent context.Context, f observeFlags, stderr io.Writer) int {
	dir, timeout := f.Dir, f.Timeout
	clock := observer.SystemClock{}
	st, err := newObserverStack(clock, timeout)
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}

	watcher, note := newWatcher()
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:             st.obs,
		Refresh:          st.obs,
		Watch:            watcher,
		Tree:             watchkq.OSTree{},
		Jobs:             st.jobs,
		Clock:            clock,
		Paths:            st.paths,
		SubscriberBuffer: observeSubscriberBuffer,
	})
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}

	srv, err := sockserve.New(sockserve.Config{
		Dir:        dir,
		ClaudeRoot: st.paths.ClaudeRoot,
		TT:         observer.ResolveVersion(),
		Role:       f.Role,
		Clock:      clock,
		Deps: sockserve.Deps{
			// The loop pushes and holds the published snapshot; the core
			// answers pulls, enriches and refreshes. Two objects, five narrow
			// ports, no fat interface either of them would have to fake.
			Events:  loop,
			Core:    st.obs,
			Enrich:  st.obs,
			Refresh: st.obs,
			Rebuild: loop,
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}

	if err := srv.Listen(); err != nil {
		var running *sockserve.ErrAlreadyRunning
		if errors.As(err, &running) {
			// §5.3: print its path and pid, exit 0. A second `tt observe` is a
			// user asking for an observer and finding one, not a failure.
			fmt.Fprintf(stderr, "tt observe: %v\n", running)
			return jsonout.ExitOK
		}
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}

	// SIGTERM and SIGINT are the whole of the shutdown path, which is what
	// `bye{reason:"shutdown"}` having exactly one reason means.
	ctx, stopSignals := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := loop.Run(ctx); err != nil {
			fmt.Fprintf(stderr, "tt observe: the watch loop stopped: %v\n", err)
		}
	}()

	// §5.5(c)'s status-line writer. The DECISION is made here, once, before
	// anything is written: with --status-line=auto and no `#{E:@tt_status}` in
	// the user's status-right or status-left the writer is disabled and issues
	// zero tmux writes, which is §2.3's guarantee for anyone who has not opted
	// in. It subscribes to the same loop the socket server does, so it needs no
	// timer and wakes only on a real coalesced state change.
	statusLine := newStatusLineWriter(f.StatusLine)
	if statusLine.Enabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := statusLine.Run(ctx, loop); err != nil {
				fmt.Fprintf(stderr, "tt observe: the status line stopped: %v\n", err)
			}
		}()
		fmt.Fprintf(stderr, "tt observe: writing %s on every change (--status-line=%s)\n",
			optStatus, f.StatusLine)
	}

	fmt.Fprintf(stderr, "tt observe: listening on %s (pid %d); SIGTERM or `tt observe --stop` ends it\n",
		srv.SocketPath(), os.Getpid())
	if note != "" {
		fmt.Fprintf(stderr, "tt observe: %s\n", note)
	}

	serveErr := srv.Serve(ctx)
	wg.Wait()

	// Do not exit while a corroboration spawn is still unwinding: P4's live
	// defect 2 left the cross-process lock on disk that way, where every other
	// tt process answers without corroboration until it ages out.
	drain, cancelDrain := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDrain()
	st.obs.AwaitCorroboration(drain)

	if serveErr != nil {
		fmt.Fprintf(stderr, "tt: %v\n", serveErr)
		return jsonout.ExitNoRead
	}
	fmt.Fprintln(stderr, "tt observe: stopped")
	return jsonout.ExitOK
}

// runObserveStop is `tt observe --stop`.
//
// All three outcomes are exit 0. "No server is running" is a fact, like "no
// Claude is running" on S1, and a script that stops an observer twice should
// not fail the second time.
func runObserveStop(dir string, stderr io.Writer) int { return stopServer("tt observe", dir, stderr) }

// stopServer is the corroborated stop shared by `tt observe --stop` and `tt
// serve --stop` (moox §4.5): the same pid file, the same evidence, the same
// refusal; only the label in front of each message differs.
func stopServer(label, dir string, stderr io.Writer) int {
	// The Claude tree is named so the state directory is refused if it
	// resolves inside it (code review round 2, A1). An unresolvable home is
	// refused by statedir (ErrNoClaudeRoot, moox §2.4) rather than skipping
	// the check, so --stop then reports the error and touches nothing.
	claudeRoot := ""
	if paths, err := claudePaths(); err == nil {
		claudeRoot = paths.ClaudeRoot
	}
	out, err := sockserve.StopIn(sockserve.StopConfig{Dir: dir, ClaudeRoot: claudeRoot})
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}
	switch {
	case out.Signalled:
		fmt.Fprintf(stderr, "%s: sent SIGTERM to pid %d (corroborated by %s)\n", label,
			out.PID, out.Corroborated)
	case out.StaleCleaned && out.Kept != "":
		fmt.Fprintf(stderr, "%s: pid %d is gone; removed the stale pid file and left %s alone: %s\n", label,
			out.PID, out.Socket, out.Kept)
	case out.StaleCleaned:
		fmt.Fprintf(stderr, "%s: pid %d is gone; removed the stale socket and pid file\n", label, out.PID)
	case out.Refused:
		// The ONE non-zero outcome, because the user asked for a stop and did
		// not get one. Nothing was signalled and nothing was removed: the pid
		// file survives crashes and reboots, pids get recycled, and a SIGTERM
		// to the wrong pid on this machine most likely lands on one of the
		// user's own Claude sessions.
		if out.PID > 0 {
			fmt.Fprintf(stderr, "%s: refusing to signal pid %d: %s\n", label, out.PID, out.Reason)
		} else {
			// The record itself was refused (a symlink, not a regular file),
			// so there is no pid to name.
			fmt.Fprintf(stderr, "%s: refusing to act: %s\n", label, out.Reason)
		}
		fmt.Fprintf(stderr, "%s: if you are sure, remove %s yourself\n", label, out.Path)
		return jsonout.ExitNoRead
	default:
		fmt.Fprintf(stderr, "%s: no server is running (no pid file at %s)\n", label, out.Path)
	}
	return jsonout.ExitOK
}

// ---------------------------------------------------------------------------
// S2 — `tt mcp`
// ---------------------------------------------------------------------------

// mcpStdin is the seam a test reads the protocol from. It is a variable for
// one reason: `tt mcp`'s whole job is to read stdin until it ends, and a test
// that could not substitute it would have to either hang or drive a real pipe
// through os.Stdin — which is process-global state inside a parallel test
// binary.
var mcpStdin io.Reader = os.Stdin

// mcpFlags is what `tt mcp` was asked for.
type mcpFlags struct {
	// Dir is where the observer socket and pid file live.
	Dir string
	// Local forces this process's own reads even when a server is running.
	Local bool
	// Timeout bounds one corroboration spawn.
	Timeout time.Duration
	// PublishJobText is --publish-job-text: publish the free-text `needs` and
	// `detail` to MCP clients in full, instead of as "[withheld: N chars]".
	// Off by default; every local surface shows them either way.
	PublishJobText bool
}

// jobTextMode names the two states for the startup line, so the user can see
// which one is in force without reading the code or a schema.
func jobTextMode(publish bool) string {
	if publish {
		return "published in full (--publish-job-text)"
	}
	return "withheld (presence and length only)"
}

// runMCP is S2: decide where the data comes from, then speak JSON-RPC on stdio
// until the client goes away.
//
// The decision is made ONCE, here, and the server is given a SessionSource it
// cannot interrogate (§7.5a). `tt mcp`'s two paths differ in exactly one
// visible way — describe_sources' watch block says which one is in force —
// because a consumer deciding whether to trust an age needs to know whether
// anything is pushing.
func runMCP(args []string, stdout, stderr io.Writer) int {
	f, code, ok := parseMCPFlags(args, stdout, stderr)
	if !ok {
		return code
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	deps, surface, closeSrc, err := mcpSource(ctx, f, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}
	defer closeSrc()

	srv, err := mcpserve.New(mcpserve.Config{
		TT:      observer.ResolveVersion(),
		Surface: surface,
		Server:  func() *wire.PidFile { return observerPidFile(f.Dir) },
		// The doctor reads the filesystem DIRECTLY even when this process is
		// attached to a running server over the socket, and that is the point:
		// its job is to check what Claude Code is writing right now, and asking
		// an observer whether its own inputs still have the shape it expects
		// gets the observer's opinion rather than the format's. The socket path
		// carries no doctor frame for the same reason.
		Doctor: func(ctx context.Context) observer.DoctorReport {
			return runDoctorReport(ctx, f.Timeout)
		},
		PublishJobText: f.PublishJobText,
		Deps:           deps,
	})
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}

	fmt.Fprintf(stderr, "tt mcp: serving %d read-only tools on stdio (%s), job text %s\n",
		len(mcpserve.ToolNames), surface.Mode, jobTextMode(f.PublishJobText))

	if err := srv.Serve(ctx, mcpStdin, stdout); err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return jsonout.ExitNoRead
	}
	return jsonout.ExitOK
}

// mcpSource applies §7.5's decision table: dial first, fall back on any of the
// three refusals, and report which happened.
//
// A local fallback arms NO watch — §4.2's "one-shot surfaces never arm a watch"
// covers a pull surface too, and 30 MCP clients each arming 65 descriptors is
// the accumulation this repository has been burned by. The cost is that a local
// `tt mcp` re-reads on every call, which is the same cost `tt sessions` pays.
func mcpSource(ctx context.Context, f mcpFlags, stderr io.Writer) (mcpserve.Deps, wire.SurfaceWatch, func(), error) {
	path := sockclient.SocketPath(sockclient.Config{Dir: f.Dir})

	if !f.Local {
		// Answers over the socket are derived at ANSWER time from this
		// process's clock (§7.5c, code review round 2 A7), with the same
		// liveness thresholds the local path resolves — the retention window
		// from the vendor's own settings.
		cfg := sockclient.Config{Dir: f.Dir, Log: stderr}
		if paths, err := claudePaths(); err == nil {
			cfg.Policy = resolvePolicy(paths)
		}
		client, err := sockclient.Dial(ctx, cfg)
		switch {
		case err == nil:
			fmt.Fprintf(stderr, "tt mcp: attached to the observer at %s (pid %d)\n", path, client.ServerPID())
			return mcpserve.Deps{Core: client, Enrich: client, Refresh: client},
				wire.SurfaceWatch{
					Mode:     observer.WatchModeViaSocket,
					Attached: true,
					Socket:   path,
				},
				func() { _ = client.Close() },
				nil
		case errors.Is(err, sockclient.ErrNoServer):
			// The ordinary case: nothing auto-starts the observer.
		default:
			// A schema mismatch or an unanswered handshake is worth saying
			// out loud — it means a server IS running and this build refused
			// it, which is §7.5(e) working rather than failing.
			fmt.Fprintf(stderr, "tt mcp: not using the observer at %s: %v\n", path, err)
		}
	}

	obs, err := newObserver(observer.SystemClock{}, f.Timeout)
	if err != nil {
		return mcpserve.Deps{}, wire.SurfaceWatch{}, func() {}, err
	}
	note := "answering from this process's own reads; no watch is armed"
	if f.Local {
		note = "--local: answering from this process's own reads without dialling the observer"
	}
	return mcpserve.Deps{Core: obs, Enrich: obs, Refresh: obs},
		wire.SurfaceWatch{
			Mode:     observer.WatchModeOnDemand,
			Attached: false,
			Socket:   path,
			Note:     note,
		},
		func() {
			// Do not exit while a corroboration spawn is still unwinding: P4's
			// live defect 2 left the cross-process lock on disk that way.
			drain, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			obs.AwaitCorroboration(drain)
		},
		nil
}

// observerPidFile is the running server's pid file, or nil.
//
// Read-only and read AT CALL TIME: a server can be started or stopped while a
// Claude session holds this process open, so a value captured at startup would
// tell a model something that stopped being true hours ago. The liveness check
// is kill(pid, 0) — nothing is signalled and the socket is not dialled, and a
// stale file is reported as "no server" rather than cleaned up, because
// removing it is `--stop`'s job in the one package permitted to write.
func observerPidFile(dir string) *wire.PidFile {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, sockserve.PidName))
	if err != nil {
		return nil
	}
	rec, err := wire.DecodePidFile(data)
	if err != nil {
		return nil
	}
	if alive, err := proclive.New().Alive(rec.PID); err != nil || !alive {
		return nil
	}
	return &rec
}

// parseMCPFlags turns argv into what runMCP needs.
func parseMCPFlags(args []string, stdout, stderr io.Writer) (mcpFlags, int, bool) {
	fs := flag.NewFlagSet("tt mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A no-op on purpose: see parseSessionsFlags. The branches below print
	// mcpUsage themselves, once, on the stream that fits.
	fs.Usage = func() {}

	var (
		socketDir  = fs.String("socket-dir", "", "where the observer socket lives")
		local      = fs.Bool("local", false, "do not dial the socket")
		spawnBound = fs.Duration("timeout", 5*time.Second, "bound on one corroboration spawn")
		publishJob = fs.Bool("publish-job-text", false,
			"publish the free-text needs/detail to MCP clients in full")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, mcpUsage)
			return mcpFlags{}, jsonout.ExitOK, false
		}
		fmt.Fprint(stderr, "\n"+mcpUsage)
		return mcpFlags{}, jsonout.ExitUsage, false
	}
	if n := fs.NArg(); n > 0 {
		fmt.Fprintf(stderr, "tt mcp: unexpected argument %q\n\n%s", fs.Arg(0), mcpUsage)
		return mcpFlags{}, jsonout.ExitUsage, false
	}
	if *spawnBound <= 0 {
		fmt.Fprintf(stderr, "tt mcp: --timeout must be positive, got %s\n\n%s", *spawnBound, mcpUsage)
		return mcpFlags{}, jsonout.ExitUsage, false
	}

	dir := strings.TrimSpace(*socketDir)
	if dir == "" {
		dir = ttStateDir()
	}
	return mcpFlags{
		Dir: dir, Local: *local, Timeout: *spawnBound, PublishJobText: *publishJob,
	}, jsonout.ExitOK, true
}

// observeFlags is what `tt observe` was asked for. It is a struct rather than
// five returns because P9 added two more flags and a six-value signature is
// one where a caller silently swaps two bools.
type observeFlags struct {
	// Dir is where the socket and pid file live.
	Dir string
	// Stop is --stop: signal the running server and exit.
	Stop bool
	// Pane is --pane: render the panel in this tmux pane and serve NOTHING.
	// It implies --no-serve (§5.5b) — a renderer never binds the socket.
	Pane bool
	// Auto is --auto, meaningful only with --pane: the scope hooks created this
	// pane, so it is a thin renderer that never arms a watch of its own.
	Auto bool
	// Timeout bounds one corroboration spawn.
	Timeout time.Duration
	// StatusLine is §5.5(c)'s writer mode, defaulting to auto (opt-in by
	// placeholder detection).
	StatusLine statusLineMode
	// Role is the pid file's role: wire.RoleObserve for `tt observe`,
	// wire.RoleServe when `tt serve --foreground` hosts the observer.
	Role string
}

// parseObserveFlags turns argv into what runObserve needs.
func parseObserveFlags(args []string, stdout, stderr io.Writer) (observeFlags, int, bool) {
	fs := flag.NewFlagSet("tt observe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A no-op on purpose: see parseSessionsFlags. The branches below print
	// observeUsage themselves, once, on the stream that fits.
	fs.Usage = func() {}

	var (
		socketDir  = fs.String("socket-dir", "", "where the socket and pid file live")
		stopFlag   = fs.Bool("stop", false, "SIGTERM the running server")
		paneFlag   = fs.Bool("pane", false, "render the panel in this tmux pane")
		autoFlag   = fs.Bool("auto", false, "with --pane: a hook-created pane")
		spawnBound = fs.Duration("timeout", 5*time.Second, "bound on one corroboration spawn")
		statusLine = fs.String("status-line", statusLineAuto.String(), "auto|always|never")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, observeUsage)
			return observeFlags{}, jsonout.ExitOK, false
		}
		fmt.Fprint(stderr, "\n"+observeUsage)
		return observeFlags{}, jsonout.ExitUsage, false
	}
	if n := fs.NArg(); n > 0 {
		fmt.Fprintf(stderr, "tt observe: unexpected argument %q\n\n%s", fs.Arg(0), observeUsage)
		return observeFlags{}, jsonout.ExitUsage, false
	}
	if *spawnBound <= 0 {
		fmt.Fprintf(stderr, "tt observe: --timeout must be positive, got %s\n\n%s", *spawnBound, observeUsage)
		return observeFlags{}, jsonout.ExitUsage, false
	}

	mode, ok := parseStatusLineMode(*statusLine)
	if !ok {
		fmt.Fprintf(stderr, "tt observe: --status-line must be auto, always or never, got %q\n\n%s",
			*statusLine, observeUsage)
		return observeFlags{}, jsonout.ExitUsage, false
	}

	// --auto describes a PANE; on its own it would silently start a server that
	// the user never asked for, so it is refused rather than ignored.
	if *autoFlag && !*paneFlag {
		fmt.Fprintf(stderr, "tt observe: --auto is only meaningful with --pane\n\n%s", observeUsage)
		return observeFlags{}, jsonout.ExitUsage, false
	}

	// §5.5(b): --pane with any SERVER-only flag is a usage error, exit 2. It is
	// refused rather than resolved by precedence for the same reason
	// --enrich/--no-transcript is: either answer would be a guess at which of
	// two opposite intentions the user had, and the wrong guess here either
	// binds a socket in a renderer or renders a pane in a server.
	if *paneFlag {
		var serverOnly []string
		if *stopFlag {
			serverOnly = append(serverOnly, "--stop")
		}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "status-line" {
				serverOnly = append(serverOnly, "--status-line")
			}
		})
		if len(serverOnly) > 0 {
			fmt.Fprintf(stderr, "tt observe: --pane is a renderer and %s is a server flag\n\n%s",
				strings.Join(serverOnly, " and "), observeUsage)
			return observeFlags{}, jsonout.ExitUsage, false
		}
	}

	dir := strings.TrimSpace(*socketDir)
	if dir == "" {
		dir = ttStateDir()
	}
	if dir == "" {
		fmt.Fprintln(stderr, "tt observe: no state directory: neither $XDG_STATE_HOME nor a home directory is set")
		return observeFlags{}, jsonout.ExitNoRead, false
	}
	return observeFlags{
		Dir:        dir,
		Stop:       *stopFlag,
		Pane:       *paneFlag,
		Auto:       *autoFlag,
		Timeout:    *spawnBound,
		StatusLine: mode,
	}, jsonout.ExitOK, true
}

// newWatcher builds the kqueue watcher, or a stand-in that reports it is
// unavailable.
//
// §6.6: a degraded observer is a correct ON-DEMAND observer — it answers every
// query by re-reading and simply cannot push — so a kqueue this machine will
// not give us is a value in the snapshot and a note on stderr, never an exit.
// The stand-in exists because watchkq.New fails BEFORE Watch, which is the one
// degradation path the loop cannot discover on its own.
func newWatcher() (observer.Watcher, string) {
	w, err := watchkq.New(watchkq.Caps{})
	if err != nil {
		return unavailableWatcher{err: err},
			fmt.Sprintf("no kqueue (%v); answering on demand, with no push", err)
	}
	return w, ""
}

// unavailableWatcher is observer.Watcher for a machine that cannot watch.
type unavailableWatcher struct{ err error }

func (u unavailableWatcher) Watch(context.Context) (<-chan observer.Change, error) {
	return nil, u.err
}

func (u unavailableWatcher) Reconcile(context.Context, []observer.WatchTarget) (observer.ArmReport, error) {
	return observer.ArmReport{
		ByKind:   map[observer.TargetKind]int{},
		Degraded: observer.DegradedKqueueUnavailable,
	}, nil
}

func (u unavailableWatcher) Release(context.Context, []observer.WatchTarget) (observer.ArmReport, error) {
	return observer.ArmReport{
		ByKind:   map[observer.TargetKind]int{},
		Degraded: observer.DegradedKqueueUnavailable,
	}, nil
}

// parseSessionsFlags turns argv into jsonout.Options. It returns ok=false with
// an exit code for both failure and --help, so the caller has one branch.
//
// The third return is "--doctor was asked for", carried out rather than folded
// into jsonout.Options: the doctor produces a different document from a
// different reader, and a field on the snapshot options would invite some later
// code to render one as the other.
func parseSessionsFlags(args []string, stdout, stderr io.Writer) (jsonout.Options, time.Duration, bool, int, bool) {
	fs := flag.NewFlagSet("tt sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Usage is a no-op on purpose. flag.Parse calls it on -h/--help AND on a
	// bad flag, and the two branches below already print sessionsUsage — to
	// stdout for help, to stderr for an error. A Usage that printed too put the
	// text on BOTH streams for --help and twice on stderr for a bad flag. A nil
	// Usage is no better: flag would fall back to its generated listing, which
	// documents neither the exit codes nor the long-form flags.
	fs.Usage = func() {}

	var (
		asJSON      = fs.Bool("json", false, "emit the machine-readable snapshot")
		all         = fs.Bool("all", false, "include dead and launched sessions and drop every narrowing")
		waiting     = fs.Bool("waiting", false, "only sessions needing attention")
		kind        = fs.String("kind", "", "interactive|background")
		cwdPrefix   = fs.String("cwd-prefix", "", "only sessions under this path")
		includeDead = fs.Bool("include-dead", false, "include dead sessions")
		noTranscr   = fs.Bool("no-transcript", false, "read no transcript at all")
		// --enrich is the explicit positive form of the S1 default. It exists
		// because "the default is on" is invisible on a command line, and a
		// user or a script asking for the colour pass by name should not have
		// to know that asking for nothing already gets it.
		enrich  = fs.Bool("enrich", false, "run the enrichment pass (already the default)")
		refresh = fs.Bool("refresh", false, "re-read corroboration first")
		timeout = fs.Duration("timeout", 5*time.Second, "read deadline")
		doctor  = fs.Bool("doctor", false, "diagnose the formats rather than list sessions")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// --help is a successful request for the usage text, and it goes to
			// stdout because the user asked for it.
			fmt.Fprint(stdout, sessionsUsage)
			return jsonout.Options{}, 0, false, jsonout.ExitOK, false
		}
		// flag has already described the error on stderr.
		fmt.Fprint(stderr, "\n"+sessionsUsage)
		return jsonout.Options{}, 0, false, jsonout.ExitUsage, false
	}

	if n := fs.NArg(); n > 0 {
		fmt.Fprintf(stderr, "tt sessions: unexpected argument %q\n\n%s", fs.Arg(0), sessionsUsage)
		return jsonout.Options{}, 0, false, jsonout.ExitUsage, false
	}
	if *doctor {
		// §9.5's doctor, P10's deliverable. Every OTHER flag on this line is
		// refused rather than ignored: --doctor does not list sessions, so a
		// narrowing flag beside it describes a filter that will not be applied,
		// and silently dropping it is how a user comes to believe they
		// diagnosed a subset. --json is the one exception, because it selects
		// the FORM of the answer rather than its content.
		var offenders []string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "doctor", "json", "timeout":
			default:
				offenders = append(offenders, "--"+f.Name)
			}
		})
		if len(offenders) > 0 {
			fmt.Fprintf(stderr, "tt sessions: --doctor takes no %s\n\n%s",
				strings.Join(offenders, ", no "), sessionsUsage)
			return jsonout.Options{}, 0, false, jsonout.ExitUsage, false
		}
	}
	if *enrich && *noTranscr {
		// Refused rather than resolved by precedence. The two flags are exact
		// opposites, so either answer would be a guess at which one the user
		// meant, and a wrong guess here silently changes what the output
		// contains.
		fmt.Fprintf(stderr, "tt sessions: --enrich and --no-transcript are opposites\n\n%s", sessionsUsage)
		return jsonout.Options{}, 0, false, jsonout.ExitUsage, false
	}
	if *timeout <= 0 {
		fmt.Fprintf(stderr, "tt sessions: --timeout must be positive, got %s\n\n%s", *timeout, sessionsUsage)
		return jsonout.Options{}, 0, false, jsonout.ExitUsage, false
	}

	// Start from the DEFAULTS and override. A SnapshotOptions{} literal has
	// IncludeUnknownLiveness false, which silently drops every live: null
	// session — the exact behaviour M7 exists to forbid. This is the one trap
	// P1's log names for this phase.
	snapOpts := observer.DefaultSnapshotOptions()
	// S1 collects transcripts by default (§9.2a names this as the per-surface
	// default, deliberately, so AC11 can equalise it explicitly later).
	snapOpts.IncludeTranscripts = !*noTranscr
	// And --no-transcript means what §5.2 says it means: NO transcript is read,
	// the background half's 64 KiB timing tail included. Before the black-box
	// fix it turned off only the enrichment pass, so every job still reported
	// transcriptCollected: true and a lastActivityAt on a run that promised
	// neither.
	snapOpts.NoTranscriptReads = *noTranscr
	snapOpts.IncludeDead = *includeDead
	snapOpts.Filter.WaitingOnly = *waiting
	snapOpts.Filter.CWDPrefix = *cwdPrefix

	if *kind != "" {
		k, ok := parseKind(*kind)
		if !ok {
			fmt.Fprintf(stderr, "tt sessions: --kind must be interactive or background, got %q\n\n%s", *kind, sessionsUsage)
			return jsonout.Options{}, 0, false, jsonout.ExitUsage, false
		}
		snapOpts.Filter.Kinds = []observer.Kind{k}
	}

	if *all {
		// --all is "show me everything you know", so it both widens the
		// liveness set and clears every narrowing — including ones given
		// earlier on the same line. Defined here because §5.2 lists the flag
		// and never says what it means; last-flag-wins would be the
		// alternative and it makes `--waiting --all` ambiguous.
		snapOpts.IncludeDead = true
		snapOpts.IncludeUnknownLiveness = true
		// "Everything you know" includes the sessions other sessions launched
		// (an MCP helper's headless child), which every default view hides.
		// IncludeSpawned is set ON the cleared filter rather than before it,
		// because it lives in Filter and the reset would otherwise drop it.
		snapOpts.Filter = observer.Filter{IncludeSpawned: true}
	}

	return jsonout.Options{
		JSON:     *asJSON,
		TT:       observer.ResolveVersion(),
		Refresh:  *refresh,
		Snapshot: snapOpts,
	}, *timeout, *doctor, jsonout.ExitOK, true
}

// parseKind maps the flag's spelling to the domain vocabulary. An unrecognised
// value is a usage error rather than KindUnknown: KindUnknown is what we report
// about a SESSION whose kind the vendor did not tell us, and accepting it here
// would make `--kind=typo` silently match the sessions we understand least.
func parseKind(s string) (observer.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(observer.KindInteractive):
		return observer.KindInteractive, true
	case string(observer.KindBackground):
		return observer.KindBackground, true
	default:
		return "", false
	}
}

// newObserver wires the production adapters to the core.
//
// The paths are derived once, here, and handed to the core as data: §6.5's
// source reports have to name them and a port that could be asked for its own
// path would be an adapter's vocabulary leaking into the core.
//
// Note which port is absent: Watch arrives with the socket server, because a
// one-shot command arms no watch (§4.2, "one-shot surfaces never arm a watch").
// observer.New validates only the ports the snapshot path dereferences, so this
// composition root is honest about what this build can answer rather than
// passing stubs that would each have to lie.
//
// spawnTimeout is the --timeout value, deliberately: §4.3(a) makes the spawn
// bound "one timeout, every surface", and `tt sessions --timeout=2s` is the
// example it gives. A separate knob would let a caller ask for a 2 s answer and
// wait 5 s for a corroboration spawn inside it.
func newObserver(clock observer.Clock, spawnTimeout time.Duration) (*observer.Observer, error) {
	st, err := newObserverStack(clock, spawnTimeout)
	if err != nil {
		return nil, err
	}
	return st.obs, nil
}

// observerStack is the wiring plus the two pieces of it a LONG-LIVED surface
// needs in its own right.
//
// §6.3's wave 3 has to read the job records to learn which transcripts to arm
// before the answer-building read happens, so the watch loop holds the same
// JobsSource the core does — the port, not a second adapter over the same
// directory, because two readers of one tree is two answers to "what is in it".
// Paths travel with it for the same reason they travel into the core: the loop
// arms them and something has to name them.
type observerStack struct {
	obs   *observer.Observer
	jobs  observer.JobsSource
	paths observer.Paths
}

func newObserverStack(clock observer.Clock, spawnTimeout time.Duration) (observerStack, error) {
	paths, err := claudePaths()
	if err != nil {
		return observerStack{}, err
	}
	policy := resolvePolicy(paths)
	jobs := jobsfs.New(paths.Jobs, clock)

	obs, err := observer.New(observer.Deps{
		Registry:    registryfs.New(paths.Registry, clock),
		Jobs:        jobs,
		Transcripts: transcriptfs.New(paths.Projects, clock),
		Agents: agentscli.New(agentscli.Config{
			StateDir:     ttStateDir(),
			ClaudeRoot:   paths.ClaudeRoot,
			SpawnTimeout: spawnTimeout,
			StaleAfterMs: policy.StaleAfterMs,
			TT:           observer.ResolveVersion(),
			Clock:        clock,
		}),
		Proc:  proclive.New(),
		Panes: tmuxpanes.New(ttTarget()),
		// moox §4.5: a live session whose process is not under any of this
		// server's panes is another server's, never dead here.
		Members: procmember.New(procmember.Config{ByPidDir: byPidDir}),
		Clock:   clock,
	}, observer.Config{Paths: paths, Policy: policy})
	if err != nil {
		return observerStack{}, err
	}
	return observerStack{obs: obs, jobs: jobs, paths: paths}, nil
}

// ttStateDir is tt's OWN state directory — ~/.local/state/tt — and the only
// filesystem location anything in tt writes to. It is resolved here, in the
// composition root, and handed to the one adapter allowed to write, so that the
// path and the permission to use it arrive together.
//
// Nothing is created here. The directory is made lazily, through statedir —
// the one package the §9.4a write audit allows to write at all, and which
// refuses a directory that RESOLVES inside the Claude tree — by the cache
// writer and the socket server. This function returning a path is not a write,
// and keeping it that way is what lets the audit stay keyed on packages.
//
// It is PER SERVER (moox §4.2, §4.5): the default server keeps today's
// ~/.local/state/tt, every other one gets ~/.local/state/tt/sockets/<key>, so
// `tt mcp`, the sidebar and `tt sessions` reach their own server's observer
// without being told where it is.
func ttStateDir() string {
	if p, ok := ttPaths(); ok {
		return p.TTState
	}
	return agentscli.DefaultStateDir()
}

// byPidDir is the shared by-pid registry for the foreign-server rule's start
// tokens (moox §4.2's one rule, R3-C2): @claude-continuity-by-pid-dir if the
// server sets it (test servers only), else the shared default. Read once, the
// first time a membership is judged.
var byPidDir = sync.OnceValue(func() string {
	p, ok := ttPaths()
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := tmuxCommand(ctx, "show-options", "-gqv", "@claude-continuity-by-pid-dir").Output()
	if err != nil {
		return p.ByPidDir
	}
	return p.ByPid(strings.TrimSpace(string(out)))
})

// resolvePolicy reads the one threshold in the liveness policy that is not a
// guess: the vendor's own transcript retention window.
//
// Read ONCE, at startup, read-only, from ~/.claude/settings.json's
// `cleanupPeriodDays`. Absent on this machine (measured), so the vendor's
// documented default of 30 days is in force — which the measured 30.11-day
// floor over `projects/*/*.jsonl` confirms independently.
//
// It is read through an fs.FS rooted at the Claude root, which has no write
// method, so this cannot become a write to ~/.claude/ even by mistake.
func resolvePolicy(paths observer.Paths) observer.Policy {
	policy := observer.DefaultPolicy()
	if ms, ok := transcriptfs.RetentionMs(os.DirFS(paths.ClaudeRoot)); ok {
		policy.TranscriptRetentionMs = ms
	}
	return policy
}

// claudePaths locates the Claude Code state tree. CLAUDE_CONFIG_DIR is honoured
// because that is the documented way to move it, and because a test that wants
// a different tree should not have to move $HOME.
//
// Nothing here creates a directory. An absent tree is a fact the registry
// source reports (§6.6), not something to provision.
func claudePaths() (observer.Paths, error) {
	root := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return observer.Paths{}, fmt.Errorf("locating the home directory: %w", err)
		}
		root = filepath.Join(home, ".claude")
	}
	return observer.Paths{
		ClaudeRoot: root,
		Registry:   filepath.Join(root, "sessions"),
		Jobs:       filepath.Join(root, "jobs"),
		Projects:   filepath.Join(root, "projects"),
	}, nil
}
