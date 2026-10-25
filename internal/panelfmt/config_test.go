package panelfmt

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func defaults(t *testing.T) *Set {
	t.Helper()
	return Defaults([]byte(defaultConf), testReg, Options{})
}

// TestTheBuiltInConfRoundTrips: the §6.6 text parses clean, sets every
// template and both thresholds, and renders a row with the spring filling
// the line.
func TestTheBuiltInConfRoundTrips(t *testing.T) {
	s := defaults(t)
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	for name, tp := range map[string]*Template{"row": s.Row, "meta": s.Meta, "attn_row": s.AttnRow, "attn_why": s.AttnWhy, "attn_meta": s.AttnMeta} {
		if tp == nil {
			t.Errorf("%s is nil", name)
		}
	}
	if s.Ctx != (Thresholds{Warn: 200_000, Alert: 400_000}) {
		t.Errorf("ctx = %+v", s.Ctx)
	}
	if s.AttnWhy.wrap != 2 {
		t.Errorf("attn_why wrap = %d, want 2", s.AttnWhy.wrap)
	}
	line, ok := s.Row.Line(46, texts(map[string]string{
		"gutter": "▌", "status": " busy   2m ", "name": "tt-ui-rebuild", "ctx": " 194k ",
	}))
	want := "▌ busy   2m  tt-ui-rebuild" + strings.Repeat(" ", 14) + " 194k "
	if !ok || line != want || width(line) != 46 {
		t.Errorf("row = %q (w=%d), want %q", line, width(line), want)
	}
}

func TestDefaultsPanicsOnAnyProblem(t *testing.T) {
	for name, src := range map[string]string{
		"soft: unknown token": `row = "{nope}"` + "\nattn_row = \"{a}\"\n",
		"fatal: bad key":      "rows = \"{a}\"\n",
		"missing row":         `attn_row = "{a}"`,
		"missing attn_row":    `row = "{a}"`,
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Defaults did not panic")
				}
			}()
			Defaults([]byte(src), testReg, Options{})
		})
	}
}

// TestAFileOverridesOnlyTheKeysItSets: a missing key keeps its default.
func TestAFileOverridesOnlyTheKeysItSets(t *testing.T) {
	d := defaults(t)
	s := ParseConfig([]byte(`meta = "{a} | {b}"`), testReg, Options{}, d)
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if got, _ := s.Meta.Line(80, texts(map[string]string{"a": "1", "b": "2"})); got != "1 | 2" {
		t.Errorf("meta = %q", got)
	}
	if s.Row.parts == nil || &s.Row.parts[0][0] != &d.Row.parts[0][0] {
		t.Error("row is not the default's (parsed nodes should be shared)")
	}
	if s.Ctx != d.Ctx {
		t.Errorf("ctx = %+v, want the default %+v", s.Ctx, d.Ctx)
	}
}

// TestAnEmptyLineTemplateIsNil (#33): "", ” and whitespace-only all mean
// the line does not exist.
func TestAnEmptyLineTemplateIsNil(t *testing.T) {
	src := "meta = \"\"\nattn_why = ''\nattn_meta = \"   \"\n"
	s := ParseConfig([]byte(src), testReg, Options{}, defaults(t))
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if s.Meta != nil || s.AttnWhy != nil || s.AttnMeta != nil {
		t.Errorf("meta=%v attn_why=%v attn_meta=%v, want all nil", s.Meta, s.AttnWhy, s.AttnMeta)
	}
	if s.Row == nil || s.AttnRow == nil {
		t.Error("row/attn_row lost")
	}
}

func TestThresholds(t *testing.T) {
	cases := []struct {
		src  string
		want Thresholds
	}{
		{"ctx_warn = 150k", Thresholds{150_000, 400_000}},
		{"ctx_alert = 1.5m", Thresholds{200_000, 1_500_000}},
		{"ctx_alert=2M", Thresholds{200_000, 2_000_000}},
		{"ctx_warn = 300000 # plain count", Thresholds{300_000, 400_000}},
		{"ctx_warn = 1k\nctx_alert = 100M", Thresholds{1_000, 100_000_000}},
		{"ctx_warn = 2.5K", Thresholds{2_500, 400_000}},
		{"ctx_alert = 500k\nctx_warn = 450k", Thresholds{450_000, 500_000}},
	}
	for _, c := range cases {
		s := ParseConfig([]byte(c.src), testReg, Options{}, defaults(t))
		if len(s.Problems) != 0 || s.Ctx != c.want {
			t.Errorf("%q: ctx=%+v problems=%v, want %+v", c.src, s.Ctx, s.Problems, c.want)
		}
	}
}

