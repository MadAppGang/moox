package main

// Code review round 2, A11: the panel RENDERS three transcript facts — the
// agent colour as the row's accent, the subagent count and the API-error count
// — and neither production source ever asked for them. The local watch loop's
// rebuilds never read a transcript (§7.5f: IncludeTranscripts is false for
// every watch-driven rebuild), and the socket source's connection kept the
// server's default enrich:false. Every P9 rendering test handed the panel a
// PRE-ENRICHED fake snapshot, which is exactly why none of them noticed.
//
// So the two composition tests here build the production source over a real
// Claude tree fixture — registry record, transcript with an agent-color
// record, subagent meta files — and assert on the RENDERED row. No snapshot in
// them is constructed by hand.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
)

// withTrueColor makes lipgloss emit colour for the length of one test. Under
// `go test` stdout is not a terminal, so the default renderer is ASCII and
// every accent would render identically — which would make a colour assertion
// vacuous. Restored on cleanup; no test in this package runs in parallel.
func withTrueColor(t *testing.T) {
	t.Helper()
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(profile)
		lipgloss.SetHasDarkBackground(dark)
	})
}

// paintedSessionID is the fixture's session.
const paintedSessionID = "a11a11a1-0000-4000-8000-00000000c010"

// writePaintedFixture lays out one live interactive session the way Claude
// Code does: its registry record (pid = this test process, the only pid a test
// can honestly call alive; no tmux reference, so no tmux command is issued),
// its transcript with a green agent-color record and one API-error message,
// and two subagent meta files beside it.
func writePaintedFixture(t *testing.T, root string) {
	t.Helper()
	slug := filepath.Join(root, "projects", "-tmp-painted")
	subagents := filepath.Join(slug, paintedSessionID, "subagents")
	for _, d := range []string{filepath.Join(root, "sessions"), filepath.Join(root, "jobs"), subagents} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	pid := os.Getpid()
	record := fmt.Sprintf(`{"pid": %d, "sessionId": %q, "cwd": "/tmp/painted", "startedAt": %d,
  "version": "2.1.276", "kind": "interactive", "name": "painted", "nameSource": "user",
  "updatedAt": %d, "status": "busy", "statusUpdatedAt": %d}`, pid, paintedSessionID, now-60_000, now, now)
	stamp := observer.FormatTime(time.Now())
	// The assistant record with a usage object is what the context badge
	// counts: input + cache creation + cache read = 132000, "132k" (FR6).
	// Built here, in code: a transcript "message" key may not live under
	// testdata (the privacy denylist).
	transcript := strings.Join([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q}`, stamp),
		`{"type":"agent-color","agentColor":"green"}`,
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"model":"claude-opus-5-5",`+
			`"usage":{"input_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":129000,"output_tokens":7}}}`, stamp),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"isApiErrorMessage":true}`, stamp),
	}, "\n") + "\n"
	for path, body := range map[string]string{
		filepath.Join(root, "sessions", fmt.Sprintf("%d.json", pid)): record,
		filepath.Join(slug, paintedSessionID+".jsonl"):               transcript,
		filepath.Join(subagents, "agent-one.meta.json"):              `{}`,
		filepath.Join(subagents, "agent-two.meta.json"):              `{}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// paintedName is the fixture's name as the panel draws it with its transcript's
// colour: the exact bytes, so an uncoloured or wrongly coloured name fails.
// The name is drawn in the hue's soft tint ({name:colour}, §6.2); the full
// hue is the gutter's.
func paintedName() string { return tintedName("green", "painted") }

// tintedName is a name as the built-in {name:badge} draws it for a /color:
// on the soft badge of that hue.
func tintedName(colour, name string) string {
	return paintSegment(nameBadge(name, sessionHue(colour), false))
}

// paintedGutter is the row's gutter in the transcript's hue.
func paintedGutter() string {
	return lipgloss.NewStyle().Foreground(sessionHue("green").gutter).Render(gutterGlyph)
}

