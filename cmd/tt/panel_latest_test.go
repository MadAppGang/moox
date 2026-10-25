package main

// The latest-text lines (FR9, architecture revision 4): the panel-side reader
// of a transcript's newest assistant text, its cache, the lines it draws
// under a row, and the guarantee that the text never leaves the panel.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// ---------------------------------------------------------------------------
// Synthetic transcripts. Written into t.TempDir() at test time, never into
// testdata/: a transcript fixture carries `message` and `content` keys, which
// the privacy denylist bans from every committed fixture.
// ---------------------------------------------------------------------------

// jsonLine is one transcript record as a JSONL line.
func jsonLine(t testing.TB, rec map[string]any) string {
	t.Helper()
	b, err := json.Marshal(rec)
	must(t, err)
	return string(b)
}

// assistantRecord is a main-thread assistant turn with these text blocks.
func assistantRecord(t testing.TB, texts ...string) string {
	var blocks []any
	for _, x := range texts {
		blocks = append(blocks, map[string]any{"type": "text", "text": x})
	}
	return jsonLine(t, map[string]any{
		"type": "assistant", "isSidechain": false,
		"message": map[string]any{"model": "claude-opus-5-5", "role": "assistant", "content": blocks},
	})
}

// toolUseRecord is an assistant turn that only calls a tool: its input is
// command text the reader must never show.
func toolUseRecord(t testing.TB, command string) string {
	return jsonLine(t, map[string]any{
		"type": "assistant", "isSidechain": false,
		"message": map[string]any{"model": "claude-opus-5-5", "content": []any{
			map[string]any{"type": "thinking", "thinking": "THINKING-" + command},
			map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": command}},
		}},
	})
}

// toolResultRecord is the user-side record carrying a tool's output.
func toolResultRecord(t testing.TB, output string) string {
	return jsonLine(t, map[string]any{
		"type": "user",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": output},
		}},
	})
}

// userRecord is a prompt the user typed.
func userRecord(t testing.TB, text string) string {
	return jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

// sidechainRecord is a subagent's assistant turn: not the session's own voice.
func sidechainRecord(t testing.TB, text string) string {
	return jsonLine(t, map[string]any{
		"type": "assistant", "isSidechain": true,
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})
}

// writeTranscript writes records as a transcript file and returns its path.
func writeTranscript(t testing.TB, dir, name string, records ...string) string {
	t.Helper()
	path := filepath.Join(dir, name+".jsonl")
	must(t, os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600))
	return path
}

// fleetLatestText is the latest text withLatestText gives a session: long
// enough to wrap to three lines at the sidebar's width.
func fleetLatestText(name string) string {
	return name + " is reading the failing test output, then fixing the parser so the golden frames match again before the next run."
}

// withLatestText gives the fleet's sessions a transcript whose newest
// assistant text is fleetLatestText, followed by tool traffic and a user
// prompt the reader must skip: every session, or those whose index some
// selects.
func withLatestText(t testing.TB, f *fleet, some ...func(i int) bool) {
	t.Helper()
	dir := t.TempDir()
	snap := *f.snap
	snap.Sessions = append([]observer.Session(nil), f.snap.Sessions...)
	for i := range snap.Sessions {
		if len(some) > 0 && !some[0](i) {
			continue
		}
		s := &snap.Sessions[i]
		facts := observer.TranscriptFacts{}
		if s.Transcript != nil {
			facts = *s.Transcript
		}
		facts.Path = writeTranscript(t, dir, s.SessionID+"-"+string(s.Kind),
			userRecord(t, "USER-PROMPT please fix it"),
			assistantRecord(t, fleetLatestText(s.Name)),
			toolUseRecord(t, "go test ./... # TOOL-INPUT"),
			toolResultRecord(t, "TOOL-OUTPUT ok"),
		)
		s.Transcript = &facts
	}
	f.snap = &snap
}

// ---------------------------------------------------------------------------
// The reader
// ---------------------------------------------------------------------------

