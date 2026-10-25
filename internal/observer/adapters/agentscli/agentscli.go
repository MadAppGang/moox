// Package agentscli is the corroboration adapter: it runs `claude agents --json`
// and turns its output into an observer.AgentsResult.
//
// It is the ONE driven adapter that starts a process, and the one place in this
// module — beside the socket server — that writes to the filesystem at all. Both
// facts are deliberate and both are bounded here rather than trusted:
//
//	the spawn      is single-flighted by the CORE (§4.3a) and bounded by
//	               spawnTimeout here, with the child's whole process GROUP killed
//	               on expiry so a wedged `claude` cannot outlive the request that
//	               asked for it.
//	the write      goes to ~/.local/state/tt/ and nowhere else. The static audit
//	               allowlists this package by name (§9.4a); the runtime guard in
//	               cache.go refuses a state directory inside the Claude tree,
//	               because an AST pass cannot evaluate a path built at runtime.
//
// # Why there is a cache and a lock at all
//
// The spawn costs ~130 ms (measured), so it cannot be on the hot path, and on
// this machine there can be dozens of tt processes. A cache file alone does NOT
// bound that: N processes starting together all read the cold cache before any
// has written it back, and all N spawn. That was the only finding all four
// round-2 reviewers raised. The fix is an atomic O_EXCL lock — the cache bounds
// the STAGGERED fan-out, the lock bounds the SIMULTANEOUS one, and neither
// substitutes for the other (§4.3b).
//
// # What this adapter deliberately does not do
//
// It holds no opinion about the vendor. Decision D2: "we rely on this tool and
// by default assume it works fine, unless proven opposite." So there is no
// retry, no output sniffing, no version gate and no fallback command — only a
// runtime bound and an honest report of what happened. Everything the listing
// says is carried verbatim into AgentsEntry and mapped by the core's own tables.
package agentscli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// The vendor invocation. Split into command and args so the whole of it is
// visible in one place and so a test can point the command at a fixture script
// without reassembling a string.
const (
	DefaultCommand = "claude"
)

// DefaultArgs is the sanctioned interface, verbatim (§0.1, phase-2 finding 2).
func DefaultArgs() []string { return []string{"agents", "--json"} }

// Bounds. Every one of them is a policy knob rather than a fact about the
// world, and each is published in the source report so a consumer can see which
// value was in force.
const (
	// DefaultSpawnTimeout is §4.3(a)'s bound: 38x the measured 130 ms spawn. On
	// expiry the child's process group is killed, the previous listing is kept,
	// and the caller gets a degraded answer — never an error and never a hang.
	DefaultSpawnTimeout = 5 * time.Second
	// DefaultLockWait is how long a contended process waits on the cache file
	// before answering WITHOUT corroboration. Deliberately longer than the
	// measured spawn (130 ms) and deliberately far shorter than spawnTimeout:
	// waiting out a wedged holder would turn one stuck process into 34 stuck
	// processes, which is the failure the lock exists to prevent.
	DefaultLockWait = 250 * time.Millisecond
	// lockPollInterval is how often the contended path re-stats the CACHE file
	// (never the lock — the lock says who is working, the cache says whether the
	// answer has landed).
	lockPollInterval = 10 * time.Millisecond
	// stalelockGrace is added to spawnTimeout before a lock counts as
	// abandoned. A process killed mid-spawn would otherwise lock out every
	// other process on the machine permanently.
	staleLockGrace = time.Second
	// maxOutputBytes bounds what is read from the child. 10 270 B measured for
	// 39 entries; 8 MiB is ~30 000 entries and exists so a runaway child cannot
	// be answered with unbounded memory.
	maxOutputBytes = 8 << 20
	// stderrExcerpt is how much of the child's stderr travels into the error
	// message. Enough to see "command not found", short enough not to paste a
	// stack trace into a source report.
	stderrExcerpt = 400
)

