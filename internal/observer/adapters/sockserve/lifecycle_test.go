package sockserve_test

// Code review round 2, A6: a connection's reader returned on EOF without
// telling the serving loop, which stayed subscribed and blocked in its select.
// Nothing was ever written to an idle machine's connections, so the write
// deadline could not notice either — every closed client kept its admission
// slot, its subscription and two goroutines until some later filesystem event
// made a write fail. 128 open/close cycles (an MCP client started and stopped,
// observer panes opened and closed) locked every new client out.

import (
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

func waitUntil(t *testing.T, within time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf(msg, args...)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClosedClientsReleaseTheirSlotsWithNoFurtherEvent(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	cycles := sockserve.DefaultMaxClients + 16
	for i := 0; i < cycles; i++ {
		c := h.dial()
		hello := c.frame()
		if hello.Type != wire.FrameHello {
			t.Fatalf("cycle %d: the server answered %s (%s) instead of hello; %d closed clients "+
				"still hold slots", i, hello.Type, hello.Detail, h.srv.Clients())
		}
		c.expect(wire.FrameSnapshot)
		_ = c.nc.Close()
	}

	// No filesystem event, no rebuild, nothing written: only the closes.
	waitUntil(t, 2*time.Second, func() bool { return h.srv.Clients() == 0 },
		"%d connections are still counted after every client closed", h.srv.Clients())
	waitUntil(t, 2*time.Second, func() bool { return h.loop.Subscribers() == 0 },
		"%d subscriptions outlived their clients", h.loop.Subscribers())
}

// Closing only the WRITE half is a defined lifecycle, not a disconnect: a
// client that sends its request and shuts down its write side (what `nc -U`
// does at the end of its input) is still answered, and the server then closes
// the connection and releases it.
func TestAHalfClosedClientIsAnsweredAndThenReleased(t *testing.T) {
	h := newHarness(t, nil)
	h.core.set(t0, session("s1", nil))
	h.publish()

	c := h.dial()
	c.handshake()
	c.send(wire.ClientFrame{Type: wire.FrameSnapshot, ReqID: ptr("half")})
	type closeWriter interface{ CloseWrite() error }
	if err := c.nc.(closeWriter).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	f, _ := c.until(wire.FrameSnapshot)
	if f.ReqID == nil || *f.ReqID != "half" {
		t.Fatalf("the reply echoes %v, want reqId half", f.ReqID)
	}
	waitUntil(t, 2*time.Second, func() bool { return h.srv.Clients() == 0 },
		"a half-closed client that got its answer is still counted")
}
