package main

// panel_tokens.go is the token table (architecture §3): what each {token} a
// panel.conf template names draws for one session.
//
// It is a table of resolver functions, not a type hierarchy: the token set is
// small and closed (users edit templates, not token code), and a map of funcs
// is the language-native way to extract five-plus branches. The table is also
// the single source of truth for which tokens and options exist —
// panelRegistry is derived from it, so the parser can never know a token the
// renderer cannot draw, or the reverse.
//
// Two rules are stated separately on purpose (R5):
//   - chip TONE follows s.Attention on every row, band or not, including an
//     UNCLEAR row whose liveness is unknown;
//   - template CHOICE (attn_* versus row/meta) follows panelRow.band, which is
//     decided by the grouping and is not touched here.
//
// Every session-supplied string passes through sanitizeText before it becomes
// a Part (§2.7), so no control character can reach the terminal and the
// engine's width arithmetic measures exactly what is drawn.

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

// The fit defaults of §2.4: the name gives way first and keeps 12 columns,
// the group-like labels next and keep 6, the model last and keeps 4.
const (
	nameShrink   = 1
	nameMinCols  = 12
	labelShrink  = 2
	labelMinCols = 6
	modelShrink  = 3
	modelMinCols = 4
	bodyShrink   = 1 // why and needs, when drawn on one line
)

// The option names the tokens accept. Declared once so the registry and the
// token functions cannot disagree about a spelling.
const (
	optChip   = "chip"
	optIcon   = "icon"
	optQuiet  = "quiet"
	optWord   = "word"
	optNoAge  = "noage"
	optShort  = "short"
	optColour = "colour"
	optColor  = "color"
	optTint   = "tint"
	optBold   = "bold"
	optFull   = "full"
	optPlain  = "plain"
	optHide   = "hide"
	optWrap   = "wrap"
	optN      = "n"
	optChar   = "char"
	optBadge  = "badge"
	optNoBar  = "nobar"
	optCode   = "code"
)

// Fixed texts a token draws. None of them is session text.
const (
	claudeModelPrefix = "claude-"
	modelDateDigits   = 8 // a trailing -YYYYMMDD
	compactGlyph      = "⟳"
	liveUnknownMark   = "live?"
	backgroundKind    = "bg"
	subSuffix         = " sub"
	apiErrSuffix      = " api err"
	unknownChipLabel  = "?"
	waitingPrefix     = "waiting "
)

// rowContext is what every row of one frame shares.
type rowContext struct {
	now time.Time
	ctx panelfmt.Thresholds
	// project is the display label of a working directory's project, "" when
	// it has none. It is resolved lazily: only a template that names
	// {project} pays for it.
	project func(cwd string) string
	// icons are panel.conf's icon_<status> glyphs, by status name; a status
	// it does not name draws its entry of defaultStatusIcons.
	icons map[string]string
	// window is the context window of a model id (panel.conf's window rules,
	// Set.Window), for the ctx bar; nil knows none.
	window func(model string) (int64, bool)
}

// tokenFunc draws one token for one row. It does the token-specific work only;
// the engine applies the common options (plain, dim, min=, max=, shrink=,
// drop=) to whatever it returns (§2.6).
type tokenFunc func(r *panelRow, rc *rowContext, tk panelfmt.Token) panelfmt.Segment

// panelToken is one entry of the table: the options the token claims for
// itself, and how it draws.
type panelToken struct {
	opts map[string]panelfmt.OptSpec
	draw tokenFunc
}

var (
	flagOpt = panelfmt.OptSpec{Kind: panelfmt.OptFlag}
	wrapOpt = panelfmt.OptSpec{Kind: panelfmt.OptInt, Min: 1, Max: 2}
	charOpt = panelfmt.OptSpec{Kind: panelfmt.OptString}
)

