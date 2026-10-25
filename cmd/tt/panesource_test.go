package main

// Tests for panesource.go, the observer pane's socket source (code review
// MEDIUM 5). Every transition is driven through the two seams — the dial and
// the directory watch — so nothing here opens a socket or starts a server, and
// NOTHING touches tmux. The one real kqueue test watches a t.TempDir().

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// blockingDirWait is a watch that never fires on its own: it returns when its
// context ends. It is what "no server, nothing changes" looks like.
func blockingDirWait(ctx context.Context) dirWait {
	return dirWait{
		wait:    func() error { <-ctx.Done(); return ctx.Err() },
		release: func() {},
	}
}

// fakeSocketClient is one connection to a pretend server.
type fakeSocketClient struct {
	pid    int
	ch     chan observer.Event
	mu     sync.Mutex
	closed bool
	subErr error
}

func newFakeSocketClient(pid int) *fakeSocketClient {
	return &fakeSocketClient{pid: pid, ch: make(chan observer.Event, 8)}
}

func (f *fakeSocketClient) Subscribe(context.Context, observer.Filter) (<-chan observer.Event, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.ch, nil
}

func (f *fakeSocketClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSocketClient) ServerPID() int { return f.pid }

func (f *fakeSocketClient) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// scriptedSource drives a socketSource through a scripted sequence of dial
// answers and directory events, recording the order of arm and dial calls.
type scriptedSource struct {
	mu       sync.Mutex
	calls    []string
	dials    []func() (socketClient, error)
	wakes    chan struct{}
	armed    int
	released int
	armErr   error
}

func newScriptedSource() *scriptedSource {
	return &scriptedSource{wakes: make(chan struct{}, 8)}
}

func (s *scriptedSource) record(what string) {
	s.mu.Lock()
	s.calls = append(s.calls, what)
	s.mu.Unlock()
}

func (s *scriptedSource) dial(context.Context, string) (socketClient, error) {
	s.record("dial")
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dials) == 0 {
		return nil, stubErr("no server")
	}
	next := s.dials[0]
	s.dials = s.dials[1:]
	return next()
}

func (s *scriptedSource) arm(ctx context.Context, _ string) (dirWait, error) {
	s.record("arm")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armErr != nil {
		return dirWait{}, s.armErr
	}
	s.armed++
	return dirWait{
		wait: func() error {
			select {
			case <-s.wakes:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		release: func() {
			s.mu.Lock()
			s.released++
			s.mu.Unlock()
		},
	}, nil
}

func (s *scriptedSource) source() *socketSource {
	return &socketSource{dir: "/nowhere", dial: s.dial, arm: s.arm}
}

func (s *scriptedSource) snapshot() (calls []string, armed, released int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...), s.armed, s.released
}

func nextEvent(t *testing.T, ch <-chan observer.Event) observer.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the stream closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
	}
	return observer.Event{}
}

