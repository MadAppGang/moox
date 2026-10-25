//go:build tmuxe2e

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// snapDir writes snap as a one-snapshot store (json + moox-last) in a temp
// dir, for tmuxtest.WithSnapshot.
func snapDir(t *testing.T, snap *snapshot.Snapshot) string {
	t.Helper()
	dir := t.TempDir()
	b, err := snapshot.Encode(snap)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "moox-20261025T000000.json", string(b))
	writeFile(t, dir, "moox-last", "moox-20261025T000000.json\n")
	return dir
}

func waitOpt(t *testing.T, s *tmuxtest.Server, name string) string {
	t.Helper()
	var v string
	tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool { v = s.Opt(t, name); return v != "" })
	return v
}

func plant(dir, name string) func(*tmuxtest.Server) {
	return func(*tmuxtest.Server) {
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600)
	}
}

// pendingPlant plants pending/<n> in the server's own per-socket dir.
func pendingPlant(t *testing.T, n string) tmuxtest.Option {
	return tmuxtest.WithPrepare(func(s *tmuxtest.Server) { plant(s.Paths(t).Pending, n)(s) })
}

// TestE2E_Gate_ScratchServerNeverRestores (B27, G1, R19, RR5): a server whose
// socket @moox-sockets does not match never restores — not even with a valid
// snapshot under its key.
func TestE2E_Gate_ScratchServerNeverRestores(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithSnapshot(snapDir(t, snapOf("a:0:1", "a:1:1"))),
		tmuxtest.WithHarnessLines("set -g @moox-sockets 'nothing-matches-this'"))
	if got := waitOpt(t, s, "@moox-active"); got != "0" {
		t.Fatalf("@moox-active %q", got)
	}
	time.Sleep(time.Second) // a boot restore, had one been started, would have created `a` by now
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "boot" {
		t.Fatalf("sessions %q", got)
	}
	if v := s.Opt(t, "@moox-boot-result"); v != "" {
		t.Fatalf("@moox-boot-result %q on a non-matching server", v)
	}
}

// TestE2E_Restore_NoSidebarDuringRestore (RR9, U4): tt.conf's sidebar holds
// off for the whole restore — @moox-restoring holds `<kind>:<pid>:<epoch>`,
// so the hold tests "set", not "1" — and no observer lands in a window before
// its saved layout is applied (that made select-layout fail: "have 3 panes
// but need 2", LAYOUT-FALLBACK on the V4 real-data restore).
func TestE2E_Restore_NoSidebarDuringRestore(t *testing.T) {
	wide := func(ids ...string) string {
		root := &layout.Node{Kind: layout.KindH, W: 241, H: 60}
		for i, id := range ids {
			root.Children = append(root.Children, &layout.Node{Kind: layout.KindPane, W: 120, H: 60, X: 121 * i, Last: -1, Z: -1,
				SavedPane: id, Index: i})
		}
		return root.EncodeV1()
	}
	snap := snapOf("wide:0:2", "wide:1:2", "wide:2:1")
	snap.Windows[0].Layout, snap.Windows[1].Layout = wide("%1", "%2"), wide("%3", "%4")
	snap.Windows[2].Layout = (&layout.Node{Kind: layout.KindPane, W: 241, H: 60, Last: -1, Z: -1, SavedPane: "%5"}).EncodeV1()
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithSize(250, 60), tmuxtest.WithSnapshot(snapDir(t, snap)),
		tmuxtest.WithEnv("MOOX_STUB_COMMANDS=1"))
	if !tmuxtest.Poll(60*time.Second, 200*time.Millisecond, func() bool {
		return s.Opt(t, "@moox-boot-result") != "" && s.Opt(t, "@moox-restoring") == ""
	}) {
		t.Fatalf("restore did not finish; daemon.log:\n%s", daemonLog(t, s))
	}
	log := daemonLog(t, s)
	if strings.Contains(log, "LAYOUT-FALLBACK") || !strings.Contains(log, "0 misses, 0 warnings") {
		t.Fatalf("daemon.log:\n%s", log)
	}
}