// panelTokens is the §3 registry. `color` is an alias entry of `colour` (#26);
// the aliases of the name option are options of their own.
var panelTokens = map[string]panelToken{
	"gutter":  {opts: map[string]panelfmt.OptSpec{optChar: charOpt}, draw: drawGutter},
	"status":  {opts: map[string]panelfmt.OptSpec{optChip: flagOpt, optIcon: flagOpt, optBadge: flagOpt, optCode: flagOpt, optQuiet: flagOpt, optWord: flagOpt, optNoAge: flagOpt}, draw: drawStatus},
	"age":     {opts: map[string]panelfmt.OptSpec{optShort: flagOpt, optQuiet: flagOpt}, draw: drawAge},
	"runtime": {opts: map[string]panelfmt.OptSpec{optBadge: flagOpt, optQuiet: flagOpt}, draw: drawRuntime},
	"waiting": {draw: drawWaiting},
	"name":    {opts: map[string]panelfmt.OptSpec{optColour: flagOpt, optColor: flagOpt, optTint: flagOpt, optBold: flagOpt, optBadge: flagOpt}, draw: drawName},
	"group":   {draw: drawGroup},
	"project": {draw: drawProject},
	"tmux":    {draw: drawTmux},
	"pane":    {opts: map[string]panelfmt.OptSpec{optFull: flagOpt}, draw: drawPane},
	"colour":  {draw: drawColour},
	"color":   {draw: drawColour},
	"ctx":     {opts: map[string]panelfmt.OptSpec{optPlain: flagOpt, optHide: flagOpt, optNoBar: flagOpt}, draw: drawCtx},
	"ctxbar":  {draw: drawCtxBar},
	"tokens":  {draw: drawTokens},
	"model":   {opts: map[string]panelfmt.OptSpec{optShort: flagOpt}, draw: drawModel},
	"compact": {draw: drawCompact},
	"wait":    {draw: drawWait},
	"needs":   {opts: map[string]panelfmt.OptSpec{optWrap: wrapOpt}, draw: drawNeeds},
	"why":     {opts: map[string]panelfmt.OptSpec{optWrap: wrapOpt}, draw: drawWhy},
	"sub":     {opts: map[string]panelfmt.OptSpec{optN: flagOpt}, draw: drawSub},
	"apierr":  {opts: map[string]panelfmt.OptSpec{optN: flagOpt}, draw: drawAPIErr},
	"live":    {draw: drawLive},
	"kind":    {draw: drawKind},
	"id":      {draw: drawID},
}

// panelRegistry is what the parser checks templates against, derived from
// the table so the two cannot drift.
var panelRegistry = func() panelfmt.Registry {
	reg := make(panelfmt.Registry, len(panelTokens))
	for name, t := range panelTokens {
		reg[name] = panelfmt.TokenSpec{Options: t.opts}
	}
	return reg
}()

// rowResolver is the engine's Resolver for one row.
func rowResolver(r *panelRow, rc *rowContext) panelfmt.Resolver {
	return func(tk panelfmt.Token) panelfmt.Segment {
		t, ok := panelTokens[tk.Name]
		if !ok {
			// The engine never asks for a token the registry refused; an
			// Empty segment is the harmless answer if it ever did.
			return panelfmt.Segment{Empty: true}
		}
		return t.draw(r, rc, tk)
	}
}

// ---------------------------------------------------------------------------
// Sanitising (§2.7)
// ---------------------------------------------------------------------------

// sanitizeText makes session text safe to draw: every C0 control (TAB, CR and
// LF included), DEL and C1 control becomes a space, runs of whitespace
// collapse to one space, and the ends are trimmed. An ESC can therefore never
// reach the terminal, and ansi.StringWidth measures what is actually drawn.
func sanitizeText(s string) string {
	if isCleanText(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	gap := false
	for _, r := range s {
		if isControlRune(r) || unicode.IsSpace(r) {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte(' ')
		}
		gap = false
		b.WriteRune(r)
	}
	return b.String()
}

// isCleanText is sanitizeText's fast path: nothing to replace, no run of
// spaces, nothing to trim — the common case for a session name.
func isCleanText(s string) bool {
	prevSpace := true // a leading space is not clean
	for _, r := range s {
		switch {
		case isControlRune(r):
			return false
		case r == ' ':
			if prevSpace {
				return false
			}
			prevSpace = true
		case unicode.IsSpace(r):
			return false
		default:
			prevSpace = false
		}
	}
	return !prevSpace || s == ""
}

// isControlRune is C0, DEL or C1.
func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// ---------------------------------------------------------------------------
// Segment helpers
// ---------------------------------------------------------------------------

// textSeg is one part of session text, sanitised; Empty when nothing is left.
func textSeg(text string, style lipgloss.Style) panelfmt.Segment {
	text = sanitizeText(text)
	if text == "" {
		return panelfmt.Segment{Empty: true}
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: text, Style: style}}}
}

