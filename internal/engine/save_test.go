package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

const (
	sid1 = "aaaaaaaa-0000-4000-8000-000000000001"
	sid2 = "bbbbbbbb-0000-4000-8000-000000000002"
	sid3 = "cccccccc-0000-4000-8000-000000000003"
)

func (h *harness) last(t *testing.T) *snapshot.Snapshot {
	t.Helper()
	l, err := h.e.Last()
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	return l.Snapshot
}

func (h *harness) store(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(h.e.paths, h.e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func paneByID(s *snapshot.Snapshot, id string) *snapshot.Pane {
	for i := range s.Panes {
		if s.Panes[i].SavedID == id {
			return &s.Panes[i]
		}
	}
	return nil
}

// workspace is a small typical world: two sessions, three windows.
func workspace() (*world, map[string][]string) {
	w := newWorld()
	w.session("dotfiles")
	w.session("api.v2")
	_, a := w.window("dotfiles", 1, 2)
	_, b := w.window("dotfiles", 2, 1)
	_, c := w.window("api.v2", 1, 3)
	return w, map[string][]string{"d1": a, "d2": b, "a1": c}
}

func TestSave_PublishesValidSnapshot(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	res := h.save(t, SaveRequest{Reason: "test", Contents: true})
	if !res.Published || res.Panes != 6 || res.Windows != 3 {
		t.Fatalf("%+v\n%s", res, h.log.String())
	}
	s := h.last(t)
	if s.Source != snapshot.SourceSave || s.Server.Pid != 812 || s.TmuxVersion != "3.8" || s.Socket.Name != "moox-test-fake" ||
		!strings.HasPrefix(s.Socket.Key, "moox-test-fake@") || s.Producer != "moox 0.0.0-test" {
		t.Fatalf("header: %+v", s)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
	if w.opts["@moox_status"] != "saved 12:01" || w.opts["@moox_save_seq"] != "1" {
		t.Fatalf("post chain: %v", w.opts)
	}
	fi, err := os.Stat(filepath.Join(h.e.paths.Snapshots, res.File))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode %v %v", fi.Mode(), err)
	}
}

// TestSave_ExecCountIsConstant (B25): a 66-pane workspace with contents costs
// the same few forks as a 6-pane one — never one per pane.
func TestSave_ExecCountIsConstant(t *testing.T) {
	counts := map[int]int{}
	for _, n := range []int{6, 66} {
		w := newWorld()
		w.session("big")
		for i := 0; i < n/3; i++ {
			w.window("big", i+1, 3)
		}
		h := newHarness(t, w)
		res := h.save(t, SaveRequest{Contents: true})
		if res.Panes != n {
			t.Fatalf("%d panes saved, want %d", res.Panes, n)
		}
		if res.Execs > 5 {
			t.Fatalf("%d panes: %d execs (B25 allows <= 5)\n%q", n, res.Execs, w.calls)
		}
		counts[n] = res.Execs
		// a structural (debounce) save of the same server: the read, the local
		// automatic-rename read (I-5: every window here inherits), ps, and the
		// status write
		h.w.panes[0].title = "changed"
		res2 := h.save(t, SaveRequest{})
		if res2.Execs > 4 || !res2.Published {
			t.Fatalf("structural save: %+v", res2)
		}
	}
	if counts[6] != counts[66] {
		t.Fatalf("exec count depends on pane count: %v", counts)
	}
}

// TestSave_OneTmuxReadChain (S6, S7): one tmux exec reads everything, before
// the one ps.
// TestSave_AutomaticRenameKeepsLocalValueEqualToGlobal (I-5, T6): with the
// global automatic-rename off, a window that sets `off` itself saves "off"
// (not "unset": a restore onto a server whose global is on would otherwise
// re-enable renaming), one that sets nothing saves "unset", and one whose
// effective value differs saves its own. One extra exec asks the ambiguous
// windows, whatever their number.
func TestSave_AutomaticRenameKeepsLocalValueEqualToGlobal(t *testing.T) {
	w := newWorld()
	w.globalAuto = "off"
	w.session("s")
	for i := 0; i < 3; i++ {
		w.window("s", i, 1)
	}
	w.windows[0].auto, w.windows[0].autoLocal = false, true // set -w automatic-rename off
	w.windows[1].auto = false                               // inherits the global off
	w.windows[2].auto = true                                // set -w automatic-rename on
	h := newHarness(t, w)
	if res := h.save(t, SaveRequest{}); !res.Published {
		t.Fatalf("not published: %+v", res)
	}
	got := map[int]string{}
	for _, x := range h.last(t).Windows {
		got[x.Index] = x.AutomaticRename
	}
	want := map[int]string{0: snapshot.AutoRenameOff, 1: snapshot.AutoRenameUnset, 2: snapshot.AutoRenameOn}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("automatic_rename %v, want %v", got, want)
	}
	local := 0
	for _, c := range w.calls {
		if strings.Contains(strings.Join(c, " "), "show-options -wqv -t") {
			local++
		}
	}
	if local != 1 {
		t.Fatalf("%d execs asked windows for their local value, want 1:\n%q", local, w.calls)
	}
}

func TestSave_OneTmuxReadChain(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	h.save(t, SaveRequest{})
	first := strings.Join(w.calls[0], " ")
	for _, want := range []string{"list-clients", "list-sessions", "list-windows -a", "list-panes -a", "show-options -gwv automatic-rename"} {
		if !strings.Contains(first, want) {
			t.Fatalf("the first exec does not %s: %s", want, first)
		}
	}
	if w.psCalls != 1 {
		t.Fatalf("%d ps calls", w.psCalls)
	}
}

// TestSave_NoResurrectHookOptionsRead (I1): nothing of resurrect's is read or
// set.
func TestSave_NoResurrectHookOptionsRead(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	h.save(t, SaveRequest{Contents: true})
	for _, c := range w.calls {
		if strings.Contains(strings.Join(c, " "), "resurrect") {
			t.Fatalf("a save touched resurrect: %q", c)
		}
	}
}

// TestSave_StepOrder (S4).
func TestSave_StepOrder(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	var steps []string
	h.e.onStep = func(s string) { steps = append(steps, s) }
	h.save(t, SaveRequest{Contents: true})
	want := []string{"lock", "read", "ps", "recordings", "registry", "sidmap", "contents", "validate", "guard", "publish", "post", "retention"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps %v\nwant  %v", steps, want)
	}
}

// TestSave_ClaudishReplayInjectsLiveModel (B11, B12, R9, R10): an interactive
// claudish pick survives only in the Claude child's environment; it is read
// with one single-pid ps eww and injected into the replay.
func TestSave_ClaudishReplayInjectsLiveModel(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	p := w.pane(ids["d1"][0])
	launcher := w.child(p.pid, "node /home/user/.bun/bin/claudish -d")
	cl := w.child(launcher, "claude --dangerously-skip-permissions")
	w.env[cl] = "claude --dangerously-skip-permissions PATH=/bin CLAUDISH_ACTIVE_MODEL_NAME=cx@gpt-5.6-sol"
	h.register(t, cl, sid1)
	h.record(t, p.id, "c --worktree foo")
	h.save(t, SaveRequest{})
	got := paneByID(h.last(t), p.id)
	if got.Role != snapshot.RoleClaude || got.Claude == nil || got.Claude.SessionID != sid1 ||
		got.Claude.Launcher != "claudish" || got.Claude.Replay != "-d --model cx@gpt-5.6-sol" || got.Launch != "c --worktree foo" {
		t.Fatalf("%+v %+v", got, got.Claude)
	}
}

// TestSave_PsEwwSinglePidOnly (B12): ps eww runs once per model-less claudish
// pane, one pid at a time, and never for a launcher that names its model.
func TestSave_PsEwwSinglePidOnly(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	var want []int
	for i, id := range ids["a1"] {
		p := w.pane(id)
		args := "-d"
		if i == 2 {
			args = "--model explicit -d"
		}
		launcher := w.child(p.pid, "node /home/user/.bun/bin/claudish "+args)
		cl := w.child(launcher, "claude")
		h.register(t, cl, []string{sid1, sid2, sid3}[i])
		if i < 2 {
			want = append(want, cl)
		}
	}
	plain := w.child(w.pane(ids["d1"][0]).pid, "claude")
	h.register(t, plain, "dddddddd-0000-4000-8000-000000000004")
	h.save(t, SaveRequest{})
	if !reflect.DeepEqual(w.envCalls, want) {
		t.Fatalf("ps eww for %v, want exactly %v (one pid per call)", w.envCalls, want)
	}
}

// TestSave_RecordingAttachedByPaneID (B13, B15): launch/<N> is found by the
// pane's id even after its position changed.
func TestSave_RecordingAttachedByPaneID(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	second := ids["a1"][1]
	h.record(t, second, "npm run dev\nsecond line\n")
	w.killPane(t, ids["a1"][0]) // second is now api.v2:1.1
	h.save(t, SaveRequest{})
	got := paneByID(h.last(t), second)
	if got.Key != "api.v2:1.1" || got.Launch != "npm run dev" || got.Role != snapshot.RoleShell {
		t.Fatalf("%+v", got)
	}
	if p := paneByID(h.last(t), ids["a1"][2]); p.Launch != "" {
		t.Fatalf("another pane got the recording: %+v", p)
	}
}

// TestSave_EmptyRecordingIsAbsent (B15).
func TestSave_EmptyRecordingIsAbsent(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	h.record(t, ids["d1"][0], "")
	h.record(t, ids["d1"][1], "\n")
	h.save(t, SaveRequest{})
	for _, id := range ids["d1"] {
		if p := paneByID(h.last(t), id); p.Launch != "" {
			t.Fatalf("%s: %q", id, p.Launch)
		}
	}
}

// TestSave_TmuxMCPHelperIsLayoutOnly (B14, R20): an agent helper pane is saved
// with no command and no sid.
func TestSave_TmuxMCPHelperIsLayoutOnly(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	p := w.pane(ids["d1"][1])
	p.mcpOwner, p.mcpPane, p.cmd = "agent", p.id, "claude"
	cl := w.child(p.pid, "claude --model haiku -p scratch")
	h.register(t, cl, sid2)
	h.record(t, p.id, "cd /tmp/x && claude --model haiku")
	h.save(t, SaveRequest{})
	got := paneByID(h.last(t), p.id)
	if got.Role != snapshot.RoleMCPHelper || got.Claude != nil || got.Launch != "" || got.FullCommand != "" {
		t.Fatalf("%+v", got)
	}
}

// TestSave_AcquiredPaneKept (B14): a pane tmux-mcp adopted is the user's.
func TestSave_AcquiredPaneKept(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	p := w.pane(ids["d1"][1])
	p.mcpOwner, p.mcpPane = "acquired", p.id
	cl := w.child(p.pid, "claude")
	h.register(t, cl, sid2)
	h.save(t, SaveRequest{})
	got := paneByID(h.last(t), p.id)
	if got.Role != snapshot.RoleClaude || got.SID() != sid2 {
		t.Fatalf("%+v", got)
	}
	// a leaked option naming another pane is not a helper either
	q := w.pane(ids["d2"][0])
	q.mcpOwner, q.mcpPane = "agent", "%999"
	h.save(t, SaveRequest{Contents: true})
	if got := paneByID(h.last(t), q.id); got.Role != snapshot.RoleShell {
		t.Fatalf("%+v", got)
	}
}

// TestSave_FreshEachTimeIdempotent (B16): a snapshot is built from live state
// every time; saving the same server twice publishes once, and nothing
// accumulates.
func TestSave_FreshEachTimeIdempotent(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	cl := w.child(w.pane(ids["d1"][0]).pid, "claude")
	h.register(t, cl, sid1)
	first := h.save(t, SaveRequest{})
	second := h.save(t, SaveRequest{})
	if !first.Published || second.Published || !second.Unchanged() || first.SnapshotFP != second.SnapshotFP {
		t.Fatalf("%+v / %+v", first, second)
	}
	s := h.last(t)
	if len(s.Sids()) != 1 || len(s.Panes) != 6 {
		t.Fatalf("%+v", s.Panes)
	}
}

// TestStore_UnchangedNotRepublishedAcrossTimes (S8): equal structure and
// equal contents are `unchanged` whatever the time between them.
func TestStore_UnchangedNotRepublishedAcrossTimes(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	w.pane(ids["d1"][0]).contents = "$ ls\nfile\n"
	if r := h.save(t, SaveRequest{Contents: true}); !r.Published {
		t.Fatal(r)
	}
	for _, gap := range []int{1, 60, 60 * 24} {
		h.clock = h.clock.Add(time.Duration(gap) * time.Minute)
		r := h.save(t, SaveRequest{Contents: true})
		if r.Published {
			t.Fatalf("republished after %d min with nothing changed", gap)
		}
	}
	names, _ := os.ReadDir(h.e.paths.Snapshots)
	n := 0
	for _, e := range names {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d snapshots on disk", n)
	}
	if _, ok := w.opts["@moox-want-contents"]; ok {
		t.Fatal("want-contents not cleared by an unchanged contents save")
	}
}

// TestSave_ContentsReuseKeyedByPaneID (§6.1, R2-C7): capture A and B, kill A
// (B becomes S:W.1), structural save: B keeps B's text, A's entry is unused.
func TestSave_ContentsReuseKeyedByPaneID(t *testing.T) {
	w := newWorld()
	w.session("s")
	_, ids := w.window("s", 1, 2)
	a, b := ids[0], ids[1]
	w.pane(a).contents = "A's scrollback\n"
	w.pane(b).contents = "B's scrollback\n"
	h := newHarness(t, w)
	h.save(t, SaveRequest{Contents: true})
	w.killPane(t, a)
	res := h.save(t, SaveRequest{}) // structural: no capture
	if res.ContentsCaptured || !res.Published {
		t.Fatalf("%+v", res)
	}
	s := h.last(t)
	pb := paneByID(s, b)
	if len(s.Panes) != 1 || pb.Key != "s:1.1" || !pb.Contents || s.Contents == nil {
		t.Fatalf("%+v %+v", s.Panes, s.Contents)
	}
	entries, err := h.store(t).LoadContents(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[pb.SavedID]) != "B's scrollback\n" {
		t.Fatalf("pane %s (now s:1.1) would get %q", pb.SavedID, entries[pb.SavedID])
	}
	if s.Contents.Panes != 1 {
		t.Fatalf("contents panes %d", s.Contents.Panes)
	}
}

// TestSave_ContentsNotReusedAcrossServerLives (§6.1): the same pane ids in a
// new server life are other panes.
func TestSave_ContentsNotReusedAcrossServerLives(t *testing.T) {
	w := newWorld()
	w.session("s")
	_, ids := w.window("s", 1, 2)
	w.pane(ids[0]).contents = "old life\n"
	h := newHarness(t, w)
	h.save(t, SaveRequest{Contents: true})
	w.pid, w.start = 999, w.start+3600 // reboot: same ids, new server
	w.pane(ids[0]).title = "x"
	h.save(t, SaveRequest{})
	s := h.last(t)
	if s.Contents != nil {
		t.Fatalf("reused another server life's contents: %+v", s.Contents)
	}
	for _, p := range s.Panes {
		if p.Contents {
			t.Fatalf("%s marked contents", p.Key)
		}
	}
}

// TestSave_ContentsCadence (S8): a structural (debounce) save does not
// capture; the detach flag and an explicit request do, and the flag is
// cleared by the save that honoured it.
func TestSave_ContentsCadence(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	captures := func() int {
		n := 0
		for _, c := range w.calls {
			if strings.Contains(strings.Join(c, " "), "capture-pane") {
				n++
			}
		}
		return n
	}
	h.save(t, SaveRequest{Reason: "debounce"})
	if captures() != 0 {
		t.Fatal("a debounce save captured contents")
	}
	w.opts["@moox-want-contents"] = "1"
	w.pane(ids["d1"][0]).contents = "detach\n"
	if r := h.save(t, SaveRequest{Reason: "debounce"}); !r.ContentsCaptured || captures() != 1 {
		t.Fatalf("detach flag ignored: %+v", r)
	}
	if _, ok := w.opts["@moox-want-contents"]; ok {
		t.Fatal("flag not cleared")
	}
	w.pane(ids["d1"][0]).contents = "request\n"
	if r := h.save(t, SaveRequest{Reason: "request", Contents: true}); !r.ContentsCaptured || captures() != 2 {
		t.Fatalf("request ignored: %+v", r)
	}
}

// TestSave_ContentsOption (I9): @moox-capture-contents off disables capture.
func TestSave_ContentsOption(t *testing.T) {
	w, _ := workspace()
	w.opts["@moox-capture-contents"] = "off"
	h := newHarness(t, w)
	if r := h.save(t, SaveRequest{Contents: true}); r.ContentsCaptured || h.last(t).Contents != nil {
		t.Fatalf("%+v", r)
	}
	w.opts["@moox-capture-contents"] = "on"
	w.opts["@moox-capture-lines"] = "500"
	h.save(t, SaveRequest{Contents: true})
	found := false
	for _, c := range w.calls {
		if j := strings.Join(c, " "); strings.Contains(j, "capture-pane") {
			found = strings.Contains(j, "-S -500")
		}
	}
	if !found {
		t.Fatal("@moox-capture-lines not honoured")
	}
}

// TestSave_ContentsAllRolesFullHistory (B46, PR-C10): every pane restored as
// a pane is captured with full history (claude, programs, helpers), except
// tt-observer and frozen rows.
func TestSave_ContentsAllRolesFullHistory(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	obs := w.pane(ids["d1"][1])
	obs.ttObs = "1"
	frozen := w.pane(ids["d2"][0])
	frozen.title = "❄ FROZEN k1 1p/1s 2026-10-20"
	prog := w.pane(ids["a1"][0])
	prog.cmd = "nvim"
	cl := w.child(w.pane(ids["a1"][1]).pid, "claude")
	h.register(t, cl, sid1)
	h.save(t, SaveRequest{Contents: true})
	var captured []string
	for _, c := range w.calls {
		for i, a := range c {
			if a == "capture-pane" {
				captured = append(captured, c[i+3])
				if c[i+4] != "-S" || c[i+5] != "-" || c[i+1] != "-epJ" {
					t.Fatalf("not full history: %q", c[i:i+6])
				}
			}
		}
	}
	want := []string{ids["d1"][0], ids["a1"][0], ids["a1"][1], ids["a1"][2]}
	if !reflect.DeepEqual(captured, want) {
		t.Fatalf("captured %v, want %v", captured, want)
	}
	s := h.last(t)
	if paneByID(s, obs.id).Contents || paneByID(s, frozen.id).Contents || !paneByID(s, prog.id).Contents {
		t.Fatal("contents flags")
	}
	if paneByID(s, frozen.id).FrozenKey != "k1" || paneByID(s, obs.id).Role != snapshot.RoleTTObserver {
		t.Fatal("roles")
	}
}

func TestSave_CaptureChainResumesAfterDeadPane(t *testing.T) {
	w, ids := workspace()
	for i, id := range append(append([]string{}, ids["d1"]...), ids["a1"]...) {
		w.pane(id).contents = fmt.Sprintf("text %d\n", i)
	}
	w.failCapture[ids["d1"][1]] = true // died between the read and the capture
	h := newHarness(t, w)
	h.save(t, SaveRequest{Contents: true})
	s := h.last(t)
	if paneByID(s, ids["d1"][1]).Contents || !paneByID(s, ids["a1"][2]).Contents || !paneByID(s, ids["d1"][0]).Contents {
		t.Fatalf("%+v", s.Panes)
	}
	if !strings.Contains(h.log.String(), "CONTENTS-SKIP "+ids["d1"][1]) {
		t.Fatal(h.log.String())
	}
}

// TestSave_ByPidGC_SafeAcrossServers (X12): another server's live entry is
// kept; dead and recycled entries are removed; one that started after the ps
// is left alone.
func TestSave_ByPidGC_SafeAcrossServers(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	other := w.child(1, "claude") // a Claude in some other server's pane
	h.register(t, other, sid1)
	h.mustWrite(t, filepath.Join(h.byPid(), fmt.Sprint(other)+".start"), "Sat Oct 25 03:00:00 2026\n")
	dead := 77777
	w.dead[dead] = true
	h.register(t, dead, sid2)
	h.mustWrite(t, filepath.Join(h.byPid(), "77777.start"), "Fri Oct 24 01:00:00 2026\n")
	recycled := w.child(w.pane(ids["d1"][0]).pid, "claude")
	h.register(t, recycled, sid3)
	h.mustWrite(t, filepath.Join(h.byPid(), fmt.Sprint(recycled)+".start"), "Mon Jan  1 00:00:00 2024\n")
	late := 88888
	w.extraAlive[late] = true
	h.register(t, late, "eeeeeeee-0000-4000-8000-000000000005")

	h.save(t, SaveRequest{})
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(h.byPid(), name))
		return err == nil
	}
	if !exists(fmt.Sprint(other)+".session-id") || !exists(fmt.Sprint(other)+".start") {
		t.Fatal("another server's live entry was removed")
	}
	if exists("77777.session-id") || exists("77777.start") {
		t.Fatal("dead entry kept")
	}
	if exists(fmt.Sprint(recycled)+".session-id") || paneByID(h.last(t), ids["d1"][0]).SID() != "" {
		t.Fatal("recycled pid trusted")
	}
	if !exists("88888.session-id") {
		t.Fatal("a pid that started after the ps was removed")
	}
}

