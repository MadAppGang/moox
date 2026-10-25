package sockserve

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
	"github.com/MadAppGang/moox/internal/statedir"
)

// The two file names in tt's state directory. They are constants rather than
// configuration because three other surfaces (§7.5's client, §9.5's doctor,
// §5.5's panel) have to find them without being told.
const (
	SocketName = "observer.sock"
	PidName    = "observer.pid"
)

// DefaultMaxClients is §5.3's cap. Beyond it a connection is answered with
// error{unsupported} and closed, rather than accepted and starved.
//
// 128, up from §5.3's 32, because @tt_obs_scope="all" made observer panes
// clients of this server — one per window you have looked at — and this
// machine carries 35 windows (measured 2026-09-19). At 32, the 33rd window's
// pane would have been refused and would have waited forever. A client costs
// one descriptor, two goroutines and a subscription queue that allocates only
// as it fills, so the cap is a guard against pathology, not a budget.
const DefaultMaxClients = 128

// DefaultWriteTimeout bounds one frame's write. A client that has stopped
// reading and whose socket buffer is full costs the server this long, once,
// and is then closed WITHOUT a bye — a client that cannot be written to cannot
// be told goodbye either.
const DefaultWriteTimeout = 5 * time.Second

// MaxFrameBytes is the longest client line accepted, matching Claude's own
// socket (§5.3) so a consumer that already speaks it needs no new reader. A
// 40-session snapshot is ~33 KB, and nothing a CLIENT sends is large at all.
const MaxFrameBytes = 1 << 20

// maxAcceptErrors bounds a pathological accept loop.
//
// It exists because the alternative is worse in both directions: an accept()
// that keeps failing (EMFILE is the realistic one) would spin at full CPU in a
// server whose whole premise is that it costs nothing when idle, and the
// obvious fix — sleep and retry — is a timer on exactly the path AC9 forbids.
// So a run of consecutive accept failures ends Serve with the error instead,
// where the caller can report it.
const maxAcceptErrors = 16

// Snapshotter is the pull half of the incoming port.
type Snapshotter interface {
	Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error)
}

// Subscriber is the push half: the watch loop.
//
// Latest is part of it because hello and pong both report the state the server
// currently holds, and reaching for a fresh rebuild to answer either would
// make connecting (or pinging) a way to poll.
type Subscriber interface {
	Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error)
	Latest() *observer.Snapshot
}

// Enricher is optional. A nil one means the server does not declare the
// "enrich" capability, and §7.5(f) makes that visible rather than a guess: a
// client seeing no "enrich" in hello reports transcriptCollected:false instead
// of waiting for facts that will not come.
type Enricher interface {
	Enrich(ctx context.Context, snap *observer.Snapshot, ids []string) (*observer.Snapshot, error)
}

// Refresher is optional: the corroboration re-read behind a refresh frame. The
// core owns the rate limit, the single flight and the backoff (§4.3), so this
// adapter adds none of its own — a second rate limiter would be a second
// policy to disagree with the first.
type Refresher interface {
	RefreshSources(ctx context.Context, sources []string) observer.RefreshOutcome
}

// Rebuilder is optional: the one serialized publish path (§7.2). A refresh
// re-reads corroboration and then rebuilds THROUGH THE LOOP, so every
// subscriber sees the corroborated state as deltas and the requesting client's
// sources frame describes the same snapshot everyone else just received.
type Rebuilder interface {
	Rebuild(ctx context.Context, trigger string) (*observer.Snapshot, observer.ArmReport, error)
}

// Deps are the narrow ports, each one a consumer-defined interface for P2
// finding 2's reason: *observer.Observer and *observer.WatchLoop are two
// different objects, and an adapter demanding one fat interface could be given
// neither without stubs that lie.
type Deps struct {
	Events  Subscriber  // required
	Core    Snapshotter // required
	Enrich  Enricher    // optional
	Refresh Refresher   // optional
	Rebuild Rebuilder   // optional
}

