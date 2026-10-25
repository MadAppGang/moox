package main

// panesource.go is where an observer PANE gets its data when it must not watch
// for itself: attached to the one `tt observe` server over its socket, or
// waiting for that server to appear.
//
// # Why it exists (code review, MEDIUM 5)
//
// §5.5(b2) says scope="all" is affordable only because every observer pane is a
// thin renderer attached to ONE `tt observe` — ~36 renderers sharing one set of
// kqueue watches, not 36 observers each holding ~57 descriptors. Until this
// file, a pane at scope != "off" armed its own watch whenever a server WAS
// running (the refusal only fired when none was), so the normal case paid the
// full price per pane. Now:
//
//	auto pane (hook-created), or any pane at scope != "off"
//	    -> this source, and nothing else. It never arms a watch of its own.
//	manual pane at scope == "off" (the one-window toggle)
//	    -> this source when a server is running, else its own watch
//	       (one pane may self-observe; 36 may not).
//
// # Waiting without a timer
//
// With no server the pane says so ("observer not running", under a button that
// starts one) and blocks on a kqueue watch of the socket's directory — the
// nearest ancestor that exists, if the directory has not been created yet. The
// server binding its socket is a directory write, which wakes the watch; the
// pane dials and attaches. There is no retry interval and no poll: an idle
// waiting pane executes nothing until the directory changes (the user's rule —
// no timer, no poll loop).
//
// The watch is armed BEFORE the dial, the same arm-before-read rule the
// observer core follows (§6.3): a server that binds between a failed dial and
// the wait is seen either by the dial or by the watch, never by neither.
//
// When the server goes away (`tt observe --stop`, a crash, a reboot) the
// socket reaches EOF, the pane says "observer stopped" and goes back to
// waiting, and a restarted server is picked up the same way — whether it was
// restarted by hand or by the panel's own start button. Attached, the pane
// holds one socket descriptor and no watch; waiting, it holds a kqueue and one
// directory descriptor.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockclient"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
)

// The footer lines the socket source puts on the panel. They travel as the
// Reason of a snapshot-less resync, which panel.apply turns into the note.
// These two are matched EXACTLY by the panel (panelOffline), which draws its
// start button under them. The instruction they used to carry ("- start `tt
// observe`") is the button's job now, and a note that still told the user to go
// and type it would be one instruction too many. The composite notes below —
// the degraded cases, where the pane could not arm a watch and so will never
// see a server appear — deliberately do NOT match, because a button that
// started a server this pane cannot notice would be a lie.
const (
	noteObserverNotRunning = "observer not running"
	noteObserverStopped    = "observer stopped"
	// noteViaObserver opens the note an attached source puts on its baseline,
	// "via tt observe (pid N)". It names the source and says nothing wrong, so
	// the default view does not draw it (panelSourceNote).
	noteViaObserver = "via tt observe"
)

// socketSourceBuffer is how far the source may run ahead of the panel's pump.
// It matches panelBuffer: the pump drains it continuously, and a full buffer
// blocks the forwarder (never drops), which is the same backpressure the loop
// and sockclient apply.
const socketSourceBuffer = panelBuffer

// errWatchEnded is a directory watch whose channel closed under it — the
// kqueue died. The source says so and stops waiting rather than re-arming in a
// loop, which is the spin the HIGH-1 review finding was about.
var errWatchEnded = errors.New("the directory watch ended")

// socketClient is what the source needs from a connection to the server.
// sockclient.Client satisfies it; tests hand in a double.
type socketClient interface {
	Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error)
	Close() error
	ServerPID() int
}

// dirWait is one armed directory watch. wait blocks until the directory
// changes or the watch's context ends; release frees the descriptors and must
// be called exactly once.
type dirWait struct {
	wait    func() error
	release func()
}

// The two seams. They are variables so the tests can drive every transition —
// no server, a server appearing, a server leaving — without a socket, a
// kqueue or a timer.
var (
	dialObserverSocket = func(ctx context.Context, dir string) (socketClient, error) {
		c, err := sockclient.Dial(ctx, sockclient.Config{Dir: dir})
		if err != nil {
			return nil, err
		}
		// Code review round 2, A11: the panel renders the agent colour, the
		// subagent count and the API-error count, and the server sends them
		// only on a connection that asked for an enriched stream (§7.5f).
		// Asked here, once per connection, so every reconnect asks again. A
		// failure costs the colour and nothing else: the stream it would have
		// enriched is still the complete status answer.
		_ = c.EnrichStream()
		return c, nil
	}
	armSocketDirWatch = armDirWatch
)

// socketRefresher is the optional half of a connection: the server's
// `refresh` round trip (§7.5d). sockclient.Client has it; a test double that
// only streams does not, and a source holding one refuses `r` honestly.
type socketRefresher interface {
	RefreshSources(ctx context.Context, sources []string) observer.RefreshOutcome
}

