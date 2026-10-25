package agentscli

// §4.3(b) — the versioned cache and the O_EXCL lock.
//
// This file is the whole of the cross-process bound, and it is worth being
// precise about which half does what, because revision 1 of the architecture
// claimed one of them did both and that sentence had to be withdrawn:
//
//	the CACHE bounds the STAGGERED fan-out. Processes starting seconds apart
//	  read a fresh answer and spawn nothing.
//	the LOCK  bounds the SIMULTANEOUS fan-out. Processes starting together all
//	  read the cold cache — atomic publication protects them from torn JSON and
//	  provides no mutual exclusion whatsoever — so exactly one of them must win
//	  a race that cannot be expressed as a read.
//
// O_CREAT|O_EXCL is that race, decided by the kernel in one syscall, with no
// advisory-locking semantics to get wrong and nothing to clean up on a crash
// except a file whose mtime says when it was abandoned.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"golang.org/x/sys/unix"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
	"github.com/MadAppGang/moox/internal/statedir"
)

// The two files, named once. They sit beside each other in the state directory
// so the rename that publishes the cache is same-directory and therefore atomic.
const (
	CacheFileName = "agents-cache.json"
	LockFileName  = "agents-cache.lock"
)

// The cache envelope's keys. M6: the file every tt process on the machine reads
// is VERSIONED, which §7.5(e) already required of the socket and revision 1 did
// not require of this. A long-lived process from an older build is the expected
// case — §10 installs only at P9 while the user runs tt daily.
const (
	keySchema     = "schema"
	keyTT         = "tt"
	keyObservedAt = "observedAt"
	keyPayload    = "payload"
	// keyClaudeRoot is what the listing DESCRIBES, and it is checked on read
	// exactly as the schema is.
	//
	// FOUND BY THE LIVE RUN, not by a fixture. `claude agents --json` reads
	// CLAUDE_CONFIG_DIR, so a listing taken with that variable pointing
	// somewhere else describes a DIFFERENT machine state — and a single shared
	// cache file would then serve it, confidently, to the next ordinary
	// `tt sessions` for up to staleAfterMs. The hermetic CLI test did precisely
	// that to the author's own cache: a one-entry fixture listing, published to
	// ~/.local/state/tt, and the live check read it back as a `hit` reporting
	// one session where the machine had thirty-nine.
	keyClaudeRoot = "claudeRoot"
)

// errLockHeld says another process holds the spawn right. It is not a failure:
// the caller's correct response is to wait briefly and then answer without
// corroboration.
var errLockHeld = errors.New("agentscli: cache lock held")

// errNoStateDir says there is nowhere to write. Every write path fails closed on
// it, which degrades to a normal spawn.
var errNoStateDir = errors.New("agentscli: no state directory")

// envelope is a decoded cache file. The payload is the vendor's bytes verbatim;
// nothing here re-encodes the listing.
type envelope struct {
	schema     int
	tt         string
	claudeRoot string
	observedAt time.Time
	payload    []byte
}

// readCache returns the cached envelope and whether it is USABLE.
//
// False covers all five ways the cache can fail to be an answer — absent,
// unreadable, not JSON, no timestamp, and a schema we do not speak — and they
// are deliberately not distinguished to the caller. §4.3(b): "A schema mismatch
// is treated EXACTLY as a parse failure", and every one of them means the same
// thing operationally: this process must take the lock and spawn.
func (s *Source) readCache() (envelope, bool) {
	if s.stateDir == "" {
		return envelope{}, false
	}
	// Read through the verified directory, never through a symlink at the
	// cache's name: a planted link would otherwise be a way to feed every tt
	// process on the machine a listing somebody else wrote.
	d, err := statedir.OpenExisting(s.stateDir, s.claudeRoot)
	if err != nil {
		return envelope{}, false
	}
	data, _, err := d.ReadFile(CacheFileName)
	_ = d.Close()
	if err != nil {
		return envelope{}, false
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return envelope{}, false
	}

	env := envelope{payload: raw[keyPayload]}
	if err := json.Unmarshal(raw[keySchema], &env.schema); err != nil {
		return envelope{}, false
	}
	if env.schema != wire.Schema {
		return envelope{}, false
	}
	_ = json.Unmarshal(raw[keyTT], &env.tt)

	// A cache describing another Claude tree is a MISS, not an answer. Same
	// treatment as a schema mismatch, for the same reason: it is well-formed
	// and about something else.
	var root string
	_ = json.Unmarshal(raw[keyClaudeRoot], &root)
	if root != s.claudeRoot {
		return envelope{}, false
	}

	var at string
	if err := json.Unmarshal(raw[keyObservedAt], &at); err != nil {
		return envelope{}, false
	}
	env.observedAt, err = observer.ParseTime(at)
	if err != nil || env.observedAt.IsZero() {
		return envelope{}, false
	}
	if len(env.payload) == 0 {
		return envelope{}, false
	}
	return env, true
}

