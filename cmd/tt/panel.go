package main

// panel.go is S4's TUI panel (§5.5a): a toggled (ctrl+o) column beside the
// session list that shows what every Claude Code session on this machine is
// doing right now.
//
// It lives in package main rather than in internal/ for the reason §5.5(a)
// gives: it needs theme.go's adaptive palette and it has to compose with the
// existing model. It is a driving adapter living in the composition root — the
// one place where "adapter" and "composition root" coincide, which is normal
// for a TUI.
//
// Three things here are load-bearing and each one is a rule from elsewhere in
// the design:
//
//  1. THE RENDERING CONTRACT IS TOTAL (H11). Every one of the nine statuses,
//     plus `live: null` and `live: false`, lands in exactly one group. A
//     session the panel cannot classify would be a session that silently
//     disappears from the only always-visible surface, which is the failure
//     H11 was written about.
//
//  2. THE PANEL OWNS ELAPSED TIME, THE SERVER OWNS TIMESTAMPS. It never
//     renders the precomputed statusAgeMs — that value froze at the instant of
//     the snapshot, so a quiet waiting session would display "14m" forever
//     while `tt sessions --json` fetched a minute later said 15m. Instead it
//     stores the snapshot and humanises `now - statusSince` against its own
//     clock, advanced by a tick that runs ONLY while the panel is open. That
//     tick is the single clock in this design that ticks (the user's override
//     of D3 permits it explicitly: "a visible TUI panel may tick its own clock
//     while it is on screen"), and toggle-off cancels the context so no further
//     tick is ever issued.
//
//     The tick fires when a displayed age can next CHANGE, not every second:
//     ages under a minute show seconds, under a day minutes, beyond that
//     hours, so a panel whose youngest row is 14 minutes old wakes once a
//     minute (capped at a minute, and never sooner than a second). At
//     scope="all" there is one panel per window, visible or not, so a
//     per-second clock in each is a cost nobody is looking at. (Measured: the
//     larger share of an idle pane's CPU was bubbletea's 60 fps renderer, not
//     this tick — pane_paint.go's painter is that fix; this one removes the other
//     59 of every 60 wake-ups.) An event that brings in a younger row re-arms
//     the tick sooner, and a superseded tick is dropped by its sequence number
//     rather than left to run as a second chain.
//
//  3. NOTHING IS RENDERED AT ZERO WIDTH. The panel is a column, and a column
//     of zero columns is nothing at all. That is also what keeps the existing
//     model_test.go View assertions byte-identical: a closed or unsized panel
//     contributes no bytes.
//
// # The data source, and what happens when it stops
//
// The panel depends on ONE narrow port, panelSource, which is the Subscribe
// half of observer.SessionSource. §7.5(a)'s rule is that a driving adapter
// cannot tell which implementation it holds, so there is no `if attached` in
// this file: the local watch loop and the pane's socket source (panesource.go)
// are the same thing from here.
//
// A subscription delivers a snapshot baseline and then deltas. P7's live defect
// 4 found that a consumer attaching before the first publication receives
// deltas with no baseline — its note says "P9 must do the same thing", and this
// is that: a delta arriving before any baseline is DROPPED rather than applied
// to an empty snapshot, because the baseline that follows already contains
// everything those deltas would have said.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
)

// panelWidth is the column count the panel occupies when open. It matches
// @tt_obs_width's default (§5.5b2) so the in-tt panel and the tmux side pane
// are the same width, and a layout learned in one reads the same in the other.
const panelWidth = 46

// panelTickInterval is the SHORTEST tick: one second is the finest unit the
// panel displays (an age under a minute). It is also the first tick after
// opening, before there is a snapshot to compute the next change from.
const panelTickInterval = time.Second

// panelTickCeiling is the LONGEST tick. An hour-granular age would allow an
// hour, but a minute bounds how stale any row can get after something the
// schedule did not see coming (revealing the ENDED group, say), at one wake-up
// a minute.
const panelTickCeiling = time.Minute

// panelBuffer is how many messages the pump may run ahead of the update loop.
// The pump blocks rather than drops when it is full, and toggle-off cancels the
// context it blocks in, so a full buffer costs latency and never a leak.
const panelBuffer = 64

// keyPanel toggles the panel. ctrl+o is free against the existing chord set
// (ctrl+t/r/x/s/p/n/c), and ctrl+a is the tmux prefix and never arrives here.
const keyPanel = "ctrl+o"

// keyPanelRefresh asks the source to re-read. It is §5.5(a)'s plain letter
// and belongs to `tt observe --pane`, which has no session list: in the TUI
// every printable key is a search character, so refresh is left to the pane
// program, which is the surface the degraded "press r to refresh" footer
// actually appears on.
//
// There is no ENDED toggle any more (the user, 2026-10-01: "we don't need
// them"): a session that finished or whose process is gone is not drawn at
// all. `tt sessions --json --all` still lists it.
const (
	keyPanelRefresh = "r"
	// keyPanelMode and keyPanelModeTUI cycle the grouping (panel_modes.go):
	// a plain letter in the sidebar, the same letter as a chord in the TUI. g is Codex's agent command center's
	// key for the same verb. ctrl+s would have matched `claude agents`, but it
	// is tt's restore, and on the empty state it rebuilds the workspace.
	keyPanelMode    = "g"
	keyPanelModeTUI = "ctrl+g"
	// keyScrollDown and keyScrollUp move the sidebar's scroll area a line
	// (panel_scroll.go), as in less and vim; the arrows do too.
	keyScrollDown = "j"
	keyScrollUp   = "k"
)

