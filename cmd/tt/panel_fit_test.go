package main

// Code review round 2, A9: the panel ignored its height. With the user's
// normal 30–40 sessions it rendered ~75 lines, model.View joined that with the
// session manager, and Bubble Tea v1.3.10 then painted only the LAST
// terminal-height lines — so "NEEDS YOU", both titles and the upper session
// manager rows were never on screen. The P9 tests asserted on the complete
// View() string, which contained everything, and so passed.
//
// These tests therefore assert on what Bubble Tea actually paints, computed
// the way its renderer computes it (bubbleTeaVisible), never on View() alone.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// bubbleTeaVisible is the frame Bubble Tea v1.3.10's standard renderer paints
// for a view on a width x height terminal, as plain text. It mirrors the two
// things flush() does to a frame (standard_renderer.go):
//
//   - lines 180–187: a frame taller than the terminal loses lines from the TOP
//     ("we drop lines from the top of the render buffer if necessary, as we
//     can't navigate the cursor into the terminal's scrollback buffer");
//   - lines 229–235: every line wider than the terminal is truncated to it.
//
// The renderer's own call is ansi.Truncate, so this uses it too.
func bubbleTeaVisible(view string, width, height int) []string {
	lines := strings.Split(view, "\n")
	if height > 0 && len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if width > 0 {
			l = ansi.Truncate(l, width, "")
		}
		out[i] = ansi.Strip(l)
	}
	return out
}

// fitPopulation is the user's normal load, measured on this machine (P9: 37
// sessions, 4 needing attention, 3 unclear; today 45): 35 sessions across
// every group, the attention rows carrying the second line that says WHY.
func fitPopulation(now time.Time) *observer.Snapshot {
	live, dead := true, false
	var ss []observer.Session
	add := func(id, name string, st observer.Status, alive *bool, ageMin int, tune func(*observer.Session)) {
		since := now.Add(-time.Duration(ageMin) * time.Minute)
		s := observer.Session{
			SessionID: id, Name: name, Status: st, Attention: st.IsAttention(),
			Live: alive, StatusSince: &since, ObservedAt: now,
			Tmux: &observer.TmuxRef{Pane: "%" + strconv.Itoa(100+len(ss))},
		}
		if tune != nil {
			tune(&s)
		}
		ss = append(ss, s)
	}
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("w%d", i), fmt.Sprintf("wait-%d", i), observer.StatusWaiting, &live, 10+i*7,
			func(s *observer.Session) { s.WaitReason = observer.WaitReasonInputNeeded })
	}
	add("b0", "blocked-job", observer.StatusBlocked, &live, 41, func(s *observer.Session) {
		s.Kind = observer.KindBackground
		s.Tmux = nil
		s.Needs = ptr("confirm the migration plan")
	})
	for i := 0; i < 6; i++ {
		add(fmt.Sprintf("a%d", i), fmt.Sprintf("busy-%d", i), observer.StatusBusy, &live, 2+i, nil)
	}
	for i := 0; i < 20; i++ {
		add(fmt.Sprintf("i%02d", i), fmt.Sprintf("idle-%02d", i), observer.StatusIdle, &live, 30+i, nil)
	}
	for i := 0; i < 2; i++ {
		add(fmt.Sprintf("u%d", i), fmt.Sprintf("hazy-%d", i), observer.StatusBusy, nil, 60+i, func(s *observer.Session) {
			s.Advisories = []observer.Advisory{{Code: "transcript-absent"}}
		})
	}
	add("d0", "done-0", observer.StatusDone, &live, 90, nil)
	add("d1", "gone-1", observer.StatusIdle, &dead, 95, nil)
	return &observer.Snapshot{
		Seq: 1, ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{}, Sessions: ss,
	}
}

// overflowPattern is the old overflow line, "+N more: …": the scroll area
// replaced it, and a frame must never draw it.
var overflowPattern = regexp.MustCompile(`\+(\d+) more`)