// pumpPanelUntil feeds the panel's own message channel into its own update
// path — the path the program drives — until the rendered panel satisfies
// done, and fails with the last rendering otherwise.
func pumpPanelUntil(t *testing.T, p *panel, within time.Duration, done func(view string) bool) string {
	t.Helper()
	deadline := time.After(within)
	for {
		if view := p.view(); done(view) {
			return view
		}
		select {
		case msg := <-p.msgs:
			p.update(msg)
		case <-deadline:
			t.Fatalf("the panel never rendered the enriched row within %s; last rendering:\n%s\n(raw %q)",
				within, ansi.Strip(p.view()), p.view())
			return ""
		}
	}
}

// withFactsMeta installs a panel.conf whose meta line draws the transcript's
// two counts. The built-in view no longer shows them (FR9: the lines under a
// row are its latest text), and these tests prove every fact arrives.
func withFactsMeta(t *testing.T) {
	t.Helper()
	newConfFixture(t).write(t, `meta = "     {sub} · {apierr}"`+"\n", time.Now())
}

// enrichedRow is the verdict: the gutter and the name in the transcript's
// colour, the context badge counting its usage, and the two counts on its
// meta line (withFactsMeta).
func enrichedRow(view string) bool {
	return strings.Contains(view, paintedName()) &&
		strings.Contains(view, paintedGutter()) &&
		strings.Contains(ansi.Strip(view), "132k") &&
		strings.Contains(ansi.Strip(view), "2 sub") &&
		strings.Contains(ansi.Strip(view), "1 api err")
}

// The local branch — the ^o panel in the TUI, and a manual sidebar at scope
// "off" with no server — through localPanelSource itself: the real core, the
// real kqueue watch loop and the real transcriptfs, over the fixture tree.
func TestTheLocalPanelSourceRendersTheTranscriptsColourSubagentsAndErrors(t *testing.T) {
	withTrueColor(t)
	withFactsMeta(t)
	root := t.TempDir()
	writePaintedFixture(t, root)
	hermeticTree(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	src, _, err := localPanelSource(ctx, observer.SystemClock{})
	if err != nil {
		t.Fatalf("localPanelSource: %v", err)
	}

	p := newPanel(src, observer.SystemClock{})
	p.width, p.height = 80, 40
	p.start()
	t.Cleanup(p.close)

	view := pumpPanelUntil(t, &p, 10*time.Second, enrichedRow)
	t.Logf("local source, rendered:\n%s", ansi.Strip(view))
}

// The socket branch — every automatic sidebar — through the real `tt observe`
// server (serveObserve) and the pane's real socket source, whose production
// dial must negotiate §7.5(f)'s standing enrich:true.
func TestAnAttachedPaneNegotiatesAnEnrichedStreamAndRendersTheColour(t *testing.T) {
	withFactsMeta(t)
	withTrueColor(t)
	root := t.TempDir()
	writePaintedFixture(t, root)
	hermeticTree(t, root)

	// sun_path is 104 bytes on darwin; t.TempDir() is longer.
	sockDir, err := os.MkdirTemp("", "tt-a11-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		serveObserve(ctx, observeFlags{Dir: sockDir, Timeout: 500 * time.Millisecond, StatusLine: statusLineNever}, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("serveObserve did not return after its context was cancelled")
		}
	})
	sock := filepath.Join(sockDir, sockserve.SocketName)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) { //nolint:forbidigo // test-only poll
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the observer socket never appeared")
		}
	}

	p := newPanel(newSocketSource(sockDir), observer.SystemClock{})
	p.width, p.height = 80, 40
	p.start()
	t.Cleanup(p.close)

	// The source's note names the server it attached to. The panel carries
	// it, and the default view no longer draws it (FR9).
	view := pumpPanelUntil(t, &p, 10*time.Second, func(v string) bool {
		return enrichedRow(v) && strings.HasPrefix(p.note, "via tt observe")
	})
	if strings.Contains(view, "via tt observe") {
		t.Errorf("the source note is drawn on the default view:\n%s", ansi.Strip(view))
	}
	t.Logf("socket source, rendered:\n%s", ansi.Strip(view))
}

// ---------------------------------------------------------------------------
// the pump's enrichment, unit by unit
// ---------------------------------------------------------------------------

