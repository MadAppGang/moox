package panelfmt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Template keys, as panel.conf spells them.
const (
	keyRow      = "row"
	keyMeta     = "meta"
	keyAttnRow  = "attn_row"
	keyAttnWhy  = "attn_why"
	keyAttnMeta = "attn_meta"
)

// optWrap is the one token-specific option the engine itself understands: it
// marks the attention "why" token that word-wraps (§2.5). Which tokens accept
// it is still the Registry's call.
const (
	optWrap         = "wrap"
	defaultWrapRows = 2
)

// tabWidth is what a TAB in a template literal (a raw TAB or the "\t" escape)
// becomes: the same four spaces lipgloss would draw, expanded here so that
// display-width measurement and rendering agree.
const tabWidth = 4

// commonSpecs are the options every token accepts (§2.6). The engine applies
// them after the Resolver returns, unless the token's own TokenSpec declares
// an option of the same name — then the token owns it (e.g. {ctx:plain} keeps
// its tone colour, where the common plain would strip it).
var commonSpecs = map[string]OptSpec{
	"plain":  {Kind: OptFlag},
	"dim":    {Kind: OptFlag},
	"min":    {Kind: OptInt, Min: 0, Max: 999},
	"max":    {Kind: OptInt, Min: 1, Max: 999},
	"shrink": {Kind: OptInt, Min: 0, Max: 99},
	"drop":   {Kind: OptInt, Min: 0, Max: 99},
}

// common is a token's parsed common options, resolved once at parse time.
type common struct {
	plain, dim                         bool
	min, max, shrink, drop             int
	hasMin, hasMax, hasShrink, hasDrop bool
}

// node is one element of a template part: a literal (tok < 0) or a token.
type node struct {
	lit string // literal text: escapes resolved, TABs expanded
	w   int    // display width of lit
	tok int    // index into Template.toks, or -1 for a literal
}

// tokNode is one {call} with what the engine needs to render it.
type tokNode struct {
	Token
	com     common
	hasWrap bool // carries the wrap option (honoured or not)
}

// parseTemplate parses one template string. src is the decoded string value;
// colAt maps a byte offset in src to its 1-based column in panel.conf. A
// fatal problem returns a nil template.
func parseTemplate(src string, colAt func(int) int, line int, key string, reg Registry) (*Template, problems) {
	var (
		probs  problems
		t      = &Template{}
		cur    []node
		lit    strings.Builder
		spring bool
	)
	flush := func() {
		if lit.Len() == 0 {
			return
		}
		s := strings.ReplaceAll(lit.String(), "\t", strings.Repeat(" ", tabWidth))
		cur = append(cur, node{lit: s, w: ansi.StringWidth(s), tok: -1})
		lit.Reset()
	}

	for i := 0; i < len(src); {
		switch c := src[i]; c {
		case '{':
			if i+1 < len(src) && src[i+1] == '{' {
				lit.WriteByte('{')
				i += 2
				continue
			}
			j := i + 1
			for j < len(src) && src[j] != '}' && src[j] != '{' {
				j++
			}
			if j >= len(src) || src[j] == '{' {
				probs.fatal(line, colAt(i), `unclosed "{"`)
				return nil, probs
			}
			raw := src[i+1 : j]
			flush()
			if raw == ">" {
				if spring {
					probs.fatal(line, colAt(i), "a second spring {>}; a template has at most one")
					return nil, probs
				}
				spring = true
				t.parts = append(t.parts, cur)
				cur = nil
			} else {
				tn, p := parseCall(raw, line, colAt(i), func(off int) int { return colAt(i + 1 + off) }, reg)
				probs = append(probs, p...)
				cur = append(cur, node{tok: len(t.toks)})
				t.toks = append(t.toks, tn)
			}
			i = j + 1
		case '}':
			if i+1 < len(src) && src[i+1] == '}' {
				lit.WriteByte('}')
				i += 2
				continue
			}
			probs.fatal(line, colAt(i), `stray "}" (write }} for a literal brace)`)
			return nil, probs
		default:
			lit.WriteByte(c)
			i++
		}
	}
	flush()
	t.parts = append(t.parts, cur)
	t.spring = spring
	probs = append(probs, t.settleWrap(line, key)...)
	return t, probs
}