// assertTheFrameAccountsForEverySession is the common verdict on one painted
// frame: the frame is not taller than the terminal (so Bubble Tea dropped
// nothing), the panel's title and NEEDS YOU are on it, EVERY attention row is
// on it in full, and every session is either on it by name or counted by the
// overflow line or the folded ENDED line — nothing silently cut.
func assertTheFrameAccountsForEverySession(t *testing.T, label, view string, width, height int, snap *observer.Snapshot, now time.Time) []string {
	t.Helper()
	if n := strings.Count(view, "\n") + 1; n > height {
		t.Errorf("%s: View() is %d lines on a %d-line terminal; Bubble Tea drops the top %d", label, n, height, n-height)
	}
	vis := bubbleTeaVisible(view, width, height)
	frame := strings.Join(vis, "\n")

	// The header's tabs and NEEDS YOU: the panel's top was not cut.
	for _, want := range []string{"ux │", "NEEDS YOU"} {
		if !strings.Contains(frame, want) {
			t.Errorf("%s: %q is not on screen:\n%s", label, want, frame)
		}
	}

	shown, attention := 0, 0
	for _, s := range snap.Sessions {
		// The rows NEEDS YOU holds. An attention row whose liveness is
		// unknown is UNCLEAR, as it has always been, and is not promised.
		if panelGroupOf(s) == groupNeedsYou {
			attention++
			// The whole row, both ends included: the status code badge and
			// the runtime badge before the name (L2: its text four cells
			// after the runtime), and the ctx count at the right edge after
			// it (plus a bar, for a template that opts into one) — a panel
			// pushed off the right edge would show the name and lose the
			// count. The row may sit beside the session list (the TUI), so it
			// is found anywhere on the line.
			row := regexp.MustCompile(`(?m)(^|\s|▌|❯) ` + regexp.QuoteMeta(statusCode(s)) + ` +` +
				regexp.QuoteMeta(fmtRuntime(panelRuntime(s, now))) + `    ` + regexp.QuoteMeta(s.Name) + ` +\S+(?: [▰▱]{5})? *$`)
			if !row.MatchString(frame) {
				t.Errorf("%s: the attention row %q is not on screen in full:\n%s", label, s.Name, frame)
			}
		}
		if regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(s.Name) + `\s`).MatchString(frame + "\n") {
			shown++
		}
	}
	accounted := shown + summarisedIn(panelParts(vis))
	// Ended sessions are not drawn: the frame accounts for the live ones.
	live := 0
	for _, s := range snap.Sessions {
		if panelGroupOf(observer.AsOf(&observer.Snapshot{Sessions: []observer.Session{s}}, now, observer.DefaultPolicy()).Sessions[0]) != groupEnded {
			live++
		}
	}
	// Rows below the window are the scroll area's: the panel says so in its
	// top-right corner, and a caller that holds the panel scrolls to check
	// (unreachable). Nothing is cut or summed up any more.
	if scrolls := strings.Contains(panelParts(vis)[0], "▼"); scrolls {
		if overflowPattern.MatchString(frame) {
			t.Errorf("%s: a scrolling frame still counts hidden rows:\n%s", label, frame)
		}
	} else if accounted != live {
		t.Errorf("%s: %d sessions on screen + counted = %d, want all %d live sessions accounted for:\n%s",
			label, shown, accounted, live, frame)
	}
	if attention == 0 {
		t.Fatalf("%s: the fixture has no attention rows, so this proves nothing", label)
	}
	return vis
}

// partialLine is a busy group's folded rows ("  + 20 idle · 2 unclear"), and
// mergedLine a tight frame's quiet groups folded into one line ("  + 5
// groups: 14 idle · 2 unclear"). A collapsed group is its title drawn alone
// (collapsedTitles).
var (
	partialLine = regexp.MustCompile(`^\s+\+ ((?:\d+ (?:idle|unclear|active|ended))(?: · \d+ (?:idle|unclear|active|ended))*)$`)
	mergedLine  = regexp.MustCompile(`^\s+\+ \d+ groups?: ((?:\d+ (?:idle|unclear|active|ended))(?: · \d+ (?:idle|unclear|active|ended))*)$`)
	countWord   = regexp.MustCompile(`(\d+) (?:idle|unclear|active|ended)`)
)

// collapsedTitles is the section titles a frame draws alone — a collapsed
// group, whose title's count is all it says of its rows. A title over rows is
// followed by one (a truncated frame never draws a title without a row), and
// a partial fold with no active row by its "+ N idle" line.
func collapsedTitles(fl []frameLine) []frameLine {
	var out []frameLine
	for i, l := range fl {
		if l.kind != lineHeading {
			continue
		}
		if i+1 < len(fl) && (fl[i+1].kind == lineRow || partialLine.MatchString(fl[i+1].text)) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// panelParts is the panel's column of each painted line: its last
// panelWidth display columns (the TUI draws the session list to its left).
func panelParts(vis []string) []string {
	out := make([]string, len(vis))
	for i, l := range vis {
		out[i] = strings.TrimRight(ansi.TruncateLeft(l, max(ansi.StringWidth(l)-panelWidth, 0), ""), " ")
	}
	return out
}

// summarisedIn is how many sessions a frame accounts for by count rather than
// by name: its collapsed groups (their titles' counts) and its folded rows.
func summarisedIn(lines []string) int {
	n := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " ")
		var counts string
		switch {
		case mergedLine.MatchString(l):
			counts = mergedLine.FindStringSubmatch(l)[1]
		case partialLine.MatchString(l):
			counts = partialLine.FindStringSubmatch(l)[1]
		default:
			continue
		}
		for _, m := range countWord.FindAllStringSubmatch(counts, -1) {
			v, _ := strconv.Atoi(m[1])
			n += v
		}
	}
	for _, l := range collapsedTitles(parseFrame(lines)) {
		v, _ := strconv.Atoi(l.right)
		n += v
	}
	return n
}

