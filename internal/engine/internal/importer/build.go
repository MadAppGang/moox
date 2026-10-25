package importer

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// Continuity's tail tags (pre_save.sh:17, I5).
const (
	tagSID    = "CLAUDE_SID"
	tagCmd    = "CLAUDE_CMD"
	tagReplay = "CLAUDISH_REPLAY"
)

// syntheticPrefix names a pane whose layout leaf gave no usable pane id.
const syntheticPrefix = "%imp"

// build parses data and assembles the snapshot (contents and live
// recordings are attached by the caller).
func build(src Source, data []byte) (*Result, error) {
	if !isResurrect(data) {
		return nil, ErrNotResurrect
	}
	p := parse(data)
	res := &Result{Contents: map[string][]byte{}}
	rep := &res.Report
	rep.Rows, rep.Continuations = p.rows, p.continuations

	// H2: exactly one state row. Zero is a truncated dump (R2/R22), two a
	// doubled one; either is invalid, which also lets a directory import
	// fall back to an older file (H3).
	if len(p.states) != 1 {
		return nil, fmt.Errorf("%w: %d state rows (want exactly 1)", snapshot.ErrInvalid, len(p.states))
	}
	s := &snapshot.Snapshot{
		Format: snapshot.FormatName, Version: snapshot.Version,
		Producer: src.Producer, Source: snapshot.SourceImport,
		Client: &snapshot.Client{Session: p.states[0].session, LastSession: p.states[0].last},
	}
	res.Snapshot = s

	// Windows first: every pane must name one.
	type wkey struct {
		s string
		i int
	}
	winPanes := map[wkey][]int{} // window → indexes into s.Panes
	haveWin := map[wkey]bool{}
	var windows []snapshot.Window
	for _, w := range p.windows {
		k := wkey{w.session, w.index}
		if haveWin[k] {
			return nil, fmt.Errorf("%w: duplicate window %s:%d (line %d)", snapshot.ErrInvalid, w.session, w.index, w.line)
		}
		haveWin[k] = true
		windows = append(windows, windowOf(w, rep))
	}

	// Panes.
	seen := map[string]bool{}
	for _, r := range p.panes {
		k := r.key()
		if seen[k] {
			return nil, fmt.Errorf("%w: duplicate pane %s (line %d)", snapshot.ErrInvalid, k, r.line)
		}
		seen[k] = true
		wk := wkey{r.session, r.window}
		if !haveWin[wk] {
			rep.Dropped++
			rep.warnf("PANE-NO-WINDOW %s: no window row; dropped", k)
			continue
		}
		switch r.shape {
		case shapeRealigned:
			rep.Realigned++
		case shapeCollapsed:
			rep.Collapsed++
		}
		pane := paneOf(r, src, rep)
		winPanes[wk] = append(winPanes[wk], len(s.Panes))
		s.Panes = append(s.Panes, pane)
	}

	// Windows left with no pane are dropped (Validate would refuse them).
	for i, w := range windows {
		k := wkey{w.Session, w.Index}
		if len(winPanes[k]) == 0 {
			rep.Dropped++
			rep.warnf("WINDOW-NO-PANES %s:%d: dropped", w.Session, w.Index)
			continue
		}
		assignIDs(&windows[i], winPanes[k], s.Panes, rep)
		s.Windows = append(s.Windows, windows[i])
	}
	assignSynthetic(s.Panes)

	sessionsOf(s, p, rep)
	for _, pn := range s.Panes {
		if pn.SID() != "" {
			rep.Sids++
		}
	}
	rep.Panes, rep.Windows, rep.Sessions = len(s.Panes), len(s.Windows), len(s.Sessions)
	return res, nil
}

