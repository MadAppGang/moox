package panelfmt_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// fullVals gives every token of the §6.6 default templates a value, so two
// templates render identically only if they are the same template.
func fullVals() vals {
	return vals{
		"gutter": "|", "status": "WAIT 13m", "name": "alpha", "group": "grp", "ctx": "132k",
		"wait": "input-needed", "pane": "%1", "sub": "2 sub", "apierr": "1 api err", "live": "live?",
		"needs": "needs-text", "why": "why text here", "kind": "bg", "id": "abcd1234", "model": "opus-5",
		"a": "A", "b": "B", "c": "C",
	}
}

func sameRender(t *testing.T, label string, got, want *panelfmt.Template) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s: got nil=%v, want nil=%v", label, got == nil, want == nil)
		}
		return
	}
	r := fullVals().resolver()
	g, gok := got.Line(80, r)
	w, wok := want.Line(80, r)
	if plain(g) != plain(w) || gok != wok {
		t.Errorf("%s: renders %q (%v); the default renders %q (%v)", label, plain(g), gok, plain(w), wok)
	}
}

func renderRow(t *testing.T, s *panelfmt.Set) string {
	t.Helper()
	out, _ := line(t, s.Row, 80, fullVals().resolver())
	return plain(out)
}

// REQ-1: key = string, whitespace and comments.
func TestBlackboxConfigAcceptsWhitespaceAndComments(t *testing.T) {
	withProfile(t, termenv.Ascii)
	for _, tc := range []struct{ name, src, want string }{
		{"spaces", "row = '{a}'\n", "A"},
		{"no spaces", "row='{a}'\n", "A"},
		{"tabs and leading ws", "  \trow\t=\t'{a}'\t\n", "A"},
		{"double quotes", "row = \"{a}\"\n", "A"},
		{"full-line comment and blank lines", "# a comment\n\n   \nrow = '{a}'\n# end\n", "A"},
		{"trailing comment", "row = '{a}'   # trailing words\n", "A"},
		{"trailing comment no space", "row = '{a}'# x\n", "A"},
		{"hash inside dq is literal", "row = \"{a} #1\"\n", "A #1"},
		{"hash inside sq is literal", "row = '{a} #2' # real comment\n", "A #2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := clean(t, tc.src)
			if got := renderRow(t, s); got != tc.want {
				t.Errorf("row renders %q; want %q", got, tc.want)
			}
		})
	}
}

// REQ-1: dq escapes decode, sq strings are raw.
func TestBlackboxConfigStringEscapes(t *testing.T) {
	withProfile(t, termenv.Ascii)
	for _, tc := range []struct{ name, src, want string }{
		{"dq escaped quote and backslash", `row = "{a}\"x\"\\"` + "\n", `A"x"\`},
		{"sq keeps backslash-t raw", `row = '{a}\t'` + "\n", `A\t`},
		{"sq may contain a double quote", `row = '{a}"q"'` + "\n", `A"q"`},
		{"dq may contain a single quote", `row = "{a}'s"` + "\n", `A's`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := clean(t, tc.src)
			if got := renderRow(t, s); got != tc.want {
				t.Errorf("row renders %q; want %q", got, tc.want)
			}
		})
	}
	// The grammar defines the \t escape but not how a TAB is drawn; a raw TAB
	// has no fixed display width, so the spec-safe promise is that it reaches
	// the row as one or more spaces between A and B, with no control byte.
	t.Run("dq escaped tab", func(t *testing.T) {
		s := clean(t, `row = "{a}\tB"`+"\n")
		got := renderRow(t, s)
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("row renders %q; contains control character %U", got, r)
			}
		}
		mid, ok := strings.CutPrefix(got, "A")
		if ok {
			mid, ok = strings.CutSuffix(mid, "B")
		}
		if !ok || mid == "" || strings.Trim(mid, " ") != "" {
			t.Errorf("row renders %q; want A, one or more spaces, then B", got)
		}
	})
}

// REQ-2, REQ-4: an empty file is the defaults, thresholds included.
func TestBlackboxConfigEmptyFileIsTheDefaults(t *testing.T) {
	withProfile(t, termenv.Ascii)
	d := defaults(t)
	for _, src := range []string{"", "\n", "# only a comment\n"} {
		s := clean(t, src)
		sameRender(t, "row", s.Row, d.Row)
		sameRender(t, "meta", s.Meta, d.Meta)
		sameRender(t, "attn_row", s.AttnRow, d.AttnRow)
		sameRender(t, "attn_why", s.AttnWhy, d.AttnWhy)
		sameRender(t, "attn_meta", s.AttnMeta, d.AttnMeta)
		if s.Ctx != (panelfmt.Thresholds{Warn: 200_000, Alert: 400_000}) {
			t.Errorf("%q: Ctx = %+v; want 200k/400k", src, s.Ctx)
		}
	}
}