// The whole lifecycle: no server -> a server appears -> it serves -> it goes
// away -> waiting again, with every watch released and the watch always armed
// BEFORE the dial it guards.
func TestTheSocketSourceWaitsAttachesAndWaitsAgainWithoutATimer(t *testing.T) {
	ss := newScriptedSource()
	server := newFakeSocketClient(4242)
	ss.dials = []func() (socketClient, error){
		func() (socketClient, error) { return nil, stubErr("no server") },
		func() (socketClient, error) { return server, nil },
		// after the server's stream ends: nobody answers
		func() (socketClient, error) { return nil, stubErr("no server") },
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := ss.source().Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}

	// 1. no server: say so, and wait (nothing else happens until the dir moves).
	if ev := nextEvent(t, ch); ev.Type != observer.EventResync || ev.Snapshot != nil || ev.Reason != noteObserverNotRunning {
		t.Fatalf("first event %+v, want the not-running note", ev)
	}

	// 2. the directory changes (the server bound its socket): dial, attach, and
	// the baseline carries the attach note.
	ss.wakes <- struct{}{}
	base := &observer.Snapshot{Seq: 7}
	server.ch <- observer.Event{Type: observer.EventSnapshot, Snapshot: base}
	ev := nextEvent(t, ch)
	if ev.Type != observer.EventSnapshot || ev.Snapshot != base || ev.Reason != "via tt observe (pid 4242)" {
		t.Fatalf("baseline %+v, want the server's snapshot with the attach note", ev)
	}
	server.ch <- observer.Event{Type: observer.EventSessionChanged, SessionID: "a"}
	if ev := nextEvent(t, ch); ev.Type != observer.EventSessionChanged || ev.Reason != "" {
		t.Fatalf("delta %+v, want it forwarded untouched", ev)
	}

	// 3. the server goes away: the pane says so, closes the client, re-dials
	// at once (a restart is usually already there), finds nobody, and waits.
	close(server.ch)
	if ev := nextEvent(t, ch); ev.Reason != noteObserverStopped || ev.Snapshot != nil {
		t.Fatalf("after EOF %+v, want the stopped note", ev)
	}
	if ev := nextEvent(t, ch); ev.Reason != noteObserverNotRunning {
		t.Fatalf("after the re-dial %+v, want the not-running note", ev)
	}
	if !server.isClosed() {
		t.Fatal("the ended connection was not closed")
	}

	cancel()
	for range ch {
	}
	calls, armed, released := ss.snapshot()
	if armed != released {
		t.Fatalf("armed %d directory watches and released %d: a watch leaked", armed, released)
	}
	// Every dial is immediately preceded by an arm: arm-before-read.
	for i, c := range calls {
		if c == "dial" && (i == 0 || calls[i-1] != "arm") {
			t.Fatalf("dial %d was not preceded by an arm: %v", i, calls)
		}
	}
	if got := strings.Count(strings.Join(calls, " "), "dial"); got != 3 {
		t.Fatalf("%d dials, want 3 (one per scripted answer, and no retry loop): %v", got, calls)
	}
}

// A directory that cannot be watched is not retried on a schedule: the pane
// says so once and idles until it is closed. One dial, one arm, no loop.
func TestASocketSourceThatCannotWatchSaysSoOnceAndIdles(t *testing.T) {
	ss := newScriptedSource()
	ss.armErr = stubErr("kqueue unavailable")

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := ss.source().Subscribe(ctx, observer.Filter{})
	if ev := nextEvent(t, ch); ev.Reason != noteObserverNotRunning {
		t.Fatalf("first event %+v", ev)
	}
	if ev := nextEvent(t, ch); !strings.Contains(ev.Reason, "reopen this pane") || !strings.Contains(ev.Reason, "kqueue unavailable") {
		t.Fatalf("second event %+v, want the cannot-watch note naming the cause", ev)
	}
	time.Sleep(50 * time.Millisecond)
	calls, _, _ := ss.snapshot()
	if strings.Join(calls, " ") != "arm dial" {
		t.Fatalf("calls %v, want exactly one arm and one dial", calls)
	}
	cancel()
	for range ch {
	}
}

// A connection that ends WITHOUT delivering even a baseline is never
// re-dialled in a loop: the source waits for the directory instead. That is
// the guard against a server that accepts and immediately closes.
func TestAConnectionThatDeliversNothingIsNotRedialledInALoop(t *testing.T) {
	ss := newScriptedSource()
	empty := newFakeSocketClient(1)
	close(empty.ch)
	ss.dials = []func() (socketClient, error){func() (socketClient, error) { return empty, nil }}

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := ss.source().Subscribe(ctx, observer.Filter{})
	if ev := nextEvent(t, ch); ev.Reason != noteObserverStopped {
		t.Fatalf("first event %+v, want the stopped note", ev)
	}
	time.Sleep(50 * time.Millisecond)
	calls, armed, _ := ss.snapshot()
	if got := strings.Count(strings.Join(calls, " "), "dial"); got != 1 {
		t.Fatalf("%d dials after an empty connection, want 1: %v", got, calls)
	}
	if armed != 2 {
		t.Fatalf("%d watches armed, want 2 (the first, then a fresh one to wait on)", armed)
	}
	cancel()
	for range ch {
	}
}

