//go:build tmuxe2e

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// TestE2E_Save_DirectSaveDuringRestoreNotPublished (R2-C3, A15, §5.4): a
// direct save is paused by a test hook right after its read (it read the
// pre-restore server: only the bootstrap); a restore starts, sets
// @moox-restoring and waits for save.lock, which the paused save holds; the
// save resumes and returns `pristine` — nothing published, moox-last
// unchanged — and only then does the restore get the lock and run.
func TestE2E_Save_DirectSaveDuringRestoreNotPublished(t *testing.T) {
	s := tmuxtest.Start(t)
	s.Run(t, "set-option", "-g", "@moox-active", "1")
	e := e2eEngine(t, s)
	lastName := publish(t, e, snapOf("a:0:1", "a:1:1"))
	ptr := filepath.Join(e.Paths().Snapshots, "moox-last")
	before, err := os.ReadFile(ptr)
	if err != nil {
		t.Fatal(err)
	}

	read, resume := make(chan struct{}), make(chan struct{})
	e.afterRead = func() { close(read); <-resume }
	type saveOut struct {
		res SaveResult
		err error
	}
	saved := make(chan saveOut, 1)
	go func() {
		res, err := e.Save(context.Background(), SaveRequest{Reason: "direct", Direct: true, Contents: true})
		saved <- saveOut{res, err}
	}()
	<-read

	// The restore is a second engine (a second process in production): its
	// own lock handle, so it waits on the kernel lock like another process.
	r := e2eEngine(t, s)
	r.lockWait = 30 * time.Second
	// What the daemon does when it decides a restore (§6.4a answer, §6.4
	// boot mark): @moox-restoring first, then Restore, which waits for the
	// lock.
	s.Run(t, "set-option", "-g", "@moox-restoring", KindRestore+":"+itoa(os.Getpid())+":"+itoa(int(time.Now().Unix())))
	restored := make(chan error, 1)
	var res RestoreResult
	go func() {
		var err error
		res, err = r.Restore(context.Background(), RestoreRequest{Kind: KindRestore, Stub: true,
			Path: filepath.Join(e.Paths().Snapshots, lastName)})
		restored <- err
	}()
	if !tmuxtest.Poll(10*time.Second, 20*time.Millisecond, func() bool { return s.Opt(t, "@moox-restoring") != "" }) {
		t.Fatal("the restore never set @moox-restoring")
	}
	select {
	case err := <-restored:
		t.Fatalf("the restore ran while the save held save.lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(resume)
	out := <-saved
	if out.err != nil || !out.res.Pristine || out.res.Published {
		t.Fatalf("the paused save: %+v %v, want pristine", out.res, out.err)
	}
	after, _ := os.ReadFile(ptr)
	if string(after) != string(before) {
		t.Fatalf("moox-last moved: %q → %q", before, after)
	}
	if err := <-restored; err != nil {
		t.Fatalf("the restore after the save: %v", err)
	}
	<-res.Launch
	if got, _ := os.ReadFile(ptr); string(got) != string(before) {
		t.Fatalf("moox-last moved during the restore: %q", got)
	}
	if v := s.Opt(t, "@moox-restoring"); v != "" {
		t.Fatalf("@moox-restoring left %q", v)
	}
}
