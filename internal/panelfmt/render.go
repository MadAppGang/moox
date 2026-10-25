package panelfmt

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// slot is one token resolved for one row.
type slot struct {
	seg   Segment // after the common options; Parts are the engine's own copy
	w     int     // natural display width
	empty bool    // nothing to say: collapses with its separator (§2.3)
}

// piece is one drawn element of a collapsed part: a literal, or a slot.
type piece struct {
	lit  string
	w    int // literal width
	slot int // index into the slots, or -1 for a literal
}

// Line renders the template on one line of at most width display columns.
// With a spring the line is exactly width wide; without one it is not padded.
// It reports false when every token (the spring excepted) is Empty: the line
// does not exist and contributes zero lines to the frame. A template without
// any token is only literals and always draws them.
//
// A wrap token rendered on one line shrinks (rank 1 unless it set a rank),
// since its text is the line's body.
func (t *Template) Line(width int, r Resolver) (string, bool) {
	slots := t.resolve(r)
	if t.isEmpty(slots) {
		return "", false
	}
	return t.fit(width, t.oneLine(slots)), true
}

// One renders attn_why on a single truncated line, for the ladder rungs that
// cannot afford the wrapped form. It MUST stay identical to Line: it is part
// of the frozen API (types.go) that callers outside this package build on, and
// it is kept as a name for that use, not as a second rendering. It is a call
// to Line and nothing else, so the two cannot drift; the fuzz in fit_test.go
// checks every input against both.
func (t *Template) One(width int, r Resolver) (string, bool) {
	return t.Line(width, r)
}

// Wrap renders attn_why across 0..max lines: everything before the wrap token
// on the first line, the wrapped text word-broken by display width, its
// continuation lines indented to the wrap token's start column, and "…" on the
// last line when text is left over (§2.5). Zero lines means the line is EMPTY.
//
// The line count is min(max, the token's wrap=N). A template whose wrap token
// is not honoured (or is Empty for this row) renders as its single Line. A
// trailing literal after the wrap token ends the last line; its width is
// reserved on every line. With a spring, the spring takes its minimum of one
// column here, since the wrapped text is what fills the slack. When the text
// before the wrap token leaves no column for it, the result is the One line.
func (t *Template) Wrap(width, max int, r Resolver) []string {
	if max <= 0 {
		return nil
	}
	slots := t.resolve(r)
	if t.isEmpty(slots) {
		return nil
	}
	wi := t.wrapSlot()
	if wi < 0 || slots[wi].empty {
		return []string{t.fit(width, t.oneLine(slots))}
	}

	// Flatten the parts, the spring at its one-column minimum, and split the
	// line at the wrap token: what comes before it heads the first line, what
	// comes after it (a trailing literal) ends the last.
	var flat []piece
	for pi, part := range t.collapse(func(i int) bool { return slots[i].empty }) {
		if pi > 0 {
			flat = append(flat, litPiece(" "))
		}
		flat = append(flat, part...)
	}
	k := 0
	for j, pc := range flat {
		if pc.slot == wi {
			k = j
		}
	}
	prefix, trail := flat[:k], flat[k+1:]
	widths := naturalWidths(slots)
	pw, tw := t.measure(prefix, widths), t.measure(trail, widths)
	avail := width - pw - tw

	seg := slots[wi].seg
	text := joinParts(seg.Parts)
	var spans []span
	more := false
	if avail >= 1 {
		spans, more = wrapSpans(text, avail, min(max, t.wrap))
	}
	if len(spans) == 0 {
		return []string{t.fit(width, t.oneLine(slots))}
	}
	if more {
		spans[len(spans)-1] = ellipsize(text, spans[len(spans)-1], avail)
	}

	head, _ := t.paintPieces(prefix, slots, widths)
	indent := strings.Repeat(" ", pw)
	tail, _ := t.paintPieces(trail, slots, widths)
	out := make([]string, len(spans))
	for n, sp := range spans {
		var b strings.Builder
		if n == 0 {
			b.WriteString(head)
		} else {
			b.WriteString(indent)
		}
		b.WriteString(paintParts(sliceParts(seg.Parts, sp.start, sp.end, sp.tail)))
		if n == len(spans)-1 {
			b.WriteString(tail)
		}
		out[n] = clip(b.String(), width)
	}
	return out
}

// resolve turns every token into a slot: an unknown one into the in-place
// error segment, a known one through the Resolver and then the common
// options (§2.6).
func (t *Template) resolve(r Resolver) []slot {
	slots := make([]slot, len(t.toks))
	for i, tn := range t.toks {
		var seg Segment
		switch {
		case !tn.Known:
			seg = Segment{Parts: []Part{{Text: "{" + tn.Raw + "?}", Style: t.opts.ErrorStyle}}}
		case r == nil:
			seg = Segment{Empty: true}
		default:
			seg = r(tn.Token)
		}
		seg = applyCommon(seg, tn.com, t.opts)
		w := ansi.StringWidth(joinParts(seg.Parts))
		// A segment that draws nothing is Empty whatever it claims: drawing
		// it would leave its separators around a hole ("a ·  · c").
		slots[i] = slot{seg: seg, w: w, empty: seg.Empty || w == 0}
	}
	return slots
}

