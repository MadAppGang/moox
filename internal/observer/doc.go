// Package observer is the read-only core of tt's Claude Code session observer.
//
// It joins three sources of truth about Claude Code sessions — the session
// registry (~/.claude/sessions/*.json), the background job tree
// (~/.claude/jobs/<shortId>/state.json) and the session transcripts
// (~/.claude/projects/<slug>/*.jsonl) — corroborated by `claude agents --json`,
// and presents one domain vocabulary to four independent adapters (CLI JSON,
// MCP server, unix socket stream, TUI panel).
//
// # The boundary contract
//
// This package is the hexagon. It owns the domain types (session.go, status.go),
// the policy (liveness.go, and later join.go / refresh.go / diff.go), the
// advisory vocabulary (advisories.go) and the ports (ports.go). It imports
// nothing from adapters/ and nothing that touches the outside world: no
// syscall, no os/exec, no net. Every fact about the machine arrives through one
// of the seven driven ports in ports.go; every answer leaves through the one
// incoming port, SessionSource.
//
// A reviewer finding a syscall, unix, exec or net type in ports.go has found a
// real defect: a port must speak the core's vocabulary, never the adapter's.
//
// # Read-only, and why that is a hard constraint
//
// Nothing in this package or its adapters may create, truncate, move, lock or
// delete anything under ~/.claude/. Thirty-plus live Claude Code processes own
// that tree. Session files are opened for reading and nothing else. The only
// filesystem directory tt writes to is ~/.local/state/tt/ (the socket, the pid
// file, the corroboration cache and its lock).
//
// # Degrade silently on unknown data
//
// Claude's on-disk formats are undocumented and will change. The rule
// throughout is: prefer "no opinion" to a wrong answer, and keep the raw value.
// An unrecognised status maps to StatusUnknown with RawStatus carrying the
// literal; an unresolvable liveness question answers nil (tri-state), never
// false. Failures are values in the snapshot (SourceReport.LastError,
// Snapshot.Warnings), not errors that blank the response.
//
// # Implementation status
//
// This is phase P0 of ten: the domain, the ports, the advisory registry, the
// liveness policy, the wire codec and its inverse, and fakes for every driven
// port. The use cases (join, refresh, diff, the Observer type itself) and all
// adapters land in P1 through P9.
package observer
