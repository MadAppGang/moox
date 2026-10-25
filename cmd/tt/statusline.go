package main

// statusline.go is §5.5(c): the tmux status-line indicator, the only genuinely
// always-on surface tmux can give.
//
// tmux has no global pane — a pane belongs to a window and there is no object
// above windows — but it does have a status line, drawn in every window of
// every client, and a format variable in it costs no pane, no PTY and no
// process. So the division of labour is:
//
//	the status line answers "is anything waiting?" for free and always;
//	the panel answers "for how long?" while you are looking (§5.5a).
//
// # How it is fed without a timer and without a poll
//
// `tt observe` writes one global tmux option on each coalesced state change,
// from the same serialized loop that publishes snapshots, and only when the
// rendered string DIFFERS from the last one written:
//
//	tmux set-option -gq @tt_status "<rendered>"
//
// and that one exec is the whole cost. §5.5(c) also issued `refresh-client -S`
// after it; MEASURED on tmux 3.7c against an isolated server, with
// status-interval 0 so nothing else could have repainted, the set-option ALONE
// redraws every attached client's status line (setting any option pushes a
// redraw to every client). The refresh was a second exec per change that did
// nothing, and from a process with no client of its own it could only target
// whichever client tmux judged "current" — so it is gone. Measured event rate
// is three per nine minutes, so that is ~0.33 execs a minute and exactly zero
// while nothing changes. The rejected alternative —
// `#(tt sessions --status-line)` in the status line — makes tmux FORK that
// process on every status redraw, i.e. every 5 s, per client, forever. That is
// a poll loop with extra steps and the NFR forbids it.
//
// # Why writing a GLOBAL option is allowed here when §2.3 forbids it
//
// §2.3 permits exactly one tmux write: a pane-scoped option on tt's own pane,
// justified because it dies with the pane. @tt_status is global, which is the
// shape that broke in C1. The distinction that makes it acceptable is stated
// in §5.5(c) and is worth repeating where the code is:
//
//	@tt_status holds a DISPLAY STRING, not a reference to an object. A stale
//	pane id is destructive — something acts on it. A stale count is cosmetic.
//
// And three guards, because "cosmetic" is not "free":
//
//  1. OPT-IN, AUTO-DETECTED. --status-line=auto (the default) reads
//     status-right, status-left and status-format ONCE at startup and enables
//     the writer only if @tt_status appears in any of them. With the placeholder absent the writer
//     issues ZERO tmux calls, so §2.3's guarantee is unchanged for anyone who
//     has not asked for this. --status-line=always|never override.
//  2. CLEARED ON EXIT. set-option -gu @tt_status on SIGTERM/SIGINT. On a crash
//     the last string persists — the honest residual, bounded to a wrong count
//     in a status bar and clearable with one command.
//  3. WRITE-ONLY. Nothing reads @tt_status back, so it cannot become
//     load-bearing state the way a stored pane id did.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/MadAppGang/moox/internal/observer"
)

// optStatus is the one global option tt writes, and §2.3's enumerated list
// grows by exactly this one entry.
const optStatus = "@tt_status"

// The theme markers the user's theme script publishes. They are what lets a
// badge follow a light/dark switch INSTANTLY: the colours below are emitted as
// a tmux conditional on them, so flipping the theme repaints the bar without tt
// being told anything.
//
//   - optThemeDark is the per-client predicate: its value is a format that
//     expands to 1 or 0 for the client whose bar is being drawn, so each
//     screen attached to one server paints its own badges.
//   - optThemeMode is the older, server-wide marker ("light" or "dark"), the
//     fallback on a server that does not publish the predicate.
//
// With neither published, the bar's own background is read for its luminance,
// and failing that the mode is "dark".
const (
	optThemeDark   = "@theme_dark"
	optThemeMode   = "@theme_mode"
	optThemeMantle = "@thm_mantle"
	optThemeBg     = "@thm_bg"
)

