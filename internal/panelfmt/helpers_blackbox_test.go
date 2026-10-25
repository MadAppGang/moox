// Black-box tests for internal/panelfmt, written from the architecture
// contract (§2 grammar, §7.1 API) only. They use the exported surface of the
// package and nothing else, so they live in the external test package.
//
// Global render state: lipgloss's colour profile is process-global. Every test
// that renders pins it through withProfile and restores it in Cleanup; none of
// these tests call t.Parallel.
package panelfmt_test

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// defaultConf is the built-in panel.conf of contract §6.6, verbatim.
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

// testRegistry mirrors the §3 token table (token-specific options only; the
// common options are implicit) plus the neutral tokens a, b, c, w, p used by
// the §2.3 worked cases.
func testRegistry() panelfmt.Registry {
	flag := panelfmt.OptSpec{Kind: panelfmt.OptFlag}
	wrap := panelfmt.OptSpec{Kind: panelfmt.OptInt, Min: 1, Max: 2}
	flags := func(names ...string) panelfmt.TokenSpec {
		m := map[string]panelfmt.OptSpec{}
		for _, n := range names {
			m[n] = flag
		}
		return panelfmt.TokenSpec{Options: m}
	}
	return panelfmt.Registry{
		"a": {Options: map[string]panelfmt.OptSpec{
			"short": flag,
			"lvl":   {Kind: panelfmt.OptInt, Min: 0, Max: 9},
			"label": {Kind: panelfmt.OptString},
		}},
		"b": flags(), "c": flags(), "w": flags(), "p": flags(),
		"gutter":  {Options: map[string]panelfmt.OptSpec{"char": {Kind: panelfmt.OptString}}},
		"status":  flags("chip", "quiet", "word", "noage"),
		"age":     flags("short"),
		"name":    flags("colour", "color", "tint", "bold"),
		"group":   flags(),
		"project": flags(),
		"tmux":    flags(),
		"pane":    flags("full"),
		"colour":  flags(),
		"color":   flags(),
		"ctx":     flags("hide"),
		"tokens":  flags(),
		"model":   flags("short"),
		"compact": flags(),
		"wait":    flags(),
		"needs":   {Options: map[string]panelfmt.OptSpec{"wrap": wrap}},
		"why":     {Options: map[string]panelfmt.OptSpec{"wrap": wrap}},
		"sub":     flags("n"),
		"apierr":  flags("n"),
		"live":    flags(),
		"kind":    flags(),
		"id":      flags(),
	}
}

func plainOpts() panelfmt.Options {
	return panelfmt.Options{LiteralStyle: lipgloss.NewStyle(), DimStyle: lipgloss.NewStyle(), ErrorStyle: lipgloss.NewStyle()}
}

// withProfile pins lipgloss's global colour profile for one test.
func withProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func defaults(t *testing.T) *panelfmt.Set {
	t.Helper()
	return defaultsWith(t, plainOpts())
}

func defaultsWith(t *testing.T, opts panelfmt.Options) *panelfmt.Set {
	t.Helper()
	d := panelfmt.Defaults([]byte(defaultConf), testRegistry(), opts)
	if d == nil {
		t.Fatal("Defaults returned nil for the §6.6 built-in")
	}
	return d
}

func parse(t *testing.T, src string) *panelfmt.Set {
	t.Helper()
	return parseWith(t, src, plainOpts())
}

func parseWith(t *testing.T, src string, opts panelfmt.Options) *panelfmt.Set {
	t.Helper()
	s := panelfmt.ParseConfig([]byte(src), testRegistry(), opts, defaultsWith(t, opts))
	if s == nil {
		t.Fatalf("ParseConfig returned nil (contract: never nil) for %q", src)
	}
	return s
}

// clean parses src and fails the test on any problem.
func clean(t *testing.T, src string) *panelfmt.Set {
	t.Helper()
	s := parse(t, src)
	if len(s.Problems) != 0 {
		t.Fatalf("unexpected problems for %q: %+v", src, s.Problems)
	}
	return s
}

func hasFatal(s *panelfmt.Set) bool {
	for _, p := range s.Problems {
		if p.Fatal {
			return true
		}
	}
	return false
}

