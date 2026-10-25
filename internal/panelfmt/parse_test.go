package panelfmt

import (
	"reflect"
	"testing"
)

// TestTemplateProblemsArePositioned: every fatal and soft template problem,
// with its exact column (identity mapping: column = byte offset + 1).
func TestTemplateProblemsArePositioned(t *testing.T) {
	cases := []struct {
		name, key, src string
		want           Problem
		nilTpl         bool
	}{
		{"unclosed at end", keyRow, "{a} {b", Problem{Line: 1, Col: 5, Msg: `unclosed "{"`, Fatal: true}, true},
		{"brace inside a token", keyRow, "{a {b}", Problem{Line: 1, Col: 1, Msg: `unclosed "{"`, Fatal: true}, true},
		{"stray close", keyRow, "a } b", Problem{Line: 1, Col: 3, Msg: `stray "}" (write }} for a literal brace)`, Fatal: true}, true},
		{"second spring", keyRow, "{a}{>}{b}{>}", Problem{Line: 1, Col: 10, Msg: "a second spring {>}; a template has at most one", Fatal: true}, true},
		{"unknown token", keyRow, "x {foo}", Problem{Line: 1, Col: 3, Msg: "unknown token {foo}"}, false},
		{"removed window token", keyRow, "{window}", Problem{Line: 1, Col: 1, Msg: "unknown token {window}"}, false},
		{"unknown option", keyRow, "{a} {ctx:pct}", Problem{Line: 1, Col: 10, Msg: `unknown option "pct" in {ctx:pct}`}, false},
		{"removed bar option", keyRow, "{ctx:bar20}", Problem{Line: 1, Col: 6, Msg: `unknown option "bar" in {ctx:bar20}`}, false},
		{"bad name", keyRow, "{Name}", Problem{Line: 1, Col: 1, Msg: "bad token name {Name}"}, false},
		{"empty token", keyRow, "{a}{}", Problem{Line: 1, Col: 4, Msg: "empty token {}"}, false},
		{"empty option", keyRow, "{ctx:}", Problem{Line: 1, Col: 6, Msg: `bad option "" in {ctx:}`}, false},
		{"option value empty", keyRow, "{name:min=}", Problem{Line: 1, Col: 7, Msg: `bad option "min=" in {name:min=}`}, false},
		{"flag with value", keyRow, "{ctx:plain=1}", Problem{Line: 1, Col: 6, Msg: `option "plain" takes no value in {ctx:plain=1}`}, false},
		{"string without value", keyRow, "{gutter:char}", Problem{Line: 1, Col: 9, Msg: `option "char" needs a value (char=…) in {gutter:char}`}, false},
		{"common int without number", keyRow, "{name:min}", Problem{Line: 1, Col: 7, Msg: `option "min" needs a number (min=N) in {name:min}`}, false},
		{"int not a number", keyRow, "{name:max=x}", Problem{Line: 1, Col: 7, Msg: `option "max" wants a number, not "x" in {name:max=x}`}, false},
		{"int out of range", keyAttnWhy, "{why:wrap=3}", Problem{Line: 1, Col: 6, Msg: "option wrap=3 out of range 1..2 in {why:wrap=3}"}, false},
		{"common out of range", keyRow, "{name:max=0}", Problem{Line: 1, Col: 7, Msg: "option max=0 out of range 1..999 in {name:max=0}"}, false},
		{"wrap outside attn_why", keyMeta, "    {why:wrap}", Problem{Line: 1, Col: 5, Msg: "wrap is only honoured in attn_why; {why:wrap} renders on one line"}, false},
		{"wrap not last", keyAttnWhy, "{why:wrap} {a}", Problem{Line: 1, Col: 1, Msg: "{why:wrap} must be the last token to wrap; it renders on one line"}, false},
		{"second wrap", keyAttnWhy, "{needs:wrap}{why:wrap}", Problem{Line: 1, Col: 13, Msg: "a second wrap token {why:wrap}; it renders on one line"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp, probs := parseTemplate(c.src, identity, 1, c.key, testReg)
			if len(probs) != 1 {
				t.Fatalf("problems = %+v, want exactly %+v", probs, c.want)
			}
			if probs[0] != c.want {
				t.Errorf("problem = %+v\n      want %+v", probs[0], c.want)
			}
			if (tp == nil) != c.nilTpl {
				t.Errorf("template nil = %v, want %v (fatal drops it, soft keeps it)", tp == nil, c.nilTpl)
			}
		})
	}
}

