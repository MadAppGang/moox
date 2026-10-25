package main

// `tt sessions --no-transcript`, end to end through the real composition root.
//
// The independent black-box suite found the flag narrower than its contract:
// with it, a background job still reported transcriptCollected: true and a
// lastActivityAt, because the 64 KiB timing read ignored it. §5.2 says the flag
// makes transcriptCollected false on EVERY session and lastActivityAt null.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

func TestNoTranscriptSetsBothTranscriptOptions(t *testing.T) {
	on, _, _, _, ok := parseSessionsFlags(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok || on.Snapshot.NoTranscriptReads {
		t.Fatalf("the default reads transcripts (ok %v, NoTranscriptReads %v)", ok, on.Snapshot.NoTranscriptReads)
	}
	off, _, _, _, ok := parseSessionsFlags([]string{"--no-transcript"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !ok || !off.Snapshot.NoTranscriptReads || off.Snapshot.IncludeTranscripts {
		t.Fatalf("--no-transcript: NoTranscriptReads %v IncludeTranscripts %v, want true and false",
			off.Snapshot.NoTranscriptReads, off.Snapshot.IncludeTranscripts)
	}
}

func TestNoTranscriptReadsNoTranscriptThroughTheRealCLI(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(root, "projects", "own", "job-thread.jsonl")
	jobDir := filepath.Join(root, "jobs", "0770b000")
	for _, d := range []string{filepath.Join(root, "sessions"), filepath.Dir(transcript), jobDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stamp := observer.FormatTime(time.Now().Add(-time.Minute))
	if err := os.WriteFile(transcript, []byte(fmt.Sprintf(`{"type":"assistant","timestamp":%q}`+"\n", stamp)), 0o600); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"sessionId":"0770b000-0000-4000-8000-000000000001","daemonShort":"0770b000",`+
		`"state":"blocked","tempo":"blocked","updatedAt":%q,"linkScanPath":%q}`, stamp, transcript)
	if err := os.WriteFile(filepath.Join(jobDir, "state.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	hermeticTree(t, root)

	run := func(args ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runSessions(append([]string{"--json"}, args...), &stdout, &stderr); code != 0 {
			t.Fatalf("tt sessions %v exited %d: %s", args, code, stderr.String())
		}
		var envelope map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, stdout.String())
		}
		sessions, _ := envelope["sessions"].([]any)
		if len(sessions) != 1 {
			t.Fatalf("%d sessions, want the one job", len(sessions))
		}
		return sessions[0].(map[string]any)
	}

	// The control: by default the job is timed from its own transcript.
	if s := run(); s["transcriptCollected"] != true || s["lastActivityAt"] != stamp {
		t.Fatalf("control: transcriptCollected %v lastActivityAt %v, want true and %s",
			s["transcriptCollected"], s["lastActivityAt"], stamp)
	}

	s := run("--no-transcript")
	if s["transcriptCollected"] != false {
		t.Errorf("transcriptCollected = %v under --no-transcript, want false", s["transcriptCollected"])
	}
	if s["lastActivityAt"] != nil {
		t.Errorf("lastActivityAt = %v under --no-transcript, want null: no transcript was read", s["lastActivityAt"])
	}
}
