package saver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWatch drives Watcher.Run with scripted waiter outcomes, a scripted "is
// the server gone" answer, and timers that fire at once (recording the delay
// each was armed with).
type fakeWatch struct {
	waits   []error // one per waiter run; ctx is cancelled when they run out
	gone    []bool  // one per confirmation
	delays  []time.Duration
	rearms  int
	signals int
	logs    []string
}

type firedTimer struct{ c chan time.Time }

func (f firedTimer) C() <-chan time.Time { return f.c }
func (f firedTimer) Stop() bool          { return false }

func (f *fakeWatch) run(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := Watcher{
		Channel: ChanDirty,
		Logf:    func(format string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(format, a...)) },
		Signal:  func() { f.signals++ },
		Wait: func(context.Context) error {
			if len(f.waits) == 0 {
				cancel()
				return errors.New("cancelled")
			}
			err := f.waits[0]
			f.waits = f.waits[1:]
			return err
		},
		Gone: func(context.Context) bool {
			if len(f.gone) == 0 {
				return false
			}
			g := f.gone[0]
			f.gone = f.gone[1:]
			return g
		},
		NewTimer: func(d time.Duration) Timer {
			if d != WaiterRearm {
				f.delays = append(f.delays, d)
			} else {
				f.rearms++
			}
			c := make(chan time.Time, 1)
			c <- time.Time{}
			return firedTimer{c}
		},
	}
	return w.Run(ctx)
}

func (f *fakeWatch) logged(prefix string) int {
	n := 0
	for _, l := range f.logs {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func TestWatcher_ExitsOnlyWhenTheServerIsConfirmedGone(t *testing.T) {
	f := &fakeWatch{waits: []error{nil, nil, errors.New("exit status 1")}, gone: []bool{true}}
	if err := f.run(t); !errors.Is(err, ErrServerGone) {
		t.Fatalf("run = %v, want ErrServerGone", err)
	}
	if len(f.delays) != 0 || f.signals != 2 {
		t.Fatalf("delays %v signals %d", f.delays, f.signals)
	}
}

func TestWatcher_RespawnsWithBackoffWhileTheServerAnswers(t *testing.T) {
	boom := errors.New("signal: killed")
	f := &fakeWatch{waits: []error{boom, boom, nil, boom}}
	if err := f.run(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want context.Canceled (the daemon ended, not the server)", err)
	}
	// Two failures (1s, 2s), a healthy signal resets the count, one more (1s).
	want := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if fmt.Sprint(f.delays) != fmt.Sprint(want) {
		t.Fatalf("backoff = %v, want %v", f.delays, want)
	}
	if f.rearms != 1 || f.signals != 1 {
		t.Fatalf("%d re-arm pauses, %d signals; want 1 and 1", f.rearms, f.signals)
	}
	if n := f.logged("WAITER-RESPAWN"); n != 3 {
		t.Fatalf("%d WAITER-RESPAWN lines, want 3: %q", n, f.logs)
	}
}

func TestWatcher_GivesUpAfterTheBackoffRunsOut(t *testing.T) {
	boom := errors.New("exit status 1")
	var waits []error
	for i := 0; i < len(WaiterBackoff)+1; i++ {
		waits = append(waits, boom)
	}
	f := &fakeWatch{waits: waits}
	if err := f.run(t); !errors.Is(err, ErrGaveUp) {
		t.Fatalf("run = %v, want ErrGaveUp", err)
	}
	if fmt.Sprint(f.delays) != fmt.Sprint(WaiterBackoff) {
		t.Fatalf("backoff = %v, want %v", f.delays, WaiterBackoff)
	}
	if f.logged("WAITER-GAVE-UP") != 1 {
		t.Fatalf("no WAITER-GAVE-UP line: %q", f.logs)
	}
}

// TestRun_EventsReachTheLoopAndServerGoneEndsIt: Run wires the four
// channels' waiters to the loop (a dirty signal becomes a debounced save,
// a save signal an immediate one) and returns ErrServerGone when one waiter
// confirms the server gone.
func TestRun_EventsReachTheLoopAndServerGoneEndsIt(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	signals := map[string]func(){}
	gone := make(chan struct{})
	ready := make(chan struct{}, 4)
	cfg := h.m.c
	cfg.Clock = SystemClock{}
	cfg.Debounce = 20 * time.Millisecond
	cfg.NewWatcher = func(ch string, signal func()) func(context.Context) error {
		mu.Lock()
		signals[ch] = signal
		mu.Unlock()
		return func(ctx context.Context) error {
			ready <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-gone:
				if ch == ChanDirty {
					return ErrServerGone
				}
				<-ctx.Done()
				return ctx.Err()
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg) }()
	for i := 0; i < 4; i++ {
		<-ready
	}
	waitSaves := func(n int) {
		deadline := time.Now().Add(5 * time.Second)
		for h.eng.saveCount() < n && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	mu.Lock()
	signals[ChanSave]()
	mu.Unlock()
	waitSaves(1)
	mu.Lock()
	signals[ChanDirty]()
	mu.Unlock()
	waitSaves(2)
	if n := h.eng.saveCount(); n != 2 {
		t.Fatalf("%d saves, want the key save and the debounced one", n)
	}
	close(gone)
	select {
	case err := <-done:
		if !errors.Is(err, ErrServerGone) {
			t.Fatalf("Run = %v, want ErrServerGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the server went")
	}
}