// socketSource is a panelSource that is attached to the one `tt observe`
// server whenever there is one, and waits for it when there is not.
type socketSource struct {
	dir  string
	dial func(ctx context.Context, dir string) (socketClient, error)
	arm  func(ctx context.Context, dir string) (dirWait, error)

	// attached is the connection run is forwarding right now, or nil while it
	// waits. It is what `r` asks; mu guards it because the panel's refresh
	// command runs on its own goroutine.
	mu       sync.Mutex
	attached socketClient
}

// newSocketSource binds the source to a socket directory and to the seams as
// they are right now.
func newSocketSource(dir string) *socketSource {
	return &socketSource{dir: dir, dial: dialObserverSocket, arm: armSocketDirWatch}
}

// Subscribe starts the connect-or-wait loop and returns its stream. The first
// event is either the server's baseline or a snapshot-less resync saying why
// there is none. The stream closes when ctx ends and at no other time.
func (s *socketSource) Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(chan observer.Event, socketSourceBuffer)
	go s.run(ctx, f, out)
	return out, nil
}

// run is the whole lifecycle, one iteration per connection attempt.
func (s *socketSource) run(ctx context.Context, f observer.Filter, out chan<- observer.Event) {
	defer close(out)
	for ctx.Err() == nil {
		w, armErr := s.arm(ctx, s.dir)
		c, err := s.dial(ctx, s.dir)
		if err == nil {
			// Attached: the socket's EOF is the only signal needed from here,
			// so the directory watch is released for the life of the
			// connection.
			if armErr == nil {
				w.release()
			}
			s.attach(c)
			delivered := s.forward(ctx, c, f, out)
			s.attach(nil)
			_ = c.Close()
			if ctx.Err() != nil {
				return
			}
			if !sendEvent(ctx, out, waitingEvent(noteObserverStopped)) {
				return
			}
			if delivered {
				// A connection that served and then ended: the server stopped
				// or restarted. Try again at once — a restart is usually
				// already listening — and wait only if nobody answers.
				continue
			}
			// A connection that ended without delivering even its baseline is
			// never re-dialled in a loop. Wait for the directory instead.
			w, armErr = s.arm(ctx, s.dir)
		} else if !sendEvent(ctx, out, waitingEvent(s.refusalNote(err))) {
			if armErr == nil {
				w.release()
			}
			return
		}

		if armErr != nil {
			// Nothing to wait ON. Say so once and idle: no retry interval, no
			// poll, and the pane still closes normally.
			sendEvent(ctx, out, waitingEvent(fmt.Sprintf(
				"%s - reopen this pane once it runs (cannot watch %s: %v)",
				noteObserverNotRunning, s.dir, armErr)))
			<-ctx.Done()
			return
		}
		werr := w.wait()
		w.release()
		if ctx.Err() != nil {
			return
		}
		if werr != nil {
			sendEvent(ctx, out, waitingEvent(fmt.Sprintf(
				"%s - reopen this pane once it runs (%v)", noteObserverNotRunning, werr)))
			<-ctx.Done()
			return
		}
	}
}

// attach records the connection `r` should ask, or nil when there is none.
func (s *socketSource) attach(c socketClient) {
	s.mu.Lock()
	s.attached = c
	s.mu.Unlock()
}

// Rebuild is `r` on an attached pane: ask the server to re-read the
// corroboration source now. The footer that offers the key ("claude agents
// check failing … — r to retry") is drawn on exactly this source, and until 2026-10-21
// it had no Rebuild, so the key the footer named did nothing. The new listing
// is not returned here: the server publishes it on the stream, where every
// attached pane sees it.
//
// A refusal is an error, which the panel puts in the footer: a key that looks
// like it worked when the server said no would be the same lie in a new place.
func (s *socketSource) Rebuild(ctx context.Context, _ string) (*observer.Snapshot, observer.ArmReport, error) {
	s.mu.Lock()
	c := s.attached
	s.mu.Unlock()
	if c == nil {
		return nil, observer.ArmReport{}, errors.New(noteObserverNotRunning)
	}
	r, ok := c.(socketRefresher)
	if !ok {
		return nil, observer.ArmReport{}, errors.New("this observer cannot refresh")
	}
	out := r.RefreshSources(ctx, []string{observer.SourceAgentsCLI})
	if reason, rejected := out.Rejected[observer.SourceAgentsCLI]; rejected {
		return nil, observer.ArmReport{}, fmt.Errorf("corroboration %s", reason)
	}
	return nil, observer.ArmReport{}, nil
}

// refusalNote says why a dial failed. "Not running" is the ordinary case; a
// server that IS running and refused this pane (over its client cap, or a
// schema this build will not attach to) is named, because telling the user to
// start something that is already running would send them the wrong way.
func (s *socketSource) refusalNote(err error) string {
	if running, pid := observerRunning(s.dir); running {
		return fmt.Sprintf("observer pid %d did not attach this pane: %v", pid, err)
	}
	return noteObserverNotRunning
}