// TestSave_GroupedSecondaryRowsDropped (S5).
func TestSave_GroupedSecondaryRowsDropped(t *testing.T) {
	w := newWorld()
	w.sessions = append(w.sessions, fSession{id: "$2", name: "api", group: "g", grouped: true},
		fSession{id: "$5", name: "peek", group: "g", grouped: true})
	w.best = "api"
	wid, ids := w.window("api", 1, 2)
	for _, p := range append([]fPane(nil), w.panes...) { // list-panes -a lists them once per member
		p.session = "peek"
		w.panes = append(w.panes, p)
	}
	w.windows = append(w.windows, fWindow{id: wid, session: "peek", index: 1, name: "zsh", active: true, layout: w.windows[0].layout, w: 160, h: 40})
	cl := w.child(w.pane(ids[0]).pid, "claude")
	h := newHarness(t, w)
	h.register(t, cl, sid1)
	h.save(t, SaveRequest{})
	s := h.last(t)
	if len(s.Panes) != 2 || len(s.Windows) != 1 || len(s.Sessions) != 2 {
		t.Fatalf("%d panes %d windows %d sessions", len(s.Panes), len(s.Windows), len(s.Sessions))
	}
	if s.Sessions[1].Name != "peek" || s.Sessions[1].GroupedWith != "api" || s.Sessions[0].Group != "g" {
		t.Fatalf("%+v", s.Sessions)
	}
	if p := paneByID(s, ids[0]); p.Session != "api" || p.SID() != sid1 {
		t.Fatalf("%+v", p)
	}
}

