package blackbox_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func readContentsFrames(t *testing.T, data []byte, identity wireServer) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := bufio.NewReader(gz)
	header, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("contents header: %v", err)
	}
	want := fmt.Sprintf("#moox-contents 1 %d %d\n", identity.PID, identity.StartTime)
	if header != want {
		t.Fatalf("contents header=%q, want exact %q", header, want)
	}
	frames := map[string]string{}
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF && line == "" {
			break
		}
		if err != nil {
			t.Fatalf("contents record header: %v (%q)", err, line)
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "#moox-pane" || !strings.HasPrefix(fields[1], "%") {
			t.Fatalf("bad record header %q", line)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size > 16<<20 {
			t.Fatalf("bad/oversize fixture record length %q", fields[2])
		}
		if _, exists := frames[fields[1]]; exists {
			t.Fatalf("duplicate pane-id contents record %s", fields[1])
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			t.Fatal(err)
		}
		frames[fields[1]] = string(payload)
	}
	return frames
}

func snapshotFrames(e *sandbox, target string, snapshot wireSnapshot) map[string]string {
	e.t.Helper()
	if snapshot.Contents == nil {
		e.t.Fatal("capture-on snapshot has null contents")
	}
	contents := snapshot.Contents
	// §4.2 names published contents moox-<ts>.contents.gz; an explicit
	// `import --out FILE` names its JSON, so only the published form needs moox-.
	published := strings.HasPrefix(filepath.Base(target), "moox-")
	if contents.File == "" || filepath.Base(contents.File) != contents.File || (published && !strings.HasPrefix(contents.File, "moox-")) || !strings.HasSuffix(contents.File, ".contents.gz") {
		e.t.Fatalf("contents filename=%q, want same-directory *.contents.gz basename (moox-* when published)", contents.File)
	}
	path := filepath.Join(filepath.Dir(target), contents.File)
	e.requireInside(path)
	data := readFile(e.t, path)
	digest := sha256.Sum256(data)
	if contents.SHA256 != hex.EncodeToString(digest[:]) {
		e.t.Errorf("contents sha256=%q, want %x", contents.SHA256, digest)
	}
	if _, err := time.Parse(time.RFC3339, contents.CapturedAt); err != nil {
		e.t.Errorf("captured_at=%q", contents.CapturedAt)
	}
	requireMode(e.t, path, 0600)
	frames := readContentsFrames(e.t, data, contents.Server)
	if len(frames) != contents.Panes {
		e.t.Errorf("contents.panes=%d, actual records=%d", contents.Panes, len(frames))
	}
	return frames
}

func printMarker(s *server, id, marker string) {
	s.e.t.Helper()
	s.mustTmux("send-keys", "-t", id, "-l", "printf '%s\\n' "+shQuote(marker))
	s.mustTmux("send-keys", "-t", id, "Enter")
	eventually(s.e.t, 5*time.Second, "own shell marker output", func() (bool, string) {
		text := s.mustTmux("capture-pane", "-p", "-t", id)
		// Match a whole output line, not the typed printf command line.
		return strings.Contains("\n"+text+"\n", "\n"+marker+"\n"), text
	})
}

func captureMarkedWorkspace(e *sandbox) (*server, string, wireSnapshot, map[string]string) {
	e.t.Helper()
	s := e.newServer("contents", "SELF", false)
	s.makeRestorable()
	ids := strings.Fields(s.mustTmux("list-panes", "-a", "-F", "#{pane_id}"))
	markers := map[string]string{}
	for i, id := range ids {
		marker := fmt.Sprintf("MOOX_BB_%s_PANE_%d", e.nonce, i)
		markers[id] = marker
		printMarker(s, id, marker)
	}
	s.mustTmux("set-option", "-g", "@moox-capture-contents", "on")
	_, target := s.save()
	snapshot := decodeWrittenSnapshot(e.t, readFile(e.t, target), "save")
	return s, target, snapshot, markers
}

// TEST-43, REQ-20/REQ-21: real shell output is serialized using the exact gzip
// framing/hash and pane IDs. Each pane's frame contains its marker, not another's.
func TestContents_SaveCapturesEveryShellPaneWithExactFramingHashAndIdentity(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	_, target, snapshot, markers := captureMarkedWorkspace(e)
	frames := snapshotFrames(e, target, snapshot)
	if snapshot.Contents.Server != snapshot.Server {
		t.Errorf("contents and snapshot server lives differ: %+v / %+v", snapshot.Contents.Server, snapshot.Server)
	}
	if len(frames) != len(markers) {
		t.Errorf("captured frames=%d, want all %d panes", len(frames), len(markers))
	}
	for _, pane := range snapshot.Panes {
		text, exists := frames[pane.SavedID]
		if !exists || !pane.Contents || !strings.Contains(text, markers[pane.SavedID]) {
			t.Errorf("missing own frame/marker for %+v: %q", pane, text)
		}
		for other, marker := range markers {
			if other != pane.SavedID && strings.Contains(text, marker) {
				t.Errorf("pane %s received %s scrollback", pane.SavedID, other)
			}
		}
	}
}