// TestFatalProblemsArePositionedAndFallBack: each is fatal, located exactly,
// and the Set is the defaults with the problem attached.
func TestFatalProblemsArePositionedAndFallBack(t *testing.T) {
	cases := []struct {
		name, src string
		want      Problem
	}{
		{"unknown key", "# c\n  rows = \"{a}\"", Problem{Line: 2, Col: 3, Msg: `unknown key "rows"`}},
		{"upper-case key", "Row = \"{a}\"", Problem{Line: 1, Col: 1, Msg: `unknown key "Row"`}},
		{"not a key", "= \"{a}\"", Problem{Line: 1, Col: 1, Msg: `unexpected '=' where a key belongs`}},
		{"missing =", "row \"{a}\"", Problem{Line: 1, Col: 5, Msg: `expected "=" after row`}},
		{"unquoted", "row = {a}", Problem{Line: 1, Col: 7, Msg: "expected a quoted string"}},
		{"unterminated", "meta = \"{a}", Problem{Line: 1, Col: 8, Msg: "unterminated string"}},
		{"unterminated raw", "meta = '{a}", Problem{Line: 1, Col: 8, Msg: "unterminated string"}},
		{"escape at end", "meta = \"{a}\\", Problem{Line: 1, Col: 8, Msg: "unterminated string"}},
		{"unknown escape", "meta = \"{a}\\n\"", Problem{Line: 1, Col: 12, Msg: `unknown escape \n (only \" \\ \t)`}},
		{"text after value", "meta = \"{a}\" x", Problem{Line: 1, Col: 14, Msg: "unexpected text after the value"}},
		{"unclosed brace, file column", "meta = \"ab{a\"", Problem{Line: 1, Col: 11, Msg: `unclosed "{"`}},
		{"second spring", "row = \"{a}{>}{>}\"", Problem{Line: 1, Col: 14, Msg: "a second spring {>}; a template has at most one"}},
		{"empty row", "row = \"\"", Problem{Line: 1, Msg: "row must not be empty"}},
		{"blank attn_row", "\nattn_row = '  '", Problem{Line: 2, Msg: "attn_row must not be empty"}},
		{"bad size", "ctx_warn = 20x", Problem{Line: 1, Col: 12, Msg: `bad size "20x" (write it like 200k, 1.5m or 400000)`}},
		{"quoted size", "ctx_warn = \"200k\"", Problem{Line: 1, Col: 12, Msg: `bad size "\"200k\"" (write it like 200k, 1.5m or 400000)`}},
		{"two decimals", "ctx_warn = 1.25m", Problem{Line: 1, Col: 12, Msg: `bad size "1.25m" (write it like 200k, 1.5m or 400000)`}},
		{"below 1k", "ctx_warn = 0.5k", Problem{Line: 1, Col: 12, Msg: "ctx_warn must be between 1k and 100M, not 0.5k"}},
		{"above 100M", "ctx_alert = 101M", Problem{Line: 1, Col: 13, Msg: "ctx_alert must be between 1k and 100M, not 101M"}},
		{"warn not below default alert", "\n\nctx_warn = 500k", Problem{Line: 3, Msg: "ctx_warn (500k) must be below ctx_alert (400k)"}},
		{"warn equals alert", "ctx_warn = 300k\nctx_alert = 300k", Problem{Line: 2, Msg: "ctx_warn (300k) must be below ctx_alert (300k)"}},
		{"control character", "meta = \"{a}\x1b[31m\"", Problem{Line: 1, Col: 12, Msg: "control character U+001B"}},
		{"C1 control", "meta = \"\u009b{a}\"", Problem{Line: 1, Col: 9, Msg: "control character U+009B"}},
		{"invalid UTF-8", "meta = \"\xff{a}\"", Problem{Line: 1, Msg: "the line is not valid UTF-8"}},
		{"line too long", "meta = \"" + strings.Repeat("x", 1100) + "\"", Problem{Line: 1, Msg: "the line is longer than 1 KiB"}},
	}
	d := defaults(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := ParseConfig([]byte(c.src), testReg, Options{}, d)
			c.want.Fatal = true
			if len(s.Problems) != 1 || s.Problems[0] != c.want {
				t.Fatalf("problems = %+v\n        want [%+v]", s.Problems, c.want)
			}
			if s.Row == nil || s.AttnRow == nil || s.Ctx != d.Ctx {
				t.Error("a fatal file did not fall back to the defaults")
			}
		})
	}
}