// TestE2E_Purge_PendingOnColdStart_NoSnapshot (B28, RR7, I3): the cold gate
// purges pending/ even with no snapshot.
func TestE2E_Purge_PendingOnColdStart_NoSnapshot(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), pendingPlant(t, "5"))
	if got := waitOpt(t, s, "@moox-boot-result"); got != "SKIPPED:no-snapshot" {
		t.Fatalf("boot result %q", got)
	}
	if exists(filepath.Join(s.Paths(t).Pending, "5")) {
		t.Fatal("pending/5 survived the cold gate")
	}
}

// TestE2E_Purge_PendingOnColdStart_InvalidSnapshot: an invalid snapshot does
// not stop the purge.
func TestE2E_Purge_PendingOnColdStart_InvalidSnapshot(t *testing.T) {
	bad := t.TempDir()
	writeFile(t, bad, "moox-20261025T000000.json", "{not json")
	writeFile(t, bad, "moox-last", "moox-20261025T000000.json\n")
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithSnapshot(bad), pendingPlant(t, "6"))
	if got := waitOpt(t, s, "@moox-boot-result"); got != "SKIPPED:no-snapshot" {
		t.Fatalf("boot result %q", got)
	}
	if exists(filepath.Join(s.Paths(t).Pending, "6")) {
		t.Fatal("pending/6 survived")
	}
}

// TestE2E_Purge_PendingOnColdStart_Handover (G3, U3): MOOX_NO_AUTO_RESTORE in
// the starting env: HANDOVER, no restore, the variable unset, pending purged.
func TestE2E_Purge_PendingOnColdStart_Handover(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithSnapshot(snapDir(t, snapOf("a:0:1", "a:1:1"))),
		tmuxtest.WithEnv("MOOX_NO_AUTO_RESTORE=1"), pendingPlant(t, "7"))
	if got := waitOpt(t, s, "@moox-boot-result"); got != "HANDOVER" {
		t.Fatalf("boot result %q", got)
	}
	if got := sessionNames(t, s); len(got) != 1 || got[0] != "boot" {
		t.Fatalf("sessions %q: a handover restored", got)
	}
	if env, _ := s.RunErr("show-environment", "-g", "MOOX_NO_AUTO_RESTORE"); strings.HasPrefix(env, "MOOX_NO_AUTO_RESTORE=") {
		t.Fatalf("still set: %q", env)
	}
	if exists(filepath.Join(s.Paths(t).Pending, "7")) || s.Opt(t, "@moox-restore-result") != "" {
		t.Fatal("pending survived, or HANDOVER reached @moox-restore-result")
	}
}

// TestE2E_Purge_PendingOnColdStart_UnknownStartTime: when #{start_time}
// cannot be read the pending purge still runs and launch/ is kept. (A real
// server always reports its start time, so this one runs on the fake.)
func TestE2E_Purge_PendingOnColdStart_UnknownStartTime(t *testing.T) {
	f := newRFake()
	f.start = 0
	e, _, _ := newRestoreEngine(t, f)
	writeFile(t, e.paths.Pending, "8", "x")
	writeFile(t, e.paths.Launch, "8", "x")
	if err := os.Chtimes(filepath.Join(e.paths.Launch, "8"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	g, _ := e.BootGate(context.Background(), 0)
	if exists(filepath.Join(e.paths.Pending, "8")) || !exists(filepath.Join(e.paths.Launch, "8")) || g.StartTime != 0 {
		t.Fatalf("%+v", g)
	}
}

// TestE2E_Purge_LaunchOnColdStart (R15, R16): on a cold start a launch/<N>
// older than the new server is gone, a newer one stays.
func TestE2E_Purge_LaunchOnColdStart(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithPrepare(func(s *tmuxtest.Server) {
		d := s.Paths(t).Launch
		plant(d, "1")(s)
		plant(d, "2")(s)
		_ = os.Chtimes(filepath.Join(d, "1"), time.Unix(1000, 0), time.Unix(1000, 0))
		future := time.Now().Add(time.Hour)
		_ = os.Chtimes(filepath.Join(d, "2"), future, future)
	}))
	waitOpt(t, s, "@moox-boot-result")
	d := s.Paths(t).Launch
	if exists(filepath.Join(d, "1")) || !exists(filepath.Join(d, "2")) {
		t.Fatalf("launch/1 exists=%v (want gone), launch/2 exists=%v (want kept)", exists(filepath.Join(d, "1")), exists(filepath.Join(d, "2")))
	}
}

// TestE2E_Purge_OnlyOwnSocket (X2, X3, R18): a cold start purges only its own
// key's directories.
func TestE2E_Purge_OnlyOwnSocket(t *testing.T) {
	b := tmuxtest.StartCold(t, tmuxtest.WithBinaries())
	waitOpt(t, b, "@moox-boot-result")
	bp := b.Paths(t).Pending
	plant(bp, "9")(b)
	a := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), pendingPlant(t, "5"))
	waitOpt(t, a, "@moox-boot-result")
	if exists(filepath.Join(a.Paths(t).Pending, "5")) {
		t.Fatal("a's own pending survived")
	}
	if !exists(filepath.Join(bp, "9")) {
		t.Fatal("a's cold gate purged b's pending")
	}
}