// panelSource is everything the panel needs from the observer: a subscription
// that starts with a baseline and continues with deltas.
//
// It is deliberately NARROWER than observer.SessionSource. The panel never
// calls Snapshot (the subscription's first event IS the snapshot) and never
// Refresh, so a port carrying those would be methods a test double has to
// implement in order to lie about. Enrichment is the optional panelEnricher.
type panelSource interface {
	Subscribe(ctx context.Context, f observer.Filter) (<-chan observer.Event, error)
}

// panelEnricher is the optional half a source implements when the stream it
// delivers is NOT enriched — the local watch loop's (pane.go: localStream),
// whose rebuilds never read a transcript. The panel renders three transcript
// facts on every row (the agent colour as the accent, the subagent count, the
// API-error count), so the pump asks for them (code review round 2, A11).
//
// A source whose stream is already enriched does not implement it: the pane's
// socket source asks the SERVER for an enriched stream instead
// (panesource.go), which enriches each changed session once for the
// connection rather than once per render.
type panelEnricher interface {
	Enrich(ctx context.Context, snap *observer.Snapshot, ids []string) (*observer.Snapshot, error)
}

// panelRefresher is the optional half: a source that can be asked to re-read
// now. The local watch loop can (Rebuild); a degraded on-demand source is the
// case §5.5(a) writes the "press r to refresh" footer for. Sources that cannot
// are used through panelSource alone and `r` is inert.
type panelRefresher interface {
	Rebuild(ctx context.Context, trigger string) (*observer.Snapshot, observer.ArmReport, error)
}

// The messages the panel puts into the update loop. Every one carries the
// generation it belongs to: a message already in flight when the panel closes
// must not be applied to the next opening, which is the same stale-reference
// hazard C1 was about, one process in.
type (
	panelEventMsg struct {
		gen int
		ev  observer.Event
	}
	panelErrMsg struct {
		gen  int
		note string
	}
	// panelTickMsg carries the tick chain it belongs to: an event that needs
	// an earlier tick starts a new chain (seq+1), and the old one is dropped
	// when it fires instead of re-issuing itself as a second chain.
	panelTickMsg      struct{ gen, seq int }
	panelRefreshedMsg struct {
		gen  int
		note string
	}
)

// panel is the panel's whole state. It is a value on the model, like every
// other piece of the TUI, and the two things that cannot be values — the
// subscription's context and the message channel — are held as a cancel func
// and a channel, both replaced on every open.
type panel struct {
	open   bool
	width  int
	height int

	// src is an INJECTED source (the pane program wires one; tests wire a
	// double). When it is nil the source is opened at toggle-on through
	// openPanelSource, which is §5.5(a)'s "decided once at panel OPEN".
	src panelSource
	// active is the source actually in use for this opening, whichever way it
	// arrived. Nothing in this file branches on which one it is (§7.5a).
	active panelSource
	clock  observer.Clock

	// ctx and cancel are the subscription's lifetime. cancel is called on
	// toggle-off and is what stops both the pump goroutine and the re-issuing
	// tick/wait commands — the goroutine-leak rule from golang.md §3.1.
	ctx    context.Context
	cancel context.CancelFunc
	msgs   chan tea.Msg

	// gen is incremented on every open, so a message from a previous opening
	// can be recognised and dropped.
	gen int

	// snap is the last baseline plus every delta applied to it, or nil when no
	// baseline has arrived yet. A nil snap is "waiting", never "empty".
	snap *observer.Snapshot
	// now is the instant the durations are rendered against. It moves on every
	// tick and on every event, and it is the ONLY clock read this panel makes.
	now time.Time

	// tickSeq is the current tick chain and tickDue when it next fires.
	tickSeq int
	tickDue time.Time
	// note is the one-line footer: the watch mode, a degradation, or an error.
	note string

	// surface is where the panel is drawn. The two surfaces bind different
	// keys to the same verbs — a plain letter in the pane, a chord in the TUI,
	// whose printable keys all belong to the search box — so every hint the
	// panel prints has to name the key of the surface it is on.
	surface panelSurface
	// policy is the liveness policy the render clock re-derives with: the
	// resolved one, with this machine's transcript retention, not the default.
	policy observer.Policy

	// mode is how the sessions are grouped (panel_modes.go).
	mode panelMode
	// scroll is the scroll area's offset in lines (j/k in the sidebar), kept
	// within what the last frame could scroll (panel_scroll.go).
	scroll int
	// filter narrows the rows to the sessions it matches (/ in the sidebar);
	// filtering is true while its prompt takes the keys.
	filter    string
	filtering bool
	// picking is the "show only" picker open (`f`, panel_pick.go);
	// pickSection (with its title) and pickState are the section and state
	// filters it set, which combine.
	picking     bool
	pickSection string
	pickTitle   string
	pickState   stripState
	pickStateOn bool
	// blurred is the sidebar's pane not holding the focus (the user,
	// 2026-10-01: show when the panel is in focus). Focused, the chrome is
	// lit: the active tab filled, the baseline under it and the key strip's
	// ══ in the accent. At rest the tab keeps only its accent text and the
	// rest falls back to the rule colour. Only the pane reports focus; the
	// TUI column and panel-render are always lit.
	blurred bool
	// cursor is the highlighted row's session (panel_cursor.go), "" before
	// the first move; cursorAt is the index it was last drawn at, where it
	// lands when its session leaves the list.
	cursor   string
	cursorAt int
	// clickedRow is whether the last clickAt landed on a session row.
	clickedRow bool
	// navigable is a live sidebar, the one surface with keys to move a
	// cursor with: tt panel-render's drawing and the TUI column have none, so
	// they draw no cursor.
	navigable bool
	// memo is what must outlive this copy of the panel: the last frame, the
	// collapse decisions it carries into the next, and the project cache.
	memo *panelMemo
}

