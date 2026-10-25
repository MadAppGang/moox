package saver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
)

// TestSaver_CeilingClampsDebounce (R2-M8, V2): dirty events every 400 ms
// until 29.6 s, then silence. The debounce (3 s) never gets to fire, and the
// one save lands AT the 30 s ceiling — not at 29.6 + 3 = 32.6 s.
func TestSaver_CeilingClampsDebounce(t *testing.T) {
	h := newHarness(t)
	for at := time.Duration(0); at <= 29600*time.Millisecond; at += 400 * time.Millisecond {
		h.advance(at - h.elapsed())
		h.m.onDirty()
	}
	h.advance(10 * time.Second)
	got := h.saveTimes()
	if len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("saves at %v, want exactly one at 30s", got)
	}
	if r := h.eng.saves[0].req.Reason; r != "max-wait" {
		t.Fatalf("the ceiling save is logged %q, want max-wait", r)
	}
	h.idle()

	// A stream that never stops saves AT every ceiling, by the dirty event
	// that finds the ceiling passed or by the clamped timer.
	h2 := newHarness(t)
	for at := time.Duration(0); at <= 70*time.Second; at += 400 * time.Millisecond {
		h2.advance(at - h2.elapsed())
		h2.m.onDirty()
	}
	if got := h2.saveTimes(); len(got) < 2 || got[0] > 30*time.Second || got[1]-got[0] > 30*time.Second+400*time.Millisecond {
		t.Fatalf("a steady stream saved at %v, want one at least every ~30s", got)
	}
}

// TestSaver_DebounceCollapsesABurst (V2's unit half): a burst of events is
// one save, Debounce after the last.
func TestSaver_DebounceCollapsesABurst(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 10; i++ {
		h.m.onDirty()
		h.advance(100 * time.Millisecond)
	}
	h.advance(10 * time.Second)
	if got := h.saveTimes(); len(got) != 1 || got[0] != 900*time.Millisecond+3*time.Second {
		t.Fatalf("saves at %v, want one at 3.9s", got)
	}
	if r := h.eng.saves[0].req; r.Contents || r.Reason != "debounce" {
		t.Fatalf("debounce save %+v, want reason debounce without contents", r)
	}
	h.idle()
}

// TestSaver_IdleHasNoArmedTimer (tt §9.3a, R2-C4): after every quiescent
// sequence with no client attached, no timer exists.
func TestSaver_IdleHasNoArmedTimer(t *testing.T) {
	seqs := map[string]func(h *harness){
		"nothing":          func(h *harness) {},
		"dirty":            func(h *harness) { h.m.onDirty() },
		"burst":            func(h *harness) { h.m.onDirty(); h.m.onDirty(); h.advance(time.Second); h.m.onDirty() },
		"touch":            func(h *harness) { h.m.onTouch() },
		"touch then dirty": func(h *harness) { h.m.onTouch(); h.m.onDirty() },
		"detach":           func(h *harness) { h.tmux.set("@moox-want-contents", "1"); h.m.onDirty() },
		"key save":         func(h *harness) { h.m.onSaveRequest() },
		"busy then free": func(h *harness) {
			h.eng.result = func(n int, _ engine.SaveRequest) (engine.SaveResult, error) {
				if n == 1 {
					return engine.SaveResult{}, &engine.BusyError{Pid: 9}
				}
				return engine.SaveResult{}, nil
			}
			h.m.onDirty()
		},
	}
	for name, seq := range seqs {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			seq(h)
			h.advance(time.Hour)
			h.idle()
		})
	}
}

