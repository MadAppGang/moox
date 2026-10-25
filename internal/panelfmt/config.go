package panelfmt

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// File limits (§2.1). Beyond either, the file is fatal.
const (
	maxFileBytes = 64 << 10
	maxLineBytes = 1 << 10
)

// Threshold keys and their bounds (§2.1).
const (
	keyCtxWarn   = "ctx_warn"
	keyCtxAlert  = "ctx_alert"
	minThreshold = 1_000
	maxThreshold = 100_000_000
)

// keyTextLines is how many lines of text are drawn under a row, and
// MaxTextLines the most it may be (revision 4, FR9).
const (
	keyTextLines = "text_lines"
	MaxTextLines = 3
)

// defaultCtx is what ctx_warn and ctx_alert are when no file sets them.
var defaultCtx = Thresholds{Warn: 200_000, Alert: 400_000}

var templateKeys = [...]string{keyRow, keyMeta, keyAttnRow, keyAttnWhy, keyAttnMeta}

// entry is one template key as a file set it.
type entry struct {
	tpl  *Template // nil when the file set it to ""
	line int
}

// parsed is everything one panel.conf said, before it is laid over a base.
type parsed struct {
	tpls                map[string]entry
	warn, alert         int64
	warnLine, alertLine int // 0 = not set by the file
	// icons are the usable icon values the file set, by status name;
	// iconLines records every icon key the file named (usable or not), so a
	// second one is reported as a duplicate.
	icons     map[string]string
	iconLines map[string]int
	// windows are the file's `window` rules, in file order (windows.go).
	windows []WindowRule
	// textLines is `text_lines`, and textLine the line that set it (0 = not
	// set by the file).
	textLines, textLine int
}

// ParseConfig parses a panel.conf. Keys the file omits keep their value from
// defaults, so a file may override only meta. It never returns nil: on a
// fatal problem it returns defaults' templates and thresholds with the
// problems attached, so a broken edit can never leave the panel without a
// format. opts is stored on the returned Set, and every template the Set
// holds — its own or one taken from defaults — renders with it.
//
// defaults must not be nil; it is normally the Set Defaults returned.
func ParseConfig(src []byte, reg Registry, opts Options, defaults *Set) *Set {
	if defaults == nil {
		panic("panelfmt: ParseConfig needs a defaults Set")
	}
	p, probs := parse(src, reg, defaults.Ctx)
	set := &Set{
		Row:       withOpts(defaults.Row, opts),
		Meta:      withOpts(defaults.Meta, opts),
		AttnRow:   withOpts(defaults.AttnRow, opts),
		AttnWhy:   withOpts(defaults.AttnWhy, opts),
		AttnMeta:  withOpts(defaults.AttnMeta, opts),
		Ctx:       defaults.Ctx,
		Icons:     cloneIcons(defaults.Icons),
		Windows:   layWindows(nil, defaults.Windows),
		TextLines: defaults.TextLines,
		Problems:  probs,
		opts:      opts,
	}
	if probs.hasFatal() {
		return set
	}
	set.apply(p, opts)
	return set
}

// Defaults parses the built-in panel.conf (defaultPanelConf). It panics on any
// problem, fatal or soft: the built-in text ships in the binary, so a mistake
// in it is a bug a test must catch, not a condition to recover from. It must
// set row and attn_row; ctx_warn and ctx_alert default to 200k and 400k.
func Defaults(src []byte, reg Registry, opts Options) *Set {
	p, probs := parse(src, reg, defaultCtx)
	if len(probs) > 0 {
		panic("panelfmt: built-in panel.conf: " + probs[0].Format("defaultPanelConf"))
	}
	set := &Set{Ctx: defaultCtx, opts: opts}
	set.apply(p, opts)
	if set.Row == nil || set.AttnRow == nil {
		panic("panelfmt: built-in panel.conf must set row and attn_row")
	}
	return set
}

// apply lays what a file set over the Set.
func (s *Set) apply(p parsed, opts Options) {
	slots := map[string]**Template{
		keyRow: &s.Row, keyMeta: &s.Meta, keyAttnRow: &s.AttnRow,
		keyAttnWhy: &s.AttnWhy, keyAttnMeta: &s.AttnMeta,
	}
	for k, e := range p.tpls {
		*slots[k] = withOpts(e.tpl, opts)
	}
	s.Ctx = Thresholds{Warn: p.warn, Alert: p.alert}
	if len(p.icons) > 0 && s.Icons == nil {
		s.Icons = make(map[string]string, len(p.icons))
	}
	for name, glyph := range p.icons {
		s.Icons[name] = glyph
	}
	s.Windows = layWindows(p.windows, s.Windows)
	if p.textLine != 0 {
		s.TextLines = p.textLines
	}
}

