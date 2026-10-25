// Package panelfmt is the engine that draws the rows of the tt session panel
// from panel.conf templates: it parses the file and its template grammar,
// resolves each {token} through a caller-supplied function, collapses empty
// tokens together with the separators around them, fits the line to a width
// in display columns, and word-wraps the attention "why" line.
//
// It knows nothing about sessions. The panel (package main) owns the token
// table and turns one session into Segments; this package only arranges them.
// Keeping it a separate package is what makes "the engine knows no sessions" a
// compiler-checked fact rather than a convention: inside package main (about
// 17.6k lines) it could reach panel state, here it cannot import observer at
// all. It is not a port and has one implementation — the same reason
// internal/projectkey is its own package. It imports lipgloss and x/ansi only.
//
// The types in this file and the exported functions and methods (Line, One
// and Wrap in render.go, ParseConfig and Defaults in config.go, Format in
// problem.go) are FROZEN (Phase C0, review #15): the context-facts, palette
// and engine phases are built in parallel against them, so a change here is
// a change to three phases at once. Unexported fields may still be added; the
// exported surface may not change.
package panelfmt

import "github.com/charmbracelet/lipgloss"

// OptKind is the shape of a token option's value, as the Registry declares it.
type OptKind int

const (
	// OptFlag takes no value: {ctx:plain}, {model:short}. The zero value, so a
	// spec that forgets its kind is the strictest one — a flag given "=x" is a
	// soft problem rather than a silently ignored value.
	OptFlag OptKind = iota
	// OptInt takes an integer bounded by OptSpec.Min and OptSpec.Max:
	// {why:wrap=2}, {name:min=12}. Out of range is a soft problem (§2.8).
	OptInt
	// OptString takes free text up to the next ":" or "}": {gutter:char=X}.
	OptString
)

// OptSpec declares one option a token accepts. Min and Max bound an OptInt
// (inclusive) and are ignored for the other kinds.
type OptSpec struct {
	Kind     OptKind
	Min, Max int
}

// TokenSpec lists the options specific to one token. The common options
// (plain, dim, min=, max=, shrink=, drop=) are implicit on every token and are
// never listed here: the engine applies them after the token function returns
// (§2.6), so a token function cannot forget one and cannot apply one twice.
type TokenSpec struct {
	Options map[string]OptSpec
}

// Registry is the closed set of tokens the panel can resolve: token name → its
// own options. It is the parser's only source of truth for what is "known" —
// a name or option missing from it is a soft problem that renders in place as
// a coral {raw?} (§2.8), so a typo is visible on the panel, not swallowed. The
// `color` alias of the `colour` token is an entry of its own (#26).
type Registry map[string]TokenSpec

// Part is one styled run of a Segment. Text is session text that the caller
// has already sanitised (§2.7): no control character reaches this package, so
// ansi.StringWidth measures exactly what is drawn.
type Part struct {
	Text  string
	Style lipgloss.Style
}

// Segment is what one token resolved to. It is a slice of parts because a
// status chip is two differently-filled halves that must fit, truncate and
// re-split as one unit (§2.4); plain text is one part.
type Segment struct {
	Parts []Part // 1 part for text, 2 for a chip
	// Empty marks a token with nothing to say. It vanishes together with the
	// separator before it (§2.3), and a line whose every token is Empty is not
	// drawn at all — so an all-empty meta leaves no blank indented line.
	Empty bool
	// The width-fitting fields (§2.4). Min is the width a shrinking segment
	// gives way to before anything is dropped. Shrink is the order in which
	// segments give way, rank ascending, right-to-left within a rank; 0 is
	// rigid. Drop is the order in which segments are removed whole once
	// shrinking is not enough; 0 is never dropped. The common options min=,
	// shrink= and drop= override what the token function set.
	Min, Shrink, Drop int
}

// Opt is one parsed option of a token, `optname [digits] [=value]`. Name is
// the option name with any digit suffix removed; Value is the text after "="
// ("" when there is none). N and HasN carry the option's integer — the digit
// suffix, or an OptInt's "=value" — so a token function never parses numbers
// itself and HasN tells an explicit 0 from an absent one.
type Opt struct {
	Name, Value string
	N           int
	HasN        bool
}