// statusLineLeadIcon marks the whole group as Claude's before the first badge:
// a robot, in the bar's own foreground rather than on a fill, because it is a
// label and not a state. It is the Material Design robot (U+F06A9) — the same
// Nerd Font subfamily as the host and clock chips already on this bar — and it
// is dropped by setting its option to a space.
const (
	optIconLead         = "@tt_icon_claude"
	statusLineLeadIcon  = "\U000F06A9"
	statusLineLeadSpace = " "
)

// The three badges, in render order: what each counts, the glyph it carries,
// and its fill in each mode.
//
// # Why tt owns these colours instead of using the user's palette
//
// It did use the palette, and the result was measured on the user's own bar and
// rejected in one sentence: "the colour are islieading, 5 looks like red and
// erro looks liek ntohg hapeing". The fills were @thm_peach (#af3a03, a burnt
// orange that reads as ALARM) for waiting and @thm_red (#f2c2c2, a pale wash)
// for errors — exactly inverted. That is not a bad palette; it is a palette
// tuned for a different job, and its own file says so: theme.sh's light section
// states that chip fills are "soft washes carrying dark text" and that "THREE
// ENTRIES BREAK THAT RULE ON PURPOSE — blue, peach and teal are saturated …
// used as foreground strokes … and never as a chip fill". Semantic weight —
// error louder than waiting, unclear quieter than both — cannot be borrowed
// from slots that were not tuned for it.
//
// So the fills are tt's, one per mode, and they are chosen to sit in the user's
// two palettes rather than against them: the light amber and sand ARE that
// theme's @thm_yellow and @thm_overlay_0, the light red is the Gruvbox neutral
// red the theme derives from, and the dark three are the Catppuccin peach and
// surface with tt's own panel error colour (theme.go's colorError), so the
// sidebar and the bar agree on what "error" looks like.
//
// The ordering rule is the theme file's own: "If red and orange ever start
// looking alike, move orange toward yellow rather than shifting red" — which is
// why waiting is an amber rather than an orange.
//
// The glyphs are Nerd Font Font-Awesome (the user's terminal is JetBrains Mono
// NL Nerd Font Propo, and their bar already carries Material glyphs), and each
// is overridable with a tmux option for a font that lacks it:
//
//	set -g @tt_icon_waiting "!"
var statusLineBadges = []struct {
	// name is the badge and the word its wordless form falls back to.
	name string
	// option is the tmux option that overrides the glyph.
	option string
	// icon is the default glyph: a bell (someone is waiting for YOU), a
	// warning triangle, and a question mark.
	icon string
	// light and dark are the fills.
	light, dark string
}{
	{"waiting", "@tt_icon_waiting", "\uf0f3", "#d79921", "#fab387"},
	{"error", "@tt_icon_error", "\uf071", "#cc241d", "#ff5f5f"},
	{"unclear", "@tt_icon_unclear", "\uf059", "#bdae93", "#585b70"},
}

// statusLinePaint is everything the row needs to draw itself: the group glyph
// that leads it, and how each badge is drawn, keyed by its name.
type statusLinePaint struct {
	lead   string
	badges map[string]statusLineBadgeStyle
}

// statusLineBadgeStyle is one badge's parts, each already in the spelling tmux
// wants — a literal colour, or the theme conditional (statusLineByMode) that
// resolves when the bar is painted.
type statusLineBadgeStyle struct{ fill, ink, icon string }

// The two inks. They are tt's own literals rather than the user's @thm_crust,
// because the fills are tt's own now: a fill and its ink have to be measured
// together, and measuring against a colour that can change between publishes
// would be measuring the wrong thing.
const (
	statusLineDarkInk  = "#11111b"
	statusLineLightInk = "#ffffff"
)