func TestOptionsAreParsed(t *testing.T) {
	cases := []struct {
		src  string
		want []Opt
	}{
		{"{why:wrap=2}", []Opt{{Name: "wrap", Value: "2", N: 2, HasN: true}}},
		{"{why:wrap}", []Opt{{Name: "wrap"}}},
		{"{why:wrap1}", []Opt{{Name: "wrap", N: 1, HasN: true}}},
		{"{name:min12:colour}", []Opt{{Name: "min", N: 12, HasN: true}, {Name: "colour"}}},
		{"{name:shrink=0}", []Opt{{Name: "shrink", Value: "0", N: 0, HasN: true}}},
		{"{gutter:char=▌}", []Opt{{Name: "char", Value: "▌"}}},
		{"{ctx:plain:hide}", []Opt{{Name: "plain"}, {Name: "hide"}}},
	}
	for _, c := range cases {
		tp := mustTpl(t, keyAttnWhy, c.src)
		got := tp.toks[0]
		if !got.Known || !reflect.DeepEqual(got.Opts, c.want) {
			t.Errorf("%s: known=%v opts=%+v, want %+v", c.src, got.Known, got.Opts, c.want)
		}
		if got.Raw != c.src[1:len(c.src)-1] || got.Line != 1 || got.Col != 1 {
			t.Errorf("%s: raw=%q line=%d col=%d", c.src, got.Raw, got.Line, got.Col)
		}
	}
}

// TestATokensOwnOptionShadowsTheCommonOne: {ctx:plain} belongs to ctx (the
// count in its tone colour, no fill, §3), so the engine must not also strip
// the colour; {name:plain} is the common option.
func TestATokensOwnOptionShadowsTheCommonOne(t *testing.T) {
	if tp := mustTpl(t, keyRow, "{ctx:plain}"); tp.toks[0].com.plain {
		t.Error("{ctx:plain}: the engine applies plain, but ctx declares it")
	}
	if tp := mustTpl(t, keyRow, "{name:plain:dim:min=3:max=9:shrink=2:drop=4}"); tp.toks[0].com != (common{
		plain: true, dim: true, min: 3, max: 9, shrink: 2, drop: 4,
		hasMin: true, hasMax: true, hasShrink: true, hasDrop: true,
	}) {
		t.Errorf("{name:…} common = %+v", tp.toks[0].com)
	}
}

func TestWrapIsHonouredOnlyAsTheLastTokenOfAttnWhy(t *testing.T) {
	cases := []struct {
		key, src string
		want     int
	}{
		{keyAttnWhy, "    {why:wrap=2}", 2},
		{keyAttnWhy, "    {why:wrap=1}", 1},
		{keyAttnWhy, "    {why:wrap}", defaultWrapRows},
		{keyAttnWhy, "    {why:wrap}.", 2}, // a trailing literal is not a token
		{keyAttnWhy, "{a}{>}{why:wrap}", 2},
		{keyAttnWhy, "    {why}", 0},
	}
	for _, c := range cases {
		if tp := mustTpl(t, c.key, c.src); tp.wrap != c.want {
			t.Errorf("%s: wrap = %d, want %d", c.src, tp.wrap, c.want)
		}
	}
	for _, src := range []string{"{why:wrap} {a}", "{needs:wrap}{why:wrap}"} {
		tp, _ := parseTemplate(src, identity, 1, keyAttnWhy, testReg)
		if tp.wrap != 0 {
			t.Errorf("%s: wrap honoured (%d), want a single line", src, tp.wrap)
		}
	}
}

func TestLiteralsAndBraceEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{"{{{a}}}", "{x}"},
		{"{{a}}", "{a}"},
		{"a}}b{{c", "a}b{c"},
		{"[{a}]", "[x]"},
		{"\t{a}", "    x"}, // a TAB literal is drawn as the four spaces lipgloss would
	}
	for _, c := range cases {
		got, ok := mustTpl(t, keyRow, c.src).Line(80, texts(map[string]string{"a": "x"}))
		if !ok || got != c.want {
			t.Errorf("%q → %q (ok=%v), want %q", c.src, got, ok, c.want)
		}
	}
}

func TestSpringSplitsTheTemplateIntoTwoParts(t *testing.T) {
	tp := mustTpl(t, keyRow, "{a} · {b}{>}{c}")
	if !tp.spring || len(tp.parts) != 2 {
		t.Fatalf("spring=%v parts=%d", tp.spring, len(tp.parts))
	}
	if tp := mustTpl(t, keyRow, "{a} · {b}"); tp.spring || len(tp.parts) != 1 {
		t.Fatalf("no spring: spring=%v parts=%d", tp.spring, len(tp.parts))
	}
}