// windowOf maps a window row. Resurrect stores no size, so width/height come
// from the layout's root cell. A v1 layout whose checksum fails is
// re-encoded from its parsed body (tmux would refuse it otherwise).
func windowOf(w windowRec, rep *Report) snapshot.Window {
	out := snapshot.Window{Session: w.session, Index: w.index, Name: w.name, Active: w.active,
		Last: strings.Contains(w.flags, "-"), Zoomed: strings.Contains(w.flags, "Z"), Layout: w.layout}
	switch w.autoRename {
	case "on":
		out.AutomaticRename = snapshot.AutoRenameOn
	case "off":
		out.AutomaticRename = snapshot.AutoRenameOff
	case ":", "":
		out.AutomaticRename = snapshot.AutoRenameUnset
	default:
		out.AutomaticRename = snapshot.AutoRenameUnset
		rep.warnf("AUTO-RENAME %s:%d: unknown value; unset", w.session, w.index)
	}
	out.LayoutFormat = snapshot.LayoutV1
	if strings.HasPrefix(w.layout, "{") {
		out.LayoutFormat = snapshot.LayoutV2
	}
	root, _, err := layout.Parse(w.layout)
	if errors.Is(err, layout.ErrChecksum) && len(w.layout) > 5 {
		body := w.layout[5:]
		fixed := fmt.Sprintf("%04x,%s", layout.Checksum(body), body)
		if root, _, err = layout.Parse(fixed); err == nil {
			out.Layout = root.EncodeV1()
			rep.warnf("LAYOUT-CHECKSUM %s:%d: checksum did not match; re-encoded", w.session, w.index)
		}
	}
	if err != nil {
		rep.warnf("LAYOUT-UNPARSED %s:%d: %v", w.session, w.index, err)
		return out
	}
	out.Width, out.Height = root.W, root.H
	return out
}

// assignIDs gives each pane of a window the pane id its layout leaf carries
// (v2 "I", v1 the trailing ID), so a saved_id matches the layout and the
// restore can remove an observer's leaf by it (B45, §6.3). Panes are matched
// to leaves in pane-index order, which is tmux's assignment order. When the
// counts differ (3.8 floating panes) or an id is missing or repeated, the
// window's panes get synthetic ids instead (assignSynthetic).
func assignIDs(w *snapshot.Window, idx []int, panes []snapshot.Pane, rep *Report) {
	root, _, err := layout.Parse(w.Layout)
	if err != nil {
		return
	}
	leaves := root.Leaves()
	if len(leaves) != len(idx) {
		rep.warnf("LAYOUT-LEAVES %s:%d: %d leaves, %d panes", w.Session, w.Index, len(leaves), len(idx))
		return
	}
	order := append([]int(nil), idx...)
	sort.SliceStable(order, func(a, b int) bool { return panes[order[a]].Index < panes[order[b]].Index })
	ids := map[string]bool{}
	for _, l := range leaves {
		if l.SavedPane == "" || ids[l.SavedPane] {
			return
		}
		ids[l.SavedPane] = true
	}
	for i, pi := range order {
		panes[pi].SavedID = leaves[i].SavedPane
	}
}

// assignSynthetic gives every pane still without a saved_id a synthetic one
// (%imp<n>), unique in the snapshot.
func assignSynthetic(panes []snapshot.Pane) {
	n := 0
	for i := range panes {
		if panes[i].SavedID == "" {
			n++
			panes[i].SavedID = fmt.Sprintf("%s%d", syntheticPrefix, n)
		}
	}
}