// The reader takes the newest main-thread assistant record that has text,
// and only its text blocks: a newer tool call, its thinking, its output, a
// subagent's turn and a user prompt are all skipped, and nothing of them is
// ever in the result.
func TestTheLatestTextIsTheNewestAssistantTextAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "one",
		assistantRecord(t, "OLDER assistant text"),
		userRecord(t, "USER-PROMPT do the thing"),
		assistantRecord(t, "Running the tests now.", "They should take a minute."),
		toolUseRecord(t, "rm -rf dist # TOOL-INPUT"),
		toolResultRecord(t, "TOOL-OUTPUT removed"),
		sidechainRecord(t, "SIDECHAIN subagent text"),
		userRecord(t, "USER-PROMPT later"),
	)
	got := readLatestAssistantText(path)
	if got != "Running the tests now. They should take a minute." {
		t.Errorf("latest text %q", got)
	}
	for _, banned := range []string{"OLDER", "USER-PROMPT", "TOOL-INPUT", "TOOL-OUTPUT", "THINKING", "SIDECHAIN", "rm -rf"} {
		if strings.Contains(got, banned) {
			t.Errorf("the latest text carries %q: %q", banned, got)
		}
	}

	// Nothing but tool traffic and prompts: no text at all.
	none := writeTranscript(t, dir, "none", userRecord(t, "USER-PROMPT"), toolUseRecord(t, "ls"), toolResultRecord(t, "x"))
	if got := readLatestAssistantText(none); got != "" {
		t.Errorf("a tail with no assistant text reads %q", got)
	}
	// A missing file, a torn line and a non-JSON line are "no text".
	if got := readLatestAssistantText(filepath.Join(dir, "absent.jsonl")); got != "" {
		t.Errorf("a missing transcript reads %q", got)
	}
	torn := filepath.Join(dir, "torn.jsonl")
	must(t, os.WriteFile(torn, []byte(assistantRecord(t, "kept")+"\nnot json\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"te"), 0o600))
	if got := readLatestAssistantText(torn); got != "kept" {
		t.Errorf("a torn last line hides the text before it: %q", got)
	}
}

// The text is sanitised — no control character can reach the terminal, and
// runs of whitespace are one space — and markdown's code backticks and bold
// stars are dropped.
func TestTheLatestTextIsSanitised(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "esc", assistantRecord(t, "Done:\x1b[31m **all** green\r\n\tsee `go test`\x07 now\u009b2J"))
	got := readLatestAssistantText(path)
	if strings.ContainsAny(got, "\x1b\x07\r\n\t\u009b`*") {
		t.Errorf("the latest text kept a control character or a markdown mark: %q", got)
	}
	if got != "Done: [31m all green see go test now 2J" {
		t.Errorf("sanitised text %q", got)
	}
}

// Only the tail is read: a transcript whose only assistant text sits before
// the last latestTailBytes draws nothing, and a long text keeps its END.
func TestOnlyTheTailIsReadAndALongTextKeepsItsEnd(t *testing.T) {
	dir := t.TempDir()
	filler := userRecord(t, strings.Repeat("x", latestTailBytes))
	early := writeTranscript(t, dir, "early", assistantRecord(t, "TOO-FAR-BACK"), filler)
	if got := readLatestAssistantText(early); got != "" {
		t.Errorf("text before the tail was read: %q", got)
	}
	long := strings.Repeat("word ", 3000) + "THE-END"
	path := writeTranscript(t, dir, "long", assistantRecord(t, long))
	got := readLatestAssistantText(path)
	if len(got) > latestKeepBytes || !strings.HasSuffix(got, "THE-END") || !strings.HasPrefix(got, "word") {
		t.Errorf("a long text kept %d bytes, starting %q and ending %q", len(got), got[:min(10, len(got))], got[max(len(got)-10, 0):])
	}
}

// The cache reads a transcript once while its size and mtime stand still, and
// again when either moves; a transcript no row asked for in a frame is
// dropped on the next.
func TestTheLatestTextIsCachedBySizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "c", assistantRecord(t, "first"))
	c := newLatestCache()
	reads := 0
	c.read = func(p string) string { reads++; return readLatestAssistantText(p) }

	for range 3 {
		c.sweep()
		if got := c.text(path); got != "first" {
			t.Fatalf("cached text %q", got)
		}
	}
	if reads != 1 {
		t.Errorf("an unchanged transcript was read %d times, want once", reads)
	}
	writeTranscript(t, dir, "c", assistantRecord(t, "first"), assistantRecord(t, "second and longer"))
	if got := c.text(path); got != "second and longer" || reads != 2 {
		t.Errorf("a grown transcript reads %q after %d reads, want the new text on the second", got, reads)
	}
	// Same size, new mtime: read again.
	writeTranscript(t, dir, "c", assistantRecord(t, "first"), assistantRecord(t, "second and LONGER"))
	must(t, os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)))
	if got := c.text(path); got != "second and LONGER" || reads != 3 {
		t.Errorf("a rewritten transcript of the same size reads %q after %d reads", got, reads)
	}
	c.sweep() // the frame that asked for it ends
	c.sweep() // a frame that did not
	if len(c.entries) != 0 {
		t.Errorf("the cache kept %d transcripts no frame asked for", len(c.entries))
	}
	if got := c.text(""); got != "" {
		t.Errorf("an empty path reads %q", got)
	}
}