// REQ-2: a file may override only one key; the rest keep their defaults.
func TestBlackboxConfigMissingKeyKeepsDefault(t *testing.T) {
	withProfile(t, termenv.Ascii)
	d := defaults(t)
	s := clean(t, "meta = '<<{b}>>'\n")
	sameRender(t, "row", s.Row, d.Row)
	sameRender(t, "attn_row", s.AttnRow, d.AttnRow)
	sameRender(t, "attn_why", s.AttnWhy, d.AttnWhy)
	sameRender(t, "attn_meta", s.AttnMeta, d.AttnMeta)
	if s.Meta == nil {
		t.Fatal("Meta is nil")
	}
	if got, _ := s.Meta.Line(80, fullVals().resolver()); plain(got) != "<<B>>" {
		t.Errorf("meta renders %q; want <<B>>", plain(got))
	}
}

// REQ-2 (#33): an empty meta / attn_why / attn_meta means the line does not exist.
func TestBlackboxConfigEmptyOptionalLinesAreNil(t *testing.T) {
	s := clean(t, "meta = \"\"\nattn_why = ''\nattn_meta = \"\"\n")
	if s.Meta != nil || s.AttnWhy != nil || s.AttnMeta != nil {
		t.Errorf("Meta nil=%v AttnWhy nil=%v AttnMeta nil=%v; want all nil", s.Meta == nil, s.AttnWhy == nil, s.AttnMeta == nil)
	}
	if s.Row == nil || s.AttnRow == nil {
		t.Error("Row and AttnRow must never be nil")
	}
}

// REQ-2, REQ-6: an empty row or attn_row is fatal and the whole file falls back.
func TestBlackboxConfigEmptyRowIsFatal(t *testing.T) {
	withProfile(t, termenv.Ascii)
	d := defaults(t)
	for _, key := range []string{"row", "attn_row"} {
		t.Run(key, func(t *testing.T) {
			s := parse(t, "meta = '<<{a}>>'\n"+key+" = \"\"\n")
			if !hasFatal(s) {
				t.Fatalf("empty %s: problems %+v; want a fatal one", key, s.Problems)
			}
			sameRender(t, "row", s.Row, d.Row)
			sameRender(t, "attn_row", s.AttnRow, d.AttnRow)
			sameRender(t, "meta (discarded with the file)", s.Meta, d.Meta)
		})
	}
}

// REQ-3: a duplicate key — the later one wins, with a soft note.
func TestBlackboxConfigDuplicateKeyLaterWins(t *testing.T) {
	withProfile(t, termenv.Ascii)
	s := parse(t, "meta = '{a}'\nmeta = '{b}'\n")
	if !softOnly(s) {
		t.Fatalf("problems %+v; want exactly soft ones", s.Problems)
	}
	if s.Problems[0].Line != 2 {
		t.Errorf("duplicate reported on line %d; want 2 (the later key)", s.Problems[0].Line)
	}
	if got, _ := s.Meta.Line(80, fullVals().resolver()); plain(got) != "B" {
		t.Errorf("meta renders %q; want B (the later value)", plain(got))
	}
}

// REQ-4: threshold sizes.
func TestBlackboxConfigThresholds(t *testing.T) {
	for _, tc := range []struct {
		src         string
		warn, alert int64
	}{
		{"ctx_warn = 150k\n", 150_000, 400_000},
		{"ctx_alert = 500000\n", 200_000, 500_000},
		{"ctx_warn = 1.5k\nctx_alert = 1.5m\n", 1_500, 1_500_000},
		{"ctx_warn = 2K\nctx_alert = 3M\n", 2_000, 3_000_000},
		{"ctx_warn = 1k\nctx_alert = 100m\n", 1_000, 100_000_000},
		{"ctx_warn=250k\nctx_alert=300k # tight\n", 250_000, 300_000},
		{"ctx_warn = 199999\nctx_alert = 200000\n", 199_999, 200_000},
	} {
		t.Run(strings.ReplaceAll(tc.src, "\n", "|"), func(t *testing.T) {
			s := clean(t, tc.src)
			if s.Ctx != (panelfmt.Thresholds{Warn: tc.warn, Alert: tc.alert}) {
				t.Errorf("Ctx = %+v; want Warn=%d Alert=%d", s.Ctx, tc.warn, tc.alert)
			}
		})
	}
}