// Config is everything the source needs. Every field has a documented default,
// so the zero Config is a working production adapter.
type Config struct {
	// Command and Args are the invocation. Defaults: `claude agents --json`.
	Command string
	Args    []string

	// StateDir is where the cache and the lock live. Default: DefaultStateDir().
	// It is the ONLY directory this package writes to.
	StateDir string
	// ClaudeRoot is the tree that must never be written to. The cache writer
	// refuses to run when StateDir is inside it (§9.4a's runtime half).
	ClaudeRoot string

	// SpawnTimeout defaults to DefaultSpawnTimeout. `tt sessions --timeout=2s`
	// sets it for that process (§4.3a, "one timeout, every surface").
	SpawnTimeout time.Duration
	// StaleAfterMs is how old a cached listing may be and still be used.
	// Defaults to observer.DefaultStaleAfterMs. It is the SAME knob the core's
	// policy carries, passed in rather than re-defaulted here, so the cache
	// cannot serve a listing the core would immediately call stale.
	StaleAfterMs int64
	// LockWait defaults to DefaultLockWait.
	LockWait time.Duration

	// TT is tt's own version string, recorded in the cache envelope so a
	// consumer of the cache can see which build wrote it.
	TT string

	Clock observer.Clock

	// Run is the spawn seam. Non-nil replaces the exec entirely, which is how
	// the cross-process lock tests drive sixteen "processes" without sixteen
	// real children. Production leaves it nil.
	Run func(ctx context.Context) ([]byte, error)
}

// Source implements observer.AgentsSource.
//
// It holds no listing and no mutex: the cache file IS the shared state, and
// single-flighting concurrent callers inside one process is the core's job
// (§4.3a). A per-process cache here would be a second answer to the same
// question, able to disagree with the file every other tt process reads.
type Source struct {
	command      string
	args         []string
	stateDir     string
	claudeRoot   string
	spawnTimeout time.Duration
	staleAfterMs int64
	lockWait     time.Duration
	tt           string
	clock        observer.Clock
	run          func(ctx context.Context) ([]byte, error)
}

// New returns a Source. Nothing is created on disk here: a constructor that
// made a directory would write on a path that may never take a spawn.
func New(cfg Config) *Source {
	s := &Source{
		command:      cfg.Command,
		args:         cfg.Args,
		stateDir:     cfg.StateDir,
		claudeRoot:   cfg.ClaudeRoot,
		spawnTimeout: cfg.SpawnTimeout,
		staleAfterMs: cfg.StaleAfterMs,
		lockWait:     cfg.LockWait,
		tt:           cfg.TT,
		clock:        cfg.Clock,
		run:          cfg.Run,
	}
	if s.command == "" {
		s.command = DefaultCommand
	}
	if s.args == nil {
		s.args = DefaultArgs()
	}
	if s.stateDir == "" {
		s.stateDir = DefaultStateDir()
	}
	if s.claudeRoot == "" {
		s.claudeRoot = defaultClaudeRoot()
	}
	if s.spawnTimeout <= 0 {
		s.spawnTimeout = DefaultSpawnTimeout
	}
	if s.staleAfterMs <= 0 {
		s.staleAfterMs = observer.DefaultStaleAfterMs
	}
	if s.lockWait <= 0 {
		s.lockWait = DefaultLockWait
	}
	if s.clock == nil {
		s.clock = observer.SystemClock{}
	}
	return s
}

// CommandLine is the exact command this source runs, for the source report.
func (s *Source) CommandLine() string {
	return strings.TrimSpace(s.command + " " + strings.Join(s.args, " "))
}

// StateDir is where the cache and lock live, for the source report and for the
// AC13 audit's runtime half.
func (s *Source) StateDir() string { return s.stateDir }

// List implements observer.AgentsSource.
//
// It returns a nil error in every case. A failure is a VALUE here — OK false
// plus Err — because §7.4 makes a source failure a value in the snapshot rather
// than an error that kills the response, and because a corroboration source
// that could fail a query would be able to blank an answer the authoritative
// sources had already produced. The error return exists for the port's shape
// and for an implementation that needs it.
//
// §4.3(b)'s five outcomes, in the order they are attempted:
//
//	hit             the cache is fresh. No lock, no spawn.
//	miss            we took the lock and spawned. The result is published.
//	hit-after-wait  the lock was held and the holder published in time.
//	contended       the lock was held and the wait expired. NO corroboration
//	                this cycle, and deliberately NO spawn: going without for one
//	                cycle is a correct degraded answer (corroboration is optional
//	                by construction), while spawning anyway is the stampede and
//	                waiting is a 34-process pile-up.
//	unwritable      we spawned and could not publish. The listing is still
//	                returned; only the sharing failed.
func (s *Source) List(ctx context.Context) (observer.AgentsResult, error) {
	now := s.clock.Now()

	if env, ok := s.readCache(); ok && s.fresh(env, now) {
		return s.fromCache(env, observer.AgentsCacheHit, 0), nil
	}

	held, err := s.acquire(now)
	if errors.Is(err, errLockHeld) {
		return s.waitForPublication(ctx, now), nil
	}
	if err != nil {
		// The lock could not be created for a reason that is not contention —
		// an unwritable state directory, most likely. Corroborate anyway: the
		// cache is a convenience, and the bound that matters (one spawn in
		// flight per process) is the core's, not this file's.
		return s.spawnAndPublish(ctx, observer.AgentsCacheUnwritable), nil
	}
	defer held.release()

	return s.spawnAndPublish(ctx, observer.AgentsCacheMiss), nil
}