// panelSurface is the program a panel is drawn in.
type panelSurface int

const (
	// surfaceTUI is the panel column inside the full-screen tt.
	surfaceTUI panelSurface = iota
	// surfacePane is `tt observe --pane`, the sidebar.
	surfacePane
)

// newPanel builds a closed panel over a source. A nil source is legal and
// means "this build could not wire one": the panel then opens, says so in its
// footer, and renders nothing else — which is a better answer than a toggle
// that appears to do nothing.
func newPanel(src panelSource, clock observer.Clock) panel {
	mode, _ := loadPanelMode()
	p := panel{src: src, clock: clock, policy: panelPolicy(), mode: mode, memo: newPanelMemo()}
	// The first frame draws with the user's panel.conf, not with the built-in
	// format for one frame and the file's from the next update (R6). The
	// sidebar's newPaneModel builds its panel here too.
	p.followConfig()
	return p
}

// panelPolicy resolves the liveness policy once per process. It is a seam so
// tests do not read the real ~/.claude/settings.json.
var panelPolicy = sync.OnceValue(func() observer.Policy {
	paths, err := claudePaths()
	if err != nil {
		return observer.DefaultPolicy()
	}
	return resolvePolicy(paths)
})

// toggle opens or closes the panel and returns the commands that make it move.
//
// Opening starts exactly two repeating commands: the tick, and the wait that
// pumps the subscription into the update loop. Closing cancels the context
// both of them select on, which is what makes "no tick is issued after
// toggle-off" a property of the code rather than a promise.
func (p *panel) toggle() tea.Cmd {
	if p.open {
		p.close()
		return nil
	}
	return p.start()
}

// start opens the panel and subscribes.
func (p *panel) start() tea.Cmd {
	p.open = true
	p.gen++
	p.snap = nil
	p.note = ""
	p.now = p.clockNow()
	// A panel.conf edited while the panel was closed applies on its first
	// frame after reopening.
	p.followConfig()

	ctx, cancel := context.WithCancel(context.Background())

	src := p.src
	if src == nil {
		opened, note, err := openPanelSource(ctx, p.clock)
		if err != nil {
			// Opening the source is the one decision §5.5(a) makes at
			// toggle-on, and a failure here is a footer rather than a dead
			// panel: the context is released immediately so nothing ticks over
			// a panel that has no data to re-render.
			cancel()
			p.note = "observer unavailable: " + err.Error()
			return nil
		}
		src, p.note = opened, note
	}

	p.ctx, p.cancel = ctx, cancel
	p.active = src
	p.msgs = make(chan tea.Msg, panelBuffer)
	go panelPump(ctx, src, p.gen, p.msgs)

	p.tickSeq, p.tickDue = 0, p.now.Add(panelTickInterval)
	return tea.Batch(panelTick(ctx, p.gen), waitPanelMsg(ctx, p.msgs))
}

// close cancels the subscription and forgets the state it produced.
func (p *panel) close() {
	p.open = false
	if p.cancel != nil {
		p.cancel()
	}
	p.ctx, p.cancel, p.msgs = nil, nil, nil
	p.active = nil
	p.snap = nil
	p.note = ""
}

// refresh is the `r` verb: ask the source to re-read now. It is a no-op on a
// source that cannot (see panelRefresher), because a key that appears to do
// something and does not is worse than one that is not bound.
func (p *panel) refresh() tea.Cmd {
	if !p.open || p.active == nil {
		return nil
	}
	return panelRefreshCmd(p.ctx, p.active, p.gen)
}

// clockNow reads the injected clock, or falls back to the domain's own
// SystemClock when the panel was built without one (which only a test can do).
func (p *panel) clockNow() time.Time {
	if p.clock == nil {
		return observer.SystemClock{}.Now()
	}
	return p.clock.Now()
}