// newStatusLinePaint resolves every badge for a palette mode.
//
// mode is "light" or "dark" to pin the colours, or "" to emit a CONDITIONAL on
// the theme markers the user's own theme script publishes, so the badges
// follow a light/dark switch the moment it happens rather than at the next
// state change.
func newStatusLinePaint(mode string, icons map[string]string) statusLinePaint {
	paint := statusLinePaint{
		lead:   statusLineLeadIcon,
		badges: map[string]statusLineBadgeStyle{},
	}
	// An option tmux reports as EMPTY is one the user never set, and keeps the
	// default. A non-empty value is an override, even when it trims to
	// nothing: `set -g @tt_icon_claude " "` is how a glyph is removed.
	if raw, ok := icons[optIconLead]; ok && raw != "" {
		paint.lead = strings.TrimSpace(raw)
	}
	for _, badge := range statusLineBadges {
		icon := badge.icon
		if raw, ok := icons[badge.option]; ok && raw != "" {
			icon = strings.TrimSpace(raw)
		}
		style := statusLineBadgeStyle{icon: icon}
		switch mode {
		case "light":
			style.fill, style.ink = badge.light, statusLineInkFor(badge.light)
		case "dark":
			style.fill, style.ink = badge.dark, statusLineInkFor(badge.dark)
		default:
			style.fill = statusLineByMode(badge.light, badge.dark)
			style.ink = statusLineByMode(statusLineInkFor(badge.light), statusLineInkFor(badge.dark))
		}
		paint.badges[badge.name] = style
	}
	return paint
}

// statusLineByMode is the tmux conditional that picks light or dark when the
// bar is painted, in two layers:
//
//	#{?@theme_dark,#{?#{E:@theme_dark},DARK,LIGHT},#{?#{==:#{@theme_mode},light},LIGHT,DARK}}
//
// When @theme_dark is published, `#{?@theme_dark,…}` takes the first branch.
// That test only asks whether the option is non-empty; the inner #{E:…}
// evaluates the predicate, for the client drawing this bar, so two screens of
// opposite themes see their own fills at once. On a server that does not
// publish it, the second branch decides by @theme_mode, byte for byte the
// conditional this emitted before; an unset @theme_mode takes the dark branch,
// the safer guess for a terminal nobody has described.
//
// light and dark MUST be comma-free (they are hex colours): a comma ends a tmux conditional branch.
func statusLineByMode(light, dark string) string {
	return "#{?" + optThemeDark + ",#{?#{E:" + optThemeDark + "}," + dark + "," + light + "}," +
		"#{?#{==:#{" + optThemeMode + "},light}," + light + "," + dark + "}}"
}

// statusLineInkFor picks the ink for one fill by WCAG contrast: the better of
// tt's two, always.
func statusLineInkFor(fill string) string {
	bg, ok := parseHexColour(fill)
	if !ok {
		return statusLineDarkInk
	}
	dark, _ := parseHexColour(statusLineDarkInk)
	light, _ := parseHexColour(statusLineLightInk)
	if contrastRatio(light, bg) > contrastRatio(dark, bg) {
		return statusLineLightInk
	}
	return statusLineDarkInk
}

// parseHexColour reads #rgb or #rrggbb into sRGB components scaled to 0..1.
func parseHexColour(s string) ([3]float64, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return [3]float64{}, false
	}
	hex := s[1:]
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return [3]float64{}, false
	}
	var out [3]float64
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return [3]float64{}, false
		}
		out[i] = float64(v) / 255
	}
	return out, true
}

// relativeLuminance is WCAG 2.1's, on sRGB components already scaled to 0..1.
func relativeLuminance(c [3]float64) float64 {
	lin := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}

// contrastRatio is WCAG 2.1's, between 1 and 21.
func contrastRatio(a, b [3]float64) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// statusLineMode is --status-line's value.
type statusLineMode int

const (
	// statusLineAuto enables the writer only when the placeholder is present.
	statusLineAuto statusLineMode = iota
	// statusLineAlways writes regardless of detection, for a user who
	// interpolates @tt_status somewhere this detection cannot see (inside
	// another option that status-right expands with #{E:…}, say).
	statusLineAlways
	// statusLineNever disables it entirely, and is the one-flag answer to a
	// reviewer who rejects §11 assumption 10's widening.
	statusLineNever
)

