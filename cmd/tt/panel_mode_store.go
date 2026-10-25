package main

import (
	"github.com/MadAppGang/moox/internal/statedir"
)

// panelModeFile is where the chosen grouping is kept, in tt's state directory
// (~/.local/state/tt, honouring XDG_STATE_HOME): one word, "project", "status",
// "tmux" or "colour".
//
// ONE file for every surface. Pressing the key in any sidebar changes the
// mode of every sidebar and of the TUI's panel, each picking it up on its next
// observer event or render tick — within a second while anything is running,
// a minute at most when all is idle. The user asked for one mode, not one per
// window. And it is written the moment it changes rather than on exit: a
// sidebar is killed, not quit, whenever its window closes or a layout is
// re-applied.
//
// Plain text rather than JSON because the wire package owns the module's only
// JSON shapes (§5.1), and a word needs no envelope.
const panelModeFile = "panel-mode"

// loadPanelMode and savePanelMode are seams: tests must neither read nor
// write the developer's real choice.
var (
	loadPanelMode = func() (panelMode, bool) { return readPanelMode(ttStateDir(), panelClaudeRoot()) }
	savePanelMode = func(m panelMode) error { return writePanelMode(ttStateDir(), panelClaudeRoot(), m) }
)

// readPanelMode reads the saved mode. False when there is none, or it cannot
// be read, or it is not a mode this build knows — every one of which means
// "use the default", so they are not told apart.
func readPanelMode(stateDir, claudeRoot string) (panelMode, bool) {
	d, err := statedir.OpenExisting(stateDir, claudeRoot)
	if err != nil {
		return defaultPanelMode, false
	}
	defer func() { _ = d.Close() }()
	data, _, err := d.ReadFile(panelModeFile)
	if err != nil {
		return defaultPanelMode, false
	}
	return parsePanelMode(string(data))
}

// writePanelMode saves the mode, atomically: statedir writes a temp file and
// renames it into place in the same verified directory, and refuses a
// symlink at the file's name.
func writePanelMode(stateDir, claudeRoot string, m panelMode) error {
	d, err := statedir.Open(stateDir, claudeRoot)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.WriteFile(panelModeFile, []byte(m.String()+"\n"), 0o600)
}

// panelClaudeRoot is the Claude tree statedir must never write inside, or ""
// when it cannot be located — which statedir refuses (ErrNoClaudeRoot, moox
// §2.4), so an unlocatable tree costs the saved mode, never the refusal.
func panelClaudeRoot() string {
	paths, err := claudePaths()
	if err != nil {
		return ""
	}
	return paths.ClaudeRoot
}

// cycleMode moves the panel to the next mode and saves it. A save that fails
// still changes this panel — the key did something — and says why the choice
// will not outlive it.
func (p *panel) cycleMode() {
	p.chooseMode(p.mode.next())
}

// chooseMode moves the panel straight to a mode and saves it, as a sidebar tab
// hotkey does; the save is followed by every other open surface like a cycle.
func (p *panel) chooseMode(m panelMode) {
	p.setMode(m)
	if err := savePanelMode(p.mode); err != nil {
		p.note = "grouping not saved: " + err.Error()
	}
}

// setMode changes the grouping. The collapse state is keyed on the mode, so
// nothing a previous mode decided is carried into this one.
func (p *panel) setMode(m panelMode) {
	if m != p.mode {
		p.scroll = 0 // another grouping is another list: start at its top
	}
	p.mode = m
}

// followSavedMode adopts a mode another surface saved. It runs on every
// observer event and render tick, which is how every open sidebar follows one
// key press without a timer of its own.
func (p *panel) followSavedMode() {
	if m, ok := loadPanelMode(); ok && m != p.mode {
		p.setMode(m)
	}
}
