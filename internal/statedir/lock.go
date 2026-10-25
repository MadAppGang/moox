package statedir

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// ErrBusy means another holder has the lock. It is returned at once: Lock
// never waits (§5.6). A caller that must wait retries on its own schedule.
var ErrBusy = errors.New("statedir: lock is held")

// ErrLockFile means the operation would unlink, rename or replace a file this
// process uses as a lock. A lock file is created once and NEVER unlinked,
// renamed or replaced by moox (§5.6, R3-C3): deleting a lock to reclaim it
// needs a second lock around the reclaim, and the kernel lock is that lock.
var ErrLockFile = errors.New("statedir: refusing to unlink, rename or replace a lock file")

// Lock is one held flock(2) on a file in a verified directory.
//
// Why flock and nothing else (§5.6, Q11): rev 2 reclaimed a stale lock with
// lstat+unlink, which is two syscalls, and Q11 (a) replayed the window between
// them with real syscalls until two processes both held. With flock there is
// nothing to reclaim: the kernel drops the lock with the holder's last
// descriptor (a SIGKILLed holder's lock was free 3 ms later, Q11 b3), and
// descriptors are O_CLOEXEC, so a child the holder spawns never keeps it.
type Lock struct {
	dir  *Dir
	name string
	fd   int
	id   Info
	mu   *sync.Mutex

	once sync.Once
}

// lockRegistry holds, per lock file (canonical dir + name), the process-local
// mutex taken before flock — flock is per open file description, but Linux
// NFS emulates it with per-process locks, so two goroutines of one process
// would not exclude each other there (§5.6) — and remembers the name so the
// rest of this package refuses to unlink, rename or replace it.
var lockRegistry = struct {
	sync.Mutex
	m map[string]*sync.Mutex
}{m: map[string]*sync.Mutex{}}

func lockKey(d *Dir, name string) string { return filepath.Join(d.canonical, name) }

// refuseLocked is ErrLockFile for a name this process has ever locked.
func refuseLocked(d *Dir, name string) error {
	lockRegistry.Lock()
	_, ok := lockRegistry.m[lockKey(d, name)]
	lockRegistry.Unlock()
	if ok {
		return fmt.Errorf("%w: %s", ErrLockFile, d.Join(name))
	}
	return nil
}

func processMutex(d *Dir, name string) *sync.Mutex {
	lockRegistry.Lock()
	defer lockRegistry.Unlock()
	k := lockKey(d, name)
	mu, ok := lockRegistry.m[k]
	if !ok {
		mu = &sync.Mutex{}
		lockRegistry.m[k] = mu
	}
	return mu
}

// TryLock takes the exclusive lock on name, creating the file 0600 if absent,
// and writes record into it FOR DIAGNOSTICS ONLY (doctor, "busy <pid>"); no
// decision ever reads the record. It returns ErrBusy at once if anyone holds
// it, this process included.
//
// Sequence (§5.6): process mutex TryLock → open O_CREAT|O_NOFOLLOW|O_CLOEXEC
// (never O_EXCL, never unlinked) → flock(LOCK_EX|LOCK_NB) → the descriptor and
// the name must be the same inode (something outside moox may have replaced
// the file between open and lock; one retry, then ErrBusy) → ftruncate +
// write the record.
func (d *Dir) TryLock(name string, record []byte) (*Lock, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	mu := processMutex(d, name)
	if !mu.TryLock() {
		return nil, fmt.Errorf("%w: %s (this process)", ErrBusy, d.Join(name))
	}
	for attempt := 0; attempt < 2; attempt++ {
		l, retry, err := d.tryLockOnce(name, mu)
		if err == nil {
			if werr := l.writeRecord(record); werr != nil {
				l.Release()
				return nil, werr
			}
			return l, nil
		}
		if !retry {
			mu.Unlock()
			return nil, err
		}
	}
	mu.Unlock()
	return nil, fmt.Errorf("%w: %s was replaced while locking", ErrBusy, d.Join(name))
}

// tryLockOnce returns retry=true only for the replaced-file case.
func (d *Dir) tryLockOnce(name string, mu *sync.Mutex) (*Lock, bool, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, false, fmt.Errorf("%w: %s", ErrSymlink, d.Join(name))
		}
		return nil, false, &fs.PathError{Op: "open", Path: d.Join(name), Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, false, &fs.PathError{Op: "fstat", Path: d.Join(name), Err: err}
	}
	id := infoOf(&st)
	if id.Type != TypeRegular {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("%w: %s is a %s", ErrNotRegular, d.Join(name), id.Type)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, fmt.Errorf("%w: %s", ErrBusy, d.Join(name))
		}
		return nil, false, &fs.PathError{Op: "flock", Path: d.Join(name), Err: err}
	}
	l := &Lock{dir: d, name: name, fd: fd, id: id, mu: mu}
	if err := l.Check(); err != nil {
		_ = unix.Close(fd)
		return nil, true, err
	}
	return l, false, nil
}

func (l *Lock) writeRecord(record []byte) error {
	if err := unix.Ftruncate(l.fd, 0); err != nil {
		return &fs.PathError{Op: "truncate", Path: l.dir.Join(l.name), Err: err}
	}
	if _, err := unix.Seek(l.fd, 0, 0); err != nil {
		return &fs.PathError{Op: "seek", Path: l.dir.Join(l.name), Err: err}
	}
	if err := writeAll(l.fd, record); err != nil {
		return &fs.PathError{Op: "write", Path: l.dir.Join(l.name), Err: err}
	}
	return nil
}

// Check is the publish re-check (§5.6): the held descriptor and the name must
// still be the same inode. A mismatch — an external `rm save.lock`, a file
// moved over it — is ErrBusy, so even then two publishers cannot both proceed.
func (l *Lock) Check() error {
	var st unix.Stat_t
	if err := unix.Fstat(l.fd, &st); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBusy, l.dir.Join(l.name), err)
	}
	cur, err := l.dir.Lstat(l.name)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBusy, l.dir.Join(l.name), err)
	}
	if !infoOf(&st).SameFile(cur) || cur.Type != TypeRegular {
		return fmt.Errorf("%w: %s no longer names the locked file", ErrBusy, l.dir.Join(l.name))
	}
	return nil
}

// Release drops the lock by closing the descriptor. Idempotent; safe to defer
// on every path. The file stays.
func (l *Lock) Release() {
	l.once.Do(func() {
		_ = unix.Close(l.fd)
		l.mu.Unlock()
	})
}

// Info is the identity of the locked file.
func (l *Lock) Info() Info { return l.id }
