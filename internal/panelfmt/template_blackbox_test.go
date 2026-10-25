package panelfmt_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// REQ-8: the Resolver receives the token as written: name, parsed options,
// raw text, known flag and its line in panel.conf.
func TestBlackboxTokenCarriesParsedOptions(t *testing.T) {
	withProfile(t, termenv.Ascii)
	s := clean(t, "# first line\nrow = '{a:lvl3:short:label=x y} {b}'\nmeta = '{a:lvl=0}'\nattn_meta = '{a:lvl=7}'\n")
	var seen []panelfmt.Token
	r := func(tok panelfmt.Token) panelfmt.Segment {
		seen = append(seen, tok)
		return text("X")
	}
	first := func(label string) panelfmt.Token {
		t.Helper()
		for _, tok := range seen {
			if tok.Name == "a" {
				return tok
			}
		}
		t.Fatalf("%s: resolver never received token a (got %+v)", label, seen)
		return panelfmt.Token{}
	}

	s.Row.Line(80, r)
	tok := first("row")
	want := []panelfmt.Opt{
		{Name: "lvl", N: 3, HasN: true},
		{Name: "short"},
		{Name: "label", Value: "x y"},
	}
	if !reflect.DeepEqual(tok.Opts, want) {
		t.Errorf("Opts = %+v; want %+v", tok.Opts, want)
	}
	if tok.Raw != "a:lvl3:short:label=x y" {
		t.Errorf("Raw = %q; want %q", tok.Raw, "a:lvl3:short:label=x y")
	}
	if !tok.Known {
		t.Error("Known = false for a registered token with registered options")
	}
	if tok.Line != 2 {
		t.Errorf("Line = %d; want 2", tok.Line)
	}
	if tok.Col < 1 {
		t.Errorf("Col = %d; want a 1-based column", tok.Col)
	}

	seen = nil
	s.Meta.Line(80, r)
	if got := first("meta").Opts; !reflect.DeepEqual(got, []panelfmt.Opt{{Name: "lvl", Value: "0", N: 0, HasN: true}}) {
		t.Errorf("explicit zero: Opts = %+v; want HasN with N=0", got)
	}
	seen = nil
	s.AttnMeta.Line(80, r)
	if got := first("attn_meta").Opts; !reflect.DeepEqual(got, []panelfmt.Opt{{Name: "lvl", Value: "7", N: 7, HasN: true}}) {
		t.Errorf("OptInt =value: Opts = %+v; want N=7 HasN Value=7", got)
	}
}

// REQ-9: doubled braces are literal braces.
func TestBlackboxDoubledBracesAreLiterals(t *testing.T) {
	withProfile(t, termenv.Ascii)
	r := vals{"a": "A", "b": "B"}.resolver()
	for _, tc := range []struct{ tpl, want string }{
		{"{{{a}}}", "{A}"},
		{"{a}{{>}}{b}", "A{>}B"},
		{"}}{{", "}{"},
	} {
		got, _ := line(t, rowTemplate(t, tc.tpl), 20, r)
		if plain(got) != tc.want {
			t.Errorf("%s renders %q; want %q (and no spring padding)", tc.tpl, plain(got), tc.want)
		}
	}
}

// REQ-10: the spring makes the line exactly W; without it the line is not padded.
func TestBlackboxSpringFillsTheLine(t *testing.T) {
	withProfile(t, termenv.Ascii)
	r := vals{"a": "A", "b": "B"}.resolver()
	for _, w := range []int{3, 10, 20, 46, 120} {
		got, ok := line(t, rowTemplate(t, "{a}{>}{b}"), w, r)
		if !ok || width(got) != w {
			t.Errorf("W=%d: spring line is %d columns (ok=%v); want exactly %d", w, width(got), ok, w)
		}
		if p := plain(got); p != "A"+strings.Repeat(" ", w-2)+"B" {
			t.Errorf("W=%d: spring line %q", w, p)
		}
	}
	got, _ := line(t, rowTemplate(t, "{a} {b}"), 20, r)
	if plain(got) != "A B" {
		t.Errorf("no spring: %q; want %q (lines are not padded)", plain(got), "A B")
	}
}

// REQ-11: an unknown token or option is soft, kept, and drawn in place.
func TestBlackboxUnknownTokenOrOptionIsVisible(t *testing.T) {
	withProfile(t, termenv.Ascii)
	for _, tc := range []struct {
		tpl, want, never string
	}{
		{"{a} {foo}", "A {foo?}", "foo"},
		{"{a:pct} {b}", "{a:pct?} B", "a"},
		{"{a:lvl=12} {b}", "{a:lvl=12?} B", "a"},     // out of range 0..9
		{"{a:short=x} {b}", "{a:short=x?} B", "a"},   // a flag given a value
		{"{b} {ctx:bar12}", "B {ctx:bar12?}", "ctx"}, // removed by R2
		{"{b} {window}", "B {window?}", "window"},    // removed by R2
	} {
		t.Run(tc.tpl, func(t *testing.T) {
			s := parse(t, "row = '"+tc.tpl+"'\n")
			if !softOnly(s) {
				t.Fatalf("problems %+v; want soft only (the template is kept)", s.Problems)
			}
			called := map[string]bool{}
			r := func(tok panelfmt.Token) panelfmt.Segment {
				called[tok.Name] = true
				return vals{"a": "A", "b": "B", "ctx": "48k"}.resolver()(tok)
			}
			got, ok := line(t, s.Row, 80, r)
			if !ok || plain(got) != tc.want {
				t.Errorf("renders %q (ok=%v); want %q", plain(got), ok, tc.want)
			}
			if called[tc.never] {
				t.Errorf("the Resolver was called for the unknown token %q", tc.never)
			}
			if p := s.Problems[0]; p.Line != 1 || p.Col < 1 {
				t.Errorf("problem %+v; want line 1 with a column", p)
			}
		})
	}
}