// spawnAndPublish runs the child, publishes the raw bytes to the cache, and
// reports which cache outcome that produced.
//
// The bytes are cached VERBATIM rather than re-encoded from the parsed entries.
// Two reasons, and the second is the load-bearing one: a re-encode would make
// the cache a second format with its own bugs, and a field this build ignores
// would be lost for the build that reads the cache next.
func (s *Source) spawnAndPublish(ctx context.Context, outcome string) observer.AgentsResult {
	raw, spawnMs, err := s.spawn(ctx)
	observedAt := s.clock.Now()

	if err != nil {
		return observer.AgentsResult{
			ObservedAt: observedAt,
			Command:    s.CommandLine(),
			SpawnMs:    spawnMs,
			Cache:      outcome,
			OK:         false,
			Err:        err,
		}
	}

	entries, perr := decodeEntries(raw)
	if perr != nil {
		// Unparseable output is a failed listing, not an empty one. An empty
		// Entries slice with OK true would tell the liveness policy that the
		// vendor lists nothing, which is positive evidence of death for every
		// background job on the machine.
		return observer.AgentsResult{
			ObservedAt: observedAt,
			Command:    s.CommandLine(),
			SpawnMs:    spawnMs,
			Cache:      outcome,
			OK:         false,
			Err:        perr,
		}
	}

	if outcome == observer.AgentsCacheMiss {
		if werr := s.writeCache(raw, observedAt); werr != nil {
			outcome = observer.AgentsCacheUnwritable
		}
	}

	return observer.AgentsResult{
		Entries:    entries,
		ObservedAt: observedAt,
		Command:    s.CommandLine(),
		SpawnMs:    spawnMs,
		Cache:      outcome,
		OK:         true,
	}
}

// spawn runs the child under a hard deadline and returns its stdout.
//
// Three details that are not decoration:
//
//   - Setpgid puts the child in its own process group and Cancel kills the whole
//     GROUP. `claude` is a node wrapper; killing only the direct child leaves the
//     real work running, and this repository's history is mostly about processes
//     nobody remembered starting (226 idle MCP servers, 61 orphaned probes).
//   - WaitDelay bounds Wait itself. Without it, a child that ignored the signal
//     but held its stdout pipe open would hang the read after the kill.
//   - stdout goes to a bounded buffer. CombinedOutput would fold stderr into the
//     JSON we are about to parse.
func (s *Source) spawn(ctx context.Context) ([]byte, int64, error) {
	if s.run != nil {
		start := s.clock.Now()
		runCtx, cancel := context.WithTimeout(ctx, s.spawnTimeout)
		defer cancel()
		out, err := s.run(runCtx)
		return out, s.clock.Now().Sub(start).Milliseconds(), err
	}

	runCtx, cancel := context.WithTimeout(ctx, s.spawnTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, s.command, s.args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid = the process group. The child is the group leader
		// because of Setpgid above, so this reaches the node process and every
		// helper it forked.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	// The child runs from "/", never from tt's own cwd. `tt observe` lives for
	// days and is often started inside a git worktree that is later removed;
	// a child that inherited that deleted directory was refused by the CLI on
	// every spawn ("The current working directory was deleted"), and the
	// listing went 8h50m stale on 2026-10-21. `claude agents` lists every
	// session on the machine whatever its cwd, and "/" always exists.
	cmd.Dir = "/"

	var stdout, stderr boundedBuffer
	stdout.limit = maxOutputBytes
	stderr.limit = stderrExcerpt
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The child inherits no stdin. A vendor CLI that decided to prompt would
	// otherwise block until the deadline instead of failing immediately.
	cmd.Stdin = nil

	start := s.clock.Now()
	err := cmd.Run()
	spawnMs := s.clock.Now().Sub(start).Milliseconds()

	if err != nil {
		if ctxErr := runCtx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, spawnMs, fmt.Errorf("%s: timed out after %s: %w",
				s.CommandLine(), s.spawnTimeout, context.DeadlineExceeded)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, spawnMs, fmt.Errorf("%s: %s: %w", s.CommandLine(), msg, err)
		}
		return nil, spawnMs, fmt.Errorf("%s: %w", s.CommandLine(), err)
	}
	return stdout.Bytes(), spawnMs, nil
}