// Config is everything the server needs that is not a port.
type Config struct {
	// Dir is tt's state directory — the ONLY place this package writes. It is
	// a constructor parameter precisely so §9.4(b)'s containment test can put
	// it inside t.TempDir() instead of the real $HOME.
	Dir string
	// ClaudeRoot is refused as a write location (hard constraint 1). It is
	// required: statedir answers an empty one with ErrNoClaudeRoot (moox §2.4).
	ClaudeRoot string
	// TT is tt's version string, resolved once by the composition root.
	TT string
	// PID is what hello and the pid file report. Zero means os.Getpid().
	PID int
	// Role is the pid file's role (wire.RoleServe / wire.RoleObserve): which
	// command bound the socket. Empty writes no role.
	Role string
	// NotCapable is the declared list. Nil means observer.DefaultNotCapable.
	NotCapable []string
	// MaxClients zero means DefaultMaxClients.
	MaxClients int
	// WriteTimeout zero means DefaultWriteTimeout.
	WriteTimeout time.Duration
	// WriteBufferBytes bounds the kernel socket buffer per connection. Zero
	// leaves the OS default. It bounds how much a stuck client can hold in
	// kernel memory, and it is what lets a test reach the resync path with a
	// handful of frames instead of megabytes.
	WriteBufferBytes int
	// Clock is read ONCE, at bind, for the pid file's startedAt. It is
	// deliberately not used anywhere else: every timestamp this server
	// publishes derives from the rebuild's own instant, never from a second
	// clock read (P2 finding 1 — the rule three live defects came from
	// breaking).
	Clock observer.Clock
	Deps  Deps
}

// ErrAlreadyRunning says another tt observe holds the socket. It carries the
// path and, when the pid file is readable, that server's pid — so the caller
// can print something actionable and exit 0 rather than treating a healthy
// server as a failure.
type ErrAlreadyRunning struct {
	Path string
	PID  int
}

func (e *ErrAlreadyRunning) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("another tt observe is already listening on %s (pid %d)", e.Path, e.PID)
	}
	return fmt.Sprintf("another tt observe is already listening on %s", e.Path)
}

// ErrOutsideStateDir is returned when the configured directory RESOLVES to a
// location inside the Claude tree — however it is spelled, through however many
// symlinks (code review round 2, A2). The check itself is statedir's; this
// sentinel is what callers of this package test for.
var ErrOutsideStateDir = errors.New("sockserve: refusing to write inside the Claude tree")

// Server is one listening socket and its connections.
type Server struct {
	cfg        Config
	socketPath string
	pidPath    string

	ln *net.UnixListener
	// dir is the verified state directory, held by descriptor from Listen
	// until Close. Every file this server creates or removes is created or
	// removed RELATIVE to it (A2), and sockID/pidID are the identities of the
	// two entries it created, so Close removes those files and never a
	// replacement somebody put at the same name.
	dir    *statedir.Dir
	sockID statedir.Info
	pidID  statedir.Info

	mu     sync.Mutex
	conns  map[int64]*conn
	nextID int64
	closed bool
}

// New validates the configuration and computes the paths. It performs NO I/O:
// binding is Listen's job, so a caller can construct a server and report a
// configuration error before anything exists on disk.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Dir == "":
		return nil, errors.New("sockserve: no state directory")
	case cfg.Deps.Events == nil:
		return nil, errors.New("sockserve: missing port: Subscriber")
	case cfg.Deps.Core == nil:
		return nil, errors.New("sockserve: missing port: Snapshotter")
	}
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	if cfg.NotCapable == nil {
		cfg.NotCapable = observer.DefaultNotCapable()
	}
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = DefaultMaxClients
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = observer.SystemClock{}
	}
	return &Server{
		cfg:        cfg,
		socketPath: filepath.Join(cfg.Dir, SocketName),
		pidPath:    filepath.Join(cfg.Dir, PidName),
		conns:      map[int64]*conn{},
	}, nil
}

// SocketPath is where the server listens.
func (s *Server) SocketPath() string { return s.socketPath }

// PidPath is where the pid file lives.
func (s *Server) PidPath() string { return s.pidPath }

