package main

// P9's tests for §5.5(c)'s status-line writer.
//
// THE CENTRAL ONE is the zero-write assertion: with --status-line=auto and no
// `#{E:@tt_status}` in the user's status line, the writer must issue no tmux
// write at all. That is §2.3's guarantee for everybody who has not opted in,
// and it is the case §9.6 tests against a real server by counting set-option
// calls with a wrapper on PATH. Here it is counted through the same seams the
// production code goes through, so the assertion holds without a tmux server.
//
// NO TEST IN THIS FILE REACHES TMUX. Both seams are stubbed, and a stub that
// is never called is what most of these assertions are about.

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// tmuxRecorder records every tmux invocation both seams would have made, so a
// test can assert on the calls AND on their absence.
type tmuxRecorder struct {
	mu    sync.Mutex
	calls [][]string
	// values answers `show -gv <option>`, keyed by option name.
	values map[string]string
	err    error
}

func newTmuxRecorder(values map[string]string) *tmuxRecorder {
	if values == nil {
		values = map[string]string{}
	}
	return &tmuxRecorder{values: values}
}

// install replaces both tmux seams for the duration of the test.
func (r *tmuxRecorder) install(t *testing.T) {
	t.Helper()
	stubSeam(t, &tmuxCapture, func(args ...string) (string, error) {
		r.record(args)
		if r.err != nil {
			return "", r.err
		}
		if len(args) == 3 && args[0] == "show" {
			return r.values[args[2]], nil
		}
		// display-message -p "<tab-joined #{option} refs>": answer it the way
		// tmux would, so the paint read is exercised rather than stubbed away.
		// An expanded ref (#{E:@x}) gets the answer a test gives "E:@x" when it
		// gives one, so a test can tell a raw read from an expanded one. Without
		// it the E: is stripped and the option's own value answers, which is
		// what tmux does for an option holding a plain value.
		if len(args) == 3 && args[0] == "display-message" && args[1] == "-p" {
			fields := strings.Split(args[2], "\t")
			for i, f := range fields {
				key := strings.TrimSuffix(strings.TrimPrefix(f, "#{"), "}")
				if v, ok := r.values[key]; ok {
					fields[i] = v
					continue
				}
				fields[i] = r.values[strings.TrimPrefix(key, "E:")]
			}
			return strings.Join(fields, "\t"), nil
		}
		return "", nil
	})
	stubSeam(t, &tmuxSet, func(args ...string) error {
		r.record(args)
		return r.err
	})
}

func (r *tmuxRecorder) record(args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
}

func (r *tmuxRecorder) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// writes is every call that CHANGES tmux state: a set-option, or the status
// refresh that follows one. Reads (`show -gv`) are deliberately not counted —
// §2.3's guarantee and §9.6's test case are both about writes.
func (r *tmuxRecorder) writes() [][]string {
	var out [][]string
	for _, c := range r.all() {
		if len(c) == 0 {
			continue
		}
		if c[0] == "set-option" || c[0] == "refresh-client" {
			out = append(out, c)
		}
	}
	return out
}

// countsSnapshot is a snapshot carrying just the aggregate the indicator
// renders, plus whatever sessions the error count needs.
func countsSnapshot(attention, unknown int, sessions ...observer.Session) *observer.Snapshot {
	counts := observer.NewCounts()
	counts.Attention = attention
	counts.Unknown = unknown
	counts.Total = attention + unknown + len(sessions)
	return &observer.Snapshot{
		ObservedAt: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Counts:     counts,
		Sources:    map[string]observer.SourceReport{},
		Sessions:   sessions,
	}
}

// ---------------------------------------------------------------------------
// the §2.3 guarantee
// ---------------------------------------------------------------------------