// REQ-11: common options are implicit on every token and are not problems.
func TestBlackboxCommonOptionsAreKnownEverywhere(t *testing.T) {
	clean(t, "row = '{a:plain} {b:dim} {c:min=2:max=9:shrink=1:drop=1} {name:colour:min=12}'\n")
}

// REQ-11: the in-place error is drawn in ErrorStyle.
func TestBlackboxUnknownTokenUsesErrorStyle(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	opts := plainOpts()
	opts.ErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D5E"))
	s := parseWith(t, "row = '{b} {foo}'\n", opts)
	got, _ := s.Row.Line(80, vals{"b": "B"}.resolver())
	cs := cells(got)
	if c := cellOf(t, cs, 'f'); !sameRGB(c.fg, "#FF4D5E") {
		t.Errorf("{foo?} foreground %+v; want ErrorStyle #FF4D5E (raw %q)", c.fg, got)
	}
	if c := cellOf(t, cs, 'B'); sameRGB(c.fg, "#FF4D5E") {
		t.Error("a known token is drawn in ErrorStyle")
	}
}

// REQ-12: the §2.3 worked cases and the leading/trailing/gap rules.
func TestBlackboxEmptyTokensCollapseWithTheirSeparator(t *testing.T) {
	withProfile(t, termenv.Ascii)
	for _, tc := range []struct {
		name, tpl string
		v         vals
		want      string
	}{
		{"a,∅,c", "{a} · {b} · {c}", vals{"a": "a", "c": "c"}, "a · c"},
		{"∅,b,c", "{a} · {b} · {c}", vals{"b": "b", "c": "c"}, "b · c"},
		{"a,∅,∅", "{a} · {b} · {c}", vals{"a": "a"}, "a"},
		{"a,b,c", "{a} · {b} · {c}", vals{"a": "a", "b": "b", "c": "c"}, "a · b · c"},
		{"non-whitespace gap preferred", "{group} · {kind} {id}", vals{"group": "authflow", "id": "be9663df"}, "authflow · be9663df"},
		{"all present", "{group} · {kind} {id}", vals{"group": "authflow", "kind": "bg", "id": "be9663df"}, "authflow · bg be9663df"},
		{"gap and trailing dropped", "{wait} ({pane})", vals{"wait": "input-needed"}, "input-needed"},
		{"gap and trailing kept", "{wait} ({pane})", vals{"wait": "input-needed", "pane": "%9"}, "input-needed (%9)"},
		{"first non-whitespace skipped gap", "{a}, {b}; {c}", vals{"a": "a", "c": "c"}, "a, c"},
		{"whitespace gap loses to a real one", "{a} {b} · {c}", vals{"a": "a", "c": "c"}, "a · c"},
		{"all gaps whitespace: the one before the next token", "{a} {b}  {c}", vals{"a": "a", "c": "c"}, "a  c"},
		{"leading literal drawn when a token is", ">> {a} · {b}", vals{"b": "b"}, ">> b"},
		{"leading literal dropped when none is", ">> {a} · {b}", vals{}, ""},
		{"trailing literal needs its token", "{a} · {b}!", vals{"a": "a"}, "a"},
		{"trailing literal kept", "{a} · {b}!", vals{"a": "a", "b": "b"}, "a · b!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := line(t, rowTemplate(t, tc.tpl), 80, tc.v.resolver())
			if tc.want == "" {
				if ok {
					t.Errorf("an all-empty line was drawn: %q", plain(got))
				}
				return
			}
			if !ok || plain(got) != tc.want {
				t.Errorf("%s with %v renders %q (ok=%v); want %q", tc.tpl, tc.v, plain(got), ok, tc.want)
			}
		})
	}
}

// REQ-12, #18: the spring splits the template; each part collapses on its own.
func TestBlackboxSpringPartsCollapseIndependently(t *testing.T) {
	withProfile(t, termenv.Ascii)
	got, ok := line(t, rowTemplate(t, "{name} · {group}{>}{ctx}"), 20, vals{"name": "n", "ctx": "48k"}.resolver())
	if want := "n" + strings.Repeat(" ", 16) + "48k"; !ok || plain(got) != want {
		t.Errorf("renders %q; want %q (no dangling ' · ')", plain(got), want)
	}
	got, ok = line(t, rowTemplate(t, "<{a}>{>}{b}"), 10, vals{"b": "x"}.resolver())
	if p := plain(got); !ok || strings.ContainsAny(p, "<>") || strings.TrimSpace(p) != "x" || !strings.HasSuffix(p, "x") {
		t.Errorf("left part with no token drew its literals: %q (ok=%v)", p, ok)
	}
}

