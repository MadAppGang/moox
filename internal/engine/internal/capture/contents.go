package capture

import (
	"strconv"
	"strings"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// MaxChainBytes bounds one contents chain's argv. P1 measured (PV-ARGV) that
// one tmux exec carries about 16 KiB of argv; a chain stays well under it, so
// a large workspace takes ceil(panes/~200) execs instead of failing.
const MaxChainBytes = 12 << 10

// CaptureLines turns @moox-capture-lines into capture-pane's -S value: full
// history ("-") by default, which is resurrect's
// @resurrect-pane-contents-area full that this Mac runs; a positive integer
// is an opt-in cap.
func CaptureLines(opt string) string {
	opt = strings.TrimSpace(opt)
	if n, err := strconv.Atoi(opt); err == nil && n > 0 {
		return "-" + strconv.Itoa(n)
	}
	return "-"
}

// WantsContents is @moox-capture-contents (default on).
func WantsContents(opt string) bool { return strings.TrimSpace(opt) != "off" }

// ContentsPanes is every pane whose contents are captured: all but
// tt-observer and frozen rows, whose banner is the restored text (PR-C10).
func ContentsPanes(s *snapshot.Snapshot) []string {
	var ids []string
	for _, p := range s.Panes {
		if p.Role == snapshot.RoleTTObserver || p.Role == snapshot.RoleFrozen {
			continue
		}
		ids = append(ids, p.SavedID)
	}
	return ids
}

// marker is printed by `display-message -p -t <id>` through #{pane_id}: a
// literal pane id in a display-message template is mangled, because tmux runs
// the template through strftime first ("%0b" printed "Oct" on 3.8).
func marker(nonce string) string { return nonce + "P #{pane_id}" }

// Chain is one contents exec: its argv and the pane ids it captures, in
// order.
type Chain struct {
	Args []string
	IDs  []string
}

// ContentsChains is the argv of each contents exec:
//
//	display -p -t %id '<nonce>P #{pane_id}' ; capture-pane -epJ -t %id -S - ; …
//
// split on the nonce lines afterwards (one fork for all panes, where
// resurrect forks once per pane), chunked under MaxChainBytes. A pane that is
// gone fails its own display first, so the chain stops BEFORE its marker.
func ContentsChains(nonce string, ids []string, lines string) []Chain {
	var out []Chain
	var cur Chain
	size := 0
	for _, id := range ids {
		cmd := [][]string{
			{"display-message", "-p", "-t", id, marker(nonce)},
			{"capture-pane", "-epJ", "-t", id, "-S", lines},
		}
		n := 0
		for _, c := range cmd {
			for _, a := range c {
				n += len(a) + 1
			}
			n += 2
		}
		if len(cur.IDs) > 0 && size+n > MaxChainBytes {
			out = append(out, cur)
			cur, size = Chain{}, 0
		}
		for _, c := range cmd {
			if len(cur.Args) > 0 {
				cur.Args = append(cur.Args, ";")
			}
			cur.Args = append(cur.Args, c...)
		}
		cur.IDs = append(cur.IDs, id)
		size += n
	}
	if len(cur.IDs) > 0 {
		out = append(out, cur)
	}
	return out
}

// ParseContents splits a contents exec's output on its marker lines. Each
// entry has its trailing blank lines stripped and ends in one "\n" (empty if
// the pane printed nothing). last is the id of the last marker seen: if the
// chain stopped early (a pane died between the read and the capture, so its
// display failed and tmux skipped the rest, E5), the pane AFTER last is the
// one that is gone and the caller retries the panes after that.
func ParseContents(out []byte, nonce string) (entries map[string][]byte, last string) {
	entries = map[string][]byte{}
	pre := nonce + "P "
	var curID string
	var buf strings.Builder
	flush := func() {
		if curID == "" {
			return
		}
		text := strings.TrimRight(buf.String(), " \t\r\n")
		if text != "" {
			text += "\n"
		}
		entries[curID] = []byte(text)
		buf.Reset()
	}
	for _, line := range strings.SplitAfter(string(out), "\n") {
		trimmed := strings.TrimRight(line, "\n")
		if id, ok := strings.CutPrefix(trimmed, pre); ok && !strings.Contains(id, " ") && strings.HasPrefix(id, "%") {
			flush()
			curID, last = id, id
			continue
		}
		if curID != "" {
			buf.WriteString(line)
		}
	}
	flush()
	return entries, last
}
