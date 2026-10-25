package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/statedir"
)

// Lock is a held save.lock (B22, §5.6): flock(2) LOCK_EX|LOCK_NB on a file
// that is created once and never unlinked, renamed or replaced. A second
// holder — another process, or another goroutine of this one — gets ErrBusy
// at once; the kernel releases the lock when the holder's descriptor closes
// or the holder dies, so there is nothing to reclaim (X7: a previous server
// life's lock cannot outlive its process).
type Lock struct{ l *statedir.Lock }

// Record is what the lock file holds FOR DIAGNOSTICS ONLY (doctor, the
// `busy <pid>` result); no decision reads it. The shape is cc_common.sh:307's.
type Record struct {
	Pid        int
	ServerPid  int
	Epoch      int64
	StartToken string
}

func (r Record) bytes() []byte {
	return []byte(fmt.Sprintf("%d\t%d\t%d\t%s\n", r.Pid, r.ServerPid, r.Epoch, r.StartToken))
}

// BusyError is ErrBusy with the holder's pid as its record names it (0 if
// unreadable).
type BusyError struct{ Pid int }

func (e *BusyError) Error() string {
	if e.Pid > 0 {
		return fmt.Sprintf("busy %d", e.Pid)
	}
	return "busy"
}

// Is makes errors.Is(err, ErrBusy) hold.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// TryLock takes save.lock or returns a *BusyError at once; it never waits.
func (s *Store) TryLock(rec Record) (*Lock, error) {
	l, err := s.state.TryLock(LockName, rec.bytes())
	if err == nil {
		return &Lock{l: l}, nil
	}
	if errors.Is(err, statedir.ErrBusy) {
		return nil, &BusyError{Pid: s.holderPid()}
	}
	return nil, err
}

func (s *Store) holderPid() int {
	b, _, err := s.state.ReadFile(LockName)
	if err != nil {
		return 0
	}
	f := strings.SplitN(string(b), "\t", 2)
	pid, _ := strconv.Atoi(strings.TrimSpace(f[0]))
	return pid
}

// LockRetry is the bounded wait's step (§5.4: the restore retries the same
// non-blocking call every 250 ms; it is not a blocking lock).
const LockRetry = 250 * time.Millisecond

// LockWait retries TryLock every LockRetry until ctx ends, then returns the
// last busy error. Each retry is one time.NewTimer armed by the busy result
// it follows and drained at once (tt §9.3a: one-shot, armed by an event,
// self-disarming, never armed while idle).
func (s *Store) LockWait(ctx context.Context, rec Record) (*Lock, error) {
	for {
		l, err := s.TryLock(rec)
		if err == nil || !errors.Is(err, ErrBusy) {
			return l, err
		}
		t := time.NewTimer(LockRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
	}
}

// Release drops the lock. Idempotent, safe to defer; the file stays.
func (l *Lock) Release() {
	if l != nil && l.l != nil {
		l.l.Release()
	}
}

// check is the publish re-check: the held descriptor and save.lock must still
// be the same inode.
func (l *Lock) check() error {
	if l == nil || l.l == nil {
		return errors.New("store: write without the save lock")
	}
	return l.l.Check()
}