// REQ-12: a line whose every token is EMPTY contributes no line at all.
func TestBlackboxAllEmptyLineIsNotDrawn(t *testing.T) {
	withProfile(t, termenv.Ascii)
	for _, tpl := range []string{"    {w} · {p}", "{a}{>}{b}", "  ({a})  "} {
		got, ok := rowTemplate(t, tpl).Line(46, vals{}.resolver())
		if ok {
			t.Errorf("%q with every token empty returned a line: %q", tpl, got)
		}
		if strings.TrimSpace(plain(got)) != "" {
			t.Errorf("%q: an EMPTY line still carries text %q", tpl, plain(got))
		}
	}
}

// REQ-13 (R7): literals are drawn in LiteralStyle, tokens in their own style.
func TestBlackboxLiteralsUseLiteralStyle(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	opts := plainOpts()
	opts.LiteralStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#123456"))
	s := parseWith(t, "row = '{a} · {b}'\n", opts)
	got, _ := s.Row.Line(80, vals{"a": "A", "b": "B"}.resolver())
	cs := cells(got)
	if c := cellOf(t, cs, '·'); !sameRGB(c.fg, "#123456") {
		t.Errorf("literal '·' fg %+v; want LiteralStyle #123456 (raw %q)", c.fg, got)
	}
	for _, ch := range []rune{'A', 'B'} {
		if c := cellOf(t, cs, ch); sameRGB(c.fg, "#123456") {
			t.Errorf("token %q is drawn in LiteralStyle", ch)
		}
	}
}

// REQ-14: plain and dim are applied by the engine, not the token function.
func TestBlackboxEngineAppliesPlainAndDim(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	opts := plainOpts()
	opts.DimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#010203"))
	styled := segs{"a": {Parts: []panelfmt.Part{{Text: "A", Style: lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FF0000")).Background(lipgloss.Color("#0000FF"))}}}}
	render := func(tpl string) cell {
		t.Helper()
		s := parseWith(t, "row = '"+tpl+"'\n", opts)
		if len(s.Problems) != 0 {
			t.Fatalf("%s: problems %+v", tpl, s.Problems)
		}
		got, _ := s.Row.Line(40, styled.resolver())
		return cellOf(t, cells(got), 'A')
	}
	if c := render("{a}"); !sameRGB(c.fg, "#FF0000") || !sameRGB(c.bg, "#0000FF") {
		t.Fatalf("control: the resolver's style is not drawn (fg %+v bg %+v)", c.fg, c.bg)
	}
	if c := render("{a:plain}"); c.fg != nil || c.bg != nil {
		t.Errorf("plain: fg %+v bg %+v; want neither", c.fg, c.bg)
	}
	if c := render("{a:dim}"); !sameRGB(c.fg, "#010203") || !sameRGB(c.bg, "#0000FF") {
		t.Errorf("dim: fg %+v bg %+v; want DimStyle fg #010203 and the background kept", c.fg, c.bg)
	}
}

// REQ-14: min=, max=, shrink= and drop= override the segment's fit fields.
func TestBlackboxEngineAppliesFitOptions(t *testing.T) {
	withProfile(t, termenv.Ascii)
	r := segs{"a": text("abcdefghij"), "b": text("XY")}.resolver()
	for _, tc := range []struct {
		name, tpl string
		w         int
		want      string
	}{
		{"control: rigid overflow is hard-clipped", "{a} {b}", 10, "abcdefghij"},
		{"shrink= makes a rigid token give way", "{a:shrink=1:min=4} {b}", 10, "abcdef… XY"},
		{"control: rigid first token clips the second", "{b} {a}", 10, "XY abcdefg"},
		{"drop= removes the token and its gap", "{b:drop=1} {a}", 10, "abcdefghij"},
		{"min= low enough: shrinking suffices, nothing dropped", "{a:shrink=1:min=2} {b:drop=1}", 10, "abcdef… XY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := line(t, rowTemplate(t, tc.tpl), tc.w, r)
			if plain(got) != tc.want {
				t.Errorf("%s at W=%d renders %q; want %q", tc.tpl, tc.w, plain(got), tc.want)
			}
		})
	}
	// min= high: the shrink stops at 9 columns, so the drop has to happen.
	got, _ := line(t, rowTemplate(t, "{a:shrink=1:min=9} {b:drop=1}"), 10, r)
	if p := plain(got); strings.Contains(p, "XY") || !strings.HasPrefix(p, "abcdefgh") {
		t.Errorf("min=9: renders %q; want a kept at ≥ 9 columns and b dropped", p)
	}
	// max= caps the natural width.
	got, _ = line(t, rowTemplate(t, "{a:max=4}"), 80, r)
	if p := plain(got); width(p) > 4 || !strings.HasPrefix(p, "abc") {
		t.Errorf("max=4 renders %q (%d columns)", p, width(p))
	}
}