// TestSaver_BackupReArmsWhileAttached (R2-C4, continuum parity): a client
// attached, continuous output, no touch after the first: three intervals →
// three contents captures; the chain re-arms itself while attached.
func TestSaver_BackupReArmsWhileAttached(t *testing.T) {
	h := newHarness(t)
	h.eng.attached = 1
	h.m.onTouch() // client-attached
	h.advance(45 * time.Minute)
	got := h.saveTimes()
	if len(got) != 3 || got[0] != 15*time.Minute || got[1] != 30*time.Minute || got[2] != 45*time.Minute {
		t.Fatalf("backup saves at %v, want 15m 30m 45m", got)
	}
	for _, s := range h.eng.saves {
		if !s.req.Contents || s.req.Reason != "backup" {
			t.Fatalf("backup save %+v did not ask for contents", s.req)
		}
	}
	if h.m.armedTimers() != 1 {
		t.Fatalf("%d timers armed while attached, want the one backup", h.m.armedTimers())
	}
	// Touches while armed never extend or duplicate the chain.
	h.m.onTouch()
	h.m.onTouch()
	if h.clk.live() != 1 {
		t.Fatalf("%d live timers after touches, want 1", h.clk.live())
	}
}

// TestSaver_BackupStopsWhenDetached: the firing whose save sees no client
// ends the chain.
func TestSaver_BackupStopsWhenDetached(t *testing.T) {
	h := newHarness(t)
	h.eng.attached = 1
	h.m.onTouch()
	h.advance(15 * time.Minute) // fires attached → re-armed
	h.eng.attached = 0
	h.advance(15 * time.Minute) // fires detached → ends
	h.advance(2 * time.Hour)
	if got := h.saveTimes(); len(got) != 2 {
		t.Fatalf("backup saves at %v, want 2", got)
	}
	h.idle()
}

// TestSaver_DetachRequestsContents (R2-C4): the client-detached hook sets
// @moox-want-contents and signals moox-dirty; the saver answers it with one
// debounced save, and the engine's chained read turns that save into a
// contents save (asserted on a real server by TestE2E_V2_DetachSaveCarriesContents).
// The saver itself never forces contents on a debounce save, so a dirty
// event without the flag captures nothing new.
func TestSaver_DetachRequestsContents(t *testing.T) {
	h := newHarness(t)
	var sawFlag []bool
	h.eng.result = func(_ int, r engine.SaveRequest) (engine.SaveResult, error) {
		sawFlag = append(sawFlag, r.Contents || h.tmux.opt("@moox-want-contents") == "1")
		h.tmux.set("@moox-want-contents", "")
		return engine.SaveResult{Published: true, File: "x"}, nil
	}
	h.tmux.set("@moox-want-contents", "1") // the hook's `set -g`
	h.m.onDirty()                          // the hook's `wait-for -S moox-dirty`
	h.advance(5 * time.Second)
	h.m.onDirty() // a later structural change, no flag
	h.advance(5 * time.Second)
	if fmt.Sprint(sawFlag) != "[true false]" {
		t.Fatalf("contents per save %v, want [true false]", sawFlag)
	}
	for _, s := range h.eng.saves {
		if s.req.Contents {
			t.Fatalf("a debounce save forced contents: %+v", s.req)
		}
	}
	h.idle()
}

// TestSaver_TwoDriftsSeparatedByCleanBackup (V2): touch → the backup fires
// on a clean server → unchanged; touch → drift → fire → one save.
func TestSaver_TwoDriftsSeparatedByCleanBackup(t *testing.T) {
	h := newHarness(t)
	published := 0
	h.eng.result = func(n int, _ engine.SaveRequest) (engine.SaveResult, error) {
		if n == 1 {
			return engine.SaveResult{SnapshotFP: "a"}, nil // unchanged
		}
		published++
		return engine.SaveResult{Published: true, File: "f", SnapshotFP: "b"}, nil
	}
	h.m.onTouch()
	h.advance(16 * time.Minute)
	h.m.onTouch()
	h.advance(16 * time.Minute)
	if got := h.saveTimes(); len(got) != 2 || published != 1 {
		t.Fatalf("saves %v published %d, want 2 saves and 1 publish", got, published)
	}
	if h.m.snapshotFP != "b" {
		t.Fatalf("snapshotFP %q, want the published one", h.m.snapshotFP)
	}
	h.idle()
}

