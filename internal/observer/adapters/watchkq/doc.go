// Package watchkq is the filesystem watch adapter: a kqueue implementation of
// observer.Watcher on darwin, an inotify one on linux (watch_linux.go, which
// says where the two differ), and a nowatch implementation everywhere else.
//
// It also carries the ENUMERATION half of the watch protocol (OSTree, an
// observer.PathLister), because deciding what to arm requires knowing which
// directories and files exist, and that read belongs on the same side of the
// boundary as the descriptors it feeds.
//
// # What this package is not allowed to do, and how that is enforced
//
// It never writes. Every open is O_RDONLY|O_EVTONLY|O_CLOEXEC — the flag set
// §9.4a's static audit recognises as provably read-only, so "this adapter
// cannot modify ~/.claude/" is a property an AST pass checks rather than a rule
// a reviewer has to remember. O_EVTONLY is darwin's "descriptor is for events
// only": it does not update atime and does not prevent an unmount. On linux
// inotify opens nothing per target: inotify_add_watch takes a path and an
// event mask, and the only descriptor is the inotify instance itself.
//
// It never starts a process. No exec, no tmux, no shell.
//
// # The two measured facts the implementation is shaped by (§0.5)
//
// E1: the kernel QUEUES every post-registration vnode event, even before the
// first kevent() call. That is what makes arm-before-read sufficient: once a
// target is armed the watcher may go away and scan for 40 ms and lose nothing.
//
// E5: close(fd) DISCARDS every event already queued on that descriptor. So
// Reconcile leaving an unchanged target alone is a CORRECTNESS requirement, not
// an optimisation — a gratuitous close-then-open silently drops exactly the
// notifications the watch exists for. Reconcile therefore closes a descriptor
// in only two cases: the target left the desired set, or the path's identity
// changed underneath it (dev+ino compared through fstat, which is what a
// rename-publish of a job record looks like from here). Re-passing an
// already-armed target is a no-op, which E6 measured is safe.
//
// # Coalescing and backpressure
//
// The kevent loop must never block, because blocking it delays every other
// target's event. Hints go out on a buffered channel with a non-blocking send;
// when the buffer is full the hint is COUNTED and dropped, which is sound
// precisely because a hint says only THAT something changed: while the channel
// is non-empty a rebuild is already coming, and the rebuild reads the whole
// tree.
package watchkq
