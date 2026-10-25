package snapshot_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustDecode(t *testing.T, b []byte) *snapshot.Snapshot {
	t.Helper()
	s, err := snapshot.Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return s
}

// minimal is the smallest valid snapshot: one session, one window, one pane.
func minimal() *snapshot.Snapshot {
	return &snapshot.Snapshot{
		Format: snapshot.FormatName, Version: snapshot.Version, Source: snapshot.SourceSave,
		Socket:   snapshot.Socket{Name: "default", Key: "default"},
		Client:   &snapshot.Client{},
		Sessions: []snapshot.Session{{Name: "main", ActiveWindow: 1, LastWindow: -1}},
		Windows: []snapshot.Window{{Session: "main", Index: 1, ID: "@1", Name: "sh", Active: true,
			AutomaticRename: snapshot.AutoRenameUnset, Layout: "b25f,80x24,0,0,1", LayoutFormat: snapshot.LayoutV1,
			Width: 80, Height: 24}},
		Panes: []snapshot.Pane{{Key: "main:1.1", Session: "main", Window: 1, Index: 1, SavedID: "%1",
			Cwd: "/home/user", Active: true, CurrentCommand: "sh", Role: snapshot.RoleShell}},
	}
}

// TestSnapshot_EncodeIsStrict: every fixture decodes, re-encodes, and the
// re-encoded document passes a DisallowUnknownFields decoder, so a typo in our
// own writer (a field the reader would silently ignore) fails CI (§6.1).
func TestSnapshot_EncodeIsStrict(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	inputs := map[string][]byte{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		inputs[filepath.Base(f)] = b
	}
	mb, err := snapshot.Encode(minimal())
	if err != nil {
		t.Fatal(err)
	}
	inputs["minimal()"] = mb

	for name, in := range inputs {
		s := mustDecode(t, in)
		out, err := snapshot.Encode(s)
		if err != nil {
			t.Fatalf("%s: Encode: %v", name, err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.DisallowUnknownFields()
		var strict snapshot.Snapshot
		if err := dec.Decode(&strict); err != nil {
			t.Fatalf("%s: strict decode of our own output: %v", name, err)
		}
		if !reflect.DeepEqual(&strict, s) {
			t.Fatalf("%s: round trip changed the snapshot", name)
		}
		if !bytes.HasSuffix(out, []byte("}\n")) || !bytes.HasPrefix(out, []byte("{\n \"format\"")) {
			t.Fatalf("%s: not one-space indented and newline terminated:\n%.80s", name, out)
		}
		// The fixture files are written in the canonical form.
		if strings.HasSuffix(name, ".json") && !bytes.Equal(out, in) {
			t.Errorf("%s: fixture is not in canonical Encode form", name)
		}
	}
}

func TestSnapshot_DecodeIsLenient(t *testing.T) {
	in := fixture(t, "full.json")
	var m map[string]any
	if err := json.Unmarshal(in, &m); err != nil {
		t.Fatal(err)
	}
	m["a_future_field"] = map[string]any{"x": 1}
	m["panes"].([]any)[0].(map[string]any)["future_pane_field"] = "y"
	b, _ := json.Marshal(m)
	s := mustDecode(t, b)
	if len(s.Panes) != 4 || s.Panes[0].SID() == "" {
		t.Fatalf("lenient decode lost data: %+v", s.Panes)
	}

	m["version"] = 2
	b, _ = json.Marshal(m)
	if _, err := snapshot.Decode(b); !errors.Is(err, snapshot.ErrNewerFormat) {
		t.Fatalf("version 2: got %v, want ErrNewerFormat", err)
	}
}

func TestSnapshot_DecodeRequiredFields(t *testing.T) {
	in := fixture(t, "full.json")
	for _, drop := range []string{"format", "version", "sessions", "windows", "panes"} {
		var m map[string]any
		_ = json.Unmarshal(in, &m)
		delete(m, drop)
		b, _ := json.Marshal(m)
		if _, err := snapshot.Decode(b); !errors.Is(err, snapshot.ErrInvalid) {
			t.Errorf("without %q: got %v, want ErrInvalid", drop, err)
		}
	}
	for _, drop := range []string{"key", "session", "window", "index"} {
		var m map[string]any
		_ = json.Unmarshal(in, &m)
		delete(m["panes"].([]any)[1].(map[string]any), drop)
		b, _ := json.Marshal(m)
		if _, err := snapshot.Decode(b); !errors.Is(err, snapshot.ErrInvalid) {
			t.Errorf("pane without %q: got %v, want ErrInvalid", drop, err)
		}
	}
	var m map[string]any
	_ = json.Unmarshal(in, &m)
	m["format"] = "tmux-resurrect"
	b, _ := json.Marshal(m)
	if _, err := snapshot.Decode(b); !errors.Is(err, snapshot.ErrInvalid) {
		t.Errorf("foreign format: got %v", err)
	}
}

// TestSnapshot_NoPositionalFields (B17, B24): every value is a named field. A
// pane whose title is empty, and fields holding tabs, the old resurrect
// separator, the ':' sentinel and spaces, come back exactly.
func TestSnapshot_NoPositionalFields(t *testing.T) {
	s := minimal()
	s.Panes[0].Title = ""
	s.Panes[0].Cwd = "/home/user/a\tb: c"
	s.Panes[0].FullCommand = ":vim\tx"
	s.Panes[0].Launch = "sleep 1000; true"
	s.Windows[0].Name = "\t:fix; test"
	b, err := snapshot.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	got := mustDecode(t, b)
	if !reflect.DeepEqual(got.Panes, s.Panes) || got.Windows[0].Name != s.Windows[0].Name {
		t.Fatalf("round trip lost a field:\n got %+v\nwant %+v", got.Panes, s.Panes)
	}
	// Every pane object carries the same named keys, empty or not.
	var raw struct{ Panes []map[string]json.RawMessage }
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"title", "cwd", "launch", "claude", "frozen_key"} {
		if _, ok := raw.Panes[0][k]; !ok {
			t.Errorf("pane has no %q key: absent values must be \"\" or null, never omitted", k)
		}
	}
	if string(raw.Panes[0]["claude"]) != "null" {
		t.Errorf("absent claude object = %s, want null", raw.Panes[0]["claude"])
	}
}

// TestSnapshot_EmptyTitleKeepsCwd (RR1, B24): the row that collapsed
// resurrect's columns.
func TestSnapshot_EmptyTitleKeepsCwd(t *testing.T) {
	s := mustDecode(t, fixture(t, "full.json"))
	p := s.Panes[1]
	if p.Title != "" || p.Cwd != "/home/user/dir with space" || p.CurrentCommand != "zsh" {
		t.Fatalf("empty-title pane read back as %+v", p)
	}
}

func TestValidate_FixtureValid(t *testing.T) {
	if err := snapshot.Validate(mustDecode(t, fixture(t, "full.json"))); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(minimal()); err != nil {
		t.Fatal(err)
	}
}

// TestValidate_DuplicateKeys (B18): duplicates by KEY, never by whole line.
func TestValidate_DuplicateKeys(t *testing.T) {
	cases := map[string]func(s *snapshot.Snapshot){
		"pane key": func(s *snapshot.Snapshot) {
			p := s.Panes[0]
			p.Title, p.SavedID = "a different title", "%9" // differs as a line, same key
			s.Panes = append(s.Panes, p)
		},
		"window key": func(s *snapshot.Snapshot) {
			w := s.Windows[0]
			w.Name, w.ID = "other", "@9"
			s.Windows = append(s.Windows, w)
		},
		"session name": func(s *snapshot.Snapshot) { s.Sessions = append(s.Sessions, s.Sessions[0]) },
	}
	for name, mut := range cases {
		s := minimal()
		mut(s)
		if err := snapshot.Validate(s); !errors.Is(err, snapshot.ErrInvalid) || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("%s: got %v, want a duplicate ErrInvalid", name, err)
		}
	}
}