// TestSave_ObserverRole (B45, S10).
func TestSave_ObserverRole(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	p := w.pane(ids["d1"][1])
	p.start = `/bin/sh -c "tt observe --pane --auto; exit"`
	p.cmd = "tt"
	q := w.pane(ids["a1"][2])
	q.ttObs = "1"
	h.save(t, SaveRequest{})
	s := h.last(t)
	if paneByID(s, p.id).Role != snapshot.RoleTTObserver || paneByID(s, q.id).Role != snapshot.RoleTTObserver {
		t.Fatalf("%+v", s.Panes)
	}
	if s.RestoredPanes() != 4 {
		t.Fatalf("restored panes %d", s.RestoredPanes())
	}
}

// TestSave_FullCommandRule (R3, PR-M12).
func TestSave_FullCommandRule(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	p := w.pane(ids["d1"][0])
	opRun := w.child(p.pid, "op run --env-file x -- claude")
	w.child(opRun, "claude")
	w.child(p.pid, "a second child")
	q := w.pane(ids["d2"][0])
	for i := range w.procs {
		if w.procs[i].pid == q.pid {
			w.procs[i].cmd = "claude --dangerously-skip-permissions" // Claude exec'd as the pane process
		}
	}
	h.register(t, q.pid, sid1)
	h.save(t, SaveRequest{})
	s := h.last(t)
	if got := paneByID(s, p.id).FullCommand; got != "op run --env-file x -- claude" {
		t.Errorf("depth-1 child: %q", got)
	}
	if got := paneByID(s, q.id); got.FullCommand != "claude --dangerously-skip-permissions" || got.SID() != sid1 {
		t.Errorf("depth 0: %+v", got)
	}
}