// With --status-line=auto and no placeholder in either half of the status
// line, the writer issues ZERO tmux writes — not on construction, not on
// publish, not on clear, and it does not even subscribe.
func TestWithNoPlaceholderTheStatusLineWriterIssuesZeroTmuxWrites(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{
		"status-right": "#(continuum_save.sh)#{E:@catppuccin_status_date_time}",
		"status-left":  "#{E:@catppuccin_status_session}",
	})
	rec.install(t)

	w := newStatusLineWriter(statusLineAuto)
	if w.Enabled() {
		t.Fatal("the writer enabled itself with no @tt_status in the status line")
	}

	if wrote := w.Publish(countsSnapshot(3, 1)); wrote {
		t.Fatal("a disabled writer wrote the option")
	}
	w.Clear()

	src := newFakeStatusSource(countsSnapshot(3, 1))
	if err := w.Run(context.Background(), src); err != nil {
		t.Fatalf("Run on a disabled writer returned %v", err)
	}
	if got := src.subscriptions(); got != 0 {
		t.Fatalf("a disabled writer took %d subscriptions, want 0", got)
	}

	if calls := rec.writes(); len(calls) != 0 {
		t.Fatalf("a non-opted-in user's tmux received %d writes: %v", len(calls), calls)
	}
}

// --status-line=never issues nothing at all, reads included: it is the
// one-flag answer to a reviewer who rejects §11 assumption 10's widening.
func TestStatusLineNeverIssuesNoTmuxCallsAtAllNotEvenTheDetection(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{"status-right": "#{E:@tt_status}"})
	rec.install(t)

	w := newStatusLineWriter(statusLineNever)
	if w.Enabled() {
		t.Fatal("--status-line=never enabled the writer")
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("--status-line=never issued %d tmux calls: %v", len(calls), calls)
	}
}

// ---------------------------------------------------------------------------
// detection and writing
// ---------------------------------------------------------------------------

func TestThePlaceholderIsDetectedInEitherHalfOfTheStatusLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   bool
	}{
		{"right", map[string]string{"status-right": "#{E:@tt_status} #(x.sh)"}, true},
		{"left", map[string]string{"status-left": "#{E:@tt_status}"}, true},
		{"status-format", map[string]string{"status-format": "#[align=left]#{E:@tt_status}\n#[align=right]x"}, true},
		{"neither", map[string]string{"status-right": "#(x.sh)"}, false},
		{"unset", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newTmuxRecorder(tc.values).install(t)
			if got := newStatusLineWriter(statusLineAuto).Enabled(); got != tc.want {
				t.Fatalf("detection = %v, want %v for %v", got, tc.want, tc.values)
			}
		})
	}
}

// --status-line=always writes without detection, for a placeholder this
// detection cannot see (a nested format, or a status-format array entry).
func TestStatusLineAlwaysWritesWithoutDetection(t *testing.T) {
	rec := newTmuxRecorder(nil)
	rec.install(t)

	w := newStatusLineWriter(statusLineAlways)
	if !w.Enabled() {
		t.Fatal("--status-line=always did not enable the writer")
	}
	if !w.Publish(countsSnapshot(3, 0)) {
		t.Fatal("the writer did not write")
	}
	// ONE exec per change. The refresh-client -S that used to follow it was
	// measured redundant on tmux 3.7c (a global option set repaints every
	// client), so a second call here is a regression, not a belt.
	writes := rec.writes()
	if len(writes) != 1 {
		t.Fatalf("one change produced %d writes, want exactly one set-option: %v", len(writes), writes)
	}
	if got := writes[0]; got[0] != "set-option" || got[1] != "-gq" || got[2] != optStatus ||
		!strings.Contains(got[3], "bold] \uf0f3 3 #[default]") {
		t.Fatalf("the option write is %v, want set-option -gq @tt_status <the waiting badge>", got)
	}
	for _, c := range rec.all() {
		if c[0] == "refresh-client" {
			t.Fatalf("the writer still issues %v", c)
		}
	}
}

// An unchanged aggregate costs nothing. This is what makes the cost
// proportional to real state changes rather than to events: ~0.67 execs a
// minute at the measured event rate, and zero while nothing changes.
func TestAnUnchangedAggregateCostsNoTmuxCalls(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{"status-right": "#{E:@tt_status}"})
	rec.install(t)

	w := newStatusLineWriter(statusLineAuto)
	if !w.Publish(countsSnapshot(3, 1)) {
		t.Fatal("the first publish did not write")
	}
	before := len(rec.writes())

	for i := 0; i < 5; i++ {
		if w.Publish(countsSnapshot(3, 1)) {
			t.Fatal("an unchanged aggregate was written again")
		}
	}
	if after := len(rec.writes()); after != before {
		t.Fatalf("five unchanged publishes added %d writes", after-before)
	}

	if !w.Publish(countsSnapshot(4, 1)) {
		t.Fatal("a changed aggregate was not written")
	}
	if got, want := w.Writes(), 2; got != want {
		t.Fatalf("the writer reports %d writes, want %d", got, want)
	}
}