// Clients is how many connections are attached.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Listen binds the socket and writes the pid file.
//
// The order matters and is not arbitrary: the socket is bound FIRST, so a
// second tt observe discovers the first one before it arms 65 kqueue
// descriptors, and the pid file is written only once there is something for it
// to describe. A failure after the bind unwinds the bind.
//
// Everything goes through the verified state directory (statedir): the
// directory is checked by its CANONICAL path against the Claude tree and held
// by descriptor, and a symlink at either of this server's file names refuses
// the start before anything is bound.
func (s *Server) Listen() error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	if err := s.refuseForeignEntries(); err != nil {
		s.releaseDir()
		return err
	}

	ln, err := s.bind()
	if err != nil {
		s.releaseDir()
		return err
	}
	s.ln = ln

	// 0600 inside a 0700 directory, which is the whole of S3's access control
	// (§5.3): filesystem permissions already restrict it to this uid, and
	// there is nothing behind it that uid could not read directly. Hence no
	// token file and no peer-pid check, both of which Claude's socket has and
	// both of which would be ceremony here.
	if err := s.dir.Chmod(SocketName, 0o600); err != nil {
		s.unwindBind()
		return fmt.Errorf("sockserve: chmod %s: %w", s.socketPath, err)
	}
	if err := s.writePidFile(); err != nil {
		s.unwindBind()
		return err
	}
	return nil
}

// ensureDir opens (creating 0700 as needed) the state directory, and refuses
// one that resolves inside the Claude tree.
func (s *Server) ensureDir() error {
	d, err := statedir.Open(s.cfg.Dir, s.cfg.ClaudeRoot)
	if errors.Is(err, statedir.ErrInsideClaudeTree) {
		return fmt.Errorf("%w: %v", ErrOutsideStateDir, err)
	}
	if err != nil {
		return fmt.Errorf("sockserve: %w", err)
	}
	s.dir = d
	return nil
}

// refuseForeignEntries refuses to start over a symlink at either of this
// server's names. A link at observer.pid is exactly what made os.WriteFile
// truncate its target (A2); a link at observer.sock would be dialled THROUGH by
// the stale-socket rule. Neither is something this server put there.
func (s *Server) refuseForeignEntries() error {
	for _, name := range []string{PidName, SocketName} {
		info, err := s.dir.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sockserve: %w", err)
		}
		if info.Type == statedir.TypeSymlink {
			return fmt.Errorf("sockserve: %s is a symlink; refusing to start over it: %w",
				s.dir.Join(name), statedir.ErrSymlink)
		}
	}
	return nil
}

// unwindBind undoes a bind that Listen cannot complete: the listener closes and
// the socket it created — that inode and no other — is removed.
func (s *Server) unwindBind() {
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
	}
	_ = s.dir.Remove(SocketName, statedir.TypeSocket, &s.sockID)
	s.releaseDir()
}

func (s *Server) releaseDir() {
	if s.dir != nil {
		_ = s.dir.Close()
	}
}

// bind implements §5.3's stale-socket rule, which never unlinks blindly.
//
//	bind -> EADDRINUSE ?
//	  -> net.Dial it
//	      -> dial succeeds  : another tt observe is live. Report it, exit 0.
//	      -> ECONNREFUSED   : stale socket from a crashed process. Unlink once.
//	      -> any other error: refuse to unlink. Fail.
//
// The third branch is the one that matters: unlinking a socket we could not
// dial for an unknown reason is how a healthy server gets silently displaced,
// and the failure mode of refusing is a message the user can act on.
func (s *Server) bind() (*net.UnixListener, error) {
	ln, id, err := s.dir.ListenUnix(SocketName)
	if err == nil {
		s.sockID = id
		return ln, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("sockserve: listening on %s: %w", s.socketPath, err)
	}

	probe, derr := net.Dial("unix", s.socketPath)
	switch {
	case derr == nil:
		_ = probe.Close()
		return nil, &ErrAlreadyRunning{Path: s.socketPath, PID: s.recordedPID()}
	case errors.Is(derr, syscall.ECONNREFUSED):
		// Nobody is listening: the file outlived its process. It is removed
		// only if it really is a socket — a regular file at this name is
		// somebody's data, whatever dial said about it.
		if rerr := s.dir.Remove(SocketName, statedir.TypeSocket, nil); rerr != nil {
			return nil, fmt.Errorf("sockserve: removing the stale socket %s: %w", s.socketPath, rerr)
		}
		ln, id, err = s.dir.ListenUnix(SocketName)
		if err != nil {
			return nil, fmt.Errorf("sockserve: re-binding %s after removing a stale socket: %w", s.socketPath, err)
		}
		s.sockID = id
		return ln, nil
	default:
		return nil, fmt.Errorf("sockserve: %s exists and cannot be dialled; refusing to unlink it: %w",
			s.socketPath, derr)
	}
}

