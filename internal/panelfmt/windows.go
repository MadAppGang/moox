package panelfmt

import (
	"fmt"
	"path"
)

// The window rules (`window <glob> = <size>`). Each says how large the
// context window of the models whose id matches glob is, so the panel can
// fill its context bar against it. The glob is path.Match syntax, matched
// against the whole model id (claude-opus-5-5, claude-sonnet-5-20260901); the
// FIRST matching rule wins. A file's rules are tried before the built-in
// ones, so a file can override any of them, and a model no rule matches has
// no known window (the panel then draws the count without a bar).
//
// Like ctx_warn and ctx_alert, a malformed rule is FATAL: a size is a number
// the panel measures against, and a guessed one would draw a wrong bar.
const (
	keyWindow     = "window"
	minWindowSize = 1_000
	maxWindowSize = 100_000_000
)

// WindowRule is one `window` line: the models it names and their window in
// tokens.
type WindowRule struct {
	Glob string
	Size int64
}

// Window is the context window of a model id: the Size of the first rule
// whose Glob matches it, and whether one did. An empty id matches nothing.
func (s *Set) Window(model string) (int64, bool) {
	if s == nil || model == "" {
		return 0, false
	}
	for _, r := range s.Windows {
		if ok, err := path.Match(r.Glob, model); err == nil && ok {
			return r.Size, true
		}
	}
	return 0, false
}

// windowLine parses the rest of a `window` line from raw[i] (just after the
// key): the glob, "=", and the size. It returns the offset after the size, or
// -1 when a fatal problem ends the line.
func (p *parsed) windowLine(raw string, i, line int, probs *problems) int {
	col := func(off int) int { return runeCol(raw, off) }
	i = skipWS(raw, i)
	start := i
	for i < len(raw) && raw[i] != ' ' && raw[i] != '\t' && raw[i] != '=' && raw[i] != '#' {
		i++
	}
	glob := raw[start:i]
	if glob == "" {
		probs.fatal(line, col(start), `expected a model pattern after window, like window claude-opus-5* = 1M`)
		return -1
	}
	if _, err := path.Match(glob, ""); err != nil {
		probs.fatal(line, col(start), fmt.Sprintf("bad model pattern %q (%v)", glob, err))
		return -1
	}
	i = skipWS(raw, i)
	if i == len(raw) || raw[i] != '=' {
		probs.fatal(line, col(i), fmt.Sprintf(`expected "=" after window %s`, glob))
		return -1
	}
	i = skipWS(raw, i+1)
	end := i
	for end < len(raw) && raw[end] != ' ' && raw[end] != '\t' && raw[end] != '#' {
		end++
	}
	word := raw[i:end]
	n, ok := parseSize(word)
	switch {
	case !ok:
		probs.fatal(line, col(i), fmt.Sprintf("bad size %q (write it like 200k, 1M or 1000000)", word))
		return -1
	case n < minWindowSize || n > maxWindowSize:
		probs.fatal(line, col(i), fmt.Sprintf("a window must be between 1k and 100M, not %s", word))
		return -1
	}
	p.windows = append(p.windows, WindowRule{Glob: glob, Size: n})
	return end
}

// layWindows is the rule list a file's rules make over a base's: the file's
// first, in file order, then the base's, so the file's win.
func layWindows(file, base []WindowRule) []WindowRule {
	if len(file) == 0 && len(base) == 0 {
		return nil
	}
	out := make([]WindowRule, 0, len(file)+len(base))
	return append(append(out, file...), base...)
}
