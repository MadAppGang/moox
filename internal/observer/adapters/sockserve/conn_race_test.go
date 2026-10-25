package sockserve

// Code review MEDIUM 6: a data race on conn.cancel.
//
// `serve` used to write c.cancel on the connection goroutine, while `stop` —
// reached from `stopConns` on the Serve goroutine — read it, and nothing
// ordered the two: `admit` publishes the conn into s.conns under s.mu BEFORE
// the serve goroutine runs its first statement. A shutdown that began in that
// window read c.cancel concurrently with its write. The practical outcome was
// benign (stop read nil, and serve still unwound because its parent context
// was cancelled too), but Go defines it as undefined behaviour, and `-race`
// only passed because nothing scheduled that interleaving.
//
// This test schedules the interleaving on purpose: each iteration admits a
// connection, lets serve() run its first statements, and only then calls
// stopConns() — with NOTHING synchronising the test goroutine with the
// connection goroutine in between (a sleep is not a happens-before edge). The
// pause is what makes it deterministic: the first draft cancelled the parent
// context from the test goroutine instead, and the closed Done channel that
// serve's context.WithCancel then observed WAS a happens-before edge, which hid
// the race from the detector. Measured: that draft passed -race on the old
// code; this one fails it (see the implementation log). It is internal because
// admit/serve/stopConns are the three parties to the race.

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// raceEvents is the smallest Subscriber: a stream that ends with its context.
type raceEvents struct{}

func (raceEvents) Subscribe(ctx context.Context, _ observer.Filter) (<-chan observer.Event, error) {
	ch := make(chan observer.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (raceEvents) Latest() *observer.Snapshot { return nil }

// raceCore is never asked anything: the test sends no request.
type raceCore struct{}

func (raceCore) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	return nil, errors.New("raceCore: not used")
}

func TestStoppingAConnectionThatHasNotStartedServingIsNotADataRace(t *testing.T) {
	srv, err := New(Config{
		Dir:          t.TempDir(),
		Deps:         Deps{Events: raceEvents{}, Core: raceCore{}},
		WriteTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 20; i++ {
		server, client := net.Pipe()
		go func() { _, _ = io.Copy(io.Discard, client) }()

		parent, cancelParent := context.WithCancel(context.Background())
		c, ok := srv.admit(parent, server)
		if !ok {
			t.Fatalf("iteration %d: admit refused a connection under the cap", i)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			c.serve()
			srv.forget(c.id)
		}()
		// Let serve() get past its first statements, then take the shutdown
		// path. time.Sleep orders nothing, so on the old code serve's write of
		// c.cancel and stop's read of it are unordered, which is the race.
		time.Sleep(5 * time.Millisecond)
		srv.stopConns()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancelParent()
			t.Fatalf("iteration %d: a stopped connection did not unwind", i)
		}
		cancelParent()
		_ = client.Close()
	}
}
