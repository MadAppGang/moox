package main

// P9's tests for the panel (§5.5a). Every one of them drives the panel through
// its real message path — there is no test-only rendering entry point — and
// none of them touches tmux, a socket, a Claude tree or a clock it does not
// own.

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// ---------------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------------

// panelClock is the panel's injected clock. The panel is the ONE place in this
// design permitted to have a clock that ticks (the user's override of D3), so
// it is the one place a fake clock has to be able to drive.
type panelClock struct {
	mu  sync.Mutex
	now time.Time
}

func newPanelClock(now time.Time) *panelClock { return &panelClock{now: now} }

func (c *panelClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *panelClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakePanelSource is a panelSource that hands out a channel the test writes
// to. It records how many subscriptions were taken and whether each one's
// context was cancelled, which is how the goroutine-leak test proves the
// panel let go.
type fakePanelSource struct {
	mu   sync.Mutex
	ch   chan observer.Event
	subs int
	err  error
}

func newFakePanelSource() *fakePanelSource {
	return &fakePanelSource{ch: make(chan observer.Event, 8)}
}

func (f *fakePanelSource) Subscribe(_ context.Context, _ observer.Filter) (<-chan observer.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs++
	if f.err != nil {
		return nil, f.err
	}
	return f.ch, nil
}

func (f *fakePanelSource) subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs
}

// waitSubscriptions waits for exactly want subscriptions and then asserts the
// count STAYS there. The wait is needed because Subscribe is called from the
// pump goroutine, so "how many subscriptions has this panel taken" is not
// observable synchronously — and the property being asserted is an upper bound
// (no re-subscription, §5.5a's "decided once, never re-probed in a loop"), so
// polling for the number and then holding it is the honest shape.
func waitSubscriptions(t *testing.T, src *fakePanelSource, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := src.subscriptions()
		if got == want {
			break
		}
		if got > want {
			t.Fatalf("the panel took %d subscriptions, want %d", got, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panel took %d subscriptions after 2s, want %d", got, want)
		}
		time.Sleep(time.Millisecond) //nolint:forbidigo // test-only poll
	}
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // test-only settle
	if got := src.subscriptions(); got != want {
		t.Fatalf("the subscription count moved to %d, want it to stay at %d", got, want)
	}
}

// openTestPanel returns an OPEN panel over a fake source, with the tick and
// pump already started, plus the fake and the clock.
func openTestPanel(t testing.TB, now time.Time) (*panel, *fakePanelSource, *panelClock) {
	t.Helper()
	src := newFakePanelSource()
	clock := newPanelClock(now)
	p := newPanel(src, clock)
	p.width = panelWidth
	p.height = 40
	cmd := p.toggle()
	if cmd == nil {
		t.Fatal("toggling the panel open returned no command; the tick and the pump are both missing")
	}
	t.Cleanup(p.close)
	return &p, src, clock
}

// deliver pushes one event through the panel's own update path.
func deliver(t testing.TB, p *panel, ev observer.Event) {
	t.Helper()
	p.update(panelEventMsg{gen: p.gen, ev: ev})
}

