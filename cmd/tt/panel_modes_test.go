package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// fleet is a fixture shaped like the live machine the research measured: a
// few repositories each holding several sessions, most of them in linked
// worktrees; a dozen tmux sessions that do not line up with the repositories;
// a minority of sessions with a /color; background sessions with no pane,
// which are most of what waits on the user.
type fleet struct {
	root string
	snap *observer.Snapshot
	now  time.Time
}

// repo lays out a repository at root/name with n linked worktrees under
// .claude/worktrees, the way `git worktree add` does, and returns the
// checkout directories: the main one first.
func repo(t testing.TB, root, name string, worktrees int) []string {
	t.Helper()
	main := filepath.Join(root, name)
	common := filepath.Join(main, ".git")
	must(t, os.MkdirAll(common, 0o755))
	dirs := []string{main}
	for i := 0; i < worktrees; i++ {
		wt := fmt.Sprintf("wt%d", i)
		dir := filepath.Join(main, ".claude", "worktrees", wt)
		private := filepath.Join(common, "worktrees", wt)
		must(t, os.MkdirAll(private, 0o755))
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(private, "commondir"), []byte("../..\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(private, "gitdir"), []byte(filepath.Join(dir, ".git")+"\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+private+"\n"), 0o644))
		dirs = append(dirs, dir)
	}
	return dirs
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFleet(t testing.TB, now time.Time) fleet {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	dirs := map[string][]string{
		"claudish": repo(t, root, "claudish", 5),
		"dotfiles": repo(t, root, "dotfiles", 2),
		"madbench": repo(t, root, "madbench", 2),
		"magus":    repo(t, root, "magus", 4),
		"authflow": repo(t, root, "authflow", 4),
		"mnemex":   repo(t, root, "mnemex", 1),
		"nimbus":   repo(t, root, "nimbus", 1),
		"dingo":    repo(t, root, "dingo", 0),
		"twin":     repo(t, root, "twin", 0),
	}

	live := true
	var ss []observer.Session
	n := 0
	add := func(proj, tmux, colour string, st observer.Status, ageMin int, tune func(*observer.Session)) {
		d := dirs[proj]
		since := now.Add(-time.Duration(ageMin) * time.Minute)
		s := observer.Session{
			SessionID: fmt.Sprintf("s%02d", n), Name: fmt.Sprintf("%s-%02d", proj, n),
			Kind: observer.KindInteractive, Status: st, Attention: st.IsAttention(),
			Live: &live, StatusSince: &since, ObservedAt: now, CWD: d[n%len(d)],
		}
		if tmux != "" {
			s.Tmux = &observer.TmuxRef{Session: tmux, Pane: fmt.Sprintf("%%%d", 100+n)}
		}
		if colour != "" {
			s.Transcript = &observer.TranscriptFacts{AgentColor: colour, ColorFound: true}
		}
		if st == observer.StatusWaiting {
			s.WaitReason = observer.WaitReasonInputNeeded
		}
		if tune != nil {
			tune(&s)
		}
		ss = append(ss, s)
		n++
	}
	background := func(s *observer.Session) {
		s.Kind = observer.KindBackground
		s.Tmux = nil
		s.Needs = ptr("say the word and I will")
	}

	// Waiting on the user: two interactive, three background.
	add("magus", "magus", "blue", observer.StatusWaiting, 13, nil)
	add("madbench", "madbench", "", observer.StatusWaiting, 1, nil)
	add("dotfiles", "", "", observer.StatusBlocked, 1590, background)
	add("twin", "", "", observer.StatusBlocked, 11, background)
	add("mnemex", "", "cyan", observer.StatusBlocked, 60, background)
	// Busy.
	for i, p := range []string{"claudish", "claudish", "madbench", "madbench", "dotfiles", "magus", "authflow"} {
		tmux := map[string]string{"claudish": "claudish", "madbench": "madbench", "dotfiles": "magai", "magus": "magus", "authflow": "authflow"}[p]
		colour := []string{"", "green", "", "cyan", "", "", "red"}[i]
		add(p, tmux, colour, observer.StatusBusy, 1+i, nil)
	}
	// Idle, spread over every project and a dozen tmux sessions.
	idle := []struct{ proj, tmux, colour string }{
		{"claudish", "claudish", ""}, {"claudish", "claudish", "purple"}, {"claudish", "magus", ""},
		{"claudish", "claudish", ""}, {"dotfiles", "magai", ""}, {"dotfiles", "dotfiles", ""},
		{"magus", "magus", ""}, {"magus", "magus", "blue"}, {"magus", "mnemex", ""},
		{"authflow", "authflow", ""}, {"authflow", "authflow", ""}, {"authflow", "authflow", "yellow"},
		{"authflow", "authflow", ""}, {"authflow", "passgate", ""}, {"mnemex", "mnemex", ""},
		{"mnemex", "mnemex", "green"}, {"nimbus", "cosmos", ""}, {"nimbus", "cosmos", ""},
		{"dingo", "passgate", ""}, {"twin", "magai", "orange"}, {"madbench", "madbench", ""},
		{"madbench", "orbit-demo", ""}, {"dotfiles", "", ""}, {"nimbus", "", ""}, {"magus", "magus", "pink"},
		{"claudish", "claudish", ""}, {"authflow", "authflow", "red"}, {"mnemex", "mnemex", ""},
		{"magus", "magus", ""},
	}
	for i, x := range idle {
		add(x.proj, x.tmux, x.colour, observer.StatusIdle, 30+i*97, nil)
	}
	// Unclear and ended.
	add("dotfiles", "", "", observer.StatusBlocked, 60000, func(s *observer.Session) {
		background(s)
		s.Live = nil
	})
	add("claudish", "claudish", "", observer.StatusBusy, 400, func(s *observer.Session) { s.Live = nil })
	add("authflow", "", "", observer.StatusDone, 90, background)
	add("magus", "", "", observer.StatusFailed, 95, background)
	add("madbench", "", "", observer.StatusDone, 99, background)

	return fleet{root: root, now: now, snap: &observer.Snapshot{
		Seq: 1, ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{}, Sessions: ss,
	}}
}

