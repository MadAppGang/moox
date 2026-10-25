package main

// Code review round 2, A3, through the PRODUCTION wiring of `tt observe`.
//
// The watch loop counted a transcript's change hint and discarded WHICH
// transcript it was, and Snapshot never filled the changed-paths set Invariant
// B's fifth predicate reads — so a transcript append on a job whose state.json
// had not moved re-ran the rebuild and then served lastActivityAt from the
// timing cache. A working background job that keeps appending while its record
// stays byte-identical would drift into job-working-stale after 15 minutes,
// directly defeating decision D1 (the job's own conversation thread IS its
// timing source).
//
// Real core, real kqueue watcher, real jobsfs/transcriptfs, real socket; the
// tree is t.TempDir(). No tmux: --status-line=never and no pane references.

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

func TestATranscriptOnlyAppendReachesASocketSubscriber(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "jobs", "wrk1")
	project := filepath.Join(root, "projects", "own")
	for _, d := range []string{filepath.Join(root, "sessions"), jobDir, project} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hermeticTree(t, root)
	// No `claude` on PATH at all: corroboration is simply absent, which leaves
	// the job's liveness unknown (visible by the default rule) and costs the
	// rebuilds nothing.

	// A WORKING job whose record will not change again, and its own thread.
	start := time.Now().Add(-2 * time.Minute)
	first := observer.FormatTime(start)
	transcript := filepath.Join(project, "wrk1-thread.jsonl")
	if err := os.WriteFile(transcript, []byte(fmt.Sprintf(`{"type":"assistant","timestamp":%q}`+"\n", first)), 0o600); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"sessionId":"working-job","daemonShort":"wrk1","state":"working","tempo":"active",`+
		`"updatedAt":%q,"createdAt":%q,"linkScanPath":%q}`, first, first, transcript)
	if err := os.WriteFile(filepath.Join(jobDir, "state.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	sockDir, err := os.MkdirTemp("", "tt-a3-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		serveObserve(ctx, observeFlags{Dir: sockDir, Timeout: 500 * time.Millisecond, StatusLine: statusLineNever}, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("serveObserve did not return after its context was cancelled")
		}
	})

	var nc net.Conn
	for deadline := time.Now().Add(5 * time.Second); ; {
		if nc, err = net.Dial("unix", filepath.Join(sockDir, sockserve.SocketName)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the observer socket never became reachable: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { _ = nc.Close() })
	rd := bufio.NewReaderSize(nc, 1<<20)

	// hello, then the baseline, which must already carry the thread's time.
	if f := wiringFrame(t, nc, rd, 5*time.Second); f["type"] != "hello" {
		t.Fatalf("first frame %v, want hello", f["type"])
	}
	var baseline map[string]any
	for baseline == nil {
		if f := wiringFrame(t, nc, rd, 5*time.Second); f["type"] == "snapshot" {
			baseline = f
		}
	}
	found := false
	for _, raw := range baseline["snapshot"].(map[string]any)["sessions"].([]any) {
		s := raw.(map[string]any)
		if s["sessionId"] == "working-job" {
			found = true
			if s["lastActivityAt"] != first {
				t.Fatalf("baseline lastActivityAt = %v, want %s", s["lastActivityAt"], first)
			}
		}
	}
	if !found {
		t.Fatalf("the working job is not in the baseline: %v", baseline)
	}

	// The job keeps working: its thread grows, its record does not change.
	second := observer.FormatTime(start.Add(90 * time.Second))
	f, err := os.OpenFile(transcript, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, `{"type":"assistant","timestamp":%q}`+"\n", second); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if err := nc.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break
		}
		frame := decodeWiringLine(t, line)
		if frame["type"] != "session_changed" {
			continue
		}
		s, _ := frame["session"].(map[string]any)
		if s["sessionId"] == "working-job" && s["lastActivityAt"] == second {
			return
		}
	}
	t.Fatalf("a transcript-only append (record byte-identical) never reached the subscriber: "+
		"lastActivityAt stayed at %s instead of %s", first, second)
}

func decodeWiringLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("not a JSON frame: %v\n%s", err, line)
	}
	return f
}
