package importer

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Record types resurrect writes, in file order (save.sh:242-245).
const (
	typeGrouped = "grouped_session"
	typePane    = "pane"
	typeWindow  = "window"
	typeState   = "state"
)

// Pane-row shapes (research/resurrect-behaviours.md §1).
const (
	shapeNormal    = "normal"    // 11 fields
	shapeRealigned = "realigned" // 12: continuity put back the empty title
	shapeCollapsed = "collapsed" // 11: empty title collapsed, never realigned
)

// tagRe is a continuity tail tag: ";KEY=value" (pre_save.sh:17).
var tagRe = regexp.MustCompile(`^;([A-Z][A-Z0-9_]*)=`)

type tag struct{ key, value string }

type paneRec struct {
	line          int
	session       string
	window, index int
	winFlags      string
	title, cwd    string
	active        bool
	cmd, full     string
	shape         string
	tags          []tag
}

type windowRec struct {
	line       int
	session    string
	index      int
	name       string
	active     bool
	flags      string
	layout     string
	autoRename string
}

type groupedRec struct {
	name, original string
	alt, active    int // -1: none
}

type stateRec struct{ session, last string }

type parsed struct {
	panes         []paneRec
	windows       []windowRec
	grouped       []groupedRec
	states        []stateRec
	rows          int
	continuations int
}

// isResurrect: the first non-empty line is a record type followed by a tab.
func isResurrect(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		t, _, ok := bytes.Cut(line, []byte("\t"))
		if !ok {
			return false
		}
		switch string(t) {
		case typeGrouped, typePane, typeWindow, typeState:
			return true
		}
		return false
	}
	return false
}

func parse(data []byte) parsed {
	var p parsed
	// afterPane: the last record parsed was a pane row, so a continuation
	// line belongs to it (I-2).
	afterPane := false
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		n := i + 1
		f := strings.Split(line, "\t")
		ok, pane := false, false
		switch f[0] {
		case typePane:
			var r paneRec
			if r, ok = parsePane(f); ok {
				r.line = n
				r.full = strings.TrimRight(r.full, " \t")
				p.panes = append(p.panes, r)
				pane = true
			}
		case typeWindow:
			var w windowRec
			if w, ok = parseWindow(f); ok {
				w.line = n
				p.windows = append(p.windows, w)
			}
		case typeState:
			if len(f) == 2 || len(f) == 3 {
				s := stateRec{session: f[1]}
				if len(f) == 3 {
					s.last = f[2]
				}
				p.states = append(p.states, s)
				ok = true
			}
		case typeGrouped:
			var g groupedRec
			if g, ok = parseGrouped(f); ok {
				p.grouped = append(p.grouped, g)
			}
		}
		// Any other first field — or a record type whose field shape fails
		// (RR15) — is a continuation line of the previous pane row's
		// multi-line full_command (resurrect lists every child, one per
		// physical line, and an argv holding a newline spans lines too). It
		// is counted, never judged a duplicate row (R23), and attached to
		// that pane's full_command verbatim after a "\n", in order (I-2,
		// research/resurrect-behaviours.md "Attach continuation lines to the
		// preceding pane record"). One that follows no pane row is counted
		// only. Restore still reads the FIRST line alone (relaunch.FirstLine,
		// PR-H14): the extra children never reach a rule match.
		switch {
		case ok:
			p.rows++
			afterPane = pane
		default:
			p.continuations++
			if afterPane {
				last := &p.panes[len(p.panes)-1]
				last.full += "\n" + line
			}
		}
	}
	return p
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func bit(s string) (bool, bool) {
	switch s {
	case "0":
		return false, true
	case "1":
		return true, true
	}
	return false, false
}

// sentinel strips resurrect's leading ':' (helpers.sh:58-60); ok is false
// when the field does not carry one.
func sentinel(s string) (string, bool) {
	if !strings.HasPrefix(s, ":") {
		return "", false
	}
	return s[1:], true
}

