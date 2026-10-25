//go:build tmuxe2e

package saver

// The saver on real, ISOLATED tmux servers (§12.2): every server is
// tmuxtest.StartCold on the real moox.conf — its socket and every state
// directory under a scratch root, /bin/sh (or the vendored hook-shell
// fixture) in every pane — with the binaries of this tree, so the cold gate,
// the daemon (`tt serve --foreground`), its saver and `moox save` run
// exactly as they will on the user's machine, against nothing of the user's.
//
// Run: go test -tags tmuxe2e ./internal/saver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func cold(t *testing.T, opts ...tmuxtest.Option) *tmuxtest.Server {
	t.Helper()
	return tmuxtest.StartCold(t, append([]tmuxtest.Option{tmuxtest.WithBinaries()}, opts...)...)
}

func scratchEnv(s *tmuxtest.Server) []string {
	return []string{"HOME=" + s.Home, "XDG_STATE_HOME=" + s.XS, "XDG_DATA_HOME=" + s.XD, "XDG_CONFIG_HOME=" + s.XC,
		"PATH=" + os.Getenv("PATH"), "LANG=" + os.Getenv("LANG")}
}

// moox runs this tree's moox against s with s's scratch environment only.
func moox(t *testing.T, s *tmuxtest.Server, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(s.Bin, "moox"), args...)
	cmd.Env = scratchEnv(s)
	out, err := cmd.Output()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("moox %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)), code
}

func dlog(t *testing.T, s *tmuxtest.Server) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Paths(t).State, "daemon.log"))
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// snaps lists the published moox snapshots, oldest first.
func snaps(t *testing.T, s *tmuxtest.Server) []string {
	t.Helper()
	ents, err := os.ReadDir(s.Paths(t).Snapshots)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, "moox-") && strings.HasSuffix(n, ".json") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// waitSaver waits until the daemon's saver runs (saver.state written).
func waitSaver(t *testing.T, s *tmuxtest.Server) {
	t.Helper()
	p := filepath.Join(s.Paths(t).State, "saver.state")
	if !tmuxtest.Poll(15*time.Second, 50*time.Millisecond, func() bool { _, err := os.Stat(p); return err == nil }) {
		t.Fatalf("no saver within 15s; daemon.log:\n%s", dlog(t, s))
	}
}