// oneLine makes a wrap token shrinkable for the single-line forms.
func (t *Template) oneLine(slots []slot) []slot {
	for i, tn := range t.toks {
		if tn.hasWrap && slots[i].seg.Shrink == 0 {
			slots[i].seg.Shrink = 1
		}
	}
	return slots
}

// isEmpty is rule 2 of §2.3: the line exists unless it has tokens and every
// one of them is Empty.
func (t *Template) isEmpty(slots []slot) bool {
	if len(slots) == 0 {
		return false
	}
	for _, s := range slots {
		if !s.empty {
			return false
		}
	}
	return true
}

// wrapSlot is the honoured wrap token's index (the last token), or -1.
func (t *Template) wrapSlot() int {
	if t.wrap == 0 {
		return -1
	}
	return len(t.toks) - 1
}

// applyCommon applies the common options the token did not claim for itself.
// It always copies Parts, so a Resolver may return a shared slice, and
// replaces any control character left in the text: an ESC must never reach
// the terminal, and the width must measure what is drawn.
func applyCommon(seg Segment, c common, opts Options) Segment {
	parts := make([]Part, len(seg.Parts))
	for i, p := range seg.Parts {
		p.Text = cleanText(p.Text)
		if c.plain {
			p.Style = p.Style.UnsetForeground().UnsetBackground()
		}
		if c.dim {
			p.Style = p.Style.Foreground(opts.DimStyle.GetForeground())
		}
		parts[i] = p
	}
	seg.Parts = parts
	if c.hasMax && ansi.StringWidth(joinParts(parts)) > c.max {
		seg.Parts = truncParts(parts, c.max)
	}
	if c.hasMin {
		seg.Min = c.min
	}
	if c.hasShrink {
		seg.Shrink = c.shrink
	}
	if c.hasDrop {
		seg.Drop = c.drop
	}
	return seg
}

// collapse applies §2.3 rules 3-5 to every part: gone reports a token that
// draws nothing (Empty, or dropped by the fitter).
func (t *Template) collapse(gone func(int) bool) [][]piece {
	out := make([][]piece, len(t.parts))
	for pi, nodes := range t.parts {
		out[pi] = collapsePart(nodes, gone)
	}
	return out
}

// collapsePart draws one part. A part's leading literal is drawn only before
// its first drawn token; a gap only between two drawn tokens, choosing the
// first non-whitespace one among the gaps it bridges (#14); the trailing
// literal only when the part's last token is drawn (#23). A part with no
// token at all draws its literals as they are.
//
// Brackets belong to the token they touch (§2.3 rule 7): the opening brackets
// that end a literal ("(" in " (") open the token after it, and the closing
// brackets that start one (")" in ") · ") close the token before it. They are
// drawn exactly when that token is, so "{a} ({b})" is "(b)" without a, "a"
// without b, and "{a} ({b}) · {c}" is "a · c" without b — never "b)" or
// "a (c". With every token drawn a gap is drawn whole, as before.
func collapsePart(nodes []node, gone func(int) bool) []piece {
	var (
		toks  []int    // token indexes, in order
		gaps  []string // gaps[i] sits before toks[i]; gaps[0] is the leading literal
		trail string
	)
	lit := ""
	for _, n := range nodes {
		if n.tok < 0 {
			lit += n.lit
			continue
		}
		toks = append(toks, n.tok)
		gaps = append(gaps, lit)
		lit = ""
	}
	trail = lit
	if len(toks) == 0 {
		if trail == "" {
			return nil
		}
		return []piece{litPiece(trail)}
	}

	var out []piece
	add := func(s string) {
		if s != "" {
			out = append(out, litPiece(s))
		}
	}
	last := -1 // position in toks of the last drawn token
	for i, tk := range toks {
		if gone(tk) {
			continue
		}
		switch {
		case last < 0 && i == 0:
			add(gaps[0]) // the leading literal, its token drawn
		case last < 0:
			// The leading literal, less the brackets that open the part's
			// first token (not drawn), then the brackets that open this one.
			lead, _ := splitOpeners(gaps[0])
			_, _, open := splitGap(gaps[i])
			add(lead + open)
		default:
			// The brackets closing the last drawn token, the separator the
			// bridged gaps choose, and the brackets opening this token.
			closeLast, _, _ := splitGap(gaps[last+1])
			seps := make([]string, 0, i-last)
			for _, g := range gaps[last+1 : i+1] {
				_, sep, _ := splitGap(g)
				seps = append(seps, sep)
			}
			_, _, open := splitGap(gaps[i])
			add(closeLast + pickGap(seps) + open)
		}
		out = append(out, piece{slot: tk})
		last = i
	}
	switch {
	case last == len(toks)-1:
		add(trail)
	case last >= 0:
		// The last drawn token is followed by skipped ones: only the brackets
		// that close it are drawn.
		closeLast, _, _ := splitGap(gaps[last+1])
		add(closeLast)
	}
	return out
}

