package blackbox_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TEST-25, REQ-15: no attached client and status off throughout. The assertion
// uses the documented publish sequence, not calls to internal dirty functions.
func TestSaveEvents_HeadlessLayoutBurstPublishesExactlyOneDebouncedSnapshot(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	// The daemon reads save timing when it starts; set it in the server config.
	e.timing = [3]string{"500ms", "3s", "0"}
	s := e.newServer("events", "SELF", false)
	s.makeRestorable()
	s.save()
	waitSequenceQuiet(s, 1500*time.Millisecond)
	if got := s.mustTmux("list-clients", "-F", "#{client_name}"); got != "" {
		t.Fatalf("test unexpectedly has attached clients: %q", got)
	}
	if got := s.mustTmux("show-options", "-gqv", "status"); got != "off" {
		t.Fatalf("test requires status off, got %q", got)
	}
	if got := s.mustTmux("show-options", "-gqv", "status-right"); strings.Contains(got, "#(") {
		// V2: "status-right contains no #(". Non-fatal so the debounce
		// behaviour below is still exercised and reported independently.
		t.Errorf("status-right contains a prohibited command trigger: %q", got)
	}
	before := sequence(s)
	// A single tmux command queue prevents slow test-process startup from
	// stretching the burst past the 500 ms debounce interval.
	s.mustTmux(
		"new-window", "-d", "-t", "magus:", "-n", "burst", "-c", e.home, "/bin/sh", ";",
		"split-window", "-d", "-t", "magus:burst", "-c", e.home, "/bin/sh", ";",
		"kill-pane", "-t", "magus:burst.1")
	eventually(t, 8*time.Second, "one debounced publish after headless layout changes", func() (bool, string) {
		got := sequence(s)
		return got == before+1, "unexpected save sequence"
	})
	// Observe >2 debounce periods after the first save to catch duplicate timers,
	// without a blind sleep or dependence on file timestamp granularity.
	end := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(end) {
		if got := sequence(s); got != before+1 {
			t.Fatalf("burst published more than once: sequence=%d, want %d", got, before+1)
		}
		<-time.After(40 * time.Millisecond)
	}
	e.singleSnapshot()
}

// waitSequenceQuiet waits until @moox_save_seq has not changed for `quiet`, so
// debounced saves caused by fixture setup cannot be miscounted as the burst's.
func waitSequenceQuiet(s *server, quiet time.Duration) {
	s.e.t.Helper()
	last, since := sequence(s), time.Now()
	eventually(s.e.t, 15*time.Second, "save sequence to settle after fixture setup", func() (bool, string) {
		if now := sequence(s); now != last {
			last, since = now, time.Now()
		}
		return time.Since(since) >= quiet, fmt.Sprintf("seq=%d still changing", last)
	})
}