// quiet waits until the snapshot count has not changed for d.
func quiet(t *testing.T, s *tmuxtest.Server, d time.Duration) []string {
	t.Helper()
	last, since := len(snaps(t, s)), time.Now()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if n := len(snaps(t, s)); n != last {
			last, since = n, time.Now()
		}
		if time.Since(since) >= d {
			return snaps(t, s)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the snapshot count never settled; daemon.log:\n%s", dlog(t, s))
	return nil
}

func countLines(log, substr string) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// evidence appends to the named file in $MOOX_EVIDENCE_DIR (the session's
// validation/ dir) when it is set; it never writes anywhere else.
func evidence(t *testing.T, name, text string) {
	t.Helper()
	dir := os.Getenv("MOOX_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(text)
}

// ---------------------------------------------------------------------------
// V2: event-driven save
// ---------------------------------------------------------------------------

// TestE2E_V2_DebouncedSave (V2, S1–S3): no client; a burst of new-window,
// split, kill-pane produces exactly ONE new snapshot, Debounce after the
// last event; status-right holds no `#(` (no status-bar save trigger).
func TestE2E_V2_DebouncedSave(t *testing.T) {
	s := cold(t, tmuxtest.WithHarnessLines("set -g @moox-save-debounce 500ms"))
	waitSaver(t, s)
	before := quiet(t, s, 1500*time.Millisecond)
	logBefore := countLines(dlog(t, s), "SAVE reason=debounce")

	start := time.Now()
	wid := strings.TrimSpace(s.Run(t, "new-window", "-d", "-n", "burst", "-P", "-F", "#{window_id}"))
	pane := strings.TrimSpace(s.Run(t, "split-window", "-d", "-t", wid, "-P", "-F", "#{pane_id}"))
	s.Run(t, "kill-pane", "-t", pane)
	burst := time.Since(start)
	after := quiet(t, s, 2*time.Second)

	log := dlog(t, s)
	saves := countLines(log, "SAVE reason=debounce") - logBefore
	if len(after) != len(before)+1 || saves != 1 {
		t.Fatalf("a burst of 3 changes in %s gave %d new snapshot(s) from %d debounced save(s), want 1 and 1\nbefore %v\nafter %v\n%s",
			burst, len(after)-len(before), saves, before, after, log)
	}
	newest, err := os.ReadFile(filepath.Join(s.Paths(t).Snapshots, after[len(after)-1]))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.Decode(newest)
	if err != nil || len(snap.Windows) != 2 || len(snap.Panes) != 2 {
		t.Fatalf("the debounced snapshot: %v windows=%d panes=%d", err, len(snap.Windows), len(snap.Panes))
	}
	for _, opt := range []string{"status-right", "status-left"} {
		if v := s.Opt(t, opt); saveTrigger(v) != "" {
			t.Fatalf("%s runs a save from the status line: %q", opt, saveTrigger(v))
		}
	}
	if got := s.Opt(t, "@moox_status"); !strings.HasPrefix(got, "saved ") {
		t.Errorf("@moox_status %q after a publish", got)
	}
	evidence(t, "v2-event-save.md", fmt.Sprintf("### TestE2E_V2_DebouncedSave (%s)\n\n"+
		"- server: StartCold `%s`, no client attached, `@moox-save-debounce 500ms`\n"+
		"- burst: new-window, split-window, kill-pane in %s\n"+
		"- snapshots before: %d (%v)\n- snapshots after:  %d (%v)\n- debounced saves in daemon.log for the burst: %d\n"+
		"- newest snapshot: %d windows, %d panes\n- status-right save trigger: %q (none; its only `#(` job is the host label)\n- daemon.log SAVE lines:\n\n```\n%s```\n\n",
		time.Now().Format(time.RFC3339), s.Name, burst.Round(time.Millisecond), len(before), before, len(after), after, saves,
		len(snap.Windows), len(snap.Panes), saveTrigger(s.Opt(t, "status-right")), grepLines(log, "SAVE ")))
}

// saveTrigger returns the first `#(…)` job in a status format that saves
// (continuum's `#(…continuum_save.sh)`, a resurrect save, a moox save), or
// "". Other `#(…)` jobs — the host label — are not save triggers.
func saveTrigger(format string) string {
	for rest := format; ; {
		i := strings.Index(rest, "#(")
		if i < 0 {
			return ""
		}
		rest = rest[i:]
		j := strings.IndexByte(rest, ')')
		if j < 0 {
			j = len(rest) - 1
		}
		job := rest[:j+1]
		for _, w := range []string{"save", "continuum", "resurrect"} {
			if strings.Contains(job, w) {
				return job
			}
		}
		rest = rest[j+1:]
	}
}

// lastTwo dumps the two newest snapshots and their contents files.
func lastTwo(t *testing.T, s *tmuxtest.Server) string {
	t.Helper()
	all := snaps(t, s)
	if len(all) > 2 {
		all = all[len(all)-2:]
	}
	var b strings.Builder
	for _, n := range all {
		data, _ := os.ReadFile(filepath.Join(s.Paths(t).Snapshots, n))
		fmt.Fprintf(&b, "%s:\n%s\n", n, data)
	}
	return b.String()
}

func grepLines(log, substr string) string {
	var b strings.Builder
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, substr) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// TestE2E_V2_MaxWaitCeiling (V2, R2-M8): a steady stream of changes every
// 400 ms with a 1 s debounce never lets the debounce fire, and still saves
// by the ceiling (2 s here).
func TestE2E_V2_MaxWaitCeiling(t *testing.T) {
	s := cold(t, tmuxtest.WithHarnessLines("set -g @moox-save-debounce 1s", "set -g @moox-save-max-wait 2s"))
	waitSaver(t, s)
	before := quiet(t, s, 2500*time.Millisecond)
	logBefore := countLines(dlog(t, s), "SAVE reason=max-wait")
	wid := strings.Fields(s.Run(t, "list-windows", "-F", "#{window_id}"))[0]
	start := time.Now()
	for i := 0; time.Since(start) < 5*time.Second; i++ {
		s.Run(t, "rename-window", "-t", wid, fmt.Sprintf("w%d", i))
		time.Sleep(400 * time.Millisecond)
	}
	during := len(snaps(t, s)) - len(before)
	log := dlog(t, s)
	ceiling := countLines(log, "SAVE reason=max-wait") - logBefore
	if during < 2 || ceiling < 2 {
		t.Fatalf("a 5 s stream with a 2 s ceiling published %d snapshot(s), %d by the ceiling; want ≥2\n%s", during, ceiling, log)
	}
	evidence(t, "v2-event-save.md", fmt.Sprintf("### TestE2E_V2_MaxWaitCeiling (%s)\n\n"+
		"- rename-window every 400 ms for 5 s, `@moox-save-debounce 1s`, `@moox-save-max-wait 2s`\n"+
		"- published during the stream: %d; saves by the ceiling: %d (the debounce never fired during the stream)\n\n```\n%s```\n\n",
		time.Now().Format(time.RFC3339), during, ceiling, grepLines(log, "SAVE ")))
}

// TestE2E_Saver_NoClientNoStatusBar (S1, S2, S3): continuum's three parts —
// the status-bar trigger, its lock, its background save — are gone: no `#(`
// in the status line, and an idle server with no client makes no save at
// all once it settled.
func TestE2E_Saver_NoClientNoStatusBar(t *testing.T) {
	s := cold(t, tmuxtest.WithHarnessLines("set -g @moox-save-debounce 300ms"))
	waitSaver(t, s)
	settled := quiet(t, s, 1500*time.Millisecond)
	n := countLines(dlog(t, s), "SAVE reason=")
	time.Sleep(4 * time.Second)
	if got := snaps(t, s); len(got) != len(settled) || countLines(dlog(t, s), "SAVE reason=") != n {
		t.Fatalf("an idle server saved: %v → %v\n%s", settled, got, dlog(t, s))
	}
	for _, opt := range []string{"status-right", "status-left"} {
		if v := s.Opt(t, opt); saveTrigger(v) != "" {
			t.Fatalf("%s runs a save from the status line: %q", opt, saveTrigger(v))
		}
	}
	if strings.Contains(s.Run(t, "show-options", "-g"), "continuum") {
		t.Fatal("continuum options are set")
	}
}

// ---------------------------------------------------------------------------
// moox save through the daemon (§5.5)
// ---------------------------------------------------------------------------

// TestE2E_SaveRequest_ThroughDaemon: `moox save` with a saver running is a
// request (the daemon logs it), answered on its own channel; a second one
// with nothing changed is "unchanged"; --direct saves in process.
func TestE2E_SaveRequest_ThroughDaemon(t *testing.T) {
	s := cold(t)
	waitSaver(t, s)
	quiet(t, s, 4*time.Second)
	// a fixed name: automatic-rename would rename the window between the
	// two saves (from "tmux" to the shell), a real change
	np := strings.TrimSpace(s.Run(t, "new-window", "-d", "-n", "extra", "-P", "-F", "#{pane_id}"))
	// Contents are part of the fingerprint: let the new shell print its
	// prompt first, or the next capture differs from this one.
	if !tmuxtest.Poll(5*time.Second, 50*time.Millisecond, func() bool {
		return strings.TrimSpace(s.Run(t, "capture-pane", "-p", "-t", np)) != ""
	}) {
		t.Fatal("the new pane never printed a prompt")
	}
	time.Sleep(300 * time.Millisecond)
	out, code := moox(t, s, "save", "-S", s.T.Socket, "--reason", "e2e one")
	if code != 0 || !strings.HasPrefix(out, "published moox-") {
		t.Fatalf("moox save: %q exit %d\n%s", out, code, dlog(t, s))
	}
	if !strings.Contains(dlog(t, s), "reason=request: e2e one") {
		t.Fatalf("the save did not go through the daemon:\n%s", dlog(t, s))
	}
	out, code = moox(t, s, "save", "-S", s.T.Socket)
	if code != 0 || out != "unchanged" {
		t.Fatalf("second moox save: %q exit %d\n%s", out, code, lastTwo(t, s))
	}
	out, code = moox(t, s, "save", "--direct", "-S", s.T.Socket)
	if code != 0 || out != "unchanged" {
		all := snaps(t, s)
		var dump strings.Builder
		for _, n := range all[len(all)-2:] {
			b, _ := os.ReadFile(filepath.Join(s.Paths(t).Snapshots, n))
			fmt.Fprintf(&dump, "%s:\n%s\n", n, b)
		}
		t.Fatalf("moox save --direct: %q exit %d\n%s", out, code, dump.String())
	}
	for _, line := range strings.Split(s.Run(t, "show-options", "-g"), "\n") {
		if strings.HasPrefix(line, SaveReqPrefix) || strings.HasPrefix(line, SaveResPrefix) {
			t.Errorf("a request/result option was left behind: %s", line)
		}
	}
}

// TestSaveRequest_LateWaiterStillAcked (Q6 (b)): the daemon answers BEFORE
// the requester starts waiting; the per-request channel keeps the signal, so
// the late waiter returns at once with its own result.
func TestSaveRequest_LateWaiterStillAcked(t *testing.T) {
	s := tmuxtest.Start(t)
	h := newHarness(t, func(c *Config) { c.Tmux = s.T })
	nonce := "0a0b0c0d0e0f1011"
	s.Run(t, "set-option", "-g", SaveReqPrefix+nonce, FormatSaveRequest(time.Now().Unix(), false, "late"))
	h.m.onSaveRequest() // answered and signalled; nobody is waiting yet
	time.Sleep(300 * time.Millisecond)
	done := make(chan error, 1)
	go func() { _, err := s.RunErr("wait-for", SavedChanPrefix+nonce); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late waiter never woke: the signal was lost")
	}
	if got := strings.TrimSpace(s.Run(t, "show-options", "-gqv", SaveResPrefix+nonce)); !strings.HasSuffix(got, " published moox-1.json") {
		t.Fatalf("result %q", got)
	}
	// and another requester's channel was not signalled by it
	other := make(chan error, 1)
	go func() { _, err := s.RunErr("wait-for", SavedChanPrefix+"ffffffffffffffff"); other <- err }()
	select {
	case <-other:
		t.Fatal("an unrelated requester's channel woke")
	case <-time.After(500 * time.Millisecond):
	}
	s.Run(t, "wait-for", "-S", SavedChanPrefix+"ffffffffffffffff")
	<-other
}

// TestSaveRequest_ReasonIsNeverFormatExpanded (G2-C1 rebuttal evidence):
// a `moox save --reason` holding #{…}, #(…), ##, %-strftime and a trailing
// ';' is written exactly as cmd/moox/cmd_save.go writes it (set-option
// without -F, inside Chain) and read back by the daemon's batched
// display-message: it arrives byte-identical and runs nothing. tmux never
// re-expands the VALUE of a #{@option} (only the E: modifier does), and
// display-message's strftime pass runs over the template only.
func TestSaveRequest_ReasonIsNeverFormatExpanded(t *testing.T) {
	s := tmuxtest.Start(t)
	h := newHarness(t, func(c *Config) { c.Tmux = s.T })
	marker := filepath.Join(t.TempDir(), "ran")
	reason := "#{socket_path} #(touch " + marker + ") ##{pane_id} %H:%M #[fg=red] a;b;"
	nonce := "1a1b1c1d1e1f2021"
	s.Run(t, tmuxtarget.Chain(
		[]string{"set-option", "-g", SaveReqPrefix + nonce, FormatSaveRequest(time.Now().Unix(), false, reason)},
	)...)
	h.m.onSaveRequest()
	time.Sleep(1500 * time.Millisecond) // a #() job would have run by now
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("#(…) in the reason ran inside the server")
	}
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	if len(h.eng.saves) != 1 {
		t.Fatalf("saves = %d, want 1", len(h.eng.saves))
	}
	if got, want := h.eng.saves[0].req.Reason, "request: "+reason; got != want {
		t.Fatalf("recorded reason\n got %q\nwant %q", got, want)
	}
}

// TestSaveRequest_SeparatorInValueDoesNotWedge (G1-M8): a request value
// holding the batch separator \x1e (or a newline) made readOptions fail its
// field count, so nothing was ever answered or unset and every later
// request timed out. FormatSaveRequest now strips them, and a value set by
// hand is read one option at a time instead.
func TestSaveRequest_SeparatorInValueDoesNotWedge(t *testing.T) {
	if v := FormatSaveRequest(1, false, "a\x1eb\nc\rd"); strings.ContainsAny(v, "\x1e\n\r") {
		t.Fatalf("FormatSaveRequest kept a separator: %q", v)
	}
	s := tmuxtest.Start(t)
	h := newHarness(t, func(c *Config) { c.Tmux = s.T })
	nonce := "2a2b2c2d2e2f3031"
	s.Run(t, "set-option", "-g", SaveReqPrefix+nonce, "1"+FieldSep+FieldSep+"x\x1ey")
	h.m.onSaveRequest()
	if got := strings.TrimSpace(s.Run(t, "show-options", "-gqv", SaveResPrefix+nonce)); !strings.HasSuffix(got, " published moox-1.json") {
		t.Fatalf("the request was not answered: result %q", got)
	}
	if v := strings.TrimSpace(s.Run(t, "show-options", "-gqv", SaveReqPrefix+nonce)); v != "" {
		t.Fatalf("the request option was left set: %q", v)
	}
}

// ---------------------------------------------------------------------------
// cold start (§6.4, E4, Q1)
// ---------------------------------------------------------------------------

// seedSnapshotDir writes a valid two-window snapshot (shell rows, cwd home,
// one row with a recorded launch) as moox-last in a fresh dir.
func seedSnapshotDir(t *testing.T, cwd string, launch string) string {
	t.Helper()
	dir := t.TempDir()
	s := twoWindowSnapshot(cwd, launch)
	b, err := snapshot.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	name := "moox-20261025T100000.json"
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "moox-last"), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func twoWindowSnapshot(cwd, launch string) *snapshot.Snapshot {
	s := &snapshot.Snapshot{Format: snapshot.FormatName, Version: 1, Source: snapshot.SourceSave,
		Client: &snapshot.Client{Session: "work"}, CreatedAt: "2026-10-25T10:00:00Z"}
	s.Sessions = []snapshot.Session{{Name: "work", ActiveWindow: 0, LastWindow: -1}}
	for w := 0; w < 2; w++ {
		id := "%" + strconv.Itoa(w+1)
		p := snapshot.Pane{Key: snapshot.PaneKey("work", w, 0), Session: "work", Window: w, Index: 0, SavedID: id,
			Cwd: cwd, Role: snapshot.RoleShell, Active: true, CurrentCommand: "sh"}
		if w == 1 && launch != "" {
			p.Launch = launch
		}
		s.Panes = append(s.Panes, p)
		s.Windows = append(s.Windows, snapshot.Window{Session: "work", Index: w, Name: "w" + strconv.Itoa(w),
			Layout: layoutV1(w + 1), LayoutFormat: snapshot.LayoutV1, AutomaticRename: snapshot.AutoRenameUnset,
			Width: 80, Height: 24, Active: w == 0})
	}
	return s
}

// TestE2E_ColdStart_GateSeesZeroSessions (PV13, E4, B27) and
// TestE2E_ColdStart_ServerSurvivesGateReturn (Q1): the gate runs before the
// starting client's new-session (it sees 0 sessions and decides the boot
// restore), the starting client exits 0, the server survives the gate's
// return, and the daemon's bounded poll finds the bootstrap and restores
// into a new session, dropping the bootstrap.
func TestE2E_ColdStart_GateSeesZeroSessions(t *testing.T) {
	home := t.TempDir()
	s := cold(t, tmuxtest.WithSnapshot(seedSnapshotDir(t, home, "")), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"))
	if !tmuxtest.Poll(60*time.Second, 100*time.Millisecond, func() bool { return s.Opt(t, "@moox-boot-result") != "" }) {
		t.Fatalf("no boot result:\n%s", dlog(t, s))
	}
	log := dlog(t, s)
	if !strings.Contains(log, "BOOT-GATE cold=true restore=true") {
		t.Fatalf("the gate did not see a cold server:\n%s", log)
	}
	// shell rows only: nothing to resume, so the clean answer is NOTHING
	if got := s.Opt(t, "@moox-boot-result"); got != "NOTHING" || !strings.Contains(dlog(t, s), "0 misses") {
		t.Fatalf("@moox-boot-result %q:\n%s", got, log)
	}
	names := strings.Fields(s.Run(t, "list-sessions", "-F", "#{session_name}"))
	if fmt.Sprint(names) != "[work]" {
		t.Fatalf("sessions %v, want the restored [work] alone (bootstrap dropped)", names)
	}
}

func TestE2E_ColdStart_ServerSurvivesGateReturn(t *testing.T) {
	s := cold(t, tmuxtest.WithSnapshot(seedSnapshotDir(t, t.TempDir(), "")), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"))
	// StartCold already failed the test if the starting client exited non-zero.
	if !s.Alive() {
		t.Fatal("the server did not survive the gate's return")
	}
	if !tmuxtest.Poll(60*time.Second, 100*time.Millisecond, func() bool {
		return s.Opt(t, "@moox-restoring") == "" &&
			s.Opt(t, "@moox-boot-result") != ""
	}) {
		t.Fatalf("the boot restore never ended:\n%s", dlog(t, s))
	}
	log := dlog(t, s)
	for _, want := range []string{"SERVE-START", "SAVER-START", "RESTORE-START kind=boot", "LAUNCH VERDICT", "RESTORE-END kind=boot"} {
		if !strings.Contains(log, want) {
			t.Fatalf("daemon.log lacks %q:\n%s", want, log)
		}
	}
}

// TestE2E_ColdStart_RestoreAfterConfigComplete (R30, I4): the boot restore
// starts only once the bootstrap session exists, which is after cfg_done,
// so everything the config sets after the gate — active.conf's hooks,
// tt.conf's sidebar slot — is in place when the restore reads the server.
func TestE2E_ColdStart_RestoreAfterConfigComplete(t *testing.T) {
	s := cold(t, tmuxtest.WithSnapshot(seedSnapshotDir(t, t.TempDir(), "")), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"))
	if !tmuxtest.Poll(60*time.Second, 100*time.Millisecond, func() bool { return s.Opt(t, "@moox-boot-result") != "" }) {
		t.Fatalf("no boot result:\n%s", dlog(t, s))
	}
	hooks := s.Run(t, "show-hooks", "-g")
	for _, slot := range []string{"[101]", "[102]", "[103]", "[104]"} {
		if !strings.Contains(hooks, slot) {
			t.Fatalf("hook slot %s missing after the boot restore:\n%s", slot, hooks)
		}
	}
	log := dlog(t, s)
	gate, start := strings.Index(log, "BOOT-GATE cold=true"), strings.Index(log, "RESTORE-START kind=boot")
	if gate < 0 || start < 0 || start < gate {
		t.Fatalf("the restore did not start after the gate:\n%s", log)
	}
}

// TestE2E_ColdStart_BootstrapHistoryLimit (RR17): options.conf sets
// history-limit before the gate, so every pane — the restored ones and any
// the bootstrap had — has the configured 50000.
func TestE2E_ColdStart_BootstrapHistoryLimit(t *testing.T) {
	s := cold(t, tmuxtest.WithSnapshot(seedSnapshotDir(t, t.TempDir(), "")), tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"))
	if !tmuxtest.Poll(60*time.Second, 100*time.Millisecond, func() bool { return s.Opt(t, "@moox-boot-result") != "" }) {
		t.Fatalf("no boot result:\n%s", dlog(t, s))
	}
	for _, line := range strings.Split(strings.TrimSpace(s.Run(t, "list-panes", "-a", "-F", "#{pane_id} #{history_limit}")), "\n") {
		if !strings.HasSuffix(line, " 50000") {
			t.Fatalf("pane %q: history-limit is not the configured 50000", line)
		}
	}
}

// TestE2E_Handover_AutosaveKeepsMooxLast (A15, U3): a cold start with
// MOOX_NO_AUTO_RESTORE=1 is a HANDOVER — no boot restore — and the daemon,
// running as saver only, answers the bootstrap's own session-created event
// with a save that the pristine guard refuses: moox-last still names the
// two-window snapshot.
func TestE2E_Handover_AutosaveKeepsMooxLast(t *testing.T) {
	s := cold(t, tmuxtest.WithSnapshot(seedSnapshotDir(t, t.TempDir(), "")), tmuxtest.WithEnv("MOOX_NO_AUTO_RESTORE=1"),
		tmuxtest.WithHarnessLines("set -g @moox-save-debounce 300ms"))
	waitSaver(t, s)
	if got := s.Opt(t, "@moox-boot-result"); got != "HANDOVER" {
		t.Fatalf("@moox-boot-result %q, want HANDOVER", got)
	}
	if !tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool {
		return strings.Contains(dlog(t, s), "-> pristine")
	}) {
		t.Fatalf("no pristine save answered the bootstrap:\n%s", dlog(t, s))
	}
	out, code := moox(t, s, "save", "-S", s.T.Socket)
	if code != 0 || out != "pristine" {
		t.Fatalf("moox save on the handover bootstrap: %q exit %d", out, code)
	}
	ptr, err := os.ReadFile(filepath.Join(s.Paths(t).Snapshots, "moox-last"))
	if err != nil || strings.TrimSpace(string(ptr)) != "moox-20261025T100000.json" {
		t.Fatalf("moox-last %q %v, want the seeded two-window snapshot", ptr, err)
	}
	if got := snaps(t, s); len(got) != 1 {
		t.Fatalf("snapshots %v: the bootstrap was published", got)
	}
}

// resurrectFixture is a two-window resurrect save (v1 layouts) whose panes
// sit in cwd.
func resurrectFixture(cwd string) string {
	return "pane\tlegacy\t0\t1\t:*\t0\tone\t:" + cwd + "\t1\tsh\t:\n" +
		"pane\tlegacy\t1\t0\t:-\t0\ttwo\t:" + cwd + "\t1\tsh\t:\n" +
		"window\tlegacy\t0\t:alpha\t1\t:*\t" + layoutV1(1) + "\toff\n" +
		"window\tlegacy\t1\t:beta\t0\t:-\t" + layoutV1(2) + "\toff\n" +
		"state\tlegacy\tlegacy\n"
}

// TestE2E_ColdStart_ImportPublishedBeforeRestore (D12, PR-M4, R2-H10): the
// first moox boot on a socket whose snapshot dir holds resurrect's `last`
// and no moox snapshot publishes the import as moox-<ts>.json (source
// import-resurrect) under save.lock, points moox-last at it, and restores
// FROM that file. Resurrect's own files are not touched.
func TestE2E_ColdStart_ImportPublishedBeforeRestore(t *testing.T) {
	var resFile string
	var resSum string
	s := cold(t, tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"), tmuxtest.WithPrepare(func(s *tmuxtest.Server) {
		dir := s.Paths(t).Snapshots
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		resFile = filepath.Join(dir, "tmux_resurrect_20261024T100000.txt")
		body := resurrectFixture(s.Home)
		if err := os.WriteFile(resFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(resFile), filepath.Join(dir, "last")); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		resSum = hex.EncodeToString(sum[:])
	}))
	if !tmuxtest.Poll(60*time.Second, 100*time.Millisecond, func() bool { return s.Opt(t, "@moox-boot-result") != "" }) {
		t.Fatalf("no boot result:\n%s", dlog(t, s))
	}
	log := dlog(t, s)
	if !strings.Contains(log, "reason=restore-import") {
		t.Fatalf("the gate did not offer the import:\n%s", log)
	}
	pub, start := strings.Index(log, "IMPORT-PUBLISHED"), strings.Index(log, "RESTORE-START kind=boot")
	if pub < 0 || start < 0 || pub > start {
		t.Fatalf("the import was not published before the restore started:\n%s", log)
	}
	// shell rows only: nothing to resume, so the clean answer is NOTHING
	if got := s.Opt(t, "@moox-boot-result"); got != "NOTHING" || !strings.Contains(dlog(t, s), "0 misses") {
		t.Fatalf("@moox-boot-result %q:\n%s", got, log)
	}
	ptr, err := os.ReadFile(filepath.Join(s.Paths(t).Snapshots, "moox-last"))
	if err != nil {
		t.Fatal(err)
	}
	first := snaps(t, s)[0]
	b, err := os.ReadFile(filepath.Join(s.Paths(t).Snapshots, first))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.Decode(b)
	if err != nil || snap.Source != snapshot.SourceImport || len(snap.Windows) != 2 {
		t.Fatalf("the first moox snapshot %s: %v source=%q windows=%d (moox-last %q)", first, err, snap.Source,
			len(snap.Windows), ptr)
	}
	if !strings.Contains(log, "source="+filepath.Join(s.Paths(t).Snapshots, first)) {
		t.Fatalf("the restore did not read the published import %s:\n%s", first, log)
	}
	names := strings.Fields(s.Run(t, "list-sessions", "-F", "#{session_name}"))
	if fmt.Sprint(names) != "[legacy]" {
		t.Fatalf("sessions %v, want [legacy]", names)
	}
	after, err := os.ReadFile(resFile)
	sum := sha256.Sum256(after)
	if err != nil || hex.EncodeToString(sum[:]) != resSum {
		t.Fatalf("resurrect's file changed: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(s.Paths(t).Snapshots, "last")); err != nil || l != filepath.Base(resFile) {
		t.Fatalf("resurrect's last changed: %q %v", l, err)
	}
}

// ---------------------------------------------------------------------------
// suppression across processes (§5.4, §6.4a)
// ---------------------------------------------------------------------------

// requestRestore is the §6.4a requester, by hand (moox restore is P7): one
// option per request and the shared wake channel, then the requester's own
// ack channel.
func requestRestore(t *testing.T, s *tmuxtest.Server, path string) string {
	t.Helper()
	nonce := fmt.Sprintf("%016x", time.Now().UnixNano())
	s.Run(t, tmuxtarget.Chain(
		[]string{"set-option", "-g", RestoreReqPrefix + nonce, FormatRestoreRequest(os.Getpid(), time.Now().Unix(), true, path)},
		[]string{"wait-for", "-S", ChanRestoreReq},
	)...)
	waitChannel(t, s, RestoreAckPrefix+nonce, 15*time.Second)
	if got := strings.TrimSpace(s.Run(t, "show-options", "-gqv", RestoreResPrefix+nonce)); got != "started" {
		t.Fatalf("restore request answered %q:\n%s", got, dlog(t, s))
	}
	return nonce
}

func waitChannel(t *testing.T, s *tmuxtest.Server, ch string, within time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	cmd := s.T.Command(t.Context(), "wait-for", ch)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait-for %s: %v", ch, err)
		}
	case <-time.After(within):
		_ = cmd.Process.Kill()
		t.Fatalf("no signal on %s within %s:\n%s", ch, within, dlog(t, s))
	}
}

// TestE2E_Restore_CrossProcessSuppressesUntilLaunch (K4, PR-C3, B29): a
// requested, stubbed restore; between "panes created" (moox-restored-<n>)
// and the LAUNCH verdict, `moox save` from another process exits 75
// ("restoring") and nothing is published; after the verdict exactly one save
// follows.
func TestE2E_Restore_CrossProcessSuppressesUntilLaunch(t *testing.T) {
	s := cold(t, tmuxtest.WithShell(tmuxtest.HookZsh(t)), tmuxtest.WithHarnessLines("set -g @moox-save-debounce 500ms"))
	waitSaver(t, s)
	before := quiet(t, s, 1500*time.Millisecond)
	src := filepath.Join(seedSnapshotDir(t, s.Home, "sleep 1000"), "moox-20261025T100000.json")

	nonce := requestRestore(t, s, src)
	waitChannel(t, s, RestoredPrefix+nonce, 60*time.Second)
	if s.Opt(t, "@moox-restoring") == "" {
		t.Fatalf("the LAUNCH verdict was already written when the panes were; the window under test is empty:\n%s", dlog(t, s))
	}
	out, code := moox(t, s, "save", "-S", s.T.Socket, "--reason", "during")
	if code != 75 || out != "restoring" {
		t.Fatalf("moox save during the restore: %q exit %d, want restoring/75\n%s", out, code, dlog(t, s))
	}
	out, code = moox(t, s, "save", "--direct", "-S", s.T.Socket)
	if code != 75 {
		t.Fatalf("moox save --direct during the restore: %q exit %d, want 75", out, code)
	}
	if got := snaps(t, s); len(got) != len(before) {
		t.Fatalf("a snapshot was published during the restore: %v → %v", before, got)
	}
	if !tmuxtest.Poll(120*time.Second, 200*time.Millisecond, func() bool { return s.Opt(t, "@moox-restoring") == "" }) {
		t.Fatalf("the LAUNCH verdict never came:\n%s", dlog(t, s))
	}
	after := quiet(t, s, 2*time.Second)
	if len(after) != len(before)+1 {
		t.Fatalf("after the verdict %d snapshot(s) were published, want exactly 1\nbefore %v\nafter %v\n%s",
			len(after)-len(before), before, after, dlog(t, s))
	}
	log := dlog(t, s)
	if !strings.Contains(log, "RESTORE-END kind=restore") || !strings.Contains(log, "LAUNCH VERDICT") {
		t.Fatalf("daemon.log:\n%s", log)
	}
}

// ---------------------------------------------------------------------------
// V1: multi-socket isolation
// ---------------------------------------------------------------------------

// realDirs are the user's real state directories V1 checks (read-only).
func realDirs(t *testing.T) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		filepath.Join(home, ".config", "tmux-claude"),
		filepath.Join(home, ".config", "tmux-cc"),
		filepath.Join(home, ".local", "state", "moox"),
		filepath.Join(home, ".local", "state", "tt"),
		filepath.Join(home, ".local", "share", "tmux", "resurrect"),
	}
}