// panelPump forwards one subscription into the update loop.
//
// It is the only goroutine the panel starts. Every send selects on ctx.Done,
// so a cancelled panel cannot leave it parked on a channel nobody reads.
//
// When the source's stream is unenriched (panelEnricher) the pump enriches it
// HERE, off the update loop: a first pass over a cold cache reads up to 200 KB
// per transcript (measured on this machine: 45 sessions in 0.3–0.8 s), and
// that must never freeze the TUI's keys. A baseline is forwarded twice — as
// it arrived, so the rows appear at once, then enriched — and an added or
// changed session is enriched before it is forwarded, which is one cached
// lookup when its transcript has not grown. Events stay in order because this
// one goroutine handles them one at a time.
func panelPump(ctx context.Context, src panelSource, gen int, out chan<- tea.Msg) {
	// IncludeDead (code review round 2, A10, core): a session that ends while
	// the panel is open arrives as a change it keeps — in ENDED — rather than as
	// a removal that loses it.
	ch, err := src.Subscribe(ctx, observer.Filter{IncludeDead: true})
	if err != nil {
		panelSend(ctx, out, panelErrMsg{gen: gen, note: "observer unavailable: " + err.Error()})
		return
	}
	enricher, _ := src.(panelEnricher)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// The stream ended with the panel still open. The pane's socket
				// source never ends its stream early (it waits for a server
				// instead), so this is a local watch loop that stopped, which
				// the footer says rather than hides.
				panelSend(ctx, out, panelErrMsg{gen: gen, note: "observer stopped — press r to refresh"})
				return
			}
			if enricher == nil {
				panelSend(ctx, out, panelEventMsg{gen: gen, ev: ev})
				continue
			}
			if ev.Snapshot != nil && (ev.Type == observer.EventSnapshot || ev.Type == observer.EventResync) {
				panelSend(ctx, out, panelEventMsg{gen: gen, ev: ev})
				if enriched, ok := enrichPanelEvent(ctx, enricher, ev); ok {
					panelSend(ctx, out, panelEventMsg{gen: gen, ev: enriched})
				}
				continue
			}
			enriched, _ := enrichPanelEvent(ctx, enricher, ev)
			panelSend(ctx, out, panelEventMsg{gen: gen, ev: enriched})
		}
	}
}

// enrichPanelEvent fills in the transcript facts an event's sessions carry:
// the whole snapshot of a baseline, the one session of an addition or a
// change. It reports whether it changed the event. Removals, source changes
// and degradations carry no session to enrich and pass through.
//
// It never mutates what it was given: the snapshot is the published one
// (immutable, §7.3), and Enrich returns a copy. A failed or cancelled
// enrichment returns the event unchanged — the row without its colour is
// still the complete status answer.
func enrichPanelEvent(ctx context.Context, e panelEnricher, ev observer.Event) (observer.Event, bool) {
	switch ev.Type {
	case observer.EventSnapshot, observer.EventResync:
		if ev.Snapshot == nil {
			return ev, false
		}
		out, err := e.Enrich(ctx, ev.Snapshot, nil)
		if err != nil || out == nil {
			return ev, false
		}
		ev.Snapshot = out
		return ev, true
	case observer.EventSessionAdded, observer.EventSessionChanged:
		if ev.Session == nil {
			return ev, false
		}
		one := &observer.Snapshot{ObservedAt: ev.At, Sessions: []observer.Session{*ev.Session}}
		out, err := e.Enrich(ctx, one, nil)
		if err != nil || out == nil || len(out.Sessions) != 1 {
			return ev, false
		}
		s := out.Sessions[0]
		ev.Session = &s
		return ev, true
	}
	return ev, false
}

// panelSend delivers one message unless the panel closed first.
func panelSend(ctx context.Context, out chan<- tea.Msg, msg tea.Msg) {
	select {
	case out <- msg:
	case <-ctx.Done():
	}
}

// waitPanelMsg reads ONE message and is re-issued by the update loop, which is
// the supported way to pump a channel into bubbletea v1: a value-receiver
// model cannot call p.Send, and main.go owns the *tea.Program.
//
// It returns a nil message when the panel closed, and bubbletea ignores nil —
// so cancelling the context ends the chain instead of leaving a command parked
// on a channel forever.
func waitPanelMsg(ctx context.Context, in <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-in:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

// panelTick is the render clock's first tick of an opening (chain 0, one
// second). It is armed only while the panel is open and it is re-issued from
// the update loop, so cancelling the context stops it after at most one
// interval and nothing re-arms it.
func panelTick(ctx context.Context, gen int) tea.Cmd {
	return panelTickAfter(ctx, gen, 0, panelTickInterval)
}

// panelTickAfter is one tick of chain seq, d from now.
func panelTickAfter(ctx context.Context, gen, seq int, d time.Duration) tea.Cmd {
	if ctx == nil {
		return nil
	}
	return tea.Tick(d, func(time.Time) tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		return panelTickMsg{gen: gen, seq: seq}
	})
}