// Cancelling the context closes the stream and leaves no goroutine behind,
// whichever state the source was in.
func TestTheSocketSourceLeavesNoGoroutineBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		ss := newScriptedSource()
		ctx, cancel := context.WithCancel(context.Background())
		ch, _ := ss.source().Subscribe(ctx, observer.Filter{})
		nextEvent(t, ch)
		cancel()
		for range ch {
		}
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })
}

// The real directory watch, on a temp dir: a file appearing wakes it, and a
// directory that does not exist yet is watched through its nearest ancestor.
func TestTheDirectoryWatchWakesOnASocketAppearingWithoutATimer(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kqueue is darwin-only in this build")
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "state", "tt")
	if got := nearestExistingDir(stateDir); got != root {
		t.Fatalf("nearestExistingDir(%s) = %s, want %s", stateDir, got, root)
	}
	if got := nearestExistingDir(""); got != "" {
		t.Fatalf("nearestExistingDir(\"\") = %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The directory is missing, so the watch sits on its ancestor; creating
	// the directory chain is what wakes it.
	w, err := armDirWatch(ctx, stateDir)
	if err != nil {
		t.Fatalf("armDirWatch: %v", err)
	}
	woke := make(chan error, 1)
	go func() { woke <- w.wait() }()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("wait returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("creating the directory did not wake the watch")
	}
	w.release()

	// Now the directory exists: a socket file appearing in it wakes a fresh one.
	w, err = armDirWatch(ctx, stateDir)
	if err != nil {
		t.Fatalf("armDirWatch: %v", err)
	}
	go func() { woke <- w.wait() }()
	if err := os.WriteFile(filepath.Join(stateDir, "observer.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("wait returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a file appearing in the directory did not wake the watch")
	}
	w.release()

	// Released watches stay released: a wait after release returns promptly.
	done := make(chan error, 1)
	go func() { done <- w.wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("wait blocked on a released watch")
	}
}

// The panel turns a source's note into its footer: a snapshot-less resync says
// why there is nothing, a baseline's Reason replaces it, and a resync that
// carries a snapshot (sockclient's server-side resync) leaves it alone.
func TestThePanelShowsTheSourcesOwnNote(t *testing.T) {
	p := newPanel(newFakePanelSource(), newPanelClock(time.Now()))
	p.open = true

	p.apply(waitingEvent(noteObserverNotRunning))
	if p.snap != nil || p.note != noteObserverNotRunning {
		t.Fatalf("after a snapshot-less resync: snap=%v note=%q", p.snap, p.note)
	}
	p.width, p.height = panelWidth, 20
	view := p.view()
	if !strings.Contains(view, noteObserverNotRunning) {
		t.Fatalf("the waiting panel does not say what is wrong:\n%s", view)
	}
	// And it offers the fix rather than naming it: the note used to end in
	// "- start `tt observe`", which was a command the user had to go and type
	// somewhere else.
	if !strings.Contains(view, panelStartLabel) {
		t.Fatalf("the waiting panel offers no way to start one:\n%s", view)
	}
	if strings.Contains(view, "`tt observe`") {
		t.Fatalf("the panel still tells the user to type a command:\n%s", view)
	}

	p.apply(observer.Event{Type: observer.EventSnapshot, Snapshot: &observer.Snapshot{}, Reason: "via tt observe (pid 9)"})
	if p.snap == nil || p.note != "via tt observe (pid 9)" {
		t.Fatalf("after the baseline: snap=%v note=%q", p.snap, p.note)
	}

	p.apply(observer.Event{Type: observer.EventResync, Snapshot: &observer.Snapshot{}, Reason: "subscriber-overflow"})
	if p.note != "via tt observe (pid 9)" {
		t.Fatalf("a server-side resync replaced the note with %q", p.note)
	}
}

// refreshingSocketClient is a connection whose server answers the refresh
// round trip with a scripted outcome.
type refreshingSocketClient struct {
	*fakeSocketClient
	outcome observer.RefreshOutcome
	asked   [][]string
}

func (r *refreshingSocketClient) RefreshSources(_ context.Context, sources []string) observer.RefreshOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, sources)
	return r.outcome
}

