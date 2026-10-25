package main

// pane_paint.go draws the sidebar itself, as soon as something changes and
// only the lines that changed (the user, 2026-10-06: "we should optimise the
// redrawing … redraw only the things which changed").
//
// bubbletea v1's renderer already writes only changed lines, but it paints on
// a ticker, never on demand: a key waits for the next tick (250 ms at the 4 fps
// the sidebar ran at), and every tick, idle or not, copies and compares the
// whole frame. One sidebar per window pays that around the clock. So the pane
// program runs without bubbletea's renderer, and paintedPane paints after each
// Update instead:
//
//   - a frame equal to the last costs one comparison per line and writes
//     nothing; nothing wakes when nothing happens;
//   - a changed frame writes only its changed lines, each placed with an
//     absolute cursor move, in one write;
//   - a resize repaints everything on a cleared screen.
//
// Without the renderer, bubbletea's terminal modes are no-ops too, so the
// painter owns them: the alternate screen and a hidden cursor while it runs,
// focus reporting, and the mouse, which it follows from paneModel.mouse.

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// The terminal modes the painter sets and restores.
const (
	paintEnter       = "\x1b[?1049h\x1b[?25l\x1b[?1004h" // alternate screen, hide the cursor, report focus
	paintLeave       = "\x1b[?1004l\x1b[?25h\x1b[?1049l"
	paintMouseOn     = "\x1b[?1002h\x1b[?1006h" // cell-motion tracking, SGR coordinates
	paintMouseOff    = "\x1b[?1002l\x1b[?1006l"
	paintClearScreen = "\x1b[2J"
)

// framePainter writes frames to the terminal, line by line against the last.
type framePainter struct {
	out           io.Writer
	width, height int
	last          []string
	mouse         bool
}

// paint draws view, writing only the lines that differ from the last frame.
// It reports whether anything was written.
func (fp *framePainter) paint(view string) bool {
	lines := strings.Split(view, "\n")
	if fp.height > 0 && len(lines) > fp.height {
		lines = lines[:fp.height]
	}
	var buf bytes.Buffer
	for i, l := range lines {
		if i < len(fp.last) && fp.last[i] == l {
			continue
		}
		if fp.width > 0 {
			l = ansi.Truncate(l, fp.width, "")
		}
		buf.WriteString(ansi.CursorPosition(1, i+1))
		buf.WriteString(l)
		buf.WriteString("\x1b[0m")
		if fp.width <= 0 || ansi.StringWidth(l) < fp.width {
			buf.WriteString(ansi.EraseLineRight)
		}
	}
	if len(fp.last) > len(lines) {
		buf.WriteString(ansi.CursorPosition(1, len(lines)+1))
		buf.WriteString(ansi.EraseScreenBelow)
	}
	fp.last = lines
	if buf.Len() == 0 {
		return false
	}
	_, _ = fp.out.Write(buf.Bytes())
	return true
}

// resize starts the next frame on a cleared screen.
func (fp *framePainter) resize(width, height int) {
	fp.width, fp.height, fp.last = width, height, nil
	_, _ = io.WriteString(fp.out, paintClearScreen)
}

// setMouse turns mouse reporting on or off, when that changes.
func (fp *framePainter) setMouse(on bool) {
	if on == fp.mouse {
		return
	}
	fp.mouse = on
	if on {
		_, _ = io.WriteString(fp.out, paintMouseOn)
	} else {
		_, _ = io.WriteString(fp.out, paintMouseOff)
	}
}

// enter and leave set up the terminal for the painter and put it back.
func (fp *framePainter) enter() { _, _ = io.WriteString(fp.out, paintEnter) }

func (fp *framePainter) leave() {
	fp.setMouse(false)
	_, _ = io.WriteString(fp.out, paintLeave)
}

// paintTerminal is the terminal set-up bubbletea skips when it runs without a
// renderer: raw input (else keys would wait for enter and echo), and the
// pane's size, sent at start and on every SIGWINCH. It returns the undo.
func paintTerminal(ctx context.Context, p *tea.Program, in, out any) func() {
	restore := func() {}
	if f, ok := in.(term.File); ok && term.IsTerminal(f.Fd()) {
		if state, err := term.MakeRaw(f.Fd()); err == nil {
			restore = func() { _ = term.Restore(f.Fd(), state) }
		}
	}
	f, ok := out.(term.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return restore
	}
	sendSize := func() {
		if w, h, err := term.GetSize(f.Fd()); err == nil {
			p.Send(tea.WindowSizeMsg{Width: w, Height: h})
		}
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(winch)
		sendSize()
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				sendSize()
			}
		}
	}()
	return restore
}

// paintedPane is a paneModel whose frames the painter draws: every Update is
// followed by a paint, and View is never asked for (the program runs without a
// renderer).
type paintedPane struct {
	pane    paneModel
	painter *framePainter
}

func (m paintedPane) Init() tea.Cmd { return m.pane.Init() }

func (m paintedPane) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.painter.resize(size.Width, size.Height)
	}
	next, cmd := m.pane.Update(msg)
	m.pane = next.(paneModel)
	m.painter.setMouse(m.pane.mouse)
	m.painter.paint(m.pane.View())
	return m, cmd
}

func (m paintedPane) View() string { return "" }
