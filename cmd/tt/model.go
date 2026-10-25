package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/MadAppGang/moox/internal/observer"
)

// mode is the modal state of the TUI. In modeList the session list is shown and
// the custom keybindings are active. The other modes overlay a huh form.
type mode int

const (
	modeList mode = iota
	modeNew
	modeRename
	modeConfirmDelete
)

const formKey = "value"

// The action keys are ctrl-chords rather than plain letters because every
// printable key now belongs to the search box (see typeAhead). ctrl+a is
// deliberately unused: it is the tmux prefix, so it never reaches this program
// when tt runs inside tmux — which is most of the time.
const (
	keyNew    = "ctrl+t" // "new tab", the one chord every terminal user already knows
	keyRename = "ctrl+r"
	keyKill   = "ctrl+x"

	// keyRestore rebuilds the workspace from the tmux-resurrect snapshot. It is
	// live ONLY on the empty state (see canRestore), so it cannot fire over
	// running sessions no matter how it is mistyped.
	//
	// "s" for snapshot. resurrect's own chord for restore is prefix+C-r, which
	// is taken here by rename — and its C-s means SAVE, not restore. The
	// collision is only nominal: those bindings need the tmux prefix, and ^a
	// never reaches this program.
	keyRestore = "ctrl+s"
)

// listTitle is the header, shared by the list and by the empty state so the two
// views can't drift apart.
const listTitle = "tmux sessions"

// The styles are built from the adaptive palette in theme.go, so each one
// resolves to its light or dark variant when it renders. Note that Faint(true)
// is deliberately avoided for text that must stay readable: faint on a light
// background washes out to near-invisible, so those styles name a color.
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorTitle).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(colorMeta).
			Padding(0, 1)

	// statusStyle renders the transient error/rejection footer in a distinct
	// red so a failed action reads as a failure, not as more help text.
	statusStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Padding(0, 1)

	emptyStyle = lipgloss.NewStyle().
			Foreground(colorMeta).
			Padding(1, 2)

	// The two lines under the empty state. They carry emptyStyle's horizontal
	// indent but not its vertical padding, so they stack directly beneath it
	// instead of each opening a blank line of their own.
	emptyMetaStyle = lipgloss.NewStyle().
			Foreground(colorMeta).
			Padding(0, 2)

	// emptyHintStyle puts the restore offer on the accent color: it is the one
	// thing you can actually do from an empty list, so it should read as an
	// action rather than as more grey prose.
	emptyHintStyle = lipgloss.NewStyle().
			Foreground(colorAccent).
			Padding(0, 2)

	// Chip styling for list rows: the session NAME is the bright, prominent
	// label; the window COUNT is the faint, secondary meta — like a chip where
	// the label pops and the meta recedes. The selected row swaps to the accent
	// color and is marked with a leading bar so it's unmistakable even in a
	// monochrome screen capture.
	normalNameStyle = lipgloss.NewStyle().
			Foreground(colorName)

	selectedNameStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorAccent)

	metaStyle = lipgloss.NewStyle().
			Foreground(colorMeta)

	// selectedBar is the leading indicator drawn in front of the highlighted
	// row; normalGutter keeps unselected rows aligned with it. It is rendered
	// lazily (not at package init) because the theme is not resolved until
	// main() runs — baking the string in at init would freeze the wrong color.
	normalGutter = "  "
)

// maxRowWidth is how far right the badges go on a wide terminal.
const maxRowWidth = 64

// badgeColours is one badge's text and fill. The badges borrow the panel's
// chips so the list and the sidebar speak one colour language: "local" is the
// RUN chip's green, "remote" the IDL chip's blue, the window count the group
// title's count chip.
type badgeColours struct{ fg, bg lipgloss.AdaptiveColor }

var (
	localBadge   = badgeColours{panelPal.view.runFg, panelPal.view.runBg}
	remoteBadge  = badgeColours{panelPal.view.idleFg, panelPal.view.idleBg}
	windowsBadge = badgeColours{colorMeta, panelPal.view.countBg}
)