// Guard 2: the option is unset on a clean shutdown, so an absent server
// leaves no residue in the status line.
func TestTheOptionIsUnsetOnACleanShutdown(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{"status-right": "#{E:@tt_status}"})
	rec.install(t)

	w := newStatusLineWriter(statusLineAuto)
	w.Publish(countsSnapshot(2, 0))
	w.Clear()

	var unset bool
	for _, c := range rec.writes() {
		if len(c) >= 3 && c[0] == "set-option" && c[1] == "-gu" && c[2] == optStatus {
			unset = true
		}
	}
	if !unset {
		t.Fatalf("Clear did not unset the option: %v", rec.writes())
	}

	// And clearing twice is not two writes: the second has nothing to clear.
	before := len(rec.writes())
	w.Clear()
	if after := len(rec.writes()); after != before {
		t.Fatal("clearing an already-cleared option wrote again")
	}
}

// Run is driven by the event stream and clears on the way out. There is no
// timer in it: every wake-up is a real coalesced state change.
func TestRunWritesOnEveryEventAndClearsWhenTheContextEnds(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{"status-right": "#{E:@tt_status}"})
	rec.install(t)

	src := newFakeStatusSource(countsSnapshot(1, 0))
	w := newStatusLineWriter(statusLineAuto)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, src) }()

	src.emit(observer.Event{Type: observer.EventSnapshot})
	waitFor(t, func() bool { return w.Writes() == 1 })

	src.setLatest(countsSnapshot(2, 0))
	src.emit(observer.Event{Type: observer.EventSessionChanged, SessionID: "a"})
	waitFor(t, func() bool { return w.Writes() == 2 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when the context ended")
	}

	last := rec.writes()[len(rec.writes())-1]
	if last[0] != "set-option" || last[1] != "-gu" {
		t.Fatalf("the last write is %v, want the option to be unset on shutdown", last)
	}
}

// ---------------------------------------------------------------------------
// what it says
// ---------------------------------------------------------------------------

