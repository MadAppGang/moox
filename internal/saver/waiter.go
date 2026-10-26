package saver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Why a watcher (and with it the daemon) ended.
var (
	// ErrServerGone: a waiter exited and the confirmation found no server.
	ErrServerGone = errors.New("saver: the tmux server is gone")
	// ErrGaveUp: WAITER-GAVE-UP, the respawn backoff ran out while the
	// server still answered.
	ErrGaveUp = errors.New("saver: WAITER-GAVE-UP")
)

// WaiterBackoff is the respawn backoff (§4.5 Lifetime, PR-M5): each delay is
// a one-shot timer armed by a failure; after len(WaiterBackoff) failed
// respawns in a row the watcher gives up.
var WaiterBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// WaiterHealthy is how long a waiter must stay blocked before its failure no
// longer counts as consecutive.
const WaiterHealthy = 30 * time.Second

// WaiterRearm is the pause between a waiter's zero exit and the next waiter.
const WaiterRearm = 100 * time.Millisecond

// Watcher is one blocked `wait-for <channel>` child, re-spawned for as long
// as its server lives (§5.2 `waiter`). A non-zero exit is only a SUSPICION:
// Gone confirms with a stat of the socket and one `display -p '#{pid}'`. Gone
// → ErrServerGone. Alive → respawn after a one-shot backoff (1, 2, 4, 8,
// 16 s), WAITER-RESPAWN; when the respawns run out, ErrGaveUp. A ZERO exit is
// a signal on the channel (or tmux flushing its waiters on the way down):
// Signal is called and the waiter re-armed after WaiterRearm.
type Watcher struct {
	Target  tmuxtarget.Target
	Channel string
	Logf    func(format string, a ...any)
	// Signal is called once per zero exit; nil ignores signals.
	Signal func()
	// Seq, when set, is an option every signaller increments before its
	// `wait-for -S` (SeqDirty). tmux keeps a signal for a later waiter only
	// while it is the only one: a second `-S` on a woken channel with no
	// waiter blocked deletes it (cmd-wait-for.c cmd_wait_for_signal →
	// cmd_wait_for_remove), so an even number of signals between two waiters
	// is lost without trace — the two hooks of one rename, the four of a
	// new-session. The waiter then runs `display -p #{Seq} ; wait-for ;
	// display -p #{Seq}` as ONE client, which prints the counter at the
	// instant it blocks and again as it wakes; a block value that differs
	// from the previous wake value (or, for the first waiter, any value at
	// all) is a signal that never arrived, and Signal is called for it.
	Seq string
	// Seams for tests; nil means the real ones.
	Wait     func(ctx context.Context) error
	Gone     func(ctx context.Context) bool
	NewTimer func(d time.Duration) Timer
}

// Run blocks until ctx ends (ctx.Err()), the server is gone (ErrServerGone)
// or the backoff ran out (ErrGaveUp).
func (w Watcher) Run(ctx context.Context) error {
	logf := w.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	wait, gone, newTimer := w.Wait, w.Gone, w.NewTimer
	logStart := true
	if wait == nil {
		lastWake, woke := "", false
		missed := func(atBlock string) {
			if (woke && atBlock != lastWake) || (!woke && atBlock != "") {
				if w.Signal != nil {
					w.Signal()
				}
			}
		}
		wait = func(ctx context.Context) error {
			atWake, err := w.waitFor(ctx, logStart, logf, missed)
			if err == nil && w.Seq != "" {
				lastWake, woke = atWake, true
			}
			return err
		}
	}
	if gone == nil {
		gone = func(ctx context.Context) bool { return ServerGone(ctx, w.Target) }
	}
	if newTimer == nil {
		newTimer = SystemClock{}.NewTimer
	}
	failures := 0
	for {
		started := time.Now()
		err := wait(ctx)
		logStart = false
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			failures = 0
			if w.Signal != nil {
				w.Signal()
			}
			// Re-arm after a short one-shot pause armed by this exit (tt
			// §9.3a): tmux flushes every waiter with exit 0 on its way down
			// and then waits for its clients to go, so an instant re-arm
			// would keep an exiting server alive. Nothing is lost: a signal
			// with no waiter blocked is kept for the next one (PV6).
			if !pauseFor(ctx, newTimer, WaiterRearm) {
				return ctx.Err()
			}
			continue
		}
		if gone(ctx) {
			logf("SERVER-GONE confirmed after the %s waiter exited: %v", w.Channel, err)
			return ErrServerGone
		}
		if time.Since(started) >= WaiterHealthy {
			failures = 0
		}
		if failures >= len(WaiterBackoff) {
			logf("WAITER-GAVE-UP channel=%s after %d consecutive failures; last: %v", w.Channel, failures+1, err)
			return ErrGaveUp
		}
		d := WaiterBackoff[failures]
		failures++
		logf("WAITER-RESPAWN channel=%s in %s (failure %d, server still answers): %v", w.Channel, d, failures, err)
		logStart = true
		// One-shot, armed by this failure, stopped if the daemon ends first.
		if !pauseFor(ctx, newTimer, d) {
			return ctx.Err()
		}
	}
}

func pauseFor(ctx context.Context, newTimer func(time.Duration) Timer, d time.Duration) bool {
	t := newTimer(d)
	select {
	case <-ctx.Done():
		t.Stop()
		return false
	case <-t.C():
		return true
	}
}

// waitFor runs one blocked `tmux -u -S <sock> wait-for <channel>` child. The
// first spawn and every respawn after a failure are logged (WAITER-START), a
// re-arm after a signal is not: one line per event would grow daemon.log
// with every layout change.
//
// With Seq set the child is `display -p #{Seq} ; wait-for <channel> ;
// display -p #{Seq}`: blocked is called with the first line while the child
// is still blocked, and the second line (the counter as it woke) is returned.
func (w Watcher) waitFor(ctx context.Context, logStart bool, logf func(string, ...any), blocked func(atBlock string)) (string, error) {
	if w.Seq == "" {
		cmd := w.Target.Command(ctx, "wait-for", w.Channel)
		if err := cmd.Start(); err != nil {
			return "", err
		}
		if logStart {
			logf("WAITER-START pid=%d channel=%s", cmd.Process.Pid, w.Channel)
		}
		return "", cmd.Wait()
	}
	seq := []string{"display-message", "-p", "#{" + w.Seq + "}"}
	cmd := w.Target.Command(ctx, tmuxtarget.Chain(seq, []string{"wait-for", w.Channel}, seq)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if logStart {
		logf("WAITER-START pid=%d channel=%s seq=%s", cmd.Process.Pid, w.Channel, w.Seq)
	}
	r := bufio.NewReader(out)
	atBlock, berr := r.ReadString('\n')
	if berr == nil {
		blocked(strings.TrimSpace(atBlock))
	}
	atWake, _ := r.ReadString('\n')
	_, _ = io.Copy(io.Discard, r)
	return strings.TrimSpace(atWake), cmd.Wait()
}

// ServerGone is the confirmation: the socket file is gone, or tmux itself
// says no server is running there. Any other failure is not proof.
func ServerGone(ctx context.Context, t tmuxtarget.Target) bool {
	if _, err := os.Stat(t.Socket); errors.Is(err, fs.ErrNotExist) {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := t.Run(cctx, "display-message", "-p", "#{pid}")
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{"no server running", "error connecting to", "server exited", "lost server"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
