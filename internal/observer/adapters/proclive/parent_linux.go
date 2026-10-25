//go:build linux

package proclive

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processParent reads pid's parent from /proc/<pid>/stat: field 4, ppid. One
// file read and no child process, for the reason Parent gives.
//
// The command name (field 2) is in parentheses and may itself contain spaces
// or parentheses — a process can name itself "a) 1 2 (" — so fields are
// counted from the LAST ')', exactly as sockserve's proc_linux.go does for the
// start time. Counting from the first space would hand back a field of the
// name as the ppid.
func processParent(pid int) (int, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, errors.New("malformed /proc stat")
	}
	// After ")" come field 3 (state) and field 4 (ppid).
	fields := strings.Fields(string(stat[end+1:]))
	const ppidIdx = 4 - 3
	if len(fields) <= ppidIdx {
		return 0, errors.New("short /proc stat")
	}
	ppid, err := strconv.Atoi(fields[ppidIdx])
	if err != nil {
		return 0, fmt.Errorf("ppid field: %w", err)
	}
	return ppid, nil
}