// enrichingFakeSource is a panelSource whose stream is unenriched and which
// can enrich, like the local loop. Enrich paints every session it is handed
// and records what it was asked for.
type enrichingFakeSource struct {
	*fakePanelSource
	mu    sync.Mutex
	calls [][]string
}

func (f *enrichingFakeSource) Enrich(_ context.Context, snap *observer.Snapshot, _ []string) (*observer.Snapshot, error) {
	out := *snap
	out.Sessions = append([]observer.Session(nil), snap.Sessions...)
	var ids []string
	for i := range out.Sessions {
		n := 3
		out.Sessions[i].Transcript = &observer.TranscriptFacts{AgentColor: "green", ColorFound: true, Subagents: &n, SubagentsKnown: true}
		out.Sessions[i].TranscriptCollected = true
		ids = append(ids, out.Sessions[i].SessionID)
	}
	f.mu.Lock()
	f.calls = append(f.calls, ids)
	f.mu.Unlock()
	return &out, nil
}

// A baseline is shown at once and then again enriched; an added or changed
// session is enriched before it is shown; a removal passes straight through.
// The published snapshot is never mutated.
func TestThePumpEnrichesBaselinesAndChangedSessionsFromAnUnenrichedSource(t *testing.T) {
	withFactsMeta(t)
	withTrueColor(t)
	now := time.Now()
	src := &enrichingFakeSource{fakePanelSource: newFakePanelSource()}
	p := newPanel(src, newPanelClock(now))
	p.width, p.height = 80, 40
	p.start()
	t.Cleanup(p.close)

	live := true
	since := now.Add(-time.Minute)
	base := observer.Session{SessionID: "s1", Name: "painted", Status: observer.StatusIdle, Live: &live, StatusSince: &since}
	published := &observer.Snapshot{ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{base}}
	src.ch <- snapshotEvent(published)
	pumpPanelUntil(t, &p, 2*time.Second, func(v string) bool {
		return strings.Contains(v, paintedName()) && strings.Contains(ansi.Strip(v), "3 sub")
	})
	if published.Sessions[0].Transcript != nil {
		t.Fatal("enrichment mutated the published snapshot")
	}

	added := observer.Session{SessionID: "s2", Name: "second", Status: observer.StatusBusy, Live: &live, StatusSince: &since}
	src.ch <- observer.Event{Type: observer.EventSessionAdded, SessionID: "s2", Session: &added, At: now}
	pumpPanelUntil(t, &p, 2*time.Second, func(v string) bool {
		return strings.Contains(v, tintedName("green", "second"))
	})
	if added.Transcript != nil {
		t.Fatal("enrichment mutated the delta's session")
	}

	src.ch <- observer.Event{Type: observer.EventSessionRemoved, SessionID: "s2", Reason: observer.RemovedPIDDead, At: now}
	pumpPanelUntil(t, &p, 2*time.Second, func(v string) bool { return !strings.Contains(ansi.Strip(v), "second") })

	src.mu.Lock()
	defer src.mu.Unlock()
	// One pass for the baseline (every session), one for the addition (only
	// that session), none for the removal.
	if len(src.calls) != 2 || len(src.calls[0]) != 1 || len(src.calls[1]) != 1 || src.calls[1][0] != "s2" {
		t.Fatalf("Enrich was asked for %v, want [[s1] [s2]]", src.calls)
	}
}

// A source whose stream is ALREADY enriched (the socket source, attached with
// enrich:true) is not re-enriched by the panel: it does not implement Enrich,
// and the pump must not need it to.
func TestAnEnrichedStreamIsRenderedAsItArrives(t *testing.T) {
	withFactsMeta(t)
	withTrueColor(t)
	now := time.Now()
	p, src, _ := openTestPanel(t, now)
	live := true
	n := 1
	src.ch <- snapshotEvent(&observer.Snapshot{ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{SessionID: "s1", Name: "painted", Status: observer.StatusIdle, Live: &live,
			Transcript: &observer.TranscriptFacts{AgentColor: "green", ColorFound: true, Subagents: &n, SubagentsKnown: true}}}})
	pumpPanelUntil(t, p, 2*time.Second, func(v string) bool {
		return strings.Contains(v, paintedName()) && strings.Contains(ansi.Strip(v), "1 sub")
	})
}