// parsePane reads one pane row in any of its three shapes. Fields 1–6 are
// the same in all of them: pane S W win_active :win_flags P.
func parsePane(f []string) (paneRec, bool) {
	// Tail tags: every trailing ";KEY=" field after the command.
	core := f
	var tags []tag
	for len(core) > 11 {
		last := core[len(core)-1]
		m := tagRe.FindStringSubmatch(last)
		if m == nil {
			break
		}
		tags = append([]tag{{key: m[1], value: last[len(m[0]):]}}, tags...)
		core = core[:len(core)-1]
	}
	if len(core) < 11 {
		return paneRec{}, false
	}
	var r paneRec
	var ok bool
	r.session = core[1]
	if r.window, ok = atoi(core[2]); !ok {
		return r, false
	}
	if _, ok = bit(core[3]); !ok {
		return r, false
	}
	if r.winFlags, ok = sentinel(core[4]); !ok {
		return r, false
	}
	if r.index, ok = atoi(core[5]); !ok {
		return r, false
	}
	r.tags = tags

	_, f8IsBit := bit(core[7])
	_, f11IsInt := atoi(core[10])
	switch {
	// Realigned (12 fields): field 7 empty and field 11 a bare integer (the
	// pid); cwd = f8, active = f9, cmd = f10, full = f12.
	case len(core) >= 12 && core[6] == "" && f11IsInt && strings.HasPrefix(core[7], ":") && strings.HasPrefix(core[11], ":"):
		r.shape = shapeRealigned
		r.cwd = core[7][1:]
		if r.active, ok = bit(core[8]); !ok {
			return r, false
		}
		r.cmd = core[9]
		r.full = strings.Join(core[11:], "\t")[1:]
	// Collapsed, never realigned (11 fields): the path shifted into field 7
	// and field 8 is pane_active. f10 is the pid and the full command was
	// computed from the wrong pid (f11 is ":"), so it is dropped.
	case len(core) == 11 && strings.HasPrefix(core[6], ":") && f8IsBit:
		r.shape = shapeCollapsed
		r.cwd = core[6][1:]
		r.active, _ = bit(core[7])
		r.cmd = core[8]
		if _, ok = atoi(core[9]); !ok {
			return r, false
		}
	// Normal (11 fields): pane S W wa :flags P title :path active cmd :full.
	// A full_command holding a TAB spills into further fields; they are
	// joined back.
	case strings.HasPrefix(core[7], ":") && strings.HasPrefix(core[10], ":"):
		r.shape = shapeNormal
		r.title = core[6]
		r.cwd = core[7][1:]
		if r.active, ok = bit(core[8]); !ok {
			return r, false
		}
		r.cmd = core[9]
		r.full = strings.Join(core[10:], "\t")[1:]
	default:
		return r, false
	}
	return r, true
}

// parseWindow: window S W :name active :flags layout auto_rename (8 fields).
func parseWindow(f []string) (windowRec, bool) {
	if len(f) != 8 {
		return windowRec{}, false
	}
	var w windowRec
	var ok bool
	w.session = f[1]
	if w.index, ok = atoi(f[2]); !ok {
		return w, false
	}
	if w.name, ok = sentinel(f[3]); !ok {
		return w, false
	}
	if w.active, ok = bit(f[4]); !ok {
		return w, false
	}
	if w.flags, ok = sentinel(f[5]); !ok {
		return w, false
	}
	w.layout = f[6]
	w.autoRename = f[7]
	return w, w.layout != ""
}

// parseGrouped: grouped_session name original :alt :active (save.sh:175).
func parseGrouped(f []string) (groupedRec, bool) {
	if len(f) != 5 {
		return groupedRec{}, false
	}
	g := groupedRec{name: f[1], original: f[2], alt: -1, active: -1}
	alt, ok1 := sentinel(f[3])
	act, ok2 := sentinel(f[4])
	if !ok1 || !ok2 || g.name == "" || g.original == "" {
		return g, false
	}
	if n, ok := atoi(alt); ok {
		g.alt = n
	}
	if n, ok := atoi(act); ok {
		g.active = n
	}
	return g, true
}

// unescapePath undoes resurrect's `\ ` (save.sh:198, R36/C4) and expands a
// leading "~" to home.
func unescapePath(p, home string) string {
	p = strings.ReplaceAll(p, `\ `, " ")
	if home != "" {
		if p == "~" {
			return home
		}
		if strings.HasPrefix(p, "~/") {
			return home + p[1:]
		}
	}
	return p
}

func (r paneRec) key() string { return fmt.Sprintf("%s:%d.%d", r.session, r.window, r.index) }