// TestSaver_BusyDoesNotPollForever (§5.6): another process holds the lock
// forever. The saver retries 3, 6, 12, 24, 48 s after the busy results, then
// gives up with a status line, and holds no timer until the next event.
func TestSaver_BusyDoesNotPollForever(t *testing.T) {
	h := newHarness(t)
	h.eng.result = func(int, engine.SaveRequest) (engine.SaveResult, error) {
		return engine.SaveResult{}, &engine.BusyError{Pid: 4242}
	}
	h.m.onDirty()
	h.advance(10 * time.Minute)
	// the debounce (3 s), then the five busy retries 3, 6, 12, 24, 48 s
	want := []time.Duration{3 * time.Second, 3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second,
		48 * time.Second}
	var gaps []time.Duration
	prev := time.Duration(0)
	for _, at := range h.saveTimes() {
		gaps = append(gaps, at-prev)
		prev = at
	}
	if fmt.Sprint(gaps) != fmt.Sprint(want) {
		t.Fatalf("busy retry gaps %v, want %v", gaps, want)
	}
	h.idle()
	if got := h.tmux.opt("@moox_status"); got != "save busy (lock pid 4242)" {
		t.Fatalf("@moox_status %q", got)
	}
	if !strings.Contains(h.logText(), "BUSY-GAVE-UP") {
		t.Fatalf("no BUSY-GAVE-UP:\n%s", h.logText())
	}
	// The next dirty event restarts the cycle.
	h.eng.result = nil
	h.m.onDirty()
	h.advance(time.Minute)
	if n := h.eng.saveCount(); n != 7 {
		t.Fatalf("%d saves after the next event, want 7", n)
	}
	h.idle()
}

// TestSaver_UnparsableRestoringIsRestoring (RR4, §5.4): a save refused
// because ANOTHER process owns the server (a live or unparsable
// @moox-restoring) keeps the server dirty and arms nothing: that process
// ends by signalling moox-dirty, and the next event saves.
func TestSaver_UnparsableRestoringIsRestoring(t *testing.T) {
	for _, refusal := range []error{engine.ErrRestoring, engine.ErrShuttingDown} {
		h := newHarness(t)
		h.eng.result = func(n int, _ engine.SaveRequest) (engine.SaveResult, error) {
			if n == 1 {
				return engine.SaveResult{}, fmt.Errorf("%w (@moox-restoring \"garbage\" does not parse)", refusal)
			}
			return engine.SaveResult{Published: true, File: "f"}, nil
		}
		h.m.onDirty()
		h.advance(time.Minute)
		if !h.m.dirty {
			t.Fatalf("%v: a refused save cleared dirty", refusal)
		}
		h.idle()
		h.m.onDirty() // the restorer's closing signal
		h.advance(time.Minute)
		if n := h.eng.saveCount(); n != 2 || h.m.dirty {
			t.Fatalf("%v: %d saves, dirty=%v after the closing signal", refusal, n, h.m.dirty)
		}
	}
}