// ---------------------------------------------------------------------------
// The lines under a row
// ---------------------------------------------------------------------------

// A row outside the band draws the LAST three lines of its latest text,
// wrapped to the width less the gutter and the indent; its /color bar runs
// down through every one of them. A band row draws its why instead.
func TestTheLatestTextLinesCarryTheBarAndKeepTheEnd(t *testing.T) {
	withPanelTheme(t, true)
	f := newFleet(t, fleetNow)
	withLatestText(t, &f)
	p := sidebar(t, f.now, f.snap, modeProject, 0)
	fl := parseFrame(plain(p.view()))
	styled := strings.Split(p.view(), "\n")

	coloured, plainRows := 0, 0
	for i, l := range fl {
		if l.kind != lineRow {
			continue
		}
		under, _ := bandLines(fl, l.name)
		band := false
		for _, s := range f.snap.Sessions {
			// In the band, or an Attention row kept in its home group (R5):
			// either draws why it waits instead.
			if s.Name == l.name && (panelGroupOf(s) == groupNeedsYou || s.Attention) {
				band = true
			}
		}
		if band {
			if len(under) == 0 || !strings.HasPrefix(under[0], "waiting ") {
				t.Errorf("band row %s draws %q, want its why", l.name, under)
			}
			continue
		}
		want := lastLines(fleetLatestText(l.name), panelWidth-1-panelTextIndent, panelTextLines)
		if strings.Join(under, "|") != strings.Join(want, "|") {
			t.Errorf("%s draws %q, want the last three lines %q", l.name, under, want)
		}
		for j := 1; j <= len(under); j++ {
			m := fl[i+j]
			if m.bar != l.gutter {
				t.Errorf("%s: line %d under it has bar %v, the row's gutter %v", l.name, j, m.bar, l.gutter)
			}
			if w := ansi.StringWidth(styled[i+j]); w > panelWidth {
				t.Errorf("%s: a text line is %d wide", l.name, w)
			}
			// The bar is the row's gutter, painted byte for byte the same.
			if l.gutter && !strings.HasPrefix(styled[i+j], gutterPrefix(styled[i])) {
				t.Errorf("%s: the bar under it is not painted like its gutter: %q vs %q", l.name, styled[i+j], styled[i])
			}
		}
		if l.gutter {
			coloured++
		} else {
			plainRows++
		}
	}
	if coloured == 0 || plainRows == 0 {
		t.Fatalf("the fleet drew %d coloured and %d plain rows with text; both are needed", coloured, plainRows)
	}
}

// The bar runs through a meta line too: a coloured band row's why and then its
// home group (attn_meta), every line under it opening with the row's gutter.
func TestTheBarRunsThroughTheWhyAndTheMetaLine(t *testing.T) {
	withPanelTheme(t, true)
	f := newFleet(t, fleetNow)
	p := sidebar(t, f.now, f.snap, modeTmux, 0)
	view := p.view()
	fl, styled := parseFrame(plain(view)), strings.Split(view, "\n")
	for i, l := range fl {
		if l.kind != lineRow || l.name != "mnemex-04" {
			continue
		}
		under, _ := bandLines(fl, l.name)
		if !l.gutter || len(under) != 2 || under[1] != "background" {
			t.Fatalf("mnemex-04 (coloured, in the band, home background) draws %q under it", under)
		}
		for j := 1; j <= len(under); j++ {
			if !fl[i+j].bar || !strings.HasPrefix(styled[i+j], gutterPrefix(styled[i])) {
				t.Errorf("line %d under mnemex-04 does not carry its bar: %q", j, styled[i+j])
			}
		}
		return
	}
	t.Fatal("mnemex-04 is not drawn")
}

// gutterPrefix is a painted row's gutter cell: its bytes up to and including
// the "▌".
func gutterPrefix(line string) string {
	if i := strings.Index(line, "▌"); i >= 0 {
		return line[:i+len("▌")]
	}
	return ""
}

