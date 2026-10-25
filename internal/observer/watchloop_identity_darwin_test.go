//go:build darwin

package observer_test

// Code review round 2, A4 and G3, against a REAL kqueue over a scratch tree.
//
// A4: the arm waves made NO call when every wanted path was already armed with
// the same kind, so they could not notice that a path's VNODE had been
// replaced underneath its descriptor. The "release-only" call after the publish
// went through the same adapter Reconcile, which DID notice — and re-armed the
// replacement there, after the read. A registry record replaced by rename and
// then rewritten in place between the read and that late arm was seen by
// nobody: the old descriptor is on a dead inode, and a directory vnode gets
// nothing on an in-place write. The published status stayed wrong until some
// unrelated event.
//
// Nothing here touches ~/.claude/: the tree is t.TempDir().

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

// hookCore is a Snapshotter whose READ runs a hook, which is how a test puts a
// mutation exactly between the read and the post-publication phase.
//
// Every read returns one session whose name changes, so every publication has
// a delta a subscriber is actually sent.
type hookCore struct {
	mu    sync.Mutex
	hook  func()
	now   time.Time
	reads int
}

func (c *hookCore) Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.mu.Lock()
	hook := c.hook
	c.hook = nil
	c.reads++
	n := c.reads
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	live := true
	return &observer.Snapshot{ObservedAt: c.now, Sessions: []observer.Session{{
		SessionID: "s1", Kind: observer.KindInteractive, Status: observer.StatusIdle,
		Name: "read-" + string(rune('a'+n%26)), Live: &live, ObservedAt: c.now,
	}}}, nil
}

func (c *hookCore) setHook(f func()) {
	c.mu.Lock()
	c.hook = f
	c.mu.Unlock()
}

// drainHints empties the hint channel until it has been quiet for quiet.
func drainHints(hints <-chan observer.Change, quiet time.Duration) {
	for {
		select {
		case <-hints:
		case <-time.After(quiet):
			return
		}
	}
}

func TestAReplacedRecordWrittenInPlaceBetweenTheReadAndTheReleaseIsStillNoticed(t *testing.T) {
	root := t.TempDir()
	reg := filepath.Join(root, "sessions")
	if err := os.MkdirAll(reg, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(reg, "4242.json")
	if err := os.WriteFile(record, []byte(`{"status":"idle"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := watchkq.New(watchkq.Caps{})
	if err != nil {
		t.Skipf("no kqueue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hints, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	core := &hookCore{now: loopT0}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:  core,
		Watch: w,
		Tree:  watchkq.OSTree{},
		Clock: fakes.NewClock(loopT0),
		Paths: observer.Paths{ClaudeRoot: root, Registry: reg},
	})
	if err != nil {
		t.Fatalf("NewWatchLoop: %v", err)
	}
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatalf("startup Rebuild: %v", err)
	}
	drainHints(hints, 100*time.Millisecond)

	// 1. The vendor REPLACES the record (write-temp, rename over). The old
	//    descriptor is now on an unlinked inode; the directory fires.
	staged := filepath.Join(reg, ".4242.json.tmp")
	if err := os.WriteFile(staged, []byte(`{"status":"busy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, record); err != nil {
		t.Fatal(err)
	}
	drainHints(hints, 150*time.Millisecond) // the hint that WOULD trigger this rebuild

	// 2. The rebuild that hint causes. During its READ, the vendor rewrites the
	//    new inode IN PLACE — the registry's normal write mechanism.
	core.setHook(func() {
		f, err := os.OpenFile(record, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Errorf("in-place write: %v", err)
			return
		}
		_, _ = f.WriteString(`{"status":"waiting"}`)
		_ = f.Close()
	})
	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// 3. That write must produce a hint, so the NEXT rebuild reads "waiting".
	//    It can only if the replacement vnode was armed BEFORE the read.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-hints:
			if c.Ref == record {
				return
			}
		case <-deadline:
			t.Fatal("an in-place write to a replaced record between the read and the " +
				"release produced no hint at all: the replacement was armed only after the " +
				"read, and that write is lost until some unrelated event")
		}
	}
}

// G3 + A4's other half: what runs after the publish is RELEASE-ONLY, and the
// arm report published with the snapshot describes the armed set as of that
// publication — ByKind included, not left empty because no wave happened to
// arm anything new.
func TestThePostPublicationPhaseArmsNothingAndThePublishedReportCarriesTheArmedSet(t *testing.T) {
	root := t.TempDir()
	reg := filepath.Join(root, "sessions")
	jobs := filepath.Join(root, "jobs")
	for _, d := range []string{reg, filepath.Join(jobs, "aaaa")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(reg, "1.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	w := fakes.NewWatcher(8)
	core := &hookCore{now: loopT0}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:  core,
		Watch: w,
		Tree:  watchkq.OSTree{},
		Clock: fakes.NewClock(loopT0),
		Paths: observer.Paths{ClaudeRoot: root, Registry: reg, Jobs: jobs},
	})
	if err != nil {
		t.Fatalf("NewWatchLoop: %v", err)
	}
	ctx := context.Background()
	if _, _, err := loop.Rebuild(ctx, "startup"); err != nil {
		t.Fatalf("startup: %v", err)
	}

	// The steady state: nothing new to arm. Two things are captured.
	//
	//   - any ARMING call (Reconcile) the loop makes after its read. The old
	//     loop made one — its "release-only" call — and this hook fires on it.
	//   - the arm report as it stands the moment the publication is visible to
	//     a subscriber. On the old loop the post-publication Reconcile is held
	//     in the hook until the subscriber has read it, so the read cannot
	//     drift past the later bookkeeping by luck; on a release-only loop
	//     nothing is held, and the report must be complete either way.
	var (
		mu           sync.Mutex
		readDone     bool
		armAfterRead int
	)
	reportRead := make(chan struct{})
	w.ArmHook = func([]observer.WatchTarget) {
		mu.Lock()
		after := readDone
		if after {
			armAfterRead++
		}
		mu.Unlock()
		if after {
			select {
			case <-reportRead:
			case <-time.After(time.Second):
			}
		}
	}
	core.setHook(func() {
		mu.Lock()
		readDone = true
		mu.Unlock()
	})
	events := make(chan observer.ArmReport, 1)
	sub, err := loop.Subscribe(ctx, observer.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	<-sub // the baseline of the startup publication
	go func() {
		<-sub
		events <- loop.LastArmReport()
		close(reportRead)
	}()

	if _, _, err := loop.Rebuild(ctx, "watch"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	mu.Lock()
	after := armAfterRead
	mu.Unlock()
	if after != 0 {
		t.Errorf("%d arming (Reconcile) calls ran after the read; the post-publication phase "+
			"must be release-only", after)
	}

	var rep observer.ArmReport
	select {
	case rep = <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("the subscriber never saw the publication")
	}
	armed := loop.Armed()
	want := map[observer.TargetKind]int{}
	for _, k := range armed {
		want[k]++
	}
	if len(want) == 0 {
		t.Fatal("nothing armed; the fixture is broken")
	}
	for k, n := range want {
		if rep.ByKind[k] != n {
			t.Errorf("published ByKind[%v] = %d, want %d (armed set %v, report %v)", k, rep.ByKind[k], n, want, rep.ByKind)
		}
	}
}