// THE A9 TEST. The real TUI model, the real key path (ctrl+o), the user's
// population, and the terminal sizes people actually use — asserted on the
// region Bubble Tea paints.
func TestTheTUIsVisibleRegionKeepsBothTitlesAndEveryAttentionRow(t *testing.T) {
	now := time.Now()
	snap := fitPopulation(now)
	stubSeam(t, &openPanelSource, func(context.Context, observer.Clock) (panelSource, string, error) {
		return newFakePanelSource(), "", nil
	})

	var tmux []session
	for i := 0; i < 14; i++ { // the user's 14 tmux sessions
		tmux = append(tmux, session{name: fmt.Sprintf("tmux-%02d", i), windows: 2})
	}
	for _, width := range []int{100, 140} {
		for _, height := range []int{24, 30, 40} {
			label := fmt.Sprintf("%dx%d", width, height)
			m := newTestModel(tmux...)
			// The fixture's clock, not the system's: the ages are asserted to
			// the minute, and a clock read a millisecond on either side of
			// `now` renders 10m as 9m.
			m.panel.clock = newPanelClock(now)
			m, _ = applyMsg(m, tea.WindowSizeMsg{Width: width, Height: height})
			m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlO})
			if !m.panel.open {
				t.Fatalf("%s: ctrl+o did not open the panel", label)
			}
			m.panel.update(panelEventMsg{gen: m.panel.gen, ev: snapshotEvent(snap)})

			vis := assertTheFrameAccountsForEverySession(t, label, m.View(), width, height, snap, now)
			frame := strings.Join(vis, "\n")
			// The session manager is still whole: its title and its first row.
			for _, want := range []string{listTitle, "tmux-00"} {
				if !strings.Contains(frame, want) {
					t.Errorf("%s: the session manager's %q is not on screen:\n%s", label, want, frame)
				}
			}
			// And the panel starts where layoutPanel put it: its tabs sit in
			// the column after the list's, not wherever the widest body line
			// (the help footer) happened to end.
			off := strings.Index(ansi.Strip(m.panel.lines()[0]), "Project")
			if col := strings.Index(vis[0], "Project"); col < 0 || ansi.StringWidth(vis[0][:col]) != width-m.panel.width+off {
				t.Errorf("%s: the panel's tabs are at column %d, want %d (list %d + panel %d = %d):\n%s",
					label, ansi.StringWidth(vis[0][:max(col, 0)]), width-m.panel.width+off, width-m.panel.width, m.panel.width, width, frame)
			}
			m.panel.close()
		}
	}
}

// The tmux sidebar (`tt observe --pane`) is the panel alone in a 46-column
// pane, and it had the same defect: a 50-row pane showed the bottom 50 of 75
// lines.
func TestThePaneSidebarFitsItsPaneAndSaysWhatItLeftOut(t *testing.T) {
	now := time.Now()
	snap := fitPopulation(now)
	for _, height := range []int{24, 30, 40} {
		label := fmt.Sprintf("pane 46x%d", height)
		pm := newPaneModel(newFakePanelSource(), newPanelClock(now), "", paneOptions{})
		updated, _ := pm.Update(tea.WindowSizeMsg{Width: panelWidth, Height: height})
		pm = updated.(paneModel)
		pm.panel.update(panelEventMsg{gen: pm.panel.gen, ev: snapshotEvent(snap)})
		vis := assertTheFrameAccountsForEverySession(t, label, pm.View(), panelWidth, height, snap, now)
		frame := strings.Join(vis, "\n")
		// The first line is the panel's tabs: nothing above it was cut.
		if !strings.HasPrefix(strings.TrimSpace(vis[0]), "[P]") {
			t.Errorf("%s: the first painted line is %q, want the tabs", label, vis[0])
		}
		// Some rows had to go at these heights, and the frame says so: its
		// top-right corner says the list scrolls (▼), or it folded groups.
		if !strings.Contains(vis[0], "▼") && summarisedIn(vis) == 0 {
			t.Errorf("%s: 35 sessions do not fit in %d rows, yet nothing says the list goes on:\n%s", label, height, frame)
		}
		// And j reaches every one of them.
		if strings.Contains(vis[0], "▼") {
			if missing := unreachable(t, &pm.panel, snap); len(missing) > 0 {
				t.Errorf("%s: %v are on no scroll position and in no count", label, missing)
			}
		}
		pm.panel.close()
	}
}

