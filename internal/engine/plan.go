package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/internal/sidmap"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// Plan is what a restore creates (§6.4 step 2), computed purely from a
// snapshot: it is also `moox restore --dry-run`'s output (§6.4a step 0).
type Plan struct {
	// Sessions: primaries in snapshot order, then grouped members (S5, T11).
	Sessions []PlanSession
	Windows  []PlanWindow
	// Rows are the panes the restore creates, in creation order.
	Rows []PlanRow
	// Dropped are the tt-observer rows, removed from their layouts (B45).
	Dropped []string
	// Tombstones are the indices of frozen rows in Rows; they are handled
	// first after creation (B38).
	Tombstones []int
	Warnings   []string
}

// PlanSession is one session.
type PlanSession struct {
	Name         string
	GroupedWith  string // "" for a primary
	ActiveWindow int
	LastWindow   int // -1: none
	// Windows are indices into Plan.Windows, in window-index order.
	Windows []int
	// Width, Height: the largest window of the session, for new-session -x -y
	// (web-findings 2.3).
	Width, Height int
}

// PlanWindow is one window.
type PlanWindow struct {
	Session         string
	Index           int
	Name            string
	Active, Last    bool
	Zoomed          bool
	AutomaticRename string
	// Layout is the saved layout after RemoveLeaf of dropped observers, in
	// LayoutFormat; the executor re-encodes it for the server's version.
	Layout       string
	LayoutFormat string
	Width        int
	Height       int
	// Rows are indices into Plan.Rows in the order the panes are created,
	// which is the order select-layout assigns cells (layout leaf order).
	Rows []int
	// ActiveRow is the index into Plan.Rows of the active pane, or -1.
	ActiveRow int
}

// PlanRow is one pane.
type PlanRow struct {
	Key    string // the saved S:W.P
	Window int    // index into Plan.Windows
	Pane   snapshot.Pane
	// Decision (§6.5): Cmd is what MOOX_RESUME carries ("" = nothing).
	Cmd       string
	Kind      string
	Claude    bool
	Qualified bool
	Reason    string
	// Original is the command before stub mode replaced it (verdicts judge
	// the original's exec token).
	Original string
	// Contents: the pane gets MOOX_RESTORE_CONTENT (its saved scrollback, or
	// a tombstone's banner).
	Contents bool
}

// Resumes reports whether the row's creating command carries MOOX_RESUME.
func (r PlanRow) Resumes() bool { return r.Cmd != "" }

// planInputs are what buildPlan needs beyond the snapshot.
type planInputs struct {
	relaunch relaunch.Options
	// contents reports whether a saved_id has a contents entry.
	contents func(savedID string) bool
	// banner reports whether a frozen key has a banner file.
	banner func(key string) bool
}

// Plan is the pure dry run (§6.4a step 0): what Restore would create, with
// the default relaunch options and without contents lookups.
func (e *Engine) Plan(_ context.Context, s *snapshot.Snapshot) (Plan, error) {
	return buildPlan(s, planInputs{relaunch: relaunch.Options{NvimSession: true}})
}

func buildPlan(s *snapshot.Snapshot, in planInputs) (Plan, error) {
	if err := snapshot.Validate(s); err != nil {
		return Plan{}, err
	}
	if in.contents == nil {
		in.contents = func(string) bool { return false }
	}
	if in.banner == nil {
		in.banner = func(string) bool { return false }
	}
	ro := in.relaunch
	if s.Source == snapshot.SourceImport {
		ro.UnenrichedImport = true
	}
	var p Plan

	type wkey struct {
		s string
		i int
	}
	byWindow := map[wkey][]snapshot.Pane{}
	for _, pn := range s.Panes {
		byWindow[wkey{pn.Session, pn.Window}] = append(byWindow[wkey{pn.Session, pn.Window}], pn)
	}
	windowsOf := map[string][]snapshot.Window{}
	for _, w := range s.Windows {
		windowsOf[w.Session] = append(windowsOf[w.Session], w)
	}

	addSession := func(se snapshot.Session) {
		ps := PlanSession{Name: se.Name, GroupedWith: se.GroupedWith, ActiveWindow: se.ActiveWindow, LastWindow: se.LastWindow}
		if se.GroupedWith != "" {
			p.Sessions = append(p.Sessions, ps)
			return
		}
		ws := append([]snapshot.Window(nil), windowsOf[se.Name]...)
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].Index < ws[j].Index })
		for _, w := range ws {
			wi, ok := p.addWindow(w, byWindow[wkey{w.Session, w.Index}], ro, in)
			if !ok {
				continue
			}
			ps.Windows = append(ps.Windows, wi)
			pw := p.Windows[wi]
			ps.Width, ps.Height = max(ps.Width, pw.Width), max(ps.Height, pw.Height)
		}
		if len(ps.Windows) == 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("SESSION-EMPTY %q: every pane was dropped", se.Name))
			return
		}
		p.Sessions = append(p.Sessions, ps)
	}
	for _, se := range s.Sessions {
		if se.GroupedWith == "" {
			addSession(se)
		}
	}
	created := map[string]bool{}
	for _, ps := range p.Sessions {
		created[ps.Name] = true
	}
	for _, se := range s.Sessions {
		if se.GroupedWith != "" && created[se.GroupedWith] {
			addSession(se)
		}
	}
	return p, nil
}

