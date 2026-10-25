package capture

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/sidmap"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// Ignored session prefixes (PR-C8): tt's throwaway popups and a renamed
// bootstrap are never saved, with their windows and panes.
var ignoredSessionPrefixes = []string{"tt-popup-", "moox-boot-"}

// FrozenTitlePrefix marks a tombstone pane (B38).
const FrozenTitlePrefix = "❄ FROZEN "

// shells are the foreground commands that make a pane a "shell".
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true,
	"tcsh": true, "csh": true, "login": true}

// IsShell reports whether a pane_current_command is a shell.
func IsShell(cmd string) bool {
	cmd = strings.TrimPrefix(path.Base(strings.TrimSpace(cmd)), "-")
	return cmd == "" || shells[cmd]
}

// Live is the read after the row filter (§6.2 step 2, PR-C8): ignored
// sessions dropped, and for every session group only the PRIMARY's windows
// and panes kept. list-panes -a reports a grouped pane once per member with
// the same pane_id; without this a secondary could take the primary's sid
// under B10.
type Live struct {
	Sessions []SessionRow // kept sessions, in list order (primaries and secondaries)
	// GroupedWith maps a secondary's name to its primary's name.
	GroupedWith map[string]string
	Windows     []WindowRow // the primaries' (and ungrouped sessions') windows
	AllWindows  []WindowRow // every kept session's window rows, for active/last
	Panes       []PaneRow
	Ignored     []string
}

func sessionNum(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "$"))
	if err != nil {
		return 1 << 30
	}
	return n
}

// Filter applies the row filter.
func Filter(r Read) Live {
	l := Live{GroupedWith: map[string]string{}}
	primary := map[string]SessionRow{} // group → primary (lowest session_id)
	for _, s := range r.Sessions {
		if ignored(s.Name) {
			continue
		}
		if s.Grouped && s.Group != "" {
			if p, ok := primary[s.Group]; !ok || sessionNum(s.ID) < sessionNum(p.ID) {
				primary[s.Group] = s
			}
		}
	}
	keep := map[string]bool{}
	for _, s := range r.Sessions {
		if ignored(s.Name) {
			l.Ignored = append(l.Ignored, s.Name)
			continue
		}
		l.Sessions = append(l.Sessions, s)
		if s.Grouped && s.Group != "" {
			if p := primary[s.Group]; p.Name != s.Name {
				l.GroupedWith[s.Name] = p.Name
				continue
			}
		}
		keep[s.Name] = true
	}
	for _, w := range r.Windows {
		if ignored(w.Session) {
			continue
		}
		l.AllWindows = append(l.AllWindows, w)
		if keep[w.Session] {
			l.Windows = append(l.Windows, w)
		}
	}
	for _, p := range r.Panes {
		if keep[p.Session] {
			l.Panes = append(l.Panes, p)
		}
	}
	return l
}

