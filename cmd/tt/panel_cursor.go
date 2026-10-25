package main

// panel_cursor.go is the sidebar's cursor (the user, 2026-10-01: "add
// navigation: a cursor, select the items, and on enter switch tmux to that
// session"). j/k (or ↓/↑) move a highlight over the session rows in the order
// they are drawn, across sections; the rows scroll to keep it in view; enter
// switches the tmux client to the highlighted session's pane — its session,
// window and pane at once (`switch-client -t %N`).
//
// The cursor is a session's identity, not a line number, so it stays on its
// session while rows move under it; when that session leaves the list it lands
// on the row that took its place. It is drawn only in a sidebar holding the
// focus: at rest, as with the shortcuts, there is nothing to select with.

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/MadAppGang/moox/internal/observer"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// panelNavRow is one selectable row of the last frame, in drawing order: the
// session, and the lines it spans in the frame's body (the lines between the
// header and the footer, scrolled() coordinates). top is the section title
// above a section's first row, so moving onto it brings the title into view.
type panelNavRow struct {
	key              string
	s                observer.Session
	top, first, last int
}

// panelSessionKey is a session's identity: (sessionId, kind), as everywhere.
func panelSessionKey(s observer.Session) string {
	return s.SessionID + "\x00" + string(s.Kind)
}

// cursorShown reports whether the cursor is drawn: a live sidebar holding the
// focus, and only while its keys move the cursor — not while the picker or
// the filter's prompt takes them (the user, 2026-10-06: "when navigation is
// not working we should hide the selection marker").
func (p panel) cursorShown() bool {
	return p.navigable && p.surface == surfacePane && !p.blurred && !p.picking && !p.filtering
}

// navRows lists a planned frame's rows, in body coordinates (head is the
// header's line count).
func navRows(items []panelItem, head int) []panelNavRow {
	var nav []panelNavRow
	for i, it := range items {
		switch it.kind {
		case itemRow:
			r := panelNavRow{key: panelSessionKey(it.row.s), s: it.row.s, top: i - head, first: i - head, last: i - head}
			if i > 0 && items[i-1].kind == itemHeading {
				r.top--
			}
			nav = append(nav, r)
		case itemText, itemMeta:
			if n := len(nav); n > 0 && it.row != nil && panelSessionKey(it.row.s) == nav[n-1].key {
				nav[n-1].last = i - head
			}
		}
	}
	return nav
}

// cursorIndex is the cursor's row in nav: its session's, else the row now at
// the index it was last on, else the first; -1 when there are no rows.
func (p panel) cursorIndex(nav []panelNavRow) int {
	if len(nav) == 0 {
		return -1
	}
	if p.cursor != "" {
		for i, r := range nav {
			if r.key == p.cursor {
				return i
			}
		}
	}
	return min(max(p.cursorAt, 0), len(nav)-1)
}

// moveCursor moves the cursor delta rows and scrolls to keep its row, and its
// section's title above a first row, in view. The first row scrolls to the
// top, so the header's band is in view again.
func (p *panel) moveCursor(delta int) {
	if p.memo == nil {
		return
	}
	p.lines() // the frame the reader sees, and its rows
	nav := p.memo.nav
	i := p.cursorIndex(nav)
	if i < 0 {
		return
	}
	i = min(max(i+delta, 0), len(nav)-1)
	p.cursor, p.cursorAt = nav[i].key, i
	view := p.memo.view
	if view <= 0 {
		return // everything fits: nothing to scroll
	}
	r := nav[i]
	switch {
	case i == 0:
		p.scroll = 0
	case r.top < p.scroll:
		p.scroll = r.top
	case r.last >= p.scroll+view:
		p.scroll = r.last - view + 1
	}
	p.scroll = min(max(p.scroll, 0), p.memo.scrollMax)
}

// cursorToPane puts the cursor on the session running in the tmux pane id,
// scrolled into view; a pane no row shows leaves it where it is.
func (p *panel) cursorToPane(id string) {
	if p.snap == nil || id == "" {
		return
	}
	for _, s := range p.snap.Sessions {
		if s.Tmux != nil && s.Tmux.Pane == id {
			p.cursor = panelSessionKey(s)
			p.moveCursor(0)
			return
		}
	}
}

// paneCameFrom is the pane the user left to focus this sidebar: in the
// sidebar's window, the one tmux marks as last active, else the only other
// pane there.
type paneCameFrom struct{ pane string }

// cameFrom asks tmux which pane that was (the user, 2026-10-06: "by default
// on navigation we should position and highlight the current pane").
func (m paneModel) cameFrom() tea.Cmd {
	own := m.opts.Pane
	if own == "" {
		return nil
	}
	return func() tea.Msg {
		out, err := tmuxCapture("list-panes", "-t", own, "-F", "#{pane_id} #{pane_last}")
		if err != nil {
			return nil
		}
		return paneCameFrom{pane: pickCameFrom(out, own)}
	}
}