// REQ-4, REQ-6: a bad size, an out-of-range value or warn ≥ alert is fatal.
func TestBlackboxConfigBadThresholdsAreFatal(t *testing.T) {
	for _, src := range []string{
		"ctx_warn = 2x\n",
		"ctx_warn = k\n",
		"ctx_warn = \n",
		"ctx_warn = -5k\n",
		"ctx_warn = 1.25m\n",                  // one decimal digit at most
		"ctx_warn = 999\n",                    // below 1k
		"ctx_alert = 101m\n",                  // above 100M
		"ctx_alert = 100000001\n",             // above 100M
		"ctx_warn = 300k\nctx_alert = 300k\n", // warn must be < alert
		"ctx_warn = 300k\nctx_alert = 250k\n",
		"ctx_alert = 150k\n", // below the default warn of 200k
		"ctx_warn = '200k'\n",
	} {
		t.Run(strings.ReplaceAll(src, "\n", "|"), func(t *testing.T) {
			s := parse(t, src)
			if !hasFatal(s) {
				t.Fatalf("problems %+v; want a fatal one", s.Problems)
			}
			if s.Ctx != (panelfmt.Thresholds{Warn: 200_000, Alert: 400_000}) {
				t.Errorf("after a fatal file Ctx = %+v; want the defaults 200k/400k", s.Ctx)
			}
		})
	}
}

// fileOfSize is a valid panel.conf of exactly n bytes: one row line, then
// comment lines of at most 1000 bytes.
func fileOfSize(n int) string {
	var b strings.Builder
	b.WriteString("row = '<<{a}>>'\n")
	for rest := n - b.Len(); rest > 0; rest = n - b.Len() {
		chunk := min(rest, 1000)
		if chunk == 1 {
			b.WriteString("\n")
			continue
		}
		b.WriteString("#" + strings.Repeat("x", chunk-2) + "\n")
	}
	return b.String()
}

// REQ-5: 64 KiB file and 1 KiB line limits.
func TestBlackboxConfigSizeLimits(t *testing.T) {
	withProfile(t, termenv.Ascii)
	d := defaults(t)

	ok := fileOfSize(64 * 1024)
	if len(ok) != 65536 {
		t.Fatalf("fixture is %d bytes", len(ok))
	}
	s := clean(t, ok)
	if got := renderRow(t, s); got != "<<A>>" {
		t.Errorf("64 KiB file: row renders %q; want <<A>>", got)
	}

	big := parse(t, fileOfSize(64*1024+1))
	if !hasFatal(big) {
		t.Errorf("65537-byte file: problems %+v; want fatal", big.Problems)
	}
	sameRender(t, "row after a too-big file", big.Row, d.Row)

	short := clean(t, "row = '<<{a}>>'\n#"+strings.Repeat("y", 998)+"\n")
	if got := renderRow(t, short); got != "<<A>>" {
		t.Errorf("1000-byte line: row renders %q", got)
	}

	long := parse(t, "row = '<<{a}>>'\n#"+strings.Repeat("y", 1100)+"\n")
	if !hasFatal(long) {
		t.Fatalf("1101-byte line: problems %+v; want fatal", long.Problems)
	}
	sameRender(t, "row after a too-long line", long.Row, d.Row)
}

// REQ-6, REQ-7, REQ-10: every fatal class discards the whole file, and the
// problem names the line it is on.
func TestBlackboxConfigFatalClasses(t *testing.T) {
	withProfile(t, termenv.Ascii)
	d := defaults(t)
	for _, tc := range []struct {
		name, src string
		line      int
	}{
		{"unterminated dq string", "# c\nrow = \"{a}\n", 2},
		{"unterminated sq string", "row = '{a}\n", 1},
		{"unknown key", "\n\nrows = '{a}'\n", 3},
		{"unknown key colour", "colour = '{a}'\n", 1},
		{"missing =", "row '{a}'\n", 1},
		{"unquoted template", "row = {a}\n", 1},
		{"junk after the string", "row = '{a}' junk\n", 1},
		{"unclosed brace", "# 1\n# 2\nrow = '{a'\n", 3},
		{"unclosed brace mid-template", "row = '{a} · {b'\n", 1},
		{"stray closing brace", "row = 'a}b'\n", 1},
		{"second spring", "row = '{a}{>}{b}{>}{c}'\n", 1},
		{"bad size", "\nctx_warn = 2x\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A valid meta line in the same file must be discarded with it.
			s := parse(t, "meta = '<<{a}>>'\n"+tc.src)
			want := tc.line + 1
			var fatal *panelfmt.Problem
			for i := range s.Problems {
				if s.Problems[i].Fatal {
					fatal = &s.Problems[i]
					break
				}
			}
			if fatal == nil {
				t.Fatalf("problems %+v; want a fatal one", s.Problems)
			}
			if fatal.Line != want {
				t.Errorf("fatal problem %+v on line %d; want line %d", *fatal, fatal.Line, want)
			}
			if f := fatal.Format("panel.conf"); !strings.HasPrefix(f, fmt.Sprintf("panel.conf:%d:", want)) {
				t.Errorf("Format = %q; want prefix panel.conf:%d:", f, want)
			}
			sameRender(t, "row", s.Row, d.Row)
			sameRender(t, "meta", s.Meta, d.Meta)
		})
	}
}