// shrinkable sets a segment's fit fields: it gives way at rank, down to min
// columns (or its own width, when that is less).
func shrinkable(seg panelfmt.Segment, rank, minCols int) panelfmt.Segment {
	if seg.Empty {
		return seg
	}
	w := 0
	for _, p := range seg.Parts {
		w += ansi.StringWidth(p.Text)
	}
	seg.Shrink, seg.Min = rank, min(minCols, w)
	return seg
}

// hasOpt reports whether the token carries any of the named options.
func hasOpt(tk panelfmt.Token, names ...string) bool {
	for _, o := range tk.Opts {
		for _, n := range names {
			if o.Name == n {
				return true
			}
		}
	}
	return false
}

// optValue is the value of a string option, and whether it was given.
func optValue(tk panelfmt.Token, name string) (string, bool) {
	for _, o := range tk.Opts {
		if o.Name == name {
			return o.Value, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// chipLabels are the status words a chip shows, 4 columns at most (§3). An
// attention row shows its word in upper case, so loudness survives a terminal
// with no colour at all.
var chipLabels = map[observer.Status]string{
	observer.StatusWaiting: "wait",
	observer.StatusBlocked: "blck",
	observer.StatusBusy:    "busy",
	observer.StatusWorking: "work",
	observer.StatusIdle:    "idle",
	observer.StatusShell:   "sh",
	observer.StatusUnknown: unknownChipLabel,
	observer.StatusDone:    "done",
	observer.StatusFailed:  "fail",
}

// statusLabel is the chip's word for a session. A status this build has never
// seen shows its own first four columns, sanitised, rather than a guess.
func statusLabel(s observer.Session) string {
	label, ok := chipLabels[s.Status]
	if !ok {
		label = sanitizeText(string(s.Status))
		if label == "" {
			label = unknownChipLabel
		}
		label = ansi.Truncate(label, chipLabelCols, "")
	}
	if s.Attention {
		label = strings.ToUpper(label)
	}
	return label
}

// statusCodes are the three-letter codes {status:badge} shows (feedback 3).
// Upper case on every row: the badge's colour, not its case, says whether a
// row needs you. shell is "SH", padded to three by the badge.
var statusCodes = map[observer.Status]string{
	observer.StatusBusy:    "RUN",
	observer.StatusWorking: "WRK",
	observer.StatusWaiting: "ASK",
	observer.StatusBlocked: "BLK",
	observer.StatusIdle:    "IDL",
	observer.StatusShell:   "SH",
	observer.StatusUnknown: unclearCode,
	observer.StatusDone:    "END",
	observer.StatusFailed:  "ERR",
}

// unclearCode is the code of an unknown status, and of any status this build
// has never seen: never a guess at what it might mean.
const unclearCode = "???"

// statusCode is a session's three-letter code.
func statusCode(s observer.Session) string {
	if c, ok := statusCodes[s.Status]; ok {
		return c
	}
	return unclearCode
}

// badgeToneOf is a session's status badge tone. It follows s.Attention on
// EVERY row (R5), as the chip's does: blocked is loud coral, any other
// attention loud honey. Everything else is the soft tone of its status.
func badgeToneOf(s observer.Session) badgeTone {
	if s.Attention {
		if s.Status == observer.StatusBlocked {
			return badgeBlk
		}
		return badgeAsk
	}
	return softBadgeTone(s)
}

// softBadgeTone is the tone a session gets without attention — and what
// {status:badge:quiet} forces.
func softBadgeTone(s observer.Session) badgeTone {
	switch s.Status {
	case observer.StatusBusy, observer.StatusWorking:
		return badgeRun
	case observer.StatusIdle:
		return badgeIdle
	case observer.StatusShell:
		return badgeShell
	case observer.StatusDone:
		return badgeEnded
	case observer.StatusFailed:
		return badgeFail
	case observer.StatusWaiting:
		return badgeAskSoft
	case observer.StatusBlocked:
		return badgeBlkSoft
	}
	return badgeUnclear
}

// panelRuntime is how long a session has been running (since StartedAt), as
// the panel's clock sees it; -1 when it has no start time, rendered "—".
func panelRuntime(s observer.Session, now time.Time) time.Duration {
	if s.StartedAt == nil {
		return -1
	}
	return max(now.Sub(*s.StartedAt), 0)
}

// The statuses an icon_<status> key names (panelfmt.IconNames), and the
// built-in glyph of each. Every one is a single Emoji_Presentation codepoint:
// no U+FE0F and no ZWJ sequence, whose widths terminals (Blink over SSH among
// them) disagree on. Each is two columns, the icon slot's width.
//
// busy and work draw 🔄 (U+1F504), not ⚡ (U+26A1): a BMP symbol that predates
// emoji was narrow in pre-Unicode-9 width tables, and some fonts still draw ⚡
// one column wide while x/ansi counts two, which shifted every busy row left
// by a column. A plane-1 emoji (U+1Fxxx) has been two columns everywhere from
// the start, so prefer one when a glyph changes.
const (
	iconBusy    = "busy"
	iconWork    = "work"
	iconWaiting = "waiting"
	iconBlocked = "blocked"
	iconIdle    = "idle"
	iconShell   = "shell"
	iconUnclear = "unclear"
	iconEnded   = "ended"
)

var defaultStatusIcons = map[string]string{
	iconBusy:    "\U0001F504", // anticlockwise arrows button
	iconWork:    "\U0001F504", // anticlockwise arrows button
	iconWaiting: "\u23F3",     // hourglass with flowing sand
	iconBlocked: "\u26D4",     // no entry
	iconIdle:    "\U0001F4A4", // zzz
	iconShell:   "\U0001F41A", // spiral shell
	iconUnclear: "\u2753",     // question mark
	iconEnded:   "\u2705",     // check mark button
}

// iconStatusOf is the icon key a session's status draws. A status this build
// has never seen is unclear, as its chip would be "?".
func iconStatusOf(s observer.Session) string {
	switch s.Status {
	case observer.StatusBusy:
		return iconBusy
	case observer.StatusWorking:
		return iconWork
	case observer.StatusWaiting:
		return iconWaiting
	case observer.StatusBlocked:
		return iconBlocked
	case observer.StatusIdle:
		return iconIdle
	case observer.StatusShell:
		return iconShell
	case observer.StatusDone, observer.StatusFailed:
		return iconEnded
	}
	return iconUnclear
}

// statusIcon is the glyph a session's {status:icon} draws: panel.conf's, else
// the built-in one.
func statusIcon(s observer.Session, icons map[string]string) string {
	key := iconStatusOf(s)
	if g, ok := icons[key]; ok {
		return g
	}
	return defaultStatusIcons[key]
}

// statusTone is the chip's tone. It follows s.Attention on EVERY row (R5):
// blocked is coral, any other attention is honey. Everything else is calm.
func statusTone(s observer.Session) chipTone {
	if s.Attention {
		if s.Status == observer.StatusBlocked {
			return chipCoral
		}
		return chipHoney
	}
	return calmTone(s)
}

// calmTone is the tone a session gets without attention — and what
// {status:quiet} forces.
func calmTone(s observer.Session) chipTone {
	switch s.Status {
	case observer.StatusBusy, observer.StatusWorking:
		return chipBusy
	case observer.StatusFailed:
		return chipFail
	}
	return chipDim
}

func drawStatus(r *panelRow, rc *rowContext, tk panelfmt.Token) panelfmt.Segment {
	s := r.s
	tone := statusTone(s)
	if hasOpt(tk, optQuiet) {
		tone = calmTone(s)
	}
	switch {
	case hasOpt(tk, optWord):
		style := panelMetaStyle
		if s.Attention && !hasOpt(tk, optQuiet) {
			style = panelAlertStyle.Bold(true)
		}
		word := sanitizeText(string(s.Status))
		if word == "" {
			word = string(observer.StatusUnknown)
		}
		return panelfmt.Segment{Parts: []panelfmt.Part{{Text: word, Style: style}}}
	case hasOpt(tk, optIcon):
		return statusIconCell(sanitizeText(statusIcon(s, rc.icons)))
	case hasOpt(tk, optCode):
		return statusCodeBadge(statusCode(s), statusBadgeTone(s, tk))
	case hasOpt(tk, optBadge):
		return statusBadge(statusCode(s), fmtRuntime(panelRuntime(s, rc.now)), statusBadgeTone(s, tk))
	case hasOpt(tk, optNoAge):
		return statusChipLabel(statusLabel(s), tone)
	}
	return statusChip(statusLabel(s), fmtAgeShort(panelAge(s, rc.now)), tone)
}

// statusBadgeTone is the badge tone a token draws a session in: its own
// (badgeToneOf), or the soft one when the token says :quiet.
func statusBadgeTone(s observer.Session, tk panelfmt.Token) badgeTone {
	if hasOpt(tk, optQuiet) {
		return softBadgeTone(s)
	}
	return badgeToneOf(s)
}

// drawAge is how long the session has been in its status, as plain text: no
// fill. It is slate, except on a row whose session has Attention, where it
// carries the loud chip's hue as bold text (honey, or the alert coral for
// blocked), so the attention cue survives an icon that has no colour of its
// own. {age:quiet} keeps it slate. age:short is right-aligned in 3 columns,
// so the names after it line up whatever the age.
func drawAge(r *panelRow, rc *rowContext, tk panelfmt.Token) panelfmt.Segment {
	age := panelAge(r.s, rc.now)
	text := panelHumanise(age)
	if hasOpt(tk, optShort) {
		text = alignRight(fmtAgeShort(age), chipAgeCols)
	}
	tone := statusTone(r.s)
	if hasOpt(tk, optQuiet) {
		tone = calmTone(r.s)
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: text, Style: ageStyleFor(tone)}}}
}

// drawRuntime is {runtime}: how long the session has been running, as plain
// slate text (fmtRuntime). Empty when it has no start time. {status:badge}
// carries the same value in its right half.
//
// {runtime:badge} (feedback 4, the built-in row) is the value as its own
// fixed-width badge: solid in the loud hue on an Attention row, the neutral
// value fill otherwise ({runtime:badge:quiet} always neutral). It is never
// Empty: with no start time it shows "—", so the names after it still align.
func drawRuntime(r *panelRow, rc *rowContext, tk panelfmt.Token) panelfmt.Segment {
	d := panelRuntime(r.s, rc.now)
	if hasOpt(tk, optBadge) {
		return runtimeBadge(fmtRuntime(d), statusBadgeTone(r.s, tk))
	}
	if d < 0 {
		return panelfmt.Segment{Empty: true}
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: fmtRuntime(d), Style: panelMetaStyle}}}
}