// cloneIcons copies an icon table, so a Set never shares a map it could
// change with the Set it was laid over.
func cloneIcons(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// withOpts returns t bound to opts. Templates are immutable, so the copy
// shares its parsed nodes with the original.
func withOpts(t *Template, opts Options) *Template {
	if t == nil {
		return nil
	}
	c := *t
	c.opts = opts
	return &c
}

// parse reads a whole file. base supplies the thresholds the file does not
// set, so ctx_warn < ctx_alert is checked on the values that will be used.
func parse(src []byte, reg Registry, base Thresholds) (parsed, problems) {
	p := parsed{tpls: map[string]entry{}, warn: base.Warn, alert: base.Alert,
		icons: map[string]string{}, iconLines: map[string]int{}}
	var probs problems
	if len(src) > maxFileBytes {
		probs.fatal(0, 0, fmt.Sprintf("the file is larger than %d KiB", maxFileBytes>>10))
		return p, probs
	}
	for n, raw := range strings.Split(string(src), "\n") {
		line := n + 1
		raw = strings.TrimSuffix(raw, "\r")
		switch {
		case len(raw) > maxLineBytes:
			probs.fatal(line, 0, fmt.Sprintf("the line is longer than %d KiB", maxLineBytes>>10))
			continue
		case !utf8.ValidString(raw):
			probs.fatal(line, 0, "the line is not valid UTF-8")
			continue
		}
		if off, r := firstControl(raw); off >= 0 {
			probs.fatal(line, runeCol(raw, off), fmt.Sprintf("control character U+%04X", r))
			continue
		}
		probs = append(probs, p.line(raw, line, reg)...)
	}
	if p.warn >= p.alert {
		at := max(p.warnLine, p.alertLine)
		probs.fatal(at, 0, fmt.Sprintf("%s (%s) must be below %s (%s)",
			keyCtxWarn, fmtSize(p.warn), keyCtxAlert, fmtSize(p.alert)))
	}
	for _, k := range [...]string{keyRow, keyAttnRow} {
		if e, ok := p.tpls[k]; ok && e.tpl == nil {
			probs.fatal(e.line, 0, k+" must not be empty")
		}
	}
	sortByLine(probs)
	return p, probs
}

// line parses one line of the file: `[ws] [entry] [ws] [comment]`.
func (p *parsed) line(raw string, line int, reg Registry) problems {
	var probs problems
	col := func(off int) int { return runeCol(raw, off) }

	i := skipWS(raw, 0)
	if i == len(raw) || raw[i] == '#' {
		return nil
	}
	start := i
	for i < len(raw) && isKeyByte(raw[i]) {
		i++
	}
	key := raw[start:i]
	icon, isIcon := iconName(key)
	switch {
	case key == "":
		r, _ := utf8.DecodeRuneInString(raw[i:])
		probs.fatal(line, col(i), fmt.Sprintf("unexpected %q where a key belongs", r))
		return probs
	case !isTemplateKey(key) && key != keyCtxWarn && key != keyCtxAlert && key != keyWindow && key != keyTextLines && !isIcon:
		probs.fatal(line, col(start), fmt.Sprintf("unknown key %q", key))
		return probs
	case key == keyWindow:
		// `window <glob> = <size>`: the glob sits between the key and "=",
		// and any number of rules may be given (the first match wins).
		next := p.windowLine(raw, i, line, &probs)
		if next < 0 {
			return probs
		}
		if i := skipWS(raw, next); i < len(raw) && raw[i] != '#' {
			probs.fatal(line, col(i), "unexpected text after the value")
		}
		return probs
	}
	if p.seen(key) {
		probs.soft(line, col(start), fmt.Sprintf("duplicate key %s; the later one wins", key))
	}

	i = skipWS(raw, i)
	if i == len(raw) || raw[i] != '=' {
		probs.fatal(line, col(i), fmt.Sprintf(`expected "=" after %s`, key))
		return probs
	}
	i = skipWS(raw, i+1)

	var next int
	switch {
	case isIcon:
		if next = p.iconLine(raw, i, line, key, icon, &probs); next < 0 {
			return probs
		}
	case isTemplateKey(key):
		val, cols, end, bad := readString(raw, i)
		if bad != nil {
			bad.Line = line
			return append(probs, *bad)
		}
		next = end
		var tpl *Template
		if strings.TrimSpace(val) != "" {
			var tp problems
			tpl, tp = parseTemplate(val, func(off int) int { return cols[off] }, line, key, reg)
			probs = append(probs, tp...)
			if tpl == nil {
				return probs
			}
		}
		p.tpls[key] = entry{tpl: tpl, line: line}
	case key == keyTextLines:
		end := i
		for end < len(raw) && raw[end] != ' ' && raw[end] != '\t' && raw[end] != '#' {
			end++
		}
		word := raw[i:end]
		n, err := strconv.Atoi(word)
		if err != nil || !allDigits(word) || n < 0 || n > MaxTextLines {
			probs.fatal(line, col(i), fmt.Sprintf("%s must be a whole number from 0 to %d, not %q", keyTextLines, MaxTextLines, word))
			return probs
		}
		p.textLines, p.textLine = n, line
		next = end
	default:
		end := i
		for end < len(raw) && raw[end] != ' ' && raw[end] != '\t' && raw[end] != '#' {
			end++
		}
		word := raw[i:end]
		n, ok := parseSize(word)
		switch {
		case !ok:
			probs.fatal(line, col(i), fmt.Sprintf("bad size %q (write it like 200k, 1.5m or 400000)", word))
			return probs
		case n < minThreshold || n > maxThreshold:
			probs.fatal(line, col(i), fmt.Sprintf("%s must be between 1k and 100M, not %s", key, word))
			return probs
		}
		if key == keyCtxWarn {
			p.warn, p.warnLine = n, line
		} else {
			p.alert, p.alertLine = n, line
		}
		next = end
	}

	if i := skipWS(raw, next); i < len(raw) && raw[i] != '#' {
		probs.fatal(line, col(i), "unexpected text after the value")
	}
	return probs
}

func (p *parsed) seen(key string) bool {
	switch key {
	case keyCtxWarn:
		return p.warnLine != 0
	case keyCtxAlert:
		return p.alertLine != 0
	case keyTextLines:
		return p.textLine != 0
	}
	if name, ok := iconName(key); ok {
		return p.iconLines[name] != 0
	}
	_, ok := p.tpls[key]
	return ok
}

// readString reads a quoted string starting at raw[i]. It returns the decoded
// value, the file column of every decoded byte (plus one past the end), and
// the offset just past the closing quote. A double-quoted string knows the
// escapes \" \\ and \t; a single-quoted one is raw.
func readString(raw string, i int) (val string, cols []int, end int, bad *Problem) {
	fail := func(off int, msg string) (string, []int, int, *Problem) {
		return "", nil, 0, &Problem{Col: runeCol(raw, off), Msg: msg, Fatal: true}
	}
	if i == len(raw) || (raw[i] != '"' && raw[i] != '\'') {
		return fail(i, "expected a quoted string")
	}
	quote := raw[i]
	var b strings.Builder
	col := runeCol(raw, i) + 1
	for j := i + 1; j < len(raw); {
		c := raw[j]
		switch {
		case c == quote:
			return b.String(), append(cols, col), j + 1, nil
		case c == '\\' && quote == '"':
			if j+1 == len(raw) {
				return fail(i, "unterminated string")
			}
			switch raw[j+1] {
			case '"', '\\':
				b.WriteByte(raw[j+1])
			case 't':
				b.WriteByte('\t')
			default:
				r, _ := utf8.DecodeRuneInString(raw[j+1:])
				return fail(j, fmt.Sprintf(`unknown escape \%c (only \" \\ \t)`, r))
			}
			cols = append(cols, col)
			col += 2
			j += 2
		default:
			_, size := utf8.DecodeRuneInString(raw[j:])
			b.WriteString(raw[j : j+size])
			for range size {
				cols = append(cols, col)
			}
			col++
			j += size
		}
	}
	return fail(i, "unterminated string")
}

// parseSize reads `digit {digit} [ "." digit ] [ k | K | m | M ]`.
func parseSize(s string) (int64, bool) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		mult, s = 1_000, s[:len(s)-1]
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "M"):
		mult, s = 1_000_000, s[:len(s)-1]
	}
	whole, frac, dotted := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) || len(whole) > 12 {
		return 0, false
	}
	if dotted && (len(frac) != 1 || !allDigits(frac)) {
		return 0, false
	}
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	n *= mult
	if dotted {
		n += int64(frac[0]-'0') * mult / 10
	}
	return n, true
}

// fmtSize prints a threshold the way a user would write it.
func fmtSize(n int64) string {
	switch {
	case n%1_000_000 == 0:
		return strconv.FormatInt(n/1_000_000, 10) + "M"
	case n%1_000 == 0:
		return strconv.FormatInt(n/1_000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// firstControl finds the first C0 control other than TAB, DEL, or C1 control
// in s. A raw one in panel.conf could otherwise reach the terminal through a
// template literal.
func firstControl(s string) (int, rune) {
	for off, r := range s {
		if (r < 0x20 && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			return off, r
		}
	}
	return -1, 0
}

func isTemplateKey(k string) bool {
	for _, t := range templateKeys {
		if k == t {
			return true
		}
	}
	return false
}

func isKeyByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func skipWS(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sortByLine keeps problems in file order: the threshold and empty-row checks
// run after the line loop, so they are slotted back into place. The sort is
// stable, so problems on one line keep their order; whole-file problems
// (line 0) come first.
func sortByLine(ps problems) {
	slices.SortStableFunc(ps, func(a, b Problem) int { return cmp.Compare(a.Line, b.Line) })
}