// snapshotEvent wraps a snapshot as the baseline event Subscribe delivers.
func snapshotEvent(snap *observer.Snapshot) observer.Event {
	return observer.Event{Type: observer.EventSnapshot, Snapshot: snap, At: snap.ObservedAt}
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// the H11 fixture: all nine statuses, plus live == null and live == false
// ---------------------------------------------------------------------------

// totalityFixture is ONE snapshot containing every status in AllStatuses plus a
// live == null record and a live == false record, which is exactly what H11
// says a test must drive.
func totalityFixture(now time.Time) *observer.Snapshot {
	live, dead := true, false
	mk := func(id, name string, status observer.Status, alive *bool, ageMin int) observer.Session {
		since := now.Add(-time.Duration(ageMin) * time.Minute)
		return observer.Session{
			SessionID:   id,
			Name:        name,
			Status:      status,
			Attention:   status.IsAttention(),
			Live:        alive,
			StatusSince: &since,
			ObservedAt:  now,
		}
	}

	sessions := []observer.Session{
		mk("id-idle", "alpha", observer.StatusIdle, &live, 3),
		mk("id-busy", "bravo", observer.StatusBusy, &live, 4),
		mk("id-shell", "charlie", observer.StatusShell, &live, 5),
		mk("id-waiting", "delta", observer.StatusWaiting, &live, 14),
		mk("id-blocked", "echo", observer.StatusBlocked, &live, 41),
		mk("id-working", "foxtrot", observer.StatusWorking, &live, 6),
		mk("id-done", "golf", observer.StatusDone, &live, 7),
		mk("id-failed", "hotel", observer.StatusFailed, &live, 8),
		mk("id-unknown", "india", observer.StatusUnknown, &live, 9),
		// The two liveness gaps, on statuses that would otherwise land in a
		// live group — which is the whole point of them being here.
		mk("id-live-null", "juliett", observer.StatusBusy, nil, 10),
		mk("id-live-false", "kilo", observer.StatusIdle, &dead, 11),
	}

	counts := observer.NewCounts()
	counts.Total = len(sessions)
	for _, s := range sessions {
		switch {
		case s.Live == nil:
			counts.Unknown++
		case *s.Live:
			counts.Live++
		default:
			counts.Dead++
		}
		if s.Attention {
			counts.Attention++
		}
		counts.ByStatus[s.Status]++
	}

	return &observer.Snapshot{
		Seq:        1,
		ObservedAt: now,
		Counts:     counts,
		Sources:    map[string]observer.SourceReport{},
		Sessions:   sessions,
	}
}

// ---------------------------------------------------------------------------
// H11 — totality
// ---------------------------------------------------------------------------

// Every status, plus both liveness gaps, lands in EXACTLY one group. A session
// with no group would be a session that silently disappears from the panel,
// which is the defect H11 exists to prevent.
func TestEveryStatusAndBothLivenessGapsLandInExactlyOneGroup(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	snap := totalityFixture(now)

	seen := map[string][]panelGroup{}
	rows := panelRows(snap, now, observer.DefaultPolicy())
	for _, g := range panelGroups {
		for _, s := range rows[g] {
			seen[s.SessionID] = append(seen[s.SessionID], g)
		}
	}

	if len(seen) != len(snap.Sessions) {
		t.Fatalf("%d of %d sessions were grouped; the contract is total", len(seen), len(snap.Sessions))
	}
	for _, s := range snap.Sessions {
		groups := seen[s.SessionID]
		if len(groups) != 1 {
			t.Errorf("%s (%s) appears in %d groups (%v), want exactly 1", s.SessionID, s.Status, len(groups), groups)
		}
	}

	// And the specific placements H11 names, so a future refactor cannot make
	// the totality hold by putting everything in one group.
	want := map[string]panelGroup{
		"id-waiting":    groupNeedsYou,
		"id-blocked":    groupNeedsYou,
		"id-busy":       groupActive,
		"id-working":    groupActive,
		"id-idle":       groupIdle,
		"id-shell":      groupIdle,
		"id-unknown":    groupUnclear,
		"id-live-null":  groupUnclear,
		"id-done":       groupEnded,
		"id-failed":     groupEnded,
		"id-live-false": groupEnded,
	}
	for id, g := range want {
		if got := seen[id]; len(got) != 1 || got[0] != g {
			t.Errorf("%s is in %v, want %s", id, got, g.title())
		}
	}
}

// The set of rendered session ids equals the snapshot's live set: nothing that
// is not ended is dropped on the way to the screen, and nothing ended is
// drawn (the user, 2026-10-01: ended sessions are not needed on the panel).
func TestTheRenderedSetIsTheSnapshotsLiveSetWithNothingSilentlyDropped(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	snap := totalityFixture(now)

	p, _, _ := openTestPanel(t, now)
	deliver(t, p, snapshotEvent(snap))

	view := p.view()
	ended := 0
	for _, s := range observer.AsOf(snap, now, p.policy).Sessions {
		drawn := strings.Contains(view, s.Name)
		switch {
		case panelGroupOf(s) == groupEnded:
			ended++
			if drawn {
				t.Errorf("the ended %s (%s) is drawn:\n%s", s.Name, s.Status, view)
			}
		case !drawn:
			t.Errorf("%s (%s, %s) is not in the rendered panel:\n%s", s.Name, s.SessionID, s.Status, view)
		}
	}
	if ended == 0 {
		t.Fatal("the fixture has no ended session, so this proves nothing")
	}
	if strings.Contains(view, "ENDED") || strings.Contains(view, "ended (") {
		t.Errorf("the panel still mentions ended sessions:\n%s", view)
	}
}

// ---------------------------------------------------------------------------
// ordering
// ---------------------------------------------------------------------------

// NEEDS YOU is ordered longest-wait-first, and every other group by name with
// the session id as the tiebreak, so two renderings of one state agree.
func TestNeedsYouIsLongestWaitFirstAndTheOtherGroupsAreOrderedByName(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	live := true
	waiting := func(id, name string, ageMin int) observer.Session {
		since := now.Add(-time.Duration(ageMin) * time.Minute)
		return observer.Session{
			SessionID: id, Name: name, Status: observer.StatusWaiting,
			Attention: true, Live: &live, StatusSince: &since,
		}
	}
	idle := func(id, name string) observer.Session {
		since := now.Add(-time.Minute)
		return observer.Session{
			SessionID: id, Name: name, Status: observer.StatusIdle,
			Live: &live, StatusSince: &since,
		}
	}

	snap := &observer.Snapshot{
		ObservedAt: now,
		Counts:     observer.NewCounts(),
		Sources:    map[string]observer.SourceReport{},
		Sessions: []observer.Session{
			waiting("w-short", "zulu", 2),
			waiting("w-long", "alpha", 90),
			waiting("w-mid", "mike", 30),
			idle("i-b", "bravo"),
			idle("i-a", "alpha2"),
		},
	}

	rows := panelRows(snap, now, observer.DefaultPolicy())
	gotWaiting := []string{}
	for _, s := range rows[groupNeedsYou] {
		gotWaiting = append(gotWaiting, s.SessionID)
	}
	if want := []string{"w-long", "w-mid", "w-short"}; strings.Join(gotWaiting, ",") != strings.Join(want, ",") {
		t.Fatalf("NEEDS YOU order is %v, want longest wait first %v", gotWaiting, want)
	}

	gotIdle := []string{}
	for _, s := range rows[groupIdle] {
		gotIdle = append(gotIdle, s.Name)
	}
	if want := []string{"alpha2", "bravo"}; strings.Join(gotIdle, ",") != strings.Join(want, ",") {
		t.Fatalf("IDLE order is %v, want ascending by name %v", gotIdle, want)
	}
}

// Ties are broken by session id, which is what makes the order deterministic
// rather than dependent on the snapshot's own ordering.
func TestTiesAreBrokenBySessionIDSoTheOrderIsDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	live := true
	since := now.Add(-time.Minute)
	mk := func(id string) observer.Session {
		return observer.Session{
			SessionID: id, Name: "same", Status: observer.StatusIdle,
			Live: &live, StatusSince: &since,
		}
	}
	snap := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sources:  map[string]observer.SourceReport{},
		Sessions: []observer.Session{mk("c"), mk("a"), mk("b")},
	}
	rows := panelRows(snap, now, observer.DefaultPolicy())
	got := []string{}
	for _, s := range rows[groupIdle] {
		got = append(got, s.SessionID)
	}
	if strings.Join(got, "") != "abc" {
		t.Fatalf("tie order is %v, want a,b,c by session id", got)
	}
}

