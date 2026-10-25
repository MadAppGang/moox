package proclive

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

func withParentOf(t *testing.T, fn func(pid int) (int, error)) *[]int {
	t.Helper()
	var seen []int
	prev := parentOf
	parentOf = func(pid int) (int, error) {
		seen = append(seen, pid)
		return fn(pid)
	}
	t.Cleanup(func() { parentOf = prev })
	return &seen
}

// The real lookup, against the one pid whose parent the test can know without
// guessing: its own. This is the platform code itself — sysctl on darwin,
// /proc on linux — so a wrong struct field or an off-by-one field index fails
// here rather than as sessions silently never being recognised as children.
func TestTheRealLookupReturnsThisProcesssParent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no parent lookup on %s", runtime.GOOS)
	}
	got, err := New().Parent(os.Getpid())
	if err != nil {
		t.Fatalf("Parent(self): %v", err)
	}
	if want := os.Getppid(); got != want {
		t.Fatalf("Parent(%d) = %d, want %d (os.Getppid)", os.Getpid(), got, want)
	}
}

func TestANonPositivePidIsRefusedWithoutAsking(t *testing.T) {
	seen := withParentOf(t, func(int) (int, error) { return 1, nil })
	for _, pid := range []int{0, -1, -4242} {
		if ppid, err := New().Parent(pid); err == nil {
			t.Errorf("Parent(%d) = %d with no error; a non-pid has no parent to report", pid, ppid)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("the platform lookup was asked about %v", *seen)
	}
}

// An error from the platform must stay an error. A 0 or a 1 in its place would
// read as an answer ("its parent is launchd"), and the core's rule is that only
// an answer can hide a session.
func TestALookupFailureIsAnErrorNotAPid(t *testing.T) {
	cause := errors.New("sysctl: short read")
	withParentOf(t, func(int) (int, error) { return 0, cause })
	ppid, err := New().Parent(4242)
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap %v", err, cause)
	}
	if ppid != 0 {
		t.Errorf("ppid = %d alongside an error", ppid)
	}
}