// recordedPID reads the pid out of the pid file, best effort. It is used only
// to make an "already running" message actionable, so an unreadable or
// malformed file degrades to 0 rather than to an error.
func (s *Server) recordedPID() int {
	data, _, err := s.dir.ReadFile(PidName)
	if err != nil {
		return 0
	}
	rec, err := wire.DecodePidFile(data)
	if err != nil {
		return 0
	}
	return rec.PID
}

// writePidFile publishes the server's identity.
//
// This is the ONE clock read in the package, and it is for startedAt — a fact
// about this process rather than an observation about a session, and there is
// no snapshot at bind time to take an instant from.
func (s *Server) writePidFile() error {
	data, err := wire.EncodePidFile(wire.PidFile{
		PID:       s.cfg.PID,
		StartedAt: observer.FormatTime(s.cfg.Clock.Now()),
		Socket:    s.socketPath,
		TT:        s.cfg.TT,
		Role:      s.cfg.Role,
	})
	if err != nil {
		return err
	}
	// Temp file + renameat inside the verified directory, never a write
	// THROUGH whatever sits at the name (A2).
	if err := s.dir.WriteFile(PidName, data, 0o600); err != nil {
		return fmt.Errorf("sockserve: writing %s: %w", s.pidPath, err)
	}
	id, err := s.dir.Lstat(PidName)
	if err != nil {
		return fmt.Errorf("sockserve: %w", err)
	}
	s.pidID = id
	return nil
}

// Serve accepts connections until ctx is done, then says goodbye and cleans up.
//
// It blocks in accept() and nowhere else. There is no tick, no sweep and no
// deadline on the listener: an idle server with no clients is one blocked
// syscall.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("sockserve: Serve called before Listen")
	}

	// Unblock accept() on cancellation by closing the listener. The done
	// channel stops this goroutine leaking when Serve returns for any other
	// reason, which a bare `go func(){<-ctx.Done()}` would do.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.closeListener()
		case <-done:
		}
	}()

	var (
		wg       sync.WaitGroup
		failures int
		serveErr error
	)
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			failures++
			if failures >= maxAcceptErrors {
				serveErr = fmt.Errorf("sockserve: accept failed %d times in a row: %w", failures, err)
				break
			}
			continue
		}
		failures = 0

		c, admitted := s.admit(ctx, nc)
		if !admitted {
			// Over the cap: an ANSWER and then a close, so the client learns
			// why instead of seeing an unexplained EOF.
			s.refuse(nc)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.serve()
			s.forget(c.id)
		}()
	}

	// Shutdown: cancel every connection, which makes each one write its own
	// bye and close its own socket (one writer per connection, always). Each
	// write is bounded by WriteTimeout, so a stuck client delays this by that
	// much once and cannot hang it.
	s.stopConns()
	wg.Wait()
	_ = s.Close()
	return serveErr
}

// admit registers a connection if there is room. The connection's context is
// derived from ctx here, under s.mu, before the conn becomes visible to
// stopConns (code review MEDIUM 6).
func (s *Server) admit(ctx context.Context, nc net.Conn) (*conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) >= s.cfg.MaxClients {
		return nil, false
	}
	s.nextID++
	c := newConn(ctx, s.nextID, s, nc)
	s.conns[c.id] = c
	return c, true
}

// refuse answers a connection over the cap and closes it.
func (s *Server) refuse(nc net.Conn) {
	defer func() { _ = nc.Close() }()
	f := wire.NewError(nil, wire.CodeUnsupported,
		fmt.Sprintf("too many clients: %d is the limit", s.cfg.MaxClients))
	f.Seq = 1
	_ = writeFrame(nc, f, s.cfg.WriteTimeout)
}

// forget drops a finished connection.
func (s *Server) forget(id int64) {
	s.mu.Lock()
	delete(s.conns, id)
	s.mu.Unlock()
}

// stopConns cancels every attached connection.
func (s *Server) stopConns() {
	s.mu.Lock()
	list := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		list = append(list, c)
	}
	s.mu.Unlock()
	for _, c := range list {
		c.stop()
	}
}