// ---------------------------------------------------------------------------
// the rendered row
// ---------------------------------------------------------------------------

// The row carries the facts §5.5(a) lists: name, status plus waitReason, the
// humanised age, the tmux pane and the subagent count.
func TestTheRowNamesTheStatusTheWaitReasonTheAgeAndTheSubagents(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	live := true
	since := now.Add(-14 * time.Minute)
	subagents := 2

	snap := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{
			SessionID: "s1", Name: "madbench", Status: observer.StatusWaiting,
			WaitReason: observer.WaitReasonInputNeeded, Attention: true,
			Live: &live, StatusSince: &since,
			Tmux: &observer.TmuxRef{Raw: "madbench:@51.%107", Session: "madbench", Window: "@51", Pane: "%107"},
			Transcript: &observer.TranscriptFacts{
				AgentColor: "green", ColorFound: true,
				Subagents: &subagents, SubagentsKnown: true,
			},
		}},
	}

	p, _, _ := openTestPanel(t, now)
	deliver(t, p, snapshotEvent(snap))
	view := p.view()

	// The painted band row: the status as the ASK badge, and the time in the
	// status with the wait reason on the why line under it.
	for _, want := range []string{"NEEDS YOU", "madbench", " ASK ", "waiting 14m · input-needed"} {
		if !strings.Contains(ansi.Strip(view), want) {
			t.Errorf("the rendered panel does not carry %q:\n%s", want, view)
		}
	}
	// Under the band row: how long it has waited and why, then its home
	// group (no folder: the fixture has no CWD). The pane id is an internal
	// id and is not drawn by the built-in templates (feedback 3).
	under, drawn := bandLines(parseFrame(plain(view)), "madbench")
	if !drawn || len(under) != 2 || under[0] != "waiting 14m · input-needed" || under[1] != "no folder" || strings.Contains(view, "%107") {
		t.Errorf("the band row's painted lines do not carry the why and its home (and no pane id): %q\n%s", under, view)
	}
	// The subagent count left the built-in view with the meta line (FR9),
	// and a panel.conf brings it back: attn_meta is drawn on a line the why
	// leaves over.
	conf := newConfFixture(t)
	conf.write(t, `attn_meta = "     {sub}"`+"\n", now)
	p.followConfig()
	c := *snap
	deliver(t, p, snapshotEvent(&c))
	if under, _ := bandLines(parseFrame(plain(p.view())), "madbench"); len(under) != 2 || under[1] != "2 sub" {
		t.Errorf("attn_meta = {sub} does not bring the subagents back: %q", under)
	}
}

