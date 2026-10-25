package procmember

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/mooxroot"
)

func withTable(t *testing.T, out string) {
	t.Helper()
	prev := runPS
	runPS = func(context.Context) ([]byte, error) { return []byte(out), nil }
	t.Cleanup(func() { runPS = prev })
}

const table = `    1     0 Mon Oct 19 20:32:48 2026
  100     1 Mon Oct 19 20:40:00 2026
  200   100 Mon Oct 19 20:41:00 2026
  300   200 Tue Oct 20 09:00:00 2026
  400     1 Mon Oct  5 08:00:00 2026
  500   400 Tue Oct 20 09:00:01 2026
`

func TestOwnsClimbsUpToThisServersPanePids(t *testing.T) {
	withTable(t, table)
	m, err := New(Config{}).Membership(context.Background(), []int{100})
	if err != nil {
		t.Fatal(err)
	}
	for pid, want := range map[int]bool{100: true, 200: true, 300: true, 500: false, 400: false} {
		got, err := m.Owns(pid)
		if err != nil || got != want {
			t.Errorf("Owns(%d) = %v, %v; want %v", pid, got, err, want)
		}
	}
	if _, err := m.Owns(999); err == nil {
		t.Error("Owns of a pid absent from the table answered; it must be \"could not tell\"")
	}
}

func TestOwnsStopsAfterMaxHops(t *testing.T) {
	var b strings.Builder
	b.WriteString("    2     1 Mon Oct 19 20:32:48 2026\n")
	for pid := 3; pid <= 3+MaxHops+2; pid++ {
		b.WriteString(strconv.Itoa(pid) + " " + strconv.Itoa(pid-1) + " Mon Oct 19 20:32:48 2026\n")
	}
	withTable(t, b.String())
	m, err := New(Config{}).Membership(context.Background(), []int{2})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.Owns(2 + MaxHops); !ok {
		t.Errorf("a pid %d hops below the pane was not owned", MaxHops)
	}
	if ok, _ := m.Owns(2 + MaxHops + 1); ok {
		t.Errorf("a pid %d hops below the pane was owned; the bound is %d", MaxHops+1, MaxHops)
	}
}

func TestRecycledComparesTheByPidStartToken(t *testing.T) {
	withTable(t, table)
	dir := t.TempDir()
	write := func(pid int, body string) {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".start"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(200, "Mon_Oct_19_20:41:00_2026_\n") // the same process
	write(300, "Sun_Oct_18_11:11:11_2026_")   // pid 300 was somebody else then
	write(400, "Mon_Oct_5_08:00:00_2026")     // no trailing '_' (forgiven), blanks squeezed
	m, err := New(Config{ByPidDir: func() string { return dir }}).Membership(context.Background(), []int{100})
	if err != nil {
		t.Fatal(err)
	}
	for pid, want := range map[int]bool{200: false, 300: true, 400: false, 500: false} {
		if got := m.Recycled(pid); got != want {
			t.Errorf("Recycled(%d) = %v, want %v", pid, got, want)
		}
	}
}

// TestStartToken_GoMatchesShell (R2-H14, PR-M16): the token Go derives from ONE
// `ps -axo pid=,ppid=,lstart=` row is byte-identical to what the shell writes
// with a single-pid `ps -o lstart=` (squeezed to '_', outer '_' trimmed) —
// for this process and its parent, under LC_ALL=C. The shell side is the
// vendored cc_common.sh's own `_cc_start_token` (the function the hooks run,
// sourced from share/moox, not a copy), plus the literal pipeline it spells out.
// Neither may be skipped: a missing file or function is a failure.
func TestStartToken_GoMatchesShell(t *testing.T) {
	out, err := runPS(context.Background())
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	tbl := parse(out)
	lib := vendoredCommon(t)
	if lib == "" {
		t.Fatal("share/moox/plugins/tmux-claude-continuity/scripts/lib/cc_common.sh not found: the parity source is missing")
	}
	for _, pid := range []int{os.Getpid(), os.Getppid()} {
		p, ok := tbl[pid]
		if !ok {
			t.Fatalf("pid %d is not in the ps table", pid)
		}
		cmd := exec.Command("/bin/sh", "-c", `t=$(LC_ALL=C ps -o lstart= -p "$1" | tr -s '[:space:]' '_'); t=${t#_}; printf '%s' "${t%_}"`, "sh", strconv.Itoa(pid))
		sh, err := cmd.Output()
		if err != nil {
			t.Fatalf("shell token: %v", err)
		}
		if string(sh) != p.token {
			t.Errorf("pid %d: shell token %q, Go token %q", pid, sh, p.token)
		}

		// Sourced with a scrubbed environment: an inherited _CC_COMMON_LOADED
		// would make the file return before defining anything, and an inherited
		// TMUX would point its TMUX_CMD at whatever server ran the tests.
		cmd = exec.Command("/bin/sh", "-c", `unset _CC_COMMON_LOADED TMUX TMUX_CMD; . "$1" >/dev/null 2>&1; command -v _cc_start_token >/dev/null || exit 3; _cc_start_token "$2"`,
			"sh", lib, strconv.Itoa(pid))
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + t.TempDir()}
		got, err := cmd.Output()
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
			t.Fatalf("%s defines no _cc_start_token", lib)
		}
		if err != nil {
			t.Fatalf("_cc_start_token: %v", err)
		}
		if string(got) != p.token {
			t.Errorf("pid %d: _cc_start_token %q, Go token %q", pid, got, p.token)
		}
		if len(got) == 0 || string(got) == "-" {
			t.Errorf("pid %d: _cc_start_token gave %q for a live process", pid, got)
		}
	}
	if StartToken("") != "-" || StartToken("   \n") != "-" {
		t.Error("an empty lstart must give the shell's floor \"-\"")
	}
}

func vendoredCommon(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	share, err := mooxroot.DevShare(wd)
	if err != nil {
		return ""
	}
	p := filepath.Join(share, "plugins", "tmux-claude-continuity", "scripts", "lib", "cc_common.sh")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
