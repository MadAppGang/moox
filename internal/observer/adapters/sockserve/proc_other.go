//go:build !darwin && !linux

package sockserve

import (
	"errors"
	"time"
)

// processStartedAt has no portable implementation, so every other platform
// declines to have an opinion — which routes --stop to a refusal rather than
// to a signal, the safe direction.
func processStartedAt(pid int) (time.Time, error) {
	return time.Time{}, errors.New("no process start time on this platform")
}