// A blocked background agent renders its `needs` — §5.5(a) calls it the single
// most useful line on the surface — and a live: null row renders a dim marker
// plus the first advisory's code, so the UNCLEAR group explains itself.
func TestABlockedAgentRendersItsNeedsAndAnUnclearRowExplainsItself(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	live := true
	since := now.Add(-41 * time.Minute)

	snap := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{
			{
				SessionID: "bg", Name: "worker", Kind: observer.KindBackground,
				Status: observer.StatusBlocked, Attention: true,
				Live: &live, StatusSince: &since,
				ShortID: ptr("be9663df"),
				Needs:   ptr("confirm the migration plan"),
			},
			{
				SessionID: "un", Name: "hazy", Status: observer.StatusBusy,
				Live: nil, StatusSince: &since,
				Advisories: []observer.Advisory{{Code: "liveness-unknown-no-evidence"}},
			},
		},
	}

	p, _, _ := openTestPanel(t, now)
	p.width = 80 // wide enough that the needs string is not cut to nothing
	deliver(t, p, snapshotEvent(snap))
	view := p.view()

	if !strings.Contains(view, "confirm the migration plan") {
		t.Errorf("a blocked agent's needs is not rendered:\n%s", view)
	}
	if !strings.Contains(view, "live?") || !strings.Contains(view, "liveness-unknown-no-evidence") {
		t.Errorf("the live: null row does not explain itself:\n%s", view)
	}
}

// The corroboration footer speaks only when the `claude agents` check is
// FAILING. A listing that is merely old is the normal state of a quiet
// machine: the check is re-run only when a rebuild happens (there is no
// timer), and no status depends on it, so "corroboration 1m old" on an idle
// fleet was noise (the user, 2026-10-21: "what is corroboration at all").
// A failing check is still never a silent omission (§5.5a): on 2026-10-21 it
// failed for 8h50m.
func TestTheCorroborationFooterAppearsOnlyWhileTheCheckFails(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	age := int64(41_000)
	failure := "claude agents --json: exit status 1"
	snap := func(lastErr *string) *observer.Snapshot {
		return &observer.Snapshot{
			ObservedAt: now, Counts: observer.NewCounts(),
			Sources: map[string]observer.SourceReport{
				observer.SourceAgentsCLI: {ID: observer.SourceAgentsCLI, Stale: true, AgeMs: &age, LastError: lastErr},
			},
		}
	}

	p, _, _ := openTestPanel(t, now)
	deliver(t, p, snapshotEvent(snap(nil)))
	if view := p.view(); strings.Contains(view, "claude agents") || strings.Contains(view, "corroboration") {
		t.Fatalf("an old but healthy listing drew a footer:\n%s", view)
	}
	deliver(t, p, snapshotEvent(snap(&failure)))
	if view := p.view(); !strings.Contains(view, "claude agents check failing, last ok 41s ago") {
		t.Fatalf("a failing check is not reported:\n%s", view)
	}
}

// A pane takes the source reports from every sources event, not only from its
// baseline. Before 2026-10-21 apply dropped EventSources, so each sidebar kept
// the source state of the moment it connected: one window said "corroboration
// 1m old" forever while its neighbours, connected a minute apart, said nothing.
func TestASourcesEventReplacesTheSourceReports(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	age := int64(41_000)
	failure := "claude agents --json: exit status 1"
	failing := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sessions: []observer.Session{{SessionID: "a", Name: "kept", Status: observer.StatusIdle}},
		Sources: map[string]observer.SourceReport{
			observer.SourceAgentsCLI: {ID: observer.SourceAgentsCLI, Stale: true, AgeMs: &age, LastError: &failure},
		},
	}
	healthy := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sessions: failing.Sessions,
		Sources: map[string]observer.SourceReport{
			observer.SourceAgentsCLI: {ID: observer.SourceAgentsCLI, OK: true},
		},
	}

	p, _, _ := openTestPanel(t, now)
	deliver(t, p, snapshotEvent(failing))
	if !strings.Contains(p.view(), "check failing") {
		t.Fatal("precondition: the failing baseline drew no footer")
	}
	deliver(t, p, observer.Event{Type: observer.EventSources, Snapshot: healthy})
	view := p.view()
	if strings.Contains(view, "check failing") {
		t.Fatalf("the footer outlived the sources event that cleared it:\n%s", view)
	}
	if !strings.Contains(view, "kept") {
		t.Fatalf("the sources event lost the sessions:\n%s", view)
	}
}

// ---------------------------------------------------------------------------
// the render tick — the pair P9 is gated on
// ---------------------------------------------------------------------------