// An UNCLEAR row with no latest text explains itself on its text line.
func TestAnUnclearRowWithNoTextSaysWhyItIsUnclear(t *testing.T) {
	now := fleetNow
	since := now.Add(-time.Hour)
	snap := &observer.Snapshot{ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{
			SessionID: "u", Name: "hazy", Status: observer.StatusBusy, StatusSince: &since,
			Advisories: []observer.Advisory{{Code: "transcript-absent"}},
		}}}
	p := sidebar(t, now, snap, modeStatus, 0)
	under, ok := bandLines(parseFrame(plain(p.view())), "hazy")
	if !ok || strings.Join(under, "|") != "live? transcript-absent" {
		t.Errorf("the unclear row draws %q under it (shown %v)", under, ok)
	}
}

// ---------------------------------------------------------------------------
// §5.4 stays true outside the panel
// ---------------------------------------------------------------------------

// The latest text is read by the panel and drawn into its rows, and nowhere
// else: after the panel has drawn it, the snapshot the panel holds carries it
// in no field (so observer.Session and TranscriptFacts cannot), the wire
// encoding of that snapshot — what the socket, MCP and `tt sessions --json`
// send — does not contain it, and the text reaches panel-render's output only.
func TestTheLatestTextNeverReachesTheWireOrTheJSON(t *testing.T) {
	const secret = "SECRET-ASSISTANT-TEXT the panel may draw but never send"
	dir := t.TempDir()
	path := writeTranscript(t, dir, "s", assistantRecord(t, secret))
	now := fleetNow
	since, live := now.Add(-time.Minute), true
	snap := &observer.Snapshot{ObservedAt: now, Counts: observer.NewCounts(), Sources: map[string]observer.SourceReport{},
		Sessions: []observer.Session{{
			SessionID: "s1", Name: "talker", Status: observer.StatusBusy, Live: &live, StatusSince: &since,
			TranscriptCollected: true, Transcript: &observer.TranscriptFacts{Path: path},
		}}}

	p := sidebar(t, now, snap, modeProject, 0)
	p.width = 200
	if view := ansi.Strip(p.view()); !strings.Contains(view, "SECRET-ASSISTANT-TEXT") {
		t.Fatalf("the panel does not draw the latest text, so this proves nothing:\n%s", view)
	}

	// No field of what the panel holds carries it.
	if where := findString(reflect.ValueOf(p.snap), "SECRET-ASSISTANT-TEXT", "snap"); where != "" {
		t.Errorf("the snapshot carries the latest text at %s", where)
	}
	// The wire's encoding — the one codec of the socket, MCP and --json.
	data, err := json.Marshal(wire.NewSnapshot(p.snap, now, wire.Options{TT: "test"}))
	must(t, err)
	if strings.Contains(string(data), "SECRET-ASSISTANT-TEXT") {
		t.Errorf("the wire encoding carries the latest text: %s", data)
	}
	// TranscriptFacts has no place for it: its string fields are still exactly
	// the path and the two bounded identifiers (§5.4), and the session kind.
	var strs []string
	typ := reflect.TypeOf(observer.TranscriptFacts{})
	for i := range typ.NumField() {
		if k := typ.Field(i).Type; k.Kind() == reflect.String || (k.Kind() == reflect.Pointer && k.Elem().Kind() == reflect.String) {
			strs = append(strs, typ.Field(i).Name)
		}
	}
	if got := strings.Join(strs, " "); got != "Path AgentColor SessionKind Model" {
		t.Errorf("TranscriptFacts' string fields are %q: a new one could carry message text", got)
	}
}

// findString reports where in v a string containing needle is, "" for
// nowhere. It walks pointers, structs, slices, maps and interfaces.
func findString(v reflect.Value, needle, path string) string {
	switch v.Kind() {
	case reflect.String:
		if strings.Contains(v.String(), needle) {
			return path
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return findString(v.Elem(), needle, path)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if w := findString(v.Field(i), needle, path+"."+v.Type().Field(i).Name); w != "" {
				return w
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if w := findString(v.Index(i), needle, fmt.Sprintf("%s[%d]", path, i)); w != "" {
				return w
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if w := findString(v.MapIndex(k), needle, fmt.Sprintf("%s[%v]", path, k)); w != "" {
				return w
			}
		}
	}
	return ""
}
