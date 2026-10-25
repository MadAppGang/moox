// Package manifest is `moox setup`'s record of what it did (architecture §8.3):
// XC/moox/setup.json. `moox uninstall` walks it in reverse. It is a wire
// format, so it is a leaf (§2.3) and one of the packages allowed json tags
// (§2.4 WireDirs).
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
)

// FileName is the manifest's name in XC/moox.
const FileName = "setup.json"

// Version is the manifest format this moox writes.
const Version = 1

// Action kinds.
const (
	KindBackup      = "backup"       // Path was moved to Backup
	KindWrite       = "write"        // Path was created (or replaced) with content hashing to SHA256Written
	KindAppend      = "append"       // Line was appended to Path
	KindClaudeMerge = "claude-merge" // Path (settings.json) was merged; Backup holds the copy taken first
	KindMkdir       = "mkdir"        // Path was created as a directory (uninstall removes it if empty)
)

// Action is one recorded step. Every field is always present in the JSON
// (no omitempty, §5.1): "" means not applicable.
type Action struct {
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	Backup        string `json:"backup"`
	SHA256Written string `json:"sha256_written"`
	Line          string `json:"line"`
}

// Manifest is setup.json.
type Manifest struct {
	Version     int      `json:"version"`
	CreatedAt   string   `json:"created_at"`
	MooxVersion string   `json:"moox_version"`
	Mode        string   `json:"mode"`
	Actions     []Action `json:"actions"`
}

// ErrNewerFormat is a manifest written by a newer moox.
var ErrNewerFormat = errors.New("manifest: written by a newer moox")

// Encode is the on-disk form (indented, trailing newline).
func Encode(m *Manifest) ([]byte, error) {
	if m.Actions == nil {
		m.Actions = []Action{}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Decode parses setup.json.
func Decode(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Version > Version {
		return nil, fmt.Errorf("%w (version %d)", ErrNewerFormat, m.Version)
	}
	return &m, nil
}