// sidebar is a pane-surface panel showing snap in mode at width x height.
func sidebar(t testing.TB, now time.Time, snap *observer.Snapshot, mode panelMode, height int) *panel {
	t.Helper()
	p, _, _ := openTestPanel(t, now)
	p.surface, p.mode, p.height = surfacePane, mode, height
	deliver(t, p, snapshotEvent(snap))
	return p
}

func plain(view string) []string {
	var out []string
	for _, l := range strings.Split(view, "\n") {
		out = append(out, strings.TrimRight(ansi.Strip(l), " "))
	}
	return out
}

func indexOfLine(lines []string, re string) int {
	rx := regexp.MustCompile(re)
	for i, l := range lines {
		if rx.MatchString(l) {
			return i
		}
	}
	return -1
}

func TestTheModeKeyCyclesProjectStatusTmuxColour(t *testing.T) {
	m := defaultPanelMode
	var seen []string
	for i := 0; i < 5; i++ {
		seen = append(seen, m.String())
		m = m.next()
	}
	if got := strings.Join(seen, " "); got != "project status tmux colour project" {
		t.Errorf("cycle = %s", got)
	}
	for _, s := range []string{"project", "status", "tmux", "colour", "color", " Tmux\n"} {
		if _, ok := parsePanelMode(s); !ok {
			t.Errorf("%q does not parse", s)
		}
	}
	if _, ok := parsePanelMode("model"); ok {
		t.Errorf("an unknown mode parsed")
	}
}

