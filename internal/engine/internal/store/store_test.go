package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/sockets"
)

// fixture is a store over temp dirs: Snapshots and State under one root.
func fixture(t *testing.T) (*Store, sockets.Paths) {
	t.Helper()
	root := t.TempDir()
	p := sockets.Paths{
		Key: "t@00000000", Snapshots: filepath.Join(root, "snapshots"), State: filepath.Join(root, "state"),
		FreezeDir: filepath.Join(root, "frozen"),
	}
	s, err := Open(p, filepath.Join(root, "home", ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, p
}

var base = time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)

// snap builds a valid snapshot with n panes in one window; the first len(sids)
// panes are Claude panes.
func snap(n int, sids ...string) *snapshot.Snapshot {
	s := &snapshot.Snapshot{Format: snapshot.FormatName, Version: snapshot.Version, Source: snapshot.SourceSave,
		Client: &snapshot.Client{Session: "w"}, Sessions: []snapshot.Session{{Name: "w", ActiveWindow: 1, LastWindow: -1}},
		Windows: []snapshot.Window{{Session: "w", Index: 1, ID: "@1", Layout: "0000,", LayoutFormat: snapshot.LayoutV1,
			AutomaticRename: snapshot.AutoRenameUnset}}}
	for i := 1; i <= n; i++ {
		p := snapshot.Pane{Key: snapshot.PaneKey("w", 1, i), Session: "w", Window: 1, Index: i,
			SavedID: fmt.Sprintf("%%%d", i), Role: snapshot.RoleShell}
		if i <= len(sids) {
			p.Role, p.Claude = snapshot.RoleClaude, &snapshot.Claude{SessionID: sids[i-1], Launcher: "claude"}
		}
		s.Panes = append(s.Panes, p)
	}
	return s
}

func lock(t *testing.T, s *Store) *Lock {
	t.Helper()
	l, err := s.TryLock(Record{Pid: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	return l
}

// publish writes snap at time at, with optional contents, and returns its name.
func publish(t *testing.T, s *Store, l *Lock, sn *snapshot.Snapshot, at time.Time, contents map[string][]byte) string {
	t.Helper()
	jn, cn := s.NewNames(at)
	var cb []byte
	if contents != nil {
		var err error
		cb, err = snapshot.EncodeContents(snapshot.Server{Pid: 1, StartTime: 2}, contents)
		if err != nil {
			t.Fatal(err)
		}
		sn.Contents = &snapshot.Contents{File: cn, SHA256: snapshot.SHA256(cb), Server: &snapshot.Server{Pid: 1, StartTime: 2}}
	}
	if err := s.Publish(l, sn, jn, cn, cb); err != nil {
		t.Fatal(err)
	}
	return jn
}

func exists(s *Store, name string) bool {
	_, err := s.snaps.Lstat(name)
	return err == nil
}

func TestParseName(t *testing.T) {
	ok := []string{"moox-20261024T004046.json", "moox-20261024T004046.3.json", "moox-20261024T004046.contents.gz"}
	bad := []string{"last", "tmux_resurrect_20261024T004046.txt", "pane_contents.tar.gz", "moox-last",
		"moox-2026.json", "moox-20261024T004046.json.tmp-x", "moox-20261024T004046.txt"}
	for _, n := range ok {
		if _, k := ParseName(n); !k {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range bad {
		if _, k := ParseName(n); k {
			t.Errorf("%q accepted", n)
		}
	}
	a, _ := ParseName("moox-20261024T004046.json")
	b, _ := ParseName("moox-20261024T004046.0.json")
	if !newer(b, a) {
		t.Error("a same-second .0 must rank newer than the bare name")
	}
}

// TestStore_NeverPublishesInvalid (B19): an invalid snapshot is refused and
// nothing — contents, snapshot or pointer — is written.
func TestStore_NeverPublishesInvalid(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	good := publish(t, s, l, snap(2), base, nil)
	bad := snap(2)
	bad.Windows = nil // the 2026-08-17 truncated shape
	jn, cn := s.NewNames(base.Add(time.Minute))
	err := s.Publish(l, bad, jn, cn, []byte("x"))
	if !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	if exists(s, jn) || exists(s, cn) || s.Pointer() != good {
		t.Fatal("an invalid publish wrote something")
	}
}

// TestStore_FsyncOrder (I2, R21): contents, then the snapshot, then the
// pointer; each durable; the pointer moves last.
func TestStore_FsyncOrder(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	first := publish(t, s, l, snap(1), base, nil)
	var order []string
	s.onWrite = func(name string) {
		order = append(order, name)
		if name == PointerName {
			return
		}
		if s.Pointer() != first {
			t.Errorf("pointer moved before %s was written", name)
		}
	}
	jn := publish(t, s, l, snap(2), base.Add(time.Minute), map[string][]byte{"%1": []byte("x\n")})
	cn := strings.TrimSuffix(jn, ".json") + ".contents.gz"
	if !reflect.DeepEqual(order, []string{cn, jn, PointerName}) {
		t.Fatalf("write order %v", order)
	}
	if s.Pointer() != jn {
		t.Fatal("pointer not moved")
	}
	// the snapshot must name the contents it is published with
	sn := snap(1)
	j2, c2 := s.NewNames(base.Add(2 * time.Minute))
	sn.Contents = &snapshot.Contents{File: c2, SHA256: "wrong"}
	if err := s.Publish(l, sn, j2, c2, []byte("data")); err == nil {
		t.Fatal("published a snapshot whose contents sha256 is not its file's")
	}
}

// TestStore_PointerNeverDangles_ConcurrentSaves (B21, R21, RR3): concurrent
// publishers (serialised by the lock, losers get ErrBusy and retry) and
// retention never leave moox-last naming a missing or invalid file, as seen
// by concurrent readers.
func TestStore_PointerNeverDangles_ConcurrentSaves(t *testing.T) {
	s, _ := fixture(t)
	{
		l := lock(t, s)
		publish(t, s, l, snap(1), base, nil)
		l.Release()
	}
	var tick atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad, moved atomic.Int64
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Pointer-then-Load is two reads: between them the pointer
				// can move on and retention (Count 5) can legally delete the
				// file it named. Dangling means the pointer STILL names a
				// file that does not load; names never repeat (the clock
				// ticks), so an unchanged pointer is the same publish. Seen
				// on a 4-vCPU CI runner (run 40244726496), never on the Mac.
				name := s.Pointer()
				if _, err := s.Load(name); err != nil {
					if now := s.Pointer(); now == name || now == "" {
						bad.Add(1)
						t.Errorf("moox-last %q (now %q) does not load: %v", name, now, err)
					} else {
						moved.Add(1)
					}
				}
			}
		}()
	}
	var pw sync.WaitGroup
	var busy, done atomic.Int64
	for w := 0; w < 6; w++ {
		pw.Add(1)
		go func() {
			defer pw.Done()
			for i := 0; i < 25; {
				l, err := s.TryLock(Record{Pid: os.Getpid()})
				if errors.Is(err, ErrBusy) {
					busy.Add(1)
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				at := base.Add(time.Duration(tick.Add(1)) * time.Second)
				jn, cn := s.NewNames(at)
				sn := snap(1 + i%3)
				cb, _ := snapshot.EncodeContents(snapshot.Server{Pid: 1}, map[string][]byte{"%1": []byte(jn)})
				sn.Contents = &snapshot.Contents{File: cn, SHA256: snapshot.SHA256(cb)}
				if err := s.Publish(l, sn, jn, cn, cb); err != nil {
					t.Error(err)
				}
				if _, err := s.Retain(l, Policy{Count: 5, Days: 0}, at); err != nil {
					t.Error(err)
				}
				l.Release()
				done.Add(1)
				i++
			}
		}()
	}
	pw.Wait()
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d reads found moox-last dangling or invalid", bad.Load())
	}
	if done.Load() != 150 {
		t.Fatalf("%d publishes", done.Load())
	}
	t.Logf("150 publishes, %d busy retries, %d reads raced a newer publish + retention, readers never saw a dangling pointer",
		busy.Load(), moved.Load())
}

// TestStore_HealFromNewest12 (B19, B21, H1–H3, RR11, I8).
func TestStore_HealFromNewest12(t *testing.T) {
	s, p := fixture(t)
	l := lock(t, s)
	var names []string
	for i := 0; i < 14; i++ {
		names = append(names, publish(t, s, l, snap(1+i), base.Add(time.Duration(i)*time.Minute), nil))
	}
	// resurrect's own files sit in the same directory and are never candidates
	for _, f := range []string{"tmux_resurrect_20301231T000000.txt", "last", "pane_contents.tar.gz"} {
		if err := os.WriteFile(filepath.Join(p.Snapshots, f), []byte("pane\tx\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := func(n string) {
		if err := os.WriteFile(filepath.Join(p.Snapshots, n), []byte(`{"format":"moox-snapshot","version":1,"panes":[`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// pointer valid: no heal
	if last, err := s.Last(); err != nil || last.Healed || last.Name != names[13] {
		t.Fatalf("%+v %v", last, err)
	}
	// pointer names a missing file: heal to the newest valid
	if err := os.WriteFile(filepath.Join(p.Snapshots, PointerName), []byte("moox-20000101T000000.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if last, err := s.Last(); err != nil || !last.Healed || last.Name != names[13] {
		t.Fatalf("dangling: %+v %v", last, err)
	}
	// the newest 11 corrupt: the 12th newest is still found
	for _, n := range names[3:] {
		corrupt(n)
	}
	last, err := s.Last()
	if err != nil || last.Name != names[2] || !last.Healed {
		t.Fatalf("12th candidate: %+v %v", last, err)
	}
	if err := s.Heal(l, last.Name); err != nil || s.Pointer() != names[2] {
		t.Fatalf("heal write: %v %q", err, s.Pointer())
	}
	// 12 corrupt candidates: the 13th is never looked at
	corrupt(names[2])
	if err := os.WriteFile(filepath.Join(p.Snapshots, PointerName), []byte(names[13]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Last(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("beyond 12: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(p.Snapshots, "last"))
	if string(b) != "pane\tx\n" {
		t.Fatal("resurrect's last was modified")
	}
}

// TestStore_ContentsBoundToSnapshot (RR14, store side): a snapshot's contents
// are read only if the file it names still has the sha256 it recorded; a
// missing or swapped file drops contents, never pairs a layout with another
// snapshot's scrollback.
func TestStore_ContentsBoundToSnapshot(t *testing.T) {
	s, p := fixture(t)
	l := lock(t, s)
	a := publish(t, s, l, snap(1), base, map[string][]byte{"%1": []byte("A's text\n")})
	b := publish(t, s, l, snap(1), base.Add(time.Minute), map[string][]byte{"%1": []byte("B's text\n")})
	sa, _ := s.Load(a)
	sb, _ := s.Load(b)
	got, err := s.LoadContents(sa)
	if err != nil || string(got["%1"]) != "A's text\n" {
		t.Fatalf("%q %v", got, err)
	}
	// swap B's bytes in under A's name
	bb, _ := os.ReadFile(filepath.Join(p.Snapshots, sb.Contents.File))
	if err := os.WriteFile(filepath.Join(p.Snapshots, sa.Contents.File), bb, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadContents(sa); !errors.Is(err, ErrContentsMismatch) {
		t.Fatalf("swapped: %v", err)
	}
	if err := os.Remove(filepath.Join(p.Snapshots, sb.Contents.File)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadContents(sb); !errors.Is(err, ErrContentsMismatch) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := s.LoadContents(snap(1)); !errors.Is(err, ErrContentsMismatch) {
		t.Fatal("no contents named")
	}
}

// bumpVersion rewrites a published snapshot as one from a newer moox.
func bumpVersion(t *testing.T, p sockets.Paths, name string) {
	t.Helper()
	f := filepath.Join(p.Snapshots, name)
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	nb := strings.Replace(string(b), fmt.Sprintf(`"version": %d`, snapshot.Version), `"version": 99`, 1)
	if nb == string(b) {
		nb = strings.Replace(string(b), fmt.Sprintf(`"version":%d`, snapshot.Version), `"version":99`, 1)
	}
	if nb == string(b) {
		t.Fatalf("no version field in %s", b)
	}
	if err := os.WriteFile(f, []byte(nb), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStore_NewerFormatIsNeverReadPast (G1-H4): a moox-last (or, with the
// pointer gone, the newest candidate) written by a newer moox is reported
// as ErrNewerFormat — never skipped for an older snapshot, never "absent".
func TestStore_NewerFormatIsNeverReadPast(t *testing.T) {
	s, p := fixture(t)
	l := lock(t, s)
	publish(t, s, l, snap(1), base, nil)
	b := publish(t, s, l, snap(2), base.Add(time.Minute), nil)
	bumpVersion(t, p, b)
	if last, err := s.Last(); !errors.Is(err, snapshot.ErrNewerFormat) {
		t.Fatalf("pointer at a newer-format file: %+v %v, want ErrNewerFormat", last, err)
	}
	if err := os.Remove(filepath.Join(p.Snapshots, PointerName)); err != nil {
		t.Fatal(err)
	}
	if last, err := s.Last(); !errors.Is(err, snapshot.ErrNewerFormat) {
		t.Fatalf("newest candidate newer-format: %+v %v, want ErrNewerFormat", last, err)
	}
}

// TestStore_HealPrefersNewerSaveOverOlderImport (G1-H8, PR-H23, §6.2): with
// moox-last broken, an import published LATER (newer file name) but made
// from an OLDER resurrect file (created_at) loses to a save created after
// it; a save older than the import does not win.
func TestStore_HealPrefersNewerSaveOverOlderImport(t *testing.T) {
	s, p := fixture(t)
	l := lock(t, s)
	save := snap(2)
	save.CreatedAt = base.Add(2 * time.Hour).Format(time.RFC3339) // 12:00+2h
	sv := publish(t, s, l, save, base.Add(2*time.Hour), nil)
	imp := snap(1)
	imp.Source = snapshot.SourceImport
	imp.CreatedAt = base.Format(time.RFC3339) // the resurrect file's mtime
	publish(t, s, l, imp, base.Add(4*time.Hour), nil)
	if err := os.WriteFile(filepath.Join(p.Snapshots, PointerName), []byte("moox-20000101T000000.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	last, err := s.Last()
	if err != nil || last.Name != sv || !last.Healed {
		t.Fatalf("heal picked %q (%v), want the newer save %q", last.Name, err, sv)
	}

	// an import newer than every save stays the pick
	s2, p2 := fixture(t)
	l2 := lock(t, s2)
	old := snap(2)
	old.CreatedAt = base.Format(time.RFC3339)
	publish(t, s2, l2, old, base, nil)
	imp2 := snap(1)
	imp2.Source = snapshot.SourceImport
	imp2.CreatedAt = base.Add(time.Hour).Format(time.RFC3339)
	in := publish(t, s2, l2, imp2, base.Add(2*time.Hour), nil)
	if err := os.Remove(filepath.Join(p2.Snapshots, PointerName)); err != nil {
		t.Fatal(err)
	}
	if last, err := s2.Last(); err != nil || last.Name != in {
		t.Fatalf("heal picked %q (%v), want the newer import %q", last.Name, err, in)
	}
}

// TestStore_ContentsOver8MiBReusedAndLoaded (G1-H1): a contents file larger
// than statedir's 8 MiB default (full history is the default capture) is
// still found by a structural save's reuse and still loads; it used to read
// as "not found", so every later structural save dropped all scrollback.
func TestStore_ContentsOver8MiBReusedAndLoaded(t *testing.T) {
	s, p := fixture(t)
	l := lock(t, s)
	big := make([]byte, 9<<20)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range big { // incompressible, deterministic
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		big[i] = byte(x)
	}
	name := publish(t, s, l, snap(1), base, map[string][]byte{"%1": big})
	sn, err := s.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(p.Snapshots, sn.Contents.File)); err != nil || fi.Size() <= 8<<20 {
		t.Fatalf("fixture contents file is not over 8 MiB: %v %v", fi, err)
	}
	cn, data, ok, err := s.NewestContents()
	if err != nil || !ok || cn != sn.Contents.File || snapshot.SHA256(data) != sn.Contents.SHA256 {
		t.Fatalf("NewestContents: %q found=%v err=%v", cn, ok, err)
	}
	got, err := s.LoadContents(sn)
	if err != nil || len(got["%1"]) != len(big) {
		t.Fatalf("LoadContents: %d bytes, %v", len(got["%1"]), err)
	}
}

func remaining(t *testing.T, s *Store) []string {
	t.Helper()
	names, err := s.snaps.ReadDirNames()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

// TestRetention_KeepsPolicyAndTarget (S9).
func TestRetention_KeepsPolicyAndTarget(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	now := base.Add(30 * 24 * time.Hour)
	var all []string
	// one snapshot per 6 h over 30 days, each with its own contents
	for i := 0; i < 120; i++ {
		at := base.Add(time.Duration(i) * 6 * time.Hour)
		all = append(all, publish(t, s, l, snap(1), at, map[string][]byte{"%1": []byte(fmt.Sprint(i))}))
	}
	target := all[5] // moox-last points at an old snapshot
	if err := s.Heal(l, target); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retain(l, DefaultPolicy, now); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{target: true}
	for _, n := range all[len(all)-20:] {
		keep[n] = true
	}
	days := map[string]bool{}
	for i := len(all) - 1; i >= 0; i-- {
		pn, _ := ParseName(all[i])
		if pn.Stamp.Before(now.Add(-14 * 24 * time.Hour)) {
			continue
		}
		if d := pn.Stamp.Format("20060102"); !days[d] {
			days[d] = true
			keep[all[i]] = true
		}
	}
	for _, n := range all {
		cn := strings.TrimSuffix(n, ".json") + ".contents.gz"
		if exists(s, n) != keep[n] || exists(s, cn) != keep[n] {
			t.Errorf("%s: exists=%v contents=%v, want %v", n, exists(s, n), exists(s, cn), keep[n])
		}
	}
	if len(keep) < 21 {
		t.Fatalf("policy kept only %d", len(keep))
	}
	if ParsePolicy("5 2d") != (Policy{5, 2}) || ParsePolicy("x") != DefaultPolicy || ParsePolicy("7") != (Policy{7, 14}) {
		t.Fatal("ParsePolicy")
	}
}

// TestRetention_RanksOnlyMooxNames (S9): 3,413 tmux_resurrect_*.txt,
// resurrect's last and pane_contents.tar.gz are never candidates.
func TestRetention_RanksOnlyMooxNames(t *testing.T) {
	s, p := fixture(t)
	l := lock(t, s)
	foreign := []string{"last", "pane_contents.tar.gz", "notes.txt", "moox-last.bak", "moox-20200101T000000.txt"}
	for i := 0; i < 40; i++ {
		foreign = append(foreign, fmt.Sprintf("tmux_resurrect_202001%02dT000000.txt", i%28+1)+fmt.Sprint(i))
	}
	for _, f := range foreign {
		if err := os.WriteFile(filepath.Join(p.Snapshots, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		publish(t, s, l, snap(1), base.Add(time.Duration(i)*time.Hour), nil)
	}
	removed, err := s.Retain(l, Policy{Count: 2, Days: 0}, base.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 23 {
		t.Fatalf("removed %d: %v", len(removed), removed)
	}
	for _, r := range removed {
		if _, ok := ParseName(r); !ok {
			t.Fatalf("removed a non-moox name %q", r)
		}
	}
	for _, f := range foreign {
		if _, err := os.Lstat(filepath.Join(p.Snapshots, f)); err != nil {
			t.Fatalf("%s gone: %v", f, err)
		}
	}
}

// TestRetention_KeepsPinsAfterIncomplete (S9, A16): an INCOMPLETE restore pins
// its source; 40 shrinking saves under a tight policy never delete it.
func TestRetention_KeepsPinsAfterIncomplete(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	src := publish(t, s, l, snap(40), base, nil)
	if err := s.BeginAttempt(l, src); err != nil {
		t.Fatal(err)
	}
	// while the attempt runs, restoring-from protects the source too
	publish(t, s, l, snap(3), base.Add(time.Second), nil)
	if _, err := s.Retain(l, Policy{Count: 1}, base.Add(time.Minute)); err != nil || !exists(s, src) {
		t.Fatalf("restoring-from not retained: %v", err)
	}
	if pins, err := s.Settle(l, Attempt{Source: src, Boot: "INCOMPLETE", Misses: 4, Launch: "PASS"}); err != nil || !reflect.DeepEqual(pins, []string{src}) {
		t.Fatalf("%v %v", pins, err)
	}
	for i := 0; i < 40; i++ {
		at := base.Add(time.Duration(i+2) * time.Minute)
		publish(t, s, l, snap(40-i), at, nil)
		if _, err := s.Retain(l, Policy{Count: 3, Days: 0}, at); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(s, src) {
		t.Fatal("the pinned snapshot was deleted")
	}
	if _, ok := s.RestoringFrom(); ok {
		t.Fatal("restoring-from left behind")
	}
	if WarningText([]string{src}) != "restore INCOMPLETE: 1 pinned — moox snapshot show" || WarningText(nil) != "" {
		t.Fatal("warning text")
	}
}

// TestPins_TwoFailuresKeepBoth.
func TestPins_TwoFailuresKeepBoth(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	a := publish(t, s, l, snap(10), base, nil)
	b := publish(t, s, l, snap(12), base.Add(time.Minute), nil)
	for _, src := range []string{a, b} {
		if err := s.BeginAttempt(l, src); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Settle(l, Attempt{Source: src, Boot: "PASS", Launch: "FAIL"}); err != nil {
			t.Fatal(err)
		}
	}
	if pins, _ := s.Pins(); !reflect.DeepEqual(pins, []string{a, b}) {
		t.Fatalf("%v", pins)
	}
}

// TestPins_SmallerPassKeepsLargerPin (claude-internal's case): an INCOMPLETE
// reboot of 40 panes, then a complete restore of a smaller snapshot, keeps
// the original pin; a PASS of an equal-or-larger snapshot releases it.
func TestPins_SmallerPassKeepsLargerPin(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	big := publish(t, s, l, snap(40), base, nil)
	small := publish(t, s, l, snap(12), base.Add(time.Minute), nil)
	if _, err := s.Settle(l, Attempt{Source: big, Boot: "INCOMPLETE", Misses: 3, Launch: "PASS"}); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.Settle(l, Attempt{Source: small, Boot: "PASS", Launch: "PASS"}); !reflect.DeepEqual(pins, []string{big}) {
		t.Fatalf("a smaller PASS released the larger pin: %v", pins)
	}
	same := publish(t, s, l, snap(40), base.Add(2*time.Minute), nil)
	if pins, _ := s.Settle(l, Attempt{Source: same, Boot: "PASS", Launch: "PASS"}); len(pins) != 0 {
		t.Fatalf("an equal PASS did not release: %v", pins)
	}
	// an unreadable pin is kept until unpinned by hand
	if err := s.AddPin(l, "moox-20000101T000000.json"); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.Settle(l, Attempt{Source: same, Boot: "PASS", Launch: "PASS"}); len(pins) != 1 {
		t.Fatalf("%v", pins)
	}
	if err := s.Unpin(l, ""); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.Pins(); len(pins) != 0 {
		t.Fatal("unpin --all")
	}
}

// TestPins_NothingWithoutMissesReleases: BOOT NOTHING with zero misses plus a
// LAUNCH PASS (zero judged rows) is a passing attempt; NOTHING with a miss is
// not.
func TestPins_NothingWithoutMissesReleases(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	pinned := publish(t, s, l, snap(5), base, nil)
	src := publish(t, s, l, snap(5), base.Add(time.Minute), nil)
	if err := s.AddPin(l, pinned); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.Settle(l, Attempt{Source: src, Boot: "NOTHING", Misses: 1, Launch: "PASS"}); len(pins) != 2 {
		t.Fatalf("NOTHING with a miss passed: %v", pins)
	}
	if err := s.Unpin(l, src); err != nil {
		t.Fatal(err)
	}
	if pins, _ := s.Settle(l, Attempt{Source: src, Boot: "NOTHING", Misses: 0, Launch: "PASS"}); len(pins) != 0 {
		t.Fatalf("NOTHING without misses did not release: %v", pins)
	}
	for _, a := range []Attempt{{Boot: "PASS", Launch: "PASS"}, {Boot: "NOTHING", Launch: "PASS"}} {
		if !a.Passed() {
			t.Errorf("%+v should pass", a)
		}
	}
	for _, a := range []Attempt{{Boot: "INCOMPLETE", Launch: "PASS"}, {Boot: "PASS", Launch: "FAIL"}, {Boot: "PASS"}} {
		if a.Passed() {
			t.Errorf("%+v should fail", a)
		}
	}
}

// TestPins_CrashMidRestoreBecomesPin (RR4): a restore that died before its
// verdict leaves restoring-from; recovering it makes it a pin.
func TestPins_CrashMidRestoreBecomesPin(t *testing.T) {
	s, _ := fixture(t)
	l := lock(t, s)
	src := publish(t, s, l, snap(7), base, nil)
	if err := s.BeginAttempt(l, src); err != nil {
		t.Fatal(err)
	}
	// ... the daemon dies here; a later process clears the dead @moox-restoring:
	got, ok, err := s.RecoverRestoringFrom(l)
	if err != nil || !ok || got != src {
		t.Fatalf("%q %v %v", got, ok, err)
	}
	if pins, _ := s.Pins(); !reflect.DeepEqual(pins, []string{src}) {
		t.Fatalf("%v", pins)
	}
	if _, ok := s.RestoringFrom(); ok {
		t.Fatal("restoring-from remains")
	}
	if _, ok, _ := s.RecoverRestoringFrom(l); ok {
		t.Fatal("recovered twice")
	}
}

func writeFreeze(t *testing.T, p sockets.Paths, name, content string) {
	t.Helper()
	if err := os.MkdirAll(p.FreezeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.FreezeDir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClobber_BlocksSidWipe (B20, R24): ≥3 sids → 0 with an empty, missing or
// partial freeze store is blocked, and the line is logged.
func TestClobber_BlocksSidWipe(t *testing.T) {
	s, p := fixture(t)
	prev := snap(4, "s1", "s2", "s3")
	wiped := snap(4)
	if c := CheckClobber(prev, wiped, FreezeHeld(p.FreezeDir, filepath.Join(p.State, "claude"))); !c.Blocked || c.Prev != 3 {
		t.Fatalf("missing store: %+v", c)
	}
	writeFreeze(t, p, "k1.state", ";CLAUDE_SID=s1\n;CLAUDE_SID=other\n")
	writeFreeze(t, p, "k1.banner", ";CLAUDE_SID=s2 ;CLAUDE_SID=s3\n") // not a .state file
	c := CheckClobber(prev, wiped, FreezeHeld(p.FreezeDir, filepath.Join(p.State, "claude")))
	if !c.Blocked || c.Accounted != 1 {
		t.Fatalf("partial store: %+v", c)
	}
	if err := s.LogBlocked(c, base); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(p.State, "log", ClobberLogName))
	if err != nil || !strings.Contains(string(b), "BLOCKED near-total-wipe save: last had 3 CLAUDE_SID, new had 0") {
		t.Fatalf("%q %v", b, err)
	}
	// below the threshold, or a save that keeps one sid, is never blocked
	if CheckClobber(snap(2, "a", "b"), wiped, nil).Blocked || CheckClobber(prev, snap(1, "s1"), nil).Blocked ||
		CheckClobber(nil, wiped, nil).Blocked {
		t.Fatal("blocked a legitimate save")
	}
}

// TestClobber_AllowsWhenFrozenAccountsByID (B20, R25): freezing every Claude
// window accounts for every sid BY ID, so the save is published.
func TestClobber_AllowsWhenFrozenAccountsByID(t *testing.T) {
	_, p := fixture(t)
	prev := snap(5, "s1", "s2", "s3", "s4")
	writeFreeze(t, p, "a.state", "WINDOW=x\n;CLAUDE_SID=s1\n;CLAUDE_SID=s2\n")
	writeFreeze(t, p, "b.state", ";CLAUDE_SID=s3 ;CLAUDE_SID=s4\n")
	if c := CheckClobber(prev, snap(5), FreezeHeld(p.FreezeDir, filepath.Join(p.State, "claude"))); c.Blocked || c.Accounted != 4 {
		t.Fatalf("%+v", c)
	}
	// counts are by id: 4 OTHER ids in the store account for nothing
	_, q := fixture(t)
	writeFreeze(t, q, "c.state", ";CLAUDE_SID=x1 ;CLAUDE_SID=x2 ;CLAUDE_SID=x3 ;CLAUDE_SID=x4\n")
	if c := CheckClobber(prev, snap(5), FreezeHeld(q.FreezeDir, filepath.Join(q.State, "claude"))); !c.Blocked {
		t.Fatalf("other ids accounted: %+v", c)
	}
}

func TestStoreLock_BusyNamesHolderAndWaitIsBounded(t *testing.T) {
	s, _ := fixture(t)
	held, err := s.TryLock(Record{Pid: 4242, ServerPid: 812, Epoch: 1, StartToken: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.TryLock(Record{Pid: 1})
	var be *BusyError
	if !errors.As(err, &be) || be.Pid != 4242 || !errors.Is(err, ErrBusy) || err.Error() != "busy 4242" {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.LockWait(ctx, Record{Pid: 1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("LockWait: %v", err)
	}
	if el := time.Since(start); el < 500*time.Millisecond || el > 2*time.Second {
		t.Fatalf("bounded wait took %v", el)
	}
	go func() {
		t := time.NewTimer(300 * time.Millisecond)
		<-t.C
		held.Release()
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	l, err := s.LockWait(ctx2, Record{Pid: 2})
	if err != nil {
		t.Fatalf("LockWait after release: %v", err)
	}
	l.Release()
	if err := s.Publish(nil, snap(1), "moox-20261024T120000.json", "", nil); err == nil {
		t.Fatal("publish without the lock")
	}
}