// Token is one {call} as written in a template. Raw is the source text between
// the braces, kept verbatim so an unknown token or option can be drawn back in
// place as {raw?}. Known is false when the name or any option failed the
// Registry; the engine then renders the error segment and never calls the
// Resolver. Line and Col locate it in panel.conf (1-based) for Problem.
type Token struct {
	Name      string
	Opts      []Opt
	Raw       string
	Known     bool
	Line, Col int
}

// Resolver renders one known token for one row. It does the token-specific
// work only: the engine applies the common options to the Segment it returns
// (§2.6). The panel builds one Resolver per row, closed over that session.
type Resolver func(Token) Segment

// Options are the styles the engine itself draws with. They are palette
// tokens handed in by the panel, so this package holds no colour of its own.
type Options struct {
	LiteralStyle lipgloss.Style // template literals (" · ", "(" …); default slate (R7)
	DimStyle     lipgloss.Style // foreground for the `dim` common option
	ErrorStyle   lipgloss.Style // coral, for an unknown "{foo?}" drawn in place
}

// Template is one parsed template: its literals and tokens, split at the
// spring {>} into a left and a right part that collapse independently (#18),
// plus the index of its wrap token when it has one. The internals are Phase
// C's to define and are deliberately unexported: callers only render it.
//
// A Template is immutable once parsed, so one may be shared by any number of
// Sets and rendered concurrently. Line, One and Wrap live in render.go.
type Template struct {
	parts  [][]node  // one part, or two split at the spring
	toks   []tokNode // every {call}, in template order; node.tok indexes it
	spring bool      // the template has a {>}
	wrap   int       // lines of the honoured wrap token (always the last token); 0 = none
	opts   Options   // the styles of the Set this template belongs to
}

// Thresholds are the context badge's absolute edges in tokens (ctx_warn and
// ctx_alert): below Warn is quiet, Warn..Alert inclusive is medium, above
// Alert is loud. Parsing guarantees 1k ≤ Warn < Alert ≤ 100M.
type Thresholds struct {
	Warn, Alert int64
}

// Set is one complete, usable panel format: what ParseConfig and Defaults
// return, and what a panel renders every row from.
type Set struct {
	// Row and AttnRow are never nil: a row without a first line could not be
	// accounted for, so an empty one is fatal. Meta, AttnWhy and AttnMeta are
	// nil when the file sets them to "" — the line does not exist (#33).
	Row, Meta, AttnRow, AttnWhy, AttnMeta *Template
	Ctx                                   Thresholds
	// Icons are the status icons a file set (icon_busy = "🔄" …), keyed by
	// status name (busy, work, waiting, blocked, idle, shell, unclear, ended;
	// IconNames). Each is 1 or IconMaxCols columns wide, with no U+FE0F and no
	// U+200D. A status the file leaves out, or set to an unusable value (a soft
	// problem), is absent: the panel draws its built-in glyph. nil when the
	// file names none. Added after the C0 freeze, as a field of its own, so
	// every existing caller is unaffected.
	Icons map[string]string
	// Windows are the `window <glob> = <size>` rules, the file's first and
	// then the built-in ones; Window(model) returns the first match. Added
	// after the C0 freeze like Icons, as a field of its own.
	Windows []WindowRule
	// TextLines is `text_lines`: how many lines of text the panel draws under
	// a row, 0 to MaxTextLines — the latest assistant text, or why a session
	// waits. Added after the C0 freeze like Icons, as a field of its own; a
	// file that does not set it keeps the defaults' value.
	TextLines int
	// Problems holds every fatal and soft problem found, in file order. The
	// footer shows only the first; `tt panel-render --check` prints them all.
	Problems []Problem
	opts     Options // stored at parse; used by every Line/Wrap/One on this Set's templates
}

// Problem is one positioned finding in panel.conf (§2.8). Line and Col are
// 1-based, Col counted in runes; Col is 0 when the problem belongs to the
// whole line, and Line is 0 when it belongs to the whole file. A
// Fatal problem discards the whole file for the defaults; a soft one keeps
// the template and draws the offending token in place as {raw?}.
type Problem struct {
	Line, Col int
	Msg       string
	Fatal     bool
}