type fileSum struct {
	sum   string
	size  int64
	mtime time.Time
}

// hashTree hashes every regular file under root (read-only: open O_RDONLY,
// read, close). Symlinks are recorded by their target, never followed.
func hashTree(root string) (map[string]fileSum, error) {
	out := map[string]fileSum{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			out[p] = fileSum{sum: "-> " + l, mtime: info.ModTime()}
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			s := sha256.Sum256(b)
			out[p] = fileSum{sum: hex.EncodeToString(s[:]), size: info.Size(), mtime: info.ModTime()}
		}
		return nil
	})
	return out, err
}

func dirDigest(m map[string]fileSum) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\n", k, m[k].sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestE2E_V1_MultiSocketIsolation (V1, D7, K2): two cold servers on the real
// config with hook-shell panes, a (matched) and b (not matched). a gets the
// gate, hook slots 101–104, a daemon, the marker and its own snapshot dir;
// b gets @moox-active 0, no slots, no daemon, no marker, no file under its
// key — even after a command runs in its zsh pane. With
// MOOX_V1_REAL_DIRS=1 the user's real state dirs are hashed (READ-ONLY)
// before and after, and every changed file is searched for this run's nonce.
func TestE2E_V1_MultiSocketIsolation(t *testing.T) {
	checkReal := os.Getenv("MOOX_V1_REAL_DIRS") == "1"
	nonce := fmt.Sprintf("v1nonce%x", time.Now().UnixNano())
	var before map[string]map[string]fileSum
	if checkReal {
		before = map[string]map[string]fileSum{}
		for _, d := range realDirs(t) {
			m, err := hashTree(d)
			if err != nil {
				t.Fatal(err)
			}
			before[d] = m
		}
	}
	started := time.Now()

	a := cold(t, tmuxtest.WithName("moox-val-a"), tmuxtest.WithShell(tmuxtest.HookZsh(t)),
		tmuxtest.WithHarnessLines("set -g @moox-save-debounce 500ms"))
	b := cold(t, tmuxtest.WithName("moox-val-b"), tmuxtest.WithShell(tmuxtest.HookZsh(t)),
		tmuxtest.WithHarnessLines("set -g @moox-sockets 'nothing-matches'"))
	waitSaver(t, a)
	for _, s := range []*tmuxtest.Server{a, b} {
		// a command typed in each hook-shell pane, carrying the nonce
		s.Run(t, "send-keys", "-t", "boot:", "sleep 30; : "+nonce, "Enter")
		s.Run(t, "rename-session", "-t", "boot", "s-"+nonce)
	}
	time.Sleep(3 * time.Second)
	pa, pb := a.Paths(t), b.Paths(t)
	report := &strings.Builder{}
	check := func(ok bool, format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		mark := "PASS"
		if !ok {
			mark = "FAIL"
			t.Error(line)
		}
		fmt.Fprintf(report, "| %s | %s |\n", mark, line)
	}
	hooksA, hooksB := a.Run(t, "show-hooks", "-g"), b.Run(t, "show-hooks", "-g")
	check(a.Opt(t, "@moox-active") == "1", "a: @moox-active = %q (want 1)", a.Opt(t, "@moox-active"))
	for _, slot := range []string{"[101]", "[102]", "[103]", "[104]"} {
		check(strings.Contains(hooksA, slot), "a: hook slot %s registered", slot)
		check(!strings.Contains(hooksB, slot), "b: hook slot %s absent", slot)
	}
	_, errPid := os.Stat(filepath.Join(pa.TTState, "observer.pid"))
	check(errPid == nil, "a: daemon pid file in its own tt dir (%v)", errPid)
	_, errMarker := os.Stat(pa.Marker)
	check(errMarker == nil, "a: participation marker exists (%v)", errMarker)
	check(len(snaps(t, a)) > 0, "a: its own snapshot dir holds %d snapshot(s)", len(snaps(t, a)))
	// Positive control: the hook IS live — a's zsh recorded the running,
	// nonce-carrying command under a's own key — so b's silence and the
	// real dirs' silence below are not a dead hook's.
	recorded := 0
	if ents, err := os.ReadDir(pa.Launch); err == nil {
		for _, en := range ents {
			if b, err := os.ReadFile(filepath.Join(pa.Launch, en.Name())); err == nil && strings.Contains(string(b), nonce) {
				recorded++
			}
		}
	}
	check(recorded == 1, "positive control: a's own launch/ holds the nonce command (%d recording)", recorded)
	check(b.Opt(t, "@moox-active") == "0", "b: @moox-active = %q (want 0)", b.Opt(t, "@moox-active"))
	for _, p := range []string{pb.State, pb.TTState, pb.Snapshots, pb.Launch, pb.FreezeDir} {
		_, err := os.Stat(p)
		check(errors.Is(err, fs.ErrNotExist), "b: nothing under its key: %s absent", strings.TrimPrefix(p, b.Root))
	}
	_, keyB := filepath.Split(pb.State)
	check(!strings.Contains(fmt.Sprint(walkNames(t, a.Root)), keyB), "b's key appears nowhere under a's scratch root")
	check(!strings.Contains(fmt.Sprint(walkNames(t, b.Root)), "observer.pid"), "b: no daemon pid file anywhere in b's root")
	check(a.Opt(t, "@moox-sockets") == "moox-val-a" && b.Opt(t, "@moox-sockets") == "nothing-matches",
		"settings: a %q, b %q", a.Opt(t, "@moox-sockets"), b.Opt(t, "@moox-sockets"))

	var realReport strings.Builder
	if checkReal {
		fmt.Fprintf(&realReport, "| dir | files before | digest before | files after | digest after | changed files | nonce in changed |\n|---|---|---|---|---|---|---|\n")
		for _, d := range realDirs(t) {
			after, err := hashTree(d)
			if err != nil {
				t.Fatal(err)
			}
			var changed []string
			nonceHits := 0
			for p, s := range after {
				if b, ok := before[d][p]; !ok || b.sum != s.sum {
					changed = append(changed, fmt.Sprintf("%s (mtime %s)", strings.TrimPrefix(p, d+"/"), s.mtime.Format("15:04:05")))
					if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), nonce) {
						nonceHits++
					}
				}
			}
			for p := range before[d] {
				if _, ok := after[p]; !ok {
					changed = append(changed, strings.TrimPrefix(p, d+"/")+" (removed)")
				}
			}
			sort.Strings(changed)
			check(nonceHits == 0, "real %s: no changed file holds this run's nonce", d)
			fmt.Fprintf(&realReport, "| %s | %d | %.16s | %d | %.16s | %d | %d |\n", "~"+strings.TrimPrefix(d, mustHome(t)),
				len(before[d]), dirDigest(before[d]), len(after), dirDigest(after), len(changed), nonceHits)
			for _, c := range changed {
				fmt.Fprintf(&realReport, "|  ↳ %s | | | | | | |\n", c)
			}
		}
	}
	evidence(t, "v1-isolation.md", fmt.Sprintf("## TestE2E_V1_MultiSocketIsolation — %s → %s\n\n"+
		"Servers: `a` = StartCold `%s` (`@moox-sockets` = its own name), `b` = StartCold `%s` (`@moox-sockets nothing-matches`), "+
		"both on the real `share/moox/moox.conf`, `HookZsh` panes, scratch HOME/XDG. Nonce `%s` typed into each zsh pane "+
		"and used as each session's name.\n\n| result | check |\n|---|---|\n%s\n%s\n",
		started.Format(time.RFC3339), time.Now().Format(time.RFC3339), a.T.Socket, b.T.Socket, nonce, report.String(),
		realReport.String()))
}