// shownBadges say where a session is on screen right now: "local" for windows
// on this machine, "remote" for terminals elsewhere, with ×N when there is more
// than one. Nothing attached, no badges.
func shownBadges(on shownOn, filled bool) []string {
	var badges []string
	if on.here > 0 {
		badges = append(badges, badge(localBadge, counted("local", on.here), filled))
	}
	if on.remote > 0 {
		badges = append(badges, badge(remoteBadge, counted("remote", on.remote), filled))
	}
	return badges
}

func counted(label string, n int) string {
	if n > 1 {
		return label + " ×" + strconv.Itoa(n)
	}
	return label
}

// badge draws a label outlined — coloured text in brackets — or, on the
// selected row, filled. Both are the label plus two cells, so a row's badges
// keep their columns as the selection moves over it.
func badge(c badgeColours, label string, filled bool) string {
	if filled {
		return lipgloss.NewStyle().Foreground(c.fg).Background(c.bg).Padding(0, 1).Render(label)
	}
	return lipgloss.NewStyle().Foreground(c.fg).Render("[" + label + "]")
}

func selectedBar() string {
	return lipgloss.NewStyle().Foreground(colorAccent).Render("▌ ")
}

// compactDelegate renders each session on a single line: the name on the
// left, and on the right the where-it-is-shown badges then the window count, right-aligned so the window count lines up down the list. It embeds list.DefaultDelegate so it
// inherits Height (1 when ShowDescription is false), Spacing, Update, and the
// help funcs — only the visual Render is overridden.
type compactDelegate struct {
	list.DefaultDelegate
}

func newCompactDelegate() compactDelegate {
	d := list.NewDefaultDelegate()
	d.ShowDescription = false // single-line rows
	d.SetSpacing(0)           // no blank line between rows
	return compactDelegate{DefaultDelegate: d}
}

// Render draws one session row. Each piece is rendered with its own style and
// concatenated, which keeps each style's ANSI reset self-contained (the
// supported way to multi-tone a single line — see the Charm list delegate docs).
// The badges are pushed to the row's right edge, capped at maxRowWidth so a
// full-screen terminal doesn't strand them a hundred columns from the name; on
// a row too narrow for that they follow the name after two spaces instead.
func (d compactDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	s, ok := item.(session)
	if !ok {
		return
	}

	selected := index == m.Index()

	name := normalNameStyle.Render(s.name)
	gutter := normalGutter
	if selected {
		name = selectedNameStyle.Render(s.name)
		gutter = selectedBar()
	}

	right := strings.Join(append(shownBadges(s.shown, selected), badge(windowsBadge, s.Description(), selected)), " ")
	left := gutter + name
	gap := max(2, min(m.Width(), maxRowWidth)-lipgloss.Width(left)-lipgloss.Width(right))

	fmt.Fprintf(w, "%s%s%s", left, strings.Repeat(" ", gap), right) //nolint:errcheck
}

// model is the root bubbletea model.
type model struct {
	list   list.Model
	mode   mode
	form   *huh.Form
	help   help.Model
	width  int
	height int

	// renameTarget holds the original name during a rename so we can issue the
	// rename against the correct session even if the highlight moves.
	renameTarget string

	// deleteTarget holds the name to kill while the confirm dialog is open.
	deleteTarget string

	// selected is set to the session name to attach to once the program exits.
	selected string

	// status holds a short, human-readable message about the last failed (or
	// rejected) action — a tmux error, or a duplicate-name rejection. It is
	// rendered as a footer in modeList and cleared on the next key that enters
	// the list update path, so it never lingers past the user's next move.
	status string

	// snap is the tmux-resurrect snapshot found on disk, read at startup and
	// again on every refresh. It is what the empty state offers to bring back;
	// a zero snapshot means there is nothing to offer.
	snap snapshot

	// restoring is true while the restore runs. restore.sh is a blocking script
	// that takes seconds on a real workspace, so it runs as a tea.Cmd and the
	// view shows a spinner rather than a frozen screen.
	restoring bool
	spinner   spinner.Model

	// panel is S4's Claude-session panel (§5.5a, panel.go). It is CLOSED here
	// and it subscribes to nothing until ctrl+o opens it, so a bare `tt` costs
	// exactly what it always did.
	panel panel
}