// TestSaver_SuppressedDuringRestore (B29, D10, G2, I7, PR-C3): from the
// moment a restore is decided until RESTORE-END no save runs and no timer is
// armed; dirty events collapse into ONE save afterwards; save requests are
// answered "restoring" without saving.
func TestSaver_SuppressedDuringRestore(t *testing.T) {
	h := newHarness(t)
	h.eng.attached = 1
	h.m.onTouch() // a backup armed before the restore
	h.m.onDirty() // and a debounce
	h.eng.boot = true
	h.m.startBoot(Boot{Stub: true})
	if !h.m.restoring || h.m.armedTimers() != 0 {
		t.Fatalf("restoring=%v armed=%d after the boot decision", h.m.restoring, h.m.armedTimers())
	}
	for i := 0; i < 20; i++ {
		h.m.onDirty()
		h.m.onTouch()
		h.advance(time.Second)
	}
	h.tmux.set(SaveReqPrefix+"aaaa", FormatSaveRequest(1, false, "freeze"))
	h.m.onSaveRequest()
	h.advance(time.Hour)
	if n := h.eng.saveCount(); n != 0 {
		t.Fatalf("%d saves during the restore", n)
	}
	if got := h.tmux.opt(SaveResPrefix + "aaaa"); !strings.HasSuffix(got, " restoring") {
		t.Fatalf("request answered %q, want restoring", got)
	}
	if h.tmux.signalled(SavedChanPrefix+"aaaa") != 1 {
		t.Fatal("the requester's channel was not signalled")
	}
	h.idle()
	h.eng.launch <- engine.LaunchVerdict{Result: "PASS"}
	h.endRestore()
	h.advance(10 * time.Second)
	if n := h.eng.saveCount(); n != 1 {
		t.Fatalf("%d saves after RESTORE-END, want exactly 1", n)
	}
	if got := h.eng.restores; len(got) != 1 || got[0].Kind != engine.KindBoot || !got[0].Stub {
		t.Fatalf("restores %+v", got)
	}
}

// TestSaver_BootNoBootstrapSkips (R2-M3): the bounded poll finds no
// bootstrap → SKIPPED:no-bootstrap, nothing restored, saving resumes.
func TestSaver_BootNoBootstrapSkips(t *testing.T) {
	h := newHarness(t)
	h.eng.boot = false
	h.m.startBoot(Boot{})
	h.m.onDirty()
	h.endRestore()
	h.advance(10 * time.Second)
	if fmt.Sprint(h.eng.skipped) != "[no-bootstrap]" || len(h.eng.restores) != 0 || h.eng.saveCount() != 1 {
		t.Fatalf("skipped %v restores %d saves %d", h.eng.skipped, len(h.eng.restores), h.eng.saveCount())
	}
}

// TestSaver_HeartbeatNeverBlocksSave (B23, §5.7): the heartbeat runs after a
// PUBLISHED save, in its own goroutine; a slow one never delays the next
// save, and that save skips its heartbeat.
func TestSaver_HeartbeatNeverBlocksSave(t *testing.T) {
	h := newHarness(t)
	h.eng.hbStart = make(chan struct{}, 4)
	h.eng.hbBlock = make(chan struct{})
	h.m.onDirty()
	h.advance(5 * time.Second)
	select {
	case <-h.eng.hbStart:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat after a published save")
	}
	h.m.onDirty()
	h.advance(5 * time.Second) // must not block on the running heartbeat
	if h.eng.saveCount() != 2 {
		t.Fatalf("%d saves", h.eng.saveCount())
	}
	if !strings.Contains(h.logText(), "HEARTBEAT-SKIPPED") {
		t.Fatalf("the second save did not skip the running heartbeat:\n%s", h.logText())
	}
	close(h.eng.hbBlock)
	h.m.hbWG.Wait()
	// unchanged saves start none
	h.eng.result = func(int, engine.SaveRequest) (engine.SaveResult, error) { return engine.SaveResult{}, nil }
	h.m.onDirty()
	h.advance(5 * time.Second)
	h.m.hbWG.Wait()
	if h.eng.hbCount != 1 {
		t.Fatalf("%d heartbeats, want 1 (published saves only)", h.eng.hbCount)
	}
}

// ---------------------------------------------------------------------------
// save requests (§5.5)
// ---------------------------------------------------------------------------