// parseStatusLineMode maps the flag spelling. An unrecognised value is a usage
// error rather than a silent default: "--status-line=on" meaning "never" is
// exactly the surprise this rejects.
func parseStatusLineMode(s string) (statusLineMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return statusLineAuto, true
	case "always":
		return statusLineAlways, true
	case "never":
		return statusLineNever, true
	default:
		return statusLineAuto, false
	}
}

func (m statusLineMode) String() string {
	switch m {
	case statusLineAlways:
		return "always"
	case statusLineNever:
		return "never"
	default:
		return "auto"
	}
}

// statusLineSource is what the writer needs from the observer: the same
// event stream the socket server subscribes to, plus the published snapshot
// those events describe.
//
// It takes the COUNTS from the published snapshot rather than folding deltas
// itself, because the counts in the envelope are recomputed over the visible
// set by the core's own counting function — a second tally here would be a
// second answer to the question a user reads first.
type statusLineSource interface {
	Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error)
	Latest() *observer.Snapshot
}

// statusLineWriter owns the option. One instance per `tt observe` process.
type statusLineWriter struct {
	// mu guards the two mutable fields. The writer is driven by ONE goroutine
	// in production (Run's), but its counters are read from another — by the
	// test that asserts the write count, and by the doctor (§9.5) later — and
	// `go test -race` is the thing that said so rather than a review.
	mu sync.Mutex
	// on is the result of the startup decision. When it is false NOTHING in
	// this type issues a tmux call, ever — which is the §2.3 guarantee and is
	// asserted directly by a test that counts calls through the seams.
	on bool
	// last is the string most recently written, so an unchanged aggregate
	// costs zero execs. It is the PLAIN rendering — the counts and their
	// words — because that is what changes when state changes, and comparing
	// it costs no tmux call. The styled badges are built only once this says
	// there is something new to write, which is what keeps the palette read
	// off the event path.
	last string
	// writes counts the set-option calls issued, for the tests and for the
	// doctor.
	writes int
}

// newStatusLineWriter makes the startup decision described in guard 1.
//
// The detection READS two options. Those reads are the only tmux traffic a
// non-opted-in user sees, they happen once, and they are reads: the guarantee
// §2.3 makes and §9.6 tests is about WRITES, and with the placeholder absent
// this returns a writer that issues none.
func newStatusLineWriter(mode statusLineMode) *statusLineWriter {
	w := &statusLineWriter{}
	switch mode {
	case statusLineNever:
		return w
	case statusLineAlways:
		w.on = true
	default:
		w.on = statusLinePlaceholderPresent()
	}
	return w
}

// statusLinePlaceholderPresent reports whether the user has interpolated
// @tt_status into their status line: either half, or any line of
// status-format (an array option; `show -gv` prints every entry, one per line).
func statusLinePlaceholderPresent() bool {
	for _, opt := range []string{"status-right", "status-left", "status-format"} {
		out, err := tmuxCapture("show", "-gv", opt)
		if err != nil {
			continue
		}
		if strings.Contains(out, optStatus) {
			return true
		}
	}
	return false
}