// REQ-7: every problem is collected, in file order, with a column for a token.
func TestBlackboxConfigProblemsInFileOrder(t *testing.T) {
	s := parse(t, "row = '{a} {foo}'\nmeta = '{b}'\nattn_meta = '{a:pct}'\n")
	if !softOnly(s) {
		t.Fatalf("problems %+v; want soft ones only", s.Problems)
	}
	if len(s.Problems) < 2 {
		t.Fatalf("problems %+v; want both the unknown token and the unknown option", s.Problems)
	}
	for i := 1; i < len(s.Problems); i++ {
		if s.Problems[i].Line < s.Problems[i-1].Line {
			t.Errorf("problems out of file order: %+v", s.Problems)
		}
	}
	first, last := s.Problems[0], s.Problems[len(s.Problems)-1]
	if first.Line != 1 || last.Line != 3 {
		t.Errorf("problem lines %d..%d; want 1..3", first.Line, last.Line)
	}
	// "row = '{a} {foo}'": the brace of {foo} is rune 12, its name starts at 13.
	if first.Col != 12 && first.Col != 13 {
		t.Errorf("unknown token column %d; want 12 (the brace) or 13 (the name)", first.Col)
	}
	if !strings.Contains(first.Msg, "foo") {
		t.Errorf("message %q does not name the token", first.Msg)
	}
}

// REQ-7: Problem.Format.
func TestBlackboxProblemFormat(t *testing.T) {
	for _, tc := range []struct {
		p    panelfmt.Problem
		want string
	}{
		{panelfmt.Problem{Line: 3, Col: 14, Msg: "unknown token {foo}"}, "panel.conf:3:14: unknown token {foo}"},
		{panelfmt.Problem{Line: 7, Msg: "duplicate key meta"}, "panel.conf:7: duplicate key meta"},
		{panelfmt.Problem{Line: 2, Col: 1, Msg: "x"}, "panel.conf:2:1: x"},
	} {
		if got := tc.p.Format("panel.conf"); got != tc.want {
			t.Errorf("Format(%+v) = %q; want %q", tc.p, got, tc.want)
		}
	}
	fatal := panelfmt.Problem{Line: 7, Msg: `unclosed "{"`, Fatal: true}
	if got := fatal.Format("panel.conf"); !strings.HasPrefix(got, `panel.conf:7: unclosed "{"`) {
		t.Errorf("fatal Format = %q; want prefix %q", got, `panel.conf:7: unclosed "{"`)
	}
	if got := (panelfmt.Problem{Line: 4, Msg: "m"}).Format("/x/y/panel.conf"); got != "/x/y/panel.conf:4: m" {
		t.Errorf("Format with a path = %q", got)
	}
}

// REQ-17: the built-in file parses cleanly; a bad built-in panics.
func TestBlackboxDefaults(t *testing.T) {
	d := defaults(t)
	if len(d.Problems) != 0 {
		t.Errorf("built-in has problems: %+v", d.Problems)
	}
	if d.Row == nil || d.Meta == nil || d.AttnRow == nil || d.AttnWhy == nil || d.AttnMeta == nil {
		t.Errorf("a default template is nil: row=%v meta=%v attn_row=%v attn_why=%v attn_meta=%v",
			d.Row != nil, d.Meta != nil, d.AttnRow != nil, d.AttnWhy != nil, d.AttnMeta != nil)
	}
	if d.Ctx != (panelfmt.Thresholds{Warn: 200_000, Alert: 400_000}) {
		t.Errorf("default Ctx = %+v", d.Ctx)
	}
	for _, bad := range []string{"row = '{a'\n", "row = ''\n", "ctx_warn = 2x\n"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Defaults(%q) did not panic", bad)
				}
			}()
			panelfmt.Defaults([]byte(bad), testRegistry(), plainOpts())
		}()
	}
}