// 90 minutes of ticks advance the displayed duration with NO new snapshot.
// This is the H5/§5.5(a) property: the server ships timestamps, the renderer
// owns elapsed time.
func TestNinetyMinutesOfTicksAdvanceTheAgeWithNoNewSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, src, clock := openTestPanel(t, now)

	live := true
	since := now.Add(-14 * time.Minute)
	started := now.Add(-3 * time.Hour)
	snap := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{
			SessionID: "s1", Name: "quiet", Status: observer.StatusWaiting,
			Attention: true, Live: &live, StatusSince: &since, StartedAt: &started,
		}},
	}
	deliver(t, p, snapshotEvent(snap))

	before := p.view()
	if !strings.Contains(before, "14m") {
		t.Fatalf("the first render does not show 14m:\n%s", before)
	}

	// Ninety minutes, and not one new snapshot: the same subscription, the
	// same stored state, only the clock moved.
	clock.advance(90 * time.Minute)
	if cmd := p.update(panelTickMsg{gen: p.gen}); cmd == nil {
		t.Fatal("a tick on a visible panel did not re-issue the tick")
	}

	after := p.view()
	if after == before {
		t.Fatalf("90 minutes of ticks did not change the rendered string:\n%s", after)
	}
	// The why line shows age:short, so 1h44m reads "waiting 1h" — the {age}
	// token still renders the long form, and a template can ask for it. The
	// badge's runtime moved with the same ticks: 3h00m → 4h30m.
	fl := parseFrame(plain(after))
	if under, ok := bandLines(fl, "quiet"); !ok || len(under) == 0 || under[0] != "waiting 1h" {
		t.Fatalf("the age did not advance to 1h (why %q):\n%s", under, after)
	}
	if r, ok := frameRow(fl, "quiet"); !ok || r.runtime != "4h30m" {
		t.Fatalf("the runtime did not advance to 4h30m (row %+v):\n%s", r, after)
	}
	if got := panelHumanise(panelAge(snap.Sessions[0], p.now)); got != "1h44m" {
		t.Fatalf("the long age is %q, want 1h44m", got)
	}
	waitSubscriptions(t, src, 1)
}

// And the other half: after toggle-off no tick is issued. Two assertions,
// because there are two mechanisms — the message is dropped without re-issuing
// (so the chain ends), and the command itself yields nothing once the context
// is cancelled (so an in-flight tick cannot restart it).
func TestNoTickIsIssuedOnceThePanelIsToggledOff(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, _, _ := openTestPanel(t, now)

	ctx, gen := p.ctx, p.gen
	if cmd := panelTick(ctx, gen); cmd == nil {
		t.Fatal("an open panel has no tick command")
	}

	p.toggle() // off
	if p.open {
		t.Fatal("the panel is still open after a second toggle")
	}
	if cmd := p.update(panelTickMsg{gen: gen}); cmd != nil {
		t.Fatal("a tick after toggle-off re-issued itself; the clock would run forever")
	}
	if msg := panelTick(ctx, gen)(); msg != nil {
		t.Fatalf("the tick command yielded %#v after the context was cancelled, want nil", msg)
	}
	if view := p.view(); view != "" {
		t.Fatalf("a closed panel renders %q, want nothing", view)
	}
}

// ---------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------

// Opening and closing the panel leaves no goroutine behind — the leak rule
// from golang.md §3.1, asserted rather than asserted-in-a-comment.
func TestOpeningAndClosingThePanelLeavesNoGoroutineBehind(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	settle(t)
	before := runtime.NumGoroutine()

	src := newFakePanelSource()
	p := newPanel(src, newPanelClock(now))
	for i := 0; i < 5; i++ {
		p.toggle() // on
		if !p.open {
			t.Fatal("the panel did not open")
		}
		p.toggle() // off
	}

	settle(t)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines went from %d to %d across five open/close cycles", before, after)
	}
	// One subscription per opening, and not one more: the source is decided
	// once at toggle-on and never re-probed in a loop.
	waitSubscriptions(t, src, 5)
}

// settle waits for goroutines started by a previous step to finish, so the
// leak assertion measures the panel rather than the scheduler.
func settle(t *testing.T) {
	t.Helper()
	base := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		runtime.Gosched()
		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // test-only settle
		if n := runtime.NumGoroutine(); n <= base {
			base = n
		}
	}
}

// A delta that arrives before any baseline is DROPPED, not applied to an empty
// snapshot. P7's live defect 4 found this on the socket and its log entry says
// "P9 must do the same thing".
func TestADeltaBeforeTheBaselineIsDroppedRatherThanAppliedToNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, _, _ := openTestPanel(t, now)

	live := true
	since := now.Add(-time.Minute)
	orphan := observer.Session{
		SessionID: "early", Name: "premature", Status: observer.StatusIdle,
		Live: &live, StatusSince: &since,
	}
	deliver(t, p, observer.Event{
		Type: observer.EventSessionAdded, SessionID: "early", Session: &orphan, At: now,
	})

	if p.snap != nil {
		t.Fatal("a delta with no baseline created a snapshot out of nothing")
	}
	if view := p.view(); strings.Contains(view, "premature") {
		t.Fatalf("the dropped delta was rendered:\n%s", view)
	}

	// The baseline that follows carries it, which is why dropping is safe.
	deliver(t, p, snapshotEvent(&observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{}, Sessions: []observer.Session{orphan},
	}))
	if view := p.view(); !strings.Contains(view, "premature") {
		t.Fatalf("the baseline does not carry the session:\n%s", view)
	}
}