// TestValidate_TruncatedDump (B18, R22, RR2): the 2026-08-17 shape — panes but
// no windows — and every other partial dump are refused.
func TestValidate_TruncatedDump(t *testing.T) {
	cases := map[string]func(s *snapshot.Snapshot){
		"no windows (192 panes, 0 windows)": func(s *snapshot.Snapshot) { s.Windows = nil },
		"no panes":                          func(s *snapshot.Snapshot) { s.Panes = nil },
		"no client":                         func(s *snapshot.Snapshot) { s.Client = nil },
		"pane names no window": func(s *snapshot.Snapshot) {
			s.Panes[0].Window, s.Panes[0].Key = 7, "main:7.1"
		},
		"window has no panes": func(s *snapshot.Snapshot) {
			w := s.Windows[0]
			w.Index, w.ID = 2, "@2"
			s.Windows = append(s.Windows, w)
		},
		"window names no session": func(s *snapshot.Snapshot) { s.Sessions = nil },
		"key disagrees":           func(s *snapshot.Snapshot) { s.Panes[0].Key = "main:1.2" },
		"unsafe sid": func(s *snapshot.Snapshot) {
			s.Panes[0].Claude = &snapshot.Claude{SessionID: "x; touch /tmp/pwn"}
		},
		"v1 layout labelled v2": func(s *snapshot.Snapshot) { s.Windows[0].LayoutFormat = snapshot.LayoutV2 },
		"v2 layout labelled v1": func(s *snapshot.Snapshot) { s.Windows[0].Layout = `{"V":2}` },
	}
	for name, mut := range cases {
		s := minimal()
		mut(s)
		if err := snapshot.Validate(s); !errors.Is(err, snapshot.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	// A dump cut off mid-file does not decode at all.
	full := fixture(t, "full.json")
	if _, err := snapshot.Decode(full[:len(full)/2]); !errors.Is(err, snapshot.ErrInvalid) {
		t.Errorf("truncated file: got %v", err)
	}
}

func TestSafeSID(t *testing.T) {
	for _, ok := range []string{"0b9a1c2d-0000-4000-8000-000000000001", "a_b.c-1"} {
		if !snapshot.SafeSID(ok) {
			t.Errorf("SafeSID(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "a b", "x;y", "$(id)", "a\nb", "`x`", "a'b"} {
		if snapshot.SafeSID(bad) {
			t.Errorf("SafeSID(%q) = true", bad)
		}
	}
}

func TestFingerprint_IgnoresTimeProducerServerAndContentsName(t *testing.T) {
	a := mustDecode(t, fixture(t, "full.json"))
	b := mustDecode(t, fixture(t, "full.json"))
	b.CreatedAt, b.Producer = "2030-01-01T00:00:00Z", "moox 9.9.9"
	b.Server = &snapshot.Server{Pid: 1, StartTime: 2}
	b.Contents.File, b.Contents.CapturedAt, b.Contents.Panes = "moox-20300101T000000.contents.gz", "later", 7
	if snapshot.Fingerprint(a) != snapshot.Fingerprint(b) {
		t.Fatal("fingerprint depends on time/producer/server/contents name")
	}
	b.Contents.SHA256 = strings.Repeat("1", 64)
	if snapshot.Fingerprint(a) == snapshot.Fingerprint(b) {
		t.Fatal("fingerprint ignores the contents sha256")
	}
	c := mustDecode(t, fixture(t, "full.json"))
	c.Panes[1].Cwd = "/elsewhere"
	if snapshot.Fingerprint(a) == snapshot.Fingerprint(c) {
		t.Fatal("fingerprint ignores structure")
	}
}

func TestContents_RoundTripAndDeterministic(t *testing.T) {
	entries := map[string][]byte{"%12": []byte("twelve\n"), "%9": []byte(""), "%1": []byte("a\n#moox-pane %2 3\nfake\n")}
	srv := snapshot.Server{Pid: 812, StartTime: 1761262800}
	b1, err := snapshot.EncodeContents(srv, entries)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := snapshot.EncodeContents(srv, entries)
	if !bytes.Equal(b1, b2) || snapshot.SHA256(b1) != snapshot.SHA256(b2) {
		t.Fatal("equal entries must give equal bytes (S8: unchanged contents keep the fingerprint)")
	}
	gotSrv, got, err := snapshot.DecodeContents(b1)
	if err != nil {
		t.Fatal(err)
	}
	if gotSrv != srv || !reflect.DeepEqual(got, entries) {
		t.Fatalf("round trip: %+v %q", gotSrv, got)
	}
	if _, _, err := snapshot.DecodeContents(b1[:len(b1)-6]); err == nil {
		t.Fatal("a truncated contents file decoded")
	}
	if _, _, err := snapshot.DecodeContents([]byte("not gzip")); !errors.Is(err, snapshot.ErrContents) {
		t.Fatalf("garbage: %v", err)
	}
}
