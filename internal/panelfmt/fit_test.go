package panelfmt

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func fitSeg(text string, shrink, min, drop int) Segment {
	return Segment{Parts: []Part{{Text: text}}, Shrink: shrink, Min: min, Drop: drop}
}

func TestTheSpringAbsorbsTheSlack(t *testing.T) {
	tp := mustTpl(t, keyRow, "{a}{>}{b}")
	for _, w := range []int{10, 11, 46, 120} {
		got, _ := tp.Line(w, texts(map[string]string{"a": "left", "b": "right"}))
		if width(got) != w || !strings.HasPrefix(got, "left ") || !strings.HasSuffix(got, " right") {
			t.Errorf("@%d: %q (w=%d)", w, got, width(got))
		}
	}
	// Without a spring a line is not padded.
	if got, _ := mustTpl(t, keyRow, "{a} {b}").Line(46, texts(map[string]string{"a": "left", "b": "right"})); got != "left right" {
		t.Errorf("no spring: %q", got)
	}
}

// TestFittingOrder walks §2.4 steps 2-5 on hand-sized lines.
func TestFittingOrder(t *testing.T) {
	cases := []struct {
		name, tpl string
		segs      map[string]Segment
		width     int
		want      string
	}{
		{"rank 1 shrinks before rank 2", "{a} {b}",
			map[string]Segment{"a": fitSeg("aaaaaaaa", 2, 3, 0), "b": fitSeg("bbbbbbbb", 1, 3, 0)}, 14, "aaaaaaaa bbbb…"},
		{"then rank 2, each to its min", "{a} {b}",
			map[string]Segment{"a": fitSeg("aaaaaaaa", 2, 3, 0), "b": fitSeg("bbbbbbbb", 1, 3, 0)}, 10, "aaaaa… bb…"},
		{"right to left within a rank", "{a} {b}",
			map[string]Segment{"a": fitSeg("aaaaaaaa", 1, 3, 0), "b": fitSeg("bbbbbbbb", 1, 3, 0)}, 14, "aaaaaaaa bbbb…"},
		// a (10) cannot reach 24-15 at its min of 6, so b goes with its " · ";
		// the fit restarts from natural widths and a gives back only one column.
		{"min holds until drops are spent", "{a} · {b} · {c}",
			map[string]Segment{"a": fitSeg("aaaaaaaaaa", 1, 6, 0), "b": fitSeg("bbbbb", 0, 0, 1), "c": fitSeg("ccc", 0, 0, 0)}, 15, "aaaaaaaa… · ccc"},
		{"a drop takes its separator and un-shrinks the rest", "{a} · {b} · {c}",
			map[string]Segment{"a": fitSeg("aaaaaaaaaa", 1, 6, 0), "b": fitSeg("bbbbb", 0, 0, 1), "c": fitSeg("ccc", 0, 0, 0)}, 16, "aaaaaaaaaa · ccc"},
		{"drops go lowest rank first, rightmost first", "{a} {b} {c} {d}",
			map[string]Segment{"a": fitSeg("aa", 0, 0, 0), "b": fitSeg("bb", 0, 0, 2), "c": fitSeg("cc", 0, 0, 1), "d": fitSeg("dd", 0, 0, 1)}, 8, "aa bb cc"},
		{"drops continue only while over", "{a} {b} {c} {d}",
			map[string]Segment{"a": fitSeg("aa", 0, 0, 0), "b": fitSeg("bb", 0, 0, 2), "c": fitSeg("cc", 0, 0, 1), "d": fitSeg("dd", 0, 0, 1)}, 7, "aa bb"},
		{"below min, down to one column", "{a} {b}",
			map[string]Segment{"a": fitSeg("aaaaaaaa", 1, 6, 0), "b": fitSeg("bbbbbbbb", 2, 6, 0)}, 6, "… bbb…"},
		{"rigid content is hard-clipped at the width", "{a} {b}",
			map[string]Segment{"a": seg("aaaaaaaa"), "b": seg("bbbbbbbb")}, 12, "aaaaaaaa bbb"},
		{"labelled: the name gives way to 12 before the label moves", "{name} · {group}",
			map[string]Segment{"name": fitSeg("madbench-workspace-5e", 1, 12, 0), "group": fitSeg("authflow12", 2, 6, 0)}, 25, "madbench-wo… · authflow12"},
		{"labelled: then the label to 6", "{name} · {group}",
			map[string]Segment{"name": fitSeg("madbench-workspace-5e", 1, 12, 0), "group": fitSeg("authflow12", 2, 6, 0)}, 21, "madbench-wo… · authf…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := mustTpl(t, keyRow, c.tpl).Line(c.width, vals(c.segs))
			if got != c.want || width(got) > c.width {
				t.Errorf("@%d → %q (w=%d), want %q", c.width, got, width(got), c.want)
			}
		})
	}
}