// nextTick is how long until any age or runtime this panel displays can
// change, clamped to [panelTickInterval, panelTickCeiling]. Rows in a hidden
// ENDED group display neither and are skipped. untilAgeChanges' units (a
// second under a minute, a minute under a day) are never coarser than
// fmtRuntime's, so it serves the runtime too; no second timer is involved.
func (p *panel) nextTick() time.Duration {
	best := panelTickCeiling
	if p.snap != nil {
		for _, s := range p.snap.Sessions {
			if panelGroupOf(s) == groupEnded {
				continue // not drawn
			}
			for _, d := range [...]time.Duration{panelAge(s, p.now), panelRuntime(s, p.now)} {
				if d < 0 {
					continue
				}
				if next := untilAgeChanges(d); next < best {
					best = next
				}
			}
		}
	}
	if best < panelTickInterval {
		best = panelTickInterval
	}
	return best
}

// untilAgeChanges is how long until panelHumanise(age) renders differently:
// the next whole second under a minute, the next whole minute under a day,
// the next whole hour beyond.
func untilAgeChanges(age time.Duration) time.Duration {
	unit := time.Hour
	switch {
	case age < time.Minute:
		unit = time.Second
	case age < 24*time.Hour:
		unit = time.Minute
	}
	return unit - age%unit
}

// rearm starts a new, EARLIER tick chain when the schedule needs one — after
// an event brought in a row whose age changes sooner than the pending tick —
// and returns nil when the pending tick is soon enough.
func (p *panel) rearm() tea.Cmd {
	d := p.nextTick()
	due := p.now.Add(d)
	if !due.Before(p.tickDue) {
		return nil
	}
	p.tickSeq++
	p.tickDue = due
	return panelTickAfter(p.ctx, p.gen, p.tickSeq, d)
}

// panelRefreshFailed opens the note a failed `r` leaves in the footer.
const panelRefreshFailed = "refresh failed: "

// panelRefreshCmd asks a refreshable source to re-read now. It is `r`.
func panelRefreshCmd(ctx context.Context, src panelSource, gen int) tea.Cmd {
	r, ok := src.(panelRefresher)
	if !ok || ctx == nil {
		return nil
	}
	return func() tea.Msg {
		if _, _, err := r.Rebuild(ctx, "panel-refresh"); err != nil {
			return panelRefreshedMsg{gen: gen, note: panelRefreshFailed + err.Error()}
		}
		return panelRefreshedMsg{gen: gen}
	}
}

// update applies one panel message and returns whatever command keeps the
// panel alive. A message from a previous generation is dropped and re-issues
// nothing, which is how a closed panel goes quiet.
func (p *panel) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case panelEventMsg:
		if !p.live(msg.gen) {
			return nil
		}
		p.apply(msg.ev)
		p.now = p.clockNow()
		// Every observer event is also a chance to follow a mode another
		// surface saved: in a busy fleet events come far more often than the
		// idle tick's one minute, and the read is one small file. panel.conf
		// is followed the same way: one stat, and a read only when it moved.
		p.followSavedMode()
		p.followConfig()
		if tick := p.rearm(); tick != nil {
			return tea.Batch(waitPanelMsg(p.ctx, p.msgs), tick)
		}
		return waitPanelMsg(p.ctx, p.msgs)

	case panelErrMsg:
		if !p.live(msg.gen) {
			return nil
		}
		p.note = msg.note
		return waitPanelMsg(p.ctx, p.msgs)

	case panelRefreshedMsg:
		if !p.live(msg.gen) {
			return nil
		}
		if msg.note != "" {
			p.note = msg.note
		} else if strings.HasPrefix(p.note, panelRefreshFailed) {
			// A refresh that worked retires the failure an earlier one left.
			p.note = ""
		}
		return nil

	case panelTickMsg:
		if !p.live(msg.gen) {
			// The tick is dropped rather than re-issued. This is the assertion
			// in the render-tick pair's second half: after toggle-off no
			// further tick exists.
			return nil
		}
		if msg.seq != p.tickSeq {
			// A chain an event superseded: dropped, so there is only ever one.
			return nil
		}
		p.now = p.clockNow()
		p.followSavedMode()
		p.followConfig()
		d := p.nextTick()
		p.tickDue = p.now.Add(d)
		return panelTickAfter(p.ctx, p.gen, p.tickSeq, d)
	}
	return nil
}

// live reports whether a message belongs to the current opening.
func (p *panel) live(gen int) bool { return p.open && gen == p.gen }