// TEST-44, REQ-20/REQ-21: structural saves reuse only matching pane IDs in the
// same server life. A new server on the same socket must not reuse old frames.
func TestContents_StructuralSaveNeverAssignsDeadPaneOrPreviousServerScrollback(t *testing.T) {
	for _, kind := range []string{"same_server", "new_server"} {
		t.Run(kind, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			if kind == "new_server" {
				s, _, original, _ := captureMarkedWorkspace(e)
				s.stop()
				fresh := e.newServer("contents", "SELF", false)
				fresh.makeRestorable()
				_, target := fresh.save() // new server has capture off, as the wrapper requires
				snapshot := decodeWrittenSnapshot(t, readFile(t, target), "save")
				if snapshot.Server == original.Server {
					t.Fatal("fixture did not create a distinct server life")
				}
				if snapshot.Contents != nil {
					t.Errorf("new server reused old server contents: %+v", snapshot.Contents)
				}
				for _, p := range snapshot.Panes {
					if p.Contents {
						t.Errorf("new pane incorrectly has old contents: %+v", p)
					}
				}
				return
			}
			s := e.newServer("contents", "SELF", false)
			s.makeRestorable()
			first := s.mustTmux("display-message", "-p", "-t", "magus:0", "#{pane_id}")
			second := s.mustTmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", first, "-c", e.home, "/bin/sh")
			deadMarker, keptMarker := "DEAD_"+e.nonce, "KEPT_"+e.nonce
			printMarker(s, first, deadMarker)
			printMarker(s, second, keptMarker)
			s.mustTmux("set-option", "-g", "@moox-capture-contents", "on")
			_, oldTarget := s.save()
			old := decodeWrittenSnapshot(t, readFile(t, oldTarget), "save")
			oldFrames := snapshotFrames(e, oldTarget, old)
			if !strings.Contains(oldFrames[second], keptMarker) {
				t.Fatal("fixture failed to capture surviving pane marker")
			}
			oldKey := ""
			for _, p := range old.Panes {
				if p.SavedID == second {
					oldKey = p.Key
				}
			}
			s.mustTmux("set-option", "-g", "@moox-capture-contents", "off")
			s.mustTmux("kill-pane", "-t", first)
			created := s.mustTmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", second, "-c", e.home, "/bin/sh")
			e.rememberPanePIDs(s)
			_, target := s.save()
			snapshot := decodeWrittenSnapshot(t, readFile(t, target), "save")
			if snapshot.Contents == nil {
				// The contract does not say whether a save with
				// @moox-capture-contents off reuses earlier contents or drops
				// them. Dropping them cannot misassign scrollback; then no row
				// may claim contents. Reuse itself is a Known Gap in that case.
				for _, p := range snapshot.Panes {
					if p.Contents {
						t.Errorf("pane claims contents but snapshot has none: %+v", p)
					}
				}
				t.Log("capture-off save published contents:null; reuse branch not observable black-box")
				return
			}
			frames := snapshotFrames(e, target, snapshot)
			if snapshot.Contents.SHA256 != old.Contents.SHA256 || snapshot.Contents.Server != old.Contents.Server {
				t.Error("same-server structural save did not reuse existing captured contents")
			}
			seenKept, seenNew := false, false
			for _, p := range snapshot.Panes {
				switch p.SavedID {
				case second:
					seenKept = true
					if p.Key == oldKey {
						t.Fatal("fixture did not shift surviving pane's positional key")
					}
					if !p.Contents || !strings.Contains(frames[p.SavedID], keptMarker) || strings.Contains(frames[p.SavedID], deadMarker) {
						t.Errorf("surviving pane has incorrect scrollback: %+v", p)
					}
				case created:
					seenNew = true
					if p.Contents {
						t.Errorf("new pane inherited old positional contents: %+v", p)
					}
					if _, exists := frames[p.SavedID]; exists {
						t.Error("new uncaptured pane unexpectedly has a frame")
					}
				}
			}
			if !seenKept || !seenNew {
				t.Error("missing kept or newly created pane in saved snapshot")
			}
			if _, err := os.Stat(oldTarget); err != nil {
				t.Errorf("structural save lost source snapshot: %v", err)
			}
		})
	}
}