// pickCameFrom reads list-panes' "<id> <last>" lines: the last active pane
// other than own, else the only other pane, else "".
func pickCameFrom(out, own string) string {
	var others []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || f[0] == own || !paneIDPattern.MatchString(f[0]) {
			continue
		}
		if f[1] == "1" {
			return f[0]
		}
		others = append(others, f[0])
	}
	if len(others) == 1 {
		return others[0]
	}
	return ""
}

// cursorSession is the highlighted session, if there is one.
func (p *panel) cursorSession() (observer.Session, bool) {
	if p.memo == nil {
		return observer.Session{}, false
	}
	p.lines()
	i := p.cursorIndex(p.memo.nav)
	if i < 0 {
		return observer.Session{}, false
	}
	return p.memo.nav[i].s, true
}

// jump switches the tmux client this sidebar is shown in to the highlighted
// session's pane. A background job with no pane opens in a popup instead
// (openBackground). Any other session with no pane (not in tmux, or a
// reference not in the documented shape) is not a place to go; nothing
// happens.
func (m paneModel) jump() (tea.Model, tea.Cmd) {
	s, ok := m.panel.cursorSession()
	if !ok {
		return m, nil
	}
	if s.Tmux == nil || !paneIDPattern.MatchString(s.Tmux.Pane) {
		if s.Kind == observer.KindBackground {
			return m, openBackground(s)
		}
		return m, nil
	}
	target := s.Tmux.Pane
	return m, func() tea.Msg {
		_ = tmuxSet("switch-client", "-t", target)
		return nil
	}
}

// cursorLine paints one line of the highlighted row. The row's first line
// (head) is the selection bar: every item on it — status, runtime, name, ctx —
// drops its own colours and takes the active tab's, bold, the panel's width,
// with a ❯ in its gutter (the user, 2026-10-03: "the selected line should
// affect the colour of all items … time and status as well"). The row's
// other lines sit on the cursor fill, a tint of the same accent: the fill is
// restored after every reset inside the line, and the dimmest greys, which
// would sink into it, are lifted to slate.
func (p panel) cursorLine(line string, head bool) string {
	if head {
		return p.selectionBar(line)
	}
	pad := strings.Repeat(" ", max(p.width-ansi.StringWidth(line), 0))
	bg := sgrOf(lipgloss.NewStyle().Background(panelPal.view.cursorBg))
	if bg == "" {
		return line + pad // the same cells, without the fill
	}
	slate := sgrOf(lipgloss.NewStyle().Foreground(panelPal.slate))
	for _, dim := range []lipgloss.AdaptiveColor{panelPal.ash, panelPal.view.latest} {
		if seq := sgrOf(lipgloss.NewStyle().Foreground(dim)); seq != "" {
			line = strings.ReplaceAll(line, seq, slate)
		}
	}
	s := strings.ReplaceAll(line, "\x1b[0m", "\x1b[0m"+bg)
	s = strings.ReplaceAll(s, "\x1b[49m", "\x1b[49m"+bg)
	return bg + s + pad + "\x1b[0m"
}

// selectionBar is the highlighted row's first line as the selection bar: its
// text, the gutter turned into ❯, in the active tab's colours across the
// panel's width.
func (p panel) selectionBar(line string) string {
	text := ansi.Strip(line)
	if _, n := utf8.DecodeRuneInString(text); n > 0 {
		text = "❯" + text[n:]
	}
	text += strings.Repeat(" ", max(p.width-ansi.StringWidth(text), 0))
	v := panelPal.view
	return lipgloss.NewStyle().Foreground(v.tabOnFg).Background(v.tabOnBg).Bold(true).Render(text)
}

// sgrOf is the SGR that opens style in the current colour profile and theme,
// "" when the profile has no colour.
func sgrOf(style lipgloss.Style) string {
	painted := style.Render(" ")
	if i := strings.Index(painted, " "); i > 0 && strings.HasPrefix(painted, "\x1b[") {
		return painted[:i]
	}
	return ""
}

// popupKey opens the highlighted session in a popup over the window, where
// enter switches to it (the user, 2026-10-22: "switching to that session is
// good, we need another keystroke to pop it up … i want space").
const popupKey = " "

// windowIDPattern is a tmux window id, @N.
var windowIDPattern = regexp.MustCompile(`^@[0-9]+$`)

// popup is popupKey's action. A background job opens with claude attach
// (openBackground); a session in a pane opens as a peek at that pane
// (peekPane); anything else has nothing to show and says so.
func (m paneModel) popup() (tea.Model, tea.Cmd) {
	s, ok := m.panel.cursorSession()
	if !ok {
		return m, nil
	}
	if s.Kind == observer.KindBackground {
		return m, openBackground(s)
	}
	if s.Tmux == nil || !paneIDPattern.MatchString(s.Tmux.Pane) {
		return m, func() tea.Msg { return paneNoteMsg{note: "no tmux pane to pop up"} }
	}
	return m, peekPane(s)
}