// When the attention rows ALONE overflow, the longest waits are the ones in
// view, the corner says the list goes on, and scrolling reaches every one —
// nothing is cut, and no line sums them up.
func TestWhenNeedsYouAloneOverflowsTheLongestWaitsStayAndTheRestScroll(t *testing.T) {
	now := time.Now()
	live := true
	var ss []observer.Session
	for i := 0; i < 30; i++ {
		since := now.Add(-time.Duration(i+1) * time.Minute)
		ss = append(ss, observer.Session{
			SessionID: fmt.Sprintf("w%02d", i), Name: fmt.Sprintf("wait-%02d", i),
			Status: observer.StatusWaiting, WaitReason: observer.WaitReasonInputNeeded,
			Attention: true, Live: &live, StatusSince: &since,
		})
	}
	snap := &observer.Snapshot{ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{}, Sessions: ss}

	p, _, _ := openTestPanel(t, now)
	p.height = 24
	deliver(t, p, snapshotEvent(snap))
	view := p.view()
	vis := bubbleTeaVisible(view, panelWidth, 24)
	frame := strings.Join(vis, "\n")

	if n := strings.Count(view, "\n") + 1; n > 24 {
		t.Fatalf("%d lines in a 24-line panel", n)
	}
	// Longest wait first: wait-29 (30m) is in view, wait-00 (1m) is not.
	if !strings.Contains(frame, "wait-29") || strings.Contains(frame, "wait-00 ") {
		t.Fatalf("the longest waits are not the ones in view:\n%s", frame)
	}
	if !strings.Contains(vis[0], "▼") || overflowPattern.MatchString(frame) {
		t.Fatalf("the corner does not say the list goes on, or a line counts hidden rows:\n%s", frame)
	}
	seen := frame
	for range 100 {
		p.scrollBy(1)
		seen += "\n" + strings.Join(plain(p.view()), "\n")
	}
	if last := plain(p.view())[0]; strings.Contains(last, "▼") || !strings.Contains(last, "▲") {
		t.Errorf("100 j's did not reach the bottom: %q", last)
	}
	for _, s := range ss {
		if !strings.Contains(seen, s.Name+" ") {
			t.Errorf("%s is on no scroll position", s.Name)
		}
	}
}

// No height is too small to respect: at every height from 1 to 12 the panel is
// never taller than it was given.
func TestThePanelIsNeverTallerThanItsHeightAtAnySize(t *testing.T) {
	now := time.Now()
	snap := fitPopulation(now)
	p, _, _ := openTestPanel(t, now)
	p.note = "observer stopped - waiting for it to restart"
	deliver(t, p, snapshotEvent(snap))
	for h := 1; h <= 12; h++ {
		p.height = h
		if n := strings.Count(p.view(), "\n") + 1; n > h {
			t.Errorf("height %d: the panel rendered %d lines:\n%s", h, n, p.view())
		}
	}
	// Before any baseline, too.
	p.snap = nil
	for h := 1; h <= 4; h++ {
		p.height = h
		if n := strings.Count(p.view(), "\n") + 1; n > h {
			t.Errorf("waiting, height %d: %d lines", h, n)
		}
	}
}

// A panel that fits renders byte for byte what an unbounded one would: the
// height bound changes nothing until it has to.
func TestAPanelThatFitsIsExactlyWhatAnUnboundedOneRenders(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, _, _ := openTestPanel(t, now)
	// Status mode: a grouped mode below its height floor folds its quiet
	// groups by design, so "exactly as tall as it needs" is only a no-op in
	// status.
	p.mode = modeStatus
	deliver(t, p, snapshotEvent(totalityFixture(now)))
	p.height = 0
	unbounded := p.view()
	p.height = strings.Count(unbounded, "\n") + 1
	if got := p.view(); got != unbounded {
		t.Fatalf("a panel exactly tall enough differs from an unbounded one:\n--- bounded\n%s\n--- unbounded\n%s", got, unbounded)
	}
	if strings.Contains(unbounded, "more") {
		t.Fatalf("an unbounded panel carries an overflow line:\n%s", unbounded)
	}
}
