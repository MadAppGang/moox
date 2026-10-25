package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// session is a single tmux session. It implements list.Item so it can be
// rendered and filtered directly by the bubbles list component.
type session struct {
	name    string
	windows int
	// lastAttached is the unix timestamp (seconds) of when this session was
	// last attached. It is 0 for sessions never attached this boot (e.g.
	// sessions restored by tmux-resurrect), and drives the MRU sort.
	lastAttached int64
	// created is the unix timestamp (seconds) of when this session was created.
	// It is the secondary sort key, used to order the never-attached sessions
	// (which all tie at lastAttached 0) most-recent-first.
	created int64
	// attached reports whether a client is currently attached to this session.
	attached bool
	// shown counts the terminals this session is on screen in right now, split
	// into windows on this machine and remote ones (ssh/mosh in from elsewhere).
	// The row draws them as badges on the right.
	shown shownOn
}

// shownOn is how many attached clients a session has, by where they paint.
type shownOn struct {
	here, remote int
}

func (s session) Title() string { return s.name }

func (s session) Description() string {
	if s.windows == 1 {
		return "1 window"
	}
	return strconv.Itoa(s.windows) + " windows"
}

// FilterValue is what the list's built-in "/" search matches against.
func (s session) FilterValue() string { return s.name }

// listSessions queries tmux for the current sessions. It is a variable so tests
// can substitute a deterministic source instead of shelling out to tmux.
var listSessions = queryTmuxSessions

// queryTmuxSessions queries tmux for the current sessions in a single call,
// pulling every field the list needs at once (name, window count, and the
// timestamps the MRU sort keys on). If no tmux server is running (or the
// command otherwise fails), it returns an empty slice and a nil error so the
// caller can show a graceful empty state instead of crashing.
//
// The fields are TAB-separated; a literal "\t" in the format string survives
// tmux verbatim and gives us an unambiguous separator (session names can't
// contain tabs). last_attached is EMPTY for sessions never attached this boot
// (e.g. restored sessions), which we parse as 0.
func queryTmuxSessions() []session {
	const format = "#{session_name}\t#{session_windows}\t#{session_last_attached}\t#{session_created}\t#{session_attached}"

	out, err := tmuxCommand(context.Background(), "list-sessions", "-F", format).Output()
	if err != nil {
		return nil
	}

	var sessions []session
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Guard the column count: a short/malformed line is skipped rather than
		// indexed into and panicking. atoiOr treats unparseable/empty numeric
		// fields (notably an empty last_attached) as 0.
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		sessions = append(sessions, session{
			name:         fields[0],
			windows:      atoiOr(fields[1], 0),
			lastAttached: int64(atoiOr(fields[2], 0)),
			created:      int64(atoiOr(fields[3], 0)),
			// session_attached is a client COUNT, not a flag: two terminals
			// on one session read "2".
			attached: atoiOr(fields[4], 0) > 0,
		})
	}

	shown := sessionsShown(clientsOf(), processTable())
	for i := range sessions {
		sessions[i].shown = shown[sessions[i].name]
	}
	return sessions
}

// client is one terminal attached to the server: the session it shows and the
// pid of its `tmux` client process.
type client struct {
	session string
	pid     int
}

// clientsOf lists the attached clients. A failure is "no clients": the icons
// are a hint, never a reason to fail the list.
func clientsOf() []client {
	out, err := tmuxCommand(context.Background(), "list-clients", "-F", "#{client_session}\t#{client_pid}").Output()
	if err != nil {
		return nil
	}
	var clients []client
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		clients = append(clients, client{session: fields[0], pid: atoiOr(fields[1], 0)})
	}
	return clients
}

// proc is one row of the process table: its parent, and the command's base name.
type proc struct {
	ppid int
	comm string
}

// processTable reads every process's parent and command in one `ps` call, so
// classifying N clients costs one fork, not one per ancestor.
func processTable() map[int]proc {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}
	table := map[int]proc{}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		// comm is a full path on macOS and may hold spaces
		// (".../Application Support/iTerm2/iTermServer-3.7.6").
		comm := strings.Join(f[2:], " ")
		table[atoiOr(f[0], 0)] = proc{ppid: atoiOr(f[1], 0), comm: filepath.Base(comm)}
	}
	return table
}

// remoteServers are the daemons a remote terminal's shell hangs off. A client
// with one of them among its ancestors is painting on another machine.
var remoteServers = map[string]bool{
	"sshd": true, "sshd-session": true, "tsshd": true, "mosh-server": true, "etserver": true,
}

// sessionsShown counts each session's clients by where they paint. A client
// whose ancestry reaches a remote server is remote; anything else (an iTerm2
// window, a nested tmux client) is on this machine.
func sessionsShown(clients []client, table map[int]proc) map[string]shownOn {
	shown := map[string]shownOn{}
	for _, c := range clients {
		remote := false
		// The depth cap guards a table read mid-fork from looping on a cycle.
		for pid, depth := c.pid, 0; pid > 1 && depth < 64; depth++ {
			p, ok := table[pid]
			if !ok {
				break
			}
			if remoteServers[strings.TrimPrefix(p.comm, "-")] {
				remote = true
				break
			}
			pid = p.ppid
		}
		on := shown[c.session]
		if remote {
			on.remote++
		} else {
			on.here++
		}
		shown[c.session] = on
	}
	return shown
}

// atoiOr parses s as an integer, returning fallback when s is empty or not a
// number. tmux leaves last_attached empty for never-attached sessions, so the
// fallback keeps parsing total instead of erroring per row.
func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

// The mutators are variables, like listSessions, so tests can substitute a
// recording fake to assert whether (and with what) they were invoked — without
// shelling out to a real tmux server.
var (
	killSession   = tmuxKillSession
	newSession    = tmuxNewSession
	renameSession = tmuxRenameSession
)

// tmuxKillSession kills the named tmux session.
func tmuxKillSession(name string) error {
	return runTmux("kill-session", "-t", name)
}

// tmuxNewSession creates a new detached tmux session so the TUI keeps control
// of the terminal.
func tmuxNewSession(name string) error {
	return runTmux("new-session", "-d", "-s", name)
}

// tmuxRenameSession renames an existing tmux session.
func tmuxRenameSession(oldName, newName string) error {
	return runTmux("rename-session", "-t", oldName, newName)
}

// runTmux runs a tmux command and folds its stderr into the returned error so
// the failure message we surface to the user is tmux's own (e.g. "duplicate
// session: foo") rather than a bare "exit status 1". CombinedOutput captures
// the stderr that .Run() would otherwise discard.
func runTmux(args ...string) error {
	out, err := tmuxCommand(context.Background(), args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	return nil
}