// The header (FR9): the grouping tabs, centred, the active one filled with
// the accent and the others dim; on the sidebar each tab shows its hotkey in
// square brackets, in the TUI (where the letters are search characters) it
// shows none. Under them the baseline, thin, with the thick stretch exactly
// under the active tab; then the fleet's count strip; then two blank lines.
func TestTheHeaderIsTheTabsTheBaselineAndTheCounts(t *testing.T) {
	withPanelTheme(t, true)
	f := newFleet(t, fleetNow)
	for _, surface := range []panelSurface{surfacePane, surfaceTUI} {
		for _, mode := range panelModes {
			p := sidebar(t, f.now, f.snap, mode, 0) // unbounded: no scroll status in the corner
			p.surface = surface
			view := p.view()
			lines, styled := plain(view), strings.Split(view, "\n")
			label := fmt.Sprintf("%s mode, surface %d", mode, surface)

			want := "   [P]roject │ S[t]atus │ T[m]ux │ [C]olour"
			if surface == surfaceTUI {
				want = "       Project │ Status │ Tmux │ Colour"
			}
			if lines[0] != want {
				t.Errorf("%s: tabs %q, want %q", label, lines[0], want)
			}
			var active panelTab
			for _, tab := range panelTabs {
				if tab.mode == mode {
					active = tab
				}
			}
			text := " " + tabText(active.label, active.hot, surface == surfacePane) + " "
			if !strings.Contains(styled[0], tabOnStyle.Render(text)) {
				t.Errorf("%s: the active tab %q is not filled with the accent: %q", label, text, styled[0])
			}
			if n := strings.Count(styled[0], sgrRGB(t, 48, panelPal.view.tabOnBg.Dark)); n != 1 {
				t.Errorf("%s: %d tabs are filled with the accent, want exactly the active one", label, n)
			}

			// The thick stretch sits exactly under the active tab.
			start := ansi.StringWidth(lines[0][:strings.Index(lines[0], strings.TrimRight(text, " "))]) // plain() trims the last tab's pad
			end := start + len(text)
			wantBase := strings.Repeat("─", start) + strings.Repeat("━", end-start) + strings.Repeat("─", panelWidth-end)
			if lines[1] != wantBase {
				t.Errorf("%s: baseline\n%q, want\n%q", label, lines[1], wantBase)
			}
			if lines[3] != "" || lines[4] != "" || lines[5] == "" {
				t.Errorf("%s: the header is not three lines and two blank ones: %q", label, lines[:6])
			}
		}
	}
}

// The count strip counts the whole fleet's live sessions by status — needs
// you, running, idle, unclear — draws only the counts that are not zero, and
// fills the gaps either side with the fade. Ended sessions are not counted.
func TestTheCountStripCountsTheFleetAndDrawsOnlyWhatIsNotZero(t *testing.T) {
	f := newFleet(t, fleetNow)
	var want panelCounts
	for _, s := range observer.AsOf(f.snap, f.now, observer.DefaultPolicy()).Sessions {
		if st, ok := stripStateOf(panelGroupOf(s)); ok {
			want[st]++
		}
	}
	if want[stripNeed] == 0 || want[stripRun] == 0 || want[stripIdle] == 0 {
		t.Fatalf("the fleet counts %v; the test needs every live state but unclear", want)
	}
	p := sidebar(t, f.now, f.snap, modeProject, 0)
	strip := plain(p.view())[2]
	var badges []string
	for st := stripState(0); st < stripStates; st++ {
		if want[st] > 0 {
			badges = append(badges, fmt.Sprintf(" %d  %s ", want[st], stripLabels[st]))
		}
	}
	body := strings.Join(badges, "")
	if !strings.Contains(strip, body) {
		t.Errorf("strip %q, want the badges %q", strip, body)
	}
	if !regexp.MustCompile(`^░+▒+▓+ `).MatchString(strip) || !regexp.MustCompile(` ▓+▒+░+$`).MatchString(strip) {
		t.Errorf("strip %q is not framed by the two fades", strip)
	}
	if w := ansi.StringWidth(strip); w != panelWidth {
		t.Errorf("the strip and its fades are %d wide, want the panel's %d", w, panelWidth)
	}

	// Only the non-zero ones: a fleet with nothing unclear draws no ???.
	var calm []observer.Session
	for _, s := range f.snap.Sessions {
		if panelGroupOf(s) != groupUnclear {
			calm = append(calm, s)
		}
	}
	c := *f.snap
	c.Sessions = calm
	p = sidebar(t, f.now, &c, modeProject, 0)
	if strip := plain(p.view())[2]; strings.Contains(strip, "???") || !strings.Contains(strip, "NEED") {
		t.Errorf("with nothing unclear the strip is %q", strip)
	}
	// The fades go first on a narrow panel, then the labels shorten. The
	// strip here is 26 columns; a fade wants two cells and a space a side.
	p.width = 31
	if strip := plain(p.view())[2]; strings.ContainsAny(strip, "░▒▓") || !strings.Contains(strip, "NEED") {
		t.Errorf("at 31 columns the strip is %q, want the full labels and no fades", strip)
	}
	p.width = 20
	if strip := plain(p.view())[2]; strings.Contains(strip, "NEED") || !strings.Contains(strip, " N ") || ansi.StringWidth(strip) > 20 {
		t.Errorf("at 20 columns the strip is %q, want one-letter labels", strip)
	}
}

