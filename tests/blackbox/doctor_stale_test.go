package blackbox_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ageWholeTree(t *testing.T, root string, at time.Time) {
	t.Helper()
	paths := []string{}
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink == 0 {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Children first, root last. All descendant mtimes count toward "newest".
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// TEST-40, REQ-10: stale means BOTH missing socket AND newest mtime >14 days.
// Use 13/15 days, not a wall-clock race at precisely the 14-day boundary.
// No socket location/key-derivation algorithm is invented: actual gate exports
// are obtained before shutting down the owned servers.
func TestDoctor_FixStaleRemovesOnlyOldMissingSocketTreesAndNeverLegacyDefault(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	old := e.newServer("stale-old", "SELF", false)
	recent := e.newServer("stale-recent", "SELF", false)
	live := e.newServer("stale-live", "SELF", false)
	type tree struct{ moox, tt string }
	roots := map[*server]tree{}
	for _, s := range []*server{old, recent, live} {
		s.waitReady()
		key := s.globalEnv("MOOX_STATE_KEY")
		roots[s] = tree{filepath.Join(e.state, "moox", "sockets", key), filepath.Join(e.state, "tt", "sockets", key)}
		for _, dir := range []string{roots[s].moox, roots[s].tt} {
			writeFile(t, filepath.Join(dir, "blackbox-preserve"), e.nonce+"\n", 0600)
		}
	}
	var daemons []int
	for _, s := range []*server{old, recent, live} {
		for _, pid := range e.daemonPIDs(s) {
			if pid != old.pid && pid != recent.pid && pid != live.pid {
				daemons = append(daemons, pid)
			}
		}
	}
	old.stop()
	recent.stop()
	// Leave the owned socket alive but stop its daemon before aging its files, so
	// background log writes cannot make the old-but-live branch vacuously recent.
	requireCode(t, e.run(e.tt, nil, "", "serve", "--stop", "--tmux-socket", live.path), 0)
	// A daemon logs its exit after its server ends; aging before that write
	// would make the "old" tree recent again (a harness race, not a product bug).
	e.waitDaemonsGone(daemons)
	// tmux leaves its socket file behind when the server exits; §4.2 stale
	// requires that the socket file does NOT exist. Remove only our own dead
	// servers' socket files (inside the isolated root).
	for _, s := range []*server{old, recent} {
		e.requireInside(s.path)
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	age := time.Now().Add(-15 * 24 * time.Hour)
	for _, dir := range []string{roots[old].moox, roots[old].tt, roots[live].moox, roots[live].tt} {
		ageWholeTree(t, dir, age)
	}
	// A recent CHILD must keep an otherwise-old tree alive.
	for _, dir := range []string{roots[recent].moox, roots[recent].tt} {
		ageWholeTree(t, dir, age)
		child := filepath.Join(dir, "blackbox-preserve")
		young := time.Now().Add(-13 * 24 * time.Hour)
		if err := os.Chtimes(child, young, young); err != nil {
			t.Fatal(err)
		}
	}
	legacy := []string{
		filepath.Join(e.state, "moox", "sockets", "default"),
		filepath.Join(e.state, "tt"), // assert only the direct legacy payload, not sockets/*
		filepath.Join(e.data, "tmux", "resurrect"),
		filepath.Join(e.home, ".config", "tmux-claude"),
	}
	preserved := map[string]string{}
	for _, dir := range legacy {
		path := filepath.Join(dir, "legacy-nonce")
		writeFile(t, path, "legacy-"+e.nonce+"\n", 0600)
		if err := os.Chtimes(path, age, age); err != nil {
			t.Fatal(err)
		}
		preserved[path] = string(readFile(t, path))
	}
	ageWholeTree(t, legacy[0], age)
	ageWholeTree(t, legacy[2], age)
	ageWholeTree(t, legacy[3], age)
	// The fixture deliberately stops the live server's daemon, so doctor may
	// legitimately report that check as failed. §12.4 defines no doctor-specific
	// exit for a failed health check; only usage/unknown codes are wrong here.
	requireDoctorRan := func(r result) {
		t.Helper()
		if r.code != 0 && r.code != 1 {
			t.Fatalf("doctor exit=%d, want 0 or 1 (a reported failed check)\n%s%s", r.code, r.out, r.err)
		}
	}
	// Evidence for the stale precondition (diagnostic, not an oracle).
	for _, s := range []*server{old, recent, live} {
		_, sockErr := os.Lstat(s.path)
		newest := time.Time{}
		for _, dir := range []string{roots[s].moox, roots[s].tt} {
			_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if info, err := os.Lstat(path); err == nil && info.ModTime().After(newest) {
					newest = info.ModTime()
				}
				return nil
			})
		}
		t.Logf("precondition %s: socket stat err=%v, newest mtime age=%s", s.name, sockErr, time.Since(newest).Round(time.Hour))
	}
	r := e.mooxRun("doctor", "--all")
	requireDoctorRan(r)
	// Human report field names aren't supplied; the actual stale key or absolute
	// path must be visible, rather than a vacuous nonempty stdout assertion.
	if !strings.Contains(r.out+r.err, filepath.Base(roots[old].moox)) {
		t.Errorf("doctor did not list stale key %q: %s%s", filepath.Base(roots[old].moox), r.out, r.err)
	}
	for _, dir := range []string{roots[old].moox, roots[old].tt} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("diagnostic doctor removed stale tree before --fix: %v", err)
		}
	}
	requireDoctorRan(e.mooxRun("doctor", "--all", "--fix", "stale"))
	for _, dir := range []string{roots[old].moox, roots[old].tt} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("stale tree remains: %s (%v)", dir, err)
		}
	}
	for _, s := range []*server{recent, live} {
		for _, dir := range []string{roots[s].moox, roots[s].tt} {
			if got := string(readFile(t, filepath.Join(dir, "blackbox-preserve"))); got != e.nonce+"\n" {
				t.Errorf("non-stale child changed: %s", dir)
			}
		}
	}
	for path, want := range preserved {
		if got := string(readFile(t, path)); got != want {
			t.Errorf("legacy/default changed: %s", path)
		}
	}
	live.requireActive("1")
}