// Close releases the socket and the pid file. It is idempotent, so the
// ordinary path (Serve returning) and a failure path (Listen unwinding) can
// both call it.
//
// Both files are removed HERE, through the verified directory, and only if
// each is still the very inode this server created: Go's unlink-on-close is
// off (it unlinks by path), and a server that found its socket or pid file
// replaced while it ran leaves the replacement alone. A server that crashes
// leaves both behind, which is exactly the state bind's stale-socket rule and
// Stop's stale branch are written for.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	err := s.closeListener()
	if s.dir == nil {
		return err
	}
	if s.ln != nil {
		if rerr := s.dir.Remove(SocketName, statedir.TypeSocket, &s.sockID); rerr != nil &&
			!errors.Is(rerr, fs.ErrNotExist) && err == nil {
			err = fmt.Errorf("sockserve: removing %s: %w", s.socketPath, rerr)
		}
	}
	if s.pidID != (statedir.Info{}) {
		if rerr := s.dir.Remove(PidName, statedir.TypeRegular, &s.pidID); rerr != nil &&
			!errors.Is(rerr, fs.ErrNotExist) && err == nil {
			err = fmt.Errorf("sockserve: removing %s: %w", s.pidPath, rerr)
		}
	}
	s.releaseDir()
	return err
}

// closeListener closes the listener once, tolerating the already-closed case
// that cancellation and Close both reach.
func (s *Server) closeListener() error {
	if s.ln == nil {
		return nil
	}
	if err := s.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("sockserve: closing the listener: %w", err)
	}
	return nil
}

// capabilities is what hello declares, derived from the ports that are
// actually wired. A capability for a nil port would be a promise this build
// cannot keep, and §7.5(f) has clients act on the absence.
func (s *Server) capabilities() []string {
	caps := []string{wire.CapSnapshot, wire.CapDeltas, wire.CapFilter, wire.CapPing, wire.CapIncludeDead,
		wire.CapIncludeSpawned}
	if s.cfg.Deps.Refresh != nil {
		caps = append(caps, wire.CapRefresh)
	}
	if s.cfg.Deps.Enrich != nil {
		caps = append(caps, wire.CapEnrich)
	}
	return caps
}

// ---------------------------------------------------------------------------
// --stop
// ---------------------------------------------------------------------------

// StopOutcome is what Stop did, so the caller can print one honest line.
type StopOutcome struct {
	// Found is whether a pid file existed at all.
	Found bool
	// PID is the pid it named, 0 when there was none.
	PID int
	// Signalled is whether SIGTERM was delivered.
	Signalled bool
	// StaleCleaned is whether a pid file naming a dead process — and the
	// socket beside it — were removed.
	StaleCleaned bool
	// Path is the pid file consulted.
	Path string
	// Socket is the socket the record named, which is what was dialled.
	Socket string
	// Corroborated names the evidence that the pid really is this server:
	// "hello" when the server answering the socket declared that pid itself,
	// "start-time" when the process's start instant matched the record's.
	// Empty on every outcome that signalled nothing.
	Corroborated string
	// Refused is set when the pid could NOT be corroborated. Nothing was
	// signalled and nothing was removed; Reason says what was seen.
	Refused bool
	// Reason is why a refusal refused, in a sentence the user can act on.
	Reason string
	// Kept names an entry a stale cleanup found at the socket's name and
	// deliberately left alone because it is not a socket (A1): a regular file
	// there is somebody's data, whatever the pid file says.
	Kept string
}

// stopDialBound and stopHelloBound are how long corroboration may take. Both
// are read/connect deadlines on a call already in flight rather than scheduled
// timers, which is the same shape §9.3a permits everywhere else in this file.
const (
	stopDialBound  = 2 * time.Second
	stopHelloBound = 2 * time.Second
)

// stopStartSkew and stopStartSkewBack bound how far the pid file's startedAt
// may sit from the process's real start instant and still corroborate it.
//
// Forward (the record is LATER than the start) is the normal direction and
// gets a minute: the record is written at bind, after flag parsing and after
// the source stack is built, so it is always a little after the process began.
// A minute is generous for that and still small enough that a recycled pid
// would have to be born inside the same minute to slip through.
//
// Backward gets two seconds rather than zero, and the reason is measured: a
// record written by a tool that truncates its own timestamps, or a clock
// stepped by NTP between the process starting and the file being written, can
// put startedAt fractionally BEFORE the start instant the kernel reports. Zero
// tolerance turns that into a refusal on a perfectly healthy server — which is
// how a safety check becomes the thing the user routes around. Two seconds is
// far below the hours-to-days gap a pid recycled across a reboot produces.
const (
	stopStartSkew     = time.Minute
	stopStartSkewBack = 2 * time.Second
)

// stopVerdict is what corroboration concluded about the recorded pid.
type stopVerdict int