// drawWaiting is {waiting}: "waiting 13h", how long a session that needs you
// has been in its status (age:short), in the loud hue of its row (bold
// honey, or the alert coral for blocked). It opens the attention why line,
// since the status badge shows the runtime, not this. Empty on a row without
// Attention, and when the status time is not known.
func drawWaiting(r *panelRow, rc *rowContext, _ panelfmt.Token) panelfmt.Segment {
	age := panelAge(r.s, rc.now)
	if !r.s.Attention || age < 0 {
		return panelfmt.Segment{Empty: true}
	}
	return panelfmt.Segment{Parts: []panelfmt.Part{{Text: waitingPrefix + fmtAgeShort(age), Style: ageStyleFor(statusTone(r.s))}}}
}

// ---------------------------------------------------------------------------
// Identity: gutter, name, group-like labels, colour
// ---------------------------------------------------------------------------

func drawGutter(r *panelRow, _ *rowContext, tk panelfmt.Token) panelfmt.Segment {
	glyph, ok := optValue(tk, optChar)
	if !ok {
		glyph = gutterGlyph
	}
	return gutterCell(sessionHue(panelAgentColor(r.s)), sanitizeText(glyph))
}

// rowName is the name a row shows: the session's own, else its short id.
func rowName(s observer.Session) string {
	if name := sanitizeText(s.Name); name != "" {
		return name
	}
	return sanitizeText(panelShortID(s))
}

