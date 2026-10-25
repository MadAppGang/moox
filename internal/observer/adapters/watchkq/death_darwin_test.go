//go:build darwin

package watchkq

// The dead-watch regression, at the adapter end.
//
// `loop()` returns on ANY non-EINTR kevent() error, not only on the wake-up
// Close triggers. Before this test existed, that path closed the hint channel
// and left `closed` false, so the next Watch() handed the SAME closed channel
// back with a nil error — and a caller written to re-arm on failure span on it
// at ~1.08M calls/second while Mode() still said "kqueue".
//
// This is an internal test because the condition it drives — the loop exiting
// on a kernel error — cannot be produced from outside the package without
// either a fake kevent() in the production path or closing a descriptor out
// from under a blocked kevent(), which on darwin does not reliably return. So
// it calls the loop's one exit path directly, on a watcher whose loop was
// never started, and asserts what that exit path must leave behind.
//
// Nothing here goes near ~/.claude/: the only target armed is a file this test
// created in t.TempDir().

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
)

func TestADeadLoopMarksTheWatcherUnsupportedRatherThanReturningItsClosedChannel(t *testing.T) {
	w, err := New(Caps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The state a watcher is in when its loop is running: started, so Watch()
	// takes the "already going" route that returns w.hints unconditionally.
	w.mu.Lock()
	w.started = true
	w.mu.Unlock()
	hints := w.hints

	w.markDead()

	ch, err := w.Watch(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Watch after the loop died returned err=%v, want ErrUnsupported: a nil error "+
			"here is what turns a dead watch into a re-arm spin", err)
	}
	if ch != nil {
		t.Errorf("Watch after the loop died returned a channel (%v); the caller must be told to "+
			"stop selecting, not handed something to select on", ch)
	}
	if _, open := <-hints; open {
		t.Error("the hint channel was not closed, so a consumer would block forever")
	}

	// Reconcile must refuse too — an arm on a dead kqueue is an EBADF dressed
	// as an arm failure.
	if _, rerr := w.Reconcile(context.Background(), nil); !errors.Is(rerr, ErrUnsupported) {
		t.Errorf("Reconcile after the loop died returned %v, want ErrUnsupported", rerr)
	}

	// Close is idempotent over it.
	if cerr := w.Close(); cerr != nil {
		t.Errorf("Close after the loop died returned %v, want nil", cerr)
	}
}

// Close() returns early when `closed` is already set, so the exit path that
// sets it inherits the descriptors Close would have released. A watch that dies
// holding ~57 open descriptors inside ~/.claude/ is the accumulation this
// repository has been burned by twice.
func TestADeadLoopReleasesTheDescriptorsItWasHolding(t *testing.T) {
	w, err := New(Caps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if werr := os.WriteFile(path, []byte("{}\n"), 0o600); werr != nil {
		t.Fatalf("writing the fixture: %v", werr)
	}
	if _, rerr := w.Reconcile(context.Background(), []observer.WatchTarget{
		{Kind: observer.TargetJobFile, Path: path},
	}); rerr != nil {
		t.Fatalf("Reconcile: %v", rerr)
	}
	if len(w.FDs()) != 1 {
		t.Fatalf("the fixture target was not armed: %v", w.FDs())
	}

	w.mu.Lock()
	w.started = true
	w.mu.Unlock()
	w.markDead()

	if fds := w.FDs(); len(fds) != 0 {
		t.Errorf("a dead loop left %d descriptors armed: %v", len(fds), fds)
	}
}
