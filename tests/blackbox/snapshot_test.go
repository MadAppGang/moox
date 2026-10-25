package blackbox_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TEST-11, REQ-10/REQ-35. TEST-40 adds the now-supplied stale-state rules.
func TestDoctor_HealthyMatchedServerReturnsExit0(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("doctor", "SELF", false)
	s.waitReady()
	s.requireActive("1")
	r := s.mooxRun("doctor")
	requireCode(t, r, 0)
	if strings.TrimSpace(r.out+r.err) == "" {
		t.Error("doctor printed no checks")
	}
}

// TEST-12, REQ-21/REQ-22/REQ-35: pointer framing and privacy.
// TEST-35/36/43/44 independently exercise the supplied schema/contents contract.
func TestSnapshot_SavePublishesPrivateNonDanglingBasenamePointer(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("save", "SELF", false)
	s.makeRestorable()
	before := sequence(s)
	pointer, target := s.save()
	if got := sequence(s); got != before+1 {
		t.Errorf("save sequence=%d, want %d", got, before+1)
	}
	if filepath.Dir(pointer) != filepath.Dir(target) {
		t.Error("moox-last did not resolve within its own directory")
	}
	// Exact schema and path checks are covered separately in the extensions.
	state := s.globalEnv("MOOX_STATE_DIR")
	e.requireInside(state)
	if _, err := os.Stat(filepath.Join(state, "active")); err != nil {
		t.Errorf("matched server has no documented state-dir/active marker: %v", err)
	}
}

// TEST-13, REQ-21/REQ-35.
func TestSnapshot_UnchangedDirectSaveReturns0WithoutRepublishing(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("unchanged", "SELF", false)
	s.makeRestorable()
	pointer, target := s.save()
	wantPointer, wantTarget := string(readFile(t, pointer)), string(readFile(t, target))
	before := sequence(s)
	requireCode(t, s.mooxRun("save", "--direct"), 0)
	if got := sequence(s); got != before {
		t.Errorf("unchanged save republished: sequence=%d, want %d", got, before)
	}
	if got := string(readFile(t, pointer)); got != wantPointer {
		t.Errorf("unchanged save changed pointer: got %q, want %q", got, wantPointer)
	}
	if got := string(readFile(t, target)); got != wantTarget {
		t.Error("unchanged save mutated the published snapshot")
	}
}

// TEST-14, REQ-22/REQ-35. The command's human-readable output fields are not
// specified, so assert exit 0 and byte-for-byte nonmutation, not guessed labels.
func TestSnapshot_ShowReturns0AndDoesNotChangeSnapshotOrPointer(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("show", "SELF", false)
	s.makeRestorable()
	pointer, _ := s.save()
	before := image(t, filepath.Dir(pointer))
	requireCode(t, s.mooxRun("snapshot", "show"), 0)
	equalImage(t, image(t, filepath.Dir(pointer)), before, "snapshot show")
}

// TEST-15, REQ-22/REQ-35: §8.1 requires FILE or --all even with no pins.
func TestSnapshot_UnpinWithNothingPinnedReturns0AndPreservesSnapshot(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("unpin", "SELF", false)
	s.makeRestorable()
	pointer, target := s.save()
	wantPointer, wantTarget := string(readFile(t, pointer)), string(readFile(t, target))
	requireCode(t, s.mooxRun("snapshot", "unpin", "--all"), 0)
	if got := string(readFile(t, pointer)); got != wantPointer {
		t.Errorf("nothing-to-unpin changed last pointer: %q != %q", got, wantPointer)
	}
	if got := string(readFile(t, target)); got != wantTarget {
		t.Error("nothing-to-unpin deleted or modified the snapshot")
	}
	e.singleSnapshot()
}

// TEST-16, REQ-39/REQ-35.
func TestRestore_PopulatedServerRefusesWithExit3AndLeavesWorkspaceUnchanged(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("populated", "SELF", false)
	s.makeRestorable()
	pointer, _ := s.save()
	s.mustTmux("new-window", "-d", "-t", "magus:", "-n", "unsaved-current-work", "-c", e.home, "/bin/sh")
	before, last := s.view(), string(readFile(t, pointer))
	requireCode(t, s.mooxRun("restore"), 3)
	if got := s.view(); !reflect.DeepEqual(got, before) {
		t.Errorf("refused restore mutated workspace\nbefore=%q\nafter=%q", before, got)
	}
	if got := string(readFile(t, pointer)); got != last {
		t.Errorf("refused restore changed moox-last: %q != %q", got, last)
	}
}

// TEST-17, REQ-35: dry-run is local, even when the server is populated.
func TestRestore_DryRunOfSavedSnapshotReturns0WithoutChangingWorkspace(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("dryrestore", "SELF", false)
	s.makeRestorable()
	pointer, _ := s.save()
	before, files := s.view(), image(t, filepath.Dir(pointer))
	requireCode(t, s.mooxRun("restore", "--dry-run"), 0)
	if got := s.view(); !reflect.DeepEqual(got, before) {
		t.Errorf("dry-run changed panes: %q != %q", got, before)
	}
	equalImage(t, image(t, filepath.Dir(pointer)), files, "restore --dry-run snapshot files")
}

// TEST-18, REQ-35: invalidate an actual generated snapshot. TEST-36 also
// exercises the now-documented individual required fields and version rules.
func TestRestore_InvalidSnapshotDryRunReturnsExit4WithoutChangingPanes(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("invalid", "SELF", false)
	s.makeRestorable()
	_, target := s.save()
	writeFile(t, target, "not a moox snapshot: black-box corrupt fixture\n", 0600)
	before := s.view()
	requireCode(t, s.mooxRun("restore", "--dry-run"), 4)
	if got := s.view(); !reflect.DeepEqual(got, before) {
		t.Errorf("invalid local restore changed panes: %q != %q", got, before)
	}
}

// TEST-19, REQ-17/REQ-35: an unparsable restoring option means restoring,
// without fabricating a lock file or inspecting any private state.
// Exit 75 per §12.4 "busy: … a restore/restart owns the server" (orchestrator
// ruling 2026-10-25); 5 is only the clobber guard on snapshot content.
func TestSnapshot_UnparseableRestoreGuardBlocksDirectSaveWithBusy75(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("guard", "SELF", false)
	s.makeRestorable()
	pointer, _ := s.save()
	before, seq := image(t, filepath.Dir(pointer)), sequence(s)
	s.mustTmux("set-option", "-g", "@moox-restoring", "unparseable-blackbox-guard")
	defer s.mustTmux("set-option", "-gu", "@moox-restoring")
	requireCode(t, s.mooxRun("save", "--direct"), 75)
	equalImage(t, image(t, filepath.Dir(pointer)), before, "guarded save")
	if got := sequence(s); got != seq {
		t.Errorf("guarded save published sequence %d, want %d", got, seq)
	}
}
