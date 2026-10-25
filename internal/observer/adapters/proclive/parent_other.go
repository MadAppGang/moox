//go:build !darwin && !linux

package proclive

import "errors"

// processParent has no portable, fork-free implementation, so every other
// platform declines to answer. The core reads the error as "no evidence", so
// the consequence is the safe one: no session is ever hidden as another's
// child here, which is exactly what tt did before ancestry existed.
func processParent(int) (int, error) {
	return 0, errors.New("no parent pid lookup on this platform")
}