// restoreDoneMsg carries the outcome of the restore back into the model. The
// error is the whole payload: what came back is read off tmux by refresh().
type restoreDoneMsg struct{ err error }

// restoreCmd runs the restore off the UI goroutine. Bubble Tea runs a cmd in its
// own goroutine, which is the only reason a multi-second blocking script is
// acceptable here.
func restoreCmd() tea.Cmd {
	return func() tea.Msg { return restoreDoneMsg{err: restoreSnapshot()} }
}

func newModel() model {
	delegate := newCompactDelegate()

	l := list.New(toItems(sortSessions(listSessions())), delegate, 0, 0)
	l.Title = listTitle
	l.Styles.Title = titleStyle
	l.SetShowHelp(true)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)

	// Every plain-letter binding the list ships with has to go, because those
	// letters are now search characters: they can never reach the list, so
	// leaving them bound would only put keys in the help line that do nothing
	// ("↑/k up" when k actually types a "k"). Each one is rebound to the
	// non-letter equivalent it already had, plus the emacs/fzf ctrl-chords for
	// the cursor, which is what most people reach for inside a search box.
	l.KeyMap.CursorUp = key.NewBinding(
		key.WithKeys("up", "ctrl+p"), key.WithHelp("↑/^p", "up"))
	l.KeyMap.CursorDown = key.NewBinding(
		key.WithKeys("down", "ctrl+n"), key.WithHelp("↓/^n", "down"))
	l.KeyMap.PrevPage = key.NewBinding(
		key.WithKeys("pgup"), key.WithHelp("pgup", "prev page"))
	l.KeyMap.NextPage = key.NewBinding(
		key.WithKeys("pgdown"), key.WithHelp("pgdn", "next page"))
	l.KeyMap.GoToStart = key.NewBinding(
		key.WithKeys("home"), key.WithHelp("home", "first"))
	l.KeyMap.GoToEnd = key.NewBinding(
		key.WithKeys("end"), key.WithHelp("end", "last"))

	// Same problem, but it can't be rebound: the list binds "q"/"esc" to quit,
	// and "q" is now a character you type. Quitting moves to updateList.
	// DisableQuitKeybindings is the supported way to drop them —
	// Quit.SetEnabled(false) would be undone the next time the list recomputes
	// its keybindings.
	l.DisableQuitKeybindings()

	// Render the help ourselves (see keyHelp). The list hides its own keys
	// while the search box is focused, which is now exactly backwards: the
	// verbs act on the highlighted match and are at their most useful mid-search.
	l.SetShowHelp(false)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colorAccent)

	return model{
		list:    l,
		mode:    modeList,
		help:    help.New(),
		spinner: sp,
		snap:    loadSnapshot(),
		// A nil source means "decide at open", which is §5.5(a)'s rule: the
		// data source is chosen once when the panel opens, never probed in a
		// loop, and never wired at startup where it would cost a bare `tt` a
		// watch it may never use.
		panel: newPanel(nil, observer.SystemClock{}),
	}
}

// canRestore reports whether the restore offer applies. Both halves are
// load-bearing: there must be nothing in the list to attach to, and there must
// be a snapshot worth bringing back. The first is what keeps a mistyped ^s from
// ever rebuilding a workspace over live work — the offer is simply unreachable
// while any session exists.
func (m model) canRestore() bool {
	return len(m.list.Items()) == 0 && !m.snap.empty()
}

// helpState names which footer the current view needs. The states genuinely
// advertise different keys — esc clears a search but quits an unfiltered list,
// and an empty list has no row to rename or kill — so they get their own sets
// rather than one list with dead entries in it.
type helpState int

