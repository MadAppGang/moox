package observer

// This file decides Session.SpawnedBy: which sessions were launched BY another
// session rather than by a person.
//
// WHY THE PROCESS TREE. Measured 2026-10-21: a Claude session in tmux pane %24
// (claude 27603) ran the claudish MCP helper, which ran `magmux --headless`,
// which ran a second claude (45393) on its own pty. That child writes an
// ordinary ~/.claude/sessions/<pid>.json (kind interactive, entrypoint cli) and
// `claude agents --json` lists it, so no field in either source separates it
// from a session a person started — the panel drew it as a RUN row under
// "no tmux". The ancestry is the one fact that does separate it, and it is
// generic: claudish, terminal-mux isolated slots and anything else a session
// spawns all leave the launching session's pid among the child's ancestors.
//
// THE RULE IS POSITIVE EVIDENCE ONLY, for the same reason the liveness rule is
// live != false rather than live == true: hiding a session is the consequential
// direction, and a session that vanishes because a lookup failed is a session a
// person can no longer find. So:
//
//   - a walk that errors anywhere answers "not spawned", never "spawned";
//   - only a LIVE session's pid counts as a launcher. A dead record's pid is a
//     number the kernel may since have handed to anything — a shell, the tmux
//     server — and matching it would hide every session under that process;
//   - a session is never its own launcher, even through a looping tree;
//   - a pid two sessions both claim names neither, because it cannot say which.
//
// The walk is bounded (maxAncestryHops) and memoised per rebuild: sessions
// share most of their ancestry (the tmux server, launchd), so one rebuild asks
// the kernel about each pid at most once. Nothing is remembered ACROSS
// rebuilds, deliberately — a process is reparented when its parent dies, and
// that reparenting is exactly the change that should make an orphaned child
// visible again.

// maxAncestryHops bounds one session's walk up the tree. The measured chain
// from a claudish child to its launcher is six hops; a tmux pane's process is
// four or five below launchd. 32 is far past any real depth and still cheap
// enough to be a hard stop for a torn or looping read.
const maxAncestryHops = 32

// parentAnswer is one memoised Parent lookup.
type parentAnswer struct {
	ppid int
	err  error
}

// markSpawned sets SpawnedBy on every session whose process descends from
// another live session's process, and clears it on every other session.
//
// It writes every session's SpawnedBy, including to nil, so the field is a
// function of THIS rebuild's tree alone and never a value carried over from a
// previous one by the join.
func markSpawned(sessions []Session, parent func(pid int) (int, error)) {
	for i := range sessions {
		sessions[i].SpawnedBy = nil
	}
	if parent == nil {
		return
	}

	launchers, ambiguous := launcherPIDs(sessions)
	if len(launchers) == 0 {
		return
	}

	memo := map[int]parentAnswer{}
	lookup := func(pid int) (int, error) {
		if a, ok := memo[pid]; ok {
			return a.ppid, a.err
		}
		ppid, err := parent(pid)
		memo[pid] = parentAnswer{ppid: ppid, err: err}
		return ppid, err
	}

	for i := range sessions {
		s := &sessions[i]
		// Only a process kill -0 has just vouched for is walked. A dead pid's
		// lookup would error anyway; skipping it saves the syscall, and a
		// gate that could not answer is no basis for a walk either.
		if s.PID == nil || s.ProcAlive == nil || !*s.ProcAlive {
			continue
		}
		if id, ok := launcherOf(*s.PID, s.SessionID, launchers, ambiguous, lookup); ok {
			launcher := id
			s.SpawnedBy = &launcher
		}
	}
}

// launcherPIDs indexes the sessions that may count as a launcher by pid: a pid,
// a process kill -0 vouched for, and liveness that is not positively false.
//
// live != false rather than live == true, so a tmux gate error (live: null) on
// the launching session does not unhide its children for a rebuild — the pid
// was still vouched for. A live: false session with a live pid is a record
// whose pane is gone, which is the recycled-pid case and is excluded.
//
// A pid claimed by two sessions goes into ambiguous instead: either could be
// the launcher, and a guess is not positive evidence.
func launcherPIDs(sessions []Session) (map[int]string, map[int]bool) {
	launchers := map[int]string{}
	ambiguous := map[int]bool{}
	for _, s := range sessions {
		if s.PID == nil || s.ProcAlive == nil || !*s.ProcAlive {
			continue
		}
		if s.Live != nil && !*s.Live {
			continue
		}
		pid := *s.PID
		if prev, ok := launchers[pid]; ok && prev != s.SessionID {
			ambiguous[pid] = true
			continue
		}
		launchers[pid] = s.SessionID
	}
	for pid := range ambiguous {
		delete(launchers, pid)
	}
	return launchers, ambiguous
}

// launcherOf walks up from pid and returns the nearest ancestor that is
// another session's process.
//
// The walk starts at pid's PARENT, so the session's own pid is never compared
// against the index; a tree that loops back to it is caught by the self check
// and, failing that, by the hop bound. It stops — answering "not spawned" — at
// the first error, at pid 1 or below (launchd/init, or a lookup that returned
// nonsense), at an ambiguous pid, and after maxAncestryHops.
func launcherOf(pid int, self string, launchers map[int]string, ambiguous map[int]bool,
	lookup func(int) (int, error)) (string, bool) {
	cur := pid
	for hop := 0; hop < maxAncestryHops; hop++ {
		ppid, err := lookup(cur)
		if err != nil || ppid <= 1 || ppid == cur {
			return "", false
		}
		if ambiguous[ppid] {
			return "", false
		}
		if id, ok := launchers[ppid]; ok && id != self {
			return id, true
		}
		cur = ppid
	}
	return "", false
}