const (
	// stopOurs: the pid is this server, on evidence.
	stopOurs stopVerdict = iota
	// stopStale: nothing is listening on the recorded socket, so the pair
	// outlived its process and neither file should be believed.
	stopStale
	// stopUnknown: it could not be established either way. Refuse.
	stopUnknown
)

type stopEvidence struct {
	verdict  stopVerdict
	evidence string
	reason   string
}

// Stop implements `tt observe --stop`: SIGTERM to the pid in the pid file —
// but only to a pid something else agrees is this server.
//
// It lives here rather than in package main because the stale branch REMOVES
// files, and it does so through the same verified state directory (statedir,
// the one package §9.4a permits to write) that created them — see StopIn for
// exactly what it may touch.
//
// # Why the pid is corroborated first
//
// The pid file is removed only on a clean Close, so a SIGKILL, a panic or a
// reboot leaves it behind — and ~/.local/state/tt/ survives reboots, after
// which pids restart low and climb back through the same range. Signalling the
// integer found in that file therefore eventually signals an unrelated live
// process owned by this uid, and on this machine the live Claude pids and the
// recycled range fully overlap (measured: 1061-98693). The victim of a stale
// pid file is most likely one of the user's own Claude sessions, killed by the
// one command in a read-only tool that sends a signal.
//
// bind() already refuses to unlink a socket it cannot dial, for the same
// reason in the other direction — "unlinking a socket we could not dial for an
// unknown reason is how a healthy server gets silently displaced" — so this is
// that care applied to the pid, using what the record already carries.
//
// FOUR outcomes now, of which three are exit 0:
//
//	signalled : the socket answered and the server named that pid itself, or
//	            the process's start instant matched the record's.
//	stale     : nothing is listening. Both files go, nothing is signalled.
//	nothing   : no pid file at all. "No server is running" is a fact, like "no
//	            Claude is running" on S1, and stopping twice must not fail.
//	refused   : the pid could not be corroborated. Nothing is signalled and
//	            nothing is removed — a refusal the user can act on beats a
//	            SIGTERM they cannot take back.
//
// Stop names the Claude tree as claudeRoot: statedir refuses an empty one
// (ErrNoClaudeRoot, moox §2.4), so "no tree to avoid" cannot be said by
// omission any more.
func Stop(dir, claudeRoot string) (StopOutcome, error) {
	return StopIn(StopConfig{Dir: dir, ClaudeRoot: claudeRoot})
}

// StopConfig is what a contained --stop needs.
type StopConfig struct {
	// Dir is the state directory. The pid file AND the socket are both
	// derived from it and from nothing else.
	Dir string
	// ClaudeRoot is refused as a location, exactly as Listen refuses it.
	// Required, as for Listen (ErrNoClaudeRoot on empty, moox §2.4).
	ClaudeRoot string
}

