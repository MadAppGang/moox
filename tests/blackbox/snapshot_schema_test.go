package blackbox_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Contract-only wire types: §6.1, not imported or copied from the product.
type wireServer struct {
	PID       int   `json:"pid"`
	StartTime int64 `json:"start_time"`
}
type wireContents struct {
	File       string     `json:"file"`
	SHA256     string     `json:"sha256"`
	Panes      int        `json:"panes"`
	CapturedAt string     `json:"captured_at"`
	Server     wireServer `json:"server" wire:"nullable"`
}
type wireSession struct {
	Name         string `json:"name"`
	Group        string `json:"group"`
	GroupedWith  string `json:"grouped_with"`
	ActiveWindow int    `json:"active_window"`
	LastWindow   int    `json:"last_window"`
}
type wireWindow struct {
	Session         string `json:"session"`
	Index           int    `json:"index"`
	ID              string `json:"id"`
	Name            string `json:"name"`
	Active          bool   `json:"active"`
	Last            bool   `json:"last"`
	Zoomed          bool   `json:"zoomed"`
	AutomaticRename string `json:"automatic_rename"`
	Layout          string `json:"layout"`
	LayoutFormat    string `json:"layout_format"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
}
type wireClaude struct {
	SessionID string `json:"session_id"`
	Launcher  string `json:"launcher"`
	Replay    string `json:"replay"`
}
type wirePane struct {
	Key            string      `json:"key"`
	Session        string      `json:"session"`
	Window         int         `json:"window"`
	Index          int         `json:"index"`
	SavedID        string      `json:"saved_id"`
	Title          string      `json:"title"`
	Cwd            string      `json:"cwd"`
	Active         bool        `json:"active"`
	CurrentCommand string      `json:"current_command"`
	FullCommand    string      `json:"full_command"`
	Role           string      `json:"role"`
	Launch         string      `json:"launch"`
	Claude         *wireClaude `json:"claude"`
	FrozenKey      string      `json:"frozen_key"`
	Contents       bool        `json:"contents"`
}
type wireSnapshot struct {
	Format      string `json:"format"`
	Version     int    `json:"version"`
	CreatedAt   string `json:"created_at"`
	Producer    string `json:"producer"`
	Source      string `json:"source"`
	TmuxVersion string `json:"tmux_version"`
	Socket      struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	} `json:"socket"`
	Server wireServer `json:"server" wire:"nullable"`
	Client struct {
		Session     string `json:"session"`
		LastSession string `json:"last_session"`
	} `json:"client"`
	Contents *wireContents `json:"contents"`
	Sessions []wireSession `json:"sessions"`
	Windows  []wireWindow  `json:"windows"`
	Panes    []wirePane    `json:"panes"`
}

// DisallowUnknownFields detects writer typos; recursively checking field presence
// also detects omissions that Go's zero-value decoding would otherwise hide.
func requireWireFields(t *testing.T, raw json.RawMessage, typ reflect.Type, path string) {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return
		}
		requireWireFields(t, raw, typ.Elem(), path)
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			t.Fatalf("%s must be a JSON object: %s (%v)", path, raw, err)
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := field.Tag.Get("json")
			child, ok := obj[key]
			if !ok {
				t.Fatalf("%s.%s missing: writer must not omit fields", path, key)
			}
			// §6.1: "an absent object is null". Objects the contract marks
			// "or null" (server life of an import, contents.server) may be null;
			// the field must still be present. Save tests assert the live pid.
			if field.Tag.Get("wire") == "nullable" && bytes.Equal(bytes.TrimSpace(child), []byte("null")) {
				continue
			}
			requireWireFields(t, child, field.Type, path+"."+key)
		}
	case reflect.Slice:
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatalf("%s must be an array: %v", path, err)
		}
		for i, row := range rows {
			requireWireFields(t, row, typ.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	default:
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			t.Errorf("%s is null, want the contract's scalar type", path)
		}
	}
}

func decodeWrittenSnapshot(t *testing.T, data []byte, source string) wireSnapshot {
	t.Helper()
	var snapshot wireSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatalf("strict v1 writer decode: %v\n%s", err, data)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("extra data after snapshot: %v", err)
	}
	requireWireFields(t, data, reflect.TypeOf(snapshot), "$")
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(data), "", " "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	if !bytes.Equal(pretty.Bytes(), data) {
		t.Error("snapshot must be newline-terminated JSON with one-space indentation")
	}
	if snapshot.Format != "moox-snapshot" || snapshot.Version != 1 || snapshot.Source != source {
		t.Errorf("snapshot identity=%q/%d/%q, want moox-snapshot/1/%s", snapshot.Format, snapshot.Version, snapshot.Source, source)
	}
	if _, err := time.Parse(time.RFC3339, snapshot.CreatedAt); err != nil {
		t.Errorf("created_at is not RFC3339: %q", snapshot.CreatedAt)
	}
	if !strings.HasPrefix(snapshot.Producer, "moox ") {
		t.Errorf("producer=%q, want moox version", snapshot.Producer)
	}
	if len(snapshot.Sessions) == 0 || len(snapshot.Windows) == 0 || len(snapshot.Panes) == 0 {
		t.Fatalf("empty saved/imported workspace: %+v", snapshot)
	}
	return snapshot
}

// TEST-35, REQ-21: exact field names/types, explicit null/empty values, formatting,
// and live values are asserted separately from the existing pointer tests.
func TestSnapshot_SaveWritesStrictV1SchemaAndExactLivePaneValues(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("schema", "SELF", false)
	s.makeRestorable()
	ids := strings.Fields(s.mustTmux("list-panes", "-a", "-F", "#{pane_id}"))
	s.mustTmux("select-pane", "-t", ids[0], "-T", "")
	s.mustTmux("select-pane", "-t", ids[1], "-T", "Unicode ✳ # literal; \\")
	// automatic_rename mirrors resurrect's window-LOCAL value ("unset" when the
	// window has none; the wrapper's off is global). Give window 1 a local off.
	s.mustTmux("set-window-option", "-t", "magus:1", "automatic-rename", "off")
	_, target := s.save()
	snapshot := decodeWrittenSnapshot(t, readFile(t, target), "save")
	if snapshot.Socket.Name != s.name || snapshot.Socket.Key != s.globalEnv("MOOX_STATE_KEY") {
		t.Errorf("snapshot socket=%+v, want selected name/key", snapshot.Socket)
	}
	if snapshot.Server.PID != s.pid || snapshot.Server.StartTime <= 0 {
		t.Errorf("server identity=%+v", snapshot.Server)
	}
	if snapshot.Contents != nil {
		t.Error("contents must be explicit null with capture off and no previous capture")
	}
	tmuxVersion := strings.TrimSpace(e.run(e.tmux, nil, "", "-V").out)
	if snapshot.TmuxVersion != strings.TrimPrefix(tmuxVersion, "tmux ") {
		t.Errorf("tmux_version=%q, want %q", snapshot.TmuxVersion, tmuxVersion)
	}
	live := map[string][]string{}
	for _, line := range strings.Split(s.mustTmux("list-panes", "-a", "-F", strings.Join([]string{
		"#{session_name}", "#{window_index}", "#{pane_index}", "#{pane_id}", "#{pane_title}",
		"#{pane_current_path}", "#{pane_active}", "#{pane_current_command}"}, viewSep)), "\n") {
		fields := strings.Split(line, viewSep)
		if len(fields) != 8 {
			t.Fatalf("bad live oracle: %q", line)
		}
		live[fields[0]+":"+fields[1]+"."+fields[2]] = fields
	}
	if len(snapshot.Panes) != len(live) {
		t.Fatalf("panes=%d, want %d", len(snapshot.Panes), len(live))
	}
	seen := map[string]bool{}
	for _, pane := range snapshot.Panes {
		row, ok := live[pane.Key]
		if !ok || seen[pane.Key] {
			t.Fatalf("unexpected/duplicate pane key %q", pane.Key)
		}
		seen[pane.Key] = true
		if pane.Session != row[0] || strconv.Itoa(pane.Window) != row[1] || strconv.Itoa(pane.Index) != row[2] ||
			pane.SavedID != row[3] || pane.Title != row[4] || pane.Cwd != row[5] || pane.Active != (row[6] == "1") || pane.CurrentCommand != row[7] {
			t.Errorf("pane=%+v, live=%q", pane, row)
		}
		if pane.Claude != nil || pane.Launch != "" || pane.FrozenKey != "" || pane.Contents || pane.Role != "shell" {
			t.Errorf("plain /bin/sh pane has non-shell/absent-field values: %+v", pane)
		}
	}
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Name != "magus" || snapshot.Sessions[0].Group != "" || snapshot.Sessions[0].GroupedWith != "" {
		t.Errorf("sessions=%+v", snapshot.Sessions)
	}
	for _, w := range snapshot.Windows {
		if w.Session != "magus" || w.Width <= 0 || w.Height <= 0 || !regexp.MustCompile(`^@[0-9]+$`).MatchString(w.ID) {
			t.Errorf("invalid saved window: %+v", w)
		}
		want := "v1"
		if strings.HasPrefix(w.Layout, "{") {
			want = "v2"
		}
		rename := map[int]string{0: "unset", 1: "off"}[w.Index]
		if w.LayoutFormat != want || w.AutomaticRename != rename {
			t.Errorf("window layout/rename fields=%+v", w)
		}
	}
}

func objectRows(doc map[string]any, field string) []any { return doc[field].([]any) }
func firstObject(doc map[string]any, field string) map[string]any {
	return objectRows(doc, field)[0].(map[string]any)
}

// TEST-36, REQ-21/REQ-35: mutate only public JSON fields of a real snapshot.
// Every subcase creates its own server, source and output; no ordering dependency.
func TestSnapshot_DryRunAcceptsAdditiveFieldsButRejectsInvalidOrNewerSnapshots(t *testing.T) {
	type fixtureCase struct {
		name    string
		change  func(map[string]any)
		code    int
		upgrade bool
	}
	cases := []fixtureCase{
		{"additive_fields", func(d map[string]any) {
			d["future_optional"] = map[string]any{"v": true}
			firstObject(d, "panes")["future_pane"] = "safe"
		}, 0, false},
		{"newer_version", func(d map[string]any) { d["version"] = 2 }, 4, true},
		{"wrong_format", func(d map[string]any) { d["format"] = "other-snapshot" }, 4, false},
		{"wrong_version_type", func(d map[string]any) { d["version"] = "1" }, 4, false},
		{"empty_windows", func(d map[string]any) { d["windows"] = []any{} }, 4, false},
		{"empty_panes", func(d map[string]any) { d["panes"] = []any{} }, 4, false},
		{"duplicate_pane", func(d map[string]any) { d["panes"] = append(objectRows(d, "panes"), firstObject(d, "panes")) }, 4, false},
		{"duplicate_window", func(d map[string]any) { d["windows"] = append(objectRows(d, "windows"), firstObject(d, "windows")) }, 4, false},
		{"pane_missing_window", func(d map[string]any) { firstObject(d, "panes")["window"] = 99999 }, 4, false},
		{"window_without_pane", func(d map[string]any) { d["panes"] = objectRows(d, "panes")[1:] }, 4, false},
		{"missing_client", func(d map[string]any) { delete(d, "client") }, 4, false},
		{"invalid_claude_sid", func(d map[string]any) {
			firstObject(d, "panes")["claude"] = map[string]any{"session_id": "bad sid;!", "launcher": "claude", "replay": ""}
		}, 4, false},
		{"mismatched_layout_format", func(d map[string]any) {
			w := firstObject(d, "windows")
			if strings.HasPrefix(w["layout"].(string), "{") {
				w["layout_format"] = "v1"
			} else {
				w["layout_format"] = "v2"
			}
		}, 4, false},
	}
	for _, field := range []string{"format", "version", "sessions", "windows", "panes"} {
		field := field
		cases = append(cases, fixtureCase{"missing_" + field, func(d map[string]any) { delete(d, field) }, 4, false})
	}
	for _, field := range []string{"key", "session", "window", "index"} {
		field := field
		cases = append(cases, fixtureCase{"pane_missing_" + field, func(d map[string]any) { delete(firstObject(d, "panes"), field) }, 4, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			s := e.newServer("decode", "SELF", false)
			s.makeRestorable()
			pointer, original := s.save()
			var doc map[string]any
			if err := json.Unmarshal(readFile(t, original), &doc); err != nil {
				t.Fatal(err)
			}
			tc.change(doc)
			data, err := json.MarshalIndent(doc, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(e.base, "control", "candidate.json")
			writeFile(t, fixture, string(data)+"\n", 0600)
			before, files, candidate := s.view(), image(t, filepath.Dir(pointer)), string(readFile(t, fixture))
			r := s.mooxRun("restore", "--snapshot", fixture, "--dry-run")
			requireCode(t, r, tc.code)
			if tc.upgrade && !strings.Contains(strings.ToLower(r.out+r.err), "upgrade moox") {
				t.Errorf("newer-format refusal must say upgrade moox: %q %q", r.out, r.err)
			}
			if !reflect.DeepEqual(s.view(), before) {
				t.Error("local validation changed the live workspace")
			}
			equalImage(t, image(t, filepath.Dir(pointer)), files, "local validation source snapshots")
			if string(readFile(t, fixture)) != candidate {
				t.Error("local validation modified its input")
			}
		})
	}
}
