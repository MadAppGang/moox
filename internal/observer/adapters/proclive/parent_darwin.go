//go:build darwin

package proclive

import "golang.org/x/sys/unix"

// processParent reads pid's parent from the kernel's process table:
// sysctl kern.proc.pid.<pid> returns the kinfo_proc, whose eproc carries the
// ppid. It is the same read-only call sockserve's proc_darwin.go uses for a
// start time, and it costs one syscall and no child.
//
// A pid with no process behind it comes back from x/sys as an error (the
// kernel returns no record, which SysctlKinfoProc reports as a short read), so
// "gone" is never mistaken for "parent 0".
func processParent(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Ppid), nil
}