func TestTheIndicatorSaysTheCountsAndDisappearsWhenThereIsNothingToSay(t *testing.T) {
	failed := observer.Session{SessionID: "f", Status: observer.StatusFailed}
	apiErr := observer.Session{
		SessionID: "e", Status: observer.StatusIdle,
		Transcript: &observer.TranscriptFacts{APIErrors: 2},
	}

	for _, tc := range []struct {
		name string
		snap *observer.Snapshot
		want string
	}{
		{"waiting and error", countsSnapshot(3, 0, failed), "3 waiting · 1 error"},
		{"waiting and unclear", countsSnapshot(2, 1), "2 waiting · 1 unclear"},
		{"api errors count too", countsSnapshot(0, 0, apiErr), "1 error"},
		{"all three", countsSnapshot(3, 2, failed), "3 waiting · 1 error · 2 unclear"},
		{"nothing to say", countsSnapshot(0, 0), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusLine(tc.snap, statusLinePaint{}); got != tc.want {
				t.Fatalf("renderStatusLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// A dead session is not counted as an error unless it FAILED: "dead" is a
// liveness fact and the indicator is about what needs a human.
func TestADeadButNotFailedSessionIsNotAnError(t *testing.T) {
	dead := false
	snap := countsSnapshot(0, 0, observer.Session{
		SessionID: "d", Status: observer.StatusIdle, Live: &dead,
		Transcript: &observer.TranscriptFacts{APIErrors: 5},
	})
	if got := renderStatusLine(snap, statusLinePaint{}); got != "" {
		t.Fatalf("renderStatusLine = %q, want empty for a dead session", got)
	}
}

func TestTheStatusLineModeSpellingsAreExactlyThreeAndATypoIsRefused(t *testing.T) {
	for in, want := range map[string]statusLineMode{
		"auto": statusLineAuto, "always": statusLineAlways, "never": statusLineNever,
		"AUTO": statusLineAuto, " always ": statusLineAlways,
	} {
		got, ok := parseStatusLineMode(in)
		if !ok || got != want {
			t.Errorf("parseStatusLineMode(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	for _, bad := range []string{"on", "off", "yes", "", "aut"} {
		if _, ok := parseStatusLineMode(bad); ok {
			t.Errorf("parseStatusLineMode(%q) was accepted", bad)
		}
	}
}

// --pane is a renderer, so pairing it with a server-only flag is a usage
// error rather than a silently-resolved precedence (§5.5b).
func TestPaneWithAServerOnlyFlagIsAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"--pane", "--stop"},
		{"--pane", "--status-line=always"},
	} {
		var stdout, stderr strings.Builder
		_, code, ok := parseObserveFlags(args, &stdout, &stderr)
		if ok {
			t.Fatalf("%v was accepted", args)
		}
		if code != 2 {
			t.Fatalf("%v exited %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("%v wrote %q to stdout", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "renderer") {
			t.Fatalf("%v: stderr does not explain the refusal:\n%s", args, stderr.String())
		}
	}
}

func TestAnUnknownStatusLineModeIsAUsageError(t *testing.T) {
	var stdout, stderr strings.Builder
	_, code, ok := parseObserveFlags([]string{"--status-line=on"}, &stdout, &stderr)
	if ok || code != 2 {
		t.Fatalf("--status-line=on gave ok=%v code=%d, want a usage error", ok, code)
	}
	if !strings.Contains(stderr.String(), "auto, always or never") {
		t.Fatalf("stderr does not name the three spellings:\n%s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------------

// fakeStatusSource is a statusLineSource: an event channel plus the published
// snapshot those events describe.
type fakeStatusSource struct {
	mu     sync.Mutex
	ch     chan observer.Event
	latest *observer.Snapshot
	subs   int
}

func newFakeStatusSource(latest *observer.Snapshot) *fakeStatusSource {
	return &fakeStatusSource{ch: make(chan observer.Event, 8), latest: latest}
}

func (f *fakeStatusSource) Subscribe(_ context.Context, _ observer.Filter) (<-chan observer.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs++
	return f.ch, nil
}

func (f *fakeStatusSource) Latest() *observer.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest
}

func (f *fakeStatusSource) setLatest(snap *observer.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest = snap
}

func (f *fakeStatusSource) emit(ev observer.Event) { f.ch <- ev }

func (f *fakeStatusSource) subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs
}

// waitFor polls a condition the writer's own goroutine satisfies.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition was not met within 2s")
		}
		time.Sleep(time.Millisecond) //nolint:forbidigo // test-only poll
	}
}

// ---------------------------------------------------------------------------
// the badges
// ---------------------------------------------------------------------------

// A badge is a glyph and a number on a colour, and nothing else. The user's
// verdict on the version that used their own palette was that it read
// backwards — "5 looks like red and erro looks liek ntohg hapeing" — so the
// weights are tt's now: an amber for waiting, a real red for errors, a grey
// that recedes for unclear.
func TestTheBadgesAreAGlyphAndANumberOnASemanticColour(t *testing.T) {
	snap := countsSnapshot(3, 2, observer.Session{SessionID: "f", Status: observer.StatusFailed})

	light := renderStatusLine(snap, newStatusLinePaint("light", nil))
	want := statusLineLeadIcon + " " +
		"#[fg=#11111b,bg=#d79921,bold] \uf0f3 3 #[default]" +
		"#[fg=#ffffff,bg=#cc241d,bold] \uf071 1 #[default]" +
		"#[fg=#11111b,bg=#bdae93,bold] \uf059 2 #[default]"
	if light != want {
		t.Fatalf("light render =\n %q\nwant\n %q", light, want)
	}
	for _, unwanted := range []string{"waiting", "error", "unclear", "·"} {
		if strings.Contains(light, unwanted) {
			t.Errorf("a painted badge still spells out %q:\n%s", unwanted, light)
		}
	}

	dark := renderStatusLine(snap, newStatusLinePaint("dark", nil))
	for _, fill := range []string{"bg=#fab387", "bg=#ff5f5f", "bg=#585b70"} {
		if !strings.Contains(dark, fill) {
			t.Errorf("the dark row is missing %s:\n%s", fill, dark)
		}
	}
	if dark == light {
		t.Fatal("the two modes render identically")
	}
}

// With no mode pinned, the colours are a tmux conditional the bar resolves when
// it is painted, so a light/dark switch repaints it immediately instead of at
// the next state change. Two layers (architecture §5.7): the per-client
// predicate @theme_dark when the theme script publishes it, so each screen's
// bar picks its own fill, and otherwise @theme_mode, exactly as before.
func TestWithNoModePinnedTheColoursFollowTheThemeOptionLive(t *testing.T) {
	got := renderStatusLine(countsSnapshot(1, 0), newStatusLinePaint("", nil))
	want := statusLineLeadIcon + " " +
		"#[fg=#{?@theme_dark,#{?#{E:@theme_dark},#11111b,#11111b},#{?#{==:#{@theme_mode},light},#11111b,#11111b}}," +
		"bg=#{?@theme_dark,#{?#{E:@theme_dark},#fab387,#d79921},#{?#{==:#{@theme_mode},light},#d79921,#fab387}}," +
		"bold] \uf0f3 1 #[default]"
	if got != want {
		t.Fatalf("conditional render =\n %q\nwant\n %q", got, want)
	}
}

// perClientThemePredicate is the @theme_dark value theme.sh publishes
// (architecture §5.1): 1 or 0 for the client whose bar is being drawn, from its
// entry in @theme_screens, else from @theme_default.
const perClientThemePredicate = "#{?#{m:* #{client_tty}=dark *,#{@theme_screens}},1," +
	"#{?#{m:* #{client_tty}=light *,#{@theme_screens}},0,#{==:#{@theme_default},dark}}}"

// lastStatusWrite is the value of the last `set-option -gq @tt_status` a
// recorder saw.
func lastStatusWrite(t *testing.T, rec *tmuxRecorder) string {
	t.Helper()
	var value string
	for _, c := range rec.writes() {
		if len(c) == 4 && c[0] == "set-option" && c[1] == "-gq" && c[2] == optStatus {
			value = c[3]
		}
	}
	if value == "" {
		t.Fatalf("no @tt_status write among %v", rec.writes())
	}
	return value
}

// A server running the per-client theme publishes @theme_dark, a predicate
// each client's bar evaluates for itself. The badges must stay a CONDITIONAL
// there: pinned to one mode, every screen would get the fill of whichever
// theme the bar's background measured at the last publish. The bar here
// measures cream and @theme_mode is not published, so a writer that did not
// look at @theme_dark would pin "light".
func TestAPublishedPerClientPredicateKeepsTheBadgesConditional(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{
		"status-right": "#{E:@tt_status}",
		"@theme_dark":  perClientThemePredicate,
		"@thm_mantle":  "#f2e5bc",
	})
	rec.install(t)
	w := newStatusLineWriter(statusLineAuto)
	if !w.Publish(countsSnapshot(1, 0)) {
		t.Fatal("the publish wrote nothing")
	}

	got := lastStatusWrite(t, rec)
	want := "bg=#{?@theme_dark,#{?#{E:@theme_dark},#fab387,#d79921},#{?#{==:#{@theme_mode},light},#d79921,#fab387}},bold]"
	if !strings.Contains(got, want) {
		t.Fatalf("@tt_status is\n %q\nwant the conditional fill\n %q", got, want)
	}
	if strings.Contains(got, "bg=#d79921,") || strings.Contains(got, "bg=#fab387,") {
		t.Fatalf("the fill was pinned to one mode:\n %q", got)
	}
}

// A server without the per-client theme (an older config, or another machine's own
// tmux) publishes only @theme_mode. The combined conditional resolves there
// through its else-branch, and that branch is byte for byte the conditional tt
// emitted before, so what reads it (TestBadgesSurviveATmuxThatWillNotAnswerTheRead,
// gate case 28's Contains) keeps holding. All three badges, fill and ink.
func TestTheConditionalFallsBackToTheModeMarkerOnOlderServers(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{
		"status-right": "#{E:@tt_status}",
		"@theme_mode":  "light",
	})
	rec.install(t)
	w := newStatusLineWriter(statusLineAuto)
	failed := observer.Session{SessionID: "f", Status: observer.StatusFailed}
	if !w.Publish(countsSnapshot(1, 1, failed)) {
		t.Fatal("the publish wrote nothing")
	}

	got := lastStatusWrite(t, rec)
	for _, badge := range statusLineBadges {
		for part, pair := range map[string][2]string{
			"bg": {badge.light, badge.dark},
			"fg": {statusLineInkFor(badge.light), statusLineInkFor(badge.dark)},
		} {
			light, dark := pair[0], pair[1]
			old := "#{?#{==:#{@theme_mode},light}," + light + "," + dark + "}"
			want := part + "=#{?@theme_dark,#{?#{E:@theme_dark}," + dark + "," + light + "}," + old + "}"
			if !strings.Contains(got, want) {
				t.Errorf("%s %s: @tt_status lacks\n %q\nin\n %q", badge.name, part, want, got)
			}
		}
	}
}

// Every consumer of @thm_* reads it EXPANDED now (architecture D7): under the
// per-client theme the entry holds a conditional, and a raw `#{@thm_mantle}`
// hands back that format text, which measures as no colour and falls to dark.
// The raw and expanded answers differ here, so the test sees which was asked.
func TestTheBackgroundFallbackReadsThePaletteExpanded(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{
		"status-right":  "#{E:@tt_status}",
		"@thm_mantle":   "#{?#{E:@theme_dark},#181825,#f2e5bc}",
		"E:@thm_mantle": "#f2e5bc",
	})
	rec.install(t)

	paint := newStatusLineWriter(statusLineAuto).readPaint()
	if got := paint.badges["waiting"].fill; got != "#d79921" {
		t.Fatalf("the waiting fill is %q, want the light #d79921 measured off the expanded palette", got)
	}
}