// TestSave_DetachedKeepsClientSession (RR16): with no client, client.session
// is tmux's best session and last_session the previous snapshot's, if it
// still exists.
func TestSave_DetachedKeepsClientSession(t *testing.T) {
	w, _ := workspace()
	w.clients = []fClient{{session: "dotfiles", last: "api.v2", activity: 10}, {session: "api.v2", last: "", activity: 5}}
	h := newHarness(t, w)
	h.save(t, SaveRequest{})
	if c := h.last(t).Client; c.Session != "dotfiles" || c.LastSession != "api.v2" {
		t.Fatalf("attached: %+v", c)
	}
	w.clients = nil
	w.best = "api.v2"
	h.save(t, SaveRequest{})
	if c := h.last(t).Client; c.Session != "api.v2" || c.LastSession != "api.v2" {
		t.Fatalf("detached: %+v", c)
	}
}

// TestSave_PristineNeverReplacesRestorable (A15, R2-C3): a server holding only
// its bootstrap pane never replaces a restorable moox-last, unless Force.
func TestSave_PristineNeverReplacesRestorable(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	good := h.save(t, SaveRequest{})
	// the server restarts with only its bootstrap (cold start, handover, ...)
	boot := newWorld()
	boot.session("main")
	boot.window("main", 1, 1)
	h.w, h.e.tmux, h.e.procs = boot, boot, boot
	res := h.save(t, SaveRequest{Reason: "debounce"})
	if !res.Pristine || res.Published {
		t.Fatalf("%+v", res)
	}
	if l, _ := h.e.Last(); l.Name != good.File || len(l.Snapshot.Windows) != 3 {
		t.Fatalf("moox-last replaced: %+v", l)
	}
	// a bootstrap that runs something, or holds a sid, is not pristine
	h.record(t, boot.panes[0].id, "make serve")
	if r := h.save(t, SaveRequest{}); !r.Published {
		t.Fatalf("a recording pane is not the bootstrap: %+v", r)
	}
	// and with nothing restorable to protect, or with Force, it publishes
	h2 := newHarness(t, newWorld())
	h2.w.session("main")
	h2.w.window("main", 1, 1)
	if r := h2.save(t, SaveRequest{}); !r.Published {
		t.Fatalf("first save of a fresh server: %+v", r)
	}
	h3 := newHarness(t, w)
	h3.save(t, SaveRequest{})
	h3.w, h3.e.tmux, h3.e.procs = boot, boot, boot
	boot.panes[0].id = "%77" // no recording for this one
	if r := h3.save(t, SaveRequest{Force: true}); !r.Published {
		t.Fatalf("Force: %+v", r)
	}
}

