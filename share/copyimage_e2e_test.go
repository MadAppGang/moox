//go:build tmuxe2e

package share_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// stubRpaster serves the three rpaster endpoints paste.sh uses on a Unix
// socket ONLY, answering with its own image bytes.
func stubRpaster(t *testing.T, sock string, image []byte) *atomic.Int64 {
	t.Helper()
	if _, err := os.Lstat(sock); err == nil {
		t.Skipf("%s already exists (not this test's): refusing to replace it", sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hits := &atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/image/meta", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprintf(w, "AVAILABLE=true\nFORMAT=png\nWIDTH=1\nHEIGHT=1\nSIZE_BYTES=%d\n", len(image))
	})
	mux.HandleFunc("/image", func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = w.Write(image) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock) })
	return hits
}

// tcpSentinel listens on a loopback port and counts every connection: the
// TCP fallback paste.sh and find-socket.sh take when no socket resolves.
func tcpSentinel(t *testing.T) (port int, hits *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hits = &atomic.Int64{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port, hits
}

// rpasterSocketFor is find-socket.sh's priority-1 path for an SSH_CONNECTION
// (the first 16 hex of its sha256), derived here independently.
func rpasterSocketFor(conn string) string {
	sum := sha256.Sum256([]byte(conn))
	return "/tmp/rpaster-" + hex.EncodeToString(sum[:])[:16] + ".sock"
}

// TestE2E_CopyImage_UnixOnlyEndpointPerSession (architecture §7 copy-image
// row, R3-H3, G3-H5, G3-2-M2): two sessions on a server that loaded moox.conf,
// each from its own SSH connection, each with a stub rpaster listening ONLY on
// that connection's Unix socket. The plugin's session-created[105] hook
// (find-socket.sh) writes RPASTER_SOCKET into each SESSION's environment (never
// -g), and paste.sh run as the prefix V job runs it — no TMUX_PANE, the session
// and pane ids as $1/$2 — resolves its own session's socket: each pane gets the
// path of a file holding ITS stub's image, and the TCP fallback port is never
// touched.
func TestE2E_CopyImage_UnixOnlyEndpointPerSession(t *testing.T) {
	for _, tool := range []string{"curl", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	port, tcpHits := tcpSentinel(t)

	s := tmuxtest.Start(t)
	stubGate(t, s, "1")
	// Before the plugin loads, so its find-socket probes use the sentinel too.
	s.Run(t, "set", "-g", "@clip-image-port", fmt.Sprint(port))
	s.SourceMoox(t)
	if !strings.Contains(s.Run(t, "show-hooks", "-g"), "session-created[105]") {
		t.Fatal("copy-image's session-created[105] hook is not registered")
	}

	// Connections from the documentation ranges (RFC 5737), random ports so
	// the derived /tmp socket paths are this run's alone.
	nonce := make([]byte, 2)
	_, _ = rand.Read(nonce)
	type sess struct {
		name, conn, sock, pane string
		image                  []byte
		hits                   *atomic.Int64
	}
	sessions := []*sess{
		{name: "ci-a", conn: fmt.Sprintf("192.0.2.10 %d 198.51.100.1 22", 40000+int(nonce[0])), image: []byte("IMAGE-OF-SESSION-A")},
		{name: "ci-b", conn: fmt.Sprintf("203.0.113.20 %d 198.51.100.1 22", 50000+int(nonce[1])), image: []byte("IMAGE-OF-SESSION-B")},
	}
	for _, x := range sessions {
		x.sock = rpasterSocketFor(x.conn)
		x.hits = stubRpaster(t, x.sock, x.image)
	}
	for _, x := range sessions {
		s.Run(t, "new-session", "-d", "-s", x.name, "-e", "SSH_CONNECTION="+x.conn, "/bin/sh")
		x.pane = strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", x.name+":", "#{pane_id}"))
	}

	// find-socket.sh (a -b job) writes each session's own socket.
	for _, x := range sessions {
		want := "RPASTER_SOCKET=" + x.sock
		if !tmuxtest.Poll(10*time.Second, 100*time.Millisecond, func() bool {
			out, _ := s.RunErr("show-environment", "-t", x.name, "RPASTER_SOCKET")
			return strings.TrimSpace(out) == want
		}) {
			out, _ := s.RunErr("show-environment", "-t", x.name, "RPASTER_SOCKET")
			t.Fatalf("session %s: RPASTER_SOCKET = %q, want %q (find-socket.sh did not write the session scope)", x.name, strings.TrimSpace(out), want)
		}
	}
	if out, _ := s.RunErr("show-environment", "-g", "RPASTER_SOCKET"); strings.Contains(out, "RPASTER_SOCKET=") {
		t.Errorf("RPASTER_SOCKET is set globally (%q): one value would serve every session", strings.TrimSpace(out))
	}

	paste := filepath.Join(s.Share, "plugins", "tmux-copy-image", "plugin", "scripts", "paste.sh")
	if s.Share == "" {
		share, err := tmuxtest.ShareRoot()
		if err != nil {
			t.Fatal(err)
		}
		paste = filepath.Join(share, "plugins", "tmux-copy-image", "plugin", "scripts", "paste.sh")
	}
	pid := strings.TrimSpace(s.Run(t, "display-message", "-p", "#{pid}"))
	for _, x := range sessions {
		sid := strings.TrimSpace(s.Run(t, "display-message", "-p", "-t", x.name+":", "#{session_id}"))
		saveDir := t.TempDir()
		before := x.hits.Load()
		// As the binding's job runs it: TMUX set, no TMUX_PANE, ids as $1 $2.
		cmd := exec.Command("bash", paste, sid, x.pane)
		cmd.Env = s.Env("TMUX="+s.T.Socket+","+pid+",0", "TMPDIR="+saveDir)
		cmd.Dir = s.Home
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("session %s: paste.sh: %v: %s", x.name, err, out)
		}
		if x.hits.Load()-before < 3 {
			t.Errorf("session %s: its own stub saw %d requests, want health + meta + image", x.name, x.hits.Load()-before)
		}
		files, _ := filepath.Glob(filepath.Join(saveDir, "clip-*.png"))
		if len(files) != 1 {
			t.Fatalf("session %s: saved files %v, want one clip-*.png", x.name, files)
		}
		got, _ := os.ReadFile(files[0])
		if string(got) != string(x.image) {
			t.Errorf("session %s: pasted image %q, want its own %q", x.name, got, x.image)
		}
		if !tmuxtest.Poll(5*time.Second, 100*time.Millisecond, func() bool {
			return strings.Contains(s.Run(t, "capture-pane", "-p", "-J", "-t", x.pane), files[0])
		}) {
			t.Errorf("session %s: pane %s never showed the pasted path %s", x.name, x.pane, files[0])
		}
	}
	if n := tcpHits.Load(); n != 0 {
		t.Errorf("the TCP fallback port was connected to %d times: a session fell back from its Unix socket", n)
	}
}