// StopIn is Stop with the Claude tree named, which is what the CLI calls.
//
// # What it will touch (code review round 2, A1)
//
// Stop used to take the pid file's "socket" key as a DELETION TARGET: with the
// recorded pid gone it unlinked that path, whatever and wherever it was. A
// stale or hand-edited record naming a Claude transcript made the ordinary
// cleanup command delete that transcript. Now:
//
//   - the only two entries it can ever remove are PidName and SocketName in
//     the VERIFIED state directory (canonical path checked against the Claude
//     tree, held by descriptor, never following a symlink);
//   - the socket it dials and removes is derived from that directory, and a
//     record naming any OTHER socket is refused outright — nothing signalled,
//     nothing removed — because a record that disagrees with where it was
//     found cannot be believed about anything else either;
//   - a symlinked or non-regular pid file is refused the same way;
//   - removal checks TYPE and identity: the pid file only if it is still the
//     regular file that was read, the socket only if it really is a socket.
func StopIn(cfg StopConfig) (StopOutcome, error) {
	out := StopOutcome{Path: filepath.Join(cfg.Dir, PidName)}

	d, err := statedir.OpenExisting(cfg.Dir, cfg.ClaudeRoot)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No state directory means no pid file: "no server is running".
		return out, nil
	case errors.Is(err, statedir.ErrInsideClaudeTree):
		return out, fmt.Errorf("%w: %v", ErrOutsideStateDir, err)
	case err != nil:
		return out, fmt.Errorf("sockserve: %w", err)
	}
	defer func() { _ = d.Close() }()
	out.Path = d.Join(PidName)

	data, pidInfo, err := d.ReadFile(PidName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case errors.Is(err, statedir.ErrSymlink), errors.Is(err, statedir.ErrNotRegular):
		out.Found = true
		out.Refused, out.Reason = true, fmt.Sprintf(
			"%s is not a regular file (%v); refusing to act on it", out.Path, err)
		return out, nil
	case err != nil:
		return out, fmt.Errorf("sockserve: reading %s: %w", out.Path, err)
	}
	rec, err := wire.DecodePidFile(data)
	if err != nil {
		return out, err
	}
	out.Found = true
	out.PID = rec.PID
	out.Socket = d.Join(SocketName)

	if rec.Socket != "" && !sameSocket(rec.Socket, d) {
		out.Refused, out.Reason = true, fmt.Sprintf(
			"%s names the socket %s, but the socket of this state directory is %s; refusing "+
				"to signal or remove anything on the word of a record that points elsewhere",
			PidName, rec.Socket, out.Socket)
		return out, nil
	}

	switch ev := corroborate(rec, out.Socket); ev.verdict {
	case stopStale:
		return cleanStalePair(d, out, pidInfo)
	case stopUnknown:
		out.Refused, out.Reason = true, ev.reason
		return out, nil
	default:
		out.Corroborated = ev.evidence
	}

	switch err := syscall.Kill(rec.PID, syscall.SIGTERM); {
	case err == nil:
		out.Signalled = true
		return out, nil
	case errors.Is(err, syscall.ESRCH):
		// It died between the corroboration and the signal. That is the
		// crashed-server state arriving a few microseconds late, and it
		// reconciles the same way.
		out.Corroborated = ""
		return cleanStalePair(d, out, pidInfo)
	default:
		return out, fmt.Errorf("sockserve: signalling pid %d: %w", rec.PID, err)
	}
}

// sameSocket reports whether a recorded socket path names SocketName in the
// verified directory, by spelling or by canonical location. The server writes
// filepath.Join(cfg.Dir, SocketName), so a record from this directory matches
// one way or the other even when --socket-dir was spelled through a symlink.
func sameSocket(recorded string, d *statedir.Dir) bool {
	if filepath.Base(recorded) != SocketName {
		return false
	}
	abs, err := filepath.Abs(recorded)
	if err != nil {
		return false
	}
	if filepath.Dir(abs) == d.Path() {
		return true
	}
	canon, err := statedir.Canonical(filepath.Dir(abs))
	return err == nil && canon == d.Canonical()
}

// cleanStalePair removes a pid file whose process is gone and the socket beside
// it — the same reconciliation bind's ECONNREFUSED branch performs from the
// other direction — through the verified directory, and only entries of the
// right type: the pid file only if it is still the file that was read, the
// socket only if it is a socket.
func cleanStalePair(d *statedir.Dir, out StopOutcome, pidInfo statedir.Info) (StopOutcome, error) {
	if err := d.Remove(PidName, statedir.TypeRegular, &pidInfo); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, fmt.Errorf("sockserve: removing the stale %s: %w", out.Path, err)
	}
	switch err := d.Remove(SocketName, statedir.TypeSocket, nil); {
	case err == nil, errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, statedir.ErrWrongType):
		out.Kept = err.Error()
	default:
		return out, fmt.Errorf("sockserve: removing the stale %s: %w", out.Socket, err)
	}
	out.StaleCleaned = true
	return out, nil
}