const (
	helpBrowsing helpState = iota
	helpSearching
	helpEmptyRestorable
	helpRestoring
)

// keyHelp returns the footer keys for the given state.
//
// It is authored here rather than assembled from the list's own keymap because
// the list's enabled-flags no longer describe what works: it disables its
// cursor keys while the search box is focused (updateList drives the cursor by
// hand instead), and it hides any extra keys during a search — which is where
// the verbs are most useful now that they act on the highlighted match.
func keyHelp(state helpState) []key.Binding {
	switch state {
	case helpRestoring:
		// The restore is an external process. Quitting tt lets go of the TUI;
		// it does not stop the restore, and the footer must not imply it does.
		return []key.Binding{
			key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("^c", "leave it running")),
		}
	case helpEmptyRestorable:
		// No row exists, so rename/kill/cursor keys are all dead here. Only the
		// three things that do something are listed.
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter", keyRestore), key.WithHelp("enter/^s", "restore")),
			key.NewBinding(key.WithKeys(keyNew), key.WithHelp("^t", "new")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "quit")),
		}
	}

	keys := []key.Binding{
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "attach")),
		key.NewBinding(key.WithKeys(keyNew), key.WithHelp("^t", "new")),
		key.NewBinding(key.WithKeys(keyRename), key.WithHelp("^r", "rename")),
		key.NewBinding(key.WithKeys(keyKill), key.WithHelp("^x", "kill")),
		key.NewBinding(key.WithKeys(keyPanel), key.WithHelp("^o", "claude")),
		key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑/^p", "up")),
		key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓/^n", "down")),
	}

	if state == helpSearching {
		return append(keys, key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear")))
	}
	return append(keys,
		// Not a real binding — the only honest way to advertise "every key you
		// press searches" in a list of key/description pairs.
		key.NewBinding(key.WithKeys("type"), key.WithHelp("type", "search")),
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "quit")),
	)
}

func toItems(sessions []session) []list.Item {
	items := make([]list.Item, len(sessions))
	for i, s := range sessions {
		items[i] = s
	}
	return items
}

// sortSessions orders sessions most-recently-attached first (MRU), in place,
// and returns the same slice for call-site convenience. It lives in the model
// layer rather than queryTmuxSessions so it stays testable through the
// listSessions seam and works regardless of where the sessions came from.
//
// The keys, in order:
//   - lastAttached DESCENDING — the session you touched most recently floats up.
//   - created DESCENDING as the tiebreak — notably, every never-attached session
//     ties at lastAttached 0, so newest-created wins among them.
//   - name ASCENDING as the final tiebreak — keeps the order deterministic.
//
// session_activity is deliberately NOT a key: it bumps on any background pane
// output (a chatty Claude session, a tailing log) and would wrongly float
// unattended sessions to the top.
func sortSessions(sessions []session) []session {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if a.lastAttached != b.lastAttached {
			return a.lastAttached > b.lastAttached
		}
		if a.created != b.created {
			return a.created > b.created
		}
		return a.name < b.name
	})
	return sessions
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// The terminal switched light/dark (theme_live.go): the next frame is
	// drawn in the new theme, and there is nothing else to do.
	if followTheme(msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.help.Width = msg.Width
		// Reserve the last row for the footer (help, or a status message) so
		// the view is exactly the terminal height and nothing scrolls the
		// title off the top. layoutPanel does that and takes the panel's
		// column off the list's width — zero columns while it is closed, so
		// this is the same call it always was.
		m.layoutPanel()
		// Keep an open form sized too, so it doesn't render collapsed.
		if m.form != nil {
			m.form = m.form.WithWidth(msg.Width).WithHeight(msg.Height)
		}
		return m, nil

	case restoreDoneMsg:
		m.restoring = false
		if msg.err != nil {
			m.status = "restore failed: " + msg.err.Error()
			return m, nil
		}
		// refresh() re-queries tmux, so what lands in the list is what the
		// restore actually produced — not what the snapshot promised.
		return m.refresh(), nil

	case spinner.TickMsg:
		// The spinner reschedules itself forever once started. Dropping the tick
		// when nothing is running is what stops that loop after the restore
		// returns.
		if !m.restoring {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case panelEventMsg, panelErrMsg, panelTickMsg, panelRefreshedMsg:
		// The panel owns its own messages, including the decision to re-issue
		// the tick and the channel read. A message from a closed panel returns
		// no command, which is what ends both chains (panel.go: live/gen).
		return m, m.panel.update(msg)
	}

	if m.mode != modeList {
		return m.updateForm(msg)
	}
	return m.updateList(msg)
}

