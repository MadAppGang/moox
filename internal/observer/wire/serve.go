package wire

import (
	"encoding/json"
	"fmt"
)

// ServeStatusSchema is the version of `tt serve --status --json`'s document.
const ServeStatusSchema = 1

// ServeStatus is `tt serve --status --json` (moox §4.5): what the per-server
// daemon for one tmux server is doing. It lives here, in the one package that
// carries json tags, like every other document tt prints.
//
// Every field is always present. A value nobody could read is null, never
// absent and never a zero that looks like an answer.
type ServeStatus struct {
	Schema int `json:"schema"`
	// Socket is the tmux socket the status is about; Key its per-socket state key.
	Socket string `json:"socket"`
	Key    string `json:"key"`
	// StateDir is the moox runtime dir, TTState the observer's dir.
	StateDir string `json:"stateDir"`
	TTState  string `json:"ttState"`
	// Running is "a daemon's pid file names a live process".
	Running bool    `json:"running"`
	PID     *int    `json:"pid"`
	Version *string `json:"version"`
	// StartedAt is the daemon's pid-file startedAt.
	StartedAt *string `json:"startedAt"`
	// Active is the server's @moox-active, null when unset or unreadable.
	Active *string `json:"active"`
	// Participating is "the participation marker exists" (moox §3.3).
	Participating bool `json:"participating"`
	// SaverState, LastSave and LastRestore are null until the saver (P5) and
	// the engine write them.
	SaverState  *string `json:"saverState"`
	LastSave    *string `json:"lastSave"`
	LastRestore *string `json:"lastRestore"`
}

// EncodeServeStatus renders the document, newline-terminated.
func EncodeServeStatus(s ServeStatus) ([]byte, error) {
	s.Schema = ServeStatusSchema
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("wire: encode serve status: %w", err)
	}
	return append(b, '\n'), nil
}