func TestSave_RestoringRefusals(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	live := w.child(1, "tt serve --foreground")

	w.opts["@moox-restoring"] = fmt.Sprintf("boot:%d:1", live)
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrRestoring) {
		t.Fatalf("live boot: %v", err)
	}
	w.opts["@moox-restoring"] = fmt.Sprintf("shutdown:%d:1", live)
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("live shutdown: %v", err)
	}
	if r, err := h.e.Save(context.Background(), SaveRequest{ShutdownOwner: live}); err != nil || !r.Published {
		t.Fatalf("the shutdown owner's own save: %+v %v", r, err)
	}
	w.opts["@moox-restoring"] = "garbage"
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrRestoring) {
		t.Fatalf("unparsable: %v", err)
	}
	// a dead restorer is cleared, and its restoring-from becomes a pin (RR4)
	st := h.store(t)
	l, err := st.TryLock(store.Record{Pid: 1})
	if err != nil {
		t.Fatal(err)
	}
	src, _ := h.e.Last()
	if err := st.BeginAttempt(l, src.Name); err != nil {
		t.Fatal(err)
	}
	l.Release()
	w.dead[4444] = true
	w.opts["@moox-restoring"] = "restore:4444:99"
	w.panes[0].title = "changed"
	if _, err := h.e.Save(context.Background(), SaveRequest{}); err != nil {
		t.Fatalf("dead restorer: %v", err)
	}
	if _, ok := w.opts["@moox-restoring"]; ok {
		t.Fatal("dead @moox-restoring not cleared")
	}
	if pins, _ := st.Pins(); !reflect.DeepEqual(pins, []string{src.Name}) {
		t.Fatalf("pins %v", pins)
	}
}

