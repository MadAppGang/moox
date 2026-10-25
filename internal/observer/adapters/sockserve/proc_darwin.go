//go:build darwin

package sockserve

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// processStartedAt is when the kernel says pid began.
//
// sysctl rather than `ps -o lstart=` deliberately: --stop's whole job here is
// to avoid harming a process it did not mean to touch, and a corroboration
// that spawns a child to answer the question is a corroboration that costs a
// fork on a machine whose history is orphaned processes. This is one read-only
// syscall and no child at all.
//
// An unknown pid comes back as an error rather than as a zero instant, because
// "we could not tell" and "it started at the epoch" must not be the same value
// to the caller deciding whether to send a signal.
func processStartedAt(pid int) (time.Time, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: %w", pid, err)
	}
	tv := kp.Proc.P_starttime
	return time.Unix(tv.Sec, int64(tv.Usec)*1000), nil
}