// TestSaveRequest_ResultsTable: every save outcome reaches the requester as
// its §5.5 result word on its own option and channel, and maps to the exit
// code of the table.
func TestSaveRequest_ResultsTable(t *testing.T) {
	cases := []struct {
		name   string
		res    engine.SaveResult
		err    error
		result string
		exit   int
	}{
		{"published", engine.SaveResult{Published: true, File: "moox-20261025T120000.json"}, nil,
			"published moox-20261025T120000.json", 0},
		{"unchanged", engine.SaveResult{}, nil, "unchanged", 0},
		{"pristine", engine.SaveResult{Pristine: true}, nil, "pristine", 0},
		{"restoring", engine.SaveResult{}, fmt.Errorf("%w (boot:1:2)", engine.ErrRestoring), "restoring", 75},
		{"shutting-down", engine.SaveResult{}, engine.ErrShuttingDown, "shutting-down", 75},
		{"busy", engine.SaveResult{}, &engine.BusyError{Pid: 77}, "busy 77", 75},
		{"blocked", engine.SaveResult{}, engine.ErrBlocked, "blocked", 5},
		{"invalid", engine.SaveResult{}, fmt.Errorf("x: %w", engine.ErrInvalid), "invalid", 4},
		{"not-active", engine.SaveResult{}, engine.ErrNotActive, "not-active", 3},
		{"error", engine.SaveResult{}, errors.New("disk\nfull"), "error disk full", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.eng.result = func(int, engine.SaveRequest) (engine.SaveResult, error) { return c.res, c.err }
			h.tmux.set(SaveReqPrefix+"0123456789abcdef", FormatSaveRequest(h.clk.Now().Unix(), false, "freeze: manual"))
			h.m.onSaveRequest()
			got := h.tmux.opt(SaveResPrefix + "0123456789abcdef")
			epoch, word, _ := strings.Cut(got, " ")
			if word != c.result || epoch != fmt.Sprint(h.clk.Now().Unix()) {
				t.Fatalf("answer %q, want \"<epoch> %s\"", got, c.result)
			}
			if ExitCode(word) != c.exit {
				t.Fatalf("exit %d for %q, want %d", ExitCode(word), word, c.exit)
			}
			if h.tmux.opt(SaveReqPrefix+"0123456789abcdef") != "" {
				t.Fatal("the request option was not unset")
			}
			if h.tmux.signalled(SavedChanPrefix+"0123456789abcdef") != 1 {
				t.Fatal("moox-saved-<nonce> was not signalled exactly once")
			}
			if r := h.eng.saves[0].req; !r.Contents || r.Reason != "request: freeze: manual" {
				t.Fatalf("the request's save %+v", r)
			}
		})
	}
	if ExitCode("") != 1 || ExitCode("weird") != 1 {
		t.Fatal("an unknown word must exit 1")
	}
}

// TestSaveRequest_OverlappingRequestsEachAcked: two pending requests are ONE
// save and two answers, each on its own channel.
func TestSaveRequest_OverlappingRequestsEachAcked(t *testing.T) {
	h := newHarness(t)
	h.tmux.set(SaveReqPrefix+"aaaaaaaaaaaaaaaa", FormatSaveRequest(1, false, "freeze"))
	h.tmux.set(SaveReqPrefix+"bbbbbbbbbbbbbbbb", FormatSaveRequest(2, false, "thaw"))
	h.m.onSaveRequest()
	if h.eng.saveCount() != 1 {
		t.Fatalf("%d saves for two requests", h.eng.saveCount())
	}
	for _, n := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
		if !strings.Contains(h.tmux.opt(SaveResPrefix+n), "published") || h.tmux.signalled(SavedChanPrefix+n) != 1 {
			t.Fatalf("request %s: answer %q, signals %d", n, h.tmux.opt(SaveResPrefix+n), h.tmux.signalled(SavedChanPrefix+n))
		}
	}
}

// TestSaveRequest_ForceReachesEngine and _BatchForceIfAny (R3-H2): the f
// flag is the engine's Force; in a batch, one forced request forces the one
// save.
func TestSaveRequest_ForceReachesEngine(t *testing.T) {
	h := newHarness(t)
	h.tmux.set(SaveReqPrefix+"cccccccccccccccc", FormatSaveRequest(1, true, "restart"))
	h.m.onSaveRequest()
	if !h.eng.saves[0].req.Force {
		t.Fatal("--force did not reach the engine")
	}
	h.tmux.set(SaveReqPrefix+"dddddddddddddddd", FormatSaveRequest(1, false, ""))
	h.m.onSaveRequest()
	if h.eng.saves[1].req.Force {
		t.Fatal("an unforced request was forced")
	}
}

