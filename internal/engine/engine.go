// Package engine is moox's save/restore engine, behind one facade
// (architecture §1.3, §6): callers see Engine and its methods; the eight
// sub-packages under internal/ are unreachable from outside, which the
// compiler enforces.
//
// P3 delivered the save half (Engine.Save, Engine.Last); P4b the restore
// half (Engine.Plan, Engine.Restore, BootGate, ManualPreconditions, Purge)
// and its two verdicts.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// The save results that are errors (§5.5 table; the saver maps each to its
// state-machine branch and `moox save` to its exit code).
var (
	// ErrBusy: another save holds save.lock (a running restore holds it too).
	// The error is a *BusyError naming the holder's pid when it can.
	ErrBusy = store.ErrBusy
	// ErrBlocked: the clobber guard kept moox-last (B20).
	ErrBlocked = errors.New("engine: save blocked by the clobber guard")
	// ErrRestoring: @moox-restoring names a live restore, or does not parse
	// (fail closed, §5.4).
	ErrRestoring = errors.New("engine: a restore owns the server")
	// ErrShuttingDown: @moox-restoring is a live `shutdown:` (moox restart).
	ErrShuttingDown = errors.New("engine: a restart owns the server")
	// ErrInvalid: the snapshot failed Validate and was not published (B19).
	ErrInvalid = snapshot.ErrInvalid
	// ErrNotActive: the server is not matched by @moox-sockets (§3.3).
	ErrNotActive = errors.New("engine: server not matched by @moox-sockets")
	// ErrNoSnapshot: no moox snapshot validates.
	ErrNoSnapshot = store.ErrNoSnapshot
	// ErrNewerFormat: a snapshot written by a newer moox.
	ErrNewerFormat = snapshot.ErrNewerFormat
)

// BusyError is ErrBusy with the lock holder's pid.
type BusyError = store.BusyError

// tmuxRunner runs one tmux exec (one command or one chain). tmuxtarget.Target
// is the real one.
type tmuxRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// procLister is the process half: the one ps table, the job-control table
// of the idle-shell check, a single pid's environment, and liveness.
type procLister interface {
	Table(ctx context.Context) ([]byte, error)
	Jobs(ctx context.Context) ([]byte, error)
	Env(ctx context.Context, pid int) ([]byte, error)
	Alive(pid int) bool
}

// Options configure an Engine.
type Options struct {
	Target tmuxtarget.Target
	// Env supplies HOME and XDG_* for the per-socket layout (§4.2). Tests
	// pass a scratch environment; nil means the process environment.
	Env sockets.Env
	// ClaudeRoot is the Claude tree statedir refuses to write in; default
	// $HOME/.claude.
	ClaudeRoot string
	// Producer is the snapshot's producer, e.g. "moox 0.1.0".
	Producer string
	// Log receives one line per notable event (daemon.log). nil discards.
	Log io.Writer
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Engine is one server's engine.
type Engine struct {
	opts       Options
	paths      sockets.Paths
	claudeRoot string
	tmux       tmuxRunner
	procs      procLister
	scripts    scriptRunner
	now        func() time.Time
	log        io.Writer

	// test hooks (nil/zero in production)
	onStep    func(step string)
	afterRead func()
	timing    *launchTiming // LAUNCH probe timing
	newTimer  timerFunc     // the probe chain's one-shot timers
	lockWait  time.Duration // the restore's bounded save.lock wait
}

// New builds an Engine for opts.Target's server.
func New(opts Options) (*Engine, error) {
	if opts.Target.Socket == "" {
		return nil, tmuxtarget.ErrNoSocket
	}
	env := opts.Env
	if env == nil {
		env = tmuxtarget.OSEnv{}
	}
	paths, err := sockets.For(opts.Target.Socket, env)
	if err != nil {
		return nil, err
	}
	root := opts.ClaudeRoot
	if root == "" {
		roots, err := sockets.RootsFrom(env)
		if err != nil {
			return nil, err
		}
		root = filepath.Join(roots.Home, ".claude")
	}
	e := &Engine{opts: opts, paths: paths, claudeRoot: root, tmux: opts.Target, procs: osProcs{}, scripts: osScripts{},
		now: opts.Now, log: opts.Log}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = io.Discard
	}
	return e, nil
}

// Paths is the server's per-socket layout.
func (e *Engine) Paths() sockets.Paths { return e.paths }

func (e *Engine) logf(format string, a ...any) {
	fmt.Fprintf(e.log, "%s %s\n", e.now().Format("2006-01-02T15:04:05"), fmt.Sprintf(format, a...))
}

func (e *Engine) step(s string) {
	if e.onStep != nil {
		e.onStep(s)
	}
}

func newNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// LastResult is Engine.Last's answer.
type LastResult struct {
	Name     string // basename in the snapshot directory
	Path     string // absolute
	Snapshot *snapshot.Snapshot
	Healed   bool // moox-last was bad; it now names Name (if the lock was free)
}

// Last is the read side (§6.2): moox-last → file → Decode → Validate, and on
// any failure the heal scan over the newest 12 moox snapshots (B19, B21,
// H1–H3). A heal rewrites moox-last when save.lock is free, and logs HEALED.
func (e *Engine) Last() (LastResult, error) {
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return LastResult{}, err
	}
	defer func() { _ = st.Close() }()
	last, err := st.Last()
	if err != nil {
		return LastResult{}, err
	}
	res := LastResult{Name: last.Name, Path: st.Path(last.Name), Snapshot: last.Snapshot, Healed: last.Healed}
	if last.Healed {
		if l, lerr := st.TryLock(e.record()); lerr == nil {
			// Re-read under the lock: a save may have published (and pointed
			// moox-last at) a newer snapshot between the scan and the lock,
			// and a heal must never rewind it (G1-M5).
			now, nerr := st.Last()
			switch {
			case errors.Is(nerr, ErrNewerFormat):
				// A newer moox published meanwhile: refuse as the first read
				// would have, never hand back the older pick (G1-2-L2).
				l.Release()
				return LastResult{}, nerr
			case nerr != nil:
			case !now.Healed:
				res = LastResult{Name: now.Name, Path: st.Path(now.Name), Snapshot: now.Snapshot}
			default:
				res = LastResult{Name: now.Name, Path: st.Path(now.Name), Snapshot: now.Snapshot, Healed: true}
				if herr := st.Heal(l, now.Name); herr == nil {
					e.logf("HEALED moox-last %q -> %s", now.Pointer, now.Name)
				}
			}
			l.Release()
		}
	}
	return res, nil
}