// No mode folds a group, at any height (the user, 2026-09-29: "we should not
// fold groups like that"): every group shows every row under its title, and
// neither a count line ("+ 5 idle", "+ 5 groups: …", "+12 more: …") nor a
// title drawn alone stands in for rows. A frame that does not fit keeps its
// grouping (before revision 4 it fell back to status and said "(short)") and
// scrolls, and every session is on some scroll position.
func TestNoModeFoldsAGroupAndWhatDoesNotFitScrolls(t *testing.T) {
	f := newFleet(t, fleetNow)
	gone := regexp.MustCompile(`(?m)^\s*\+ ?\d+ `)
	for _, mode := range panelModes {
		scrolled := 0
		for h := 72; h >= 20; h -= 4 {
			p := sidebar(t, f.now, f.snap, mode, h)
			lines := plain(p.view())
			frame := strings.Join(lines, "\n")
			label := fmt.Sprintf("%s at %d lines", mode, h)
			if fr := p.frame(); fr.mode != mode {
				t.Errorf("%s: frame mode %s", label, fr.mode)
			}
			if strings.Contains(frame, "(short)") || (mode != modeStatus && indexOfLine(lines, headingRe("ACTIVE")) >= 0) {
				t.Errorf("%s fell back to status:\n%s", label, frame)
			}
			// Judged over the whole list: a title at the window's bottom edge
			// has its rows below it, not none.
			list := scrolledList(t, p)
			p.scroll = 0
			if gone.MatchString(frame) || len(collapsedTitles(parseFrame(append([]string{""}, list...)))) > 0 {
				t.Errorf("%s folds a group:\n%s", label, strings.Join(list, "\n"))
			}
			if strings.Contains(lines[0], "▼") {
				scrolled++
				if missing := unreachable(t, p, f.snap); len(missing) > 0 {
					t.Errorf("%s: %v are on no scroll position:\n%s", label, missing, frame)
				}
			}
		}
		if scrolled == 0 {
			t.Errorf("%s: no height scrolled", mode)
		}
	}
}

// unreachable is the sessions a panel shows at no scroll position, by name or
// in a collapsed title's or a fold line's count. It scrolls p from the top to
// the bottom with scrollBy, as j does.
func unreachable(t *testing.T, p *panel, snap *observer.Snapshot) []string {
	t.Helper()
	p.scroll = 0
	list := scrolledList(t, p)
	seen := strings.Join(list, "\n") + "\n"
	counted := summarisedIn(list)
	var missing []string
	for _, s := range snap.Sessions {
		if panelGroupOf(s) == groupEnded {
			continue
		}
		if !regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(s.Name) + `\s`).MatchString(seen) {
			missing = append(missing, s.Name)
		}
	}
	if len(missing) <= counted {
		return nil
	}
	return missing
}

