// Package snapshot is moox's on-disk snapshot format, version 1 (architecture
// §6.1). It is the second wire package of the module (§2.4 JSONTags): the
// json tags of the format live here and nowhere else, and the package imports
// nothing of the module, so nothing it depends on can change the format.
//
// Rules the format keeps:
//
//   - One JSON file per snapshot, UTF-8, indented with one space, "\n"
//     terminated. No omitempty (the module bans it): an absent object is null,
//     an absent string is "", an absent window index is -1.
//   - Every value is a named field. Nothing is positional and nothing is split
//     on whitespace, so an empty title can never shift a cwd into its place
//     (B17, B24, R1).
//   - Decode is lenient (unknown fields are ignored, so a downgraded binary
//     reads a newer additive snapshot); Encode is strict (the tests round-trip
//     every fixture through a DisallowUnknownFields decoder). Version changes
//     only for a breaking change (PR-M17).
package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// FormatName is the value of the "format" field.
const FormatName = "moox-snapshot"

// Version is the newest format version this binary reads and the one it
// writes.
const Version = 1

// Sources.
const (
	SourceSave   = "save"
	SourceImport = "import-resurrect"
)

// Roles say how a pane is RESTORED AS A PANE. A role never decides on its own
// whether a command is replayed; that is relaunch's job (§6.5, PR-H19).
const (
	RoleShell      = "shell"
	RoleClaude     = "claude"
	RoleProgram    = "program"
	RoleTTObserver = "tt-observer"
	RoleMCPHelper  = "mcp-helper"
	RoleFrozen     = "frozen"
)

// Layout formats as tmux reported them.
const (
	LayoutV1 = "v1"
	LayoutV2 = "v2"
)

// Automatic-rename tri-state (T6).
const (
	AutoRenameOn    = "on"
	AutoRenameOff   = "off"
	AutoRenameUnset = "unset"
)

var (
	// ErrInvalid is a snapshot that must not be published or restored: it does
	// not decode, misses a required field, or fails Validate (B18, B19).
	ErrInvalid = errors.New("snapshot: invalid")
	// ErrNewerFormat is a snapshot written by a newer moox (exit 4, "upgrade
	// moox").
	ErrNewerFormat = errors.New("snapshot: written by a newer moox; upgrade moox")
)

// Snapshot is one saved workspace.
type Snapshot struct {
	Format      string    `json:"format"`
	Version     int       `json:"version"`
	CreatedAt   string    `json:"created_at"`
	Producer    string    `json:"producer"`
	Source      string    `json:"source"`
	TmuxVersion string    `json:"tmux_version"`
	Socket      Socket    `json:"socket"`
	Server      *Server   `json:"server"`
	Client      *Client   `json:"client"`
	Contents    *Contents `json:"contents"`
	Sessions    []Session `json:"sessions"`
	Windows     []Window  `json:"windows"`
	Panes       []Pane    `json:"panes"`
}

// Socket names the server the snapshot was taken from (§4.1).
type Socket struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Server is one server life: pane ids are unique within it (R2-C7).
type Server struct {
	Pid       int   `json:"pid"`
	StartTime int64 `json:"start_time"`
}

// Same reports whether two server lives are the same one. A nil is never the
// same as anything.
func (s *Server) Same(o *Server) bool {
	return s != nil && o != nil && s.Pid == o.Pid && s.StartTime == o.StartTime
}

// Client is the attached client's session and last session (T11, RR16).
type Client struct {
	Session     string `json:"session"`
	LastSession string `json:"last_session"`
}

// Contents names the pane-contents file the snapshot was built with, by name
// AND sha256 (RR14), and the server life its entries are keyed in.
type Contents struct {
	File       string  `json:"file"`
	SHA256     string  `json:"sha256"`
	Panes      int     `json:"panes"`
	CapturedAt string  `json:"captured_at"`
	Server     *Server `json:"server"`
}