func drawName(r *panelRow, _ *rowContext, tk panelfmt.Token) panelfmt.Segment {
	if hasOpt(tk, optBadge) {
		name := rowName(r.s)
		if name == "" {
			return panelfmt.Segment{Empty: true} // no name and no id to show
		}
		seg := nameBadge(name, sessionHue(panelAgentColor(r.s)), r.class == groupEnded)
		if hasOpt(tk, optBold) {
			for i := range seg.Parts {
				seg.Parts[i].Style = seg.Parts[i].Style.Bold(true)
			}
		}
		return shrinkable(seg, nameShrink, nameMinCols)
	}
	style := panelNameStyle
	switch {
	case r.class == groupEnded:
		style = endedNameStyle // ENDED keeps its gutter and recedes the name
	case hasOpt(tk, optColour, optColor, optTint):
		style = nameStyle(sessionHue(panelAgentColor(r.s)))
	}
	if hasOpt(tk, optBold) {
		style = style.Bold(true)
	}
	return shrinkable(textSeg(rowName(r.s), style), nameShrink, nameMinCols)
}

// drawGroup is the row's own group label: a band row's home, the real group
// of a row folded into "other". Empty when the name already says it.
func drawGroup(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	label := sanitizeText(r.label)
	if label == "" || nameSays(rowName(r.s), label) {
		return panelfmt.Segment{Empty: true}
	}
	return shrinkable(textSeg(label, panelMetaStyle), labelShrink, labelMinCols)
}

