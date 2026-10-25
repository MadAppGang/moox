//go:build linux

package sockserve

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat's starttime. It has been
// 100 on every Linux architecture Go supports for as long as /proc has existed,
// and reading it properly (sysconf(_SC_CLK_TCK)) would need cgo.
const clockTicks = 100

// processStartedAt is when the kernel says pid began: /proc/<pid>/stat's
// starttime (ticks since boot) added to /proc/stat's btime (boot, as wall-clock
// seconds). Two file reads and no child process, for the reason proc_darwin.go
// gives.
//
// btime is whole seconds, so the result can be up to a second early. That
// widens the gap --stop compares against stopStartSkew (a minute), never the
// backwards tolerance, so a healthy server is not refused over it.
//
// An unknown pid comes back as an error rather than as a zero instant, because
// "we could not tell" and "it started at the epoch" must not be the same value
// to the caller deciding whether to send a signal.
func processStartedAt(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: not a pid", pid)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: %w", pid, err)
	}
	// The command name (field 2) is in parentheses and may itself contain
	// spaces or parentheses, so fields are counted from the LAST ')'.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: malformed /proc stat", pid)
	}
	// After ")" come fields 3 onwards; starttime is field 22.
	fields := strings.Fields(string(stat[end+1:]))
	const starttimeIdx = 22 - 3
	if len(fields) <= starttimeIdx {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: short /proc stat", pid)
	}
	ticks, err := strconv.ParseUint(fields[starttimeIdx], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: %w", pid, err)
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the start time of pid %d: %w", pid, err)
	}
	sinceBoot := time.Duration(ticks) * (time.Second / clockTicks)
	return boot.Add(sinceBoot), nil
}

// bootTime is /proc/stat's btime.
func bootTime() (time.Time, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}