func mustHome(t *testing.T) string {
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func walkNames(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ---------------------------------------------------------------------------
// F1: per-server processes die with their server
// ---------------------------------------------------------------------------

// TestE2E_Observe_ExitsWithServer (F1, D8): a standalone `tt observe
// --tmux-socket S` — what the sidebar's start button runs when no daemon
// takes the server — exits within a few seconds of its server ending.
func TestE2E_Observe_ExitsWithServer(t *testing.T) {
	s := cold(t, tmuxtest.WithHarnessLines("set -g @moox-sockets 'nothing-matches'"))
	cmd := exec.Command(filepath.Join(s.Bin, "tt"), "observe", "--tmux-socket", s.T.Socket)
	cmd.Env = append(scratchEnv(s), "CLAUDE_CONFIG_DIR="+filepath.Join(s.Home, ".claude"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	sock := filepath.Join(s.Paths(t).TTState, "observer.sock")
	if !tmuxtest.Poll(15*time.Second, 50*time.Millisecond, func() bool { _, err := os.Stat(sock); return err == nil }) {
		t.Fatalf("the observer never listened:\n%s", stderr.String())
	}
	s.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("tt observe (pid %d) still runs 10 s after its server ended:\n%s", cmd.Process.Pid, stderr.String())
	}
	if !strings.Contains(stderr.String(), "SERVER-GONE") {
		t.Fatalf("it did not exit because the server went:\n%s", stderr.String())
	}
}