// apply folds one event into the panel's local snapshot.
//
// The baseline rule is P7 live defect 4's, carried into this surface as its
// log entry requires: a delta that arrives before any baseline is dropped. The
// snapshot that follows is the published state and already contains everything
// the dropped deltas would have said.
func (p *panel) apply(ev observer.Event) {
	switch ev.Type {
	case observer.EventSnapshot, observer.EventResync:
		p.snap = ev.Snapshot
		// A source may describe its own state in Reason, and this is the one
		// place that reads it: the pane's socket source puts "via tt observe
		// (pid N)" on each connection's baseline, and "observer not running -
		// start `tt observe`" on a resync that carries NO snapshot, i.e. when
		// there is nothing to show and the panel should say why. No other
		// source sets Reason on a baseline, and a resync that does carry a
		// snapshot (sockclient's server-side resync) leaves the note alone.
		if ev.Reason != "" && (ev.Type == observer.EventSnapshot || ev.Snapshot == nil) {
			p.note = ev.Reason
		}
		return
	case observer.EventSources:
		// A source's health changed. Only the reports are taken: the sessions
		// stay as the deltas left them. Dropping this event froze each sidebar
		// on the source state of the moment it connected (2026-10-21: one
		// window said "corroboration 1m old" for good, its neighbours nothing).
		if p.snap != nil && ev.Snapshot != nil {
			next := *p.snap
			next.Sources = ev.Snapshot.Sources
			p.snap = &next
		}
		return
	}

	if p.snap == nil || ev.SessionID == "" {
		return
	}

	// Copy before mutating: the snapshot came from an atomic pointer and is
	// immutable by contract (§7.3). The sessions slice is the only thing that
	// changes, so only it is copied.
	next := *p.snap
	next.Sessions = append([]observer.Session(nil), p.snap.Sessions...)

	switch ev.Type {
	case observer.EventSessionAdded, observer.EventSessionChanged:
		if ev.Session == nil {
			return
		}
		// Identity is (sessionId, kind) — code review round 2, A14 (core).
		replaced := false
		for i := range next.Sessions {
			if next.Sessions[i].SessionID == ev.SessionID && next.Sessions[i].Kind == ev.Session.Kind {
				next.Sessions[i] = *ev.Session
				replaced = true
				break
			}
		}
		if !replaced {
			next.Sessions = append(next.Sessions, *ev.Session)
		}
	case observer.EventSessionRemoved:
		kept := next.Sessions[:0]
		for _, s := range next.Sessions {
			if s.SessionID != ev.SessionID || (ev.Kind != "" && s.Kind != ev.Kind) {
				kept = append(kept, s)
			}
		}
		next.Sessions = kept
	case observer.EventWatchDegraded:
		p.note = "watch degraded: " + ev.Reason
		return
	default:
		return
	}

	p.snap = &next
}

// panelConnected is the at-rest footer's word for a live feed.
const panelConnected = "connected"

// connection is the sidebar's link to the observer, for its footer at rest:
// "connected" while it holds a snapshot and no offline note; "" otherwise,
// when the panel's own note (offline, degraded, an error) already says what
// is wrong. A time would mislead: the observer sends only changes, so a live
// feed over a quiet fleet would read as stale (the user, 2026-10-01).
func (p panel) connection() string {
	if p.snap == nil || p.offline() {
		return ""
	}
	return panelConnected
}

// ---------------------------------------------------------------------------
// H11 — the grouping, total over every status
// ---------------------------------------------------------------------------

// panelGroup is one of the five render groups.
type panelGroup int

const (
	groupNeedsYou panelGroup = iota
	groupActive
	groupIdle
	groupUnclear
	groupEnded
)

// panelGroups is the render order, which is also H11's numbering.
var panelGroups = []panelGroup{groupNeedsYou, groupActive, groupIdle, groupUnclear, groupEnded}

// title is the group's heading.
func (g panelGroup) title() string {
	switch g {
	case groupNeedsYou:
		return "NEEDS YOU"
	case groupActive:
		return "ACTIVE"
	case groupIdle:
		return "IDLE"
	case groupUnclear:
		return "UNCLEAR"
	default:
		return "ENDED"
	}
}

// panelGroupOf classifies one session. It is TOTAL: every status, and every
// value of Live, reaches exactly one return.
//
// The order of the tests is the whole specification, because H11's clauses
// overlap and a document cannot leave that to the reader:
//
//   - ENDED first. done/failed and `live: false` are the terminal evidence, and
//     ENDED is the group that can be hidden, so a done job must not be parked
//     in UNCLEAR where `d` cannot hide it. This is the ONE place this file
//     resolves H11's overlap differently from a literal reading of its clause
//     4 ("live == null regardless of status"): a done record with unknown
//     liveness is ENDED, not UNCLEAR, because "it finished" is knowledge and
//     "we cannot say whether it is alive" adds nothing to it. Recorded as a
//     divergence in the implementation log rather than decided silently.
//   - UNCLEAR next, which is clause 4 for every non-terminal status: a
//     `live: null` session has no business being reported as active or idle,
//     since the evidence for the claim is exactly what is missing.
//   - then the three live groups, in H11's order.
//   - the final return is what makes the function total: a status this build
//     has never seen (Status is an OBSERVED set, §3.3) is UNCLEAR rather than
//     absent.
func panelGroupOf(s observer.Session) panelGroup {
	if s.Status.IsTerminal() || (s.Live != nil && !*s.Live) {
		return groupEnded
	}
	if s.Live == nil || s.Status == observer.StatusUnknown {
		return groupUnclear
	}
	if s.Attention {
		return groupNeedsYou
	}
	switch s.Status {
	case observer.StatusBusy, observer.StatusWorking:
		return groupActive
	case observer.StatusIdle, observer.StatusShell:
		return groupIdle
	}
	return groupUnclear
}