func softOnly(s *panelfmt.Set) bool {
	if len(s.Problems) == 0 {
		return false
	}
	return !hasFatal(s)
}

// vals is a Resolver built from plain strings: "" or a missing name is EMPTY.
type vals map[string]string

func (v vals) resolver() panelfmt.Resolver {
	return func(tok panelfmt.Token) panelfmt.Segment {
		s, ok := v[tok.Name]
		if !ok || s == "" {
			return panelfmt.Segment{Empty: true}
		}
		return panelfmt.Segment{Parts: []panelfmt.Part{{Text: s}}}
	}
}

// segs is a Resolver returning prepared segments; a missing name is EMPTY.
type segs map[string]panelfmt.Segment

func (m segs) resolver() panelfmt.Resolver {
	return func(tok panelfmt.Token) panelfmt.Segment {
		s, ok := m[tok.Name]
		if !ok {
			return panelfmt.Segment{Empty: true}
		}
		return s
	}
}

func text(s string) panelfmt.Segment {
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: s}}}
}

func shrinkable(s string, rank, min int) panelfmt.Segment {
	seg := text(s)
	seg.Shrink, seg.Min = rank, min
	return seg
}

// rowTemplate parses `row = '<tpl>'` and requires it to be clean.
func rowTemplate(t *testing.T, tpl string) *panelfmt.Template {
	t.Helper()
	s := clean(t, "row = '"+tpl+"'\n")
	if s.Row == nil {
		t.Fatal("Row is nil")
	}
	return s.Row
}

func line(t *testing.T, tpl *panelfmt.Template, w int, r panelfmt.Resolver) (string, bool) {
	t.Helper()
	out, ok := tpl.Line(w, r)
	if got := ansi.StringWidth(out); got > w {
		t.Errorf("line %q is %d columns, wider than W=%d", ansi.Strip(out), got, w)
	}
	return out, ok
}

func plain(s string) string { return ansi.Strip(s) }

func width(s string) int { return ansi.StringWidth(s) }

// ---- a small SGR reader: which colours paint each visible rune ----

type rgb struct{ r, g, b int }

type cell struct {
	ch     rune
	fg, bg *rgb
}

// cells walks a styled line and returns every visible rune with the
// truecolor foreground/background in force when it was drawn.
func cells(s string) []cell {
	var fg, bg *rgb
	var out []cell
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j >= len(s) {
					break
				}
				if s[j] == 'm' {
					fg, bg = applySGR(s[i+2:j], fg, bg)
				}
				i = j + 1
				continue
			}
			i++ // other escapes are not expected from lipgloss; skip the ESC
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		out = append(out, cell{ch: r, fg: fg, bg: bg})
		i += n
	}
	return out
}

func applySGR(params string, fg, bg *rgb) (*rgb, *rgb) {
	if params == "" {
		return nil, nil
	}
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		n, _ := strconv.Atoi(ps[k])
		switch {
		case n == 0:
			fg, bg = nil, nil
		case n == 39:
			fg = nil
		case n == 49:
			bg = nil
		case n == 38 || n == 48:
			if k+4 < len(ps) && ps[k+1] == "2" {
				c := &rgb{atoi(ps[k+2]), atoi(ps[k+3]), atoi(ps[k+4])}
				if n == 38 {
					fg = c
				} else {
					bg = c
				}
				k += 4
			} else if k+2 < len(ps) && ps[k+1] == "5" {
				k += 2
			}
		}
	}
	return fg, bg
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// cellOf returns the first cell drawing rune ch.
func cellOf(t *testing.T, cs []cell, ch rune) cell {
	t.Helper()
	for _, c := range cs {
		if c.ch == ch {
			return c
		}
	}
	t.Fatalf("rune %q not drawn", ch)
	return cell{}
}

func sgrOf(hex string) string {
	c, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil {
		panic(err)
	}
	return strconv.Itoa(int(c>>16&0xff)) + ";" + strconv.Itoa(int(c>>8&0xff)) + ";" + strconv.Itoa(int(c&0xff))
}

func sameRGB(c *rgb, hex string) bool {
	if c == nil {
		return false
	}
	return strconv.Itoa(c.r)+";"+strconv.Itoa(c.g)+";"+strconv.Itoa(c.b) == sgrOf(hex)
}
