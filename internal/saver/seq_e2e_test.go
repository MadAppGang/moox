//go:build tmuxe2e

package saver

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// counted is what one moox-dirty hook does (active.conf).
var counted = []string{"set-option", "-gF", SeqDirty, "#{e|+:#{" + SeqDirty + "},1}"}

func signalDirty(t *testing.T, s *tmuxtest.Server, n int) {
	t.Helper()
	var cmds [][]string
	for i := 0; i < n; i++ {
		cmds = append(cmds, counted, []string{"wait-for", "-S", ChanDirty})
	}
	s.Run(t, tmuxtarget.Chain(cmds...)...)
}

func expectSignal(sig <-chan struct{}, within time.Duration) bool {
	select {
	case <-sig:
		return true
	case <-time.After(within):
		return false
	}
}

func drain(sig <-chan struct{}, quiet time.Duration) {
	for expectSignal(sig, quiet) {
	}
}

// TestE2E_Waiter_SeqCatchesCancelledSignals (CI run 40244726496, the
// black-box ceiling test): two hook signals that land while no waiter is
// blocked cancel out in tmux — the second `wait-for -S` deletes the woken
// channel — so the trailing rename after a max-wait save was never saved.
// With Seq the waiter sees the counter move between its last wake and its
// next block and signals anyway. The control without Seq shows the loss is
// real on this tmux.
func TestE2E_Waiter_SeqCatchesCancelledSignals(t *testing.T) {
	for _, c := range []struct {
		name string
		seq  string
		want bool
	}{{"seq", SeqDirty, true}, {"control-no-seq", "", false}} {
		t.Run(c.name, func(t *testing.T) {
			s := tmuxtest.Start(t)
			sig := make(chan struct{}, 16)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			w := Watcher{Target: s.T, Channel: ChanDirty, Seq: c.seq, Signal: func() { sig <- struct{}{} }}
			go func() { done <- w.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			// Let the waiter block, then one signal wakes it.
			time.Sleep(300 * time.Millisecond)
			signalDirty(t, s, 1)
			if !expectSignal(sig, 3*time.Second) {
				t.Fatal("a single signal to a blocked waiter was not delivered")
			}
			drain(sig, 50*time.Millisecond)
			// Within the waiter's re-arm pause (WaiterRearm, 100 ms) no waiter
			// is blocked: two signals now cancel out in tmux.
			signalDirty(t, s, 2)
			if got := expectSignal(sig, 2*time.Second); got != c.want {
				t.Fatalf("two signals while no waiter was blocked: delivered=%v, want %v", got, c.want)
			}
			if c.want {
				// And nothing more: an idle server gets no further signal.
				if expectSignal(sig, 600*time.Millisecond) {
					drain(sig, 300*time.Millisecond)
					if expectSignal(sig, 600*time.Millisecond) {
						t.Fatal("the waiter keeps signalling on an idle server")
					}
				}
			}
		})
	}
}