// splitGap splits a literal between two tokens into the closing brackets that
// belong to the token before it, the separator, and the opening brackets that
// belong to the token after it (§2.3 rule 7). A bracket run may hold blanks:
// ") · (" is ")", " · ", "(", and " ( " is "", " ", "( ".
func splitGap(g string) (closers, sep, openers string) {
	end := 0 // just past the last closing bracket of the leading run
	for i := 0; i < len(g) && (g[i] == ' ' || isCloser(g[i])); i++ {
		if isCloser(g[i]) {
			end = i + 1
		}
	}
	closers, rest := g[:end], g[end:]
	sep, openers = splitOpeners(rest)
	return closers, sep, openers
}

// splitOpeners splits off the opening brackets (and the blanks among and
// after them) that end a literal.
func splitOpeners(g string) (rest, openers string) {
	start := len(g) // the first opening bracket of the trailing run
	for j := len(g) - 1; j >= 0 && (g[j] == ' ' || isOpener(g[j])); j-- {
		if isOpener(g[j]) {
			start = j
		}
	}
	return g[:start], g[start:]
}

func isOpener(b byte) bool { return b == '(' || b == '[' || b == '{' }
func isCloser(b byte) bool { return b == ')' || b == ']' || b == '}' }

// pickGap chooses the separator between two drawn tokens from the gaps they
// bridge, left to right: the first that is not whitespace-only, else the one
// right before the next token.
func pickGap(cands []string) string {
	for _, g := range cands {
		if strings.TrimSpace(g) != "" {
			return g
		}
	}
	return cands[len(cands)-1]
}

func litPiece(s string) piece { return piece{lit: s, w: ansi.StringWidth(s), slot: -1} }

// measure is the width of pieces with every slot at its allotted width.
func (t *Template) measure(ps []piece, widths []int) int {
	n := 0
	for _, pc := range ps {
		if pc.slot < 0 {
			n += pc.w
		} else {
			n += widths[pc.slot]
		}
	}
	return n
}

// paintPieces renders pieces, each slot truncated to its allotted width, and
// returns the string and its display width.
func (t *Template) paintPieces(ps []piece, slots []slot, widths []int) (string, int) {
	var b strings.Builder
	w := 0
	for _, pc := range ps {
		if pc.slot < 0 {
			b.WriteString(t.opts.LiteralStyle.Render(pc.lit))
			w += pc.w
			continue
		}
		s := slots[pc.slot]
		parts := s.seg.Parts
		if widths[pc.slot] < s.w {
			parts = truncParts(parts, widths[pc.slot])
		}
		b.WriteString(paintParts(parts))
		w += ansi.StringWidth(joinParts(parts))
	}
	return b.String(), w
}

func paintParts(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Style.Render(p.Text))
		}
	}
	return b.String()
}

func joinParts(parts []Part) string {
	if len(parts) == 1 {
		return parts[0].Text
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// truncParts cuts a segment to at most w display columns ending in "…", on its
// concatenated text, and re-splits the result at the part boundaries (§2.4):
// a chip cut inside its value half keeps both halves' styles. The "…" takes
// the style of the part where the cut falls.
func truncParts(parts []Part, w int) []Part {
	text := joinParts(parts)
	if ansi.StringWidth(text) <= w {
		return parts
	}
	if w <= 0 {
		return nil
	}
	keep := ansi.Truncate(text, w-1, "")
	if !strings.HasPrefix(text, keep) {
		keep = "" // not reachable on plain text; never mis-split
	}
	// "hello…", not "hello …": a cut that lands after a blank drops it.
	keep = strings.TrimRight(keep, " ")
	return sliceParts(parts, 0, len(keep), "…")
}

// sliceParts restricts parts to the byte range [start, end) of their
// concatenated text and appends tail to the part holding byte end (the last
// part when end is the end of the text).
func sliceParts(parts []Part, start, end int, tail string) []Part {
	holder := len(parts) - 1
	off := 0
	for i, p := range parts {
		if off <= end && end < off+len(p.Text) {
			holder = i
			break
		}
		off += len(p.Text)
	}
	var out []Part
	off = 0
	for i, p := range parts {
		pend := off + len(p.Text)
		lo, hi := max(start, off), min(end, pend)
		text := ""
		if lo < hi {
			text = p.Text[lo-off : hi-off]
		}
		if i == holder {
			text += tail
		}
		if text != "" {
			out = append(out, Part{Text: text, Style: p.Style})
		}
		off = pend
	}
	return out
}

// cleanText replaces control characters (C0 including TAB, DEL, C1) with a
// space. The panel sanitises session text before it gets here (§2.7); this
// is the engine's own guarantee that nothing it draws can move the cursor or
// measure differently from what the terminal shows.
func cleanText(s string) string {
	clean := true
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

// clip is the last word on width (§2.4 step 5): whatever came before, nothing
// drawn is ever wider than width.
func clip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > width {
		return ansi.Truncate(s, width, "")
	}
	return s
}
