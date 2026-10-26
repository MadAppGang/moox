package engine

import (
	"io"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// TestRestore_SessionArgs (CI run 40244726496, tmux 3.6a and 3.8): tmux
// vis-encodes session names, so a restore creates each session with the
// name decoded — `x\\y` as a save read it back is created as `x\y` and
// stored as `x\\y` again, not one backslash longer per restore. Before 3.7
// ':' and '.' become '_' as well, and a second name that would be stored the
// same gets a "-2" suffix instead of a "duplicate session" failure.
func TestRestore_SessionArgs(t *testing.T) {
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{{Name: "a:b"}, {Name: "a.b"}, {Name: `x\\y`}, {Name: "main"}}}
	e := &Engine{log: io.Discard, now: time.Now}
	for _, c := range []struct {
		ver  string
		want map[string]string // snapshot name → -s argument
	}{
		{"3.6a", map[string]string{"a:b": "a:b", "a.b": "a.b-2", `x\\y`: `x\y`, "main": "main"}},
		{"3.8", map[string]string{"a:b": "a:b", "a.b": "a.b", `x\\y`: `x\y`, "main": "main"}},
	} {
		v, err := tmuxtarget.ParseVersion(c.ver)
		if err != nil {
			t.Fatal(err)
		}
		r := &restorer{e: e, ver: v}
		r.sessArg = r.sessionArgs(snap)
		stored := map[string]bool{}
		for name, want := range c.want {
			if got := r.sessionArgOf(name); got != want {
				t.Errorf("tmux %s: %q is created as %q, want %q", c.ver, name, got, want)
			}
			if stored[r.storedName(name)] {
				t.Errorf("tmux %s: two sessions would be stored as %q", c.ver, r.storedName(name))
			}
			stored[r.storedName(name)] = true
		}
		if got := r.storedName(`x\\y`); got != `x\\y` {
			t.Errorf("tmux %s: %q would be stored as %q, not unchanged", c.ver, `x\\y`, got)
		}
	}
}
