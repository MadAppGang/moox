package sockserve

// The strongest half of --stop's corroboration: what the server itself says.
//
// These are internal tests because they drive `corroborate` directly, and that
// is deliberate — the alternative is to let `Stop` run, which ends in a
// syscall.Kill. A test that reached the signal would either have to kill the
// test binary or nominate some other live pid to shoot at, which is precisely
// the harm this change exists to prevent.
//
// Nothing is spawned and nothing is signalled here: a plain unix listener
// writes one hello frame and closes.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// helloServer answers one connection with a hello frame declaring pid, then
// closes. It is the smallest thing that is indistinguishable from a real
// server to the one question corroboration asks it.
func helloServer(t *testing.T, pid int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttstop")
	if err != nil {
		t.Fatalf("making a temp state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, SocketName)
	if n := len(path); n >= 104 {
		t.Fatalf("the socket path is %d bytes, which sockaddr_un cannot hold", n)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	frame := wire.NewHello(wire.Options{TT: "test"}, pid, nil, nil, nil)
	line, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding hello: %v", err)
	}
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_, _ = conn.Write(append(line, '\n'))
			_ = conn.Close()
		}
	}()
	return path
}

// The pid this process can always corroborate is its own: it is live, and its
// start instant is whatever the kernel says. Using os.Getpid() here is safe
// precisely because corroborate() never signals anything.
func TestCorroborationBelievesAServerThatDeclaresThePidItself(t *testing.T) {
	socket := helloServer(t, os.Getpid())
	rec := wire.PidFile{
		PID: os.Getpid(),
		// Deliberately WRONG by two days, so that only the hello can be what
		// carried the verdict: the start-time gate would refuse this record.
		StartedAt: observer.FormatTime(time.Now().Add(-48 * time.Hour)),
		Socket:    socket,
	}
	ev := corroborate(rec, socket)
	if ev.verdict != stopOurs {
		t.Fatalf("corroborate returned %+v, want the hello to settle it", ev)
	}
	if ev.evidence != "hello" {
		t.Errorf("evidence = %q, want %q", ev.evidence, "hello")
	}
}

// A live server that names a DIFFERENT pid than the file does means one of the
// two is stale and there is no way to tell which. Refuse: the file is the half
// that survives crashes and reboots, and the cost of being wrong is somebody
// else's process.
func TestCorroborationRefusesWhenTheServerNamesADifferentPid(t *testing.T) {
	socket := helloServer(t, os.Getpid()+1)
	rec := wire.PidFile{
		PID:       os.Getpid(),
		StartedAt: observer.FormatTime(time.Now()),
		Socket:    socket,
	}
	ev := corroborate(rec, socket)
	if ev.verdict != stopUnknown {
		t.Fatalf("corroborate returned %+v, want a refusal", ev)
	}
	if ev.reason == "" {
		t.Error("a refusal with no reason is a refusal the user cannot act on")
	}
}

// A socket path holding something that is not a socket — or a socket that
// errors in a way neither ECONNREFUSED nor ENOENT explains — is bind()'s third
// branch: refuse rather than guess. Unlinking a socket we could not dial is how
// a healthy server gets silently displaced, and signalling a pid we could not
// dial for is the same mistake pointed at a process.
func TestCorroborationRefusesASocketPathItCannotDial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SocketName)
	if err := os.WriteFile(path, []byte("not a socket\n"), 0o600); err != nil {
		t.Fatalf("seeding the path: %v", err)
	}
	rec := wire.PidFile{
		PID:       os.Getpid(),
		StartedAt: observer.FormatTime(time.Now()),
		Socket:    path,
	}
	ev := corroborate(rec, path)
	if ev.verdict != stopUnknown {
		t.Fatalf("corroborate returned %+v, want a refusal", ev)
	}
}

// A pid the kernel cannot have allocated (darwin's ceiling is 99998) is not a
// process, so there is nothing to signal and both files outlived their server.
func TestCorroborationCallsAnImpossiblePidStale(t *testing.T) {
	ev := corroborate(wire.PidFile{PID: 1 << 30, StartedAt: observer.FormatTime(time.Now())}, "/nonexistent/observer.sock")
	if ev.verdict != stopStale {
		t.Fatalf("corroborate returned %+v, want a stale pair", ev)
	}
}

// The start-time gate's two boundaries, on this process — which is live, whose
// start instant the kernel knows, and which nothing here signals.
//
// The backward tolerance is not decoration. A record whose startedAt was
// truncated to whole seconds, or written either side of an NTP step, lands
// fractionally BEFORE the kernel's start instant, and a zero-tolerance gate
// refuses that healthy server — which is how a safety check becomes the thing
// the user routes around.
func TestTheStartTimeGateToleratesTruncationButNotAReusedPid(t *testing.T) {
	started, err := processStartedAt(os.Getpid())
	if err != nil {
		t.Skipf("this platform has no process start time: %v", err)
	}
	// A path that cannot be dialled as a socket sends corroborate down the
	// start-time branch without anything listening.
	socket := filepath.Join(t.TempDir(), SocketName)

	for _, tc := range []struct {
		name  string
		at    time.Time
		grant bool
	}{
		{"written a moment after the process started", started.Add(time.Second), true},
		{"truncated to just before it", started.Add(-500 * time.Millisecond), true},
		{"written well before it: a recycled pid", started.Add(-48 * time.Hour), false},
		{"written long after it: a recycled pid the other way", started.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := wire.PidFile{PID: os.Getpid(), StartedAt: observer.FormatTime(tc.at), Socket: socket}
			ev := startTimeEvidence(rec, "nothing is listening")
			if got := ev.verdict == stopOurs; got != tc.grant {
				t.Fatalf("verdict %+v, want granted=%t", ev, tc.grant)
			}
			if ev.verdict == stopOurs && ev.evidence != "start-time" {
				t.Errorf("evidence = %q, want %q", ev.evidence, "start-time")
			}
		})
	}
}