func TestSaveRequest_BatchForceIfAny(t *testing.T) {
	h := newHarness(t)
	h.tmux.set(SaveReqPrefix+"eeeeeeeeeeeeeeee", FormatSaveRequest(1, false, "a"))
	h.tmux.set(SaveReqPrefix+"ffffffffffffffff", FormatSaveRequest(1, true, "b"))
	h.m.onSaveRequest()
	if h.eng.saveCount() != 1 || !h.eng.saves[0].req.Force {
		t.Fatalf("saves %+v", h.eng.saves)
	}
}

// TestSaveRequest_StaleResultsUnset (§5.5 step 2): results older than 300 s
// (dead requesters) are unset by the next answer; fresh ones are kept.
func TestSaveRequest_StaleResultsUnset(t *testing.T) {
	h := newHarness(t)
	now := h.clk.Now().Unix()
	h.tmux.set(SaveResPrefix+"old", fmt.Sprintf("%d published x", now-301))
	h.tmux.set(SaveResPrefix+"new", fmt.Sprintf("%d published y", now-10))
	h.tmux.set(SaveReqPrefix+"1111111111111111", FormatSaveRequest(now, false, ""))
	h.m.onSaveRequest()
	if h.tmux.opt(SaveResPrefix+"old") != "" || h.tmux.opt(SaveResPrefix+"new") == "" {
		t.Fatalf("old=%q new=%q", h.tmux.opt(SaveResPrefix+"old"), h.tmux.opt(SaveResPrefix+"new"))
	}
}

// TestSaveRequest_ReasonMayHoldSeparators (R3-H2): the reason is the last
// field, so spaces and the separator itself survive.
func TestSaveRequest_ReasonMayHoldSeparators(t *testing.T) {
	r, ok := ParseSaveRequest("n", FormatSaveRequest(5, true, "a b"+FieldSep+"c"))
	if !ok || r.Epoch != 5 || !r.Force || r.Reason != "a b"+FieldSep+"c" {
		t.Fatalf("%+v %v", r, ok)
	}
	if _, ok := ParseSaveRequest("n", "garbage"); ok {
		t.Fatal("garbage parsed")
	}
}

// ---------------------------------------------------------------------------
// restore requests (§6.4a)
// ---------------------------------------------------------------------------

// TestRestoreRequest_TwoConcurrentOneStartsOneRefused: the oldest valid
// request starts (and @moox-restoring names the daemon), the other is
// refused busy-restoring; each on its own channel; a request while a restore
// runs is refused too.
func TestRestoreRequest_TwoConcurrentOneStartsOneRefused(t *testing.T) {
	h := newHarness(t)
	snap := writeValidSnapshot(t)
	h.alive[100], h.alive[200] = true, true
	h.tmux.set(RestoreReqPrefix+"bbbb", FormatRestoreRequest(200, 20, true, snap))
	h.tmux.set(RestoreReqPrefix+"aaaa", FormatRestoreRequest(100, 10, true, snap))
	h.m.onRestoreRequest()
	if got := h.tmux.opt(RestoreResPrefix + "aaaa"); got != "started" {
		t.Fatalf("oldest answered %q", got)
	}
	if got := h.tmux.opt(RestoreResPrefix + "bbbb"); got != "refused busy-restoring" {
		t.Fatalf("second answered %q", got)
	}
	for _, n := range []string{"aaaa", "bbbb"} {
		if h.tmux.signalled(RestoreAckPrefix+n) != 1 || h.tmux.opt(RestoreReqPrefix+n) != "" {
			t.Fatalf("%s: ack %d, request left %q", n, h.tmux.signalled(RestoreAckPrefix+n), h.tmux.opt(RestoreReqPrefix+n))
		}
	}
	if v := h.tmux.opt("@moox-restoring"); !strings.HasPrefix(v, "restore:") {
		t.Fatalf("@moox-restoring %q", v)
	}
	if !h.m.restoring {
		t.Fatal("the loop is not restoring")
	}
	h.tmux.set(RestoreReqPrefix+"cccc", FormatRestoreRequest(100, 30, false, snap))
	h.m.onRestoreRequest()
	if got := h.tmux.opt(RestoreResPrefix + "cccc"); got != "refused busy-restoring" {
		t.Fatalf("a request during a restore answered %q", got)
	}
	h.eng.launch <- engine.LaunchVerdict{Result: "PASS"}
	h.endRestore()
	if r := h.eng.restores; len(r) != 1 || r[0].Path != snap || !r[0].Stub || r[0].Nonce != "aaaa" || r[0].Kind != engine.KindRestore {
		t.Fatalf("restores %+v", r)
	}
}