// addWindow plans one window: observers dropped and removed from the layout
// (B45, RR10, §6.3), the rest ordered by the layout's leaf order.
func (p *Plan) addWindow(w snapshot.Window, panes []snapshot.Pane, ro relaunch.Options, in planInputs) (int, bool) {
	var keep []snapshot.Pane
	var drop []string
	for _, pn := range panes {
		if pn.Role == snapshot.RoleTTObserver {
			p.Dropped = append(p.Dropped, pn.Key)
			drop = append(drop, pn.SavedID)
			continue
		}
		keep = append(keep, pn)
	}
	if len(keep) == 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("WINDOW-EMPTY %s:%d: only observer panes", w.Session, w.Index))
		return 0, false
	}
	pw := PlanWindow{Session: w.Session, Index: w.Index, Name: w.Name, Active: w.Active, Last: w.Last, Zoomed: w.Zoomed,
		AutomaticRename: w.AutomaticRename, Layout: w.Layout, LayoutFormat: w.LayoutFormat, Width: w.Width,
		Height: w.Height, ActiveRow: -1}

	root, format, err := layout.Parse(w.Layout)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("LAYOUT-UNPARSABLE %s:%d: %v", w.Session, w.Index, err))
		root = nil
	}
	if root != nil && len(drop) > 0 {
		cl := root.Clone()
		ok := true
		for _, id := range drop {
			if err := cl.RemoveLeaf(id); err != nil {
				p.Warnings = append(p.Warnings, fmt.Sprintf("LAYOUT-REMOVE-FAILED %s:%d %s: %v", w.Session, w.Index, id, err))
				ok = false
				break
			}
		}
		if ok {
			root = cl
			pw.Layout = root.Encode(format)
		}
	}
	if root != nil && (pw.Width == 0 || pw.Height == 0) {
		pw.Width, pw.Height = root.W, root.H
	}

	// Creation order = leaf order, when every kept row names a distinct leaf;
	// otherwise pane-index order (tmux assigns cells to panes in list order).
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].Index < keep[j].Index })
	if root != nil {
		pos := map[string]int{}
		for i, lf := range root.Leaves() {
			if lf.SavedPane != "" {
				pos[lf.SavedPane] = i
			}
		}
		all := true
		seen := map[int]bool{}
		for _, pn := range keep {
			i, ok := pos[pn.SavedID]
			if !ok || seen[i] {
				all = false
				break
			}
			seen[i] = true
		}
		if all {
			sort.SliceStable(keep, func(i, j int) bool { return pos[keep[i].SavedID] < pos[keep[j].SavedID] })
		}
	}

	wi := len(p.Windows)
	for _, pn := range keep {
		ri := len(p.Rows)
		row := p.planRow(pn, ro, in)
		row.Window = wi
		p.Rows = append(p.Rows, row)
		pw.Rows = append(pw.Rows, ri)
		if pn.Active {
			pw.ActiveRow = ri
		}
		if pn.Role == snapshot.RoleFrozen {
			p.Tombstones = append(p.Tombstones, ri)
		}
	}
	p.Windows = append(p.Windows, pw)
	return wi, true
}

func (p *Plan) planRow(pn snapshot.Pane, ro relaunch.Options, in planInputs) PlanRow {
	orig := ro
	orig.Stub = false
	o := relaunch.Compose(pn, orig)
	d := o
	if ro.Stub {
		d = relaunch.Compose(pn, ro)
	}
	row := PlanRow{Key: pn.Key, Pane: pn, Cmd: d.Cmd, Kind: d.Kind, Claude: d.Claude, Qualified: d.Qualified,
		Reason: d.Reason, Original: o.Cmd}
	switch pn.Role {
	case snapshot.RoleFrozen:
		row.Contents = pn.FrozenKey != "" && in.banner(pn.FrozenKey)
	default:
		row.Contents = pn.Contents && pn.SavedID != "" && in.contents(pn.SavedID)
	}
	if d.Reason != "" && d.Reason != "skipped_unknown" {
		p.Warnings = append(p.Warnings, fmt.Sprintf("RELAUNCH %s: %s", pn.Key, d.Reason))
	}
	return row
}

// bootRows are the plan's rows as the BOOT accounting sees them.
func (p Plan) bootRows() []verdict.Row {
	rows := make([]verdict.Row, 0, len(p.Rows))
	for _, r := range p.Rows {
		rows = append(rows, verdict.Row{Key: r.Key, Considered: r.Qualified && r.Kind != relaunch.KindFrozen,
			Resume: r.Resumes()})
	}
	return rows
}

// launchToken is the exec token a non-Claude row is judged by (§6.5).
func launchToken(cmd string) string { return sidmap.ExecBase(cmd) }
