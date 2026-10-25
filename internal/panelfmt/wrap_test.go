package panelfmt

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

const migration = "confirm the migration plan before it touches the prod replica (step 4 of 6)"

func spanTexts(text string, spans []span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = text[s.start:s.end] + s.tail
	}
	return out
}

func TestWrapSpans(t *testing.T) {
	cases := []struct {
		name, text string
		limit, max int
		want       []string
		more       bool
	}{
		{"two lines, mockup", migration, 42, 2, []string{"confirm the migration plan before it", "touches the prod replica (step 4 of 6)"}, false},
		{"fits on one", "short why", 42, 2, []string{"short why"}, false},
		{"left over", migration, 20, 2, []string{"confirm the", "migration plan"}, true},
		{"one line", migration, 20, 1, []string{"confirm the"}, true},
		{"a word too long is hard-broken", "abcdefghijkl mn", 5, 4, []string{"abcde", "fghij", "kl mn"}, false},
		{"hard break leaves more", "abcdefghijkl", 5, 2, []string{"abcde", "fghij"}, true},
		{"spaces are break points only", "  a   b  ", 3, 2, []string{"a", "b"}, false},
		{"wide text breaks on graphemes", "名前名前名前", 5, 3, []string{"名前", "名前", "名前"}, false},
		{"emoji ZWJ stays whole", "👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧", 5, 3, []string{"👨‍👩‍👧👨‍👩‍👧", "👨‍👩‍👧"}, false},
		{"empty", "", 10, 2, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spans, more := wrapSpans(c.text, c.limit, c.max)
			got := spanTexts(c.text, spans)
			if len(got) == 0 && len(c.want) == 0 {
				got = c.want
			}
			if !reflect.DeepEqual(got, c.want) || more != c.more {
				t.Errorf("got %q more=%v, want %q more=%v", got, more, c.want, c.more)
			}
		})
	}
}

func TestEllipsize(t *testing.T) {
	text := "migration plan"
	if got := spanTexts(text, []span{ellipsize(text, span{start: 0, end: 9}, 20)}); got[0] != "migration…" {
		t.Errorf("room for the tail: %q", got[0])
	}
	if got := spanTexts(text, []span{ellipsize(text, span{start: 0, end: 14}, 14)}); got[0] != "migration…" {
		t.Errorf("no room: %q, want the last word given up", got[0])
	}
	long := "a supercalifragilistic"
	if got := spanTexts(long, []span{ellipsize(long, span{start: 0, end: len(long)}, 22)}); got[0] != "a supercalifragilisti…" {
		t.Errorf("one long word: %q, want a grapheme cut (\"a…\" would keep too little)", got[0])
	}
}

// TestTheAttentionWhyWraps: the §6.7 mockup lines, continuation indented to
// the wrap token's column, "…" when text is left over, and the line count
// capped by both wrap=N and the caller's max.
func TestTheAttentionWhyWraps(t *testing.T) {
	why := texts(map[string]string{"why": migration})
	d := defaults(t)

	got := d.AttnWhy.Wrap(46, 2, why)
	want := []string{"    confirm the migration plan before it", "    touches the prod replica (step 4 of 6)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mockup @46:\n got %q\nwant %q", got, want)
	}

	got = d.AttnWhy.Wrap(30, 2, why)
	want = []string{"    confirm the migration plan", "    before it touches the…"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("left over @30:\n got %q\nwant %q", got, want)
	}

	if got := d.AttnWhy.Wrap(30, 1, why); !reflect.DeepEqual(got, []string{"    confirm the migration…"}) {
		t.Errorf("max 1: %q", got)
	}
	one := mustTpl(t, keyAttnWhy, "    {why:wrap=1}")
	if got := one.Wrap(30, 2, why); len(got) != 1 {
		t.Errorf("wrap=1 with max 2: %q", got)
	}
	if got := d.AttnWhy.Wrap(46, 0, why); got != nil {
		t.Errorf("max 0: %q", got)
	}
}

func TestWrapPrefixSpringAndTrailingLiteral(t *testing.T) {
	cases := []struct {
		name, tpl string
		width     int
		want      []string
	}{
		{"prefix token sets the indent", "{a}: {why:wrap}", 20,
			[]string{"db: confirm the", "    migration plan…"}},
		{"trailing literal ends the last line", `    "{why:wrap}"`, 26,
			[]string{`    "confirm the`, `     migration plan…"`}},
		{"spring takes one column", "{a}{>}{why:wrap}", 18,
			[]string{"db confirm the", "   migration plan…"}},
		{"wrap token left of a token-less right part", "{why:wrap}{>}|", 14,
			[]string{"confirm the", "migration… |"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustTpl(t, keyAttnWhy, c.tpl).Wrap(c.width, 2, texts(map[string]string{"a": "db", "why": migration}))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n got %q\nwant %q", got, c.want)
			}
			for _, l := range got {
				if width(l) > c.width {
					t.Errorf("%q is %d wide", l, width(l))
				}
			}
		})
	}
}

// TestWrapFallsBackToOneLine: an Empty or unhonoured wrap token, or no room
// for it, gives the single line; an all-Empty template gives no line.
func TestWrapFallsBackToOneLine(t *testing.T) {
	r := texts(map[string]string{"a": "db", "why": migration})
	if got := mustTpl(t, keyAttnWhy, "{a} {why:wrap}").Wrap(40, 2, texts(map[string]string{"a": "db"})); !reflect.DeepEqual(got, []string{"db"}) {
		t.Errorf("empty why: %q", got)
	}
	if got := mustTpl(t, keyAttnWhy, "    {why:wrap}").Wrap(40, 2, texts(nil)); got != nil {
		t.Errorf("all empty: %q", got)
	}
	meta, _ := parseTemplate("    {why:wrap}", identity, 1, keyMeta, testReg)
	if got := meta.Wrap(30, 2, r); !reflect.DeepEqual(got, []string{"    confirm the migration pla…"}) {
		t.Errorf("unhonoured wrap: %q", got)
	}
	narrow := mustTpl(t, keyAttnWhy, "{a}-long-prefix {why:wrap}")
	if got := narrow.Wrap(12, 2, r); len(got) != 1 || width(got[0]) > 12 {
		t.Errorf("no room: %q", got)
	}
}

// TestOneTruncatesTheWhy: the ladder's single-line form cuts the wrap token
// even when its token function left it rigid.
func TestOneTruncatesTheWhy(t *testing.T) {
	d := defaults(t)
	got, ok := d.AttnWhy.One(20, texts(map[string]string{"why": migration}))
	if !ok || got != "    confirm the mig…" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	if _, ok := d.AttnWhy.One(20, texts(nil)); ok {
		t.Error("One exists when Line would not")
	}
}

// TestWrappedLinesKeepTheSegmentsStyles: a two-part why keeps each part's
// style across the break.
func TestWrappedLinesKeepTheSegmentsStyles(t *testing.T) {
	a := lipgloss.NewStyle().Foreground(lipgloss.Color("#CDD6F4"))
	b := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4D5E"))
	withTrueColor(t, func() {
		r := vals(map[string]Segment{"why": {Parts: []Part{{Text: "needs you: ", Style: a}, {Text: "confirm the plan", Style: b}}}})
		got := mustTpl(t, keyAttnWhy, "{why:wrap}").Wrap(18, 2, r)
		want := []string{a.Render("needs you: ") + b.Render("confirm"), b.Render("the plan")}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("\n got %q\nwant %q", got, want)
		}
		if strings.Contains(got[1], "\x1b[38;2;205;214;244m") {
			t.Error("the first part's style leaked into the second line")
		}
	})
}