func TestSave_NotActiveAndBusy(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	w.opts["@moox-active"] = "0"
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrNotActive) {
		t.Fatalf("%v", err)
	}
	w.opts["@moox-active"] = "1"
	st := h.store(t)
	l, err := st.TryLock(store.Record{Pid: 31337})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	n := len(w.calls)
	_, err = h.e.Save(context.Background(), SaveRequest{})
	var be *BusyError
	if !errors.Is(err, ErrBusy) || !errors.As(err, &be) || be.Pid != 31337 {
		t.Fatalf("%v", err)
	}
	if len(w.calls) != n {
		t.Fatal("a busy save talked to tmux (the lock comes first)")
	}
}

// B19 at the engine: a live state that fails Validate (an injected sid) is
// never published.
func TestSave_InvalidNotPublished(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	good := h.save(t, SaveRequest{})
	cl := w.child(w.pane(ids["d1"][0]).pid, "claude")
	h.register(t, cl, "0000; touch /tmp/pwn")
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
	if l, _ := h.e.Last(); l.Name != good.File {
		t.Fatal("moox-last moved")
	}
}

// B20 at the engine: a wipe of every sid with an empty freeze store is
// blocked; freezing all of them first lets the save through.
func TestSave_ClobberGuard(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	var cls []int
	for i, id := range ids["a1"] {
		cl := w.child(w.pane(id).pid, "claude")
		h.register(t, cl, []string{sid1, sid2, sid3}[i])
		cls = append(cls, cl)
	}
	good := h.save(t, SaveRequest{})
	for _, cl := range cls {
		w.dead[cl] = true
	}
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("%v", err)
	}
	if l, _ := h.e.Last(); l.Name != good.File || w.opts["@moox_status"] != "save blocked" {
		t.Fatal("moox-last moved or status not set")
	}
	h.mustWrite(t, filepath.Join(h.e.paths.FreezeDir, "k.state"), ";CLAUDE_SID="+sid1+" ;CLAUDE_SID="+sid2+"\n;CLAUDE_SID="+sid3+"\n")
	if r := h.save(t, SaveRequest{}); !r.Published {
		t.Fatalf("%+v", r)
	}
}