// Deltas after a baseline upsert and remove, and they do not mutate the
// published snapshot they came from (published snapshots are immutable, §7.3).
func TestDeltasAfterTheBaselineUpsertAndRemoveWithoutMutatingThePublishedSnapshot(t *testing.T) {
	withPanelTheme(t, true) // the loud age is read from the colours
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, _, _ := openTestPanel(t, now)

	live := true
	since := now.Add(-time.Minute)
	base := observer.Session{
		SessionID: "a", Name: "one", Status: observer.StatusIdle,
		Live: &live, StatusSince: &since,
	}
	published := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(),
		Sources: map[string]observer.SourceReport{}, Sessions: []observer.Session{base},
	}
	deliver(t, p, snapshotEvent(published))

	changed := base
	changed.Status = observer.StatusWaiting
	changed.Attention = true
	deliver(t, p, observer.Event{
		Type: observer.EventSessionChanged, SessionID: "a", Session: &changed,
		Changed: []string{"status"}, At: now,
	})
	if r, ok := frameRow(parseStyledFrame(t, p.view()), "one"); !ok || r.status != "waiting" || r.tone != badgeAsk {
		t.Fatalf("the change was not applied (row %+v):\n%s", r, p.view())
	}
	if published.Sessions[0].Status != observer.StatusIdle {
		t.Fatal("applying a delta mutated the published snapshot")
	}

	deliver(t, p, observer.Event{
		Type: observer.EventSessionRemoved, SessionID: "a",
		Reason: observer.RemovedPIDDead, At: now,
	})
	if view := p.view(); strings.Contains(view, "one") {
		t.Fatalf("the removal was not applied:\n%s", view)
	}
}

// A source that cannot be subscribed to is a footer, not a dead panel.
func TestASourceThatRefusesToSubscribeBecomesAFooterLine(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	src := newFakePanelSource()
	src.err = stubErr("no observer here")
	p := newPanel(src, newPanelClock(now))
	p.width = panelWidth
	p.toggle()
	t.Cleanup(p.close)

	// The pump reports through the channel the panel is already pumping, so
	// the message is taken the same way an event is.
	select {
	case msg := <-p.msgs:
		p.update(msg)
	case <-time.After(time.Second):
		t.Fatal("the pump reported no error for a source that refused to subscribe")
	}
	if view := p.view(); !strings.Contains(view, "observer unavailable") {
		t.Fatalf("the refusal is not in the footer:\n%s", view)
	}
}

// ---------------------------------------------------------------------------
// rendering invariants that protect the existing TUI
// ---------------------------------------------------------------------------

// A closed panel, and an open one with no width, both render exactly nothing.
// This is what keeps model_test.go's View assertions byte-identical.
func TestAClosedOrZeroWidthPanelRendersNothing(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	closed := newPanel(newFakePanelSource(), newPanelClock(now))
	closed.width = panelWidth
	if got := closed.view(); got != "" {
		t.Fatalf("a closed panel rendered %q", got)
	}

	p, _, _ := openTestPanel(t, now)
	deliver(t, p, snapshotEvent(totalityFixture(now)))
	if got := p.view(); got == "" {
		t.Fatal("an open panel with width rendered nothing")
	}
	p.width = 0
	if got := p.view(); got != "" {
		t.Fatalf("a zero-width panel rendered %q", got)
	}
}

// The model's View is unchanged while the panel is closed, and the list keeps
// the whole width. Asserted against the body the model rendered before P9.
func TestTheModelViewAndListWidthAreUnchangedWhileThePanelIsClosed(t *testing.T) {
	m := newTestModel(
		session{name: "one", windows: 1},
		session{name: "two", windows: 2},
	)
	if got, want := m.View(), m.viewBody(); got != want {
		t.Fatalf("View with the panel closed differs from the body:\n got %q\nwant %q", got, want)
	}
	if w := m.list.Width(); w != 100 {
		t.Fatalf("the list is %d columns wide with the panel closed, want the full 100", w)
	}
}

