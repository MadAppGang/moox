package share_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// TestE2E_Clobber_ShellFreezeAndGoGuardReadSameDir (R2-C2): the freeze store a
// shell freeze writes (cc_store.sh's cc_store_ns_dir, which every freeze and
// thaw writes through) is the directory the Go side reads for the same server
// (sockets.Paths.FreezeDir, which the engine's clobber guard uses): one root,
// one namespace = the gate's MOOX_STATE_KEY, no double namespace. Checked for
// the login default server (key "default", today's frozen/default/) and for
// another socket (key "<name>@<hash>"), with HOME pointing at a scratch dir.
// No tmux server is needed: outside tmux the shell names its server by
// MOOX_SOCKET, exactly as the daemon's heartbeat runs it.
func TestE2E_Clobber_ShellFreezeAndGoGuardReadSameDir(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(tmp, "tmux-"+strconv.Itoa(os.Getuid()), "default")
	other := filepath.Join(t.TempDir(), "work")
	store := filepath.Join(shareDir, "plugins", "tmux-claude-continuity", "scripts", "lib", "cc_store.sh")
	abs, err := filepath.Abs(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, sock := range []string{legacy, other} {
		p, err := sockets.For(sock, tmuxtarget.MapEnv{"HOME": home})
		if err != nil {
			t.Fatal(err)
		}
		key := sockets.Key(sock)
		cmd := exec.Command("bash", "-c", `. "$1"; cc_store_ns_dir`, "_", abs)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "MOOX_SOCKET=" + sock, "MOOX_STATE_KEY=" + key}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: cc_store_ns_dir: %v: %s", sock, err, out)
		}
		shell := strings.TrimSpace(string(out))
		if shell != p.FreezeDir {
			t.Errorf("socket %s (key %s): shell freeze dir %q, Go FreezeDir %q", sock, key, shell, p.FreezeDir)
		}
		if sock == legacy && filepath.Base(shell) != "default" {
			t.Errorf("the login default server's freeze dir is %q, want today's frozen/default", shell)
		}
	}
}