// waitForPublication is the contended branch. It polls the CACHE file, not the
// lock: the question is "has the answer landed", and a released lock without a
// published answer would send us back for a spawn we agreed not to take.
func (s *Source) waitForPublication(ctx context.Context, start time.Time) observer.AgentsResult {
	// One-shot timers only. §9.3a bans time.After/Tick/NewTicker/Sleep outside
	// one allowlisted site; a timer armed for this wait and stopped when it ends
	// satisfies every clause of the rule it is written under.
	deadline := time.NewTimer(s.lockWait)
	defer deadline.Stop()

	for {
		step := time.NewTimer(lockPollInterval)
		select {
		case <-ctx.Done():
			step.Stop()
			return s.contended(start, ctx.Err())
		case <-deadline.C:
			step.Stop()
			return s.contended(start, nil)
		case <-step.C:
		}
		step.Stop()

		if env, ok := s.readCache(); ok && s.fresh(env, s.clock.Now()) {
			waited := s.clock.Now().Sub(start).Milliseconds()
			return s.fromCache(env, observer.AgentsCacheHitAfterWait, waited)
		}
	}
}

// contended is the answer a process gives when it declined to spawn. OK is
// false, which the liveness policy reads as rule 6b(ii) — "the source never
// ran" — and answers live: null with its evidence rather than guessing.
func (s *Source) contended(start time.Time, err error) observer.AgentsResult {
	return observer.AgentsResult{
		ObservedAt: s.clock.Now(),
		Command:    s.CommandLine(),
		Cache:      observer.AgentsCacheContended,
		WaitedMs:   s.clock.Now().Sub(start).Milliseconds(),
		OK:         false,
		Err:        err,
	}
}

// fromCache decodes a cached envelope into a result.
func (s *Source) fromCache(env envelope, outcome string, waitedMs int64) observer.AgentsResult {
	entries, err := decodeEntries(env.payload)
	if err != nil {
		// A cached payload that will not parse is treated as a miss would be —
		// except that we have already decided not to spawn on this path, so the
		// honest answer is "no corroboration", not "the vendor lists nothing".
		return observer.AgentsResult{
			ObservedAt: env.observedAt,
			Command:    s.CommandLine(),
			Cache:      outcome,
			WaitedMs:   waitedMs,
			OK:         false,
			Err:        err,
		}
	}
	return observer.AgentsResult{
		Entries:    entries,
		ObservedAt: env.observedAt,
		Command:    s.CommandLine(),
		Cached:     true,
		Cache:      outcome,
		WaitedMs:   waitedMs,
		OK:         true,
	}
}

// fresh reports whether a cached listing may still be used. The comparison is
// against the SAME staleAfterMs the core's policy carries, so the cache can
// never serve something the core would immediately mark stale.
func (s *Source) fresh(env envelope, now time.Time) bool {
	if env.observedAt.IsZero() {
		return false
	}
	ageMs := now.Sub(env.observedAt).Milliseconds()
	if ageMs < 0 {
		// A cache written by a process whose clock is ahead of ours. Treat it as
		// fresh rather than as a miss: it was written after we started, which is
		// the strongest possible freshness evidence.
		return true
	}
	return ageMs <= s.staleAfterMs
}

// DefaultStateDir is ~/.local/state/tt, honouring XDG_STATE_HOME.
//
// It is tt's own state directory and the only filesystem location tt writes to.
// XDG_RUNTIME_DIR is deliberately NOT consulted: it is unset on this machine
// (measured), and a cache that moved between a runtime dir and a state dir
// depending on the environment would split the cross-process bound in two.
func DefaultStateDir() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "tt")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory means no cache. Returning "" makes every write fail
		// closed (cache.go refuses an empty root), which degrades to a normal
		// spawn — the documented behaviour for an unwritable cache.
		return ""
	}
	return filepath.Join(home, ".local", "state", "tt")
}

// defaultClaudeRoot mirrors the composition root's own resolution, so a Source
// built with a bare Config still refuses to write into the Claude tree.
func defaultClaudeRoot() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// boundedBuffer collects at most limit bytes and silently drops the rest. A
// child that writes more than the bound is a format change, and the parse
// failure it produces is the right diagnosis; an unbounded read would be a way
// for a runaway process to take the machine down with it.
type boundedBuffer struct {
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
		}
	}
	// The full length is reported so the child never sees a short write.
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte  { return b.buf }
func (b *boundedBuffer) String() string { return string(b.buf) }

var _ observer.AgentsSource = (*Source)(nil)