// Toggling the panel splits the width and gives it back. ctrl+o is dispatched
// through the real key path, so this also proves the chord reaches the panel
// rather than the search box.
func TestCtrlOSplitsTheWidthAndGivesItBack(t *testing.T) {
	stubSeam(t, &openPanelSource, func(ctx context.Context, _ observer.Clock) (panelSource, string, error) {
		return newFakePanelSource(), "", nil
	})

	m := newTestModel(session{name: "one", windows: 1})
	m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	t.Cleanup(m.panel.close)

	if !m.panel.open {
		t.Fatal("ctrl+o did not open the panel")
	}
	if m.panel.width != panelWidth {
		t.Fatalf("the panel is %d columns, want %d", m.panel.width, panelWidth)
	}
	if want := 100 - panelWidth; m.list.Width() != want {
		t.Fatalf("the list is %d columns with the panel open, want %d", m.list.Width(), want)
	}

	m, _ = applyMsg(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.panel.open {
		t.Fatal("a second ctrl+o did not close the panel")
	}
	if m.list.Width() != 100 {
		t.Fatalf("the list is %d columns after closing, want the full 100 back", m.list.Width())
	}
}

// A terminal too narrow to split gets no panel at all: a 46-column panel
// beside a 14-column list is two unusable surfaces.
func TestTheNarrowTerminalGetsNoPanelColumns(t *testing.T) {
	for _, tc := range []struct{ total, want int }{
		{0, 0}, {40, 0}, {59, 0}, {60, 30}, {80, 40}, {92, 46}, {200, 46},
	} {
		if got := panelColumns(tc.total); got != tc.want {
			t.Errorf("panelColumns(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// units
// ---------------------------------------------------------------------------

func TestHumanisedDurationsUseTheCoarseUnitsThePanelPromises(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "—"}, // no statusSince: never a fabricated zero
		{0, "0s"},
		{12 * time.Second, "12s"},
		{time.Minute, "1m"},
		{14*time.Minute + 3*time.Second, "14m"},
		{104 * time.Minute, "1h44m"},
		{25 * time.Hour, "1d01h"},
	} {
		if got := panelHumanise(tc.in); got != tc.want {
			t.Errorf("panelHumanise(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// panelTruncate counts DISPLAY COLUMNS (§1.2): a CJK glyph is two, so the old
// rune count let "日本語…" take 7 columns of a 4-column budget. ASCII results
// are byte-identical to the old ones.
func TestTruncationCountsDisplayColumnsAndMarksWhatItCut(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{
		{"madbench", 20, "madbench"},
		{"madbench", 8, "madbench"},
		{"madbench", 4, "mad…"},
		{"日本語のセッション", 4, "日…"},
		{"日本語のセッション", 5, "日本…"},
		{"🚀🚀🚀", 4, "🚀…"},
		{"x", 0, ""},
		{"xyz", 1, "…"},
	} {
		got := panelTruncate(tc.in, tc.width)
		if got != tc.want {
			t.Errorf("panelTruncate(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
		if w := ansi.StringWidth(got); w > tc.width {
			t.Errorf("panelTruncate(%q, %d) is %d columns wide", tc.in, tc.width, w)
		}
	}
}

// ---------------------------------------------------------------------------
// the adaptive tick: wake when a displayed age can change, not every second
// ---------------------------------------------------------------------------

// untilAgeChanges is the whole schedule, and its boundaries are the ones
// panelHumanise renders: seconds under a minute, minutes under a day, hours.
func TestTheTickWaitsExactlyUntilTheDisplayedAgeChanges(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want time.Duration
	}{
		{0, time.Second},
		{1500 * time.Millisecond, 500 * time.Millisecond},
		{59*time.Second + 500*time.Millisecond, 500 * time.Millisecond}, // "59s" -> "1m"
		{time.Minute, time.Minute},
		{14*time.Minute + 20*time.Second, 40 * time.Second},
		{23*time.Hour + 59*time.Minute + 30*time.Second, 30 * time.Second}, // -> "1d00h"
		{24 * time.Hour, time.Hour},
		{3*24*time.Hour + 10*time.Minute, 50 * time.Minute},
	} {
		if got := untilAgeChanges(tc.age); got != tc.want {
			t.Errorf("untilAgeChanges(%s) = %s, want %s", tc.age, got, tc.want)
		}
		// And the rendered string really is unchanged just before, and changed
		// at, the computed instant.
		d := untilAgeChanges(tc.age)
		if tc.age+d-time.Millisecond > tc.age && panelHumanise(tc.age+d-time.Millisecond) != panelHumanise(tc.age) {
			t.Errorf("age %s: the string changed BEFORE the computed tick", tc.age)
		}
		if panelHumanise(tc.age+d) == panelHumanise(tc.age) {
			t.Errorf("age %s: the string did not change AT the computed tick (%s)", tc.age, d)
		}
	}
}

// A panel whose youngest row is 14 minutes old re-arms for the next minute
// boundary, not for the next second — the fix for scope="all"'s idle cost.
func TestAQuietPanelTicksOncePerDisplayedChangeNotEverySecond(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, _, _ := openTestPanel(t, now)
	live := true
	since := now.Add(-14*time.Minute - 20*time.Second)
	deliver(t, p, snapshotEvent(&observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{SessionID: "s1", Name: "quiet", Status: observer.StatusIdle, Live: &live, StatusSince: &since}},
	}))
	if got := p.nextTick(); got != 40*time.Second {
		t.Fatalf("nextTick = %s, want 40s (the next minute boundary of a 14m20s age)", got)
	}
	if p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq}) == nil {
		t.Fatal("a tick of the current chain did not re-arm")
	}
	if !p.tickDue.Equal(p.now.Add(40 * time.Second)) {
		t.Fatalf("the next tick is due %s after now, want 40s", p.tickDue.Sub(p.now))
	}

	// No rows with an age: the ceiling, not a busy clock.
	p.snap = &observer.Snapshot{}
	if got := p.nextTick(); got != panelTickCeiling {
		t.Fatalf("an empty panel ticks every %s, want the %s ceiling", got, panelTickCeiling)
	}
}

// An event that brings in a row whose age changes SOONER starts a new chain,
// and the old chain's tick is dropped when it fires: never two chains.
func TestAYoungerRowRearmsTheTickAndTheOldChainIsDropped(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p, _, clock := openTestPanel(t, now)
	live := true
	old := now.Add(-3 * time.Hour)
	base := &observer.Snapshot{
		ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{SessionID: "old", Name: "old", Status: observer.StatusIdle, Live: &live, StatusSince: &old}},
	}
	deliver(t, p, snapshotEvent(base))
	// Let the first (1 s) tick fire so the schedule is on its long interval.
	clock.advance(time.Second)
	p.update(panelTickMsg{gen: p.gen, seq: p.tickSeq})
	chain := p.tickSeq

	fresh := p.now
	cmd := p.update(panelEventMsg{gen: p.gen, ev: observer.Event{
		Type: observer.EventSessionAdded, SessionID: "new",
		Session: &observer.Session{SessionID: "new", Name: "new", Status: observer.StatusBusy, Live: &live, StatusSince: &fresh},
	}})
	if cmd == nil {
		t.Fatal("the event re-armed nothing")
	}
	if p.tickSeq != chain+1 {
		t.Fatalf("tickSeq %d, want a new chain %d", p.tickSeq, chain+1)
	}
	if got := p.tickDue.Sub(p.now); got != time.Second {
		t.Fatalf("the new chain fires in %s, want 1s for a 0s-old row", got)
	}
	if p.update(panelTickMsg{gen: p.gen, seq: chain}) != nil {
		t.Fatal("the superseded chain's tick re-armed itself: two clocks would run")
	}
}

// A row fills the panel's width exactly when its name has to be cut: the name
// gets every column the status and age leave. An unknown age renders as "—",
// three bytes and one column, and measuring it in bytes cut such names two
// columns short of the edge.
func TestALongNameIsCutAtThePanelsEdgeWhateverTheAgeLooksLike(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p := newPanel(nil, newPanelClock(now))
	p.width, p.now = panelWidth, now
	long := strings.Repeat("n", 80)
	since := now.Add(-3 * time.Minute)
	for _, s := range []observer.Session{
		{SessionID: "unknown-age", Name: long, Status: observer.StatusIdle},
		{SessionID: "known-age", Name: long, Status: observer.StatusIdle, StatusSince: &since},
	} {
		if got := lipgloss.Width(p.rowHead(s, "")); got != p.width {
			t.Errorf("%s: row is %d columns wide, want the panel's %d", s.SessionID, got, p.width)
		}
	}
}

// The failing-check footer offers a retry only where there is one: the
// sidebar binds r, and the TUI's r is a search character.
func TestTheRefreshHintAppearsOnlyWhereRefreshIsBound(t *testing.T) {
	age := int64(5 * time.Minute / time.Millisecond)
	failure := "exit status 1"
	p := newPanel(nil, newPanelClock(time.Now()))
	p.snap = &observer.Snapshot{Sources: map[string]observer.SourceReport{
		observer.SourceAgentsCLI: {Stale: true, AgeMs: &age, LastError: &failure},
	}}
	if note := p.footerNote(); note == "" || strings.Contains(note, "retry") {
		t.Errorf("the TUI's footer offers a retry it has no key for, or none at all: %q", note)
	}
	p.surface = surfacePane
	if note := p.footerNote(); !strings.Contains(note, "r to retry") {
		t.Errorf("the sidebar's footer lost its retry hint: %q", note)
	}
}

// rowHead is one non-band row's first line, drawn from the panel's row
// template (the built-in one unless panel.conf says otherwise), for tests that
// assert on a single row. label names the row's group in the line itself —
// the real group of a row folded into "other" — and "" names none. The layout
// draws from the pre-rendered panelRow.lines (panel_rows.go); this is the same
// rendering for one row.
func (p panel) rowHead(s observer.Session, label string) string {
	set := p.confSet()
	r := panelRow{s: s, class: panelGroupOf(s), label: label}
	return p.renderRow(set, &r, p.rowContext(set, nil)).head
}