// peekPane opens a popup holding a second view of the session's pane: a
// throwaway tmux session grouped with the pane's own (same windows, its own
// current window), so looking does not move any other client off its window.
// Like enter, it makes the pane the active one in its window.
func peekPane(s observer.Session) tea.Cmd {
	pane := s.Tmux.Pane
	then := ""
	if windowIDPattern.MatchString(s.Tmux.Window) {
		then = ` \; select-window -t ` + popupSession + ":" + s.Tmux.Window
	}
	then += ` \; select-pane -t ` + pane
	return showPopup(s, popupShell("new-session -d -s "+popupSession+" -t "+pane, false, then))
}

// paneNoteMsg puts a line in the footer: what an action could not do.
type paneNoteMsg struct{ note string }

// shortIDPattern is a job's daemonShort: what `claude attach` takes, and
// nothing that could be read as a flag or a second word.
var shortIDPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// backgroundRefusal is why a background job cannot be opened, or "" when it
// can. The checks are the ones `claude attach` would fail on, said in the
// sidebar's words before the user is sent into a popup that only errors (the
// user, 2026-10-21: a job whose worktree was deleted opened to "Workspace not
// trusted").
func backgroundRefusal(s observer.Session, stat func(string) (os.FileInfo, error)) string {
	name := sanitizeText(s.Name)
	if name == "" {
		name = "this job"
	}
	switch {
	case s.Live != nil && !*s.Live && s.LiveReason == observer.LiveReasonJobTranscriptGone:
		return name + ": its conversation is gone, nothing to resume"
	case s.Live != nil && !*s.Live:
		return name + " has ended"
	case s.ShortID == nil || !shortIDPattern.MatchString(*s.ShortID):
		return name + ": no job id to attach to"
	case s.CWD == "":
		return name + ": no folder recorded"
	}
	if fi, err := stat(s.CWD); err != nil || !fi.IsDir() {
		return name + ": its folder is gone (" + sanitizeText(s.CWD) + ")"
	}
	return ""
}

// openBackground opens a background job in a popup over this window, in
// the job's folder: `claude attach`, inside a throwaway tmux session
// (popupShell) so prefix Space closes it without a keystroke reaching Claude.
// Ctrl+Z, which returns claude attach to the shell, closes it too. The job
// keeps running either way.
func openBackground(s observer.Session) tea.Cmd {
	if why := backgroundRefusal(s, os.Stat); why != "" {
		return func() tea.Msg { return paneNoteMsg{note: why} }
	}
	create := "new-session -d -s " + popupSession + " -c " + shellQuote(s.CWD) +
		" " + shellQuote("claude attach "+*s.ShortID)
	return showPopup(s, popupShell(create, true, ""))
}

// popupSession names a popup's throwaway session after the popup shell's
// pid, which the shell expands: unique per popup. tmux.conf's prefix Space
// detaches a client whose session is named this way, which closes the popup.
const popupSession = "tt-popup-$$"

// popupShell is the popup's shell command: create the session DETACHED, run
// then, attach, and only then make it die with its last client.
//
// noSidebar turns the sidebar off for a session with a window of its own
// (claude attach): its hooks fire on attach, after the scope is off, where a
// session created attached would grow a sidebar. Never for a grouped peek:
// its windows are the real ones, and the sidebar's hooks reading "off" there
// would take the real sidebars down.
//
// The tmux it runs is this process's Target, spelled out (moox §4.5): `TMUX=`
// still stops tmux refusing to nest, and `-S <socket>` lands the throwaway
// session on THIS server rather than on whatever the popup shell's
// environment would pick — the default server, once TMUX is emptied.
func popupShell(create string, noSidebar bool, then string) string {
	t := ttTarget()
	cmd := `TMUX= ` + shellQuote(t.Bin) + ` -u -S ` + shellQuote(t.Socket) + ` ` + create
	if noSidebar {
		cmd += ` \; set-option -t ` + popupSession + ` @tt_obs_scope off`
	}
	return cmd + then +
		` \; attach-session -t ` + popupSession +
		` \; set-option -t ` + popupSession + ` destroy-unattached on`
}

// showPopup runs shell in a popup titled with the session's name and the key
// that closes it.
func showPopup(s observer.Session, shell string) tea.Cmd {
	title := " " + sanitizeText(s.Name) + " · prefix space closes "
	args := []string{"display-popup", "-E", "-w", "90%", "-h", "90%", "-T", title, shell}
	return func() tea.Msg {
		if err := tmuxSet(args...); err != nil {
			return paneNoteMsg{note: "could not open it: " + err.Error()}
		}
		return nil
	}
}

// shellQuote quotes s as one sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
