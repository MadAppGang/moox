package blackbox_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type resurrectFixture struct {
	dir, file, sid, launch string
	cwd                    map[string]string
	layouts                map[string]string
}

func tsv(fields ...string) string { return strings.Join(fields, "\t") + "\n" }

// All fixture bytes are authored from resurrect-snapshot-format.md. No live
// snapshot, recordings, archive or implementation is read or copied.
func authorResurrectFixture(e *sandbox) resurrectFixture {
	e.t.Helper()
	f := resurrectFixture{
		dir:    filepath.Join(e.base, "control", "resurrect"),
		sid:    "01234567-89ab-4cde-8fab-0123456789ab",
		launch: "claudish --model example; printf '%s' 'inner;semicolon'",
		cwd: map[string]string{
			"magus:1.1":    filepath.Join(e.home, "project with spaces"),
			"magus:2.1":    filepath.Join(e.home, "collapsed#cwd"),
			"mnemex:1.1":   filepath.Join(e.home, "realigned;cwd"),
			"claudish:1.1": filepath.Join(e.home, "claude-cwd"),
		},
		layouts: map[string]string{
			// Valid tmux checksums, and pane ids unique across the server
			// (%1..%4) as tmux guarantees within one server life: tmux never
			// reports an invalid checksum or two windows holding the same pane.
			"magus:1":    "b25e,80x24,0,0,1",
			"magus:2":    `{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":1,"I":"%2"}}`,
			"mnemex:1":   "b260,80x24,0,0,3",
			"claudish:1": `{"V":2,"L":{"t":"p","w":80,"h":24,"x":0,"y":0,"a":true,"i":1,"I":"%4"}}`,
		},
	}
	for _, cwd := range f.cwd {
		mustMkdir(e.t, cwd)
	}
	f.file = filepath.Join(f.dir, "tmux_resurrect_20261024T004046.txt")
	escaped := strings.Replace(f.cwd["magus:1.1"], " ", "\\ ", 1)
	data := tsv("pane", "magus", "1", "1", ":*", "1", "normal ✳ title; #", ":"+escaped, "1", "sh", ":sleep 1000") +
		"helper --flag=two\n" +
		// Collapsed, never realigned: path is in 7, field 8 is not colon-prefixed.
		tsv("pane", "magus", "2", "0", ":-", "1", ":"+f.cwd["magus:2.1"], "1", "sh", "4567", ":") +
		// Continuity-realigned: empty field 7, pid in 11, command in 12.
		tsv("pane", "mnemex", "1", "1", ":*", "1", "", ":"+f.cwd["mnemex:1.1"], "1", "sh", "5678", ":") +
		tsv("pane", "claudish", "1", "1", ":*", "1", "✳ Claude # literal", ":"+f.cwd["claudish:1.1"], "1", "2.1.309",
			":claude --dangerously-skip-permissions", ";CLAUDE_SID="+f.sid,
			";CLAUDE_CMD="+base64.StdEncoding.EncodeToString([]byte(f.launch)), ";CLAUDISH_REPLAY=--model example -d") +
		tsv("window", "magus", "1", ":normal window", "1", ":*", f.layouts["magus:1"], "off") +
		tsv("window", "magus", "2", ":empty; title #", "0", ":-", f.layouts["magus:2"], "on") +
		tsv("window", "mnemex", "1", ":realigned", "1", ":*", f.layouts["mnemex:1"], ":") +
		tsv("window", "claudish", "1", ":Claude", "1", ":*", f.layouts["claudish:1"], "off") +
		tsv("state", "magus") // Exactly two fields, not a trailing empty third.
	writeFile(e.t, f.file, data, 0600)
	if err := os.Symlink(filepath.Base(f.file), filepath.Join(f.dir, "last")); err != nil {
		e.t.Fatal(err)
	}
	writeFile(e.t, filepath.Join(f.dir, "tmux_resurrect_20261023T004046.txt"), "# older source must remain untouched\n", 0600)
	writeFile(e.t, filepath.Join(f.dir, "archive", "blackbox-nonce"), e.nonce+"\n", 0600)
	return f
}