// settleWrap decides whether the template's wrap token is honoured: there must
// be exactly one, it must be the last token, and the key must be attn_why
// (§2.2). Anything else is a soft problem and the token renders on one line.
func (t *Template) settleWrap(line int, key string) problems {
	var probs problems
	var wraps []int
	for i, tn := range t.toks {
		if tn.hasWrap {
			wraps = append(wraps, i)
		}
	}
	if len(wraps) == 0 {
		return nil
	}
	for _, i := range wraps[1:] {
		tn := t.toks[i]
		probs.soft(line, tn.Col, fmt.Sprintf("a second wrap token {%s}; it renders on one line", tn.Raw))
	}
	first := t.toks[wraps[0]]
	switch {
	case len(wraps) > 1:
		// reported above; nothing is honoured
	case key != keyAttnWhy:
		probs.soft(line, first.Col, fmt.Sprintf("wrap is only honoured in %s; {%s} renders on one line", keyAttnWhy, first.Raw))
	case wraps[0] != len(t.toks)-1:
		probs.soft(line, first.Col, fmt.Sprintf("{%s} must be the last token to wrap; it renders on one line", first.Raw))
	default:
		t.wrap = defaultWrapRows
		for _, o := range first.Opts {
			if o.Name == optWrap && o.HasN && o.N > 0 {
				t.wrap = o.N
			}
		}
	}
	return probs
}

// parseCall parses the text between the braces of one token. Any problem is
// soft: the token is kept with Known false and renders in place as {raw?}.
// Only the first problem of a token is reported.
func parseCall(raw string, line, col int, colAt func(int) int, reg Registry) (tokNode, problems) {
	var probs problems
	tn := tokNode{Token: Token{Raw: raw, Line: line, Col: col}}
	fields := strings.Split(raw, ":")
	name := fields[0]
	tn.Name = name
	switch {
	case raw == "":
		probs.soft(line, col, "empty token {}")
		return tn, probs
	case !validName(name):
		probs.soft(line, col, fmt.Sprintf("bad token name {%s}", raw))
		return tn, probs
	}
	spec, ok := reg[name]
	if !ok {
		probs.soft(line, col, fmt.Sprintf("unknown token {%s}", raw))
		return tn, probs
	}

	off := len(name) + 1 // byte offset of the current option within raw
	for _, f := range fields[1:] {
		optCol := colAt(off)
		off += len(f) + 1
		o, own, msg := parseOpt(f, spec)
		if msg != "" {
			probs.soft(line, optCol, fmt.Sprintf("%s in {%s}", msg, raw))
			tn.Opts = nil
			return tn, probs
		}
		tn.Opts = append(tn.Opts, o)
		if o.Name == optWrap && own {
			tn.hasWrap = true
		}
		if !own {
			tn.com.set(o)
		}
	}
	tn.Known = true
	return tn, probs
}

// parseOpt parses `optname [digits] [=value]` and validates it against the
// token's own spec first, then the common options. own reports that the
// token's spec declared it. msg is non-empty on a problem.
func parseOpt(f string, spec TokenSpec) (o Opt, own bool, msg string) {
	i := 0
	for i < len(f) && f[i] >= 'a' && f[i] <= 'z' {
		i++
	}
	o.Name = f[:i]
	if o.Name == "" {
		return o, false, fmt.Sprintf("bad option %q", f)
	}
	d := i
	for i < len(f) && f[i] >= '0' && f[i] <= '9' {
		i++
	}
	digits := f[d:i]
	hasValue := false
	if i < len(f) {
		if f[i] != '=' || i+1 == len(f) {
			return o, false, fmt.Sprintf("bad option %q", f)
		}
		o.Value = f[i+1:]
		hasValue = true
	}

	sp, own := spec.Options[o.Name]
	if !own {
		var ok bool
		if sp, ok = commonSpecs[o.Name]; !ok {
			return o, false, fmt.Sprintf("unknown option %q", o.Name)
		}
	}
	switch sp.Kind {
	case OptFlag:
		if digits != "" || hasValue {
			return o, own, fmt.Sprintf("option %q takes no value", o.Name)
		}
	case OptString:
		if digits != "" || !hasValue {
			return o, own, fmt.Sprintf("option %q needs a value (%s=…)", o.Name, o.Name)
		}
	case OptInt:
		num := digits
		if hasValue {
			if digits != "" {
				return o, own, fmt.Sprintf("bad option %q", f)
			}
			num = o.Value
		}
		if num == "" {
			if !own {
				// A common numeric option means nothing without its number.
				return o, own, fmt.Sprintf("option %q needs a number (%s=N)", o.Name, o.Name)
			}
			return o, own, "" // the token decides its own default (wrap → 2)
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return o, own, fmt.Sprintf("option %q wants a number, not %q", o.Name, num)
		}
		if (sp.Min != 0 || sp.Max != 0) && (n < sp.Min || n > sp.Max) {
			return o, own, fmt.Sprintf("option %s=%d out of range %d..%d", o.Name, n, sp.Min, sp.Max)
		}
		o.N, o.HasN = n, true
	}
	return o, own, ""
}

func (c *common) set(o Opt) {
	switch o.Name {
	case "plain":
		c.plain = true
	case "dim":
		c.dim = true
	case "min":
		c.min, c.hasMin = o.N, true
	case "max":
		c.max, c.hasMax = o.N, true
	case "shrink":
		c.shrink, c.hasShrink = o.N, true
	case "drop":
		c.drop, c.hasDrop = o.N, true
	}
}

// validName is `lower { lower | digit | "_" }`.
func validName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}