// The glyph is overridable, for a font without it.
func TestTheBadgeGlyphCanBeOverriddenPerBadge(t *testing.T) {
	paint := newStatusLinePaint("dark", map[string]string{
		"@tt_icon_waiting": "!", "@tt_icon_error": " X ", "@tt_icon_unclear": "",
	})
	got := renderStatusLine(countsSnapshot(1, 1, observer.Session{
		SessionID: "f", Status: observer.StatusFailed,
	}), paint)
	if !strings.Contains(got, "bold] ! 1 ") {
		t.Errorf("the waiting override was not used:\n%s", got)
	}
	if !strings.Contains(got, "bold] X 1 ") {
		t.Errorf("the error override was not trimmed and used:\n%s", got)
	}
	if !strings.Contains(got, "\uf059 1 ") {
		t.Errorf("an empty override did not fall back to the default glyph:\n%s", got)
	}
	// A value that trims to nothing REMOVES the glyph — an option tmux reports
	// as empty is one nobody set, so those two cannot mean the same thing.
	bare := renderStatusLine(countsSnapshot(1, 0), newStatusLinePaint("dark",
		map[string]string{"@tt_icon_waiting": " "}))
	if !strings.Contains(bare, "bold] 1 #[default]") {
		t.Errorf("a blank override did not remove the glyph:\n%s", bare)
	}
}