func ignored(name string) bool {
	for _, p := range ignoredSessionPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// SidmapPanes is the pane table the climb runs over.
func (l Live) SidmapPanes() []sidmap.Pane {
	out := make([]sidmap.Pane, 0, len(l.Panes))
	for _, p := range l.Panes {
		out = append(out, sidmap.Pane{Pid: p.Pid, Target: snapshot.PaneKey(p.Session, p.Window, p.Index), ID: p.ID})
	}
	return out
}

// PaneNumber is "%12" → "12", the name of a launch recording (B15).
func PaneNumber(id string) string { return strings.TrimPrefix(id, "%") }

// ClaudishModelPids lists the Claude pids whose launcher names no model, so
// the caller reads their environment with one single-pid ps eww each (B12).
func ClaudishModelPids(res sidmap.Result, ps Table) []int {
	var out []int
	for _, e := range res.ByPane {
		if e.ClaudishPid == 0 {
			continue
		}
		launcher, _ := ps.Get(e.ClaudishPid)
		if !HasExplicitModel(ClaudishReplay(launcher.Command, "")) {
			out = append(out, e.Pid)
		}
	}
	sort.Ints(out)
	return out
}

// Input is everything Build needs.
type Input struct {
	Live       Live
	Server     ServerInfo
	Clients    []ClientRow
	GlobalAuto string
	PS         Table
	SIDs       sidmap.Result
	// Recordings maps a pane id to its recording's first line (absent or
	// empty file: no entry, B15).
	Recordings map[string]string
	// Models maps a Claude pid to its CLAUDISH_ACTIVE_MODEL_NAME.
	Models      map[int]string
	Prev        *snapshot.Snapshot
	Socket      snapshot.Socket
	Producer    string
	CreatedAt   time.Time
	ExcludePane string // §8.5 step 4: moox restart's own pane
}

// Report is what Build has to say for the log.
type Report struct {
	Dups     []sidmap.Dup
	Warnings []string
}

// Build assembles the snapshot from live state (§6.2 steps 6–7). Each
// snapshot is built fresh from live state, so enrichment is idempotent by
// construction (B16). Pane contents are attached by the caller.
func Build(in Input) (*snapshot.Snapshot, Report) {
	rep := Report{Dups: in.SIDs.Dups}
	s := &snapshot.Snapshot{
		Format: snapshot.FormatName, Version: snapshot.Version,
		CreatedAt: in.CreatedAt.Format(time.RFC3339), Producer: in.Producer, Source: snapshot.SourceSave,
		TmuxVersion: in.Server.Version, Socket: in.Socket,
		Server: &snapshot.Server{Pid: in.Server.Pid, StartTime: in.Server.StartTime},
	}

	panes := in.Live.Panes
	windows := in.Live.Windows
	if in.ExcludePane != "" {
		panes, windows, rep.Warnings = exclude(panes, windows, in.ExcludePane, rep.Warnings)
	}

	perWindow := map[string]int{}
	for _, p := range panes {
		perWindow[p.WindowID]++
	}
	haveWindow := map[string]bool{}
	gone := map[string]bool{} // windows whose only pane was excluded
	for _, w := range windows {
		if perWindow[w.ID] == 0 {
			gone[w.ID] = true
			continue // its only pane was excluded
		}
		haveWindow[w.Session] = true
		s.Windows = append(s.Windows, window(w, in.GlobalAuto))
		if n, _, err := layout.Parse(w.Layout); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("LAYOUT-UNPARSED %s:%d: %v", w.Session, w.Index, err))
		} else if n.LeafCount() != perWindow[w.ID] {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("LAYOUT-LEAVES %s:%d: %d leaves, %d panes",
				w.Session, w.Index, n.LeafCount(), perWindow[w.ID]))
		}
	}

	kept := map[string]bool{}
	for _, se := range in.Live.Sessions {
		prim, secondary := in.Live.GroupedWith[se.Name]
		if !secondary && !haveWindow[se.Name] {
			continue // every window excluded
		}
		if secondary && !haveWindow[prim] {
			continue
		}
		active, last := activeLast(in.Live.AllWindows, se.Name, gone)
		s.Sessions = append(s.Sessions, snapshot.Session{Name: se.Name, Group: groupOf(se), GroupedWith: prim,
			ActiveWindow: active, LastWindow: last})
		kept[se.Name] = true
	}

	for _, p := range panes {
		s.Panes = append(s.Panes, pane(p, in))
	}
	s.Client = client(in, kept)
	return s, rep
}

func groupOf(s SessionRow) string {
	if s.Grouped {
		return s.Group
	}
	return ""
}

// activeLast is a session's active and last window index; a window in gone
// (dropped with its excluded pane) is neither, so the snapshot never names a
// window it does not hold (G1-H9).
func activeLast(rows []WindowRow, session string, gone map[string]bool) (int, int) {
	active, last, first := -1, -1, -1
	for _, w := range rows {
		if w.Session != session || gone[w.ID] {
			continue
		}
		if first < 0 {
			first = w.Index
		}
		if w.Active {
			active = w.Index
		}
		if w.Last {
			last = w.Index
		}
	}
	if active < 0 {
		active = first
	}
	return active, last
}

func window(w WindowRow, globalAuto string) snapshot.Window {
	// Tri-state (T6): the window's own value when it was asked
	// (ParseLocalAutoRename, I-5). Otherwise only the EFFECTIVE value is
	// known: one that differs from the global must be local; one equal to it
	// is taken as unset.
	auto := snapshot.AutoRenameUnset
	globalOn := globalAuto != "off"
	switch {
	case w.LocalRead && w.Local == "on":
		auto = snapshot.AutoRenameOn
	case w.LocalRead && w.Local == "off":
		auto = snapshot.AutoRenameOff
	case w.LocalRead:
		// asked, and it sets nothing of its own: unset
	case w.AutoRename != globalOn:
		auto = snapshot.AutoRenameOff
		if w.AutoRename {
			auto = snapshot.AutoRenameOn
		}
	}
	lf := snapshot.LayoutV1
	if strings.HasPrefix(w.Layout, "{") {
		lf = snapshot.LayoutV2
	}
	return snapshot.Window{Session: w.Session, Index: w.Index, ID: w.ID, Name: w.Name, Active: w.Active, Last: w.Last,
		Zoomed: w.Zoomed, AutomaticRename: auto, Layout: w.Layout, LayoutFormat: lf, Width: w.Width, Height: w.Height}
}

