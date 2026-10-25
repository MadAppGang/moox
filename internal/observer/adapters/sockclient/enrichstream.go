package sockclient

// enrichstream.go — ADDED by code review round 2 (A11), from the UI side, and
// deliberately a file of its own so it touches nothing in sockclient.go.
//
// §7.5(f) gives a connection two ways to get transcript facts: a one-off
// {"type":"snapshot","enrich":true} (Client.Enrich, which leaves the stream as
// it is), and a new `subscribe` carrying "enrich": true, which changes the
// connection's STANDING setting so every baseline and every delta the server
// sends from then on is enriched ("thereafter re-enriches a session whenever
// it … is newly added" — sockserve's conn.enriched). Until this file the client
// could only do the first, so an attached panel — which renders the agent
// colour, the subagent count and the API-error count on every row — was
// served enrich:false for its whole life and drew none of them.

import (
	"fmt"

	"github.com/MadAppGang/moox/internal/observer/wire"
)

// EnrichStream asks the server to enrich this connection's stream from now
// on. The answer is a fresh, enriched baseline, which arrives like any other
// snapshot frame: it replaces the local state, and every subscriber receives
// the sessions whose facts it filled in as session_changed deltas (or, if it
// subscribes afterwards, the enriched baseline itself).
//
// It sends no reqId on purpose. A reply that echoes a reqId is routed to its
// waiter and never applied to the local state (read -> deliverReply), and the
// point of this request is precisely to change that state for every
// subscriber. Nothing waits for it, so it arms no bound: a server that ignores
// it, or answers it with an error frame, leaves the stream unenriched — the
// status answer is complete without a colour, which is §7.5(f)'s own
// degradation rule.
//
// A server whose hello did not declare "enrich" is not asked (§7.5f: "treated
// as enrich:false … rather than guessing"), and that is not an error.
func (c *Client) EnrichStream() error {
	if !c.Capable(wire.CapEnrich) {
		return nil
	}
	c.mu.Lock()
	closed := c.closed
	// Code review round 2 (CORE, A10): a subscribe REPLACES the connection's
	// filter as well as its enrichment, so this frame carries the whole
	// standing subscription — the dead population the handshake asked for
	// included — rather than {"enrich": true} alone, which reset it.
	c.standing.enrich = true
	line := c.standing.line(nil)
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if _, err := c.conn.Write(line); err != nil {
		return fmt.Errorf("sockclient: sending an enriched subscribe: %w", err)
	}
	return nil
}