// scrolledList is a scrolling panel's whole list, rebuilt from its windows:
// the first window, then the line each j brings into view. The header is the
// frame's first headerLines+headerGap lines and the footer its last
// footerGap+footerLines (full chrome), as scrolled lays them out.
func scrolledList(t *testing.T, p *panel) []string {
	t.Helper()
	head, tail := headerLines+headerGap, footerGap+footerLines
	var list []string
	for i := range 500 {
		lines := plain(p.view())
		window := lines[head : len(lines)-tail]
		if i == 0 {
			list = append(list, window...)
		} else {
			list = append(list, window[len(window)-1])
		}
		if !strings.Contains(lines[0], "▼") {
			return list
		}
		p.scrollBy(1)
	}
	t.Fatal("500 scroll steps did not reach the bottom")
	return nil
}

func TestProjectModeGroupsWorktreesWithTheirRepositoryUnderAPinnedBand(t *testing.T) {
	withPanelTheme(t, true) // the band's loud ages are read from the colours
	f := newFleet(t, time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC))
	p := sidebar(t, f.now, f.snap, modeProject, 0)
	lines := plain(p.view())
	frame := strings.Join(lines, "\n")

	band := indexOfLine(lines, headingRe("NEEDS YOU"))
	if band != headerLines+headerGap {
		t.Fatalf("NEEDS YOU is at line %d, want %d — right under the header:\n%s", band, headerLines+headerGap, frame)
	}
	fl := parseStyledFrame(t, p.view())
	if fl[band].right != "5" {
		t.Errorf("the band heading counts %q, want its 5 rows:\n%s", fl[band].right, frame)
	}
	// Every attention row is in the band, longest wait first, each with the
	// loud badge attention gets.
	var order []string
	for _, l := range fl[band+1:] {
		if l.kind == lineRow {
			order = append(order, l.name)
			if !l.tone.loud() {
				t.Errorf("band row %s has a calm badge %q:\n%s", l.name, l.runtime, frame)
			}
		} else if l.kind != lineIndented {
			break
		}
	}
	want := []string{"dotfiles-02", "mnemex-04", "magus-00", "twin-03", "madbench-01"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("band = %v, want %v (longest wait first):\n%s", order, want, frame)
	}
	// A band row names its home on its attn_meta line, after its why (the
	// head is the name and the badges; the why starts with the wait, FR2) —
	// but not when the name already starts with it: magus-00 lives in magus.
	if _, meta, why, isBand := renderedRow(t, p, "magus-00"); !isBand || strings.Contains(meta, "magus") || len(why) == 0 || !strings.HasPrefix(why[0], "waiting ") {
		t.Errorf("a band row repeats a label its name already says: band %v, meta %q, why %q", isBand, meta, why)
	}

	// One group per repository, whichever worktree a session runs in: seven of
	// authflow's sessions are spread over its main checkout and 4 worktrees.
	// Each title counts the rows under it; the ones it lent the band are
	// counted in the band's title and the header's strip (FR9: no per-group
	// "needs you" tail).
	for label, right := range map[string]string{
		"claudish": "8", "magus": "6", "authflow": "7", "madbench": "4",
	} {
		if h, ok := frameHeading(fl, label); !ok || h.right != right {
			t.Errorf("heading %q: %+v, want the right side %q:\n%s", label, h, right, frame)
		}
	}
	if strings.Count(frame, "authflow-") != 7 {
		t.Errorf("authflow's 7 sessions are not all under it:\n%s", frame)
	}
	// Stable order: by name, whatever is busy.
	var heads []string
	for _, l := range fl {
		if l.kind == lineHeading && l.heading != "NEEDS YOU" {
			heads = append(heads, l.heading)
		}
	}
	for i := 1; i < len(heads); i++ {
		if heads[i-1] > heads[i] && heads[i] != "other" {
			t.Errorf("groups out of order: %v", heads)
		}
	}
}