func TestSave_ExcludePane(t *testing.T) {
	w, ids := workspace()
	h := newHarness(t, w)
	h.save(t, SaveRequest{ExcludePane: ids["d1"][1]})
	s := h.last(t)
	if paneByID(s, ids["d1"][1]) != nil || len(s.Panes) != 5 {
		t.Fatalf("%+v", s.Panes)
	}
	h.save(t, SaveRequest{ExcludePane: ids["d2"][0]}) // a window's only pane: the window goes
	if s := h.last(t); len(s.Windows) != 2 || len(s.Panes) != 5 {
		t.Fatalf("%d windows %d panes", len(s.Windows), len(s.Panes))
	}
}

// TestSave_NewerFormatMooxLastNeverSuperseded (G1-H4): after a downgrade,
// moox-last names a snapshot this binary cannot read. A save refuses with
// ErrNewerFormat ("upgrade moox") and leaves the pointer and the directory
// alone, instead of treating the workspace as absent and publishing over it.
func TestSave_NewerFormatMooxLastNeverSuperseded(t *testing.T) {
	w, _ := workspace()
	h := newHarness(t, w)
	good := h.save(t, SaveRequest{})
	f := filepath.Join(h.e.paths.Snapshots, good.File)
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	nb := strings.Replace(string(b), fmt.Sprintf(`"version": %d`, snapshot.Version), `"version": 99`, 1)
	if nb == string(b) {
		t.Fatalf("no version field: %s", b)
	}
	h.mustWrite(t, f, nb)
	before, _ := os.ReadDir(h.e.paths.Snapshots)
	w.window("dotfiles", 3, 1) // a real change
	h.clock = h.clock.Add(time.Minute)
	if _, err := h.e.Save(context.Background(), SaveRequest{}); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("save over a newer-format moox-last: %v, want ErrNewerFormat\n%s", err, h.log.String())
	}
	after, _ := os.ReadDir(h.e.paths.Snapshots)
	if len(after) != len(before) {
		t.Fatalf("files %d -> %d: a snapshot was published over the newer one", len(before), len(after))
	}
	if p := h.store(t).Pointer(); p != good.File {
		t.Fatalf("moox-last moved to %q", p)
	}
}
