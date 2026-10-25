package sockserve_test

// Code review round 2, A1 and A2: the read-only guarantee, held against the
// two ways the old checks could be walked around.
//
//	A1  `tt observe --stop` deleted whatever path the pid file's "socket" key
//	    named, whenever the recorded pid was gone. A stale or hand-edited
//	    record naming a Claude transcript turned the ordinary cleanup command
//	    into an unlink of that transcript.
//	A2  containment compared path SPELLINGS. A state directory reached
//	    through a symlink (itself, or any ancestor) into the Claude tree
//	    passed the lexical check, and a symlinked observer.pid was followed by
//	    os.WriteFile and truncated its target.
//
// Every fixture below builds a stand-in Claude tree in a temp directory and
// asserts on the stand-in's CONTENT, not on an error string: the property is
// that the victim survives, and an error that was returned after the damage
// was done would satisfy a weaker test.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
)

// deadPID is a pid the kernel cannot ever have allocated: darwin's ceiling is
// 99998, so kill() answers ESRCH by construction and no test here can ever
// nominate a real process as the "stale" one.
const deadPID = 1 << 30

// victimFile writes a file standing in for a Claude transcript and returns its
// path and content.
func victimFile(t *testing.T, claude string) (string, []byte) {
	t.Helper()
	dir := filepath.Join(claude, "projects", "-Users-someone")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a-real-conversation.jsonl")
	content := []byte(`{"type":"user","message":"the user's own words"}` + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func assertUntouched(t *testing.T, path string, want []byte) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the victim %s is gone: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("the victim %s is no longer a regular file (%v)", path, info.Mode())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the victim: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the victim was rewritten: %q, want %q", got, want)
	}
}

// entriesUnder lists every path below root, so a test can say "the Claude tree
// gained nothing" rather than "this one file was not created".
func entriesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// A1 (a): a pid file naming an unrelated regular file cannot delete it.
func TestStopNeverDeletesAFileThePidRecordNamesOutsideTheStateDirectory(t *testing.T) {
	dir := tempStateDir(t)
	claude := tempStateDir(t)
	victim, content := victimFile(t, claude)

	// Stale (the pid is gone) and pointing somewhere it must never point.
	writePidFile(t, dir, deadPID, victim)

	out, err := sockserve.Stop(dir, dir+"-claude-root")
	if err != nil {
		t.Logf("Stop returned %v", err)
	}
	assertUntouched(t, victim, content)
	if out.Signalled {
		t.Fatalf("Stop signalled on a record it should have refused: %+v", out)
	}
	if !out.Refused || out.Reason == "" {
		t.Errorf("Stop reported %+v; a record whose socket disagrees with the state "+
			"directory must be refused, with a reason", out)
	}
	// And the refusal is total: the pid file is left for the user, exactly as
	// the other refusal path leaves it.
	if _, err := os.Lstat(filepath.Join(dir, sockserve.PidName)); err != nil {
		t.Errorf("a refused Stop removed the pid file: %v", err)
	}
}

// A1 (a'): even the RIGHT path is removed only when it is really a socket. A
// regular file sitting at the socket's name is somebody's data, and --stop is
// not the command that gets to decide it is garbage.
func TestStopRemovesOnlyASocketAtTheSocketName(t *testing.T) {
	dir := tempStateDir(t)
	sock := filepath.Join(dir, sockserve.SocketName)
	content := []byte("not a socket, and not ours to delete")
	if err := os.WriteFile(sock, content, 0o600); err != nil {
		t.Fatal(err)
	}
	writePidFile(t, dir, deadPID, sock)

	out, err := sockserve.Stop(dir, dir+"-claude-root")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertUntouched(t, sock, content)
	if out.Signalled {
		t.Fatalf("Stop signalled: %+v", out)
	}
	if _, err := os.Lstat(filepath.Join(dir, sockserve.PidName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale pid file survived (err = %v); only the non-socket should have", err)
	}
}

// A2 (b), ancestor form: the configured state directory is spelled outside the
// Claude tree but one of its ancestors is a symlink INTO it.
func TestAStateDirectoryWhoseAncestorIsASymlinkIntoTheClaudeTreeIsRefused(t *testing.T) {
	claude := tempStateDir(t)
	links := tempStateDir(t)
	alias := filepath.Join(links, "looks-harmless")
	if err := os.Symlink(claude, alias); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(alias, "tt")

	h := newHarness(t, func(o *options) {
		o.dir = state
		o.claudeRoot = claude
		o.dontListen = true
	})
	err := h.srv.Listen()
	if err == nil {
		_ = h.srv.Close()
	}
	if !errors.Is(err, sockserve.ErrOutsideStateDir) {
		t.Errorf("Listen returned %v, want ErrOutsideStateDir", err)
	}
	if got := entriesUnder(t, claude); len(got) != 0 {
		t.Fatalf("the Claude tree gained %v through a symlinked ancestor", got)
	}
}

// A2 (b), direct form: the state directory itself is a symlink into the tree.
func TestAStateDirectoryThatIsASymlinkIntoTheClaudeTreeIsRefused(t *testing.T) {
	claude := tempStateDir(t)
	inside := filepath.Join(claude, "state")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	links := tempStateDir(t)
	state := filepath.Join(links, "tt")
	if err := os.Symlink(inside, state); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(o *options) {
		o.dir = state
		o.claudeRoot = claude
		o.dontListen = true
	})
	err := h.srv.Listen()
	if err == nil {
		_ = h.srv.Close()
	}
	if !errors.Is(err, sockserve.ErrOutsideStateDir) {
		t.Errorf("Listen returned %v, want ErrOutsideStateDir", err)
	}
	if got := entriesUnder(t, inside); len(got) != 0 {
		t.Fatalf("the Claude tree gained %v through a symlinked state directory", got)
	}
}