func TestSingletonGroupsFoldIntoOtherWithTheirOwnNameOnTheRow(t *testing.T) {
	f := newFleet(t, time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC))
	p := sidebar(t, f.now, f.snap, modeTmux, 0)
	lines := plain(p.view())
	frame := strings.Join(lines, "\n")
	other := indexOfLine(lines, headingRe("other"))
	if other < 0 {
		t.Fatalf("no other group:\n%s", frame)
	}
	// orbit-demo holds one session: folded, and the row says where it lives,
	// on the built-in meta line under it (the head row is the badges, the
	// name and the count; feedback 3).
	folded := false
	fl := parseFrame(lines)
	for _, r := range frameRows(fl) {
		under, _ := bandLines(fl, r.name)
		if slices.Contains(under, "orbit-demo") && r.status == "idle" && regexp.MustCompile(`^madbench-\d+$`).MatchString(r.name) {
			folded = true
		}
	}
	if !folded {
		t.Errorf("a folded row does not name its tmux session:\n%s", frame)
	}
	if indexOfLine(lines, headingRe("orbit-demo")) >= 0 {
		t.Errorf("a group of one was drawn as a group:\n%s", frame)
	}
}

// A group's label is session text — a tmux session name here — and reaches the
// terminal as a heading and a collapsed line: it is sanitised like every other
// piece of session text (§2.7, §10), so an escape in it cannot colour, move or
// ring anything.
func TestAGroupLabelIsSanitisedBeforeItIsDrawn(t *testing.T) {
	now := fleetNow
	since := now.Add(-time.Hour)
	evil := "evil\x1b[31mX\x07"
	var ss []observer.Session
	for i := 0; i < 2; i++ {
		ss = append(ss, observer.Session{
			SessionID: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("row-%d", i),
			Status: observer.StatusIdle, StatusSince: &since,
			Tmux: &observer.TmuxRef{Session: evil, Pane: fmt.Sprintf("%%%d", i)},
		})
	}
	snap := &observer.Snapshot{Seq: 1, ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{}, Sessions: ss}
	p := sidebar(t, now, snap, modeTmux, 0)
	view := p.view()
	if strings.ContainsAny(view, "\x07") || strings.Contains(view, "\x1b[31mX") {
		t.Errorf("the label's control characters reached the terminal: %q", view)
	}
	if !strings.Contains(ansi.Strip(view), "evil [31mX") {
		t.Errorf("the heading does not draw the sanitised label:\n%s", strings.Join(plain(view), "\n"))
	}
	fr := p.frame()
	for i := range fr.secs {
		if line := p.sectionTitle(&fr.secs[i]); strings.ContainsAny(line, "\x07") || strings.Contains(line, "\x1b[31mX") {
			t.Errorf("the section title (a collapsed group's whole line) carries the label's control characters: %q", line)
		}
	}
}