// TestE2E_Purge_ReloadKeepsPending (R16): a reload is not cold: pending/
// is kept.
func TestE2E_Purge_ReloadKeepsPending(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries())
	waitOpt(t, s, "@moox-boot-result")
	p := s.Paths(t).Pending
	plant(p, "999")(s)
	s.SourceMoox(t)
	if !exists(filepath.Join(p, "999")) {
		t.Fatal("a reload purged pending/")
	}
}

// TestE2E_Hook_InheritedEnvWritesNothing (C1, R17): a scratch server whose
// global env was inherited from a matching server (MOOX_SOCKET names THAT
// server) runs hook shells that consume nothing and record nothing; the same
// delivery on the matching server itself is consumed (positive control).
func TestE2E_Hook_InheritedEnvWritesNothing(t *testing.T) {
	a, ea := hookServer(t)
	env := map[string]string{}
	for _, line := range strings.Split(a.Run(t, "show-environment", "-g"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(k, "MOOX_") {
			env[k] = v
		}
	}
	if env["MOOX_SOCKET"] == "" || env["MOOX_ACTIVE"] != "1" {
		t.Fatalf("a's env %v", env)
	}
	control := filepath.Join(t.TempDir(), "control")
	a.Run(t, "new-window", "-d", "-e", "MOOX_RESUME=touch "+shq(control))
	if !tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool { return exists(control) }) {
		t.Fatal("positive control: a's own hook never consumed MOOX_RESUME")
	}
	before, _ := os.ReadDir(ea.Paths().Launch)

	var kv []string
	for k, v := range env {
		kv = append(kv, k+"="+v)
	}
	c := tmuxtest.Start(t, append([]tmuxtest.Option{tmuxtest.WithShell(tmuxtest.HookZsh(t))}, tmuxtest.WithEnv(kv...))...)
	marker := filepath.Join(t.TempDir(), "inherited")
	pane := strings.TrimSpace(c.Run(t, "new-window", "-d", "-P", "-F", "#{pane_id}", "-e", "MOOX_RESUME=touch "+shq(marker)))
	time.Sleep(2 * time.Second)
	if exists(marker) {
		t.Fatal("the inherited env's MOOX_RESUME ran on the scratch server")
	}
	after, _ := os.ReadDir(ea.Paths().Launch)
	if len(after) != len(before) {
		t.Fatalf("a's launch/ changed: %d → %d entries (pane %s)", len(before), len(after), pane)
	}
}

// TestE2E_Hook_ForeignServerWritesNothing (B5, X1): on a server the gate did
// not match (MOOX_ACTIVE 0), hook shells consume and record nothing, and
// nothing appears under its key.
func TestE2E_Hook_ForeignServerWritesNothing(t *testing.T) {
	s := tmuxtest.StartCold(t, tmuxtest.WithBinaries(), tmuxtest.WithShell(tmuxtest.HookZsh(t)),
		tmuxtest.WithHarnessLines("set -g @moox-sockets 'nothing-matches-this'"))
	if got := waitOpt(t, s, "@moox-active"); got != "0" {
		t.Fatalf("@moox-active %q", got)
	}
	marker := filepath.Join(t.TempDir(), "foreign")
	s.Run(t, "new-window", "-d", "-e", "MOOX_RESUME=touch "+shq(marker))
	time.Sleep(2 * time.Second)
	if exists(marker) {
		t.Fatal("MOOX_RESUME ran on a non-matching server")
	}
	if exists(s.Paths(t).State) {
		t.Fatalf("files under the excluded key: %s", s.Paths(t).State)
	}
}