// panelRows groups and orders the sessions of one snapshot.
//
// Within a group: attention descending by age (longest wait first), everything
// else ascending by name, ties broken by sessionId so two runs over one state
// produce the same order. The age is derived HERE from now - statusSince, not
// read from statusAgeMs, for the reason at the top of this file.
func panelRows(snap *observer.Snapshot, now time.Time, policy observer.Policy) map[panelGroup][]observer.Session {
	out := map[panelGroup][]observer.Session{}
	if snap == nil {
		return out
	}
	// Code review round 2, A7 (core, one line): group on liveness derived at
	// THIS render clock — the server never republishes a time-only crossing.
	snap = observer.AsOf(snap, now, policy)
	for _, s := range snap.Sessions {
		g := panelGroupOf(s)
		out[g] = append(out[g], s)
	}
	for g := range out {
		rows := out[g]
		byAge := g == groupNeedsYou
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if byAge {
				ageA, ageB := panelAge(a, now), panelAge(b, now)
				if ageA != ageB {
					return ageA > ageB
				}
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.SessionID < b.SessionID
		})
	}
	return out
}

// panelAge is how long this session has been in its current status, as the
// panel's clock sees it. A session with no statusSince has age -1, which sorts
// below every real duration and renders as "—".
func panelAge(s observer.Session, now time.Time) time.Duration {
	if s.StatusSince == nil {
		return -1
	}
	d := now.Sub(*s.StatusSince)
	if d < 0 {
		return 0
	}
	return d
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// The panel's styles, built from theme_panel.go's palette tokens (§6.1) so
// each resolves to its light or dark variant at render time. No colour is
// spelled here: the palette is the one place that does.
var (
	panelTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(panelPal.sky)
	panelGroupStyle = lipgloss.NewStyle().Bold(true).Foreground(panelPal.cloud)
	// panelBandStyle is the NEEDS YOU heading's label: the one heading drawn
	// in the alert colour, because it is the one that asks for you.
	panelBandStyle = lipgloss.NewStyle().Bold(true).Foreground(panelPal.alert)
	panelNameStyle = lipgloss.NewStyle().Foreground(panelPal.cloud)
	// panelBodyStyle is the attention body text: what a session waits for.
	panelBodyStyle = lipgloss.NewStyle().Foreground(panelPal.cloud)
	panelMetaStyle = lipgloss.NewStyle().Foreground(panelPal.slate)
	// panelNoteStyle is the dimmest line: the footer and the config note.
	panelNoteStyle = lipgloss.NewStyle().Foreground(panelPal.ash)
	// panelAlertStyle is coral as TEXT, which the palette lifts to the text
	// floor on every page (panelPal.alert, not the coral fill).
	panelAlertStyle = lipgloss.NewStyle().Foreground(panelPal.alert)
	// panelRoseStyle is an error that is not a call to act: API errors.
	panelRoseStyle = lipgloss.NewStyle().Foreground(panelPal.rose)
	// The start button reads as pressable rather than as one more line of
	// text: reverse video on the title colour, which both palettes already
	// carry, so it inverts correctly on a light terminal too.
	panelButtonStyle = lipgloss.NewStyle().Bold(true).Reverse(true).Foreground(colorTitle)
)

// panelStartLabel is the button's text, and the token the click hit-test finds
// it by. A pane with no observer to show is otherwise a dead end: the note said
// what was wrong and left the user to go and type the fix somewhere else.
const panelStartLabel = "start the observer"

// panelStartKey is the key that presses it. A plain letter, because the only
// surface that can show this button is `tt observe --pane`, where §5.5(a)'s
// plain letters are free (the TUI's panel watches for itself and never goes
// offline, and every printable key there belongs to the search box).
const panelStartKey = "s"

// panelOffline reports whether a note means "there is no observer to attach
// to". The two strings are the socket source's own, matched exactly: the
// degraded variants that embed them describe a pane that cannot see a server
// appear, and a start button there would start something this pane would never
// notice.
func panelOffline(note string) bool {
	return note == noteObserverNotRunning || note == noteObserverStopped
}

// offline is panelOffline over this panel's note.
func (p panel) offline() bool { return panelOffline(p.note) }

// startButton is the rendered button line.
func (p panel) startButton() string {
	return panelButtonStyle.Render(panelTruncate(" ▶ "+panelStartLabel+"   "+panelStartKey+" ", p.width))
}

// startButtonRow is the row the button occupies in this panel's rendered
// lines, or -1 when there is none — the whole of the click hit-test. The row
// is enough: a click anywhere along it presses the button, which is what a
// 46-column sidebar wants, and it costs no layout arithmetic in the surfaces
// that draw the panel.
func (p panel) startButtonRow() int {
	if !p.offline() {
		return -1
	}
	lines := p.lines()
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], panelStartLabel) {
			return i
		}
	}
	return -1
}