// writeCache publishes a listing by temp-file + rename(2) in the same directory.
//
// Rename is atomic, so no reader ever sees a partial file. That is ALL it
// provides — the mutual exclusion is the lock's, and conflating the two is the
// error this whole mechanism exists to correct.
func (s *Source) writeCache(payload []byte, observedAt time.Time) error {
	d, err := s.openStateDir()
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	body, err := json.Marshal(map[string]json.RawMessage{
		keySchema:     json.RawMessage(fmt.Sprintf("%d", wire.Schema)),
		keyTT:         mustQuote(s.tt),
		keyClaudeRoot: mustQuote(s.claudeRoot),
		keyObservedAt: mustQuote(observer.FormatTime(observedAt)),
		keyPayload:    json.RawMessage(payload),
	})
	if err != nil {
		return fmt.Errorf("agentscli: encoding the cache envelope: %w", err)
	}

	// Temp file + renameat in the SAME verified directory: atomic, and a
	// failed publish leaves no temp file behind (statedir cleans it up). A
	// symlink at the cache's name is refused rather than replaced.
	if err := d.WriteFile(CacheFileName, body, 0o600); err != nil {
		return fmt.Errorf("agentscli: publishing the cache: %w", err)
	}
	return nil
}

// heldLock is the right to spawn. It exists as a type so release is one call
// with one meaning and so a caller cannot forget which of the two files to
// unlink.
//
// It holds the verified directory for as long as the lock is held, and the
// identity of the lock file it created: release removes THAT file and never a
// lock some other process created after this one was judged abandoned.
type heldLock struct {
	dir *statedir.Dir
	fd  int
	id  statedir.Info
}

// release closes the descriptor and UNLINKS the lock. Order matters only in
// that both must happen: a closed-but-present lock would block every other
// process until its mtime aged past the stale grace.
func (h heldLock) release() {
	if h.fd >= 0 {
		_ = unix.Close(h.fd)
	}
	if h.dir != nil {
		_ = h.dir.Remove(LockFileName, statedir.TypeRegular, &h.id)
		_ = h.dir.Close()
	}
}

// acquire takes the spawn right, atomically.
//
// A lock whose mtime is older than spawnTimeout + 1 s is ABANDONED: its holder
// cannot still be spawning, because its own deadline has passed. It is removed
// and the open retried exactly ONCE. Once, not in a loop: two processes both
// finding the same stale lock must not both conclude they may keep trying, and
// a single retry bounds the damage from a process killed mid-spawn to one cycle.
func (s *Source) acquire(now time.Time) (heldLock, error) {
	d, err := s.openStateDir()
	if err != nil {
		return heldLock{fd: -1}, err
	}

	fd, id, err := d.CreateExclusive(LockFileName, 0o600)
	if err == nil {
		return heldLock{dir: d, fd: fd, id: id}, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		_ = d.Close()
		return heldLock{fd: -1}, err
	}

	stale, ok := s.lockAbandoned(d, now)
	if !ok {
		_ = d.Close()
		return heldLock{fd: -1}, errLockHeld
	}
	if stale != nil {
		if err := d.Remove(LockFileName, stale.Type, stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = d.Close()
			return heldLock{fd: -1}, errLockHeld
		}
	}
	fd, id, err = d.CreateExclusive(LockFileName, 0o600)
	if err != nil {
		// Someone else won the retry. That is the correct outcome and not an
		// error: exactly one process holds the right.
		_ = d.Close()
		return heldLock{fd: -1}, errLockHeld
	}
	return heldLock{dir: d, fd: fd, id: id}, nil
}

// lockAbandoned reports whether a lock's holder can no longer be spawning, and
// which entry to remove if so (nil when it has already gone).
//
// Something that is not a regular file at the lock's name — a symlink, say —
// was not created by any tt, which creates the lock O_EXCL|O_NOFOLLOW; it is
// treated as abandoned and its ENTRY removed (unlinkat never follows it), so a
// planted link cannot switch corroboration off for the whole machine.
func (s *Source) lockAbandoned(d *statedir.Dir, now time.Time) (*statedir.Info, bool) {
	info, err := d.Lstat(LockFileName)
	if err != nil {
		// It vanished between the failed open and this stat, which means the
		// holder released it. Retrying is exactly right.
		return nil, true
	}
	if info.Type != statedir.TypeRegular {
		return &info, true
	}
	if now.Sub(info.ModTime) > s.spawnTimeout+staleLockGrace {
		return &info, true
	}
	return nil, false
}

// openStateDir opens (creating 0700 as needed) the state directory, and
// REFUSES one that resolves anywhere inside the Claude tree — through a
// symlinked ancestor as much as by spelling (code review round 2, A2).
//
// This is §9.4a's runtime half. The static audit is keyed on the package, which
// is what an AST pass can actually decide; it cannot evaluate a path built at
// runtime from an environment variable. So the containment the audit asserts
// structurally is asserted here as behaviour, and a test drives it with a state
// directory pointed deliberately inside a fake Claude root.
func (s *Source) openStateDir() (*statedir.Dir, error) {
	if s.stateDir == "" {
		return nil, errNoStateDir
	}
	d, err := statedir.Open(s.stateDir, s.claudeRoot)
	if errors.Is(err, statedir.ErrInsideClaudeTree) {
		return nil, fmt.Errorf("%w: %v (hard constraint 1)", errNoStateDir, err)
	}
	if err != nil {
		return nil, fmt.Errorf("agentscli: %w", err)
	}
	return d, nil
}

// mustQuote JSON-quotes a string. The inputs are a version string and a
// formatted timestamp, neither of which can fail to marshal, and an error
// return here would be a branch no test could reach.
func mustQuote(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}
