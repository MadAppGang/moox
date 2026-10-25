package panelfmt_test

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// REQ-15: shrink rank ascending wins over position; right-to-left within a rank.
func TestBlackboxFitShrinkOrder(t *testing.T) {
	withProfile(t, termenv.Ascii)
	tpl := rowTemplate(t, "{a} {b}")
	for _, tc := range []struct {
		name         string
		rankA, rankB int
		want         string
	}{
		{"lower rank on the right shrinks first", 2, 1, "aaaaaaaa bbbbb…"},
		{"lower rank on the left shrinks first", 1, 2, "aaaaa… bbbbbbbb"},
		{"same rank: right-to-left", 1, 1, "aaaaaaaa bbbbb…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := segs{"a": shrinkable("aaaaaaaa", tc.rankA, 2), "b": shrinkable("bbbbbbbb", tc.rankB, 2)}.resolver()
			got, _ := line(t, tpl, 15, r)
			if plain(got) != tc.want {
				t.Errorf("W=15 renders %q; want %q", plain(got), tc.want)
			}
		})
	}
}

// REQ-15: segments stop at their min, then droppable segments go whole.
func TestBlackboxFitMinThenDrop(t *testing.T) {
	withProfile(t, termenv.Ascii)
	c := text("cc")
	c.Drop = 1
	r := segs{"a": shrinkable("aaaaaaaa", 1, 6), "b": shrinkable("bbbbbbbb", 2, 6), "c": c}.resolver()
	got, _ := line(t, rowTemplate(t, "{a} {b} {c}"), 14, r)
	p := plain(got)
	if strings.Contains(p, "c") {
		t.Errorf("renders %q; the droppable segment should be gone", p)
	}
	if !strings.HasPrefix(p, "aaaaa") || !strings.Contains(p, " bbbbb") {
		t.Errorf("renders %q; a and b must keep at least their min of 6 columns", p)
	}
	// Without a droppable segment the same squeeze keeps c (it is rigid) and
	// takes both shrinkers to their minimum first.
	got, _ = line(t, rowTemplate(t, "{a} {b} {c}"), 16, segs{"a": shrinkable("aaaaaaaa", 1, 6), "b": shrinkable("bbbbbbbb", 2, 6), "c": text("cc")}.resolver())
	if want := "aaaaa… bbbbb… cc"; plain(got) != want {
		t.Errorf("W=16 renders %q; want %q (rank 1 to its min, then rank 2 to its min)", plain(got), want)
	}
}

// REQ-15: past every min, shrinkers go down to one column; rigid overflow is
// hard-clipped to exactly W.
func TestBlackboxFitBelowMinAndHardClip(t *testing.T) {
	withProfile(t, termenv.Ascii)
	got, _ := line(t, rowTemplate(t, "{a} {b}"), 10, segs{"a": shrinkable("aaaaaaaa", 1, 6), "b": text("bbbbbbbb")}.resolver())
	if plain(got) != "… bbbbbbbb" {
		t.Errorf("below min renders %q; want %q", plain(got), "… bbbbbbbb")
	}
	got, _ = line(t, rowTemplate(t, "{a} {b}"), 10, segs{"a": text("aaaaaaaa"), "b": text("bbbbbbbb")}.resolver())
	if plain(got) != "aaaaaaaa b" || width(got) != 10 {
		t.Errorf("hard clip renders %q (%d cols); want %q", plain(got), width(got), "aaaaaaaa b")
	}
}

// REQ-15: display columns, never bytes; wide glyphs and grapheme clusters are
// never split and never push the line past W.
func TestBlackboxFitWideGlyphs(t *testing.T) {
	withProfile(t, termenv.Ascii)
	tpl := rowTemplate(t, "{a}{>}{b}")
	for w := 4; w <= 30; w++ {
		got, _ := line(t, tpl, w, segs{"a": shrinkable("名前名前名前名前", 1, 2), "b": text("48k")}.resolver())
		p := plain(got)
		if strings.ContainsRune(p, utf8.RuneError) {
			t.Errorf("W=%d: invalid UTF-8 in %q", w, p)
		}
		if w >= 10 && (!strings.HasSuffix(p, "48k") || !strings.HasPrefix(p, "名前")) {
			t.Errorf("W=%d: renders %q; want the CJK name cut, the badge kept", w, p)
		}
		if w >= 10 && w < 20 && !strings.Contains(p, "…") {
			t.Errorf("W=%d: a cut name must end in …: %q", w, p)
		}
	}
	for w := 1; w <= 14; w++ {
		for _, tc := range []struct{ name, s string }{
			{"skin tone", strings.Repeat("👍🏽", 6)},
			{"flags", strings.Repeat("🇺🇦", 6)},
			{"rocket", strings.Repeat("🚀", 6)},
			{"rigid CJK", "名前名前名前"},
		} {
			seg := shrinkable(tc.s, 1, 1)
			if tc.name == "rigid CJK" {
				seg = text(tc.s)
			}
			got, _ := line(t, rowTemplate(t, "{a}"), w, segs{"a": seg}.resolver())
			p := plain(got)
			if strings.Count(p, "👍") != strings.Count(p, "👍🏽") {
				t.Errorf("%s W=%d: a skin-tone cluster was split: %q", tc.name, w, p)
			}
			ri := 0
			for _, r := range p {
				if r >= 0x1F1E6 && r <= 0x1F1FF {
					ri++
				}
			}
			if ri%2 != 0 {
				t.Errorf("%s W=%d: a flag was split: %q", tc.name, w, p)
			}
		}
	}
}

