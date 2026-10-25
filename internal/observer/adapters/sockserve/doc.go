// Package sockserve is S3: the unix-socket event stream behind `tt observe`.
//
// It is a DRIVING adapter. It holds narrow slices of the incoming port and
// nothing else — no filesystem source, no clock arithmetic, no encoder. Every
// byte it writes is produced by wire (§5.1), which is what makes AC11's
// four-surface agreement structural: the snapshot frame's payload IS the same
// wire.Snapshot value `tt sessions --json` prints, encoded by the same code,
// so "identical" is a property of the value rather than a comparison somebody
// maintains.
//
// # What runs, and what does not
//
// There is NO timer in this package. Not an idle timeout, not a keepalive, not
// a staleness sweep — the user's override is explicit ("that is one single
// server written in go with minimal resources. lets keep it run forever"), and
// AC9 therefore keeps its original "no timer, no poll loop" wording and is
// literally satisfiable. An idle server is three blocked syscalls: the watcher
// in kevent(), the accept loop in accept(), and one pair of goroutines per
// connection in read() and select. Nothing wakes any of them up.
//
// The only deadlines are per-write socket deadlines, armed while a write is in
// flight and cleared by its completion. They are not a schedule: nothing is
// armed while nothing is being written, which is the fourth clause of §9.3a's
// timer rule.
//
// # Backpressure, and the property that matters
//
// The rebuild loop is NEVER blocked by a client. Each connection subscribes
// through observer.WatchLoop.Subscribe, which owns the one bounded queue per
// subscriber and turns an overflow into a resync — so a client that stops
// reading costs itself its deltas and costs the machine one dropped queue,
// never a delayed rebuild. This package deliberately adds no second queue in
// front of that one: two bounded queues in series would be two places a frame
// can be dropped and two answers to "what did this client last see".
//
// # Who notices when it lingers
//
// A run-forever server needs to be discoverable, and three mechanisms make it
// so, all of them here: the socket at a documented path, a pid file carrying
// {pid, startedAt, socket, tt} written at bind and removed at exit, and
// `tt observe --stop` (Stop below) which signals the pid in that file. Nothing
// ever auto-starts it (§7.5) — which is the more important half, since every
// process-accumulation incident this repository has had was a process nobody
// remembered starting.
//
// # Where it may write
//
// tt's own state directory, injected as Config.Dir, and nowhere else. This is
// one of exactly two packages §9.4a's static audit permits to write at all,
// and the audit is keyed on the package because an AST pass cannot evaluate a
// path built at runtime. The runtime half is here: every write goes through
// paths derived from Dir, ensureDir REFUSES a Dir inside the Claude tree, and
// the refusal is a returned error rather than a panic — matching the shape
// P4's agentscli chose, so the two writing packages behave the same way when
// pointed somewhere they must not write.
package sockserve