// Session is one session. A grouped secondary has GroupedWith set to its
// primary's name and owns no windows (S5).
type Session struct {
	Name         string `json:"name"`
	Group        string `json:"group"`
	GroupedWith  string `json:"grouped_with"`
	ActiveWindow int    `json:"active_window"`
	LastWindow   int    `json:"last_window"` // -1: none
}

// Window is one window of a primary session.
type Window struct {
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

// Pane is one pane row.
type Pane struct {
	Key            string  `json:"key"` // "S:W.P"
	Session        string  `json:"session"`
	Window         int     `json:"window"`
	Index          int     `json:"index"`
	SavedID        string  `json:"saved_id"` // the pane id in the server life the snapshot names
	Title          string  `json:"title"`
	Cwd            string  `json:"cwd"`
	Active         bool    `json:"active"`
	CurrentCommand string  `json:"current_command"`
	FullCommand    string  `json:"full_command"`
	Role           string  `json:"role"`
	Launch         string  `json:"launch"` // first line of launch/<N>; "" if none (B1, B13)
	Claude         *Claude `json:"claude"`
	FrozenKey      string  `json:"frozen_key"`
	Contents       bool    `json:"contents"` // the contents file holds an entry for SavedID
}

// Claude is a pane's Claude session.
type Claude struct {
	SessionID string `json:"session_id"`
	Launcher  string `json:"launcher"` // "claude" | "claudish"
	Replay    string `json:"replay"`   // claudish replay flags (B12)
}

// SID is the pane's session id, or "".
func (p Pane) SID() string {
	if p.Claude == nil {
		return ""
	}
	return p.Claude.SessionID
}

// PaneKey is the "S:W.P" key of a pane.
func PaneKey(session string, window, index int) string {
	return fmt.Sprintf("%s:%d.%d", session, window, index)
}

// Encode writes the snapshot: one-space indent, "\n" terminated, empty lists
// as [] (never null, which Decode would read as a missing required field).
func Encode(s *Snapshot) ([]byte, error) {
	c := *s
	if c.Sessions == nil {
		c.Sessions = []Session{}
	}
	if c.Windows == nil {
		c.Windows = []Window{}
	}
	if c.Panes == nil {
		c.Panes = []Pane{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(&c); err != nil {
		return nil, fmt.Errorf("snapshot: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// required top-level and per-pane fields (§6.1). Their absence is ErrInvalid;
// every other field is optional on read.
var (
	requiredTop  = []string{"format", "version", "sessions", "windows", "panes"}
	requiredPane = []string{"key", "session", "window", "index"}
)

// Decode reads a snapshot leniently: unknown fields are ignored, a version
// newer than Version is ErrNewerFormat, and a missing required field or a
// document that does not parse is ErrInvalid.
func Decode(data []byte) (*Snapshot, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for _, k := range requiredTop {
		if raw, ok := top[k]; !ok || string(raw) == "null" {
			return nil, fmt.Errorf("%w: missing %q", ErrInvalid, k)
		}
	}
	var head struct {
		Format  string
		Version int
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if head.Format != FormatName {
		return nil, fmt.Errorf("%w: format %q", ErrInvalid, head.Format)
	}
	if head.Version > Version {
		return nil, fmt.Errorf("%w (version %d > %d)", ErrNewerFormat, head.Version, Version)
	}
	if head.Version < 1 {
		return nil, fmt.Errorf("%w: version %d", ErrInvalid, head.Version)
	}
	var panes []map[string]json.RawMessage
	if err := json.Unmarshal(top["panes"], &panes); err != nil {
		return nil, fmt.Errorf("%w: panes: %v", ErrInvalid, err)
	}
	for i, p := range panes {
		for _, k := range requiredPane {
			if raw, ok := p[k]; !ok || string(raw) == "null" {
				return nil, fmt.Errorf("%w: pane %d: missing %q", ErrInvalid, i, k)
			}
		}
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return &s, nil
}