// Role is §6.2 step 7, in its order. A role says how a pane is RESTORED AS A
// PANE; it never decides on its own whether a command is replayed (PR-H19).
func Role(p PaneRow, fullCommand, sid string) (role, frozenKey string) {
	switch {
	case p.TTObs == "1" || strings.Contains(p.StartCommand, "tt observe --pane") ||
		strings.Contains(fullCommand, "tt observe --pane"):
		return snapshot.RoleTTObserver, ""
	case p.MCPOwner == "agent" && p.MCPPane == p.ID:
		return snapshot.RoleMCPHelper, ""
	case strings.HasPrefix(p.Title, FrozenTitlePrefix) || p.CCFrozen != "":
		key := p.CCFrozen
		if key == "" {
			if f := strings.Fields(strings.TrimPrefix(p.Title, FrozenTitlePrefix)); len(f) > 0 {
				key = f[0]
			}
		}
		return snapshot.RoleFrozen, key
	case sid != "":
		return snapshot.RoleClaude, ""
	case !IsShell(p.CurrentCommand):
		return snapshot.RoleProgram, ""
	}
	return snapshot.RoleShell, ""
}

func pane(p PaneRow, in Input) snapshot.Pane {
	full := in.PS.FullCommand(p.Pid)
	e, hasSID := in.SIDs.ByPane[p.ID]
	sid := ""
	if hasSID {
		sid = e.SID
	}
	role, key := Role(p, full, sid)
	out := snapshot.Pane{Key: snapshot.PaneKey(p.Session, p.Window, p.Index), Session: p.Session, Window: p.Window,
		Index: p.Index, SavedID: p.ID, Title: p.Title, Cwd: p.Cwd, Active: p.Active, CurrentCommand: p.CurrentCommand,
		FullCommand: full, Role: role, Launch: in.Recordings[p.ID], FrozenKey: key}
	if role == snapshot.RoleMCPHelper {
		// saved as plain layout: no command and no sid (B14, R20)
		out.FullCommand, out.Launch = "", ""
		return out
	}
	if hasSID {
		c := &snapshot.Claude{SessionID: e.SID, Launcher: "claude"}
		if e.ClaudishPid != 0 {
			launcher, _ := in.PS.Get(e.ClaudishPid)
			c.Launcher = "claudish"
			c.Replay = ClaudishReplay(launcher.Command, in.Models[e.Pid])
		}
		out.Claude = c
	}
	return out
}

func client(in Input, kept map[string]bool) *snapshot.Client {
	c := &snapshot.Client{}
	var best *ClientRow
	for i := range in.Clients {
		if best == nil || in.Clients[i].Activity > best.Activity {
			best = &in.Clients[i]
		}
	}
	keptOr := func(name string) string {
		if kept[name] {
			return name
		}
		return ""
	}
	if best != nil {
		c.Session, c.LastSession = keptOr(best.Session), keptOr(best.LastSession)
		if c.Session == "" {
			c.Session = keptOr(in.Server.BestSession)
		}
		return c
	}
	// No client: tmux's best session, and the previous snapshot's
	// last_session if that session still exists (RR16, PR-H15).
	c.Session = keptOr(in.Server.BestSession)
	if in.Prev != nil && in.Prev.Client != nil {
		c.LastSession = keptOr(in.Prev.Client.LastSession)
	}
	return c
}

// exclude drops one pane (§8.5 step 4) and removes its leaf from its
// window's layout, so the saved geometry stays exact (§6.3).
func exclude(panes []PaneRow, windows []WindowRow, id string, warn []string) ([]PaneRow, []WindowRow, []string) {
	var wid string
	wasActive := false
	out := make([]PaneRow, 0, len(panes))
	for _, p := range panes {
		if p.ID == id {
			wid, wasActive = p.WindowID, p.Active
			continue
		}
		out = append(out, p)
	}
	if wid == "" {
		return panes, windows, warn
	}
	// The excluded pane is usually the focused one (`moox restart` from the
	// pane it runs in): a window keeps exactly one active pane, so the first
	// remaining pane of the window takes over (G1-H9).
	if wasActive {
		for i := range out {
			if out[i].WindowID == wid {
				out[i].Active = true
				break
			}
		}
	}
	ws := make([]WindowRow, len(windows))
	copy(ws, windows)
	for i := range ws {
		if ws[i].ID != wid {
			continue
		}
		if wasActive {
			ws[i].Zoomed = false // tmux zooms the active pane, which is gone
		}
		n, f, err := layout.Parse(ws[i].Layout)
		if err == nil {
			err = n.RemoveLeaf(id)
		}
		if errors.Is(err, layout.ErrOnlyLeaf) {
			continue // the window goes with its only pane
		}
		if err != nil {
			warn = append(warn, fmt.Sprintf("LAYOUT-REMOVE-FAILED %s (excluded %s): %v", wid, id, err))
			continue
		}
		ws[i].Layout = n.Encode(f)
	}
	return out, ws, warn
}

// Pristine reports the bootstrap shape (A15): exactly one session with one
// window and one plain shell pane, with no recording and no sid.
func Pristine(s *snapshot.Snapshot) bool {
	if len(s.Sessions) != 1 || len(s.Windows) != 1 || len(s.Panes) != 1 {
		return false
	}
	p := s.Panes[0]
	return p.Role == snapshot.RoleShell && p.Launch == "" && p.SID() == ""
}
