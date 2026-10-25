package main

// AC10's first-record case through the PRODUCTION wiring of `tt observe`.
//
// WHY THIS FILE EXISTS. The independent black-box suite ran the real binary
// against a tree holding one record-less job directory, connected a client,
// and never received a baseline snapshot — and so never saw the first
// state.json that arrived afterwards either. Every in-repo AC10 test passed at
// the time, because every one of them built the watch loop, the watcher and
// the server by hand and none connected a client to what serveObserve
// composes. The defect was not in the arming (the directory WAS armed and the
// rename WAS delivered); it was that a subscriber attached before the first
// publication of an EMPTY session set was sent nothing at all.
//
// So this test goes through serveObserve itself: the real core, the real
// kqueue watcher, the real OSTree, the real jobsfs/transcriptfs adapters and
// the real socket server, pointed at a hermetic tree by hermeticTree. The only
// substitution is the `claude` on PATH, and it is there to make the first
// rebuild SLOW (the rebuild waits observer.CorroborationWait on it), which is
// what makes "the client subscribed before the first publication" the
// deterministic case rather than the lucky one.
//
// No tmux is involved: --status-line=never issues no tmux call at all, and no
// registry record carries a pane reference, so the pane gate is never asked.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
)

// wiringFrame reads one NDJSON frame into an untyped map. Untyped on purpose:
// §5.1 bans a json-tagged struct outside `wire`, tests included.
func wiringFrame(t *testing.T, nc net.Conn, rd *bufio.Reader, within time.Duration) map[string]any {
	t.Helper()
	if err := nc.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	line, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading a frame from the real observer: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("not a JSON frame: %v\n%s", err, line)
	}
	return f
}

func TestTheRealObserveWiringSendsABaselineAndDiscoversAFirstJobRecord(t *testing.T) {
	// ---- a Claude tree whose only job directory has no record yet ----
	root := t.TempDir()
	firstDir := filepath.Join(root, "jobs", "first")
	for _, d := range []string{filepath.Join(root, "sessions"), firstDir, filepath.Join(root, "projects", "own")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hermeticTree(t, root)

	// A `claude` that outlives the spawn bound, so the startup rebuild spends
	// CorroborationWait on it and the client below attaches before anything
	// is published. /bin/sleep by absolute path: the child inherits this PATH.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// The socket directory is short on purpose: sun_path is 104 bytes on
	// darwin, and t.TempDir() embeds this test's long name.
	sockDir, err := os.MkdirTemp("", "tt-wiring-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	// ---- the production composition, on a context this test owns ----
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	exitCode := -1
	go func() {
		defer close(exited)
		exitCode = serveObserve(ctx, observeFlags{
			Dir:        sockDir,
			Timeout:    500 * time.Millisecond,
			StatusLine: statusLineNever,
		}, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("serveObserve did not return after its context was cancelled")
		}
	})

	sock := filepath.Join(sockDir, sockserve.SocketName)
	var nc net.Conn
	for deadline := time.Now().Add(5 * time.Second); ; {
		if nc, err = net.Dial("unix", sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the observer socket never became reachable: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { _ = nc.Close() })
	rd := bufio.NewReaderSize(nc, 1<<20)

	// ---- 1. hello, BEFORE the first publication ----
	hello := wiringFrame(t, nc, rd, 5*time.Second)
	if hello["type"] != "hello" {
		t.Fatalf("the first frame is %v, want hello", hello["type"])
	}
	if hello["sources"] != nil {
		t.Fatal("hello already carries sources, so the first publication happened before this " +
			"client attached and the test did not exercise the case it exists for")
	}

	// ---- 2. the baseline — for an EMPTY session set. This is the frame that
	// never came. ----
	snap := wiringFrame(t, nc, rd, 5*time.Second)
	if snap["type"] != "snapshot" {
		t.Fatalf("the frame after hello is %v, want the baseline snapshot", snap["type"])
	}
	envelope, _ := snap["snapshot"].(map[string]any)
	if envelope == nil {
		t.Fatalf("the baseline carries no snapshot envelope: %v", snap)
	}
	if sessions, _ := envelope["sessions"].([]any); len(sessions) != 0 {
		t.Fatalf("the baseline carries %d sessions, want 0", len(sessions))
	}
	jobsWatch, _ := envelope["sources"].(map[string]any)["jobs"].(map[string]any)["watch"].(map[string]any)
	if jobsWatch == nil || jobsWatch["mode"] != "kqueue" {
		t.Fatalf("sources.jobs.watch = %v, want a kqueue watch", jobsWatch)
	}
	if n, _ := jobsWatch["armedDirs"].(float64); n < 2 {
		t.Fatalf("sources.jobs.watch.armedDirs = %v, want the jobs root AND the record-less "+
			"directory (C1a)", jobsWatch["armedDirs"])
	}

	// ---- 3. the first record arrives by atomic rename INTO that directory,
	// its transcript written beforehand, exactly as the black-box does it ----
	// Written in the wire's own spelling (millisecond precision, always three
	// digits) so the comparison below is exact whatever the clock reads.
	stamp := observer.FormatTime(time.Now())
	transcript := filepath.Join(root, "projects", "own", "first-thread.jsonl")
	if err := os.WriteFile(transcript, []byte(fmt.Sprintf(`{"type":"assistant","timestamp":%q}`+"\n", stamp)), 0o600); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"sessionId":"first-session","daemonShort":"first","state":"blocked","tempo":"blocked",`+
		`"updatedAt":%q,"createdAt":%q,"linkScanPath":%q}`, stamp, stamp, transcript)
	staged := filepath.Join(root, "staged-state.json")
	if err := os.WriteFile(staged, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := os.Rename(staged, filepath.Join(firstDir, "state.json")); err != nil {
		t.Fatal(err)
	}

	var added map[string]any
	for i := 0; i < 20 && added == nil; i++ {
		f := wiringFrame(t, nc, rd, 3*time.Second)
		if f["type"] == "session_added" {
			added = f
		}
	}
	if added == nil {
		t.Fatal("no session_added for the first record in a record-less job directory")
	}
	t.Logf("first-record latency through the production wiring: %v", time.Since(start))

	session, _ := added["session"].(map[string]any)
	if session["sessionId"] != "first-session" {
		t.Fatalf("session_added names %v, want first-session", session["sessionId"])
	}
	// Invariant B: the timing tail was read in the rebuild that DISCOVERED the
	// job, although nothing has appended to the transcript since.
	if session["lastActivityAt"] != stamp {
		t.Fatalf("lastActivityAt = %v, want %s from the transcript's only record",
			session["lastActivityAt"], stamp)
	}

	// ---- and the tree it watched is exactly what the test wrote ----
	cancel()
	select {
	case <-exited:
		if exitCode != 0 {
			t.Fatalf("serveObserve exited %d on a clean shutdown", exitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveObserve did not stop")
	}
	entries, err := os.ReadDir(firstDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("the job directory holds %v after the run (err %v)", names(entries), err)
	}
}