// panelAccents maps the colour NAMES a transcript's agent-color record carries
// (measured values: blue, green, cyan, purple, red, pink, orange) onto each
// name's full hue — the gutter colour, which colour mode also gives the
// group's heading. The values come from the palette's session-hue table, so a
// hue is defined once. An unknown name gets no accent rather than a guessed
// hue — the same "prefer no opinion over a wrong answer" rule the core
// follows.
var panelAccents = func() map[string]lipgloss.AdaptiveColor {
	m := make(map[string]lipgloss.AdaptiveColor, len(panelPal.hues))
	for name, h := range panelPal.hues {
		m[name] = h.gutter
	}
	return m
}()

// view renders the panel column, or "" when it contributes nothing.
//
// The zero-width case is first and it is a guarantee, not an optimisation: a
// closed or unsized panel must add no bytes to the model's View, which is what
// keeps model_test.go's existing assertions byte-identical.
//
// It is never taller than p.height (code review round 2, A9). That is not
// cosmetic: Bubble Tea v1.3.10's renderer paints only the LAST terminal-height
// lines of a frame (standard_renderer.go:180–187, "we drop lines from the top
// of the render buffer"), so an over-tall panel does not scroll — it loses its
// title and NEEDS YOU, the two things it exists to show. With the user's
// normal 35 sessions P9's layout was ~75 lines. A height of 0 means "not sized
// yet" and bounds nothing.
func (p panel) view() string {
	if !p.open || p.width <= 0 {
		return ""
	}
	return strings.Join(p.lines(), "\n")
}

// panelClamp keeps at most height lines (the top ones); a height of 0 keeps
// everything. It is the last line of defence, not the layout: lines() already
// fits, and this only ever cuts at heights too small to fit a title and a row.
func panelClamp(lines []string, height int) []string {
	if height > 0 && len(lines) > height {
		return lines[:height]
	}
	return lines
}

// panelAgentColor is the session's agent colour, or "".
func panelAgentColor(s observer.Session) string {
	if s.Transcript == nil || !s.Transcript.ColorFound {
		return ""
	}
	return s.Transcript.AgentColor
}

// panelShortID names a session that has no name: the background agent's short
// id when it has one, else the first 8 characters of the session id.
func panelShortID(s observer.Session) string {
	if s.ShortID != nil && *s.ShortID != "" {
		return *s.ShortID
	}
	if len(s.SessionID) > 8 {
		return s.SessionID[:8]
	}
	return s.SessionID
}

// footerNote is the one dim line under the rows: the panel's own note when it
// has one, else a line saying the `claude agents` cross-check is failing.
//
// §5.5a asked for a footer whenever the agents-cli source is STALE. It now
// speaks only while the check FAILS (a stale listing with a lastError). An
// old listing is the normal state of a quiet machine: the observer re-runs the
// check only when a rebuild happens, never on a timer, and no reported status
// depends on it — so "corroboration 1m old" on an idle fleet told the reader
// nothing they could act on (the user, 2026-10-21: "what is corroboration at
// all"). A failing check is still never a silent omission: on 2026-10-21 it
// failed for 8h50m.
func (p panel) footerNote() string {
	if note := p.shownNote(); note != "" {
		return note
	}
	if p.snap == nil {
		return ""
	}
	src, ok := p.snap.Sources[observer.SourceAgentsCLI]
	if !ok || !src.Stale || src.LastError == nil {
		return ""
	}
	note := "claude agents check failing"
	if src.AgeMs != nil {
		note += ", last ok " + panelHumanise(time.Duration(*src.AgeMs)*time.Millisecond) + " ago"
	}
	if p.surface == surfacePane {
		// Only the pane has a refresh verb. In the TUI `r` is a search
		// character, so the hint would send the reader to type into the box.
		note += " — " + keyPanelRefresh + " to retry"
	}
	return note
}

// shownNote is the panel's note as the default view draws it: every note
// that says something is wrong or stale (offline, degraded, an error, a failed
// refresh), and none that only says where the data comes from. "via tt observe
// (pid N)" and panel-render's "snapshot of …" left the default view in
// revision 4 (FR9): the header already says what the panel shows.
func (p panel) shownNote() string {
	if panelSourceNote(p.note) {
		return ""
	}
	return p.note
}

// panelSourceNote reports whether a note only names the panel's source.
func panelSourceNote(note string) bool {
	return strings.HasPrefix(note, noteViaObserver) || note == panelSnapshotNote ||
		strings.HasPrefix(note, panelSnapshotNote+" of ")
}

// panelHumanise renders a duration the way a person reads one. The units are
// coarse on purpose: the question the panel answers is "how long has this been
// waiting", and to that question 14m and 14m03s are the same answer.
//
// A negative duration is "—": that is panelAge's encoding of "this session has
// no statusSince", and inventing a zero there would claim a measurement that
// does not exist.
func panelHumanise(d time.Duration) string {
	switch {
	case d < 0:
		return "—"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d/time.Minute)%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d/(24*time.Hour)), int(d/time.Hour)%24)
	}
}

// panelTruncate cuts s to at most width DISPLAY COLUMNS, ending in "…" when it
// had to cut. Columns, not runes and not bytes: a wide glyph (CJK, most emoji)
// is two columns, and counting it as one let a line run two columns past the
// panel's edge per wide character. It never splits a grapheme. For ASCII the
// result is byte-identical to the old rune count.
func panelTruncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}
