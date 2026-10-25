// Package capture turns live server state into a snapshot (architecture §6.2
// steps 2–8). It is pure: it builds the argv of the ONE chained tmux read and
// of the contents chain, and parses what tmux and ps printed. The engine runs
// the execs and the file reads, so every rule here is testable with plain
// strings, and the exec budget (B25: 3–5 forks per save, independent of pane
// count) is visible in one place.
package capture

import (
	"strconv"
	"strings"
)

// Sep separates fields in every format the read uses. tmux keeps it intact
// with -u (PV11), and nothing a user types into a name normally holds it; a
// row whose field count is wrong is re-read alone (RR1, R13).
const Sep = "\x1f"

// Record types, one byte after the nonce.
const (
	recServer  = 'S'
	recClient  = 'C'
	recSession = 's'
	recWindow  = 'w'
	recPane    = 'p'
	recGlobal  = 'G'
)

// The field lists. The first field of every row is its id, so a row whose
// count is wrong can still be re-read by id.
var (
	serverFields = []string{"#{pid}", "#{start_time}", "#{version}", "#{session_name}",
		"#{@moox-restoring}", "#{@moox-active}", "#{@moox-want-contents}", "#{@claude-continuity-by-pid-dir}",
		"#{@moox-capture-contents}", "#{@moox-capture-lines}", "#{@moox-retention}", "#{@moox_save_seq}"}
	clientFields  = []string{"#{client_session}", "#{client_last_session}", "#{client_activity}"}
	sessionFields = []string{"#{session_id}", "#{session_name}", "#{session_grouped}", "#{session_group}"}
	windowFields  = []string{"#{window_id}", "#{session_name}", "#{window_index}", "#{window_name}",
		"#{window_active}", "#{window_last_flag}", "#{window_zoomed_flag}", "#{window_layout}",
		"#{automatic-rename}", "#{window_width}", "#{window_height}"}
	paneFields = []string{"#{pane_id}", "#{session_name}", "#{window_index}", "#{window_id}", "#{pane_index}",
		"#{pane_title}", "#{pane_current_path}", "#{pane_active}", "#{pane_current_command}", "#{pane_pid}",
		"#{pane_dead}", "#{history_size}", "#{cursor_y}", "#{alternate_on}", "#{@tt_obs}",
		"#{pane_start_command}", "#{@mcp_owner}", "#{@mcp_pane}", "#{@cc-frozen}"}
)

// Indexes of the free-text fields a re-read drops (they are what can hold a
// stray separator or newline).
const (
	paneTitleField  = 5
	windowNameField = 3
)

func format(nonce string, rec byte, fields []string) string {
	return nonce + string(rec) + Sep + strings.Join(fields, Sep)
}

// ReadArgs is the argv (after -u -S) of the ONE tmux read of a save (B15,
// B25, S6, S7): the server fields, clients, sessions, windows and panes, then
// the global automatic-rename value (for the per-window tri-state, T6).
// Without a client, `display -p '#{session_name}'` resolves tmux's best (most
// recently used) session, resurrect's fallback (PR-H15).
func ReadArgs(nonce string) []string {
	return chain(
		[]string{"display-message", "-p", format(nonce, recServer, serverFields)},
		[]string{"list-clients", "-F", format(nonce, recClient, clientFields)},
		[]string{"list-sessions", "-F", format(nonce, recSession, sessionFields)},
		[]string{"list-windows", "-a", "-F", format(nonce, recWindow, windowFields)},
		[]string{"list-panes", "-a", "-F", format(nonce, recPane, paneFields)},
		[]string{"display-message", "-p", nonce + string(recGlobal)},
		[]string{"show-options", "-gwv", "automatic-rename"},
	)
}

// chain joins commands with ";" (no argument here ends in ';', so the
// tmuxtarget escaper has nothing to do; it is not imported to keep this
// package's argv rules in one file).
func chain(cmds ...[]string) []string {
	var out []string
	for i, c := range cmds {
		if i > 0 {
			out = append(out, ";")
		}
		out = append(out, c...)
	}
	return out
}

// recLocal frames one window's local automatic-rename in the local read.
const recLocal = 'L'

// AmbiguousAutoRename lists the windows whose LOCAL automatic-rename the read
// cannot tell (I-5, T6): `#{automatic-rename}` is the effective value, so a
// window whose effective value equals the global one either sets that same
// value itself or sets nothing. A window whose effective value differs from
// the global one must set its own, and needs no question.
func AmbiguousAutoRename(r Read) []string {
	globalOn := r.GlobalAutoRename != "off"
	var ids []string
	for _, w := range r.Windows {
		if w.AutoRename == globalOn && strings.HasPrefix(w.ID, "@") {
			ids = append(ids, w.ID)
		}
	}
	return uniq(ids)
}