func TestTmuxModePutsSessionsWithNoPaneInABackgroundGroupLast(t *testing.T) {
	withPanelTheme(t, true) // the band's loud ages are read from the colours
	f := newFleet(t, time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC))
	p := sidebar(t, f.now, f.snap, modeTmux, 0)
	lines := plain(p.view())
	frame := strings.Join(lines, "\n")

	bg := indexOfLine(lines, headingRe("background"))
	other := indexOfLine(lines, headingRe("other"))
	noTmux := indexOfLine(lines, headingRe("no tmux"))
	if bg < 0 || other < 0 || noTmux < 0 || !(other < noTmux && noTmux < bg) {
		t.Errorf("catch-alls at other %d, no tmux %d, background %d: want in that order, last:\n%s", other, noTmux, bg, frame)
	}
	// The band names a background session's group too — on its attn_meta
	// line, painted under the why while the band has the room — and the head
	// carries the loud coral BLK badge.
	fl := parseStyledFrame(t, p.view())
	under, drawn := bandLines(fl, "dotfiles-02")
	if !drawn || len(under) != 2 || !strings.HasPrefix(under[0], "waiting ") || under[1] != "background" {
		t.Errorf("a background band row does not say, on screen, that it has no pane: %q:\n%s", under, frame)
	}
	if r, ok := frameRow(fl, "dotfiles-02"); !ok || r.status != "blocked" || r.tone != badgeBlk {
		t.Errorf("the blocked band row is not drawn with its loud BLK badge (%+v):\n%s", r, frame)
	}
	// And the background group's title counts the one row under it.
	if h := fl[bg]; h.kind != lineHeading || h.right != "1" {
		t.Errorf("background title = %q", lines[bg])
	}
}

func TestColourModeFollowsTheSpectrumWithNoColourLast(t *testing.T) {
	f := newFleet(t, time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC))
	p := sidebar(t, f.now, f.snap, modeColour, 0)
	lines := plain(p.view())
	frame := strings.Join(lines, "\n")

	var heads []string
	for _, l := range parseFrame(lines) {
		// A title, over rows or alone (a collapsed group).
		if l.kind == lineHeading && l.heading != "NEEDS YOU" {
			heads = append(heads, l.heading)
		}
	}
	want := "red orange yellow green cyan blue purple pink no colour"
	if got := strings.Join(heads, " "); got != want {
		t.Errorf("colour groups = %s, want %s:\n%s", got, want, frame)
	}
	// The row is drawn in its colour already, so the band does not spell it:
	// not on its head, and not on the attn_meta line a band row's home is
	// named on.
	if r, ok := frameRow(parseFrame(lines), "magus-00"); !ok || r.group != "" {
		t.Errorf("a band row names its colour in words (%+v):\n%s", r, frame)
	}
	if _, meta, _, isBand := renderedRow(t, p, "magus-00"); !isBand || strings.Contains(meta, "blue") {
		t.Errorf("a band row names its colour in words: band %v, meta %q", isBand, meta)
	}
}

// Every session is on screen or counted, the band is never cut, and the panel
// is never taller than its pane — in every mode, at every height the research
// measured a sidebar or a window at, on both surfaces.
func TestEveryModeAccountsForEverySessionAtEveryRealHeight(t *testing.T) {
	f := newFleet(t, time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC))
	for _, surface := range []panelSurface{surfacePane, surfaceTUI} {
		for _, mode := range panelModes {
			for h := 24; h <= 72; h++ {
				p := sidebar(t, f.now, f.snap, mode, h)
				p.surface = surface
				label := fmt.Sprintf("%s surface %d, %d lines", mode, surface, h)
				vis := assertTheFrameAccountsForEverySession(t, label, p.view(), panelWidth, h, f.snap, f.now)
				// A scrolling frame: every session is on some scroll
				// position, or counted in a fold.
				if strings.Contains(vis[0], "▼") {
					if missing := unreachable(t, p, f.snap); len(missing) > 0 {
						t.Errorf("%s: %v are on no scroll position and in no count", label, missing)
					}
				}
			}
		}
	}
}

// summarisedLines counts a frame's collapsed and partial lines.
func summarisedLines(lines []string) int {
	n := len(collapsedTitles(parseFrame(lines)))
	for _, l := range lines {
		if partialLine.MatchString(l) || mergedLine.MatchString(l) {
			n++
		}
	}
	return n
}