// updateList handles input while the session list is active.
//
// The key scheme is fzf-shaped rather than vim-shaped: plain printable keys go
// straight into the search box, so every verb is a ctrl-chord and every verb
// works in BOTH states (browsing and searching). That symmetry is the point —
// you type a few letters, then act on the match without first "accepting" the
// search.
func (m model) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	ks := k.String()

	// While a restore is running the list is inert. Swallowing every key is what
	// stops a second ^s from starting a concurrent restore, or an enter from
	// attaching to a server that is still being built. ctrl+c is the exception
	// and is honest about what it does: the restore is a separate process, so
	// letting go of the TUI leaves it to finish on its own.
	if m.restoring {
		if ks == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	}

	// Clear any stale status message on the user's next keystroke. This sits
	// above the verb switch — most verbs return early (quit, open form), so
	// clearing here is the only spot that fires for every key and keeps an
	// error from lingering past one move.
	m.status = ""

	searching := m.list.FilterState() == list.Filtering

	switch ks {
	case "ctrl+c":
		return m, tea.Quit

	case "esc":
		// esc backs out one level at a time: it drops an active search first
		// (the list handles that below) and only quits when there is nothing
		// left to back out of.
		if m.list.FilterState() != list.Unfiltered {
			break
		}
		return m, tea.Quit

	case "enter":
		// Attach to whatever is highlighted, searching or not. Handling enter
		// here rather than letting the list have it is deliberate: the list
		// would treat enter as "accept the filter" and stop, costing a second
		// enter to actually get into the session.
		if s, ok := m.list.SelectedItem().(session); ok {
			m.selected = s.name
			return m, tea.Quit
		}
		// No match under the cursor. On the empty state with a snapshot on disk
		// that is not "nothing to do" — bringing the workspace back is the only
		// action there is, so enter does it. Reaching this branch with sessions
		// present (a search matching nothing) leaves enter inert as before,
		// because canRestore requires an empty list.
		if m.canRestore() {
			return m.startRestore()
		}
		return m, nil

	case "up", "down", "ctrl+p", "ctrl+n":
		// Drive the cursor ourselves. The list disables its own cursor
		// keybindings while the search input has focus, so without this the
		// highlight would be stuck on the first match the moment you typed —
		// and the vim keys it normally offers (j/k) are now search characters.
		if ks == "up" || ks == "ctrl+p" {
			m.list.CursorUp()
		} else {
			m.list.CursorDown()
		}
		return m, nil

	case keyNew:
		m.mode = modeNew
		// Seed the prompt with the search text: searching for a session that
		// doesn't exist yet is exactly the moment you want to create it, so
		// the name you already typed carries over instead of being retyped.
		m.form = m.newNameForm("New session name", m.searchText())
		return m, m.form.Init()

	case keyRename:
		if s, ok := m.list.SelectedItem().(session); ok {
			m.mode = modeRename
			m.renameTarget = s.name
			m.form = m.newNameForm("Rename session", s.name)
			return m, m.form.Init()
		}
		return m, nil

	case keyRestore:
		if m.canRestore() {
			return m.startRestore()
		}
		// Deliberately silent when there is nothing to restore: ^s is not
		// advertised outside the empty state, so a stray press is a no-op rather
		// than an error the user has to dismiss.
		return m, nil

	case keyPanel:
		// S4's toggle (§5.5a). Opening subscribes and starts the render tick;
		// closing cancels the context both of those select on.
		cmd := m.panel.toggle()
		m.layoutPanel()
		return m, cmd

	case keyPanelModeTUI:
		// Cycle the panel's grouping: the sidebar's `g`, as a chord for the
		// same reason as ctrl+d above. Saved, so every sidebar follows.
		if m.panel.open {
			m.panel.cycleMode()
		}
		return m, nil

	case keyKill:
		if s, ok := m.list.SelectedItem().(session); ok {
			m.mode = modeConfirmDelete
			m.deleteTarget = s.name
			m.form = m.confirmForm("Kill session " + s.name + "?")
			return m, m.form.Init()
		}
		return m, nil

	default:
		// Type-to-search. Opening the search box here and then falling through
		// lets the very keystroke that opened it land in the input, so the
		// first character isn't swallowed.
		if !searching && typeAhead(k) {
			m.list.SetFilterState(list.Filtering)
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// startRestore kicks off the restore and hands back the two commands that make
// it visible: the spinner's first tick, and the restore itself.
func (m model) startRestore() (tea.Model, tea.Cmd) {
	m.restoring = true
	m.status = ""
	return m, tea.Batch(m.spinner.Tick, restoreCmd())
}

// typeAhead reports whether a keystroke should open the search box. Only a bare
// printable rune qualifies:
//
//   - modified keys (ctrl/alt chords) are verbs, and arrive with a different
//     Type or with Alt set;
//   - "/" is left alone so the list's own filter binding still handles it,
//     which keeps the documented "/" habit working and opens an empty search.
//     It still types normally once the box is open, since this only decides
//     whether to OPEN it;
//   - a leading space would start a search that matches everything, so it is
//     not a reason to open the box — once it is open, spaces type normally.
func typeAhead(k tea.KeyMsg) bool {
	if k.Type != tea.KeyRunes || k.Alt || len(k.Runes) == 0 {
		return false
	}
	r := k.Runes[0]
	return r != '/' && unicode.IsPrint(r) && !unicode.IsSpace(r)
}

// searchText returns what the user has typed into the search box, or "" when no
// search is active. It is what seeds the new-session prompt.
func (m model) searchText() string {
	if m.list.FilterState() == list.Unfiltered {
		return ""
	}
	return strings.TrimSpace(m.list.FilterValue())
}

// updateForm routes input to the active huh form and reacts to its state.
func (m model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	// esc cancels the prompt and returns to the list without changes. huh does
	// not bind esc to abort by default, so we handle it here explicitly.
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyEsc {
		return m.resetToList(), nil
	}

	fm, cmd := m.form.Update(msg)
	if f, ok := fm.(*huh.Form); ok {
		m.form = f
	}

	switch m.form.State {
	case huh.StateCompleted:
		return m.applyForm()
	case huh.StateAborted:
		// Form aborted internally (e.g. ctrl+c handled by huh): drop back to the
		// list without making changes.
		return m.resetToList(), nil
	}
	return m, cmd
}

// applyForm runs the tmux command for the just-completed form, then refreshes
// the list and returns to list mode. A rejected name (collision) or a failed
// tmux mutation sets m.status, which survives refresh() (a value receiver that
// never touches the field) and is shown on the next list View.
//
// A successful create is the one case that does NOT return to the list: naming
// a session is asking to be in it, so it hands the name to main's attach path
// and quits instead of making you find the new row and press enter.
func (m model) applyForm() (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeNew:
		if name := m.form.GetString(formKey); name != "" {
			if msg := m.validateName(name, ""); msg != "" {
				m.status = msg
				break
			}
			if err := newSession(name); err != nil {
				m.status = "create failed: " + err.Error()
				break
			}
			m.selected = name
			// resetToList before quitting so the form isn't left on screen for
			// the final render on the way out.
			return m.resetToList(), tea.Quit
		}
	case modeRename:
		if name := m.form.GetString(formKey); name != "" && name != m.renameTarget {
			// Exclude the rename target from the collision check: renaming a
			// session to a different new name must not trip over its own old one.
			if msg := m.validateName(name, m.renameTarget); msg != "" {
				m.status = msg
				break
			}
			if err := renameSession(m.renameTarget, name); err != nil {
				m.status = "rename failed: " + err.Error()
			}
		}
	case modeConfirmDelete:
		if m.form.GetBool(formKey) {
			if err := killSession(m.deleteTarget); err != nil {
				m.status = "delete failed: " + err.Error()
			}
		}
	}
	return m.refresh(), nil
}

// validateName checks a proposed session name against the sessions currently in
// the list and returns a human-readable rejection message, or "" if the name is
// acceptable. excluding names a session to ignore in the comparison (the rename
// target, so renaming around it doesn't self-collide). This is a pure function
// of the list contents, so it's unit-testable without driving the huh form.
func (m model) validateName(name, excluding string) string {
	for _, it := range m.list.Items() {
		s, ok := it.(session)
		if !ok {
			continue
		}
		if s.name == excluding {
			continue
		}
		if s.name == name {
			return "session " + name + " already exists"
		}
	}
	return ""
}

// refresh re-queries tmux and returns to list mode. It clears any active
// filter: SetItems re-applies the previous filter against the new item set, and
// after a delete/rename that stale filter can collapse VisibleItems() to zero
// (the "No items." bug) even though sessions remain. Resetting shows the full,
// current list.
func (m model) refresh() model {
	m.list.SetItems(toItems(sortSessions(listSessions())))
	m.list.ResetFilter()
	// Re-read the snapshot too. After a restore that brought nothing back, the
	// empty state has to describe what is on disk NOW — and after killing the
	// last session it has to know whether there is anything to offer at all.
	m.snap = loadSnapshot()
	return m.resetToList()
}

func (m model) resetToList() model {
	m.mode = modeList
	m.form = nil
	m.renameTarget = ""
	m.deleteTarget = ""
	return m
}

func (m model) newNameForm(title, initial string) *huh.Form {
	input := huh.NewInput().
		Key(formKey).
		Title(title).
		Value(&initial)

	f := huh.NewForm(huh.NewGroup(input)).WithShowHelp(true)
	if m.width > 0 {
		f = f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

func (m model) confirmForm(title string) *huh.Form {
	confirm := huh.NewConfirm().
		Key(formKey).
		Title(title).
		Affirmative("Yes").
		Negative("No")

	f := huh.NewForm(huh.NewGroup(confirm)).WithShowHelp(true)
	if m.width > 0 {
		f = f.WithWidth(m.width).WithHeight(m.height)
	}
	return f
}

// layoutPanel re-splits the terminal between the list and the panel. With the
// panel closed the panel takes zero columns and this is exactly the SetSize
// call model.go always made, which is what keeps model_test.go's View
// assertions byte-identical.
//
// The panel is a full-height column beside the body (the list plus its footer
// row, m.height lines together), so it gets every row of the terminal and
// renders no more than that (panel.go: view). The help footer gets the BODY's
// width, not the terminal's (code review round 2, A9): at the terminal's width
// its line is wider than the list, and JoinHorizontal places the panel after
// the widest body line — measured, the panel's title landed at column 110 of
// 140, and past the edge of a 100-column terminal entirely.
func (m *model) layoutPanel() {
	cols := 0
	if m.panel.open {
		cols = panelColumns(m.width)
	}
	m.panel.width = cols
	m.panel.height = max(0, m.height)
	m.help.Width = max(0, m.width-cols)
	m.list.SetSize(max(0, m.width-cols), max(0, m.height-1))
}

// panelColumns is how many columns the panel may take out of a terminal that
// wide. Under 60 columns it takes none: a 46-column panel beside a 14-column
// list is two unusable surfaces instead of one working one.
func panelColumns(total int) int {
	if total < 60 {
		return 0
	}
	if total < 2*panelWidth {
		return total / 2
	}
	return panelWidth
}

// View renders the list (or a form, or the empty state) and, when the panel is
// open, the panel beside it. A closed panel renders "" and this returns the
// body unchanged — byte for byte what it returned before P9.
func (m model) View() string {
	body := m.viewBody()
	pv := m.panel.view()
	if pv == "" {
		return body
	}
	// Each column is held to the region layoutPanel gave it. The body is cut
	// to its columns and padded out to them, so the panel starts exactly where
	// the list ends whatever the widest body line is — the help footer, a
	// status message, a form — rather than wherever JoinHorizontal would have
	// put it (A9). The cut leaves one blank column as a gutter: the help is
	// the body's only full-width line, and bubbles' help does not always
	// truncate itself (it adds the next item anyway when no "…" fits), so
	// without it the footer ran straight into the panel ("search •+20 more").
	cols := max(0, m.width-m.panel.width)
	body = lipgloss.PlaceHorizontal(cols, lipgloss.Left, lipgloss.NewStyle().MaxWidth(max(0, cols-1)).Render(body))
	return lipgloss.JoinHorizontal(lipgloss.Top, body, pv)
}

func (m model) viewBody() string {
	if m.mode != modeList && m.form != nil {
		return m.form.View()
	}

	if m.restoring {
		return titleStyle.Render(listTitle) + "\n\n" +
			emptyStyle.Render(m.spinner.View()+"restoring "+m.snap.contents()+"…") + "\n" +
			m.footer()
	}

	if len(m.list.Items()) == 0 {
		return titleStyle.Render(listTitle) + "\n\n" + m.emptyBody() + "\n" + m.footer()
	}

	return m.list.View() + "\n" + m.footer()
}

// emptyBody renders the empty state. With a snapshot on disk it stops being a
// dead end and becomes an offer: what is there to bring back (the count and the
// age are what tell you whether it is the snapshot you wanted), then what enter
// will actually do. The hint says what happens rather than repeating the footer's
// key list, because "start tmux" is the part the footer has no room to say.
//
// time.Now() enters here rather than inside summary() so the formatting stays a
// pure function a test can pin.
func (m model) emptyBody() string {
	if m.snap.empty() {
		return emptyStyle.Render("no tmux sessions")
	}
	return emptyStyle.Render("no tmux sessions") + "\n" +
		emptyMetaStyle.Render("last snapshot: "+m.snap.summary(time.Now())) + "\n" +
		emptyHintStyle.Render("press enter to start tmux and bring it back")
}

// footer renders the single bottom row, shared by both View paths: the
// transient status message when there is one, otherwise the help.
//
// Sharing one row is deliberate. It keeps the view exactly the terminal height
// whether or not an error is showing, and for the single keystroke a status
// survives, the error is worth more than the help text it displaces.
func (m model) footer() string {
	if m.status != "" {
		return statusStyle.Render(m.status)
	}
	keys := keyHelp(m.helpState())
	if m.panel.open {
		// The panel's own chords, listed only while there is a panel for
		// them to act on — both do nothing when it is closed — and right
		// after ^o, since the help line is cut to the list's width and the
		// list narrows when the panel opens.
		for i, b := range keys {
			if len(b.Keys()) > 0 && b.Keys()[0] == keyPanel {
				panelKeys := []key.Binding{
					key.NewBinding(key.WithKeys(keyPanelModeTUI), key.WithHelp("^g", "group")),
				}
				keys = append(keys[:i+1], append(panelKeys, keys[i+1:]...)...)
				break
			}
		}
	}
	return footerStyle.Render(m.help.ShortHelpView(keys))
}

// helpState picks the footer for the current view. The order matters: a restore
// in flight overrides everything, and the restore offer outranks the browsing
// footer because reaching it means the list is empty and those keys are dead.
func (m model) helpState() helpState {
	switch {
	case m.restoring:
		return helpRestoring
	case m.canRestore():
		return helpEmptyRestorable
	case m.list.FilterState() == list.Filtering:
		return helpSearching
	default:
		return helpBrowsing
	}
}