// paneOf maps a pane row (saved_id is assigned by the window pass).
func paneOf(r paneRec, src Source, rep *Report) snapshot.Pane {
	k := r.key()
	out := snapshot.Pane{Key: k, Session: r.session, Window: r.window, Index: r.index, Title: r.title,
		Cwd: unescapePath(r.cwd, src.Home), Active: r.active, CurrentCommand: r.cmd,
		FullCommand: strings.TrimRight(r.full, " \t")}

	var sid, replay string
	hasReplay := false
	for _, t := range r.tags {
		switch t.key {
		case tagSID:
			if snapshot.SafeSID(t.value) {
				sid = t.value
			} else {
				rep.warnf("SID-REJECTED %s: not in [A-Za-z0-9_.-] (B34)", k)
			}
		case tagCmd:
			// The recording, base64 of the typed line, decoded FULLY with its
			// padding (no cut at '=', I5). First line only (B1, R35).
			if line, ok := decodeB64(t.value); ok {
				out.Launch = firstLine(line)
			} else if t.value != "" {
				rep.warnf("CMD-UNDECODABLE %s: ;CLAUDE_CMD= is not base64", k)
			}
		case tagReplay:
			replay, hasReplay = t.value, true // verbatim
		default:
			rep.warnf("TAG-UNKNOWN %s: ;%s= ignored", k, t.key)
		}
	}

	// Roles judge the first physical line only (PR-H14): continuation lines
	// are other children, not this pane's command.
	first, _, _ := strings.Cut(out.FullCommand, "\n")
	role, frozenKey := capture.Role(capture.PaneRow{Title: out.Title, CurrentCommand: out.CurrentCommand},
		first, sid)
	out.Role, out.FrozenKey = role, frozenKey
	if sid != "" && role == snapshot.RoleClaude {
		c := &snapshot.Claude{SessionID: sid, Launcher: "claude"}
		if hasReplay || isClaudish(first) || isClaudish(out.Launch) {
			c.Launcher = "claudish"
			c.Replay = replay
		}
		out.Claude = c
	} else if sid != "" {
		rep.warnf("SID-ON-%s %s: session id kept off a %s row", strings.ToUpper(role), k, role)
	}

	// B40/R14: a title scarred ":/path" by the old empty-title shift. On a
	// shell row whose cwd is empty or $HOME and whose path exists, the path
	// is the real cwd. The scar is cleared on every shell row.
	if role == snapshot.RoleShell && strings.HasPrefix(out.Title, ":/") {
		scar := unescapePath(out.Title[1:], src.Home)
		out.Title = ""
		if (out.Cwd == "" || (src.Home != "" && out.Cwd == src.Home)) && isDir(scar) {
			out.Cwd = scar
			rep.HealedTitles++
		} else {
			rep.warnf("TITLE-SCAR %s: scarred title cleared; cwd kept", k)
		}
	}
	return out
}

func decodeB64(v string) (string, bool) {
	if v == "" {
		return "", false
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil {
		return string(b), true
	}
	if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(v, "=")); err == nil {
		return string(b), true
	}
	return "", false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, "\r")
}

func isClaudish(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) > 0 && path.Base(f[0]) == "claudish"
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// sessionsOf lists the sessions in first-appearance order (panes, then
// windows), then the grouped secondaries (S5): a secondary owns no windows
// and names its primary.
func sessionsOf(s *snapshot.Snapshot, p parsed, rep *Report) {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, w := range s.Windows {
		add(w.Session)
	}
	groupOf := map[string]string{}
	for _, g := range p.grouped {
		groupOf[g.original] = g.original
	}
	for _, n := range names {
		active, last, first := -1, -1, -1
		for _, w := range s.Windows {
			if w.Session != n {
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
		s.Sessions = append(s.Sessions, snapshot.Session{Name: n, Group: groupOf[n], ActiveWindow: active, LastWindow: last})
	}
	for _, g := range p.grouped {
		if seen[g.name] {
			rep.warnf("GROUPED-HAS-WINDOWS %s: a grouped secondary with its own rows; kept as a primary", g.name)
			continue
		}
		if !seen[g.original] {
			rep.Dropped++
			rep.warnf("GROUPED-NO-PRIMARY %s: its original %s has no windows; dropped", g.name, g.original)
			continue
		}
		seen[g.name] = true
		s.Sessions = append(s.Sessions, snapshot.Session{Name: g.name, Group: g.original, GroupedWith: g.original,
			ActiveWindow: g.active, LastWindow: g.alt})
	}
}
