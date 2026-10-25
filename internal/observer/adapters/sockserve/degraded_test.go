package sockserve_test

// Code review round 2, A8: once the watch failed or died, the loop declared
// on-demand mode and waited; a socket `snapshot` request was still answered
// from the PUBLISHED snapshot unless nothing had ever been published. The
// "on-demand fallback" was a server serving the state of the moment its watch
// died, forever. Now, with the watch degraded, every pull is a serialized
// authoritative rebuild through the loop — and a watch that dies mid-run
// republishes at once, so the health report says on-demand rather than kqueue.

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

func waitingSession() observer.Session {
	return session("s1", func(s *observer.Session) {
		s.Status, s.RawStatus, s.Attention = observer.StatusWaiting, "waiting", true
		s.WaitReason = observer.WaitReasonInputNeeded
	})
}

func pull(t *testing.T, c *client, id string) wire.Frame {
	t.Helper()
	c.send(wire.ClientFrame{Type: wire.FrameSnapshot, ReqID: ptr(id)})
	for i := 0; i < 16; i++ {
		f, _ := c.until(wire.FrameSnapshot)
		if f.ReqID != nil && *f.ReqID == id {
			return f
		}
	}
	t.Fatalf("no reply to %s", id)
	return wire.Frame{}
}

func TestWithTheWatchUnavailableASnapshotRequestIsAFreshRead(t *testing.T) {
	h := newHarness(t, func(o *options) { o.watchErr = errNoKqueue{} })
	h.core.set(t0, session("s1", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.loop.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return h.loop.Latest() != nil })

	c := h.dial()
	c.handshake()

	// The registry changes. Nothing is watching it.
	h.core.set(t0.Add(time.Minute), waitingSession())

	f := pull(t, c, "after-change")
	if got := f.Snapshot.Sessions[0].Status; got != string(observer.StatusWaiting) {
		t.Fatalf("with no watch, a snapshot request answered status %q — the state from before "+
			"the change — instead of reading", got)
	}
}

func TestAWatchThatDiesMidRunRepublishesAsOnDemandAndPullsReadFresh(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.loop.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return h.loop.Latest() != nil && h.loop.Mode() == observer.WatchModeKqueue })

	c := h.dial()
	c.handshake()

	// The kernel side of the watch goes away (the fake hands back the same
	// closed channel, the real adapter's worst case). The death is published
	// at once — a republication whose watch report says on-demand (this
	// harness watches no path, so the report's mode is "absent" either way;
	// the republication itself is what the old loop never made).
	seqBefore := h.loop.Latest().Seq
	h.watcher.Close()
	deadline := time.Now().Add(readBound)
	for h.loop.Latest().Seq == seqBefore {
		if time.Now().After(deadline) {
			t.Fatalf("the watch died and nothing was republished: the published snapshot still "+
				"describes a pushing watch (seq %d)", seqBefore)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.loop.Mode() != observer.WatchModeOnDemand {
		t.Fatalf("mode %q after the watch died", h.loop.Mode())
	}

	h.core.set(t0.Add(time.Minute), waitingSession())
	f := pull(t, c, "after-death")
	if got := f.Snapshot.Sessions[0].Status; got != string(observer.StatusWaiting) {
		t.Fatalf("after the watch died a pull answered %q, the pre-death state", got)
	}
}

// Code review round 2, A12, end to end: a change to a source's health with no
// session change reaches an attached client as a `sources` frame. The server
// always had the handler; the loop never produced the event.
func TestASourceOnlyHealthChangeReachesAnAttachedClientAsASourcesFrame(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()
	c := h.dial()
	c.handshake()

	h.core.mu.Lock()
	reg := h.core.sources[observer.SourceRegistry]
	msg := "permission denied"
	reg.OK, reg.LastError = false, &msg
	h.core.sources[observer.SourceRegistry] = reg
	h.core.mu.Unlock()
	h.publish()

	f, skipped := c.until(wire.FrameSources)
	if len(skipped) != 0 {
		t.Fatalf("frames before the sources frame: %s", typesOf(skipped))
	}
	got := f.Sources[observer.SourceRegistry]
	if got.OK || got.LastError == nil || *got.LastError != msg || f.Cause != wire.CausePiggyback {
		t.Fatalf("sources frame %+v (cause %q), want the registry reported failing", got, f.Cause)
	}
}