func TestAFileLargerThan64KiBIsFatal(t *testing.T) {
	src := strings.Repeat("# padding padding padding padding padding padding\n", 1400)
	s := ParseConfig([]byte(src), testReg, Options{}, defaults(t))
	want := Problem{Msg: "the file is larger than 64 KiB", Fatal: true}
	if len(s.Problems) != 1 || s.Problems[0] != want {
		t.Fatalf("problems = %+v", s.Problems)
	}
	if got := s.Problems[0].Format("panel.conf"); got != "panel.conf: the file is larger than 64 KiB" {
		t.Errorf("format = %q", got)
	}
}

// TestColumnsCountRunesAcrossEscapes: a token's column is where its "{" sits
// in the file, in runes, after escapes and multi-byte text before it.
func TestColumnsCountRunesAcrossEscapes(t *testing.T) {
	cases := []struct {
		src string
		col int
	}{
		{`meta = "{foo}"`, 9},
		{`meta = "\"{foo}"`, 11},
		{`meta = "\t{foo}"`, 11},
		{`meta = "é · {foo}"`, 13},
		{`meta = '\{foo}'`, 10},
		{`	meta	=	"{a} {foo}"`, 14},
	}
	for _, c := range cases {
		s := ParseConfig([]byte(c.src), testReg, Options{}, defaults(t))
		want := Problem{Line: 1, Col: c.col, Msg: "unknown token {foo}"}
		if len(s.Problems) != 1 || s.Problems[0] != want {
			t.Errorf("%s: problems = %+v, want col %d", c.src, s.Problems, c.col)
		}
	}
}

// TestSoftProblemsKeepTheTemplate: the file applies and the token renders in
// place, coral, as {raw?}.
func TestSoftProblemsKeepTheTemplate(t *testing.T) {
	s := ParseConfig([]byte("\n\nmeta = \"{a} · {foo} · {ctx:pct}\""), testReg, Options{}, defaults(t))
	want := []Problem{
		{Line: 3, Col: 15, Msg: "unknown token {foo}"},
		{Line: 3, Col: 28, Msg: `unknown option "pct" in {ctx:pct}`},
	}
	if len(s.Problems) != 2 || s.Problems[0] != want[0] || s.Problems[1] != want[1] {
		t.Fatalf("problems = %+v", s.Problems)
	}
	if got := s.Problems[0].Format("panel.conf"); got != "panel.conf:3:15: unknown token {foo}" {
		t.Errorf("note = %q", got)
	}
	got, ok := s.Meta.Line(80, texts(map[string]string{"a": "x"}))
	if !ok || got != "x · {foo?} · {ctx:pct?}" {
		t.Errorf("meta = %q", got)
	}
}

func TestADuplicateKeyIsSoftAndTheLaterWins(t *testing.T) {
	s := ParseConfig([]byte("meta = \"{a}\"\n meta = \"{b}\"\nctx_warn = 100k\nctx_warn = 150k"), testReg, Options{}, defaults(t))
	want := []Problem{
		{Line: 2, Col: 2, Msg: "duplicate key meta; the later one wins"},
		{Line: 4, Col: 1, Msg: "duplicate key ctx_warn; the later one wins"},
	}
	if len(s.Problems) != 2 || s.Problems[0] != want[0] || s.Problems[1] != want[1] {
		t.Fatalf("problems = %+v", s.Problems)
	}
	if got, _ := s.Meta.Line(80, texts(map[string]string{"a": "A", "b": "B"})); got != "B" {
		t.Errorf("meta = %q, want the later one", got)
	}
	if s.Ctx.Warn != 150_000 {
		t.Errorf("warn = %d", s.Ctx.Warn)
	}
}

// TestEveryProblemIsCollectedInFileOrder: --check prints them all, so the
// parse goes on past a fatal line; the whole file still falls back.
func TestEveryProblemIsCollectedInFileOrder(t *testing.T) {
	src := "meta = \"{foo}\"\nbogus = 1\nctx_alert = 150k\nrow = \"{a\""
	s := ParseConfig([]byte(src), testReg, Options{}, defaults(t))
	var lines []int
	for _, p := range s.Problems {
		lines = append(lines, p.Line)
	}
	if len(s.Problems) != 4 || lines[0] != 1 || lines[1] != 2 || lines[2] != 3 || lines[3] != 4 {
		t.Fatalf("problems = %+v", s.Problems)
	}
	if s.Problems[0].Fatal || !s.Problems[1].Fatal || !s.Problems[2].Fatal || !s.Problems[3].Fatal {
		t.Errorf("fatal flags wrong: %+v", s.Problems)
	}
	if got, _ := s.Meta.Line(80, texts(map[string]string{"wait": "input-needed"})); got != "    input-needed" {
		t.Errorf("meta = %q, want the default's", got)
	}
}

