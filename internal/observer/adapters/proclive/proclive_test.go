package proclive

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func withKill(t *testing.T, fn func(pid int, sig syscall.Signal) error) *[]int {
	t.Helper()
	var seen []int
	prev := kill
	kill = func(pid int, sig syscall.Signal) error {
		seen = append(seen, pid)
		if sig != 0 {
			t.Errorf("a signal other than 0 was sent: %v — this package must never affect a process", sig)
		}
		return fn(pid, sig)
	}
	t.Cleanup(func() { kill = prev })
	return &seen
}

func TestTheThreeOutcomesAreDistinguishable(t *testing.T) {
	// The three-way distinction is the whole point: an errored gate is NOT
	// evidence of death, and folding it into a false would report a live
	// session as dead on a transient failure.
	cases := []struct {
		name      string
		err       error
		wantAlive bool
		wantErr   bool
	}{
		{"running and ours", nil, true, false},
		{"no such process", syscall.ESRCH, false, false},
		{"exists, not ours", syscall.EPERM, true, false},
		{"gate could not answer", syscall.EINVAL, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withKill(t, func(int, syscall.Signal) error { return tc.err })
			alive, err := New().Alive(4242)
			if alive != tc.wantAlive {
				t.Errorf("alive = %v, want %v", alive, tc.wantAlive)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestWrappedErrnosAreRecognised(t *testing.T) {
	// A syscall error can arrive wrapped (os.SyscallError, *fs.PathError).
	// errors.Is is used rather than == for exactly this, and an ESRCH that
	// failed to match would be read as "the gate errored" — which keeps the
	// previous answer instead of recording a real death, so a dead session
	// would never leave the live set.
	withKill(t, func(int, syscall.Signal) error {
		return os.NewSyscallError("kill", syscall.ESRCH)
	})
	alive, err := New().Alive(4242)
	if err != nil {
		t.Fatalf("a wrapped ESRCH was not recognised: %v", err)
	}
	if alive {
		t.Error("a wrapped ESRCH reported the process alive")
	}
}

func TestNonPositivePidsAreRefusedWithoutAskingTheKernel(t *testing.T) {
	// kill(2) reads pid 0 as "every process in the caller's process group" and
	// a negative pid as a process group. With signal 0 nothing is sent, but
	// the ANSWER would be about the wrong subject — and a "yes" here would
	// vouch for a session that has no process at all.
	for _, pid := range []int{0, -1, -4242} {
		seen := withKill(t, func(int, syscall.Signal) error { return nil })
		alive, err := New().Alive(pid)
		if err == nil {
			t.Errorf("pid %d was not refused", pid)
		}
		if alive {
			t.Errorf("pid %d was reported alive", pid)
		}
		if len(*seen) != 0 {
			t.Errorf("pid %d reached the kernel: %v", pid, *seen)
		}
	}
}

func TestTheRealGateAgreesWithItselfOnThisProcess(t *testing.T) {
	// The one test that uses the real syscall, and it is safe by construction:
	// our own pid, signal 0. It is worth having because the fake above can
	// only prove the mapping, not that syscall.Kill is being called at all.
	alive, err := New().Alive(os.Getpid())
	if err != nil {
		t.Fatalf("probing our own pid: %v", err)
	}
	if !alive {
		t.Error("the gate reports this very process as dead")
	}
}

func TestTheErrorCarriesThePid(t *testing.T) {
	withKill(t, func(int, syscall.Signal) error { return syscall.EINVAL })
	_, err := New().Alive(4242)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, syscall.EINVAL) {
		t.Errorf("the underlying errno was not wrapped: %v", err)
	}
	if got := err.Error(); !contains(got, "4242") {
		t.Errorf("the error does not name the pid: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