// REQ-15: a two-part chip truncates on its concatenated text and keeps both
// halves' styles.
func TestBlackboxFitTwoPartChip(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	chip := panelfmt.Segment{Parts: []panelfmt.Part{
		{Text: "ABCD", Style: lipgloss.NewStyle().Background(lipgloss.Color("#FF0000"))},
		{Text: "EFGH", Style: lipgloss.NewStyle().Background(lipgloss.Color("#0000FF"))},
	}, Shrink: 1, Min: 2}
	got, _ := line(t, rowTemplate(t, "{a}"), 6, segs{"a": chip}.resolver())
	if plain(got) != "ABCDE…" {
		t.Errorf("chip at W=6 renders %q; want %q", plain(got), "ABCDE…")
	}
	cs := cells(got)
	if c := cellOf(t, cs, 'A'); !sameRGB(c.bg, "#FF0000") {
		t.Errorf("first half background %+v; want #FF0000", c.bg)
	}
	if c := cellOf(t, cs, 'E'); !sameRGB(c.bg, "#0000FF") {
		t.Errorf("second half background %+v; want #0000FF", c.bg)
	}
	full, _ := line(t, rowTemplate(t, "{a}"), 20, segs{"a": chip}.resolver())
	if plain(full) != "ABCDEFGH" {
		t.Errorf("chip with room renders %q; want ABCDEFGH (no divider between halves)", plain(full))
	}
}

// REQ-15, REQ-16: property — whatever the segments, nothing exceeds W.
func TestBlackboxFitNeverExceedsWidthProperty(t *testing.T) {
	withProfile(t, termenv.Ascii)
	rows := []*panelfmt.Template{
		rowTemplate(t, "{a} · {b} · {c}"),
		rowTemplate(t, "{a}{>}{b}"),
		rowTemplate(t, "{gutter}{status} {name} · {group}{>}{ctx}"),
		rowTemplate(t, "    {w} · {p} · {a}"),
		rowTemplate(t, "{a} ({b}){>}{c} {w}"),
		rowTemplate(t, "[{a:shrink=2:min=3}] {b:drop=1} {c:max=5}{>}{p:dim}"),
	}
	whys := []*panelfmt.Template{
		clean(t, "attn_why = '    {why:wrap=2}'\n").AttnWhy,
		clean(t, "attn_why = '{a} · {why:wrap=2}'\n").AttnWhy,
		clean(t, "attn_why = '{why:wrap=1}'\n").AttnWhy,
	}
	pool := []string{"a", "b", "Z", "名", "前", "🚀", "é", "·", "-", " ", "👍🏽", "🇺🇦", "…", "x"}
	rng := rand.New(rand.NewPCG(20260925, 7))
	randText := func() string {
		var b strings.Builder
		for n := rng.IntN(40); n > 0; n-- {
			b.WriteString(pool[rng.IntN(len(pool))])
		}
		return strings.TrimSpace(b.String())
	}
	randSeg := func() panelfmt.Segment {
		txt := randText()
		if txt == "" {
			return panelfmt.Segment{Empty: true}
		}
		seg := panelfmt.Segment{Shrink: rng.IntN(4), Min: rng.IntN(16), Drop: rng.IntN(3)}
		if cut := rng.IntN(3); cut == 0 && utf8.RuneCountInString(txt) > 1 {
			rs := []rune(txt)
			k := 1 + rng.IntN(len(rs)-1)
			seg.Parts = []panelfmt.Part{{Text: string(rs[:k])}, {Text: string(rs[k:])}}
		} else {
			seg.Parts = []panelfmt.Part{{Text: txt}}
		}
		return seg
	}
	names := []string{"a", "b", "c", "w", "p", "gutter", "status", "name", "group", "ctx", "why"}
	for i := 0; i < 3000; i++ {
		m := segs{}
		for _, n := range names {
			m[n] = randSeg()
		}
		w := 10 + rng.IntN(111)
		r := m.resolver()
		for _, tpl := range rows {
			if out, _ := tpl.Line(w, r); width(out) > w {
				t.Fatalf("case %d W=%d: Line is %d columns: %q", i, w, width(out), plain(out))
			}
		}
		for _, tpl := range whys {
			ls := tpl.Wrap(w, 2, r)
			if len(ls) > 2 {
				t.Fatalf("case %d: Wrap returned %d lines", i, len(ls))
			}
			for _, l := range ls {
				if width(l) > w {
					t.Fatalf("case %d W=%d: Wrap line is %d columns: %q", i, w, width(l), plain(l))
				}
			}
			if one, _ := tpl.One(w, r); width(one) > w {
				t.Fatalf("case %d W=%d: One is %d columns: %q", i, w, width(one), plain(one))
			}
		}
	}
}
