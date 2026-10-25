package panelfmt

import (
	"os"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// TestMain pins the colour profile: plain text by default, so most tests
// compare what is drawn; the style tests switch to truecolor and back.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

// withTrueColor runs f with SGR output on. Not parallel: the profile is global.
func withTrueColor(t *testing.T, f func()) {
	t.Helper()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	f()
}

var (
	int12  = OptSpec{Kind: OptInt, Min: 1, Max: 2}
	strOpt = OptSpec{Kind: OptString}
	flag   = OptSpec{Kind: OptFlag}
)

// testReg mirrors the shape of the panel's registry (§3): enough tokens for
// the default panel.conf, plus the single letters the table tests use.
var testReg = Registry{
	"a": {}, "b": {}, "c": {}, "d": {}, "w": {}, "p": {},
	"gutter": {Options: map[string]OptSpec{"char": strOpt}},
	"status": {Options: map[string]OptSpec{"chip": flag, "quiet": flag, "word": flag, "noage": flag}},
	"age":    {Options: map[string]OptSpec{"short": flag}},
	"name":   {Options: map[string]OptSpec{"colour": flag, "color": flag, "tint": flag, "bold": flag}},
	"group":  {}, "project": {}, "tmux": {},
	"pane":   {Options: map[string]OptSpec{"full": flag}},
	"colour": {}, "color": {},
	"ctx":     {Options: map[string]OptSpec{"plain": flag, "hide": flag}},
	"tokens":  {},
	"model":   {Options: map[string]OptSpec{"short": flag}},
	"compact": {}, "wait": {},
	"needs":  {Options: map[string]OptSpec{"wrap": int12}},
	"why":    {Options: map[string]OptSpec{"wrap": int12}},
	"sub":    {Options: map[string]OptSpec{"n": flag}},
	"apierr": {Options: map[string]OptSpec{"n": flag}},
	"live":   {}, "kind": {}, "id": {},
}

// defaultConf is the §6.6 built-in text, as the panel will embed it.
const defaultConf = `# ~/.config/tt/panel.conf — how each row of the tt session panel is drawn.
# Edits apply on the panel's next update; ` + "`tt panel-render --check`" + ` lints this file.
# {token[:option[:option]]}   {{ and }} are literal braces   {>} is the spring (fills the gap)
# A token with nothing to say vanishes together with the separator before it.

row       = "{gutter}{status:chip} {name:colour} · {group}{>}{ctx}"
meta      = "    {wait} · {pane} · {sub} · {apierr} · {live} · {needs}"

attn_row  = "{gutter}{status:chip} {name:colour}{>}{ctx}"
attn_why  = "    {why:wrap=2}"
attn_meta = "    {group} · {kind} {id} · {pane} · {model:short} · {sub} · {apierr} · {live}"

# Context badge colour: green below ctx_warn, honey up to ctx_alert, red above it.
ctx_warn  = 200k
ctx_alert = 400k
`

// seg is a one-part text segment in the default style.
func seg(text string) Segment { return Segment{Parts: []Part{{Text: text}}} }

// vals is a Resolver over fixed segments; a missing name is Empty.
func vals(m map[string]Segment) Resolver {
	return func(tk Token) Segment {
		s, ok := m[tk.Name]
		if !ok {
			return Segment{Empty: true}
		}
		return s
	}
}

// texts is vals for plain strings; "" means Empty.
func texts(m map[string]string) Resolver {
	sm := make(map[string]Segment, len(m))
	for k, v := range m {
		if v != "" {
			sm[k] = seg(v)
		}
	}
	return vals(sm)
}

// identity maps a template's byte offsets to 1-based columns, as if the
// template started the line.
func identity(off int) int { return off + 1 }

// mustTpl parses a template (as key) and fails the test on any problem.
func mustTpl(t *testing.T, key, src string) *Template {
	t.Helper()
	tp, probs := parseTemplate(src, identity, 1, key, testReg)
	if len(probs) > 0 || tp == nil {
		t.Fatalf("parse %q: %v", src, probs)
	}
	return tp
}

// width is the display width of a painted line.
func width(s string) int { return ansi.StringWidth(s) }