// Ended sessions are not drawn in any mode (the user, 2026-10-01: "we don't
// need them"): no ENDED title, no row, no hint line counting them.
func TestEndedSessionsAreNotDrawn(t *testing.T) {
	now := time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC)
	f := newFleet(t, now)
	var ended []string
	for _, s := range f.snap.Sessions {
		if panelGroupOf(s) == groupEnded {
			ended = append(ended, s.Name)
		}
	}
	if len(ended) == 0 {
		t.Fatal("the fleet has no ended session, so this proves nothing")
	}
	for _, mode := range panelModes {
		p := sidebar(t, now, f.snap, mode, 0)
		view := strings.Join(plain(p.view()), "\n")
		for _, name := range ended {
			if regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(name) + `\s`).MatchString(view + "\n") {
				t.Errorf("%s: the ended %s is drawn", mode, name)
			}
		}
		if strings.Contains(view, "ENDED") || strings.Contains(view, "ended") {
			t.Errorf("%s: the panel mentions ended sessions:\n%s", mode, view)
		}
	}
}

// The mode is saved where every surface reads it, as one plain word, and
// anything else there reads as "no choice made".
func TestTheChosenModeIsSavedAsOneWordAndReadBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "tt")
	noClaude := dir + "-claude-root" // statedir requires a tree to refuse (moox §2.4)
	if _, ok := readPanelMode(dir, noClaude); ok {
		t.Fatal("a mode was read from a directory that does not exist")
	}
	for _, m := range panelModes {
		if err := writePanelMode(dir, noClaude, m); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, panelModeFile))
		must(t, err)
		if string(data) != m.String()+"\n" {
			t.Errorf("saved %q for %s", data, m)
		}
		if got, ok := readPanelMode(dir, noClaude); !ok || got != m {
			t.Errorf("read back %s (%v), want %s", got, ok, m)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, panelModeFile), []byte("model\n"), 0o600))
	if got, ok := readPanelMode(dir, noClaude); ok || got != defaultPanelMode {
		t.Errorf("an unknown word read as %s (%v)", got, ok)
	}
	// And never inside the Claude tree, whatever the state directory says.
	if err := writePanelMode(filepath.Join(dir, "claude", "tt"), filepath.Join(dir, "claude"), modeTmux); err == nil {
		t.Error("the mode was written inside the Claude tree")
	}
}

// g in the sidebar and ctrl+g in the TUI move to the next mode and save it;
// every other open panel follows on its next tick.
func TestTheModeKeysCycleSaveAndAreFollowedEverywhere(t *testing.T) {
	var saved []panelMode
	stubSeam(t, &savePanelMode, func(m panelMode) error { saved = append(saved, m); return nil })
	now := time.Date(2026, 9, 23, 4, 11, 0, 0, time.UTC)

	pm := newPaneModel(newFakePanelSource(), newPanelClock(now), "", paneOptions{})
	updated, _ := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	pm = updated.(paneModel)
	if pm.panel.mode != modeStatus || len(saved) != 1 || saved[0] != modeStatus {
		t.Errorf("g: mode %s, saved %v; want status, saved once", pm.panel.mode, saved)
	}
	pm.panel.close()

	m := newTestModel()
	m.panel.clock = newPanelClock(now)
	m, _ = applyMsg(m, tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.panel.mode != defaultPanelMode {
		t.Errorf("ctrl+g changed a CLOSED panel's mode to %s", m.panel.mode)
	}
	m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.panel.mode != modeStatus || saved[len(saved)-1] != modeStatus {
		t.Errorf("ctrl+g: mode %s, saved %v", m.panel.mode, saved)
	}
	if !strings.Contains(ansi.Strip(m.footer()), "^g group") {
		t.Errorf("the help line does not list ^g while the panel is open: %q", ansi.Strip(m.footer()))
	}

	// Another surface saved tmux: this one follows on its next tick.
	stubSeam(t, &loadPanelMode, func() (panelMode, bool) { return modeTmux, true })
	m.panel.update(panelTickMsg{gen: m.panel.gen, seq: m.panel.tickSeq})
	if m.panel.mode != modeTmux {
		t.Errorf("after a tick the panel is in %s, want the saved tmux", m.panel.mode)
	}
	m.panel.close()
}
