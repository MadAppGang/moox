package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// twoWindowResurrect is a resurrect save with two windows (v2 layouts).
const twoWindowResurrect = "pane\tapi\t1\t1\t:*\t1\tt\t:/\t1\tzsh\t:\n" +
	"pane\tapi\t2\t0\t:-\t1\tu\t:/\t1\tzsh\t:\n" +
	"window\tapi\t1\t:w\t1\t:*\t" +
	`{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":1,"I":"%7"}}` + "\toff\n" +
	"window\tapi\t2\t:v\t0\t:-\t" +
	`{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":1,"I":"%8"}}` + "\toff\n" +
	"state\tapi\n"

func seedResurrect(t *testing.T, e *Engine, body string) {
	t.Helper()
	dir := e.paths.Snapshots
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "tmux_resurrect_20261024T004046.txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, filepath.Join(dir, "last")); err != nil {
		t.Fatal(err)
	}
}

// TestGate_ImportAvailable (PR-M4, D12): no valid moox snapshot and an
// importable resurrect `last` with more than one window → the cold gate
// decides a boot restore of the import; one window → SKIPPED:one-window;
// nothing to import → SKIPPED:no-snapshot. The gate writes nothing to the
// snapshot directory: the import is published by the restore.
func TestGate_ImportAvailable(t *testing.T) {
	f := newRFake()
	e, _, _ := newRestoreEngine(t, f)
	seedResurrect(t, e, twoWindowResurrect)
	g, err := e.BootGate(context.Background(), 0)
	if err != nil || !g.Restore || !g.Import || g.Reason != "restore-import" {
		t.Fatalf("%+v %v", g, err)
	}
	if names := mooxNames(t, e); len(names) != 0 {
		t.Fatalf("the gate published %v", names)
	}

	f1 := newRFake()
	e1, _, _ := newRestoreEngine(t, f1)
	seedResurrect(t, e1, twoWindowResurrect)
	// a one-window resurrect save
	one := "pane\tapi\t1\t1\t:*\t1\tt\t:/\t1\tzsh\t:\nwindow\tapi\t1\t:w\t1\t:*\t" +
		`{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":1,"I":"%7"}}` + "\toff\nstate\tapi\n"
	if err := os.WriteFile(filepath.Join(e1.paths.Snapshots, "tmux_resurrect_20261024T004046.txt"), []byte(one), 0o600); err != nil {
		t.Fatal(err)
	}
	if g, _ := e1.BootGate(context.Background(), 0); g.Restore || g.Result != BootSkippedPrefix+"one-window" {
		t.Fatalf("one window: %+v", g)
	}

	f2 := newRFake()
	e2, _, _ := newRestoreEngine(t, f2)
	if g, _ := e2.BootGate(context.Background(), 0); g.Restore || g.Result != BootSkippedPrefix+"no-snapshot" || g.Import {
		t.Fatalf("nothing to import: %+v", g)
	}

	// A valid moox snapshot wins: the import is not even considered.
	f3 := newRFake()
	e3, _, _ := newRestoreEngine(t, f3)
	seedResurrect(t, e3, twoWindowResurrect)
	publish(t, e3, snapOf("a:0:1", "a:1:1"))
	if g, _ := e3.BootGate(context.Background(), 0); !g.Restore || g.Import {
		t.Fatalf("with a moox snapshot: %+v", g)
	}
}

func mooxNames(t *testing.T, e *Engine) []string {
	t.Helper()
	ents, _ := os.ReadDir(e.paths.Snapshots)
	var out []string
	for _, en := range ents {
		if n := en.Name(); len(n) > 5 && n[:5] == "moox-" {
			out = append(out, n)
		}
	}
	return out
}

// TestRestore_BootPublishesImportFirst (R2-H10): the boot restore of a
// socket with no moox snapshot publishes the import (source
// import-resurrect, moox-last pointing at it) under the lock it holds, then
// restores from that file; a manual restore never imports.
func TestRestore_BootPublishesImportFirst(t *testing.T) {
	f := newRFake()
	f.addSession("boot")
	e, _, log := newRestoreEngine(t, f)
	seedResurrect(t, e, twoWindowResurrect)
	res, err := e.Restore(context.Background(), RestoreRequest{Kind: KindBoot, Stub: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	<-res.Launch
	last, err := e.Last()
	if err != nil || last.Snapshot.Source != snapshot.SourceImport || res.Source != last.Path {
		t.Fatalf("Last %+v %v; restored from %s\n%s", last, err, res.Source, log)
	}
	if len(res.Created) != 2 {
		t.Fatalf("created %v", res.Created)
	}

	g := newRFake()
	g.addSession("boot")
	e2, _, _ := newRestoreEngine(t, g)
	seedResurrect(t, e2, twoWindowResurrect)
	if _, err := e2.Restore(context.Background(), RestoreRequest{Kind: KindRestore, Stub: true}); err == nil {
		t.Fatal("a manual restore with no moox snapshot imported")
	}
	if names := mooxNames(t, e2); len(names) != 0 {
		t.Fatalf("a manual restore published %v", names)
	}
}
