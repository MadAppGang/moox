// Package sockclient is §7.5's via-socket mode: an observer.SessionSource
// backed by a running `tt observe` instead of by this process's own watch.
//
// # Why it is a SessionSource and nothing more
//
// §7.5(a): driving adapters hold a SessionSource and cannot tell which
// implementation they have. `mcpserve` and `panel.go` contain no
// `if attached { … } else { … }`, which is the property that makes the
// attached path the DEFAULT on this machine rather than an exotic one — when
// the user is running `tt observe`, every surface shares that one watch instead
// of arming 65 descriptors of its own.
//
// # It decodes nothing itself
//
// §7.5(b), and H7's rule: the local *observer.Snapshot is built by `wire`.
// `wire.DecodeSnapshot` turns the nested envelope into a struct (and names the
// fields this build does not know — P0 finding 1: that advisory can only be
// raised where BYTES become a struct, so `json.Unmarshal` + `ToDomain` would
// make `wire-unknown-field` unreachable in production), `wire.ToDomain` turns
// it into the domain, and `wire.ApplySessionFrame` folds every delta. A second
// decoder of the domain, in reverse, is precisely the drift the one-encoder
// rule exists to prevent — and it would be the load-bearing component of the
// very four-surface agreement it undermines.
//
// # What it refuses
//
// §7.5(e): a server whose schema or protocol is not this build's is NOT
// attached to. It is logged once, closed, and the caller falls back — a
// long-lived `tt observe` from an older build is the EXPECTED case, and
// silently re-serving an older schema would turn this package into a way to
// violate AC11 across processes.
//
// M11: the wait for the first snapshot frame is bounded at one second. An
// unbounded wait would hang an MCP `list_sessions` call or a TUI launch on a
// wedged server instead of degrading, which is the failure mode this whole
// design exists to avoid.
//
// # Timers, clocks and writes
//
// No timer: the only deadlines are socket read deadlines, armed while a read is
// in flight and cleared by its completion, plus the caller's own context. The
// clock is never read for a published value — every timestamp this package
// hands on came from the server's rebuild instant, which is the rule three live
// defects in P2, P3 and P4 came from breaking. Nothing here writes to the
// filesystem at all.
package sockclient