func TestCommentsBlankLinesAndCRLF(t *testing.T) {
	src := "# a comment\r\n\r\n   \t\r\nmeta = \"{a} # not a comment\" # a comment\r\nrow='{b}'#tight\r\n"
	s := ParseConfig([]byte(src), testReg, Options{}, defaults(t))
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if got, _ := s.Meta.Line(80, texts(map[string]string{"a": "x"})); got != "x # not a comment" {
		t.Errorf("meta = %q", got)
	}
	if got, _ := s.Row.Line(80, texts(map[string]string{"b": "y"})); got != "y" {
		t.Errorf("row = %q", got)
	}
}

func TestProblemFormat(t *testing.T) {
	cases := []struct {
		p    Problem
		want string
	}{
		{Problem{Line: 7, Msg: `unclosed "{"`}, `panel.conf:7: unclosed "{"`},
		{Problem{Line: 3, Col: 14, Msg: "unknown token {foo}"}, "panel.conf:3:14: unknown token {foo}"},
		{Problem{Msg: "the file is larger than 64 KiB"}, "panel.conf: the file is larger than 64 KiB"},
	}
	for _, c := range cases {
		if got := c.p.Format("panel.conf"); got != c.want {
			t.Errorf("%+v → %q, want %q", c.p, got, c.want)
		}
	}
}

// TestOptionsBindEveryTemplateOfTheSet: templates the file sets and templates
// kept from defaults both draw literals in the ParseConfig call's options.
func TestOptionsBindEveryTemplateOfTheSet(t *testing.T) {
	withTrueColor(t, func() {
		lit := lipgloss.NewStyle().Foreground(lipgloss.Color("#7A7F9A"))
		s := ParseConfig([]byte(`meta = "{a} · {b}"`), testReg, Options{LiteralStyle: lit}, defaults(t))
		sep := lit.Render(" · ")
		if got, _ := s.Meta.Line(80, texts(map[string]string{"a": "1", "b": "2"})); got != "1"+sep+"2" {
			t.Errorf("file template: %q", got)
		}
		got, _ := s.AttnMeta.Line(80, texts(map[string]string{"group": "g", "id": "i"}))
		if got != lit.Render("    ")+"g"+sep+"i" {
			t.Errorf("default template: %q", got)
		}
	})
}

// text_lines (revision 4): how many lines are drawn under a row, 0 to 3. A
// file that does not set it keeps the defaults' value; a file that does
// overrides it; anything but a whole number in range is fatal, and the
// defaults' value stands.
func TestTextLinesIsAWholeNumberFromZeroToThree(t *testing.T) {
	base := Defaults([]byte(defaultConf+"text_lines = 3\n"), testReg, Options{})
	if base.TextLines != 3 {
		t.Fatalf("the defaults' text_lines = %d, want 3", base.TextLines)
	}
	if s := ParseConfig([]byte(`meta = ""`+"\n"), testReg, Options{}, base); s.TextLines != 3 || len(s.Problems) != 0 {
		t.Errorf("a file without text_lines: %d, problems %v", s.TextLines, s.Problems)
	}
	for _, n := range []string{"0", "1", "2", "3"} {
		s := ParseConfig([]byte("text_lines = "+n+"  # comment\n"), testReg, Options{}, base)
		if len(s.Problems) != 0 || s.TextLines != int(n[0]-'0') {
			t.Errorf("text_lines = %s: %d, problems %v", n, s.TextLines, s.Problems)
		}
	}
	for _, bad := range []string{"4", "-1", "two", "1.5", "+2", ""} {
		s := ParseConfig([]byte("text_lines = "+bad+"\n"), testReg, Options{}, base)
		if len(s.Problems) != 1 || !s.Problems[0].Fatal || s.Problems[0].Line != 1 || s.TextLines != 3 ||
			!strings.Contains(s.Problems[0].Msg, "text_lines must be a whole number from 0 to 3") {
			t.Errorf("text_lines = %q: %d, problems %+v", bad, s.TextLines, s.Problems)
		}
	}
	s := ParseConfig([]byte("text_lines = 1\ntext_lines = 2\n"), testReg, Options{}, base)
	if s.TextLines != 2 || len(s.Problems) != 1 || s.Problems[0].Fatal || s.Problems[0].Line != 2 {
		t.Errorf("a second text_lines: %d, problems %+v", s.TextLines, s.Problems)
	}
}
