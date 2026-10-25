package panelfmt_test

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// The §6.7 mockup's why text and its two wrapped lines at 46 columns.
const mockWhy = "confirm the migration plan before it touches the prod replica (step 4 of 6)"

var mockLines = []string{
	"    confirm the migration plan before it",
	"    touches the prod replica (step 4 of 6)",
}

func whyTemplate(t *testing.T, tpl string) *panelfmt.Template {
	t.Helper()
	s := clean(t, "attn_why = '"+tpl+"'\n")
	if s.AttnWhy == nil {
		t.Fatal("AttnWhy is nil")
	}
	return s.AttnWhy
}

func wrapped(t *testing.T, tpl *panelfmt.Template, w, max int, why string) []string {
	t.Helper()
	ls := tpl.Wrap(w, max, vals{"why": why, "a": "A"}.resolver())
	out := make([]string, len(ls))
	for i, l := range ls {
		if width(l) > w {
			t.Errorf("wrap line %d is %d columns > W=%d: %q", i, width(l), w, plain(l))
		}
		if strings.Contains(l, "\n") {
			t.Errorf("wrap line %d contains a newline: %q", i, plain(l))
		}
		out[i] = strings.TrimRight(plain(l), " ")
	}
	return out
}

// REQ-16: word wrap by display width reproduces the mockup exactly.
func TestBlackboxWrapMockupAt46(t *testing.T) {
	withProfile(t, termenv.Ascii)
	got := wrapped(t, whyTemplate(t, "    {why:wrap=2}"), 46, 2, mockWhy)
	if strings.Join(got, "\n") != strings.Join(mockLines, "\n") {
		t.Errorf("wrap at 46:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(mockLines, "\n"))
	}
}

// REQ-16: text left over after N lines ends the last line in "…".
func TestBlackboxWrapOverflowEndsInEllipsis(t *testing.T) {
	withProfile(t, termenv.Ascii)
	got := wrapped(t, whyTemplate(t, "    {why:wrap=2}"), 46, 2, mockWhy+" "+mockWhy+" "+mockWhy)
	if len(got) != 2 {
		t.Fatalf("got %d lines; want 2: %q", len(got), got)
	}
	if got[0] != mockLines[0] {
		t.Errorf("first line %q; want %q", got[0], mockLines[0])
	}
	if !strings.HasSuffix(got[1], "…") {
		t.Errorf("last line %q does not end in …", got[1])
	}
}

// REQ-16: a single word longer than the room is hard-broken, never overflowing.
func TestBlackboxWrapHardBreaksALongWord(t *testing.T) {
	withProfile(t, termenv.Ascii)
	got := wrapped(t, whyTemplate(t, "    {why:wrap=2}"), 20, 2, "a supercalifragilisticexpialidociousness")
	if len(got) == 0 || len(got) > 2 {
		t.Fatalf("got %d lines; want 1..2: %q", len(got), got)
	}
	joined := strings.Join(got, "")
	if !strings.Contains(joined, "supercal") {
		t.Errorf("the long word vanished: %q", got)
	}
	for _, l := range got {
		if !strings.HasPrefix(l, "    ") {
			t.Errorf("line %q lost the indent", l)
		}
	}
}

// REQ-16: the line cap is min(N, max); a bare wrap means N = 2.
func TestBlackboxWrapLineCaps(t *testing.T) {
	withProfile(t, termenv.Ascii)
	one := wrapped(t, whyTemplate(t, "    {why:wrap=2}"), 46, 1, mockWhy)
	if len(one) != 1 || !strings.HasSuffix(one[0], "…") {
		t.Errorf("max=1: %q; want one line ending in …", one)
	}
	n1 := wrapped(t, whyTemplate(t, "    {why:wrap=1}"), 46, 2, mockWhy)
	if len(n1) != 1 || !strings.HasSuffix(n1[0], "…") {
		t.Errorf("wrap=1: %q; want one line ending in …", n1)
	}
	bare := wrapped(t, whyTemplate(t, "    {why:wrap}"), 46, 2, mockWhy)
	if strings.Join(bare, "\n") != strings.Join(mockLines, "\n") {
		t.Errorf("bare wrap (default 2): %q; want the two mockup lines", bare)
	}
	short := wrapped(t, whyTemplate(t, "    {why:wrap=2}"), 46, 2, "permission-prompt")
	if len(short) != 1 || short[0] != "    permission-prompt" {
		t.Errorf("short why: %q; want one line", short)
	}
}

// REQ-16: continuation lines are indented to the wrap token's start column.
func TestBlackboxWrapContinuationIndent(t *testing.T) {
	withProfile(t, termenv.Ascii)
	got := wrapped(t, whyTemplate(t, "  >> {why:wrap=2}"), 46, 2, mockWhy)
	if len(got) != 2 {
		t.Fatalf("got %d lines; want 2: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "  >> confirm") {
		t.Errorf("first line %q", got[0])
	}
	if !strings.HasPrefix(got[1], strings.Repeat(" ", 5)) || got[1][5] == ' ' {
		t.Errorf("continuation %q; want exactly 5 columns of indent (the token's column)", got[1])
	}
}

// REQ-16: nothing to say → no lines; One reports an EMPTY line.
func TestBlackboxWrapEmptyWhy(t *testing.T) {
	withProfile(t, termenv.Ascii)
	tpl := whyTemplate(t, "    {why:wrap=2}")
	if ls := tpl.Wrap(46, 2, vals{}.resolver()); len(ls) != 0 {
		t.Errorf("empty why: Wrap = %q; want no lines", ls)
	}
	if one, ok := tpl.One(46, vals{}.resolver()); ok {
		t.Errorf("empty why: One = %q, true; want false", plain(one))
	}
}

// REQ-16: One is the why on a single truncated line.
func TestBlackboxOneTruncatesToOneLine(t *testing.T) {
	withProfile(t, termenv.Ascii)
	tpl := whyTemplate(t, "    {why:wrap=2}")
	one, ok := tpl.One(46, vals{"why": mockWhy}.resolver())
	p := plain(one)
	if !ok || strings.Contains(p, "\n") || width(one) > 46 {
		t.Fatalf("One = %q (ok=%v, %d cols)", p, ok, width(one))
	}
	if !strings.HasPrefix(p, "    confirm the migration") || !strings.HasSuffix(strings.TrimRight(p, " "), "…") {
		t.Errorf("One = %q; want the indented why cut with …", p)
	}
	short, ok := tpl.One(46, vals{"why": "permission-prompt"}.resolver())
	if !ok || strings.TrimRight(plain(short), " ") != "    permission-prompt" {
		t.Errorf("short One = %q (ok=%v)", plain(short), ok)
	}
}

// REQ-16, REQ-11: wrap outside attn_why, not last, or twice is a soft problem;
// out of place it renders as a single line.
func TestBlackboxWrapMisuseIsSoft(t *testing.T) {
	withProfile(t, termenv.Ascii)
	s := parse(t, "row = '{why:wrap=2}'\n")
	if !softOnly(s) {
		t.Fatalf("wrap in row: problems %+v; want soft only", s.Problems)
	}
	got, ok := line(t, s.Row, 20, vals{"why": mockWhy}.resolver())
	if p := plain(got); !ok || strings.Contains(p, "\n") || !strings.HasPrefix(p, "confirm") {
		t.Errorf("wrap in row renders %q (ok=%v); want the why on one line", p, ok)
	}
	for _, src := range []string{
		"attn_why = '{why:wrap=2} {b}'\n",
		"attn_why = '{needs:wrap=1} {why:wrap=2}'\n",
		"attn_why = '    {why:wrap=3}'\n",
		"attn_why = '    {why:wrap=0}'\n",
	} {
		if s := parse(t, src); !softOnly(s) {
			t.Errorf("%q: problems %+v; want soft only", src, s.Problems)
		}
	}
}