func (r *refreshingSocketClient) askedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

// `r` on an attached pane. The panel draws "corroboration Nm old — r to
// refresh" on exactly this source, and before 2026-10-21 the key did nothing:
// the socket source had no refresh, so panelRefreshCmd found no refresher and
// returned nil. The refresh is the server's `refresh` round trip; the new
// listing comes back on the stream like any other change.
func TestRefreshOnAnAttachedSocketSourceAsksTheServer(t *testing.T) {
	var _ panelRefresher = (*socketSource)(nil)

	ss := newScriptedSource()
	server := &refreshingSocketClient{
		fakeSocketClient: newFakeSocketClient(4242),
		outcome:          observer.RefreshOutcome{Refreshed: []string{observer.SourceAgentsCLI}},
	}
	ss.dials = []func() (socketClient, error){
		func() (socketClient, error) { return server, nil },
	}
	src := ss.source()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, _, err := src.Rebuild(ctx, "panel-refresh"); err == nil {
		t.Fatal("a source attached to nothing reported a refresh it could not make")
	}

	ch, err := src.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	server.ch <- observer.Event{Type: observer.EventSnapshot, Snapshot: &observer.Snapshot{Seq: 1}}
	nextEvent(t, ch)

	if _, _, err := src.Rebuild(ctx, "panel-refresh"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if server.askedCount() != 1 {
		t.Fatalf("the server was asked %d times, want 1", server.askedCount())
	}

	server.mu.Lock()
	server.outcome = observer.RefreshOutcome{Rejected: map[string]string{observer.SourceAgentsCLI: observer.RejectBackoff}}
	server.mu.Unlock()
	_, _, err = src.Rebuild(ctx, "panel-refresh")
	if err == nil || !strings.Contains(err.Error(), observer.RejectBackoff) {
		t.Fatalf("a rejected refresh returned %v, want an error naming %q", err, observer.RejectBackoff)
	}
}

// A refresh that worked retires the "refresh failed" note an earlier one left;
// any other note (offline, an error from the stream) is not the refresh's to
// clear.
func TestASuccessfulRefreshClearsOnlyARefreshFailure(t *testing.T) {
	p := panel{open: true, gen: 3, note: panelRefreshFailed + "corroboration backoff"}
	p.update(panelRefreshedMsg{gen: 3})
	if p.note != "" {
		t.Fatalf("note = %q after a successful refresh, want it cleared", p.note)
	}
	p.note = noteObserverStopped
	p.update(panelRefreshedMsg{gen: 3})
	if p.note != noteObserverStopped {
		t.Fatalf("note = %q, want the unrelated note kept", p.note)
	}
}

// The chain that appears between choosing the watch target and arming it: the
// per-server socket dir is tt/sockets/<key>, made by one MkdirAll, so a watch
// chosen on an ancestor can miss the levels below it. The watch must end up on
// the deepest directory that exists once it is armed, or the socket being bound
// there never wakes it (tmuxlive case 25: the start button bound the socket and
// the panel kept saying "observer not running").
func TestTheDirectoryWatchFollowsAChainCreatedWhileArming(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kqueue is darwin-only in this build")
	}
	root := t.TempDir()
	sockDir := filepath.Join(root, "tt", "sockets", "key")
	made := false
	dirWatchArming = func(string) {
		if !made {
			made = true
			if err := os.MkdirAll(sockDir, 0o700); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { dirWatchArming = nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := armDirWatch(ctx, sockDir)
	if err != nil {
		t.Fatalf("armDirWatch: %v", err)
	}
	defer w.release()
	woke := make(chan error, 1)
	go func() { woke <- w.wait() }()
	if err := os.WriteFile(filepath.Join(sockDir, "observer.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("wait returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a socket bound in a directory created while the watch was arming did not wake it")
	}
}
