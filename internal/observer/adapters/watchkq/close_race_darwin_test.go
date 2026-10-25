//go:build darwin

package watchkq

// Code review round 2, G1: Close() posted its EVFILT_USER wake-up AFTER
// releasing the mutex, while the loop's deferred markDead() closes the kqueue
// under that same mutex. A loop that exited on its own (any kevent() error)
// could therefore close the kqueue between Close's unlock and its kevent(), and
// Close then issued kevent() on a closed descriptor — or, had anything opened a
// file in that window, on a REUSED one. And Close returned without waiting for
// the loop, so "closed" did not mean the reader had stopped.
//
// Internal tests, for the same reason death_darwin_test.go is one: the loop
// exiting on its own cannot be produced from outside the package. Here it is
// produced the way the kernel would see it — a wake-up event on the kqueue
// that Close did not post — racing a Close.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// After Close returns, the reader has stopped: the hint channel is closed and
// the kqueue descriptor is released. Before the fix Close returned as soon as
// it had POSTED the wake-up, with the loop still running.
func TestCloseReturnsOnlyAfterTheLoopHasStoppedAndReleasedTheKqueue(t *testing.T) {
	for i := 0; i < 100; i++ {
		w, err := New(Caps{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		hints, err := w.Watch(ctx)
		if err != nil {
			cancel()
			t.Fatalf("Watch: %v", err)
		}
		if err := w.Close(); err != nil {
			cancel()
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		select {
		case _, open := <-hints:
			if open {
				t.Fatalf("iteration %d: a hint arrived after Close", i)
			}
		default:
			cancel()
			t.Fatalf("iteration %d: Close returned while the kevent loop was still running", i)
		}
		w.mu.Lock()
		kq := w.kq
		w.mu.Unlock()
		if kq != -1 {
			t.Fatalf("iteration %d: Close returned with the kqueue (fd %d) still held", i, kq)
		}
		cancel()
	}
}

// The race itself: the loop dies on its own at the same moment Close runs.
// Close must never issue kevent() on a kqueue the loop's exit path closed.
//
// The interleaving is forced rather than hoped for. The test holds w.mu, fires
// a wake-up nobody called Close for (the loop returns on it exactly as it
// returns on any kevent() error), and waits until the dying loop is queued on
// the mutex inside markDead. Close is then started and queues on it too. When
// the test lets go, whichever wins, the other runs immediately after — and the
// old Close, having set `closed` and unlocked, then called kevent() on the
// descriptor markDead had just closed: EBADF, or with a busy process a kevent()
// on whatever file had been handed that number.
func TestACloseRacingALoopThatDiesOnItsOwnNeverUsesAClosedKqueue(t *testing.T) {
	failures := 0
	for i := 0; i < 200; i++ {
		w, err := New(Caps{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		hints, err := w.Watch(ctx)
		if err != nil {
			cancel()
			t.Fatalf("Watch: %v", err)
		}

		w.mu.Lock()
		die := unix.Kevent_t{Ident: wakeIdent, Filter: unix.EVFILT_USER, Flags: unix.EV_ENABLE, Fflags: unix.NOTE_TRIGGER}
		_, _ = unix.Kevent(w.kq, []unix.Kevent_t{die}, nil, nil)
		time.Sleep(time.Millisecond) // the loop wakes and blocks in markDead's Lock

		closed := make(chan error, 1)
		go func() { closed <- w.Close() }()
		time.Sleep(time.Millisecond) // Close blocks on the same Lock
		w.mu.Unlock()

		if err := <-closed; err != nil {
			failures++
			t.Errorf("iteration %d: Close raced the loop's own exit and failed: %v — it issued "+
				"kevent() on a kqueue the exit path had already closed", i, err)
		}
		if _, open := <-hints; open {
			t.Errorf("iteration %d: the hint channel is still open", i)
		}
		cancel()
		if failures >= 5 {
			t.Fatalf("stopping after %d failures", failures)
		}
	}
}

// The structural half of G1, which fails on the EDIT rather than on a lucky
// schedule: in Close, every kevent() on the kqueue happens while w.mu is held.
// markDead closes the kqueue under that mutex, so a kevent() issued after
// Close's Unlock can land on a descriptor that is already closed — or reused.
// The forced interleaving above cannot hit the old window reliably (Go's mutex
// hands the lock to the queued markDead first, and the gap between Unlock and
// kevent is sub-microsecond), so this is the assertion that pins the rule.
func TestCloseIssuesNoKeventAfterReleasingTheMutex(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "watch_darwin.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var closeFn *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "Close" && fn.Recv != nil {
			closeFn = fn
		}
	}
	if closeFn == nil {
		t.Fatal("(*Watcher).Close is gone from watch_darwin.go")
	}
	// Walk Close in source order, tracking whether the mutex is held. A branch
	// that ends in `return` (the early-outs) is checked with the state it
	// starts in, and its own Unlock does not leak into the code after it.
	var bad []string
	var walk func(stmts []ast.Stmt, held bool) bool
	callsIn := func(n ast.Node, held *bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Lock":
				*held = true
			case "Unlock":
				*held = false
			case "Kevent":
				if !*held {
					bad = append(bad, fset.Position(call.Pos()).String())
				}
			}
			return true
		})
	}
	walk = func(stmts []ast.Stmt, held bool) bool {
		for _, st := range stmts {
			if ifs, ok := st.(*ast.IfStmt); ok && ifs.Else == nil && len(ifs.Body.List) > 0 {
				if _, ret := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt); ret {
					callsIn(ifs.Cond, &held)
					walk(ifs.Body.List, held)
					continue
				}
			}
			callsIn(st, &held)
		}
		return held
	}
	walk(closeFn.Body.List, false)
	if len(bad) > 0 {
		t.Fatalf("Close calls kevent() without holding w.mu at %v; markDead can close the "+
			"kqueue in that window", bad)
	}
}