// Whatever the fill, the ink is the better of tt's two by WCAG contrast, and
// for the six colours it actually ships it clears AA for body text.
func TestEveryShippedBadgeInkClearsTheContrastFloor(t *testing.T) {
	const floor = 4.5
	for _, badge := range statusLineBadges {
		for mode, fill := range map[string]string{"light": badge.light, "dark": badge.dark} {
			bg, ok := parseHexColour(fill)
			if !ok {
				t.Fatalf("%s/%s: the shipped fill %q does not parse", badge.name, mode, fill)
			}
			ink := statusLineInkFor(fill)
			fg, _ := parseHexColour(ink)
			if got := contrastRatio(fg, bg); got < floor {
				t.Errorf("%s/%s: %s on %s is %.2f:1, want >= %.1f", badge.name, mode, ink, fill, got, floor)
			}
			other := statusLineDarkInk
			if ink == statusLineDarkInk {
				other = statusLineLightInk
			}
			alt, _ := parseHexColour(other)
			if contrastRatio(fg, bg) < contrastRatio(alt, bg) {
				t.Errorf("%s/%s took %s over the better %s", badge.name, mode, ink, other)
			}
		}
	}
}

// The user's complaint, as properties. "the colour are islieading, 5 looks like
// red and erro looks liek ntohg hapeing" came from two specific failures: the
// waiting fill was a burnt orange dark enough to read as alarm, and the error
// fill was a pale wash. Luminance contrast alone does not catch that — on a
// dark bar a readable red is always dimmer than a readable amber — so the
// properties are about the COLOURS: an error that is a saturated red, a waiting
// hue visibly further toward yellow, and an unclear that is grey and recedes.
func TestTheBadgeColoursCannotReadBackwards(t *testing.T) {
	for _, tc := range []struct{ mode, bar string }{
		{"light", "#f2e5bc"},
		{"dark", "#181825"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			fill := func(name string) string {
				for _, b := range statusLineBadges {
					if b.name == name {
						if tc.mode == "dark" {
							return b.dark
						}
						return b.light
					}
				}
				t.Fatalf("no badge named %q", name)
				return ""
			}
			errH, errS, errL := hsl(t, fill("error"))
			waitH, waitS, _ := hsl(t, fill("waiting"))
			_, unclearS, _ := hsl(t, fill("unclear"))

			// A true red, not a wash: this is what @thm_red's #f2c2c2
			// (S 0.65, L 0.86) failed, and why the error badge looked like
			// nothing was happening.
			if errS < 0.5 || errL > 0.72 {
				t.Errorf("the error fill %s is S %.2f L %.2f — a wash, not a red", fill("error"), errS, errL)
			}
			if errH > 20 && errH < 340 {
				t.Errorf("the error fill %s is hue %.0f°, which is not red", fill("error"), errH)
			}
			// Visibly further toward yellow than the red, so the two badges
			// cannot be confused at a glance — the theme file's own rule:
			// "move orange toward yellow rather than shifting red".
			if delta := waitH - errH; delta < 15 {
				t.Errorf("the waiting hue %.0f° is only %.0f° from the error hue %.0f°",
					waitH, delta, errH)
			}
			if waitS < 0.4 {
				t.Errorf("the waiting fill %s is S %.2f — too washed out to notice", fill("waiting"), waitS)
			}
			// Grey, and the quietest of the three against the bar.
			if unclearS > 0.25 {
				t.Errorf("the unclear fill %s is S %.2f — a colour, not a dim", fill("unclear"), unclearS)
			}
			bar, ok := parseHexColour(tc.bar)
			if !ok {
				t.Fatal("the fixture bar does not parse")
			}
			weight := func(name string) float64 {
				c, _ := parseHexColour(fill(name))
				return contrastRatio(c, bar)
			}
			if weight("unclear") >= weight("waiting") || weight("unclear") >= weight("error") {
				t.Errorf("the unclear badge (%.2f:1 against the bar) does not recede behind waiting (%.2f:1) and error (%.2f:1)",
					weight("unclear"), weight("waiting"), weight("error"))
			}
		})
	}
}