// corroborate asks whether the recorded pid is really this server.
//
//	is the pid a live process at all?
//	  -> no  : there is nothing to signal and both files outlived their
//	           server. Stale pair, cleaned, nothing signalled.
//	  -> yes : dial the recorded socket
//	      -> a hello naming THAT pid        : ours. The server said so itself,
//	                                          and a recycled pid cannot forge
//	                                          a hello on our own socket.
//	      -> a hello naming a different pid : refuse. The pid file and the live
//	                                          server disagree and we do not
//	                                          know which of them is stale.
//	      -> answered, nothing usable       : fall through to the start
//	                                          instant, so a WEDGED server can
//	                                          still be stopped.
//	      -> ECONNREFUSED / ENOENT          : the socket is gone but the pid
//	                                          lives. Only the start instant can
//	                                          say whether it is ours.
//	      -> anything else                  : refuse, exactly as bind() does
//	                                          with a socket it cannot dial.
func corroborate(rec wire.PidFile, socket string) stopEvidence {
	if _, err := processStartedAt(rec.PID); err != nil {
		// Not a process: the pid file and the socket outlived the server that
		// wrote them. This is also the branch a pid above the kernel's ceiling
		// takes, which is how the stale case is testable without picking a
		// recycled pid and risking one of the user's own processes.
		return stopEvidence{verdict: stopStale}
	}

	conn, derr := net.DialTimeout("unix", socket, stopDialBound)
	switch {
	case derr == nil:
		defer func() { _ = conn.Close() }()
		switch pid, herr := helloPID(conn); {
		case herr != nil:
			return startTimeEvidence(rec, fmt.Sprintf(
				"%s answered but sent no usable hello (%v)", socket, herr))
		case pid == rec.PID:
			return stopEvidence{verdict: stopOurs, evidence: "hello"}
		default:
			return stopEvidence{verdict: stopUnknown, reason: fmt.Sprintf(
				"the server answering %s reports pid %d, but %s names pid %d; "+
					"refusing to signal a pid no running observer claims",
				socket, pid, PidName, rec.PID)}
		}
	case errors.Is(derr, syscall.ECONNREFUSED), errors.Is(derr, os.ErrNotExist):
		// Linux answers a connect() to a regular file with ECONNREFUSED too,
		// where darwin says ENOTSOCK and lands in the refusal below. Something
		// at this path that is not a socket is not a server that died, so it
		// gets that refusal on both.
		if fi, lerr := os.Lstat(socket); lerr == nil && fi.Mode()&os.ModeSocket == 0 {
			return stopEvidence{verdict: stopUnknown, reason: fmt.Sprintf(
				"%s exists and is not a socket; refusing to signal pid %d on the word of "+
					"a file that survives crashes and reboots", socket, rec.PID)}
		}
		return startTimeEvidence(rec, fmt.Sprintf("nothing is listening on %s", socket))
	default:
		return stopEvidence{verdict: stopUnknown, reason: fmt.Sprintf(
			"%s exists and cannot be dialled (%v); refusing to signal pid %d on the word of "+
				"a file that survives crashes and reboots", socket, derr, rec.PID)}
	}
}

// helloPID reads the first frame off a freshly dialled connection and returns
// the pid the server declared. Bounded by a read deadline, and it reads ONE
// line: hello is the first thing every connection is sent.
func helloPID(conn net.Conn) (int, error) {
	if err := conn.SetReadDeadline(time.Now().Add(stopHelloBound)); err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), MaxFrameBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("the connection closed before hello")
	}
	frame, _, err := wire.DecodeFrame(sc.Bytes())
	if err != nil {
		return 0, err
	}
	if frame.Type != wire.FrameHello {
		return 0, fmt.Errorf("the first frame was %q, not hello", frame.Type)
	}
	if frame.PID <= 0 {
		return 0, errors.New("hello declared no pid")
	}
	return frame.PID, nil
}

// startTimeEvidence is the second gate, and it defeats pid reuse independently
// of the socket: a recycled pid is a process that started later than the record
// that names it.
//
// It reads the kernel's process table through sysctl rather than spawning ps —
// a read-only syscall, no child process, nothing to accumulate.
func startTimeEvidence(rec wire.PidFile, why string) stopEvidence {
	recorded, err := observer.ParseTime(rec.StartedAt)
	if err != nil {
		return stopEvidence{verdict: stopUnknown, reason: fmt.Sprintf(
			"%s, and %s carries no usable startedAt to corroborate pid %d against",
			why, PidName, rec.PID)}
	}
	started, err := processStartedAt(rec.PID)
	if err != nil {
		return stopEvidence{verdict: stopUnknown, reason: fmt.Sprintf(
			"%s, and pid %d's start instant could not be read (%v)", why, rec.PID, err)}
	}
	skew := recorded.Sub(started)
	if skew < -stopStartSkewBack || skew > stopStartSkew {
		return stopEvidence{verdict: stopUnknown, reason: fmt.Sprintf(
			"%s, and pid %d started at %s while the record was written at %s — that is a "+
				"different process wearing a recycled pid, not this server",
			why, rec.PID, observer.FormatTime(started), rec.StartedAt)}
	}
	return stopEvidence{verdict: stopOurs, evidence: "start-time"}
}