func drawProject(r *panelRow, rc *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if rc.project == nil {
		return panelfmt.Segment{Empty: true}
	}
	return shrinkable(textSeg(rc.project(r.s.CWD), panelMetaStyle), labelShrink, labelMinCols)
}

func drawTmux(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if r.s.Tmux == nil {
		return panelfmt.Segment{Empty: true}
	}
	return shrinkable(textSeg(r.s.Tmux.Session, panelMetaStyle), labelShrink, labelMinCols)
}

func drawPane(r *panelRow, _ *rowContext, tk panelfmt.Token) panelfmt.Segment {
	t := r.s.Tmux
	if t == nil {
		return panelfmt.Segment{Empty: true}
	}
	if hasOpt(tk, optFull) && t.Raw != "" {
		return textSeg(t.Raw, panelMetaStyle)
	}
	return textSeg(t.Pane, panelMetaStyle)
}

// drawColour is the /color name in its own hue; a name with no hue (a hex
// agentColor, say) is still said, in slate.
func drawColour(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	name := panelAgentColor(r.s)
	style := panelMetaStyle
	if h := sessionHue(name); h.known() {
		style = lipgloss.NewStyle().Foreground(h.gutter)
	}
	return textSeg(name, style)
}

// ---------------------------------------------------------------------------
// Transcript facts: context, model, compactions, subagents, errors
// ---------------------------------------------------------------------------

// contextTokens is the measured context size, or nil when it was not measured.
func contextTokens(s observer.Session) *int64 {
	if s.Transcript == nil {
		return nil
	}
	return s.Transcript.ContextTokens
}

// drawCtx is {ctx}: the count badge and, by default, the bar filled against
// the model's window (feedback 3). {ctx:nobar} is the badge alone and
// {ctx:plain} the bare count, as before the bar.
func drawCtx(r *panelRow, rc *rowContext, tk panelfmt.Token) panelfmt.Segment {
	used := contextTokens(r.s)
	if used == nil && hasOpt(tk, optHide) {
		return panelfmt.Segment{Empty: true}
	}
	tone := ctxRowTone(used, rc.ctx, r.s.Attention)
	switch {
	case hasOpt(tk, optPlain):
		return ctxPlain(used, tone)
	case hasOpt(tk, optNoBar):
		return ctxBadge(used, tone)
	}
	return ctxWithBar(used, contextWindow(r.s, rc), tone)
}

// drawCtxBar is {ctxbar}: the bar alone. Empty when the count or the window
// is not known.
func drawCtxBar(r *panelRow, rc *rowContext, _ panelfmt.Token) panelfmt.Segment {
	used, window := contextTokens(r.s), contextWindow(r.s, rc)
	if used == nil || window <= 0 {
		return panelfmt.Segment{Empty: true}
	}
	return panelfmt.Segment{Parts: ctxBar(*used, window, ctxRowTone(used, rc.ctx, r.s.Attention))}
}

// contextWindow is the window of the session's model by panel.conf's window
// rules, or 0 when no rule knows it.
func contextWindow(s observer.Session, rc *rowContext) int64 {
	if rc.window == nil || s.Transcript == nil {
		return 0
	}
	if w, ok := rc.window(s.Transcript.Model); ok {
		return w
	}
	return 0
}

func drawTokens(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	used := contextTokens(r.s)
	if used == nil {
		return panelfmt.Segment{Empty: true}
	}
	return textSeg(fmtTokens(*used), panelMetaStyle)
}

// shortModel drops the "claude-" prefix and a trailing -YYYYMMDD:
// claude-opus-5-5-20260901 is opus-5-5.
func shortModel(m string) string {
	m = strings.TrimPrefix(m, claudeModelPrefix)
	if i := len(m) - modelDateDigits - 1; i > 0 && m[i] == '-' && allASCIIDigits(m[i+1:]) {
		m = m[:i]
	}
	return m
}

func allASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func drawModel(r *panelRow, _ *rowContext, tk panelfmt.Token) panelfmt.Segment {
	if r.s.Transcript == nil {
		return panelfmt.Segment{Empty: true}
	}
	m := sanitizeText(r.s.Transcript.Model)
	if hasOpt(tk, optShort) {
		m = shortModel(m)
	}
	return shrinkable(textSeg(m, panelMetaStyle), modelShrink, modelMinCols)
}

func drawCompact(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if r.s.Transcript == nil || r.s.Transcript.Compactions <= 0 {
		return panelfmt.Segment{Empty: true}
	}
	return textSeg(compactGlyph+strconv.Itoa(r.s.Transcript.Compactions), panelMetaStyle)
}

// subagents is the known, positive subagent count.
func subagents(s observer.Session) (int, bool) {
	t := s.Transcript
	if t == nil || !t.SubagentsKnown || t.Subagents == nil || *t.Subagents <= 0 {
		return 0, false
	}
	return *t.Subagents, true
}

func drawSub(r *panelRow, _ *rowContext, tk panelfmt.Token) panelfmt.Segment {
	n, ok := subagents(r.s)
	if !ok {
		return panelfmt.Segment{Empty: true}
	}
	text := strconv.Itoa(n)
	if !hasOpt(tk, optN) {
		text += subSuffix
	}
	return textSeg(text, panelMetaStyle)
}

// drawAPIErr is isApiErrorMessage records in the transcript's tail, in the
// dim rose the palette gives errors that are not a call to act.
func drawAPIErr(r *panelRow, _ *rowContext, tk panelfmt.Token) panelfmt.Segment {
	if r.s.Transcript == nil || r.s.Transcript.APIErrors <= 0 {
		return panelfmt.Segment{Empty: true}
	}
	text := strconv.Itoa(r.s.Transcript.APIErrors)
	if !hasOpt(tk, optN) {
		text += apiErrSuffix
	}
	return textSeg(text, panelRoseStyle)
}

// ---------------------------------------------------------------------------
// Why a session waits, and what the observer could not tell
// ---------------------------------------------------------------------------

func drawWait(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if r.s.WaitReason == observer.WaitReasonNone {
		return panelfmt.Segment{Empty: true}
	}
	return textSeg(string(r.s.WaitReason), panelMetaStyle)
}

// needsText is what a blocked agent says it needs — the one field here that
// carries text a user wrote. Only a blocked session's counts, as before.
func needsText(s observer.Session) string {
	if s.Needs == nil || s.Status != observer.StatusBlocked {
		return ""
	}
	return sanitizeText(*s.Needs)
}

func drawNeeds(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	return shrinkable(textSeg(needsText(r.s), panelBodyStyle), bodyShrink, 1)
}

// drawWhy is the attention body: what a blocked agent needs, else why the
// session waits.
func drawWhy(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	return shrinkable(textSeg(whyText(r.s), panelBodyStyle), bodyShrink, 1)
}

// whyText is what a session waits for: a blocked agent's needs, else its
// wait reason, else "".
func whyText(s observer.Session) string {
	if why := needsText(s); why != "" {
		return why
	}
	if s.WaitReason != observer.WaitReasonNone {
		return string(s.WaitReason)
	}
	return ""
}

// drawLive is §5.5(a)'s marker for a live: null row plus the first advisory's
// code, so the UNCLEAR group explains itself.
func drawLive(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if r.s.Live != nil {
		return panelfmt.Segment{Empty: true}
	}
	return textSeg(liveMarker(r.s), panelMetaStyle)
}

// liveMarker is "live?" and the first advisory's code: what an UNCLEAR row
// says about itself.
func liveMarker(s observer.Session) string {
	marker := liveUnknownMark
	if len(s.Advisories) > 0 {
		if code := sanitizeText(s.Advisories[0].Code); code != "" {
			marker += " " + code
		}
	}
	return marker
}

func drawKind(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if r.s.Kind != observer.KindBackground {
		return panelfmt.Segment{Empty: true}
	}
	return textSeg(backgroundKind, panelMetaStyle)
}

func drawID(r *panelRow, _ *rowContext, _ panelfmt.Token) panelfmt.Segment {
	if r.s.ShortID == nil {
		return panelfmt.Segment{Empty: true}
	}
	return textSeg(*r.s.ShortID, panelMetaStyle)
}