// TestRestoreRequest_PathWithSpaces and refusals: the path is the last
// field; a dead requester, an invalid snapshot and failed preconditions are
// each refused, and nothing starts.
func TestRestoreRequest_PathWithSpaces(t *testing.T) {
	r, ok := ParseRestoreRequest("n", FormatRestoreRequest(7, 8, false, "/a b/c"+FieldSep+"d.json"))
	if !ok || r.Pid != 7 || r.Epoch != 8 || r.Stub || r.Path != "/a b/c"+FieldSep+"d.json" {
		t.Fatalf("%+v %v", r, ok)
	}
	if _, ok := ParseRestoreRequest("n", FormatRestoreRequest(7, 8, false, "relative.json")); ok {
		t.Fatal("a relative path parsed: the requester resolves it (R2-L8)")
	}
}

func TestRestoreRequest_Refusals(t *testing.T) {
	snap := writeValidSnapshot(t)
	cases := []struct {
		name, path string
		alive      bool
		pre        error
		want       string
	}{
		{"dead requester", snap, false, nil, "refused invalid"},
		{"invalid snapshot", "/nonexistent/moox.json", true, nil, "refused invalid"},
		{"populated", snap, true, fmt.Errorf("%w: %w", engine.ErrPreconditions, engine.ErrPopulated), "refused populated: "},
		{"not active", snap, true, engine.ErrNotActive, "refused not-active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.alive[5] = c.alive
			h.eng.precond = c.pre
			h.tmux.set(RestoreReqPrefix+"9999", FormatRestoreRequest(5, 1, false, c.path))
			h.m.onRestoreRequest()
			if got := h.tmux.opt(RestoreResPrefix + "9999"); !strings.HasPrefix(got, c.want) {
				t.Fatalf("answered %q, want %q…", got, c.want)
			}
			if h.m.restoring || len(h.eng.restores) != 0 || h.tmux.opt("@moox-restoring") != "" {
				t.Fatal("a refused request started a restore")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// options and the watcher
// ---------------------------------------------------------------------------

func TestReadTiming_DurationsAndSeconds(t *testing.T) {
	f := newFakeTmux()
	if got := ReadTiming(context.Background(), f); got != (Timing{DefaultDebounce, DefaultMaxWait, DefaultBackup}) {
		t.Fatalf("defaults %+v", got)
	}
	f.set("@moox-save-debounce", "500ms")
	f.set("@moox-save-max-wait", "20")
	f.set("@moox-save-backup", "0")
	if got := ReadTiming(context.Background(), f); got != (Timing{500 * time.Millisecond, 20 * time.Second, 0}) {
		t.Fatalf("parsed %+v", got)
	}
	f.set("@moox-save-debounce", "nonsense")
	if got := ReadTiming(context.Background(), f); got.Debounce != DefaultDebounce {
		t.Fatalf("an unparsable value %+v", got)
	}
}