// TestAChipIsCutOnItsWholeTextAndReSplit (§2.4): the two halves keep their
// own styles; "…" takes the style of the half where the cut falls.
func TestAChipIsCutOnItsWholeTextAndReSplit(t *testing.T) {
	label := lipgloss.NewStyle().Background(lipgloss.Color("#725025"))
	value := lipgloss.NewStyle().Background(lipgloss.Color("#FFB454"))
	chip := []Part{{Text: " WAIT ", Style: label}, {Text: " 13m ", Style: value}}
	cases := []struct {
		w    int
		want []Part
	}{
		{11, chip},
		{9, []Part{{Text: " WAIT ", Style: label}, {Text: " 1…", Style: value}}},
		{7, []Part{{Text: " WAIT…", Style: label}}},
		{1, []Part{{Text: "…", Style: label}}},
	}
	for _, c := range cases {
		got := truncParts(chip, c.w)
		if len(got) != len(c.want) {
			t.Fatalf("@%d: %+v", c.w, got)
		}
		for i := range got {
			if got[i].Text != c.want[i].Text || got[i].Style.GetBackground() != c.want[i].Style.GetBackground() {
				t.Errorf("@%d part %d: %q, want %q (style kept: %v)", c.w, i, got[i].Text, c.want[i].Text,
					got[i].Style.GetBackground() == c.want[i].Style.GetBackground())
			}
		}
	}
}

// TestWideTextNeverSplitsAGrapheme: CJK and emoji names cut on grapheme
// boundaries, and the spring still makes the line exactly the width.
func TestWideTextNeverSplitsAGrapheme(t *testing.T) {
	tp := mustTpl(t, keyRow, "{name}{>}{ctx}")
	for _, name := range []string{"名前名前名前名前名前", "🚀🚀🚀🚀🚀🚀🚀🚀", "👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧", "a名b🚀c👍🏽d名前"} {
		for w := 7; w <= 26; w++ {
			got, _ := tp.Line(w, vals(map[string]Segment{"name": fitSeg(name, 1, 12, 0), "ctx": seg(" 48k ")}))
			if width(got) != w {
				t.Errorf("%s @%d: %q is %d wide", name, w, got, width(got))
			}
			if !strings.HasSuffix(got, " 48k ") {
				t.Errorf("%s @%d: %q lost the rigid badge", name, w, got)
			}
			head := strings.TrimRight(strings.TrimSuffix(got, " 48k "), " ")
			head = strings.TrimSuffix(head, "…")
			if !strings.HasPrefix(name, head) || !graphemeBoundary(name, len(head)) {
				t.Errorf("%s @%d: %q split a grapheme", name, w, got)
			}
		}
	}
}

// graphemeBoundary reports whether byte off of s falls between graphemes.
func graphemeBoundary(s string, off int) bool {
	for i := 0; i < len(s); {
		if i == off {
			return true
		}
		g, _ := ansi.FirstGraphemeCluster(s[i:], ansi.GraphemeWidth)
		i += len(g)
	}
	return off == len(s)
}

// The property tests draw from clusters whose width x/ansi and a terminal
// agree on: ASCII, precomposed Latin, CJK, emoji with a skin tone and a ZWJ
// family. Every drawn line must decompose back into these (plus the engine's
// "…" and the literals), which proves no grapheme was split.
var (
	propClusters = []string{"a", "b", "x", "Z", "-", "é", "ñ", "名", "前", "日", "本", "🚀", "👍🏽", "👨‍👩‍👧", "▌"}
	propLiterals = []string{" · ", " ", "", "(", ") ", "    ", "名 ", " | "}
	propTokens   = []string{"a", "b", "c", "d", "w", "p"}
)

func propText(rng *rand.Rand) string {
	var b strings.Builder
	for w := range rng.IntN(5) {
		if w > 0 {
			b.WriteByte(' ')
		}
		for range 1 + rng.IntN(8) {
			b.WriteString(propClusters[rng.IntN(len(propClusters))])
		}
	}
	return b.String()
}

