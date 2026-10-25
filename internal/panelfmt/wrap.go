package panelfmt

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// span is one wrapped line: the byte range [start, end) of the text, and a
// tail drawn after it ("…" on the last line when text is left over).
type span struct {
	start, end int
	tail       string
}

// wrapSpans word-wraps text on spaces into at most maxLines lines of at most
// limit display columns (§2.5). A word wider than limit is hard-broken at a
// grapheme boundary; runs of spaces are break points and are not drawn at a
// line's start or end. It reports more when text was left over.
func wrapSpans(text string, limit, maxLines int) (lines []span, more bool) {
	if limit < 1 || maxLines < 1 {
		return nil, text != ""
	}
	var (
		cur span
		has bool
	)
	for i := 0; i < len(text); {
		if text[i] == ' ' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] != ' ' {
			j++
		}
		word := span{start: i, end: j}
		for word.start < word.end {
			if has {
				if ansi.StringWidth(text[cur.start:word.end]) <= limit {
					cur.end, word.start = word.end, word.end
					break
				}
				lines, has = append(lines, cur), false
				if len(lines) == maxLines {
					return lines, true
				}
				continue
			}
			if ansi.StringWidth(text[word.start:word.end]) <= limit {
				cur, has = word, true
				word.start = word.end
				break
			}
			// A word wider than the line: break it at the widest grapheme
			// prefix that fits — at least one grapheme, so the loop moves.
			pre := ansi.Truncate(text[word.start:word.end], limit, "")
			if pre == "" {
				pre, _ = ansi.FirstGraphemeCluster(text[word.start:word.end], ansi.GraphemeWidth)
			}
			lines = append(lines, span{start: word.start, end: word.start + len(pre)})
			word.start += len(pre)
			if len(lines) == maxLines {
				return lines, word.start < word.end || hasWord(text[j:])
			}
		}
		i = j
	}
	if has {
		lines = append(lines, cur)
	}
	return lines, false
}

// ellipsize marks the last line as cut: "…" after it when it fits within
// limit. Otherwise the line gives up its last word ("before it touches the…",
// not "…the pro…") as long as that keeps at least half the line, and is cut
// at a grapheme when it would not (one long word).
func ellipsize(text string, sp span, limit int) span {
	sp.tail = "…"
	line := text[sp.start:sp.end]
	if ansi.StringWidth(line)+1 <= limit {
		return sp
	}
	if i := strings.LastIndexByte(line, ' '); i > 0 {
		keep := strings.TrimRight(line[:i], " ")
		if w := ansi.StringWidth(keep); w > 0 && 2*w >= limit {
			sp.end = sp.start + len(keep)
			return sp
		}
	}
	keep := strings.TrimRight(ansi.Truncate(line, limit-1, ""), " ")
	sp.end = sp.start + len(keep)
	return sp
}

func hasWord(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			return true
		}
	}
	return false
}
