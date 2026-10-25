package panelfmt

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestSeparatorsCollapseWithEmptyTokens is every row of the §2.3 table, plus
// the spring split (#18), the gap preference (#14) and the trailing literal
// (#23). "" is an Empty token.
func TestSeparatorsCollapseWithEmptyTokens(t *testing.T) {
	cases := []struct {
		name, tpl string
		vals      map[string]string
		want      string // "" with ok false = the line is dropped
		ok        bool
	}{
		{"middle empty", "{a} · {b} · {c}", map[string]string{"a": "a", "c": "c"}, "a · c", true},
		{"first empty", "{a} · {b} · {c}", map[string]string{"b": "b", "c": "c"}, "b · c", true},
		{"tail empty", "{a} · {b} · {c}", map[string]string{"a": "a"}, "a", true},
		{"all empty drops the line", "    {w} · {p}", nil, "", false},
		{"leading literal kept when a later token draws", "    {w} · {p}", map[string]string{"p": "%12"}, "    %12", true},
		{"non-whitespace gap preferred (#14)", "{group} · {kind} {id}", map[string]string{"group": "authflow", "id": "be9663df"}, "authflow · be9663df", true},
		{"every gap drawn when full", "{group} · {kind} {id}", map[string]string{"group": "authflow", "kind": "bg", "id": "be9663df"}, "authflow · bg be9663df", true},
		{"all-whitespace candidates: the one before the next token", "{a}  {b} {c}", map[string]string{"a": "a", "c": "c"}, "a c", true},
		{"adjacent tokens bridge to the real separator", "{a} · {b}{c}", map[string]string{"a": "a", "c": "c"}, "a · c", true},
		{"trailing literal needs its token (#23)", "{wait} ({pane})", map[string]string{"wait": "input-needed"}, "input-needed", true},
		{"trailing literal drawn with its token", "{wait} ({pane})", map[string]string{"wait": "input-needed", "pane": "%9"}, "input-needed (%9)", true},
		{"leading literal needs a token of its part", "[{a}]", nil, "", false},
		// §2.3 rule 7: a bracket belongs to the token it touches, and is
		// drawn exactly when that token is — never an unbalanced "%9)".
		{"brackets of a later token survive an empty first", "{a} ({b})", map[string]string{"b": "%9"}, "(%9)", true},
		{"brackets of an empty token vanish with it", "{a} ({b})", map[string]string{"a": "x"}, "x", true},
		{"both drawn: the gap whole", "{a} ({b})", map[string]string{"a": "x", "b": "%9"}, "x (%9)", true},
		{"square brackets likewise", "{a} [{b}]", map[string]string{"b": "%9"}, "[%9]", true},
		{"a leading bracket belongs to the first token", "({a}) {b}", map[string]string{"b": "b"}, "b", true},
		{"its closer is drawn when later tokens are empty", "({a}) {b}", map[string]string{"a": "a"}, "(a)", true},
		{"an empty bracketed middle bridges to the real separator", "{a} ({b}) · {c}", map[string]string{"a": "a", "c": "c"}, "a · c", true},
		{"bracketed middle drawn", "{a} ({b}) · {c}", map[string]string{"a": "a", "b": "b", "c": "c"}, "a (b) · c", true},
		{"adjacent bracket groups", "{a}({b})({c})", map[string]string{"a": "a", "c": "c"}, "a(c)", true},
		{"blanks inside the brackets travel with them", "{a} ( {b} )", map[string]string{"b": "b"}, "( b )", true},
		{"token-less template draws its literals", "── rule ──", nil, "── rule ──", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := mustTpl(t, keyMeta, c.tpl).Line(80, texts(c.vals))
			if got != c.want || ok != c.ok {
				t.Errorf("%q → %q (ok=%v), want %q (ok=%v)", c.tpl, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestTheSpringCollapsesEachPartOnItsOwn (#18): the left part's dangling
// " · " cannot survive an empty token next to the spring.
func TestTheSpringCollapsesEachPartOnItsOwn(t *testing.T) {
	tp := mustTpl(t, keyRow, "{name} · {group}{>}{ctx}")
	got, ok := tp.Line(20, texts(map[string]string{"name": "n", "ctx": "48k"}))
	if want := "n" + strings.Repeat(" ", 16) + "48k"; !ok || got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// An empty right part: the spring still fills the line to the width.
	got, _ = mustTpl(t, keyRow, "{a}{>} [{ctx}]").Line(12, texts(map[string]string{"a": "left"}))
	if got != "left"+strings.Repeat(" ", 8) {
		t.Errorf("empty right part: %q", got)
	}
	// A right part's leading literal draws with its token.
	got, _ = mustTpl(t, keyRow, "{a}{>} [{ctx}]").Line(12, texts(map[string]string{"a": "left", "ctx": "9k"}))
	if got != "left"+strings.Repeat(" ", 3)+" [9k]" {
		t.Errorf("right part: %q", got)
	}
}

// TestLiteralsCarryTheLiteralStyle (R7): literals draw in LiteralStyle,
// tokens in their own segment styles, and collapse ignores styles.
func TestLiteralsCarryTheLiteralStyle(t *testing.T) {
	withTrueColor(t, func() {
		slate := lipgloss.NewStyle().Foreground(lipgloss.Color("#7A7F9A"))
		cloud := lipgloss.NewStyle().Foreground(lipgloss.Color("#CDD6F4"))
		s := ParseConfig([]byte(`meta = "    {a} · {b} · {c}"`), testReg, Options{LiteralStyle: slate}, defaults(t))
		got, _ := s.Meta.Line(80, vals(map[string]Segment{
			"a": {Parts: []Part{{Text: "%12", Style: cloud}}},
			"c": {Parts: []Part{{Text: "3 sub", Style: cloud}}},
		}))
		want := slate.Render("    ") + cloud.Render("%12") + slate.Render(" · ") + cloud.Render("3 sub")
		if got != want {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})
}

// TestTheEngineAppliesTheCommonOptions (§2.6) on a Resolver that ignores
// them.
func TestTheEngineAppliesTheCommonOptions(t *testing.T) {
	coral := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D5E")).Background(lipgloss.Color("#222222")).Bold(true)
	ash := lipgloss.NewStyle().Foreground(lipgloss.Color("#6E7290"))
	r := func(Token) Segment { return Segment{Parts: []Part{{Text: "hello world", Style: coral}}} }
	opts := Options{DimStyle: ash}

	withTrueColor(t, func() {
		plain := ParseConfig([]byte(`meta = "{a:plain}"`), testReg, opts, defaults(t))
		if got, _ := plain.Meta.Line(80, r); got != lipgloss.NewStyle().Bold(true).Render("hello world") {
			t.Errorf("plain: %q (fg and bg must go, bold stays)", got)
		}
		dim := ParseConfig([]byte(`meta = "{a:dim}"`), testReg, opts, defaults(t))
		want := coral.Foreground(ash.GetForeground()).Render("hello world")
		if got, _ := dim.Meta.Line(80, r); got != want {
			t.Errorf("dim: %q, want %q", got, want)
		}
		// A token's own option of the same name is not the engine's to apply.
		own := ParseConfig([]byte(`meta = "{ctx:plain}"`), testReg, opts, defaults(t))
		if got, _ := own.Meta.Line(80, r); got != coral.Render("hello world") {
			t.Errorf("{ctx:plain} restyled by the engine: %q", got)
		}
	})

	cases := []struct {
		tpl   string
		width int
		want  string
	}{
		{"{a:max=5}", 80, "hell…"},
		{"{a:max=20}", 80, "hello world"},
		{"{a} {b:shrink=1:min=4}", 10, "hi hello…"}, // b made shrinkable, a stays rigid
		{"{a} {b}", 10, "hi hello w"},               // no fit fields: the line is hard-clipped
		{"{a} · {b:drop=1}", 10, "hi"},              // dropped with its separator
		{"{a:shrink=1:min=1} {b:shrink=2}", 10, "… hello w…"},
	}
	for _, c := range cases {
		r := texts(map[string]string{"a": "hi", "b": "hello world"})
		if strings.HasPrefix(c.tpl, "{a:max") {
			r = func(Token) Segment { return seg("hello world") }
		}
		got, _ := mustTpl(t, keyMeta, c.tpl).Line(c.width, r)
		if got != c.want || width(got) > c.width {
			t.Errorf("%s @%d → %q, want %q", c.tpl, c.width, got, c.want)
		}
	}
}

// TestAnUnknownTokenRendersInPlace: coral {raw?}, rigid, never Empty, and the
// Resolver is never asked about it.
func TestAnUnknownTokenRendersInPlace(t *testing.T) {
	coral := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D5E"))
	s := ParseConfig([]byte(`meta = "{foo} · {ctx:pct}"`), testReg, Options{ErrorStyle: coral}, defaults(t))
	called := false
	r := func(Token) Segment { called = true; return seg("x") }
	withTrueColor(t, func() {
		got, ok := s.Meta.Line(80, r)
		want := coral.Render("{foo?}") + " · " + coral.Render("{ctx:pct?}")
		if !ok || got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	if called {
		t.Error("the Resolver was called for an unknown token")
	}
	// Rigid: at a narrow width the line is clipped, not shrunk to "…".
	if got, _ := s.Meta.Line(8, r); got != "{foo?} ·" {
		t.Errorf("narrow: %q", got)
	}
}

// TestASegmentThatDrawsNothingIsEmpty: a zero-width segment would leave its
// separators around a hole, so it collapses like an Empty one.
func TestASegmentThatDrawsNothingIsEmpty(t *testing.T) {
	r := vals(map[string]Segment{"a": seg("a"), "b": {Parts: []Part{{Text: ""}}}, "c": seg("c")})
	if got, _ := mustTpl(t, keyMeta, "{a} · {b} · {c}").Line(80, r); got != "a · c" {
		t.Errorf("got %q", got)
	}
	if _, ok := mustTpl(t, keyMeta, "  {b}").Line(80, r); ok {
		t.Error("a line of zero-width tokens exists")
	}
}

// TestControlCharactersNeverReachTheTerminal: the panel sanitises session
// text (§2.7); the engine still replaces any control character that slips
// through, so width is measured on what is drawn.
func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	r := texts(map[string]string{"a": "evil\x1b]0;title\x07name\tx\r\n\u009b"})
	got, _ := mustTpl(t, keyMeta, "{a}").Line(80, r)
	if strings.ContainsAny(got, "\x1b\x07\t\r\n\u009b") {
		t.Fatalf("a control character got through: %q", got)
	}
	if got != "evil ]0;title name x   " {
		t.Errorf("got %q", got)
	}
}

// TestTheResolversPartsAreNotModified: a Resolver may hand back a shared
// slice; truncation and the common options work on the engine's copy.
func TestTheResolversPartsAreNotModified(t *testing.T) {
	shared := []Part{{Text: "a-long-name"}, {Text: "\tx"}}
	r := func(Token) Segment { return Segment{Parts: shared, Shrink: 1} }
	mustTpl(t, keyMeta, "{a:plain:max=4}").Line(3, r)
	if shared[0].Text != "a-long-name" || shared[1].Text != "\tx" {
		t.Errorf("shared parts modified: %+v", shared)
	}
}

func TestLineReportsFalseOnlyWhenEveryTokenIsEmpty(t *testing.T) {
	tp := mustTpl(t, keyRow, "{a}{>}{b}")
	if _, ok := tp.Line(20, texts(nil)); ok {
		t.Error("all empty: ok")
	}
	if got, ok := tp.Line(20, texts(map[string]string{"b": "b"})); !ok || got != strings.Repeat(" ", 19)+"b" {
		t.Errorf("right only: %q ok=%v", got, ok)
	}
	if _, ok := tp.Line(20, nil); ok {
		t.Error("a nil Resolver makes every known token Empty")
	}
}