func propSegment(rng *rand.Rand) Segment {
	if rng.IntN(5) == 0 {
		return Segment{Empty: true}
	}
	s := Segment{Shrink: rng.IntN(4), Min: rng.IntN(16), Drop: rng.IntN(3)}
	for range 1 + rng.IntN(2) {
		s.Parts = append(s.Parts, Part{Text: propText(rng)})
	}
	return s
}

// propTemplate builds a random template: tokens and literals, at most one
// spring, and for attn_why maybe a trailing {why:wrap}.
func propTemplate(rng *rand.Rand, wrap bool) string {
	var b strings.Builder
	n := 1 + rng.IntN(6)
	spring := -1
	if rng.IntN(2) == 0 {
		spring = rng.IntN(n + 1)
	}
	for i := 0; i <= n; i++ {
		if i == spring {
			b.WriteString("{>}")
		}
		b.WriteString(propLiterals[rng.IntN(len(propLiterals))])
		if i < n {
			fmt.Fprintf(&b, "{%s}", propTokens[rng.IntN(len(propTokens))])
		}
	}
	if wrap {
		b.WriteString(fmt.Sprintf("{why:wrap=%d}", 1+rng.IntN(2)))
		if rng.IntN(3) == 0 {
			b.WriteString(")")
		}
	}
	return b.String()
}

// decomposes reports whether s is made only of the property alphabet.
func decomposes(s string) bool {
	alpha := slices.Concat(propClusters, []string{"…", " ", "·", "(", ")", "|", "{", "}", "?"})
	slices.SortFunc(alpha, func(a, b string) int { return len(b) - len(a) })
	for s != "" {
		ok := false
		for _, a := range alpha {
			if a != "" && strings.HasPrefix(s, a) {
				s, ok = s[len(a):], true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// TestPropertyEveryLineFitsItsWidth: random templates and segments, W in
// 10..120, both colour profiles. Every line from Line, One and Wrap is at
// most W display columns, a spring line is W (W-1 only when the final clip
// met a double-width grapheme), no grapheme is split, and the text is valid
// UTF-8.
func TestPropertyEveryLineFitsItsWidth(t *testing.T) {
	for _, profile := range []termenv.Profile{termenv.Ascii, termenv.TrueColor} {
		lipgloss.SetColorProfile(profile)
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("#89B4FA")).Background(lipgloss.Color("#30394A"))
		opts := Options{LiteralStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("#7A7F9A"))}
		rng := rand.New(rand.NewPCG(1, uint64(profile)))
		for iter := range 3000 {
			wrap := iter%3 == 0
			key := keyRow
			if wrap {
				key = keyAttnWhy
			}
			src := propTemplate(rng, wrap)
			tp, probs := parseTemplate(src, identity, 1, key, testReg)
			if tp == nil || len(probs) > 0 {
				t.Fatalf("template %q: %v", src, probs)
			}
			tp = withOpts(tp, opts)
			segs := map[string]Segment{}
			for _, name := range append(slices.Clone(propTokens), "why") {
				s := propSegment(rng)
				for i := range s.Parts {
					s.Parts[i].Style = style
				}
				segs[name] = s
			}
			r := vals(segs)
			w := 10 + rng.IntN(111)

			check := func(kind, line string) {
				t.Helper()
				plain := ansi.Strip(line)
				switch {
				case width(line) > w:
					t.Fatalf("%s %q @%d: %q is %d wide", kind, src, w, plain, width(line))
				case !utf8.ValidString(plain) || !decomposes(plain):
					t.Fatalf("%s %q @%d: %q split a grapheme", kind, src, w, plain)
				}
			}
			line, ok := tp.Line(w, r)
			check("Line", line)
			if ok && tp.spring && width(line) < w-1 {
				t.Fatalf("Line %q @%d: spring line %q is %d wide", src, w, ansi.Strip(line), width(line))
			}
			if one, ok1 := tp.One(w, r); ok1 != ok || one != line {
				t.Fatalf("One and Line disagree on %q @%d: %q/%v vs %q/%v", src, w, one, ok1, line, ok)
			}
			lines := tp.Wrap(w, 2, r)
			if (len(lines) == 0) == ok || len(lines) > 2 {
				t.Fatalf("Wrap %q @%d: %d lines, Line ok=%v", src, w, len(lines), ok)
			}
			for _, l := range lines {
				check("Wrap", l)
			}
		}
	}
	lipgloss.SetColorProfile(termenv.Ascii)
}