// hsl is the test's own colour maths, so the properties above are checked
// against something other than the code that produced the colours.
func hsl(t *testing.T, hex string) (h, s, l float64) {
	t.Helper()
	c, ok := parseHexColour(hex)
	if !ok {
		t.Fatalf("%q does not parse", hex)
	}
	r, g, b := c[0], c[1], c[2]
	max, min := r, r
	for _, v := range []float64{g, b} {
		if v > max {
			max = v
		}
		if v < min {
			min = v
		}
	}
	l = (max + min) / 2
	d := max - min
	if d == 0 {
		return 0, 0, l
	}
	den := 1 - math.Abs(2*l-1)
	if den > 0 {
		s = d / den
	}
	switch max {
	case r:
		h = 60 * math.Mod((g-b)/d+6, 6)
	case g:
		h = 60 * ((b-r)/d + 2)
	default:
		h = 60 * ((r-g)/d + 4)
	}
	return h, s, l
}

// The read that resolves the badges happens once per real change and never
// while nothing changes: the comparison that decides whether to write is made
// on the plain counts, which cost no tmux call.
func TestTheBadgePaintIsReadOnlyWhenTheCountsChange(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{
		"status-right": "#{E:@tt_status}", "@theme_mode": "light",
	})
	rec.install(t)
	w := newStatusLineWriter(statusLineAuto)
	reads := func() int {
		n := 0
		for _, c := range rec.all() {
			if len(c) > 1 && c[0] == "display-message" {
				n++
			}
		}
		return n
	}
	before := reads()

	if !w.Publish(countsSnapshot(2, 0)) {
		t.Fatal("the first publish wrote nothing")
	}
	if got := reads() - before; got != 1 {
		t.Fatalf("%d paint reads for the first publish, want 1", got)
	}
	for i := 0; i < 10; i++ {
		if w.Publish(countsSnapshot(2, 0)) {
			t.Fatal("an unchanged aggregate was written")
		}
	}
	if got := reads() - before; got != 1 {
		t.Fatalf("%d paint reads while nothing changed, want 1", got)
	}
	if !w.Publish(countsSnapshot(3, 0)) {
		t.Fatal("a changed aggregate was not written")
	}
	if got := reads() - before; got != 2 {
		t.Fatalf("%d paint reads after a change, want 2", got)
	}
}

