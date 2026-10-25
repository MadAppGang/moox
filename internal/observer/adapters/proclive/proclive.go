// Package proclive answers "is this process still there" with kill(pid, 0),
// which is the first of the two gates interactive liveness needs (measured fact
// 8: a live pid AND an existing pane — registry records are never deleted, and
// 32 of 63 were dead pids before a reboot).
//
// Signal 0 performs error checking only; no signal is sent. Nothing this
// package does can affect the target process, which is what makes it safe to
// point at 30+ live Claude processes.
//
// It also answers "who is this process's parent" (Parent), by the same rule:
// one read-only kernel query per pid, never a forked `ps`.
package proclive

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/MadAppGang/moox/internal/observer"
)

// kill is the syscall, as a package-level variable so tests substitute a
// deterministic fake instead of hunting for a pid that is reliably dead. This
// follows the existing seam pattern in tmux.go (listSessions, killSession,
// newSession, renameSession are package vars for exactly this reason).
var kill = syscall.Kill

// Source implements observer.ProcLive.
type Source struct{}

// New returns the production gate.
func New() Source { return Source{} }

// Alive reports whether pid names a live process.
//
// The three outcomes are deliberately distinguishable, because an errored gate
// is NOT evidence of death and must not be folded into a false:
//
//	(true,  nil)  the process exists — either we own it (no error) or we do not
//	              (EPERM, which still proves existence)
//	(false, nil)  ESRCH: there is no such process. Positive evidence of death.
//	(false, err)  the gate could not answer. The liveness policy keeps the
//	              previous value and attaches liveness-unknown rather than
//	              reporting a live session as dead.
func (Source) Alive(pid int) (bool, error) {
	// A non-positive pid is refused rather than passed to the kernel. kill(2)
	// reads pid 0 as "every process in the caller's process group" and negative
	// pids as process groups; with signal 0 that sends nothing, but asking the
	// question at all would return an answer about the wrong subject, and a
	// "yes" here would vouch for a session that has no process.
	if pid <= 0 {
		return false, fmt.Errorf("proclive: refusing to probe pid %d", pid)
	}

	err := kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		// The process exists and belongs to another user. Existence is what we
		// asked about, and EPERM answers it.
		return true, nil
	default:
		return false, fmt.Errorf("proclive: kill(%d, 0): %w", pid, err)
	}
}

// parentOf is the platform's parent-pid lookup (parent_darwin.go,
// parent_linux.go, parent_other.go), as a package variable for the same reason
// kill is one: a test substitutes it rather than depending on the shape of the
// real process tree.
var parentOf = processParent

// Parent returns pid's parent pid — the one edge of the process tree the core
// walks to find a session launched by another session (observer/spawned.go).
//
// One read-only kernel query and no child process, deliberately: `ps -o ppid=`
// would answer the same question with a fork per pid per rebuild, on a machine
// whose recorded history is orphaned processes spinning its cores. The rule
// this package already follows for kill(pid, 0) — nothing here may affect, or
// add to, the process population it is describing — covers this too.
//
// Every failure is an error rather than a guessed pid. A 0 or a 1 returned in
// place of "could not tell" would read as "this process's parent is launchd",
// which is an answer; the core treats an error as no evidence, which is what
// it is.
func (Source) Parent(pid int) (int, error) {
	// Refused for the reason Alive refuses it: pid 0 and negative pids are not
	// processes, and an answer about them would describe the wrong subject.
	if pid <= 0 {
		return 0, fmt.Errorf("proclive: refusing to look up the parent of pid %d", pid)
	}
	ppid, err := parentOf(pid)
	if err != nil {
		return 0, fmt.Errorf("proclive: parent of pid %d: %w", pid, err)
	}
	return ppid, nil
}

var _ observer.ProcLive = Source{}