// forward relays one connection's stream until it ends. It reports whether
// anything was delivered, which is what separates "the server stopped" from
// "the server would not serve us".
func (s *socketSource) forward(ctx context.Context, c socketClient, f observer.Filter, out chan<- observer.Event) bool {
	ch, err := c.Subscribe(ctx, f)
	if err != nil {
		return false
	}
	note := fmt.Sprintf("%s (pid %d)", noteViaObserver, c.ServerPID())
	delivered := false
	for {
		select {
		case <-ctx.Done():
			return delivered
		case ev, ok := <-ch:
			if !ok {
				return delivered
			}
			if !delivered && ev.Type == observer.EventSnapshot {
				// The baseline of a fresh connection carries the footer, which
				// replaces whatever "waiting" line the panel was showing.
				ev.Reason = note
			}
			delivered = true
			if !sendEvent(ctx, out, ev) {
				return delivered
			}
		}
	}
}

// waitingEvent is a resync with no snapshot: "there is nothing to show, and
// here is why". panel.apply renders its Reason as the footer.
func waitingEvent(reason string) observer.Event {
	return observer.Event{Type: observer.EventResync, Reason: reason}
}

// sendEvent delivers one event unless ctx ends first.
func sendEvent(ctx context.Context, out chan<- observer.Event, ev observer.Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// armDirWatch arms a kqueue watch on the socket directory, or on the nearest
// ancestor that exists when the directory has not been created yet (a machine
// that has never run `tt observe`). A directory target asks the kernel for
// NOTE_WRITE|NOTE_DELETE|NOTE_RENAME, so a socket being bound, a pid file being
// written, or the directory itself appearing all wake it.
//
// The target is registered under observer.TargetClaudeRoot only because that is
// a DIRECTORY kind the adapter already knows; the kind selects the event flags
// and nothing else, and this watch has nothing to do with ~/.claude.
//
// The watcher gets its own child context so release() ends the adapter's
// cancel goroutine too: without it, every wait cycle would leave one goroutine
// parked until the pane closed.
//
// # The chain appearing while the watch is armed (moox P2, tmuxlive case 25)
//
// The per-server socket directory is three levels below the state root
// (tt/sockets/<key>), and the server creates the whole chain in one MkdirAll.
// A watch on an ancestor wakes on the FIRST level appearing; by the time the
// loop re-arms, the deeper levels may already exist — and a level created
// between choosing the target and arming the kernel watch on it is an event
// nobody sees, leaving the panel waiting on a directory that never changes
// again while the socket is bound one level down. So after arming, the nearest
// existing directory is looked up again; if it moved deeper, this watch is
// dropped and the deeper one armed instead. Each retry goes at least one level
// down, so the loop is bounded by the path's depth.
func armDirWatch(ctx context.Context, dir string) (dirWait, error) {
	for {
		target := nearestExistingDir(dir)
		if target == "" {
			return dirWait{}, fmt.Errorf("no existing directory above %q", dir)
		}
		if dirWatchArming != nil {
			dirWatchArming(target)
		}
		w, err := armDirWatchOn(ctx, target)
		if err != nil {
			return dirWait{}, err
		}
		if now := nearestExistingDir(dir); len(now) > len(target) {
			w.release()
			continue
		}
		return w, nil
	}
}

// dirWatchArming is called between choosing a watch target and arming it. Nil
// in production; a test uses it to create the directory chain in exactly the
// window the re-check in armDirWatch exists for.
var dirWatchArming func(target string)

// armDirWatchOn arms the kqueue watch on one existing directory.
func armDirWatchOn(ctx context.Context, target string) (dirWait, error) {
	w, err := watchkq.New(watchkq.Caps{})
	if err != nil {
		return dirWait{}, err
	}
	wctx, cancel := context.WithCancel(ctx)
	release := func() {
		cancel()
		_ = w.Close()
	}
	rep, err := w.Reconcile(wctx, []observer.WatchTarget{{Kind: observer.TargetClaudeRoot, Path: target}})
	if err == nil && rep.Armed == 0 {
		err = fmt.Errorf("could not arm %s: %v", target, rep.Failures)
	}
	if err != nil {
		release()
		return dirWait{}, err
	}
	hints, err := w.Watch(wctx)
	if err != nil {
		release()
		return dirWait{}, err
	}
	return dirWait{
		wait: func() error {
			select {
			case _, ok := <-hints:
				if !ok {
					return errWatchEnded
				}
				return nil
			case <-wctx.Done():
				return wctx.Err()
			}
		},
		release: release,
	}, nil
}

// nearestExistingDir is dir, or its closest ancestor that exists as a
// directory, or "" if even the root does not. Read-only: nothing is created.
func nearestExistingDir(dir string) string {
	if dir == "" {
		return ""
	}
	cur := filepath.Clean(dir)
	for {
		if st, err := os.Stat(cur); err == nil && st.IsDir() {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}
