package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const importFixture = "pane\tapi\t1\t1\t:*\t1\tt\t:/home/user\t1\tzsh\t:\n" +
	"pane\tapi\t1\t1\t:*\t2\t\t:/home/user/b\t0\tzsh\t4242\t:\n" +
	"window\tapi\t1\t:w\t1\t:*\t" +
	`{"V":2,"L":{"t":"h","w":203,"h":56,"x":0,"y":0,"c":[{"t":"p","w":101,"h":56,"x":0,"y":0,"a":true,"i":1,"I":"%7"},{"t":"p","w":101,"h":56,"x":102,"y":0,"l":0,"i":2,"I":"%9"}]}}` +
	"\toff\nstate\tapi\n"

// importEnv is a scratch HOME/XDG tree; every path the engine derives lives
// under root.
func importEnv(t *testing.T) (string, tmuxtarget.MapEnv) {
	t.Helper()
	root := t.TempDir()
	return root, tmuxtarget.MapEnv{"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": filepath.Join(root, "xs"),
		"XDG_DATA_HOME": filepath.Join(root, "xd"), "XDG_CONFIG_HOME": filepath.Join(root, "xc")}
}

func importOne(t *testing.T, withContents bool) *Imported {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "tmux_resurrect_20261024T004046.txt")
	if err := os.WriteFile(f, []byte(importFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(f, old, old)
	im, err := Import(context.Background(), ImportSource{From: f, Producer: "moox test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withContents {
		im.Contents["%7"] = []byte("scrollback\n")
		im.Snapshot.Panes[0].Contents = true
	}
	return im
}

func engineFor(t *testing.T, socket string, env tmuxtarget.MapEnv, root string) *Engine {
	t.Helper()
	e, err := New(Options{Target: tmuxtarget.Target{Bin: "/nonexistent/tmux", Socket: socket}, Env: env,
		Producer: "moox test"})
	if err != nil {
		t.Fatal(err)
	}
	// Safety: nothing this test publishes may land outside the scratch tree.
	if !strings.HasPrefix(e.Paths().Snapshots, root) || !strings.HasPrefix(e.Paths().State, root) {
		t.Fatalf("paths escape the scratch tree: %s %s", e.Paths().Snapshots, e.Paths().State)
	}
	return e
}

// TestWriteImport_OutFileContentsReachRestore (G1-H6): `moox import --out
// imported.json` writes imported.contents.gz beside it; the restore reader
// accepts that name (sha256-checked) instead of dropping every imported
// scrollback, and still refuses a name that is a path.
func TestWriteImport_OutFileContentsReachRestore(t *testing.T) {
	im := importOne(t, true)
	out := filepath.Join(t.TempDir(), "imported.json")
	if _, err := WriteImport(im, out, filepath.Join(t.TempDir(), ".claude")); err != nil {
		t.Fatal(err)
	}
	root, env := importEnv(t)
	e := engineFor(t, filepath.Join(root, "moox-test-x"), env, root)
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	src, err := e.loadSource(st, out)
	if err != nil {
		t.Fatal(err)
	}
	if src.warning != "" || string(src.contents["%7"]) != "scrollback\n" {
		t.Fatalf("restore read of an --out import: contents %v, warning %q", src.contents, src.warning)
	}
	for _, bad := range []string{"../x.contents.gz", "sub/x.contents.gz", ".contents.gz", "x.json", ""} {
		if contentsNameOK(bad) {
			t.Errorf("contentsNameOK(%q) = true", bad)
		}
	}
}

func TestWriteImport_OutFileRoundTrip(t *testing.T) {
	im := importOne(t, true)
	out := filepath.Join(t.TempDir(), "sub", "imported.json")
	wrote, err := WriteImport(im, out, filepath.Join(t.TempDir(), ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 2 || filepath.Base(wrote[0]) != "imported.contents.gz" || wrote[1] != out {
		t.Fatalf("wrote %v", wrote)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s, err := snapshot.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
	if s.Source != snapshot.SourceImport || s.Contents == nil || s.Contents.File != "imported.contents.gz" || s.Contents.Server != nil {
		t.Fatalf("snapshot %+v contents %+v", s, s.Contents)
	}
	cb, _ := os.ReadFile(wrote[0])
	if snapshot.SHA256(cb) != s.Contents.SHA256 {
		t.Fatal("contents sha256 mismatch")
	}
	_, entries, err := snapshot.DecodeContents(cb)
	if err != nil || string(entries["%7"]) != "scrollback\n" {
		t.Fatalf("entries %v %v", entries, err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}

func TestWriteImport_RefusesResurrectNames(t *testing.T) {
	im := importOne(t, false)
	dir := t.TempDir()
	for _, n := range []string{"last", "tmux_resurrect_20261024T004046.txt", "pane_contents.tar.gz"} {
		if _, err := WriteImport(im, filepath.Join(dir, n), filepath.Join(dir, ".claude")); !errors.Is(err, ErrProtectedOut) {
			t.Errorf("%s: %v", n, err)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("wrote %v", ents)
	}
}

// legacySocket is THE login default server's path, which sockets.Legacy
// recognises. No tmux is ever run against it here: the engine is only asked
// to publish, and every path it derives comes from the scratch environment.
func legacySocket(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Skip(err)
	}
	return filepath.Join(real, "tmux-"+strconv.Itoa(os.Getuid()), "default")
}

func TestImport_PublishDefaultNeedsReplaceLive(t *testing.T) {
	root, env := importEnv(t)
	e := engineFor(t, legacySocket(t), env, root)
	if !e.Paths().Legacy {
		t.Fatal("the legacy socket did not resolve to the default key")
	}
	im := importOne(t, true)
	if _, err := e.PublishImport(context.Background(), im, PublishImportOptions{}); !errors.Is(err, ErrNeedsReplaceLive) {
		t.Fatalf("got %v, want ErrNeedsReplaceLive", err)
	}
	if _, err := os.Stat(e.Paths().Snapshots); err == nil {
		if ents, _ := os.ReadDir(e.Paths().Snapshots); len(ents) != 0 {
			t.Fatalf("a refused publish wrote %v", ents)
		}
	}
	res, err := e.PublishImport(context.Background(), im, PublishImportOptions{ReplaceLive: true})
	if err != nil {
		t.Fatal(err)
	}
	last, err := e.Last()
	if err != nil || last.Name != res.File || last.Snapshot.Source != snapshot.SourceImport ||
		last.Snapshot.Socket.Key != "default" || last.Snapshot.Contents == nil {
		t.Fatalf("Last after publish: %+v %v", last, err)
	}
}

func TestImport_PublishRefusesNewerSave(t *testing.T) {
	root, env := importEnv(t)
	e := engineFor(t, filepath.Join(root, "moox-test-import"), env, root)

	// A saved snapshot, created now: newer than the import's source file.
	saved := importOne(t, false).Snapshot
	saved.Source, saved.CreatedAt = snapshot.SourceSave, time.Now().Format(time.RFC3339)
	st, err := store.Open(e.Paths(), e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	l, err := st.TryLock(e.record())
	if err != nil {
		t.Fatal(err)
	}
	jn, _ := st.NewNames(time.Now())
	if err := st.Publish(l, saved, jn, "", nil); err != nil {
		t.Fatal(err)
	}
	l.Release()
	_ = st.Close()

	im := importOne(t, false)
	if _, err := e.PublishImport(context.Background(), im, PublishImportOptions{}); !errors.Is(err, ErrNewerSave) {
		t.Fatalf("got %v, want ErrNewerSave", err)
	}
	if last, _ := e.Last(); last.Name != jn {
		t.Fatalf("moox-last moved to %s", last.Name)
	}
	res, err := e.PublishImport(context.Background(), im, PublishImportOptions{ReplaceLive: true})
	if err != nil || res.Superseded != jn {
		t.Fatalf("replace-live: %+v %v", res, err)
	}
}

func TestImport_PublishBusy(t *testing.T) {
	root, env := importEnv(t)
	e := engineFor(t, filepath.Join(root, "moox-test-busy"), env, root)
	st, err := store.Open(e.Paths(), e.claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	l, err := st.TryLock(e.record())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := e.PublishImport(context.Background(), importOne(t, false), PublishImportOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}