func uniq(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// LocalAutoRenameArgs is ONE chain asking each window in ids for its own
// automatic-rename: `show-options -wqv -t <id>` without -A prints the
// window-local value, or nothing when the window inherits. Each answer is
// framed by a marker line, because "nothing" is an answer too. A window that
// vanished since the read fails its command and tmux stops the chain there;
// the windows after it keep the read's inference.
func LocalAutoRenameArgs(nonce string, ids []string) []string {
	cmds := make([][]string, 0, 2*len(ids))
	for _, id := range ids {
		cmds = append(cmds,
			[]string{"display-message", "-p", nonce + string(recLocal) + Sep + id},
			[]string{"show-options", "-wqv", "-t", id, "automatic-rename"})
	}
	return chain(cmds...)
}

// ParseLocalAutoRename applies LocalAutoRenameArgs' output to r's windows and
// returns how many windows it answered for. A marker followed by "on" or
// "off" is a local value; a marker followed by another marker (or nothing) is
// a window that inherits.
func ParseLocalAutoRename(r *Read, out []byte, nonce string) int {
	marker := nonce + string(recLocal) + Sep
	local := map[string]string{}
	lines := strings.Split(string(out), "\n")
	for i := 0; i < len(lines); i++ {
		id, ok := strings.CutPrefix(lines[i], marker)
		if !ok {
			continue
		}
		v := ""
		if i+1 < len(lines) {
			switch next := strings.TrimSpace(lines[i+1]); next {
			case "on", "off":
				v = next
				i++
			}
		}
		local[id] = v
	}
	for i := range r.Windows {
		if v, ok := local[r.Windows[i].ID]; ok {
			r.Windows[i].Local, r.Windows[i].LocalRead = v, true
		}
	}
	return len(local)
}

// RereadPaneArgs re-reads one pane by id with the title dropped.
func RereadPaneArgs(nonce, paneID string) []string {
	f := append([]string(nil), paneFields...)
	f[paneTitleField] = ""
	return []string{"display-message", "-p", "-t", paneID, format(nonce, recPane, f)}
}

// RereadWindowArgs re-reads one window by id with the name dropped.
func RereadWindowArgs(nonce, windowID string) []string {
	f := append([]string(nil), windowFields...)
	f[windowNameField] = ""
	return []string{"display-message", "-p", "-t", windowID, format(nonce, recWindow, f)}
}

// ServerInfo is the server record.
type ServerInfo struct {
	Pid             int
	StartTime       int64
	Version         string
	BestSession     string
	Restoring       string
	Active          string
	WantContents    string
	ByPidDir        string
	CaptureContents string
	CaptureLines    string
	Retention       string
	SaveSeq         int
}

// ClientRow is one attached client.
type ClientRow struct {
	Session, LastSession string
	Activity             int64
}

// SessionRow is one session.
type SessionRow struct {
	ID, Name, Group string
	Grouped         bool
}

// WindowRow is one window of one session.
type WindowRow struct {
	ID, Session  string
	Index        int
	Name         string
	Active, Last bool
	Zoomed       bool
	Layout       string
	AutoRename   bool // effective value
	// Local is the window's own automatic-rename, "on", "off" or "" (it
	// inherits), valid when LocalRead (ParseLocalAutoRename, I-5).
	Local          string
	LocalRead      bool
	Width, Height  int
	NameUnreadable bool // re-read without its name
}

// PaneRow is one pane of one session.
type PaneRow struct {
	ID, Session    string
	Window         int
	WindowID       string
	Index          int
	Title, Cwd     string
	Active         bool
	CurrentCommand string
	Pid            int
	Dead           bool
	HistorySize    int
	CursorY        int
	Alternate      bool
	TTObs          string
	StartCommand   string
	MCPOwner       string
	MCPPane        string
	CCFrozen       string
}

// Read is what the read chain printed.
type Read struct {
	Server     ServerInfo
	HaveServer bool
	Clients    []ClientRow
	Sessions   []SessionRow
	Windows    []WindowRow
	Panes      []PaneRow
	// GlobalAutoRename is `show -gwv automatic-rename`: "on" or "off".
	GlobalAutoRename string
	// BadPanes / BadWindows are ids of rows whose field count was wrong; the
	// caller re-reads them alone (RereadPaneArgs/RereadWindowArgs).
	BadPanes, BadWindows []string
	// Stray are lines that carried no nonce (a newline inside a value), for
	// the log.
	Stray []string
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// ParseRead splits the read's output. Fields are split with an EXACT count,
// so empty fields survive (RR1/R13: an empty title can never shift the cwd).
func ParseRead(out []byte, nonce string) Read {
	var r Read
	lines := strings.Split(string(out), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, nonce) || len(line) <= len(nonce) {
			if line != "" {
				r.Stray = append(r.Stray, line)
			}
			continue
		}
		rec := line[len(nonce)]
		body := line[len(nonce)+1:]
		if rec == recGlobal {
			if i+1 < len(lines) {
				r.GlobalAutoRename = strings.TrimSpace(lines[i+1])
				i++
			}
			continue
		}
		body = strings.TrimPrefix(body, Sep)
		f := strings.Split(body, Sep)
		switch rec {
		case recServer:
			if len(f) == len(serverFields) {
				r.Server, r.HaveServer = parseServer(f), true
			}
		case recClient:
			if len(f) == len(clientFields) {
				r.Clients = append(r.Clients, ClientRow{Session: f[0], LastSession: f[1], Activity: atoi64(f[2])})
			}
		case recSession:
			if len(f) == len(sessionFields) {
				r.Sessions = append(r.Sessions, SessionRow{ID: f[0], Name: f[1], Grouped: f[2] == "1", Group: f[3]})
			}
		case recWindow:
			if len(f) != len(windowFields) {
				if strings.HasPrefix(f[0], "@") {
					r.BadWindows = append(r.BadWindows, f[0])
				}
				continue
			}
			r.Windows = append(r.Windows, parseWindow(f))
		case recPane:
			if len(f) != len(paneFields) {
				if strings.HasPrefix(f[0], "%") {
					r.BadPanes = append(r.BadPanes, f[0])
				}
				continue
			}
			r.Panes = append(r.Panes, parsePane(f))
		}
	}
	return r
}

// ParseReread parses one re-read row (a pane or a window) into r, marking the
// dropped field. It reports whether the row parsed.
func ParseReread(r *Read, out []byte, nonce string) bool {
	line := strings.TrimRight(string(out), "\n")
	if strings.Contains(line, "\n") || !strings.HasPrefix(line, nonce) || len(line) <= len(nonce) {
		return false
	}
	rec := line[len(nonce)]
	f := strings.Split(strings.TrimPrefix(line[len(nonce)+1:], Sep), Sep)
	switch {
	case rec == recPane && len(f) == len(paneFields):
		r.Panes = append(r.Panes, parsePane(f))
		return true
	case rec == recWindow && len(f) == len(windowFields):
		w := parseWindow(f)
		w.NameUnreadable = true
		r.Windows = append(r.Windows, w)
		return true
	}
	return false
}

func parseServer(f []string) ServerInfo {
	return ServerInfo{Pid: atoi(f[0]), StartTime: atoi64(f[1]), Version: f[2], BestSession: f[3],
		Restoring: f[4], Active: f[5], WantContents: f[6], ByPidDir: f[7], CaptureContents: f[8],
		CaptureLines: f[9], Retention: f[10], SaveSeq: atoi(f[11])}
}

func parseWindow(f []string) WindowRow {
	return WindowRow{ID: f[0], Session: f[1], Index: atoi(f[2]), Name: f[3], Active: f[4] == "1",
		Last: f[5] == "1", Zoomed: f[6] == "1", Layout: f[7], AutoRename: f[8] == "1" || f[8] == "on",
		Width: atoi(f[9]), Height: atoi(f[10])}
}

func parsePane(f []string) PaneRow {
	return PaneRow{ID: f[0], Session: f[1], Window: atoi(f[2]), WindowID: f[3], Index: atoi(f[4]), Title: f[5],
		Cwd: f[6], Active: f[7] == "1", CurrentCommand: f[8], Pid: atoi(f[9]), Dead: f[10] == "1",
		HistorySize: atoi(f[11]), CursorY: atoi(f[12]), Alternate: f[13] == "1", TTObs: f[14],
		StartCommand: f[15], MCPOwner: f[16], MCPPane: f[17], CCFrozen: f[18]}
}

// Restoring is a parsed @moox-restoring value: "<kind>:<pid>:<epoch>".
type Restoring struct {
	Kind  string
	Pid   int
	Epoch int64
}

// ParseRestoring parses @moox-restoring. ok=false for an empty value; a
// value that does not parse is returned with valid=false, which the caller
// treats as RESTORING (fail closed, §5.4).
func ParseRestoring(v string) (r Restoring, set, valid bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Restoring{}, false, true
	}
	p := strings.Split(v, ":")
	if len(p) != 3 {
		return Restoring{}, true, false
	}
	switch p[0] {
	case "boot", "restore", "shutdown":
	default:
		return Restoring{}, true, false
	}
	pid, err1 := strconv.Atoi(p[1])
	ep, err2 := strconv.ParseInt(p[2], 10, 64)
	if err1 != nil || err2 != nil || pid <= 0 || ep < 0 {
		return Restoring{}, true, false
	}
	return Restoring{Kind: p[0], Pid: pid, Epoch: ep}, true, true
}

// ClearRestoringArgs unsets @moox-restoring only if it still holds the dead
// value the save read (a conditional, so a value set meanwhile by a new
// restore is never cleared). The value is spliced into a format, so only a
// value of ParseRestoring's grammar (no format or separator character) is
// compared; anything else gets a condition that is never true (G1-M1): a
// caller holding a raw, unvalidated value clears nothing rather than letting
// `}` or `#(` reach tmux's format expander.
func ClearRestoringArgs(value string) []string {
	if _, set, valid := ParseRestoring(value); !set || !valid || strings.TrimSpace(value) != value {
		return []string{"if-shell", "-F", "0", "set-option -gu @moox-restoring"}
	}
	return []string{"if-shell", "-F", "#{==:#{@moox-restoring}," + value + "}", "set-option -gu @moox-restoring"}
}