func assertImportedWorkspace(t *testing.T, snapshot wireSnapshot, f resurrectFixture) {
	t.Helper()
	names := []string{}
	for _, session := range snapshot.Sessions {
		names = append(names, session.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"claudish", "magus", "mnemex"}) {
		t.Errorf("imported sessions=%q", names)
	}
	if snapshot.Client.Session != "magus" || snapshot.Client.LastSession != "" {
		t.Errorf("two-field state row decoded as %+v, want magus/empty", snapshot.Client)
	}
	if len(snapshot.Windows) != 4 || len(snapshot.Panes) != 4 {
		t.Fatalf("imported %d windows/%d panes, want 4/4", len(snapshot.Windows), len(snapshot.Panes))
	}
	windowNames := map[string]string{"magus:1": "normal window", "magus:2": "empty; title #", "mnemex:1": "realigned", "claudish:1": "Claude"}
	seenWindows := map[string]bool{}
	for _, window := range snapshot.Windows {
		key := window.Session + ":" + strconv.Itoa(window.Index)
		if seenWindows[key] || window.Name != windowNames[key] || window.Layout != f.layouts[key] {
			t.Errorf("imported window=%+v, want key/name/layout %s/%q/%q", window, key, windowNames[key], f.layouts[key])
		}
		seenWindows[key] = true
		wantFormat := "v1"
		if strings.HasPrefix(f.layouts[key], "{") {
			wantFormat = "v2"
		}
		if window.LayoutFormat != wantFormat {
			t.Errorf("%s layout_format=%q, want %q", key, window.LayoutFormat, wantFormat)
		}
		rename := map[string]string{"magus:1": "off", "magus:2": "on", "mnemex:1": "unset", "claudish:1": "off"}[key]
		if window.AutomaticRename != rename {
			t.Errorf("%s automatic_rename=%q, want %q", key, window.AutomaticRename, rename)
		}
	}
	titles := map[string]string{"magus:1.1": "normal ✳ title; #", "magus:2.1": "", "mnemex:1.1": "", "claudish:1.1": "✳ Claude # literal"}
	seen := map[string]bool{}
	for _, pane := range snapshot.Panes {
		if seen[pane.Key] {
			t.Errorf("duplicate imported key %q", pane.Key)
		}
		seen[pane.Key] = true
		cwd, ok := f.cwd[pane.Key]
		if !ok || pane.Cwd != cwd || pane.Title != titles[pane.Key] {
			t.Errorf("imported pane=%+v, want key/cwd/title %s/%q/%q", pane, pane.Key, cwd, titles[pane.Key])
		}
		if pane.Key != pane.Session+":"+strconv.Itoa(pane.Window)+"."+strconv.Itoa(pane.Index) {
			t.Errorf("imported key disagrees with coordinates: %+v", pane)
		}
		switch pane.Key {
		case "magus:1.1":
			// "Attach continuation lines to the preceding pane record"; the
			// join character is not specified, only that the line is kept, in order.
			first, second := strings.Index(pane.FullCommand, "sleep 1000"), strings.Index(pane.FullCommand, "helper --flag=two")
			if first != 0 || second <= first {
				t.Errorf("continuation line not attached to full_command=%q", pane.FullCommand)
			}
		case "magus:2.1", "mnemex:1.1":
			if pane.FullCommand != "" || pane.CurrentCommand != "sh" {
				t.Errorf("pid/sentinel became a command: %+v", pane)
			}
		case "claudish:1.1":
			if pane.Launch != f.launch || pane.Claude == nil || pane.Claude.SessionID != f.sid {
				t.Errorf("continuity tags not preserved: %+v", pane)
			}
			if pane.Claude != nil && pane.Claude.Replay != "--model example -d" {
				t.Errorf("replay=%q", pane.Claude.Replay)
			}
		}
	}
	for key := range f.cwd {
		if !seen[key] {
			t.Errorf("missing imported pane %s", key)
		}
	}
}

// TEST-37, REQ-23/REQ-21: --from accepts a file or a directory's last symlink;
// --out is used so imports never need to contact a server.
func TestImport_FileAndDirectoryPreserveAllContractResurrectRowShapes(t *testing.T) {
	for _, kind := range []string{"file", "directory_symlink_last", "directory_regular_last"} {
		t.Run(kind, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
			fixture := authorResurrectFixture(e)
			source := fixture.file
			if strings.HasPrefix(kind, "directory") {
				source = fixture.dir
			}
			if kind == "directory_regular_last" {
				last := filepath.Join(fixture.dir, "last")
				if err := os.Remove(last); err != nil {
					t.Fatal(err)
				}
				writeFile(t, last, string(readFile(t, fixture.file)), 0600)
			}
			before := image(t, fixture.dir)
			output, report := filepath.Join(e.base, "control", "imported.json"), filepath.Join(e.base, "control", "report.txt")
			r := e.mooxRun("import", "--from", source, "--out", output, "--report", report)
			requireCode(t, r, 0)
			snapshot := decodeWrittenSnapshot(t, readFile(t, output), "import-resurrect")
			assertImportedWorkspace(t, snapshot, fixture)
			equalImage(t, image(t, fixture.dir), before, "read-only resurrect import source")
			if info, err := os.Stat(report); err != nil || info.Size() == 0 {
				t.Errorf("--report did not write a nonempty report: %v", err)
			}
			if pointers := e.pointerFiles(); len(pointers) != 0 {
				t.Errorf("--out unexpectedly published pointers: %q", pointers)
			}
		})
	}
}

// TEST-38, REQ-23/REQ-35.
func TestImport_NonResurrectInputReturnsExit4WithoutPublishing(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
	source := filepath.Join(e.base, "control", "not-resurrect.txt")
	output := filepath.Join(e.base, "control", "must-not-exist.json")
	writeFile(t, source, "this is not a resurrect snapshot\n", 0600)
	requireCode(t, e.mooxRun("import", "--from", source, "--out", output), 4)
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Errorf("invalid import wrote output: %v", err)
	}
	if pointers := e.pointerFiles(); len(pointers) != 0 {
		t.Errorf("invalid import published: %q", pointers)
	}
}
