package blackbox_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TEST-41, REQ-17/REQ-35: an explicitly skipped cold restore leaves only the
// bootstrap. Saving that pane must not replace the existing restorable pointer;
// the documented --force opt-out is checked in its own independent subcase.
func TestSnapshot_BootstrapOnlySavePreservesRestorableLastUnlessForced(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force_%v", force), func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			s := e.newServer("pristine", "SELF", false)
			s.makeRestorable()
			pointer, target := s.save()
			oldPointer, oldSnapshot := string(readFile(t, pointer)), string(readFile(t, target))
			s.stop()
			fresh := e.newServer("pristine", "SELF", false)
			fresh.waitReady()
			if rows := strings.Fields(fresh.mustTmux("list-panes", "-a", "-F", "#{pane_id}")); len(rows) != 1 {
				t.Fatalf("fixture has %d panes, want one bootstrap", len(rows))
			}
			before := sequence(fresh)
			args := []string{"save", "--direct"}
			if force {
				args = append(args, "--force")
			}
			requireCode(t, fresh.mooxRun(args...), 0)
			if force {
				if got := sequence(fresh); got != before+1 {
					t.Errorf("forced pristine save sequence=%d, want %d", got, before+1)
				}
				if got := string(readFile(t, pointer)); got == oldPointer {
					t.Error("--force did not publish bootstrap snapshot")
				}
			} else {
				if got := sequence(fresh); got != before {
					t.Errorf("pristine save published: seq=%d", got)
				}
				if got := string(readFile(t, pointer)); got != oldPointer {
					t.Error("bootstrap-only save clobbered last")
				}
				if got := string(readFile(t, target)); got != oldSnapshot {
					t.Error("bootstrap-only save modified restorable snapshot")
				}
			}
		})
	}
}

// TEST-42, REQ-16: continuous changes cannot reset the maximum wait forever.
// Short configured durations avoid a 30s suite cost; this tests the public
// duration option, not private timer state. The extra 2s is a harness scheduling
// allowance, not a claim about the exact 30s production boundary.
func TestSaveEvents_ContinuousChangesPublishBeforeConfiguredCeilingAndIdleStaysClean(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	// The daemon reads save timing when it starts; set it in the server config.
	e.timing = [3]string{"1s", "2s", "0"}
	s := e.newServer("ceiling", "SELF", false)
	s.makeRestorable()
	s.save()
	waitSequenceQuiet(s, 2500*time.Millisecond)
	before := sequence(s)
	start := time.Now()
	for i := 0; sequence(s) == before; i++ {
		if time.Since(start) > 4*time.Second {
			t.Fatal("continuous event stream postponed publish beyond the configured 2s ceiling plus harness allowance")
		}
		s.mustTmux("rename-window", "-t", "magus:0", fmt.Sprintf("stream-%d", i))
		<-time.After(100 * time.Millisecond)
	}
	// Stop the stream and let its trailing dirty event settle; it may legitimately
	// cause another publication after the first ceiling save.
	eventually(t, 5*time.Second, "trailing stream snapshot", func() (bool, string) {
		_, target := e.singleSnapshot()
		snapshot := decodeWrittenSnapshot(t, readFile(t, target), "save")
		wanted := s.mustTmux("display-message", "-p", "-t", "magus:0", "#{window_name}")
		for _, w := range snapshot.Windows {
			if w.Name == wanted {
				return true, wanted
			}
		}
		return false, "latest snapshot does not contain final window rename"
	})
	stable := sequence(s)
	end := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(end) {
		if got := sequence(s); got != stable {
			t.Fatalf("idle server published again: seq=%d, want %d", got, stable)
		}
		<-time.After(100 * time.Millisecond)
	}
}