// A2 (c): observer.pid is a symlink. Startup must refuse it — not follow it,
// and not quietly replace it either — and its target must be byte-identical
// afterwards.
func TestASymlinkedPidFileIsRefusedAndItsTargetIsUntouched(t *testing.T) {
	dir := tempStateDir(t)
	claude := tempStateDir(t)
	victim, content := victimFile(t, claude)
	if err := os.Symlink(victim, filepath.Join(dir, sockserve.PidName)); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(o *options) {
		o.dir = dir
		o.claudeRoot = claude
		o.dontListen = true
	})
	err := h.srv.Listen()
	if err == nil {
		_ = h.srv.Close()
	}
	assertUntouched(t, victim, content)
	if err == nil {
		t.Fatal("Listen succeeded over a symlinked pid file")
	}
	// Nothing is left listening or lying around from the aborted start.
	if _, serr := os.Lstat(filepath.Join(dir, sockserve.SocketName)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("an aborted Listen left the socket behind (err = %v)", serr)
	}
	if info, lerr := os.Lstat(filepath.Join(dir, sockserve.PidName)); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlinked pid file was replaced or removed (info %v, err %v); a "+
			"refusal leaves what it found", info, lerr)
	}
}

// A1 + A2 together: --stop pointed (through a symlink) at a directory inside
// the Claude tree refuses the directory itself, and removes nothing there even
// though the "stale" pid file would otherwise qualify for cleanup.
func TestStopRefusesAStateDirectoryThatResolvesIntoTheClaudeTree(t *testing.T) {
	claude := tempStateDir(t)
	inside := filepath.Join(claude, "state")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	writePidFile(t, inside, deadPID, filepath.Join(inside, sockserve.SocketName))
	pid, err := os.ReadFile(filepath.Join(inside, sockserve.PidName))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tempStateDir(t), "state")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}

	out, err := sockserve.StopIn(sockserve.StopConfig{Dir: alias, ClaudeRoot: claude})
	if !errors.Is(err, sockserve.ErrOutsideStateDir) {
		t.Errorf("StopIn returned (%+v, %v), want ErrOutsideStateDir", out, err)
	}
	if out.Signalled || out.StaleCleaned {
		t.Errorf("StopIn acted inside the Claude tree: %+v", out)
	}
	assertUntouched(t, filepath.Join(inside, sockserve.PidName), pid)
}

// A record written by a server started with --socket-dir spelled through a
// symlink still matches when --stop spells it canonically (and vice versa):
// the comparison is by location, not by spelling, so the containment rule does
// not turn an honest record into a refusal.
func TestStopAcceptsARecordSpellingTheSameDirectoryThroughASymlink(t *testing.T) {
	real := tempStateDir(t)
	alias := filepath.Join(tempStateDir(t), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	writePidFile(t, real, deadPID, filepath.Join(alias, sockserve.SocketName))

	out, err := sockserve.Stop(real, real+"-claude-root")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if out.Refused || !out.StaleCleaned {
		t.Fatalf("Stop reported %+v, want the stale pid file cleaned", out)
	}
}