// readPaint resolves the badges against the server as it is NOW: one tmux call
// for the two theme markers, the bar's own background and the icon overrides,
// made only when the counts changed — so it is one read per real change and
// none in between.
//
// With either marker present (@theme_dark or @theme_mode; the user's theme
// script publishes them) the colours are emitted as the conditional and follow
// a light/dark switch live, per screen where @theme_dark exists. Only the
// markers' PRESENCE is read here: @theme_dark's value is a per-client format,
// and there is no one client to evaluate it for. With both absent, the bar's
// own background is measured for luminance, and the badges are pinned to that
// mode until the next change. With nothing readable, dark.
func (w *statusLineWriter) readPaint() statusLinePaint {
	opts := []string{optThemeDark, optThemeMode, optThemeMantle, optThemeBg, optIconLead}
	for _, b := range statusLineBadges {
		opts = append(opts, b.option)
	}
	format := strings.Join(mapStrings(opts, statusLineRef), "\t")
	out, err := tmuxCapture("display-message", "-p", format)
	if err != nil {
		// A tmux that will not answer still gets badges: the colours are a
		// conditional it evaluates for itself, so nothing here had to be read
		// to draw them. Only the icon overrides and the luminance fallback are
		// lost, and both have defaults.
		return newStatusLinePaint("", nil)
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\t")
	if len(got) != len(opts) {
		return newStatusLinePaint("", nil)
	}
	values := map[string]string{}
	for i, opt := range opts {
		values[opt] = got[i]
	}
	mode := ""
	if strings.TrimSpace(values[optThemeDark]) == "" && strings.TrimSpace(values[optThemeMode]) == "" {
		mode = statusLineModeFromBackground(values[optThemeMantle], values[optThemeBg])
	}
	return newStatusLinePaint(mode, values)
}

// statusLineModeFromBackground reads the mode off the bar itself, for a config
// that publishes neither theme marker: a background lighter than mid-grey is a
// light theme. An unparseable background is "dark", the safer guess.
func statusLineModeFromBackground(values ...string) string {
	for _, v := range values {
		c, ok := parseHexColour(v)
		if !ok {
			continue
		}
		if relativeLuminance(c) > 0.18 {
			return "light"
		}
		return "dark"
	}
	return "dark"
}

// statusLineRef is an option as a format for the paint read. The palette
// entries are read EXPANDED, #{E:…}: under the per-client theme each @thm_*
// holds a conditional rather than a colour, and a raw read would hand back its
// format text, which measures as no colour at all. Expanding a plain colour
// returns it unchanged. Everything else is read raw.
func statusLineRef(option string) string {
	switch option {
	case optThemeMantle, optThemeBg:
		return "#{E:" + option + "}"
	default:
		return "#{" + option + "}"
	}
}

// mapStrings is one small helper rather than a loop at each call site.
func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

// tmuxOptionSet reports whether a global option has a non-empty value.
func tmuxOptionSet(name string) bool {
	out, err := tmuxCapture("show", "-gv", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// Enabled reports whether this writer will write.
func (w *statusLineWriter) Enabled() bool { return w != nil && w.on }

// Writes is how many option writes have been issued.
func (w *statusLineWriter) Writes() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// Publish writes the indicator for this snapshot, and reports whether it
// actually wrote. An unchanged string is not written, which is what makes the
// cost proportional to real state changes rather than to events.
func (w *statusLineWriter) Publish(snap *observer.Snapshot) bool {
	if !w.Enabled() || snap == nil {
		return false
	}
	plain := renderStatusLine(snap, statusLinePaint{})

	w.mu.Lock()
	unchanged := plain == w.last
	w.mu.Unlock()
	if unchanged {
		return false
	}
	// Something changed, so this is the one moment worth a palette read: the
	// ink is re-measured against the fills as they are NOW, which is how a
	// badge stays readable across the user's light/dark switch.
	// Something changed, so this is the one moment worth a read: the mode
	// marker and the icon overrides are taken as they are NOW.
	rendered := renderStatusLine(snap, w.readPaint())
	if err := tmuxSet("set-option", "-gq", optStatus, rendered); err != nil {
		// A failed write is not a reason to stop observing, and it is not
		// reported per-event either: the doctor compares @tt_status against
		// the live counts, which catches a systematically failing writer
		// without putting a message in front of the user on every change.
		return false
	}
	w.mu.Lock()
	w.last = plain
	w.writes++
	w.mu.Unlock()
	// No refresh-client: the set-option above already repainted every client
	// (measured; see the file comment).
	return true
}

// Clear unsets the option. Guard 2: called on a clean shutdown, so an absent
// server leaves no residue in the status line.
func (w *statusLineWriter) Clear() {
	if !w.Enabled() {
		return
	}
	w.mu.Lock()
	nothingToClear := w.last == ""
	w.last = ""
	w.mu.Unlock()
	if nothingToClear {
		return
	}
	_ = tmuxSet("set-option", "-gu", optStatus)
}

// Run feeds the writer from the event stream until ctx is done, then clears.
//
// There is no timer and no poll: it blocks on the channel the publishing loop
// fans out to, and every wake-up corresponds to a real coalesced state change.
// A disabled writer returns immediately WITHOUT subscribing, so it costs not
// even a subscription.
func (w *statusLineWriter) Run(ctx context.Context, src statusLineSource) error {
	if !w.Enabled() || src == nil {
		return nil
	}
	events, err := src.Subscribe(ctx, observer.Filter{})
	if err != nil {
		return err
	}
	defer w.Clear()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			// Rendered from the PUBLISHED snapshot rather than from the event,
			// so the counts are the core's own and a delta cannot make the
			// indicator disagree with `tt sessions --json`.
			w.Publish(src.Latest())
		}
	}
}

// renderStatusLine is what the indicator says. Three BADGES, in a fixed order,
// and NOTHING that requires a clock — §5.5(c) is deliberate about that:
// "blocked 14m" in the status line would need either a server timer (deleted
// by the user's override) or a #(…) poll (forbidden). Counts change only on
// real events, so they are exactly the aggregate an event-driven writer can
// keep true.
//
//	[ 3 waiting ][ 1 error ]   attention, then sessions with something wrong
//	[ 2 waiting ][ 1 unclear ] unclear is counts.unknown (live == null, §3.5)
//	(empty)                    nothing to say — the indicator disappears
//
// The badges abut, with no separator between them. That is madbench's own
// count-badge grammar (its benchCheckChips concatenates coloured segments
// edge to edge, and every badge it draws is bold, dark-inked, and padded by
// exactly one space INSIDE the fill), and the reason to copy it here rather
// than invent something is that these two surfaces are read by the same person
// a second apart. What is deliberately NOT copied is madbench's hex palette:
// this bar switches light and dark at runtime, so the fills stay as the user's
// own @thm_* options and the ink stays @thm_crust, exactly like the chips
// already on the bar.
func renderStatusLine(snap *observer.Snapshot, paint statusLinePaint) string {
	if snap == nil {
		return ""
	}
	counts := map[string]int{
		"waiting": snap.Counts.Attention,
		"error":   statusLineErrorCount(snap),
		"unclear": snap.Counts.Unknown,
	}

	var badges, words []string
	for _, badge := range statusLineBadges {
		n := counts[badge.name]
		if n <= 0 {
			continue
		}
		words = append(words, strconv.Itoa(n)+" "+badge.name)
		if style, ok := paint.badges[badge.name]; ok {
			body := strconv.Itoa(n)
			if style.icon != "" {
				body = style.icon + " " + body
			}
			badges = append(badges, fmt.Sprintf("#[fg=%s,bg=%s,bold] %s #[default]",
				style.ink, style.fill, body))
		}
	}
	if len(words) == 0 {
		return ""
	}
	if len(badges) != len(words) {
		// Nothing to paint with (the plain rendering the change detector
		// compares, or a tmux that would not answer): the words, joined by the
		// separator the rest of this bar uses. A bare number with no fill and
		// no glyph would say nothing at all.
		return strings.Join(words, " · ")
	}
	row := strings.Join(badges, "")
	if paint.lead != "" {
		row = paint.lead + statusLineLeadSpace + row
	}
	return row
}

// statusLineErrorCount is the "something went wrong" tally.
//
// §5.5(c) names "sessions with apiErrors > 0", and that is counted here — but
// it cannot be the whole of it, and the reason is structural rather than a
// preference: a watch-driven rebuild is built with IncludeTranscripts:false,
// so apiErrors is absent from the very snapshots this writer renders and the
// count would be permanently zero. A `failed` session is the same class of
// fact and IS present, so the two are counted as one set — a user is not told
// "0 error" while a background agent has failed.
func statusLineErrorCount(snap *observer.Snapshot) int {
	n := 0
	for i := range snap.Sessions {
		s := snap.Sessions[i]
		if s.Live != nil && !*s.Live && s.Status != observer.StatusFailed {
			continue
		}
		if s.Status == observer.StatusFailed ||
			(s.Transcript != nil && s.Transcript.APIErrors > 0) {
			n++
		}
	}
	return n
}