// With no @theme_mode the mode is read off the bar's own background, so a
// config that never publishes one still gets colours tuned for its terminal.
func TestWithoutAModeMarkerTheBarsBackgroundDecides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{"a cream bar is light", []string{"#f2e5bc", ""}, "light"},
		{"a near-black bar is dark", []string{"#181825", ""}, "dark"},
		{"the second value is used when the first is unset", []string{"", "#fbf1c7"}, "light"},
		{"nothing readable is dark", []string{"", "colour234"}, "dark"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusLineModeFromBackground(tc.values...); got != tc.want {
				t.Fatalf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

// The row is led by a robot, so the counts are legibly Claude's and not one
// more thing on a crowded bar. It is the bar's own foreground, not a fill: a
// label is not a state.
func TestTheRowIsLedByTheClaudeGlyph(t *testing.T) {
	got := renderStatusLine(countsSnapshot(1, 0), newStatusLinePaint("light", nil))
	if !strings.HasPrefix(got, statusLineLeadIcon+" #[") {
		t.Fatalf("the row does not start with the lead glyph and a space:\n%q", got)
	}
	if strings.Count(got, statusLineLeadIcon) != 1 {
		t.Fatalf("the lead glyph is drawn more than once:\n%q", got)
	}

	// Overridable, and removable, like every other glyph here.
	custom := renderStatusLine(countsSnapshot(1, 0),
		newStatusLinePaint("light", map[string]string{optIconLead: "CC"}))
	if !strings.HasPrefix(custom, "CC #[") {
		t.Fatalf("the lead override was not used:\n%q", custom)
	}
	none := renderStatusLine(countsSnapshot(1, 0),
		newStatusLinePaint("light", map[string]string{optIconLead: " "}))
	if !strings.HasPrefix(none, "#[") {
		t.Fatalf("a blank override did not remove the lead glyph:\n%q", none)
	}

	// And nothing to say is still nothing at all: no lone robot on the bar.
	if empty := renderStatusLine(countsSnapshot(0, 0), newStatusLinePaint("light", nil)); empty != "" {
		t.Fatalf("an empty row is %q, want nothing", empty)
	}
}

// A tmux that will not answer the paint read still gets badges, because their
// colours are a conditional it evaluates for itself.
func TestBadgesSurviveATmuxThatWillNotAnswerTheRead(t *testing.T) {
	rec := newTmuxRecorder(map[string]string{"status-right": "#{E:@tt_status}"})
	rec.install(t)
	w := newStatusLineWriter(statusLineAuto)
	rec.err = errors.New("no server")
	defer func() { rec.err = nil }()

	paint := w.readPaint()
	if len(paint.badges) != len(statusLineBadges) {
		t.Fatalf("an unreadable tmux left %d badges, want %d", len(paint.badges), len(statusLineBadges))
	}
	got := renderStatusLine(countsSnapshot(1, 0), paint)
	if !strings.Contains(got, "#{?#{==:#{@theme_mode},light},") {
		t.Fatalf("the fallback row carries no conditional colours:\n%q", got)
	}
}
