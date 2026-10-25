package main

// theme_live.go follows the terminal's light/dark switch while tt runs (the
// user, 2026-10-01: "add an automatic theme listener to switch light/dark").
//
// resolveTheme (theme.go) decides the first frame: TERM_THEME when it states a
// theme, else the terminal's answer to OSC 11. After that, tt asks the terminal
// to REPORT changes — DEC private mode 2031, the colour-scheme update
// notification that the dotfiles' zsh probe and Claude Code already use — and
// asks once for the current scheme (CSI ?996n). The terminal answers each
// with CSI ?997;1n (dark) or CSI ?997;2n (light). Inside tmux (3.7c, measured
// for the zsh probe) tmux relays the attached client's theme to every pane
// that has 2031 on, so a sidebar follows the window it sits in.
//
// A report flips lipgloss's background decision. Every colour tt draws is an
// AdaptiveColor resolved at render time (theme.go, theme_panel.go), so the next
// frame is in the new theme; the panel's frame cache is keyed on the theme too
// (panelFrameKey.dark). A live report beats TERM_THEME: the variable is a
// snapshot of the moment tt was launched, the report is the terminal now.
//
// Bubbletea v1.3.10 has no message for these reports: it delivers an
// unrecognised CSI sequence to Update as its unexported unknownCSISequenceMsg,
// a byte slice. themeReport recognises that type by name and reads its bytes.
// A bubbletea that ever names the type differently simply stops delivering
// reports here — tt keeps its first-frame theme, which is today's behaviour —
// and TestTheBubbleteaInputParserDeliversAThemeReport says so at once.

import (
	"bytes"
	"io"
	"reflect"

	"github.com/charmbracelet/lipgloss"
)

// The escape sequences (DEC private mode 2031 and its query, 996).
const (
	themeReportsOn  = "\x1b[?2031h"
	themeReportsOff = "\x1b[?2031l"
	themeQuery      = "\x1b[?996n"
)

// The two reports, as bubbletea hands them over: the CSI sequence whole.
var (
	themeReportDark  = []byte("\x1b[?997;1n")
	themeReportLight = []byte("\x1b[?997;2n")
)

// bubbleteaCSIType names the message type bubbletea v1 delivers an
// unrecognised CSI sequence as.
const (
	bubbleteaPkgPath = "github.com/charmbracelet/bubbletea"
	bubbleteaCSIType = "unknownCSISequenceMsg"
)

// startThemeReports turns the terminal's theme reports on and asks for the
// current theme. It is written before bubbletea starts, next to resolveTheme;
// the answers arrive later through bubbletea's input as messages.
func startThemeReports(w io.Writer) {
	_, _ = io.WriteString(w, themeReportsOn+themeQuery)
}

// stopThemeReports turns them off again, so the next program in this pane —
// the shell, whose own probe manages 2031 around each prompt — does not
// receive reports it did not ask for.
func stopThemeReports(w io.Writer) {
	_, _ = io.WriteString(w, themeReportsOff)
}

// themeReport reports whether msg is a terminal theme report, and which
// theme it states.
func themeReport(msg any) (dark bool, ok bool) {
	t := reflect.TypeOf(msg)
	if t == nil || t.PkgPath() != bubbleteaPkgPath || t.Name() != bubbleteaCSIType || t.Kind() != reflect.Slice ||
		t.Elem().Kind() != reflect.Uint8 {
		return false, false
	}
	b := reflect.ValueOf(msg).Bytes()
	switch {
	case bytes.Equal(b, themeReportDark):
		return true, true
	case bytes.Equal(b, themeReportLight):
		return false, true
	}
	return false, false
}

// followTheme applies msg when it is a theme report, and reports whether it
// was one: the caller then has nothing else to do with it.
func followTheme(msg any) bool {
	dark, ok := themeReport(msg)
	if !ok {
		return false
	}
	if lipgloss.HasDarkBackground() != dark {
		lipgloss.SetHasDarkBackground(dark)
	}
	return true
}
